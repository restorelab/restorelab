package checks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/restorelab/restorelab/internal/core"
)

// defaultMaxOutputBytes bounds how much of stdout/stderr the command check
// asks the provider to capture, so a chatty command in a recovered guest
// can't exhaust memory on the RestoreLab side.
const defaultMaxOutputBytes = 65536

// CommandCheck runs a command inside the recovered guest through the
// hypervisor's guest agent (core.Target.Exec) and validates its exit code
// and output. Because it travels over the same out-of-band channel that
// drove the restore, it needs no network path into the recovery network at
// all - which also means it can validate things a network probe never
// could (a systemd unit's actual state, a file's contents, a local socket).
//
// Params:
//   - run: command line, executed through a shell inside the guest
//   - argv: list of strings, executed directly with no shell
//     (exactly one of run/argv is required)
//   - shell: interpreter used for run (default "/bin/sh"); accepts a path,
//     or the shorthands sh, bash, cmd, powershell
//   - expect: the trimmed stdout must equal this exactly
//   - stdout_contains: substring stdout must contain
//   - stdout_matches: regular expression stdout must match
//   - expect_exit_code: required exit code (default 0)
//   - input: written to the command's stdin
//   - max_output_bytes: cap on captured output (default 65536)
type CommandCheck struct{}

func (CommandCheck) Type() string { return "command" }

func (CommandCheck) Run(ctx context.Context, target core.Target, cfg core.CheckConfig) core.CheckResult {
	p := NewParams(cfg.Params, target)

	_, hasRun := cfg.Params["run"]
	_, hasArgv := cfg.Params["argv"]

	var run string
	var argvParam []string
	if hasRun {
		run = p.RequireString("run")
	}
	if hasArgv {
		argvParam = p.StringSlice("argv")
	}

	shell := p.String("shell", "/bin/sh")
	_, hasExpect := cfg.Params["expect"]
	expect := p.String("expect", "")
	_, hasStdoutContains := cfg.Params["stdout_contains"]
	stdoutContains := p.String("stdout_contains", "")
	_, hasStdoutMatches := cfg.Params["stdout_matches"]
	stdoutMatches := p.String("stdout_matches", "")
	expectExitCode := p.Int("expect_exit_code", 0)
	input := p.String("input", "")
	maxOutputBytes := p.Int("max_output_bytes", defaultMaxOutputBytes)

	if err := p.Err(); err != nil {
		return core.CheckResult{Status: core.CheckError, Message: err.Error()}
	}

	if hasRun == hasArgv {
		return core.CheckResult{Status: core.CheckError, Message: `command: exactly one of "run" or "argv" is required`}
	}
	if hasArgv && len(argvParam) == 0 {
		return core.CheckResult{Status: core.CheckError, Message: `command: param "argv" must not be empty`}
	}

	var argv []string
	if hasRun {
		resolved, err := resolveShellArgv(shell, run)
		if err != nil {
			return core.CheckResult{Status: core.CheckError, Message: err.Error()}
		}
		argv = resolved
	} else {
		argv = argvParam
	}

	if target.Exec == nil {
		return core.CheckResult{
			Status: core.CheckError,
			Message: "command: this check needs a provider that can run commands in the guest " +
				"(no guest executor configured for this target); the workload needs the QEMU guest agent " +
				"installed and enabled",
		}
	}

	res, err := target.Exec.ExecInGuest(ctx, target.WorkloadID, core.ExecRequest{
		Argv:           argv,
		Input:          input,
		MaxOutputBytes: maxOutputBytes,
	})
	if err != nil {
		if errors.Is(err, core.ErrGuestAgentUnavailable) {
			return core.CheckResult{
				Status: core.CheckError,
				Message: fmt.Sprintf(
					"command: guest agent unavailable: %v (install and start qemu-guest-agent in the guest, "+
						"then enable the QEMU guest agent on the VM)", err),
			}
		}
		return core.CheckResult{
			Status:  core.CheckError,
			Message: fmt.Sprintf("command: could not run %q in the guest: %v", strings.Join(argv, " "), err),
		}
	}

	stdout, stderr := res.Stdout, res.Stderr
	trimmedStdout := strings.TrimSpace(stdout)
	details := map[string]any{
		"exit_code": res.ExitCode,
		"stdout":    truncate(stdout, 512),
		"stderr":    truncate(stderr, 512),
		"truncated": res.Truncated,
		"argv":      argv,
	}

	if res.ExitCode != expectExitCode {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("exit code %d, want %d%s", res.ExitCode, expectExitCode, commandEvidence(trimmedStdout, strings.TrimSpace(stderr))),
			Details: details,
		}
	}

	if hasExpect && trimmedStdout != expect {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("stdout %q, want %q", truncate(trimmedStdout, 200), truncate(expect, 200)),
			Details: details,
		}
	}

	if hasStdoutContains && !strings.Contains(stdout, stdoutContains) {
		return core.CheckResult{
			Status:  core.CheckFail,
			Message: fmt.Sprintf("stdout does not contain %q (stdout: %q)", stdoutContains, truncate(stdout, 200)),
			Details: details,
		}
	}

	if hasStdoutMatches {
		re, err := regexp.Compile(stdoutMatches)
		if err != nil {
			return core.CheckResult{
				Status:  core.CheckError,
				Message: fmt.Sprintf("command: invalid stdout_matches regex %q: %v", stdoutMatches, err),
				Details: details,
			}
		}
		if !re.MatchString(stdout) {
			return core.CheckResult{
				Status:  core.CheckFail,
				Message: fmt.Sprintf("stdout does not match /%s/ (stdout: %q)", stdoutMatches, truncate(stdout, 200)),
				Details: details,
			}
		}
	}

	msg := fmt.Sprintf("exit %d", res.ExitCode)
	if trimmedStdout != "" {
		msg = fmt.Sprintf("exit %d, stdout %q", res.ExitCode, truncate(trimmedStdout, 200))
	}
	return core.CheckResult{Status: core.CheckPass, Message: msg, Details: details}
}

// resolveShellArgv turns a "run" command line into an argv per the
// configured interpreter. /bin/sh (the default) and bash run the line
// through "-c"; cmd and powershell get their native one-shot invocation.
// A shell value that looks like a path (contains a slash or backslash) is
// used verbatim as the interpreter, also with "-c" - this covers anything
// not covered by the built-in shorthands (zsh, a custom shell, ...).
func resolveShellArgv(shell, run string) ([]string, error) {
	switch shell {
	case "sh", "/bin/sh":
		return []string{"/bin/sh", "-c", run}, nil
	case "bash":
		return []string{"bash", "-c", run}, nil
	case "cmd":
		return []string{"cmd", "/c", run}, nil
	case "powershell":
		return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", run}, nil
	default:
		if strings.ContainsAny(shell, `/\`) {
			return []string{shell, "-c", run}, nil
		}
		return nil, fmt.Errorf("command: unrecognized shell %q (use an interpreter path, or one of: sh, bash, cmd, powershell)", shell)
	}
}

var _ core.Check = CommandCheck{}

// commandEvidence quotes whatever the command actually said. Which stream
// carries the answer depends entirely on the command - `systemctl is-active`
// prints "inactive" on stdout and nothing on stderr - so a message that only
// ever quotes stderr throws away the evidence exactly when it is needed.
func commandEvidence(stdout, stderr string) string {
	switch {
	case stderr != "" && stdout != "":
		return fmt.Sprintf(": stdout %q, stderr %q", truncate(stdout, 200), truncate(stderr, 200))
	case stderr != "":
		return fmt.Sprintf(": stderr %q", truncate(stderr, 200))
	case stdout != "":
		return fmt.Sprintf(": stdout %q", truncate(stdout, 200))
	default:
		return " (no output)"
	}
}
