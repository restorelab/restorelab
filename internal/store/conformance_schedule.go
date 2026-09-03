package store

// The schedule half of the conformance suite. It is where the scheduler's
// only safety guarantee is verified: a cron slot is decided exactly once,
// whatever happens to the process that decided it, on both engines.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ScheduleConformance exercises the slot table: the claim, its idempotence,
// and reading slots back.
func ScheduleConformance(t *testing.T, open OpenFunc) {
	slotAt := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)

	t.Run("a claim writes the slot and queues the run in one go", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-1", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		run := queuedRun("6f1b7c88-0000-4000-8000-000000000001", "110")
		slot := Slot{
			PlanID:    p.ID,
			SlotAt:    slotAt,
			DecidedAt: slotAt.Add(3 * time.Second),
			Outcome:   SlotQueued,
			RunID:     run.ID,
		}
		if err := s.ClaimSlot(ctx, slot, run, p.YAML); err != nil {
			t.Fatalf("ClaimSlot: %v", err)
		}

		got, err := s.LastSlot(ctx, p.ID)
		if err != nil {
			t.Fatalf("LastSlot: %v", err)
		}
		if !got.SlotAt.Equal(slotAt) || got.Outcome != SlotQueued || got.RunID != run.ID {
			t.Fatalf("LastSlot = %+v, want the slot just claimed", got)
		}

		// The run is really in the queue: a worker can claim it. Without
		// this, a slot table could look perfect while nothing ever drilled.
		claimed, err := s.ClaimRun(ctx, "worker-1", time.Minute, slotAt.Add(time.Minute))
		if err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}
		if claimed.ID != run.ID {
			t.Fatalf("ClaimRun gave run %q, want %q", claimed.ID, run.ID)
		}
	})

	// This is the guard the whole tranche rests on. A drill is not
	// idempotent: queueing one twice restores a second time and can strand
	// the first temporary workload on the cluster.
	t.Run("the same slot cannot be claimed twice", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-2", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		first := queuedRun("6f1b7c88-0000-4000-8000-000000000002", "110")
		slot := Slot{PlanID: p.ID, SlotAt: slotAt, DecidedAt: slotAt, Outcome: SlotQueued, RunID: first.ID}
		if err := s.ClaimSlot(ctx, slot, first, p.YAML); err != nil {
			t.Fatalf("first ClaimSlot: %v", err)
		}

		second := queuedRun("6f1b7c88-0000-4000-8000-000000000003", "110")
		slot.RunID = second.ID
		if err := s.ClaimSlot(ctx, slot, second, p.YAML); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("second ClaimSlot = %v, want ErrDuplicate", err)
		}

		// And the losing run must not exist. The refusal has to be atomic,
		// not a slot row that was rejected after the run had already been
		// written - that would leave a drill queued that no slot accounts
		// for, which is the very double-drill this key exists to prevent.
		if _, err := s.GetRun(ctx, second.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetRun(loser) = %v, want ErrNotFound - the claim was not atomic", err)
		}
	})

	// The subtest above proves the refusal; this one proves what refuses.
	//
	// insertSlotSQL's NOT EXISTS clause decides which error a sequential
	// caller sees, but it cannot make two concurrent transactions agree:
	// both can read "no such slot" before either commits. Only the primary
	// key can, and this is the only test that can tell the difference.
	//
	// It is worth knowing where it bites. SQLite caps its pool at one
	// connection, so these claims serialise in Go's pool and the clause
	// alone would satisfy it. On PostgreSQL they genuinely race, and the
	// constraint is what holds - which is why this belongs in the
	// conformance suite rather than in a SQLite-only test.
	t.Run("concurrent claims of one slot produce exactly one drill", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-race", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		const claimers = 8
		var (
			wg         sync.WaitGroup
			mu         sync.Mutex
			won        int
			unexpected []error
		)
		for i := range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run := queuedRun(fmt.Sprintf("6f1b7c88-0000-4000-8000-1000000000%02d", i), "110")
				slot := Slot{
					PlanID: p.ID, SlotAt: slotAt, DecidedAt: slotAt,
					Outcome: SlotQueued, RunID: run.ID,
				}
				err := s.ClaimSlot(ctx, slot, run, p.YAML)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, ErrDuplicate):
				default:
					unexpected = append(unexpected, err)
				}
			}()
		}
		wg.Wait()

		if len(unexpected) > 0 {
			t.Fatalf("claims failed with something other than ErrDuplicate: %v", unexpected)
		}
		if won != 1 {
			t.Fatalf("%d claimers succeeded, want exactly 1", won)
		}

		// The decisive assertion: one slot means one queued drill. Two would
		// be the same backup restored twice, with one temporary workload
		// left behind on the cluster.
		queued, err := s.ListRuns(ctx, Filter{WorkloadID: "110", NotTerminal: true})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(queued) != 1 {
			t.Fatalf("%d runs queued for one slot, want exactly 1", len(queued))
		}
	})

	t.Run("a skipped slot carries its reason and no run", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-3", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		slot := Slot{
			PlanID:    p.ID,
			SlotAt:    slotAt,
			DecidedAt: slotAt.Add(6 * time.Hour),
			Outcome:   SlotSkipped,
			Reason:    "the slot was 6h0m late, past the 2h grace period",
		}
		if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
			t.Fatalf("ClaimSlot(skipped): %v", err)
		}

		got, err := s.LastSlot(ctx, p.ID)
		if err != nil {
			t.Fatalf("LastSlot: %v", err)
		}
		if got.Outcome != SlotSkipped || got.Reason == "" || got.RunID != "" {
			t.Fatalf("LastSlot = %+v, want a skipped slot with a reason and no run", got)
		}
	})

	t.Run("a queued slot with no run is refused", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-3b", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		slot := Slot{PlanID: p.ID, SlotAt: slotAt, DecidedAt: slotAt, Outcome: SlotQueued}
		if err := s.ClaimSlot(ctx, slot, nil, ""); err == nil {
			t.Fatal("ClaimSlot accepted a queued slot with no run")
		}
		// And it left nothing behind: the transaction rolled back.
		if _, err := s.LastSlot(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("LastSlot = %v, want ErrNotFound - the failed claim left a row", err)
		}
	})

	t.Run("LastSlot reports ErrNotFound for a plan never scheduled", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-4", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		if _, err := s.LastSlot(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("LastSlot = %v, want ErrNotFound", err)
		}
	})

	t.Run("ListSlots returns the most recent first", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-5", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		for i := range 3 {
			at := slotAt.AddDate(0, 0, 7*i)
			slot := Slot{PlanID: p.ID, SlotAt: at, DecidedAt: at, Outcome: SlotSkipped, Reason: "no worker"}
			if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
				t.Fatalf("ClaimSlot %d: %v", i, err)
			}
		}
		got, err := s.ListSlots(ctx, SlotFilter{PlanID: p.ID})
		if err != nil {
			t.Fatalf("ListSlots: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("ListSlots returned %d slots, want 3", len(got))
		}
		if !got[0].SlotAt.After(got[2].SlotAt) {
			t.Fatalf("ListSlots is not newest-first: %v then %v", got[0].SlotAt, got[2].SlotAt)
		}
	})

	t.Run("ListSlots without a plan spans the catalogue", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		for i, name := range []string{"one", "two"} {
			p := samplePlan("plan-6"+name, name)
			if err := s.CreatePlan(ctx, p); err != nil {
				t.Fatalf("CreatePlan %s: %v", name, err)
			}
			at := slotAt.AddDate(0, 0, i)
			slot := Slot{PlanID: p.ID, SlotAt: at, DecidedAt: at, Outcome: SlotSkipped, Reason: "x"}
			if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
				t.Fatalf("ClaimSlot %s: %v", name, err)
			}
		}
		got, err := s.ListSlots(ctx, SlotFilter{})
		if err != nil {
			t.Fatalf("ListSlots: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListSlots returned %d slots, want 2 across both plans", len(got))
		}
	})

	// "Why was this machine not tested" is a question about the machine, not
	// about one plan: a workload can be covered by several.
	t.Run("ListSlots can span every plan covering one workload", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()

		// Two plans on workload 110, one on 104.
		for _, spec := range []struct{ id, name, workload string }{
			{"plan-w1", "nightly", "110"},
			{"plan-w2", "weekly", "110"},
			{"plan-w3", "other", "104"},
		} {
			p := samplePlan(spec.id, spec.name)
			p.WorkloadID = spec.workload
			if err := s.CreatePlan(ctx, p); err != nil {
				t.Fatalf("CreatePlan %s: %v", spec.name, err)
			}
			slot := Slot{PlanID: p.ID, SlotAt: slotAt, DecidedAt: slotAt, Outcome: SlotSkipped, Reason: "x"}
			if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
				t.Fatalf("ClaimSlot %s: %v", spec.name, err)
			}
		}

		got, err := s.ListSlots(ctx, SlotFilter{WorkloadID: "110"})
		if err != nil {
			t.Fatalf("ListSlots: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListSlots returned %d slots for workload 110, want 2", len(got))
		}
		for _, slot := range got {
			if slot.PlanID == "plan-w3" {
				t.Fatalf("the listing leaked another workload's slot: %+v", slot)
			}
		}
	})

	// A slot for a plan that no longer exists answers no question.
	t.Run("deleting a plan removes its slots", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-7", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		slot := Slot{PlanID: p.ID, SlotAt: slotAt, DecidedAt: slotAt, Outcome: SlotSkipped, Reason: "x"}
		if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
			t.Fatalf("ClaimSlot: %v", err)
		}
		if err := s.DeletePlan(ctx, p.ID); err != nil {
			t.Fatalf("DeletePlan: %v", err)
		}
		got, err := s.ListSlots(ctx, SlotFilter{PlanID: p.ID})
		if err != nil {
			t.Fatalf("ListSlots: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListSlots returned %d slots after the plan was deleted, want 0", len(got))
		}
	})

	// The slot is a key, so it has to survive the round trip as the same
	// instant. A zone that leaked into the column would make two schedulers
	// in two zones disagree about what "the 03:00 slot" is.
	t.Run("a slot round trips as UTC whatever zone it was given in", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("plan-8", "linux-nightly")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		paris, err := time.LoadLocation("Europe/Paris")
		if err != nil {
			t.Skipf("no timezone database on this machine: %v", err)
		}
		local := slotAt.In(paris)
		slot := Slot{PlanID: p.ID, SlotAt: local, DecidedAt: local, Outcome: SlotSkipped, Reason: "x"}
		if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
			t.Fatalf("ClaimSlot: %v", err)
		}
		got, err := s.LastSlot(ctx, p.ID)
		if err != nil {
			t.Fatalf("LastSlot: %v", err)
		}
		if !got.SlotAt.Equal(slotAt) {
			t.Fatalf("SlotAt = %v, want the same instant as %v", got.SlotAt, slotAt)
		}

		// The same wall clock in another zone is a different instant, and
		// must therefore be a claimable slot rather than a duplicate.
		slot.SlotAt = time.Date(2026, 9, 6, 1, 0, 0, 0, paris)
		if err := s.ClaimSlot(ctx, slot, nil, ""); err != nil {
			t.Fatalf("ClaimSlot(other instant) = %v, want it accepted", err)
		}
	})
}
