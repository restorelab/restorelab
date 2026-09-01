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
