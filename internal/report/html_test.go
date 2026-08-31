package report

import (
	"bytes"
	"strings"
	"testing"
)

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

func TestHTML_NilRunErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, nil); err == nil {
		t.Fatal("expected an error for a nil run")
	}
}
