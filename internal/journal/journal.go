// Package journal records a recovery run as it happens: the run row, its
// progress events, its checks and its final timeline.
//
// It lived inside the CLI until the API gained a worker. Both write the same
// history, and two implementations of "what happened during this drill"
// would drift into two stories about the same run.
//
// Nothing here returns an error. A drill is destructive work on a production
// cluster; a locked database or a full disk must never abort one, and the
// compiler enforcing that is worth more than a convention.
package journal

import (
	"context"
	"log/slog"
	"sync"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/recovery"
	"github.com/restorelab/restorelab/internal/store"
)

// Recorder mirrors a drill into the history database as it happens.
//
// None of its methods return an error, and that is the design rather than an
// oversight. A drill is a destructive operation on a production cluster; a
// locked database, a full disk or a corrupt file must never abort it. Every
// failure becomes a debug line and the drill carries on exactly as it would
// with no database at all — which is what recorder_test's brokenStore proves.
//
// It writes as the run happens rather than in one transaction at the end,
// because a run that is interrupted is precisely the one whose trace is worth
// the most.
type Recorder struct {
	store store.Store
	log   *slog.Logger

	// Set by Prepare, before the engine has produced a run id.
	planName   string
	providerID string
	sourceID   string
	sourceName string
	planYAML   string

	mu             sync.Mutex
	runID          string
	tempWorkloadID string
	eventSeq       int64
	checkSeq       int
}

// New creates a Recorder that writes into s, logging failures to log.
func New(s store.Store, log *slog.Logger) *Recorder {
	return &Recorder{store: s, log: log}
}

// AttachTo binds the journal to a run whose row already exists - one queued
// through the API. Emit then records events against it instead of creating
// it.
//
// The event sequence still starts at 1, and that is correct rather than
// lucky: a run is executed by exactly one worker from start to finish,
// because a claimed run is never claimable again. There is no case where two
// processes number events for the same run.
func (r *Recorder) AttachTo(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runID = runID
}

// Prepare records what the run will be, before it has an id.
//
// The id is minted inside the engine, so the row cannot be created until the
// first event arrives. planYAML is the plan exactly as it is now: plans become
// editable later, and a report must keep saying what was actually checked.
func (r *Recorder) Prepare(planName, providerID, sourceID, sourceName, planYAML string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.planName = planName
	r.providerID = providerID
	r.sourceID = sourceID
	r.sourceName = sourceName
	r.planYAML = planYAML
}

// Emit mirrors one engine event. It is meant to be composed with the terminal
// printer in recovery.Deps.Emit.
func (r *Recorder) Emit(e recovery.Event) {
	ctx := context.Background()

	r.mu.Lock()
	if r.runID == "" {
		if e.RunID == "" {
			r.mu.Unlock()
			return // nothing to attach this to yet
		}
		r.runID = e.RunID
		start := &core.RecoveryRun{
			ID:               e.RunID,
			PlanName:         r.planName,
			ProviderID:       r.providerID,
			SourceWorkloadID: r.sourceID,
			SourceName:       r.sourceName,
			State:            e.State,
			StartedAt:        e.At,
		}
		planYAML := r.planYAML
		r.mu.Unlock()

		// The row has to exist before anything can reference it.
		if err := r.store.CreateRun(ctx, start, planYAML); err != nil {
			r.warn("could not record the start of this run", err)
		}
		r.mu.Lock()
	}

	// Name the temporary workload the moment the engine has allocated one,
	// and never again: nothing may be created on the cluster before the
	// database can already point back to this run.
	if e.TempWorkloadID != "" && r.tempWorkloadID == "" {
		r.tempWorkloadID = e.TempWorkloadID
		runID := r.runID
		tempWorkloadID := e.TempWorkloadID
		node := e.Node
		r.mu.Unlock()

		if err := r.store.SetTempWorkload(ctx, runID, tempWorkloadID, node); err != nil {
			r.warn("could not record the temporary workload for this run", err)
		}

		r.mu.Lock()
	}

	r.eventSeq++
	ev := store.Event{
		Seq:     r.eventSeq,
		At:      e.At,
		State:   e.State,
		Step:    e.Step,
		Status:  e.Status,
		Message: e.Message,
		Check:   e.Check,
		Err:     e.Err,
	}
	runID := r.runID

	var checkSeq int
	if e.Check != nil {
		checkSeq = r.checkSeq
		r.checkSeq++
	}
	r.mu.Unlock()

	if err := r.store.AppendEvent(ctx, runID, ev); err != nil {
		r.warn("could not record a progress event", err)
	}

	// A check result also belongs in run_checks: the report is built from the
	// checks, not from the event stream.
	if e.Check != nil {
		if err := r.store.SaveCheck(ctx, runID, checkSeq, *e.Check); err != nil {
			r.warn("could not record a check result", err)
		}
	}
}

// Finish records the run's final state along with its whole timeline.
//
// The steps are written here rather than as they end because the engine's
// authoritative Step values, with their durations, only exist on the finished
// run. Pass a context that outlives cancellation: like the cleanup, this must
// still write after a Ctrl-C.
func (r *Recorder) Finish(ctx context.Context, run *core.RecoveryRun) {
	if run == nil {
		return
	}

	r.mu.Lock()
	known := r.runID
	planYAML := r.planYAML
	r.mu.Unlock()

	// A run that failed before emitting anything has no row yet. Create one
	// rather than lose the record of a failure.
	if known == "" {
		if err := r.store.CreateRun(ctx, run, planYAML); err != nil {
			r.warn("could not record this run", err)
			return
		}
		r.mu.Lock()
		r.runID = run.ID
		r.mu.Unlock()
	}

	if err := r.store.UpdateRun(ctx, run); err != nil {
		r.warn("could not record the outcome of this run", err)
	}
	for i, step := range run.Steps {
		if err := r.store.SaveStep(ctx, run.ID, i, step); err != nil {
			r.warn("could not record a step", err)
			break // one failure means the rest will fail the same way
		}
	}
	for i, check := range run.Checks {
		if err := r.store.SaveCheck(ctx, run.ID, i, check); err != nil {
			r.warn("could not record a check", err)
			break
		}
	}
}

// warn keeps history failures at debug level. They are never the user's
// problem in the middle of a drill, and --verbose is there for when they are.
func (r *Recorder) warn(msg string, err error) {
	if r.log != nil {
		r.log.Debug(msg, "error", err)
	}
}
