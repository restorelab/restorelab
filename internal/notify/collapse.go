package notify

// The second question this package asks about a finished drill: not how it
// graded, but what it measured.
//
// It is asked only when the first question found nothing to say, because a
// run produces at most one message per channel. The delivery table holds one
// row per run and channel, deliberately, so that a restarted dispatcher
// cannot post twice into somebody's chat; there is no second message to be
// had, and a changed verdict is the bigger fact.

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/restorelab/restorelab/internal/core"
)

// DecideCollapse reports whether a value this drill measured fell to zero.
//
// values is what this run measured, by capture name, and earlier what the
// most recent drill of this workload that reached a verdict measured.
//
// The comparison is against the previous reading rather than against the
// median of the last five that the drift check judges against, and the
// difference is deliberate. A verdict has to resist one collapsed night
// becoming the new normal, which is what the median is for. A message has the
// opposite duty: it announces a change, once. A median here would keep firing
// for three more nights while the window emptied, telling a channel about a
// collapse everybody was told about on the first night, which is how the
// fourth message stops being read.
func DecideCollapse(state core.RunState, current Story, previous *Story,
	values, earlier map[string]float64) (Transition, bool) {

	// A run that reached no verdict is not evidence about the workload in
	// either direction, whatever numbers it happened to read on the way. It
	// is the rule the drift window follows when it skips verdict-less runs,
	// and the one the confidence ceiling follows. An empty Result is how a
	// cancelled run and an inconclusive one both say so.
	if !state.Terminal() || current.Result == "" {
		return Transition{}, false
	}

	name, was, ok := collapsed(values, earlier)
	if !ok {
		return Transition{}, false
	}

	return Transition{
		Kind:     KindValueCollapsed,
		Current:  current,
		Previous: previous,
		Headline: fmt.Sprintf("%s fell to zero, was %s", name, formatValue(was)),
	}, true
}

// collapsed reports the first value, by name, that fell to zero from a
// positive earlier reading.
//
// Sorted rather than taken in map order, because one drill produces one
// message and it has to be the same message every time it is decided: a
// headline that names a different table on a retry is one nobody can
// reproduce.
//
// The earlier reading must be positive rather than merely non-zero. A
// negative measurement reaching zero went up, and a rise is never a collapse,
// which is the rule CheckDrift already follows for a declared tolerance:
// max_drop is a floor, not a band.
func collapsed(values, earlier map[string]float64) (string, float64, bool) {
	names := make([]string, 0, len(values))
	for name, v := range values {
		if v == 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if was, ok := earlier[name]; ok && was > 0 {
			return name, was, true
		}
	}
	return "", 0, false
}

// Zeroed reports whether any of these readings is zero.
//
// It is separate from DecideCollapse so that the dispatcher can answer
// whether the previous drill is worth a query without making it. Nothing but
// a zero can be a collapse, so a drill whose numbers all held costs one round
// trip instead of two.
func Zeroed(values map[string]float64) bool {
	for _, v := range values {
		if v == 0 {
			return true
		}
	}
	return false
}

// valuesByName flattens what one run measured onto the capture names.
//
// The store answers by check sequence, which is the right key inside one run
// and the wrong one across two: a plan whose checks were reordered would have
// last night's row count compared against tonight's queue depth. The capture
// name is what a human reads in the report and what a message has to name, so
// it is the key here.
//
// A name that two checks of one drill both measured is dropped rather than
// resolved. There is no way to tell which of the two the previous drill
// measured under that name, and a message naming the wrong table is worse
// than no message at all.
func valuesByName(byCheck map[int]map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(byCheck))
	ambiguous := make(map[string]bool)

	for _, values := range byCheck {
		for name, v := range values {
			if _, seen := out[name]; seen {
				ambiguous[name] = true
				continue
			}
			out[name] = v
		}
	}
	for name := range ambiguous {
		delete(out, name)
	}
	return out
}

// formatValue renders a measured number for a human.
//
// It is report.FormatValue by hand rather than by import, and the duplication
// is the cheaper of the two costs. Importing the report package would put
// internal/checks, and everything a check needs to execute a command, into
// the dependency graph of a background component whose whole guarantee is
// that it cannot touch a drill. What it must not do is differ: %g and %v both
// turn 1204331 into 1.204331e+06, and a message that spells a row count in
// scientific notation asks its reader to do arithmetic before they can tell
// whether their database is empty.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
