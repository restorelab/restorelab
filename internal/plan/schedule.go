package plan

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule is a plan's cron expression bound to the zone it is read in.
//
// The zone matters more than it looks. "0 3 * * 0" means three in the morning
// where the operator lives, and reading it as UTC would move every drill by
// an hour twice a year - in one direction into the working day.
type Schedule struct {
	Expr     string
	Location *time.Location

	spec cron.Schedule
}

// ParseSchedule parses a plan's schedule field and its timezone.
//
// A plan with no schedule is not an error: most plans have none and are
// simply never scheduled. It returns a nil *Schedule in that case, which
// callers read as "not scheduled".
//
// An empty timezone means the server's local zone. That is the surprising
// choice only until you write a cron expression: "0 3 * * 0" means three in
// the morning here, and a tool that quietly read it as UTC would drill at
// five in winter.
func ParseSchedule(expr, timezone string) (*Schedule, error) {
	if expr == "" {
		// A timezone with nothing to qualify is a typo with consequences:
		// its author meant to schedule something, and nothing would run.
		if timezone != "" {
			return nil, fmt.Errorf("schedule_timezone is set to %q but there is no schedule for it to qualify", timezone)
		}
		return nil, nil
	}

	loc := time.Local
	if timezone != "" {
		var err error
		if loc, err = time.LoadLocation(timezone); err != nil {
			return nil, fmt.Errorf("schedule_timezone %q is not a known timezone: %w", timezone, err)
		}
	}

	spec, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("schedule %q is not a valid cron expression: %w", expr, err)
	}
	return &Schedule{Expr: expr, Location: loc, spec: spec}, nil
}

// Next returns the first slot strictly after after, as a UTC instant.
//
// UTC is not a preference here, it is what makes a slot usable as a key: the
// slot is stored and compared as an instant, so the same wall-clock time read
// in two zones must never collide, and one zone's reading must never drift
// from its own.
func (s Schedule) Next(after time.Time) time.Time {
	return s.spec.Next(after.In(s.Location)).UTC()
}
