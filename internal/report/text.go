package report

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/restorelab/restorelab/internal/core"
)

// Options controls how Text renders a recovery run report.
type Options struct {
	// Color enables ANSI colour output. When false, no escape codes are
	// written at all.
	Color bool
	// Verbose includes step and check details that a plain report omits:
	// non-failed step messages, and non-sensitive check details.
	Verbose bool
	// Width is an advisory wrap width for long free-text fields such as
	// check messages. Zero (the default) means "do not wrap".
	Width int
	// ASCII forces plain-ASCII status glyphs ("+", "x", "-") instead of the
	// Unicode ✓/✗ glyphs, for terminals that cannot render them reliably
	// (notably legacy Windows consoles).
	ASCII bool
}

// ANSI SGR codes used by the text renderer. Every code here is exactly two
// digits ("\x1b[NNm", 5 bytes) so that a colourised cell always carries the
// same byte overhead regardless of which colour was chosen. That uniformity
// matters: text/tabwriter measures column width in runes, including any
// ANSI escape bytes we feed it, so if every cell in a column carries the
// same fixed overhead the padding tabwriter computes still lines up
// visually once a terminal strips the escapes. Do not add single-digit or
// bold/dim codes here without re-checking that property.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiGray   = "\x1b[90m"
)

func colorize(s, code string, opts Options) string {
	if !opts.Color || code == "" {
		return s
	}
	return code + s + ansiReset
}

// stepGlyph returns the glyph and colour code for a step's status.
func stepGlyph(status core.StepStatus, opts Options) (glyph, color string) {
	switch status {
	case core.StepDone:
		if opts.ASCII {
			return "+", ansiGreen
		}
		return "✓", ansiGreen
	case core.StepFailed:
		if opts.ASCII {
			return "x", ansiRed
		}
		return "✗", ansiRed
	case core.StepSkipped:
		return "-", ansiGray
	default: // pending / running: should not appear in a finished report
		return "-", ansiGray
	}
}

// checkGlyph returns the glyph and colour code for a check's status.
func checkGlyph(status core.CheckStatus, opts Options) (glyph, color string) {
	switch status {
	case core.CheckPass:
		if opts.ASCII {
			return "+", ansiGreen
		}
		return "✓", ansiGreen
	case core.CheckFail:
		if opts.ASCII {
			return "x", ansiRed
		}
		return "✗", ansiRed
	case core.CheckError:
		return "!", ansiRed
	case core.CheckSkipped:
		return "-", ansiGray
	default:
		return "-", ansiGray
	}
}

func resultColor(result core.RunResult) string {
	switch result {
	case core.ResultSuccess:
		return ansiGreen
	case core.ResultDegraded:
		return ansiYellow
	case core.ResultFailed:
		return ansiRed
	default:
		return ""
	}
}

func verificationLabel(v core.VerificationState) string {
	switch v {
	case core.VerificationOK:
		return "verified"
	case core.VerificationFailed:
		return "verification failed"
	case core.VerificationNone:
		return "not verified"
	default:
		return "verification unknown"
	}
}

// Text renders run as a human-readable terminal report. It is the primary
// artefact RestoreLab users see, so layout and alignment are load-bearing:
// columns are aligned with text/tabwriter (header fields, checks table) or
// by hand where mixed left/right alignment is needed (the step timeline).
func Text(w io.Writer, run *core.RecoveryRun, opts Options) error {
	if run == nil {
		return fmt.Errorf("report: run is nil")
	}

	var buf bytes.Buffer

	writeHeader(&buf, run)
	writeSteps(&buf, run, opts)
	writeChecks(&buf, run, opts)
	writeVerdict(&buf, run, opts)

	_, err := w.Write(buf.Bytes())
	return err
}

func writeHeader(buf *bytes.Buffer, run *core.RecoveryRun) {
	tw := tabwriter.NewWriter(buf, 0, 4, 2, ' ', 0)

	fmt.Fprintf(tw, "Recovery Run\t%s\n", run.ID)
	fmt.Fprintf(tw, "Plan\t%s\n", run.PlanName)

	workload := run.SourceName
	if run.SourceWorkloadID != "" {
		workload = fmt.Sprintf("%s (%s)", workload, run.SourceWorkloadID)
	}
	if run.Node != "" {
		workload = fmt.Sprintf("%s on %s", workload, run.Node)
	}
	fmt.Fprintf(tw, "Workload\t%s\n", workload)

	fmt.Fprintf(tw, "Backup\t%s\n", backupSummary(run.Backup))

	if run.TempWorkloadID != "" {
		fmt.Fprintf(tw, "Temporary VM\t%s %s\n", run.TempWorkloadID, run.TempName)
	}

	tw.Flush()
}

func backupSummary(b *core.Backup) string {
	if b == nil {
		return "none"
	}
	created := b.CreatedAt.UTC().Format("2006-01-02 15:04:05") + " UTC"
	return fmt.Sprintf("%s  (age %s, %s, %s)",
		created, FormatDuration(b.Age()), FormatBytes(b.SizeBytes), verificationLabel(b.Verified))
}

func writeSteps(buf *bytes.Buffer, run *core.RecoveryRun, opts Options) {
	if len(run.Steps) == 0 {
		return
	}

	type line struct {
		plainLabel string
		glyph      string
		color      string
		name       string
		dur        string
		failMsg    string
		infoMsg    string
	}

	lines := make([]line, 0, len(run.Steps))
	nameWidth, durWidth := 0, 0

	for _, s := range run.Steps {
		glyph, color := stepGlyph(s.Status, opts)
		label := StepLabel(s.Name)
		plainLabel := glyph + " " + label

		dur := "-"
		if s.Status != core.StepSkipped {
			dur = FormatDuration(s.Duration)
		}

		l := line{plainLabel: plainLabel, glyph: glyph, color: color, name: label, dur: dur}
		if s.Status == core.StepFailed {
			l.failMsg = s.Err
			if l.failMsg == "" {
				l.failMsg = s.Message
			}
			if l.failMsg == "" {
				l.failMsg = "step failed"
			}
		} else if opts.Verbose && s.Message != "" {
			l.infoMsg = s.Message
		}
		lines = append(lines, l)

		if n := utf8.RuneCountInString(plainLabel); n > nameWidth {
			nameWidth = n
		}
		if n := utf8.RuneCountInString(dur); n > durWidth {
			durWidth = n
		}
	}

	buf.WriteString("\n")
	const gap = 2
	for _, l := range lines {
		pad := nameWidth - utf8.RuneCountInString(l.plainLabel) + gap
		buf.WriteString("  ")
		buf.WriteString(colorize(l.glyph, l.color, opts))
		buf.WriteString(" ")
		buf.WriteString(l.name)
		buf.WriteString(strings.Repeat(" ", pad))
		buf.WriteString(padLeft(l.dur, durWidth))
		buf.WriteString("\n")

		if l.failMsg != "" {
			fmt.Fprintf(buf, "      %s\n", l.failMsg)
		} else if l.infoMsg != "" {
			fmt.Fprintf(buf, "      %s\n", l.infoMsg)
		}
	}
}

func padLeft(s string, width int) string {
	n := width - utf8.RuneCountInString(s)
	if n <= 0 {
		return s
	}
	return strings.Repeat(" ", n) + s
}

func writeChecks(buf *bytes.Buffer, run *core.RecoveryRun, opts Options) {
	buf.WriteString("\nChecks\n")

	if len(run.Checks) == 0 {
		buf.WriteString("  (no checks configured)\n")
		return
	}

	tw := tabwriter.NewWriter(buf, 0, 4, 3, ' ', 0)
	for _, c := range run.Checks {
		glyph, color := checkGlyph(c.Status, opts)
		coloredGlyph := colorize(glyph, color, opts)
		coloredStatus := colorize(string(c.Status), color, opts)
		dur := FormatDuration(c.Duration)
		fmt.Fprintf(tw, "  %s %s\t%s\t%s\t%s\n", coloredGlyph, c.Name, coloredStatus, dur, c.Message)

		if opts.Verbose {
			if detail := safeDetails(c.Details); detail != "" {
				fmt.Fprintf(tw, "      %s\t\t\t\n", detail)
			}
		}
	}
	tw.Flush()
}

// safeDetails renders a check's Details map as a compact, sorted key=value
// list, dropping any key that looks like it could hold a credential. Only
// scalar values are rendered; nested structures are omitted rather than
// risking a leaked secret in report output.
func safeDetails(details map[string]any) string {
	if len(details) == 0 {
		return ""
	}
	keys := make([]string, 0, len(details))
	for k := range details {
		if looksSensitive(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := details[k]
		switch tv := v.(type) {
		case string, bool, int, int32, int64, float32, float64:
			parts = append(parts, fmt.Sprintf("%s=%v", k, tv))
		default:
			// Skip non-scalar values: they are not guaranteed safe to print.
		}
	}
	return strings.Join(parts, "  ")
}

func looksSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, bad := range []string{"pass", "secret", "token", "credential", "auth", "key"} {
		if strings.Contains(k, bad) {
			return true
		}
	}
	return false
}

func writeVerdict(buf *bytes.Buffer, run *core.RecoveryRun, opts Options) {
	buf.WriteString("\n")

	resultWord := string(run.Result)
	fmt.Fprintf(buf, "Result   %s\n", colorize(resultWord, resultColor(run.Result), opts))

	fmt.Fprintf(buf, "RTO      %s", FormatDuration(run.RTO))
	if run.RTOTarget > 0 {
		state := "met"
		if run.RTOExceeded() {
			state = "exceeded"
		}
		fmt.Fprintf(buf, "  (target %s, %s)", FormatDuration(run.RTOTarget), state)
	}
	buf.WriteString("\n")
}

// stepLabels turns the engine's step identifiers into the phrasing an
// operator reads in a timeline. Unknown steps fall back to their identifier
// with the underscores removed, so a new step is readable before anyone
// remembers to add it here.
var stepLabels = map[string]string{
	"discover_backup":     "Backup discovered",
	"prepare_environment": "Environment prepared",
	"restore":             "Restore completed",
	"start":               "Workload started",
	"wait_for_guest":      "Guest reachable",
	"run_checks":          "Checks",
	"cleanup":             "Cleanup",
}

// StepLabel returns the human-readable name of a workflow step.
func StepLabel(name string) string {
	if label, ok := stepLabels[name]; ok {
		return label
	}
	spaced := strings.ReplaceAll(name, "_", " ")
	if spaced == "" {
		return spaced
	}
	return strings.ToUpper(spaced[:1]) + spaced[1:]
}
