package core

import (
	"context"
	"time"
)

// Target is what a check runs against: the restored workload, once it is
// reachable. Vars feeds template expansion inside check parameters
// ("http://{{ .ip }}:3000/health").
type Target struct {
	IP         string
	IPs        []string
	WorkloadID string
	Node       string
	Name       string
	Vars       map[string]string

	// Exec runs commands inside the recovered guest, when the provider can.
	// It is nil otherwise, and checks that need it must report that clearly
	// rather than failing as if the service were down.
	Exec GuestExecutor

	// Baseline reads what previous drills of this workload measured, when
	// there is a history to read. Nil otherwise, and a check that needs it
	// reports that clearly rather than failing as if the data were gone.
	Baseline BaselineReader
}

// TemplateVars renders the variables exposed to check parameter templates.
func (t Target) TemplateVars() map[string]string {
	vars := map[string]string{
		"ip":     t.IP,
		"id":     t.WorkloadID,
		"node":   t.Node,
		"name":   t.Name,
		"target": t.IP,
	}
	for k, v := range t.Vars {
		vars[k] = v
	}
	return vars
}

// CheckConfig is one configured check from a recovery plan. Params holds the
// check-specific fields; each check decodes them itself.
type CheckConfig struct {
	Type          string
	Name          string
	Timeout       time.Duration
	Retries       int
	RetryInterval time.Duration
	Critical      bool // a failing critical check fails the whole run
	Params        map[string]any

	// Proves is what this check establishes when it passes. It is decided
	// where the check is built from a plan, because that is the only place
	// that knows what the check actually runs; the registry never reads it,
	// and ProvenBy only counts it for a check that passed.
	Proves ProofLevel

	// Capture names the number this check reads out of the workload, or is
	// empty. The check's trimmed stdout is parsed as a number and recorded
	// against the run under this name.
	//
	// Numbers only, and that is not a limitation to lift later: a check that
	// wants to assert a word already has expect and stdout_matches, and drift
	// needs arithmetic. There is no useful sense in which "active" drifted
	// from "inactive".
	Capture string

	// Assert is a bound on the captured value that the operator vouches for,
	// or nil. Violating it fails the check.
	Assert *AssertSpec

	// Drift is a bound on how far the captured value may fall against what
	// previous drills measured, or nil. Violating it fails the check.
	Drift *DriftSpec
}

// AssertSpec is what the plan says a captured value must be.
//
// Every field is a pointer because zero is a meaningful bound: `min: 0` says
// "never negative" and is not the same statement as saying nothing at all.
type AssertSpec struct {
	Min    *float64
	Max    *float64
	Equals *float64
}

// Any reports whether the spec states any bound at all. A spec that states
// none is a plan that meant to say something and did not, which validation
// refuses rather than silently accepting as "always true".
func (a *AssertSpec) Any() bool {
	return a != nil && (a.Min != nil || a.Max != nil || a.Equals != nil)
}

// DriftSpec is how far a captured value may fall against its baseline before
// the check fails.
//
// It is a floor, not a band. A value that grew is not a recovery failure, and
// a product that graded it as one would teach its operator to ignore red.
type DriftSpec struct {
	// MaxDrop is the tolerated fall, in the unit MaxDropIsPercent selects.
	//
	// It holds the number the operator wrote, unscaled: "10%" is MaxDrop 10
	// with MaxDropIsPercent true, never 0.1. The alternative, normalising a
	// percentage to a fraction at parse time, reads well right up to the
	// moment somebody writes 0.1 meaning a tenth of a percent, and the two
	// mistakes are a hundredfold apart in the direction that silently passes
	// a collapsed database. The unit is carried, not baked in.
	MaxDrop float64

	// MaxDropIsPercent says how to read MaxDrop: a percentage of the
	// baseline, or an absolute figure. The plan parser decides once what the
	// operator wrote, and nothing downstream ever re-reads a percent sign.
	MaxDropIsPercent bool
}

// BaselineReader reads what previous drills of this workload measured.
//
// It hangs off Target beside Exec, and it is nil for the same kind of reason:
// an optional capability the environment may not offer. Nil means there is no
// history to compare against, and a drift check with no history is skipped
// with its reason, never failed. Being unable to read the past is not
// evidence about the present, which is the rule C5 established and E1 kept.
//
// It is deliberately one read-only method rather than a store. The recovery
// engine has never held a database handle and must not start: the journal
// writes history, the engine emits events, and that separation is what stops
// a broken database from being able to fail a drill.
type BaselineReader interface {
	Values(ctx context.Context, checkName, valueName string, limit int) ([]float64, error)
}

// CheckStatus is the outcome of a single check.
type CheckStatus string

// The four outcomes a check can report. CheckError is not a failure of the
// service under test: it means the check itself could not run.
const (
	CheckPass    CheckStatus = "pass"
	CheckFail    CheckStatus = "fail"
	CheckError   CheckStatus = "error" // check could not run (bad config, unreachable target)
	CheckSkipped CheckStatus = "skipped"
)

// CheckResult is what a check reports back. It is stored and rendered as-is,
// so Message must stay human readable and free of secrets.
type CheckResult struct {
	Name        string
	Type        string
	Status      CheckStatus
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
	Attempts    int
	Message     string
	Details     map[string]any
}

// OK reports whether the check passed.
func (r CheckResult) OK() bool { return r.Status == CheckPass }

// Check runs a single technical or application-level validation.
// Implementations must honour ctx, never panic, and never mutate the target.
type Check interface {
	// Type is the identifier used in recovery plans ("tcp", "http", ...).
	Type() string
	// Run executes the check once. Retries are handled by the caller.
	Run(ctx context.Context, target Target, cfg CheckConfig) CheckResult
}
