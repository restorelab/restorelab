// Package worker executes the drills the API queues.
//
// The contract, and it is the whole of the safety story: a worker executes a
// run it has claimed from start to finish, or it dies trying. It never picks
// up a run another worker started - a claimed run is not claimable again,
// enforced by the WHERE clause of the claim itself. A drill is destructive
// and not idempotent: replaying one would allocate a second temporary id,
// restore a second time, and possibly leave the first workload behind.
//
// This package holds the only mutating provider calls reachable from an HTTP
// request, and it reaches them through recovery.Engine, with every guard the
// engine already carries.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// DefaultLease is how long a claim survives without renewal. It is
// deliberately much longer than the renewal interval: a worker that is merely
// slow must not have its drill declared dead underneath it.
const DefaultLease = 2 * time.Minute

// renewEvery is how often a running drill renews its lease and checks whether
// someone asked it to stop. The same tick does both because they are the same
// question - is this drill still wanted, and does anyone still know it runs.
const renewEvery = 15 * time.Second

// DefaultPoll is how often an idle worker asks the queue for work.
const DefaultPoll = 2 * time.Second

// Providers hands the worker live provider clients. The CLI implements it,
// for the same reason it implements api.ProviderSet: unsealing a secret
// needs the master key, and this package has no business holding one.
type Providers interface {
	Hypervisor(id string) (core.HypervisorProvider, error)
	Backups(id string) (core.BackupProvider, error)
}

// Options configures a worker.
type Options struct {
	Store     store.Store
	Providers Providers
	Config    *config.Config
	Logger    *slog.Logger

	// Owner names this worker in the lease. Empty means hostname:pid.
	Owner string
	// Concurrency caps simultaneous drills. Zero means
	// config.Limits.MaxConcurrentRestores, which itself defaults to 1.
	Concurrency int
	// Lease is how long a claim is held without renewal. Zero means
	// DefaultLease.
	Lease time.Duration
	// Poll is how often an idle worker asks for work. Zero means DefaultPoll.
	Poll time.Duration

	Now func() time.Time
}

// Worker drains the run queue.
type Worker struct {
	store     store.Store
	providers Providers
	cfg       *config.Config
	log       *slog.Logger

	owner       string
	concurrency int
	lease       time.Duration
	poll        time.Duration
	now         func() time.Time

	// renew is the renewal-and-cancellation tick, renewEvery in production.
	// It is a field rather than the constant itself so that the tests can
	// observe a cancellation without waiting a quarter of a minute for it;
	// nothing outside this package can set it, and nothing production-side
	// changes it.
	renew time.Duration
}

// New builds a worker.
func New(opts Options) (*Worker, error) {
	if opts.Store == nil {
		return nil, errors.New("worker: a store is required")
	}
	if opts.Providers == nil {
		return nil, errors.New("worker: providers are required")
	}

	w := &Worker{
		store:       opts.Store,
		providers:   opts.Providers,
		cfg:         opts.Config,
		log:         opts.Logger,
		owner:       opts.Owner,
		concurrency: opts.Concurrency,
		lease:       opts.Lease,
		poll:        opts.Poll,
		now:         opts.Now,
		renew:       renewEvery,
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.owner == "" {
		host, _ := os.Hostname()
		w.owner = fmt.Sprintf("%s:%d", host, os.Getpid())
	}
	if w.lease <= 0 {
		w.lease = DefaultLease
	}
	if w.poll <= 0 {
		w.poll = DefaultPoll
	}
	if w.concurrency <= 0 {
		w.concurrency = 1
		if w.cfg != nil && w.cfg.Limits.MaxConcurrentRestores > 0 {
			// Declared since the first release and read by nothing until now.
			w.concurrency = w.cfg.Limits.MaxConcurrentRestores
		}
	}
	return w, nil
}

// Concurrency reports how many drills this worker will run at once, so that
// `serve` can say it out loud at startup: an operator who thinks drills are
// parallel when they are serialised will misread every timing.
func (w *Worker) Concurrency() int { return w.concurrency }

// Run drains the queue until ctx is cancelled.
//
// On the way out it waits for the drills it started: killing them would leave
// temporary workloads on the cluster, which is the one outcome this whole
// design exists to avoid. Cancelling ctx asks them to stop, and each one
// still runs its cleanup on its own detached context.
func (w *Worker) Run(ctx context.Context) error {
	var (
		wg      sync.WaitGroup
		running = make(chan struct{}, w.concurrency)
	)
	defer wg.Wait()

	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopping, waiting for drills in flight", "owner", w.owner)
			return nil
		case running <- struct{}{}:
		}

		claimed, err := w.store.ClaimRun(ctx, w.owner, w.lease, w.now())
		switch {
		case errors.Is(err, store.ErrNoWork):
			<-running
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
			continue
		case err != nil:
			<-running
			if ctx.Err() != nil {
				// The claim failed because we are shutting down, not because
				// the database is unwell. Say nothing and leave.
				return nil
			}
			// A database that cannot be asked for work is not a reason to
			// stop: it may come back, and a worker that exited on a blip
			// would need a human to restart it.
			w.log.Warn("cannot claim a run", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
			continue
		}

		wg.Add(1)
		go func(q store.QueuedRun) {
			defer wg.Done()
			defer func() { <-running }()
			w.execute(ctx, q)
		}(*claimed)
	}
}

// firstNonEmpty returns the first non-empty string.
//
// A local copy of the CLI's helper. Exporting that one to share three lines
// would put a formatting detail of the command layer into the API of a
// package the worker depends on.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
