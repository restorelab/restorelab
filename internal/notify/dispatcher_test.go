package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/store"
)

var errBroken = errors.New("the database is on fire")

// fakeStore is the history, recorded rather than written.
//
// It is mutex protected for the reason the scheduler's fakeStore is: Tick is
// driven from a second goroutine in one of the tests below, and a data race
// in a fixture is a flake nobody can reproduce.
//
// CreateDelivery pushes onto the due queue as well as onto created, because
// that is what the real store does: a delivery is written due immediately, so
// the delivery pass of the same tick picks it up.
type fakeStore struct {
	mu sync.Mutex

	unnotified  []store.RunSummary
	previous    *store.RunSummary
	unevaluable bool

	// claimed is whether this dispatcher wins the claim. False is another
	// dispatcher having taken the run first.
	claimed bool

	// claims records which runs were actually claimed, in order. A run that
	// is claimed and then passed over in silence is a different outcome from
	// one that was never claimed, and only the first leaves the queue.
	claims []string

	created  []store.Delivery
	due      []store.Delivery
	settled  []store.Delivery
	prevCall int

	// values is what each run measured, by run id, in the shape the store
	// answers with: by check seq, then by capture name. valueCall counts the
	// reads, because how many there are is a contract of its own - the
	// dispatcher must not pay a round trip for a run it is not going to
	// speak about.
	values    map[string]map[int]map[string]float64
	valueCall int

	broken bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{claimed: true}
}

func (f *fakeStore) UnnotifiedRuns(context.Context, int) ([]store.RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, errBroken
	}
	return f.unnotified, nil
}

func (f *fakeStore) ClaimRunForNotify(_ context.Context, runID string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return false, errBroken
	}
	if f.claimed {
		f.claims = append(f.claims, runID)
	}
	return f.claimed, nil
}

// wasClaimed reports whether runID was taken out of the queue.
func (f *fakeStore) wasClaimed(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.claims {
		if id == runID {
			return true
		}
	}
	return false
}

func (f *fakeStore) PreviousStory(context.Context, string, store.Position) (*store.RunSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prevCall++
	if f.broken {
		return nil, false, errBroken
	}
	return f.previous, f.unevaluable, nil
}

func (f *fakeStore) CreateDelivery(_ context.Context, d store.Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return errBroken
	}
	for _, done := range f.created {
		if done.RunID == d.RunID && done.ChannelID == d.ChannelID {
			return store.ErrDuplicate
		}
	}
	f.created = append(f.created, d)
	f.due = append(f.due, d)
	return nil
}

func (f *fakeStore) DueDeliveries(context.Context, time.Time, int) ([]store.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, errBroken
	}
	return append([]store.Delivery(nil), f.due...), nil
}

func (f *fakeStore) SettleDelivery(_ context.Context, d store.Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return errBroken
	}
	f.settled = append(f.settled, d)
	// A settled delivery leaves the queue whatever became of it: a pending
	// one comes back through its next_at, which no test here rewinds.
	kept := make([]store.Delivery, 0, len(f.due))
	for _, row := range f.due {
		if row.ID != d.ID {
			kept = append(kept, row)
		}
	}
	f.due = kept
	return nil
}

func (f *fakeStore) createdRows() []store.Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Delivery(nil), f.created...)
}

func (f *fakeStore) settledRows() []store.Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Delivery(nil), f.settled...)
}

func (f *fakeStore) RunCheckValues(_ context.Context, runID string) (map[int]map[string]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valueCall++
	if f.broken {
		return nil, errBroken
	}
	return f.values[runID], nil
}

func (f *fakeStore) valueCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valueCall
}

func (f *fakeStore) previousCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prevCall
}

func (f *fakeStore) setBroken(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broken = v
}

// --- fixtures ----------------------------------------------------------------

var testNow = time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)

// terminalRun is a drill that reached a verdict, as a listing row.
func terminalRun(id string, result core.RunResult, level core.ProofLevel) store.RunSummary {
	return store.RunSummary{
		ID:               id,
		PlanName:         "nightly",
		SourceWorkloadID: "110",
		SourceName:       "web-01",
		State:            core.RunSuccess,
		Result:           result,
		StartedAt:        testNow.Add(-time.Hour),
		CompletedAt:      testNow.Add(-30 * time.Minute),
		RTO:              4 * time.Minute,
		ProofLevel:       level,
	}
}

// sealedChannel is a configured channel whose URL is sealed, as one read off
// disk always is. Nothing here bypasses the sealing: the dispatcher unseals
// it itself, which is the path an operator's channel actually takes.
func sealedChannel(t *testing.T, k crypto.Key, id, kind, rawURL string) config.Notification {
	t.Helper()
	sealed, err := crypto.Seal(k, rawURL)
	if err != nil {
		t.Fatalf("sealing the url of channel %s: %v", id, err)
	}
	return config.Notification{ID: id, Kind: kind, URL: sealed}
}

// off returns the channel with enabled explicitly false.
func off(n config.Notification) config.Notification {
	no := false
	n.Enabled = &no
	return n
}

func testKey(t *testing.T) crypto.Key {
	t.Helper()
	k, err := crypto.NewKey()
	if err != nil {
		t.Fatalf("generating a master key: %v", err)
	}
	return k
}

// newTestDispatcher builds a dispatcher whose clock the test controls, and
// hands back the log it wrote so a test can assert on what was said.
func newTestDispatcher(t *testing.T, s Store, k crypto.Key, chans ...config.Notification) (*Dispatcher, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	d, err := New(Options{
		Store: s,
		// Wrapped in a function because that is what the dispatcher takes now:
		// the configuration is read once per tick, not once at construction.
		// TestAChannelAddedAfterStartupIsUsedOnTheNextTick is the test that
		// depends on the difference.
		Channels: func() []config.Notification { return chans },
		Key:      k,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:      func() time.Time { return testNow },
		NewID:    func() string { return "delivery-1" },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return d, logs
}

// okServer answers every POST the way Discord does.
func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- the decision pass -------------------------------------------------------

// The silence is the feature. A run that moved nothing must not reach a
// channel, or the channel is muted within a week and the one message that
// mattered is never read.
func TestARunWhoseStoryDidNotChangeProducesNoDelivery(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultSuccess, core.ProofService)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofService)
	f.previous = &before

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	if rows := f.createdRows(); len(rows) != 0 {
		t.Fatalf("a run that changed nothing produced %d delivery(ies): %+v", len(rows), rows)
	}
}

func TestAChangedVerdictProducesOneDeliveryPerEnabledChannel(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultFailed, core.ProofBoot)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofService)
	f.previous = &before

	d, _ := newTestDispatcher(t, f, key,
		sealedChannel(t, key, "ops", "discord", srv.URL),
		sealedChannel(t, key, "hooks", "webhook", srv.URL),
		off(sealedChannel(t, key, "muted", "slack", srv.URL)),
	)
	d.Tick(context.Background())

	rows := f.createdRows()
	if len(rows) != 2 {
		t.Fatalf("got %d deliveries, want one per enabled channel: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.ChannelID == "muted" {
			t.Error("a disabled channel was sent a message")
		}
		if row.Kind != string(KindVerdict) {
			t.Errorf("delivery kind = %q, want %q", row.Kind, KindVerdict)
		}
		if row.State != store.DeliveryPending {
			t.Errorf("a fresh delivery is %q, want %q", row.State, store.DeliveryPending)
		}
		if !strings.Contains(row.Payload, "web-01") {
			t.Errorf("the stored payload does not name the workload: %s", row.Payload)
		}
	}
}

// A dispatcher that lost the claim stops there. Asking for the story anyway
// would be a query per run per process, and acting on it would be the
// duplicate message the claim exists to prevent.
func TestARunClaimedByAnotherDispatcherIsSkipped(t *testing.T) {
	key := testKey(t)

	f := newFakeStore()
	f.claimed = false
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultFailed, core.ProofBoot)}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", "https://example.invalid/hook"))
	d.Tick(context.Background())

	if rows := f.createdRows(); len(rows) != 0 {
		t.Fatalf("a run claimed elsewhere produced %d delivery(ies)", len(rows))
	}
	if n := f.previousCalls(); n != 0 {
		t.Fatalf("PreviousStory was called %d time(s) for a run this dispatcher did not claim", n)
	}
}

// The reason Options.Channels is a function.
//
// The dashboard can add a channel while `restorelab serve` is running. When
// the dispatcher read its channel list once in New, that channel received
// nothing until somebody restarted the process: the screen accepted the
// channel, config.yaml carried it, and the product stayed silent. An operator
// in that position concludes the alerting does not work, and they are right.
//
// So the list is re-read on every tick, and this is the test that says so.
// Without it the regression is invisible: every other test here builds its
// channels before the first tick and would stay green.
func TestAChannelAddedAfterTheDispatcherStartedIsUsedOnTheNextTick(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultFailed, core.ProofBoot)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofService)
	f.previous = &before

	source := &liveChannels{list: []config.Notification{
		sealedChannel(t, key, "ops", "discord", srv.URL),
	}}

	logs := &bytes.Buffer{}
	d, err := New(Options{
		Store:    f,
		Channels: source.get,
		Key:      key,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:      func() time.Time { return testNow },
		NewID:    func() string { return "delivery-1" },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	d.Tick(context.Background())
	if got := channelIDs(f.createdRows()); !reflect.DeepEqual(got, []string{"ops"}) {
		t.Fatalf("first tick delivered to %v, want just the configured channel", got)
	}

	// What the dashboard does: writes the channel, and nothing else. Nobody
	// tells the dispatcher.
	source.add(sealedChannel(t, key, "late", "slack", srv.URL))

	d.Tick(context.Background())

	got := channelIDs(f.createdRows())
	if !reflect.DeepEqual(got, []string{"ops", "late"}) {
		t.Fatalf("deliveries went to %v, want the channel added after startup to have one too", got)
	}
}

// A kind nobody recognises is warned about once, not once a minute.
//
// The validation used to run in New, where a typo cost one line at startup.
// Moving it into the tick without the complainedAbout map would have written
// that line every minute for as long as the typo lived, which is how a log
// stops being read at all. It is the mechanism scheduler.complainedAbout is,
// and it is here for the same reason.
func TestAnUnknownChannelKindIsComplainedAboutOnce(t *testing.T) {
	key := testKey(t)

	f := newFakeStore()
	source := &liveChannels{list: []config.Notification{
		sealedChannel(t, key, "typo", "dsicord", "https://example.invalid/hook"),
	}}

	logs := &bytes.Buffer{}
	d, err := New(Options{
		Store:    f,
		Channels: source.get,
		Key:      key,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for range 5 {
		d.Tick(context.Background())
	}

	if n := strings.Count(logs.String(), "channel will not be used"); n != 1 {
		t.Fatalf("the unusable channel was complained about %d time(s), want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "typo") {
		t.Errorf("the warning does not name the channel, so nobody can fix it:\n%s", logs.String())
	}
}

// liveChannels is a channel list that changes under the dispatcher, which is
// what the real one does: internal/cli returns a copy of the configuration
// under the mutex its writers take.
type liveChannels struct {
	mu   sync.Mutex
	list []config.Notification
}

func (c *liveChannels) get() []config.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]config.Notification(nil), c.list...)
}

func (c *liveChannels) add(n config.Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, n)
}

// channelIDs is who was written a message, in the order the rows were made.
func channelIDs(rows []store.Delivery) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ChannelID)
	}
	return out
}

// --- the delivery pass -------------------------------------------------------

// pendingDelivery is a message waiting to be posted, as DueDeliveries hands
// it back.
func pendingDelivery(channelID string, attempts int) store.Delivery {
	return store.Delivery{
		ID:        "delivery-1",
		RunID:     "run-1",
		ChannelID: channelID,
		Kind:      string(KindVerdict),
		State:     store.DeliveryPending,
		Attempts:  attempts,
		NextAt:    testNow,
		Payload:   `{"content":"hello"}`,
		CreatedAt: testNow.Add(-time.Minute),
	}
}

func TestAFailedAttemptIsRescheduledRatherThanDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	key := testKey(t)

	f := newFakeStore()
	f.due = []store.Delivery{pendingDelivery("ops", 0)}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	rows := f.settledRows()
	if len(rows) != 1 {
		t.Fatalf("got %d settled deliveries, want 1", len(rows))
	}
	got := rows[0]
	if got.State != store.DeliveryPending {
		t.Errorf("state = %q, want %q: a 500 is worth asking again", got.State, store.DeliveryPending)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if want := testNow.Add(30 * time.Second); !got.NextAt.Equal(want) {
		t.Errorf("next attempt at %v, want %v", got.NextAt, want)
	}
	if got.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 recorded on the row", got.Status)
	}
	if got.Err == "" {
		t.Error("a failed attempt recorded no reason: doctor would show a channel that is merely quiet")
	}
}

func TestTheFourthFailureIsFinal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "still broken", http.StatusBadGateway)
	}))
	defer srv.Close()
	key := testKey(t)

	f := newFakeStore()
	f.due = []store.Delivery{pendingDelivery("ops", len(Attempts)-1)}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	rows := f.settledRows()
	if len(rows) != 1 {
		t.Fatalf("got %d settled deliveries, want 1", len(rows))
	}
	got := rows[0]
	if got.State != store.DeliveryFailed {
		t.Errorf("state = %q, want %q once the window is spent", got.State, store.DeliveryFailed)
	}
	if got.Attempts != len(Attempts) {
		t.Errorf("attempts = %d, want %d", got.Attempts, len(Attempts))
	}
	if !got.NextAt.IsZero() {
		t.Errorf("a failed delivery still carries a next attempt at %v", got.NextAt)
	}
	if got.Status != http.StatusBadGateway || !strings.Contains(got.Err, "502") {
		t.Errorf("the row does not say what happened: status=%d err=%q", got.Status, got.Err)
	}
}

// A 404 is a revoked webhook. Asking again will not bring it back, and
// retrying it only delays the moment somebody is told the path is broken.
func TestARefusedChannelFailsWithoutBurningTheWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown webhook", http.StatusNotFound)
	}))
	defer srv.Close()
	key := testKey(t)

	f := newFakeStore()
	f.due = []store.Delivery{pendingDelivery("ops", 0)}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	rows := f.settledRows()
	if len(rows) != 1 || rows[0].State != store.DeliveryFailed {
		t.Fatalf("a 404 must be recorded as failed at once, got %+v", rows)
	}
}

// THE constraint of this task. A process shutting down cancels the context it
// owns, and a cancelled context is not a fact about a channel. Recording one
// as a failure would mark the whole pending queue as refused by channels that
// were working, and the operator would go looking for a breakage that never
// happened.
func TestAStoppingDispatcherLeavesItsDeliveriesPending(t *testing.T) {
	received, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(received)
		<-release
	}))
	// Released before the server is closed: httptest.Server.Close waits for
	// its handlers, and a handler waiting on a channel nobody closes is a
	// test that hangs rather than fails.
	t.Cleanup(func() { close(release); srv.Close() })
	key := testKey(t)

	f := newFakeStore()
	f.due = []store.Delivery{pendingDelivery("ops", 0)}

	d, logs := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-received
		cancel()
	}()
	d.Tick(ctx)

	if rows := f.settledRows(); len(rows) != 0 {
		t.Fatalf("a cancelled context settled %d delivery(ies) as %q; the channel was never at fault",
			len(rows), rows[0].State)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("shutting down logged a warning about a channel that was fine:\n%s", logs.String())
	}
}

// --- robustness --------------------------------------------------------------

// The scheduler's Tick contract, for the scheduler's reason: the loop
// stopping is the only failure mode that silently ends every alert in the
// installation.
func TestABrokenStoreNeverStopsTheLoop(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	f := newFakeStore()
	f.setBroken(true)
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultFailed, core.ProofBoot)}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	f.setBroken(false)
	d.Tick(context.Background())

	if rows := f.createdRows(); len(rows) != 1 {
		t.Fatalf("the tick after a database failure produced %d deliveries, want 1", len(rows))
	}
}

func TestRunReturnsWhenItsContextIsCancelled(t *testing.T) {
	key := testKey(t)
	d, _ := newTestDispatcher(t, newFakeStore(), key)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil on a cancelled context", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within two seconds of its context being cancelled")
	}
}

// A message queued for a channel somebody has since deleted has nowhere to
// go. Left pending it would come back every tick forever, so it is recorded
// as failed with the reason, which is what doctor reads.
func TestADeliveryForAnUnconfiguredChannelIsRecordedRatherThanRetriedForever(t *testing.T) {
	key := testKey(t)

	f := newFakeStore()
	f.due = []store.Delivery{pendingDelivery("deleted", 0)}

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", "https://example.invalid/hook"))
	d.Tick(context.Background())

	rows := f.settledRows()
	if len(rows) != 1 || rows[0].State != store.DeliveryFailed {
		t.Fatalf("a delivery for a removed channel was not recorded as failed: %+v", rows)
	}
	if !strings.Contains(rows[0].Err, "deleted") {
		t.Errorf("the reason does not name the channel: %q", rows[0].Err)
	}
}

// --- the two structural guards -----------------------------------------------

// Invariant 17, applied to a second background component. Automating alerts
// had to add no destructive surface to the product, and this is where that is
// guaranteed rather than promised: a dispatcher that cannot hold a provider
// or a recovery engine cannot restore, boot or delete anything, whatever a
// dead Discord does to it.
func TestOptionsCarryNoProviderOrEngine(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted options with no store")
	}

	// Reflection rather than a comment, because the field that breaks this
	// gets added by somebody who never read the comment.
	forbidden := []string{"provider", "engine", "recovery", "hypervisor", "restore"}
	typ := reflect.TypeOf(Options{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		subject := strings.ToLower(f.Name + " " + f.Type.String())
		for _, word := range forbidden {
			if strings.Contains(subject, word) {
				t.Errorf("Options.%s (%s) carries %q: the dispatcher must not be able to act on a cluster",
					f.Name, f.Type, word)
			}
		}
	}

	// A compile-time reminder beside the runtime one, as the scheduler keeps.
	var opts Options
	_ = opts.Store
}

// A webhook URL is a bearer credential. It must never reach a log line, at
// any level, nor the delivery row an operator reads in doctor: both outlive
// the incident, and both get pasted into support threads.
func TestNoChannelURLIsEverLoggedOrRecorded(t *testing.T) {
	const secretPath = "/api/webhooks/1/super-secret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	key := testKey(t)
	target := srv.URL + secretPath

	f := newFakeStore()
	f.unnotified = []store.RunSummary{terminalRun("run-1", core.ResultFailed, core.ProofBoot)}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofService)
	f.previous = &before

	d, logs := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", target))
	d.Tick(context.Background())

	if got := logs.String(); strings.Contains(got, secretPath) || strings.Contains(got, target) {
		t.Errorf("the channel url reached the log:\n%s", got)
	}
	if !strings.Contains(logs.String(), "channel=ops") {
		t.Errorf("the log never names the channel, so an operator cannot tell which one broke:\n%s", logs.String())
	}
	for _, row := range append(f.settledRows(), f.createdRows()...) {
		if strings.Contains(row.Err, secretPath) || strings.Contains(row.Payload, secretPath) {
			t.Errorf("the channel url reached a delivery row: err=%q payload=%q", row.Err, row.Payload)
		}
	}
}

// TestABacklogOlderThanADayIsClaimedButNotAnnounced covers the way this
// feature's own discipline can be defeated from the outside.
//
// Migration 0008 backfills every run that existed when it ran, so an upgrade
// is safe. What it cannot cover is an installation that drills from the CLI
// with no server, or one running with --no-notify: those accumulate runs
// nobody has considered, and the day somebody configures their first channel
// the first tick would pour weeks of history into it. Twenty green messages
// in one evening is the exact outcome the silence rules exist to prevent, so
// arriving at it through the back door is no better.
//
// The old runs must still be claimed. Skipping them without claiming would
// leave them pending forever, and the same batch would be re-read every tick.
func TestABacklogOlderThanADayIsClaimedButNotAnnounced(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	stale := terminalRun("run-old", core.ResultFailed, core.ProofBoot)
	stale.StartedAt = testNow.Add(-8 * 24 * time.Hour)
	stale.CompletedAt = testNow.Add(-8 * 24 * time.Hour).Add(10 * time.Minute)

	fresh := terminalRun("run-new", core.ResultFailed, core.ProofBoot)

	f := newFakeStore()
	f.unnotified = []store.RunSummary{stale, fresh}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofService)
	f.previous = &before

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	for _, id := range []string{"run-old", "run-new"} {
		if !f.wasClaimed(id) {
			t.Errorf("run %s was not claimed: it would be re-read on every tick, forever", id)
		}
	}

	if len(f.created) != 1 {
		t.Fatalf("%d deliveries, want 1: only the recent run is news", len(f.created))
	}
	if f.created[0].RunID != "run-new" {
		t.Errorf("delivery is about %s, want run-new: a drill from last week is archaeology, not an alert",
			f.created[0].RunID)
	}
}

// TestAgeIsMeasuredFromTheStartWhenNothingRecordedTheEnd guards the fallback.
//
// A run settled by reconciliation after its worker died is terminal without
// ever recording a completion. Reading a zero completed_at as "just now"
// would make exactly those runs permanently announceable, which is backwards:
// they are the ones least likely to be fresh.
func TestAgeIsMeasuredFromTheStartWhenNothingRecordedTheEnd(t *testing.T) {
	srv := okServer(t)
	key := testKey(t)

	orphan := terminalRun("run-orphan", core.ResultFailed, core.ProofBoot)
	orphan.State = core.RunFailed
	orphan.StartedAt = testNow.Add(-8 * 24 * time.Hour)
	orphan.CompletedAt = time.Time{}

	f := newFakeStore()
	f.unnotified = []store.RunSummary{orphan}
	before := terminalRun("run-0", core.ResultSuccess, core.ProofService)
	f.previous = &before

	d, _ := newTestDispatcher(t, f, key, sealedChannel(t, key, "ops", "discord", srv.URL))
	d.Tick(context.Background())

	if !f.wasClaimed("run-orphan") {
		t.Error("the run was not claimed")
	}
	if len(f.created) != 0 {
		t.Errorf("%d deliveries: a week-old interrupted run was announced as news", len(f.created))
	}
}
