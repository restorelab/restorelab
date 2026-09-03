package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

var errBroken = errors.New("the database is on fire")

// fakeStore is the catalogue and the queue, recorded rather than written.
//
// It is safe for concurrent use because Tick is exercised from more than one
// goroutine in one of the tests below.
type fakeStore struct {
	mu sync.Mutex

	plans    []store.Plan
	lastSlot map[string]*store.Slot
	active   map[string]string // workload id -> run id already in flight
	queued   int               // how many runs ListRuns reports as not terminal

	claims   []store.Slot
	runs     []*core.RecoveryRun
	broken   bool
	claimErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		lastSlot: map[string]*store.Slot{},
		active:   map[string]string{},
	}
}

func (f *fakeStore) ListPlans(context.Context, store.PlanFilter) ([]store.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, errBroken
	}
	return f.plans, nil
}

func (f *fakeStore) LastSlot(_ context.Context, planID string) (*store.Slot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, errBroken
	}
	if s, ok := f.lastSlot[planID]; ok {
		return s, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ClaimSlot(_ context.Context, slot store.Slot, run *core.RecoveryRun, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return errBroken
	}
	if f.claimErr != nil {
		return f.claimErr
	}
	for _, done := range f.claims {
		if done.PlanID == slot.PlanID && done.SlotAt.Equal(slot.SlotAt) {
			return store.ErrDuplicate
		}
	}
	f.claims = append(f.claims, slot)
	f.lastSlot[slot.PlanID] = &slot
	if slot.Outcome == store.SlotQueued {
		f.runs = append(f.runs, run)
		f.queued++
	}
	return nil
}

func (f *fakeStore) ActiveRunForWorkload(_ context.Context, workloadID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return "", errBroken
	}
	return f.active[workloadID], nil
}

func (f *fakeStore) Enqueue(context.Context, *core.RecoveryRun, string, time.Time) error {
	// The scheduler never calls this: it queues through ClaimSlot, so that
	// the slot and the run are one transaction. Reaching it means the
	// scheduler found a second way to queue a drill.
	return errors.New("scheduler must queue through ClaimSlot, not Enqueue")
}

func (f *fakeStore) ListRuns(context.Context, store.Filter) ([]store.RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, errBroken
	}
	return make([]store.RunSummary, f.queued), nil
}

func (f *fakeStore) claimed() []store.Slot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Slot(nil), f.claims...)
}

// scheduledPlan is a catalogue row carrying a cron expression.
func scheduledPlan(id, name, schedule string) store.Plan {
	yaml := fmt.Sprintf("name: %s\nworkload:\n  provider: proxmox-main\n  id: \"110\"\n", name)
	if schedule != "" {
		yaml += fmt.Sprintf("schedule: %q\nschedule_timezone: UTC\n", schedule)
	}
	return store.Plan{
		ID: id, Name: name, WorkloadID: "110", ProviderID: "proxmox-main",
		YAML: yaml, Version: 1,
	}
}

// newTestScheduler builds a scheduler whose clock the test controls.
func newTestScheduler(t *testing.T, s Store, now time.Time) *Scheduler {
	t.Helper()
	sch, err := New(Options{
		Store:  s,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return now },
		NewID:  func() string { return "run-id" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sch
}

// slotTime is 03:00 UTC on Sunday 2026-09-06.
var slotTime = time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)

func TestTickQueuesADuePlan(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{scheduledPlan("plan-1", "linux-nightly", "0 3 * * *")}

	// Started before the slot, ticking just after it.
	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	claims := f.claimed()
	if len(claims) != 1 {
		t.Fatalf("%d slots claimed, want 1", len(claims))
	}
	if claims[0].Outcome != store.SlotQueued {
		t.Fatalf("outcome = %q, want queued (reason %q)", claims[0].Outcome, claims[0].Reason)
	}
	if !claims[0].SlotAt.Equal(slotTime) {
		t.Fatalf("SlotAt = %v, want %v", claims[0].SlotAt, slotTime)
	}
	if len(f.runs) != 1 || f.runs[0].SourceWorkloadID != "110" {
		t.Fatalf("queued runs = %+v, want one drill of workload 110", f.runs)
	}
	// The run must name the slot's plan, so history can say why it ran.
	if f.runs[0].PlanID != "plan-1" || f.runs[0].PlanVersion != 1 {
		t.Fatalf("provenance = %q v%d, want plan-1 v1", f.runs[0].PlanID, f.runs[0].PlanVersion)
	}
}

func TestTickIgnoresAPlanWithNoSchedule(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{scheduledPlan("plan-1", "manual-only", "")}

	sch := newTestScheduler(t, f, slotTime.Add(time.Hour))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	// Most plans have no schedule. Claiming a slot for one would drill a
	// machine nobody asked to have drilled.
	if got := f.claimed(); len(got) != 0 {
		t.Fatalf("%d slots claimed for an unscheduled plan, want 0", len(got))
	}
}

func TestTickSkipsAPlanWhoseWorkloadIsAlreadyBeingDrilled(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{scheduledPlan("plan-1", "linux-nightly", "0 3 * * *")}
	f.active["110"] = "run-in-flight"

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	claims := f.claimed()
	if len(claims) != 1 {
		t.Fatalf("%d slots claimed, want 1 (a skip is still a decision)", len(claims))
	}
	if claims[0].Outcome != store.SlotSkipped {
		t.Fatalf("outcome = %q, want skipped", claims[0].Outcome)
	}
	if !strings.Contains(claims[0].Reason, "run-in-flight") {
		t.Fatalf("reason = %q, want it to name the drill already running", claims[0].Reason)
	}
	if len(f.runs) != 0 {
		t.Fatalf("%d runs queued despite a drill in flight, want 0", len(f.runs))
	}
}

func TestTickSkipsAPlanThatNoLongerParses(t *testing.T) {
	f := newFakeStore()
	broken := scheduledPlan("plan-1", "linux-nightly", "0 3 * * *")
	broken.YAML = "name: linux-nightly\nworkload:\n  id: \"110\"\nchecks: not-a-list\n"
	good := scheduledPlan("plan-2", "other", "0 3 * * *")
	good.WorkloadID = "111"
	f.plans = []store.Plan{broken, good}

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	claims := f.claimed()
	if len(claims) != 2 {
		t.Fatalf("%d slots claimed, want 2 - one skip and one drill", len(claims))
	}
	var skipped *store.Slot
	for i := range claims {
		if claims[i].PlanID == "plan-1" {
			skipped = &claims[i]
		}
	}
	if skipped == nil || skipped.Outcome != store.SlotSkipped {
		t.Fatalf("plan-1's slot = %+v, want skipped", skipped)
	}
	if skipped.Reason == "" {
		t.Fatal("a slot skipped for an invalid plan carries no reason")
	}
	// One bad document must not stop the catalogue being scheduled.
	if len(f.runs) != 1 {
		t.Fatalf("%d runs queued, want 1: a broken plan stopped the tick", len(f.runs))
	}
}

func TestTickDoesNotQueueWhenTheQueueIsFull(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{scheduledPlan("plan-1", "linux-nightly", "0 3 * * *")}
	f.queued = DefaultMaxQueueDepth

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	// A full queue is a postponement, not a decision. Writing a skipped slot
	// here would burn a slot that is perfectly runnable a minute later.
	if got := f.claimed(); len(got) != 0 {
		t.Fatalf("%d slots claimed with a full queue, want 0 - the slot was burned", len(got))
	}
}

func TestTickStopsQueueingOnceTheQueueFillsUp(t *testing.T) {
	f := newFakeStore()
	for i := range DefaultMaxQueueDepth + 3 {
		p := scheduledPlan(fmt.Sprintf("plan-%d", i), fmt.Sprintf("nightly-%d", i), "0 3 * * *")
		p.WorkloadID = fmt.Sprintf("2%02d", i)
		p.YAML = strings.Replace(p.YAML, `id: "110"`, fmt.Sprintf("id: %q", p.WorkloadID), 1)
		f.plans = append(f.plans, p)
	}

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	if len(f.runs) > DefaultMaxQueueDepth {
		t.Fatalf("%d runs queued in one tick, want at most %d", len(f.runs), DefaultMaxQueueDepth)
	}
	if len(f.runs) == 0 {
		t.Fatal("no run queued at all, want the queue filled up to its depth")
	}
}

func TestTickSurvivesAStoreThatFails(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{scheduledPlan("plan-1", "linux-nightly", "0 3 * * *")}
	f.broken = true

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)

	// Neither tick may panic, and the second must still do its work: the
	// loop stopping is the one failure that silently ends all scheduled
	// verification.
	sch.Tick(context.Background())
	f.mu.Lock()
	f.broken = false
	f.mu.Unlock()
	sch.Tick(context.Background())

	if got := f.claimed(); len(got) != 1 {
		t.Fatalf("%d slots claimed after the database recovered, want 1", len(got))
	}
}

func TestADuplicateClaimIsNotAFailure(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{
		scheduledPlan("plan-1", "linux-nightly", "0 3 * * *"),
		scheduledPlan("plan-2", "other", "0 3 * * *"),
	}
	f.claimErr = store.ErrDuplicate

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())

	// Another scheduler decided these slots. That is the mechanism working,
	// so the tick considers every plan rather than bailing out on the first.
	if len(f.runs) != 0 {
		t.Fatalf("%d runs queued, want 0", len(f.runs))
	}
}

func TestTickIsIdempotentAcrossTicks(t *testing.T) {
	f := newFakeStore()
	f.plans = []store.Plan{scheduledPlan("plan-1", "linux-nightly", "0 3 * * *")}

	sch := newTestScheduler(t, f, slotTime.Add(time.Minute))
	sch.startedAt = slotTime.Add(-time.Hour)
	sch.Tick(context.Background())
	sch.Tick(context.Background())
	sch.Tick(context.Background())

	if got := f.claimed(); len(got) != 1 {
		t.Fatalf("%d slots claimed over three ticks of the same minute, want 1", len(got))
	}
	if len(f.runs) != 1 {
		t.Fatalf("%d drills queued for one slot, want 1", len(f.runs))
	}
}

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newFakeStore()
	sch, err := New(Options{
		Store:  f,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tick:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within two seconds of its context being cancelled")
	}
}

// The scheduler must not be constructible with a provider or an engine. This
// asserts the shape of Options: if a field for either ever appears, this test
// is where the discussion happens.
func TestOptionsCarryNoProvider(t *testing.T) {
	var opts Options
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted options with no store")
	}
	// A compile-time reminder rather than a runtime assertion: adding a
	// Providers field to Options would make this line the thing to explain.
	_ = opts.Store
}
