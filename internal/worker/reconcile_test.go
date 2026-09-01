package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// guardedProvider mimics the two guards the real provider carries, because a
// reconciliation test that could delete anything would prove the opposite of
// what it claims.
type guardedProvider struct {
	mu       sync.Mutex
	restores int
	// deleteCalls counts every attempt, refused ones included. deleted
	// records only what was actually destroyed, so the two together
	// distinguish "cleanup was not attempted" from "cleanup was refused".
	deleteCalls int
	deleted     []string
	managed     map[string]bool
}

func (p *guardedProvider) Delete(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteCalls++
	n, err := strconv.Atoi(id)
	if err != nil || n < 9000 || n > 9999 {
		return fmt.Errorf("outside the reserved range: %w", core.ErrNotManaged)
	}
	if !p.managed[id] {
		return fmt.Errorf("not ours: %w", core.ErrNotManaged)
	}
	p.deleted = append(p.deleted, id)
	return nil
}

func (p *guardedProvider) Restore(context.Context, core.Backup, core.RestoreOptions) (*core.RestoreJob, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restores++
	return nil, errors.New("the test must never reach a restore during reconciliation")
}

// The rest of core.HypervisorProvider. The read-only half returns zero
// values; the half that creates something refuses out loud, because
// reconciliation reaching it at all would be the bug this suite exists for.

func (p *guardedProvider) ID() string                 { return "pve" }
func (p *guardedProvider) Kind() string               { return "fake" }
func (p *guardedProvider) Ping(context.Context) error { return nil }

func (p *guardedProvider) ListNodes(context.Context) ([]core.Node, error) { return nil, nil }

func (p *guardedProvider) ListWorkloads(context.Context) ([]core.Workload, error) { return nil, nil }

func (p *guardedProvider) GetWorkload(context.Context, string) (*core.Workload, error) {
	return nil, core.ErrNotFound
}

func (p *guardedProvider) GetStatus(context.Context, string) (*core.WorkloadStatus, error) {
	return nil, core.ErrNotFound
}

func (p *guardedProvider) AllocateWorkloadID(context.Context) (string, error) {
	return "", errors.New("reconciliation must never allocate a temporary id: that is how a drill would be replayed")
}

func (p *guardedProvider) WaitForJob(context.Context, *core.RestoreJob) (*core.TaskState, error) {
	return nil, errors.New("reconciliation must never wait on a restore job")
}

func (p *guardedProvider) Start(context.Context, string) error { return nil }
func (p *guardedProvider) Stop(context.Context, string) error  { return nil }

var _ core.HypervisorProvider = (*guardedProvider)(nil)

// staticProviders hands the worker the one hypervisor a reconciliation needs,
// and refuses when it has none - a provider that cannot be reached is a case
// reconciliation has to survive, and this is the only way to produce it.
//
// Its backup half always refuses: reconciliation never restores, so reaching
// for a backup provider would already be the failure the suite looks for.
type staticProviders struct{ hv core.HypervisorProvider }

func (p staticProviders) Hypervisor(id string) (core.HypervisorProvider, error) {
	if p.hv == nil {
		return nil, fmt.Errorf("no hypervisor is configured for %s", id)
	}
	return p.hv, nil
}

func (p staticProviders) Backups(string) (core.BackupProvider, error) {
	return nil, errors.New("reconciliation must never need a backup provider")
}

var _ Providers = staticProviders{}

// assertLeaseSettled proves settle released the lease the way FinishLease
// does: the expiry is cleared, the owner is kept. A cleared owner would make
// the interrupted run claimable all over again.
func assertLeaseSettled(t *testing.T, s store.Store, runID, wantOwner string) {
	t.Helper()
	owner, expires, err := s.RunLease(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunLease %s: %v", runID, err)
	}
	if owner != wantOwner {
		t.Errorf("lease owner = %q, want %q kept: which worker ran a drill is part of its history", owner, wantOwner)
	}
	if !expires.IsZero() {
		t.Errorf("run %s still holds a lease expiry after being settled: %v", runID, expires)
	}
}

// The invariant of the entire phase: a drill whose worker died is failed,
// its leftovers are destroyed, and it is NEVER executed again. A drill is
// destructive and not idempotent - replaying one would allocate a second
// temporary id, restore a second time, and could leave the first workload
// behind.
func TestReconcileFailsAStaleRunAndNeverRerunsIt(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	s := newStore(t) // a real SQLite file, as in internal/store
	run := &core.RecoveryRun{
		ID: "dead-1", PlanName: "adhoc-110", ProviderID: "pve",
		SourceWorkloadID: "110", State: core.RunQueued,
	}
	if err := s.Enqueue(ctx, run, "name: adhoc-110\n", base); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A worker took it, got as far as restoring, recorded what it had
	// created - and died there.
	if _, err := s.ClaimRun(ctx, "worker-that-died", time.Minute, base); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if err := s.SetState(ctx, run.ID, core.RunRestoring); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := s.SetTempWorkload(ctx, run.ID, "9001", "pve1"); err != nil {
		t.Fatalf("SetTempWorkload: %v", err)
	}

	hv := &guardedProvider{managed: map[string]bool{"9001": true}}
	w, err := New(Options{
		Store:     s,
		Providers: staticProviders{hv: hv},
		Owner:     "worker-that-lives",
		// An hour later: the dead worker's lease is long gone.
		Now: func() time.Time { return base.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.reconcile(ctx)

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != core.RunFailed {
		t.Errorf("State = %q, want FAILED", got.State)
	}
	if !strings.Contains(got.Err, "interrupted") {
		t.Errorf("Err = %q, want it to say the drill was interrupted", got.Err)
	}

	if len(hv.deleted) != 1 || hv.deleted[0] != "9001" {
		t.Errorf("deleted = %v, want exactly the temporary workload 9001", hv.deleted)
	}
	if hv.restores != 0 {
		t.Fatalf("reconciliation issued %d restore(s): an interrupted drill must never be replayed", hv.restores)
	}

	// And nothing can revive it afterwards.
	if _, err := s.ClaimRun(ctx, "worker-that-lives", time.Minute, base.Add(2*time.Hour)); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("an interrupted run was claimable again: %v", err)
	}

	assertLeaseSettled(t, s, run.ID, "worker-that-died")
}

// A run that died before it created anything has nothing to clean up, and
// must not have its cleanup attempted against an empty id.
func TestReconcileDoesNotCleanUpWhatWasNeverCreated(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	s := newStore(t)
	run := &core.RecoveryRun{
		ID: "dead-2", PlanName: "adhoc-111", ProviderID: "pve",
		SourceWorkloadID: "111", State: core.RunQueued,
	}
	if err := s.Enqueue(ctx, run, "name: adhoc-111\n", base); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.ClaimRun(ctx, "worker-that-died", time.Minute, base); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	// It died looking for a backup: nothing exists on any cluster yet, and
	// the run row names no temporary workload.
	if err := s.SetState(ctx, run.ID, core.RunDiscoveringBackup); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	hv := &guardedProvider{managed: map[string]bool{}}
	w, err := New(Options{
		Store:     s,
		Providers: staticProviders{hv: hv},
		Owner:     "worker-that-lives",
		Now:       func() time.Time { return base.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.reconcile(ctx)

	if hv.deleteCalls != 0 {
		t.Errorf("Delete was called %d time(s) for a run that created nothing: cleanup was attempted against an empty id",
			hv.deleteCalls)
	}
	if len(hv.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing", hv.deleted)
	}
	if hv.restores != 0 {
		t.Fatalf("reconciliation issued %d restore(s): an interrupted drill must never be replayed", hv.restores)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != core.RunFailed {
		t.Errorf("State = %q, want FAILED: a run with nothing to clean up still has to be settled", got.State)
	}
	if !strings.Contains(got.Err, "interrupted") {
		t.Errorf("Err = %q, want it to say the drill was interrupted", got.Err)
	}
	if strings.Contains(got.Err, "ORPHANED") {
		t.Errorf("Err = %q: a run that created nothing was reported as having leaked a workload", got.Err)
	}
	assertLeaseSettled(t, s, run.ID, "worker-that-died")
}

// A live drill belonging to another worker is left alone.
func TestReconcileIgnoresRunsWhoseLeaseIsStillGood(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	s := newStore(t)
	run := &core.RecoveryRun{
		ID: "live-1", PlanName: "adhoc-112", ProviderID: "pve",
		SourceWorkloadID: "112", State: core.RunQueued,
	}
	if err := s.Enqueue(ctx, run, "name: adhoc-112\n", base); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.ClaimRun(ctx, "worker-elsewhere", time.Minute, base); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if err := s.SetState(ctx, run.ID, core.RunRestoring); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := s.SetTempWorkload(ctx, run.ID, "9002", "pve1"); err != nil {
		t.Fatalf("SetTempWorkload: %v", err)
	}

	hv := &guardedProvider{managed: map[string]bool{"9002": true}}
	w, err := New(Options{
		Store:     s,
		Providers: staticProviders{hv: hv},
		Owner:     "worker-that-lives",
		// Half a minute into a one-minute lease: that drill is alive and
		// restoring on another machine right now.
		Now: func() time.Time { return base.Add(30 * time.Second) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.reconcile(ctx)

	if hv.deleteCalls != 0 {
		t.Fatalf("Delete was called %d time(s) against a live drill: reconciliation destroyed a running restore",
			hv.deleteCalls)
	}
	if hv.restores != 0 {
		t.Fatalf("reconciliation issued %d restore(s)", hv.restores)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != core.RunRestoring {
		t.Errorf("State = %q, want RESTORING left untouched", got.State)
	}
	if got.Err != "" {
		t.Errorf("Err = %q, want empty: a live drill was recorded as interrupted", got.Err)
	}

	// The lease is exactly as its holder left it: an untouched expiry is what
	// lets the live worker keep renewing it.
	owner, expires, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatalf("RunLease: %v", err)
	}
	if owner != "worker-elsewhere" {
		t.Errorf("lease owner = %q, want worker-elsewhere", owner)
	}
	if !expires.Equal(base.Add(time.Minute)) {
		t.Errorf("lease expiry = %v, want %v untouched", expires, base.Add(time.Minute))
	}
}

// The double lock still applies: a run row naming a workload outside the
// reserved range is refused by the provider, and reconciliation records that
// rather than pretending it cleaned up.
func TestReconcileRespectsTheDeleteGuards(t *testing.T) {
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	// The two guards, one case each. The first is the frightening one: a
	// corrupt or hand-edited row naming the production workload the drill was
	// copied from.
	cases := []struct {
		name     string
		runID    string
		workload string
		tempID   string
		managed  map[string]bool
	}{
		{
			name: "an id outside the reserved range", runID: "dead-3",
			workload: "113", tempID: "113", managed: map[string]bool{"113": true},
		},
		{
			name: "a workload that is not ours", runID: "dead-4",
			workload: "114", tempID: "9005", managed: map[string]bool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			run := &core.RecoveryRun{
				ID: tc.runID, PlanName: "adhoc-" + tc.workload, ProviderID: "pve",
				SourceWorkloadID: tc.workload, State: core.RunQueued,
			}
			if err := s.Enqueue(ctx, run, "name: x\n", base); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if _, err := s.ClaimRun(ctx, "worker-that-died", time.Minute, base); err != nil {
				t.Fatalf("ClaimRun: %v", err)
			}
			if err := s.SetState(ctx, run.ID, core.RunRestoring); err != nil {
				t.Fatalf("SetState: %v", err)
			}
			if err := s.SetTempWorkload(ctx, run.ID, tc.tempID, "pve1"); err != nil {
				t.Fatalf("SetTempWorkload: %v", err)
			}

			hv := &guardedProvider{managed: tc.managed}
			w, err := New(Options{
				Store:     s,
				Providers: staticProviders{hv: hv},
				Owner:     "worker-that-lives",
				Now:       func() time.Time { return base.Add(time.Hour) },
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			w.reconcile(ctx)

			// The guard held: the call was made and refused, and nothing was
			// destroyed.
			if hv.deleteCalls != 1 {
				t.Errorf("Delete was called %d time(s), want exactly 1", hv.deleteCalls)
			}
			if len(hv.deleted) != 0 {
				t.Fatalf("deleted = %v: the provider's guards let a refused workload through", hv.deleted)
			}
			if hv.restores != 0 {
				t.Fatalf("reconciliation issued %d restore(s)", hv.restores)
			}

			got, err := s.GetRun(ctx, run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got.State != core.RunCleanupFailed {
				t.Errorf("State = %q, want CLEANUP_FAILED: a refused cleanup must not read as a clean failure", got.State)
			}
			if !strings.Contains(got.Err, "ORPHANED WORKLOAD") {
				t.Errorf("Err = %q, want it to name the workload as orphaned", got.Err)
			}
			if !strings.Contains(got.Err, tc.tempID) {
				t.Errorf("Err = %q, want it to name workload %s: an orphan nobody can name is an orphan nobody finds",
					got.Err, tc.tempID)
			}
			if !strings.Contains(got.Err, "interrupted") {
				t.Errorf("Err = %q, want it to still say the drill was interrupted", got.Err)
			}

			assertLeaseSettled(t, s, run.ID, "worker-that-died")

			// Still terminal, still unclaimable: a cleanup that could not run
			// is not a reason to replay the drill.
			if _, err := s.ClaimRun(ctx, "worker-that-lives", time.Minute, base.Add(2*time.Hour)); !errors.Is(err, store.ErrNoWork) {
				t.Fatalf("a run whose cleanup failed was claimable again: %v", err)
			}
		})
	}
}

// A drill this very process is executing is never settled by its own sweep.
//
// Not one of the four the plan names. The periodic sweep is what makes it
// reachable: a frozen process - a suspended laptop, a paused VM, a stalled
// database - wakes with its own lease expired and its own drill still
// running. Settling it there would destroy the temporary workload of a live
// restore, from the same process that is restoring onto it. Another worker
// deciding a lease is dead is what a lease means; this one contradicting
// itself is not.
func TestReconcileLeavesAloneADrillThisWorkerIsExecuting(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	s := newStore(t)
	run := &core.RecoveryRun{
		ID: "mine-1", PlanName: "adhoc-115", ProviderID: "pve",
		SourceWorkloadID: "115", State: core.RunQueued,
	}
	if err := s.Enqueue(ctx, run, "name: x\n", base); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.ClaimRun(ctx, "worker-that-lives", time.Minute, base); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if err := s.SetState(ctx, run.ID, core.RunRestoring); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := s.SetTempWorkload(ctx, run.ID, "9003", "pve1"); err != nil {
		t.Fatalf("SetTempWorkload: %v", err)
	}

	hv := &guardedProvider{managed: map[string]bool{"9003": true}}
	w, err := New(Options{
		Store:     s,
		Providers: staticProviders{hv: hv},
		Owner:     "worker-that-lives",
		Now:       func() time.Time { return base.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.markInFlight(run.ID)
	w.reconcile(ctx)

	if hv.deleteCalls != 0 {
		t.Fatalf("this worker destroyed the temporary workload of a drill it is still running")
	}
	if got, err := s.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	} else if got.State != core.RunRestoring {
		t.Fatalf("State = %q, want RESTORING: this worker failed a drill it is still executing", got.State)
	}

	// And being in flight is the only thing that spared it: once the drill is
	// done with, the very same sweep settles it.
	w.clearInFlight(run.ID)
	w.reconcile(ctx)

	if len(hv.deleted) != 1 || hv.deleted[0] != "9003" {
		t.Errorf("deleted = %v, want 9003 once the run is no longer in flight", hv.deleted)
	}
	if got, err := s.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	} else if got.State != core.RunFailed {
		t.Errorf("State = %q, want FAILED", got.State)
	}
	if hv.restores != 0 {
		t.Fatalf("reconciliation issued %d restore(s)", hv.restores)
	}
}
