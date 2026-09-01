package cli

import (
	"errors"
	"fmt"
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
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API token and print it once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == "" {
				return fmt.Errorf("a token needs a name: it is how you revoke it later")
			}

			secret, record, err := api.NewToken(name, time.Now())
			if err != nil {
				return err
			}

			if err := a.store(cmd.Context()).CreateToken(cmd.Context(), record); err != nil {
				if errors.Is(err, store.ErrNoHistory) {
					return fmt.Errorf("the API needs a history database to keep its tokens in, and this RestoreLab has none: see `restorelab db status`")
				}
				return fmt.Errorf("could not store the token: %w", err)
			}

			fmt.Fprintf(a.out, "%s token %q created\n\n", a.ok(), name)
			fmt.Fprintf(a.out, "  %s\n\n", secret)
			fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
				"This is the only time it will be shown. Store it now."))
			fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
				"Use it as: Authorization: Bearer "+secret[:6]+"..."))
			return nil
		},
	}
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

			t := a.table(a.out, "NAME", "CREATED", "LAST USED", "STATE")
			for _, tok := range tokens {
				lastUsed := "never"
				if !tok.LastUsedAt.IsZero() {
					lastUsed = tok.LastUsedAt.Local().Format("2006-01-02 15:04")
				}
				state := "active"
				if !tok.Live() {
					state = "revoked " + tok.RevokedAt.Local().Format("2006-01-02")
				}
				t.row(tok.Name, tok.CreatedAt.Local().Format("2006-01-02 15:04"), lastUsed, state)
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
