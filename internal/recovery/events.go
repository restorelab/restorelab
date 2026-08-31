package recovery

import (
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Event is one line of the live progress stream a caller (CLI, SSE handler,
// ...) renders while a run is in flight. Message is meant to be printed
// as-is, so it must stay short and human ("Restore completed in 1m24s"),
// never a stack trace or a raw provider error dump.
//
// The engine emits one Event when a step starts, one when it ends, and one
// per check result inside the run_checks step.
type Event struct {
	RunID   string
	At      time.Time
	State   core.RunState
	Step    string
	Status  core.StepStatus
	Message string
	Check   *core.CheckResult // set only for check-result events
	Err     string
}
