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

// ErrNoHistory is returned when an operation needs a real database and there
// is none: the Noop store cannot invent an API token, and pretending to
// store one would hand an operator a credential that authenticates nothing.
//
// It is deliberately distinct from ErrNotFound. The API turns ErrNotFound
// into 401 ("your token is not valid") and ErrNoHistory into 503 ("our
// database is missing") - telling a client its credentials are wrong when
// the fault is ours is the classic way to send someone hunting for hours.
var ErrNoHistory = errors.New("store: no history database is configured")

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
	// RTOTarget is what the plan asked for. It is in the summary because the
	// confidence score needs it, and grading a workload must not mean loading
	// every full run it ever had.
	RTOTarget   time.Duration
	CleanupDone bool
}

// Position is a run's place in the listing order: when it started, then its
// id to break ties. It is what a page cursor carries.
type Position struct {
	StartedAt time.Time
	ID        string
}

// APIToken is a credential the read-only HTTP API accepts.
//
// The secret is never stored: Hash is its SHA-256, hex encoded. A leaked
// database therefore hands an attacker nothing usable, and the only moment
// the secret exists is when `token create` prints it.
type APIToken struct {
	ID         string
	Name       string
	Hash       string
	CreatedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
}

// Live reports whether the token has not been revoked.
func (t APIToken) Live() bool { return t.RevokedAt.IsZero() }

// Filter narrows a listing. A zero value lists the most recent runs.
type Filter struct {
	WorkloadID string
	State      core.RunState
	Result     core.RunResult
	Since      time.Time
	Limit      int // 0 means DefaultListLimit
	// After continues a listing from a previous page's last row: only runs
	// strictly older than this position come back.
	//
	// A keyset, never an OFFSET. A drill inserted while a dashboard pages
	// shifts every OFFSET after it, which either skips a row or shows one
	// twice - and the reader has no way to notice either.
	After *Position
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
	// SetTempWorkload records the temporary workload a run has just created,
	// as soon as it exists.
	//
	// It writes those two columns and nothing else: UpdateRun overwrites the
	// whole mutable row from a *core.RecoveryRun, and the caller here has only
	// an event. A run that dies before finishing must still leave the
	// database able to name what it left on the cluster.
	SetTempWorkload(ctx context.Context, runID, tempWorkloadID, node string) error
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

	// CreateToken records a new API token. Name and Hash are unique; a
	// duplicate of either is an error.
	CreateToken(ctx context.Context, t APIToken) error
	// TokenByHash returns the live token carrying this hash. It returns
	// ErrNotFound for an unknown or revoked hash, and ErrNoHistory when
	// there is no database to ask.
	TokenByHash(ctx context.Context, hash string) (*APIToken, error)
	// ListTokens returns every token, revoked ones included, oldest first.
	ListTokens(ctx context.Context) ([]APIToken, error)
	// RevokeToken marks the named token revoked at at. It returns
	// ErrNotFound when no live token carries that name.
	RevokeToken(ctx context.Context, name string, at time.Time) error
	// TouchToken records that a token was used at at. Callers throttle it:
	// an exact counter would cost one write per request for something nobody
	// reads to the second.
	TouchToken(ctx context.Context, id string, at time.Time) error

	// Describe names the engine and location, for `db status`. It must never
	// include a password.
	Describe() string

	Close() error
}
