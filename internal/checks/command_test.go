package checks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
)

// fakeExecutor is a scriptable core.GuestExecutor for exercising
// CommandCheck without a real hypervisor or guest agent.
type fakeExecutor struct {
	result *core.ExecResult
	err    error

	lastWorkloadID string
	lastReq        core.ExecRequest
	calls          int
}

func (f *fakeExecutor) ExecInGuest(ctx context.Context, workloadID string, req core.ExecRequest) (*core.ExecResult, error) {
	f.calls++
	f.lastWorkloadID = workloadID
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

var _ core.GuestExecutor = (*fakeExecutor)(nil)

func passResult(exitCode int, stdout, stderr string) *core.ExecResult {
	return &core.ExecResult{ExitCode: exitCode, Stdout: stdout, Stderr: stderr}
}

func runCommandCheck(t *testing.T, exec core.GuestExecutor, workloadID string, params map[string]any) core.CheckResult {
	t.Helper()
	target := core.Target{WorkloadID: workloadID, IP: "10.0.0.5", Exec: exec}
	cfg := core.CheckConfig{Type: "command", Params: params}
	return CommandCheck{}.Run(context.Background(), target, cfg)
}

func TestCommandCheck_RunShellShorthands(t *testing.T) {
	tests := []struct {
		name  string
		shell string
		want  []string
	}{
		{"default is /bin/sh", "", []string{"/bin/sh", "-c", "echo hi"}},
		{"sh shorthand", "sh", []string{"/bin/sh", "-c", "echo hi"}},
		{"explicit /bin/sh", "/bin/sh", []string{"/bin/sh", "-c", "echo hi"}},
		{"bash shorthand", "bash", []string{"bash", "-c", "echo hi"}},
		{"cmd shorthand", "cmd", []string{"cmd", "/c", "echo hi"}},
		{"powershell shorthand", "powershell", []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", "echo hi"}},
		{"explicit interpreter path with slash", "/usr/bin/zsh", []string{"/usr/bin/zsh", "-c", "echo hi"}},
		{"explicit interpreter path with backslash", `C:\tools\bash.exe`, []string{`C:\tools\bash.exe`, "-c", "echo hi"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExecutor{result: passResult(0, "", "")}
			params := map[string]any{"run": "echo hi"}
			if tt.shell != "" {
				params["shell"] = tt.shell
			}
			res := runCommandCheck(t, exec, "vm-1", params)
			if res.Status != core.CheckPass {
				t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
			}
			if got := exec.lastReq.Argv; !equalStrings(got, tt.want) {
				t.Fatalf("Argv = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommandCheck_UnrecognizedShell(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "echo hi", "shell": "fish"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	if !strings.Contains(res.Message, "fish") {
		t.Fatalf("Message should mention the bad shell, got: %q", res.Message)
	}
	if exec.calls != 0 {
		t.Fatal("should not have called the executor for a config error")
	}
}

func TestCommandCheck_ArgvUsedVerbatim(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "ok", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"argv": []any{"systemctl", "is-active", "postgresql"}})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	want := []string{"systemctl", "is-active", "postgresql"}
	if got := exec.lastReq.Argv; !equalStrings(got, want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

func TestCommandCheck_BothRunAndArgv_ConfigError(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{
		"run":  "echo hi",
		"argv": []any{"echo", "hi"},
	})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	if exec.calls != 0 {
		t.Fatal("should not have called the executor for a config error")
	}
}

func TestCommandCheck_NeitherRunNorArgv_ConfigError(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	if exec.calls != 0 {
		t.Fatal("should not have called the executor for a config error")
	}
}

func TestCommandCheck_EmptyArgv_ConfigError(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"argv": []any{}})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
}

func TestCommandCheck_TemplateExpansionInRun(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "ok", "")}
	target := core.Target{WorkloadID: "vm-1", IP: "10.0.0.5", Exec: exec}
	cfg := core.CheckConfig{Type: "command", Params: map[string]any{
		"run": "curl http://{{ .ip }}/health",
	}}
	res := CommandCheck{}.Run(context.Background(), target, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	want := []string{"/bin/sh", "-c", "curl http://10.0.0.5/health"}
	if got := exec.lastReq.Argv; !equalStrings(got, want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

func TestCommandCheck_ExitCodeMismatch(t *testing.T) {
	exec := &fakeExecutor{result: passResult(3, "", "Unit postgresql.service could not be found")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "systemctl is-active postgresql"})
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "exit code 3") || !strings.Contains(res.Message, "want 0") {
		t.Fatalf("Message should report the exit code mismatch, got: %q", res.Message)
	}
	if !strings.Contains(res.Message, "Unit postgresql.service could not be found") {
		t.Fatalf("Message should quote stderr as evidence, got: %q", res.Message)
	}
}

func TestCommandCheck_ExpectExitCode_Custom(t *testing.T) {
	exec := &fakeExecutor{result: passResult(2, "", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "false", "expect_exit_code": 2})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}

func TestCommandCheck_ExpectExactMatch(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "  active\n", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "systemctl is-active foo", "expect": "active"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass (trimmed stdout should equal expect)", res.Status, res.Message)
	}
}

func TestCommandCheck_ExpectMismatch(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "inactive", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "systemctl is-active foo", "expect": "active"})
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, `"inactive"`) || !strings.Contains(res.Message, `"active"`) {
		t.Fatalf("Message should quote both actual and expected stdout, got: %q", res.Message)
	}
}

func TestCommandCheck_StdoutContains(t *testing.T) {
	pass := runCommandCheck(t, &fakeExecutor{result: passResult(0, "server: listening on :8080", "")}, "vm-1",
		map[string]any{"run": "cat log", "stdout_contains": "listening"})
	if pass.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", pass.Status, pass.Message)
	}

	fail := runCommandCheck(t, &fakeExecutor{result: passResult(0, "server: crashed", "")}, "vm-1",
		map[string]any{"run": "cat log", "stdout_contains": "listening"})
	if fail.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", fail.Status)
	}
	if !strings.Contains(fail.Message, "listening") || !strings.Contains(fail.Message, "crashed") {
		t.Fatalf("Message should quote both the wanted substring and the actual stdout, got: %q", fail.Message)
	}
}

func TestCommandCheck_StdoutMatches(t *testing.T) {
	pass := runCommandCheck(t, &fakeExecutor{result: passResult(0, "version: 3.2.1", "")}, "vm-1",
		map[string]any{"run": "myapp --version", "stdout_matches": `version: \d+\.\d+\.\d+`})
	if pass.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", pass.Status, pass.Message)
	}

	fail := runCommandCheck(t, &fakeExecutor{result: passResult(0, "version: unknown", "")}, "vm-1",
		map[string]any{"run": "myapp --version", "stdout_matches": `version: \d+\.\d+\.\d+`})
	if fail.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", fail.Status)
	}
}

func TestCommandCheck_InvalidStdoutMatchesRegex(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "hi", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "echo hi", "stdout_matches": `[`})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError (bad regex is a config problem, not a failed check)", res.Status)
	}
}

func TestCommandCheck_AssertionPrecedence(t *testing.T) {
	// Exit code wrong AND expect wrong AND stdout_contains wrong: only the
	// exit code mismatch should be reported.
	exec := &fakeExecutor{result: passResult(1, "nope", "boom")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{
		"run":             "true",
		"expect":          "yep",
		"stdout_contains": "yep",
	})
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
	if !strings.Contains(res.Message, "exit code 1") {
		t.Fatalf("exit code mismatch should take precedence, got: %q", res.Message)
	}

	// Exit code right, expect wrong AND stdout_contains wrong: expect wins.
	exec2 := &fakeExecutor{result: passResult(0, "nope", "")}
	res2 := runCommandCheck(t, exec2, "vm-1", map[string]any{
		"run":             "true",
		"expect":          "yep",
		"stdout_contains": "yep",
	})
	if !strings.Contains(res2.Message, `want "yep"`) {
		t.Fatalf("expect mismatch should take precedence over stdout_contains, got: %q", res2.Message)
	}

	// expect right, stdout_contains AND stdout_matches wrong: stdout_contains wins.
	exec3 := &fakeExecutor{result: passResult(0, "nope", "")}
	res3 := runCommandCheck(t, exec3, "vm-1", map[string]any{
		"run":             "true",
		"stdout_contains": "yep",
		"stdout_matches":  "^yep$",
	})
	if !strings.Contains(res3.Message, "does not contain") {
		t.Fatalf("stdout_contains mismatch should take precedence over stdout_matches, got: %q", res3.Message)
	}
}

func TestCommandCheck_Pass_MessageIncludesStdout(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "active", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "systemctl is-active foo"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "exit 0") || !strings.Contains(res.Message, "active") {
		t.Fatalf("Message should be short and concrete, got: %q", res.Message)
	}
}

func TestCommandCheck_Pass_EmptyStdout_MessageOmitsStdout(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "true"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if res.Message != "exit 0" {
		t.Fatalf("Message = %q, want %q", res.Message, "exit 0")
	}
}

func TestCommandCheck_NilExec_ConfigError(t *testing.T) {
	target := core.Target{WorkloadID: "vm-1"} // Exec is nil
	cfg := core.CheckConfig{Type: "command", Params: map[string]any{"run": "echo hi"}}
	res := CommandCheck{}.Run(context.Background(), target, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, Message = %q, want CheckError (missing capability is not a broken service)", res.Status, res.Message)
	}
	if !strings.Contains(strings.ToLower(res.Message), "guest agent") {
		t.Fatalf("Message should explain the QEMU guest agent requirement, got: %q", res.Message)
	}
}

func TestCommandCheck_GuestAgentUnavailable_ConfigError(t *testing.T) {
	exec := &fakeExecutor{err: fmt.Errorf("call failed: %w", core.ErrGuestAgentUnavailable)}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "echo hi"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, Message = %q, want CheckError (agent unavailable is not a failed check)", res.Status, res.Message)
	}
	if !strings.Contains(strings.ToLower(res.Message), "qemu-guest-agent") {
		t.Fatalf("Message should name the fix, got: %q", res.Message)
	}
}

func TestCommandCheck_OtherTransportError_ConfigError(t *testing.T) {
	exec := &fakeExecutor{err: errors.New("connection reset")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "echo hi"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
}

func TestCommandCheck_InputAndMaxOutputBytesForwarded(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "ok", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{
		"run":              "cat",
		"input":            "hello from stdin",
		"max_output_bytes": 4096,
	})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if exec.lastReq.Input != "hello from stdin" {
		t.Fatalf("Input = %q, want %q", exec.lastReq.Input, "hello from stdin")
	}
	if exec.lastReq.MaxOutputBytes != 4096 {
		t.Fatalf("MaxOutputBytes = %d, want 4096", exec.lastReq.MaxOutputBytes)
	}
}

func TestCommandCheck_MaxOutputBytesDefault(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "ok", "")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "true"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, want CheckPass", res.Status)
	}
	if exec.lastReq.MaxOutputBytes != 65536 {
		t.Fatalf("MaxOutputBytes = %d, want default 65536", exec.lastReq.MaxOutputBytes)
	}
}

func TestCommandCheck_WorkloadIDForwarded(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}
	runCommandCheck(t, exec, "vm-42", map[string]any{"run": "true"})
	if exec.lastWorkloadID != "vm-42" {
		t.Fatalf("workloadID = %q, want %q", exec.lastWorkloadID, "vm-42")
	}
}

func TestCommandCheck_Details(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "stdout-data", "stderr-data")}
	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "echo hi"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, want CheckPass", res.Status)
	}
	for _, key := range []string{"exit_code", "stdout", "stderr", "truncated", "argv"} {
		if _, ok := res.Details[key]; !ok {
			t.Fatalf("Details missing key %q: %+v", key, res.Details)
		}
	}
	if res.Details["exit_code"] != 0 {
		t.Fatalf("Details[exit_code] = %v, want 0", res.Details["exit_code"])
	}
	if res.Details["stdout"] != "stdout-data" {
		t.Fatalf("Details[stdout] = %v, want %q", res.Details["stdout"], "stdout-data")
	}
}

func TestDefault_KnowsCommandType(t *testing.T) {
	r := Default()
	if _, ok := r.Get("command"); !ok {
		t.Fatal("Default() should register the command check type")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ core.Check = CommandCheck{}

// osAwareExecutor is a fakeExecutor that also answers core.GuestOSDetector,
// the way a real Proxmox provider does.
type osAwareExecutor struct {
	fakeExecutor
	guestOS  core.GuestOS
	osErr    error
	osCalls  int
	lastOSID string
}

func (f *osAwareExecutor) GuestOS(ctx context.Context, workloadID string) (core.GuestOS, error) {
	f.osCalls++
	f.lastOSID = workloadID
	if f.osErr != nil {
		return core.GuestOS{}, f.osErr
	}
	return f.guestOS, nil
}

var _ core.GuestOSDetector = (*osAwareExecutor)(nil)

func TestCommandCheck_AutoShellFromGuestOS(t *testing.T) {
	tests := []struct {
		name    string
		guestOS core.GuestOS
		want    []string
	}{
		{
			name:    "windows guest gets cmd",
			guestOS: core.GuestOS{Family: core.GuestOSWindows, ID: "mswindows", Name: "Microsoft Windows"},
			want:    []string{"cmd", "/c", "hostname"},
		},
		{
			name:    "linux guest gets /bin/sh",
			guestOS: core.GuestOS{Family: core.GuestOSLinux, ID: "debian", Name: "Debian GNU/Linux"},
			want:    []string{"/bin/sh", "-c", "hostname"},
		},
		{
			name:    "unrecognised family falls back to /bin/sh",
			guestOS: core.GuestOS{ID: "freebsd", Name: "FreeBSD"},
			want:    []string{"/bin/sh", "-c", "hostname"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &osAwareExecutor{guestOS: tt.guestOS}
			exec.result = passResult(0, "host", "")

			res := runCommandCheck(t, exec, "vm-9001", map[string]any{"run": "hostname"})
			if res.Status != core.CheckPass {
				t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
			}
			if got := exec.lastReq.Argv; !equalStrings(got, tt.want) {
				t.Fatalf("Argv = %v, want %v", got, tt.want)
			}
			if exec.osCalls != 1 {
				t.Fatalf("GuestOS calls = %d, want 1", exec.osCalls)
			}
			if exec.lastOSID != "vm-9001" {
				t.Fatalf("GuestOS workloadID = %q, want %q", exec.lastOSID, "vm-9001")
			}
		})
	}
}

func TestCommandCheck_ExplicitShellSkipsDetection(t *testing.T) {
	exec := &osAwareExecutor{guestOS: core.GuestOS{Family: core.GuestOSWindows}}
	exec.result = passResult(0, "", "")

	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "hostname", "shell": "bash"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if exec.osCalls != 0 {
		t.Fatalf("GuestOS calls = %d, want 0: an explicit shell must be obeyed verbatim", exec.osCalls)
	}
	if got, want := exec.lastReq.Argv, []string{"bash", "-c", "hostname"}; !equalStrings(got, want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

func TestCommandCheck_ArgvSkipsDetection(t *testing.T) {
	exec := &osAwareExecutor{guestOS: core.GuestOS{Family: core.GuestOSWindows}}
	exec.result = passResult(0, "", "")

	res := runCommandCheck(t, exec, "vm-1", map[string]any{"argv": []any{"hostname"}})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if exec.osCalls != 0 {
		t.Fatalf("GuestOS calls = %d, want 0: argv runs with no shell at all", exec.osCalls)
	}
}

func TestCommandCheck_ExecutorWithoutDetectionStillRuns(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "", "")}

	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "hostname"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if got, want := exec.lastReq.Argv, []string{"/bin/sh", "-c", "hostname"}; !equalStrings(got, want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

// A guest that is still booting cannot answer get-osinfo. That must not stop
// the check from trying: the default shell may well be right, and the check
// gets retried anyway.
func TestCommandCheck_DetectionFailureStillRunsDefaultShell(t *testing.T) {
	exec := &osAwareExecutor{osErr: fmt.Errorf("%w: agent not answering", core.ErrGuestAgentUnavailable)}
	exec.result = passResult(0, "host", "")

	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "hostname"})
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if got, want := exec.lastReq.Argv, []string{"/bin/sh", "-c", "hostname"}; !equalStrings(got, want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
}

// When detection failed AND the command could not run, the operator needs
// both halves of the story: the exec error, and the fact that the shell was
// a guess.
func TestCommandCheck_DetectionFailureIsReportedWhenExecFails(t *testing.T) {
	exec := &osAwareExecutor{osErr: errors.New("no permission to ask")}
	exec.err = fmt.Errorf("%w: agent not answering", core.ErrGuestAgentUnavailable)

	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "hostname"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	for _, want := range []string{"guest OS could not be detected", "/bin/sh", "no permission to ask", `"shell"`} {
		if !strings.Contains(res.Message, want) {
			t.Fatalf("Message should mention %q, got: %q", want, res.Message)
		}
	}
}

func TestCommandCheck_NoFallbackNoteWhenDetectionWorked(t *testing.T) {
	exec := &osAwareExecutor{guestOS: core.GuestOS{Family: core.GuestOSLinux}}
	exec.err = fmt.Errorf("%w: agent went away mid-run", core.ErrGuestAgentUnavailable)

	res := runCommandCheck(t, exec, "vm-1", map[string]any{"run": "hostname"})
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError", res.Status)
	}
	if strings.Contains(res.Message, "could not be detected") {
		t.Fatalf("Message must not blame detection when it succeeded, got: %q", res.Message)
	}
}

// --- capture, assert and drift ------------------------------------------
//
// The judgement itself is exercised in values_test.go, against pure
// functions and no guest at all. What is tested here is the wiring: that
// nothing is measured out of a command that already failed its own contract,
// that a declared bound changes the verdict, and that an unreadable history
// never does.

// fakeBaselines is a scriptable core.BaselineReader. It records what it was
// asked for, because "which history did this check read" is a question the
// design has an opinion about: a check may only ever see its own.
type fakeBaselines struct {
	values []float64
	err    error

	calls         int
	lastCheckName string
	lastValueName string
	lastLimit     int
}

func (f *fakeBaselines) Values(_ context.Context, checkName, valueName string, limit int) ([]float64, error) {
	f.calls++
	f.lastCheckName = checkName
	f.lastValueName = valueName
	f.lastLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.values, nil
}

var _ core.BaselineReader = (*fakeBaselines)(nil)

// runCaptureCheck runs a command check that carries capture/assert/drift,
// against a target whose Baseline may be nil (the no-history case).
func runCaptureCheck(t *testing.T, exec core.GuestExecutor, baselines core.BaselineReader, cfg core.CheckConfig) core.CheckResult {
	t.Helper()
	target := core.Target{WorkloadID: "vm-1", IP: "10.0.0.5", Exec: exec}
	if baselines != nil {
		target.Baseline = baselines
	}
	cfg.Type = "command"
	return CommandCheck{}.Run(context.Background(), target, cfg)
}

// capturedValue reads one measurement back out of a result's details.
func capturedValue(t *testing.T, res core.CheckResult, name string) (float64, bool) {
	t.Helper()
	values, ok := res.Details[DetailCaptured].(map[string]float64)
	if !ok {
		return 0, false
	}
	v, ok := values[name]
	return v, ok
}

// A capture with no bound records the number and changes no verdict. That is
// C5 carried forward: RestoreLab does not accuse a backup on a statistic
// nobody agreed to.
func TestCommandCheck_CaptureRecordsTheValue(t *testing.T) {
	exec := &fakeExecutor{result: passResult(0, "1204331\n", "")}
	res := runCaptureCheck(t, exec, nil, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "psql -tAc " + sqlCount},
		Capture: "rows",
	})

	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	got, ok := capturedValue(t, res, "rows")
	if !ok {
		t.Fatalf("no captured value in details %#v", res.Details)
	}
	if got != 1204331 {
		t.Fatalf("captured %v, want 1204331", got)
	}
}

// sqlCount is the query the design note uses, kept out of the test body so
// its quoting does not fight the Go string literal.
const sqlCount = `'select count(*) from orders'`

// The measurement comes after the command's own contract, and this is the
// case that matters: a number read out of a command that already failed is
// not a measurement of anything, and recording it would put a reading nobody
// can trust into the window the next drill is graded against.
func TestCommandCheck_NothingIsCapturedFromACommandThatFailed(t *testing.T) {
	tests := []struct {
		name   string
		result *core.ExecResult
		params map[string]any
	}{
		{"bad exit code", passResult(1, "0\n", "could not connect"), map[string]any{"run": "q"}},
		{"expect mismatch", passResult(0, "0\n", ""), map[string]any{"run": "q", "expect": "1204331"}},
		{"stdout_contains mismatch", passResult(0, "0\n", ""), map[string]any{"run": "q", "stdout_contains": "12"}},
		{"stdout_matches mismatch", passResult(0, "0\n", ""), map[string]any{"run": "q", "stdout_matches": `^1\d+$`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baselines := &fakeBaselines{values: []float64{1204331}}
			res := runCaptureCheck(t, &fakeExecutor{result: tt.result}, baselines, core.CheckConfig{
				Name:    "orders",
				Params:  tt.params,
				Capture: "rows",
				Assert:  &core.AssertSpec{Min: floatPtr(1)},
				Drift:   &core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true},
			})

			if res.Status != core.CheckFail {
				t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
			}
			if _, ok := capturedValue(t, res, "rows"); ok {
				t.Errorf("a value was captured out of a command that failed its own contract: %#v", res.Details)
			}
			if baselines.calls != 0 {
				t.Errorf("the history was read for a command that failed its own contract")
			}
		})
	}
}

// A capture that cannot be read fails the check rather than erroring it.
// core.CheckError makes no claim about the workload and never degrades a
// verdict; a query that was supposed to print a row count and printed nothing
// is a fact about the restored workload, and grading it as "the check could
// not run" is the silent pass this whole slice exists to prevent.
func TestCommandCheck_CaptureRefusesWhatIsNotANumber(t *testing.T) {
	for _, stdout := range []string{"", "three\n", "NaN\n", "1\n2\n"} {
		t.Run(fmt.Sprintf("%q", stdout), func(t *testing.T) {
			res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, stdout, "")}, nil, core.CheckConfig{
				Name:    "orders",
				Params:  map[string]any{"run": "q"},
				Capture: "rows",
			})

			if res.Status != core.CheckFail {
				t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
			}
			if !strings.Contains(res.Message, "rows") {
				t.Errorf("Message should name the capture, got %q", res.Message)
			}
			if _, ok := capturedValue(t, res, "rows"); ok {
				t.Errorf("an unreadable capture was recorded anyway: %#v", res.Details)
			}
		})
	}
}

func TestCommandCheck_AssertViolationFailsTheCheck(t *testing.T) {
	res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "0\n", "")}, nil, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "q"},
		Capture: "rows",
		Assert:  &core.AssertSpec{Min: floatPtr(1)},
	})

	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
	}
	// The capture name, the reading and the bound: the three facts an
	// operator needs at 03:00 without opening the plan.
	for _, want := range []string{"rows", "0", "1"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("Message %q should mention %q", res.Message, want)
		}
	}
	// The value is still recorded. A reading that broke a bound is exactly
	// the one the next report should be able to show.
	if got, ok := capturedValue(t, res, "rows"); !ok || got != 0 {
		t.Errorf("captured = %v (present %v), want 0 recorded", got, ok)
	}
}

func TestCommandCheck_AssertSatisfiedPasses(t *testing.T) {
	res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "1204331\n", "")}, nil, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "q"},
		Capture: "rows",
		Assert:  &core.AssertSpec{Min: floatPtr(1)},
	})

	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}

func TestCommandCheck_DriftViolationFailsTheCheck(t *testing.T) {
	baselines := &fakeBaselines{values: []float64{1200000, 1204331, 1210000}}
	res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "0\n", "")}, baselines, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "q"},
		Capture: "rows",
		Drift:   &core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true},
	})

	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "1204331") {
		t.Errorf("Message %q should name the baseline it was judged against", res.Message)
	}
}

// A check reads its own history and nobody else's. The workload is fixed
// where the reader is built; the check may only name its own check and its
// own capture, and five is the window the median is defined over.
func TestCommandCheck_DriftReadsOnlyItsOwnHistory(t *testing.T) {
	baselines := &fakeBaselines{values: []float64{1204331}}
	runCaptureCheck(t, &fakeExecutor{result: passResult(0, "1204331\n", "")}, baselines, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "q"},
		Capture: "rows",
		Drift:   &core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true},
	})

	if baselines.lastCheckName != "orders" || baselines.lastValueName != "rows" {
		t.Errorf("the history was read for check %q value %q, want orders/rows",
			baselines.lastCheckName, baselines.lastValueName)
	}
	if baselines.lastLimit != DriftWindow {
		t.Errorf("the window was %d, want %d", baselines.lastLimit, DriftWindow)
	}
}

// Being unable to read the past is not evidence about the present. Three ways
// of having no history, one verdict: the drift half is skipped with its
// reason and the check is not failed on it.
func TestCommandCheck_DriftWithoutAHistoryIsSkippedNotFailed(t *testing.T) {
	tests := []struct {
		name      string
		baselines core.BaselineReader
		reason    string
	}{
		{"no reader at all", nil, "no history"},
		{"the history could not be read", &fakeBaselines{err: errors.New("database is locked")}, "database is locked"},
		{"no previous drill measured this", &fakeBaselines{}, "no previous drill"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "1204331\n", "")}, tt.baselines, core.CheckConfig{
				Name:    "orders",
				Params:  map[string]any{"run": "q"},
				Capture: "rows",
				Drift:   &core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true},
			})

			if res.Status != core.CheckPass {
				t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
			}
			skipped, _ := res.Details[DetailDriftSkipped].(string)
			if skipped == "" {
				t.Fatalf("the drift check was not reported as skipped: %#v", res.Details)
			}
			if !strings.Contains(skipped, tt.reason) {
				t.Errorf("skip reason %q should say why (%q)", skipped, tt.reason)
			}
			// The reason has to reach the operator, not just the database.
			if !strings.Contains(res.Message, skipped) {
				t.Errorf("Message %q should carry the skip reason %q", res.Message, skipped)
			}
			if _, ok := capturedValue(t, res, "rows"); !ok {
				t.Errorf("the value was not recorded even though the command succeeded: %#v", res.Details)
			}
		})
	}
}

// The two halves are independent. A history that cannot be read must not stop
// the bound the operator actually vouched for from being judged.
func TestCommandCheck_AssertIsJudgedEvenWhenDriftIsSkipped(t *testing.T) {
	res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "0\n", "")}, nil, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "q"},
		Capture: "rows",
		Assert:  &core.AssertSpec{Min: floatPtr(1)},
		Drift:   &core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true},
	})

	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail on the declared minimum", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "minimum") {
		t.Errorf("Message %q should be about the assert, not about the missing history", res.Message)
	}
}

// A check with no capture must not gain a history read, a details key or any
// new way to fail. Most checks in most plans are this one.
func TestCommandCheck_NoCaptureTouchesNothing(t *testing.T) {
	baselines := &fakeBaselines{values: []float64{1204331}}
	res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "hello\n", "")}, baselines, core.CheckConfig{
		Name:   "smoke",
		Params: map[string]any{"run": "echo hello"},
	})

	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if baselines.calls != 0 {
		t.Errorf("a check with no capture read the history %d times", baselines.calls)
	}
	if _, ok := res.Details[DetailCaptured]; ok {
		t.Errorf("a check with no capture reported a captured value: %#v", res.Details)
	}
}

// The baseline a drift verdict was judged against travels with the result, so
// a report can show the comparison rather than a bare number.
func TestCommandCheck_TheBaselineIsRecordedBesideTheValue(t *testing.T) {
	baselines := &fakeBaselines{values: []float64{1200000, 1204331, 1210000}}
	res := runCaptureCheck(t, &fakeExecutor{result: passResult(0, "1206890\n", "")}, baselines, core.CheckConfig{
		Name:    "orders",
		Params:  map[string]any{"run": "q"},
		Capture: "rows",
		Drift:   &core.DriftSpec{MaxDrop: 10, MaxDropIsPercent: true},
	})

	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	got, ok := res.Details[DetailBaseline].(map[string]float64)
	if !ok {
		t.Fatalf("no baseline in details %#v", res.Details)
	}
	if got["rows"] != 1204331 {
		t.Fatalf("baseline = %v, want the median 1204331", got["rows"])
	}
}
