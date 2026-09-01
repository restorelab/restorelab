package worker

import (
	"context"
	"fmt"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// reconcile settles the runs whose worker died.
//
// It runs at startup and periodically, because the dead worker may be
// another process entirely - the queue is shared, and a machine that was
// power-cycled leaves rows nobody else is watching.
//
// It never re-runs anything. That is the rule the whole phase is built
// around: a drill is destructive and not idempotent, so an interrupted one
// is failed and cleaned up, never replayed. A retry system that "helpfully"
// re-executed a queued job would be actively dangerous here, which is why
// there is none.
func (w *Worker) reconcile(ctx context.Context) {
	stale, err := w.store.StaleRuns(ctx, w.now())
	if err != nil {
		w.log.Warn("cannot look for interrupted runs", "err", err)
		return
	}
	for _, q := range stale {
		if w.isInFlight(q.ID) {
			// This process is executing that drill right now. Its lease looks
			// expired only because this program was frozen - a suspended
			// laptop, a paused VM, a stalled database - and the renewal tick
			// has not caught up yet. Settling it here would destroy the
			// temporary workload of a live restore, which is the exact
			// outcome the phase exists to avoid. Another worker deciding the
			// same thing is a different matter, and is what the lease means;
			// this process contradicting itself is not.
			w.log.Warn("skipping the reconciliation of a drill this worker is still executing",
				"run_id", q.ID)
			continue
		}
		w.settleInterrupted(ctx, q)
	}
}

// settleInterrupted fails one interrupted run and tries to remove what it
// left behind.
func (w *Worker) settleInterrupted(ctx context.Context, q store.QueuedRun) {
	log := w.log.With("run_id", q.ID, "workload", q.SourceWorkloadID)
	log.Warn("found an interrupted drill; failing it and cleaning up, never re-running it")

	// The run row is the only place that knows what was created, which is
	// why the temporary workload is written the moment it exists.
	run, err := w.store.GetRun(ctx, q.ID)
	if err != nil {
		log.Warn("cannot read the interrupted run", "err", err)
		return
	}

	reason := "interrupted: the worker running this drill did not survive"
	if run.TempWorkloadID != "" {
		if hv, perr := w.providers.Hypervisor(q.ProviderID); perr != nil {
			reason += fmt.Sprintf("; the temporary workload %s on node %s could not be reached to be removed: %v",
				run.TempWorkloadID, run.Node, perr)
		} else if derr := Cleanup(ctx, hv, run.TempWorkloadID); derr != nil {
			// Naming the orphan is the point. A cleanup that failed silently
			// would leave a workload on the cluster and no trace of it.
			reason += fmt.Sprintf("; ORPHANED WORKLOAD: %s on node %s could not be removed: %v",
				run.TempWorkloadID, run.Node, derr)
			w.settle(ctx, q.ID, core.RunCleanupFailed, reason)
			return
		} else {
			log.Info("removed the workload the interrupted drill had left", "temp_workload", run.TempWorkloadID)
		}
	}

	w.settle(ctx, q.ID, core.RunFailed, reason)
}

// settle writes an interrupted run's final state and releases its lease.
func (w *Worker) settle(ctx context.Context, runID string, state core.RunState, reason string) {
	if err := w.store.SetState(ctx, runID, state); err != nil {
		w.log.Warn("could not record the state of an interrupted run", "run_id", runID, "err", err)
	}
	if err := w.store.SetRunError(ctx, runID, reason); err != nil {
		w.log.Warn("could not record why a run was interrupted", "run_id", runID, "err", err)
	}
	if err := w.store.FinishLease(ctx, runID); err != nil {
		w.log.Warn("could not release the lease of an interrupted run", "run_id", runID, "err", err)
	}
}

// Cleanup destroys a temporary workload, and is the only mutating provider
// call this product exposes outside a run.
//
// It carries no guard of its own on purpose: the provider's Delete already
// refuses anything outside the reserved temporary id range before it makes a
// single network call, and anything whose description does not carry
// restorelab_managed=true. Re-implementing those checks here would create a
// second place for them to drift.
func Cleanup(ctx context.Context, hv core.HypervisorProvider, workloadID string) error {
	return hv.Delete(ctx, workloadID)
}
