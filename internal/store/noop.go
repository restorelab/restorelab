package store

import (
	"context"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// Noop satisfies Store by doing nothing at all.
//
// It is wired whenever there is no usable database: none configured, the file
// unreadable, the server unreachable, the schema behind. Callers therefore
// never carry an "if store != nil" branch, and the guarantee that a missing
// database changes nothing is testable, because the same code path runs in
// both cases.
type Noop struct{}

var _ Store = Noop{}

func (Noop) CreateRun(context.Context, *core.RecoveryRun, string) error     { return nil }
func (Noop) UpdateRun(context.Context, *core.RecoveryRun) error             { return nil }
func (Noop) SetTempWorkload(context.Context, string, string, string) error  { return nil }
func (Noop) SaveStep(context.Context, string, int, core.Step) error         { return nil }
func (Noop) SaveCheck(context.Context, string, int, core.CheckResult) error { return nil }
func (Noop) AppendEvent(context.Context, string, Event) error               { return nil }
func (Noop) ListRuns(context.Context, Filter) ([]RunSummary, error)         { return nil, nil }
func (Noop) LastRuns(context.Context, []string) (map[string]RunSummary, error) {
	return map[string]RunSummary{}, nil
}
func (Noop) Events(context.Context, string, int64) ([]Event, error) { return nil, nil }
func (Noop) Describe() string                                       { return "no database (history is not being recorded)" }
func (Noop) Close() error                                           { return nil }

// GetRun reports ErrNotFound rather than nil: a caller asking for a specific
// run must be told it does not have it, not handed an empty one.
func (Noop) GetRun(context.Context, string) (*core.RecoveryRun, error) {
	return nil, ErrNotFound
}

// The token methods are the one place Noop cannot do nothing quietly.
//
// Everywhere else, a missing database costs history and nothing more. A
// token that appears to have been created but authenticates nothing would be
// worse than an error: the operator would paste it into a dashboard and
// spend an afternoon on a 401. So these say plainly that there is no
// database, and `token create` refuses.
func (Noop) CreateToken(context.Context, APIToken) error { return ErrNoHistory }

func (Noop) TokenByHash(context.Context, string) (*APIToken, error) {
	return nil, ErrNoHistory
}

func (Noop) ListTokens(context.Context) ([]APIToken, error) { return nil, ErrNoHistory }

func (Noop) RevokeToken(context.Context, string, time.Time) error { return ErrNoHistory }

// TouchToken is the exception: it is bookkeeping about a token that was
// already accepted, so failing silently is right.
func (Noop) TouchToken(context.Context, string, time.Time) error { return nil }

// The session methods refuse for the reason the token ones do. A dashboard
// cannot open a session without a database, and saying so is more useful than
// handing back a cookie that names a row nobody wrote.
func (Noop) CreateSession(context.Context, Session, time.Time) error { return ErrNoHistory }

func (Noop) SessionByHash(context.Context, string, time.Time) (*Session, *APIToken, error) {
	return nil, nil, ErrNoHistory
}

func (Noop) DeleteSession(context.Context, string) error { return ErrNoHistory }

func (Noop) DeleteExpiredSessions(context.Context, time.Time) (int64, error) {
	return 0, ErrNoHistory
}

// The plan methods refuse for the same reason the token ones do: a plan the
// caller believes stored, and that is not, is worse than an error. `plan
// apply` must say it could not apply anything.
func (Noop) CreatePlan(context.Context, Plan) error      { return ErrNoHistory }
func (Noop) UpdatePlan(context.Context, Plan, int) error { return ErrNoHistory }
func (Noop) DeletePlan(context.Context, string) error    { return ErrNoHistory }

func (Noop) GetPlan(context.Context, string) (*Plan, error) { return nil, ErrNoHistory }

func (Noop) ListPlans(context.Context, PlanFilter) ([]Plan, error) { return nil, ErrNoHistory }

// The queue methods follow the same rule as the tokens: silence where a
// missing database only costs history, an error where succeeding would be a
// lie the caller acts on.

// Enqueue refuses rather than pretending: a run that seems queued and that
// no worker will ever see would leave a caller waiting for a drill that does
// not exist.
func (Noop) Enqueue(context.Context, *core.RecoveryRun, string, time.Time) error {
	return ErrNoHistory
}

func (Noop) SetState(context.Context, string, core.RunState) error { return nil }

func (Noop) SetRunError(context.Context, string, string) error { return nil }

func (Noop) RequestCancel(context.Context, string, time.Time) (bool, error) {
	return false, ErrNoHistory
}

func (Noop) CancelRequested(context.Context, string) (bool, error) { return false, nil }

func (Noop) ActiveRunForWorkload(context.Context, string) (string, error) {
	return "", ErrNoHistory
}

// ClaimRun reports an empty queue rather than an error: a worker running
// without a database has nothing to do, and should idle quietly rather than
// log a failure every tick.
func (Noop) ClaimRun(context.Context, string, time.Duration, time.Time) (*QueuedRun, error) {
	return nil, ErrNoWork
}

func (Noop) RenewLease(context.Context, string, string, time.Time) error { return nil }

func (Noop) FinishLease(context.Context, string) error { return nil }

// StaleRuns reports nothing to reconcile, for the same reason as ClaimRun.
func (Noop) StaleRuns(context.Context, time.Time) ([]QueuedRun, error) { return nil, nil }

// RunLease reports ErrNotFound, for the same reason GetRun does: a caller
// asking about one specific run must be told there is no such run, not
// handed an empty lease it would read as "nobody is running this".
func (Noop) RunLease(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, ErrNotFound
}

// ClaimSlot refuses without a database, and that refusal is the honest
// answer rather than a limitation. With nowhere to record that a slot was
// decided, a scheduler would re-decide the same slot every tick and queue a
// drill each time - so scheduling is the one feature that genuinely needs a
// database, and it says so.
func (Noop) ClaimSlot(context.Context, Slot, *core.RecoveryRun, string) error {
	return ErrNoHistory
}

func (Noop) LastSlot(context.Context, string) (*Slot, error) { return nil, ErrNoHistory }

func (Noop) ListSlots(context.Context, SlotFilter) ([]Slot, error) { return nil, ErrNoHistory }

// The notification methods.
//
// Silence for the claim and the two queries: an installation with no history
// has no runs to announce, and returning an error would make the dispatcher
// log a warning every tick about a database the operator deliberately does
// not have. A claim that reports false is also the safe answer, because false
// means "somebody else has this run" and the caller then says nothing.
//
// The two writes are the exception, and it is the exception the token methods
// already make: pretending to have recorded a message that will never be sent
// would leave a caller believing somebody was told.
func (Noop) ClaimRunForNotify(context.Context, string, time.Time) (bool, error) { return false, nil }

func (Noop) UnnotifiedRuns(context.Context, int) ([]RunSummary, error) { return nil, nil }

func (Noop) PreviousStory(context.Context, string, Position) (*RunSummary, bool, error) {
	return nil, false, nil
}

func (Noop) CreateDelivery(context.Context, Delivery) error { return ErrNoHistory }

func (Noop) DueDeliveries(context.Context, time.Time, int) ([]Delivery, error) { return nil, nil }

func (Noop) SettleDelivery(context.Context, Delivery) error { return ErrNoHistory }

// LastDeliveries is silence for the reason the other reads are: with no
// history there are no deliveries to describe, and doctor asking about a
// channel that has never been used gets the same empty answer either way.
func (Noop) LastDeliveries(context.Context, []string) (map[string]Delivery, error) {
	return map[string]Delivery{}, nil
}

// The captured-value methods are all silence, with no ErrNoHistory anywhere.
//
// The write is the plain case this type was built for: history is optional
// and must never fail a drill, so a value nobody can store costs the next
// drill its baseline and costs this drill nothing.
//
// The two reads are silence for a sharper reason. Empty is not a stand-in for
// an error here, it is the true answer: with no database there is no history,
// and "no history" is precisely what the drift evaluation is already written
// to handle - it reports the check skipped with its reason rather than failing
// it. Returning an error instead would turn a missing database into a failed
// check, which is the one thing this package exists to prevent.
func (Noop) SaveCheckValue(context.Context, string, int, string, float64) error { return nil }

func (Noop) CapturedValues(context.Context, string, string, string, int) ([]float64, error) {
	return nil, nil
}

func (Noop) RunCheckValues(context.Context, string) (map[int]map[string]float64, error) {
	return map[int]map[string]float64{}, nil
}
