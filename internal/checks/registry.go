package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Registry maps check type names ("tcp", "http", ...) to implementations,
// and knows how to run a configured check to completion (retries, per-
// attempt timeout, panic recovery).
type Registry struct {
	checks map[string]core.Check
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{checks: make(map[string]core.Check)}
}

// Register adds c to the registry, keyed by c.Type(). A later registration
// for the same type replaces the earlier one.
func (r *Registry) Register(c core.Check) {
	r.checks[c.Type()] = c
}

// Get looks up a check by type name.
func (r *Registry) Get(typ string) (core.Check, bool) {
	c, ok := r.checks[typ]
	return c, ok
}

// Types returns every registered check type, sorted.
func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.checks))
	for t := range r.checks {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// Default returns a Registry with every built-in check type registered.
func Default() *Registry {
	r := New()
	r.Register(PingCheck{})
	r.Register(TCPCheck{})
	r.Register(newHTTPCheck("http"))
	r.Register(newHTTPCheck("https"))
	r.Register(DNSCheck{})
	r.Register(CommandCheck{})
	return r
}

func displayName(cfg core.CheckConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return cfg.Type
}

// RunOne resolves cfg.Type in the registry and runs it to completion. Each
// attempt gets its own context bounded by cfg.Timeout (when set); the
// registry retries up to cfg.Retries extra times, spaced by
// cfg.RetryInterval, while the result is not passing, and respects ctx
// cancellation between attempts. Total elapsed time is measured across
// every attempt. The returned result always has Name, Type, StartedAt,
// CompletedAt, Duration and Attempts filled in.
func (r *Registry) RunOne(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	start := time.Now()
	name := displayName(cfg)

	check, ok := r.Get(cfg.Type)
	if !ok {
		now := time.Now()
		return core.CheckResult{
			Name:        name,
			Type:        cfg.Type,
			Status:      core.CheckError,
			StartedAt:   start,
			CompletedAt: now,
			Duration:    now.Sub(start),
			Attempts:    0,
			Message:     fmt.Sprintf("unknown check type %q (known types: %s)", cfg.Type, strings.Join(r.Types(), ", ")),
		}
	}

	maxAttempts := cfg.Retries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var result core.CheckResult
	attempts := 0

attemptLoop:
	for attempts < maxAttempts {
		attempts++

		if err := ctx.Err(); err != nil {
			result = core.CheckResult{
				Status:  core.CheckError,
				Message: fmt.Sprintf("run cancelled before attempt %d: %v", attempts, err),
			}
			break attemptLoop
		}

		result = runAttempt(ctx, check, target, cfg)
		if result.OK() || attempts >= maxAttempts {
			break attemptLoop
		}

		if cfg.RetryInterval > 0 {
			timer := time.NewTimer(cfg.RetryInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				result = core.CheckResult{
					Status:  core.CheckError,
					Message: fmt.Sprintf("run cancelled while waiting to retry: %v", ctx.Err()),
				}
				break attemptLoop
			case <-timer.C:
			}
		}
	}

	now := time.Now()
	result.Name = name
	result.Type = cfg.Type
	result.StartedAt = start
	result.CompletedAt = now
	result.Duration = now.Sub(start)
	result.Attempts = attempts
	return result
}

// runAttempt runs a single attempt of check, applying cfg.Timeout as a
// per-attempt deadline and recovering from any panic so a broken check
// implementation can never take down the whole run.
func runAttempt(ctx context.Context, check core.Check, target core.Target, cfg core.CheckConfig) (result core.CheckResult) {
	defer func() {
		if p := recover(); p != nil {
			result = core.CheckResult{
				Status:  core.CheckError,
				Message: fmt.Sprintf("check %q panicked: %v", check.Type(), p),
			}
		}
	}()

	attemptCtx := ctx
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	return check.Run(attemptCtx, target, cfg)
}

// RunAll runs every check in cfgs, in order, against target. If ctx is
// cancelled before a check starts, that check (and every one after it) is
// reported as CheckSkipped rather than dropped, so the returned slice
// always accounts for every configured check.
func (r *Registry) RunAll(ctx context.Context, target core.Target, cfgs []core.CheckConfig) []core.CheckResult {
	results := make([]core.CheckResult, len(cfgs))
	for i, cfg := range cfgs {
		if err := ctx.Err(); err != nil {
			now := time.Now()
			results[i] = core.CheckResult{
				Name:        displayName(cfg),
				Type:        cfg.Type,
				Status:      core.CheckSkipped,
				StartedAt:   now,
				CompletedAt: now,
				Message:     fmt.Sprintf("run cancelled: %v", err),
			}
			continue
		}
		results[i] = r.RunOne(ctx, target, cfg)
	}
	return results
}
