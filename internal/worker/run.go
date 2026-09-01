package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/restorelab/restorelab/internal/checks"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/journal"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/recovery"
	"github.com/restorelab/restorelab/internal/store"
)

// execute runs one claimed drill, from its stored plan snapshot.
//
// The plan comes from the snapshot, not from anything editable: the run was
// queued against a plan, and that is the plan it must run - phase B3 makes
// plans editable, and a drill that silently changed shape between queueing
// and execution would be impossible to reason about afterwards.
//
// The history writes below deliberately run on contexts a shutdown cannot
// cancel; see mirrorState, fail, and the call to Finish. That is what the
// nolint states, rather than waives.
//
//nolint:contextcheck // history must outlive cancellation, see the comments below
func (w *Worker) execute(parent context.Context, q store.QueuedRun) {
	log := w.log.With("run_id", q.ID, "workload", q.SourceWorkloadID)

	p, err := plan.Parse([]byte(q.PlanSnapshot))
	if err != nil {
		w.fail(q.ID, fmt.Errorf("the stored plan cannot be read: %w", err))
		return
	}

	hv, err := w.providers.Hypervisor(q.ProviderID)
	if err != nil {
		w.fail(q.ID, fmt.Errorf("provider %s is unavailable: %w", q.ProviderID, err))
		return
	}
	bp, err := w.providers.Backups(firstNonEmpty(q.BackupProviderID, q.ProviderID))
	if err != nil {
		w.fail(q.ID, fmt.Errorf("backup provider is unavailable: %w", err))
		return
	}

	network, err := w.resolveNetwork(p)
	if err != nil {
		w.fail(q.ID, err)
		return
	}

	// One journal and one engine per run: recovery.Deps.Emit is a single
	// callback fixed at construction, so a shared engine would send one run's
	// events to another run's recorder.
	rec := journal.New(w.store, log)
	rec.AttachTo(q.ID)

	engine, err := recovery.New(recovery.Deps{
		Hypervisor: hv,
		Backups:    bp,
		Checks:     checks.Default(),
		Logger:     log,
		Emit: func(e recovery.Event) {
			rec.Emit(e)
			w.mirrorState(q.ID, e)
		},
	})
	if err != nil {
		w.fail(q.ID, err)
		return
	}

	// The run's own context: cancelling it stops this drill and nothing else.
	// It derives from the worker's, so a shutdown asks every drill to stop.
	runCtx, cancel := context.WithCancel(parent)
	defer cancel()

	stop := w.holdLease(runCtx, q.ID, cancel)

	run, runErr := engine.Run(runCtx, p, recovery.RunOptions{
		RunID:   q.ID, // the row already exists; the engine must not mint another id
		Network: network,
		Node:    p.Restore.Node,
		Storage: p.Restore.Storage,
		Pool:    p.Restore.Pool,
	})
	stop()

	// Finish writes the authoritative timeline on a context the shutdown
	// cannot cancel: the drill that was interrupted is precisely the one
	// whose trace is worth the most.
	rec.Finish(context.WithoutCancel(parent), run)
	if err := w.store.FinishLease(context.WithoutCancel(parent), q.ID); err != nil {
		log.Warn("could not release the lease", "err", err)
	}
	if runErr != nil {
		log.Warn("drill finished with an error", "err", runErr, "state", run.State)
	}
}

// resolveNetwork turns the plan's network reference into the profile the
// engine restores onto.
//
// The engine refuses a network that is not marked isolated, so nothing is
// re-checked here. What this does add is the plan's bridge override, which
// exists so a plan can name a profile and still pin the bridge it lands on.
func (w *Worker) resolveNetwork(p *plan.Plan) (core.NetworkConfig, error) {
	if w.cfg == nil {
		return core.NetworkConfig{}, errors.New("worker: no configuration is loaded, so no network profile can be resolved")
	}

	name := firstNonEmpty(p.Restore.Network, w.cfg.Defaults.Network, "isolated")
	network, err := w.cfg.ResolveNetwork(name)
	if err != nil {
		return core.NetworkConfig{}, err
	}
	if p.Restore.Bridge != "" {
		network.Bridge = p.Restore.Bridge
	}
	return network, nil
}

// holdLease renews the lease while the drill runs, and cancels it when
// somebody asks for the run to stop. It returns the function that stops it.
//
// Both jobs live on the same tick on purpose: they answer the same question -
// is this drill still wanted, and does anyone still believe it is running.
func (w *Worker) holdLease(ctx context.Context, runID string, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	var once sync.Once

	go func() {
		ticker := time.NewTicker(w.renew)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			if err := w.store.RenewLease(ctx, runID, w.owner, w.now().Add(w.lease)); err != nil {
				if ctx.Err() != nil {
					// The drill is already stopping; the failed renewal is a
					// consequence of that, not news.
					return
				}
				// Losing the lease means another process believes it owns
				// this drill. Stopping is the only safe answer: two engines
				// restoring the same run would be exactly the state the
				// claim exists to prevent.
				w.log.Error("lost the lease on a running drill, stopping it", "run_id", runID, "err", err)
				cancel()
				return
			}

			asked, err := w.store.CancelRequested(ctx, runID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				w.log.Warn("cannot read the cancellation flag", "run_id", runID, "err", err)
				continue
			}
			if asked {
				w.log.Info("cancellation requested, stopping the drill", "run_id", runID)
				cancel()
				return
			}
		}
	}()

	return func() { once.Do(func() { close(done) }) }
}

// fail settles a run that could not even be started, and releases its lease.
//
// It never leaves a claimed run in a non-terminal state: that run would be
// reported stale forever and could never be claimed again.
//
// It writes on its own context for the same reason the cleanup does: a run
// that failed while the worker was shutting down still has to be recorded as
// failed, or it becomes the interrupted run nobody can explain.
//
//nolint:contextcheck // settling a claimed run must outlive cancellation, see above
func (w *Worker) fail(runID string, err error) {
	ctx := context.Background()
	w.log.Error("drill could not start", "run_id", runID, "err", err)
	if serr := w.store.SetState(ctx, runID, core.RunFailed); serr != nil {
		w.log.Warn("could not record the failure", "run_id", runID, "err", serr)
	}
	if lerr := w.store.FinishLease(ctx, runID); lerr != nil {
		w.log.Warn("could not release the lease", "run_id", runID, "err", lerr)
	}
}

// mirrorState keeps the run row's state in step with the engine, so the queue
// and any dashboard reading it tell the truth while the drill is running.
// The journal writes events; this writes the one column a listing shows.
//
// Like the journal's own writes, it runs on a context that a cancelled drill
// cannot take down: the last state a stopped run reached is part of its
// record.
//
//nolint:contextcheck // state mirroring must outlive cancellation, see above
func (w *Worker) mirrorState(runID string, e recovery.Event) {
	if e.State == "" {
		return
	}
	if err := w.store.SetState(context.Background(), runID, e.State); err != nil {
		w.log.Debug("could not mirror the run state", "run_id", runID, "err", err)
	}
}
