package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
)

func TestFormatValueNeverGoesExponential(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1204331, "1204331"},
		{1206890, "1206890"},
		{0, "0"},
		{1.5, "1.5"},
		{-3, "-3"},
		{1e21, "1000000000000000000000"},
	}
	for _, tc := range cases {
		if got := FormatValue(tc.in); got != tc.want {
			t.Errorf("FormatValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// capturedCheck is a command check that read a number, with whatever the plan
// declared about it already folded into the details the way the check writes
// them.
func capturedCheck(captured, baseline map[string]float64) core.CheckResult {
	c := core.CheckResult{
		Name:    "orders",
		Type:    "command",
		Status:  core.CheckPass,
		Message: "exit 0",
		Details: map[string]any{},
	}
	if captured != nil {
		c.Details[checks.DetailCaptured] = captured
	}
	if baseline != nil {
		c.Details[checks.DetailBaseline] = baseline
	}
	return c
}

func TestCheckDTOCarriesTheValueAndItsBaseline(t *testing.T) {
	dto := NewCheckDTO(capturedCheck(
		map[string]float64{"orders": 1206890},
		map[string]float64{"orders": 1204331},
	))

	if len(dto.Values) != 1 {
		t.Fatalf("values = %#v, want exactly one", dto.Values)
	}
	got := dto.Values[0]
	if got.Name != "orders" || got.Value != 1206890 {
		t.Errorf("value = %#v, want orders/1206890", got)
	}
	if got.Baseline == nil {
		t.Fatal("baseline is nil, want 1204331")
	}
	if *got.Baseline != 1204331 {
		t.Errorf("baseline = %v, want 1204331", *got.Baseline)
	}
}

func TestCheckDTOLeavesTheBaselineNullWhenThereIsNone(t *testing.T) {
	dto := NewCheckDTO(capturedCheck(map[string]float64{"orders": 1206890}, nil))

	if len(dto.Values) != 1 {
		t.Fatalf("values = %#v, want exactly one", dto.Values)
	}
	if dto.Values[0].Baseline != nil {
		t.Errorf("baseline = %v, want nil: a first drill has nothing to compare against",
			*dto.Values[0].Baseline)
	}
}

// A check that captured nothing carries no values at all, so a client can
// tell "this check measures nothing" from "this check measured zero".
func TestCheckDTOOfACheckThatMeasuredNothingCarriesNoValues(t *testing.T) {
	dto := NewCheckDTO(core.CheckResult{Name: "ssh", Type: "tcp", Status: core.CheckPass})
	if dto.Values != nil {
		t.Errorf("values = %#v, want none", dto.Values)
	}
}

// Details is a map of any that reaches the report either straight from the
// check or back out of the database as decoded JSON. Both shapes have to
// read, or a value would appear on a live drill and vanish from its own
// stored report.
func TestCheckDTOReadsValuesBackOutOfDecodedJSON(t *testing.T) {
	c := core.CheckResult{
		Name: "orders", Type: "command", Status: core.CheckPass,
		Details: map[string]any{
			checks.DetailCaptured: map[string]any{"orders": float64(1206890)},
			checks.DetailBaseline: map[string]any{"orders": float64(1204331)},
		},
	}
	dto := NewCheckDTO(c)
	if len(dto.Values) != 1 || dto.Values[0].Value != 1206890 {
		t.Fatalf("values = %#v, want orders/1206890", dto.Values)
	}
	if dto.Values[0].Baseline == nil || *dto.Values[0].Baseline != 1204331 {
		t.Errorf("baseline = %#v, want 1204331", dto.Values[0].Baseline)
	}
}

func TestCheckDTOValuesAreSortedByName(t *testing.T) {
	dto := NewCheckDTO(capturedCheck(
		map[string]float64{"rows": 3, "orders": 2, "customers": 1}, nil))

	var names []string
	for _, v := range dto.Values {
		names = append(names, v.Name)
	}
	want := []string{"customers", "orders", "rows"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", names, want)
	}
}

// A baseline recorded for a name nothing captured is not a value: it would
// be a comparison against nothing at all.
func TestCheckDTOIgnoresABaselineWithNoValue(t *testing.T) {
	dto := NewCheckDTO(capturedCheck(nil, map[string]float64{"orders": 1204331}))
	if dto.Values != nil {
		t.Errorf("values = %#v, want none", dto.Values)
	}
}

func TestValueLineShowsTheComparison(t *testing.T) {
	baseline := 1204331.0
	line := valueLine(CapturedValueDTO{Name: "orders", Value: 1206890, Baseline: &baseline})
	if want := "orders: 1206890 (baseline 1204331)"; line != want {
		t.Errorf("line = %q, want %q", line, want)
	}
}

func TestValueLineWithNoBaselineShowsTheNoValueGlyph(t *testing.T) {
	line := valueLine(CapturedValueDTO{Name: "orders", Value: 0})
	if want := "orders: 0 (baseline --)"; line != want {
		t.Errorf("line = %q, want %q", line, want)
	}
}

// runWithACapturedValue is a finished drill whose one check read a number.
func runWithACapturedValue(baseline map[string]float64) *core.RecoveryRun {
	started := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	return &core.RecoveryRun{
		ID:               "run-1",
		PlanName:         "nightly",
		SourceWorkloadID: "110",
		SourceName:       "db",
		State:            core.RunSuccess,
		Result:           core.ResultSuccess,
		StartedAt:        started,
		CompletedAt:      started.Add(time.Minute),
		Checks: []core.CheckResult{
			capturedCheck(map[string]float64{"orders": 1206890}, baseline),
		},
		RTO: time.Minute,
	}
}

func TestHTMLShowsACapturedValueBesideItsCheck(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, runWithACapturedValue(map[string]float64{"orders": 1204331})); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if want := "orders: 1206890 (baseline 1204331)"; !strings.Contains(buf.String(), want) {
		t.Errorf("the page does not contain %q:\n%s", want, buf.String())
	}
}

func TestHTMLShowsAMissingBaselineAsTheNoValueGlyph(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, runWithACapturedValue(nil)); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	page := buf.String()
	if want := "orders: 1206890 (baseline --)"; !strings.Contains(page, want) {
		t.Errorf("the page does not contain %q:\n%s", want, page)
	}
	if strings.Contains(page, "baseline 0") {
		t.Error("an unknown baseline is rendered as 0, which reads as an empty database")
	}
}

func TestTextShowsACapturedValueBesideItsCheck(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, runWithACapturedValue(map[string]float64{"orders": 1204331}), Options{}); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if want := "orders: 1206890 (baseline 1204331)"; !strings.Contains(buf.String(), want) {
		t.Errorf("the report does not contain %q:\n%s", want, buf.String())
	}
}

func TestJSONCarriesTheValuesOfACheck(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, runWithACapturedValue(map[string]float64{"orders": 1204331})); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var doc struct {
		Checks []struct {
			Values []struct {
				Name     string   `json:"name"`
				Value    float64  `json:"value"`
				Baseline *float64 `json:"baseline"`
			} `json:"values"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Checks) != 1 || len(doc.Checks[0].Values) != 1 {
		t.Fatalf("checks = %#v, want one check with one value", doc.Checks)
	}
	v := doc.Checks[0].Values[0]
	if v.Name != "orders" || v.Value != 1206890 || v.Baseline == nil || *v.Baseline != 1204331 {
		t.Errorf("value = %#v, want orders 1206890 baseline 1204331", v)
	}
}
