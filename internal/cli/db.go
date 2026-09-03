package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/store"
)

func newDBCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Inspect and migrate the drill history database",
	}
	cmd.AddCommand(newDBStatusCmd(a), newDBMigrateCmd(a))
	return cmd
}

func newDBStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which database holds the drill history",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Describe reads the configuration without connecting, so this
			// says something useful even when the database is unreachable.
			fmt.Fprintf(a.out, "Configured: %s\n", store.Describe(a.storeConfig()))

			s := a.store(cmd.Context())
			fmt.Fprintf(a.out, "In use:     %s\n", s.Describe())

			runs, err := s.ListRuns(cmd.Context(), store.Filter{Limit: 1})
			if err != nil {
				return fmt.Errorf("could not read the drill history: %w", err)
			}
			if len(runs) == 0 {
				fmt.Fprintln(a.out, "\nNo drill recorded yet.")
				return nil
			}
			fmt.Fprintf(a.out, "\nMost recent drill: %s on %s\n",
				shortID(runs[0].ID), runs[0].StartedAt.Local().Format("2006-01-02 15:04"))
			return nil
		},
	}
}

func newDBMigrateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending schema migrations",
		Long: `Applies pending schema migrations.

The embedded SQLite database migrates itself whenever RestoreLab opens it, so
this command is mostly for a PostgreSQL history, which is deliberately never
migrated as a side effect of running a command, because a shared database may
serve more than this instance.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applied, err := store.Migrate(cmd.Context(), a.storeConfig())
			if err != nil {
				return err
			}
			if len(applied) == 0 {
				fmt.Fprintln(a.out, "Schema is up to date.")
				return nil
			}
			fmt.Fprintf(a.out, "%s applied %d migration(s): %v\n", a.ok(), len(applied), applied)
			return nil
		},
	}
}
