package recovery

import (
	"context"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// cleanupTimeout bounds the detached cleanup context. Ten minutes is
// generous for a stop+delete against a single small VM; it exists as a
// backstop, not a tuning knob.
const cleanupTimeout = 10 * time.Minute

// cleanup ALWAYS runs (called from Run's deferred block) and decides,
// independently of the run's outcome, whether the temporary workload should
// actually be destroyed:
//
//   - needsCleanup == false: nothing was ever created on the provider
//     (failed before Restore was submitted, or this was a dry run) — a no-op.
//   - opts.KeepWorkload, or plan.Cleanup.KeepOnFailure on a run that failed
//     or was cancelled, or !plan.Cleanup.CleanupAlways() on a run that did
//     complete: the workload
//     is deliberately left running, logged loudly because it is now the
//     operator's responsibility to remove it.
//   - otherwise: Stop then Delete, always by the TEMPORARY id — never the
//     source workload id. Delete failing is treated as an incident, not a
//     soft error: it sets run.State to core.RunCleanupFailed and names the
//     exact VMID and node so an admin can remove it by hand.
//
// It runs on its own context derived from context.Background(), NOT the
// run's ctx: a cancelled or timed-out run must still have its temporary
// workload torn down, otherwise cancelling a run becomes a way to leak VMs.
//
// It returns nil unless the delete itself failed, in which case it returns
// an error wrapping the provider's error (for errors.Is/As) — the loud,
// VMID-naming message is recorded on run.Err and run.State regardless, so
// the report carries it even for callers that only look at the run.
//
// The nolint below is that invariant stated to the linter, not a waiver.
// contextcheck is right that this ignores the run's ctx and builds its own
// from context.Background(); that is precisely the point. Rebinding the Stop
// and Delete calls to the run's ctx would make Ctrl-C on a drill a reliable
// way to leak a temporary VM onto a cluster, which is the failure mode this
// whole function exists to prevent. The 10-minute timeout is what keeps the
// detached context from being unbounded.
//
//nolint:contextcheck // detached cleanup context is a safety invariant, see above
func (e *Engine) cleanup(_ context.Context, run *core.RecoveryRun, p *plan.Plan, opts RunOptions, tempID, node string, needsCleanup bool) error {
	if !needsCleanup {
		run.CleanupDone = true
		return nil
	}

	// beginStep (called below, directly or via recordCleanupSkipped) always
	// sets run.State to core.RunCleaningUp for the duration of this step, as
	// every other step does. Unlike every other step, this one runs after
	// the run has already been graded (Success/Degraded/Failed) — so unless
	// cleanup itself fails, that graded state must be restored once cleanup
	// is done, not left showing "cleaning up" forever.
	gradedState := run.State

	if opts.KeepWorkload {
		e.logKept(run, tempID, node, "KeepWorkload was requested")
		e.recordCleanupSkipped(run, fmt.Sprintf(
			"cleanup skipped: KeepWorkload requested — workload %s on node %s left running, remove it by hand", tempID, node))
		run.State = gradedState
		return nil
	}

	// A cancelled run counts as "did not complete" for cleanup policy, the
	// same as a failed one did back when a Ctrl-C was graded FAILED. Both
	// halves of that matter: keep_on_failure (an explicit debugging opt-in)
	// keeps applying to an interrupted drill, and "cleanup.always: false" —
	// which only ever meant "leave a healthy drill up so I can poke at it" —
	// must never become a way for a Ctrl-C to leak a temporary VM.
	incomplete := run.Result == core.ResultFailed || run.State == core.RunFailed || run.State == core.RunCancelled
	if incomplete && p.Cleanup.KeepOnFailure {
		e.logKept(run, tempID, node, "plan.cleanup.keep_on_failure is set and the run did not complete")
		e.recordCleanupSkipped(run, fmt.Sprintf(
			"cleanup skipped: keep_on_failure is set — workload %s on node %s left running for debugging, remove it by hand", tempID, node))
		run.State = gradedState
		return nil
	}

	if !incomplete && !p.Cleanup.CleanupAlways() {
		// Always defaults to true; an explicit "always: false" on a run that
		// didn't fail means the operator wants to inspect it.
		e.recordCleanupSkipped(run, fmt.Sprintf(
			"cleanup skipped by plan policy (cleanup.always: false) — workload %s left on node %s", tempID, node))
		run.State = gradedState
		return nil
	}

	cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	idx := e.beginStep(run, StepCleanup, core.RunCleaningUp)

	// Stop is safe to retry and best-effort: if it fails we still attempt
	// Delete, since some providers can delete a running workload directly.
	if serr := e.retry(cctx, 3, 2*time.Second, func() error { return e.hv.Stop(cctx, tempID) }); serr != nil {
		e.log.Warn("failed to stop temporary workload before delete, attempting delete anyway",
			"run_id", run.ID, "workload_id", tempID, "err", serr)
	}

	// Delete is NEVER retried: it is destructive, and unlike Stop a partial
	// or ambiguous failure here must be surfaced loudly, not silently
	// repeated against a workload that may already be half-deleted.
	if derr := e.hv.Delete(cctx, tempID); derr != nil {
		run.CleanupDone = false
		run.State = core.RunCleanupFailed // deliberately overrides gradedState: an orphan VM trumps everything
		msg := fmt.Sprintf(
			"ORPHANED WORKLOAD: failed to delete temporary workload %s on node %s (run %s) — remove it manually: %v",
			tempID, node, run.ID, derr)
		run.Err = msg
		e.log.Error(msg, "run_id", run.ID, "workload_id", tempID, "node", node, "err", derr)
		e.endStep(run, idx, core.StepFailed, msg, derr)
		return fmt.Errorf("cleanup: orphaned workload %s on node %s: %w", tempID, node, derr)
	}

	run.CleanupDone = true
	e.endStep(run, idx, core.StepDone, fmt.Sprintf("temporary workload %s removed", tempID), nil)
	run.State = gradedState
	return nil
}

// recordCleanupSkipped adds a visible, skipped cleanup step so the report
// shows a workload was deliberately left behind rather than silently
// forgotten.
func (e *Engine) recordCleanupSkipped(run *core.RecoveryRun, message string) {
	idx := e.beginStep(run, StepCleanup, core.RunCleaningUp)
	e.endStep(run, idx, core.StepSkipped, message, nil)
	run.CleanupDone = false
}

func (e *Engine) logKept(run *core.RecoveryRun, tempID, node, reason string) {
	e.log.Warn("KEEPING restored workload — MANUAL CLEANUP REQUIRED",
		"run_id", run.ID, "workload_id", tempID, "node", node, "reason", reason)
}
