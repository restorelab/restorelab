package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/diag"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
	"github.com/restorelab/restorelab/internal/store"
)

func newDoctorCmd(a *app) *cobra.Command {
	var (
		providerID string
		workloadID string
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that everything a recovery drill needs is in place",
		Long: `Inspects the configured provider and reports what is ready and what is not:
credentials, node reachability, storages holding backups, an isolated network
to restore onto, and whether workloads have a guest agent and recent backups.

It changes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.rawAPI {
				a.verbose = true
			}
			return a.doctor(cmd.Context(), providerID, workloadID)
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "provider to inspect")
	cmd.Flags().StringVar(&workloadID, "workload", "", "also inspect one workload in detail")
	cmd.Flags().BoolVar(&a.rawAPI, "raw", false, "print raw API responses (implies --verbose); for reporting a discovery bug")
	return cmd
}

// doctor runs the readiness diagnostic and prints it.
//
// The findings come from internal/diag, which the HTTP API serialises from
// the same call: there is one definition of "ready for a drill", and both
// surfaces read it. What stays here is the debugging output nobody would put
// in an API - raw Proxmox responses, effective permissions, the tour of why
// a backup someone swears they took is not visible.
func (a *app) doctor(ctx context.Context, providerID, workloadID string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}

	hv, entry, err := a.hypervisor(providerID)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s  %s\n\n", a.paint(colorBold, entry.ID), a.paint(colorDim, entry.Endpoint))

	in := diag.Input{
		Provider:    hv,
		ProviderID:  entry.ID,
		Endpoint:    entry.Endpoint,
		Node:        entry.Node,
		WorkloadID:  workloadID,
		HistoryDesc: store.Describe(a.storeConfig()),
	}
	// A missing backup provider is a finding, not a reason to stop.
	if bp, _, err := a.backups(providerID); err == nil {
		in.Backups = bp
	}
	in.NetworkName = cfg.Defaults.Network
	if in.NetworkName == "" {
		in.NetworkName = "isolated"
	}
	in.Network, in.NetworkErr = cfg.ResolveNetwork(in.NetworkName)

	rep := diag.Run(ctx, in)
	for _, f := range rep.Findings {
		a.printFinding(f)
	}

	a.doctorDetails(ctx, hv, providerID, entry.Node, workloadID, rep)

	fmt.Fprintln(a.out)
	if n := rep.Problems(); n > 0 {
		return fmt.Errorf("%d problem(s) found", n)
	}
	fmt.Fprintf(a.out, "%s ready to run a recovery drill\n", a.ok())
	return nil
}

// printFinding renders one finding, with its detail indented underneath.
func (a *app) printFinding(f diag.Finding) {
	glyph := a.ok()
	switch f.Level {
	case diag.LevelFail:
		glyph = a.fail()
	case diag.LevelWarn:
		glyph = a.warn()
	}
	fmt.Fprintf(a.out, "  %s %s\n", glyph, f.Title)
	if f.Detail != "" {
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim, f.Detail))
	}
}

// hasFinding reports whether the diagnostic said something matching text in
// an area. It is how the extra explanations know when they are wanted.
func hasFinding(r diag.Report, area, text string) bool {
	for _, f := range r.Findings {
		if f.Area == area && strings.Contains(f.Title, text) {
			return true
		}
	}
	return false
}

// doctorDetails prints what only a human staring at a terminal wants: the
// explanations behind a failure, and under --raw, what Proxmox actually
// answered.
func (a *app) doctorDetails(ctx context.Context, hv core.HypervisorProvider,
	providerID, node, workloadID string, rep diag.Report) {

	// "You have no backups" is worth turning into "your backup is still
	// running" or "your last backup task failed", both of which are visible
	// through the API and neither of which is a finding.
	if hasFinding(rep, diag.AreaStorage, "no backups found") {
		if pve, ok := hv.(*proxmox.Provider); ok {
			a.reportRunningBackups(ctx, pve, node)
		}
	}
	if workloadID != "" && hasFinding(rep, diag.AreaWorkload, "no backup to restore") {
		a.explainMissingBackup(ctx, hv, workloadID)
	}

	if !a.verbose && !a.rawAPI {
		return
	}

	fmt.Fprintf(a.out, "\n%s\n", a.paint(colorDim, "-- diagnostics --"))

	if pve, ok := hv.(*proxmox.Provider); ok {
		probes := []string{"/storage"}
		if workloadID != "" {
			probes = append(probes, "/vms/"+workloadID)
		}
		a.doctorPermissions(ctx, pve, probes)

		if storages, err := pve.ListStorages(ctx, node); err == nil {
			for _, s := range storages {
				role := "no backup content"
				if s.HoldsBackups() {
					role = "holds backups"
				}
				state := "active"
				if !s.Active {
					state = "inactive"
				}
				fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
					fmt.Sprintf("storage %-16s %-10s %-18s %s", s.ID, s.Type, role, state)))
			}
		}

		if a.rawAPI && node != "" {
			if raw, err := pve.Raw(ctx, "/nodes/"+node+"/network", url.Values{"type": {"bridge"}}); err == nil {
				fmt.Fprintf(a.out, "      raw type=bridge %s\n", a.paint(colorDim, string(raw)))
			} else {
				fmt.Fprintf(a.out, "      raw type=bridge error: %v\n", err)
			}
		}

		if a.rawAPI && workloadID != "" {
			a.reportBackupNICs(ctx, pve, providerID, workloadID)
		}
	}
}

// reportBackupNICs prints the network interfaces stored inside the workload's
// latest backup.
//
// This is the view that caught the multi-NIC defect: a backup carrying a
// second interface on a production bridge is refused at creation time, and
// nothing else shows what the backup actually holds before a drill touches
// it. It is deliberately --raw only: it costs two extra API calls.
func (a *app) reportBackupNICs(ctx context.Context, pve *proxmox.Provider, providerID, workloadID string) {
	bp, _, err := a.backups(providerID)
	if err != nil {
		return
	}
	backup, err := bp.GetLatestBackup(ctx, workloadID)
	if err != nil || backup == nil {
		return
	}
	w, err := pve.GetWorkload(ctx, workloadID)
	if err != nil {
		return
	}

	cfg, err := pve.BackupConfig(ctx, w.Node, backup.ID)
	if err != nil {
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow,
			fmt.Sprintf("cannot read the config stored in the backup: %v", err)))
		return
	}
	nets := proxmox.BackupNetworkDevices(cfg)
	fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
		fmt.Sprintf("backup carries %d network interface(s): %v", len(nets), nets)))
	for _, n := range nets {
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim, "  "+n+": "+cfg[n]))
	}
}

// reportRunningBackups turns "you have no backups" into "your backup is still
// running", which is a very different thing to tell someone who just started
// one.
func (a *app) reportRunningBackups(ctx context.Context, pve *proxmox.Provider, node string) {
	tasks, err := pve.RunningTasks(ctx, node)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if t.Type != "vzdump" && t.Type != "qmbackup" {
			continue
		}
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow,
			fmt.Sprintf("a backup task is running right now (%s %s), wait for it to finish and run doctor again", t.Type, t.ID)))
		return
	}
}

// explainMissingBackup answers the question the operator is about to ask:
// "but I made one". The two usual answers are that they took a snapshot, or
// that the backup task failed - and both are visible through the API.
func (a *app) explainMissingBackup(ctx context.Context, hv core.HypervisorProvider, id string) {
	pve, ok := hv.(*proxmox.Provider)
	if !ok {
		return
	}
	hint := func(format string, args ...any) {
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow, fmt.Sprintf(format, args...)))
	}

	snaps, err := pve.ListSnapshots(ctx, id)
	if err != nil && a.verbose {
		hint("could not list snapshots: %v", err)
	}
	if err == nil {
		var real []string
		for _, s := range snaps {
			if !s.Current {
				real = append(real, s.Name)
			}
		}
		if len(real) > 0 {
			hint("this workload has %d snapshot(s) (%s) but no backup", len(real), strings.Join(real, ", "))
			hint("a snapshot is not a backup: it lives on the same storage as the workload and dies with it")
			hint("take a real backup instead:  vzdump %s --storage local --mode snapshot --compress zstd", id)
			return
		}
	}

	w, err := hv.GetWorkload(ctx, id)
	if err != nil {
		return
	}
	tasks, err := pve.RecentBackupTasks(ctx, w.Node, 20)
	if err != nil {
		if a.verbose {
			hint("could not list backup tasks: %v", err)
		}
		return
	}
	if a.verbose {
		hint("%d recent backup task(s) on %s", len(tasks), w.Node)
		for _, t := range tasks {
			hint("  task %s id=%q status=%q", t.Type, t.ID, t.Status)
		}
	}
	for _, t := range tasks {
		if t.Running {
			hint("a backup task for %s is still running, wait for it to finish", t.ID)
			return
		}
		if !t.OK() {
			hint("the last backup task on this node failed: %s", t.Status)
			hint("check it in the Proxmox task log before blaming RestoreLab")
			return
		}
	}
}

// doctorPermissions prints what Proxmox says this token can do, which is the
// only reliable way to tell a missing ACL from a missing privilege.
func (a *app) doctorPermissions(ctx context.Context, pve *proxmox.Provider, probePaths []string) {
	perms, err := pve.EffectivePermissions(ctx, "")
	if err != nil {
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow, fmt.Sprintf("cannot read effective permissions: %v", err)))
		return
	}
	// Also probe the exact paths Proxmox checks when it decides whether to
	// show a backup volume: an ACL on /vms only reaches /vms/<id> when it was
	// created with propagation.
	for _, probe := range probePaths {
		if sub, err := pve.EffectivePermissions(ctx, probe); err == nil {
			for path, privs := range sub {
				key := path
				if key == "" {
					key = probe
				}
				perms[key] = privs
			}
		}
	}

	paths := make([]string, 0, len(perms))
	for path := range perms {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		granted := make([]string, 0, len(perms[path]))
		for priv, on := range perms[path] {
			if on {
				granted = append(granted, priv)
			}
		}
		sort.Strings(granted)
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
			fmt.Sprintf("perm %-24s %s", path, strings.Join(granted, ","))))
	}
}
