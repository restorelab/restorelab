package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

func mustSchedule(t *testing.T, expr, tz string) *plan.Schedule {
	t.Helper()
	s, err := plan.ParseSchedule(expr, tz)
	if err != nil {
		t.Fatalf("ParseSchedule(%q, %q): %v", expr, tz, err)
	}
	return s
}

func TestDecide(t *testing.T) {
	grace := 2 * time.Hour
	// Daily at 03:00 UTC; the slot under test is 2026-09-06 03:00.
	slot := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	started := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		last       *store.Slot
		now        time.Time
		wantSlot   time.Time // zero means "nothing due"
		wantSkip   bool
		wantReason string // substring
	}{
		{
			name:     "nothing due before the slot",
			now:      slot.Add(-time.Minute),
			wantSlot: time.Time{},
		},
		{
			name:     "due on the minute",
			now:      slot,
			wantSlot: slot,
		},
		{
			name:     "due, an hour late, inside the grace period",
			now:      slot.Add(time.Hour),
			wantSlot: slot,
		},
		{
			name:     "due at the very edge of the grace period",
			now:      slot.Add(grace),
			wantSlot: slot,
		},
		{
			name:       "six hours late is skipped, not caught up",
			now:        slot.Add(6 * time.Hour),
			wantSlot:   slot,
			wantSkip:   true,
			wantReason: "grace period",
		},
		{
			name:     "a slot already decided is not decided again",
			last:     &store.Slot{SlotAt: slot, Outcome: store.SlotQueued},
			now:      slot.Add(time.Minute),
			wantSlot: time.Time{},
		},
		{
			name:     "the following slot becomes due after the last one",
			last:     &store.Slot{SlotAt: slot, Outcome: store.SlotQueued},
			now:      slot.Add(25 * time.Hour),
			wantSlot: slot.Add(24 * time.Hour),
		},
		{
			// A slot that was skipped is still decided: the scheduler must
			// move past it rather than reconsider it every tick.
			name:     "a skipped slot is not reconsidered",
			last:     &store.Slot{SlotAt: slot, Outcome: store.SlotSkipped, Reason: "late"},
			now:      slot.Add(time.Minute),
			wantSlot: time.Time{},
		},
	}

	sched := mustSchedule(t, "0 3 * * *", "UTC")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(sched, tc.last, started, tc.now, grace)
			if tc.wantSlot.IsZero() {
				if got != nil {
					t.Fatalf("decide = %+v, want nil (nothing due)", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("decide = nil, want slot %v", tc.wantSlot)
			}
			if !got.SlotAt.Equal(tc.wantSlot) {
				t.Fatalf("SlotAt = %v, want %v", got.SlotAt, tc.wantSlot)
			}
			if got.Skip != tc.wantSkip {
				t.Fatalf("Skip = %v, want %v (reason %q)", got.Skip, tc.wantSkip, got.Reason)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("Reason = %q, want it to mention %q", got.Reason, tc.wantReason)
			}
			if !tc.wantSkip && got.Reason != "" {
				t.Fatalf("a slot that is going to run carries reason %q, want none", got.Reason)
			}
		})
	}
}

func TestDecideIgnoresAPlanWithNoSchedule(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	if got := decide(nil, nil, now.Add(-time.Hour), now, 2*time.Hour); got != nil {
		t.Fatalf("decide(nil schedule) = %+v, want nil", got)
	}
}

// A plan written today must not back-fill the slots it would have had since
// January. The first slot a new plan gets is its next one.
func TestDecideDoesNotBackfillANewPlan(t *testing.T) {
	sched := mustSchedule(t, "0 3 * * *", "UTC")
	started := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if got := decide(sched, nil, started, started.Add(time.Minute), 2*time.Hour); got != nil {
		t.Fatalf("decide = %+v, want nil: the first slot is in the future", got)
	}
}

// The reason a slot was skipped is what an operator reads when they ask why a
// machine was not tested, so it has to carry the measurement and not just a
// verdict.
func TestASkippedSlotSaysHowLateItWas(t *testing.T) {
	sched := mustSchedule(t, "0 3 * * *", "UTC")
	slot := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	started := slot.Add(-time.Hour)

	got := decide(sched, nil, started, slot.Add(6*time.Hour), 2*time.Hour)
	if got == nil || !got.Skip {
		t.Fatalf("decide = %+v, want a skipped slot", got)
	}
	for _, want := range []string{"6h", "2h"} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("Reason = %q, want it to mention %q", got.Reason, want)
		}
	}
}

// Autumn's repeated hour must not drill twice in one night.
//
// The cron parser genuinely answers with two instants here - 02:30 CEST and
// 02:30 CET are an hour apart in UTC - and the primary key cannot tell them
// apart, because they are different slots. decide is what has to.
func TestTheAutumnFallBackDrillsOnce(t *testing.T) {
	sched, err := plan.ParseSchedule("30 2 * * *", "Europe/Paris")
	if err != nil {
		t.Skipf("no timezone database on this machine: %v", err)
	}
	started := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Hour

	// The first slot of the fall-back night, decided and recorded.
	first := decide(sched, nil, started, started.Add(15*time.Hour), grace)
	if first == nil {
		t.Fatal("decide = nil, want the 02:30 slot of the fall-back night")
	}
	last := &store.Slot{SlotAt: first.SlotAt, Outcome: store.SlotQueued}

	// An hour later the wall clock says 02:30 again. Nothing new is due.
	if got := decide(sched, last, started, first.SlotAt.Add(time.Hour), grace); got != nil {
		t.Fatalf("decide = %+v at the repeated 02:30, want nil - that is one drill, twice", got)
	}

	// And the next night's slot still comes, roughly a day later rather than
	// a day and an hour: skipping the repeat must not skip a real slot.
	next := decide(sched, last, started, first.SlotAt.Add(26*time.Hour), grace)
	if next == nil {
		t.Fatal("decide = nil the next night, want the following slot")
	}
	if gap := next.SlotAt.Sub(first.SlotAt); gap < 23*time.Hour || gap > 26*time.Hour {
		t.Fatalf("the next slot is %v after the fall-back slot, want about a day", gap)
	}
}

// The spring jump removes an hour: a slot inside it must still happen, at the
// first instant that exists.
func TestSlotsAcrossTheSpringForward(t *testing.T) {
	sched, err := plan.ParseSchedule("30 2 * * *", "Europe/Paris")
	if err != nil {
		t.Skipf("no timezone database on this machine: %v", err)
	}
	// 2026-03-29: 02:00 CET jumps to 03:00 CEST, so 02:30 does not exist.
	from := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	first := sched.Next(from) // 2026-03-28 02:30 has passed; this is the 29th
	second := sched.Next(first)
	if !second.After(first) {
		t.Fatalf("slots did not advance across the spring jump: %v then %v", first, second)
	}
	// Whatever the parser decides the missing slot maps to, it must not stall.
	if gap := second.Sub(first); gap > 26*time.Hour {
		t.Fatalf("a whole day was skipped across the spring jump: %v", gap)
	}
}
