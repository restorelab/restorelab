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

// ErrDuplicate is returned when a write would collide with a name another
// row already holds.
var ErrDuplicate = errors.New("store: that name is already taken")

// ErrVersionConflict is returned when an update carried an expected version
// that is no longer the current one: somebody else wrote in between.
var ErrVersionConflict = errors.New("store: the plan changed since it was read")

// Plan is a recovery plan held in the catalogue.
//
// YAML is the document exactly as it was submitted, bytes included: comments
// and key order survive, so exporting a plan gives back what was written.
// The other fields are derived from it at write time and exist to list and
// filter; the text is what carries authority.
type Plan struct {
	ID          string
	Name        string
	Description string
	WorkloadID  string
	ProviderID  string
	YAML        string
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PlanFilter narrows a plan listing. A zero value lists every plan by name.
type PlanFilter struct {
	WorkloadID string
	Limit      int // 0 means DefaultListLimit
}

// Slot is one cron slot the scheduler has decided about.
//
// SlotAt is the instant the cron designated, and it is a key rather than a
// timestamp: it is always UTC, and it is what makes a scheduled drill
// impossible to queue twice.
type Slot struct {
	PlanID    string
	SlotAt    time.Time
	DecidedAt time.Time
	Outcome   SlotOutcome
	// Reason says why a slot was skipped, in words an operator can act on.
	// Empty for a queued slot.
	Reason string
	// RunID names the drill this slot queued. Empty for a skipped slot.
	RunID string
}

// SlotOutcome is what the scheduler decided about a slot.
//
// There are only two, and there is deliberately no "pending": a slot the
// scheduler has not decided about yet has no row at all. A third value would
// mean a slot could be half-claimed, which is the state this table exists to
// make unrepresentable.
type SlotOutcome string

const (
	SlotQueued  SlotOutcome = "queued"
	SlotSkipped SlotOutcome = "skipped"
)

// SlotFilter narrows a slot listing. A zero value lists every plan's slots,
// most recent first.
type SlotFilter struct {
	PlanID string
	// WorkloadID lists the slots of every plan covering this workload. A
	// machine can be covered by more than one plan, and "why was this
	// machine not tested" is a question about the machine rather than about
	// any one plan - so it is resolved in one join here rather than by a
	// caller fetching each plan's slots in turn.
	WorkloadID string
	Limit      int // 0 means DefaultListLimit
}

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
	ID       string
	PlanName string
	// PlanID is the stored plan this run came from, empty for an ad-hoc
	// drill. It is in the summary so a listing can group by plan without
	// loading every run.
	PlanID           string
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
	// Scopes is what this token may do. Empty means read only: a token
	// recorded before scopes existed must not gain a right by being read
	// back after a migration.
	Scopes []string
}

// Live reports whether the token has not been revoked.
func (t APIToken) Live() bool { return t.RevokedAt.IsZero() }

// Scopes an API token can hold.
const (
	// ScopeRead is everything the read-only API serves. It is the default,
	// and the only scope a token created before scopes existed can have.
	ScopeRead = "read"
	// ScopeOperate triggers, cancels and cleans up. It is the difference
	// between a dashboard that shows the fleet and one that can destroy and
	// recreate machines in it.
	ScopeOperate = "operate"
	// ScopeManage writes the catalogue: it creates, changes and deletes the
	// plans. It is deliberately not implied by operate. Triggering a drill
	// and deciding what a drill is are two different powers, and a token
	// handed to a dashboard so it can launch one has no business rewriting
	// the definition of what it launches.
	ScopeManage = "manage"
)

// Can reports whether the token holds a scope. Read is implied by every
// token, including one that only holds operate: an operator that cannot see
// what it just triggered would be a strange thing to build.
func (t APIToken) Can(scope string) bool {
	if scope == ScopeRead {
		return true
	}
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Session is a browser's authenticated connection to the API.
//
// It names a token; it carries no authority of its own. That is what makes
// revoking a token enough to end every session opened with it, and what stops
// a cookie from becoming a token in disguise: the scopes are read from the
// token on every request, never from the session.
//
// The secret is never stored. Hash is its SHA-256, exactly as for a token.
type Session struct {
	ID        string
	Hash      string
	TokenID   string
	CreatedAt time.Time
	ExpiresAt time.Time
	// UserAgent is a label, so a human can pick their own session out of a
	// list. Nothing depends on it and nothing should.
	UserAgent string
}

// ErrNoWork is returned by ClaimRun when the queue holds nothing to run.
var ErrNoWork = errors.New("store: no queued run to claim")

// ErrAlreadySettled is returned when an operation asks something of a run
// that has already reached a terminal state.
var ErrAlreadySettled = errors.New("store: run has already settled")

// QueuedRun is what a worker needs to execute a run it just claimed: the id
// to run under, and the plan exactly as it was when the run was queued.
type QueuedRun struct {
	ID               string
	PlanName         string
	ProviderID       string
	BackupProviderID string
	SourceWorkloadID string
	PlanSnapshot     string
	QueuedAt         time.Time
}

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
	// NotTerminal restricts the listing to runs that have not settled: the
	// queue, and whatever is running. The set of terminal states lives in
	// one place (terminalStates, which a test keeps in step with
	// core.RunState.Terminal), so a state added to core cannot quietly stop
	// being filtered here.
	NotTerminal bool
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

	// Enqueue records a run to be executed later, in state QUEUED. It is the
	// only way a run enters the system without a worker having started it.
	Enqueue(ctx context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error
	// SetState writes just the run's state, as the drill progresses.
	SetState(ctx context.Context, runID string, state core.RunState) error
	// SetRunError writes just the run's error message.
	//
	// It exists for the same reason SetState does: reconciliation settles a
	// run it never loaded as a *core.RecoveryRun, and UpdateRun would
	// overwrite the whole mutable row from a value it does not have. A run
	// failed because its worker died has to be able to say so.
	SetRunError(ctx context.Context, runID, message string) error
	// RequestCancel asks for a run to stop. It returns true when the run was
	// settled on the spot - a queued run nobody has claimed is cancelled
	// here, because nothing exists to clean up.
	RequestCancel(ctx context.Context, runID string, at time.Time) (bool, error)
	// CancelRequested reports whether a cancellation was asked for. The
	// worker polls it: the API and the worker may be different processes,
	// and the database is the only channel they share.
	CancelRequested(ctx context.Context, runID string) (bool, error)
	// ActiveRunForWorkload returns the id of this workload's queued or
	// running drill, or "" when it has none.
	ActiveRunForWorkload(ctx context.Context, workloadID string) (string, error)

	// ClaimRun takes ownership of the oldest queued run and returns what a
	// worker needs to execute it, or ErrNoWork.
	//
	// A run that has ever been claimed is never claimable again - the query
	// requires lease_owner to be null. That is the invariant that makes an
	// interrupted drill impossible to replay: reconciliation fails it, and
	// nothing can revive it.
	ClaimRun(ctx context.Context, owner string, lease time.Duration, now time.Time) (*QueuedRun, error)
	// RenewLease extends a lease held by owner. It fails when the caller is
	// not the holder.
	RenewLease(ctx context.Context, runID, owner string, until time.Time) error
	// FinishLease clears the expiry of a run that has ended. The owner is
	// kept: which worker ran a drill is part of its history.
	FinishLease(ctx context.Context, runID string) error
	// StaleRuns lists claimed runs that are not finished and whose lease has
	// expired: their worker died. They are never re-run, only failed.
	StaleRuns(ctx context.Context, now time.Time) ([]QueuedRun, error)
	// RunLease reports which worker holds a run and until when. An empty
	// owner means nobody has claimed it; a zero expiry means the run has
	// finished and released its lease, while keeping the owner - which
	// worker ran a drill is part of its history.
	//
	// StaleRuns cannot answer this: it skips every terminal run, which is
	// exactly what a finished drill is. Without this method the only way to
	// see a lease is to read the columns, which couples a caller to the
	// schema - and `GET /api/v1/queue` needs precisely this answer.
	RunLease(ctx context.Context, runID string) (owner string, expires time.Time, err error)

	// GetRun loads a run with its steps and checks. idOrPrefix accepts a
	// unique prefix of the id, the way git accepts a short sha. It returns
	// ErrNotFound, or ErrAmbiguous when a prefix matches several runs.
	GetRun(ctx context.Context, idOrPrefix string) (*core.RecoveryRun, error)
	// ListRuns returns summaries, most recent first.
	ListRuns(ctx context.Context, f Filter) ([]RunSummary, error)
	// LastRuns returns each of these workloads' most recent run, keyed by
	// workload id. A workload that has never been drilled is absent from the
	// map rather than present and empty: "never tested" and "tested, and it
	// went badly" are different answers, and a caller that cannot tell them
	// apart will render one as the other.
	//
	// It exists so a listing can say when each machine was last drilled with
	// one query instead of one per row. An empty id list is not an error.
	LastRuns(ctx context.Context, workloadIDs []string) (map[string]RunSummary, error)
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

	// CreateSession records a session and drops every session that has
	// already expired, in the same transaction. The sweep lives here because
	// this is the only statement that ever grows the table: it cleans itself
	// at exactly the rate it fills, without a goroutine to own.
	CreateSession(ctx context.Context, s Session, now time.Time) error
	// SessionByHash returns the session carrying this hash together with the
	// token it names - but only when the session has not expired and the
	// token is live. It returns ErrNotFound otherwise, without saying which
	// condition failed.
	//
	// The two answers come from one query on purpose. Revocation writes
	// revoked_at rather than deleting the row, so ON DELETE CASCADE does not
	// fire for it; a caller left to check that itself would eventually
	// forget, and a revoked credential would keep working for twelve hours.
	SessionByHash(ctx context.Context, hash string, now time.Time) (*Session, *APIToken, error)
	// DeleteSession removes a session. Removing one that is not there is not
	// an error: logging out twice is not a failure.
	DeleteSession(ctx context.Context, hash string) error
	// DeleteExpiredSessions removes every session that has expired. It is the
	// statement CreateSession runs as its sweep, exposed on its own so the
	// sweep can be tested and driven without opening a session.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)

	// CreatePlan records a new plan. Name is unique; a duplicate is
	// ErrDuplicate.
	CreatePlan(ctx context.Context, p Plan) error
	// UpdatePlan overwrites a plan and increments its version. expected > 0
	// requires the current version to match, and returns ErrVersionConflict
	// otherwise; 0 overwrites whatever is there. It does not report the new
	// version: the increment happens in SQL, so the only honest way to know
	// it is to read the row back.
	UpdatePlan(ctx context.Context, p Plan, expected int) error
	// GetPlan resolves a reference: an exact name first, then an exact id,
	// then a unique id prefix. ErrNotFound, or ErrAmbiguous when a prefix
	// matches more than one plan.
	GetPlan(ctx context.Context, ref string) (*Plan, error)
	// ListPlans returns the catalogue ordered by name.
	ListPlans(ctx context.Context, f PlanFilter) ([]Plan, error)
	// DeletePlan removes a plan. Its runs keep their name and snapshot and
	// only lose the link: ON DELETE SET NULL, so history reads identically
	// before and after.
	DeletePlan(ctx context.Context, ref string) error

	// ClaimSlot records the decision taken for one cron slot and, when that
	// decision is to drill, queues the run - in the same transaction.
	//
	// It returns ErrDuplicate when the slot has already been decided, and
	// that refusal is the whole safety story of scheduling. A drill is not
	// idempotent: replaying one allocates a second temporary workload and can
	// strand the first. The primary key on (plan_id, slot_at) is what makes
	// queueing the same slot twice impossible, whatever happens to the
	// process in between - which a lease could not do, because a lease
	// cannot cover the gap between writing the run and recording that it was
	// written.
	//
	// run and planYAML are nil and empty for a skipped slot; a queued slot
	// without a run is an error, and leaves nothing behind.
	ClaimSlot(ctx context.Context, slot Slot, run *core.RecoveryRun, planYAML string) error
	// LastSlot returns the most recent slot decided for this plan, or
	// ErrNotFound when it has never been scheduled. It is where the
	// scheduler resumes from: the next slot is the first one after this.
	LastSlot(ctx context.Context, planID string) (*Slot, error)
	// ListSlots returns decided slots, most recent first. Skipped slots are
	// included, because "why was this machine not tested" is the question
	// the table exists to answer.
	ListSlots(ctx context.Context, f SlotFilter) ([]Slot, error)

	// Describe names the engine and location, for `db status`. It must never
	// include a password.
	Describe() string

	Close() error
}
