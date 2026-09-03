package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

// enqueued is one row Enqueue was handed: the run, the plan snapshot, and
// when it was queued.
type enqueued struct {
	run      *core.RecoveryRun
	planYAML string
	at       time.Time
}

// fakeQueue records what it was asked to write and answers the conflict
// check with whatever the test set. It writes nothing anywhere else: this
// package must be provable without a database.
type fakeQueue struct {
	active     string // what ActiveRunForWorkload answers
	activeErr  error
	enqueueErr error

	queued   []enqueued
	askedFor []string // the workload ids the conflict check was asked about
}

func (f *fakeQueue) ActiveRunForWorkload(_ context.Context, workloadID string) (string, error) {
	f.askedFor = append(f.askedFor, workloadID)
	return f.active, f.activeErr
}

func (f *fakeQueue) Enqueue(_ context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.queued = append(f.queued, enqueued{run: run, planYAML: planYAML, at: at})
	return nil
}

// testPlan is a plan as the caller hands it over: already parsed, defaulted
// and validated. This package never validates - by the time a drill is being
// queued, a plan that could not become one is already a 400.
func testPlan(workloadID, providerID string) *plan.Plan {
	return &plan.Plan{
		Name: "linux-nightly",
		Workload: plan.WorkloadRef{
			ID:       workloadID,
			Provider: providerID,
		},
		RTOTarget: plan.Duration(5 * time.Minute),
	}
}

func TestPrepareCarriesProvenanceFromAStoredPlan(t *testing.T) {
	q := &fakeQueue{}
	stored := &store.Plan{ID: "plan-abc", Name: "linux-nightly", Version: 3}

	got, err := Prepare(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), Stored: stored,
		ID: "run-1", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Run.PlanID != "plan-abc" || got.Run.PlanVersion != 3 {
		t.Fatalf("provenance = %q v%d, want plan-abc v3", got.Run.PlanID, got.Run.PlanVersion)
	}
}

func TestPrepareLeavesProvenanceEmptyForAnAdhocDrill(t *testing.T) {
	q := &fakeQueue{}

	got, err := Prepare(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), ID: "run-1", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// A plan_id invented for an ad-hoc drill would point at a row that does
	// not exist.
	if got.Run.PlanID != "" || got.Run.PlanVersion != 0 {
		t.Fatalf("provenance = %q v%d, want empty for an ad-hoc drill",
			got.Run.PlanID, got.Run.PlanVersion)
	}
}

func TestPrepareRefusesAWorkloadAlreadyBeingDrilled(t *testing.T) {
	q := &fakeQueue{active: "run-in-flight"}

	_, err := Prepare(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), ID: "run-2", At: time.Now(),
	})

	var busy *ErrAlreadyRunning
	if !errors.As(err, &busy) {
		t.Fatalf("Prepare = %v, want *ErrAlreadyRunning", err)
	}
	if busy.ActiveRunID != "run-in-flight" || busy.WorkloadID != "110" {
		t.Fatalf("ErrAlreadyRunning = %+v, want run-in-flight on workload 110", busy)
	}
	// Two concurrent drills of one workload would restore the same backup
	// twice; a dashboard that double-clicks must not queue two of them.
	if len(q.queued) != 0 {
		t.Fatalf("%d runs queued despite the conflict, want 0", len(q.queued))
	}
}

func TestPrepareTakesTheWorkloadFromThePlan(t *testing.T) {
	q := &fakeQueue{}

	got, err := Prepare(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), ID: "run-3", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Run.SourceWorkloadID != "110" {
		t.Fatalf("SourceWorkloadID = %q, want 110", got.Run.SourceWorkloadID)
	}
	// One source for the workload, so the lock taken and the row written
	// cannot end up disagreeing about what is being drilled.
	if len(q.askedFor) != 1 || q.askedFor[0] != "110" {
		t.Fatalf("the conflict check was asked about %v, want [110]", q.askedFor)
	}
}

func TestPrepareFallsBackToTheDefaultProvider(t *testing.T) {
	q := &fakeQueue{}

	got, err := Prepare(context.Background(), q, Request{
		Plan: testPlan("110", ""), DefaultProvider: "proxmox-main",
		ID: "run-4", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Run.ProviderID != "proxmox-main" {
		t.Fatalf("ProviderID = %q, want the configured default", got.Run.ProviderID)
	}
}

func TestPrepareSnapshotIsTheDefaultedPlanRemarshalled(t *testing.T) {
	q := &fakeQueue{}
	p := testPlan("110", "proxmox-main")

	got, err := Prepare(context.Background(), q, Request{
		Plan: p, ID: "run-5", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// The snapshot is what the worker executes, so editing or deleting the
	// catalogue row afterwards cannot change what this drill did.
	var round plan.Plan
	if err := yaml.Unmarshal([]byte(got.PlanYAML), &round); err != nil {
		t.Fatalf("the snapshot is not valid YAML: %v", err)
	}
	if round.Workload.ID != "110" || round.Name != p.Name {
		t.Fatalf("snapshot round-tripped to %+v, want the plan that was queued", round)
	}
}

func TestPrepareCarriesTheRTOTarget(t *testing.T) {
	q := &fakeQueue{}

	got, err := Prepare(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), ID: "run-6", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Run.RTOTarget != 5*time.Minute {
		t.Fatalf("RTOTarget = %v, want 5m - the run is graded against it", got.Run.RTOTarget)
	}
}

func TestEnqueueWritesWhatPrepareBuilt(t *testing.T) {
	q := &fakeQueue{}
	at := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)

	run, err := Enqueue(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), ID: "run-7", At: at,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(q.queued) != 1 {
		t.Fatalf("%d runs queued, want 1", len(q.queued))
	}
	if q.queued[0].run != run {
		t.Fatal("Enqueue returned a different run than the one it wrote")
	}
	if !q.queued[0].at.Equal(at) {
		t.Fatalf("queued at %v, want %v", q.queued[0].at, at)
	}
	if q.queued[0].planYAML == "" {
		t.Fatal("the run was queued with no plan snapshot")
	}
}

func TestEnqueueReportsAConflictWithoutWriting(t *testing.T) {
	q := &fakeQueue{active: "run-in-flight"}

	_, err := Enqueue(context.Background(), q, Request{
		Plan: testPlan("110", "proxmox-main"), ID: "run-8", At: time.Now(),
	})

	var busy *ErrAlreadyRunning
	if !errors.As(err, &busy) {
		t.Fatalf("Enqueue = %v, want *ErrAlreadyRunning", err)
	}
	if len(q.queued) != 0 {
		t.Fatalf("%d runs queued despite the conflict, want 0", len(q.queued))
	}
}

func TestPrepareRequiresAPlan(t *testing.T) {
	q := &fakeQueue{}
	if _, err := Prepare(context.Background(), q, Request{ID: "run-9", At: time.Now()}); err == nil {
		t.Fatal("Prepare accepted a request with no plan")
	}
}
