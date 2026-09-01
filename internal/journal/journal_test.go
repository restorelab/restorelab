package journal

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/recovery"
	"github.com/restorelab/restorelab/internal/store"
)

var errBroken = errors.New("disk is full")

// brokenStore fails every single call, the way a full disk, a locked file or
// a corrupt database would.
type brokenStore struct{ calls int }

func (b *brokenStore) CreateRun(context.Context, *core.RecoveryRun, string) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) UpdateRun(context.Context, *core.RecoveryRun) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) SetTempWorkload(context.Context, string, string, string) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) SaveStep(context.Context, string, int, core.Step) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) SaveCheck(context.Context, string, int, core.CheckResult) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) AppendEvent(context.Context, string, store.Event) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) GetRun(context.Context, string) (*core.RecoveryRun, error) {
	return nil, errBroken
}
func (b *brokenStore) ListRuns(context.Context, store.Filter) ([]store.RunSummary, error) {
	return nil, errBroken
}
func (b *brokenStore) Events(context.Context, string, int64) ([]store.Event, error) {
	return nil, errBroken
}
func (b *brokenStore) CreateToken(context.Context, store.APIToken) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) TokenByHash(context.Context, string) (*store.APIToken, error) {
	return nil, errBroken
}
func (b *brokenStore) ListTokens(context.Context) ([]store.APIToken, error) {
	return nil, errBroken
}
func (b *brokenStore) RevokeToken(context.Context, string, time.Time) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) TouchToken(context.Context, string, time.Time) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) Enqueue(context.Context, *core.RecoveryRun, string, time.Time) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) SetState(context.Context, string, core.RunState) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) SetRunError(context.Context, string, string) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) RequestCancel(context.Context, string, time.Time) (bool, error) {
	b.calls++
	return false, errBroken
}
func (b *brokenStore) CancelRequested(context.Context, string) (bool, error) {
	return false, errBroken
}
func (b *brokenStore) ActiveRunForWorkload(context.Context, string) (string, error) {
	return "", errBroken
}
func (b *brokenStore) ClaimRun(context.Context, string, time.Duration, time.Time) (*store.QueuedRun, error) {
	return nil, errBroken
}
func (b *brokenStore) RenewLease(context.Context, string, string, time.Time) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) FinishLease(context.Context, string) error {
	b.calls++
	return errBroken
}
func (b *brokenStore) StaleRuns(context.Context, time.Time) ([]store.QueuedRun, error) {
	return nil, errBroken
}
func (b *brokenStore) RunLease(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, errBroken
}
func (b *brokenStore) Describe() string { return "broken" }
func (b *brokenStore) Close() error     { return errBroken }

var _ store.Store = (*brokenStore)(nil)

// tempWorkloadCall records one SetTempWorkload invocation seen by spyStore.
type tempWorkloadCall struct {
	runID          string
	tempWorkloadID string
	node           string
}

// spyStore records what it was asked to write.
type spyStore struct {
	store.Noop
	events        []store.Event
	steps         []core.Step
	checks        []core.CheckResult
	runs          []*core.RecoveryRun
	plans         []string
	tempWorkloads []tempWorkloadCall
}

func (s *spyStore) CreateRun(_ context.Context, run *core.RecoveryRun, planYAML string) error {
	s.runs = append(s.runs, run)
	s.plans = append(s.plans, planYAML)
	return nil
}
func (s *spyStore) AppendEvent(_ context.Context, _ string, ev store.Event) error {
	s.events = append(s.events, ev)
	return nil
}
func (s *spyStore) SaveStep(_ context.Context, _ string, _ int, step core.Step) error {
	s.steps = append(s.steps, step)
	return nil
}
func (s *spyStore) SaveCheck(_ context.Context, _ string, _ int, check core.CheckResult) error {
	s.checks = append(s.checks, check)
	return nil
}
func (s *spyStore) SetTempWorkload(_ context.Context, runID, tempWorkloadID, node string) error {
	s.tempWorkloads = append(s.tempWorkloads, tempWorkloadCall{runID: runID, tempWorkloadID: tempWorkloadID, node: node})
	return nil
}

func quietRecorder(s store.Store) *Recorder {
	return New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The guarantee this whole design rests on: a store that fails every call must
// not surface a single error to the caller. A drill is destructive work on a
// production cluster; the journal does not command the operation.
//
// The recorder's methods return nothing at all, so this test is really about
// keeping that true — if someone adds an error return, this stops compiling.
func TestRecorderSwallowsEveryStoreFailure(t *testing.T) {
	broken := &brokenStore{}
	rec := quietRecorder(broken)
	ctx := context.Background()
	now := time.Now().UTC()

	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: adhoc-110\n")
	rec.Emit(recovery.Event{RunID: "abc", At: now, State: core.RunRestoring, Step: "restore", Status: core.StepRunning, Message: "restoring"})
	rec.Emit(recovery.Event{RunID: "abc", At: now, State: core.RunRestoring, Step: "restore", Status: core.StepDone, Message: "done"})
	rec.Emit(recovery.Event{RunID: "abc", At: now, State: core.RunRunningChecks,
		Check: &core.CheckResult{Name: "COMMAND", Type: "command", Status: core.CheckPass}})

	run := &core.RecoveryRun{
		ID: "abc", PlanName: "adhoc-110", StartedAt: now,
		State: core.RunSuccess, Result: core.ResultSuccess,
		Steps:  []core.Step{{Name: "restore", Status: core.StepDone}},
		Checks: []core.CheckResult{{Name: "COMMAND", Status: core.CheckPass}},
	}
	rec.Finish(ctx, run)

	if broken.calls == 0 {
		t.Fatal("the recorder never tried to write; this test would prove nothing")
	}
}

// Every event must reach the store, numbered in emission order — that number
// is what phase B's SSE replays from.
func TestRecorderNumbersEventsInEmissionOrder(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	for i := 0; i < 5; i++ {
		rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunRestoring})
	}

	if len(spy.events) != 5 {
		t.Fatalf("store received %d events, want 5", len(spy.events))
	}
	for i, ev := range spy.events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, i+1)
		}
	}
}

// The run row has to exist before an event can reference it, and the plan is
// snapshotted at that moment.
func TestRecorderCreatesTheRunOnTheFirstEvent(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: adhoc-110\n")

	rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunDiscoveringBackup})
	rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunRestoring})

	if len(spy.runs) != 1 {
		t.Fatalf("store received %d CreateRun calls, want exactly 1", len(spy.runs))
	}
	if spy.runs[0].ID != "abc" || spy.runs[0].SourceWorkloadID != "110" {
		t.Errorf("created run = %+v, want id abc for workload 110", spy.runs[0])
	}
	if spy.plans[0] != "name: adhoc-110\n" {
		t.Errorf("plan snapshot = %q, want the plan as it was at the start", spy.plans[0])
	}
}

// An event with no run id yet cannot be attached to anything, and must not
// create a row with an empty id.
func TestRecorderIgnoresEventsBeforeARunIDExists(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	rec.Emit(recovery.Event{At: time.Now().UTC(), State: core.RunQueued})

	if len(spy.runs) != 0 || len(spy.events) != 0 {
		t.Fatalf("an event with no run id was recorded: runs=%d events=%d", len(spy.runs), len(spy.events))
	}
}

// A check event must also land in run_checks: the report is built from the
// checks, not from the event stream.
func TestRecorderSavesCheckResults(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunRunningChecks,
		Check: &core.CheckResult{Name: "COMMAND", Status: core.CheckPass}})

	if len(spy.checks) != 1 {
		t.Fatalf("store received %d checks, want 1", len(spy.checks))
	}
	if spy.checks[0].Name != "COMMAND" {
		t.Errorf("check = %+v, want COMMAND", spy.checks[0])
	}
}

// Finish writes the engine's authoritative timeline, which only exists on the
// finished run.
func TestRecorderFinishWritesTheTimeline(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")
	rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunRestoring})

	run := &core.RecoveryRun{
		ID: "abc", State: core.RunSuccess, Result: core.ResultSuccess,
		Steps: []core.Step{
			{Name: "discover_backup", Status: core.StepDone, Duration: 445 * time.Millisecond},
			{Name: "restore", Status: core.StepDone, Duration: 4700 * time.Millisecond},
		},
		Checks: []core.CheckResult{{Name: "COMMAND", Status: core.CheckPass}},
	}
	rec.Finish(context.Background(), run)

	if len(spy.steps) != 2 {
		t.Fatalf("store received %d steps, want 2", len(spy.steps))
	}
	if spy.steps[1].Duration != 4700*time.Millisecond {
		t.Errorf("step durations were lost: %+v", spy.steps)
	}
}

// A run that failed before emitting anything still deserves a record: that is
// exactly the failure someone will want to look up later.
func TestRecorderFinishRecordsARunThatNeverEmitted(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	run := &core.RecoveryRun{
		ID: "abc", State: core.RunFailed, Result: core.ResultFailed,
		Err: "no backup found for workload 110", StartedAt: time.Now().UTC(),
	}
	rec.Finish(context.Background(), run)

	if len(spy.runs) != 1 {
		t.Fatalf("store received %d CreateRun calls, want 1: a failure before the first event is still history", len(spy.runs))
	}
}

func TestRecorderFinishOnANilRunDoesNothing(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)

	rec.Finish(context.Background(), nil)

	if len(spy.runs) != 0 {
		t.Fatalf("a nil run was recorded: %+v", spy.runs)
	}
}

// An event carrying the identity of the temporary workload must reach the
// store before that workload is ever created on the cluster — that ordering
// is the whole point of TempWorkloadID/Node existing on Event at all.
func TestRecorderRecordsTempWorkloadAsSoonAsItIsKnown(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	rec.Emit(recovery.Event{
		RunID: "abc", At: time.Now().UTC(), State: core.RunRestoring,
		TempWorkloadID: "9099", Node: "private-other",
	})

	if len(spy.tempWorkloads) != 1 {
		t.Fatalf("store received %d SetTempWorkload calls, want 1", len(spy.tempWorkloads))
	}
	got := spy.tempWorkloads[0]
	if got.runID != "abc" || got.tempWorkloadID != "9099" || got.node != "private-other" {
		t.Errorf("SetTempWorkload call = %+v, want {abc 9099 private-other}", got)
	}
}

// The engine repeats TempWorkloadID/Node on every event once they are known,
// so the recorder must not write them to the store more than once per run.
func TestRecorderWritesTempWorkloadOnlyOnce(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	for i := 0; i < 5; i++ {
		rec.Emit(recovery.Event{
			RunID: "abc", At: time.Now().UTC(), State: core.RunRestoring,
			TempWorkloadID: "9099", Node: "private-other",
		})
	}

	if len(spy.tempWorkloads) != 1 {
		t.Fatalf("store received %d SetTempWorkload calls, want exactly 1", len(spy.tempWorkloads))
	}
}

// The events emitted before prepare_environment has allocated an identity
// carry no TempWorkloadID, and must not trigger a write at all.
func TestRecorderSkipsTempWorkloadWhenNotYetAllocated(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunDiscoveringBackup})

	if len(spy.tempWorkloads) != 0 {
		t.Fatalf("store received %d SetTempWorkload calls, want 0: no identity was allocated yet", len(spy.tempWorkloads))
	}
}

// The same guarantee as TestRecorderSwallowsEveryStoreFailure, but focused on
// SetTempWorkload: a store that fails it must not surface anything to the
// caller, and the drill carries on.
func TestRecorderSwallowsTempWorkloadFailure(t *testing.T) {
	broken := &brokenStore{}
	rec := quietRecorder(broken)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: adhoc-110\n")

	rec.Emit(recovery.Event{
		RunID: "abc", At: time.Now().UTC(), State: core.RunRestoring,
		TempWorkloadID: "9099", Node: "private-other",
	})

	if broken.calls == 0 {
		t.Fatal("the recorder never tried to write; this test would prove nothing")
	}
}

// A run queued through the API already has its row: the API wrote it when it
// accepted the request, and it holds the plan snapshot as it was then.
// Creating it again would be a duplicate key at best, and at worst would
// overwrite the queued row with a fresh one that has forgotten it was queued.
func TestRecorderAttachedToAnExistingRunDoesNotCreateIt(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.AttachTo("queued-1")

	rec.Emit(recovery.Event{RunID: "queued-1", At: time.Now().UTC(), State: core.RunRestoring})

	if len(spy.runs) != 0 {
		t.Fatalf("the journal created a run row that already existed: %+v", spy.runs)
	}
	if len(spy.events) != 1 {
		t.Fatalf("the event was not recorded: %d", len(spy.events))
	}
}

// And the CLI path is unchanged: with no AttachTo, the first event still
// creates the row.
func TestRecorderWithoutAttachStillCreatesTheRun(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)
	rec.Prepare("adhoc-110", "proxmox-main", "110", "linux-test", "name: x\n")

	rec.Emit(recovery.Event{RunID: "abc", At: time.Now().UTC(), State: core.RunDiscoveringBackup})

	if len(spy.runs) != 1 {
		t.Fatalf("CreateRun calls = %d, want 1", len(spy.runs))
	}
}
