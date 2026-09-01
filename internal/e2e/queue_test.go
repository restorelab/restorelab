package e2e

// The whole of phase B2, assembled: a real HTTP API, a real worker, a real
// drill history and the simulated cluster of drill_test.go.
//
// Everything below goes through the product's own path - POST queues a row,
// a worker claims it and builds an engine, the engine restores into the
// isolated bridge and destroys what it made, and the run settles in the
// database. Nothing here calls the engine directly; that is what makes it
// worth running.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
	"github.com/restorelab/restorelab/internal/store"
	"github.com/restorelab/restorelab/internal/worker"
)

// The drill every test in this file queues: one in-guest command, so the
// whole thing needs no network path at all into the isolated bridge. The
// address the fake guest agent reports is unreachable from this process by
// design, and that is the point of checking a service from inside.
const (
	queueCheck    = "cmd:systemctl is-active postgresql"
	queueCheckCmd = "/bin/sh -c systemctl is-active postgresql"
	queueProvider = "proxmox-test"
)

// restorePath is the one request that creates a machine. Counting it is how
// the crash test proves the absence that matters.
var restorePath = "/api2/json/nodes/" + node + "/qemu"

// tempWorkloadPath is the temporary workload's own resource, whose DELETE is
// the cleanup this phase promises.
var tempWorkloadPath = restorePath + "/" + tempVMID

// --- the history -------------------------------------------------------------

// newQueueStore returns a real, migrated drill history: SQLite by default,
// and PostgreSQL when RESTORELAB_TEST_DATABASE_URL points at a server.
//
// The engine matters here more than anywhere else in this package. The claim
// is the one query RestoreLab writes twice - SELECT ... FOR UPDATE SKIP
// LOCKED on PostgreSQL, an immediate transaction on SQLite - and this file is
// the only place where the complete assembly, API and worker and cluster,
// ever meets it.
func newQueueStore(t *testing.T) store.Store {
	t.Helper()

	dsn := os.Getenv("RESTORELAB_TEST_DATABASE_URL")
	if dsn == "" {
		return newTestHistory(t)
	}

	ctx := context.Background()
	schema := "rl_e2e_" + sanitiseSchema(t.Name())

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})

	scoped := dsn + "&search_path=" + schema
	if !strings.Contains(dsn, "?") {
		scoped = dsn + "?search_path=" + schema
	}

	cfg := store.Config{URL: scoped}
	if _, err := store.Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// sanitiseSchema turns a Go test name into a bare PostgreSQL identifier.
func sanitiseSchema(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// --- holding a drill still ---------------------------------------------------

// requestGate freezes one kind of request to the simulated cluster, so that a
// test can act while a drill is genuinely mid-flight.
//
// It is what replaces a sleep: "wait until the drill has restored and is
// about to power the clone on" is a condition, and the gate is how this file
// states it.
type requestGate struct {
	match func(*http.Request) bool

	reached chan struct{}
	release chan struct{}

	hit    sync.Once
	opened sync.Once
}

func newRequestGate(match func(*http.Request) bool) *requestGate {
	return &requestGate{
		match:   match,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// startGate holds the POST that powers the temporary workload on. By then the
// restore has happened, the clone exists on the cluster, its production
// network has been stripped and RestoreLab's ownership metadata is on it -
// which is the state a cancellation or a crash has to survive.
func startGate() *requestGate {
	return newRequestGate(func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Path == tempWorkloadPath+"/status/start"
	})
}

// hold blocks a matching request until the gate is opened.
func (g *requestGate) hold(r *http.Request) {
	if g == nil || !g.match(r) {
		return
	}
	g.hit.Do(func() { close(g.reached) })
	<-g.release
}

// waitReached blocks until the gated request has arrived.
func (g *requestGate) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the drill never reached the gated request")
	}
}

// open lets every held request through, now and in future.
func (g *requestGate) open() {
	g.opened.Do(func() { close(g.release) })
}

// gatedCluster is the simulated Proxmox with a gate in front of it.
//
// The gate blocks before fakePVE's own lock is taken, so a held request
// freezes one drill and not the whole cluster: the reconciliation that has to
// destroy the frozen drill's leftovers still gets served.
type gatedCluster struct {
	pve  *fakePVE
	gate *requestGate
}

func (c *gatedCluster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.gate.hold(r)
	c.pve.ServeHTTP(w, r)
}

// --- the fixture -------------------------------------------------------------

// queueProviders hands both the API and the worker the same live client for
// the simulated cluster, and reports the configured providers with their
// secrets still in them - so that the leak sweep proves the API redacts
// rather than that the test forgot to supply anything.
type queueProviders struct {
	provider *proxmox.Provider
	entries  []config.Provider
}

func (p queueProviders) Entries() []config.Provider { return p.entries }

func (p queueProviders) Hypervisor(string) (core.HypervisorProvider, error) {
	return p.provider, nil
}

func (p queueProviders) Backups(string) (core.BackupProvider, error) {
	return p.provider, nil
}

var (
	_ api.ProviderSet  = queueProviders{}
	_ worker.Providers = queueProviders{}
)

// queueFixture is the whole product: a simulated cluster, a real history, a
// real API server and the tokens to reach it.
type queueFixture struct {
	pve     *fakePVE
	history store.Store
	cfg     *config.Config
	server  *httptest.Server

	// operate is a token that may trigger; read may not. Both are real
	// tokens in the real store.
	operate string
	read    string
}

// newQueueFixture builds it. Pass a gate to freeze a drill mid-flight, or nil.
func newQueueFixture(t *testing.T, gate *requestGate) *queueFixture {
	t.Helper()

	pve := newFakePVE(guestAddr)
	pve.execResults[queueCheckCmd] = execResult{Stdout: "active\n"}

	cluster := httptest.NewServer(&gatedCluster{pve: pve, gate: gate})
	t.Cleanup(cluster.Close)
	if gate != nil {
		// Registered last, so it runs first: a held request would otherwise
		// keep cluster.Close waiting for a handler that never returns.
		t.Cleanup(gate.open)
	}

	provider, err := proxmox.New(proxmox.Config{
		ID:          queueProvider,
		Endpoint:    cluster.URL,
		TokenID:     leakProviderTokenID,
		TokenSecret: leakProviderSecret,
		// Generous on purpose: a gated request is meant to be released by the
		// test, not by a client timeout deciding the drill failed.
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("proxmox.New: %v", err)
	}

	cfg := config.New()
	cfg.Database.URL = "postgres://restorelab:" + leakDBPassword + "@db.internal:5432/history"
	cfg.Defaults.Provider = queueProvider
	cfg.Providers = []config.Provider{{
		ID: queueProvider, Kind: "proxmox", Roles: []string{"hypervisor", "backup"},
		Endpoint: cluster.URL,
		TokenID:  leakProviderTokenID, TokenSecret: leakProviderSecret,
	}}

	history := newQueueStore(t)

	f := &queueFixture{pve: pve, history: history, cfg: cfg}
	f.operate = mintToken(t, history, "e2e-operate", store.ScopeRead, store.ScopeOperate)
	f.read = mintToken(t, history, "e2e-read", store.ScopeRead)

	f.server = httptest.NewServer(api.New(api.Options{
		History:   history,
		Tokens:    history,
		Providers: queueProviders{provider: provider, entries: cfg.Providers},
		Config:    cfg,
	}))
	t.Cleanup(f.server.Close)

	return f
}

// mintToken creates a real API token with the given scopes and returns its
// secret.
func mintToken(t *testing.T, history store.Store, name string, scopes ...string) string {
	t.Helper()

	secret, record, err := api.NewToken(name, time.Now())
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	record.Scopes = scopes
	if err := history.CreateToken(context.Background(), record); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return secret
}

// startWorker runs a real worker against the fixture and returns the function
// that stops it. The stop is idempotent and also registered as a cleanup, so
// a test that ends early never leaves a drill in flight.
func (f *queueFixture) startWorker(t *testing.T, owner string, mutate func(*worker.Options)) func() {
	t.Helper()

	opts := worker.Options{
		Store:     f.history,
		Providers: queueProviders{provider: f.provider(t)},
		Config:    f.cfg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Owner:     owner,
		Lease:     time.Minute,
		Poll:      2 * time.Millisecond,
		// Short enough that a cancellation is noticed inside a test rather
		// than inside a coffee break.
		RenewEvery: 5 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}

	w, err := worker.New(opts)
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			t.Errorf("worker.Run: %v", err)
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the worker did not stop within 30s")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// provider builds a second client onto the same simulated cluster, for a
// worker that is not the one the API holds. Both talk to the same fake, which
// is the point: two processes, one cluster.
func (f *queueFixture) provider(t *testing.T) *proxmox.Provider {
	t.Helper()

	p, err := proxmox.New(proxmox.Config{
		ID:          queueProvider,
		Endpoint:    f.cfg.Providers[0].Endpoint,
		TokenID:     leakProviderTokenID,
		TokenSecret: leakProviderSecret,
		Timeout:     time.Minute,
	})
	if err != nil {
		t.Fatalf("proxmox.New: %v", err)
	}
	return p
}

// --- talking to the API ------------------------------------------------------

// do performs an authenticated request and returns the status and body.
func (f *queueFixture) do(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func (f *queueFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	return f.do(t, http.MethodGet, path, f.operate, "")
}

// triggerBody is the smallest request that describes this file's drill.
func triggerBody() string {
	return fmt.Sprintf(`{"workload_id":%q,"provider":%q,"node":%q,"checks":[%q],"rto_target":"10m"}`,
		sourceVMID, queueProvider, node, queueCheck)
}

// trigger queues the drill and returns the run id the API handed out.
func (f *queueFixture) trigger(t *testing.T) string {
	t.Helper()

	status, body := f.do(t, http.MethodPost, "/api/v1/recovery-runs", f.operate, triggerBody())
	if status != http.StatusCreated {
		t.Fatalf("POST /recovery-runs = %d: %s", status, body)
	}

	var dto struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("the queued run is not JSON: %v (%s)", err, body)
	}
	if dto.ID == "" {
		t.Fatalf("the API queued a drill without telling the caller its id: %s", body)
	}
	if dto.State != string(core.RunQueued) {
		t.Errorf("state = %q, want QUEUED: nothing has run yet", dto.State)
	}
	return dto.ID
}

// --- waiting on conditions, never on the clock -------------------------------

// waitUntil polls a condition up to a deadline. It is deliberately not a
// sleep: a test that sleeps and hopes becomes flaky the day CI runs it under
// -race, which is the day this suite finally gets a race detector.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitSettled waits for a run to reach a state it will never leave.
func waitSettled(t *testing.T, history store.Store, runID string) *core.RecoveryRun {
	t.Helper()

	waitUntil(t, "run "+runID+" to settle", func() bool {
		run, err := history.GetRun(context.Background(), runID)
		return err == nil && run.State.Terminal()
	})
	return loadRun(t, history, runID)
}

func loadRun(t *testing.T, history store.Store, runID string) *core.RecoveryRun {
	t.Helper()

	run, err := history.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun %s: %v", runID, err)
	}
	return run
}

// --- reading the simulated cluster -------------------------------------------

// hasVM reports whether a workload is on the cluster, under the fake's own
// lock: unlike the tests in drill_test.go, these run while a drill is still
// going.
func hasVM(pve *fakePVE, id string) bool {
	pve.mu.Lock()
	defer pve.mu.Unlock()
	_, ok := pve.vms[id]
	return ok
}

// countRequests counts the recorded requests matching a method and an exact
// path.
func countRequests(pve *fakePVE, method, path string) int {
	n := 0
	for _, r := range pve.recorded() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// countRestores counts every request that would create a machine.
func countRestores(pve *fakePVE) int {
	return countRequests(pve, http.MethodPost, restorePath)
}

// --- the tests ---------------------------------------------------------------

// The whole phase, in one test: POST queues, the worker claims and executes,
// the drill restores into isolation, the temporary workload is destroyed,
// and the run ends SUCCESS in the database.
func TestQueuedDrillRunsEndToEnd(t *testing.T) {
	f := newQueueFixture(t, nil)

	// Queued before any worker exists, so the queue can be read in the one
	// state that is otherwise a race to observe.
	runID := f.trigger(t)

	status, body := f.get(t, "/api/v1/queue")
	if status != http.StatusOK {
		t.Fatalf("GET /queue = %d: %s", status, body)
	}
	if !strings.Contains(body, runID) {
		t.Errorf("the queue does not show the drill that is waiting in it: %s", body)
	}

	f.startWorker(t, "worker-e2e", nil)
	run := waitSettled(t, f.history, runID)

	if run.State != core.RunSuccess || run.Result != core.ResultSuccess {
		t.Fatalf("state/result = %s/%s (%s), want SUCCESS", run.State, run.Result, run.Err)
	}
	if run.TempWorkloadID != tempVMID {
		t.Errorf("TempWorkloadID = %q, want the temporary id %q the cluster handed out", run.TempWorkloadID, tempVMID)
	}
	if run.Node != node {
		t.Errorf("Node = %q, want %q", run.Node, node)
	}
	if run.Backup == nil || run.Backup.ID != backupVolid {
		t.Errorf("Backup = %+v, want the volid discovered from the storage", run.Backup)
	}
	if run.RTO <= 0 {
		t.Error("RTO must be measured for a drill triggered over HTTP too")
	}
	if !run.CleanupDone {
		t.Error("CleanupDone = false: the temporary workload was not torn down")
	}
	if len(run.Checks) != 1 || !run.Checks[0].OK() {
		t.Errorf("checks = %+v, want the one in-guest check to have passed", run.Checks)
	}
	if len(run.Steps) == 0 {
		t.Error("the run recorded no steps: its timeline is what a report is made of")
	}

	// The cluster is as it was, minus nothing and plus nothing.
	if hasVM(f.pve, tempVMID) {
		t.Error("the temporary workload is still on the cluster after a successful drill")
	}
	if !hasVM(f.pve, sourceVMID) {
		t.Fatal("the SOURCE workload was destroyed - this is the worst possible bug")
	}
	if n := countRestores(f.pve); n != 1 {
		t.Errorf("the cluster saw %d restores, want exactly 1", n)
	}
	assertNoDestructiveCallOnSource(t, f.pve)
	assertHardened(t, f.pve)
	assertRestoreParams(t, f.pve)

	// And the drill it just ran is readable over the API that triggered it.
	status, body = f.get(t, "/api/v1/recovery-runs/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET the finished run = %d: %s", status, body)
	}
	for _, want := range []string{runID, "SUCCESS"} {
		if !strings.Contains(body, want) {
			t.Errorf("the finished run does not carry %q: %s", want, body)
		}
	}
}

// The stream a dashboard uses, against a real run.
func TestSSEFollowsARealDrillToItsEnd(t *testing.T) {
	f := newQueueFixture(t, nil)
	runID := f.trigger(t)

	// The stream is opened before the worker starts, so it follows the drill
	// rather than reading its transcript afterwards.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.server.URL+"/api/v1/recovery-runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.read)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	f.startWorker(t, "worker-sse", nil)

	var (
		ids       []int64
		messages  []string
		doneState string
		lastID    int64
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			n, perr := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if perr != nil {
				t.Fatalf("the stream sent an unusable event id %q", line)
			}
			ids = append(ids, n)
			lastID = n

		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			var ev struct {
				Seq     int64  `json:"seq"`
				State   string `json:"state"`
				Step    string `json:"step"`
				Message string `json:"message"`
			}
			var end struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal([]byte(payload), &ev); err == nil && ev.Seq > 0 {
				if ev.Seq != lastID {
					t.Errorf("frame id %d carries seq %d: Last-Event-ID would resume in the wrong place", lastID, ev.Seq)
				}
				messages = append(messages, ev.Step+" "+ev.Message)
				continue
			}
			if err := json.Unmarshal([]byte(payload), &end); err == nil {
				doneState = end.State
			}
		}
		if doneState != "" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	if doneState != string(core.RunSuccess) {
		t.Fatalf("the stream ended on state %q, want SUCCESS (frames: %v)", doneState, messages)
	}
	if len(ids) < 3 {
		t.Fatalf("the stream carried %d progress frames, want a timeline: %v", len(ids), messages)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("event ids are not increasing: %v", ids)
		}
	}

	var sawRestore bool
	for _, m := range messages {
		if strings.Contains(m, "restore") {
			sawRestore = true
		}
	}
	if !sawRestore {
		t.Errorf("the stream never mentioned the restore: %v", messages)
	}

	// The stream is a view of the stored journal, not a second source of
	// truth: what a dashboard saw live must be exactly what a reconnecting
	// one replays.
	stored, err := f.history.Events(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != len(ids) {
		t.Errorf("the stream sent %d frames, the journal holds %d events", len(ids), len(stored))
	}
}

// Cancel mid-drill: the run ends CANCELLED, and - the part that matters - the
// temporary workload is gone from the simulated cluster.
func TestCancellingMidDrillStillDestroysTheTemporaryWorkload(t *testing.T) {
	gate := startGate()
	f := newQueueFixture(t, gate)
	f.startWorker(t, "worker-cancel", nil)

	runID := f.trigger(t)

	// Frozen with the clone already restored onto the isolated bridge: this
	// is the only moment at which a cancellation has anything to clean up.
	gate.waitReached(t)
	if !hasVM(f.pve, tempVMID) {
		t.Fatal("the gate opened before the temporary workload existed; the test would prove nothing")
	}

	status, body := f.do(t, http.MethodPost, "/api/v1/recovery-runs/"+runID+"/cancel", f.operate, "")
	if status != http.StatusAccepted {
		t.Fatalf("cancel = %d: %s\nwant 202: a worker holds this drill and has to tear it down first",
			status, body)
	}

	run := waitSettled(t, f.history, runID)
	if run.State != core.RunCancelled {
		t.Errorf("state = %s (%s), want CANCELLED: stopping a drill is a decision, not a failed backup",
			run.State, run.Err)
	}
	if run.Result != "" {
		t.Errorf("result = %q, want empty: a cancelled drill graded nothing", run.Result)
	}

	// The part that matters.
	waitUntil(t, "the temporary workload to be destroyed", func() bool {
		return !hasVM(f.pve, tempVMID)
	})
	if n := countRequests(f.pve, http.MethodDelete, tempWorkloadPath); n == 0 {
		t.Error("the cancelled drill never asked the cluster to destroy its clone")
	}
	if !hasVM(f.pve, sourceVMID) {
		t.Fatal("the SOURCE workload was destroyed")
	}
	assertNoDestructiveCallOnSource(t, f.pve)
}

// The crash. A worker is stopped hard while a drill is in flight; a second
// worker starts and reconciles.
//
// Assert, in this order of importance:
//  1. no second Restore was ever issued for that run
//  2. the temporary workload was deleted, by its temporary id
//  3. the run is FAILED and names what happened
func TestAnInterruptedDrillIsFailedAndCleanedButNeverReplayed(t *testing.T) {
	gate := startGate()
	f := newQueueFixture(t, gate)

	// The worker that dies. Its lease is short and it never renews it -
	// RenewEvery is an hour - which is exactly what a process killed with
	// SIGKILL looks like from the database: a claim that stops being
	// refreshed, and a drill that never comes back to settle itself.
	stopDead := f.startWorker(t, "worker-that-dies", func(o *worker.Options) {
		o.Lease = 200 * time.Millisecond
		o.RenewEvery = time.Hour
	})
	// It is frozen inside the cluster for the whole test, so it has to be
	// released before it can be stopped - and before the deferred cleanups
	// try to close a server that is still serving it.
	defer func() {
		gate.open()
		stopDead()
	}()

	runID := f.trigger(t)

	// Dead with a machine on the cluster and the row saying so. Both halves
	// matter: the temporary id is written the moment it exists precisely so
	// that a worker which never comes back still leaves a way to find what it
	// made.
	gate.waitReached(t)
	waitUntil(t, "the temporary workload to be recorded", func() bool {
		run, err := f.history.GetRun(context.Background(), runID)
		return err == nil && run.TempWorkloadID == tempVMID
	})
	if !hasVM(f.pve, tempVMID) {
		t.Fatal("the drill was frozen before it created anything; the test would prove nothing")
	}

	restoresBefore := countRestores(f.pve)
	if restoresBefore != 1 {
		t.Fatalf("the interrupted drill issued %d restores before dying, want 1", restoresBefore)
	}

	// The worker that lives. It reconciles at startup and every lease after
	// that, and it must never re-run anything.
	f.startWorker(t, "worker-that-lives", func(o *worker.Options) {
		o.Lease = 150 * time.Millisecond
	})

	run := waitSettled(t, f.history, runID)

	// 1. The invariant of the whole phase, and it is an absence: reconciling
	//    an interrupted drill must not restore a second time. A replay would
	//    allocate a second temporary id, restore over a cluster that already
	//    has one clone of this workload, and leave the first behind.
	if n := countRestores(f.pve); n != 1 {
		t.Fatalf("the cluster saw %d restores: an interrupted drill was replayed", n)
	}
	// An absence has to stay absent. Keep the living worker polling well past
	// its own reconciliation tick and look again.
	settledAt := time.Now()
	for time.Since(settledAt) < 500*time.Millisecond {
		if n := countRestores(f.pve); n != 1 {
			t.Fatalf("the cluster saw %d restores after reconciliation: the run was replayed", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// And nothing can ever revive it: the claim's WHERE clause, not a
	// convention.
	if _, err := f.history.ClaimRun(context.Background(), "worker-that-tries",
		time.Minute, time.Now()); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("an interrupted run was claimable again: %v", err)
	}

	// 2. What the dead worker left behind is gone, destroyed by the id the
	//    row recorded for it.
	if n := countRequests(f.pve, http.MethodDelete, tempWorkloadPath); n == 0 {
		t.Errorf("nothing ever asked the cluster to destroy %s: the clone is orphaned", tempVMID)
	}
	if hasVM(f.pve, tempVMID) {
		t.Errorf("the temporary workload %s is still on the cluster after reconciliation", tempVMID)
	}

	// 3. The run says what happened, in words an operator can act on.
	if run.State != core.RunFailed {
		t.Errorf("state = %s, want FAILED: an interrupted drill is not a successful one", run.State)
	}
	if !strings.Contains(run.Err, "interrupted") {
		t.Errorf("Err = %q, want it to say the drill was interrupted", run.Err)
	}
	if !hasVM(f.pve, sourceVMID) {
		t.Fatal("the SOURCE workload was destroyed")
	}
	assertNoDestructiveCallOnSource(t, f.pve)
}

// One drill per workload, proven against the real path rather than the store.
func TestASecondTriggerForTheSameWorkloadIsRefused(t *testing.T) {
	gate := startGate()
	f := newQueueFixture(t, gate)
	stop := f.startWorker(t, "worker-once", nil)

	first := f.trigger(t)
	gate.waitReached(t)

	// A dashboard double-click, while the first drill is genuinely running.
	status, body := f.do(t, http.MethodPost, "/api/v1/recovery-runs", f.operate, triggerBody())
	if status != http.StatusConflict {
		t.Fatalf("the second trigger = %d: %s\nwant 409: one drill per workload at a time", status, body)
	}
	if !strings.Contains(body, first) {
		t.Errorf("the refusal does not name the drill already in flight: %s", body)
	}

	gate.open()
	run := waitSettled(t, f.history, first)
	if run.State != core.RunSuccess {
		t.Fatalf("state = %s (%s), want SUCCESS", run.State, run.Err)
	}

	if n := countRestores(f.pve); n != 1 {
		t.Fatalf("the cluster saw %d restores: the refused trigger reached it anyway", n)
	}
	runs, err := f.history.ListRuns(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("the history holds %d runs, want 1: the refusal must not have queued a row", len(runs))
	}

	// The refusal is about a drill in flight, not a workload struck off for
	// good. With the worker stopped, the row stays queued and runs nothing.
	stop()
	if status, body := f.do(t, http.MethodPost, "/api/v1/recovery-runs", f.operate, triggerBody()); status != http.StatusCreated {
		t.Fatalf("triggering again once the first drill finished = %d: %s\nwant 201", status, body)
	}
}

// And the invariant that survived B1: no response ever carries a secret, now
// including the write endpoints and their error bodies.
func TestWriteEndpointsNeverLeakSecrets(t *testing.T) {
	f := newQueueFixture(t, nil)

	// A real queued run, so the cancel paths have something to answer about.
	runID := f.trigger(t)

	type call struct {
		method string
		path   string
		token  string
		body   string
	}
	calls := []call{
		// The happy paths.
		{http.MethodGet, "/api/v1/queue", f.operate, ""},
		{http.MethodPost, "/api/v1/recovery-runs/" + runID + "/cancel", f.operate, ""},

		// Bodies that cannot become a drill.
		{http.MethodPost, "/api/v1/recovery-runs", f.operate, "not json at all"},
		{http.MethodPost, "/api/v1/recovery-runs", f.operate, `{}`},
		{http.MethodPost, "/api/v1/recovery-runs", f.operate, `{"workload_id":"101","rto_target":"soon"}`},
		{http.MethodPost, "/api/v1/recovery-runs", f.operate, `{"workload_id":"101","checks":["nonsense:"]}`},
		{http.MethodPost, "/api/v1/recovery-runs", f.operate, `{"workload_id":"101","provider":"nope"}`},

		// Cancelling what is finished, and what never existed.
		{http.MethodPost, "/api/v1/recovery-runs/" + runID + "/cancel", f.operate, ""},
		{http.MethodPost, "/api/v1/recovery-runs/zzzzzzzz/cancel", f.operate, ""},

		// Cleanup: refused up front, refused by the cluster, and asked of a
		// provider that does not exist.
		{http.MethodPost, "/api/v1/cleanup/" + sourceVMID, f.operate, ""},
		{http.MethodPost, "/api/v1/cleanup/notanumber", f.operate, ""},
		{http.MethodPost, "/api/v1/cleanup/" + tempVMID, f.operate, ""},
		{http.MethodPost, "/api/v1/cleanup/" + tempVMID + "?provider=nope", f.operate, ""},

		// A token that may read but not operate: the 403 bodies.
		{http.MethodPost, "/api/v1/recovery-runs", f.read, triggerBody()},
		{http.MethodPost, "/api/v1/recovery-runs/" + runID + "/cancel", f.read, ""},
		{http.MethodPost, "/api/v1/cleanup/" + tempVMID, f.read, ""},

		// And no token at all: the 401 bodies.
		{http.MethodPost, "/api/v1/recovery-runs", "", triggerBody()},
		{http.MethodPost, "/api/v1/recovery-runs/" + runID + "/cancel", "", ""},
		{http.MethodPost, "/api/v1/cleanup/" + tempVMID, "", ""},
	}

	forbidden := map[string]string{
		f.operate:           "the caller's own API token",
		f.read:              "the read-only API token",
		leakDBPassword:      "the history database password",
		leakProviderSecret:  "the provider secret",
		leakProviderTokenID: "the provider token id, which is half a credential",
	}

	for _, c := range calls {
		// Every body, whatever the status: a success is swept exactly like a
		// refusal, because the endpoints that answer 201 render a run and the
		// ones that answer 4xx render a problem document, and either could
		// carry something sealed.
		status, body := f.do(t, c.method, c.path, c.token, c.body)
		for secret, what := range forbidden {
			if strings.Contains(body, secret) {
				t.Errorf("%s %s (%d) leaked %s:\n%s", c.method, c.path, status, what, body)
			}
		}
	}

	// The read-only token must genuinely have been refused, or the sweep
	// above would be walking bodies nobody would ever see.
	if status, body := f.do(t, http.MethodPost, "/api/v1/recovery-runs", f.read, triggerBody()); status != http.StatusForbidden {
		t.Fatalf("a read-only token triggered a drill: %d %s", status, body)
	}
	if status, _ := f.do(t, http.MethodPost, "/api/v1/recovery-runs", "", triggerBody()); status != http.StatusUnauthorized {
		t.Fatalf("an anonymous caller reached the trigger endpoint: %d", status)
	}

	// Nothing above may have reached the cluster with a machine to make.
	if n := countRestores(f.pve); n != 0 {
		t.Fatalf("the leak sweep caused %d restores: an HTTP handler executed a drill", n)
	}
}
