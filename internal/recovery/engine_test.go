package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// ---- test helpers -------------------------------------------------------

func newTestEngine(t *testing.T, hv core.HypervisorProvider, backups core.BackupProvider, checks CheckRunner, clock *fakeClock) *Engine {
	t.Helper()
	e, err := New(Deps{
		Hypervisor: hv,
		Backups:    backups,
		Checks:     checks,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        clock.Now,
		Sleep:      fakeSleepFn(clock),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func mustParsePlan(t *testing.T, yamlSrc string) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("parse plan: %v\n---\n%s", err, yamlSrc)
	}
	return p
}

func isolatedNetwork() core.NetworkConfig {
	return core.NetworkConfig{Bridge: "vmbr99", Isolated: true}
}

func hasCallPrefix(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const planWithChecksYAML = `
name: happy-plan
workload:
  provider: fake
  id: "100"
  name: web-01
backup:
  strategy: latest
restore:
  node: node-a
startup:
  timeout: 10s
checks:
  - type: tcp
    name: web-tcp
    port: 80
`

const planNoChecksYAML = `
name: simple-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
startup:
  timeout: 10s
`

const planSkipStartupYAML = `
name: skip-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
startup:
  skip: true
`

const planStaleBackupYAML = `
name: stale-plan
workload:
  provider: fake
  id: "100"
backup:
  strategy: latest
  max_age: 24h
restore:
  node: node-a
`

const planTwoChecksYAML = `
name: two-checks-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
startup:
  timeout: 10s
checks:
  - type: tcp
    name: critical-check
    port: 80
  - type: tcp
    name: optional-check
    port: 81
    critical: false
`

func planWithMemoryLimit(t *testing.T) *plan.Plan {
	return mustParsePlan(t, `
name: capacity-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
  memory_limit: 4096
startup:
  timeout: 10s
`)
}

func planWithKeepOnFailure(t *testing.T) *plan.Plan {
	return mustParsePlan(t, `
name: keep-on-failure-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
startup:
  timeout: 10s
checks:
  - type: tcp
    name: critical-check
    port: 80
cleanup:
  keep_on_failure: true
`)
}

// ---- happy path ----------------------------------------------------------

func TestRun_HappyPath(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr: "fake-hv",
		latestBackup: &core.Backup{
			ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour),
		},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning, IPs: []string{"10.0.0.5"}},
		},
	}
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "web-tcp", Type: "tcp", Status: core.CheckPass},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	wantSteps := []string{StepDiscoverBackup, StepPrepareEnvironment, StepRestore, StepStart, StepWaitForGuest, StepRunChecks, StepCleanup}
	if len(run.Steps) != len(wantSteps) {
		names := make([]string, len(run.Steps))
		for i, s := range run.Steps {
			names[i] = s.Name
		}
		t.Fatalf("step sequence = %v, want %v", names, wantSteps)
	}
	for i, name := range wantSteps {
		if run.Steps[i].Name != name {
			t.Errorf("step %d: name = %q, want %q", i, run.Steps[i].Name, name)
		}
		if run.Steps[i].Status != core.StepDone {
			t.Errorf("step %s: status = %s, want done", run.Steps[i].Name, run.Steps[i].Status)
		}
	}

	if run.State != core.RunSuccess {
		t.Errorf("run.State = %s, want %s", run.State, core.RunSuccess)
	}
	if run.Result != core.ResultSuccess {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultSuccess)
	}
	if !run.CleanupDone {
		t.Error("expected CleanupDone == true")
	}

	// The single most dangerous bug in this product: Delete must be called
	// with the TEMPORARY id, never the source workload id.
	if run.TempWorkloadID == "" {
		t.Fatal("expected a temp workload id to be recorded on the run")
	}
	if run.TempWorkloadID == p.Workload.ID {
		t.Fatalf("temp workload id (%s) must never equal the source workload id (%s)", run.TempWorkloadID, p.Workload.ID)
	}
	if got, want := hv.deleteCalls, []string{run.TempWorkloadID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Delete calls = %v, want exactly %v (cleanup called exactly once, with the temp id)", got, want)
	}

	wantRTO := stepEnd(run, StepRunChecks).Sub(run.StartedAt)
	if run.RTO != wantRTO {
		t.Errorf("run.RTO = %v, want %v (end of run_checks - StartedAt)", run.RTO, wantRTO)
	}
}

// ---- backup discovery ------------------------------------------------

func TestRun_NoBackup(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{idStr: "fake-hv", latestBackupErr: core.ErrNoBackup}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planNoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrNoBackup) {
		t.Errorf("err = %v, want it to wrap core.ErrNoBackup", err)
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if run.State != core.RunFailed {
		t.Errorf("run.State = %s, want %s", run.State, core.RunFailed)
	}
	if hasCallPrefix(hv.Calls(), "Restore(") || hasCallPrefix(hv.Calls(), "Delete(") {
		t.Fatalf("no restore/delete expected, got calls: %v", hv.Calls())
	}
}

func TestRun_StaleBackup(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr: "fake-hv",
		latestBackup: &core.Backup{
			ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-48 * time.Hour),
		},
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planStaleBackupYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if !strings.Contains(strings.ToUpper(run.Err), "STALE") {
		t.Errorf("run.Err = %q, want a prominent stale-backup message", run.Err)
	}
	if hasCallPrefix(hv.Calls(), "Restore(") {
		t.Fatalf("no restore expected for a stale backup, got calls: %v", hv.Calls())
	}
}

// ---- network isolation -------------------------------------------------

func TestRun_NonIsolatedNetworkRefused(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planNoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: core.NetworkConfig{Isolated: false}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrNetworkNotIsolated) {
		t.Errorf("err = %v, want it to wrap core.ErrNetworkNotIsolated", err)
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if hasCallPrefix(hv.Calls(), "AllocateWorkloadID") || hasCallPrefix(hv.Calls(), "Restore(") {
		t.Fatalf("no allocation/restore expected before a network refusal, got calls: %v", hv.Calls())
	}
}

func TestRun_NetworkValidatorRejects(t *testing.T) {
	clock := newFakeClock()
	base := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
	}
	hv := newFakeProviderWithExtras(base)
	hv.validateIsolationErr = core.ErrNetworkNotIsolated
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planNoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrNetworkNotIsolated) {
		t.Errorf("err = %v, want it to wrap core.ErrNetworkNotIsolated", err)
	}
	if !hv.validateIsolationCalled {
		t.Error("expected ValidateIsolation to have been called")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if hasCallPrefix(hv.Calls(), "AllocateWorkloadID") || hasCallPrefix(hv.Calls(), "Restore(") {
		t.Fatalf("no allocation/restore expected before a network refusal, got calls: %v", hv.Calls())
	}
}

// ---- capacity ------------------------------------------------------------

func TestRun_InsufficientCapacity(t *testing.T) {
	clock := newFakeClock()
	base := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
	}
	hv := newFakeProviderWithExtras(base)
	hv.nodeCapacity = &core.Node{
		MemoryTotalBytes: 8 * 1024 * 1024 * 1024,
		MemoryUsedBytes:  7 * 1024 * 1024 * 1024, // 1 GiB free
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := planWithMemoryLimit(t) // asks for 4096 MiB

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrInsufficientCapacity) {
		t.Errorf("err = %v, want it to wrap core.ErrInsufficientCapacity", err)
	}
	if !hv.nodeCapacityCalled {
		t.Error("expected NodeCapacity to have been called")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if hasCallPrefix(hv.Calls(), "AllocateWorkloadID") || hasCallPrefix(hv.Calls(), "Restore(") {
		t.Fatalf("no allocation/restore expected before a capacity refusal, got calls: %v", hv.Calls())
	}
}

// ---- restore failures ------------------------------------------------

func TestRun_RestoreTaskFailure(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		waitTasks:    []*core.TaskState{{Success: false, Message: "disk image corrupt"}},
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planNoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if run.TempWorkloadID == "" {
		t.Fatal("expected a temp workload id to have been allocated")
	}
	if got, want := hv.deleteCalls, []string{run.TempWorkloadID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Delete calls = %v, want cleanup still attempted for the allocated temp id %v", got, want)
	}
}

// ---- guest boot ------------------------------------------------------

func TestRun_GuestNeverGetsIP(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning}, // always running, never an IP
		},
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, `
name: timeout-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
startup:
  timeout: 5s
`)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, core.ErrTimeout) {
		t.Errorf("err = %v, want it to wrap core.ErrTimeout", err)
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if !run.CleanupDone {
		t.Error("expected cleanup to have run for the allocated temp id")
	}
	if got, want := hv.deleteCalls, []string{run.TempWorkloadID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Delete calls = %v, want %v", got, want)
	}
}

// ---- check grading -----------------------------------------------------

func TestRun_CriticalCheckFailure(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning, IPs: []string{"10.0.0.5"}},
		},
	}
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "critical-check", Type: "tcp", Status: core.CheckFail, Message: "connection refused"},
		{Name: "optional-check", Type: "tcp", Status: core.CheckPass},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planTwoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if !run.CleanupDone {
		t.Error("expected cleanup to still run after a critical check failure")
	}
}

func TestRun_NonCriticalCheckFailure_Degraded(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning, IPs: []string{"10.0.0.5"}},
		},
	}
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "critical-check", Type: "tcp", Status: core.CheckPass},
		{Name: "optional-check", Type: "tcp", Status: core.CheckFail, Message: "timed out"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planTwoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run returned error: %v, want nil (degraded is not an error)", err)
	}
	if run.Result != core.ResultDegraded {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultDegraded)
	}
	if run.State != core.RunSuccess {
		t.Errorf("run.State = %s, want %s", run.State, core.RunSuccess)
	}
}

func TestRun_RTOExceeded_Degraded(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning},                            // no IP yet
			{PowerState: core.PowerStateRunning},                            // still no IP
			{PowerState: core.PowerStateRunning, IPs: []string{"10.0.0.5"}}, // up, after two poll intervals
		},
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, `
name: rto-plan
workload:
  provider: fake
  id: "100"
restore:
  node: node-a
startup:
  timeout: 30s
rto_target: 2s
`)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run returned error: %v, want nil (degraded is not an error)", err)
	}
	if !run.RTOExceeded() {
		t.Fatalf("expected RTO (%v) to exceed target (%v)", run.RTO, run.RTOTarget)
	}
	if run.Result != core.ResultDegraded {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultDegraded)
	}
}

// ---- cleanup policy and failure -----------------------------------------

func TestRun_CleanupFailure(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		deleteErr:    errors.New("provider unreachable"),
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planSkipStartupYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if run.State != core.RunCleanupFailed {
		t.Errorf("run.State = %s, want %s", run.State, core.RunCleanupFailed)
	}
	if run.CleanupDone {
		t.Error("expected CleanupDone == false")
	}
	if !strings.Contains(run.Err, run.TempWorkloadID) {
		t.Errorf("run.Err = %q, want it to name the exact VMID %q", run.Err, run.TempWorkloadID)
	}
	if !strings.Contains(run.Err, "node-a") {
		t.Errorf("run.Err = %q, want it to name the node", run.Err)
	}
}

func TestRun_CtxCancelledMidRun_CleanupStillRuns(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:          "fake-hv",
		latestBackup:   &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		startChecksCtx: true,
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planNoChecksYAML)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Start is ever reached

	run, err := e.Run(ctx, p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error (the run was cancelled)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	// The proof that cleanup runs on its own detached context: it still
	// completes successfully even though the run's own ctx is cancelled.
	if !run.CleanupDone {
		t.Error("expected cleanup to still run (and succeed) despite the cancelled run ctx")
	}
	if got, want := hv.deleteCalls, []string{run.TempWorkloadID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Delete calls = %v, want %v", got, want)
	}
}

func TestRun_KeepOnFailure(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning, IPs: []string{"10.0.0.5"}},
		},
	}
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "critical-check", Type: "tcp", Status: core.CheckFail},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := planWithKeepOnFailure(t)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error (critical check failed)")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if run.CleanupDone {
		t.Error("expected CleanupDone == false: keep_on_failure must skip cleanup")
	}
	if len(hv.deleteCalls) != 0 {
		t.Fatalf("Delete calls = %v, want none (keep_on_failure)", hv.deleteCalls)
	}
}

func TestRun_KeepWorkload(t *testing.T) {
	clock := newFakeClock()
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
	}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planSkipStartupYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork(), KeepWorkload: true})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.CleanupDone {
		t.Error("expected CleanupDone == false: KeepWorkload must skip cleanup")
	}
	if len(hv.deleteCalls) != 0 {
		t.Fatalf("Delete calls = %v, want none (KeepWorkload)", hv.deleteCalls)
	}
}

// ---- retry helper --------------------------------------------------------

func TestEngine_Retry(t *testing.T) {
	clock := newFakeClock()
	e := newTestEngine(t, &fakeProvider{}, &fakeProvider{}, &fakeCheckRunner{}, clock)

	t.Run("retries a retryable error until success", func(t *testing.T) {
		calls := 0
		err := e.retry(context.Background(), 3, time.Second, func() error {
			calls++
			if calls < 3 {
				return core.Retryable(errors.New("transient"))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("retry returned error: %v", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry a non-retryable error", func(t *testing.T) {
		calls := 0
		sentinel := errors.New("boom")
		err := e.retry(context.Background(), 5, time.Second, func() error {
			calls++
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want %v", err, sentinel)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want exactly 1 (no retry for a non-retryable error)", calls)
		}
	})

	t.Run("gives up after attempts exhausted", func(t *testing.T) {
		calls := 0
		err := e.retry(context.Background(), 3, time.Millisecond, func() error {
			calls++
			return core.Retryable(errors.New("still failing"))
		})
		if err == nil {
			t.Fatal("expected an error")
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})
}

// ---- panic safety ---------------------------------------------------------

type panicOnStartProvider struct {
	*fakeProvider
}

func (p *panicOnStartProvider) Start(ctx context.Context, id string) error {
	panic("simulated provider panic")
}

func TestRun_PanicInsideProviderCall(t *testing.T) {
	clock := newFakeClock()
	base := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour)},
	}
	hv := &panicOnStartProvider{fakeProvider: base}
	e := newTestEngine(t, hv, hv, &fakeCheckRunner{}, clock)
	p := mustParsePlan(t, planNoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
	if !strings.Contains(run.Err, "internal error") {
		t.Errorf("run.Err = %q, want it to mention the recovered panic", run.Err)
	}
	if run.TempWorkloadID == "" {
		t.Fatal("expected a temp workload id to have been allocated before the panic")
	}
	if got, want := hv.deleteCalls, []string{run.TempWorkloadID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Delete calls = %v, want cleanup to still run after the panic: %v", got, want)
	}
	if !run.CleanupDone {
		t.Error("expected cleanup to have completed despite the panic")
	}
}
