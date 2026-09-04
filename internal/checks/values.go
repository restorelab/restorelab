package checks

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/restorelab/restorelab/internal/core"
)

// Why this file exists.
//
// `exit 0` proves a process ran and `select 1` proves PostgreSQL started. An
// empty, perfectly booted database passes both, and a drill that restores an
// empty database and reports SUCCESS is worse than no drill at all: it is a
// reason to stop looking. A check that reads a number out of the restored
// workload is what turns "the plan says this proves data" into something the
// product has actually seen.
//
// The judgement lives here, in four pure functions, deliberately away from the
// check that runs the command and away from the store that keeps the history.
// Nothing below opens a socket, runs a command or touches a database, so every
// rule it encodes can be tested exactly, including the ones that only bite on
// a value nobody would think to produce on purpose.
//
// Three of those rules are the whole reason the file is separate:
//
//   - a value that is not a finite number is refused at the door, because
//     every comparison against NaN is false and a check that compares against
//     NaN passes silently forever;
//   - the baseline is a median, so one collapsed reading cannot become the
//     new normal;
//   - a rise is never a drift violation, and a zero baseline is never a
//     division.

// ParseCaptured reads the number a check printed.
//
// It has to survive what a real command prints rather than what a test writes.
// `psql -tAc 'select count(*) from orders'` emits the row, a trailing newline,
// and, depending on the client and the connection, blank lines around it and
// padding inside it. A guest running Windows adds carriage returns. All of
// that is noise around one number, and stripping it here means no check has to
// know about it.
//
// Two refusals are the point of the function:
//
// NaN and Inf. strconv.ParseFloat accepts "NaN", "Inf", "+Inf" and
// "Infinity" as perfectly good float64 values, and a NaN stored as a baseline
// is a check that can never fail again: every comparison against NaN is false,
// so no bound is ever violated and nobody is ever told. Refusing them at the
// only place a value enters the product is cheaper than defending every
// comparison downstream, and it turns a silent pass into a loud error.
//
// More than one number. Taking the first line would quietly record something
// the operator never asked to measure, and a wrong number recorded silently is
// the exact failure this whole slice exists to prevent. A command that prints
// two rows is a command whose query needs fixing, and saying so is the useful
// answer.
func ParseCaptured(stdout string) (float64, error) {
	lines := make([]string, 0, 1)
	for _, line := range strings.Split(stdout, "\n") {
		// TrimSpace also removes the carriage return of a CRLF line ending,
		// so no separate Windows case is needed.
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	switch {
	case len(lines) == 0:
		return 0, errors.New("the command printed nothing to capture")
	case len(lines) > 1:
		return 0, fmt.Errorf("the command printed %d values and a capture is one measurement: %s",
			len(lines), truncate(strings.Join(lines, " / "), 120))
	}

	text := lines[0]
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if !finite(value) {
		return 0, fmt.Errorf("%q is not a finite number, and a value that compares false "+
			"against every bound would pass every check forever", text)
	}
	return value, nil
}

// Baseline is the median of the values given, or false when there are none.
//
// The median, not the mean and not the previous value, and the choice is the
// design rather than a detail.
//
// The previous value alone ratchets. A collapse from 1204331 to 0 fails once,
// and then the next drill compares 0 against 0, finds no drop and passes. The
// empty database becomes the new normal on the second night, which is the
// precise moment everybody stops paying attention.
//
// The mean has the mirror problem: one anomalous reading, taken while a batch
// job was mid-flight, poisons the baseline for as long as it stays in the
// window, and a collapse drags it far enough that the next collapse fits
// inside the tolerance.
//
// The median of a short window resists both. It does mean the first few drills
// of a workload have a thinner baseline, which is honest: there is less to
// compare against, and the check says so rather than pretending.
//
// Non-finite values are skipped rather than trusted. ParseCaptured is the gate
// that should stop them ever reaching storage, but a single NaN in the slice
// would make the sort order meaningless and hand back a NaN baseline, which is
// the silent-pass failure again one layer down. Skipping costs one comparison
// per value and makes "Baseline never returns NaN" true unconditionally.
func Baseline(values []float64) (float64, bool) {
	// This copy is not incidental. The caller owns the slice a store returned
	// and may render it beside the verdict; sorting in place would reorder
	// its "most recent first" behind its back.
	sorted := make([]float64, 0, len(values))
	for _, v := range values {
		if finite(v) {
			sorted = append(sorted, v)
		}
	}
	if len(sorted) == 0 {
		return 0, false
	}
	slices.Sort(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid], true
	}
	// Halved before adding rather than after: (lo+hi)/2 overflows to +Inf for
	// values near the top of float64, and an infinite baseline is the silent
	// pass again.
	return sorted[mid-1]/2 + sorted[mid]/2, true
}

// CheckAssert reports the first bound violated, or "" when all hold.
//
// The bounds are tested in the order the plan documents them, min then max
// then equals, so that the same reading always produces the same sentence in a
// report. Reporting only the first is deliberate: an operator reading a failed
// check at 03:00 needs the one fact that changed the verdict, not a list.
//
// Both bounds are inclusive. `min: 1` says "this table is never empty", and a
// table holding exactly one row is not empty.
//
// A spec with no bound at all returns "" rather than failing. The plan parser
// refuses that spec at write time, which is where it belongs; here it just
// means nobody stated an opinion.
func CheckAssert(value float64, a core.AssertSpec) string {
	// A non-finite value is graded as a violation, never as "within bounds".
	// Every comparison below would be false, so the honest-looking version of
	// this function returns "" and the check passes. ParseCaptured should make
	// this unreachable; it stays because "unreachable" is a property of the
	// current call site, not of the function.
	if !finite(value) {
		return fmt.Sprintf("captured value %s is not a finite number, so no bound can be judged against it",
			formatNumber(value))
	}

	switch {
	case a.Min != nil && value < *a.Min:
		return fmt.Sprintf("captured value %s is below the declared minimum of %s",
			formatNumber(value), formatNumber(*a.Min))
	case a.Max != nil && value > *a.Max:
		return fmt.Sprintf("captured value %s is above the declared maximum of %s",
			formatNumber(value), formatNumber(*a.Max))
	case a.Equals != nil && value != *a.Equals:
		return fmt.Sprintf("captured value %s is not the declared %s",
			formatNumber(value), formatNumber(*a.Equals))
	}
	return ""
}

// CheckDrift reports a violation, or "" when the value is within tolerance.
//
// max_drop is a floor, not a band: a value that grew is never a violation. A
// table that gained rows since the last drill is not a failed restoration, and
// a product that graded it as one would teach its operator to ignore red,
// which costs more than the check was ever worth.
//
// A zero baseline with a percentage tolerance is not a division. Going from 0
// to 0 is not a 0 percent drop and not an infinite one, it is no drop at all,
// and the rise guard above answers it without arithmetic. The naive version
// divides by zero, produces NaN, compares NaN against the tolerance, gets
// false, and passes forever. A genuine fall below a zero baseline is reported
// instead, because no percentage of zero permits any fall; returning "" there
// would be the NaN behaviour rewritten by hand.
//
// The percentage is taken against the baseline's magnitude. Captured values
// are usually row counts, but free space and deltas are values too, and
// dividing by a negative baseline yields a negative percentage that compares
// under every tolerance, so every collapse would pass.
//
// The caller decides what to do when there is no baseline at all. It is not
// this function's business, and the answer is not "fail": being unable to read
// the past is not evidence about the present.
func CheckDrift(value, baseline float64, d core.DriftSpec) string {
	if !finite(value) || !finite(baseline) {
		return fmt.Sprintf("drift cannot be judged: captured value %s against baseline %s is not a finite comparison",
			formatNumber(value), formatNumber(baseline))
	}

	// A rise, or no change at all. Checked before any arithmetic, which is
	// also what makes the zero baseline harmless.
	if value >= baseline {
		return ""
	}
	drop := baseline - value

	if !d.MaxDropIsPercent {
		if drop <= d.MaxDrop {
			return ""
		}
		return fmt.Sprintf("captured value %s is %s below the baseline %s, more than the declared max_drop of %s",
			formatNumber(value), formatNumber(drop), formatNumber(baseline), formatNumber(d.MaxDrop))
	}

	if baseline == 0 {
		return fmt.Sprintf("captured value %s fell below a baseline of 0, and no percentage of a zero baseline permits any fall",
			formatNumber(value))
	}
	pct := drop / math.Abs(baseline) * 100
	if pct <= d.MaxDrop {
		return ""
	}
	return fmt.Sprintf("captured value %s is %s%% below the baseline %s, more than the declared max_drop of %s%%",
		formatNumber(value), formatPercent(pct), formatNumber(baseline), formatNumber(d.MaxDrop))
}

// finite reports whether v is a number a bound can be judged against.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// formatNumber renders a captured value the way its source printed it.
//
// 'f' with -1 precision, never %g and never %v: a row count of 1204331 reaching
// a report as 1.204331e+06 makes the reader do arithmetic at 03:00 to find out
// whether their database is empty. -1 still drops trailing zeros, so 1.5 stays
// 1.5 and 1204331 stays 1204331.
func formatNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// formatPercent renders a computed percentage to one decimal, dropping it when
// it is zero. A drop of 20.000000000000004 percent is a fact about binary
// floating point, not about the workload, and printing it that way makes the
// message look broken.
func formatPercent(pct float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(pct, 'f', 1, 64), ".0")
}
