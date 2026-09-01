package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // for steal, the one thing no Store method may do

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// base is the worker's clock in every test. It is fixed on purpose: nothing
// the worker computes from the time of day then has more than one possible
// answer, so the lease it writes is the same on every run of the suite.
var base = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

// cluster is an in-memory hypervisor and backup source. Unlike the fake in
// internal/api, its destructive methods are real here - the worker restores
// for a living - but every one of them counts its calls, which is what lets a
// test assert what was done and, more importantly, what was not.
//
// It is a local copy rather than an import: a fake shared between packages
// ends up serving two needs and serving neither.
type cluster struct {
	mu sync.Mutex

	allocN       int
	restoresByID map[string]int // keyed by the run id stamped in the metadata
	restoreTotal int
	targets      []string // temporary workload ids handed to Restore
	started      []string
	stopped      []string
	deleted      []string
	waits        int

	inflight    int
	maxInflight int

	// noBackupFor makes GetLatestBackup fail for these source workloads, the
	// cheapest way to make a drill fail inside the engine.
	noBackupFor map[string]bool

	// gate, when non-nil, holds every Restore until it is closed. It is how a
	// test pins a known number of drills in flight at once.
	gate chan struct{}

	// blockWaitForJob keeps the restore job pending until the run's context
	// is cancelled. The restore itself has already succeeded by then, so the
	// temporary workload exists and must still be destroyed.
	blockWaitForJob bool
}

func newCluster() *cluster {
	return &cluster{
		restoresByID: map[string]int{},
		noBackupFor:  map[string]bool{},
	}
}

func (c *cluster) ID() string                 { return "pve" }
func (c *cluster) Kind() string               { return "fake" }
func (c *cluster) Ping(context.Context) error { return nil }

func (c *cluster) ListNodes(context.Context) ([]core.Node, error) {
	return []core.Node{{ID: "pve1", Online: true}}, nil
}

func (c *cluster) ListWorkloads(context.Context) ([]core.Workload, error) { return nil, nil }

func (c *cluster) GetWorkload(context.Context, string) (*core.Workload, error) {
	return nil, core.ErrNotFound
}

func (c *cluster) GetStatus(_ context.Context, id string) (*core.WorkloadStatus, error) {
	return &core.WorkloadStatus{ID: id, PowerState: core.PowerStateRunning}, nil
}

func (c *cluster) AllocateWorkloadID(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allocN++
	return fmt.Sprintf("9%03d", c.allocN), nil
}

func (c *cluster) Restore(ctx context.Context, _ core.Backup, opts core.RestoreOptions) (*core.RestoreJob, error) {
	runID := opts.Metadata[core.MetadataRecoveryRunID]

	c.mu.Lock()
	c.restoresByID[runID]++
	c.restoreTotal++
	c.targets = append(c.targets, opts.TargetWorkloadID)
	c.inflight++
	if c.inflight > c.maxInflight {
		c.maxInflight = c.inflight
	}
	gate := c.gate
	c.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			c.leaveRestore()
			return nil, ctx.Err()
		}
	}
	c.leaveRestore()

	return &core.RestoreJob{ID: "job-" + opts.TargetWorkloadID, WorkloadID: opts.TargetWorkloadID, Node: opts.Node}, nil
}

func (c *cluster) leaveRestore() {
	c.mu.Lock()
	c.inflight--
	c.mu.Unlock()
}

func (c *cluster) WaitForJob(ctx context.Context, job *core.RestoreJob) (*core.TaskState, error) {
	c.mu.Lock()
	c.waits++
	block := c.blockWaitForJob
	c.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &core.TaskState{ID: job.ID, Success: true}, nil
}

func (c *cluster) Start(_ context.Context, id string) error {
	c.mu.Lock()
	c.started = append(c.started, id)
	c.mu.Unlock()
	return nil
}

func (c *cluster) Stop(_ context.Context, id string) error {
	c.mu.Lock()
	c.stopped = append(c.stopped, id)
	c.mu.Unlock()
	return nil
}

func (c *cluster) Delete(_ context.Context, id string) error {
	c.mu.Lock()
	c.deleted = append(c.deleted, id)
	c.mu.Unlock()
	return nil
}

func (c *cluster) ListBackups(_ context.Context, workloadID string) ([]core.Backup, error) {
	b, err := c.GetLatestBackup(context.Background(), workloadID)
	if err != nil {
		return nil, err
	}
	return []core.Backup{*b}, nil
}

func (c *cluster) GetLatestBackup(_ context.Context, workloadID string) (*core.Backup, error) {
	c.mu.Lock()
	refuse := c.noBackupFor[workloadID]
	c.mu.Unlock()
	if refuse {
		return nil, core.ErrNoBackup
	}
	return &core.Backup{
		ID:         "backup-" + workloadID,
		WorkloadID: workloadID,
		Node:       "pve1",
		CreatedAt:  base.Add(-2 * time.Hour),
	}, nil
}

// restoresFor reports how many times this run reached the provider.
func (c *cluster) restoresFor(runID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restoresByID[runID]
}

func (c *cluster) totalRestores() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restoreTotal
}

// runIDsRestored names the runs the engine actually restored under, which is
// how a test can tell a queued id from one the engine minted for itself.
func (c *cluster) runIDsRestored() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.restoresByID))
	for id := range c.restoresByID {
		out = append(out, id)
	}
	return out
}

func (c *cluster) inflightNow() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight
}

func (c *cluster) peakInflight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxInflight
}

func (c *cluster) waitsSeen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits
}

func (c *cluster) wasDeleted(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.deleted {
		if d == id {
			return true
		}
	}
	return false
}

// providerSet hands the same cluster to the worker as both roles, the way a
// single Proxmox endpoint serves both in production.
type providerSet struct {
	hv core.HypervisorProvider
	bp core.BackupProvider
}

func (p providerSet) Hypervisor(string) (core.HypervisorProvider, error) { return p.hv, nil }
func (p providerSet) Backups(string) (core.BackupProvider, error)        { return p.bp, nil }

// drillPlan is the smallest plan that exercises the whole workflow without
// waiting for anything: a restore-only drill, which skips the boot and the
// guest wait and therefore has no timers in it at all.
func drillPlan(workloadID string) string {
	return fmt.Sprintf(`name: drill-%s
workload:
  provider: pve
  id: "%s"
restore:
  node: pve1
  network: isolated
startup:
  skip: true
`, workloadID, workloadID)
}

// history is a store plus the path it lives at.
//
// The path is here for steal alone. Reading a lease goes through
// store.RunLease; taking one away from its holder cannot, and must not: every
// query in the store either requires the caller to already hold the lease
// (RenewLease) or requires there to be none (ClaimRun), which is precisely
// the invariant this phase is built on. A store method that handed a claimed
// run to somebody else would be a way to run two engines against one drill,
// and it would exist in production to serve one test.
type history struct {
	store.Store
	path string
}

func newStore(t *testing.T) *history {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := store.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &history{Store: s, path: path}
}

// steal hands a claimed run to somebody else behind the worker's back. It is
// the only way to reproduce, in a test, the state a worker must never keep
// working through: another process believes it owns this drill.
func (h *history) steal(t *testing.T, runID, owner string) {
	t.Helper()

	db, err := sql.Open("sqlite", h.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open the history file: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`UPDATE runs SET lease_owner = ? WHERE id = ?`, owner, runID); err != nil {
		t.Fatalf("steal the lease of %s: %v", runID, err)
	}
}

func enqueue(t *testing.T, s store.Store, runID, workloadID, planYAML string, at time.Time) {
	t.Helper()
	run := &core.RecoveryRun{
		ID:               runID,
		PlanName:         "drill-" + workloadID,
		ProviderID:       "pve",
		SourceWorkloadID: workloadID,
		SourceName:       workloadID,
	}
	if err := s.Enqueue(context.Background(), run, planYAML, at); err != nil {
		t.Fatalf("Enqueue %s: %v", runID, err)
	}
}

// testWorker builds a worker whose every source of real time is injected: a
// fixed clock, a poll measured in milliseconds, and a renewal tick short
// enough that a cancellation is noticed inside a test rather than inside a
// coffee break.
func testWorker(t *testing.T, s store.Store, c *cluster, mutate func(*Options)) *Worker {
	t.Helper()

	opts := Options{
		Store:       s,
		Providers:   providerSet{hv: c, bp: c},
		Config:      config.New(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Owner:       "worker-test",
		Concurrency: 1,
		Lease:       time.Minute,
		Poll:        2 * time.Millisecond,
		Now:         func() time.Time { return base },
	}
	if mutate != nil {
		mutate(&opts)
	}

	w, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.renew = 5 * time.Millisecond
	return w
}

// start runs the worker in the background and returns the function that stops
// it and waits for it to finish, so a test never leaves a drill in flight.
func start(t *testing.T, w *Worker) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the worker did not stop within 30s")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitFor polls a condition up to a deadline. It is deliberately not a sleep:
// a test that sleeps and hopes becomes flaky the day CI runs it under -race.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForTerminal(t *testing.T, s store.Store, runIDs ...string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("runs %v to settle", runIDs), func() bool {
		for _, id := range runIDs {
			r, err := s.GetRun(context.Background(), id)
			if err != nil || !r.State.Terminal() {
				return false
			}
		}
		return true
	})
}

func getRun(t *testing.T, s store.Store, id string) *core.RecoveryRun {
	t.Helper()
	r, err := s.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun %s: %v", id, err)
	}
	return r
}

// assertLeaseReleased proves the worker let go of a run it had finished with:
// the expiry is cleared, and the owner is kept because which worker ran a
// drill is part of its history - and because a cleared owner would make the
// run claimable all over again.
//
// It asks the store rather than the file. StaleRuns cannot answer this - it
// skips every terminal run, which is exactly what a finished drill is - so
// until RunLease existed the only way to see a lease was to read its two
// columns, and a test that knows the column names breaks the day one is
// renamed while proving nothing to whoever reads the interface.
func assertLeaseReleased(t *testing.T, s store.Store, runIDs ...string) {
	t.Helper()
	for _, id := range runIDs {
		owner, expires, err := s.RunLease(context.Background(), id)
		if err != nil {
			t.Fatalf("RunLease %s: %v", id, err)
		}
		if !expires.IsZero() {
			t.Errorf("run %s still holds a lease expiry after finishing: %v", id, expires)
		}
		if owner == "" {
			t.Errorf("run %s lost its lease owner: it would be claimable again", id)
		}
	}
}

// Un drill mis en file est exécuté, une fois, et la ligne finit terminale.
func TestWorkerRunsAQueuedDrill(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	c := newCluster()
	enqueue(t, s, "run-1", "110", drillPlan("110"), base)

	w := testWorker(t, s, c, nil)
	stop := start(t, w)

	// Asserted before the run settles, because it is the one thing a timeout
	// would hide: the engine must run under the id the queue handed out. A
	// minted id would leave the journal writing into a row nobody reads.
	waitFor(t, "the drill to reach the provider", func() bool { return c.totalRestores() > 0 })
	if n := c.restoresFor("run-1"); n != 1 {
		t.Fatalf("the engine restored under %v, not under the queued id run-1", c.runIDsRestored())
	}

	waitForTerminal(t, s, "run-1")
	stop()

	r := getRun(t, s, "run-1")
	if r.State != core.RunSuccess {
		t.Fatalf("state = %s (%s), want SUCCESS", r.State, r.Err)
	}
	if r.Result != core.ResultSuccess {
		t.Errorf("result = %q, want SUCCESS", r.Result)
	}
	if !r.CleanupDone {
		t.Error("cleanup_done = false: the temporary workload was left on the cluster")
	}

	// Once, and under the id the queue handed out. A second restore would be a
	// second temporary workload; a different id would write the journal into
	// a row nobody is reading.
	if n := c.restoresFor("run-1"); n != 1 {
		t.Errorf("Restore ran %d time(s) for run-1, want exactly 1", n)
	}
	if n := c.totalRestores(); n != 1 {
		t.Errorf("%d restore(s) in total for a single queued drill", n)
	}

	if r.TempWorkloadID == "" {
		t.Fatal("the run names no temporary workload: nothing could reconcile it")
	}
	if !c.wasDeleted(r.TempWorkloadID) {
		t.Errorf("temporary workload %s was never deleted", r.TempWorkloadID)
	}
	if len(r.Steps) == 0 {
		t.Error("the run has no timeline: the journal recorded nothing")
	}

	// The journal wrote against the queued id rather than one the engine
	// minted for itself.
	events, err := s.Events(ctx, "run-1", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) == 0 {
		t.Error("no progress events recorded against the queued run id")
	}

	assertLeaseReleased(t, s, "run-1")

	// And the queue is empty: a finished run is never handed out again.
	if _, err := s.ClaimRun(ctx, "someone-else", time.Minute, base); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("ClaimRun after the drill = %v, want ErrNoWork", err)
	}
}

// Le test central du chantier : deux workers sur la même file n'exécutent
// jamais le même run.
func TestTwoWorkersNeverRunTheSameDrill(t *testing.T) {
	s := newStore(t)
	c := newCluster()

	const runs = 8
	ids := make([]string, 0, runs)
	for i := 0; i < runs; i++ {
		id := fmt.Sprintf("run-%02d", i)
		// One workload each: two queued drills of the same workload is a state
		// the API refuses to create.
		enqueue(t, s, id, fmt.Sprintf("%d", 200+i), drillPlan(fmt.Sprintf("%d", 200+i)),
			base.Add(time.Duration(i)*time.Second))
		ids = append(ids, id)
	}

	a := testWorker(t, s, c, func(o *Options) { o.Owner = "worker-a"; o.Concurrency = 2 })
	b := testWorker(t, s, c, func(o *Options) { o.Owner = "worker-b"; o.Concurrency = 2 })

	stopA := start(t, a)
	stopB := start(t, b)

	waitForTerminal(t, s, ids...)
	stopA()
	stopB()

	for _, id := range ids {
		if n := c.restoresFor(id); n != 1 {
			t.Errorf("run %s was restored %d time(s): a drill is destructive and not idempotent", id, n)
		}
		if r := getRun(t, s, id); r.State != core.RunSuccess {
			t.Errorf("run %s ended %s (%s), want SUCCESS", id, r.State, r.Err)
		}
	}
	if n := c.totalRestores(); n != runs {
		t.Errorf("%d restores for %d queued drills", n, runs)
	}
	assertLeaseReleased(t, s, ids...)
}

// La limite de concurrence est enfin réelle.
func TestWorkerHonoursMaxConcurrentRestores(t *testing.T) {
	s := newStore(t)
	c := newCluster()
	c.gate = make(chan struct{}) // every restore hangs until this closes

	const runs = 5
	ids := make([]string, 0, runs)
	for i := 0; i < runs; i++ {
		id := fmt.Sprintf("run-%d", i)
		enqueue(t, s, id, fmt.Sprintf("%d", 300+i), drillPlan(fmt.Sprintf("%d", 300+i)),
			base.Add(time.Duration(i)*time.Second))
		ids = append(ids, id)
	}

	cfg := config.New()
	cfg.Limits.MaxConcurrentRestores = 2
	w := testWorker(t, s, c, func(o *Options) {
		o.Config = cfg
		o.Concurrency = 0 // zero means "read the limit that has never been read"
	})

	if got := w.Concurrency(); got != 2 {
		t.Fatalf("Concurrency() = %d, want 2 from config.Limits.MaxConcurrentRestores", got)
	}

	stop := start(t, w)

	waitFor(t, "drills to be in flight", func() bool { return c.inflightNow() >= 2 })
	if n := c.inflightNow(); n != 2 {
		t.Fatalf("%d drills in flight at once, want 2: the cap is not enforced", n)
	}

	// The semaphore is full, so nothing else can even be claimed: with five
	// drills queued, exactly three rows are still untouched. This is a state
	// that cannot drift while the gate is held, not a snapshot taken on hope.
	queued := 0
	for _, id := range ids {
		if getRun(t, s, id).State == core.RunQueued {
			queued++
		}
	}
	if queued != runs-2 {
		t.Fatalf("%d run(s) still queued while 2 are in flight, want %d: the cap let more through", queued, runs-2)
	}

	close(c.gate)
	waitForTerminal(t, s, ids...)
	stop()

	if peak := c.peakInflight(); peak != 2 {
		t.Fatalf("peak concurrent restores = %d, want 2", peak)
	}
	if n := c.totalRestores(); n != runs {
		t.Errorf("%d restores for %d queued drills", n, runs)
	}
}

// Une annulation demandée pendant l'exécution arrête le run et détruit quand
// même le workload temporaire.
func TestWorkerCancelsARunningDrillAndStillCleansUp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	c := newCluster()
	// The restore itself succeeds - the temporary workload exists - and the
	// job then hangs. That is the only interesting moment to cancel at: there
	// is now something on the cluster that must be destroyed anyway.
	c.blockWaitForJob = true
	enqueue(t, s, "run-c", "110", drillPlan("110"), base)

	w := testWorker(t, s, c, nil)
	stop := start(t, w)

	waitFor(t, "the restore job to be pending", func() bool { return c.waitsSeen() > 0 })

	settled, err := s.RequestCancel(ctx, "run-c", base)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if settled {
		t.Fatal("a drill in flight was settled on the spot: only the worker can stop one that has created something")
	}

	waitForTerminal(t, s, "run-c")
	stop()

	r := getRun(t, s, "run-c")
	if r.State != core.RunCancelled {
		t.Fatalf("state = %s (%s), want CANCELLED", r.State, r.Err)
	}
	if r.Result != "" {
		t.Errorf("result = %q, want empty: a stopped drill proves nothing either way", r.Result)
	}
	if r.TempWorkloadID == "" {
		t.Fatal("the cancelled run names no temporary workload")
	}
	if !c.wasDeleted(r.TempWorkloadID) {
		t.Fatalf("temporary workload %s survived the cancellation: cancelling became a way to leak a VM", r.TempWorkloadID)
	}
	if !r.CleanupDone {
		t.Error("cleanup_done = false on a cancelled run")
	}
	assertLeaseReleased(t, s, "run-c")
}

// Perdre le bail arrête le drill : un autre processus croit posséder ce run.
//
// Not one of the five the plan names, but the plan calls the behaviour out by
// itself, and it was the one branch of holdLease nothing exercised. Two
// engines restoring the same run is precisely the state the claim exists to
// prevent, and a worker that kept going after losing its claim would be how
// to reach it.
func TestWorkerStopsADrillWhoseLeaseWasTakenAway(t *testing.T) {
	s := newStore(t)
	c := newCluster()
	c.blockWaitForJob = true
	enqueue(t, s, "run-s", "110", drillPlan("110"), base)

	w := testWorker(t, s, c, nil)
	stop := start(t, w)

	waitFor(t, "the restore job to be pending", func() bool { return c.waitsSeen() > 0 })
	s.steal(t, "run-s", "another-worker")

	waitForTerminal(t, s, "run-s")
	stop()

	r := getRun(t, s, "run-s")
	if r.State != core.RunCancelled {
		t.Fatalf("state = %s (%s), want CANCELLED: the drill kept running without its claim", r.State, r.Err)
	}
	if n := c.restoresFor("run-s"); n != 1 {
		t.Errorf("Restore ran %d time(s) for run-s", n)
	}
	if !c.wasDeleted(r.TempWorkloadID) {
		t.Errorf("temporary workload %s was left behind by a drill that lost its lease", r.TempWorkloadID)
	}
}

// Un run qui échoue libère son bail : le worker ne se bloque pas dessus.
func TestWorkerReleasesTheLeaseOnFailure(t *testing.T) {
	t.Run("the drill fails inside the engine", func(t *testing.T) {
		s := newStore(t)
		c := newCluster()
		c.noBackupFor["666"] = true

		enqueue(t, s, "run-bad", "666", drillPlan("666"), base)
		enqueue(t, s, "run-good", "110", drillPlan("110"), base.Add(time.Second))

		w := testWorker(t, s, c, nil)
		stop := start(t, w)
		waitForTerminal(t, s, "run-bad", "run-good")
		stop()

		bad := getRun(t, s, "run-bad")
		if bad.State != core.RunFailed {
			t.Fatalf("state = %s, want FAILED", bad.State)
		}
		if bad.Err == "" {
			t.Error("a failed run recorded no error")
		}
		if n := c.restoresFor("run-bad"); n != 0 {
			t.Errorf("Restore ran %d time(s) for a drill with no backup", n)
		}

		// The point of the test: the worker took the next run rather than
		// sitting on a lease it could no longer use.
		if good := getRun(t, s, "run-good"); good.State != core.RunSuccess {
			t.Fatalf("the next run ended %s (%s), want SUCCESS: the worker blocked on the failure",
				good.State, good.Err)
		}
		assertLeaseReleased(t, s, "run-bad", "run-good")
	})

	t.Run("the stored plan cannot be read", func(t *testing.T) {
		s := newStore(t)
		c := newCluster()

		enqueue(t, s, "run-junk", "110", "::: not a plan :::", base)
		enqueue(t, s, "run-next", "111", drillPlan("111"), base.Add(time.Second))

		w := testWorker(t, s, c, nil)
		stop := start(t, w)
		waitForTerminal(t, s, "run-junk", "run-next")
		stop()

		junk := getRun(t, s, "run-junk")
		if junk.State != core.RunFailed {
			t.Fatalf("state = %s, want FAILED: a claimed run must never be left non-terminal", junk.State)
		}
		if n := c.totalRestores(); n != 1 {
			t.Errorf("%d restores, want 1: the unreadable plan reached the provider", n)
		}
		if next := getRun(t, s, "run-next"); next.State != core.RunSuccess {
			t.Fatalf("the next run ended %s (%s), want SUCCESS", next.State, next.Err)
		}
		assertLeaseReleased(t, s, "run-junk", "run-next")
	})
}

// A plan describes a drill, not a deployment: it names the workload and what
// to check, and leaves node, storage and pool to whoever executes it. The CLI
// has always filled them in from the configuration; the worker did not, and
// every drill triggered over HTTP failed on a real cluster because of it.
//
// The pool is the one that bites. A least-privilege service account holds its
// destructive rights on the drill pool alone, so a restore landing outside it
// is refused with a bare "Permission check failed" - which reads like a
// broken token rather than a misplaced VM.
func TestWorkerFillsInPlacementTheConfigurationDecides(t *testing.T) {
	cfg := config.New()
	cfg.Providers = []config.Provider{{
		ID: "pve", Kind: "proxmox", Endpoint: "https://pve.example.com:8006",
		Pool: "restorelab",
	}}
	cfg.Defaults.Provider = "pve"
	cfg.Defaults.Node = "pve1"
	cfg.Defaults.Storage = "local-zfs"

	w, err := New(Options{Store: store.Noop{}, Providers: staticProviders{}, Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := w.placement("pve", "", deploymentPool); got != "restorelab" {
		t.Errorf("pool = %q, want restorelab: the provider entry decides where a drill is allowed to run", got)
	}
	if got := w.placement("pve", "", deploymentNode); got != "pve1" {
		t.Errorf("node = %q, want pve1", got)
	}
	if got := w.placement("pve", "", deploymentStorage); got != "local-zfs" {
		t.Errorf("storage = %q, want local-zfs", got)
	}

	// What the plan says wins: it was written by someone who meant it.
	if got := w.placement("pve", "other-pool", deploymentPool); got != "other-pool" {
		t.Errorf("pool = %q, want the plan's own value", got)
	}
	if got := w.placement("unknown-provider", "", deploymentPool); got != "" {
		t.Errorf("pool = %q, want empty for a provider that is not configured", got)
	}
}
