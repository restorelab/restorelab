// Package trigger turns a validated recovery plan into a queued drill.
//
// It exists so that a drill launched from an HTTP request and one launched by
// the scheduler take the same path. The conflict check, the plan snapshot and
// the run row are decided here, once: two implementations of "queue a drill"
// would eventually disagree about the guards, and the one that drifted would
// be the automated one nobody is watching.
//
// It calls no provider and holds no credential. Everything here is one read
// and one write against the queue.
package trigger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

// Queue is the slice of the store this package needs.
type Queue interface {
	ActiveRunForWorkload(ctx context.Context, workloadID string) (string, error)
	Enqueue(ctx context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error
}

// Request is one drill to queue.
//
// Plan is already parsed, defaulted and validated: a description that could
// not become a drill has to have been refused before this point, so that a
// bad request is never a queued row somebody has to explain later.
type Request struct {
	Plan   *plan.Plan
	Stored *store.Plan // the catalogue row it came from; nil for an ad-hoc drill

	// DefaultProvider is used when the plan names no provider.
	DefaultProvider string

	ID string    // the run id to use
	At time.Time // when the drill was asked for
}

// Prepared is a drill ready to be written.
//
// It is handed back rather than written directly so that a caller with its
// own transaction can do the writing - the scheduler claims a slot and
// queues the run in one atomic act, and could not use a function that had
// already written half of it.
type Prepared struct {
	Run      *core.RecoveryRun
	PlanYAML string
}

// ErrAlreadyRunning reports that the workload already has a drill in flight.
type ErrAlreadyRunning struct {
	WorkloadID  string
	ActiveRunID string
}

func (e *ErrAlreadyRunning) Error() string {
	return fmt.Sprintf("run %s is queued or running for workload %s", e.ActiveRunID, e.WorkloadID)
}

// Prepare builds the run and its plan snapshot, refusing a workload that
// already has a drill in flight. It writes nothing.
func Prepare(ctx context.Context, q Queue, req Request) (*Prepared, error) {
	if req.Plan == nil {
		return nil, errors.New("trigger: a request needs a plan")
	}
	p := req.Plan

	// The workload comes from the plan in both cases: an ad-hoc plan carries
	// the id the body gave, and a stored plan carries its own. One source, so
	// the lock taken below and the row written afterwards cannot end up
	// disagreeing about what is being drilled.
	workloadID := p.Workload.ID

	// Same rule for the provider, and the same reason. The ad-hoc path
	// already resolved the configured default into the plan before it was
	// built, so reading it back off the plan here changes nothing for it and
	// gives a stored plan that names no provider the same fallback.
	providerID := p.Workload.Provider
	if providerID == "" {
		providerID = req.DefaultProvider
	}

	// One drill per workload at a time. Two concurrent drills of the same
	// workload would restore the same backup twice, and a dashboard that
	// double-clicks must not queue two of them.
	active, err := q.ActiveRunForWorkload(ctx, workloadID)
	if err != nil {
		return nil, err
	}
	if active != "" {
		return nil, &ErrAlreadyRunning{WorkloadID: workloadID, ActiveRunID: active}
	}

	// The snapshot is the defaulted plan re-marshalled, for a stored plan as
	// much as for an ad-hoc one: what the worker executes is this text, so
	// there is one shape of snapshot and it is never the catalogue row that
	// runs. Deleting or editing the plan afterwards cannot change what this
	// drill did.
	planYAML, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}

	run := &core.RecoveryRun{
		ID:               req.ID,
		PlanName:         p.Name,
		ProviderID:       providerID,
		SourceWorkloadID: workloadID,
		State:            core.RunQueued,
		RTOTarget:        time.Duration(p.RTOTarget),
	}
	// Provenance, and only when there is any: an ad-hoc drill came from
	// nowhere but its request, and a plan_id invented for it would point at
	// a row that does not exist.
	if req.Stored != nil {
		run.PlanID = req.Stored.ID
		run.PlanVersion = req.Stored.Version
	}

	return &Prepared{Run: run, PlanYAML: string(planYAML)}, nil
}

// Enqueue prepares a drill and writes it to the queue.
func Enqueue(ctx context.Context, q Queue, req Request) (*core.RecoveryRun, error) {
	prepared, err := Prepare(ctx, q, req)
	if err != nil {
		return nil, err
	}
	if err := q.Enqueue(ctx, prepared.Run, prepared.PlanYAML, req.At); err != nil {
		return nil, err
	}
	return prepared.Run, nil
}
