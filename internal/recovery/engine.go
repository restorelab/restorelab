// Package recovery implements RestoreLab's recovery engine: the workflow
// that turns a plan.Plan into a proven (or disproven) recovery, end to end —
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
// safe, production-appropriate default when left nil — see New.
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
// as the workflow got — callers (the CLI, the report writer) build the
// report from this run even when the run failed. Run returns a non-nil error
// exactly when the run did not succeed: run.Result == core.ResultFailed, or
// cleanup itself failed (run.State == core.RunCleanupFailed, which can
// happen even after a graded Success/Degraded run — both are joined via
// errors.Join when they occur together). A core.ResultDegraded run with
// cleanup done returns a nil error — it recovered, just not perfectly.
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
		// errors.Is/As against core sentinels like core.ErrNoBackup) —
		// run.Err is only ever the flattened string form of it, which is
		// not enough to reconstruct the chain.
		workflowErr error
	)

	defer func() {
		if r := recover(); r != nil {
			// An orphaned VM is worse than a crash: turn the panic into a
			// failed run instead of letting it skip cleanup entirely.
			e.log.Error("panic recovered in recovery engine", "run_id", run.ID, "panic", r)
			perr := fmt.Errorf("internal error (recovered panic): %v", r)
			e.markFailed(run, perr)
			workflowErr = perr
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
		e.markFailed(run, cerr)
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

// gradeSuccess grades a run that completed the workflow without a fatal step
// error: SUCCESS when every critical check passed and the RTO target was
// met, DEGRADED when the workload recovered but a non-critical check failed
// or the RTO target was exceeded. A critical check failure never reaches
// here — runChecks turns that into a step error handled by markFailed.
func (e *Engine) gradeSuccess(run *core.RecoveryRun, p *plan.Plan) {
	degraded := run.RTOExceeded()
	if !degraded {
		critical := criticalMap(p)
		for _, c := range run.Checks {
			if !c.OK() && !critical[c.Name] {
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
	e.emit(Event{RunID: run.ID, At: started, State: state, Step: name, Status: core.StepRunning, Message: humanStepStart(name)})
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
	ev := Event{RunID: run.ID, At: st.CompletedAt, State: st.State, Step: st.Name, Status: status, Message: message}
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
