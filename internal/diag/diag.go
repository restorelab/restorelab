// Package diag runs the readiness checks a recovery drill depends on, and
// returns them as data rather than printing them.
//
// They used to live inside the doctor command, interleaved with its output.
// The HTTP API has to answer the same question in JSON, and two
// implementations of "is this cluster ready for a drill" would disagree
// within a release - the CLI would gain a check the API never learned about.
// This package is the single implementation: the CLI renders it, the API
// serialises it.
//
// Nothing here mutates anything. The provider methods it calls are the
// read-only ones, and the fakes in the tests fail outright if a destructive
// one is ever reached.
package diag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Level is how bad a finding is.
type Level string

const (
	// LevelOK is something that works.
	LevelOK Level = "ok"
	// LevelWarn is something that may bite but does not stop a drill. "I
	// could not verify this" lives here: not being able to check is not the
	// same as knowing it is wrong.
	LevelWarn Level = "warn"
	// LevelFail is something that would stop or endanger a drill.
	LevelFail Level = "fail"
)

// Areas a finding can belong to. They are the sections doctor prints and the
// keys a dashboard groups by, so they are constants rather than free text.
const (
	AreaCredentials = "credentials"
	AreaNodes       = "nodes"
	AreaStorage     = "storage"
	AreaNetwork     = "network"
	AreaWorkload    = "workload"
	AreaHistory     = "history"
)

// Finding is one observation. Title is the line a human reads; Detail is the
// paragraph underneath it, and is empty far more often than not.
type Finding struct {
	Level  Level
	Area   string
	Title  string
	Detail string
}

// Report is everything one diagnostic run observed, in the order it observed
// it.
type Report struct {
	ProviderID string
	Endpoint   string
	Findings   []Finding
}

// Problems counts the findings that would stop a drill.
func (r Report) Problems() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == LevelFail {
			n++
		}
	}
	return n
}

// OK reports whether nothing would stop a drill. Warnings do not count.
func (r Report) OK() bool { return r.Problems() == 0 }

func (r *Report) add(level Level, area, title, detail string) {
	r.Findings = append(r.Findings, Finding{Level: level, Area: area, Title: title, Detail: detail})
}

func (r *Report) ok(area, title, detail string)   { r.add(LevelOK, area, title, detail) }
func (r *Report) warn(area, title, detail string) { r.add(LevelWarn, area, title, detail) }
func (r *Report) fail(area, title, detail string) { r.add(LevelFail, area, title, detail) }

// Input is everything Run needs. Everything optional may be zero: a nil
// Backups simply skips the backup findings, an empty WorkloadID skips the
// per-workload ones.
type Input struct {
	// Provider is the hypervisor to inspect. Run reports a failure rather
	// than panicking when it is nil.
	Provider core.HypervisorProvider
	// Backups looks up restore points. Optional.
	Backups core.BackupProvider

	ProviderID string
	Endpoint   string

	// Node is the node to inspect. Empty means the first online one.
	Node string

	// NetworkName is the profile's name, for the message.
	NetworkName string
	// Network is the resolved profile.
	Network core.NetworkConfig
	// NetworkErr is set when the profile could not be resolved at all, which
	// is a configuration problem rather than a cluster one.
	NetworkErr error

	// WorkloadID asks for a workload to be inspected in detail. Optional.
	WorkloadID string

	// HistoryDesc is store.Store.Describe(), reported as-is. It is already
	// free of passwords; do not put a raw DSN here.
	HistoryDesc string
}

// Run performs the diagnostic. It never returns an error: every failure is a
// finding, which is the whole point of the package.
func Run(ctx context.Context, in Input) Report {
	r := Report{ProviderID: in.ProviderID, Endpoint: in.Endpoint}

	if in.Provider == nil {
		r.fail(AreaCredentials, "no hypervisor provider is configured",
			"add one with `restorelab connect` or `restorelab provider add`")
		return r
	}

	if err := in.Provider.Ping(ctx); err != nil {
		// Nothing after this would mean anything: every other check needs the
		// API to answer.
		r.fail(AreaCredentials, "cannot reach the API", err.Error())
		return r
	}
	r.ok(AreaCredentials, "API reachable, credentials accepted", "")

	node := in.Node
	if nodes, err := in.Provider.ListNodes(ctx); err != nil {
		r.fail(AreaNodes, "cannot list nodes", err.Error())
	} else {
		online := 0
		for _, n := range nodes {
			if n.Online {
				online++
			}
		}
		r.ok(AreaNodes, fmt.Sprintf("%d node(s), %d online", len(nodes), online), "")
		if node == "" {
			for _, n := range nodes {
				if n.Online {
					node = n.ID
					break
				}
			}
		}
	}

	r.appendStorages(ctx, in, node)
	r.appendNetwork(ctx, in, node)
	if in.WorkloadID != "" {
		r.appendWorkload(ctx, in)
	}
	if in.HistoryDesc != "" {
		r.ok(AreaHistory, "drill history: "+in.HistoryDesc, "")
	}
	return r
}

// appendNetwork reports whether there is somewhere safe to restore onto.
//
// The three outcomes are deliberately different: a profile that is not marked
// isolated is a configuration failure, a bridge proven to have an uplink is a
// cluster failure, and a bridge we are not allowed to look at is a warning.
// Conflating the last two either blocks legitimate drills or hides a real
// danger - see core.ErrIsolationUnverified.
func (r *Report) appendNetwork(ctx context.Context, in Input, node string) {
	switch {
	case in.NetworkErr != nil:
		r.fail(AreaNetwork, fmt.Sprintf("no network profile %q in the config", in.NetworkName),
			in.NetworkErr.Error())
		return
	case !in.Network.Isolated:
		r.fail(AreaNetwork, fmt.Sprintf("network profile %q is not marked isolated", in.NetworkName),
			"a drill would be refused before any mutating call")
		return
	}

	validator, canValidate := in.Provider.(core.NetworkValidator)
	if !canValidate || node == "" {
		r.warn(AreaNetwork, fmt.Sprintf("cannot verify bridge %q from here", in.Network.Bridge),
			"the drill will proceed on the plan's assertion that the network is isolated")
		return
	}

	err := validator.ValidateIsolation(ctx, node, in.Network)
	switch {
	case errors.Is(err, core.ErrIsolationUnverified):
		r.warn(AreaNetwork,
			fmt.Sprintf("cannot verify bridge %q on %s with these credentials", in.Network.Bridge, node),
			"Proxmox does not show this token the node's bridges; a drill will proceed on the plan's assertion that the network is isolated")
	case err != nil:
		r.fail(AreaNetwork, fmt.Sprintf("bridge %q on %s is not usable", in.Network.Bridge, node),
			err.Error()+" - see docs/network-isolation.md to create a bridge with no uplink")
	default:
		r.ok(AreaNetwork, fmt.Sprintf("isolated bridge %q present on %s", in.Network.Bridge, node), "")
	}
}

// appendWorkload inspects one workload the way `doctor --workload` does.
func (r *Report) appendWorkload(ctx context.Context, in Input) {
	w, err := in.Provider.GetWorkload(ctx, in.WorkloadID)
	if err != nil {
		r.fail(AreaWorkload, fmt.Sprintf("workload %s cannot be read", in.WorkloadID), err.Error())
		return
	}
	r.ok(AreaWorkload, fmt.Sprintf("workload %s (%s) on %s", w.ID, w.Name, w.Node), "")

	status, err := in.Provider.GetStatus(ctx, in.WorkloadID)
	switch {
	case err != nil:
		r.warn(AreaWorkload, fmt.Sprintf("cannot read the status of %s", in.WorkloadID), err.Error())
	case status == nil:
		r.warn(AreaWorkload, fmt.Sprintf("no status reported for %s", in.WorkloadID), "")
	case status.PowerState != core.PowerStateRunning:
		r.warn(AreaWorkload,
			fmt.Sprintf("workload %s is %s", in.WorkloadID, status.PowerState),
			"the guest agent can only be checked while the workload runs")
	case !status.AgentReady:
		// A warning, not a failure: network checks work without an agent.
		// Only in-guest checks and address discovery need one.
		r.warn(AreaWorkload, fmt.Sprintf("no QEMU guest agent responding on %s", in.WorkloadID),
			"in-guest checks and address discovery need it: install qemu-guest-agent, then enable the agent in the workload's options")
	default:
		r.ok(AreaWorkload,
			fmt.Sprintf("guest agent responding on %s (%v)", in.WorkloadID, status.IPs), "")
	}

	if in.Backups == nil {
		r.warn(AreaWorkload, "no backup provider available", "backups of this workload were not looked up")
		return
	}

	backup, err := in.Backups.GetLatestBackup(ctx, in.WorkloadID)
	switch {
	case errors.Is(err, core.ErrNoBackup):
		r.fail(AreaWorkload, fmt.Sprintf("workload %s has no backup to restore", in.WorkloadID),
			"RestoreLab verifies existing backups; it does not create them")
	case err != nil:
		r.fail(AreaWorkload, fmt.Sprintf("cannot look up backups of %s", in.WorkloadID), err.Error())
	case backup == nil:
		r.fail(AreaWorkload, fmt.Sprintf("workload %s has no backup to restore", in.WorkloadID), "")
	default:
		r.ok(AreaWorkload,
			fmt.Sprintf("latest backup of %s is %s old", in.WorkloadID, humanAge(backup.Age())),
			backup.CreatedAt.UTC().Format(time.RFC3339))
	}
}

// humanAge renders a duration the way an operator says it out loud.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
