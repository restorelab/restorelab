package store

// La conformité de la file. Comme le reste, elle tourne contre les deux
// moteurs, et ici ce n'est pas une précaution de principe : le claim est la
// seule requête du projet écrite deux fois, une par moteur. Un test qui ne
// tournerait que sur SQLite ne prouverait rien de ce qui compte.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// queuedRun builds a run as the API would insert it: an id, a workload, and
// nothing else decided yet.
func queuedRun(id, workloadID string) *core.RecoveryRun {
	return &core.RecoveryRun{
		ID:               id,
		PlanName:         "adhoc-" + workloadID,
		ProviderID:       "proxmox-main",
		SourceWorkloadID: workloadID,
		State:            core.RunQueued,
	}
}

// QueueWriteConformance covers everything the queue writes that is not the
// claim itself.
func QueueWriteConformance(t *testing.T, open OpenFunc) {
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	t.Run("an enqueued run is readable and QUEUED", func(t *testing.T) {
		s := open(t)
		run := queuedRun("q1", "110")

		if err := s.Enqueue(ctx, run, "name: adhoc-110\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		got, err := s.GetRun(ctx, "q1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.State != core.RunQueued {
			t.Errorf("State = %q, want QUEUED: the queue is the runs table, and a queued run must say so", got.State)
		}
		if got.SourceWorkloadID != "110" {
			t.Errorf("SourceWorkloadID = %q, want 110", got.SourceWorkloadID)
		}
	})

	t.Run("a queued run keeps the provenance it was queued with", func(t *testing.T) {
		s := open(t)

		// A drill queued from the catalogue records which plan and which
		// version produced it. The worker that picks it up later executes the
		// snapshot, never this - but the answer to "where did this run come
		// from" has to survive the wait, because nothing downstream can
		// reconstruct it once the drill is running.
		p := samplePlan("8c4e2b17-5d93-4a60-b1f8-2e70c9d4a615", "queued-from-catalogue")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		run := queuedRun("q6", "110")
		run.PlanID = p.ID
		run.PlanVersion = 2
		if err := s.Enqueue(ctx, run, "name: web-tier\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		got, err := s.GetRun(ctx, "q6")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.PlanID != p.ID || got.PlanVersion != 2 {
			t.Errorf("provenance = %q/v%d, want %q/v2", got.PlanID, got.PlanVersion, p.ID)
		}
	})

	t.Run("an ad-hoc run is queued with no provenance at all", func(t *testing.T) {
		s := open(t)

		// The other half of the same rule: a drill nobody catalogued must not
		// acquire a plan id or a version 0 on the way in. Both columns stay
		// NULL, and read back as the zero value.
		if err := s.Enqueue(ctx, queuedRun("q7", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		got, err := s.GetRun(ctx, "q7")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.PlanID != "" || got.PlanVersion != 0 {
			t.Errorf("provenance = %q/v%d, want it empty for an ad-hoc drill", got.PlanID, got.PlanVersion)
		}
	})

	t.Run("the state moves as the drill progresses", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("q2", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		if err := s.SetState(ctx, "q2", core.RunRestoring); err != nil {
			t.Fatalf("SetState: %v", err)
		}
		got, err := s.GetRun(ctx, "q2")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.State != core.RunRestoring {
			t.Errorf("State = %q, want RESTORING", got.State)
		}
	})

	t.Run("a settled run keeps the verdict it settled on", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("q2b", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := s.SetState(ctx, "q2b", core.RunFailed); err != nil {
			t.Fatalf("SetState: %v", err)
		}

		// Two workers sweeping the same interrupted run both try to clean it
		// up; the one that loses is refused by the provider and would report
		// an orphan that the winner had already removed. ORPHANED WORKLOAD is
		// the loudest thing this product says, and it has to stay true.
		if err := s.SetState(ctx, "q2b", core.RunCleanupFailed); err != nil {
			t.Fatalf("SetState over a settled run must be a silent no-op, got: %v", err)
		}
		got, err := s.GetRun(ctx, "q2b")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.State != core.RunFailed {
			t.Errorf("State = %q, want FAILED: the first writer decides", got.State)
		}
	})

	t.Run("a run's error is written on its own, without touching anything else", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("q5", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := s.SetState(ctx, "q5", core.RunFailed); err != nil {
			t.Fatalf("SetState: %v", err)
		}

		// This is how an interrupted run says why it died: reconciliation
		// never loaded it as a full run, so UpdateRun is not available to it.
		const why = "interrupted: the worker running this drill did not survive"
		if err := s.SetRunError(ctx, "q5", why); err != nil {
			t.Fatalf("SetRunError: %v", err)
		}

		got, err := s.GetRun(ctx, "q5")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Err != why {
			t.Errorf("Err = %q, want %q", got.Err, why)
		}
		if got.State != core.RunFailed {
			t.Errorf("State = %q, want FAILED: SetRunError must not touch it", got.State)
		}
		if got.SourceWorkloadID != "110" {
			t.Errorf("SourceWorkloadID = %q, want 110: SetRunError must not touch it", got.SourceWorkloadID)
		}
	})

	t.Run("setting the error of an unknown run is not an error", func(t *testing.T) {
		s := open(t)
		// Best-effort, like SetTempWorkload: the caller has a run id and a
		// reason, not a reason to abort.
		if err := s.SetRunError(ctx, "does-not-exist", "boom"); err != nil {
			t.Fatalf("SetRunError on an unknown run = %v, want nil", err)
		}
	})

	t.Run("a workload with an active run is reported, a finished one is not", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("q3", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		// This is the check that stops two drills of the same workload from
		// restoring the same backup at once.
		id, err := s.ActiveRunForWorkload(ctx, "110")
		if err != nil {
			t.Fatalf("ActiveRunForWorkload: %v", err)
		}
		if id != "q3" {
			t.Fatalf("active run = %q, want q3", id)
		}

		if id, err := s.ActiveRunForWorkload(ctx, "104"); err != nil || id != "" {
			t.Fatalf("active run for an idle workload = %q, %v; want empty", id, err)
		}

		if err := s.SetState(ctx, "q3", core.RunSuccess); err != nil {
			t.Fatalf("SetState: %v", err)
		}
		if id, err := s.ActiveRunForWorkload(ctx, "110"); err != nil || id != "" {
			t.Fatalf("a terminal run still counts as active: %q, %v", id, err)
		}
	})

	t.Run("cancelling a queued run settles it without a worker", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("q4", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		// Nothing has been created on any cluster, so there is nothing to
		// clean up and no worker to ask: the run settles here and now.
		settled, err := s.RequestCancel(ctx, "q4", base)
		if err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if !settled {
			t.Fatal("cancelling an unclaimed queued run must settle it immediately")
		}
		got, err := s.GetRun(ctx, "q4")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.State != core.RunCancelled {
			t.Errorf("State = %q, want CANCELLED", got.State)
		}
	})

	t.Run("cancelling an unknown run is not found", func(t *testing.T) {
		s := open(t)
		if _, err := s.RequestCancel(ctx, "nope", base); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RequestCancel on an unknown run = %v, want ErrNotFound", err)
		}
	})

	t.Run("cancelling an already settled run is refused with a sentinel", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("q5", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := s.SetState(ctx, "q5", core.RunSuccess); err != nil {
			t.Fatalf("SetState: %v", err)
		}

		_, err := s.RequestCancel(ctx, "q5", base)
		if !errors.Is(err, ErrAlreadySettled) {
			t.Fatalf("RequestCancel on a settled run = %v, want ErrAlreadySettled", err)
		}
	})

	t.Run("a token carries its scopes", func(t *testing.T) {
		s := open(t)
		tok := APIToken{ID: "t1", Name: "dash", Hash: "h1", CreatedAt: base, Scopes: []string{ScopeRead}}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}

		got, err := s.TokenByHash(ctx, "h1")
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if len(got.Scopes) != 1 || got.Scopes[0] != ScopeRead {
			t.Errorf("Scopes = %v, want [read]", got.Scopes)
		}
		if got.Can(ScopeOperate) {
			t.Error("a read token may not operate")
		}
	})

	t.Run("a token created without scopes can only read", func(t *testing.T) {
		s := open(t)
		// The B1 tokens are exactly this case after migration 0003: no scope
		// was ever written for them, and the column defaults to read.
		if err := s.CreateToken(ctx, APIToken{ID: "t2", Name: "old", Hash: "h2", CreatedAt: base}); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		got, err := s.TokenByHash(ctx, "h2")
		if err != nil {
			t.Fatalf("TokenByHash: %v", err)
		}
		if got.Can(ScopeOperate) {
			t.Fatal("a token with no scopes recorded was granted operate: a migration must never widen a right")
		}
		if !got.Can(ScopeRead) {
			t.Error("a token with no scopes recorded cannot even read")
		}
	})
}

// QueueClaimConformance is the reason this suite runs against both engines.
// The claim is the one query in the project written twice - FOR UPDATE SKIP
// LOCKED on PostgreSQL, BEGIN IMMEDIATE on SQLite - and only a test that runs
// on both can say they behave the same.
func QueueClaimConformance(t *testing.T, open OpenFunc) {
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	const lease = time.Minute

	t.Run("claiming takes the oldest queued run", func(t *testing.T) {
		s := open(t)
		for i, id := range []string{"c1", "c2", "c3"} {
			run := queuedRun(id, "110")
			if err := s.Enqueue(ctx, run, "name: x\n", base.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatalf("Enqueue %s: %v", id, err)
			}
		}

		got, err := s.ClaimRun(ctx, "worker-a", lease, base)
		if err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}
		if got.ID != "c1" {
			t.Errorf("claimed %q, want the oldest (c1)", got.ID)
		}
		if got.PlanSnapshot == "" {
			t.Error("the claimed run carries no plan: the worker has nothing to execute")
		}
	})

	t.Run("an empty queue reports no work", func(t *testing.T) {
		s := open(t)
		if _, err := s.ClaimRun(ctx, "worker-a", lease, base); !errors.Is(err, ErrNoWork) {
			t.Fatalf("ClaimRun on an empty queue = %v, want ErrNoWork", err)
		}
	})

	t.Run("a claimed run is never claimable again, even after its lease expires", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("c4", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := s.ClaimRun(ctx, "worker-a", lease, base); err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}

		// Long after the lease died. This is the invariant of the whole
		// phase: a drill is destructive and not idempotent, so an interrupted
		// run is never re-run. Reconciliation fails it; nothing revives it.
		if _, err := s.ClaimRun(ctx, "worker-b", lease, base.Add(time.Hour)); !errors.Is(err, ErrNoWork) {
			t.Fatalf("a run whose worker died was claimed again: %v", err)
		}
	})

	t.Run("concurrent workers never claim the same run", func(t *testing.T) {
		s := open(t)
		const runs = 12
		for i := 0; i < runs; i++ {
			// One workload each: twelve queued drills of the same workload is
			// a state the API refuses to create, and a queue full of it would
			// be testing something that cannot happen.
			id := fmt.Sprintf("p%02d", i)
			if err := s.Enqueue(ctx, queuedRun(id, fmt.Sprintf("%d", 200+i)), "name: x\n",
				base.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("Enqueue %s: %v", id, err)
			}
		}

		var (
			mu     sync.Mutex
			seen   = map[string]int{}
			wg     sync.WaitGroup
			claims int
		)
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for {
					got, err := s.ClaimRun(ctx, fmt.Sprintf("worker-%d", worker), lease, base)
					if errors.Is(err, ErrNoWork) {
						return
					}
					if err != nil {
						t.Errorf("worker %d: ClaimRun: %v", worker, err)
						return
					}
					mu.Lock()
					seen[got.ID]++
					claims++
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()

		if claims != runs {
			t.Errorf("%d claims for %d runs", claims, runs)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("run %s was claimed %d times: two workers would restore the same backup at once", id, n)
			}
		}
	})

	t.Run("a lease is renewed by its owner and by nobody else", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("c5", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := s.ClaimRun(ctx, "worker-a", lease, base); err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}

		if err := s.RenewLease(ctx, "c5", "worker-a", base.Add(2*lease)); err != nil {
			t.Fatalf("RenewLease by the owner: %v", err)
		}
		if err := s.RenewLease(ctx, "c5", "worker-b", base.Add(3*lease)); err == nil {
			t.Fatal("a lease was renewed by a worker that does not hold it")
		}
	})

	t.Run("a lease can be read back without knowing the schema", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("c7", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		// Queued and untouched: nobody holds it, and nothing is running.
		owner, expires, err := s.RunLease(ctx, "c7")
		if err != nil {
			t.Fatalf("RunLease on a queued run: %v", err)
		}
		if owner != "" {
			t.Errorf("owner = %q on a run nobody claimed, want empty", owner)
		}
		if !expires.IsZero() {
			t.Errorf("expires = %v on a run nobody claimed, want the zero time", expires)
		}

		if _, err := s.ClaimRun(ctx, "worker-a", lease, base); err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}

		owner, expires, err = s.RunLease(ctx, "c7")
		if err != nil {
			t.Fatalf("RunLease on a claimed run: %v", err)
		}
		if owner != "worker-a" {
			t.Errorf("owner = %q, want worker-a", owner)
		}
		if !expires.Equal(base.Add(lease)) {
			t.Errorf("expires = %v, want %v", expires, base.Add(lease))
		}

		// Finished: the expiry is gone, the owner stays. Both halves matter -
		// a cleared owner would make the run claimable all over again, which
		// is the one thing this phase exists to prevent.
		if err := s.FinishLease(ctx, "c7"); err != nil {
			t.Fatalf("FinishLease: %v", err)
		}
		owner, expires, err = s.RunLease(ctx, "c7")
		if err != nil {
			t.Fatalf("RunLease after FinishLease: %v", err)
		}
		if owner != "worker-a" {
			t.Errorf("owner = %q after FinishLease, want it kept as worker-a", owner)
		}
		if !expires.IsZero() {
			t.Errorf("expires = %v after FinishLease, want the zero time", expires)
		}
	})

	t.Run("the lease of an unknown run is not found", func(t *testing.T) {
		s := open(t)
		if _, _, err := s.RunLease(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RunLease on an unknown run = %v, want ErrNotFound", err)
		}
	})

	t.Run("stale runs are the ones whose worker stopped answering", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("c6", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := s.ClaimRun(ctx, "worker-a", lease, base); err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}
		if err := s.SetState(ctx, "c6", core.RunRestoring); err != nil {
			t.Fatalf("SetState: %v", err)
		}

		fresh, err := s.StaleRuns(ctx, base.Add(30*time.Second))
		if err != nil {
			t.Fatalf("StaleRuns: %v", err)
		}
		if len(fresh) != 0 {
			t.Fatalf("a live lease was reported stale: %+v", fresh)
		}

		stale, err := s.StaleRuns(ctx, base.Add(2*lease))
		if err != nil {
			t.Fatalf("StaleRuns: %v", err)
		}
		if len(stale) != 1 || stale[0].ID != "c6" {
			t.Fatalf("stale = %+v, want c6", stale)
		}

		// A finished run is not stale, however old its lease is.
		if err := s.SetState(ctx, "c6", core.RunSuccess); err != nil {
			t.Fatalf("SetState: %v", err)
		}
		done, err := s.StaleRuns(ctx, base.Add(2*lease))
		if err != nil {
			t.Fatalf("StaleRuns: %v", err)
		}
		if len(done) != 0 {
			t.Fatalf("a finished run was reported stale: %+v", done)
		}
	})
}

// QueueStatesMatchCoreTerminal guards the one place a Go method and a SQL
// list have to agree.
func QueueStatesMatchCoreTerminal(t *testing.T) {
	for _, s := range terminalStates {
		if !s.Terminal() {
			t.Errorf("%s is in the queue's terminal list but core says it is not terminal", s)
		}
	}
	all := []core.RunState{
		core.RunQueued, core.RunDiscoveringBackup, core.RunPreparing, core.RunRestoring,
		core.RunStarting, core.RunWaitingForGuest, core.RunRunningChecks,
		core.RunGeneratingReport, core.RunCleaningUp, core.RunSuccess, core.RunFailed,
		core.RunCancelled, core.RunCleanupFailed,
	}
	for _, s := range all {
		if !s.Terminal() {
			continue
		}
		found := false
		for _, t2 := range terminalStates {
			if t2 == s {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is terminal in core but missing from the queue's SQL list: a run in that state would be claimed or reported stale forever", s)
		}
	}
}
