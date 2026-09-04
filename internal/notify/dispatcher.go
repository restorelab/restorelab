package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/crypto"
	"github.com/restorelab/restorelab/internal/store"
)

// Defaults, on the model of the scheduler's.
const (
	// DefaultTick is how often the history is examined. A drill that changed
	// its verdict is worth hearing about within a minute, and looking more
	// often than that only costs queries against a table that changes a
	// handful of times a night.
	DefaultTick = time.Minute

	// DefaultBatch caps how many runs and how many deliveries one pass
	// considers. It bounds the work of the first tick after an outage: a
	// channel unreachable all night comes back gradually rather than in one
	// burst that holds the loop for minutes.
	DefaultBatch = 50

	// DefaultTimeout is how long one POST may take. Ten seconds is what a
	// chat webhook answers in when it is healthy, and anything slower is
	// better retried than waited on.
	DefaultTimeout = 10 * time.Second

	// DefaultMaxAge is how far back the dispatcher will speak about a run.
	// Older ones are marked considered and passed over in silence.
	//
	// Migration 0008 backfills every run that existed when it ran, which
	// covers the upgrade. It does not cover the case that follows: an
	// installation drilling from the CLI with no server at all, or one
	// running with --no-notify, accumulates runs nobody has considered, and
	// the day somebody configures their first channel the first tick would
	// pour weeks of history into it. That is the failure this whole slice
	// exists to prevent, arriving through the back door.
	//
	// A day is long enough that a dispatcher down overnight still catches up
	// on everything that mattered, and short enough that no channel is ever
	// introduced to itself with a backlog. An alert about a drill from last
	// month is not an alert; it is archaeology, and the dashboard is where
	// that belongs.
	DefaultMaxAge = 24 * time.Hour
)

// Store is the slice of store.Store the dispatcher needs.
//
// It is deliberately narrow, and deliberately read-and-record only. Nothing
// here can start, stop or delete anything: the widest power this component
// has over the installation is writing a row saying somebody was told.
type Store interface {
	UnnotifiedRuns(ctx context.Context, limit int) ([]store.RunSummary, error)
	ClaimRunForNotify(ctx context.Context, runID string, at time.Time) (bool, error)
	PreviousStory(ctx context.Context, workloadID string, before store.Position) (*store.RunSummary, bool, error)
	RunCheckValues(ctx context.Context, runID string) (map[int]map[string]float64, error)
	CreateDelivery(ctx context.Context, d store.Delivery) error
	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]store.Delivery, error)
	SettleDelivery(ctx context.Context, d store.Delivery) error
}

// Options configures a dispatcher.
//
// There is deliberately no provider and no recovery engine here, exactly as
// there is none in scheduler.Options. That is invariant 17 applied to a
// second background component: a dead Discord, a full disk or a locked
// database cannot fail a drill, because this type gives the dispatcher no way
// to touch one. TestOptionsCarryNoProviderOrEngine is what keeps it true.
type Options struct {
	Store Store

	// Channels is asked for the current configuration, once per tick.
	//
	// It is a function and not a slice because the dashboard can now add,
	// disable and remove channels while this process runs. A slice read once
	// in New would have frozen the configuration at startup: the screen would
	// accept a new channel, write it to config.yaml, and nothing would ever
	// come out of it until somebody restarted `restorelab serve`. An operator
	// who added a channel and heard nothing would conclude the product does
	// not work, and they would be right about the only part they can see.
	//
	// The alternative considered was a Reload or SetChannels method called by
	// whoever writes the configuration. It was dropped because it puts the
	// duty of remembering on every future writer, and a writer that forgets
	// produces exactly the silence this component exists to prevent. Pulling
	// costs one function call a minute and cannot be forgotten.
	//
	// The function is called from the tick goroutine and from Channels(), so
	// it must be safe to call from several goroutines: the implementation in
	// internal/cli returns a copy under the same mutex its writers take. Nil
	// means no channels, which is a dispatcher that decides nothing and sends
	// nothing rather than an error.
	Channels func() []config.Notification

	// Key opens the sealed channel URLs. The dispatcher holds it for the same
	// reason the CLI does and the API does not: unsealing has to happen
	// somewhere, and it happens as late as possible, one delivery at a time.
	Key crypto.Key
	// BaseURL is server.base_url. Empty means messages carry no link, which
	// is the common case and not a degraded one.
	BaseURL string
	Logger  *slog.Logger

	// Tick is how often the history is examined. Zero means DefaultTick.
	Tick time.Duration
	// Batch caps the runs and deliveries one pass considers. Zero means
	// DefaultBatch.
	Batch int

	// MaxAge is how old a terminal run may be and still be worth announcing.
	// Zero means DefaultMaxAge. A negative value means no bound at all, which
	// only a test should ask for.
	MaxAge time.Duration
	// Timeout bounds one POST. Zero means DefaultTimeout.
	Timeout time.Duration

	// NewID generates delivery ids. Nil means uuid.NewString, matching the
	// scheduler and the API.
	NewID func() string
	Now   func() time.Time
}

// channel is a configured destination with its rendering resolved.
//
// Resolution happens once per tick rather than once at construction, because
// the configuration behind it can change while the process runs. What that
// costs is a map lookup per channel per minute; what it buys is a channel
// added from the dashboard that speaks on the next tick instead of after the
// next restart.
type channel struct {
	config config.Notification
	render Channel
}

// Dispatcher tells people what changed about their workloads.
type Dispatcher struct {
	store    Store
	channels func() []config.Notification
	key      crypto.Key
	baseURL  string
	log      *slog.Logger
	sender   *Sender

	tick   time.Duration
	batch  int
	maxAge time.Duration

	newID func() string
	now   func() time.Time

	// complainMu guards complainedAbout. Channels() may be called from the
	// goroutine that started the dispatcher while Run ticks in another, and
	// both resolve the configuration; the map is small and contended once a
	// minute, so a plain mutex is the right size of answer.
	complainMu sync.Mutex

	// complainedAbout remembers the kind each channel was last complained
	// about, so a typo in config.yaml produces one warning rather than one a
	// minute forever. It is the same mechanism scheduler.complainedAbout is,
	// for the same reason: the validation used to happen once at startup, and
	// moving it into the tick without this would have turned a single line
	// into a log nobody can read.
	//
	// Keyed by channel id and valued by kind so that a corrected typo is
	// noticed: fixing the kind and getting it wrong again warns again. Entries
	// for channels since removed are left in place, as the scheduler leaves
	// its own: the map is bounded by what a human types into a configuration
	// file, and pruning it would cost more attention than it saves.
	complainedAbout map[string]string
}

// New builds a dispatcher.
//
// A channel whose kind has no rendering is dropped with a warning rather than
// refused: this component must never be the reason a process that also drills
// workloads declines to start.
func New(opts Options) (*Dispatcher, error) {
	if opts.Store == nil {
		return nil, errors.New("notify: a store is required")
	}

	d := &Dispatcher{
		store:           opts.Store,
		channels:        opts.Channels,
		key:             opts.Key,
		baseURL:         strings.TrimSuffix(opts.BaseURL, "/"),
		log:             opts.Logger,
		tick:            opts.Tick,
		batch:           opts.Batch,
		maxAge:          opts.MaxAge,
		newID:           opts.NewID,
		now:             opts.Now,
		complainedAbout: map[string]string{},
	}

	if d.log == nil {
		d.log = slog.Default()
	}
	if d.newID == nil {
		d.newID = uuid.NewString
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.tick <= 0 {
		d.tick = DefaultTick
	}
	if d.batch <= 0 {
		d.batch = DefaultBatch
	}
	if d.maxAge == 0 {
		d.maxAge = DefaultMaxAge
	}
	d.sender = NewSender(opts.Timeout)

	return d, nil
}

// resolve reads the configuration as it stands and returns the channels that
// will actually receive a message.
//
// A channel whose kind has no rendering is dropped with a warning, once. The
// warning is here rather than in New because the configuration is read here
// now, and a kind can become invalid long after startup: somebody hand-edits
// config.yaml, or a future release retires a kind.
func (d *Dispatcher) resolve() []channel {
	if d.channels == nil {
		return nil
	}

	configured := d.channels()
	out := make([]channel, 0, len(configured))
	for _, n := range configured {
		if !n.On() {
			continue
		}
		render, err := ChannelFor(n.Kind)
		if err != nil {
			// Named, and counted out. An operator who reads "2 channels" at
			// startup after configuring three has been told which one is not
			// going to speak, which is the whole point of printing the count.
			// Said once per bad kind: this runs every minute, and a line a
			// minute is how a log stops being read.
			d.complain(n, err)
			continue
		}
		out = append(out, channel{config: n, render: render})
	}
	return out
}

// complain warns about a channel that cannot be used, at most once per id and
// kind.
func (d *Dispatcher) complain(n config.Notification, err error) {
	d.complainMu.Lock()
	defer d.complainMu.Unlock()

	if said, ok := d.complainedAbout[n.ID]; ok && said == n.Kind {
		return
	}
	d.complainedAbout[n.ID] = n.Kind
	d.log.Warn("notify: channel will not be used", "channel", n.ID, "err", err)
}

// Channels is how many configured channels would receive a message right now.
//
// It is a count at the instant it is asked, not a promise about the future:
// the configuration it reads can change a minute later, which is the whole
// point of this tranche. serve prints it once at startup for the reason the
// worker prints its concurrency, and an operator who adds a channel from the
// dashboard afterwards sees it in the dashboard's own listing rather than in
// a line printed hours ago.
func (d *Dispatcher) Channels() int { return len(d.resolve()) }

// Run examines the history until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.Tick(ctx)
		}
	}
}

// Tick makes one pass: decide what is worth saying, then say it.
//
// It never returns an error, and that is deliberate. A tick that fails has
// one job: leave the dispatcher able to try again in a minute. Nothing here
// is worth stopping the loop for, because the loop stopping is the failure
// mode that silently ends every alert in the installation while every
// dashboard still says the drills are running.
//
// The two passes are separate because they fail differently. Deciding touches
// only the database; delivering touches somebody else's server, and a channel
// that is down must not stop the next run from being considered.
func (d *Dispatcher) Tick(ctx context.Context) {
	// Resolved once, here, and handed to both passes. Once per tick rather
	// than once per run or once per delivery: a configuration that changed
	// mid-tick would otherwise queue a message for a channel the delivery
	// pass no longer knows about, and the delivery would fail as "no longer
	// configured" a fraction of a second after being written.
	channels := d.resolve()
	d.decide(ctx, channels)
	d.deliver(ctx, channels)
}

// decide claims each unconsidered run and queues what it is worth saying.
func (d *Dispatcher) decide(ctx context.Context, channels []channel) {
	runs, err := d.store.UnnotifiedRuns(ctx, d.batch)
	if err != nil {
		d.log.Warn("notify: could not read the runs nobody has been told about", "err", err)
		return
	}

	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		d.consider(ctx, run, channels)
	}
}

// consider decides one run.
//
// The claim comes first, before the story is even read. A dispatcher that
// dies between the claim and the decision stays silent about one run, which
// is the conservative outcome; claiming afterwards would mean two dispatchers
// deciding the same run and posting the same message twice.
func (d *Dispatcher) consider(ctx context.Context, run store.RunSummary, channels []channel) {
	won, err := d.store.ClaimRunForNotify(ctx, run.ID, d.now())
	if err != nil {
		d.log.Warn("notify: could not claim a run", "run_id", run.ID, "err", err)
		return
	}
	if !won {
		d.log.Debug("notify: another dispatcher is speaking about this run", "run_id", run.ID)
		return
	}

	// Claimed, so this run will not be looked at again, and then passed over.
	// The order matters: skipping before the claim would leave the run
	// pending forever and the same batch would be re-read every tick, which
	// is how a backlog becomes a permanent one.
	if d.tooOld(run) {
		d.log.Debug("notify: too old to be news, marked as considered",
			"run_id", run.ID, "workload", run.SourceWorkloadID)
		return
	}

	before, unevaluable, err := d.store.PreviousStory(ctx, run.SourceWorkloadID,
		store.Position{StartedAt: run.StartedAt, ID: run.ID})
	if err != nil {
		d.log.Warn("notify: could not read what this workload proved before",
			"run_id", run.ID, "workload", run.SourceWorkloadID, "err", err)
		return
	}

	var previous *Story
	if before != nil {
		previous = &Story{Result: before.Result, ProofLevel: before.ProofLevel}
	}
	current := Story{Result: run.Result, ProofLevel: run.ProofLevel}

	transition, said := Decide(run.State, current, previous, unevaluable)
	if !said {
		// Nothing moved in what the drill graded. The numbers it read can
		// still be news, and that is the only remaining question.
		transition, said = d.considerValues(ctx, run, current, previous, before)
	}
	if !said {
		d.log.Debug("notify: nothing changed, saying nothing",
			"run_id", run.ID, "workload", run.SourceWorkloadID)
		return
	}

	msg := d.message(run, transition)
	for _, ch := range channels {
		if ctx.Err() != nil {
			return
		}
		d.queue(ctx, ch, run, msg)
	}
}

// considerValues asks whether a number this drill read is worth a message,
// and it is asked only when nothing else about the drill was.
//
// The shape of the queries is the design. What a run measured is read once,
// and only for a run this dispatcher has already claimed: reading every
// unnotified run would be a round trip per run per tick, most of them for
// runs another dispatcher owns or for runs nobody will ever speak about. The
// previous drill is read only when this one holds a value at zero, because
// nothing else can be a collapse. An ordinary night, where every number held,
// costs exactly one extra query on a run this process was already committed
// to deciding.
func (d *Dispatcher) considerValues(ctx context.Context, run store.RunSummary,
	current Story, previous *Story, before *store.RunSummary) (Transition, bool) {

	// Two refusals that DecideCollapse would make anyway, made here so that
	// they cost nothing: a run that reached no verdict says nothing about
	// the numbers it read, and a workload with no earlier drill has no
	// reading to have fallen from.
	if run.Result == "" || before == nil {
		return Transition{}, false
	}

	values := d.measured(ctx, run.ID)
	if !Zeroed(values) {
		return Transition{}, false
	}

	return DecideCollapse(run.State, current, previous, values, d.measured(ctx, before.ID))
}

// measured reads what one run captured, keyed by capture name.
//
// A read that fails is a warning and an empty map, never a stop. Being unable
// to say what a drill measured is not a reason to say nothing at all, and the
// run has already been claimed: returning early here would leave it claimed
// and never spoken about.
func (d *Dispatcher) measured(ctx context.Context, runID string) map[string]float64 {
	byCheck, err := d.store.RunCheckValues(ctx, runID)
	if err != nil {
		d.log.Warn("notify: could not read what a drill measured",
			"run_id", runID, "err", err)
		return nil
	}
	return valuesByName(byCheck)
}

// queue renders one message for one channel and records it.
//
// The rendering happens once, here, and the bytes are stored: a retry has to
// post what the first attempt tried to post, and re-rendering would let a
// configuration change between attempts alter a message an operator is
// comparing against the run.
func (d *Dispatcher) queue(ctx context.Context, ch channel, run store.RunSummary, msg Message) {
	body, err := ch.render.Render(msg)
	if err != nil {
		d.log.Warn("notify: could not render a message",
			"run_id", run.ID, "channel", ch.config.ID, "err", err)
		return
	}

	now := d.now()
	err = d.store.CreateDelivery(ctx, store.Delivery{
		ID:        d.newID(),
		RunID:     run.ID,
		ChannelID: ch.config.ID,
		Kind:      string(msg.Transition.Kind),
		State:     store.DeliveryPending,
		// Due immediately: the delivery pass of this same tick picks it up,
		// so a verdict that changed is posted in the tick that noticed it.
		NextAt:    now,
		Payload:   string(body),
		CreatedAt: now,
	})

	switch {
	case err == nil:
		d.log.Debug("notify: message queued",
			"run_id", run.ID, "channel", ch.config.ID, "kind", msg.Transition.Kind)
	case errors.Is(err, store.ErrDuplicate):
		// This run already produced a message for this channel. The mechanism
		// working: a restarted dispatcher does not post twice to one place.
		d.log.Debug("notify: this channel has already been told about this run",
			"run_id", run.ID, "channel", ch.config.ID)
	default:
		d.log.Warn("notify: could not record a message to send",
			"run_id", run.ID, "channel", ch.config.ID, "err", err)
	}
}

// message assembles what the renderers are allowed to know about a run.
func (d *Dispatcher) message(run store.RunSummary, t Transition) Message {
	// A run reconciled by SetState after its worker died is terminal with no
	// completion time. Stamping the message with a zero time would date it to
	// year one; the start is the honest approximation and it is the same
	// fallback the unnotified listing orders by.
	at := run.CompletedAt
	if at.IsZero() {
		at = run.StartedAt
	}

	// The name is what a human recognises, the id is what always exists.
	name := run.SourceName
	if name == "" {
		name = run.SourceWorkloadID
	}

	return Message{
		Workload:   name,
		WorkloadID: run.SourceWorkloadID,
		PlanName:   run.PlanName,
		RunID:      run.ID,
		Link:       d.link(run.ID),
		At:         at,
		Transition: t,
		RTO:        run.RTO,
	}
}

// link is the dashboard address of a run, or empty when this installation
// does not know its own.
//
// It is not guessed from the listen address: RestoreLab may sit behind a
// reverse proxy, a tunnel, or a hostname only somebody else's DNS knows. A
// message with no link is better than one whose link resolves nowhere for the
// person reading it.
func (d *Dispatcher) link(runID string) string {
	if d.baseURL == "" {
		return ""
	}
	return d.baseURL + "/runs/" + runID
}

// deliver attempts every message whose time has come.
func (d *Dispatcher) deliver(ctx context.Context, channels []channel) {
	due, err := d.store.DueDeliveries(ctx, d.now(), d.batch)
	if err != nil {
		d.log.Warn("notify: could not read the queue of messages to send", "err", err)
		return
	}

	for _, row := range due {
		if ctx.Err() != nil {
			return
		}
		d.attempt(ctx, row, channels)
	}
}

// attempt posts one delivery and records what happened.
func (d *Dispatcher) attempt(ctx context.Context, row store.Delivery, channels []channel) {
	ch, ok := channelFor(channels, row.ChannelID)
	if !ok {
		// The channel was removed from the configuration after this message
		// was queued. Nobody will ever receive it, and leaving it pending
		// would hand it back every tick for the life of the installation.
		d.fail(ctx, row, fmt.Sprintf("channel %q is no longer configured", row.ChannelID))
		return
	}

	target, err := ch.config.Target(d.key)
	if err != nil {
		// An unsealable URL is a rotated master key or a hand-edited config.
		// It will not start working, so it is a failure to be seen rather
		// than an attempt to repeat. Target's error names the channel and
		// never the value, which is why it can be recorded as it stands.
		d.fail(ctx, row, err.Error())
		return
	}

	result := d.sender.Post(ctx, target, []byte(row.Payload))

	if abandoned(ctx, result) {
		// Not a failure of the channel: this process is stopping. The row
		// stays exactly as it was, so whoever starts next picks it up. The
		// alternative, recording it, would mark a whole healthy queue as
		// refused and send an operator hunting a breakage that never was.
		d.log.Debug("notify: delivery left pending, the dispatcher is stopping",
			"delivery_id", row.ID, "channel", row.ChannelID)
		return
	}

	row.Attempts++
	row.Status = result.Status

	if result.Err == nil {
		row.State = store.DeliverySent
		row.Err = ""
		row.SentAt = d.now()
		row.NextAt = time.Time{}
		// "channel" is the id, never the URL. A webhook URL is a bearer
		// credential carried in the request line itself, and a log line
		// outlives the incident it describes: the same reasoning that keeps
		// proxmox.request's Authorization header out of every message it
		// builds.
		d.log.Info("notify: message delivered",
			"delivery_id", row.ID, "run_id", row.RunID, "channel", row.ChannelID,
			"kind", row.Kind, "status", result.Status)
		d.settle(ctx, row)
		return
	}

	reason := redact(result.Err.Error(), target, row.ChannelID)

	if wait, again := NextAttempt(row.Attempts, result); again {
		row.State = store.DeliveryPending
		row.Err = reason
		row.NextAt = d.now().Add(wait)
		d.log.Warn("notify: delivery failed, trying again",
			"delivery_id", row.ID, "run_id", row.RunID, "channel", row.ChannelID,
			"status", result.Status, "attempt", row.Attempts, "next_at", row.NextAt,
			"err", reason)
		d.settle(ctx, row)
		return
	}

	row.State = store.DeliveryFailed
	row.Err = reason
	row.NextAt = time.Time{}
	// The row is kept rather than dropped, and this is the line doctor turns
	// into "this channel stopped working": a silence nobody can account for
	// is the exact failure this whole slice exists to prevent.
	d.log.Warn("notify: giving up on a delivery",
		"delivery_id", row.ID, "run_id", row.RunID, "channel", row.ChannelID,
		"status", result.Status, "attempt", row.Attempts, "err", reason)
	d.settle(ctx, row)
}

// fail records a delivery that cannot be attempted at all.
//
// The status is cleared rather than left as whatever a previous attempt saw:
// nothing answered this time, and a stale 500 next to "channel is no longer
// configured" would read as a channel that is refusing messages.
func (d *Dispatcher) fail(ctx context.Context, row store.Delivery, reason string) {
	row.Attempts++
	row.State = store.DeliveryFailed
	row.Status = 0
	row.Err = reason
	row.NextAt = time.Time{}
	d.log.Warn("notify: a queued message cannot be sent",
		"delivery_id", row.ID, "run_id", row.RunID, "channel", row.ChannelID, "err", reason)
	d.settle(ctx, row)
}

// settle writes an outcome, and complains rather than retries when it cannot.
func (d *Dispatcher) settle(ctx context.Context, row store.Delivery) {
	if err := d.store.SettleDelivery(ctx, row); err != nil {
		d.log.Warn("notify: could not record the outcome of a delivery",
			"delivery_id", row.ID, "channel", row.ChannelID, "err", err)
	}
}

// channelFor looks up a resolved channel by id.
func channelFor(channels []channel, id string) (channel, bool) {
	for _, ch := range channels {
		if ch.config.ID == id {
			return ch, true
		}
	}
	return channel{}, false
}

// abandoned reports whether a delivery ended because this process is stopping
// rather than because the channel refused it.
//
// context.Canceled can only come from a context somebody cancelled, so it
// counts on its own. context.DeadlineExceeded is ambiguous - the HTTP
// client's own timeout produces it too, and that one is a genuine fact about
// the far end - so it counts only when the caller's context is itself done.
// Getting this backwards would mark every pending message as failed the
// moment a service restarts, and doctor would report every channel dead.
func abandoned(ctx context.Context, r Result) bool {
	if r.Err == nil {
		return false
	}
	if errors.Is(r.Err, context.Canceled) {
		return true
	}
	return ctx.Err() != nil && errors.Is(r.Err, context.DeadlineExceeded)
}

// redact removes a channel's URL from anything about to be logged or stored.
//
// net/url wraps a transport failure in a *url.Error, whose message repeats the
// whole request URL - and for a Discord webhook the path is the credential. So
// a plain "connection refused" would write a working webhook URL into a
// delivery row and into a log line, which are precisely the two places an
// operator copies into a support thread. The channel id says which channel
// broke, which is the only part anybody needs.
func redact(message, target, channelID string) string {
	replacement := "<url of channel " + channelID + ">"
	if target != "" {
		message = strings.ReplaceAll(message, target, replacement)
	}
	return message
}

// tooOld reports whether a run ended too long ago to be worth announcing.
//
// It reads completed_at and falls back to started_at, because a run settled by
// reconciliation after its worker died is terminal without ever recording a
// completion. Falling back the other way, treating a missing completion as
// "just now", would make exactly those runs immortal news.
func (d *Dispatcher) tooOld(run store.RunSummary) bool {
	if d.maxAge < 0 {
		return false
	}
	ended := run.CompletedAt
	if ended.IsZero() {
		ended = run.StartedAt
	}
	if ended.IsZero() {
		// No usable timestamp at all. Say nothing rather than guess: an
		// unbounded claim about when something happened is not one to make in
		// a message somebody will act on.
		return true
	}
	return d.now().Sub(ended) > d.maxAge
}
