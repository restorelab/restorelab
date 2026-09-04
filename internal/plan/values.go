package plan

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/core"
)

// assertSpec is a check's `assert:` block as a plan writes it.
//
// It is a shape of its own rather than yaml tags on core.AssertSpec because
// the plan file is a published interface and the core struct is not: a field
// renamed in core must not silently change what an operator has to type, and
// a key renamed in the file must be a deliberate act here. The two are
// converted, not shared.
type assertSpec struct {
	Min    *float64 `yaml:"min"`
	Max    *float64 `yaml:"max"`
	Equals *float64 `yaml:"equals"`
}

// core renders the block as the runtime bound. Nil in, nil out: a check that
// declared nothing must reach the engine declaring nothing, not declaring an
// empty bound that holds for every value.
func (a *assertSpec) core() *core.AssertSpec {
	if a == nil {
		return nil
	}
	return &core.AssertSpec{Min: a.Min, Max: a.Max, Equals: a.Equals}
}

// driftSpec is a check's `drift:` block as a plan writes it.
type driftSpec struct {
	MaxDrop maxDrop `yaml:"max_drop"`
}

// core renders the block as the runtime bound. A block that stated no
// tolerance arrives here as a zero one, which Validate refuses: reading an
// empty `drift:` as "no fall at all is tolerated" would be inventing the
// strictest possible reading of something somebody left unfinished.
func (d *driftSpec) core() *core.DriftSpec {
	if d == nil {
		return nil
	}
	return &core.DriftSpec{MaxDrop: d.MaxDrop.value, MaxDropIsPercent: d.MaxDrop.isPercent}
}

// maxDrop is a drift tolerance as a plan writes it: "10%" or a bare number.
//
// The percent sign is a fact about the file, not about the comparison, and it
// is resolved here once and for all. Carrying the string further would mean
// every consumer downstream deciding again what "10" meant, and being wrong
// about it in one of them turns a tolerance for losing a tenth of a table into
// a tolerance for losing all but ten rows.
type maxDrop struct {
	value     float64
	isPercent bool
}

// UnmarshalYAML accepts "10%", "12.5%", 500, 2.5, and the quoted spellings of
// each, because a plan written by hand quotes inconsistently and the meaning
// does not change.
func (m *maxDrop) UnmarshalYAML(node *yaml.Node) error {
	var raw any
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("invalid max_drop: %w", err)
	}

	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if pct, isPct := strings.CutSuffix(s, "%"); isPct {
			f, err := parseTolerance(pct)
			if err != nil {
				return err
			}
			m.value, m.isPercent = f, true
			return nil
		}
		f, err := parseTolerance(s)
		if err != nil {
			return err
		}
		m.value, m.isPercent = f, false
	case int:
		m.value, m.isPercent = float64(v), false
	case float64:
		if !finite(v) {
			return fmt.Errorf("invalid max_drop %v: a tolerance that compares false against every value is a check that passes forever", v)
		}
		m.value, m.isPercent = v, false
	default:
		return fmt.Errorf(`invalid max_drop %v (%T): write a percentage like "10%%" or a number of units`, raw, raw)
	}
	return nil
}

// parseTolerance reads the numeric part of a max_drop.
//
// NaN and infinity are refused explicitly: strconv.ParseFloat accepts both,
// and either one silently disables the comparison it was written to make.
func parseTolerance(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf(`invalid max_drop %q: write a percentage like "10%%" or a number of units`, s)
	}
	if !finite(f) {
		return 0, fmt.Errorf("invalid max_drop %q: a tolerance that compares false against every value is a check that passes forever", s)
	}
	return f, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// statesATolerance reports whether a drift block bounds anything at all. It is
// a positive test rather than "not negative" so that zero, and a NaN that
// reached a hand-built spec without passing through the parser, are both
// refused: neither is a fall a value can stay within.
func statesATolerance(d *core.DriftSpec) bool {
	return d != nil && d.MaxDrop > 0
}

// assertYAML renders a bound back into the shape a plan writes, omitting the
// bounds nobody stated. The stored plan is what the worker executes and what
// the run's own record is read against, so a bound that did not survive being
// written out would make the snapshot lie about what was verified.
func assertYAML(a *core.AssertSpec) map[string]any {
	out := make(map[string]any, 3)
	if a.Min != nil {
		out["min"] = *a.Min
	}
	if a.Max != nil {
		out["max"] = *a.Max
	}
	if a.Equals != nil {
		out["equals"] = *a.Equals
	}
	return out
}

// driftYAML renders a tolerance back into the shape a plan writes. The percent
// sign has to come back with it: written out bare, a ten percent tolerance
// would read as a tolerance for losing ten rows the next time the document is
// parsed, which is the same value meaning something entirely different.
func driftYAML(d *core.DriftSpec) map[string]any {
	if d.MaxDropIsPercent {
		return map[string]any{"max_drop": strconv.FormatFloat(d.MaxDrop, 'f', -1, 64) + "%"}
	}
	return map[string]any{"max_drop": d.MaxDrop}
}

// capturingCheckTypes are the check types that actually read a value out of
// their output and evaluate the bounds declared on it.
//
// An allowlist, and the earlier denylist here was a mistake worth recording.
// It named the types with no output to read (ping, tcp, dns) and let every
// other type through, on the reasoning that capturing out of an HTTP response
// body is a plausible thing to want next and an allowlist would refuse a plan
// that works. But no check implements that today, so what the denylist
// actually allowed was a plan declaring capture and assert on an http check,
// passing validation, recording nothing, judging nothing, and reporting
// success.
//
// That is a silent pass on a bound somebody wrote to catch an empty database,
// which is the precise failure this whole slice exists to remove. Refusing a
// plan that does not work yet costs its author one error message at write
// time; accepting it costs them the belief that they are covered.
//
// When a second check type implements capture, add it here in the same commit
// that implements it, and Registry.RunOne is the mechanical backstop for the
// day somebody adds it here and forgets the rest.
var capturingCheckTypes = map[string]bool{
	"command": true,
}
