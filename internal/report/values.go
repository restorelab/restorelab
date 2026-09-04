package report

// What a drill measured, on its way to a screen.
//
// A check records the numbers it read out of the restored workload in its
// details, under keys internal/checks names. This file turns that untyped bag
// into the shape every surface renders: the value, and the figure it is
// compared against. The comparison travels with the value everywhere, because
// a number with nothing beside it is trivia. Somebody reading "1206890" at
// 03:00 has to know what last week said before it means anything.

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/restorelab/restorelab/internal/checks"
)

// NoValue is what this product prints where it has no number.
//
// It is the same glyph the confidence score uses for a workload nobody has
// ever tested and the same one a notification uses for an unrecorded RTO:
// "we do not know" and "we know it is zero" are different answers, and a
// report that prints 0 for the first one is telling an operator their
// database is empty.
const NoValue = "--"

// CapturedValueDTO is one number a check read out of the restored workload,
// beside what the drill judged it against.
//
// Baseline is null rather than 0 when no previous drill measured this value,
// and null rather than absent when it is unknown: a client that renders a
// missing field as an empty cell and a zero as a zero would turn a first
// drill into an alarm.
type CapturedValueDTO struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	// Baseline is the median of what previous drills of this workload
	// measured, present only when the plan declared a drift tolerance and
	// the history could answer.
	Baseline *float64 `json:"baseline"`
}

// FormatValue renders a captured number for a human.
//
// Never %g and never %v. Both switch to scientific notation above six
// figures, so a row count of 1204331 reaches the report as 1.204331e+06 and
// the reader has to do arithmetic to find out whether their database is
// empty. The verb here prints every digit and no trailing zeroes, so 1.5
// stays 1.5 and 1204331 stays 1204331.
func FormatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// FormatBaseline renders the figure a value is compared against, or the
// no-value glyph when there is none.
func FormatBaseline(baseline *float64) string {
	if baseline == nil {
		return NoValue
	}
	return FormatValue(*baseline)
}

// valueLine is the one line every surface prints for a captured value:
//
//	orders: 1206890 (baseline 1204331)
func valueLine(v CapturedValueDTO) string {
	return v.Name + ": " + FormatValue(v.Value) + " (baseline " + FormatBaseline(v.Baseline) + ")"
}

// valueLines renders every value of a check, in the order they are carried.
func valueLines(values []CapturedValueDTO) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, valueLine(v))
	}
	return out
}

// capturedValues builds the wire form of what one check measured.
//
// The names are sorted so that two renders of one finished run are the same
// bytes: a Go map has no order, and a report attached to a compliance ticket
// that differs between two reads is a report somebody has to explain.
//
// A baseline recorded under a name nothing captured is dropped. It would be a
// comparison against nothing at all, and there is no honest way to print it.
func capturedValues(details map[string]any) []CapturedValueDTO {
	captured := numberMap(details[checks.DetailCaptured])
	if len(captured) == 0 {
		return nil
	}
	baseline := numberMap(details[checks.DetailBaseline])

	names := make([]string, 0, len(captured))
	for name := range captured {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]CapturedValueDTO, 0, len(names))
	for _, name := range names {
		v := CapturedValueDTO{Name: name, Value: captured[name]}
		if b, ok := baseline[name]; ok {
			v.Baseline = &b
		}
		out = append(out, v)
	}
	return out
}

// numberMap reads a map of numbers out of a check's details.
//
// Two shapes arrive here and both are legitimate. A check that has just run
// hands over the map[string]float64 it built; the same result read back out
// of the database arrives as decoded JSON, which is a map of any holding
// float64. Handling only the first would show a value on a live drill and
// lose it from that same drill's stored report, which is the harder bug to
// notice of the two.
//
// Anything else is refused whole rather than in part. Details is filled in by
// whichever check produced the result, and a key holding something that is
// not a map of numbers is not a measurement anybody meant to record; reading
// half of it would print some of what a drill measured and silently drop the
// rest.
func numberMap(v any) map[string]float64 {
	switch t := v.(type) {
	case map[string]float64:
		return t
	case map[string]any:
		out := make(map[string]float64, len(t))
		for name, raw := range t {
			f, ok := asFloat(raw)
			if !ok {
				return nil
			}
			out[name] = f
		}
		return out
	default:
		return nil
	}
}

// asFloat reads one number, whatever numeric shape a JSON round trip left it
// in. encoding/json decodes into float64 by default and into json.Number when
// a decoder was told to; the integer cases cover a details map built in Go
// and never serialised.
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
