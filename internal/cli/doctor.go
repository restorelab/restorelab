package cli

import (
	"context"
	"errors"
	"fmt"

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
			return a.doctor(cmd.Context(), providerID, workloadID)
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "provider to inspect")
	cmd.Flags().StringVar(&workloadID, "workload", "", "also inspect one workload in detail")
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
			if err := validator.ValidateIsolation(ctx, node, network); err != nil {
				fail("bridge %q on %s: %v", network.Bridge, node, err)
				fmt.Fprintf(a.out, "      %s\n", a.paint(colorDim, "see docs/network-isolation.md to create a bridge with no uplink"))
			} else {
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
	switch {
	case errors.Is(err, core.ErrNoBackup):
		fail("workload %s has no backup to restore", id)
	case err != nil:
		fail("cannot look up backups of %s: %v", id, err)
	default:
		ok("latest backup of %s: %s (%s old, %s)", id,
			backup.CreatedAt.Local().Format("2006-01-02 15:04"), humanAge(backup.Age()), humanBytes(backup.SizeBytes))
	}
}
