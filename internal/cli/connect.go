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

	fs.BoolVar(&f.readOnly, "read-only", false, "create a read-only setup: discovery and --dry-run only, nothing destructive")
	fs.BoolVar(&f.dryRun, "dry-run", false, "show what would be created, change nothing")
	fs.BoolVarP(&f.yes, "yes", "y", false, "do not ask for confirmation")

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

	opts := proxmox.BootstrapOptions{
		UserID:    f.serviceUser,
		Comment:   "RestoreLab recovery drills",
		TokenName: f.tokenName,
		RoleName:  role,
		Pool:      f.pool,
		ReadOnly:  f.readOnly,
		Node:      f.node,
		Storages:  f.storages,
		DryRun:    f.dryRun,
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
	cfg.Upsert(entry)
	if err := cfg.SetProviderSecret(entry.ID, result.Secret, key); err != nil {
		return err
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

	a.printConnectNextSteps(f)
	return nil
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
		mode = "read-only (discovery and dry runs only)"
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
		fmt.Fprintf(a.out, "  · read-only permissions on VMs, nodes and storages\n")
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
		fmt.Fprintf(a.out, "\n%s this token is read-only. Re-run connect without --read-only when you want a real drill.\n",
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
