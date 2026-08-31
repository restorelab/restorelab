package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/core"
)

func newWorkloadsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workloads",
		Aliases: []string{"workload", "vms"},
		Short:   "Inspect the workloads a provider knows about",
	}
	cmd.AddCommand(newWorkloadsListCmd(a), newWorkloadsShowCmd(a))
	return cmd
}

func newWorkloadsListCmd(a *app) *cobra.Command {
	var (
		providerID  string
		withBackups bool
		showTemp    bool
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List workloads that can be recovery-tested",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			hv, entry, err := a.hypervisor(providerID)
			if err != nil {
				return err
			}
			workloads, err := hv.ListWorkloads(ctx)
			if err != nil {
				return err
			}

			// RestoreLab's own temporary workloads are noise here: they are an
			// artefact of a drill, not something you would drill.
			filtered := workloads[:0]
			for _, w := range workloads {
				if w.Template {
					continue
				}
				if w.Managed && !showTemp {
					continue
				}
				filtered = append(filtered, w)
			}
			workloads = filtered

			sortWorkloads(workloads)

			if len(workloads) == 0 {
				fmt.Fprintf(a.out, "No workloads found on %s\n", entry.ID)
				return nil
			}

			header := []string{"ID", "NAME", "KIND", "NODE", "STATE", "CPU", "MEMORY"}
			if withBackups {
				header = append(header, "LATEST BACKUP")
			}
			t := a.table(a.out, header...)

			var backupsFor func(string) string
			if withBackups {
				bp, _, err := a.backups(providerID)
				if err != nil {
					return err
				}
				backupsFor = func(id string) string {
					b, err := bp.GetLatestBackup(ctx, id)
					switch {
					case errors.Is(err, core.ErrNoBackup):
						return a.paint(colorRed, "none")
					case err != nil:
						return a.paint(colorYellow, "unknown")
					default:
						return fmt.Sprintf("%s (%s)", b.CreatedAt.Local().Format("2006-01-02 15:04"), humanAge(b.Age()))
					}
				}
			}

			for _, w := range workloads {
				row := []string{
					w.ID,
					w.Name,
					string(w.Kind),
					w.Node,
					a.powerState(w.PowerState),
					strconv.Itoa(w.CPUCores),
					humanBytes(w.MemoryBytes),
				}
				if withBackups {
					row = append(row, backupsFor(w.ID))
				}
				t.row(row...)
			}
			t.flush()
			return nil
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "provider to query (default: the configured default)")
	cmd.Flags().BoolVar(&withBackups, "backups", false, "look up the latest backup of each workload (one API call per workload)")
	cmd.Flags().BoolVar(&showTemp, "show-temporary", false, "include workloads created by RestoreLab")
	return cmd
}

func newWorkloadsShowCmd(a *app) *cobra.Command {
	var providerID string

	cmd := &cobra.Command{
		Use:   "show <workload-id>",
		Short: "Show a workload, its status and its backups",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			id := args[0]

			hv, _, err := a.hypervisor(providerID)
			if err != nil {
				return err
			}
			w, err := hv.GetWorkload(ctx, id)
			if err != nil {
				return err
			}
			status, statusErr := hv.GetStatus(ctx, id)

			fmt.Fprintf(a.out, "%s  %s\n\n", a.paint(colorBold, w.Name), a.paint(colorDim, "("+w.ID+")"))

			t := a.table(a.out)
			t.row("Kind", string(w.Kind))
			t.row("Node", w.Node)
			t.row("State", a.powerState(w.PowerState))
			t.row("CPU", strconv.Itoa(w.CPUCores)+" cores")
			t.row("Memory", humanBytes(w.MemoryBytes))
			if w.DiskBytes > 0 {
				t.row("Disk", humanBytes(w.DiskBytes))
			}
			if len(w.Tags) > 0 {
				t.row("Tags", strings.Join(w.Tags, ", "))
			}
			if statusErr == nil && status != nil {
				if ip := status.PrimaryIP(); ip != "" {
					t.row("IP", strings.Join(status.IPs, ", "))
				}
				if status.Uptime > 0 {
					t.row("Uptime", humanAge(status.Uptime))
				}
				t.row("Guest agent", boolWord(status.AgentReady, "responding", "not available"))
			}
			t.flush()

			bp, _, err := a.backups(providerID)
			if err != nil {
				fmt.Fprintf(a.err, "\n%s no backup provider available: %v\n", a.warn(), err)
				return nil
			}
			backups, err := bp.ListBackups(ctx, id)
			if err != nil {
				fmt.Fprintf(a.err, "\n%s could not list backups: %v\n", a.warn(), err)
				return nil
			}
			if len(backups) == 0 {
				fmt.Fprintf(a.out, "\n%s %s\n", a.fail(), a.paint(colorRed, "no backups found for this workload"))
				return nil
			}

			fmt.Fprintf(a.out, "\n%s\n", a.paint(colorBold, "Backups"))
			bt := a.table(a.out, "CREATED", "AGE", "SIZE", "VERIFIED", "ID")
			for i, b := range backups {
				if i >= 10 {
					fmt.Fprintf(a.out, "  ... and %d more\n", len(backups)-10)
					break
				}
				bt.row(
					b.CreatedAt.Local().Format("2006-01-02 15:04:05"),
					humanAge(b.Age()),
					humanBytes(b.SizeBytes),
					a.verification(b.Verified),
					b.ID,
				)
			}
			bt.flush()
			return nil
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "provider to query (default: the configured default)")
	return cmd
}

func newBackupsCmd(a *app) *cobra.Command {
	var providerID string

	cmd := &cobra.Command{
		Use:     "backups <workload-id>",
		Aliases: []string{"backup"},
		Short:   "List the restore points available for a workload",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bp, entry, err := a.backups(providerID)
			if err != nil {
				return err
			}
			backups, err := bp.ListBackups(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(backups) == 0 {
				return fmt.Errorf("no backups for workload %s on %s: %w", args[0], entry.ID, core.ErrNoBackup)
			}

			t := a.table(a.out, "CREATED", "AGE", "SIZE", "VERIFIED", "PROTECTED", "ID")
			for _, b := range backups {
				t.row(
					b.CreatedAt.Local().Format("2006-01-02 15:04:05"),
					humanAge(b.Age()),
					humanBytes(b.SizeBytes),
					a.verification(b.Verified),
					boolWord(b.Protected, "yes", ""),
					b.ID,
				)
			}
			t.flush()
			return nil
		},
	}

	cmd.Flags().StringVar(&providerID, "provider", "", "backup provider to query (default: the configured default)")
	return cmd
}

// --- formatting helpers ------------------------------------------------------

func (a *app) powerState(s core.PowerState) string {
	switch s {
	case core.PowerStateRunning:
		return a.paint(colorGreen, string(s))
	case core.PowerStateStopped:
		return a.paint(colorDim, string(s))
	default:
		return a.paint(colorYellow, string(s))
	}
}

func (a *app) verification(v core.VerificationState) string {
	switch v {
	case core.VerificationOK:
		return a.paint(colorGreen, "ok")
	case core.VerificationFailed:
		return a.paint(colorRed, "failed")
	case core.VerificationNone:
		return a.paint(colorDim, "-")
	default:
		return a.paint(colorDim, "?")
	}
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// humanAge renders a duration the way an operator reads it at a glance.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// sortWorkloads orders by numeric ID when possible: an admin thinks of VM 9 as
// coming before VM 101, which a plain string sort gets wrong.
func sortWorkloads(ws []core.Workload) {
	sort.SliceStable(ws, func(i, j int) bool {
		ni, erri := strconv.Atoi(ws[i].ID)
		nj, errj := strconv.Atoi(ws[j].ID)
		if erri == nil && errj == nil {
			return ni < nj
		}
		return ws[i].ID < ws[j].ID
	})
}
