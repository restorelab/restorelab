package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
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

	problems := 0
	fail := func(format string, args ...any) {
		problems++
		fmt.Fprintf(a.out, "  %s %s\n", a.fail(), fmt.Sprintf(format, args...))
	}
	warn := func(format string, args ...any) {
		fmt.Fprintf(a.out, "  %s %s\n", a.warn(), fmt.Sprintf(format, args...))
	}
	ok := func(format string, args ...any) {
		fmt.Fprintf(a.out, "  %s %s\n", a.ok(), fmt.Sprintf(format, args...))
	}

	// --- credentials and nodes ---
	if err := hv.Ping(ctx); err != nil {
		fail("cannot reach the API: %v", err)
		return fmt.Errorf("%d problem(s) found", problems)
	}
	ok("API reachable, credentials accepted")

	nodes, err := hv.ListNodes(ctx)
	if err != nil {
		fail("cannot list nodes: %v", err)
	} else {
		online := 0
		for _, n := range nodes {
			if n.Online {
				online++
			}
		}
		ok("%d node(s), %d online", len(nodes), online)
	}

	// --- effective permissions ---
	if isPVEProvider(hv) && a.verbose {
		probes := []string{"/storage"}
		if workloadID != "" {
			probes = append(probes, "/vms/"+workloadID)
		}
		a.doctorPermissions(ctx, hv.(*proxmox.Provider), probes)
	}

	// --- storages holding backups ---
	pve, isPVE := hv.(*proxmox.Provider)
	if isPVE {
		a.doctorStorages(ctx, pve, entry.Node, workloadID, ok, warn, fail)
	}

	// --- isolated network ---
	networkName := cfg.Defaults.Network
	if networkName == "" {
		networkName = "isolated"
	}
	network, err := cfg.ResolveNetwork(networkName)
	switch {
	case err != nil:
		fail("no network profile %q in the config", networkName)
	case !network.Isolated:
		fail("network profile %q is not marked isolated; a drill would be refused", networkName)
	default:
		validator, canValidate := hv.(core.NetworkValidator)
		node := entry.Node
		if node == "" && len(nodes) > 0 {
			node = nodes[0].ID
		}
		switch {
		case !canValidate || node == "":
			warn("cannot verify bridge %q from here", network.Bridge)
		default:
			if a.rawAPI {
				if perms, permErr := pve.EffectivePermissions(ctx, "/nodes/"+node); permErr == nil {
					for path, privs := range perms {
						granted := make([]string, 0, len(privs))
						for priv, on := range privs {
							if on {
								granted = append(granted, priv)
							}
						}
						sort.Strings(granted)
						fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
							fmt.Sprintf("perm %-24s %s", path, strings.Join(granted, ","))))
					}
				}
				if raw, rawErr := pve.Raw(ctx, "/nodes/"+node+"/network", url.Values{"type": {"bridge"}}); rawErr == nil {
					fmt.Fprintf(a.out, "      raw type=bridge %s\n", a.paint(colorDim, string(raw)))
				} else {
					fmt.Fprintf(a.out, "      raw type=bridge error: %v\n", rawErr)
				}
				if raw, rawErr := pve.Raw(ctx, "/nodes/"+node+"/network", nil); rawErr == nil {
					body := string(raw)
					if len(body) > 3000 {
						body = body[:3000] + "..."
					}
					fmt.Fprintf(a.out, "      raw %s\n", a.paint(colorDim, body))
				}
			}
			err := validator.ValidateIsolation(ctx, node, network)
			switch {
			case errors.Is(err, core.ErrIsolationUnverified):
				warn("cannot verify bridge %q on %s with these credentials", network.Bridge, node)
				fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
					"Proxmox does not show this token the node's bridges; a drill will proceed on the plan's assertion that the network is isolated"))
			case err != nil:
				fail("bridge %q on %s: %v", network.Bridge, node, err)
				fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim, "see docs/network-isolation.md to create a bridge with no uplink"))
			default:
				ok("isolated bridge %q present on %s", network.Bridge, node)
			}
		}
	}

	// --- one workload in detail ---
	if workloadID != "" {
		a.doctorWorkload(ctx, hv, providerID, workloadID, ok, warn, fail)
	}

	fmt.Fprintln(a.out)
	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	fmt.Fprintf(a.out, "%s ready to run a recovery drill\n", a.ok())
	return nil
}

func (a *app) doctorStorages(ctx context.Context, pve *proxmox.Provider, node, workloadID string,
	ok, warn, fail func(string, ...any)) {

	storages, err := pve.ListStorages(ctx, node)
	if err != nil {
		fail("cannot list storages: %v", err)
		return
	}

	var backupStorages []proxmox.Storage
	for _, s := range storages {
		if s.HoldsBackups() {
			backupStorages = append(backupStorages, s)
		}
	}

	// Every storage, not only the backup-capable ones: when a backup seems to
	// be missing, the first thing to rule out is that it landed somewhere
	// RestoreLab is not looking.
	if a.verbose || len(backupStorages) == 0 {
		for _, s := range storages {
			role := "no backup content"
			if s.HoldsBackups() {
				role = "holds backups"
			}
			state := "active"
			if !s.Active {
				state = a.paint(colorYellow, "inactive")
			}
			fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
				fmt.Sprintf("storage %-16s %-10s %-18s %s", s.ID, s.Type, role, state)))
		}
	}

	if len(backupStorages) == 0 {
		fail("no storage on this cluster advertises backup content")
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
			"RestoreLab restores from Proxmox backups: configure a backup job, or attach a Proxmox Backup Server"))
		return
	}

	if node == "" {
		if nodes, err := pve.ListNodes(ctx); err == nil {
			for _, n := range nodes {
				if n.Online {
					node = n.ID
					break
				}
			}
		}
	}

	total := 0
	for _, s := range backupStorages {
		count, err := pve.CountBackups(ctx, node, s.ID, workloadID)
		switch {
		case err != nil:
			warn("storage %q (%s): cannot read contents: %v", s.ID, s.Type, err)
		case !s.Active:
			warn("storage %q (%s) is not active", s.ID, s.Type)
		case a.verbose:
			total += count
			if a.rawAPI {
				if raw, rawErr := pve.Raw(ctx, "/nodes/"+node+"/storage/"+s.ID+"/content", nil); rawErr == nil {
					body := string(raw)
					if len(body) > 2000 {
						body = body[:2000] + "..."
					}
					fmt.Fprintf(a.out, "      raw %s\n", a.paint(colorDim, body))
				}
			}
			all, allErr := pve.ListContentIDs(ctx, node, s.ID, "", "")
			if allErr != nil {
				warn("storage %q: unfiltered content listing failed: %v", s.ID, allErr)
			} else {
				ok("storage %q (%s): %d backup(s), %d volume(s) of any kind", s.ID, s.Type, count, len(all))
				for i, v := range all {
					if i >= 10 {
						break
					}
					fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim, "volume "+v))
				}
			}
		default:
			total += count
			scope := "backup(s)"
			if workloadID != "" {
				scope = fmt.Sprintf("backup(s) for workload %s", workloadID)
			}
			ok("storage %q (%s): %d %s", s.ID, s.Type, count, scope)
		}
	}
	if total == 0 {
		fail("no backups found on any storage — there is nothing to recovery-test yet")
		a.reportRunningBackups(ctx, pve, node)
	}
}

func (a *app) doctorWorkload(ctx context.Context, hv core.HypervisorProvider, providerID, id string,
	ok, warn, fail func(string, ...any)) {

	w, err := hv.GetWorkload(ctx, id)
	if err != nil {
		fail("workload %s: %v", id, err)
		return
	}
	ok("workload %s (%s) on %s", w.ID, w.Name, w.Node)

	status, err := hv.GetStatus(ctx, id)
	switch {
	case err != nil:
		warn("cannot read the status of %s: %v", id, err)
	case status.PowerState != core.PowerStateRunning:
		warn("workload %s is %s; the guest agent can only be checked while it runs", id, status.PowerState)
	case !status.AgentReady:
		warn("no QEMU guest agent responding on %s", id)
		fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
			"in-guest checks and address discovery need it: apt install qemu-guest-agent, then enable the agent in the VM options"))
	default:
		ok("guest agent responding on %s (%v)", id, status.IPs)
	}

	bp, _, err := a.backups(providerID)
	if err != nil {
		warn("no backup provider available: %v", err)
		return
	}
	backup, err := bp.GetLatestBackup(ctx, id)
	if err == nil && a.rawAPI {
		if pve, isPVE := hv.(*proxmox.Provider); isPVE {
			if w, werr := hv.GetWorkload(ctx, id); werr == nil {
				if cfg, cerr := pve.BackupConfig(ctx, w.Node, backup.ID); cerr == nil {
					nets := proxmox.BackupNetworkDevices(cfg)
					fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim,
						fmt.Sprintf("backup carries %d network interface(s): %v", len(nets), nets)))
					for _, n := range nets {
						fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim, "  "+n+": "+cfg[n]))
					}
				} else {
					fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow,
						fmt.Sprintf("cannot read the config stored in the backup: %v", cerr)))
				}
			}
		}
	}
	switch {
	case errors.Is(err, core.ErrNoBackup):
		fail("workload %s has no backup to restore", id)
		a.explainMissingBackup(ctx, hv, id)
	case err != nil:
		fail("cannot look up backups of %s: %v", id, err)
	default:
		ok("latest backup of %s: %s (%s old, %s)", id,
			backup.CreatedAt.Local().Format("2006-01-02 15:04"), humanAge(backup.Age()), humanBytes(backup.SizeBytes))
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

func isPVEProvider(hv core.HypervisorProvider) bool {
	_, ok := hv.(*proxmox.Provider)
	return ok
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
