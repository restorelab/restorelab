package recovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// discoverBackup resolves the backup a run will restore, per
// plan.Backup.Strategy, and enforces plan.Backup.MaxAge. A backup older than
// MaxAge is the "your backups are stale" signal this product exists to
// catch, so it fails the run immediately with a message an operator cannot
// miss — never a generic "check failed" wrapper.
// resolveSourceName fills in the production workload's real name for reports
// and check templates. It is best effort on purpose: in a genuine disaster the
// source workload may no longer exist, and that must not stop the drill that
// is meant to bring it back.
func (e *Engine) resolveSourceName(ctx context.Context, run *core.RecoveryRun) {
	w, err := e.hv.GetWorkload(ctx, run.SourceWorkloadID)
	if err != nil {
		e.log.Debug("source workload could not be read, keeping the name from the plan",
			"run_id", run.ID, "workload_id", run.SourceWorkloadID, "err", err)
		return
	}
	if w.Name != "" {
		run.SourceName = w.Name
	}
	if run.Node == "" {
		run.Node = w.Node
	}
}

func (e *Engine) discoverBackup(ctx context.Context, run *core.RecoveryRun, p *plan.Plan) error {
	idx := e.beginStep(run, StepDiscoverBackup, core.RunDiscoveringBackup)

	e.resolveSourceName(ctx, run)

	backup, err := e.resolveBackup(ctx, p)
	if err != nil {
		e.endStep(run, idx, core.StepFailed, "no usable backup found", err)
		return err
	}

	if maxAge := p.Backup.MaxAge.D(); maxAge > 0 {
		if age := e.now().Sub(backup.CreatedAt); age > maxAge {
			err := fmt.Errorf(
				"STALE BACKUP: newest backup for %s is %s old, plan requires max_age %s — recovery aborted, your backups are stale",
				p.Workload.ID, age.Round(time.Second), maxAge,
			)
			e.endStep(run, idx, core.StepFailed, err.Error(), err)
			return err
		}
	}

	run.Backup = backup
	e.endStep(run, idx, core.StepDone,
		fmt.Sprintf("using backup %s from %s", backup.ID, backup.CreatedAt.Format(time.RFC3339)), nil)
	return nil
}

// resolveBackup implements the two supported backup strategies. GetLatestBackup
// and ListBackups are read-only lookups; a not-found result surfaces
// verbatim as core.ErrNoBackup so callers can match on it.
func (e *Engine) resolveBackup(ctx context.Context, p *plan.Plan) (*core.Backup, error) {
	switch p.Backup.Strategy {
	case plan.StrategySpecific:
		backups, err := e.backups.ListBackups(ctx, p.Workload.ID)
		if err != nil {
			return nil, err
		}
		for _, b := range backups {
			if b.ID == p.Backup.ID {
				found := b
				return &found, nil
			}
		}
		return nil, core.ErrNoBackup
	default: // plan.StrategyLatest, and the plan-parsing default
		return e.backups.GetLatestBackup(ctx, p.Workload.ID)
	}
}

// prepareEnvironment validates the restore network and node capacity, then
// reserves a temporary workload identity: a fresh ID, a Proxmox-safe name
// and the ownership metadata every temporary workload is stamped with.
//
// Everything before AllocateWorkloadID is read-only. AllocateWorkloadID is
// the first call in the whole workflow with any side effect on the
// provider, which is exactly why runDryRun stops short of calling this and
// instead calls checkNetworkIsolation/checkCapacity directly.
func (e *Engine) prepareEnvironment(ctx context.Context, run *core.RecoveryRun, p *plan.Plan, opts RunOptions) (tempID, tempName string, metadata map[string]string, node string, err error) {
	idx := e.beginStep(run, StepPrepareEnvironment, core.RunPreparing)

	node, err = resolveNode(run, p, opts)
	if err != nil {
		e.endStep(run, idx, core.StepFailed, "no target node", err)
		return "", "", nil, node, err
	}

	if err = e.checkNetworkIsolation(ctx, node, opts.Network); err != nil {
		e.endStep(run, idx, core.StepFailed, "network isolation check failed", err)
		return "", "", nil, node, err
	}
	if err = e.checkCapacity(ctx, node, p.Restore.MemoryLimitMB); err != nil {
		e.endStep(run, idx, core.StepFailed, "capacity check failed", err)
		return "", "", nil, node, err
	}

	id, aerr := e.hv.AllocateWorkloadID(ctx)
	if aerr != nil {
		e.endStep(run, idx, core.StepFailed, "could not allocate a temporary workload id", aerr)
		return "", "", nil, node, aerr
	}
	tempID = id
	tempName = tempWorkloadName(p.Workload.ID, e.now())
	metadata = workloadMetadata(run.ID, p.Workload.ID, e.now())

	run.TempWorkloadID = tempID
	run.TempName = tempName
	run.Node = node

	e.endStep(run, idx, core.StepDone,
		fmt.Sprintf("allocated temporary workload %s (%s) on node %s", tempID, tempName, node), nil)
	return tempID, tempName, metadata, node, nil
}

// checkNetworkIsolation enforces that a restore can only ever land on a
// network proven isolated from production. opts.Network.Isolated == false is
// refused unconditionally — there is no plan-level override for this, by
// design: an isolated-by-default posture is the entire safety premise of
// restoring a copy of production data. When the provider can additionally
// prove isolation (core.NetworkValidator), that proof is required too.
func (e *Engine) checkNetworkIsolation(ctx context.Context, node string, network core.NetworkConfig) error {
	if !network.Isolated {
		return fmt.Errorf("%w: restore network is not marked isolated — refusing to restore production data onto it",
			core.ErrNetworkNotIsolated)
	}
	if nv, ok := e.hv.(core.NetworkValidator); ok {
		err := nv.ValidateIsolation(ctx, node, network)
		switch {
		case err == nil:
		case errors.Is(err, core.ErrIsolationUnverified):
			// The provider could not read the node's network configuration.
			// That is not evidence of danger, and refusing here would make the
			// product unusable wherever a token cannot see the bridge list.
			// The plan asserted this network is isolated; proceed on that
			// assertion, loudly, and let the report carry it.
			e.log.Warn("network isolation could not be verified, proceeding on the plan's assertion",
				"node", node, "bridge", network.Bridge, "err", err)
		default:
			return err
		}
	}
	return nil
}

// checkCapacity refuses a restore the target node cannot actually host, when
// the provider is able to report capacity. Providers that don't implement
// core.CapacityReporter, or plans that don't request a memory limit, skip
// the check rather than block a restore we have no way to evaluate.
func (e *Engine) checkCapacity(ctx context.Context, node string, memoryLimitMB int) error {
	if memoryLimitMB <= 0 {
		return nil
	}
	cr, ok := e.hv.(core.CapacityReporter)
	if !ok {
		return nil
	}
	n, err := cr.NodeCapacity(ctx, node)
	if err != nil {
		return err
	}
	needed := int64(memoryLimitMB) * 1024 * 1024
	if free := n.MemoryFreeBytes(); free < needed {
		return fmt.Errorf("%w: node %s has %s free, restore needs %s",
			core.ErrInsufficientCapacity, node, humanBytes(free), humanBytes(needed))
	}
	return nil
}

// finalizer is implemented by providers that need a post-restore hardening
// pass — the Proxmox provider uses it to rewrite the restored workload's
// network onto the isolated bridge and stamp ownership metadata, since
// Proxmox's restore API restores the original network config verbatim.
// Detected structurally so core stays provider-agnostic.
type finalizer interface {
	FinalizeRestore(ctx context.Context, opts core.RestoreOptions) error
}

// restoreWorkload submits the restore job, waits for it to settle and, when
// the provider supports it, runs FinalizeRestore before anything is started.
//
// needsCleanup is set to true the instant Restore returns successfully:
// from that point the provider may have begun materialising a workload
// under tempID even if the job later fails, so cleanup must run for it no
// matter what happens afterwards. Restore itself is never retried — it is
// not idempotent (it materialises disks against a specific target ID) and a
// transient failure must surface rather than risk being silently resubmitted
// against a half-populated target.
func (e *Engine) restoreWorkload(ctx context.Context, run *core.RecoveryRun, p *plan.Plan, opts RunOptions, tempID, tempName string, metadata map[string]string, node string, needsCleanup *bool) error {
	idx := e.beginStep(run, StepRestore, core.RunRestoring)

	restoreOpts := core.RestoreOptions{
		TargetWorkloadID: tempID,
		Name:             tempName,
		Node:             node,
		Storage:          firstNonEmpty(opts.Storage, p.Restore.Storage),
		Pool:             firstNonEmpty(opts.Pool, p.Restore.Pool),
		Network:          opts.Network,
		CPULimit:         p.Restore.CPULimit,
		MemoryLimitMB:    p.Restore.MemoryLimitMB,
		BandwidthKiBps:   p.Restore.BandwidthKiBps,
		Start:            false, // the engine drives Start itself as its own step
		Metadata:         metadata,
	}

	job, err := e.hv.Restore(ctx, *run.Backup, restoreOpts)
	if err != nil {
		e.endStep(run, idx, core.StepFailed, "restore submission failed", err)
		return err
	}
	*needsCleanup = true

	var task *core.TaskState
	waitErr := e.retry(ctx, 3, 2*time.Second, func() error {
		t, werr := e.hv.WaitForJob(ctx, job)
		if werr != nil {
			return werr
		}
		task = t
		return nil
	})
	if waitErr != nil {
		e.endStep(run, idx, core.StepFailed, "restore job wait failed", waitErr)
		return waitErr
	}
	if task == nil || !task.Success {
		msg := "restore task reported failure"
		if task != nil && task.Message != "" {
			msg = task.Message
		}
		terr := fmt.Errorf("restore failed: %s", msg)
		e.endStep(run, idx, core.StepFailed, msg, terr)
		return terr
	}

	if fin, ok := e.hv.(finalizer); ok {
		// Fatal on failure: an un-hardened workload (still on whatever
		// network the provider's restore path defaulted to) must never be
		// started.
		if ferr := fin.FinalizeRestore(ctx, restoreOpts); ferr != nil {
			e.endStep(run, idx, core.StepFailed, "post-restore hardening failed", ferr)
			return ferr
		}
	}

	e.endStep(run, idx, core.StepDone, fmt.Sprintf("restore completed onto %s", tempID), nil)
	return nil
}

// startWorkload powers on the restored workload, unless the plan opts for a
// restore-only drill.
func (e *Engine) startWorkload(ctx context.Context, run *core.RecoveryRun, p *plan.Plan, tempID string) error {
	idx := e.beginStep(run, StepStart, core.RunStarting)

	if p.Startup.Skip {
		e.endStep(run, idx, core.StepSkipped, "startup skipped by plan (restore-only drill)", nil)
		return nil
	}

	err := e.retry(ctx, 3, 2*time.Second, func() error { return e.hv.Start(ctx, tempID) })
	if err != nil {
		e.endStep(run, idx, core.StepFailed, "failed to power on the restored workload", err)
		return err
	}
	e.endStep(run, idx, core.StepDone, "workload powered on", nil)
	return nil
}

// waitForGuest polls GetStatus until the guest is up, or until
// plan.Startup.Timeout elapses. "Up" means powered on and, when
// plan.Startup.WaitForIP is set, an IP address available — unless
// plan.Startup.IP pins the address, which skips discovery entirely.
func (e *Engine) waitForGuest(ctx context.Context, run *core.RecoveryRun, p *plan.Plan, tempID string) (core.Target, error) {
	idx := e.beginStep(run, StepWaitForGuest, core.RunWaitingForGuest)

	if p.Startup.Skip {
		e.endStep(run, idx, core.StepSkipped, "startup skipped, guest was not booted", nil)
		return core.Target{WorkloadID: tempID, Node: run.Node, Name: run.SourceName, Exec: e.guestExecutor()}, nil
	}

	timeout := p.Startup.Timeout.D()
	deadline := e.now().Add(timeout)
	pollEvery := plan.DefaultGuestPollEvery

	var last *core.WorkloadStatus
	for {
		status, err := e.pollStatus(ctx, tempID)
		if err != nil {
			e.endStep(run, idx, core.StepFailed, "could not query guest status", err)
			return core.Target{}, err
		}
		last = status

		if guestReady(status, p) {
			ip := p.Startup.IP
			if ip == "" {
				ip = status.PrimaryIP()
			}
			target := core.Target{
				IP:         ip,
				IPs:        status.IPs,
				WorkloadID: tempID,
				Node:       run.Node,
				Name:       run.SourceName,
				Exec:       e.guestExecutor(),
			}
			e.endStep(run, idx, core.StepDone, fmt.Sprintf("guest is up%s", ipSuffix(ip)), nil)
			return target, nil
		}

		if !e.now().Before(deadline) {
			break
		}
		wait := pollEvery
		if remaining := deadline.Sub(e.now()); remaining < wait {
			wait = remaining
		}
		if serr := e.sleep(ctx, wait); serr != nil {
			err := fmt.Errorf("wait for guest interrupted: %w", serr)
			e.endStep(run, idx, core.StepFailed, "interrupted while waiting for guest", err)
			return core.Target{}, err
		}
	}

	err := fmt.Errorf("%w: guest not ready after %s (last seen: %s)", core.ErrTimeout, timeout, describeStatus(last))
	e.endStep(run, idx, core.StepFailed, err.Error(), err)
	return core.Target{}, err
}

// pollStatus wraps GetStatus with retry: it is a read-only call explicitly
// safe to repeat, so a single flaky poll doesn't fail the whole wait.
func (e *Engine) pollStatus(ctx context.Context, tempID string) (*core.WorkloadStatus, error) {
	var status *core.WorkloadStatus
	err := e.retry(ctx, 3, time.Second, func() error {
		s, gerr := e.hv.GetStatus(ctx, tempID)
		if gerr != nil {
			return gerr
		}
		status = s
		return nil
	})
	return status, err
}

func guestReady(s *core.WorkloadStatus, p *plan.Plan) bool {
	if s.PowerState != core.PowerStateRunning {
		return false
	}
	// In-guest checks travel through the agent, so the agent responding is
	// what "ready" means for them - an address is beside the point.
	if p.Startup.WaitsForAgent() && !s.AgentReady {
		return false
	}
	if p.Startup.IP != "" {
		return true // address pinned by the plan, discovery not required
	}
	if p.Startup.WaitsForIP() {
		return s.PrimaryIP() != ""
	}
	return true
}

// guestExecutor exposes in-guest command execution to the checks when the
// provider supports it. A provider that cannot do it yields nil, and the
// checks that need it say so plainly instead of reporting a healthy service
// as broken.
func (e *Engine) guestExecutor() core.GuestExecutor {
	if x, ok := e.hv.(core.GuestExecutor); ok {
		return x
	}
	return nil
}

func ipSuffix(ip string) string {
	if ip == "" {
		return ""
	}
	return fmt.Sprintf(" (%s)", ip)
}

func describeStatus(s *core.WorkloadStatus) string {
	if s == nil {
		return "no status"
	}
	agent := "agent=no"
	if s.AgentReady {
		agent = "agent=yes"
	}
	return fmt.Sprintf("power=%s %s ips=%v", s.PowerState, agent, s.IPs)
}

// runChecks runs every configured check against the recovered workload and
// fails the run when any critical check did not pass. Non-critical failures
// are recorded but graded as DEGRADED, not FAILED, by gradeSuccess. Skipped
// entirely (no step recorded at all) when the plan has no checks — the
// caller decides whether to call this.
func (e *Engine) runChecks(ctx context.Context, run *core.RecoveryRun, p *plan.Plan, target core.Target) error {
	idx := e.beginStep(run, StepRunChecks, core.RunRunningChecks)

	cfgs := make([]core.CheckConfig, len(p.Checks))
	for i, c := range p.Checks {
		cfgs[i] = c.ToCore()
	}

	results := e.checks.RunAll(ctx, target, cfgs)
	run.Checks = results

	for _, r := range results {
		cr := r
		ev := eventFor(run)
		ev.At, ev.State, ev.Step = e.now(), core.RunRunningChecks, StepRunChecks
		ev.Status, ev.Message, ev.Check = stepStatusForCheck(r.Status), checkMessage(r), &cr
		e.emit(ev)
	}

	critical := criticalMap(p)
	var failedCritical []string
	for _, r := range results {
		if !r.OK() && critical[r.Name] {
			failedCritical = append(failedCritical, r.Name)
		}
	}

	if len(failedCritical) > 0 {
		err := fmt.Errorf("critical check(s) failed: %v", failedCritical)
		e.endStep(run, idx, core.StepFailed, err.Error(), err)
		return err
	}

	e.endStep(run, idx, core.StepDone, fmt.Sprintf("%d check(s) completed", len(results)), nil)
	return nil
}

// runDryRun resolves the backup and validates the restore plan without
// creating anything. It reuses checkNetworkIsolation/checkCapacity directly
// rather than prepareEnvironment, specifically to avoid AllocateWorkloadID —
// the first call in the workflow with a provider-side side effect.
func (e *Engine) runDryRun(ctx context.Context, run *core.RecoveryRun, p *plan.Plan, opts RunOptions) error {
	if err := e.discoverBackup(ctx, run, p); err != nil {
		e.markFailed(run, err)
		return err
	}

	idx := e.beginStep(run, StepPrepareEnvironment, core.RunPreparing)
	node, err := resolveNode(run, p, opts)
	if err != nil {
		e.endStep(run, idx, core.StepFailed, "no target node", err)
		e.markFailed(run, err)
		return err
	}

	if err := e.checkNetworkIsolation(ctx, node, opts.Network); err != nil {
		e.endStep(run, idx, core.StepFailed, "network isolation check failed", err)
		e.markFailed(run, err)
		return err
	}
	if err := e.checkCapacity(ctx, node, p.Restore.MemoryLimitMB); err != nil {
		e.endStep(run, idx, core.StepFailed, "capacity check failed", err)
		e.markFailed(run, err)
		return err
	}

	run.Node = node
	e.endStep(run, idx, core.StepDone, "dry run: restore plan validated, nothing was created", nil)

	run.State = core.RunSuccess
	run.Result = core.ResultSuccess
	run.CleanupDone = true
	return nil
}

// resolveNode decides which node the restore lands on. run.Node was filled
// from the source workload during backup discovery, so restoring beside the
// original is the default: the storages and the bridge it needs are the ones
// that node already has.
func resolveNode(run *core.RecoveryRun, p *plan.Plan, opts RunOptions) (string, error) {
	node := firstNonEmpty(opts.Node, p.Restore.Node, run.Node)
	if node == "" && run.Backup != nil {
		node = run.Backup.Node
	}
	if node == "" {
		return "", errors.New("no node to restore onto: set restore.node in the plan, --node, or defaults.node in the config")
	}
	run.Node = node
	return node, nil
}
