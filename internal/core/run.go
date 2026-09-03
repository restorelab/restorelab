package core

import "time"

// RunState is the lifecycle state of a recovery run.
type RunState string

// The stages a drill moves through, in order, followed by the terminal ones.
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

	// RunInconclusive: the drill ran to the end - the backup was restored,
	// the workload booted - but a critical check could not be evaluated, so
	// no verdict about recovery can be drawn from it.
	//
	// This is a separate ending from FAILED on purpose. RestoreLab restores
	// onto a deliberately isolated network, so a check dialled from here can
	// come back silent because the operator has no route into that network,
	// not because anything is wrong with the backup. Calling that FAILED
	// would charge a workload's confidence score for the topology it is being
	// tested from, and a report nobody can trust is worth less than no report.
	RunInconclusive RunState = "INCONCLUSIVE"
)

// Terminal reports whether no further transition is expected.
func (s RunState) Terminal() bool {
	switch s {
	case RunSuccess, RunFailed, RunCancelled, RunCleanupFailed, RunInconclusive:
		return true
	}
	return false
}

// StepStatus is the outcome of a single workflow step.
type StepStatus string

// The states a step passes through. A step that never ran is pending or
// skipped; done and failed are terminal.
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

// The three verdicts a finished run can carry.
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

	// PlanID and PlanVersion say which stored plan produced this run, and in
	// which version. They are provenance and nothing else: the engine never
	// reads them, and a run triggered ad hoc has neither. What actually ran
	// is the plan snapshot the store holds beside the run - a plan edited or
	// deleted afterwards cannot change what a report says.
	PlanID      string
	PlanVersion int

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

	// ProofLevel is what this run established about the workload: the boot,
	// the service, the data, or nothing. It is not a verdict on the drill -
	// Result is - but on what the drill is entitled to claim. A run written
	// before RestoreLab recorded this carries ProofUnknown, which means "not
	// recorded" and never "nothing was proven".
	ProofLevel ProofLevel

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
