package journal

import (
	"context"
	"errors"
	"testing"

	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// captured builds a check result carrying one measurement, the way the
// command check reports one.
func captured(name string, values map[string]float64) core.CheckResult {
	return core.CheckResult{
		Name:    name,
		Type:    "command",
		Status:  core.CheckPass,
		Details: map[string]any{checks.DetailCaptured: values},
	}
}

// A measured value has to land on the same check row the check was written
// to. CapturedValues joins check_values to run_checks by seq to recover the
// name the plan wrote, so a value one row off is a value attributed to a
// different measurement.
func TestFinishWritesValuesAtTheChecksOwnSeq(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)

	rec.Finish(context.Background(), &core.RecoveryRun{
		ID: "abc", State: core.RunSuccess, Result: core.ResultSuccess,
		Checks: []core.CheckResult{
			{Name: "boots", Type: "command", Status: core.CheckPass},
			captured("orders", map[string]float64{"rows": 1204331}),
			captured("free-space", map[string]float64{"bytes": 42.5}),
		},
	})

	want := []savedValue{
		{runID: "abc", seq: 1, name: "rows", value: 1204331},
		{runID: "abc", seq: 2, name: "bytes", value: 42.5},
	}
	if len(spy.values) != len(want) {
		t.Fatalf("store received %d values, want %d: %+v", len(spy.values), len(want), spy.values)
	}
	for i, got := range spy.values {
		if got != want[i] {
			t.Errorf("value %d = %+v, want %+v", i, got, want[i])
		}
	}
}

// Two captures on one check are written in a stable order. Go randomises map
// iteration, and a history whose write order changes run to run is a history
// that cannot be diffed when somebody is trying to work out what happened.
func TestFinishWritesSeveralValuesOfOneCheckInNameOrder(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)

	rec.Finish(context.Background(), &core.RecoveryRun{
		ID: "abc", State: core.RunSuccess,
		Checks: []core.CheckResult{captured("orders", map[string]float64{
			"rows": 1, "bytes": 2, "indexes": 3,
		})},
	})

	var names []string
	for _, v := range spy.values {
		names = append(names, v.name)
	}
	want := []string{"bytes", "indexes", "rows"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
}

// A check with no measurement writes no value row, and a details map holding
// something else under the key is ignored rather than guessed at.
func TestFinishWritesNothingForACheckThatMeasuredNothing(t *testing.T) {
	spy := &spyStore{}
	rec := quietRecorder(spy)

	rec.Finish(context.Background(), &core.RecoveryRun{
		ID: "abc", State: core.RunSuccess,
		Checks: []core.CheckResult{
			{Name: "boots", Type: "command", Status: core.CheckPass},
			{Name: "http", Type: "http", Status: core.CheckPass, Details: map[string]any{"status": 200}},
			{Name: "odd", Type: "command", Status: core.CheckPass,
				Details: map[string]any{checks.DetailCaptured: "not a map of numbers"}},
		},
	})

	if len(spy.values) != 0 {
		t.Fatalf("store received %d values, want none: %+v", len(spy.values), spy.values)
	}
}

// --- the baseline reader -------------------------------------------------

// valuesStore answers CapturedValues and records what it was asked.
type valuesStore struct {
	store.Noop
	values []float64
	err    error

	calls          int
	lastWorkloadID string
	lastCheckName  string
	lastValueName  string
	lastLimit      int
}

func (v *valuesStore) CapturedValues(_ context.Context, workloadID, checkName, valueName string, limit int) ([]float64, error) {
	v.calls++
	v.lastWorkloadID = workloadID
	v.lastCheckName = checkName
	v.lastValueName = valueName
	v.lastLimit = limit
	if v.err != nil {
		return nil, v.err
	}
	return v.values, nil
}

// The workload is fixed where the reader is built, by the caller that knows
// which drill this is. core.BaselineReader takes only a check name and a
// capture name, and that is the safety property: a check cannot ask for
// another machine's history, because it has no way to name one.
func TestBaselinesPinTheWorkloadAtConstruction(t *testing.T) {
	s := &valuesStore{values: []float64{1204331, 1200000}}
	reader := Baselines(s, "110")

	got, err := reader.Values(context.Background(), "orders", "rows", 5)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(got) != 2 || got[0] != 1204331 {
		t.Fatalf("Values = %v, want the store's answer", got)
	}
	if s.lastWorkloadID != "110" {
		t.Errorf("workload asked for = %q, want 110", s.lastWorkloadID)
	}
	if s.lastCheckName != "orders" || s.lastValueName != "rows" || s.lastLimit != 5 {
		t.Errorf("asked for check %q value %q limit %d, want orders/rows/5",
			s.lastCheckName, s.lastValueName, s.lastLimit)
	}
}

// A store that cannot answer hands its error back rather than an empty
// history. The two are not the same thing and the check treats them the same
// way, but only because it was told the truth about which one happened.
func TestBaselinesReportAStoreFailure(t *testing.T) {
	s := &valuesStore{err: errors.New("database is locked")}
	if _, err := Baselines(s, "110").Values(context.Background(), "orders", "rows", 5); err == nil {
		t.Fatal("Values swallowed a store failure and reported no history instead")
	}
}

// No store and no workload mean no reader at all, which is the nil the check
// already knows how to handle: the drift half is skipped with its reason and
// nothing fails.
func TestBaselinesAreNilWithoutAStoreOrAWorkload(t *testing.T) {
	if r := Baselines(nil, "110"); r != nil {
		t.Errorf("Baselines(nil, ...) = %v, want nil", r)
	}
	if r := Baselines(&valuesStore{}, ""); r != nil {
		t.Errorf("Baselines(store, \"\") = %v, want nil", r)
	}
}

var _ core.BaselineReader = baselineReader{}
