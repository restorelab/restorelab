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
// the check from trying — the default shell may well be right, and the check
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
