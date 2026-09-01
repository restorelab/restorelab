package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/store"
)

func newTokenCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the API tokens `restorelab serve` accepts",
		Long: `Manages the tokens the HTTP API accepts.

A token is shown once, when it is created, and stored only as a hash: there
is no command that can print it again. Lose it and create another one.`,
	}
	cmd.AddCommand(newTokenCreateCmd(a), newTokenListCmd(a), newTokenRevokeCmd(a))
	return cmd
}

func newTokenCreateCmd(a *app) *cobra.Command {
	var operate bool

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API token and print it once",
		Long: `Creates an API token and prints it once.

A token is read only unless --operate is given. An operate token can trigger
drills, cancel them and destroy the workloads they leave behind: it is a key
that can destroy and recreate machines, not a key that reads a dashboard.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == "" {
				return fmt.Errorf("a token needs a name: it is how you revoke it later")
			}

			secret, record, err := api.NewToken(name, time.Now())
			if err != nil {
				return err
			}
			// Read is always present, including on an operate token: an
			// operator that could trigger a drill but not watch it would be a
			// strange thing to hand anyone.
			record.Scopes = []string{store.ScopeRead}
			if operate {
				record.Scopes = append(record.Scopes, store.ScopeOperate)
			}

			if err := a.store(cmd.Context()).CreateToken(cmd.Context(), record); err != nil {
				if errors.Is(err, store.ErrNoHistory) {
					return fmt.Errorf("the API needs a history database to keep its tokens in, and this RestoreLab has none: see `restorelab db status`")
				}
				return fmt.Errorf("could not store the token: %w", err)
			}

			fmt.Fprintf(a.out, "%s token %q created\n\n", a.ok(), name)
			fmt.Fprintf(a.out, "  %s\n\n", secret)

			// What it can do, said at the only moment the operator is still
			// looking: a key that destroys and recreates machines must not be
			// handed out looking like a key that reads a dashboard.
			if operate {
				fmt.Fprintf(a.out, "  scopes: %s\n", strings.Join(record.Scopes, ", "))
				fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
					"This token can trigger drills, cancel them, and clean up the workloads they leave."))
				fmt.Fprintf(a.out, "  %s\n\n", a.paint(colorYellow,
					"It destroys and recreates machines. Treat it like a hypervisor credential."))
			} else {
				fmt.Fprintf(a.out, "  scopes: %s\n", strings.Join(record.Scopes, ", "))
				fmt.Fprintf(a.out, "  %s\n\n", a.paint(colorDim,
					"Read only: it can look, and change nothing. Add --operate for a token that runs drills."))
			}

			fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
				"This is the only time the secret will be shown. Store it now."))
			fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
				"Use it as: Authorization: Bearer "+secret[:6]+"..."))
			return nil
		},
	}

	cmd.Flags().BoolVar(&operate, "operate", false,
		"allow this token to trigger, cancel and clean up drills (default: read only)")
	return cmd
}

func newTokenListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List API tokens",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tokens, err := a.store(cmd.Context()).ListTokens(cmd.Context())
			if err != nil {
				if errors.Is(err, store.ErrNoHistory) {
					return fmt.Errorf("no history database, so no tokens: see `restorelab db status`")
				}
				return err
			}
			if len(tokens) == 0 {
				fmt.Fprintln(a.out, "No API token yet. Create one with `restorelab token create <name>`.")
				return nil
			}

			// SCOPES is in the table rather than behind a flag: which token
			// can destroy machines is the first thing anyone auditing this
			// list is looking for.
			t := a.table(a.out, "NAME", "SCOPES", "CREATED", "LAST USED", "STATE")
			for _, tok := range tokens {
				lastUsed := "never"
				if !tok.LastUsedAt.IsZero() {
					lastUsed = tok.LastUsedAt.Local().Format("2006-01-02 15:04")
				}
				state := "active"
				if !tok.Live() {
					state = "revoked " + tok.RevokedAt.Local().Format("2006-01-02")
				}
				scopes := strings.Join(tok.Scopes, ",")
				if scopes == "" {
					// A token recorded before scopes existed reads back with
					// none, and means read only. Saying so beats a blank.
					scopes = store.ScopeRead
				}
				t.row(tok.Name, scopes, tok.CreatedAt.Local().Format("2006-01-02 15:04"), lastUsed, state)
			}
			t.flush()
			return nil
		},
	}
}

func newTokenRevokeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke an API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := a.store(cmd.Context()).RevokeToken(cmd.Context(), args[0], time.Now())
			switch {
			case errors.Is(err, store.ErrNotFound):
				return fmt.Errorf("no active token named %q (see `restorelab token list`)", args[0])
			case errors.Is(err, store.ErrNoHistory):
				return fmt.Errorf("no history database, so no tokens: see `restorelab db status`")
			case err != nil:
				return err
			}
			fmt.Fprintf(a.out, "%s token %q revoked; requests carrying it are refused from now on\n",
				a.ok(), args[0])
			return nil
		},
	}
}
