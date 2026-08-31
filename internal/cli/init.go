package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/crypto"
)

func newInitCmd(a *app) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the configuration file and the master key",
		Long: `Creates ~/.restorelab/config.yaml with a starter isolated network profile,
and generates the master key used to seal provider secrets.

The master key is never written into the configuration file. Back it up
separately: losing it means every stored provider token must be re-entered.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := a.path()

			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}

			keyPath, created, err := a.ensureMasterKey()
			if err != nil {
				return err
			}

			cfg := config.New()
			if err := config.Save(path, cfg); err != nil {
				return err
			}

			fmt.Fprintf(a.out, "%s configuration written to %s\n", a.ok(), path)
			if created {
				fmt.Fprintf(a.out, "%s master key generated at %s\n", a.ok(), keyPath)
				fmt.Fprintf(a.out, "\n%s\n", a.paint(colorYellow, "Back this key up. Without it, stored provider tokens cannot be decrypted."))
			} else {
				fmt.Fprintf(a.out, "%s using the existing master key (%s)\n", a.ok(), keyPath)
			}

			fmt.Fprintf(a.out, `
Next steps:

  1. Create an isolated bridge on your Proxmox nodes (see docs/network-isolation.md)
  2. Create a dedicated API token          (see docs/proxmox-permissions.md)
  3. %s
  4. %s
`,
				a.paint(colorCyan, "restorelab provider add proxmox --id proxmox-main --endpoint https://pve:8006 --token-id 'restorelab@pve!drills' --token-secret ..."),
				a.paint(colorCyan, "restorelab workloads list"),
			)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing configuration file")
	return cmd
}

// ensureMasterKey returns the path of the master key, generating one when none
// exists. It never overwrites an existing key: that would orphan every sealed
// secret already on disk.
func (a *app) ensureMasterKey() (path string, created bool, err error) {
	if _, source, err := crypto.LoadKey(a.keyPath); err == nil {
		return source, false, nil
	} else if !errors.Is(err, crypto.ErrNoKey) {
		return "", false, err
	}

	path = a.keyPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, fmt.Errorf("locate home directory: %w", err)
		}
		path = filepath.Join(home, ".restorelab", "master.key")
	}

	key, err := crypto.NewKey()
	if err != nil {
		return "", false, err
	}
	defer key.Wipe()

	if err := crypto.SaveKey(path, key); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func newKeyCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage the master key used to seal provider secrets",
	}

	generate := &cobra.Command{
		Use:   "generate",
		Short: "Print a new master key without storing it",
		Long: `Prints a fresh 32-byte master key, base64 encoded, and stores nothing.

Use it to deploy RestoreLab in a container or a CI job:

    export RESTORELAB_MASTER_KEY=$(restorelab key generate)

Secrets sealed under one key cannot be opened with another.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			key, err := crypto.NewKey()
			if err != nil {
				return err
			}
			defer key.Wipe()
			fmt.Fprintln(a.out, crypto.Encode(key))
			return nil
		},
	}

	show := &cobra.Command{
		Use:   "path",
		Short: "Show where the master key is loaded from",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			key, source, err := crypto.LoadKey(a.keyPath)
			if err != nil {
				return err
			}
			key.Wipe()
			fmt.Fprintln(a.out, source)
			return nil
		},
	}

	cmd.AddCommand(generate, show)
	return cmd
}
