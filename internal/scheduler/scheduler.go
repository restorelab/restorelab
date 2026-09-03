package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
	"github.com/restorelab/restorelab/internal/trigger"
)

// Defaults, all overridable through configuration.
const (
	// DefaultTick is how often the catalogue is examined. A cron expression
	// resolves to the minute, so looking more often than that only costs
	// queries.
	DefaultTick = time.Minute

	// DefaultGracePeriod is how late a slot may be and still run. Past it,
	// the slot is skipped and recorded: a drill that starts hours outside
	// its window, because a server happened to reboot, occupies production
	// storage during the working day.
	DefaultGracePeriod = 2 * time.Hour

	// DefaultMaxQueueDepth caps how deep the queue may get before the
	// scheduler stops adding to it. Twelve plans due at 03:00 with one
	// worker would otherwise mean a twelfth drill starting mid-morning.
	DefaultMaxQueueDepth = 5
)

// Store is the slice of store.Store the scheduler needs.
//
// It queues through ClaimSlot rather than Enqueue, so that the slot and the
// run it decided on are written in one transaction. Enqueue is here only
// because trigger.Queue requires it; the scheduler never reaches it.
type Store interface {
	ListPlans(ctx context.Context, f store.PlanFilter) ([]store.Plan, error)
	LastSlot(ctx context.Context, planID string) (*store.Slot, error)
	ClaimSlot(ctx context.Context, slot store.Slot, run *core.RecoveryRun, planYAML string) error
	ActiveRunForWorkload(ctx context.Context, workloadID string) (string, error)
	Enqueue(ctx context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error
	ListRuns(ctx context.Context, f store.Filter) ([]store.RunSummary, error)
}

// Options configures a scheduler.
//
// There is deliberately no provider and no engine here. The scheduler writes
// queue rows; the worker drills. Automating drills had to add no destructive
// surface to the product, and this type is where that is guaranteed.
type Options struct {
	Store  Store
	Config *config.Config
	Logger *slog.Logger

	// Tick is how often the catalogue is examined. Zero means DefaultTick.
	Tick time.Duration
	// GracePeriod is how late a slot may be and still run. Zero means the
	// configured value, or DefaultGracePeriod.
	GracePeriod time.Duration
	// MaxQueueDepth caps the queue the scheduler will add to. Zero means the
	// configured value, or DefaultMaxQueueDepth.
	MaxQueueDepth int

	// NewID generates run ids. Nil means uuid.NewString, matching the API.
	NewID func() string
	Now   func() time.Time
}

// Scheduler queues the drills stored plans ask for.
type Scheduler struct {
	store Store
	log   *slog.Logger

	tick          time.Duration
	grace         time.Duration
	maxQueueDepth int

	newID func() string
	now   func() time.Time

	// startedAt is where a plan with no slot history begins, so that a plan
	// written today does not back-fill months of slots it never had.
	startedAt time.Time

	// complainedAbout remembers the plan version whose invalid schedule was
	// last logged, so a typo produces one line rather than one a minute
	// until somebody fixes it.
	complainedAbout map[string]int
}

// New builds a scheduler.
func New(opts Options) (*Scheduler, error) {
	if opts.Store == nil {
		return nil, errors.New("scheduler: a store is required")
	}

	s := &Scheduler{
		store:           opts.Store,
		log:             opts.Logger,
		tick:            opts.Tick,
		grace:           opts.GracePeriod,
		maxQueueDepth:   opts.MaxQueueDepth,
		newID:           opts.NewID,
		now:             opts.Now,
		complainedAbout: map[string]int{},
	}

	if s.log == nil {
		s.log = slog.Default()
	}
	if s.newID == nil {
		s.newID = uuid.NewString
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.tick <= 0 {
		s.tick = DefaultTick
	}
	if s.grace <= 0 {
		s.grace = DefaultGracePeriod
		if opts.Config != nil && opts.Config.Scheduler.GracePeriod > 0 {
			s.grace = opts.Config.Scheduler.GracePeriod
		}
	}
	if s.maxQueueDepth <= 0 {
		s.maxQueueDepth = DefaultMaxQueueDepth
		if opts.Config != nil && opts.Config.Scheduler.MaxQueueDepth > 0 {
			s.maxQueueDepth = opts.Config.Scheduler.MaxQueueDepth
		}
	}

	s.startedAt = s.now()
	return s, nil
}

// GracePeriod is how late a slot may be and still run.
func (s *Scheduler) GracePeriod() time.Duration { return s.grace }

// Run examines the catalogue until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick makes one pass over the catalogue.
//
// It never returns an error, and that is deliberate. A tick that fails has
// one job: leave the scheduler able to try again in a minute. Nothing here is
// worth stopping the loop for, because the loop stopping is the only failure
// mode that silently ends every scheduled verification in the installation.
func (s *Scheduler) Tick(ctx context.Context) {
	plans, err := s.store.ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		s.log.Warn("scheduler: could not read the plan catalogue", "err", err)
		return
	}

	// The queue depth is read once per tick rather than once per plan: it is
	// a regulation signal, not a lock, and twelve plans due at 03:00 must not
	// cost twelve counting queries.
	depth, err := s.queueDepth(ctx)
	if err != nil {
		s.log.Warn("scheduler: could not measure the queue", "err", err)
		return
	}

	for _, row := range plans {
		if ctx.Err() != nil {
			return
		}
		if depth >= s.maxQueueDepth {
			// Not a decision, a postponement: nothing is written, and the
			// slot is reconsidered at the next tick. Recording a skip here
			// would burn a slot that is perfectly runnable in a minute.
			s.log.Debug("scheduler: queue is full, postponing",
				"depth", depth, "max", s.maxQueueDepth, "plan", row.Name)
			return
		}
		if s.considerPlan(ctx, row) {
			depth++
		}
	}
}

// queueDepth counts the runs that have not settled - the queue plus whatever
// is running.
func (s *Scheduler) queueDepth(ctx context.Context) (int, error) {
	pending, err := s.store.ListRuns(ctx, store.Filter{NotTerminal: true})
	if err != nil {
		return 0, err
	}
	return len(pending), nil
}

// considerPlan decides one plan's next slot, and reports whether it queued a
// drill.
func (s *Scheduler) considerPlan(ctx context.Context, row store.Plan) bool {
	parsed, err := plan.Parse([]byte(row.YAML))
	if err != nil {
		// The document is wrong, not the database. It cannot be drilled, and
		// saying so on the slot is the only way an operator finds out - a log
		// line alone would leave the dashboard reading "never tested".
		s.claim(ctx, row, s.now(), true,
			"the stored plan is no longer valid and could not be drilled: "+err.Error(), nil, "")
		return false
	}

	sched, err := plan.ParseSchedule(parsed.Schedule, parsed.ScheduleTimezone)
	if err != nil {
		// Refused at write time since this tranche, so this is either a plan
		// written before that or one edited in the database by hand. It gets
		// one log line per version rather than one a minute.
		if s.complainedAbout[row.ID] != row.Version {
			s.complainedAbout[row.ID] = row.Version
			s.log.Warn("scheduler: plan has an invalid schedule and will not be scheduled",
				"plan", row.Name, "version", row.Version, "err", err)
		}
		return false
	}

	last, err := s.store.LastSlot(ctx, row.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("scheduler: could not read a plan's last slot", "plan", row.Name, "err", err)
		return false
	}

	now := s.now()
	d := decide(sched, last, s.startedAt, now, s.grace)
	if d == nil {
		return false
	}
	if d.Skip {
		s.claim(ctx, row, d.SlotAt, true, d.Reason, nil, "")
		return false
	}

	// The same guards a drill launched from the dashboard goes through, in
	// the same code: one drill per workload, the workload and provider read
	// off the plan, and the snapshot that is what actually runs.
	prepared, err := trigger.Prepare(ctx, s.store, trigger.Request{
		Plan:            parsed,
		Stored:          &row,
		DefaultProvider: row.ProviderID,
		ID:              s.newID(),
		At:              now,
	})
	if err != nil {
		var busy *trigger.ErrAlreadyRunning
		if errors.As(err, &busy) {
			// Not an error: a fact about the world. The slot is recorded as
			// skipped so that the reason survives, rather than the machine
			// simply looking untested.
			s.claim(ctx, row, d.SlotAt, true,
				"a drill was already in flight for this workload: "+busy.Error(), nil, "")
			return false
		}
		s.log.Warn("scheduler: could not prepare a drill", "plan", row.Name, "err", err)
		return false
	}

	return s.claim(ctx, row, d.SlotAt, false, "", prepared.Run, prepared.PlanYAML)
}

// claim records one slot decision, and reports whether a drill was queued.
//
// A duplicate is silence, not a failure: another scheduler decided this slot,
// which is the mechanism working exactly as designed.
func (s *Scheduler) claim(
	ctx context.Context, row store.Plan, slotAt time.Time,
	skip bool, reason string, run *core.RecoveryRun, planYAML string,
) bool {
	slot := store.Slot{
		PlanID:    row.ID,
		SlotAt:    slotAt,
		DecidedAt: s.now(),
		Outcome:   store.SlotQueued,
		Reason:    reason,
	}
	if skip {
		slot.Outcome = store.SlotSkipped
	} else {
		slot.RunID = run.ID
	}

	err := s.store.ClaimSlot(ctx, slot, run, planYAML)
	switch {
	case err == nil:
		if skip {
			s.log.Info("scheduler: slot skipped",
				"plan", row.Name, "slot", slotAt, "reason", reason)
			return false
		}
		s.log.Info("scheduler: drill queued",
			"plan", row.Name, "slot", slotAt, "run", run.ID, "workload", row.WorkloadID)
		return true
	case errors.Is(err, store.ErrDuplicate):
		s.log.Debug("scheduler: slot already decided elsewhere", "plan", row.Name, "slot", slotAt)
		return false
	default:
		s.log.Warn("scheduler: could not claim a slot", "plan", row.Name, "slot", slotAt, "err", err)
		return false
	}
}
