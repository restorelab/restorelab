package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/providers/proxmox"
)

func newNetworkCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "network",
		Aliases: []string{"net"},
		Short:   "Manage the isolated recovery network",
	}
	cmd.AddCommand(newNetworkCreateCmd(a))
	return cmd
}

func newNetworkCreateCmd(a *app) *cobra.Command {
	var (
		providerID string
		node       string
		bridge     string
		endpoint   string
		noApply    bool
		dryRun     bool
		yes        bool
		admin      connectFlags
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create the isolated bridge recovery drills restore onto",
		Long: `Creates a Linux bridge with no ports and no gateway on a Proxmox node: a
switch that goes nowhere, which is what keeps a restored production clone from
reaching anything.

This needs administrator credentials, not RestoreLab's service token — the
token is deliberately not allowed to reconfigure your node's network. The
password is used once, in memory, and never stored.

Applying network configuration reloads the node's networking. Adding a
portless bridge touches no existing interface, but the change is real: use
--no-apply to write the configuration without activating it, and it will take
effect at the next reboot.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := a.config()
			if err != nil {
				return err
			}

			// Default everything from what is already configured, so the
			// common case is `restorelab network create` with no flags.
			entry, entryErr := a.providerEntry(providerID, "hypervisor")
			if endpoint == "" {
				if entryErr != nil {
					return entryErr
				}
				endpoint = entry.Endpoint
			}
			if bridge == "" {
				name := cfg.Defaults.Network
				if name == "" {
					name = "isolated"
				}
				profile, err := cfg.Network(name)
				if err != nil {
					return fmt.Errorf("no bridge given and no network profile to take one from: %w", err)
				}
				if !profile.Isolated {
					return fmt.Errorf("network profile %q is not marked isolated; refusing to create a bridge for it", name)
				}
				bridge = profile.Bridge
			}
			if node == "" && entryErr == nil {
				node = entry.Node
			}
			if node == "" {
				node, err = a.firstOnlineNode(ctx, providerID)
				if err != nil {
					return err
				}
			}

			admin.insecure = admin.insecure || (entryErr == nil && entry.Insecure)
			if admin.caCert == "" && entryErr == nil {
				admin.caCert = entry.CACertPath
			}

			client, err := a.adminClient(ctx, endpoint, &admin)
			if err != nil {
				return err
			}
			defer client.Close()

			fmt.Fprintf(a.out, "\n%s\n", a.paint(colorBold,
				fmt.Sprintf("RestoreLab will create bridge %s on node %s:", bridge, node)))
			fmt.Fprintf(a.out, "  · a Linux bridge with no ports, no address and no gateway\n")
			switch {
			case dryRun:
				fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim, "dry run: nothing will be changed"))
			case noApply:
				fmt.Fprintf(a.out, "  · written to the pending configuration, active after the next reboot\n")
			default:
				fmt.Fprintf(a.out, "  %s applied immediately, which reloads the node's networking\n", a.warn())
			}
			fmt.Fprintln(a.out)

			if !dryRun && !yes && !a.confirm("Create it?") {
				fmt.Fprintln(a.out, "aborted, nothing was changed")
				return nil
			}

			result, err := client.EnsureIsolatedBridge(ctx, proxmox.BridgeOptions{
				Node:   node,
				Bridge: bridge,
				Apply:  !noApply,
				DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			a.printBridgeResult(result, dryRun)
			return nil
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "provider whose endpoint and node to use")
	cmd.Flags().StringVar(&node, "node", "", "node to create the bridge on (default: the provider's node)")
	cmd.Flags().StringVar(&bridge, "bridge", "", "bridge name (default: the isolated network profile's bridge)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Proxmox endpoint (default: the provider's)")
	cmd.Flags().BoolVar(&noApply, "no-apply", false, "write the configuration without activating it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be done, change nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	cmd.Flags().StringVar(&admin.adminUser, "admin-user", "root@pam", "administrator used once to create the bridge")
	cmd.Flags().StringVar(&admin.adminPassword, "admin-password", "", "administrator password (prefer the prompt or --admin-password-file)")
	cmd.Flags().StringVar(&admin.passwordFile, "admin-password-file", "", "read the administrator password from a file, or '-' for stdin")
	cmd.Flags().BoolVar(&admin.insecure, "insecure", false, "skip TLS certificate verification")
	cmd.Flags().StringVar(&admin.caCert, "ca-cert", "", "PEM file of the cluster's CA")

	return cmd
}

func (a *app) printBridgeResult(result *proxmox.BridgeResult, dryRun bool) {
	for _, step := range result.Steps {
		glyph := a.ok()
		switch {
		case strings.HasPrefix(step.Status, "would"):
			// Nothing happened; a tick would claim otherwise.
			glyph = a.paint(colorDim, "·")
		case step.Status == "already exists", step.Status == "skipped":
			glyph = a.paint(colorDim, "·")
		}
		fmt.Fprintf(a.out, "  %s %-52s %s\n", glyph, step.Description, a.paint(colorDim, step.Status))
		if step.Detail != "" {
			fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow, step.Detail))
		}
	}

	switch {
	case dryRun:
		fmt.Fprintf(a.out, "\n%s dry run: nothing was changed\n", a.warn())
	case result.PendingApply:
		fmt.Fprintf(a.out, "\n%s the bridge is written but not active yet; reboot the node or apply the pending network configuration\n", a.warn())
	case result.Applied:
		fmt.Fprintf(a.out, "\n%s bridge active. Verify it yourself: %s\n", a.ok(),
			a.paint(colorCyan, "brctl show"))
	}
}

// adminClient resolves administrator credentials and opens a short-lived
// administrative session. The password never leaves this process.
func (a *app) adminClient(ctx context.Context, endpoint string, f *connectFlags) (*proxmox.AdminClient, error) {
	normalised, err := normalizeEndpoint(endpoint, proxmoxPort)
	if err != nil {
		return nil, err
	}

	password, err := a.readAdminPassword(f)
	if err != nil {
		return nil, err
	}
	ca, err := readFileIfSet(f.caCert)
	if err != nil {
		return nil, err
	}

	client, err := proxmox.NewAdminClient(proxmox.AdminConfig{
		Endpoint:           normalised,
		Username:           f.adminUser,
		Password:           password,
		InsecureSkipVerify: f.insecure,
		CACertPEM:          ca,
		Timeout:            30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if err := client.Login(ctx); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// firstOnlineNode picks a node when the configuration does not name one.
func (a *app) firstOnlineNode(ctx context.Context, providerID string) (string, error) {
	hv, _, err := a.hypervisor(providerID)
	if err != nil {
		return "", err
	}
	nodes, err := hv.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Online {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("no online node found")
}
