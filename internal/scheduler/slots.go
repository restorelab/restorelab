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

// maxCollapse bounds how many missed slots one tick will step over.
//
// It only matters for downtime measured in months against a frequent cron,
// where walking every slot would cost more than the answer is worth. What is
// left over is stepped over at the next tick, so a long outage still settles
// in minutes rather than in one enormous pass.
const maxCollapse = 10_000

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

	// Where to resume from. The last decided slot is the floor whenever there
	// is one, whether it ran or was skipped: a slot already ruled on must
	// never be reconsidered, or a skipped one would come back every tick.
	//
	// startedAt is the floor only for a plan with no history at all, so that
	// a plan written today does not back-fill every slot it would have had
	// since January.
	//
	// It is deliberately not the later of the two. Taking startedAt when it
	// is more recent - a server that was off across a slot - would step over
	// the missed slot without ever examining it, so nothing would record
	// that it was missed. That silence is the failure this table exists to
	// prevent: a machine nobody tested would look exactly like one nobody
	// scheduled.
	after := startedAt
	if last != nil {
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

	// Advance to the most recent slot that is due, counting what is stepped
	// over. A server that was off for a week owes one answer - "nothing ran,
	// because nothing was running" - not one row per slot it missed, and
	// recording them one per tick would take a week to catch up on a week of
	// downtime.
	//
	// The count goes into the reason, so the single row still says how much
	// was missed.
	missed := 0
	for missed < maxCollapse {
		next := sched.Next(slot)
		if next.After(now) {
			break
		}
		slot = next
		missed++
	}

	// Late past the grace period: skipped, and never caught up. A drill
	// restores tens of gigabytes and occupies the storage it restores onto;
	// one that starts in the middle of a working day because a server
	// rebooted is an incident, not a test. Nothing about a backup is learned
	// more usefully now than at the next slot.
	if late := now.Sub(slot); late > grace {
		reason := fmt.Sprintf("the slot was %s late, past the %s grace period: "+
			"a drill that starts outside its window is an incident, not a test",
			late.Round(time.Minute), grace)
		if missed > 0 {
			reason += fmt.Sprintf(" (%d earlier slot(s) went by with it)", missed)
		}
		return &decision{SlotAt: slot, Skip: true, Reason: reason}
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
