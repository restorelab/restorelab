package api

// What the wire says about a captured value.
//
// The dashboard reads these fields to render "orders: 1206890 (baseline
// 1204331)", and web/src/api/types.ts is a hand-written mirror of them. The
// golden fixtures are the contract; these tests are what says the contract is
// not empty.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
)

// wireCheck is the run detail as a client parses it, narrowed to what a
// captured value contributes.
type wireCheck struct {
	Name   string `json:"name"`
	Values []struct {
		Name     string   `json:"name"`
		Value    float64  `json:"value"`
		Baseline *float64 `json:"baseline"`
	} `json:"values"`
}

// measuredRun is a finished drill whose command check read a row count.
func measuredRun(id string, details map[string]any) *core.RecoveryRun {
	started := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	return &core.RecoveryRun{
		ID: id, PlanName: "nightly", SourceWorkloadID: "110", SourceName: "db",
		State: core.RunSuccess, Result: core.ResultSuccess,
		StartedAt: started, CompletedAt: started.Add(time.Minute),
		RTO: time.Minute,
		Checks: []core.CheckResult{{
			Name: "orders", Type: "command", Status: core.CheckPass, Attempts: 1,
			Message: "exit 0", Details: details,
		}},
	}
}

func runChecksOnTheWire(t *testing.T, run *core.RecoveryRun) []wireCheck {
	t.Helper()

	h := newFakeHistory()
	h.byID[run.ID] = run
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs/"+run.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var doc struct {
		Checks []wireCheck `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not a run document: %v", err)
	}
	return doc.Checks
}

func TestARunCarriesWhatItsChecksMeasured(t *testing.T) {
	checksOnWire := runChecksOnTheWire(t, measuredRun("run-measured", map[string]any{
		checks.DetailCaptured: map[string]float64{"rows": 1206890},
		checks.DetailBaseline: map[string]float64{"rows": 1204331},
	}))

	if len(checksOnWire) != 1 || len(checksOnWire[0].Values) != 1 {
		t.Fatalf("checks = %#v, want one check with one value", checksOnWire)
	}
	v := checksOnWire[0].Values[0]
	if v.Name != "rows" || v.Value != 1206890 {
		t.Errorf("value = %#v, want rows/1206890", v)
	}
	if v.Baseline == nil || *v.Baseline != 1204331 {
		t.Errorf("baseline = %#v, want 1204331", v.Baseline)
	}
}

// The field is present and null, not absent. A client that renders a missing
// baseline and a zero one the same way would show an empty database where a
// first drill stands.
func TestAValueWithNoBaselineCarriesANullOne(t *testing.T) {
	checksOnWire := runChecksOnTheWire(t, measuredRun("run-first", map[string]any{
		checks.DetailCaptured: map[string]float64{"rows": 1206890},
	}))

	if len(checksOnWire) != 1 || len(checksOnWire[0].Values) != 1 {
		t.Fatalf("checks = %#v, want one check with one value", checksOnWire)
	}
	if b := checksOnWire[0].Values[0].Baseline; b != nil {
		t.Errorf("baseline = %v, want null", *b)
	}
}

func TestACheckThatMeasuredNothingCarriesNoValuesOnTheWire(t *testing.T) {
	checksOnWire := runChecksOnTheWire(t, measuredRun("run-plain", nil))
	if len(checksOnWire) != 1 {
		t.Fatalf("checks = %#v, want one", checksOnWire)
	}
	if checksOnWire[0].Values != nil {
		t.Errorf("values = %#v, want the field absent", checksOnWire[0].Values)
	}
}
