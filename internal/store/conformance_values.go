package store

// The captured-value half of the conformance suite. Like the rest of it, this
// lives outside a _test.go so both engines' test files can call it.
//
// The subtests here are mostly about what does *not* come back. Drift compares
// a number against the same workload's own past, so a filter that silently
// leaks another workload's readings, or another check's, would not fail
// loudly: it would produce a plausible baseline that is about something else.
// That is why each filter gets its own subtest rather than one combined case.

import (
	"context"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// writeCaptured records a check and the value it measured.
//
// The check row is written first because check_values names a check by its
// seq, and the name drift filters on lives on run_checks. A value with no
// check would be unreachable through CapturedValues, which is the shape the
// engine writes anyway: the journal saves the check and the value together.
func writeCaptured(ctx context.Context, t *testing.T, s Store,
	runID string, seq int, checkName, valueName string, value float64) {
	t.Helper()

	if err := s.SaveCheck(ctx, runID, seq, core.CheckResult{
		Name:   checkName,
		Type:   "command",
		Status: core.CheckPass,
	}); err != nil {
		t.Fatalf("SaveCheck %s/%d: %v", runID, seq, err)
	}
	if err := s.SaveCheckValue(ctx, runID, seq, valueName, value); err != nil {
		t.Fatalf("SaveCheckValue %s/%d/%s: %v", runID, seq, valueName, err)
	}
}

// rawStore reaches the tables directly, which the conformance suite may do
// because it lives in the package.
//
// A cascade is a schema property and there is no DeleteRun on the interface,
// so the only way to exercise one is a direct DELETE - the way
// conformance_sessions.go proves its own.
func rawStore(t *testing.T, s Store) *sqlStore {
	t.Helper()

	raw, ok := s.(*sqlStore)
	if !ok {
		t.Skip("the cascade is a schema property; only the SQL store can be asked directly")
	}
	return raw
}

// countValues counts the rows one run left behind.
//
// It exists for the cascade subtest and nothing else: once the run is gone,
// every query that joins through runs answers "nothing" whether the values
// were deleted or orphaned, so the only honest way to tell those apart is to
// count the rows.
func countValues(ctx context.Context, t *testing.T, s Store, runID string) int {
	t.Helper()

	var n int
	err := rawStore(t, s).queryRow(ctx,
		`SELECT COUNT(*) FROM check_values WHERE run_id = ?`, runID).Scan(&n)
	if err != nil {
		t.Fatalf("count the values of run %s: %v", runID, err)
	}
	return n
}

// ValuesConformance exercises what a drill measured: writing it, reading it
// back for the report, and reading the history drift compares against.
func ValuesConformance(t *testing.T, open OpenFunc) {
	ctx := context.Background()
	base := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)

	// The fractional value is the point of this subtest. A row count is a
	// whole number and an integer column would have looked adequate right up
	// to the first check that measures a duration, a ratio or a size in
	// gigabytes - and it would then have truncated it silently, which is the
	// kind of thing nobody discovers before production.
	t.Run("a value is read back with the precision it was written with", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v1", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v1", 0, "orders", "rows", 1204331.5)
		writeCaptured(ctx, t, s, "v1", 1, "latency", "ms", -0.25)

		got, err := s.RunCheckValues(ctx, "v1")
		if err != nil {
			t.Fatalf("RunCheckValues: %v", err)
		}
		if got[0]["rows"] != 1204331.5 {
			t.Errorf("value = %v, want 1204331.5: an integer column would have truncated it", got[0]["rows"])
		}
		if got[1]["ms"] != -0.25 {
			t.Errorf("value = %v, want -0.25", got[1]["ms"])
		}

		history, err := s.CapturedValues(ctx, "110", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(history) != 1 || history[0] != 1204331.5 {
			t.Errorf("CapturedValues = %v, want [1204331.5]", history)
		}
	})

	// Most recent first, because the baseline is the median of the last five
	// and "the last five" is decided here, not by the caller.
	t.Run("captured values come back most recent first and honour the limit", func(t *testing.T) {
		s := open(t)
		for i, v := range []float64{100, 200, 300, 400} {
			id := "v2" + string(rune('a'+i))
			writeRun(ctx, t, s, finishedRun(id, "110", core.RunSuccess, core.ResultSuccess,
				core.ProofData, base.Add(time.Duration(i)*time.Hour)))
			writeCaptured(ctx, t, s, id, 0, "orders", "rows", v)
		}

		got, err := s.CapturedValues(ctx, "110", "orders", "rows", 10)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		want := []float64{400, 300, 200, 100}
		if len(got) != len(want) {
			t.Fatalf("CapturedValues = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("CapturedValues = %v, want %v: the history reads backwards", got, want)
			}
		}

		limited, err := s.CapturedValues(ctx, "110", "orders", "rows", 2)
		if err != nil {
			t.Fatalf("CapturedValues with a limit: %v", err)
		}
		if len(limited) != 2 || limited[0] != 400 || limited[1] != 300 {
			t.Errorf("CapturedValues(limit 2) = %v, want [400 300]", limited)
		}
	})

	// The three filters get three subtests. Collapsing them would hide which
	// one is missing, and a missing filter here does not fail loudly: it
	// hands drift a baseline about something else entirely.
	t.Run("another workload's values are not returned", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v3a", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v3a", 0, "orders", "rows", 100)
		writeRun(ctx, t, s, finishedRun("v3b", "120", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v3b", 0, "orders", "rows", 999)

		got, err := s.CapturedValues(ctx, "110", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(got) != 1 || got[0] != 100 {
			t.Errorf("CapturedValues = %v, want [100]: another machine's history is not this machine's baseline", got)
		}
	})

	t.Run("another check's values are not returned", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v4", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v4", 0, "orders", "rows", 100)
		writeCaptured(ctx, t, s, "v4", 1, "customers", "rows", 999)

		got, err := s.CapturedValues(ctx, "110", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(got) != 1 || got[0] != 100 {
			t.Errorf("CapturedValues = %v, want [100]: two checks of one plan measure two different things", got)
		}
	})

	t.Run("another capture name's values are not returned", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v5", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v5", 0, "orders", "rows", 100)
		if err := s.SaveCheckValue(ctx, "v5", 0, "bytes", 999); err != nil {
			t.Fatalf("SaveCheckValue: %v", err)
		}

		got, err := s.CapturedValues(ctx, "110", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(got) != 1 || got[0] != 100 {
			t.Errorf("CapturedValues = %v, want [100]: one check may capture several numbers", got)
		}
	})

	// The rule the confidence ceiling already follows. A drill nobody could
	// evaluate is not evidence about the workload in either direction, and
	// letting one into the baseline would let an unevaluable night move the
	// median that the next night is graded against.
	t.Run("a run that reached no verdict contributes no baseline", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v6a", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v6a", 0, "orders", "rows", 100)

		writeRun(ctx, t, s, finishedRun("v6b", "110", core.RunInconclusive, "", core.ProofNone,
			base.Add(time.Hour)))
		writeCaptured(ctx, t, s, "v6b", 0, "orders", "rows", 0)

		writeRun(ctx, t, s, finishedRun("v6c", "110", core.RunCancelled, "", core.ProofNone,
			base.Add(2*time.Hour)))
		writeCaptured(ctx, t, s, "v6c", 0, "orders", "rows", 0)

		got, err := s.CapturedValues(ctx, "110", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(got) != 1 || got[0] != 100 {
			t.Errorf("CapturedValues = %v, want [100]: an unevaluable drill is not a reading about the workload", got)
		}

		// The value is still stored and still readable for the report. It is
		// the baseline it is kept out of, not the history.
		run, err := s.RunCheckValues(ctx, "v6b")
		if err != nil {
			t.Fatalf("RunCheckValues: %v", err)
		}
		if len(run) != 1 {
			t.Errorf("RunCheckValues = %v, want the value the drill did measure", run)
		}
	})

	t.Run("deleting a run deletes its values", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v7", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v7", 0, "orders", "rows", 100)

		if n := countValues(ctx, t, s, "v7"); n != 1 {
			t.Fatalf("the run has %d value(s) before the delete, want 1", n)
		}

		if err := rawStore(t, s).exec(ctx, `DELETE FROM runs WHERE id = ?`, "v7"); err != nil {
			t.Fatalf("delete the run: %v", err)
		}
		if n := countValues(ctx, t, s, "v7"); n != 0 {
			t.Errorf("%d value(s) outlived the run they belong to", n)
		}
	})

	// A check retries. It must not leave two readings of one drill: the
	// median of the last five would then be computed over a window that is
	// partly one night counted twice.
	t.Run("saving the same reading twice replaces it", func(t *testing.T) {
		s := open(t)
		writeRun(ctx, t, s, finishedRun("v8", "110", core.RunSuccess, core.ResultSuccess, core.ProofData, base))
		writeCaptured(ctx, t, s, "v8", 0, "orders", "rows", 100)
		if err := s.SaveCheckValue(ctx, "v8", 0, "rows", 250.5); err != nil {
			t.Fatalf("SaveCheckValue on the retry: %v", err)
		}

		got, err := s.CapturedValues(ctx, "110", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("CapturedValues = %v, want one reading: a retry wrote a second row", got)
		}
		if got[0] != 250.5 {
			t.Errorf("CapturedValues = %v, want [250.5]: the last attempt is what the drill measured", got)
		}
		if n := countValues(ctx, t, s, "v8"); n != 1 {
			t.Errorf("the run holds %d value rows, want 1", n)
		}
	})

	// Absent and empty say the same thing here, and both are answers rather
	// than failures: a first drill has no history, and that is what a first
	// drill is.
	t.Run("a workload with no history has no values", func(t *testing.T) {
		s := open(t)

		got, err := s.CapturedValues(ctx, "999", "orders", "rows", 5)
		if err != nil {
			t.Fatalf("CapturedValues: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("CapturedValues = %v, want none", got)
		}

		run, err := s.RunCheckValues(ctx, "nope")
		if err != nil {
			t.Fatalf("RunCheckValues on an unknown run: %v", err)
		}
		if len(run) != 0 {
			t.Errorf("RunCheckValues = %v, want none", run)
		}
	})
}
