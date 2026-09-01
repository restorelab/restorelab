package plan

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const fullPlan = `
name: postgres-prod
description: Nightly recovery drill for the main database
tags: [production, database]

workload:
  provider: proxmox-main
  id: "101"
  name: postgres-prod

backup:
  provider: pbs-main
  strategy: latest
  max_age: 26h

restore:
  node: pve02
  storage: local-lvm
  network: isolated
  cpu_limit: 2
  memory_limit: 4096
  bandwidth_limit: 100000
  timeout: 45m

startup:
  timeout: 180s

checks:
  - type: tcp
    port: 22
    timeout: 60s
  - type: postgres
    name: primary database
    port: 5432
    database: production
    query: SELECT 1
    retries: 3
    retry_interval: 10s
    critical: false

cleanup:
  always: true

rto_target: 5m
schedule: "0 3 * * 0"
`

func TestParseFullPlan(t *testing.T) {
	p, err := Parse([]byte(fullPlan))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if p.Name != "postgres-prod" {
		t.Errorf("Name = %q, want postgres-prod", p.Name)
	}
	if p.Workload.Provider != "proxmox-main" || p.Workload.ID != "101" {
		t.Errorf("Workload = %+v", p.Workload)
	}
	if p.Backup.Strategy != StrategyLatest {
		t.Errorf("Backup.Strategy = %q", p.Backup.Strategy)
	}
	if got, want := p.Backup.MaxAge.D(), 26*time.Hour; got != want {
		t.Errorf("Backup.MaxAge = %v, want %v", got, want)
	}
	if got, want := p.Restore.Timeout.D(), 45*time.Minute; got != want {
		t.Errorf("Restore.Timeout = %v, want %v", got, want)
	}
	if got, want := p.Startup.Timeout.D(), 3*time.Minute; got != want {
		t.Errorf("Startup.Timeout = %v, want %v", got, want)
	}
	if got, want := p.RTOTarget.D(), 5*time.Minute; got != want {
		t.Errorf("RTOTarget = %v, want %v", got, want)
	}
	if len(p.Checks) != 2 {
		t.Fatalf("len(Checks) = %d, want 2", len(p.Checks))
	}
}

// A plan must survive being written and read back, because that is exactly
// what happens to it: the API stores the plan a drill was queued against, and
// the worker parses that snapshot to execute it. A check whose params did not
// survive the trip would reach the engine empty - a tcp check with no port, a
// command check with nothing to run - and fail every drill triggered over
// HTTP. That defect existed and was caught end to end; this is its home.
func TestPlanSurvivesAYAMLRoundTrip(t *testing.T) {
	original, err := Parse([]byte(fullPlan))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	encoded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	reparsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse() of a marshalled plan failed: %v\n%s", err, encoded)
	}

	if !reflect.DeepEqual(original, reparsed) {
		t.Errorf("the plan changed on the way through YAML:\noriginal = %+v\nreparsed = %+v\nencoded:\n%s",
			original, reparsed, encoded)
	}

	// Spelled out for the two that matter, so a failure says which check lost
	// what rather than dumping two structs.
	if got := reparsed.Checks[0].Params["port"]; got != 22 {
		t.Errorf("the tcp check lost its port: Params = %+v", reparsed.Checks[0].Params)
	}
	if _, ok := reparsed.Checks[0].Params["params"]; ok {
		t.Error("params were nested under a params key instead of being inlined")
	}
	if _, ok := reparsed.Checks[0].Params["retryinterval"]; ok {
		t.Error("an untagged retryinterval leaked into the params")
	}
}

func TestCheckSpecSplitsReservedKeysFromParams(t *testing.T) {
	p, err := Parse([]byte(fullPlan))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	tcp := p.Checks[0]
	if tcp.Type != "tcp" {
		t.Errorf("Type = %q", tcp.Type)
	}
	if got, want := tcp.Timeout.D(), time.Minute; got != want {
		t.Errorf("Timeout = %v, want %v", got, want)
	}
	if got := tcp.Params["port"]; got != 22 {
		t.Errorf("Params[port] = %v (%T), want 22", got, got)
	}
	for _, reserved := range []string{"type", "name", "timeout", "retries", "retry_interval", "critical"} {
		if _, ok := tcp.Params[reserved]; ok {
			t.Errorf("Params still carries reserved key %q", reserved)
		}
	}

	pg := p.Checks[1]
	if pg.Params["database"] != "production" || pg.Params["query"] != "SELECT 1" {
		t.Errorf("postgres params = %+v", pg.Params)
	}
	if pg.Retries != 3 {
		t.Errorf("Retries = %d, want 3", pg.Retries)
	}
	if pg.IsCritical() {
		t.Error("IsCritical() = true, want false (critical: false was set)")
	}
	if !tcp.IsCritical() {
		t.Error("checks must be critical by default")
	}
}

func TestDisplayNameAndToCore(t *testing.T) {
	p, err := Parse([]byte(fullPlan))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if got, want := p.Checks[0].DisplayName(), "TCP 22"; got != want {
		t.Errorf("DisplayName() = %q, want %q", got, want)
	}
	if got, want := p.Checks[1].DisplayName(), "primary database"; got != want {
		t.Errorf("DisplayName() = %q, want %q", got, want)
	}

	cfg := p.Checks[1].ToCore()
	if cfg.Type != "postgres" || cfg.Name != "primary database" {
		t.Errorf("ToCore() = %+v", cfg)
	}
	if cfg.Timeout != DefaultCheckTimeout {
		t.Errorf("Timeout = %v, want default %v", cfg.Timeout, DefaultCheckTimeout)
	}
	if cfg.RetryInterval != 10*time.Second {
		t.Errorf("RetryInterval = %v, want 10s", cfg.RetryInterval)
	}
	if cfg.Critical {
		t.Error("Critical = true, want false")
	}
	if cfg.Params["query"] != "SELECT 1" {
		t.Errorf("Params = %+v", cfg.Params)
	}
}

func TestApplyDefaults(t *testing.T) {
	p, err := Parse([]byte(`
name: minimal
workload:
  provider: proxmox-main
  id: "101"
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if p.Backup.Strategy != StrategyLatest {
		t.Errorf("Strategy = %q, want latest", p.Backup.Strategy)
	}
	if p.Restore.Network != "isolated" {
		t.Errorf("Network = %q, want isolated (isolation is the default, always)", p.Restore.Network)
	}
	if !p.Cleanup.CleanupAlways() {
		t.Error("cleanup must default to always")
	}
	if !p.Startup.WaitsForIP() {
		t.Error("WaitsForIP() must default to true when no IP is pinned and there are no checks")
	}
	if p.Startup.Timeout.D() != DefaultStartupTimeout {
		t.Errorf("Startup.Timeout = %v", p.Startup.Timeout)
	}
	if p.Restore.Timeout.D() != DefaultRestoreTimeout {
		t.Errorf("Restore.Timeout = %v", p.Restore.Timeout)
	}
}

func TestCleanupAlwaysFalseIsRespected(t *testing.T) {
	p, err := Parse([]byte(`
name: keepit
workload: {provider: p, id: "1"}
cleanup:
  always: false
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if p.Cleanup.CleanupAlways() {
		t.Error("an explicit cleanup.always=false must be honoured")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`
name: typo
workload: {provider: p, id: "1"}
restoer:
  node: pve01
`))
	if err == nil {
		t.Fatal("Parse() error = nil, want a strict-decoding error on the misspelled key")
	}
	if !strings.Contains(err.Error(), "restoer") {
		t.Errorf("error should name the offending field, got %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "missing required fields",
			yaml: "description: nothing useful\n",
			want: []string{"name is required", "workload.id is required", "workload.provider is required"},
		},
		{
			name: "specific strategy without id",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nbackup: {strategy: specific}\n",
			want: []string{"backup.id is required"},
		},
		{
			name: "unknown strategy",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nbackup: {strategy: oldest}\n",
			want: []string{`backup.strategy "oldest" is not supported`},
		},
		{
			name: "check without type",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n  - port: 22\n",
			want: []string{"checks[0].type is required"},
		},
		{
			name: "duplicate check names",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n  - {type: tcp, port: 22}\n  - {type: tcp, port: 22}\n",
			want: []string{"duplicate check name"},
		},
		{
			name: "checks with startup skipped",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nstartup: {skip: true}\nchecks:\n  - {type: tcp, port: 22}\n",
			want: []string{"checks cannot run when startup.skip is set"},
		},
		{
			name: "negative limits",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nrestore: {cpu_limit: -1, memory_limit: -2}\n",
			want: []string{"restore.cpu_limit must be positive", "restore.memory_limit must be positive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse() error = nil, want validation failure")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: `timeout: 90s`, want: 90 * time.Second},
		{in: `timeout: 2m30s`, want: 150 * time.Second},
		{in: `timeout: 60`, want: 60 * time.Second},
		{in: `timeout: 1h`, want: time.Hour},
		{in: `timeout:`, want: 0},
		{in: `timeout: soon`, wantErr: true},
		{in: `timeout: [1]`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var out struct {
				Timeout Duration `yaml:"timeout"`
			}
			err := yamlUnmarshal([]byte(tt.in), &out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if out.Timeout.D() != tt.want {
				t.Errorf("got %v, want %v", out.Timeout.D(), tt.want)
			}
		})
	}
}

func TestDurationOr(t *testing.T) {
	if got := Duration(0).Or(time.Minute); got != time.Minute {
		t.Errorf("Or() on zero = %v, want fallback", got)
	}
	if got := Duration(3 * time.Second).Or(time.Minute); got != 3*time.Second {
		t.Errorf("Or() on set value = %v, want 3s", got)
	}
}

// yamlUnmarshal keeps the duration tests readable without repeating the
// decoder plumbing in every case.
func yamlUnmarshal(data []byte, out any) error { return yaml.Unmarshal(data, out) }

func TestStartupWaitDefaultsFollowTheChecks(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantIP    bool
		wantAgent bool
	}{
		{
			name:   "no checks still proves the guest configured its network",
			yaml:   "name: x\nworkload: {provider: p, id: \"1\"}\n",
			wantIP: true,
		},
		{
			name:   "network checks need an address",
			yaml:   "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n  - {type: tcp, port: 22}\n",
			wantIP: true,
		},
		{
			// The whole point of in-guest checks: no address, no DHCP, no
			// route into the isolated network.
			name:      "in-guest checks only wait on the agent",
			yaml:      "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n  - {type: command, run: \"true\"}\n",
			wantIP:    false,
			wantAgent: true,
		},
		{
			name:      "mixed checks need both",
			yaml:      "name: x\nworkload: {provider: p, id: \"1\"}\nchecks:\n  - {type: tcp, port: 22}\n  - {type: command, run: \"true\"}\n",
			wantIP:    true,
			wantAgent: true,
		},
		{
			name:   "a pinned address needs no discovery",
			yaml:   "name: x\nworkload: {provider: p, id: \"1\"}\nstartup: {ip: 10.99.0.14}\nchecks:\n  - {type: tcp, port: 22}\n",
			wantIP: false,
		},
		{
			name: "restore-only waits for nothing",
			yaml: "name: x\nworkload: {provider: p, id: \"1\"}\nstartup: {skip: true}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got := p.Startup.WaitsForIP(); got != tt.wantIP {
				t.Errorf("WaitsForIP() = %v, want %v", got, tt.wantIP)
			}
			if got := p.Startup.WaitsForAgent(); got != tt.wantAgent {
				t.Errorf("WaitsForAgent() = %v, want %v", got, tt.wantAgent)
			}
		})
	}
}

// An explicit value in a plan must survive the defaults: silently overriding
// what someone wrote down is how a drill fails for reasons nobody can see.
func TestExplicitStartupWaitsAreHonoured(t *testing.T) {
	p, err := Parse([]byte("name: x\nworkload: {provider: p, id: \"1\"}\nstartup: {wait_for_ip: false}\nchecks:\n  - {type: tcp, port: 22}\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if p.Startup.WaitsForIP() {
		t.Error("an explicit wait_for_ip: false must be honoured even with network checks")
	}

	p, err = Parse([]byte("name: x\nworkload: {provider: p, id: \"1\"}\nstartup: {wait_for_agent: true}\nchecks:\n  - {type: tcp, port: 22}\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !p.Startup.WaitsForAgent() {
		t.Error("an explicit wait_for_agent: true must be honoured")
	}
}
