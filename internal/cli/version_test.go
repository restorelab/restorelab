package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/version"
)

// `restorelab --version` and `restorelab version` must say the same thing,
// once.
//
// version.String() already begins with the program name, and cobra's default
// template prefixes "<use> version " to whatever Version holds - so the flag
// printed "restorelab version restorelab v0.1.0 (...)". It shipped in v0.1.0,
// in the first command anyone runs on a fresh install.
func TestVersionFlagDoesNotRepeatTheProgramName(t *testing.T) {
	var out bytes.Buffer
	a := &app{out: &out, err: &out}
	cmd := newRootCmd(a)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}

	got := strings.TrimSpace(out.String())
	if n := strings.Count(got, "restorelab"); n != 1 {
		t.Errorf("--version names the program %d times, want once: %q", n, got)
	}
	if got != version.String() {
		t.Errorf("--version = %q, want %q - the same string `version` prints", got, version.String())
	}
}
