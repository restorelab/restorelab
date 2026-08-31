package core

import "time"

// RunState is the lifecycle state of a recovery run.
type RunState string

const (
	RunQueued            RunState = "QUEUED"
	RunDiscoveringBackup RunState = "DISCOVERING_BACKUP"
	RunPreparing         RunState = "PREPARING_ENVIRONMENT"
	RunRestoring         RunState = "RESTORING"
	RunStarting          RunState = "STARTING"
	RunWaitingForGuest   RunState = "WAITING_FOR_GUEST"
	RunRunningChecks     RunState = "RUNNING_CHECKS"
	RunGeneratingReport  RunState = "GENERATING_REPORT"
	RunCleaningUp        RunState = "CLEANING_UP"
	RunSuccess           RunState = "SUCCESS"
	RunFailed            RunState = "FAILED"
	RunCancelled         RunState = "CANCELLED"
	RunCleanupFailed     RunState = "CLEANUP_FAILED"
)

// Terminal reports whether no further transition is expected.
func (s RunState) Terminal() bool {
	switch s {
	case RunSuccess, RunFailed, RunCancelled, RunCleanupFailed:
		return true
	}
	return false
}

// StepStatus is the outcome of a single workflow step.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// Step is one executed stage of a recovery run, kept for the timeline and the
// report. Durations are what the RTO breakdown is computed from.
type Step struct {
	Name        string
	State       RunState
	Status      StepStatus
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
	Message     string
	Err         string
	Details     map[string]any
}

// RunResult is the overall verdict of a recovery run.
type RunResult string

const (
	ResultSuccess  RunResult = "SUCCESS"
	ResultDegraded RunResult = "DEGRADED" // recovered, but a non-critical check failed or RTO was exceeded
	ResultFailed   RunResult = "FAILED"
)

// RecoveryRun is a single execution of a recovery plan. It is the unit the
// report, the timeline and the confidence score are built from.
type RecoveryRun struct {
	ID       string
	PlanName string

	ProviderID       string
	BackupProviderID string

	SourceWorkloadID string
	SourceName       string
	TempWorkloadID   string
	TempName         string
	Node             string

	Backup *Backup

	State  RunState
	Result RunResult

	StartedAt   time.Time
	CompletedAt time.Time

	Steps  []Step
	Checks []CheckResult

	RTO       time.Duration
	RTOTarget time.Duration

	CleanupDone bool
	Err         string
}

// RTOExceeded reports whether the run blew past its configured RTO target.
func (r RecoveryRun) RTOExceeded() bool {
	return r.RTOTarget > 0 && r.RTO > r.RTOTarget
}

// StepDuration returns the recorded duration of a named step.
func (r RecoveryRun) StepDuration(name string) time.Duration {
	for _, s := range r.Steps {
		if s.Name == name {
			return s.Duration
		}
	}
	return 0
}

// FailedChecks returns every check that did not pass.
func (r RecoveryRun) FailedChecks() []CheckResult {
	var out []CheckResult
	for _, c := range r.Checks {
		if !c.OK() {
			out = append(out, c)
		}
	}
	return out
}
