// Package cli implements the restorelab command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/providers"
	"github.com/restorelab/restorelab/internal/store"
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
	rawAPI     bool

	out io.Writer
	err io.Writer

	// lazyMu guards the lazily loaded configuration and master key.
	//
	// Until phase B2 an app was driven by exactly one goroutine: a command
	// ran, it finished. `serve` broke that - the HTTP handlers and the worker
	// both reach for a provider, and building one unseals a secret with the
	// master key. Today every concurrent path happens to funnel through
	// cliProviders' own mutex, which makes this safe by accident; a mutex
	// here makes it safe by construction, which is what it needs to be when
	// the race detector cannot run on this machine.
	lazyMu sync.Mutex
	cfg    *config.Config
	key    crypto.Key
	keySet bool

	storeOnce  sync.Once
	storeValue store.Store
}

// Execute runs the root command. It returns the process exit code.
func Execute(ctx context.Context) int {
	a := &app{out: os.Stdout, err: os.Stderr}
	// The command tree is built before the run context exists, and cobra is
	// what delivers it: ExecuteContext below stores ctx on the root command,
	// and every RunE reads it back with cmd.Context() and plumbs it down.
	// contextcheck cannot see through that indirection and asks for ctx to be
	// threaded through the constructors instead; doing so would mean every
	// newXxxCmd carrying a context it must not capture, which is the bug
	// cobra's design avoids.
	//nolint:contextcheck // cobra delivers ctx at execution time, see above
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
	f.StringVar(&a.keyPath, "master-key-file", "", "path to the master key file (default: ~/.restorelab/master.key; RESTORELAB_MASTER_KEY holds the key itself and wins over this)")
	f.BoolVar(&a.noColor, "no-color", false, "disable coloured output")
	f.BoolVarP(&a.verbose, "verbose", "v", false, "verbose output")

	cmd.SetOut(a.out)
	cmd.SetErr(a.err)

	// version.String() already begins with the program name, and cobra's
	// default template prefixes "<use> version " to whatever Version holds -
	// so `--version` answered "restorelab version restorelab v0.1.0 (...)".
	// It shipped that way in v0.1.0, in the first command anyone runs on a
	// fresh install. The flag now prints exactly what `version` prints.
	cmd.SetVersionTemplate("{{.Version}}\n")

	cmd.AddCommand(
		newInitCmd(a),
		newConnectCmd(a),
		newDoctorCmd(a),
		newNetworkCmd(a),
		newKeyCmd(a),
		newProviderCmd(a),
		newWorkloadsCmd(a),
		newBackupsCmd(a),
		newRecoveryCmd(a),
		newPlanCmd(a),
		newCleanupCmd(a),
		newRunsCmd(a),
		newDBCmd(a),
		newServeCmd(a),
		newTokenCmd(a),
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
	a.lazyMu.Lock()
	defer a.lazyMu.Unlock()
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

// forget drops the lazily loaded configuration and master key.
//
// `serve` runs the first-run wizard in the same process that serves
// afterwards, and by then it has cached the absence of a configuration. A
// restart that kept those caches would build the real server against nothing,
// which is a restart in name only.
func (a *app) forget() {
	a.lazyMu.Lock()
	defer a.lazyMu.Unlock()
	a.cfg = nil
	a.keySet = false
}

// masterKey resolves and caches the master key.
func (a *app) masterKey() (crypto.Key, error) {
	a.lazyMu.Lock()
	defer a.lazyMu.Unlock()
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

// storeConfig says where the drill history lives: the configured URL if there
// is one, the embedded file next to the config otherwise.
func (a *app) storeConfig() store.Config {
	cfg := store.Config{DefaultPath: a.defaultHistoryPath()}
	if url := os.Getenv("RESTORELAB_DATABASE_URL"); url != "" {
		cfg.URL = url
		return cfg
	}
	if loaded, err := a.config(); err == nil {
		cfg.URL = loaded.Database.URL
	}
	return cfg
}

// defaultHistoryPath is the embedded database, next to the config file and
// the master key.
func (a *app) defaultHistoryPath() string {
	return filepath.Join(filepath.Dir(a.path()), "history.db")
}

// store opens the drill history once per process.
//
// Any failure is reported once and answered with store.Noop. History is a
// convenience; nothing here may stop a drill, so this method has no error to
// return and callers have no branch to write.
func (a *app) store(ctx context.Context) store.Store {
	a.storeOnce.Do(func() {
		a.storeValue = store.Noop{}

		s, err := store.Open(ctx, a.storeConfig())
		if err != nil {
			fmt.Fprintf(a.err, "%s drill history is not being recorded: %v\n", a.warn(), err)
			return
		}
		a.storeValue = s
	})
	return a.storeValue
}
