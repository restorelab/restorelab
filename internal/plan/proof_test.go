package plan

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/core"
)

func TestCheckProvenLevelIsDeduced(t *testing.T) {
	tests := []struct {
		name string
		spec CheckSpec
		want core.ProofLevel
	}{
		{
			name: "the default drill's command proves the boot",
			spec: CheckSpec{Type: "command", Params: map[string]any{"run": "hostname"}},
			want: core.ProofBoot,
		},
		{
			// Arguments do not make a liveness probe say more: `uname -a`
			// prints more text about the same fact.
			name: "arguments do not promote a liveness command",
			spec: CheckSpec{Type: "command", Params: map[string]any{"run": "uname -a"}},
			want: core.ProofBoot,
		},
		{
			name: "an absolute path is still recognised",
			spec: CheckSpec{Type: "command", Params: map[string]any{"run": "/usr/bin/hostname"}},
			want: core.ProofBoot,
		},
		{
			name: "so is the Windows spelling",
			spec: CheckSpec{Type: "command", Params: map[string]any{"run": `C:\Windows\System32\HOSTNAME.EXE`}},
			want: core.ProofBoot,
		},
		{
			name: "the argv form is read the same way",
			spec: CheckSpec{Type: "command", Params: map[string]any{"argv": []any{"hostname"}}},
			want: core.ProofBoot,
		},
		{
			// This is the case the whole deduction exists for.
			name: "a real service check proves the service",
			spec: CheckSpec{Type: "command", Params: map[string]any{"run": "systemctl is-active postgresql"}},
			want: core.ProofService,
		},
		{
			// A command that chains is no longer described by its first
			// program. Demoting it would understate - safe, but wrong.
			name: "a chained command is not a liveness probe",
			spec: CheckSpec{Type: "command", Params: map[string]any{"run": "hostname && systemctl is-active postgresql"}},
			want: core.ProofService,
		},
		{
			// A network probe that answers proves the service listens. That
			// is service-level evidence, and it does not outrank a query
			// against the data just because it needed a route to get there.
			name: "a network probe enters at the service level",
			spec: CheckSpec{Type: "tcp", Params: map[string]any{"port": 5432}},
			want: core.ProofService,
		},
		{
			// ICMP is answered by the kernel. A guest that pings has booted
			// and configured its network; every service on it may still be
			// dead, so this is the network-side twin of `cmd:hostname`.
			name: "a ping proves the guest is up, not that anything serves",
			spec: CheckSpec{Type: "ping"},
			want: core.ProofBoot,
		},
		{
			name: "a declaration wins over the deduction",
			spec: CheckSpec{Type: "command", Proves: "data", Params: map[string]any{"run": "/opt/app/healthcheck.sh"}},
			want: core.ProofData,
		},
		{
			// Both directions. Somebody who knows their check is a smoke
			// test is believed when they say so.
			name: "a declaration can lower a check too",
			spec: CheckSpec{Type: "command", Proves: "none", Params: map[string]any{"run": "systemctl is-active postgresql"}},
			want: core.ProofNone,
		},
		{
			name: "case does not matter in a declaration",
			spec: CheckSpec{Type: "tcp", Proves: "Data", Params: map[string]any{"port": 443}},
			want: core.ProofData,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.ProvenLevel(); got != tc.want {
				t.Errorf("ProvenLevel() = %s, want %s", got, tc.want)
			}
		})
	}
}

// The rule the deduction is built on: it may understate what a check proves,
// never overstate it. Any check RestoreLab cannot read is worth at most
// SERVICE, and the only way past that is somebody saying so in the plan.
func TestTheDeductionNeverClaimsDataOnItsOwn(t *testing.T) {
	for _, run := range []string{
		"psql -tAc 'select count(*) from orders'",
		"/opt/app/healthcheck.sh --deep",
		"mysql -e 'select 1'",
	} {
		spec := CheckSpec{Type: "command", Params: map[string]any{"run": run}}
		if got := spec.ProvenLevel(); got.AtLeast(core.ProofData) {
			t.Errorf("%q deduced as %s: only a declaration may claim DATA", run, got)
		}
	}
}

// Every check built from a plan carries a level. core.ProvenBy ignores a
// check whose level is unrecorded, so a gap here would silently stop a
// perfectly good check from counting for anything.
func TestToCoreAlwaysCarriesALevel(t *testing.T) {
	for _, spec := range []CheckSpec{
		{Type: "command", Params: map[string]any{"run": "hostname"}},
		{Type: "tcp", Params: map[string]any{"port": 22}},
		{Type: "http", Params: map[string]any{"url": "http://example.test/"}},
		{Type: "ping"},
	} {
		if got := spec.ToCore().Proves; !got.Recorded() {
			t.Errorf("check %q reaches the engine with no proof level", spec.Type)
		}
	}
}

// `proves` has to survive the trip through the store, for the same reason
// every other check field does: the API stores the plan a drill was queued
// against, and the worker parses that snapshot to execute it. A declaration
// that evaporated in transit would silently downgrade every drill triggered
// over HTTP - and downgrade it quietly, which is the worst way to be wrong
// about a field whose whole job is to be honest.
func TestProvesSurvivesTheYAMLRoundTrip(t *testing.T) {
	const src = `
name: pg
workload:
  provider: pve
  id: "110"
checks:
  - type: command
    name: orders present
    run: /opt/app/healthcheck.sh
    proves: data
  - type: command
    name: alive
    run: hostname
`
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Checks[0].Proves != "data" {
		t.Fatalf("proves = %q, want data", p.Checks[0].Proves)
	}

	out, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "proves: data") {
		t.Fatalf("proves did not survive marshalling:\n%s", out)
	}

	back, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got := back.Checks[0].ProvenLevel(); got != core.ProofData {
		t.Errorf("after the round trip the check proves %s, want DATA", got)
	}
	// And the check that declared nothing must not have acquired anything.
	if back.Checks[1].Proves != "" {
		t.Errorf("an undeclared check came back declaring %q", back.Checks[1].Proves)
	}
}

// A typo in `proves` is refused when the plan is written, not silently
// ignored at three in the morning. Same argument as the schedule field.
func TestAnUnknownProvesValueIsRefusedAtWriteTime(t *testing.T) {
	const src = `
name: pg
workload:
  provider: pve
  id: "110"
checks:
  - type: command
    run: hostname
    proves: everything
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("a plan claiming proves: everything was accepted")
	}
	if !strings.Contains(err.Error(), "proves") {
		t.Errorf("error does not mention the offending field: %v", err)
	}
}

func TestPlanProvenLevel(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want core.ProofLevel
	}{
		{
			name: "a plan with no checks would prove nothing",
			plan: Plan{},
			want: core.ProofNone,
		},
		{
			name: "a restore-only drill would prove nothing",
			plan: Plan{
				Startup: StartupSpec{Skip: true},
				Checks:  []CheckSpec{{Type: "command", Params: map[string]any{"run": "hostname"}}},
			},
			want: core.ProofNone,
		},
		{
			name: "a plan is worth its most telling check",
			plan: Plan{Checks: []CheckSpec{
				{Type: "command", Params: map[string]any{"run": "hostname"}},
				{Type: "command", Params: map[string]any{"run": "systemctl is-active postgresql"}},
			}},
			want: core.ProofService,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.ProvenLevel(); got != tc.want {
				t.Errorf("ProvenLevel() = %s, want %s", got, tc.want)
			}
		})
	}
}
