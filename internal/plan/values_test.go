package plan

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const capturePlan = `
name: pg
workload:
  provider: pve
  id: "110"
checks:
  - type: command
    name: orders
    run: psql -tAc 'select count(*) from orders'
    capture: rows
    assert:
      min: 1
    drift:
      max_drop: 10%
`

func TestCaptureAssertAndDriftAreParsed(t *testing.T) {
	p, err := Parse([]byte(capturePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := p.Checks[0]

	if c.Capture != "rows" {
		t.Errorf("Capture = %q, want rows", c.Capture)
	}
	if c.Assert == nil || c.Assert.Min == nil || *c.Assert.Min != 1 {
		t.Fatalf("Assert = %+v, want min 1", c.Assert)
	}
	if c.Assert.Max != nil || c.Assert.Equals != nil {
		t.Errorf("bounds nobody wrote were invented: %+v", c.Assert)
	}
	if c.Drift == nil {
		t.Fatal("Drift = nil, want a 10 percent floor")
	}
	if c.Drift.MaxDrop != 10 || !c.Drift.MaxDropIsPercent {
		t.Errorf("Drift = %+v, want 10 percent", *c.Drift)
	}
}

// The parser settles what the operator wrote once. Nothing downstream reads a
// percent sign, so a plan whose tolerance changed meaning between the file and
// the engine would be a plan that fails a drill for a reason nobody can see.
func TestMaxDropIsResolvedByTheParser(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		want      float64
		wantPct   bool
		wantRefus bool // the value cannot be read as a tolerance at all
	}{
		{name: "a percentage", yaml: `max_drop: 10%`, want: 10, wantPct: true},
		{name: "a fractional percentage", yaml: `max_drop: 12.5%`, want: 12.5, wantPct: true},
		{name: "a quoted percentage", yaml: `max_drop: "10%"`, want: 10, wantPct: true},
		{name: "a bare number is absolute", yaml: `max_drop: 500`, want: 500},
		{name: "a fractional number is absolute", yaml: `max_drop: 2.5`, want: 2.5},
		{name: "a quoted number is still a number", yaml: `max_drop: "500"`, want: 500},
		{name: "a word is not a tolerance", yaml: `max_drop: soon`, wantRefus: true},
		{name: "an empty percentage is not a tolerance", yaml: `max_drop: "%"`, wantRefus: true},
		// strconv.ParseFloat takes both of these, and a tolerance that compares
		// false against everything is a check that passes forever.
		{name: "NaN is refused", yaml: `max_drop: .NaN`, wantRefus: true},
		{name: "infinity is refused", yaml: `max_drop: "Inf"`, wantRefus: true},
		{name: "a list is not a tolerance", yaml: `max_drop: [1]`, wantRefus: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n" +
				"  - type: command\n    run: echo 1\n    capture: rows\n    drift:\n      " + tt.yaml + "\n"
			p, err := Parse([]byte(src))
			if tt.wantRefus {
				if err == nil {
					t.Fatalf("Parse accepted %q", tt.yaml)
				}
				if !strings.Contains(err.Error(), "max_drop") {
					t.Errorf("error does not name the offending field: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			d := p.Checks[0].Drift
			if d == nil {
				t.Fatal("Drift = nil")
			}
			if d.MaxDrop != tt.want || d.MaxDropIsPercent != tt.wantPct {
				t.Errorf("MaxDrop = %v (percent %v), want %v (percent %v)",
					d.MaxDrop, d.MaxDropIsPercent, tt.want, tt.wantPct)
			}
		})
	}
}

// The number is stored exactly as the operator wrote it and the flag carries
// the unit: "10%" is 10, never 0.1.
//
// This is a contract with internal/checks.CheckDrift, which computes a drop as
// drop/baseline*100 and compares it against MaxDrop. A parser that helpfully
// normalised a percentage to a fraction would turn a ten percent tolerance
// into a tenth of one percent, and every drift check in the product would
// start accusing healthy backups. The two readings are a hundredfold apart.
func TestAPercentageIsStoredUnscaled(t *testing.T) {
	p, err := Parse([]byte(capturePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := p.Checks[0].Drift
	if d == nil {
		t.Fatal("Drift = nil")
	}
	if d.MaxDrop != 10 {
		t.Errorf("MaxDrop = %v, want 10: the percentage is carried unscaled", d.MaxDrop)
	}
	if !d.MaxDropIsPercent {
		t.Error("MaxDropIsPercent = false: the unit is carried by the flag, not baked into the number")
	}
}

// A plan is written, stored and read back to be executed: the API keeps the
// document a drill was queued against and the worker parses that snapshot. A
// bound that evaporated in transit would make the run's own record lie about
// what was verified.
func TestCapturedValueBoundsSurviveTheYAMLRoundTrip(t *testing.T) {
	original, err := Parse([]byte(capturePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	encoded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{"capture: rows", "assert:", "min: 1", "drift:", "max_drop"} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the document lost %q:\n%s", want, encoded)
		}
	}

	reparsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, encoded)
	}
	if !reflect.DeepEqual(original, reparsed) {
		t.Errorf("the plan changed on the way through YAML:\noriginal = %+v\nreparsed = %+v\nencoded:\n%s",
			original.Checks[0], reparsed.Checks[0], encoded)
	}
}

// The absolute form has to survive too, and it is the one a careless
// marshaller gets wrong: writing back "500%" would turn a floor of five
// hundred rows into a tolerance for losing five times the table.
func TestAnAbsoluteMaxDropSurvivesTheYAMLRoundTrip(t *testing.T) {
	src := "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n" +
		"  - type: command\n    run: echo 1\n    capture: rows\n    drift:\n      max_drop: 500\n"
	original, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	encoded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	reparsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, encoded)
	}
	d := reparsed.Checks[0].Drift
	if d == nil || d.MaxDrop != 500 || d.MaxDropIsPercent {
		t.Errorf("absolute tolerance came back as %+v:\n%s", d, encoded)
	}
}

// Every bound the plan can state must come back, and each one must stay unset
// when nobody wrote it: `min: 0` says "never negative" and saying nothing says
// nothing.
func TestEveryAssertBoundSurvivesTheYAMLRoundTrip(t *testing.T) {
	src := "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n" +
		"  - type: command\n    name: a\n    run: echo 1\n    capture: rows\n    assert: {min: 0, max: 10}\n" +
		"  - type: command\n    name: b\n    run: echo 1\n    capture: mode\n    assert: {equals: 3}\n"
	original, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	encoded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	reparsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, encoded)
	}
	if !reflect.DeepEqual(original, reparsed) {
		t.Fatalf("bounds changed on the way through YAML:\n%s", encoded)
	}
	if a := reparsed.Checks[0].Assert; a.Min == nil || *a.Min != 0 {
		t.Errorf("min: 0 did not survive: %+v", a)
	}
	if a := reparsed.Checks[1].Assert; a.Equals == nil || *a.Equals != 3 || a.Min != nil {
		t.Errorf("equals did not survive, or a bound was invented: %+v", a)
	}
}

// The new keys belong to CheckSpec. Reaching a check's Params, they would be
// forwarded to an implementation that never asked for them.
func TestValueKeysNeverReachParams(t *testing.T) {
	p, err := Parse([]byte(capturePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, reserved := range []string{"capture", "assert", "drift"} {
		if _, ok := p.Checks[0].Params[reserved]; ok {
			t.Errorf("Params still carries reserved key %q: %+v", reserved, p.Checks[0].Params)
		}
	}
	if p.Checks[0].Params["run"] == nil {
		t.Error("the command lost what it runs")
	}
}

func TestToCoreCarriesTheCapturedValueBounds(t *testing.T) {
	p, err := Parse([]byte(capturePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cfg := p.Checks[0].ToCore()

	if cfg.Capture != "rows" {
		t.Errorf("Capture = %q, want rows", cfg.Capture)
	}
	if !cfg.Assert.Any() || cfg.Assert.Min == nil || *cfg.Assert.Min != 1 {
		t.Errorf("Assert = %+v", cfg.Assert)
	}
	if cfg.Drift == nil || cfg.Drift.MaxDrop != 10 || !cfg.Drift.MaxDropIsPercent {
		t.Errorf("Drift = %+v", cfg.Drift)
	}
}

// A plan that cannot work says so when it is written, not at three in the
// morning when the only thing left to do is read a log.
func TestCapturedValueValidationErrors(t *testing.T) {
	const head = "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n"

	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "an assert with no bound at all",
			yaml: head + "  - type: command\n    run: echo 1\n    capture: rows\n    assert: {}\n",
			want: []string{"checks[0].assert", "min", "max", "equals"},
		},
		{
			name: "an assert whose only key is a typo",
			yaml: head + "  - type: command\n    run: echo 1\n    capture: rows\n    assert: {minimum: 1}\n",
			want: []string{"checks[0].assert"},
		},
		{
			name: "drift with nothing to compare",
			yaml: head + "  - type: command\n    run: echo 1\n    drift: {max_drop: 10%}\n",
			want: []string{"checks[0].drift", "capture"},
		},
		{
			name: "an assert with nothing to bound",
			yaml: head + "  - type: command\n    run: echo 1\n    assert: {min: 1}\n",
			want: []string{"checks[0].assert", "capture"},
		},
		{
			// The case that made this an allowlist. An http check has a body,
			// so "produces no output" was never true of it, and a denylist
			// let it through: the plan validated, the check ran, nothing read
			// the value, no bound was judged, and the drill reported success.
			// A bound written to catch an empty database that quietly judges
			// nothing is the failure this slice exists to remove.
			name: "a capture on a check type that does not read one",
			yaml: head + "  - type: http\n    url: http://x/health\n    capture: rows\n    assert: {min: 1}\n",
			want: []string{"checks[0].capture", "http", "command"},
		},
		{
			name: "a negative percentage",
			yaml: head + "  - type: command\n    run: echo 1\n    capture: rows\n    drift: {max_drop: \"-3%\"}\n",
			want: []string{"checks[0].drift.max_drop"},
		},
		{
			name: "a negative absolute figure",
			yaml: head + "  - type: command\n    run: echo 1\n    capture: rows\n    drift: {max_drop: -500}\n",
			want: []string{"checks[0].drift.max_drop"},
		},
		{
			name: "a drift block that states no tolerance",
			yaml: head + "  - type: command\n    run: echo 1\n    capture: rows\n    drift: {}\n",
			want: []string{"checks[0].drift", "max_drop"},
		},
		{
			name: "a ping prints nothing to read a number from",
			yaml: head + "  - type: ping\n    capture: rows\n",
			want: []string{"checks[0].capture", "ping"},
		},
		{
			name: "neither does a tcp probe",
			yaml: head + "  - type: tcp\n    port: 22\n    capture: rows\n",
			want: []string{"checks[0].capture", "tcp"},
		},
		{
			name: "nor a dns lookup",
			yaml: head + "  - type: dns\n    host: db.internal\n    capture: rows\n",
			want: []string{"checks[0].capture", "dns"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse() error = nil, want a validation failure")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%v", want, err)
				}
			}
		})
	}
}

// A capture with no claim attached is the case the design is built around: a
// measurement is worth keeping whether or not anybody has decided yet what it
// should be.
func TestABareCaptureIsAValidPlan(t *testing.T) {
	src := "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n" +
		"  - type: command\n    run: psql -tAc 'select count(*) from orders'\n    capture: rows\n"
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("a capture with no claim attached was refused: %v", err)
	}
	if p.Checks[0].Capture != "rows" {
		t.Errorf("Capture = %q", p.Checks[0].Capture)
	}
}

// The name is part of the key the value is stored under, so " rows" and "rows"
// would silently split one workload's history in two.
func TestACaptureNameIsTrimmed(t *testing.T) {
	src := "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n" +
		"  - type: command\n    run: echo 1\n    capture: \"  rows  \"\n"
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Checks[0].Capture != "rows" {
		t.Errorf("Capture = %q, want rows", p.Checks[0].Capture)
	}
}
