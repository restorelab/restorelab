package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
)

// shortIDLen is how much of a run id a listing shows. Eight characters is
// what git settled on for a short sha, and it is short enough to retype.
const shortIDLen = 8

func newRunsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "Inspect the history of past recovery drills",
		Long: `Inspects the drills RestoreLab has recorded.

History is kept automatically in ~/.restorelab/history.db. Nothing needs to be
installed or configured.`,
	}
	cmd.AddCommand(newRunsListCmd(a), newRunsShowCmd(a))
	return cmd
}

func newRunsListCmd(a *app) *cobra.Command {
	var (
		workload string
		state    string
		result   string
		since    string
		limit    int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List past recovery drills, most recent first",
		Long: `Lists past recovery drills, most recent first.

    restorelab runs list
    restorelab runs list --workload 110 --since 30d
    restorelab runs list --result FAILED`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := store.Filter{
				WorkloadID: workload,
				State:      core.RunState(strings.ToUpper(state)),
				Result:     core.RunResult(strings.ToUpper(result)),
				Limit:      limit,
			}
			if since != "" {
				parsed, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				f.Since = parsed
			}

			runs, err := a.store(cmd.Context()).ListRuns(cmd.Context(), f)
			if err != nil {
				return fmt.Errorf("could not read the drill history: %w", err)
			}
			renderRunList(a.out, runs)
			return nil
		},
	}

	cmd.Flags().StringVar(&workload, "workload", "", "only drills of this workload")
	cmd.Flags().StringVar(&state, "state", "", "only drills in this state (SUCCESS, FAILED, ...)")
	cmd.Flags().StringVar(&result, "result", "", "only drills with this verdict (SUCCESS, DEGRADED, FAILED)")
	cmd.Flags().StringVar(&since, "since", "", "only drills started since then: 30d, 12h, or 2026-08-01")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many to show (default 50)")
	return cmd
}

func newRunsShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show one recorded drill in full",
		Long: `Shows a recorded drill: its timeline, its checks and its RTO.

The id may be shortened, the way git accepts a short sha:

    restorelab runs show 0aca8405`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := a.store(cmd.Context()).GetRun(cmd.Context(), args[0])
			switch {
			case errors.Is(err, store.ErrNotFound):
				return fmt.Errorf("no recorded drill matches %q (try `restorelab runs list`)", args[0])
			case errors.Is(err, store.ErrAmbiguous):
				return fmt.Errorf("%q matches more than one drill: give a few more characters", args[0])
			case err != nil:
				return fmt.Errorf("could not read the drill history: %w", err)
			}

			// The same renderer as a live drill: there must not be two ways
			// to lay out a run.
			return report.Text(a.out, run, report.Options{
				Color: !a.noColor, ASCII: asciiOnly(), Verbose: a.verbose,
			})
		},
	}
}

// renderRunList lays out a listing.
//
// The id is shortened: the full uuid is noise in a table, and eight characters
// is what someone retypes into `runs show`.
func renderRunList(w io.Writer, runs []store.RunSummary) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "No drill has been recorded yet. Run one with `restorelab recovery test <workload>`.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTARTED\tWORKLOAD\tRESULT\tRTO\tCLEANUP")
	for _, r := range runs {
		name := r.SourceWorkloadID
		if r.SourceName != "" {
			name = fmt.Sprintf("%s (%s)", r.SourceName, r.SourceWorkloadID)
		}
		// An unfinished run has no verdict yet; its state is the honest
		// answer, not a blank column.
		verdict := string(r.Result)
		if verdict == "" {
			verdict = string(r.State)
		}
		cleanup := "done"
		if !r.CleanupDone {
			cleanup = "KEPT"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(r.ID),
			r.StartedAt.Local().Format("2006-01-02 15:04"),
			name,
			verdict,
			report.FormatDuration(r.RTO),
			cleanup,
		)
	}
	tw.Flush()
}

func shortID(id string) string {
	if len(id) <= shortIDLen {
		return id
	}
	return id[:shortIDLen]
}

// parseSince accepts a duration with a day suffix ("30d"), a Go duration
// ("12h"), or a plain date ("2026-08-01"). Anything else is refused rather
// than guessed at: silently listing the wrong window is worse than an error.
func parseSince(s string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("could not read %q as a date: use 30d, 12h, or 2026-08-01", s)
}
