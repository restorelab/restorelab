// Package scheduler queues the drills a stored plan's cron asks for.
//
// It is a producer of queue rows and nothing else. It holds no provider, no
// recovery engine and no cluster credential, and it cannot be constructed
// with any: automating drills had to add no destructive surface to the
// product, and the compiler is what guarantees that rather than a test.
//
// Idempotence lives in the database. A slot is a row keyed by
// (plan_id, slot_at), claimed in the same transaction as the run it queues,
// so a scheduler that dies at any point cannot queue the same drill twice. A
// drill is not idempotent - replaying one restores a second time and can
// strand the first temporary workload on the cluster - so that is the whole
// safety story, and a lease would not have been one: no lease covers the gap
// between writing a run and recording that it was written.
package scheduler

import (
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

// decision is what to do about one plan's next slot.
//
// Skip carries a Reason and still records the slot: a slot nobody drilled is
// information an operator needs, and dropping it silently is how a dashboard
// ends up saying "never tested" with no explanation.
type decision struct {
	SlotAt time.Time
	Skip   bool
	Reason string
}

// decide reports what to do about a plan's next due slot, or nil when nothing
// is due.
//
// It is deliberately a pure function of four values and no clock. Lateness,
// the grace period and the timezone are where a scheduler gets things subtly
// wrong, and none of it should need a Sunday to test.
//
// startedAt is where a plan with no slot history begins, so that a plan
// written today does not back-fill every slot it would have had since
// January.
func decide(sched *plan.Schedule, last *store.Slot, startedAt, now time.Time, grace time.Duration) *decision {
	if sched == nil {
		return nil
	}

	// The last decided slot is the floor, whether it ran or was skipped: a
	// slot the scheduler has already ruled on must never be reconsidered,
	// or a skipped one would be re-examined at every tick forever.
	after := startedAt
	if last != nil && last.SlotAt.After(after) {
		after = last.SlotAt
	}

	slot := sched.Next(after)

	// The autumn fall-back repeats an hour of wall clock, and the cron
	// parser answers with two distinct instants for it - 02:30 CEST and
	// 02:30 CET are an hour apart in UTC. The primary key cannot dedupe
	// those: they are different slots as far as the database is concerned.
	//
	// So the same local wall clock twice in a row is caught here. Nothing
	// legitimate produces it: an hourly cron gives 02:30 then 03:30, a daily
	// one gives 02:30 then 02:30 the next day. Only the fall-back can repeat
	// a date and a time, and one drill per night is what the plan asked for.
	if last != nil && sameWallClock(sched.Location, slot, last.SlotAt) {
		slot = sched.Next(slot)
	}

	if slot.After(now) {
		return nil
	}

	// Late past the grace period: skipped, and never caught up. A drill
	// restores tens of gigabytes and occupies the storage it restores onto;
	// one that starts in the middle of a working day because a server
	// rebooted is an incident, not a test. Nothing about a backup is learned
	// more usefully now than at the next slot.
	if late := now.Sub(slot); late > grace {
		return &decision{
			SlotAt: slot,
			Skip:   true,
			Reason: fmt.Sprintf("the slot was %s late, past the %s grace period: "+
				"a drill that starts outside its window is an incident, not a test",
				late.Round(time.Minute), grace),
		}
	}
	return &decision{SlotAt: slot}
}

// sameWallClock reports whether two instants read as the same date and time
// in loc, down to the minute - which is the resolution a cron expression
// works at.
//
// It is how the repeated hour of a daylight-saving fall-back is recognised,
// and it is deliberately not a comparison of the instants themselves: those
// are an hour apart, which is exactly why the naive check misses it.
func sameWallClock(loc *time.Location, a, b time.Time) bool {
	if loc == nil {
		loc = time.UTC
	}
	x, y := a.In(loc), b.In(loc)
	xd, yd := x.Year()*10000+int(x.Month())*100+x.Day(), y.Year()*10000+int(y.Month())*100+y.Day()
	return xd == yd && x.Hour() == y.Hour() && x.Minute() == y.Minute()
}
