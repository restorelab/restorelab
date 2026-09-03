package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// renderHTML renders run and fails the test if rendering itself errors.
func renderHTML(t *testing.T, run *core.RecoveryRun) string {
	t.Helper()
	var buf bytes.Buffer
	if err := HTML(&buf, run); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	return buf.String()
}

// titleOf extracts the contents of the document's <title> element.
func titleOf(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "<title>")
	end := strings.Index(out, "</title>")
	if start < 0 || end < 0 {
		t.Fatalf("no <title> element in output:\n%s", out)
	}
	return out[start+len("<title>") : end]
}

func TestHTML_ExecutesAndContainsVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, fixtureRunFailed()); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "<!doctype html>") {
		t.Errorf("expected a full HTML document, got:\n%s", out)
	}
	if !strings.Contains(out, "FAILED") {
		t.Errorf("expected the verdict FAILED in output")
	}
	if !strings.Contains(out, `class="badge bad"`) {
		t.Errorf("expected the FAILED verdict to use the 'bad' badge class")
	}
}

func TestHTML_SuccessUsesOkBadge(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, fixtureRunSuccess()); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `class="badge ok"`) {
		t.Errorf("expected the SUCCESS verdict to use the 'ok' badge class, got:\n%s", out)
	}
}

// TestHTML_EscapesCheckMessage is the explicit XSS regression test: a check
// message is remote-system-controlled data and must never reach the page
// unescaped.
func TestHTML_EscapesCheckMessage(t *testing.T) {
	run := fixtureRunFailed()
	run.Checks[1].Message = `<script>alert(1)</script>`

	var buf bytes.Buffer
	if err := HTML(&buf, run); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Fatalf("check message was not escaped; raw <script> tag present in output:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected the check message to appear HTML-escaped, got:\n%s", out)
	}
}

// TestHTML_BackupVerificationIsAPhrase pins the wording of the backup
// verification line. The bare state word ("none") used to be dropped into the
// backup line as-is, where it read as a stray word; worse, it read the same
// for a PBS snapshot nobody ever verified and for a vzdump backup whose
// format has no verification concept at all.
func TestHTML_BackupVerificationIsAPhrase(t *testing.T) {
	cases := []struct {
		name     string
		state    core.VerificationState
		format   string
		want     string
		wantNot  string
		wantBad  bool
		wantSize string
	}{
		{
			name:     "pbs snapshot verified by PBS",
			state:    core.VerificationOK,
			format:   "pbs",
			want:     "verified",
			wantSize: "4.2 GiB",
		},
		{
			name:    "pbs snapshot PBS never verified",
			state:   core.VerificationNone,
			format:  "pbs",
			want:    "never verified",
			wantNot: "not applicable",
		},
		{
			name:    "pbs-backed PVE storage, never verified",
			state:   core.VerificationNone,
			format:  "pbs-vm",
			want:    "never verified",
			wantNot: "not applicable",
		},
		{
			// The reported case: a vzdump backup carries no verification
			// state at all, so saying "not verified" would read as a reproach
			// for something this backup format cannot do.
			name:    "vzdump backup carries no verification state",
			state:   core.VerificationNone,
			format:  "vma.zst",
			want:    "verification not applicable to this backup format",
			wantNot: "never verified",
		},
		{
			name:    "provider reported a verification failure",
			state:   core.VerificationFailed,
			format:  "pbs",
			want:    "verification failed",
			wantBad: true,
		},
		{
			name:   "provider sent a state we do not recognise",
			state:  core.VerificationUnknown,
			format: "pbs",
			want:   "verification state unknown",
		},
		{
			name:   "provider never set the field",
			state:  "",
			format: "",
			want:   "verification not reported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := fixtureRunSuccess()
			run.Backup.Verified = tc.state
			run.Backup.Format = tc.format

			out := renderHTML(t, run)

			if !strings.Contains(out, tc.want) {
				t.Errorf("expected the backup line to say %q, got:\n%s", tc.want, out)
			}
			if tc.wantNot != "" && strings.Contains(out, tc.wantNot) {
				t.Errorf("did not expect the backup line to say %q, got:\n%s", tc.wantNot, out)
			}
			// The bare state word must never reach the page on its own.
			if strings.Contains(out, ", "+string(tc.state)+")") {
				t.Errorf("the raw verification state %q is still rendered bare inside the backup line", tc.state)
			}
			if got := strings.Contains(out, `class="note bad"`); got != tc.wantBad {
				t.Errorf("emphasised verification note = %v, want %v", got, tc.wantBad)
			}
			if tc.wantSize != "" && !strings.Contains(out, tc.wantSize) {
				t.Errorf("expected the backup size %q to survive, got:\n%s", tc.wantSize, out)
			}
		})
	}
}

// adhocCommandCheck is what an ad-hoc `--check 'cmd:...'` produces: no name
// in the plan, so plan.CheckSpec.DisplayName names it after its own type in
// capitals.
func adhocCommandCheck() core.CheckResult {
	return core.CheckResult{
		Name:      "COMMAND",
		Type:      "command",
		Status:    core.CheckPass,
		Duration:  120 * time.Millisecond,
		Attempts:  1,
		Message:   `exit 0, stdout "active"`,
		StartedAt: fixtureBase,
		Details: map[string]any{
			"exit_code": 0,
			"argv":      []string{"/bin/sh", "-c", "systemctl is-active postgresql"},
		},
	}
}

// TestHTML_UnnamedCommandCheckShowsWhatItRan covers the reported case: a check
// the plan did not name showed "COMMAND" in the Check column and "command" in
// the Type column, so the Check column carried no information at all.
func TestHTML_UnnamedCommandCheckShowsWhatItRan(t *testing.T) {
	run := fixtureRunSuccess()
	run.Checks = []core.CheckResult{adhocCommandCheck()}

	out := renderHTML(t, run)

	if strings.Contains(out, "<td>COMMAND</td>") {
		t.Errorf("the Check column still repeats the check type in capitals:\n%s", out)
	}
	if !strings.Contains(out, "<td>systemctl is-active postgresql</td>") {
		t.Errorf("expected the Check column to show the command that ran, got:\n%s", out)
	}
	if !strings.Contains(out, "<td>command</td>") {
		t.Errorf("the Type column must still say what kind of check this was, got:\n%s", out)
	}
}

// TestHTML_UnnamedCommandCheckAfterAStoreRoundTrip is the same check as it
// comes back out of the database, where Details has been through JSON and argv
// is a []any rather than a []string.
func TestHTML_UnnamedCommandCheckAfterAStoreRoundTrip(t *testing.T) {
	run := fixtureRunSuccess()
	check := adhocCommandCheck()
	check.Details["argv"] = []any{"cmd", "/c", "sc query Winmgmt"}
	run.Checks = []core.CheckResult{check}

	out := renderHTML(t, run)

	if !strings.Contains(out, "<td>sc query Winmgmt</td>") {
		t.Errorf("expected the Check column to show the command that ran, got:\n%s", out)
	}
}

// TestHTML_UnnamedCheckWithNothingToShowIsNotTheType covers the checks that
// record no evidence of what they targeted: the Check column must not fall
// back to shouting the type a second time.
func TestHTML_UnnamedCheckWithNothingToShowIsNotTheType(t *testing.T) {
	run := fixtureRunSuccess()
	run.Checks = []core.CheckResult{{
		Name:      "PING",
		Type:      "ping",
		Status:    core.CheckPass,
		Duration:  40 * time.Millisecond,
		Attempts:  1,
		Message:   "3/3 replies, avg 0.41ms",
		StartedAt: fixtureBase,
	}}

	out := renderHTML(t, run)

	if strings.Contains(out, "<td>PING</td>") {
		t.Errorf("the Check column still repeats the check type in capitals:\n%s", out)
	}
	if !strings.Contains(out, `<td><span class="empty">&mdash;</span></td>`) {
		t.Errorf("expected an explicit dash for a check with no name and no evidence, got:\n%s", out)
	}
}

// TestHTML_NamedChecksKeepTheirName guards the fix above from eating names the
// plan did set.
func TestHTML_NamedChecksKeepTheirName(t *testing.T) {
	out := renderHTML(t, fixtureRunSuccess())

	for _, want := range []string{"<td>TCP 22</td>", "<td>HTTP health</td>"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %s in the checks table, got:\n%s", want, out)
		}
	}
}

// TestHTML_TitleNamesTheDrillNotTheUUID covers the third reported case: the
// browser tab used to read as a bare run UUID, which identifies the report to
// nobody. The UUID must stay on the page, just not in the title.
func TestHTML_TitleNamesTheDrillNotTheUUID(t *testing.T) {
	run := fixtureRunFailed()
	run.ID = "94bce70d-1d0e-4a9b-9d4e-2f7c1a3b5c88"

	out := renderHTML(t, run)
	title := titleOf(t, out)

	if strings.Contains(title, run.ID) {
		t.Errorf("the page title still carries the run UUID: %q", title)
	}
	if !strings.Contains(title, "postgres-prod") {
		t.Errorf("the page title does not name the workload: %q", title)
	}
	if !strings.Contains(title, "2026-08-31") {
		t.Errorf("the page title does not say when the drill ran: %q", title)
	}
	if !strings.Contains(title, "FAILED") {
		t.Errorf("the page title does not carry the verdict: %q", title)
	}
	if !strings.Contains(out, run.ID) {
		t.Errorf("the run UUID must stay somewhere on the page, got:\n%s", out)
	}
}

// TestHTML_TitleFallsBackToTheWorkloadID covers a run whose workload has no
// name: the title must still identify something.
func TestHTML_TitleFallsBackToTheWorkloadID(t *testing.T) {
	run := fixtureRunSuccess()
	run.SourceName = ""

	title := titleOf(t, renderHTML(t, run))
	if !strings.Contains(title, "101") {
		t.Errorf("expected the workload id in the title, got %q", title)
	}
}

func TestHTML_NilRunErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, nil); err == nil {
		t.Fatal("expected an error for a nil run")
	}
}

// The HTML report is what gets attached to a compliance ticket, so it is the
// copy of the verdict that outlives the terminal. It has to carry the same
// qualification the terminal does.
func TestHTML_ShowsWhatTheDrillProved(t *testing.T) {
	run := fixtureRunSuccess()
	run.ProofLevel = core.ProofBoot

	var buf bytes.Buffer
	if err := HTML(&buf, run); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Proved") || !strings.Contains(out, string(core.ProofBoot)) {
		t.Error("the HTML report does not say what the drill proved")
	}
	if !strings.Contains(out, core.ProofBoot.Describe()) {
		t.Error("the level appears without the sentence that explains it")
	}
}

func TestHTML_SaysNothingWhenTheLevelWasNotRecorded(t *testing.T) {
	run := fixtureRunSuccess()
	run.ProofLevel = core.ProofUnknown

	var buf bytes.Buffer
	if err := HTML(&buf, run); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if strings.Contains(buf.String(), ">Proved<") {
		t.Error("an unrecorded level was rendered as a claim")
	}
}
