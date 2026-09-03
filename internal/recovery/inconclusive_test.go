package recovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func bootedProvider(clock *fakeClock) *fakeProvider {
	return &fakeProvider{
		idStr: "fake-hv",
		latestBackup: &core.Backup{
			ID: "backup-1", WorkloadID: "100", CreatedAt: clock.Now().Add(-time.Hour),
		},
		statuses: []core.WorkloadStatus{
			{PowerState: core.PowerStateRunning, IPs: []string{"10.0.0.5"}},
		},
	}
}

// The defect this whole change exists for.
//
// The backup restored, the workload booted, the guest answered - and then the
// only check could not be run at all, because the machine running RestoreLab
// has no route into the isolated recovery network. That is a fact about the
// operator's topology. Reporting it as FAILED tells them their backup is
// broken when it demonstrably is not, and a tool that cries wolf about
// backups is worse than no tool.
func TestRun_CriticalCheckThatCouldNotRunIsInconclusive(t *testing.T) {
	clock := newFakeClock()
	hv := bootedProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "web-tcp", Type: "tcp", Status: core.CheckError, Message: "no answer on 10.0.0.5:80"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("Run should still report an error: nothing was verified")
	}

	if run.State != core.RunInconclusive {
		t.Errorf("run.State = %s, want %s", run.State, core.RunInconclusive)
	}
	// No verdict at all, rather than a bad one. SUCCESS, DEGRADED and FAILED
	// are all claims about whether the backup restores; this run makes none.
	if run.Result != "" {
		t.Errorf("run.Result = %q, want no verdict", run.Result)
	}
	if !strings.Contains(run.Err, "could not run") {
		t.Errorf("run.Err = %q, want it to say the check could not run", run.Err)
	}
	// The temporary workload is still cleaned up: an inconclusive verdict is
	// no reason to leave a VM on somebody's cluster.
	if !run.CleanupDone {
		t.Error("expected the temporary workload to be cleaned up anyway")
	}
}

// A run that reached no verdict must not be graded as a failure by the parts
// of the product that read runs back - and the confidence score reads
// Result, not State.
func TestRun_InconclusiveCarriesNoResultForScoring(t *testing.T) {
	clock := newFakeClock()
	hv := bootedProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "web-tcp", Type: "tcp", Status: core.CheckError},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planWithChecksYAML)

	run, _ := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if run.Result == core.ResultFailed {
		t.Fatal("an unverifiable run was graded FAILED; this is the bug")
	}
	if run.Result == core.ResultSuccess || run.Result == core.ResultDegraded {
		t.Fatalf("run.Result = %q: an unverifiable run must not claim recovery either", run.Result)
	}
}

// Bad news outranks no news. If one critical check actually ran and failed,
// the drill learned something real about the backup, and that is what the
// report must say - even though another check alongside it could not run.
func TestRun_ARealFailureOutranksAnUnevaluatedCheck(t *testing.T) {
	clock := newFakeClock()
	hv := bootedProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "critical-check", Type: "tcp", Status: core.CheckError, Message: "no answer"},
		{Name: "optional-check", Type: "tcp", Status: core.CheckFail, Message: "500"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	// Both critical here: planTwoChecksYAML marks the http one non-critical,
	// so use a plan where the failing check counts.
	p := mustParsePlan(t, strings.Replace(planTwoChecksYAML, "    critical: false\n", "", 1))

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err == nil {
		t.Fatal("Run should report an error")
	}
	if run.State != core.RunFailed {
		t.Errorf("run.State = %s, want %s: a check that ran and failed is the news", run.State, core.RunFailed)
	}
	if run.Result != core.ResultFailed {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultFailed)
	}
}

// A non-critical check that could not run made no claim about the workload,
// so it must not drag a clean recovery down to DEGRADED.
func TestRun_NonCriticalCheckThatCouldNotRunDoesNotDegrade(t *testing.T) {
	clock := newFakeClock()
	hv := bootedProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "critical-check", Type: "tcp", Status: core.CheckPass},
		{Name: "optional-check", Type: "tcp", Status: core.CheckError, Message: "no answer"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planTwoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Result != core.ResultSuccess {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultSuccess)
	}
}

// A non-critical check that ran and failed still degrades: that one is real
// news, just not fatal news.
func TestRun_NonCriticalCheckThatFailedStillDegrades(t *testing.T) {
	clock := newFakeClock()
	hv := bootedProvider(clock)
	checks := &fakeCheckRunner{results: []core.CheckResult{
		{Name: "critical-check", Type: "tcp", Status: core.CheckPass},
		{Name: "optional-check", Type: "tcp", Status: core.CheckFail, Message: "500"},
	}}
	e := newTestEngine(t, hv, hv, checks, clock)
	p := mustParsePlan(t, planTwoChecksYAML)

	run, err := e.Run(context.Background(), p, RunOptions{Network: isolatedNetwork()})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Result != core.ResultDegraded {
		t.Errorf("run.Result = %s, want %s", run.Result, core.ResultDegraded)
	}
}

// The timeline is where an operator forms their first impression of what a
// drill found, and "failed" is the wrong word for a check that never ran.
func TestCheckMessage_UnevaluatedDoesNotSayFailed(t *testing.T) {
	msg := checkMessage(core.CheckResult{Name: "TCP 22", Status: core.CheckError, Message: "no answer"})
	if strings.Contains(msg, "failed") {
		t.Errorf("checkMessage = %q, want it not to call an unevaluated check a failure", msg)
	}
	if !strings.Contains(msg, "could not run") {
		t.Errorf("checkMessage = %q, want it to say the check could not run", msg)
	}
}

// INCONCLUSIVE is an ending. A poller that does not know it is terminal would
// wait for a drill that is already over.
func TestInconclusiveIsTerminal(t *testing.T) {
	if !core.RunInconclusive.Terminal() {
		t.Error("INCONCLUSIVE must be a terminal state")
	}
}
