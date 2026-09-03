package plan

import (
	"strings"
	"testing"
	"time"
)

func TestParseScheduleReturnsNilForNoSchedule(t *testing.T) {
	got, err := ParseSchedule("", "")
	if err != nil || got != nil {
		t.Fatalf(`ParseSchedule("", "") = %v, %v; want nil, nil`, got, err)
	}
}

func TestNextIsTheFollowingSlotInUTC(t *testing.T) {
	s, err := ParseSchedule("0 3 * * 0", "UTC")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	// Thursday 2026-09-03 -> the next Sunday at 03:00.
	from := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	got := s.Next(from)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
	// The slot is a database key, so it must come back as UTC whatever zone
	// the expression was read in.
	if got.Location() != time.UTC {
		t.Fatalf("Next returned a %v instant, want UTC", got.Location())
	}
}

func TestScheduleIsEvaluatedInItsOwnZone(t *testing.T) {
	s, err := ParseSchedule("0 3 * * *", "Europe/Paris")
	if err != nil {
		t.Skipf("no timezone database on this machine: %v", err)
	}
	// 03:00 Paris in September is 01:00 UTC (CEST, +02:00). Reading the
	// expression as UTC would drill two hours late in summer.
	from := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC)
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

// crontab combines day-of-month and day-of-week with OR when both are
// restricted, not AND. Getting this wrong means drills that run on the wrong
// day and nobody notices for months - it is the reason this parser is a
// dependency rather than 250 lines of our own.
func TestDayOfMonthAndDayOfWeekCombineWithOr(t *testing.T) {
	s, err := ParseSchedule("0 3 13 * 5", "UTC") // the 13th, OR any Friday
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) // a Tuesday
	want := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC) // Friday the 4th
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (Friday the 4th comes before the 13th)", got, want)
	}
}

func TestParseScheduleAcceptsTheShorthands(t *testing.T) {
	s, err := ParseSchedule("@weekly", "UTC")
	if err != nil {
		t.Fatalf("ParseSchedule(@weekly): %v", err)
	}
	from := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC) // Sunday midnight
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestParseScheduleRejectsAnInvalidExpression(t *testing.T) {
	_, err := ParseSchedule("every tuesday", "")
	if err == nil {
		t.Fatal("ParseSchedule accepted a non-cron expression")
	}
	// The message has to quote what was wrong: this is what an operator sees
	// when a plan is refused.
	if !strings.Contains(err.Error(), "every tuesday") {
		t.Fatalf("error %q does not quote the offending expression", err)
	}
}

func TestParseScheduleRejectsAnUnknownTimezone(t *testing.T) {
	_, err := ParseSchedule("0 3 * * 0", "Mars/Olympus_Mons")
	if err == nil {
		t.Fatal("ParseSchedule accepted an unknown timezone")
	}
	if !strings.Contains(err.Error(), "Mars/Olympus_Mons") {
		t.Fatalf("error %q does not name the offending timezone", err)
	}
}

// A timezone with no schedule to qualify is a typo with consequences: the
// author meant to schedule something and it silently never runs.
func TestParseScheduleRejectsATimezoneWithNoSchedule(t *testing.T) {
	if _, err := ParseSchedule("", "Europe/Paris"); err == nil {
		t.Fatal("ParseSchedule accepted a timezone with no schedule")
	}
}

// An empty timezone means the server's local zone, not UTC. A tool that
// silently read "0 3 * * 0" as UTC would drill at a different hour twice a
// year.
func TestAnEmptyTimezoneMeansLocal(t *testing.T) {
	s, err := ParseSchedule("0 3 * * *", "")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	if s.Location != time.Local {
		t.Fatalf("Location = %v, want time.Local", s.Location)
	}
}
