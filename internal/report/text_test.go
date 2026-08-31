package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

func TestText_KeyLines(t *testing.T) {
	tests := []struct {
		name string
		run  *core.RecoveryRun
		want []string
	}{
		{
			name: "success",
			run:  fixtureRunSuccess(),
			want: []string{"Result   SUCCESS", "RTO      2m06s  (target 5m00s, met)"},
		},
		{
			name: "failed",
			run:  fixtureRunFailed(),
			want: []string{"Result   FAILED", "RTO      2m06s  (target 5m00s, met)", httpHealthFailMessage},
		},
		{
			name: "degraded",
			run:  fixtureRunDegraded(),
			want: []string{"Result   DEGRADED", "(target 5m00s, exceeded)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Text(&buf, tt.run, Options{}); err != nil {
				t.Fatalf("Text: %v", err)
			}
			out := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\n--- output ---\n%s", want, out)
				}
			}
		})
	}
}

func TestText_ColorOffProducesNoEscapes(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, fixtureRunFailed(), Options{Color: false}); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("\x1b")) {
		t.Fatalf("found ANSI escape byte with Color:false\n%s", buf.String())
	}
}

func TestText_ColorOnProducesEscapes(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, fixtureRunFailed(), Options{Color: true}); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("\x1b[")) {
		t.Fatalf("expected ANSI escapes with Color:true, found none:\n%s", buf.String())
	}
}

func TestText_ASCIIFallbackHasNoMultiByteGlyphs(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, fixtureRunFailed(), Options{ASCII: true, Color: true}); err != nil {
		t.Fatalf("Text: %v", err)
	}
	for i, b := range buf.Bytes() {
		if b > 127 {
			t.Fatalf("non-ASCII byte 0x%x at offset %d in ASCII-fallback output:\n%s", b, i, buf.String())
		}
	}
}

func TestText_FailedStepShowsErrorOnFollowingLine(t *testing.T) {
	var buf bytes.Buffer
	run := fixtureRunCleanupFailed()
	if err := Text(&buf, run, Options{}); err != nil {
		t.Fatalf("Text: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "✗ Cleanup") {
		t.Errorf("expected failed-step glyph before Cleanup, got:\n%s", out)
	}
	if !strings.Contains(out, "orphaned volume rl-101-disk-0 could not be removed") {
		t.Errorf("expected the step's error message in output:\n%s", out)
	}
}

func TestText_SkippedStepRendersAsDash(t *testing.T) {
	var buf bytes.Buffer
	run := fixtureRunWithSkippedStep()
	if err := Text(&buf, run, Options{}); err != nil {
		t.Fatalf("Text: %v", err)
	}
	out := buf.String()

	var stepLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Post-restore snapshot") {
			stepLine = ln
		}
	}
	if stepLine == "" {
		t.Fatalf("skipped step line not found in output:\n%s", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(stepLine), "- ") {
		t.Errorf("expected skipped step to start with '-', got line %q", stepLine)
	}
	if !strings.HasSuffix(strings.TrimSpace(stepLine), "-") {
		t.Errorf("expected skipped step duration column to render as '-', got line %q", stepLine)
	}
}

func TestText_NilRunErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, nil, Options{}); err == nil {
		t.Fatal("expected an error for a nil run")
	}
}
