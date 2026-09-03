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

	// TempWorkloadID and Node name the temporary workload the run reserved
	// for itself, and the node it lives on. They are empty only on the
	// events emitted before prepare_environment allocated that identity, and
	// set on EVERY event from then on, including the one that opens the
	// restore step, which is the first step that creates anything on the
	// cluster.
	//
	// That ordering is the whole point, not an implementation detail:
	// nothing is created before a listener (the run recorder writing to the
	// database, an SSE client) has been told how to find it. A process
	// killed mid-drill therefore leaves an orphan that can be named and
	// reconciled, instead of an anonymous VM nobody can tie back to a run.
	TempWorkloadID string
	Node           string
}

// eventFor starts an Event pre-filled with the identity of the run and of
// whatever temporary workload it has reserved so far. Every emit in the
// engine goes through it, so no event can quietly forget those fields.
func eventFor(run *core.RecoveryRun) Event {
	return Event{
		RunID:          run.ID,
		TempWorkloadID: run.TempWorkloadID,
		Node:           run.Node,
	}
}
