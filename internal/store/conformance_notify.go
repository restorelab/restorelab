package store

// The notification half of the conformance suite. Like the rest of it, this
// lives outside a _test.go so both engines' test files can call it.
//
// The racing subtest below only really bites against PostgreSQL, the same way
// the slot test does: SQLite serialises every caller through its
// single-connection pool. Against SQLite it proves the statement says what it
// means; in CI it proves the statement holds under real concurrency.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// finishedRun builds a run that has already settled, which is the only kind
// the dispatcher ever looks at.
//
// It is declared here rather than borrowed from a _test.go because this file
// is compiled into the package proper: a helper living in a test file would
// not be visible to it.
func finishedRun(id, workload string, state core.RunState, result core.RunResult,
	level core.ProofLevel, at time.Time) *core.RecoveryRun {
	return &core.RecoveryRun{
		ID:               id,
		PlanName:         "p-" + workload,
		SourceWorkloadID: workload,
		ProviderID:       "proxmox-main",
		State:            state,
		Result:           result,
		ProofLevel:       level,
		StartedAt:        at,
		CompletedAt:      at.Add(time.Minute),
	}
}

// writeRun records a run and fails the test if the database refused it.
func writeRun(ctx context.Context, t *testing.T, s Store, run *core.RecoveryRun) {
	t.Helper()
	if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
		t.Fatalf("CreateRun %s: %v", run.ID, err)
	}
}

// sampleDelivery is one pending message, ready to be posted.
func sampleDelivery(id, runID, channel string, at time.Time) Delivery {
	return Delivery{
		ID:        id,
		RunID:     runID,
		ChannelID: channel,
		Kind:      "verdict_changed",
		State:     DeliveryPending,
		NextAt:    at,
		Payload:   `{"schema":"restorelab.notification.v1"}`,
		CreatedAt: at,
	}
}

// NotifyConformance exercises the claim, the story lookup and the delivery
// queue.
func NotifyConformance(t *testing.T, open OpenFunc) {
	ctx := context.Background()
	base := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)

	// The claim is the whole concurrency story of this feature: two
	// dispatchers against one database must not produce two messages about
	// one run.
	t.Run("only one dispatcher ever claims a run", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n1", "110", core.RunSuccess, core.ResultSuccess, core.ProofService, base))

		var (
			mu  sync.Mutex
			won int
			wg  sync.WaitGroup
		)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(dispatcher int) {
				defer wg.Done()
				got, err := s.ClaimRunForNotify(ctx, "n1", base.Add(time.Minute))
				if err != nil {
					t.Errorf("dispatcher %d: ClaimRunForNotify: %v", dispatcher, err)
					return
				}
				if !got {
					return
				}
				mu.Lock()
				won++
				mu.Unlock()
			}(i)
		}
		wg.Wait()

		if won != 1 {
			t.Fatalf("%d dispatchers claimed the same run: that is one chat message per winner", won)
		}
	})

	t.Run("a fresh terminal run is unnotified, a claimed one is not", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n2", "110", core.RunFailed, core.ResultFailed, core.ProofBoot, base))

		runs, err := s.UnnotifiedRuns(ctx, 10)
		if err != nil {
			t.Fatalf("UnnotifiedRuns: %v", err)
		}
		if len(runs) != 1 || runs[0].ID != "n2" {
			t.Fatalf("UnnotifiedRuns = %+v, want just n2", runs)
		}
		if runs[0].Result != core.ResultFailed || runs[0].ProofLevel != core.ProofBoot {
			t.Errorf("the summary lost what the run established: %+v", runs[0])
		}

		claimed, err := s.ClaimRunForNotify(ctx, "n2", base.Add(time.Minute))
		if err != nil {
			t.Fatalf("ClaimRunForNotify: %v", err)
		}
		if !claimed {
			t.Fatal("the first claim on a fresh run must win")
		}

		runs, err = s.UnnotifiedRuns(ctx, 10)
		if err != nil {
			t.Fatalf("UnnotifiedRuns after the claim: %v", err)
		}
		if len(runs) != 0 {
			t.Fatalf("a claimed run came back round: %+v", runs)
		}
	})

	// Announcing a queued drill would tell somebody it ended before it
	// started.
	t.Run("a run still in flight is never offered", func(t *testing.T) {
		s := open(t)
		if err := s.Enqueue(ctx, queuedRun("n3", "110"), "name: x\n", base); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		runs, err := s.UnnotifiedRuns(ctx, 10)
		if err != nil {
			t.Fatalf("UnnotifiedRuns: %v", err)
		}
		if len(runs) != 0 {
			t.Fatalf("a QUEUED run was offered for announcement: %+v", runs)
		}
	})

	// The baseline and the flag look at two different runs, and this is the
	// subtest that says so.
	t.Run("previous story skips verdict-less runs but reports them", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n4a", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeRun(ctx, t, s, finishedRun("n4b", "110", core.RunInconclusive, "", core.ProofNone, base.Add(time.Hour)))
		current := finishedRun("n4c", "110", core.RunSuccess, core.ResultSuccess, core.ProofService,
			base.Add(2*time.Hour))
		writeRun(ctx, t, s, current)

		prev, unevaluable, err := s.PreviousStory(ctx, "110",
			Position{StartedAt: current.StartedAt, ID: current.ID})
		if err != nil {
			t.Fatalf("PreviousStory: %v", err)
		}
		if prev == nil {
			t.Fatal("the workload has a graded run in its history and got no baseline")
		}
		if prev.ID != "n4a" {
			t.Errorf("baseline = %s, want n4a: an unevaluable drill must not become the baseline", prev.ID)
		}
		if prev.ProofLevel != core.ProofData {
			t.Errorf("baseline proof level = %q, want DATA", prev.ProofLevel)
		}
		if !unevaluable {
			t.Error("the run immediately before this one was INCONCLUSIVE and the flag says otherwise")
		}
	})

	// Nothing was attempted, so nothing was unevaluable.
	t.Run("a workload with no earlier run has no story", func(t *testing.T) {
		s := open(t)
		current := finishedRun("n5", "110", core.RunSuccess, core.ResultSuccess, core.ProofService, base)
		writeRun(ctx, t, s, current)

		prev, unevaluable, err := s.PreviousStory(ctx, "110",
			Position{StartedAt: current.StartedAt, ID: current.ID})
		if err != nil {
			t.Fatalf("PreviousStory: %v", err)
		}
		if prev != nil {
			t.Errorf("baseline = %+v, want none", prev)
		}
		if unevaluable {
			t.Error("a workload nobody ever drilled was reported as having become unevaluable")
		}
	})

	// Somebody stopped that drill. It says nothing about whether the workload
	// can be seen.
	t.Run("a cancelled run before this one is not unevaluable", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n6a", "110", core.RunSuccess, core.ResultSuccess, core.ProofService, base))
		writeRun(ctx, t, s, finishedRun("n6b", "110", core.RunCancelled, "", core.ProofNone, base.Add(time.Hour)))
		current := finishedRun("n6c", "110", core.RunSuccess, core.ResultSuccess, core.ProofService,
			base.Add(2*time.Hour))
		writeRun(ctx, t, s, current)

		prev, unevaluable, err := s.PreviousStory(ctx, "110",
			Position{StartedAt: current.StartedAt, ID: current.ID})
		if err != nil {
			t.Fatalf("PreviousStory: %v", err)
		}
		if prev == nil || prev.ID != "n6a" {
			t.Fatalf("baseline = %+v, want n6a: a cancelled run reached no verdict either", prev)
		}
		if unevaluable {
			t.Error("a cancelled run was read as the workload having become unevaluable")
		}
	})

	t.Run("a delivery is written once per run and channel", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n7", "110", core.RunFailed, core.ResultFailed, core.ProofBoot, base))

		if err := s.CreateDelivery(ctx, sampleDelivery("d1", "n7", "ops-discord", base)); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
		err := s.CreateDelivery(ctx, sampleDelivery("d2", "n7", "ops-discord", base))
		if !errors.Is(err, ErrDuplicate) {
			t.Fatalf("the second delivery for the same run and channel = %v, want ErrDuplicate", err)
		}

		// A second channel is a different message, not a duplicate.
		if err := s.CreateDelivery(ctx, sampleDelivery("d3", "n7", "ops-slack", base)); err != nil {
			t.Fatalf("CreateDelivery for a second channel: %v", err)
		}
	})

	t.Run("due deliveries respect their next attempt time", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n8", "110", core.RunFailed, core.ResultFailed, core.ProofBoot, base))

		later := base.Add(10 * time.Minute)
		d := sampleDelivery("d4", "n8", "ops-discord", base)
		d.NextAt = later
		d.Attempts = 1
		d.Status = 500
		d.Err = "internal server error"
		if err := s.CreateDelivery(ctx, d); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}

		due, err := s.DueDeliveries(ctx, base, 10)
		if err != nil {
			t.Fatalf("DueDeliveries: %v", err)
		}
		if len(due) != 0 {
			t.Fatalf("a delivery scheduled ten minutes out came back now: %+v", due)
		}

		due, err = s.DueDeliveries(ctx, later, 10)
		if err != nil {
			t.Fatalf("DueDeliveries at the scheduled time: %v", err)
		}
		if len(due) != 1 || due[0].ID != "d4" {
			t.Fatalf("DueDeliveries = %+v, want just d4", due)
		}
		got := due[0]
		if got.RunID != "n8" || got.ChannelID != "ops-discord" || got.Kind != "verdict_changed" {
			t.Errorf("the delivery lost what it is about: %+v", got)
		}
		if got.Attempts != 1 || got.Status != 500 || got.Err != "internal server error" {
			t.Errorf("the delivery lost what happened last time: %+v", got)
		}
		if got.Payload == "" {
			t.Error("the payload is stored so a retry sends what the first attempt tried to send")
		}
		if !got.NextAt.Equal(later) {
			t.Errorf("NextAt = %v, want %v", got.NextAt, later)
		}
	})

	t.Run("settling a delivery takes it out of the queue", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n9", "110", core.RunFailed, core.ResultFailed, core.ProofBoot, base))
		if err := s.CreateDelivery(ctx, sampleDelivery("d5", "n9", "ops-discord", base)); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}

		due, err := s.DueDeliveries(ctx, base, 10)
		if err != nil {
			t.Fatalf("DueDeliveries: %v", err)
		}
		if len(due) != 1 {
			t.Fatalf("DueDeliveries = %+v, want the fresh delivery", due)
		}

		sent := due[0]
		sent.State = DeliverySent
		sent.Attempts = 1
		sent.Status = 204
		sent.NextAt = time.Time{}
		sent.SentAt = base.Add(time.Second)
		if err := s.SettleDelivery(ctx, sent); err != nil {
			t.Fatalf("SettleDelivery: %v", err)
		}

		due, err = s.DueDeliveries(ctx, base.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("DueDeliveries after settling: %v", err)
		}
		if len(due) != 0 {
			t.Fatalf("a sent delivery is still queued and would be posted again: %+v", due)
		}
	})

	// Settling something that is not there is a bug in the caller, not a
	// silent success: the dispatcher would go on believing it recorded an
	// outcome it never wrote, and the message would be posted again.
	t.Run("settling a delivery that does not exist is an error", func(t *testing.T) {
		s := open(t)
		d := sampleDelivery("nope", "n0", "ops-discord", base)
		d.State = DeliverySent
		if err := s.SettleDelivery(ctx, d); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SettleDelivery on an unknown id = %v, want ErrNotFound", err)
		}
	})

	// The order matters when a channel has been unreachable for an hour: the
	// backlog has to read back in the order things happened, not backwards.
	t.Run("unnotified runs come back oldest first", func(t *testing.T) {
		s := open(t)
		for i, at := range []time.Time{base.Add(2 * time.Hour), base, base.Add(time.Hour)} {
			writeRun(ctx, t, s, finishedRun(fmt.Sprintf("na%d", i), fmt.Sprintf("11%d", i),
				core.RunSuccess, core.ResultSuccess, core.ProofService, at))
		}

		runs, err := s.UnnotifiedRuns(ctx, 10)
		if err != nil {
			t.Fatalf("UnnotifiedRuns: %v", err)
		}
		if len(runs) != 3 {
			t.Fatalf("got %d runs, want 3: %+v", len(runs), runs)
		}
		for i := 1; i < len(runs); i++ {
			if runs[i].StartedAt.Before(runs[i-1].StartedAt) {
				t.Fatalf("run %s came back before %s: the backlog is in reverse",
					runs[i].ID, runs[i-1].ID)
			}
		}
	})

	// doctor and the dashboard both ask this question, and both need the
	// answer to include a failure. A "last successful delivery" would render
	// a revoked webhook as a channel that has merely been quiet, which is the
	// exact confusion this whole slice exists to remove.
	t.Run("the last delivery of a channel is the last one, not the last good one", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("n20", "120", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeRun(ctx, t, s, finishedRun("n21", "121", core.RunFailed, core.ResultFailed, core.ProofBoot, base.Add(time.Hour)))

		good := sampleDelivery("d20", "n20", "ops-discord", base)
		good.State = DeliverySent
		good.Status = 204
		good.SentAt = base
		if err := s.CreateDelivery(ctx, good); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
		bad := sampleDelivery("d21", "n21", "ops-discord", base.Add(time.Hour))
		bad.State = DeliveryFailed
		bad.Status = 404
		bad.Err = "channel not found"
		if err := s.CreateDelivery(ctx, bad); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}

		last, err := s.LastDeliveries(ctx, []string{"ops-discord", "never-used"})
		if err != nil {
			t.Fatalf("LastDeliveries: %v", err)
		}
		got, ok := last["ops-discord"]
		if !ok {
			t.Fatal("LastDeliveries has no entry for a channel that has delivered")
		}
		if got.ID != "d21" || got.State != DeliveryFailed || got.Status != 404 {
			t.Errorf("last delivery = %s %s/%d, want d21 failed/404: a broken channel must not read as a quiet one",
				got.ID, got.State, got.Status)
		}
		if _, ok := last["never-used"]; ok {
			t.Error("a channel that has never delivered is present in the map: absent and empty say different things")
		}
	})

	t.Run("asking about no channel at all is not a query", func(t *testing.T) {
		s := open(t)
		last, err := s.LastDeliveries(ctx, nil)
		if err != nil {
			t.Fatalf("LastDeliveries(nil): %v", err)
		}
		if len(last) != 0 {
			t.Errorf("LastDeliveries(nil) = %+v, want empty", last)
		}
	})
}
