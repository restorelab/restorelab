package recovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// execProvider is a fakeProvider that can also run commands in the guest,
// like the Proxmox provider does through the QEMU guest agent.
type execProvider struct {
	*fakeProvider
	result *core.ExecResult
	err    error
	reqs   []core.ExecRequest
}

func (e *execProvider) ExecInGuest(_ context.Context, _ string, req core.ExecRequest) (*core.ExecResult, error) {
	e.reqs = append(e.reqs, req)
	if e.err != nil {
		return nil, e.err
	}
	return e.result, nil
}

func boolPtr(b bool) *bool { return &b }

func TestGuestReadyWaitsForTheAgent(t *testing.T) {
	tests := []struct {
		name   string
		status core.WorkloadStatus
		wait   plan.StartupSpec
		want   bool
	}{
		{
			name:   "stopped guest is never ready",
			status: core.WorkloadStatus{PowerState: core.PowerStateStopped, AgentReady: true},
			wait:   plan.StartupSpec{WaitForAgent: boolPtr(true)},
		},
		{
			name:   "running but the agent has not come up yet",
			status: core.WorkloadStatus{PowerState: core.PowerStateRunning},
			wait:   plan.StartupSpec{WaitForAgent: boolPtr(true)},
		},
		{
			name:   "agent responding is enough, no address needed",
			status: core.WorkloadStatus{PowerState: core.PowerStateRunning, AgentReady: true},
			wait:   plan.StartupSpec{WaitForAgent: boolPtr(true), WaitForIP: boolPtr(false)},
			want:   true,
		},
		{
			name:   "an address is still required when a network check needs one",
			status: core.WorkloadStatus{PowerState: core.PowerStateRunning, AgentReady: true},
			wait:   plan.StartupSpec{WaitForAgent: boolPtr(true), WaitForIP: boolPtr(true)},
		},
		{
			name:   "both satisfied",
			status: core.WorkloadStatus{PowerState: core.PowerStateRunning, AgentReady: true, IPs: []string{"10.99.0.14"}},
			wait:   plan.StartupSpec{WaitForAgent: boolPtr(true), WaitForIP: boolPtr(true)},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &plan.Plan{Startup: tt.wait}
			if got := guestReady(&tt.status, p); got != tt.want {
				t.Errorf("guestReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A drill made only of in-guest checks must complete without the guest ever
// getting an address: that is what removes the need to route into the
// isolated recovery network.
func TestRunWithInGuestChecksNeedsNoAddress(t *testing.T) {
	base := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "101", CreatedAt: time.Now().Add(-time.Hour)},
	}
	base.statuses = []core.WorkloadStatus{
		{PowerState: core.PowerStateRunning},                   // agent not up yet
		{PowerState: core.PowerStateRunning, AgentReady: true}, // no IP, ever
	}
	hv := &execProvider{fakeProvider: base}

	var gotTarget core.Target
	checks := &fakeCheckRunner{
		fn: func(_ context.Context, target core.Target, _ []core.CheckConfig) []core.CheckResult {
			gotTarget = target
			return []core.CheckResult{{Name: "service", Type: "command", Status: core.CheckPass}}
		},
	}

	engine := newTestEngine(t, hv, hv, checks, newFakeClock())

	p := &plan.Plan{
		Name:     "in-guest",
		Workload: plan.WorkloadRef{Provider: "fake", ID: "101"},
		Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest},
		Startup:  plan.StartupSpec{Timeout: plan.Duration(time.Minute)},
		Checks:   []plan.CheckSpec{{Type: "command", Params: map[string]any{"run": "systemctl is-active postgresql"}}},
	}
	p.ApplyDefaults()

	if p.Startup.WaitsForIP() {
		t.Fatal("a plan with only in-guest checks must not wait for an address")
	}

	run, err := engine.Run(context.Background(), p, RunOptions{
		Network: isolatedNetwork(),
		Node:    "pve1",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.Result != core.ResultSuccess {
		t.Errorf("Result = %s, want SUCCESS (err=%s)", run.Result, run.Err)
	}
	if gotTarget.IP != "" {
		t.Errorf("target IP = %q, want empty: the guest never had an address", gotTarget.IP)
	}
	if gotTarget.Exec == nil {
		t.Fatal("the checks must be handed an executor when the provider supports one")
	}
	if gotTarget.WorkloadID != run.TempWorkloadID {
		t.Errorf("target workload = %q, want the temporary id %q", gotTarget.WorkloadID, run.TempWorkloadID)
	}
}

// A provider that cannot run commands in the guest must hand the checks a nil
// executor rather than something that pretends to work.
func TestRunWithoutGuestExecutorLeavesTargetExecNil(t *testing.T) {
	hv := &fakeProvider{
		idStr:        "fake-hv",
		latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "101", CreatedAt: time.Now().Add(-time.Hour)},
	}
	hv.statuses = []core.WorkloadStatus{{PowerState: core.PowerStateRunning, IPs: []string{"10.99.0.14"}}}

	var gotTarget core.Target
	checks := &fakeCheckRunner{
		fn: func(_ context.Context, target core.Target, _ []core.CheckConfig) []core.CheckResult {
			gotTarget = target
			return []core.CheckResult{{Name: "tcp", Status: core.CheckPass}}
		},
	}

	engine := newTestEngine(t, hv, hv, checks, newFakeClock())

	p := &plan.Plan{
		Name:     "network-only",
		Workload: plan.WorkloadRef{Provider: "fake", ID: "101"},
		Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest},
		Startup:  plan.StartupSpec{Timeout: plan.Duration(time.Minute)},
		Checks:   []plan.CheckSpec{{Type: "tcp", Params: map[string]any{"port": 22}}},
	}
	p.ApplyDefaults()

	if _, err := engine.Run(context.Background(), p, RunOptions{
		Network: isolatedNetwork(),
		Node:    "pve1",
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if gotTarget.Exec != nil {
		t.Error("Target.Exec must stay nil when the provider cannot run guest commands")
	}
	if gotTarget.IP != "10.99.0.14" {
		t.Errorf("target IP = %q, want the discovered address", gotTarget.IP)
	}
}

// unverifiableProvider cannot read the node's network configuration, which is
// what a Proxmox token that Sys.Audit does not get bridge visibility for looks
// like.
type unverifiableProvider struct {
	*fakeProvider
	err error
}

func (u *unverifiableProvider) ValidateIsolation(_ context.Context, _ string, _ core.NetworkConfig) error {
	return u.err
}

// "I could not verify" must not be treated as "it is unsafe": that would block
// every drill on a cluster whose token cannot list bridges. Proven danger
// still stops the run.
func TestIsolationCheckDistinguishesUnverifiedFromUnsafe(t *testing.T) {
	tests := []struct {
		name       string
		validation error
		wantRun    bool
	}{
		{
			name:       "isolation verified",
			validation: nil,
			wantRun:    true,
		},
		{
			name:       "isolation unverifiable, the plan's assertion stands",
			validation: fmt.Errorf("cannot read bridges: %w", core.ErrIsolationUnverified),
			wantRun:    true,
		},
		{
			name:       "isolation disproven, hard stop",
			validation: fmt.Errorf("bridge has an uplink: %w", core.ErrNetworkNotIsolated),
			wantRun:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &fakeProvider{
				idStr:        "fake-hv",
				latestBackup: &core.Backup{ID: "backup-1", WorkloadID: "101", CreatedAt: time.Now().Add(-time.Hour)},
				statuses:     []core.WorkloadStatus{{PowerState: core.PowerStateRunning, IPs: []string{"10.99.0.14"}}},
			}
			hv := &unverifiableProvider{fakeProvider: base, err: tt.validation}

			checks := &fakeCheckRunner{results: []core.CheckResult{{Name: "tcp", Status: core.CheckPass}}}
			engine := newTestEngine(t, hv, hv, checks, newFakeClock())

			p := &plan.Plan{
				Name:     "isolation",
				Workload: plan.WorkloadRef{Provider: "fake", ID: "101"},
				Backup:   plan.BackupSpec{Strategy: plan.StrategyLatest},
				Startup:  plan.StartupSpec{Timeout: plan.Duration(time.Minute)},
				Checks:   []plan.CheckSpec{{Type: "tcp", Params: map[string]any{"port": 22}}},
			}
			p.ApplyDefaults()

			run, err := engine.Run(context.Background(), p, RunOptions{
				Network: isolatedNetwork(),
				Node:    "pve1",
			})

			if tt.wantRun {
				if err != nil {
					t.Fatalf("Run() error = %v, want the drill to proceed", err)
				}
				if run.TempWorkloadID == "" {
					t.Error("no workload was ever created")
				}
				return
			}

			if err == nil {
				t.Fatal("Run() error = nil, want a refusal")
			}
			if !errors.Is(err, core.ErrNetworkNotIsolated) {
				t.Errorf("error = %v, want core.ErrNetworkNotIsolated", err)
			}
			for _, call := range base.Calls() {
				if strings.HasPrefix(call, "Restore(") {
					t.Errorf("nothing may be restored onto a network known not to be isolated, got %q", call)
				}
			}
		})
	}
}
