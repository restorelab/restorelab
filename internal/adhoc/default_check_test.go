package adhoc

import (
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

func adhocPlan(t *testing.T, o Options) *plan.Plan {
	t.Helper()
	o.WorkloadID = "100"
	o.ProviderID = "fake"
	o.Node = "node-a"
	p, err := Plan(o)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

// The rule this pins is not "the default is cmd:hostname" - it is that the
// default check works with nothing more than what RestoreLab already needs to
// restore and boot a workload.
//
// A drill restores onto a deliberately isolated bridge. A default that dials
// the guest over the network only works for operators who have arranged a
// route into that network, and reports a perfectly good backup as broken for
// everybody else. Whatever the default becomes, it must not go back to
// depending on the machine it happens to run from.
func TestDefaultCheckNeedsNoRouteToTheGuest(t *testing.T) {
	p := adhocPlan(t, Options{})

	if len(p.Checks) == 0 {
		t.Fatal("a drill with no --check should still check something")
	}
	for _, c := range p.Checks {
		if c.Type != "command" {
			t.Errorf("default check type = %q: it reaches the guest over the network, "+
				"which the isolated recovery bridge exists to prevent", c.Type)
		}
	}

	// And the consequence, which is the half that is easy to forget: with no
	// network check to serve, the drill must stop waiting for a DHCP lease on
	// a bridge that has no DHCP server on it.
	if p.Startup.WaitsForIP() {
		t.Error("the default drill still waits for an IP on the isolated network")
	}
	if !p.Startup.WaitsForAgent() {
		t.Error("an in-guest check needs the guest agent, so the drill must wait for it")
	}
}

// An operator who asks for a network check gets one - this is not a ban, it
// is a default. Somebody running RestoreLab on the node, or routing their
// isolated VLAN, is testing something real that an in-guest check cannot see.
func TestExplicitNetworkChecksStillWork(t *testing.T) {
	p := adhocPlan(t, Options{Checks: []string{"tcp:22"}})

	if len(p.Checks) != 1 || p.Checks[0].Type != "tcp" {
		t.Fatalf("checks = %+v, want the tcp check that was asked for", p.Checks)
	}
	if !p.Startup.WaitsForIP() {
		t.Error("a tcp check needs an address, so the drill must wait for one")
	}
}

// How long an operator waits before being told anything.
//
// The old defaults - 10 retries, no per-attempt bound - meant a check that
// hung took eleven OS-length timeouts back to back: measured at 4m51s on a
// real drill, against a five-minute RTO target it then also blew. The exact
// numbers are a judgement call; the ceiling is not.
func TestRetryBudgetStaysUnderTwoMinutes(t *testing.T) {
	p := adhocPlan(t, Options{})

	c := p.Checks[0]
	attempts := c.Retries + 1
	worst := time.Duration(attempts)*c.Timeout.D() + time.Duration(c.Retries)*c.RetryInterval.D()

	if worst > 2*time.Minute {
		t.Errorf("a check that never answers costs %s before the drill says so "+
			"(%d attempts x %s, plus %d x %s waiting): too long to leave somebody watching a spinner",
			worst, attempts, c.Timeout.D(), c.Retries, c.RetryInterval.D())
	}
	// The other side of it: retrying at all is the point. A guest that booted
	// seconds ago needs a moment for its services, and zero grace would fail
	// good recoveries.
	if c.Retries < 3 {
		t.Errorf("Retries = %d: too few to let a just-booted guest finish starting", c.Retries)
	}
}

// A caller who sets their own values keeps them. The defaults fill silence;
// they do not overrule.
func TestExplicitRetryValuesAreKept(t *testing.T) {
	p := adhocPlan(t, Options{CheckRetries: 20, CheckInterval: 30 * time.Second})

	c := p.Checks[0]
	if c.Retries != 20 {
		t.Errorf("Retries = %d, want the 20 that were asked for", c.Retries)
	}
	if c.RetryInterval.D() != 30*time.Second {
		t.Errorf("RetryInterval = %s, want the 30s that were asked for", c.RetryInterval.D())
	}
}

// The default check must never claim more than it establishes.
//
// This is not a test of the deduction - plan has those - it is the tripwire
// on DefaultCheck itself. The default is what every first drill on a fresh
// install runs, so it is what decides the number a new user sees on their
// dashboard the first time. If somebody changes it to something that reads
// as a real service check, this fails and forces the question: does the new
// default actually prove more, or would we just be saying so?
func TestTheDefaultCheckClaimsTheBootAndNothingMore(t *testing.T) {
	p := adhocPlan(t, Options{})

	if got := p.ProvenLevel(); got != core.ProofBoot {
		t.Errorf("the default drill claims to prove %s, want BOOT: %q proves that the guest "+
			"runs and can fork a process, and a default that claimed more would put a number "+
			"on the dashboard nothing had earned", got, DefaultCheck)
	}
}

// An operator who writes a real check gets credit for it, without having to
// declare anything.
func TestAServiceCheckRaisesWhatTheDrillProves(t *testing.T) {
	p := adhocPlan(t, Options{Checks: []string{"cmd:systemctl is-active postgresql"}})

	if got := p.ProvenLevel(); got != core.ProofService {
		t.Errorf("a drill checking a real service claims to prove %s, want SERVICE", got)
	}
}
