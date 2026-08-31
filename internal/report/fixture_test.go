package report

import (
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// fixtureBase is the timestamp fixtures anchor to, so text/JSON assertions
// on formatted timestamps stay deterministic.
var fixtureBase = time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC)

func fixtureBackup() *core.Backup {
	return &core.Backup{
		ID:         "pbs:backup/vm/101/2026-08-31T03:00:00Z",
		WorkloadID: "101",
		ProviderID: "pbs-main",
		Datastore:  "main",
		Node:       "pve01",
		CreatedAt:  fixtureBase.Add(-2*time.Hour - 4*time.Minute),
		SizeBytes:  4_509_715_660, // ~4.2 GiB
		Protected:  true,
		Encrypted:  true,
		Verified:   core.VerificationOK,
		Format:     "qcow2",
	}
}

func fixtureSteps() []core.Step {
	mk := func(name string, dur time.Duration, status core.StepStatus) core.Step {
		start := fixtureBase
		return core.Step{
			Name:        name,
			State:       core.RunRestoring,
			Status:      status,
			StartedAt:   start,
			CompletedAt: start.Add(dur),
			Duration:    dur,
		}
	}
	return []core.Step{
		mk("Backup discovered", 400*time.Millisecond, core.StepDone),
		mk("Environment prepared", 200*time.Millisecond, core.StepDone),
		mk("Restore completed", 84*time.Second, core.StepDone),
		mk("VM started", 2100*time.Millisecond, core.StepDone),
		mk("Guest reachable", 29*time.Second, core.StepDone),
		mk("Checks", 11*time.Second, core.StepDone),
		mk("Cleanup", 8300*time.Millisecond, core.StepDone),
	}
}

func fixtureChecksPassing() []core.CheckResult {
	return []core.CheckResult{
		{
			Name:      "TCP 22",
			Type:      "tcp",
			Status:    core.CheckPass,
			Duration:  51 * time.Millisecond,
			Attempts:  1,
			Message:   "connected to 10.99.0.14:22",
			StartedAt: fixtureBase,
		},
		{
			Name:      "HTTP health",
			Type:      "http",
			Status:    core.CheckPass,
			Duration:  310 * time.Millisecond,
			Attempts:  1,
			Message:   "status 200",
			StartedAt: fixtureBase,
		},
	}
}

// httpHealthFailMessage is the exact message the "failed" fixture's failing
// check carries; tests assert this string appears verbatim in every
// renderer's output.
const httpHealthFailMessage = "expected status 200, got 502"

func fixtureChecksWithFailure() []core.CheckResult {
	checks := fixtureChecksPassing()
	checks[1].Status = core.CheckFail
	checks[1].Duration = 1200 * time.Millisecond
	checks[1].Message = httpHealthFailMessage
	return checks
}

// newBaseRun builds a realistic, otherwise-successful recovery run for
// "postgres-prod" that every fixture variant starts from.
func newBaseRun() *core.RecoveryRun {
	return &core.RecoveryRun{
		ID:       "b3f1a2c4",
		PlanName: "postgres-prod",

		ProviderID:       "pve",
		BackupProviderID: "pbs-main",

		SourceWorkloadID: "101",
		SourceName:       "postgres-prod",
		TempWorkloadID:   "9101",
		TempName:         "restorelab-101-20260831050000",
		Node:             "pve01",

		Backup: fixtureBackup(),

		State:  core.RunSuccess,
		Result: core.ResultSuccess,

		StartedAt:   fixtureBase,
		CompletedAt: fixtureBase.Add(2*time.Minute + 6*time.Second),

		Steps:  fixtureSteps(),
		Checks: fixtureChecksPassing(),

		RTO:       2*time.Minute + 6*time.Second,
		RTOTarget: 5 * time.Minute,

		CleanupDone: true,
	}
}

// fixtureRunSuccess is a fully successful run: every step done, every check
// passing, RTO within target.
func fixtureRunSuccess() *core.RecoveryRun {
	return newBaseRun()
}

// fixtureRunFailed mirrors the example in the report spec: the HTTP health
// check fails, which fails the whole run even though RTO was met.
func fixtureRunFailed() *core.RecoveryRun {
	run := newBaseRun()
	run.State = core.RunFailed
	run.Result = core.ResultFailed
	run.Checks = fixtureChecksWithFailure()
	run.Err = "critical check failed: HTTP health"
	return run
}

// fixtureRunDegraded has a non-critical check failure and an exceeded RTO,
// but the workload did come back up.
func fixtureRunDegraded() *core.RecoveryRun {
	run := newBaseRun()
	run.State = core.RunSuccess
	run.Result = core.ResultDegraded
	run.Checks = fixtureChecksWithFailure()
	run.RTO = 6 * time.Minute
	run.CompletedAt = run.StartedAt.Add(run.RTO)
	return run
}

// fixtureRunCleanupFailed succeeded at recovering the workload but left an
// orphaned temporary resource behind.
func fixtureRunCleanupFailed() *core.RecoveryRun {
	run := newBaseRun()
	run.State = core.RunCleanupFailed
	run.Result = core.ResultSuccess
	run.CleanupDone = false

	steps := make([]core.Step, len(run.Steps))
	copy(steps, run.Steps)
	last := len(steps) - 1
	steps[last].Status = core.StepFailed
	steps[last].Err = "orphaned volume rl-101-disk-0 could not be removed"
	run.Steps = steps

	return run
}

// fixtureRunWithSkippedStep is a success run with one extra step that was
// skipped, for renderer coverage of the "skipped" status.
func fixtureRunWithSkippedStep() *core.RecoveryRun {
	run := newBaseRun()
	steps := make([]core.Step, len(run.Steps))
	copy(steps, run.Steps)
	steps = append(steps, core.Step{
		Name:      "Post-restore snapshot",
		State:     core.RunGeneratingReport,
		Status:    core.StepSkipped,
		StartedAt: run.CompletedAt,
		Message:   "skipped: no snapshot policy configured",
	})
	run.Steps = steps
	return run
}
