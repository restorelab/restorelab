package checks

import (
	"math"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

// floatPtr is the AssertSpec bound literal. Every bound is a pointer because
// zero is a meaningful one: "min: 0" says "never negative" and is not the same
// statement as saying nothing at all.
func floatPtr(v float64) *float64 { return &v }

// What psql -tA actually prints, which is not a clean token: a trailing
// newline always, padding when the terminal is involved, and the blank lines
// the tuples-only mode still emits around the row. A parser that only handles
// the clean case works in a unit test and fails on the first real database.
func TestParseCaptured_AcceptsWhatPsqlPrints(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   float64
	}{
		{"bare integer", "1204331", 1204331},
		{"trailing newline", "1204331\n", 1204331},
		{"surrounding spaces", " 1204331 ", 1204331},
		{"decimal", "1.5", 1.5},
		{"negative", "-3", -3},
		{"blank lines around it", "\n\n1204331\n\n", 1204331},
		{"crlf from a windows guest", "\r\n1204331\r\n", 1204331},
		{"tabs and spaces", "\t 1204331 \t\n", 1204331},
		{"zero", "0\n", 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCaptured(tt.stdout)
			if err != nil {
				t.Fatalf("ParseCaptured(%q) returned %v, want %v", tt.stdout, err, tt.want)
			}
			if got != tt.want {
				t.Errorf("ParseCaptured(%q) = %v, want %v", tt.stdout, got, tt.want)
			}
		})
	}
}

// The pitfall this whole file exists to avoid. strconv.ParseFloat accepts
// "NaN", "Inf" and their spellings, and a NaN that reaches the database
// becomes a NaN baseline; every comparison against NaN is false, so the check
// passes forever and nobody is told. It has to be refused here, at the only
// place a value enters the product.
func TestParseCaptured_RefusesNonFiniteNumbers(t *testing.T) {
	for _, in := range []string{"NaN", "nan", "Inf", "inf", "+Inf", "-Inf", "Infinity", "-infinity"} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseCaptured(in)
			if err == nil {
				t.Fatalf("ParseCaptured(%q) = %v, want an error: a non-finite value silently passes every later comparison", in, got)
			}
			if !strings.Contains(err.Error(), in) {
				t.Errorf("error = %q, want it to quote what was read (%q)", err, in)
			}
		})
	}
}

func TestParseCaptured_RefusesWhatIsNotANumber(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", "three", "1204331 rows", "1,204,331", "--"} {
		t.Run("input "+in, func(t *testing.T) {
			if got, err := ParseCaptured(in); err == nil {
				t.Fatalf("ParseCaptured(%q) = %v, want an error", in, got)
			}
		})
	}
}

// Two numbers is not one measurement. Taking the first would record something
// the operator never asked for, and recording the wrong number silently is
// the exact failure this slice exists to prevent.
func TestParseCaptured_RefusesSeveralNumbers(t *testing.T) {
	got, err := ParseCaptured("1204331\n1204332\n")
	if err == nil {
		t.Fatalf("ParseCaptured of two lines = %v, want an error", got)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error = %q, want it to say how many lines were found", err)
	}
}

func TestBaseline_NoValuesIsNoBaseline(t *testing.T) {
	if got, ok := Baseline(nil); ok {
		t.Errorf("Baseline(nil) = %v, true; want ok=false", got)
	}
	if got, ok := Baseline([]float64{}); ok {
		t.Errorf("Baseline([]) = %v, true; want ok=false", got)
	}
}

func TestBaseline_OneValueIsThatValue(t *testing.T) {
	got, ok := Baseline([]float64{1204331})
	if !ok || got != 1204331 {
		t.Errorf("Baseline([1204331]) = %v, %v; want 1204331, true", got, ok)
	}
}

func TestBaseline_EvenCountIsTheMeanOfTheTwoMiddleValues(t *testing.T) {
	got, ok := Baseline([]float64{10, 20, 30, 40})
	if !ok || got != 25 {
		t.Errorf("Baseline([10 20 30 40]) = %v, %v; want 25, true", got, ok)
	}
}

// The property the whole design rests on. A drill that read 0 must not make 0
// the normal for the next drill.
//
// The two alternatives both fail here. A mean of [1200000, 1200000, 0] is
// 800000, so a second empty database sits comfortably inside a 10 percent
// tolerance of a baseline the collapse itself moved. The previous value alone
// is worse: it ratchets, failing once and then comparing 0 against 0 forever,
// which is the precise moment everybody stops paying attention.
func TestBaseline_ACollapsedReadingDoesNotBecomeTheNewNormal(t *testing.T) {
	for _, values := range [][]float64{
		{1200000, 1200000, 0},
		{0, 1200000, 1200000},
		{1200000, 0, 1200000},
	} {
		got, ok := Baseline(values)
		if !ok || got != 1200000 {
			t.Errorf("Baseline(%v) = %v, %v; want 1200000, true", values, got, ok)
		}
	}
}

// Baseline is handed the slice a store returned, and the caller may well use
// it again to render the history beside the verdict. Sorting in place would
// reorder the caller's "most recent first" behind its back.
func TestBaseline_DoesNotReorderItsInput(t *testing.T) {
	values := []float64{30, 10, 20}
	Baseline(values)
	for i, want := range []float64{30, 10, 20} {
		if values[i] != want {
			t.Fatalf("input was reordered: %v", values)
		}
	}
}

// Defence in depth behind ParseCaptured. A NaN that reached storage before
// this rule existed would otherwise poison the sort and hand back a NaN
// baseline, which compares false against everything.
func TestBaseline_IgnoresNonFiniteValues(t *testing.T) {
	got, ok := Baseline([]float64{math.NaN(), 10, 20, 30, math.Inf(1)})
	if !ok || got != 20 {
		t.Errorf("Baseline with NaN and Inf = %v, %v; want 20, true", got, ok)
	}
	if _, ok := Baseline([]float64{math.NaN()}); ok {
		t.Error("Baseline of only NaN reported a baseline; want ok=false")
	}
}

func TestCheckAssert_NoBoundIsNoOpinion(t *testing.T) {
	if msg := CheckAssert(0, core.AssertSpec{}); msg != "" {
		t.Errorf("CheckAssert with no bound = %q, want no violation", msg)
	}
}

func TestCheckAssert_Min(t *testing.T) {
	if msg := CheckAssert(1204331, core.AssertSpec{Min: floatPtr(1)}); msg != "" {
		t.Errorf("value above the minimum reported %q, want no violation", msg)
	}
	if msg := CheckAssert(1, core.AssertSpec{Min: floatPtr(1)}); msg != "" {
		t.Errorf("value exactly at the minimum reported %q; min is inclusive", msg)
	}
	msg := CheckAssert(0, core.AssertSpec{Min: floatPtr(1204331)})
	if msg == "" {
		t.Fatal("an empty table under min: 1204331 reported no violation")
	}
	assertNames(t, msg, "0", "1204331")
}

func TestCheckAssert_Max(t *testing.T) {
	if msg := CheckAssert(3, core.AssertSpec{Max: floatPtr(3)}); msg != "" {
		t.Errorf("value exactly at the maximum reported %q; max is inclusive", msg)
	}
	msg := CheckAssert(90210, core.AssertSpec{Max: floatPtr(500)})
	if msg == "" {
		t.Fatal("a value over max: 500 reported no violation")
	}
	assertNames(t, msg, "90210", "500")
}

func TestCheckAssert_Equals(t *testing.T) {
	if msg := CheckAssert(42, core.AssertSpec{Equals: floatPtr(42)}); msg != "" {
		t.Errorf("value equal to equals reported %q, want no violation", msg)
	}
	msg := CheckAssert(41, core.AssertSpec{Equals: floatPtr(42)})
	if msg == "" {
		t.Fatal("a value different from equals reported no violation")
	}
	assertNames(t, msg, "41", "42")
}

// A spec may state several bounds. Exactly one message comes back, and it is
// the one for the bound the plan states first, so that the same reading always
// produces the same sentence in the report.
func TestCheckAssert_ReportsTheFirstBoundViolated(t *testing.T) {
	spec := core.AssertSpec{Min: floatPtr(1204331), Max: floatPtr(90210), Equals: floatPtr(777)}
	msg := CheckAssert(0, spec)
	if !strings.Contains(msg, "1204331") {
		t.Errorf("CheckAssert = %q, want the min violation first", msg)
	}
	if strings.Contains(msg, "90210") || strings.Contains(msg, "777") {
		t.Errorf("CheckAssert = %q, want exactly one bound named", msg)
	}

	// All bounds hold at once: still no violation.
	if msg := CheckAssert(5, core.AssertSpec{Min: floatPtr(1), Max: floatPtr(10), Equals: floatPtr(5)}); msg != "" {
		t.Errorf("a value satisfying every bound reported %q", msg)
	}
}

// A row count of 1 204 331 must not reach the report as 1.204331e+06. The
// number an operator reads at 03:00 is the number their database printed.
func TestCheckAssert_MessageDoesNotUseScientificNotation(t *testing.T) {
	msg := CheckAssert(1204331000, core.AssertSpec{Max: floatPtr(1000)})
	if strings.Contains(msg, "e+") || strings.Contains(msg, "E+") {
		t.Errorf("CheckAssert = %q, want a plain decimal number", msg)
	}
	if !strings.Contains(msg, "1204331000") {
		t.Errorf("CheckAssert = %q, want the value written out in full", msg)
	}
}

// A non-finite value must never be graded as "within bounds". Every
// comparison against NaN is false, so the naive implementation returns "" and
// the check passes. ParseCaptured is the gate, but the gate is one call site
// away and this is cheaper than trusting it forever.
func TestCheckAssert_NonFiniteValueIsAViolation(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if msg := CheckAssert(v, core.AssertSpec{Min: floatPtr(1)}); msg == "" {
			t.Errorf("CheckAssert(%v) reported no violation", v)
		}
	}
}

// max_drop is stored as the number the operator wrote: "10%" is MaxDrop 10
// with MaxDropIsPercent, not 0.1. This test is the contract, because the plan
// parser and this evaluator have to agree on it and nothing else checks.
func TestCheckDrift_PercentageTolerance(t *testing.T) {
	pct := core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true}

	if msg := CheckDrift(950, 1000, pct); msg != "" {
		t.Errorf("a 5 percent drop under a 10 percent tolerance reported %q", msg)
	}
	if msg := CheckDrift(900, 1000, pct); msg != "" {
		t.Errorf("a drop exactly at the tolerance reported %q; the tolerance is inclusive", msg)
	}

	msg := CheckDrift(800, 1000, pct)
	if msg == "" {
		t.Fatal("a 20 percent drop under a 10 percent tolerance reported no violation")
	}
	assertNames(t, msg, "800", "1000", "20", "10")
}

func TestCheckDrift_AbsoluteTolerance(t *testing.T) {
	abs := core.DriftSpec{MaxDrop: 500}

	if msg := CheckDrift(1204000, 1204331, abs); msg != "" {
		t.Errorf("a drop of 331 under an absolute tolerance of 500 reported %q", msg)
	}
	if msg := CheckDrift(1203831, 1204331, abs); msg != "" {
		t.Errorf("a drop exactly at the tolerance reported %q; the tolerance is inclusive", msg)
	}

	msg := CheckDrift(1200000, 1204331, abs)
	if msg == "" {
		t.Fatal("a drop of 4331 under an absolute tolerance of 500 reported no violation")
	}
	assertNames(t, msg, "1200000", "1204331", "500")
}

// max_drop is a floor, not a band. A table that grew is not a failed
// restoration, and a product that graded it as one would teach its operator to
// ignore red.
func TestCheckDrift_ARiseIsNeverAViolation(t *testing.T) {
	specs := []core.DriftSpec{
		{MaxDrop: 10, MaxDropIsPercent: true},
		{MaxDrop: 500},
		{MaxDrop: 0, MaxDropIsPercent: true},
		{MaxDrop: 0},
	}
	for _, spec := range specs {
		for _, value := range []float64{1204332, 2400000, 1e9} {
			if msg := CheckDrift(value, 1204331, spec); msg != "" {
				t.Errorf("CheckDrift(%v, 1204331, %+v) = %q, want no violation for a rise", value, spec, msg)
			}
		}
		if msg := CheckDrift(1204331, 1204331, spec); msg != "" {
			t.Errorf("an unchanged value under %+v reported %q", spec, msg)
		}
	}
}

// A zero baseline with a percentage tolerance is not a division. Going from 0
// to 0 is not a 0 percent drop and not an infinite one, it is no drop at all.
// The naive implementation divides by zero, gets NaN, compares NaN against the
// tolerance, gets false, and passes silently forever.
func TestCheckDrift_ZeroBaselineWithAPercentageIsNotADivision(t *testing.T) {
	pct := core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true}

	if msg := CheckDrift(0, 0, pct); msg != "" {
		t.Errorf("CheckDrift(0, 0) = %q, want no violation: no drop happened", msg)
	}
	if msg := CheckDrift(1204331, 0, pct); msg != "" {
		t.Errorf("CheckDrift(1204331, 0) = %q, want no violation: the value rose", msg)
	}
	// Below a zero baseline there is a real fall, and no percentage of zero
	// permits any fall. Reporting it beats returning a NaN comparison.
	msg := CheckDrift(-5, 0, pct)
	if msg == "" {
		t.Error("a fall below a zero baseline reported no violation")
	}
	if strings.Contains(msg, "NaN") {
		t.Errorf("CheckDrift = %q, want no NaN in an operator-facing message", msg)
	}
}

// The same rule for an absolute tolerance: zero is a baseline like any other.
func TestCheckDrift_ZeroBaselineWithAnAbsoluteTolerance(t *testing.T) {
	if msg := CheckDrift(0, 0, core.DriftSpec{MaxDrop: 500}); msg != "" {
		t.Errorf("CheckDrift(0, 0) = %q, want no violation", msg)
	}
}

// A collapse to zero from a real baseline is the case that motivated the whole
// slice. It must trip whichever way the tolerance is written.
func TestCheckDrift_CollapseToZero(t *testing.T) {
	for _, spec := range []core.DriftSpec{
		{MaxDrop: 99, MaxDropIsPercent: true},
		{MaxDrop: 1204330},
	} {
		msg := CheckDrift(0, 1204331, spec)
		if msg == "" {
			t.Errorf("a collapse to zero under %+v reported no violation", spec)
		}
	}
}

// A negative baseline is not what a row count looks like, but free space and
// deltas are captured values too. The drop has to be measured against the
// baseline's magnitude, or the percentage comes out negative and every
// collapse passes.
func TestCheckDrift_NegativeBaselineComparesAgainstMagnitude(t *testing.T) {
	pct := core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true}
	if msg := CheckDrift(-200, -100, pct); msg == "" {
		t.Error("a fall from -100 to -200 reported no violation")
	}
	if msg := CheckDrift(-105, -100, pct); msg != "" {
		t.Errorf("a 5 percent fall from -100 reported %q", msg)
	}
}

func TestCheckDrift_NonFiniteIsAViolation(t *testing.T) {
	pct := core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true}
	for _, tt := range []struct{ value, baseline float64 }{
		{math.NaN(), 1000},
		{1000, math.NaN()},
		{math.Inf(-1), 1000},
	} {
		if msg := CheckDrift(tt.value, tt.baseline, pct); msg == "" {
			t.Errorf("CheckDrift(%v, %v) reported no violation", tt.value, tt.baseline)
		}
	}
}

// assertNames fails when the message leaves out a number the reader needs.
// The reader is somebody looking at a report at 03:00 without the plan open,
// so the value, the bound and the baseline all have to be in the sentence.
func assertNames(t *testing.T, msg string, numbers ...string) {
	t.Helper()
	for _, n := range numbers {
		if !strings.Contains(msg, n) {
			t.Errorf("message = %q, want it to name %s", msg, n)
		}
	}
}
