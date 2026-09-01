package report

import (
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// htmlView is what the HTML template renders. It is built entirely from
// Document (report.NewDocument), never from core types directly, and every
// free-text field (Message, Notes, names, ...) reaches the template as a
// plain string so html/template's contextual auto-escaping applies. Nothing
// in this file bypasses escaping via template.HTML.
type htmlView struct {
	Doc Document

	// PageTitle is the browser-tab title and Heading the page's <h1>. The
	// run ID alone answered neither: a tab reading "94bce70d-..." tells the
	// reader nothing about which drill they are looking at.
	PageTitle string
	Heading   string

	VerdictClass string
	VerdictLabel string
	GeneratedAt  string
	HasBackup    bool
	HasTemp      bool

	// BackupVerification is Backup.Verified rendered as a phrase instead of
	// the bare state word, and BackupVerificationClass emphasises it when it
	// is a genuine failure rather than an absence. See
	// backupVerificationLabel.
	BackupVerification      string
	BackupVerificationClass string

	Steps  []htmlStep
	Checks []htmlCheck
}

type htmlStep struct {
	StepDTO
	StatusClass string
	StatusLabel string
	BarPercent  int
}

type htmlCheck struct {
	CheckDTO

	// Label is what the "Check" column shows. It is CheckDTO.Name whenever
	// the plan actually named the check, and otherwise whatever the check
	// recorded about what it tested -- never a restatement of Type, which
	// has its own column. Empty means "this check has no name and recorded
	// nothing to identify it"; the template renders a dash.
	Label string

	StatusClass string
}

// HTML writes run as a single self-contained HTML document (inline CSS,
// no external requests, no JS) suitable for attaching to a compliance
// ticket. It is dark-mode aware (prefers-color-scheme) and print-friendly.
func HTML(w io.Writer, run *core.RecoveryRun) error {
	if run == nil {
		return fmt.Errorf("report: run is nil")
	}

	doc := NewDocument(run)

	view := htmlView{
		Doc:                doc,
		PageTitle:          pageTitle(doc),
		Heading:            heading(doc),
		VerdictClass:       verdictClass(run.Result),
		VerdictLabel:       string(run.Result),
		GeneratedAt:        time.Now().UTC().Format("2006-01-02 15:04:05") + " UTC",
		HasBackup:          doc.Backup != nil,
		HasTemp:            doc.TempWorkloadID != "",
		BackupVerification: backupVerificationLabel(doc.Backup),
	}
	if doc.Backup != nil && doc.Backup.Verified == string(core.VerificationFailed) {
		view.BackupVerificationClass = "bad"
	}

	maxDur := 0.0
	for _, s := range doc.Steps {
		if s.DurationSeconds > maxDur {
			maxDur = s.DurationSeconds
		}
	}
	for _, s := range doc.Steps {
		pct := 0
		if maxDur > 0 && s.DurationSeconds > 0 {
			pct = int(s.DurationSeconds/maxDur*100 + 0.5)
			if pct < 2 {
				pct = 2 // keep a visible sliver for very short steps
			}
		}
		view.Steps = append(view.Steps, htmlStep{
			StepDTO:     s,
			StatusClass: statusClass(s.Status),
			StatusLabel: statusLabel(s.Status),
			BarPercent:  pct,
		})
	}

	for _, c := range doc.Checks {
		view.Checks = append(view.Checks, htmlCheck{
			CheckDTO:    c,
			Label:       checkLabel(c),
			StatusClass: statusClass(c.Status),
		})
	}

	return htmlTemplate.Execute(w, view)
}

// workloadLabel names the workload a reader recognises: the source name when
// there is one, its id otherwise.
func workloadLabel(doc Document) string {
	if doc.SourceName != "" {
		return doc.SourceName
	}
	if doc.SourceWorkloadID != "" {
		return doc.SourceWorkloadID
	}
	if doc.PlanName != "" {
		return doc.PlanName
	}
	return "recovery drill"
}

// pageTitle is the <title>, i.e. what a browser tab, a bookmark and an
// archived file name show. The run ID is a UUID and identifies nothing to a
// human, so the title answers the three questions somebody scanning a folder
// of reports actually has: which workload, when, and did it pass. The ID
// stays on the page itself (header and footer).
func pageTitle(doc Document) string {
	parts := []string{workloadLabel(doc)}
	if !doc.StartedAt.IsZero() {
		parts = append(parts, doc.StartedAt.UTC().Format("2006-01-02"))
	}
	if doc.Result != "" {
		parts = append(parts, doc.Result)
	}
	return strings.Join(parts, " · ") + " · RestoreLab"
}

// heading is the page's <h1>. It leaves the verdict out because the verdict
// badge sits right next to it.
func heading(doc Document) string {
	return "Recovery drill · " + workloadLabel(doc)
}

// backupVerificationLabel renders BackupDTO.Verified as a phrase rather than
// the bare state word, which read as a stray "none" in the middle of the
// backup line.
//
// Verified carries core.VerificationState as a string, so it is one of "ok",
// "failed", "none" or "unknown" -- plus "" for a backup whose provider never
// set the field at all.
//
// "none" is the state that needed the most care. It means "the provider
// reported no verification", which covers two very different situations: a
// PBS snapshot that PBS could have verified and never did (worth flagging),
// and a vzdump backup, whose format has no verification concept at all
// (nothing to flag). Both providers map an absent verification onto
// VerificationNone, so the state alone cannot tell them apart -- but the
// document also carries the backup format, which can: the PBS provider
// always sets Format to "pbs", and PVE reports a PBS-backed storage as
// "pbs-vm"/"pbs-ct". Anything else is a vzdump-style backup that simply does
// not carry the information.
func backupVerificationLabel(b *BackupDTO) string {
	if b == nil {
		return ""
	}
	switch b.Verified {
	case string(core.VerificationOK):
		return "verified"
	case string(core.VerificationFailed):
		return "verification failed"
	case string(core.VerificationNone):
		if isPBSFormat(b.Format) {
			return "never verified"
		}
		return "verification not applicable to this backup format"
	case "":
		return "verification not reported"
	default:
		return "verification state unknown"
	}
}

// isPBSFormat reports whether a backup format string denotes a Proxmox Backup
// Server snapshot, the only kind of backup RestoreLab reads that carries a
// verification state.
func isPBSFormat(format string) bool {
	return format == "pbs" || strings.HasPrefix(format, "pbs-")
}

// checkLabel is what the "Check" column shows.
//
// A check the plan did not name is named by plan.CheckSpec.DisplayName, which
// falls back to the type in capitals: an ad-hoc `--check cmd:...` reaches the
// report as name "COMMAND", type "command", so the Check column shouted back
// the Type column and said nothing about what actually ran. When the name is
// nothing but the type, fall back to the evidence the check itself recorded.
func checkLabel(c CheckDTO) string {
	if c.Name != "" && !strings.EqualFold(c.Name, c.Type) {
		return c.Name
	}
	return commandLine(c.Details)
}

// commandLine recovers the command a "command" check ran from the argv it
// records in its details, unwrapping the shell that internal/checks wraps a
// plain `run:` line in ("/bin/sh -c systemctl is-active postgresql" is
// reported as "systemctl is-active postgresql").
//
// Details survives a store round-trip as JSON, so argv arrives either as the
// []string it was built as or as the []any that came back out of a database
// row; both are handled.
func commandLine(details map[string]any) string {
	argv := stringSlice(details["argv"])
	if len(argv) == 0 {
		return ""
	}
	if len(argv) >= 3 && isShellFlag(argv[len(argv)-2]) {
		argv = argv[len(argv)-1:]
	}
	return truncateLabel(strings.Join(argv, " "), 120)
}

// isShellFlag reports whether flag is the "run this line" flag of one of the
// interpreters internal/checks knows how to drive.
func isShellFlag(flag string) bool {
	switch flag {
	case "-c", "/c", "-Command":
		return true
	default:
		return false
	}
}

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

// truncateLabel keeps a derived label from stretching the table out of shape.
// It counts runes, not bytes, so it never cuts a multi-byte character in half.
func truncateLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " ") + "..."
}

func verdictClass(result core.RunResult) string {
	switch result {
	case core.ResultSuccess:
		return "ok"
	case core.ResultDegraded:
		return "warn"
	case core.ResultFailed:
		return "bad"
	default:
		return "warn"
	}
}

// statusClass maps a step or check status string onto one of the four CSS
// status classes used throughout the template.
func statusClass(status string) string {
	switch status {
	case "done", "pass":
		return "ok"
	case "failed", "fail", "error":
		return "bad"
	case "skipped":
		return "skip"
	default:
		return "pending"
	}
}

func statusLabel(status string) string {
	if status == "" {
		return "pending"
	}
	return status
}

var htmlTemplate = template.Must(template.New("report").Parse(htmlTemplateSource))

const htmlTemplateSource = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.PageTitle}}</title>
<style>
  :root{
    --bg:#f5f6f8; --panel:#ffffff; --border:#dde1e6; --text:#1b1f24; --muted:#5b6470;
    --accent:#2f6fed; --ok:#1a7f37; --ok-bg:#e6f4ea; --warn:#8a5a00; --warn-bg:#fdf1d8;
    --bad:#b3261e; --bad-bg:#fbe7e6; --bar:#c7d2fe; --bar-fill:#4f6bed;
  }
  @media (prefers-color-scheme: dark){
    :root{
      --bg:#14171c; --panel:#1c2028; --border:#2c313a; --text:#e7e9ec; --muted:#9aa3af;
      --accent:#7aa2ff; --ok:#4ada84; --ok-bg:#123321; --warn:#e8b94b; --warn-bg:#3a2f0d;
      --bad:#ef6a63; --bad-bg:#3a1414; --bar:#2c3350; --bar-fill:#5d7bf5;
    }
  }
  *{box-sizing:border-box;}
  body{
    margin:0; background:var(--bg); color:var(--text);
    font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;
  }
  .wrap{max-width:920px; margin:0 auto; padding:32px 24px 64px;}
  header{display:flex; align-items:center; justify-content:space-between; gap:16px; flex-wrap:wrap; margin-bottom:24px;}
  h1{font-size:20px; margin:0 0 4px;}
  .sub{color:var(--muted); font-size:13px;}
  .badge{
    display:inline-block; padding:8px 18px; border-radius:6px; font-size:16px; font-weight:700;
    letter-spacing:.04em; border:1px solid transparent;
  }
  .badge.ok{background:var(--ok-bg); color:var(--ok); border-color:var(--ok);}
  .badge.warn{background:var(--warn-bg); color:var(--warn); border-color:var(--warn);}
  .badge.bad{background:var(--bad-bg); color:var(--bad); border-color:var(--bad);}
  .panel{background:var(--panel); border:1px solid var(--border); border-radius:8px; padding:20px; margin-bottom:20px;}
  h2{font-size:13px; text-transform:uppercase; letter-spacing:.06em; color:var(--muted); margin:0 0 14px;}
  .grid{display:grid; grid-template-columns:repeat(auto-fit, minmax(200px,1fr)); gap:16px;}
  .field .k{color:var(--muted); font-size:12px; margin-bottom:2px;}
  .field .v{font-size:14px;}
  .field .note{color:var(--muted); font-size:12px; margin-top:2px;}
  .field .note.bad{color:var(--bad); font-weight:600;}
  .steps{display:flex; flex-direction:column; gap:10px;}
  .step{display:grid; grid-template-columns:22px 1fr auto; align-items:center; gap:10px;}
  .step .name{font-size:13px;}
  .step .dur{font-size:12px; color:var(--muted); min-width:64px; text-align:right;}
  .bar-track{grid-column:1 / -1; height:6px; background:var(--bar); border-radius:3px; overflow:hidden;}
  .bar-fill{height:100%; background:var(--bar-fill);}
  .bar-fill.bad{background:var(--bad);}
  .bar-fill.skip{background:var(--muted);}
  .dot{width:10px; height:10px; border-radius:50%; display:inline-block;}
  .dot.ok{background:var(--ok);}
  .dot.bad{background:var(--bad);}
  .dot.skip{background:var(--muted);}
  .dot.pending{background:var(--muted);}
  table{width:100%; border-collapse:collapse; font-size:13px;}
  th,td{text-align:left; padding:8px 10px; border-bottom:1px solid var(--border);}
  th{color:var(--muted); font-weight:600; font-size:11px; text-transform:uppercase; letter-spacing:.05em;}
  tr:last-child td{border-bottom:none;}
  .status-pill{display:inline-block; padding:2px 8px; border-radius:999px; font-size:11px; font-weight:600;}
  .status-pill.ok{background:var(--ok-bg); color:var(--ok);}
  .status-pill.bad{background:var(--bad-bg); color:var(--bad);}
  .status-pill.skip{background:var(--border); color:var(--muted);}
  .status-pill.pending{background:var(--border); color:var(--muted);}
  .empty{color:var(--muted); font-style:italic;}
  footer{color:var(--muted); font-size:11px; margin-top:24px;}
  @media print{
    body{background:#fff; color:#000;}
    .panel{border:1px solid #999; box-shadow:none; break-inside:avoid;}
    .badge{border-width:2px;}
    a{color:inherit; text-decoration:none;}
  }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div>
      <h1>{{.Heading}}</h1>
      <div class="sub">Plan {{.Doc.PlanName}} &middot; run {{.Doc.RunID}} &middot; generated {{.GeneratedAt}}</div>
    </div>
    <span class="badge {{.VerdictClass}}">{{.VerdictLabel}}</span>
  </header>

  <div class="panel">
    <h2>Summary</h2>
    <div class="grid">
      <div class="field">
        <div class="k">Workload</div>
        <div class="v">{{.Doc.SourceName}} ({{.Doc.SourceWorkloadID}}){{if .Doc.Node}} on {{.Doc.Node}}{{end}}</div>
      </div>
      {{if .HasTemp}}
      <div class="field">
        <div class="k">Temporary VM</div>
        <div class="v">{{.Doc.TempWorkloadID}} {{.Doc.TempName}}</div>
      </div>
      {{end}}
      <div class="field">
        <div class="k">Backup</div>
        {{if .HasBackup}}
        <div class="v">{{.Doc.Backup.CreatedAt.Format "2006-01-02 15:04:05"}} UTC ({{.Doc.Backup.Age}}, {{.Doc.Backup.Size}})</div>
        <div class="note{{if .BackupVerificationClass}} {{.BackupVerificationClass}}{{end}}">{{.BackupVerification}}</div>
        {{else}}
        <div class="v empty">none</div>
        {{end}}
      </div>
      <div class="field">
        <div class="k">Started</div>
        <div class="v">{{.Doc.StartedAt.Format "2006-01-02 15:04:05"}} UTC</div>
      </div>
      <div class="field">
        <div class="k">Completed</div>
        <div class="v">{{.Doc.CompletedAt.Format "2006-01-02 15:04:05"}} UTC</div>
      </div>
      <div class="field">
        <div class="k">RTO</div>
        <div class="v">{{.Doc.RTO}}{{if .Doc.RTOTarget}} (target {{.Doc.RTOTarget}}, {{if .Doc.RTOExceeded}}exceeded{{else}}met{{end}}){{end}}</div>
      </div>
    </div>
  </div>

  <div class="panel">
    <h2>Step Timeline</h2>
    {{if .Steps}}
    <div class="steps">
      {{range .Steps}}
      <div class="step">
        <span class="dot {{.StatusClass}}"></span>
        <span class="name">{{.Name}}</span>
        <span class="dur">{{.Duration}}</span>
        <div class="bar-track"><div class="bar-fill {{.StatusClass}}" style="width:{{.BarPercent}}%"></div></div>
      </div>
      {{end}}
    </div>
    {{else}}
    <div class="empty">No steps recorded.</div>
    {{end}}
  </div>

  <div class="panel">
    <h2>Checks</h2>
    {{if .Checks}}
    <table>
      <thead>
        <tr><th>Check</th><th>Type</th><th>Status</th><th>Duration</th><th>Message</th></tr>
      </thead>
      <tbody>
        {{range .Checks}}
        <tr>
          <td>{{if .Label}}{{.Label}}{{else}}<span class="empty">&mdash;</span>{{end}}</td>
          <td>{{.Type}}</td>
          <td><span class="status-pill {{.StatusClass}}">{{.Status}}</span></td>
          <td>{{.Duration}}</td>
          <td>{{.Message}}</td>
        </tr>
        {{end}}
      </tbody>
    </table>
    {{else}}
    <div class="empty">No checks configured.</div>
    {{end}}
  </div>

  <footer>RestoreLab &middot; schema {{.Doc.Schema}} &middot; run {{.Doc.RunID}}</footer>
</div>
</body>
</html>
`
