package journal

import (
	"context"

	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// The two ends of a captured value, and the reason they both live in this
// package.
//
// The journal is where the drill history is written, and reading it back is
// the same responsibility seen from the other side. Putting the reader here
// keeps the recovery engine free of a database handle: it receives one
// read-only method built by the caller that already knows which drill this
// is, and it never learns that a store exists. A locked or corrupt database
// can then cost a drill its baseline, which is a skipped comparison, and
// nothing else.

// capturedValues reads the measurements a check reported.
//
// The type assertion is deliberately unforgiving. Details is a map of any,
// filled in by whichever check produced the result and serialised to JSON on
// the way to the database; anything under this key that is not a map of
// numbers is something nobody meant to store as a measurement, and guessing
// at it would put a number in the history that no check ever read.
func capturedValues(r core.CheckResult) map[string]float64 {
	values, _ := r.Details[checks.DetailCaptured].(map[string]float64)
	return values
}

// baselineReader answers core.BaselineReader out of the drill history.
//
// The workload is a field rather than a parameter, and that is the safety
// property rather than a convenience. core.BaselineReader takes a check name
// and a capture name and nothing else, so a check running inside a drill has
// no way to name another machine: whose history this is was decided once, by
// the caller that knows which drill it launched.
type baselineReader struct {
	store      store.Store
	workloadID string
}

// Baselines builds the read-only history capability the recovery engine hands
// to its checks, for one workload.
//
// It returns nil when there is nothing to read from, because nil is what the
// engine and the checks already understand: a drift check with no reader is
// skipped with its reason, never failed. A store that is present but empty
// answers with no values and reaches the same place, which is what a first
// drill of a workload does.
func Baselines(s store.Store, workloadID string) core.BaselineReader {
	if s == nil || workloadID == "" {
		return nil
	}
	return baselineReader{store: s, workloadID: workloadID}
}

// Values returns what previous drills of this workload measured under this
// check and this capture name, most recent first.
func (b baselineReader) Values(ctx context.Context, checkName, valueName string, limit int) ([]float64, error) {
	return b.store.CapturedValues(ctx, b.workloadID, checkName, valueName, limit)
}
