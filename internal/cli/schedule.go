package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

func newScheduleCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "See the drills stored plans queue for themselves",
		Long: `Shows what the scheduler is going to do, and what it has done.

There is nothing to create here: a schedule lives in its plan, next to what
it drills, and is edited with ` + "`restorelab plan apply`" + ` or in the
dashboard. One place to look when a drill did not run.`,
	}
	cmd.AddCommand(newScheduleListCmd(a), newScheduleSlotsCmd(a))
	return cmd
}

func newScheduleListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Scheduled plans and when each one drills next",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			history := a.store(ctx)

			plans, err := catalog.List(ctx, history, store.PlanFilter{})
			if err != nil {
				return planStoreError("", err)
			}

			now := time.Now()
			t := a.table(a.out, "PLAN", "WORKLOAD", "SCHEDULE", "NEXT DRILL", "LAST SLOT")
			scheduled := 0
			for _, row := range plans {
				parsed, err := plan.Parse([]byte(row.YAML))
				if err != nil {
					// A plan that no longer parses cannot be scheduled, and
					// saying so here is more use than leaving it out of a
					// listing whose whole job is to explain what will run.
					t.row(row.Name, row.WorkloadID, "?", "plan is invalid", "")
					scheduled++
					continue
				}

				sched, err := plan.ParseSchedule(parsed.Schedule, parsed.ScheduleTimezone)
				if err != nil {
					t.row(row.Name, row.WorkloadID, parsed.Schedule, "invalid: "+err.Error(), "")
					scheduled++
					continue
				}
				if sched == nil {
					continue // not scheduled, which is most plans
				}
				scheduled++

				last, err := history.LastSlot(ctx, row.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNoHistory) {
					return err
				}
				t.row(row.Name, row.WorkloadID, sched.Expr,
					nextSlotText(sched, last, now), lastSlotText(last))
			}

			if scheduled == 0 {
				fmt.Fprintln(a.out,
					"No stored plan carries a schedule. Add `schedule: \"0 3 * * 0\"` to one and it will drill itself.")
				return nil
			}
			t.flush()
			return nil
		},
	}
}

func newScheduleSlotsCmd(a *app) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "slots [plan]",
		Short: "The slots the scheduler has decided, skipped ones included",
		Long: `Lists the cron slots the scheduler has decided about.

A skipped slot is the answer to "why was this machine not tested", so they are
listed alongside the drills rather than hidden.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			history := a.store(ctx)

			f := store.SlotFilter{Limit: limit}
			if len(args) == 1 {
				row, err := catalog.Get(ctx, history, args[0])
				if err != nil {
					return planStoreError(args[0], err)
				}
				f.PlanID = row.ID
			}

			slots, err := history.ListSlots(ctx, f)
			if err != nil {
				return err
			}
			if len(slots) == 0 {
				fmt.Fprintln(a.out, "The scheduler has not decided any slot yet.")
				return nil
			}

			t := a.table(a.out, "SLOT", "OUTCOME", "DETAIL")
			for _, s := range slots {
				detail := s.Reason
				if s.Outcome == store.SlotQueued {
					detail = "run " + shortID(s.RunID)
				}
				t.row(s.SlotAt.Local().Format("2006-01-02 15:04"), string(s.Outcome), detail)
			}
			t.flush()
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 0, "how many slots to show")
	return cmd
}

// nextSlotText renders when a plan drills next, in words rather than an
// instant: "in 4h" is what somebody wants at a glance, and the absolute time
// is one column away in the slot listing.
func nextSlotText(sched *plan.Schedule, last *store.Slot, now time.Time) string {
	after := now
	if last != nil && last.SlotAt.After(after) {
		after = last.SlotAt
	}
	next := sched.Next(after)
	return fmt.Sprintf("%s (in %s)", next.Local().Format("2006-01-02 15:04"),
		next.Sub(now).Round(time.Minute))
}

// lastSlotText says what became of the previous slot.
//
// "never" is a real answer and has to read as one: a plan scheduled an hour
// ago has no history, and that is not the same news as a plan whose slots
// keep being skipped.
func lastSlotText(last *store.Slot) string {
	if last == nil {
		return "never"
	}
	return fmt.Sprintf("%s %s", last.SlotAt.Local().Format("01-02 15:04"), last.Outcome)
}
