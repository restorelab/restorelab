package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/adhoc"
	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/journal"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/providers"
	"github.com/restorelab/restorelab/internal/recovery"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
)

// runFlags are shared by `recovery test` and `recovery run`.
type runFlags struct {
	providerID string
	backupID   string
	node       string
	storage    string
	pool       string
	network    string
	reportPath string
	dryRun     bool
	keep       bool

	// `recovery test` only: the ad-hoc plan is built from these.
	backup         string
	checkSpecs     []string
	checkRetries   int
	checkInterval  time.Duration
	startupTimeout time.Duration
	rtoTarget      time.Duration
	cpuLimit       int
	memoryLimitMB  int
	skipStartup    bool
}

func (f *runFlags) bind(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&f.providerID, "provider", "", "hypervisor provider to restore on")
	fs.StringVar(&f.backupID, "backup-provider", "", "backup provider to search for restore points")
	fs.StringVar(&f.node, "node", "", "node to restore on (overrides the plan)")
	fs.StringVar(&f.storage, "storage", "", "storage for the restored disks (overrides the plan)")
	fs.StringVar(&f.pool, "pool", "", "resource pool the temporary workload is created in (overrides the plan)")
	fs.StringVar(&f.network, "network", "", "network profile for the temporary workload (overrides the plan)")
	fs.StringVar(&f.reportPath, "report", "", "write the report to a file (.json, .html or .txt by extension)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "resolve the backup and validate the plan without restoring anything")
	fs.BoolVar(&f.keep, "keep", false, "keep the temporary workload instead of destroying it (debugging)")
}

func newRecoveryCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recovery",
		Short: "Run recovery drills",
	}
	cmd.AddCommand(newRecoveryTestCmd(a), newRecoveryRunCmd(a))
	return cmd
}

func newRecoveryTestCmd(a *app) *cobra.Command {
	f := &runFlags{}

	cmd := &cobra.Command{
		Use:   "test <workload-id>",
		Short: "Run a one-off recovery drill on a workload",
		Long: `Restores a workload's latest backup into an isolated environment, boots it,
runs the requested checks, measures the recovery time, and destroys the
temporary workload.

Nothing about the production workload is touched.

Checks are given with --check, repeatable:

    --check ping
    --check tcp:22
    --check http://{{ .ip }}:8080/health
    --check dns:example.com
    --check 'cmd:systemctl is-active postgresql'

A cmd: check runs inside the restored guest through the QEMU guest agent, so
it needs no network route into the isolated recovery network at all. The
interpreter is chosen from the guest's own OS - cmd on Windows, /bin/sh
elsewhere - so the same --check works on either.

With no --check, a TCP check on port 22 is used: it proves the guest booted,
configured its network, and started a service.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve the provider up front: an ad-hoc plan has to name one,
			// and failing here says "no provider configured" instead of
			// "workload.provider is required", which is our vocabulary, not
			// the user's.
			entry, err := a.providerEntry(f.providerID, providers.RoleHypervisor)
			if err != nil {
				return err
			}
			p, err := adhoc.Plan(adhoc.Options{
				WorkloadID:     args[0],
				ProviderID:     entry.ID,
				BackupProvider: f.backupID,
				Backup:         f.backup,
				Node:           f.node,
				Storage:        f.storage,
				Pool:           f.pool,
				Network:        f.network,
				Checks:         f.checkSpecs,
				CheckRetries:   f.checkRetries,
				CheckInterval:  f.checkInterval,
				StartupTimeout: f.startupTimeout,
				RTOTarget:      f.rtoTarget,
				CPULimit:       f.cpuLimit,
				MemoryLimitMB:  f.memoryLimitMB,
				SkipStartup:    f.skipStartup,
			})
			if err != nil {
				return err
			}
			return a.runPlan(cmd.Context(), p, nil, f)
		},
	}

	f.bind(cmd)
	fs := cmd.Flags()
	fs.StringVar(&f.backup, "backup", "latest", `restore point: "latest" or a backup id`)
	fs.StringArrayVar(&f.checkSpecs, "check", nil, "check to run (repeatable): ping, tcp:PORT, http://..., dns:NAME, cmd:COMMAND")
	fs.IntVar(&f.checkRetries, "check-retries", adhoc.DefaultCheckRetries, "how many times to retry a check that has not passed yet")
	fs.DurationVar(&f.checkInterval, "check-interval", adhoc.DefaultCheckInterval, "wait between check attempts")
	fs.DurationVar(&f.startupTimeout, "startup-timeout", plan.DefaultStartupTimeout, "how long to wait for the guest to become reachable")
	fs.DurationVar(&f.rtoTarget, "rto", 0, "recovery time objective the run is graded against")
	fs.IntVar(&f.cpuLimit, "cpu", 0, "cap the temporary workload's cores")
	fs.IntVar(&f.memoryLimitMB, "memory", 0, "cap the temporary workload's memory, in MiB")
	fs.BoolVar(&f.skipStartup, "no-start", false, "restore only: never boot the guest")
	return cmd
}

func newRecoveryRunCmd(a *app) *cobra.Command {
	f := &runFlags{}
	var stored string

	cmd := &cobra.Command{
		Use:   "run [plan.yaml]",
		Short: "Run a recovery drill from a plan file or a stored plan",
		Long: `Runs a recovery drill from a plan.

The plan is either a file on disk, or one stored in the catalogue:

    restorelab recovery run plans/web-tier.yaml
    restorelab recovery run --plan web-tier

A file needs no database at all: a broken history never stops a drill. A
stored plan is read from the catalogue, and the run records which plan and
which version it came from, exactly as a drill triggered over HTTP does.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 1 && stored != "":
				// Never guess. A file and a stored name are two different
				// plans, and picking one silently is how the wrong drill
				// runs against a production cluster.
				return fmt.Errorf("give a plan file or --plan, not both: they are two different plans")
			case len(args) == 1:
				p, err := plan.Load(args[0])
				if err != nil {
					return err
				}
				return a.runPlan(cmd.Context(), p, nil, f)
			case stored != "":
				row, p, err := catalog.Resolve(cmd.Context(), a.store(cmd.Context()), stored)
				if err != nil {
					return planStoreError(stored, err)
				}
				return a.runPlan(cmd.Context(), p, row, f)
			default:
				return fmt.Errorf("give a plan file, or --plan <name> to run a stored one")
			}
		},
	}

	f.bind(cmd)
	cmd.Flags().StringVar(&stored, "plan", "", "run a stored plan by name or id")
	return cmd
}

// runPlan wires the providers, the check registry and the engine together,
// streams progress, and renders the report.
//
// stored is the catalogue row the plan came from, nil for a plan file and for
// `recovery test`. It is provenance only: nothing about the execution changes
// with it, and the run is executed from p either way.
func (a *app) runPlan(ctx context.Context, p *plan.Plan, stored *store.Plan, f *runFlags) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}

	hv, hvEntry, err := a.hypervisor(firstNonEmpty(f.providerID, p.Workload.Provider))
	if err != nil {
		return err
	}
	bp, _, err := a.backups(firstNonEmpty(f.backupID, p.Backup.Provider, hvEntry.ID))
	if err != nil {
		return err
	}

	networkName := firstNonEmpty(f.network, p.Restore.Network, cfg.Defaults.Network, "isolated")
	network, err := cfg.ResolveNetwork(networkName)
	if err != nil {
		return err
	}
	if p.Restore.Bridge != "" {
		network.Bridge = p.Restore.Bridge
	}
	if !network.Isolated {
		// The engine refuses this too; failing here means the user finds out
		// before any provider call is made.
		fmt.Fprintf(a.err, "%s network profile %q is not isolated: the restored workload will be able to reach production\n",
			a.warn(), networkName)
	}

	// The drill is mirrored into the history as it happens. rec never returns
	// an error: a locked database or a full disk must not abort a destructive
	// operation on a production cluster.
	rec := journal.New(a.store(ctx), a.runLogger())
	rec.Prepare(p.Name, hvEntry.ID, p.Workload.ID, p.Workload.Name, planYAML(p))
	if stored != nil {
		// So a drill run from the terminal on a stored plan looks in the
		// history exactly like the same drill triggered over HTTP.
		rec.FromPlan(stored.ID, stored.Version)
	}
	printer := a.progressPrinter()

	engine, err := recovery.New(recovery.Deps{
		Hypervisor: hv,
		Backups:    bp,
		Checks:     checks.Default(),
		// Both consumers see every event: the terminal renders it, the
		// recorder files it. The printer runs first so that writing history
		// never delays what the user is watching.
		//
		// recorder.Emit writes on its own context.Background(), not on ctx,
		// for the same reason the cleanup does: a drill interrupted with
		// Ctrl-C is exactly the one whose trace is worth the most, and
		// plumbing ctx in here would silently stop recording the moment the
		// user cancels.
		//nolint:contextcheck // history writes must outlive cancellation, see above
		Emit: func(e recovery.Event) {
			printer(e)
			rec.Emit(e)
		},
		// The event stream is what the user reads; structured logs are for
		// --verbose and for the future server, not for a terminal timeline.
		Logger: a.runLogger(),
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s %s  %s\n\n",
		a.paint(colorBold, "Recovery drill"),
		p.Name,
		a.paint(colorDim, fmt.Sprintf("workload %s on %s, network %s (%s)",
			p.Workload.ID, hvEntry.ID, networkName, network.Bridge)),
	)

	run, runErr := engine.Run(ctx, p, recovery.RunOptions{
		Network:      network,
		Node:         firstNonEmpty(f.node, p.Restore.Node, cfg.Defaults.Node),
		Storage:      firstNonEmpty(f.storage, p.Restore.Storage, cfg.Defaults.Storage),
		Pool:         firstNonEmpty(f.pool, p.Restore.Pool, hvEntry.Pool),
		DryRun:       f.dryRun,
		KeepWorkload: f.keep,
	})

	// Record the outcome on a context that outlives cancellation, for the same
	// reason the cleanup runs on one: a drill interrupted with Ctrl-C is
	// exactly the one whose trace is worth the most.
	rec.Finish(context.WithoutCancel(ctx), run)

	// The report is written from the run even when the run failed: a failed
	// drill is exactly the case where the report matters most.
	if run != nil {
		fmt.Fprintln(a.out)
		if err := report.Text(a.out, run, report.Options{Color: !a.noColor, ASCII: asciiOnly(), Verbose: a.verbose}); err != nil {
			return err
		}
		if f.reportPath != "" {
			if err := a.writeReport(f.reportPath, run); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "\nReport written to %s\n", f.reportPath)
		}
	}

	return runErr
}

// runLogger keeps the engine's structured logs out of an interactive drill
// unless the user asked for them.
func (a *app) runLogger() *slog.Logger {
	level := slog.LevelWarn
	if a.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(a.err, &slog.HandlerOptions{Level: level}))
}

// progressPrinter renders the engine's event stream as it happens, so a long
// restore is not a silent terminal.
func (a *app) progressPrinter() func(recovery.Event) {
	return func(e recovery.Event) {
		switch {
		case e.Check != nil:
			glyph := a.ok()
			if !e.Check.OK() {
				glyph = a.fail()
			}
			fmt.Fprintf(a.out, "  %s %-24s %s\n", glyph, e.Check.Name, a.paint(colorDim, e.Check.Message))

		case e.Status == core.StepRunning:
			fmt.Fprintf(a.out, "  %s %s\n", a.paint(colorDim, "·"), e.Message)

		case e.Status == core.StepDone:
			fmt.Fprintf(a.out, "  %s %s\n", a.ok(), e.Message)

		case e.Status == core.StepFailed:
			fmt.Fprintf(a.out, "  %s %s\n", a.fail(), firstNonEmpty(e.Message, e.Err))

		case e.Status == core.StepSkipped:
			fmt.Fprintf(a.out, "  %s %s\n", a.paint(colorDim, "-"), e.Message)
		}
	}
}

// writeReport renders the run into a file, choosing the format from the
// extension.
func (a *app) writeReport(path string, run *core.RecoveryRun) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	defer func() { _ = f.Close() }() // safety net; the success path closes explicitly below

	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		err = report.JSON(f, run)
	case ".html", ".htm":
		err = report.HTML(f, run)
	case ".txt", "":
		err = report.Text(f, run, report.Options{Color: false, ASCII: true, Verbose: true})
	default:
		return fmt.Errorf("unsupported report format %q: use .json, .html or .txt", filepath.Ext(path))
	}
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return f.Close()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// planYAML renders the plan as it is at this moment, to be stored alongside
// the run.
//
// Plans become editable once they live in the database, so a run that only
// referenced its plan would let a months-old report describe checks that were
// never performed. An unmarshalable plan is not worth failing a drill over -
// the snapshot is a record, not an input.
func planYAML(p *plan.Plan) string {
	out, err := yaml.Marshal(p)
	if err != nil {
		return ""
	}
	return string(out)
}
