package e2e

// The drill is only half the product. This file answers the other half: once
// a drill has run, is it still there tomorrow, and does it say the same thing?

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/recovery"
	"github.com/restorelab/restorelab/internal/store"
)

// newTestHistory gives a real, migrated SQLite history in a throwaway
// directory — the same engine a user gets, with nothing installed.
func newTestHistory(t *testing.T) store.Store {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mirror is the six lines internal/cli's recorder does, reproduced here
// because that recorder lives in the cli package and importing it would tie
// this test to a command line it does not otherwise touch.
type mirror struct {
	store store.Store
	seq   int64
	runID string
}

func (m *mirror) emit(t *testing.T, run *core.RecoveryRun, e recovery.Event) {
	t.Helper()
	ctx := context.Background()

	if m.runID == "" && e.RunID != "" {
		m.runID = e.RunID
		if err := m.store.CreateRun(ctx, &core.RecoveryRun{
			ID: e.RunID, PlanName: run.PlanName, ProviderID: run.ProviderID,
			SourceWorkloadID: run.SourceWorkloadID, State: e.State, StartedAt: e.At,
		}, "name: e2e\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}
	if m.runID == "" {
		return
	}

	m.seq++
	if err := m.store.AppendEvent(ctx, m.runID, store.Event{
		Seq: m.seq, At: e.At, State: e.State, Step: e.Step,
		Status: e.Status, Message: e.Message, Check: e.Check, Err: e.Err,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func (m *mirror) finish(t *testing.T, run *core.RecoveryRun) {
	t.Helper()
	ctx := context.Background()

	if err := m.store.UpdateRun(ctx, run); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	for i, step := range run.Steps {
		if err := m.store.SaveStep(ctx, run.ID, i, step); err != nil {
			t.Fatalf("SaveStep %d: %v", i, err)
		}
	}
	for i, check := range run.Checks {
		if err := m.store.SaveCheck(ctx, run.ID, i, check); err != nil {
			t.Fatalf("SaveCheck %d: %v", i, err)
		}
	}
}

// A run read back out of the history must be the run the engine returned.
// Anything less and the report a user reads a month later is fiction.
func TestADrillIsReadableBackFromHistory(t *testing.T) {
	_, port := listenTCP(t)
	_, provider := newDrill(t, guestAddr)
	history := newTestHistory(t)

	var events []recovery.Event
	engine := newEngine(t, provider, &events)

	run, err := engine.Run(context.Background(), drillPlan(port, 10*time.Minute), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.Result != core.ResultSuccess {
		t.Fatalf("Result = %s, want SUCCESS: %s", run.Result, run.Err)
	}

	m := &mirror{store: history}
	for _, e := range events {
		m.emit(t, run, e)
	}
	m.finish(t, run)

	got, err := history.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.Result != run.Result || got.State != run.State {
		t.Errorf("stored State/Result = %v/%v, want %v/%v", got.State, got.Result, run.State, run.Result)
	}
	// Durations are kept in milliseconds. An RTO is a number of seconds and
	// the report renders milliseconds, so finer resolution would be storage
	// spent on digits nobody reads - but the truncation has to be exactly
	// that, and nothing more.
	if got.RTO != run.RTO.Truncate(time.Millisecond) {
		t.Errorf("stored RTO = %v, want %v", got.RTO, run.RTO.Truncate(time.Millisecond))
	}
	if got.TempWorkloadID != run.TempWorkloadID {
		t.Errorf("stored TempWorkloadID = %q, want %q", got.TempWorkloadID, run.TempWorkloadID)
	}
	if got.CleanupDone != run.CleanupDone {
		t.Errorf("stored CleanupDone = %v, want %v", got.CleanupDone, run.CleanupDone)
	}
	if len(got.Steps) != len(run.Steps) {
		t.Fatalf("stored %d steps, want %d", len(got.Steps), len(run.Steps))
	}
	for i := range run.Steps {
		if got.Steps[i].Name != run.Steps[i].Name || got.Steps[i].Status != run.Steps[i].Status {
			t.Errorf("step %d = %s/%s, want %s/%s", i,
				got.Steps[i].Name, got.Steps[i].Status, run.Steps[i].Name, run.Steps[i].Status)
		}
		if want := run.Steps[i].Duration.Truncate(time.Millisecond); got.Steps[i].Duration != want {
			t.Errorf("step %d duration = %v, want %v", i, got.Steps[i].Duration, want)
		}
	}
	if len(got.Checks) != len(run.Checks) {
		t.Fatalf("stored %d checks, want %d", len(got.Checks), len(run.Checks))
	}
	for i := range run.Checks {
		if got.Checks[i].Name != run.Checks[i].Name || got.Checks[i].Status != run.Checks[i].Status {
			t.Errorf("check %d = %s/%s, want %s/%s", i,
				got.Checks[i].Name, got.Checks[i].Status, run.Checks[i].Name, run.Checks[i].Status)
		}
	}
}

// The event stream is stored whole and in order, because phase B's SSE
// replays a reconnecting browser from it.
func TestTheEventStreamSurvivesInHistory(t *testing.T) {
	_, port := listenTCP(t)
	_, provider := newDrill(t, guestAddr)
	history := newTestHistory(t)

	var events []recovery.Event
	engine := newEngine(t, provider, &events)

	run, err := engine.Run(context.Background(), drillPlan(port, 10*time.Minute), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	m := &mirror{store: history}
	for _, e := range events {
		m.emit(t, run, e)
	}

	stored, err := history.Events(context.Background(), run.ID, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != len(events) {
		t.Fatalf("stored %d events, want the %d the engine emitted", len(stored), len(events))
	}
	for i := range stored {
		if stored[i].Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, stored[i].Seq, i+1)
		}
		if stored[i].Message != events[i].Message {
			t.Errorf("event %d message = %q, want %q", i, stored[i].Message, events[i].Message)
		}
	}
}

// A failed drill is exactly the one worth looking up later, so it has to be
// recorded as fully as a successful one - including why it failed.
func TestAFailedDrillIsRecordedWithItsReason(t *testing.T) {
	_, port := listenTCP(t)
	// A port nothing listens on: the service check will fail.
	_, deadPort := listenTCP(t)
	_ = port
	_, provider := newDrill(t, guestAddr)
	history := newTestHistory(t)

	var events []recovery.Event
	engine := newEngine(t, provider, &events)

	run, _ := engine.Run(context.Background(), drillPlan(deadPort+1, 10*time.Minute), recovery.RunOptions{
		Network: isolatedNetwork(),
		Node:    node,
	})
	if run == nil {
		t.Fatal("a failed run must still be returned: the report matters most when it failed")
	}

	m := &mirror{store: history}
	for _, e := range events {
		m.emit(t, run, e)
	}
	m.finish(t, run)

	got, err := history.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Result != run.Result {
		t.Errorf("stored Result = %v, want %v", got.Result, run.Result)
	}
	if got.Err != run.Err {
		t.Errorf("stored Err = %q, want %q", got.Err, run.Err)
	}
	// And it must be findable without knowing its id.
	listed, err := history.ListRuns(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("the failed drill is not in the listing: %+v", listed)
	}
}
