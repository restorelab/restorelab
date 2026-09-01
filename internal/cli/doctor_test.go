package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/diag"
)

func TestPrintFindingUsesTheRightGlyph(t *testing.T) {
	cases := []struct {
		level diag.Level
		want  string
	}{
		{diag.LevelOK, "[OK]"},
		{diag.LevelWarn, "[ !]"},
		{diag.LevelFail, "[!!]"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		a := &app{out: &buf, err: &buf, noColor: true}
		a.printFinding(diag.Finding{Level: tc.level, Title: "something"})

		got := buf.String()
		if !strings.Contains(got, "something") {
			t.Errorf("%s: output %q lost the title", tc.level, got)
		}
		// The ASCII glyphs are what a non-UTF-8 terminal gets; the Unicode
		// ones are the same three states.
		if !strings.Contains(got, tc.want) && !strings.ContainsAny(got, "✓!✗") {
			t.Errorf("%s: output %q carries no status glyph", tc.level, got)
		}
	}
}

func TestPrintFindingShowsTheDetailIndented(t *testing.T) {
	var buf bytes.Buffer
	a := &app{out: &buf, err: &buf, noColor: true}

	a.printFinding(diag.Finding{
		Level: diag.LevelWarn, Title: "cannot verify bridge",
		Detail: "a drill will proceed on the plan's assertion",
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want the title and its detail: %q", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[1], "      ") {
		t.Errorf("the detail line is not indented under its finding: %q", lines[1])
	}
}

func TestHasFindingMatchesAreaAndText(t *testing.T) {
	r := diag.Report{Findings: []diag.Finding{
		{Level: diag.LevelFail, Area: diag.AreaStorage, Title: "no backups found on any storage"},
		{Level: diag.LevelOK, Area: diag.AreaNodes, Title: "1 node(s), 1 online"},
	}}

	if !hasFinding(r, diag.AreaStorage, "no backups found") {
		t.Error("hasFinding missed a failure it should have matched")
	}
	if hasFinding(r, diag.AreaNodes, "no backups found") {
		t.Error("hasFinding matched across areas")
	}
	if hasFinding(r, diag.AreaWorkload, "anything") {
		t.Error("hasFinding matched an area with no findings")
	}
}
