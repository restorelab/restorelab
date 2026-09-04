package core

// ProofLevel says what a drill actually established about a workload.
//
// It exists because "the drill succeeded" and "recovery works" are not the
// same statement, and a product that conflates them is worse than no product.
// A workload drilled with the default check - a command that prints the
// hostname - has proven that the kernel came up and can fork a process. It has
// proven nothing about the service, and nothing at all about the data. Left
// unsaid, that becomes a dashboard of reassuring green that stops people from
// looking, which is the most dangerous way for a backup-verification tool to
// fail.
//
// This is the counterpart of RunInconclusive. That one says RestoreLab does
// not accuse a backup it could not verify; this one says it does not
// congratulate one it did not verify either.
type ProofLevel string

// The levels, in increasing order of what they establish. They are ordered by
// what is *proven*, not by what is expensive to arrange: a passing network
// probe proves a service listens, which is worth less than a query against the
// data even though it needs a route into the recovery network and the query
// does not.
const (
	// ProofUnknown is the zero value: no level was recorded. It is what runs
	// from before this feature carry, and it means "not recorded" - never
	// "nothing was proven". Nothing may be concluded from it in either
	// direction.
	ProofUnknown ProofLevel = ""

	// ProofNone: the drill never ran code inside the guest. A restore-only
	// drill, a guest whose agent never answered, a run that ended before its
	// checks.
	ProofNone ProofLevel = "NONE"

	// ProofBoot: the operating system started and executes code. Established
	// by the guest agent answering - it runs inside the guest - or by a
	// trivial in-guest command coming back. Never by power state alone: a
	// running hypervisor process is not a booted OS.
	ProofBoot ProofLevel = "BOOT"

	// ProofService: a service started with its real configuration and its
	// real data. Any check that is not a bare liveness probe: an in-guest
	// command against the service, or a tcp/http/dns probe that answered.
	ProofService ProofLevel = "SERVICE"

	// ProofData: the data is there and coherent.
	//
	// Claimed by a declaration, via `proves: data` in the plan, or earned by
	// a measurement: a check that captures a number and holds it to a bound
	// the plan declared has read a value out of the restored workload and
	// found it to be what the operator said it must be, which is what this
	// level means.
	//
	// A drift tolerance alone does not earn it, and the asymmetry is
	// deliberate. Drift is only evaluated when there is a history to compare
	// against, so a first drill skips it; a level that depended on how many
	// rows another table happens to hold would not be a level at all.
	ProofData ProofLevel = "DATA"
)

// proofRanks orders the levels. ProofUnknown is deliberately absent: it ranks
// below everything, so raising an unrecorded level to a real one always takes
// the real one.
var proofRanks = map[ProofLevel]int{
	ProofNone:    0,
	ProofBoot:    1,
	ProofService: 2,
	ProofData:    3,
}

// Rank orders a level against the others. An unknown or unrecognised level
// ranks -1, below ProofNone, so it never wins a comparison and never clamps
// anything.
func (l ProofLevel) Rank() int {
	if r, ok := proofRanks[l]; ok {
		return r
	}
	return -1
}

// Recorded reports whether the level is a real one. False for the zero value
// and for anything unrecognised read back from storage.
func (l ProofLevel) Recorded() bool { return l.Rank() >= 0 }

// AtLeast reports whether l establishes at least as much as other.
func (l ProofLevel) AtLeast(other ProofLevel) bool { return l.Rank() >= other.Rank() }

// Raise returns whichever of the two levels establishes more. It is how a run
// accumulates its level as it goes: each fact learned can only ever raise it,
// which is what makes the final value the maximum of everything proven.
func (l ProofLevel) Raise(other ProofLevel) ProofLevel {
	if other.Rank() > l.Rank() {
		return other
	}
	return l
}

// ParseProofLevel reads a level written by a human or read back from storage.
// The empty string parses as ProofUnknown and is valid: it is how a run that
// predates this feature is represented.
func ParseProofLevel(s string) (ProofLevel, bool) {
	l := ProofLevel(s)
	if l == ProofUnknown || l.Recorded() {
		return l, true
	}
	return ProofUnknown, false
}

// String renders the level for a report or a log line.
func (l ProofLevel) String() string {
	if l == ProofUnknown {
		return "not recorded"
	}
	return string(l)
}

// Describe renders the level as the sentence an operator needs to read: what
// was established, and by omission what was not.
func (l ProofLevel) Describe() string {
	switch l {
	case ProofNone:
		return "nothing was verified inside the guest"
	case ProofBoot:
		return "only the boot was verified"
	case ProofService:
		return "the service was verified, the data was not"
	case ProofData:
		return "the service and its data were verified"
	default:
		return "the proof level was not recorded"
	}
}

// ProofCheck pairs a check's configuration with the result it produced, which
// is the only way to know what a drill established: the configuration says
// what the check would prove, and the result says whether it proved it.
type ProofCheck struct {
	Config CheckConfig
	Result CheckResult
}

// ProvenBy returns the level established by a set of executed checks: the
// highest level among the checks that actually passed.
//
// A check that failed, that could not run, or that never ran establishes
// nothing - so it contributes nothing here, in either direction. That is the
// whole point: a service check coming back bad tells you the service is bad,
// not that the service was verified.
//
// A check whose Proves is unrecorded also contributes nothing. Every check
// built from a plan carries a level (see plan.CheckSpec.ProvenLevel), so this
// only arises for a CheckConfig assembled by hand, and staying silent about
// one is the prudent direction to be wrong in.
func ProvenBy(checks []ProofCheck) ProofLevel {
	level := ProofNone
	for _, c := range checks {
		if c.Result.Status != CheckPass {
			continue
		}
		level = level.Raise(c.Config.Proves)
	}
	return level
}
