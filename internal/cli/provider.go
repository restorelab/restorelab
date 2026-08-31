package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/providers"
)

// tokenSecretEnv lets deployments pass the secret without putting it in shell
// history or in a process listing.
const tokenSecretEnv = "RESTORELAB_TOKEN_SECRET"

func newProviderCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "provider",
		Aliases: []string{"providers"},
		Short:   "Manage Proxmox VE and Proxmox Backup Server connections",
	}
	cmd.AddCommand(
		newProviderAddCmd(a),
		newProviderListCmd(a),
		newProviderTestCmd(a),
		newProviderRemoveCmd(a),
	)
	return cmd
}

// providerFlags is the flag set shared by `provider add proxmox` and
// `provider add pbs`.
type providerFlags struct {
	id          string
	endpoint    string
	tokenID     string
	tokenSecret string
	secretFile  string
	insecure    bool
	fingerprint string
	caCertPath  string

	// proxmox
	node          string
	backupStorage string
	tempIDMin     int
	tempIDMax     int

	// pbs
	datastore  string
	pveStorage string

	noTest bool
}

func (f *providerFlags) bindCommon(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&f.id, "id", "", "identifier plans refer to (required)")
	fs.StringVar(&f.endpoint, "endpoint", "", "base URL, e.g. https://pve.example.com:8006 (required)")
	fs.StringVar(&f.tokenID, "token-id", "", "API token id, e.g. 'restorelab@pve!drills' (required)")
	fs.StringVar(&f.tokenSecret, "token-secret", "", "API token secret (prefer --token-secret-file or $"+tokenSecretEnv+")")
	fs.StringVar(&f.secretFile, "token-secret-file", "", "read the token secret from a file, or '-' for stdin")
	fs.BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification")
	fs.StringVar(&f.caCertPath, "ca-cert", "", "PEM file of a private CA")
	fs.BoolVar(&f.noTest, "no-test", false, "skip the connection test before saving")
}

func newProviderAddCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a provider",
		Long: `Adds a provider and seals its API token secret with the master key.

Avoid --token-secret on a shared machine: it lands in your shell history and in
the process list. Prefer --token-secret-file, '-' to read stdin, or the
` + tokenSecretEnv + ` environment variable.`,
	}

	pve := &providerFlags{}
	proxmoxCmd := &cobra.Command{
		Use:   "proxmox",
		Short: "Add a Proxmox VE cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.addProvider(cmd.Context(), providers.KindProxmox, pve)
		},
	}
	pve.bindCommon(proxmoxCmd)
	proxmoxCmd.Flags().StringVar(&pve.node, "node", "", "default node for API calls")
	proxmoxCmd.Flags().StringVar(&pve.backupStorage, "backup-storage", "", "storage holding backups (default: scan every backup-capable storage)")
	proxmoxCmd.Flags().IntVar(&pve.tempIDMin, "temp-id-min", 9000, "lowest VMID used for temporary workloads")
	proxmoxCmd.Flags().IntVar(&pve.tempIDMax, "temp-id-max", 9999, "highest VMID used for temporary workloads")

	pbsFlags := &providerFlags{}
	pbsCmd := &cobra.Command{
		Use:   "pbs",
		Short: "Add a Proxmox Backup Server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.addProvider(cmd.Context(), providers.KindPBS, pbsFlags)
		},
	}
	pbsFlags.bindCommon(pbsCmd)
	pbsCmd.Flags().StringVar(&pbsFlags.datastore, "datastore", "", "PBS datastore name (required)")
	pbsCmd.Flags().StringVar(&pbsFlags.pveStorage, "pve-storage", "", "name this datastore is attached under in PVE (default: the datastore name)")
	pbsCmd.Flags().StringVar(&pbsFlags.fingerprint, "fingerprint", "", "SHA-256 certificate fingerprint to pin (recommended for a self-signed PBS)")

	cmd.AddCommand(proxmoxCmd, pbsCmd)
	return cmd
}

func (a *app) addProvider(ctx context.Context, kind string, f *providerFlags) error {
	if f.id == "" || f.endpoint == "" || f.tokenID == "" {
		return fmt.Errorf("--id, --endpoint and --token-id are required")
	}

	defaultPort := proxmoxPort
	if kind == providers.KindPBS {
		defaultPort = pbsPort
	}
	endpoint, err := normalizeEndpoint(f.endpoint, defaultPort)
	if err != nil {
		return err
	}
	if kind == providers.KindPBS && f.datastore == "" {
		return fmt.Errorf("--datastore is required for a Proxmox Backup Server")
	}

	secret, err := a.readSecret(f)
	if err != nil {
		return err
	}

	cfg, err := a.config()
	if err != nil {
		return err
	}
	key, err := a.masterKey()
	if err != nil {
		return err
	}

	entry := config.Provider{
		ID:          f.id,
		Kind:        kind,
		Endpoint:    endpoint,
		TokenID:     f.tokenID,
		Insecure:    f.insecure,
		Fingerprint: f.fingerprint,
		CACertPath:  f.caCertPath,
	}
	switch kind {
	case providers.KindProxmox:
		entry.Roles = []string{providers.RoleHypervisor, providers.RoleBackup}
		entry.Node = f.node
		entry.BackupStorage = f.backupStorage
		entry.TempIDMin = f.tempIDMin
		entry.TempIDMax = f.tempIDMax
	case providers.KindPBS:
		entry.Roles = []string{providers.RoleBackup}
		entry.Datastore = f.datastore
		entry.PVEStorage = f.pveStorage
	}

	cfg.Upsert(entry)
	if err := cfg.SetProviderSecret(entry.ID, secret, key); err != nil {
		return err
	}

	// Test before persisting: writing a provider that cannot authenticate only
	// moves the failure to the next command.
	if !f.noTest {
		saved, _ := cfg.Provider(entry.ID)
		if err := a.testProvider(ctx, *saved, key); err != nil {
			return fmt.Errorf("connection test failed (use --no-test to add it anyway): %w", err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.Save(a.path(), cfg); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s provider %s added (%s)\n", a.ok(), a.paint(colorBold, entry.ID), entry.Endpoint)
	return nil
}

// readSecret resolves the token secret from the flags, a file, stdin or the
// environment, in that order.
func (a *app) readSecret(f *providerFlags) (string, error) {
	if f.tokenSecret != "" {
		fmt.Fprintf(a.err, "%s the token secret was passed on the command line; it is now in your shell history\n", a.warn())
		return f.tokenSecret, nil
	}

	if f.secretFile != "" {
		var (
			data []byte
			err  error
		)
		if f.secretFile == "-" {
			data, err = io.ReadAll(bufio.NewReader(os.Stdin))
		} else {
			data, err = os.ReadFile(f.secretFile)
		}
		if err != nil {
			return "", fmt.Errorf("read token secret: %w", err)
		}
		secret := strings.TrimSpace(string(data))
		if secret == "" {
			return "", fmt.Errorf("the token secret file is empty")
		}
		return secret, nil
	}

	if secret := os.Getenv(tokenSecretEnv); secret != "" {
		return secret, nil
	}

	return "", fmt.Errorf("no token secret provided: use --token-secret-file, --token-secret, or $%s", tokenSecretEnv)
}

func newProviderListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured providers",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			if len(cfg.Providers) == 0 {
				fmt.Fprintf(a.out, "No providers configured. Add one with %s\n", a.paint(colorCyan, "restorelab provider add proxmox --help"))
				return nil
			}

			t := a.table(a.out, "ID", "KIND", "ROLES", "ENDPOINT", "DETAILS")
			for _, p := range cfg.Providers {
				t.row(p.ID, p.Kind, strings.Join(p.Roles, ","), p.Endpoint, providerDetails(p))
			}
			t.flush()
			return nil
		},
	}
}

func providerDetails(p config.Provider) string {
	var parts []string
	if p.Datastore != "" {
		parts = append(parts, "datastore="+p.Datastore)
	}
	if p.BackupStorage != "" {
		parts = append(parts, "backup-storage="+p.BackupStorage)
	}
	if p.TempIDMin != 0 || p.TempIDMax != 0 {
		parts = append(parts, fmt.Sprintf("temp-ids=%d-%d", p.TempIDMin, p.TempIDMax))
	}
	if p.Insecure {
		parts = append(parts, "insecure")
	}
	if p.Fingerprint != "" {
		parts = append(parts, "pinned")
	}
	return strings.Join(parts, " ")
}

func newProviderTestCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "test [provider-id]",
		Short: "Check that a provider is reachable and the token works",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			key, err := a.masterKey()
			if err != nil {
				return err
			}

			targets := cfg.Providers
			if len(args) == 1 {
				p, err := cfg.Provider(args[0])
				if err != nil {
					return err
				}
				targets = []config.Provider{*p}
			}
			if len(targets) == 0 {
				return fmt.Errorf("no providers configured")
			}

			failed := 0
			for _, p := range targets {
				if err := a.testProvider(cmd.Context(), p, key); err != nil {
					failed++
					fmt.Fprintf(a.out, "%s %-16s %v\n", a.fail(), p.ID, err)
					continue
				}
				fmt.Fprintf(a.out, "%s %-16s %s\n", a.ok(), p.ID, a.paint(colorDim, p.Endpoint))
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d providers failed", failed, len(targets))
			}
			return nil
		},
	}
}

// testProvider builds a live client for the entry and pings it. Both provider
// interfaces embed Pinger's method set, so the concrete kind stays irrelevant
// here.
func (a *app) testProvider(ctx context.Context, p config.Provider, key crypto.Key) error {
	var client providers.Pinger

	if providers.HasRole(p, providers.RoleHypervisor) {
		hp, err := providers.NewHypervisor(p, key)
		if err != nil {
			return err
		}
		client = hp
	} else {
		bp, err := providers.NewBackup(p, key)
		if err != nil {
			return err
		}
		client = bp
	}
	return client.Ping(ctx)
}

func newProviderRemoveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <provider-id>",
		Aliases: []string{"rm"},
		Short:   "Remove a provider from the configuration",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			id := args[0]

			kept := make([]config.Provider, 0, len(cfg.Providers))
			for _, p := range cfg.Providers {
				if p.ID != id {
					kept = append(kept, p)
				}
			}
			if len(kept) == len(cfg.Providers) {
				return fmt.Errorf("no provider with id %q", id)
			}
			cfg.Providers = kept

			if cfg.Defaults.Provider == id {
				cfg.Defaults.Provider = ""
			}
			if cfg.Defaults.BackupProvider == id {
				cfg.Defaults.BackupProvider = ""
			}

			if err := config.Save(a.path(), cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s provider %s removed\n", a.ok(), id)
			return nil
		},
	}
}
