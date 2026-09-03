package recovery

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// fakeProvider is an in-memory core.HypervisorProvider + core.BackupProvider
// used by engine tests. Every behaviour is scriptable through plain fields
// (populate only what a given test needs: the zero value is a "happy path"
// stub); every call is recorded so tests can assert exactly what the engine
// did and did not do to the provider.
type fakeProvider struct {
	mu    sync.Mutex
	calls []string

	idStr string

	// backups
	latestBackup    *core.Backup
	latestBackupErr error
	backups         []core.Backup
	listBackupsErr  error

	// AllocateWorkloadID
	allocIDs []string // popped in order; falls back to a counter once exhausted
	allocN   int
	allocErr error

	// Restore
	restoreErr  error
	restoreJob  *core.RestoreJob
	restoreOpts []core.RestoreOptions // every call, recorded

	// WaitForJob: waitTasks/waitErrs are consumed together, index by index;
	// once exhausted the last entry of waitTasks repeats. Default (both
	// nil) is an immediate success.
	waitTasks []*core.TaskState
	waitErrs  []error
	waitN     int

	// GetStatus: statuses[i] answers the i-th call, clamped to the last
	// entry once exhausted. Default (nil) is "running, no IP".
	statuses   []core.WorkloadStatus
	statusErrs []error // aligned with statuses by index; nil entry = no error
	statusN    int

	startErr       error
	startChecksCtx bool // when true, Start fails with ctx.Err() if ctx is done
	stopErr        error
	deleteErr      error
	deleteCalls    []string
}

func (f *fakeProvider) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

// Calls returns a snapshot of every provider call made, in order.
func (f *fakeProvider) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeProvider) ID() string                     { return f.idStr }
func (f *fakeProvider) Kind() string                   { return "fake" }
func (f *fakeProvider) Ping(ctx context.Context) error { return nil }

func (f *fakeProvider) ListNodes(ctx context.Context) ([]core.Node, error) { return nil, nil }

func (f *fakeProvider) ListWorkloads(ctx context.Context) ([]core.Workload, error) {
	return nil, nil
}

func (f *fakeProvider) GetWorkload(ctx context.Context, id string) (*core.Workload, error) {
	return nil, core.ErrNotFound
}

func (f *fakeProvider) GetStatus(ctx context.Context, id string) (*core.WorkloadStatus, error) {
	f.record(fmt.Sprintf("GetStatus(%s)", id))

	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.statusN
	f.statusN++

	if i < len(f.statusErrs) && f.statusErrs[i] != nil {
		return nil, f.statusErrs[i]
	}

	var st core.WorkloadStatus
	switch {
	case len(f.statuses) == 0:
		st = core.WorkloadStatus{ID: id, PowerState: core.PowerStateRunning}
	case i < len(f.statuses):
		st = f.statuses[i]
	default:
		st = f.statuses[len(f.statuses)-1]
	}
	return &st, nil
}

func (f *fakeProvider) AllocateWorkloadID(ctx context.Context) (string, error) {
	f.record("AllocateWorkloadID")
	if f.allocErr != nil {
		return "", f.allocErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allocN < len(f.allocIDs) {
		id := f.allocIDs[f.allocN]
		f.allocN++
		return id, nil
	}
	id := fmt.Sprintf("9%03d", 900+f.allocN)
	f.allocN++
	return id, nil
}

func (f *fakeProvider) Restore(ctx context.Context, backup core.Backup, opts core.RestoreOptions) (*core.RestoreJob, error) {
	f.record(fmt.Sprintf("Restore(%s)", opts.TargetWorkloadID))
	f.mu.Lock()
	f.restoreOpts = append(f.restoreOpts, opts)
	f.mu.Unlock()

	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	if f.restoreJob != nil {
		return f.restoreJob, nil
	}
	return &core.RestoreJob{ID: "job-1", WorkloadID: opts.TargetWorkloadID, Node: opts.Node}, nil
}

func (f *fakeProvider) WaitForJob(ctx context.Context, job *core.RestoreJob) (*core.TaskState, error) {
	f.record("WaitForJob")
	f.mu.Lock()
	i := f.waitN
	f.waitN++
	f.mu.Unlock()

	if i < len(f.waitErrs) && f.waitErrs[i] != nil {
		return nil, f.waitErrs[i]
	}
	if len(f.waitTasks) == 0 {
		return &core.TaskState{ID: job.ID, Success: true}, nil
	}
	if i < len(f.waitTasks) {
		return f.waitTasks[i], nil
	}
	return f.waitTasks[len(f.waitTasks)-1], nil
}

func (f *fakeProvider) Start(ctx context.Context, id string) error {
	f.record(fmt.Sprintf("Start(%s)", id))
	if f.startChecksCtx {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return f.startErr
}

func (f *fakeProvider) Stop(ctx context.Context, id string) error {
	f.record(fmt.Sprintf("Stop(%s)", id))
	return f.stopErr
}

func (f *fakeProvider) Delete(ctx context.Context, id string) error {
	f.record(fmt.Sprintf("Delete(%s)", id))
	f.mu.Lock()
	f.deleteCalls = append(f.deleteCalls, id)
	f.mu.Unlock()
	return f.deleteErr
}

func (f *fakeProvider) ListBackups(ctx context.Context, workloadID string) ([]core.Backup, error) {
	f.record("ListBackups")
	if f.listBackupsErr != nil {
		return nil, f.listBackupsErr
	}
	return f.backups, nil
}

func (f *fakeProvider) GetLatestBackup(ctx context.Context, workloadID string) (*core.Backup, error) {
	f.record("GetLatestBackup")
	if f.latestBackupErr != nil {
		return nil, f.latestBackupErr
	}
	if f.latestBackup == nil {
		return nil, core.ErrNoBackup
	}
	return f.latestBackup, nil
}

// fakeProviderWithExtras embeds fakeProvider and additionally implements
// core.NetworkValidator, core.CapacityReporter and the engine's local
// finalizer interface, all independently scriptable. Kept as a distinct
// type (rather than always-present methods on fakeProvider) so tests can
// exercise the "provider doesn't implement this optional interface" path
// with a plain *fakeProvider.
type fakeProviderWithExtras struct {
	*fakeProvider

	validateIsolationErr    error
	validateIsolationCalled bool

	nodeCapacity       *core.Node
	nodeCapacityErr    error
	nodeCapacityCalled bool

	finalizeErr    error
	finalizeCalled bool
	finalizeOpts   core.RestoreOptions
}

func newFakeProviderWithExtras(base *fakeProvider) *fakeProviderWithExtras {
	return &fakeProviderWithExtras{fakeProvider: base}
}

func (f *fakeProviderWithExtras) ValidateIsolation(ctx context.Context, node string, network core.NetworkConfig) error {
	f.mu.Lock()
	f.validateIsolationCalled = true
	f.mu.Unlock()
	return f.validateIsolationErr
}

func (f *fakeProviderWithExtras) NodeCapacity(ctx context.Context, node string) (*core.Node, error) {
	f.mu.Lock()
	f.nodeCapacityCalled = true
	f.mu.Unlock()
	if f.nodeCapacityErr != nil {
		return nil, f.nodeCapacityErr
	}
	return f.nodeCapacity, nil
}

func (f *fakeProviderWithExtras) FinalizeRestore(ctx context.Context, opts core.RestoreOptions) error {
	f.mu.Lock()
	f.finalizeCalled = true
	f.finalizeOpts = opts
	f.mu.Unlock()
	return f.finalizeErr
}

// fakeCheckRunner is a scriptable CheckRunner.
type fakeCheckRunner struct {
	results []core.CheckResult
	fn      func(ctx context.Context, target core.Target, cfgs []core.CheckConfig) []core.CheckResult
	calls   int
}

func (f *fakeCheckRunner) RunAll(ctx context.Context, target core.Target, cfgs []core.CheckConfig) []core.CheckResult {
	f.calls++
	if f.fn != nil {
		return f.fn(ctx, target, cfgs)
	}
	return f.results
}

// fakeClock is a manually-advanced clock used as Deps.Now in tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeSleepFn returns a Deps.Sleep implementation that advances clock
// instantly instead of actually blocking, so tests exercising timeouts and
// poll loops run instantly, while still honouring ctx cancellation.
func fakeSleepFn(clock *fakeClock) func(ctx context.Context, d time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.Advance(d)
		return nil
	}
}
