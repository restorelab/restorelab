// Package store persists recovery runs so that RestoreLab can answer
// questions about time: is this workload's RTO degrading, when was it last
// validated, is this check failing for the first time.
//
// Everything here is best-effort from the caller's point of view. A drill is
// a destructive operation on a production cluster; a locked database, a full
// disk or a corrupt file must never abort it. The journal does not command
// the operation.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// ErrNotFound is returned when no run matches the given id or prefix.
var ErrNotFound = errors.New("store: run not found")

// ErrAmbiguous is returned when an id prefix matches more than one run.
var ErrAmbiguous = errors.New("store: id prefix matches more than one run")

// Event is one line of a run's progress stream, as the engine emitted it.
//
// It mirrors recovery.Event deliberately rather than importing it: store must
// stay out of the recovery package's dependency graph, so that persistence
// can never appear in the destructive path. The CLI converts between the two.
type Event struct {
	Seq     int64
	At      time.Time
	State   core.RunState
	Step    string
	Status  core.StepStatus
	Message string
	Check   *core.CheckResult
	Err     string
}

// RunSummary is the row a listing shows. It deliberately omits steps, checks
// and events: a listing of two hundred runs must not load them.
type RunSummary struct {
	ID               string
	PlanName         string
	SourceWorkloadID string
	SourceName       string
	State            core.RunState
	Result           core.RunResult
	StartedAt        time.Time
	CompletedAt      time.Time
	RTO              time.Duration
	CleanupDone      bool
}

// Filter narrows a listing. A zero value lists the most recent runs.
type Filter struct {
	WorkloadID string
	State      core.RunState
	Result     core.RunResult
	Since      time.Time
	Limit      int // 0 means DefaultListLimit
}

// DefaultListLimit caps a listing that did not ask for a size.
const DefaultListLimit = 50

// Store records recovery runs and reads them back.
//
// Implementations must never panic and must honour ctx. Callers treat every
// returned error as a warning, never as a reason to stop a drill.
type Store interface {
	// CreateRun records a run that has just started. planYAML is the plan as
	// it was at that moment, stored verbatim: plans become editable in phase
	// B, and a report must keep describing what was actually checked.
	CreateRun(ctx context.Context, run *core.RecoveryRun, planYAML string) error
	// UpdateRun overwrites the mutable fields of a run already created.
	UpdateRun(ctx context.Context, run *core.RecoveryRun) error
	// SaveStep records one step at position seq, replacing any previous
	// value at that position.
	SaveStep(ctx context.Context, runID string, seq int, step core.Step) error
	// SaveCheck records one check result at position seq, replacing any
	// previous value at that position.
	SaveCheck(ctx context.Context, runID string, seq int, check core.CheckResult) error
	// AppendEvent records one progress event. ev.Seq orders the stream.
	AppendEvent(ctx context.Context, runID string, ev Event) error

	// GetRun loads a run with its steps and checks. idOrPrefix accepts a
	// unique prefix of the id, the way git accepts a short sha. It returns
	// ErrNotFound, or ErrAmbiguous when a prefix matches several runs.
	GetRun(ctx context.Context, idOrPrefix string) (*core.RecoveryRun, error)
	// ListRuns returns summaries, most recent first.
	ListRuns(ctx context.Context, f Filter) ([]RunSummary, error)
	// Events returns a run's events with a seq strictly greater than
	// afterSeq, in order. Phase B's SSE replays from here on reconnection.
	Events(ctx context.Context, runID string, afterSeq int64) ([]Event, error)

	// Describe names the engine and location, for `db status`. It must never
	// include a password.
	Describe() string

	Close() error
}
