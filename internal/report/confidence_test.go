package report

import (
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func TestScore_NeverTested(t *testing.T) {
	c := Score(ConfidenceInput{Now: fixtureBase}, DefaultWeights())

	if c.Tested {
		t.Error("expected Tested = false")
	}
	if c.Score != 0 {
		t.Errorf("expected Score = 0, got %d", c.Score)
	}
	if len(c.Reasons) != 1 || c.Reasons[0] != "never tested" {
		t.Errorf(`expected Reasons = ["never tested"], got %v`, c.Reasons)
	}
}

// neutralSuccess returns a run that, alone, should incur zero penalties: it
// succeeded recently, met its RTO, and cleaned up.
func neutralSuccess(completedAt time.Time) *core.RecoveryRun {
	run := fixtureRunSuccess()
	run.State = core.RunSuccess
	run.Result = core.ResultSuccess
	run.CompletedAt = completedAt
	run.RTO = 2 * time.Minute
	run.RTOTarget = 5 * time.Minute
	run.CleanupDone = true
	return run
}

func TestScore_PerfectRecentSuccess(t *testing.T) {
	in := ConfidenceInput{
		LastRun:        neutralSuccess(fixtureBase.Add(-1 * time.Hour)),
		LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
		Now:            fixtureBase,
	}

	c := Score(in, DefaultWeights())

	if !c.Tested {
		t.Error("expected Tested = true")
	}
	if c.Score != 100 {
		t.Errorf("expected Score = 100, got %d (reasons: %v)", c.Score, c.Reasons)
	}
	if len(c.Reasons) != 0 {
		t.Errorf("expected no penalty reasons for a perfect run, got %v", c.Reasons)
	}
}

func TestScore_PenaltiesInIsolation(t *testing.T) {
	w := DefaultWeights()

	tests := []struct {
		name        string
		in          func() ConfidenceInput
		wantScore   int
		wantReason  string
		wantReasons int
	}{
		{
			name: "stale last success (age penalty)",
			in: func() ConfidenceInput {
				return ConfidenceInput{
					LastRun:        neutralSuccess(fixtureBase.Add(-20 * 24 * time.Hour)),
					LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
					Now:            fixtureBase,
				}
			},
			// 20 days - 7 grace days = 13 days over * 2/day = 26.
			wantScore:   74,
			wantReason:  "last successful test 20 days ago (-26)",
			wantReasons: 1,
		},
		{
			name: "last run failed",
			in: func() ConfidenceInput {
				failed := neutralSuccess(fixtureBase.Add(-1 * time.Hour))
				failed.Result = core.ResultFailed
				return ConfidenceInput{
					LastRun:        failed,
					History:        []*core.RecoveryRun{neutralSuccess(fixtureBase.Add(-2 * time.Hour))},
					LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
					Now:            fixtureBase,
				}
			},
			wantScore:   70,
			wantReason:  "last run failed (-30)",
			wantReasons: 1,
		},
		{
			name: "last run degraded",
			in: func() ConfidenceInput {
				degraded := neutralSuccess(fixtureBase.Add(-1 * time.Hour))
				degraded.Result = core.ResultDegraded
				return ConfidenceInput{
					LastRun:        degraded,
					History:        []*core.RecoveryRun{neutralSuccess(fixtureBase.Add(-2 * time.Hour))},
					LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
					Now:            fixtureBase,
				}
			},
			wantScore:   85,
			wantReason:  "last run degraded (-15)",
			wantReasons: 1,
		},
		{
			name: "RTO exceeded",
			in: func() ConfidenceInput {
				run := neutralSuccess(fixtureBase.Add(-1 * time.Hour))
				run.RTO = 6 * time.Minute
				run.RTOTarget = 5 * time.Minute
				return ConfidenceInput{
					LastRun:        run,
					LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
					Now:            fixtureBase,
				}
			},
			wantScore:   90,
			wantReason:  "RTO exceeded",
			wantReasons: 1,
		},
		{
			name: "cleanup failed",
			in: func() ConfidenceInput {
				run := neutralSuccess(fixtureBase.Add(-1 * time.Hour))
				run.State = core.RunCleanupFailed
				run.CleanupDone = false
				return ConfidenceInput{
					LastRun:        run,
					LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
					Now:            fixtureBase,
				}
			},
			wantScore:   85,
			wantReason:  "last run failed to clean up (-15)",
			wantReasons: 1,
		},
		{
			name: "no backup at all",
			in: func() ConfidenceInput {
				return ConfidenceInput{
					LastRun: neutralSuccess(fixtureBase.Add(-1 * time.Hour)),
					Now:     fixtureBase,
				}
			},
			wantScore:   50,
			wantReason:  "no backup available (-50)",
			wantReasons: 1,
		},
		{
			name: "stale backup",
			in: func() ConfidenceInput {
				return ConfidenceInput{
					LastRun:        neutralSuccess(fixtureBase.Add(-1 * time.Hour)),
					LatestBackupAt: fixtureBase.Add(-100 * time.Hour),
					Now:            fixtureBase,
				}
			},
			wantScore:   80,
			wantReason:  "latest backup is stale",
			wantReasons: 1,
		},
		{
			name: "failure rate over history",
			in: func() ConfidenceInput {
				ok := neutralSuccess(fixtureBase.Add(-2 * time.Hour))
				fail1 := neutralSuccess(fixtureBase.Add(-3 * time.Hour))
				fail1.Result = core.ResultFailed
				fail2 := neutralSuccess(fixtureBase.Add(-4 * time.Hour))
				fail2.Result = core.ResultFailed
				ok2 := neutralSuccess(fixtureBase.Add(-5 * time.Hour))
				return ConfidenceInput{
					LastRun:        neutralSuccess(fixtureBase.Add(-1 * time.Hour)),
					History:        []*core.RecoveryRun{ok, fail1, fail2, ok2},
					LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
					Now:            fixtureBase,
				}
			},
			// 2 of 4 failed = 50% * 30 = 15.
			wantScore:   85,
			wantReason:  "50% of the last 4 runs did not fully succeed (-15)",
			wantReasons: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Score(tt.in(), w)
			if c.Score != tt.wantScore {
				t.Errorf("Score = %d, want %d (reasons: %v)", c.Score, tt.wantScore, c.Reasons)
			}
			if len(c.Reasons) != tt.wantReasons {
				t.Errorf("len(Reasons) = %d, want %d: %v", len(c.Reasons), tt.wantReasons, c.Reasons)
			}
			found := false
			for _, r := range c.Reasons {
				if strings.Contains(r, tt.wantReason) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected a reason containing %q, got %v", tt.wantReason, c.Reasons)
			}
		})
	}
}

func failedRunAt(completedAt time.Time) *core.RecoveryRun {
	r := neutralSuccess(completedAt)
	r.Result = core.ResultFailed
	return r
}

// A cancelled run carries no verdict (recovery.Engine.markCancelled leaves
// Result empty). It must stay out of the failure rate entirely: counting it
// as a failure would mean that stopping a drill lowers the score that says
// whether recovery works, which would teach operators not to stop drills.
func TestScore_CancelledRunsAreNotFailures(t *testing.T) {
	cancelled := neutralSuccess(fixtureBase.Add(-3 * time.Hour))
	cancelled.State = core.RunCancelled
	cancelled.Result = ""

	in := ConfidenceInput{
		LastRun: neutralSuccess(fixtureBase.Add(-1 * time.Hour)),
		History: []*core.RecoveryRun{
			neutralSuccess(fixtureBase.Add(-2 * time.Hour)),
			cancelled,
		},
		LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
		Now:            fixtureBase,
	}

	c := Score(in, DefaultWeights())

	if c.Score != 100 {
		t.Errorf("Score = %d, want 100: a cancelled run is not evidence of failure (reasons: %v)",
			c.Score, c.Reasons)
	}
	if len(c.Reasons) != 0 {
		t.Errorf("a cancelled run produced a penalty: %v", c.Reasons)
	}
}

// The same run graded FAILED must still cost, so the test above is proving
// the empty verdict and not simply that the penalty is broken.
func TestScore_FailedRunsStillCountAgainstTheRate(t *testing.T) {
	failed := neutralSuccess(fixtureBase.Add(-3 * time.Hour))
	failed.State = core.RunFailed
	failed.Result = core.ResultFailed

	in := ConfidenceInput{
		LastRun: neutralSuccess(fixtureBase.Add(-1 * time.Hour)),
		History: []*core.RecoveryRun{
			neutralSuccess(fixtureBase.Add(-2 * time.Hour)),
			failed,
		},
		LatestBackupAt: fixtureBase.Add(-2 * time.Hour),
		Now:            fixtureBase,
	}

	c := Score(in, DefaultWeights())

	if c.Score == 100 {
		t.Error("a failed run in the history cost nothing")
	}
}

func TestScore_ClampsAtZero(t *testing.T) {
	failed := neutralSuccess(fixtureBase.Add(-1 * time.Hour))
	failed.Result = core.ResultFailed
	failed.State = core.RunCleanupFailed
	failed.CleanupDone = false
	failed.RTO = 20 * time.Minute
	failed.RTOTarget = 5 * time.Minute

	in := ConfidenceInput{
		LastRun: failed,
		History: []*core.RecoveryRun{
			failedRunAt(fixtureBase.Add(-48 * time.Hour)),
			failedRunAt(fixtureBase.Add(-72 * time.Hour)),
		},
		// No backup at all: the heaviest penalty.
		LatestBackupAt: time.Time{},
		Now:            fixtureBase,
	}

	c := Score(in, DefaultWeights())

	if c.Score != 0 {
		t.Errorf("expected Score to clamp at 0, got %d (reasons: %v)", c.Score, c.Reasons)
	}
	if !c.Tested {
		t.Error("expected Tested = true even at Score 0 (the workload was tested, it just scored badly)")
	}
	if len(c.Reasons) == 0 {
		t.Error("expected penalty reasons even when clamped to 0")
	}
}

func TestScore_ReasonsAlwaysExplainEveryPenalty(t *testing.T) {
	failed := neutralSuccess(fixtureBase.Add(-1 * time.Hour))
	failed.Result = core.ResultFailed
	failed.RTO = 20 * time.Minute
	failed.RTOTarget = 5 * time.Minute

	in := ConfidenceInput{
		LastRun:        failed,
		LatestBackupAt: time.Time{},
		Now:            fixtureBase,
	}

	c := Score(in, DefaultWeights())

	// Three penalties should have fired: no successful run on record, last
	// run failed, RTO exceeded, no backup. Every one must be explained.
	wantSubstrings := []string{
		"no successful recovery test on record",
		"last run failed",
		"RTO exceeded",
		"no backup available",
	}
	for _, want := range wantSubstrings {
		found := false
		for _, r := range c.Reasons {
			if strings.Contains(r, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a reason containing %q, got %v", want, c.Reasons)
		}
	}
}

// A drill that reached no verdict must cost a workload nothing.
//
// This is the whole point of the INCONCLUSIVE ending: a tcp: check dialled
// from a machine with no route into the isolated recovery network says
// nothing about the backup, and a score that drops because of it would make
// the dashboard lie about which workloads are at risk.
func TestScore_InconclusiveRunCostsNothing(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	success := &core.RecoveryRun{
		State:       core.RunSuccess,
		Result:      core.ResultSuccess,
		CompletedAt: now.Add(-2 * time.Hour),
	}
	// The same history, but the most recent drill could not be evaluated: it
	// ran long (five minutes of timeouts) against a one-minute target.
	inconclusive := &core.RecoveryRun{
		State:       core.RunInconclusive,
		Result:      "",
		CompletedAt: now.Add(-time.Hour),
		RTO:         5 * time.Minute,
		RTOTarget:   time.Minute,
	}

	w := DefaultWeights()
	before := Score(ConfidenceInput{
		Now: now, LastRun: success, History: []*core.RecoveryRun{success},
		LatestBackupAt: now.Add(-time.Hour),
	}, w)
	after := Score(ConfidenceInput{
		Now: now, LastRun: inconclusive, History: []*core.RecoveryRun{inconclusive, success},
		LatestBackupAt: now.Add(-time.Hour),
	}, w)

	if after.Score != before.Score {
		t.Errorf("an inconclusive drill moved the score from %d to %d: %v",
			before.Score, after.Score, after.Reasons)
	}
	for _, r := range after.Reasons {
		if strings.Contains(r, "RTO") || strings.Contains(r, "failed") {
			t.Errorf("unexpected penalty from a run that reached no verdict: %q", r)
		}
	}
}
