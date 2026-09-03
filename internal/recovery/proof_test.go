package recovery

import (
	"context"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

const planWithCommandCheckYAML = `
name: pg-plan
workload:
  provider: fake
  id: "100"
  name: pg-01
backup:
  strategy: latest
restore:
  node: node-a
startup:
  timeout: 10s
checks:
  - type: command
    name: postgres accepting connections
    run: systemctl is-active postgresql
`

// agentProvider is a guest that booted far enough for its agent to answer,
// which is the first thing in the whole workflow that happens inside the
// guest rather than beside it.
func agentProvider(clock *fakeClock) *fakeProvider {
	p := bootedProvider(clock)
	p.statuses = []core.WorkloadStatus{
		{PowerState: core.PowerStateRunning, AgentReady: true, IPs: []string{"10.0.0.5"}},
	}
	return p
}

// A drill that checked a real service, and got an answer, proved the service.
func TestRun_APassingServiceCheckProvesTheService(t *testing.T) {
	clock := newFakeClock()
	hv := agentProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "postgres accepting connections", Type: "command", Status: core.CheckPass},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithCommandCheckYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.ProofLevel != core.ProofService {
		t.Errorf("run.ProofLevel = %s, want SERVICE", run.ProofLevel)
	}
}

// The case that says the two axes are independent.
//
// The service check came back and the answer was bad, so the drill failed -
// and the boot is still verified, because the agent answered from inside the
// guest. "This backup boots but its database is down" is a more useful thing
// to be told than either half alone, and it is what an operator actually
// needs to decide what to do next.
func TestRun_AFailedServiceCheckStillLeavesTheBootProven(t *testing.T) {
	clock := newFakeClock()
	hv := agentProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "postgres accepting connections", Type: "command", Status: core.CheckFail,
			Message: "inactive"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithCommandCheckYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("a failing critical check must still fail the run")
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want FAILED", run.Result)
	}
	if run.ProofLevel != core.ProofBoot {
		t.Errorf("run.ProofLevel = %s, want BOOT: the check failed, the boot did not",
			run.ProofLevel)
	}
}

// A check that could not run establishes nothing, and neither does the
// hypervisor reporting the workload as running.
//
// This is the strict reading of "powering on is not booting": the provider
// says PowerStateRunning, the plan asked for no agent, and the only check
// came back CheckError. A guest sitting at its boot loader looks exactly like
// this from outside, so the honest answer is that nothing was established.
func TestRun_PowerStateAloneProvesNothing(t *testing.T) {
	clock := newFakeClock()
	hv := bootedProvider(clock) // running, but no agent
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "web-tcp", Type: "tcp", Status: core.CheckError, Message: "no route to host"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithChecksYAML)

	run, _ := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if run.State != core.RunInconclusive {
		t.Fatalf("run.State = %s, want INCONCLUSIVE (precondition of this test)", run.State)
	}
	if run.ProofLevel != core.ProofNone {
		t.Errorf("run.ProofLevel = %s, want NONE: the hypervisor reporting a running "+
			"process is not evidence that the OS came up", run.ProofLevel)
	}
}

// A drill that never got as far as the guest proves nothing, and says so
// rather than leaving the field unrecorded - unrecorded means "we never
// wrote this down", which is a different claim and one the score treats
// differently.
func TestRun_ARunThatNeverReachedTheGuestProvesNothing(t *testing.T) {
	clock := newFakeClock()
	hv := agentProvider(clock)
	hv.restoreErr = context.DeadlineExceeded
	checks := &fakeCheckRunner{}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithCommandCheckYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("a restore that fails must fail the run")
	}
	if run.ProofLevel != core.ProofNone {
		t.Errorf("run.ProofLevel = %s, want NONE", run.ProofLevel)
	}
	if !run.ProofLevel.Recorded() {
		t.Error("the level must be recorded as NONE, not left unrecorded")
	}
}

// The invariant of the slice, checked against the engine rather than against
// the pure function: whatever happened, the level a run ends with is never
// higher than what its passing checks and its agent actually established.
func TestRun_TheLevelIsNeverHigherThanWhatWasEstablished(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  core.CheckStatus
		agent   bool
		highest core.ProofLevel
	}{
		{"nothing answered", core.CheckError, false, core.ProofNone},
		{"only the agent answered", core.CheckError, true, core.ProofBoot},
		{"the check answered badly", core.CheckFail, true, core.ProofBoot},
		{"the check answered well", core.CheckPass, true, core.ProofService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			hv := bootedProvider(clock)
			hv.statuses = []core.WorkloadStatus{
				{PowerState: core.PowerStateRunning, AgentReady: tc.agent, IPs: []string{"10.0.0.5"}},
			}
			checks := &fakeCheckRunner{results: []core.CheckResult{
				{Name: "postgres accepting connections", Type: "command", Status: tc.status},
			}}
			e := newTestEngine(t, hv, hv, checks, clock)
			p := mustParsePlan(t, planWithCommandCheckYAML)

			run, _ := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
			if run.ProofLevel != tc.highest {
				t.Errorf("run.ProofLevel = %s, want %s", run.ProofLevel, tc.highest)
			}
		})
	}
}

// A restore-only drill boots nothing and checks nothing, and must not come
// away claiming otherwise.
func TestRun_ARestoreOnlyDrillProvesNothing(t *testing.T) {
	clock := newFakeClock()
	hv := agentProvider(clock)
	checks := &fakeCheckRunner{}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planSkipStartupYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.ProofLevel != core.ProofNone {
		t.Errorf("run.ProofLevel = %s, want NONE: nothing was booted and nothing was checked",
			run.ProofLevel)
	}

}
