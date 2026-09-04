// Package plan parses, defaults and validates RestoreLab recovery plans.
// A plan describes what to restore, where to restore it, and what must be true
// afterwards for the recovery to count as proven.
package plan

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/core"
)

// Defaults applied when a plan leaves a field empty. They are deliberately
// conservative: isolated network, cleanup always, modest limits.
const (
	DefaultStartupTimeout = 5 * time.Minute
	DefaultCheckTimeout   = 30 * time.Second
	DefaultRestoreTimeout = 60 * time.Minute
	DefaultRetryInterval  = 5 * time.Second
	DefaultGuestPollEvery = 3 * time.Second
)

// BackupStrategy selects which restore point a run uses.
type BackupStrategy string

const (
	// StrategyLatest picks the most recent backup.
	StrategyLatest BackupStrategy = "latest"
	// StrategySpecific pins an explicit backup ID.
	StrategySpecific BackupStrategy = "specific"
)

// Plan is a recovery plan as written by a user.
type Plan struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`

	Workload WorkloadRef `yaml:"workload"`
	Backup   BackupSpec  `yaml:"backup"`
	Restore  RestoreSpec `yaml:"restore"`
	Startup  StartupSpec `yaml:"startup"`
	Checks   []CheckSpec `yaml:"checks"`
	Cleanup  CleanupSpec `yaml:"cleanup"`

	// RTOTarget is the recovery time objective the run is graded against.
	RTOTarget Duration `yaml:"rto_target,omitempty"`

	// Schedule is a standard five-field cron expression, or one of the
	// @weekly family of shorthands. A plan carrying one is drilled by the
	// scheduler at the stated time; a plan without one is only ever drilled
	// on request, which is the case for most plans.
	Schedule string `yaml:"schedule,omitempty"`

	// ScheduleTimezone names the zone Schedule is read in. Empty means the
	// server's local zone, because "0 3 * * 0" means three in the morning
	// where the operator lives. It qualifies Schedule and nothing else,
	// which is why it sits beside it rather than in a block of its own.
	ScheduleTimezone string `yaml:"schedule_timezone,omitempty"`
}

// WorkloadRef points at the production workload to be recovery-tested.
type WorkloadRef struct {
	Provider string `yaml:"provider"`
	ID       string `yaml:"id"`
	Name     string `yaml:"name,omitempty"`
}

// BackupSpec selects the restore point.
type BackupSpec struct {
	Provider string         `yaml:"provider,omitempty"` // defaults to the hypervisor provider's own storage
	Strategy BackupStrategy `yaml:"strategy,omitempty"`
	ID       string         `yaml:"id,omitempty"` // required for strategy: specific

	// MaxAge fails the run early when the newest backup is older than this.
	MaxAge Duration `yaml:"max_age,omitempty"`
}

// RestoreSpec describes where and how the temporary workload is created.
type RestoreSpec struct {
	Node    string `yaml:"node,omitempty"`
	Storage string `yaml:"storage,omitempty"`
	// Pool places the temporary workload in a provider resource pool.
	// Defaults to the provider's configured pool.
	Pool string `yaml:"pool,omitempty"`

	// Network is either a named network profile declared in the RestoreLab
	// config, or the keyword "isolated" to use the configured isolated bridge.
	Network string `yaml:"network,omitempty"`
	// Bridge overrides the resolved network profile's bridge.
	Bridge string `yaml:"bridge,omitempty"`

	CPULimit       int      `yaml:"cpu_limit,omitempty"`
	MemoryLimitMB  int      `yaml:"memory_limit,omitempty"`
	BandwidthKiBps int      `yaml:"bandwidth_limit,omitempty"`
	Timeout        Duration `yaml:"timeout,omitempty"`
}

// StartupSpec controls how long the run waits for the guest to become usable.
type StartupSpec struct {
	// Skip leaves the restored workload powered off (restore-only drill).
	Skip bool `yaml:"skip,omitempty"`
	// Timeout bounds the wait for the guest to become reachable.
	Timeout Duration `yaml:"timeout,omitempty"`
	// WaitForIP requires an IP address from the guest agent before checks run.
	// When false, checks must supply their own target host. Nil means "decide
	// from the checks": pointers here so an explicit value in a plan is never
	// silently overwritten by the defaults.
	WaitForIP *bool `yaml:"wait_for_ip,omitempty"`
	// WaitForAgent requires the guest agent to respond before checks run. It
	// is what plans made only of in-guest checks wait on, since they never
	// need an address at all.
	WaitForAgent *bool `yaml:"wait_for_agent,omitempty"`
	// IP pins the guest address instead of discovering it (static addressing).
	IP string `yaml:"ip,omitempty"`
}

// CleanupSpec controls destruction of the temporary workload.
type CleanupSpec struct {
	// Always destroys the temporary workload even when the run failed.
	// Defaulting to true is a safety property, not a convenience.
	Always *bool `yaml:"always,omitempty"`
	// KeepOnFailure preserves the workload after a failed run for debugging.
	// It overrides Always and must be used deliberately.
	KeepOnFailure bool `yaml:"keep_on_failure,omitempty"`
}

// WaitsForIP reports the effective address-discovery policy.
func (s StartupSpec) WaitsForIP() bool { return s.WaitForIP != nil && *s.WaitForIP }

// WaitsForAgent reports whether the run waits for the guest agent to respond.
func (s StartupSpec) WaitsForAgent() bool { return s.WaitForAgent != nil && *s.WaitForAgent }

// CleanupAlways reports the effective cleanup policy.
func (c CleanupSpec) CleanupAlways() bool { return c.Always == nil || *c.Always }

// CheckSpec is one configured check. Known fields are typed; everything else
// is handed to the check implementation through Params.
type CheckSpec struct {
	Type          string
	Name          string
	Timeout       Duration
	Retries       int
	RetryInterval Duration
	Critical      *bool
	Params        map[string]any

	// Proves declares what this check establishes when it passes, one of
	// none/boot/service/data. Empty means "work it out", which is what
	// almost every plan will say: ProvenLevel deduces a level that is never
	// higher than the truth, and a declaration is only needed by someone who
	// knows their check proves more than RestoreLab can tell from outside.
	Proves string

	// Capture names the number this check reads out of the restored workload,
	// or is empty. The value is recorded against the run under that name and
	// nothing else happens to it: a measurement is worth keeping whether or
	// not anybody has decided yet what it should be.
	Capture string

	// Assert is what the plan says the captured value must be. Violating it
	// fails the check, because somebody wrote `min: 1` to say "this table is
	// never empty" and meant that to count.
	Assert *core.AssertSpec

	// Drift is how far the captured value may fall against what previous
	// drills of this workload measured. Violating it fails the check, for the
	// same reason: the tolerance was declared.
	//
	// Both of these are core types rather than plan-local ones because they
	// are the same statement at both ends. Their YAML shapes live in
	// values.go, which is where the file's spelling is allowed to differ from
	// the runtime's.
	Drift *core.DriftSpec
}

// reservedCheckKeys are consumed by CheckSpec itself and never forwarded to
// the check implementation.
var reservedCheckKeys = map[string]bool{
	"type": true, "name": true, "timeout": true,
	"retries": true, "retry_interval": true, "critical": true,
	"proves": true, "capture": true, "assert": true, "drift": true,
}

// UnmarshalYAML splits a check mapping into typed fields plus free-form params.
func (c *CheckSpec) UnmarshalYAML(node *yaml.Node) error {
	var raw map[string]any
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("check must be a mapping: %w", err)
	}

	var head struct {
		Type          string      `yaml:"type"`
		Name          string      `yaml:"name"`
		Timeout       Duration    `yaml:"timeout"`
		Retries       int         `yaml:"retries"`
		RetryInterval Duration    `yaml:"retry_interval"`
		Critical      *bool       `yaml:"critical"`
		Proves        string      `yaml:"proves"`
		Capture       string      `yaml:"capture"`
		Assert        *assertSpec `yaml:"assert"`
		Drift         *driftSpec  `yaml:"drift"`
	}
	if err := node.Decode(&head); err != nil {
		return err
	}

	c.Type = head.Type
	c.Name = head.Name
	c.Timeout = head.Timeout
	c.Retries = head.Retries
	c.RetryInterval = head.RetryInterval
	c.Critical = head.Critical
	c.Proves = head.Proves
	// Trimmed because the name is part of the key the value is stored and read
	// back under: " rows" and "rows" would quietly split one workload's
	// history in two, and the drift comparison would then find no baseline for
	// either half.
	c.Capture = strings.TrimSpace(head.Capture)
	c.Assert = head.Assert.core()
	c.Drift = head.Drift.core()

	c.Params = make(map[string]any, len(raw))
	for k, v := range raw {
		if !reservedCheckKeys[k] {
			c.Params[k] = v
		}
	}
	return nil
}

// MarshalYAML writes a check back in the flat shape UnmarshalYAML reads.
//
// It exists because CheckSpec's params are inlined on the way in, and struct
// marshalling would put them back under a nested `params:` key alongside a
// `retryinterval` nobody reads. That asymmetry is invisible while a plan is
// only ever written for a human to look at - and fatal the moment one is
// written to be read back. The API stores the plan a drill was queued
// against and the worker parses that snapshot to execute it: a check whose
// params did not survive the round trip would reach the engine with nothing
// in it - a tcp check with no port, a command check with nothing to run -
// and fail every drill triggered over HTTP.
//
// Params are written first so the typed fields always win: a param that
// collides with a reserved key could not survive a parse anyway, because
// UnmarshalYAML never puts one into Params.
func (c CheckSpec) MarshalYAML() (any, error) {
	out := make(map[string]any, len(c.Params)+len(reservedCheckKeys))
	for k, v := range c.Params {
		out[k] = v
	}

	out["type"] = c.Type
	if c.Name != "" {
		out["name"] = c.Name
	}
	if c.Timeout != 0 {
		out["timeout"] = c.Timeout
	}
	if c.Retries != 0 {
		out["retries"] = c.Retries
	}
	if c.RetryInterval != 0 {
		out["retry_interval"] = c.RetryInterval
	}
	if c.Critical != nil {
		out["critical"] = *c.Critical
	}
	if c.Proves != "" {
		out["proves"] = c.Proves
	}
	if c.Capture != "" {
		out["capture"] = c.Capture
	}
	if c.Assert != nil {
		out["assert"] = assertYAML(c.Assert)
	}
	if c.Drift != nil {
		out["drift"] = driftYAML(c.Drift)
	}
	return out, nil
}

// IsCritical reports whether a failure of this check fails the whole run.
// Checks are critical unless explicitly marked otherwise.
func (c CheckSpec) IsCritical() bool { return c.Critical == nil || *c.Critical }

// DisplayName is the name shown in reports and timelines.
func (c CheckSpec) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	if port, ok := c.Params["port"]; ok {
		return fmt.Sprintf("%s %v", strings.ToUpper(c.Type), port)
	}
	return strings.ToUpper(c.Type)
}

// ToCore converts the spec into the runtime check configuration.
func (c CheckSpec) ToCore() core.CheckConfig {
	return core.CheckConfig{
		Type:          c.Type,
		Name:          c.DisplayName(),
		Timeout:       c.Timeout.Or(DefaultCheckTimeout),
		Retries:       c.Retries,
		RetryInterval: c.RetryInterval.Or(DefaultRetryInterval),
		Critical:      c.IsCritical(),
		Params:        c.Params,
		Proves:        c.ProvenLevel(),
		Capture:       c.Capture,
		Assert:        c.Assert,
		Drift:         c.Drift,
	}
}

// Load reads and validates a plan from a YAML file.
func Load(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	return Parse(data)
}

// Parse decodes, defaults and validates a plan from YAML bytes.
func Parse(data []byte) (*Plan, error) {
	var p Plan
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// ApplyDefaults fills in the conservative defaults.
func (p *Plan) ApplyDefaults() {
	if p.Backup.Strategy == "" {
		p.Backup.Strategy = StrategyLatest
	}
	if p.Restore.Network == "" {
		p.Restore.Network = "isolated"
	}
	if p.Restore.Timeout == 0 {
		p.Restore.Timeout = Duration(DefaultRestoreTimeout)
	}
	if p.Startup.Timeout == 0 {
		p.Startup.Timeout = Duration(DefaultStartupTimeout)
	}
	if p.Startup.WaitForIP == nil {
		// Network checks need somewhere to connect, so the guest address is
		// discovered by default. A plan made only of in-guest checks needs no
		// address at all: waiting for one there would fail drills on guests
		// that legitimately have no DHCP inside the isolated network.
		want := !p.Startup.Skip && p.Startup.IP == "" && p.needsNetwork()
		p.Startup.WaitForIP = &want
	}
	if p.Startup.WaitForAgent == nil {
		want := !p.Startup.Skip && p.needsGuestAgent()
		p.Startup.WaitForAgent = &want
	}
	if p.Cleanup.Always == nil {
		always := true
		p.Cleanup.Always = &always
	}
	for i := range p.Checks {
		if p.Checks[i].Timeout == 0 {
			p.Checks[i].Timeout = Duration(DefaultCheckTimeout)
		}
		if p.Checks[i].RetryInterval == 0 {
			p.Checks[i].RetryInterval = Duration(DefaultRetryInterval)
		}
	}
}

// Validate reports every structural problem with the plan at once.
func (p *Plan) Validate() error {
	var errs []string

	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, "name is required")
	}
	if p.Workload.ID == "" {
		errs = append(errs, "workload.id is required")
	}
	if p.Workload.Provider == "" {
		errs = append(errs, "workload.provider is required")
	}

	switch p.Backup.Strategy {
	case StrategyLatest:
	case StrategySpecific:
		if p.Backup.ID == "" {
			errs = append(errs, `backup.id is required with strategy "specific"`)
		}
	default:
		errs = append(errs, fmt.Sprintf("backup.strategy %q is not supported (latest, specific)", p.Backup.Strategy))
	}

	if p.Restore.CPULimit < 0 {
		errs = append(errs, "restore.cpu_limit must be positive")
	}
	if p.Restore.MemoryLimitMB < 0 {
		errs = append(errs, "restore.memory_limit must be positive")
	}
	if p.Startup.Skip && len(p.Checks) > 0 {
		errs = append(errs, "checks cannot run when startup.skip is set")
	}

	// A schedule is refused here, when the plan is written, rather than
	// discovered at three in the morning by a scheduler that can only skip
	// the plan and log about it. This is the one field whose mistakes are
	// otherwise invisible: nothing fails, a machine simply stops being
	// tested.
	if _, err := ParseSchedule(p.Schedule, p.ScheduleTimezone); err != nil {
		errs = append(errs, err.Error())
	}

	seen := map[string]bool{}
	for i, c := range p.Checks {
		if c.Type == "" {
			errs = append(errs, fmt.Sprintf("checks[%d].type is required", i))
			continue
		}
		if _, ok := core.ParseProofLevel(strings.ToUpper(c.Proves)); !ok {
			errs = append(errs, fmt.Sprintf(
				"checks[%d].proves = %q: must be one of none, boot, service, data", i, c.Proves))
		}
		// A plan that cannot work should say so where it is written, not at
		// three in the morning where the only thing left to do about it is
		// read a log. Every rule below refuses a combination that has no
		// reading at all, never one that is merely unusual.
		if c.Capture != "" && c.Type != "" && !capturingCheckTypes[c.Type] {
			errs = append(errs, fmt.Sprintf(
				"checks[%d].capture: a %s check does not read a value; only a command check does",
				i, c.Type))
		}
		if c.Assert != nil && !c.Assert.Any() {
			errs = append(errs, fmt.Sprintf(
				"checks[%d].assert states no bound: set min, max or equals", i))
		}
		if c.Assert != nil && c.Capture == "" {
			errs = append(errs, fmt.Sprintf(
				"checks[%d].assert has nothing to bound: set capture to name the value it applies to", i))
		}
		if c.Drift != nil && c.Capture == "" {
			errs = append(errs, fmt.Sprintf(
				"checks[%d].drift has nothing to compare: set capture to name the value it bounds", i))
		}
		if c.Drift != nil && !statesATolerance(c.Drift) {
			errs = append(errs, fmt.Sprintf(
				`checks[%d].drift.max_drop must be a positive tolerance ("10%%" or a number of units), got %v`,
				i, c.Drift.MaxDrop))
		}

		name := c.DisplayName()
		if seen[name] {
			errs = append(errs, fmt.Sprintf("checks[%d]: duplicate check name %q, set an explicit name", i, name))
		}
		seen[name] = true
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid plan:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// guestAgentCheckTypes are the checks that run inside the guest through the
// provider rather than over the network. Extend this when a new one lands.
var guestAgentCheckTypes = map[string]bool{
	"command": true,
}

// needsNetwork reports whether any configured check connects to the guest over
// the network, which is what makes discovering its address necessary.
//
// A plan with no checks at all still waits for an address: proving the guest
// configured its network is the only signal such a drill has left.
func (p *Plan) needsNetwork() bool {
	if len(p.Checks) == 0 {
		return true
	}
	for _, c := range p.Checks {
		if !guestAgentCheckTypes[c.Type] {
			return true
		}
	}
	return false
}

// needsGuestAgent reports whether any configured check runs inside the guest.
func (p *Plan) needsGuestAgent() bool {
	for _, c := range p.Checks {
		if guestAgentCheckTypes[c.Type] {
			return true
		}
	}
	return false
}
