// Package cli implements the restorelab command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/providers"
	"github.com/restorelab/restorelab/internal/version"
)

// app carries the state every command needs. Configuration and the master key
// are loaded lazily so that `init`, `key generate` and `version` work before
// anything exists on disk.
type app struct {
	configPath string
	keyPath    string
	noColor    bool
	verbose    bool

	out io.Writer
	err io.Writer

	cfg    *config.Config
	key    crypto.Key
	keySet bool
}

// Execute runs the root command. It returns the process exit code.
func Execute(ctx context.Context) int {
	a := &app{out: os.Stdout, err: os.Stderr}
	root := newRootCmd(a)

	if err := root.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(a.err, a.paint(colorYellow, "interrupted"))
			return 130
		}
		fmt.Fprintf(a.err, "%s %v\n", a.paint(colorRed, "error:"), err)
		if hint := hintFor(err); hint != "" {
			fmt.Fprintf(a.err, "  %s\n", hint)
		}
		return 1
	}
	return 0
}

func newRootCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restorelab",
		Short: "Prove your backups can actually recover your services",
		Long: `RestoreLab restores your backups into an isolated environment, boots the
workloads, validates the services, measures your real recovery time, and
cleans everything up.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.String(),
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			// NO_COLOR is a de-facto standard; respect it, and never emit
			// escape codes when output is being piped somewhere.
			if os.Getenv("NO_COLOR") != "" || !isTerminal(os.Stdout) {
				a.noColor = true
			}
		},
	}

	f := cmd.PersistentFlags()
	f.StringVar(&a.configPath, "config", "", "path to config.yaml (default: $RESTORELAB_CONFIG or ~/.restorelab/config.yaml)")
	f.StringVar(&a.keyPath, "master-key-file", "", "path to the master key file (default: $RESTORELAB_MASTER_KEY or ~/.restorelab/master.key)")
	f.BoolVar(&a.noColor, "no-color", false, "disable coloured output")
	f.BoolVarP(&a.verbose, "verbose", "v", false, "verbose output")

	cmd.SetOut(a.out)
	cmd.SetErr(a.err)

	cmd.AddCommand(
		newInitCmd(a),
		newConnectCmd(a),
		newDoctorCmd(a),
		newKeyCmd(a),
		newProviderCmd(a),
		newWorkloadsCmd(a),
		newBackupsCmd(a),
		newRecoveryCmd(a),
		newCleanupCmd(a),
		newVersionCmd(a),
	)
	return cmd
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(a.out, version.String())
			return nil
		},
	}
}

// --- lazy loading ------------------------------------------------------------

func (a *app) path() string {
	if a.configPath != "" {
		return a.configPath
	}
	return config.DefaultPath()
}

// config loads and caches the configuration file.
func (a *app) config() (*config.Config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	cfg, err := config.Load(a.path())
	if err != nil {
		return nil, err
	}
	a.cfg = cfg
	return cfg, nil
}

// masterKey resolves and caches the master key.
func (a *app) masterKey() (crypto.Key, error) {
	if a.keySet {
		return a.key, nil
	}
	key, source, err := crypto.LoadKey(a.keyPath)
	if err != nil {
		return crypto.Key{}, err
	}
	if a.verbose {
		fmt.Fprintf(a.err, "master key loaded from %s\n", source)
	}
	a.key, a.keySet = key, true
	return key, nil
}

// providerEntry resolves a provider by ID, falling back to the configured
// default for the role, and to the only candidate when there is exactly one.
func (a *app) providerEntry(id, role string) (config.Provider, error) {
	cfg, err := a.config()
	if err != nil {
		return config.Provider{}, err
	}

	if id == "" {
		switch role {
		case providers.RoleHypervisor:
			id = cfg.Defaults.Provider
		case providers.RoleBackup:
			id = cfg.Defaults.BackupProvider
			if id == "" {
				id = cfg.Defaults.Provider
			}
		}
	}

	if id == "" {
		candidates := make([]config.Provider, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			if providers.HasRole(p, role) {
				candidates = append(candidates, p)
			}
		}
		switch len(candidates) {
		case 0:
			return config.Provider{}, fmt.Errorf("no %s provider configured: run `restorelab provider add`", role)
		case 1:
			return candidates[0], nil
		default:
			return config.Provider{}, fmt.Errorf("several %s providers are configured: pick one with --provider, or set defaults.%s in the config", role, role)
		}
	}

	p, err := cfg.Provider(id)
	if err != nil {
		return config.Provider{}, err
	}
	if !providers.HasRole(*p, role) {
		return config.Provider{}, fmt.Errorf("provider %q does not have the %q role", p.ID, role)
	}
	return *p, nil
}

func (a *app) hypervisor(id string) (core.HypervisorProvider, config.Provider, error) {
	entry, err := a.providerEntry(id, providers.RoleHypervisor)
	if err != nil {
		return nil, config.Provider{}, err
	}
	key, err := a.masterKey()
	if err != nil {
		return nil, entry, err
	}
	p, err := providers.NewHypervisor(entry, key)
	return p, entry, err
}

func (a *app) backups(id string) (core.BackupProvider, config.Provider, error) {
	entry, err := a.providerEntry(id, providers.RoleBackup)
	if err != nil {
		return nil, config.Provider{}, err
	}
	key, err := a.masterKey()
	if err != nil {
		return nil, entry, err
	}
	p, err := providers.NewBackup(entry, key)
	return p, entry, err
}

// hintFor turns the errors users actually hit into an actionable next step.
func hintFor(err error) string {
	switch {
	case errors.Is(err, config.ErrNotFound):
		return "run `restorelab init` to create one"
	case errors.Is(err, crypto.ErrNoKey):
		return "run `restorelab init`, or set RESTORELAB_MASTER_KEY"
	case errors.Is(err, core.ErrUnauthorized):
		return "check the token id and secret, and the permissions documented in docs/proxmox-permissions.md"
	case errors.Is(err, core.ErrNoBackup):
		return "the workload has no restorable backup: check your backup job and the storage the provider is pointed at"
	case errors.Is(err, core.ErrNetworkNotIsolated):
		return "see docs/network-isolation.md for creating a bridge with no uplink"
	case errors.Is(err, core.ErrNotManaged):
		return "RestoreLab only ever destroys workloads it created itself"
	}
	return ""
}
