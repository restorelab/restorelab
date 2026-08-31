package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/providers"
	"github.com/restorelab/restorelab/internal/providers/proxmox"
)

// adminPasswordEnv lets automation supply the administrator password without
// putting it in a shell history or a process listing.
const adminPasswordEnv = "RESTORELAB_ADMIN_PASSWORD"

type connectFlags struct {
	id       string
	insecure bool
	caCert   string

	adminUser     string
	adminPassword string
	passwordFile  string

	serviceUser string
	tokenName   string
	role        string
	pool        string
	noPool      bool
	node        string
	storages    []string

	readOnly bool
	dryRun   bool
	yes      bool

	createBridge bool
	bridgeName   string
	noApply      bool
}

func newConnectCmd(a *app) *cobra.Command {
	f := &connectFlags{}

	cmd := &cobra.Command{
		Use:   "connect <endpoint>",
		Short: "Connect a Proxmox cluster, creating RestoreLab's own service account",
		Long: `Connects RestoreLab to a Proxmox VE cluster in one command.

It asks for an administrator's credentials once, uses them in memory to create
a dedicated service account with the minimal privileges RestoreLab needs, and
throws them away. Only the resulting API token is stored, sealed with your
master key.

Start read-only, which is enough for discovery and dry runs:

    restorelab connect https://pve.example.com:8006 --read-only

Then widen it when you are ready to run a real drill:

    restorelab connect https://pve.example.com:8006 --token-name drills-rw

The administrator password can be typed at the prompt, read from a file with
--admin-password-file, or supplied through $` + adminPasswordEnv + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, err := normalizeEndpoint(args[0], proxmoxPort)
			if err != nil {
				return err
			}
			return a.connect(cmd.Context(), endpoint, f)
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&f.id, "id", "proxmox-main", "provider id RestoreLab will store this cluster under")
	fs.BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification")
	fs.StringVar(&f.caCert, "ca-cert", "", "PEM file of the cluster's CA (see /etc/pve/pve-root-ca.pem)")

	fs.StringVar(&f.adminUser, "admin-user", "root@pam", "administrator used once to create the service account")
	fs.StringVar(&f.adminPassword, "admin-password", "", "administrator password (prefer the prompt or --admin-password-file)")
	fs.StringVar(&f.passwordFile, "admin-password-file", "", "read the administrator password from a file, or '-' for stdin")

	fs.StringVar(&f.serviceUser, "user", "restorelab@pve", "service account to create")
	fs.StringVar(&f.tokenName, "token-name", "drills", "API token name to create")
	fs.StringVar(&f.role, "role", "", "role name to create (default: RestoreLabDrill, or RestoreLabRead with --read-only)")
	fs.StringVar(&f.pool, "pool", "restorelab", "resource pool the destructive rights are scoped to")
	fs.BoolVar(&f.noPool, "no-pool", false, "grant destructive rights on all VMs instead of a pool (not recommended)")
	fs.StringVar(&f.node, "node", "", "restrict node access to this node (default: every node)")
	fs.StringArrayVar(&f.storages, "storage", nil, "storage the restores write to (repeatable); needed for a real drill")

	fs.BoolVar(&f.readOnly, "read-only", false, "discovery and --dry-run only: cannot restore, start or destroy anything")
	fs.BoolVar(&f.dryRun, "dry-run", false, "show what would be created, change nothing")
	fs.BoolVarP(&f.yes, "yes", "y", false, "do not ask for confirmation")

	fs.BoolVar(&f.createBridge, "create-bridge", false, "also create the isolated bridge drills restore onto")
	fs.StringVar(&f.bridgeName, "bridge", "", "bridge to create (default: the isolated network profile's bridge)")
	fs.BoolVar(&f.noApply, "no-apply", false, "write the bridge configuration without activating it")

	return cmd
}

func (a *app) connect(ctx context.Context, endpoint string, f *connectFlags) error {
	// connect is often the very first command a user runs, so it must work on
	// a machine with nothing set up yet.
	cfg, key, err := a.ensureInitialised()
	if err != nil {
		return err
	}

	password, err := a.readAdminPassword(f)
	if err != nil {
		return err
	}

	ca, err := readFileIfSet(f.caCert)
	if err != nil {
		return err
	}

	admin, err := proxmox.NewAdminClient(proxmox.AdminConfig{
		Endpoint:           endpoint,
		Username:           f.adminUser,
		Password:           password,
		InsecureSkipVerify: f.insecure,
		CACertPEM:          ca,
		Timeout:            30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer admin.Close()

	if err := admin.Login(ctx); err != nil {
		return err
	}
	version, err := admin.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s connected to Proxmox VE %s as %s\n\n", a.ok(), version, f.adminUser)

	role := f.role
	if role == "" {
		role = "RestoreLabDrill"
		if f.readOnly {
			role = "RestoreLabRead"
		}
	}

	// Re-running connect must be able to reconcile roles and ACLs without
	// destroying a token that already works. Proxmox only ever reveals a
	// secret once, so this is only safe when the configuration already holds
	// that exact token's sealed secret.
	reuse := false
	if existing, err := cfg.Provider(f.id); err == nil {
		if existing.TokenID == f.serviceUser+"!"+f.tokenName && existing.TokenSecret != "" {
			reuse = true
		}
	}

	opts := proxmox.BootstrapOptions{
		UserID:    f.serviceUser,
		Comment:   "RestoreLab recovery drills",
		TokenName: f.tokenName,
		RoleName:  role,
		Pool:      f.pool,
		ReadOnly:  f.readOnly,
		Node:      f.node,
		Storages:  f.storages,
		Bridge:    a.isolatedBridge(cfg, f),
		DryRun:    f.dryRun,

		ReuseExistingToken: reuse,
	}
	if f.noPool || f.readOnly {
		opts.Pool = ""
	}

	a.describeBootstrap(endpoint, opts)
	if !f.dryRun && !f.yes && !a.confirm("Create this in Proxmox?") {
		fmt.Fprintln(a.out, "aborted, nothing was changed")
		return nil
	}

	result, err := admin.Bootstrap(ctx, opts)
	if err != nil {
		return err
	}

	fmt.Fprintln(a.out)
	for _, step := range result.Steps {
		glyph := a.ok()
		if step.Status == "already exists" || step.Status == "skipped" {
			glyph = a.paint(colorDim, "·")
		}
		line := fmt.Sprintf("  %s %-44s %s", glyph, step.Description, a.paint(colorDim, step.Status))
		fmt.Fprintln(a.out, line)
		if step.Detail != "" {
			fmt.Fprintf(a.out, "      %s\n", a.paint(colorYellow, step.Detail))
		}
	}

	if f.dryRun {
		fmt.Fprintf(a.out, "\n%s dry run: nothing was created\n", a.warn())
		return nil
	}

	// Store the provider before verifying: a token that exists in Proxmox but
	// nowhere in the config is the one failure mode we cannot recover from,
	// since PVE reveals a token secret exactly once.
	entry := config.Provider{
		ID:         f.id,
		Kind:       providers.KindProxmox,
		Roles:      []string{providers.RoleHypervisor, providers.RoleBackup},
		Endpoint:   endpoint,
		TokenID:    result.TokenID,
		Insecure:   f.insecure,
		CACertPath: f.caCert,
		Node:       f.node,
		Pool:       opts.Pool,
		TempIDMin:  9000,
		TempIDMax:  9999,
	}
	previousSecret := ""
	if existing, err := cfg.Provider(entry.ID); err == nil {
		previousSecret = existing.TokenSecret
	}

	cfg.Upsert(entry)
	switch {
	case result.Secret != "":
		if err := cfg.SetProviderSecret(entry.ID, result.Secret, key); err != nil {
			return err
		}
	case previousSecret != "":
		// The token was reused: Proxmox gave us no secret because it only
		// ever hands one out at creation. Keep the one already sealed.
		stored, err := cfg.Provider(entry.ID)
		if err != nil {
			return err
		}
		stored.TokenSecret = previousSecret
	default:
		return fmt.Errorf("no token secret available for %s: delete the token in Proxmox and run connect again", entry.TokenID)
	}
	if cfg.Defaults.Provider == "" {
		cfg.Defaults.Provider = entry.ID
	}
	if err := config.Save(a.path(), cfg); err != nil {
		return fmt.Errorf("the token was created in Proxmox but could not be saved: %w", err)
	}
	a.cfg = cfg

	fmt.Fprintf(a.out, "\n%s token %s created and sealed into %s\n", a.ok(), a.paint(colorBold, result.TokenID), a.path())

	// Now prove the token actually works, which is the only thing that makes
	// the whole exercise worth anything.
	hv, _, err := a.hypervisor(entry.ID)
	if err != nil {
		return err
	}
	nodes, err := hv.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("the token was created but cannot be used: %w", err)
	}
	fmt.Fprintf(a.out, "%s token verified: %d node(s) visible\n", a.ok(), len(nodes))

	// The isolated bridge is the last thing standing between a fresh install
	// and a real drill, and this is the only moment RestoreLab holds the
	// privileges to create it: the service token deliberately cannot.
	a.maybeCreateBridge(ctx, admin, cfg, f, hv, nodes)

	a.printConnectNextSteps(f)
	return nil
}

func (a *app) maybeCreateBridge(ctx context.Context, admin *proxmox.AdminClient, cfg *config.Config,
	f *connectFlags, hv core.HypervisorProvider, nodes []core.Node) {

	bridge := f.bridgeName
	if bridge == "" {
		name := cfg.Defaults.Network
		if name == "" {
			name = "isolated"
		}
		profile, err := cfg.Network(name)
		if err != nil || !profile.Isolated {
			return
		}
		bridge = profile.Bridge
	}

	node := f.node
	if node == "" {
		for _, n := range nodes {
			if n.Online {
				node = n.ID
				break
			}
		}
	}
	if node == "" || bridge == "" {
		return
	}

	// Nothing to do when it is already there.
	if validator, ok := hv.(core.NetworkValidator); ok {
		if err := validator.ValidateIsolation(ctx, node, core.NetworkConfig{Bridge: bridge, Isolated: true}); err == nil {
			fmt.Fprintf(a.out, "%s isolated bridge %s already present on %s\n", a.ok(), bridge, node)
			return
		}
	}

	if !f.createBridge {
		fmt.Fprintf(a.out, "\n%s no isolated bridge %q on %s yet: a drill needs one\n", a.warn(), bridge, node)
		if !isTerminal(os.Stdin) || f.yes {
			fmt.Fprintf(a.out, "  %s\n", a.paint(colorCyan, "restorelab network create"))
			return
		}
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"a Linux bridge with no ports, no address and no gateway; applying it reloads the node's networking"))
		if !a.confirm("Create it now?") {
			fmt.Fprintf(a.out, "  %s\n", a.paint(colorCyan, "restorelab network create"))
			return
		}
	}

	result, err := admin.EnsureIsolatedBridge(ctx, proxmox.BridgeOptions{
		Node:   node,
		Bridge: bridge,
		Apply:  !f.noApply,
		DryRun: f.dryRun,
	})
	if err != nil {
		fmt.Fprintf(a.err, "%s could not create the bridge: %v\n", a.fail(), err)
		fmt.Fprintf(a.err, "  %s\n", a.paint(colorDim, "see docs/network-isolation.md to create it by hand"))
		return
	}
	a.printBridgeResult(result, f.dryRun)
}

// ensureInitialised loads the configuration and master key, creating both when
// this is the first command ever run on this machine.
func (a *app) ensureInitialised() (*config.Config, crypto.Key, error) {
	keyPath, created, err := a.ensureMasterKey()
	if err != nil {
		return nil, crypto.Key{}, err
	}
	if created {
		fmt.Fprintf(a.out, "%s master key generated at %s\n", a.ok(), keyPath)
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow, "Back it up: without it, stored provider tokens cannot be decrypted."))
	}
	key, err := a.masterKey()
	if err != nil {
		return nil, crypto.Key{}, err
	}

	cfg, cfgErr := a.config()
	err = cfgErr
	if errors.Is(err, config.ErrNotFound) {
		cfg = config.New()
		if err := config.Save(a.path(), cfg); err != nil {
			return nil, crypto.Key{}, err
		}
		a.cfg = cfg
		fmt.Fprintf(a.out, "%s created %s\n", a.ok(), a.path())
	} else if err != nil {
		return nil, crypto.Key{}, err
	}
	return cfg, key, nil
}

// describeBootstrap prints, in plain words, exactly what is about to be created
// in someone's cluster. Consent needs to be informed to be worth anything.
func (a *app) describeBootstrap(endpoint string, opts proxmox.BootstrapOptions) {
	mode := "full recovery drills"
	if opts.ReadOnly {
		mode = "discovery and dry runs only: it cannot restore, start or destroy anything"
	}

	fmt.Fprintf(a.out, "%s\n", a.paint(colorBold, "RestoreLab will create, on "+endpoint+":"))
	fmt.Fprintf(a.out, "  · a role with the minimal privileges for %s\n", mode)
	if opts.Pool != "" {
		fmt.Fprintf(a.out, "  · a resource pool %q for temporary workloads\n", opts.Pool)
	}
	fmt.Fprintf(a.out, "  · a service account %q\n", opts.UserID)
	fmt.Fprintf(a.out, "  · an API token %q on that account\n", opts.TokenName)

	switch {
	case opts.ReadOnly:
		fmt.Fprintf(a.out, "  · read permissions on VMs, nodes and storages, plus the ability to allocate space,\n")
		fmt.Fprintf(a.out, "    without which Proxmox hides backup volumes from the API entirely\n")
	case opts.Pool != "":
		fmt.Fprintf(a.out, "  · destructive permissions scoped to the pool only, read-only elsewhere\n")
	default:
		fmt.Fprintf(a.out, "  %s destructive permissions on EVERY VM (no pool configured)\n", a.warn())
	}
	fmt.Fprintf(a.out, "\n  %s\n\n", a.paint(colorDim, "Your administrator password is used once, in memory, and never stored."))
}

func (a *app) printConnectNextSteps(f *connectFlags) {
	fmt.Fprintf(a.out, "\n%s\n", a.paint(colorBold, "Next:"))
	fmt.Fprintf(a.out, "  %s\n", a.paint(colorCyan, "restorelab workloads list --backups"))
	if f.readOnly {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorCyan, "restorelab recovery test <vmid> --dry-run"))
		fmt.Fprintf(a.out, "\n%s this token cannot restore, start or destroy anything. Re-run connect without --read-only for a real drill.\n",
			a.paint(colorDim, "note:"))
		return
	}
	fmt.Fprintf(a.out, "  %s\n", a.paint(colorCyan, "restorelab recovery test <vmid> --dry-run"))
	fmt.Fprintf(a.out, "  %s\n", a.paint(colorCyan, "restorelab recovery test <vmid> --check 'cmd:systemctl is-active nginx'"))
	fmt.Fprintf(a.out, "\n%s a real drill needs an isolated bridge on the target node — see docs/network-isolation.md\n",
		a.paint(colorDim, "note:"))
}

// readAdminPassword resolves the administrator password: flag, file, stdin,
// environment, then an interactive prompt with echo disabled.
func (a *app) readAdminPassword(f *connectFlags) (string, error) {
	if f.adminPassword != "" {
		fmt.Fprintf(a.err, "%s the password was passed on the command line; it is now in your shell history\n", a.warn())
		return f.adminPassword, nil
	}

	if f.passwordFile != "" {
		data, err := readFileOrStdin(f.passwordFile)
		if err != nil {
			return "", err
		}
		password := strings.TrimSpace(data)
		if password == "" {
			return "", errors.New("the password file is empty")
		}
		return password, nil
	}

	if password := os.Getenv(adminPasswordEnv); password != "" {
		return password, nil
	}

	if !isTerminal(os.Stdin) {
		return "", fmt.Errorf("no password available: use --admin-password-file, or set $%s", adminPasswordEnv)
	}

	fmt.Fprintf(a.out, "Password for %s: ", f.adminUser)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(a.out)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimSpace(string(raw))
	if password == "" {
		return "", errors.New("no password given")
	}
	return password, nil
}

func readFileIfSet(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// readFileOrStdin reads a file, or standard input when path is "-".
func readFileOrStdin(path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// isolatedBridge resolves the bridge a drill restores onto, so the bootstrap
// can grant the service account the right to attach a workload to it - and to
// nothing else.
func (a *app) isolatedBridge(cfg *config.Config, f *connectFlags) string {
	if f.bridgeName != "" {
		return f.bridgeName
	}
	name := cfg.Defaults.Network
	if name == "" {
		name = "isolated"
	}
	profile, err := cfg.Network(name)
	if err != nil || !profile.Isolated {
		return ""
	}
	return profile.Bridge
}
