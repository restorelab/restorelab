// Package recovery implements RestoreLab's recovery engine, the workflow
// that turns a plan.Plan into a proven (or disproven) recovery, end to end:
// find a backup, restore it into an isolated temporary workload, boot it,
// wait for the guest, run checks, measure the RTO, and ALWAYS clean up.
//
// This is the package where a bug deletes a production VM. Every safety
// decision below is deliberate and commented as such; treat any shortcut
// here as a production incident waiting to happen.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// Step names recorded on core.RecoveryRun.Steps and used as Event.Step. They
// are stable identifiers: reports and any future dashboard key off them.
const (
	StepDiscoverBackup     = "discover_backup"
	StepPrepareEnvironment = "prepare_environment"
	StepRestore            = "restore"
	StepStart              = "start"
	StepWaitForGuest       = "wait_for_guest"
	StepRunChecks          = "run_checks"
	StepCleanup            = "cleanup"
)

// CheckRunner executes a plan's configured checks against a recovered
// workload. internal/checks provides the real implementation; tests provide
// a scriptable fake.
type CheckRunner interface {
	RunAll(ctx context.Context, target core.Target, cfgs []core.CheckConfig) []core.CheckResult
}

// Deps wires the engine to the outside world. Every optional field has a
// safe, production-appropriate default when left nil. See New.
type Deps struct {
	Hypervisor core.HypervisorProvider
	Backups    core.BackupProvider // may be the same object as Hypervisor
	Checks     CheckRunner
	Logger     *slog.Logger // nil -> slog.Default()

	// Now and Sleep are the engine's clock. Production leaves them nil;
	// tests inject a fake clock and an instant Sleep so the full workflow,
	// including timeouts and backoffs, runs without actually waiting.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// Emit streams progress events to the CLI / an SSE handler. nil -> no-op.
	Emit func(Event)
}

// RunOptions carries the per-run knobs that come from the caller (CLI flags,
// config) rather than from the plan itself.
type RunOptions struct {
	RunID   string             // generated when empty
	Network core.NetworkConfig // resolved by the caller from config; the engine only validates it
	Node    string             // overrides plan.Restore.Node
	Storage string             // overrides plan.Restore.Storage
	Pool    string             // overrides plan.Restore.Pool
	DryRun  bool               // resolve backup + validate the plan; change nothing
	// KeepWorkload skips cleanup entirely for debugging. Every run that sets
	// it logs loudly, because it leaves a live workload on the cluster.
	KeepWorkload bool
}

// Engine runs recovery plans. Build one with New; it is safe to reuse across
// runs and safe for concurrent use as long as the underlying providers are.
type Engine struct {
	hv      core.HypervisorProvider
	backups core.BackupProvider
	checks  CheckRunner
	log     *slog.Logger
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	emit    func(Event)
}

// New builds an Engine from Deps, applying safe defaults to every optional
// field. It errors when a required dependency is missing.
func New(d Deps) (*Engine, error) {
	if d.Hypervisor == nil {
		return nil, errors.New("recovery: Deps.Hypervisor is required")
	}
	if d.Backups == nil {
		return nil, errors.New("recovery: Deps.Backups is required")
	}
	if d.Checks == nil {
		return nil, errors.New("recovery: Deps.Checks is required")
	}

	e := &Engine{
		hv:      d.Hypervisor,
		backups: d.Backups,
		checks:  d.Checks,
		log:     d.Logger,
		now:     d.Now,
		sleep:   d.Sleep,
		emit:    d.Emit,
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.sleep == nil {
		e.sleep = realSleep
	}
	if e.emit == nil {
		e.emit = func(Event) {}
	}
	return e, nil
}

// realSleep is the production Sleep implementation: a plain timer that also
// honours ctx cancellation, so a cancelled run doesn't block on a poll wait.
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run executes a recovery plan end to end.
//
// Contract: Run ALWAYS returns a non-nil *core.RecoveryRun, populated as far
// as the workflow got. Callers (the CLI, the report writer) build the
// report from this run even when the run failed. Run returns a non-nil error
// exactly when the run did not succeed: run.Result == core.ResultFailed, the
// run was cancelled (run.State == core.RunCancelled, run.Result empty), or
// cleanup itself failed (run.State == core.RunCleanupFailed, which can
// happen even after a graded Success/Degraded run; both are joined via
// errors.Join when they occur together). A core.ResultDegraded run with
// cleanup done returns a nil error: it recovered, just not perfectly.
//
// A run whose ctx was cancelled (context.Canceled, NOT
// context.DeadlineExceeded) ends in core.RunCancelled rather than
// core.RunFailed: stopping a drill is an operator's decision, not evidence
// that the backup is bad. See markCancelled.
// Unlike run.Err (a flattened string, for display), the returned error
// preserves the original chain, so callers can errors.Is/As it against core
// sentinels (core.ErrNoBackup, core.ErrNetworkNotIsolated, core.ErrTimeout,
// ...).
//
// Cleanup of anything the engine creates is unconditional: it runs from a
// deferred, panic-safe block on its own detached context, so it still
// happens when ctx is cancelled, when a step fails, or even when the
// workflow panics. An orphaned VM on the cluster is worse than a crashed
// recovery run.
func (e *Engine) Run(ctx context.Context, p *plan.Plan, opts RunOptions) (run *core.RecoveryRun, err error) {
	run = e.newRun(p, opts)
	e.log.Info("recovery run starting", "run_id", run.ID, "plan", p.Name, "workload_id", p.Workload.ID)

	// tempID/node/needsCleanup are filled in as the workflow progresses and
	// read by the deferred cleanup below, regardless of how the workflow
	// exits (normal return, early return on step failure, or panic).
	var (
		tempID       string
		node         string
		needsCleanup bool
		// workflowErr preserves the real error chain (so callers can
		// errors.Is/As against core sentinels like core.ErrNoBackup);
		// run.Err is only ever the flattened string form of it, which is
		// not enough to reconstruct the chain.
		workflowErr error
	)

	defer func() {
		panicked := false
		if r := recover(); r != nil {
			// An orphaned VM is worse than a crash: turn the panic into a
			// failed run instead of letting it skip cleanup entirely.
			panicked = true
			e.log.Error("panic recovered in recovery engine", "run_id", run.ID, "panic", r)
			perr := fmt.Errorf("internal error (recovered panic): %v", r)
			e.markFailed(run, perr)
			workflowErr = perr
		}

		// Cancellation is decided from the CONTEXT, never from the error.
		// The error chain is not a reliable witness: retry deliberately
		// returns the provider's original error rather than ctx.Err() when
		// the context is cancelled during a backoff, because that error is
		// the more useful one to show. So we ask the context directly, here,
		// at the one point every exit path from the workflow goes through.
		//
		// Only context.Canceled counts. A context.DeadlineExceeded run
		// FAILED: a drill that blew its deadline is a recovery that did not
		// happen in time, which is precisely what this product exists to
		// report, not a decision somebody made.
		//
		// A recovered panic is an internal defect and stays FAILED even if
		// the run was also cancelled: "we crashed" is the news, not "you
		// pressed Ctrl-C".
		if !panicked && workflowErr != nil && errors.Is(ctx.Err(), context.Canceled) {
			e.markCancelled(run, workflowErr)
		}

		cleanupErr := e.cleanup(ctx, run, p, opts, tempID, node, needsCleanup)
		e.finalize(run)
		err = combineErrors(workflowErr, cleanupErr)
	}()

	if opts.DryRun {
		workflowErr = e.runDryRun(ctx, run, p, opts)
		return
	}

	if derr := e.discoverBackup(ctx, run, p); derr != nil {
		e.markFailed(run, derr)
		workflowErr = derr
		return
	}

	tid, tname, metadata, nd, perr := e.prepareEnvironment(ctx, run, p, opts)
	if perr != nil {
		e.markFailed(run, perr)
		workflowErr = perr
		return
	}
	tempID, node = tid, nd

	if rerr := e.restoreWorkload(ctx, run, p, opts, tid, tname, metadata, nd, &needsCleanup); rerr != nil {
		e.markFailed(run, rerr)
		workflowErr = rerr
		return
	}

	if serr := e.startWorkload(ctx, run, p, tid); serr != nil {
		e.markFailed(run, serr)
		workflowErr = serr
		return
	}

	target, werr := e.waitForGuest(ctx, run, p, tid)
	if werr != nil {
		e.markFailed(run, werr)
		workflowErr = werr
		return
	}

	if len(p.Checks) == 0 {
		run.RTO = computeRTO(run, run.StartedAt)
		e.gradeSuccess(run, p)
		return
	}

	cerr := e.runChecks(ctx, run, p, target)
	run.RTO = computeRTO(run, run.StartedAt)
	if cerr != nil {
		if errors.Is(cerr, errChecksInconclusive) {
			e.markInconclusive(run, cerr)
		} else {
			e.markFailed(run, cerr)
		}
		workflowErr = cerr
		return
	}
	e.gradeSuccess(run, p)
	return
}

// combineErrors merges the workflow error (if any) with a cleanup error (if
// any) into a single error a caller can still errors.Is/As against either
// half of.
func combineErrors(workflowErr, cleanupErr error) error {
	switch {
	case workflowErr != nil && cleanupErr != nil:
		return errors.Join(workflowErr, cleanupErr)
	case workflowErr != nil:
		return workflowErr
	case cleanupErr != nil:
		return cleanupErr
	default:
		return nil
	}
}

// newRun builds the initial run record.
func (e *Engine) newRun(p *plan.Plan, opts RunOptions) *core.RecoveryRun {
	id := opts.RunID
	if id == "" {
		id = uuid.NewString()
	}
	sourceName := firstNonEmpty(p.Workload.Name, p.Workload.ID)
	return &core.RecoveryRun{
		ID:               id,
		PlanName:         p.Name,
		ProviderID:       e.hv.ID(),
		BackupProviderID: e.backups.ID(),
		SourceWorkloadID: p.Workload.ID,
		SourceName:       sourceName,
		Node:             firstNonEmpty(opts.Node, p.Restore.Node),
		State:            core.RunQueued,
		StartedAt:        e.now(),
		RTOTarget:        p.RTOTarget.D(),
		// A run starts having proven nothing, and can only ever raise this
		// as it learns something. A run that dies before reaching the guest
		// therefore ends where it started, which is the truth about it.
		ProofLevel: core.ProofNone,
	}
}

// markFailed records a fatal, run-ending error. It is only ever called on
// the "this run cannot continue" path; grading of an otherwise-completed run
// happens in gradeSuccess instead.
func (e *Engine) markFailed(run *core.RecoveryRun, err error) {
	run.Result = core.ResultFailed
	run.State = core.RunFailed
	run.Err = err.Error()
	e.log.Error("recovery run failed", "run_id", run.ID, "err", err)
}

// errChecksInconclusive marks a run whose critical checks could not be
// evaluated. It is a sentinel rather than a message match because the caller
// grades on it, and grading on the wording of an error is how a report starts
// lying after somebody rephrases a string.
var errChecksInconclusive = errors.New("critical check(s) could not run")

// markInconclusive records a run that completed the workflow - the backup
// restored, the workload booted - but reached no verdict, because a critical
// check could not be evaluated at all.
//
// It carries no Result, exactly like a cancelled run and for the same reason
// (see markCancelled): SUCCESS, DEGRADED and FAILED are all claims about
// whether the backup restores, and "I could not tell" is none of them. The
// store persists an empty result as NULL, and report.Score already leaves a
// verdict-less run out of the failure rate.
//
// The commonest way to get here is a tcp:, http: or ping check dialled from a
// machine with no route into the isolated recovery network. That is a fact
// about the operator's topology, not about their backup, and charging a
// workload's confidence score for it would make the whole dashboard lie.
func (e *Engine) markInconclusive(run *core.RecoveryRun, err error) {
	run.State = core.RunInconclusive
	run.Result = ""
	run.Err = err.Error()
	e.log.Warn("recovery run reached no verdict", "run_id", run.ID, "err", err)
}

// markCancelled records a run that ended because a human stopped it (Ctrl-C,
// an API cancel), as opposed to one that failed on its own. It runs after
// markFailed on the same run and deliberately overrides it: the distinction
// only becomes knowable once the workflow has unwound.
//
// It is called BEFORE cleanup, on purpose: cleanup snapshots the graded
// state and restores it when it succeeds, so a cancelled run ends CANCELLED,
// and when the delete fails, cleanup still overrides it with
// core.RunCleanupFailed. An orphan on the cluster is more urgent news than
// an operator's decision to stop.
func (e *Engine) markCancelled(run *core.RecoveryRun, err error) {
	run.State = core.RunCancelled
	// A cancelled drill reached no verdict about the workload, so it carries
	// none: SUCCESS, DEGRADED and FAILED are all claims about whether the
	// backup restores, and a run that was stopped proves nothing either way.
	// Grading it FAILED would charge the workload's confidence score
	// (report.Score penalises core.ResultFailed) for a human decision, and
	// would make the history lie about how often recovery actually works.
	// The store already persists an empty result as NULL.
	run.Result = ""
	run.Err = fmt.Sprintf("run cancelled: %v", err)
	e.log.Warn("recovery run cancelled", "run_id", run.ID, "err", err)
}

// gradeSuccess grades a run that completed the workflow without a fatal step
// error: SUCCESS when every critical check passed and the RTO target was
// met, DEGRADED when the workload recovered but a non-critical check failed
// or the RTO target was exceeded. A critical check failure never reaches
// here: runChecks turns that into a step error handled by markFailed.
func (e *Engine) gradeSuccess(run *core.RecoveryRun, p *plan.Plan) {
	degraded := run.RTOExceeded()
	if !degraded {
		critical := criticalMap(p)
		for _, c := range run.Checks {
			// Only a check that ran and came back bad degrades the verdict.
			// One that could not run (core.CheckError) or never ran
			// (core.CheckSkipped) made no claim about the workload, and
			// downgrading on it would report an unreachable check host as a
			// partly-broken backup.
			if c.Status == core.CheckFail && !critical[c.Name] {
				degraded = true
				break
			}
		}
	}

	run.State = core.RunSuccess
	if degraded {
		run.Result = core.ResultDegraded
	} else {
		run.Result = core.ResultSuccess
	}
}

// finalize stamps CompletedAt once the run (workflow + cleanup) is fully
// done. It never overrides a state cleanup already set (e.g.
// core.RunCleanupFailed).
func (e *Engine) finalize(run *core.RecoveryRun) {
	if run.CompletedAt.IsZero() {
		run.CompletedAt = e.now()
	}
	if run.State == "" {
		run.State = core.RunFailed
	}
}

// beginStep opens a new step, records it on the run and emits a "started"
// event. It returns the step's index in run.Steps for the matching endStep.
func (e *Engine) beginStep(run *core.RecoveryRun, name string, state core.RunState) int {
	run.State = state
	started := e.now()
	run.Steps = append(run.Steps, core.Step{
		Name:      name,
		State:     state,
		Status:    core.StepRunning,
		StartedAt: started,
	})
	ev := eventFor(run)
	ev.At, ev.State, ev.Step = started, state, name
	ev.Status, ev.Message = core.StepRunning, humanStepStart(name)
	e.emit(ev)
	return len(run.Steps) - 1
}

// endStep closes a step opened by beginStep, records its outcome and emits
// a matching "ended" event.
func (e *Engine) endStep(run *core.RecoveryRun, idx int, status core.StepStatus, message string, err error) {
	st := &run.Steps[idx]
	st.CompletedAt = e.now()
	st.Duration = st.CompletedAt.Sub(st.StartedAt)
	st.Status = status
	st.Message = message
	ev := eventFor(run)
	ev.At, ev.State, ev.Step = st.CompletedAt, st.State, st.Name
	ev.Status, ev.Message = status, message
	if err != nil {
		st.Err = err.Error()
		ev.Err = err.Error()
	}
	e.emit(ev)
}

// humanStepStart renders the short "starting" message shown for a step.
func humanStepStart(name string) string {
	switch name {
	case StepDiscoverBackup:
		return "Looking for a backup to restore"
	case StepPrepareEnvironment:
		return "Preparing the isolated restore environment"
	case StepRestore:
		return "Restoring backup into a temporary workload"
	case StepStart:
		return "Starting the restored workload"
	case StepWaitForGuest:
		return "Waiting for the guest to come up"
	case StepRunChecks:
		return "Running checks"
	case StepCleanup:
		return "Cleaning up the temporary workload"
	default:
		return name
	}
}
