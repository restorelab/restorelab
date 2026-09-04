package plan

import (
	"fmt"
	"strings"

	"github.com/restorelab/restorelab/internal/core"
)

// ProvenLevel is what this check establishes when it passes.
//
// A declaration in the plan wins outright, in both directions: someone who
// knows their `/opt/app/healthcheck.sh` reads a row out of the database can
// say `proves: data`, and someone who knows their check is a smoke test can
// say `proves: none`. Nobody is obliged to write either.
//
// Without a declaration the level is deduced, and the deduction is built to be
// wrong only in the safe direction - it understates, never the reverse. It has
// three rules beyond the default: a bare liveness probe, whether it is a
// command like `hostname` or a ping answered by the kernel, proves the guest
// boots - a captured value with a bound on it proves the data (see
// provesTheData) - and everything else is taken to be a real check of a real
// service. Guessing more finely than that would mean guessing what an
// arbitrary command means, and being wrong there would put a number on the
// dashboard that claims more than the drill established - the one outcome this
// whole slice exists to prevent.
func (c CheckSpec) ProvenLevel() core.ProofLevel {
	if c.Proves != "" {
		if l, ok := core.ParseProofLevel(strings.ToUpper(c.Proves)); ok && l.Recorded() {
			return l
		}
		// An unparseable value is refused by Validate. Reaching here means a
		// CheckSpec built in code rather than parsed, so fall through to the
		// deduction rather than trusting a string nobody checked.
	}

	// Ahead of the liveness table, because a bound on a value read out of the
	// workload says more about the check than the program that printed it: an
	// `echo` that has to print a number the plan vouches for is no longer a
	// liveness probe.
	if c.provesTheData() {
		return core.ProofData
	}

	if c.Type == "command" && isLivenessCommand(c.Params) {
		return core.ProofBoot
	}
	if livenessCheckTypes[c.Type] {
		return core.ProofBoot
	}
	return core.ProofService
}

// provesTheData reports whether this check reaches DATA on its own, with no
// `proves: data` in the plan.
//
// E1 wrote that only a declaration could claim DATA and that the next slice
// would refine it. This is the refinement, and it is the one deduction that
// reaches the top of the ladder: a check that captured a number out of the
// restored workload and passed a bound the plan stated on that number has read
// the data and found it to be what it was supposed to be. That is exactly what
// the level means, and unlike a declaration it is a claim RestoreLab checked
// itself - which is the difference between evidence and a promise. ProvenBy
// only counts a check that passed, so the bound was met wherever this level is
// finally recorded.
//
// A bare `capture:` with no bound stays where it was: reading a number proves
// you read a number. It is the bound that turns a measurement into a claim.
//
// Drift is deliberately not enough on its own, though it is a bound the
// operator declared just as much as `assert:` is. It is evaluated only when
// there is a history to compare against, and a first drill, or one against a
// database that would not answer, evaluates nothing and is skipped with its
// reason. A proof level that depended on whether some other table had rows in
// it would not be a proof level. A capture bounded by both is DATA on the
// strength of the assert, which is the common case the design note shows.
func (c CheckSpec) provesTheData() bool {
	return c.Capture != "" && c.Assert.Any()
}

// livenessCheckTypes are the check types that prove the guest is up without
// proving anything about a service on it.
//
// ICMP is answered by the kernel's network stack, not by anything anybody
// deployed: a guest that answers a ping has booted and configured its
// network, and every service on it may still be dead. Reading that as service
// evidence would overstate a drill in exactly the direction the proof level
// exists to prevent - it is the network-side twin of `cmd:hostname`.
var livenessCheckTypes = map[string]bool{
	"ping": true,
}

// livenessCommands are the commands that establish nothing beyond "the guest
// is running and can still fork a process" - which is genuinely worth
// knowing, and is genuinely all it is. The table only ever lowers a check's
// level, so adding to it can never make RestoreLab claim more than it proved.
//
// Matching is on the program name alone: `hostname -f` and `uname -a` prove
// no more than their bare forms.
var livenessCommands = map[string]bool{
	"hostname": true,
	"uname":    true,
	"whoami":   true,
	"true":     true,
	"echo":     true,
	"ver":      true, // the Windows equivalent of uname
}

// isLivenessCommand reports whether a command check's params amount to a bare
// liveness probe.
func isLivenessCommand(params map[string]any) bool {
	cmd, ok := commandLine(params)
	if !ok {
		return false
	}

	// A command line that chains or pipes is doing something beyond its first
	// program, and its first program is no longer what it proves. Treating
	// `hostname && systemctl is-active postgresql` as a liveness probe would
	// understate it - safe, but needlessly wrong.
	if strings.ContainsAny(cmd, "&|;\n") {
		return false
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	return livenessCommands[programName(fields[0])]
}

// commandLine renders a command check's params as a single line, from either
// form the check accepts: `run` (a shell command line) or `argv` (an argument
// vector executed directly).
func commandLine(params map[string]any) (string, bool) {
	if run, ok := params["run"].(string); ok {
		return run, true
	}

	raw, ok := params["argv"].([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			// An argv that is not a list of strings is a configuration error
			// the check itself will report. Claiming anything about it here
			// would be guessing.
			return "", false
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " "), true
}

// programName strips any directory prefix from a program, so /usr/bin/uname
// and C:\Windows\System32\hostname.exe are recognised as what they are.
func programName(arg string) string {
	name := arg
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	return name
}

// ProvenLevel is the level this plan would establish if every one of its
// checks passed. It is a promise about the plan, not a fact about a run - the
// run's own level counts only the checks that actually passed - and it exists
// so the plan editor can tell someone what their plan would prove before they
// spend five minutes drilling with it.
func (p *Plan) ProvenLevel() core.ProofLevel {
	if p.Startup.Skip || len(p.Checks) == 0 {
		return core.ProofNone
	}
	level := core.ProofNone
	for _, c := range p.Checks {
		level = level.Raise(c.ProvenLevel())
	}
	return level
}

// planProofPhrases read in the conditional, because a plan has not drilled
// anything yet. core.ProofLevel.Describe() is written for a run that already
// happened, and telling somebody editing a file that "the service was
// verified" would be a claim about a drill nobody has run.
var planProofPhrases = map[core.ProofLevel]string{
	core.ProofNone:    "no check runs inside the guest",
	core.ProofBoot:    "only the boot would be verified",
	core.ProofService: "the service would be verified, the data would not",
	core.ProofData:    "the service and its data would be verified",
}

// ProofSummary is the one-line reading of what this plan would establish,
// for the CLI's `plan validate` and the dashboard's editor.
func (p *Plan) ProofSummary() string {
	l := p.ProvenLevel()
	return fmt.Sprintf("%s, %s", l, planProofPhrases[l])
}
