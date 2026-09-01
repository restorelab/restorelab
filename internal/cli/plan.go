package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

func newPlanCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Manage the stored recovery plans",
		Long: `Manages the plans RestoreLab keeps in its database.

A stored plan is what the API triggers by name and what the scheduler will
reference. A plan file on disk still runs directly with
` + "`restorelab recovery run <file>`" + `: storing one is how it becomes
something other machines can name, not a condition for running it.

Every command here needs the history database except ` + "`plan validate`" + `,
which only reads files.`,
	}
	cmd.AddCommand(
		newPlanListCmd(a), newPlanShowCmd(a), newPlanApplyCmd(a),
		newPlanDeleteCmd(a), newPlanValidateCmd(a),
	)
	return cmd
}

func newPlanApplyCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "apply <file.yaml>...",
		Short: "Store a plan file, creating it or updating it by name",
		Long: `Stores plan files, creating each one or replacing the plan that already
carries its name.

The name inside the document is the identity, so re-applying an edited file
updates the plan rather than adding a second one. Several files in one call:
a directory of plans under version control is the normal case.

    restorelab plan apply plans/*.yaml

A document that does not validate is refused and nothing is written for it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := a.store(cmd.Context())
			for _, path := range args {
				document, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}

				saved, created, err := catalog.Save(cmd.Context(), s, document, 0)
				switch {
				case errors.Is(err, store.ErrNoHistory):
					// Not the file's fault, so it is not prefixed with one:
					// naming the path here would send someone editing YAML
					// over a missing database.
					return planStoreError(path, err)
				case err != nil:
					return fmt.Errorf("%s: %w", path, err)
				}

				// Which of the two happened is the whole answer: applying a
				// file expecting a new plan and silently overwriting someone
				// else's is exactly what this line prevents.
				if created {
					fmt.Fprintf(a.out, "%s created %s\n", a.ok(), saved.Name)
				} else {
					fmt.Fprintf(a.out, "%s updated %s to v%d\n", a.ok(), saved.Name, saved.Version)
				}
			}
			return nil
		},
	}
}

func newPlanListCmd(a *app) *cobra.Command {
	var workload string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the stored plans",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plans, err := catalog.List(cmd.Context(), a.store(cmd.Context()),
				store.PlanFilter{WorkloadID: workload})
			if err != nil {
				return planStoreError("", err)
			}
			if len(plans) == 0 {
				fmt.Fprintln(a.out, "No plan is stored yet. Store one with `restorelab plan apply <file.yaml>`.")
				return nil
			}

			t := a.table(a.out, "NAME", "WORKLOAD", "VERSION", "UPDATED")
			for _, p := range plans {
				t.row(p.Name, p.WorkloadID, fmt.Sprintf("v%d", p.Version),
					p.UpdatedAt.Local().Format("2006-01-02 15:04"))
			}
			t.flush()
			return nil
		},
	}

	cmd.Flags().StringVar(&workload, "workload", "", "only the plans for this workload")
	return cmd
}

func newPlanShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name|id>",
		Short: "Print a stored plan's document",
		Long: `Prints a stored plan's document exactly as it was applied, comments and key
order included.

The output is the file back: piping it to disk and re-applying it is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := catalog.Get(cmd.Context(), a.store(cmd.Context()), args[0])
			if err != nil {
				return planStoreError(args[0], err)
			}
			// Fprint, not Fprintln: the stored document already ends the way
			// its author ended it, and adding a newline would mean `plan
			// show` no longer gives back the bytes that went in.
			fmt.Fprint(a.out, p.YAML)
			return nil
		},
	}
}

func newPlanDeleteCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name|id>",
		Aliases: []string{"rm"},
		Short:   "Remove a stored plan",
		Long: `Removes a stored plan.

The runs it produced keep their name and the copy of the plan they actually
executed, so their reports and the confidence score are unchanged. Only the
link disappears.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The name comes back from the deletion rather than being echoed
			// from the argument: a reference can be an id prefix, and
			// "abcd1234 removed" tells an operator nothing about what they
			// just lost.
			name, err := catalog.Delete(cmd.Context(), a.store(cmd.Context()), args[0])
			if err != nil {
				return planStoreError(args[0], err)
			}
			fmt.Fprintf(a.out, "%s %s removed; the drills it produced keep their reports\n",
				a.ok(), name)
			return nil
		},
	}
}

// newPlanValidateCmd is the only plan command that needs no database. It is
// what a CI runs before it applies anything, on a machine that has no
// RestoreLab configuration at all - which is why it must never so much as
// ask for the store.
func newPlanValidateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file.yaml>...",
		Short: "Check that plan files parse and validate, without storing them",
		Long: `Checks that plan files parse and validate, and stores nothing.

It needs no database and no configuration, so it runs in CI on a checkout:

    restorelab plan validate plans/*.yaml

The first file that does not validate stops the command.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			for _, path := range args {
				p, err := plan.Load(path)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				fmt.Fprintf(a.out, "%s %s: %s\n", a.ok(), path, p.Name)
			}
			return nil
		},
	}
}

// planStoreError turns the store's sentinels into something an operator can
// act on, the way `runs show` does for a run id.
//
// ref is what the user typed, empty when the command named nothing (a
// listing). The wrapped error is kept so errors.Is still works up the stack.
func planStoreError(ref string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("no stored plan matches %q (try `restorelab plan list`): %w", ref, err)
	case errors.Is(err, store.ErrAmbiguous):
		return fmt.Errorf("%q matches more than one stored plan: give a few more characters: %w", ref, err)
	case errors.Is(err, store.ErrNoHistory):
		// The catalogue lives in the database and nowhere else, so unlike a
		// drill - which runs happily with no history at all - there is no
		// degraded mode to fall back to here.
		return fmt.Errorf("stored plans need a history database, and this RestoreLab has none: see `restorelab db status`: %w", err)
	}
	return err
}
