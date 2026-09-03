package report

import (
	"fmt"
	"math"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// ConfidenceInput is everything Score needs to grade a workload's recovery
// posture.
type ConfidenceInput struct {
	// LastRun is the most recent recovery run, or nil when the workload has
	// never been tested.
	LastRun *core.RecoveryRun
	// History is prior runs, newest first. Optional: a nil or empty slice
	// simply disables the failure-rate penalty.
	History []*core.RecoveryRun
	// LatestBackupAt is the creation time of the most recent backup RestoreLab
	// knows about for this workload, independent of whether it has ever been
	// test-restored. Zero means no backup exists at all.
	LatestBackupAt time.Time
	// Now is the reference time penalties are computed against. Callers
	// should pass time.Now(); it is a parameter so tests are deterministic.
	Now time.Time
}

// ConfidenceWeights configures every penalty Score can apply. All fields are
// public so an operator can retune the score without forking the package.
// DefaultWeights returns sane defaults.
type ConfidenceWeights struct {
	// AgeGraceDays is how many days since the last successful run incur no
	// penalty at all.
	AgeGraceDays float64
	// AgePerDayPenalty is the penalty per day past AgeGraceDays.
	AgePerDayPenalty float64
	// AgeMaxPenalty caps the age penalty regardless of how stale the last
	// successful run is.
	AgeMaxPenalty int

	// NoSuccessPenalty applies when the workload has been tested at least
	// once but no run on record ever succeeded.
	NoSuccessPenalty int

	// LastRunFailedPenalty applies when the most recent run's Result is
	// FAILED.
	LastRunFailedPenalty int
	// LastRunDegradedPenalty applies when the most recent run's Result is
	// DEGRADED.
	LastRunDegradedPenalty int

	// RTOExceededPenalty applies when the most recent run blew its RTO
	// target.
	RTOExceededPenalty int

	// CleanupFailedPenalty applies when the most recent run left cleanup in
	// a failed state (core.RunCleanupFailed): a run that cannot reliably
	// tear itself down erodes confidence in the whole process.
	CleanupFailedPenalty int

	// NoBackupPenalty applies when LatestBackupAt is zero: there is nothing
	// to restore at all.
	NoBackupPenalty int

	// BackupStaleAfter is how old the latest backup can be before it is
	// considered stale.
	BackupStaleAfter time.Duration
	// BackupStalePenalty applies once the latest backup is older than
	// BackupStaleAfter.
	BackupStalePenalty int

	// FailureRateMaxPenalty is the penalty applied when 100% of the runs in
	// History did not fully succeed; it scales linearly with the observed
	// failure rate.
	FailureRateMaxPenalty int

	// ProofNoneCap, ProofBootCap and ProofServiceCap are the ceilings each
	// proof level puts on the score: whatever the penalties come to, a
	// workload cannot score higher than what its drills actually established.
	//
	// A ceiling rather than another penalty, because the statement is not
	// "this drill went worse" but "the score cannot exceed what was proven".
	// Penalties accumulate and can be tuned away; a ceiling cannot be argued
	// with, and a drill that only ran `hostname` must not be able to display
	// 100 however well it went. There is no cap for core.ProofData: a drill
	// that verified the data has nothing left to be modest about.
	//
	// A cap of zero or less means no cap, exactly as a penalty of zero or
	// less means no penalty.
	ProofNoneCap    int
	ProofBootCap    int
	ProofServiceCap int
}

// DefaultWeights returns RestoreLab's default confidence-scoring weights.
// These are product defaults, not the result of any statistical model. See
// the Confidence doc comment.
func DefaultWeights() ConfidenceWeights {
	return ConfidenceWeights{
		AgeGraceDays:     7,
		AgePerDayPenalty: 2,
		AgeMaxPenalty:    40,

		NoSuccessPenalty: 35,

		LastRunFailedPenalty:   30,
		LastRunDegradedPenalty: 15,

		RTOExceededPenalty: 10,

		CleanupFailedPenalty: 15,

		NoBackupPenalty: 50,

		BackupStaleAfter:   48 * time.Hour,
		BackupStalePenalty: 20,

		FailureRateMaxPenalty: 30,

		ProofNoneCap:    40,
		ProofBootCap:    60,
		ProofServiceCap: 85,
	}
}

// ProofCap returns the ceiling level puts on a score, or 0 for no ceiling.
// It is exported because the API answers with the ceiling beside the score:
// a client that shows 60 has to be able to say what 60 was measured against.
func (w ConfidenceWeights) ProofCap(level core.ProofLevel) int {
	switch level {
	case core.ProofNone:
		return w.ProofNoneCap
	case core.ProofBoot:
		return w.ProofBootCap
	case core.ProofService:
		return w.ProofServiceCap
	default:
		// core.ProofData earns the full range, and an unrecorded level caps
		// nothing at all: a run from before RestoreLab wrote this down is not
		// evidence that nothing was proven, and treating it as such would
		// mark down every workload in an existing installation on the
		// strength of a fact nobody ever recorded.
		return 0
	}
}

// Confidence is the Recovery Confidence result: a product indicator meant to
// give an operator a fast, honest read on "would this recovery actually
// work today", not a scientific or statistically calibrated measure. Treat
// Score as a configurable heuristic: the Reasons are the actual value,
// since they say *why* the number is what it is; the bare integer alone
// should never be presented without them.
type Confidence struct {
	// Score is 0..100, or 0 with Tested==false when the workload has never
	// been recovery-tested. UIs should render that case as "--", not "0%".
	Score int
	// Reasons explains every penalty that was applied, most human-readable
	// first, e.g. "last successful test 12 days ago (-20)". Empty when no
	// penalty applied (a perfect score).
	Reasons []string
	// Tested reports whether the workload has ever had a recovery run.
	Tested bool
	// Proof is what the most recent drill that reached a verdict actually
	// established. core.ProofUnknown when no such drill recorded one, which
	// is what runs from before the level existed carry.
	Proof core.ProofLevel
}

// Score grades a workload's recovery posture from in using weights w. It
// never panics on partial input: a nil LastRun, an empty History and a zero
// LatestBackupAt are all valid and handled explicitly.
func Score(in ConfidenceInput, w ConfidenceWeights) Confidence {
	if in.LastRun == nil {
		return Confidence{Score: 0, Tested: false, Reasons: []string{"never tested"}}
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	score := 100
	var reasons []string

	apply := func(penalty int, reason string) {
		if penalty <= 0 {
			return
		}
		score -= penalty
		reasons = append(reasons, fmt.Sprintf("%s (-%d)", reason, penalty))
	}

	// Age of the last successful run (graded decay).
	if success := mostRecentSuccess(in); success != nil {
		age := now.Sub(success.CompletedAt)
		days := age.Hours() / 24
		if over := days - w.AgeGraceDays; over > 0 {
			penalty := int(math.Round(over * w.AgePerDayPenalty))
			if penalty > w.AgeMaxPenalty {
				penalty = w.AgeMaxPenalty
			}
			apply(penalty, fmt.Sprintf("last successful test %d days ago", int(math.Round(days))))
		}
	} else {
		apply(w.NoSuccessPenalty, "no successful recovery test on record")
	}

	// Most recent run outcome.
	switch in.LastRun.Result {
	case core.ResultFailed:
		apply(w.LastRunFailedPenalty, "last run failed")
	case core.ResultDegraded:
		apply(w.LastRunDegradedPenalty, "last run degraded")
	}

	// RTO, but only for a run that reached a verdict.
	//
	// A run with an empty Result proves nothing (a cancelled drill, or one
	// whose checks could not be evaluated), and its elapsed time proves less
	// than nothing: an inconclusive run's clock is dominated by whatever timed
	// out. Charging it for missing the RTO target would penalise a workload
	// twice over for a network its backup has nothing to do with.
	if in.LastRun.Result != "" && in.LastRun.RTOExceeded() {
		apply(w.RTOExceededPenalty, fmt.Sprintf(
			"RTO exceeded (%s actual vs %s target)",
			FormatDuration(in.LastRun.RTO), FormatDuration(in.LastRun.RTOTarget)))
	}

	// Cleanup.
	if in.LastRun.State == core.RunCleanupFailed {
		apply(w.CleanupFailedPenalty, "last run failed to clean up")
	}

	// Backup posture.
	if in.LatestBackupAt.IsZero() {
		apply(w.NoBackupPenalty, "no backup available")
	} else if age := now.Sub(in.LatestBackupAt); age > w.BackupStaleAfter {
		apply(w.BackupStalePenalty, fmt.Sprintf("latest backup is stale (%s old)", FormatDuration(age)))
	}

	// Failure rate over history.
	if len(in.History) > 0 {
		failed := 0
		counted := 0
		for _, r := range in.History {
			if r == nil {
				continue
			}
			// A run that reached no verdict is not evidence either way, so it
			// is not in the denominator. A cancelled drill carries an empty
			// Result on purpose (see recovery.Engine.markCancelled): counting
			// it as a failure would let stopping a drill lower the very score
			// that says whether recovery works.
			if r.Result == "" {
				continue
			}
			counted++
			if r.Result != core.ResultSuccess {
				failed++
			}
		}
		if counted > 0 {
			rate := float64(failed) / float64(counted)
			penalty := int(math.Round(rate * float64(w.FailureRateMaxPenalty)))
			apply(penalty, fmt.Sprintf("%.0f%% of the last %d runs did not fully succeed", rate*100, counted))
		}
	}

	// The ceiling, applied last: every penalty above says how well the drill
	// went, and this says what the drill was entitled to claim in the first
	// place. A workload drilled daily, on time, with a fresh backup and a
	// check that prints its hostname has earned every one of those points and
	// still proven only that the kernel boots.
	proof := provenLevel(in)
	if ceiling := w.ProofCap(proof); ceiling > 0 && score > ceiling {
		score = ceiling
		reasons = append(reasons, fmt.Sprintf("%s (capped at %d)", proof.Describe(), ceiling))
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	return Confidence{Score: score, Tested: true, Reasons: reasons, Proof: proof}
}

// provenLevel returns the proof level of the newest run that reached a
// verdict and recorded one.
//
// A run that reached no verdict is skipped, for the same reason it is skipped
// in the failure rate: a drill still in flight, or one somebody cancelled,
// establishes nothing yet. Without this a workload with a spotless history
// would collapse to the "nothing verified" ceiling for as long as its next
// drill is running - the score would swing on the clock rather than on the
// evidence.
func provenLevel(in ConfidenceInput) core.ProofLevel {
	runs := make([]*core.RecoveryRun, 0, len(in.History)+1)
	runs = append(runs, in.LastRun)
	runs = append(runs, in.History...)
	for _, r := range runs {
		if r == nil || r.Result == "" {
			continue
		}
		if r.ProofLevel.Recorded() {
			return r.ProofLevel
		}
	}
	return core.ProofUnknown
}

// mostRecentSuccess returns the newest successful run among LastRun and
// History, or nil if none succeeded.
func mostRecentSuccess(in ConfidenceInput) *core.RecoveryRun {
	if in.LastRun != nil && in.LastRun.Result == core.ResultSuccess {
		return in.LastRun
	}
	for _, r := range in.History {
		if r != nil && r.Result == core.ResultSuccess {
			return r
		}
	}
	return nil
}
