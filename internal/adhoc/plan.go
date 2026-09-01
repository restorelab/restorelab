// Package adhoc builds and validates the plan behind `recovery test`: a drill
// described by nothing more than a workload id and a handful of overrides,
// rather than a plan file on disk.
package adhoc

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/plan"
)

// Options is a drill described in the smallest terms that still make one:
// which workload, from which backup, checked how.
type Options struct {
	WorkloadID     string
	ProviderID     string
	BackupProvider string
	Backup         string // "" or "latest" means the most recent
	Node           string
	Storage        string
	Pool           string
	Network        string
	Checks         []string
	CheckRetries   int
	CheckInterval  time.Duration
	StartupTimeout time.Duration
	RTOTarget      time.Duration
	CPULimit       int
	MemoryLimitMB  int
	SkipStartup    bool
}

// Defaults an ad-hoc drill uses when the caller says nothing. They are here
// rather than in the CLI flags because the API needs exactly the same ones:
// a drill triggered over HTTP and one triggered from a terminal must be the
// same drill.
const (
	DefaultCheckRetries  = 10
	DefaultCheckInterval = 6 * time.Second
	DefaultCheck         = "tcp:22"
)

// applyDefaults fills what the caller left empty.
func (o *Options) applyDefaults() {
	if o.CheckRetries == 0 {
		o.CheckRetries = DefaultCheckRetries
	}
	if o.CheckInterval == 0 {
		o.CheckInterval = DefaultCheckInterval
	}
	if !o.SkipStartup && len(o.Checks) == 0 {
		o.Checks = []string{DefaultCheck}
	}
}

// Plan turns ad-hoc options into a plan, so both entry points run exactly the
// same engine over exactly the same structure.
func Plan(o Options) (*plan.Plan, error) {
	o.applyDefaults()

	p := &plan.Plan{
		Name: "adhoc-" + o.WorkloadID,
		Workload: plan.WorkloadRef{
			Provider: o.ProviderID,
			ID:       o.WorkloadID,
		},
		Backup: plan.BackupSpec{Provider: o.BackupProvider, Strategy: plan.StrategyLatest},
		Restore: plan.RestoreSpec{
			Node:          o.Node,
			Storage:       o.Storage,
			Pool:          o.Pool,
			Network:       o.Network,
			CPULimit:      o.CPULimit,
			MemoryLimitMB: o.MemoryLimitMB,
		},
		Startup:   plan.StartupSpec{Skip: o.SkipStartup, Timeout: plan.Duration(o.StartupTimeout)},
		RTOTarget: plan.Duration(o.RTOTarget),
	}

	if o.Backup != "" && o.Backup != "latest" {
		p.Backup.Strategy = plan.StrategySpecific
		p.Backup.ID = o.Backup
	}

	if !o.SkipStartup {
		for _, s := range o.Checks {
			c, err := ParseCheckSpec(s)
			if err != nil {
				return nil, err
			}
			// A drill's checks always run against a guest that booted seconds
			// ago, so retrying is the normal case, not the exception. Without
			// this a perfectly good recovery fails because systemd had not
			// finished starting yet.
			c.Retries = o.CheckRetries
			c.RetryInterval = plan.Duration(o.CheckInterval)
			p.Checks = append(p.Checks, c)
		}
	}

	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// ParseCheckSpec parses the shorthand accepted by --check.
func ParseCheckSpec(spec string) (plan.CheckSpec, error) {
	switch {
	case spec == "ping":
		return plan.CheckSpec{Type: "ping", Params: map[string]any{}}, nil

	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		return plan.CheckSpec{Type: "http", Params: map[string]any{"url": spec}}, nil

	case strings.HasPrefix(spec, "tcp:"):
		port, err := strconv.Atoi(strings.TrimPrefix(spec, "tcp:"))
		if err != nil || port < 1 || port > 65535 {
			return plan.CheckSpec{}, fmt.Errorf("invalid check %q: expected tcp:PORT with a port between 1 and 65535", spec)
		}
		return plan.CheckSpec{Type: "tcp", Params: map[string]any{"port": port}}, nil

	case strings.HasPrefix(spec, "cmd:"):
		run := strings.TrimPrefix(spec, "cmd:")
		if strings.TrimSpace(run) == "" {
			return plan.CheckSpec{}, fmt.Errorf("invalid check %q: expected cmd:COMMAND", spec)
		}
		return plan.CheckSpec{Type: "command", Params: map[string]any{"run": run}}, nil

	case strings.HasPrefix(spec, "dns:"):
		name := strings.TrimPrefix(spec, "dns:")
		if name == "" {
			return plan.CheckSpec{}, fmt.Errorf("invalid check %q: expected dns:NAME", spec)
		}
		return plan.CheckSpec{Type: "dns", Params: map[string]any{"name": name}}, nil
	}

	return plan.CheckSpec{}, fmt.Errorf("unknown check %q: expected ping, tcp:PORT, http(s)://..., dns:NAME, or cmd:COMMAND", spec)
}
