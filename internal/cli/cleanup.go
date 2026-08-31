package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/core"
)

func newCleanupCmd(a *app) *cobra.Command {
	var (
		providerID string
		all        bool
		yes        bool
	)

	cmd := &cobra.Command{
		Use:   "cleanup [workload-id]",
		Short: "Destroy temporary workloads left behind by a drill",
		Long: `Destroys a temporary workload RestoreLab created, for example after a run
that was interrupted or that used --keep.

Only workloads carrying RestoreLab's ownership metadata can be destroyed: the
provider refuses anything else, so this command cannot touch production.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			hv, entry, err := a.hypervisor(providerID)
			if err != nil {
				return err
			}

			if len(args) == 1 {
				if err := hv.Delete(ctx, args[0]); err != nil {
					return err
				}
				fmt.Fprintf(a.out, "%s workload %s destroyed\n", a.ok(), args[0])
				return nil
			}

			if !all {
				return errors.New("give a workload id, or --all to destroy every temporary workload")
			}

			workloads, err := hv.ListWorkloads(ctx)
			if err != nil {
				return err
			}
			var orphans []core.Workload
			for _, w := range workloads {
				if w.Managed {
					orphans = append(orphans, w)
				}
			}
			if len(orphans) == 0 {
				fmt.Fprintf(a.out, "%s no temporary workloads on %s\n", a.ok(), entry.ID)
				return nil
			}

			fmt.Fprintf(a.out, "%d temporary workload(s) on %s:\n", len(orphans), entry.ID)
			for _, w := range orphans {
				fmt.Fprintf(a.out, "  %s  %s  (node %s, %s)\n", w.ID, w.Name, w.Node, w.PowerState)
			}

			if !yes && !a.confirm("Destroy them all?") {
				fmt.Fprintln(a.out, "aborted")
				return nil
			}

			var failures int
			for _, w := range orphans {
				if err := hv.Delete(ctx, w.ID); err != nil {
					failures++
					fmt.Fprintf(a.out, "%s %s: %v\n", a.fail(), w.ID, err)
					continue
				}
				fmt.Fprintf(a.out, "%s %s destroyed\n", a.ok(), w.ID)
			}
			if failures > 0 {
				return fmt.Errorf("%d of %d workloads could not be destroyed", failures, len(orphans))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "provider to clean up")
	cmd.Flags().BoolVar(&all, "all", false, "destroy every temporary workload RestoreLab created")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// confirm asks for an explicit yes on stdin. A non-interactive stdin answers
// no: a destructive default must never be "go ahead".
func (a *app) confirm(question string) bool {
	if !isTerminal(os.Stdin) {
		return false
	}
	fmt.Fprintf(a.out, "%s [y/N] ", question)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
