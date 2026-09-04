package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/notify"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/scheduler"
	"github.com/restorelab/restorelab/internal/store"
	"github.com/restorelab/restorelab/internal/ui"
	"github.com/restorelab/restorelab/internal/worker"
)

// defaultListen is where serve binds unless told otherwise.
//
// Loopback, because a RestoreLab that appeared on every interface the moment
// someone typed `serve` would be a surprise, and surprises with API surfaces
// are how clusters get read by strangers. TLS is a reverse proxy's job:
// adding certificate handling to a daemon that lives behind nginx or Caddy
// would be work done twice, badly.
const defaultListen = "127.0.0.1:8080"

func newServeCmd(a *app) *cobra.Command {
	var opts serveOptions

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the HTTP API and execute the drills it queues",
		Long: `Serves RestoreLab's HTTP API and executes the drills queued through it.

    restorelab serve
    restorelab serve --listen 127.0.0.1:9000

One process does both by default, because a queue nobody drains is a queue
nobody should be allowed to fill. The two halves talk to each other only
through the database, which makes splitting them a deployment choice rather
than a rewrite: the API in a DMZ, the worker on the administration network,
two processes, one history.

    restorelab serve --no-listen                        (the worker alone)
    restorelab serve --no-worker --worker-elsewhere     (the API alone)

A process that runs the worker also queues the drills stored plans schedule
for themselves. Use --no-scheduler to stop that for one night without
touching any plan; see ` + "`restorelab schedule list`" + ` for what is coming.

Any process announces the runs that changed what a workload proves, whether or
not it runs the worker, because in a split deployment the drills happen
elsewhere. Use --no-notify to stay quiet for one night; see
` + "`restorelab notify list`" + ` for the channels.

Listening anywhere but loopback needs at least one API token, created with
` + "`restorelab token create <name>`" + `. Put TLS in front of it with a
reverse proxy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serve(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.listen, "listen", defaultListen, "address to listen on")
	f.BoolVar(&opts.noWorker, "no-worker", false,
		"serve the API without executing drills (another process must run the worker)")
	f.BoolVar(&opts.noListen, "no-listen", false,
		"execute drills without serving the API")
	f.BoolVar(&opts.workerElsewhere, "worker-elsewhere", false,
		"confirm that another process runs the worker against the same database (required with --no-worker)")
	f.BoolVar(&opts.noScheduler, "no-scheduler", false,
		"do not queue the drills stored plans schedule for themselves")
	f.BoolVar(&opts.noNotify, "no-notify", false,
		"do not send notifications for runs that change what a workload proves")
	return cmd
}

// serveOptions is what `serve` was asked to be.
type serveOptions struct {
	listen string
	// noWorker serves the API without executing anything.
	noWorker bool
	// noListen executes without serving anything.
	noListen bool
	// workerElsewhere is the operator saying, out loud, that some other
	// process drains the queue. Nothing can verify it - a worker on another
	// machine leaves no trace until it claims something - so the honest
	// design is to make the claim explicit rather than to guess.
	workerElsewhere bool
	// noScheduler stops this process queueing scheduled drills. It is the
	// switch for "we are migrating the cluster tonight, stop everything";
	// stopping one plan is done by removing its schedule.
	noScheduler bool
	// noNotify stops this process sending notifications. It is the switch for
	// a maintenance window where the drills are expected to fail and nobody
	// wants to be told about each one; silencing one channel for good is done
	// by disabling it in the configuration.
	noNotify bool
}

// check refuses the two shapes of process that cannot do their job.
//
// It runs before configuration is loaded and before the database is opened,
// because a flag combination that can never work should not first cost the
// operator a config error about something unrelated.
func (o serveOptions) check() error {
	if o.noWorker && o.noListen {
		return errors.New(
			"--no-worker and --no-listen together leave a process that neither serves nor executes: drop one")
	}
	if o.noWorker && !o.workerElsewhere {
		return errors.New(
			"--no-worker serves an API that queues drills nobody executes: a caller gets its 201 and waits forever.\n" +
				"  Run a worker against the same database, here or on another machine:\n" +
				"      restorelab serve --no-listen\n" +
				"  then start this one with --no-worker --worker-elsewhere to say that you did")
	}
	return nil
}

func (a *app) serve(ctx context.Context, opts serveOptions) error {
	if err := opts.check(); err != nil {
		return err
	}

	// No cluster to serve: run the wizard first. It returns once there is
	// one, and everything below then builds the real server on the same
	// address - which is what makes `restorelab serve` the only command an
	// installation needs.
	//
	// The question is asked through cliSetup so that "configured" means the
	// same thing here and in the router that decides whether the setup routes
	// exist. Two notions of it is how somebody ends up locked out.
	setup := &cliSetup{a: a}
	if !setup.Configured() {
		if err := a.serveSetup(ctx, opts, setup); err != nil {
			return err
		}
	}

	cfg, err := a.config()
	if err != nil {
		return err
	}
	history := a.store(ctx)

	// The worker and the API reach each other only through the database, so
	// a worker without one has nothing it could ever claim. Serving is
	// different: a read-only API still answers about the cluster.
	if opts.noListen {
		if _, none := history.(store.Noop); none {
			return errors.New("a worker needs a history database to claim its runs from, and this RestoreLab has none: see `restorelab db status`")
		}
	}

	// A server exposed with no authentication is not a configuration, it is
	// an accident. Refusing here is the only moment it can be prevented.
	if !opts.noListen && !isLoopbackAddr(opts.listen) {
		tokens, err := history.ListTokens(ctx)
		if err != nil {
			return fmt.Errorf("refusing to listen on %s: the token list cannot be read: %w", opts.listen, err)
		}
		live := 0
		for _, t := range tokens {
			if t.Live() {
				live++
			}
		}
		if live == 0 {
			return fmt.Errorf("refusing to listen on %s with no API token: create one with `restorelab token create <name>`", opts.listen)
		}
	}

	// Providers are built once and shared by both halves: proxmox.New builds
	// an HTTP client, and a second set would throw away every connection the
	// first one keeps.
	provs := &cliProviders{a: a}

	// One adapter, shared by the dispatcher and the API, and that sharing is
	// the fix rather than a convenience. It owns the mutex that serialises
	// every read and write of the channel configuration; two instances would
	// hold two mutexes and serialise nothing, so the dashboard writing a
	// channel while the dispatcher read the list would be the same data race
	// with the lock taken.
	channels := &cliNotifications{a: a}

	// A worker stops when this context does, and it is cancelled on the way
	// out of this function whatever ends it - a signal, or a listener that
	// could not bind. Without that, a serve that failed to listen would leave
	// a worker quietly draining the queue behind an error message.
	ctx, cancel := context.WithCancel(ctx)

	// Declared in this order so they run in the other one: cancel first, then
	// Wait. The reverse would wait for a worker nobody had told to stop.
	//
	// The wait itself is the whole point. The API hands back its port in
	// milliseconds; a drill in flight needs however long its cleanup needs,
	// and a worker killed mid-drill is exactly how a temporary workload is
	// left running on the cluster.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	if !opts.noWorker {
		wk, err := worker.New(worker.Options{
			Store:     history,
			Providers: provs,
			Config:    cfg,
			Logger:    a.runLogger(),
		})
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := wk.Run(ctx); err != nil {
				fmt.Fprintf(a.err, "%s worker stopped: %v\n", a.warn(), err)
			}
		}()

		if opts.noListen {
			fmt.Fprintf(a.out, "%s executing drills, not serving the API\n", a.ok())
		}
		fmt.Fprintf(a.out, "  worker: %d drill(s) at a time\n", wk.Concurrency())
	}

	// The scheduler goes beside the worker, on the same context and the same
	// wait group: a process that stops waits for it exactly as it waits for
	// a drill in flight.
	//
	// It is tied to the worker on purpose. A process that queues scheduled
	// drills without draining them fills a queue nobody empties, which is
	// the same reasoning --no-worker already carries for the API.
	if !opts.noWorker && !opts.noScheduler && cfg.SchedulerEnabled() {
		sch, err := scheduler.New(scheduler.Options{
			Store:  history,
			Config: cfg,
			Logger: a.runLogger(),
		})
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sch.Run(ctx); err != nil {
				fmt.Fprintf(a.err, "%s scheduler stopped: %v\n", a.warn(), err)
			}
		}()
		fmt.Fprintf(a.out, "  scheduler: on, %s grace for a late slot\n", sch.GracePeriod())
	} else if !cfg.SchedulerEnabled() {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"scheduler off in the configuration: plans keep their schedule, nothing acts on it"))
	}

	if err := a.startDispatcher(ctx, cfg, channels, history, opts, &wg); err != nil {
		return err
	}

	if opts.noListen {
		fmt.Fprintf(a.out, "  history: %s\n", history.Describe())
		<-ctx.Done()
		return nil
	}

	srv := api.New(serveAPIOptions(history, cfg, provs, channels, opts.noNotify))

	fmt.Fprintf(a.out, "%s listening on http://%s/api/v1\n", a.ok(), opts.listen)
	fmt.Fprintf(a.out, "  history: %s\n", history.Describe())
	if opts.noWorker {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
			"no worker here: queued drills wait for the worker you said runs elsewhere"))
	}
	if !ui.Built() {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"no dashboard in this binary: the API answers, / explains how to build one"))
	}
	if isLoopbackAddr(opts.listen) {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"loopback only; use --listen and a token to expose it, behind TLS"))
	}

	return api.Serve(ctx, opts.listen, srv, func(format string, args ...any) {
		fmt.Fprintf(a.err, format+"\n", args...)
	})
}

// startDispatcher starts the notification dispatcher beside the scheduler, on
// the same context and the same wait group, and prints what it will do.
//
// Unlike the scheduler it is deliberately not tied to the worker. The
// scheduler is, because a process that queues drills nobody drains fills a
// queue nobody empties; nothing of the sort applies here. A process that only
// serves the API still has a database full of runs somebody should hear
// about, and in a split deployment the drills are executed somewhere else
// entirely - tying alerting to the worker would mean the half of the
// installation people look at is the half that cannot speak.
//
// The line it prints is not decoration. An operator who believes alerts are
// on when they are not misreads every silence that follows, which is the
// failure this whole slice exists to prevent.
//
// It starts even when no channel is configured yet. That is not idling for
// its own sake: the channel list is re-read on every tick, so a process that
// declined to start one because config.yaml happened to be empty at boot
// would still need a restart before the first channel added from the
// dashboard said anything. What it costs while nothing is configured is two
// indexed queries a minute; what it changes is that the runs going past are
// marked as considered rather than kept, so a channel added on Friday
// announces what happens from Friday and not a backlog of the whole week.
func (a *app) startDispatcher(
	ctx context.Context, cfg *config.Config, channels *cliNotifications,
	history store.Store, opts serveOptions, wg *sync.WaitGroup,
) error {
	if opts.noNotify {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
			"notifications off (--no-notify): the channels stay configured, nothing is sent"))
		return nil
	}

	// The URLs are sealed, so the dispatcher needs the master key. Failing to
	// find it is reported and survived rather than fatal: notifications are
	// the one component that must never be the reason a process that also
	// drills workloads refuses to start.
	//
	// The warning is worth printing even with nothing configured yet, because
	// the dispatcher would start without it and then fail every message the
	// dashboard's first channel produced. Its wording does not assume there is
	// a channel today: there may be one in a minute.
	key, err := a.masterKey()
	if err != nil {
		fmt.Fprintf(a.err, "%s the master key cannot be read, so no notification will be sent: %v\n",
			a.warn(), err)
		return nil
	}

	disp, err := notify.New(notify.Options{
		Store: history,
		// The accessor, not the slice. It is read on every tick, under the
		// lock the dashboard's writes take, which is what makes a channel
		// added at 02:00 speak at 02:01 instead of after the next restart.
		Channels: channels.configured,
		Key:      key,
		BaseURL:  cfg.Server.BaseURL,
		Logger:   a.runLogger(),
	})
	if err != nil {
		return err
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := disp.Run(ctx); err != nil {
			fmt.Fprintf(a.err, "%s notifications stopped: %v\n", a.warn(), err)
		}
	}()
	// A count at this instant, and it says so when there is none. The
	// dispatcher runs either way now: the list is re-read every tick, so a
	// process that declined to start one because config.yaml was empty at boot
	// would still need a restart to use the first channel somebody adds from
	// the dashboard, which is the failure this was all for.
	if n := disp.Channels(); n > 0 {
		fmt.Fprintf(a.out, "  notifications: on, %d channel(s) right now\n", n)
	} else {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
			"notifications: on, no channel enabled yet; one added from the dashboard is used within a minute"))
	}

	if cfg.Server.BaseURL == "" {
		// Worth one line: a message with no link is a message somebody has to
		// come and find the run for, and the reason is a single setting.
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"no server.base_url: messages will carry no link back to the run"))
	}
	return nil
}

// newSetupToken mints the secret printed on the console.
//
// It is the only thing standing between an administrator password and an open
// port, so it comes from crypto/rand and is long enough that guessing it is
// not a strategy.
func newSetupToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating the setup token: %w", err)
	}
	return "rls_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// setupAPIOptions assembles what a server in setup mode needs, which is
// almost nothing.
//
// A function rather than a literal, for the same reason serveAPIOptions is
// one: a test can read it. A dependency left out by accident does not fail a
// build - it answers 503 forever, and every test that never asked stays green.
func setupAPIOptions(s api.Setup, token string) api.Options {
	return api.Options{
		Setup:      s,
		SetupToken: token,
		UI:         ui.FS(),
	}
}

// serveSetup runs the first-run wizard and returns once it has succeeded.
//
// The setup server cannot become the real one. A Server's dependencies are
// built once in api.New and never rewritten, and that is what makes it simple
// to reason about; making them mutable for something that happens once in the
// life of an installation would be a permanent cost for a momentary
// convenience. So this hands the port over instead.
func (a *app) serveSetup(ctx context.Context, opts serveOptions, setup *cliSetup) error {
	if opts.noListen {
		return errors.New(
			"nothing is configured yet, and --no-listen leaves no way to configure it: " +
				"run `restorelab serve` and open the address it prints")
	}

	token, err := newSetupToken()
	if err != nil {
		return err
	}

	srv := api.New(setupAPIOptions(setup, token))

	fmt.Fprintf(a.out, "%s RestoreLab is not configured yet.\n", a.warn())
	fmt.Fprintf(a.out, "  Open this address to set it up. The token is printed once, and used once:\n\n")
	fmt.Fprintf(a.out, "      %s\n\n", a.paint(colorCyan,
		fmt.Sprintf("http://%s/setup?token=%s", opts.listen, token)))

	// The wizard's success is what ends this server, so it gets a context of
	// its own: cancelling it must not cancel the caller's.
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		errc <- api.Serve(serveCtx, opts.listen, srv, func(format string, args ...any) {
			fmt.Fprintf(a.err, format+"\n", args...)
		})
	}()

	select {
	case err := <-errc:
		// The listener ended on its own - a port already in use, most likely.
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-srv.SetupDone():
	}

	fmt.Fprintf(a.out, "%s configured. Starting RestoreLab.\n\n", a.ok())

	// Hand the port over: stop the wizard, and wait for its listener to
	// actually let go. Without the wait, the real server binds onto a port
	// something else still holds.
	stop()
	if err := <-errc; err != nil {
		return err
	}

	// The app cached the absence of a configuration. Forget it, or the server
	// about to start is told there still is not one.
	a.forget()
	return nil
}

// serveAPIOptions assembles what the HTTP server needs.
//
// It is a function rather than a literal inline so a test can read it. A
// dependency left out of api.Options does not fail a build and does not fail
// a handler: it answers 503 forever, and every test that never asked stays
// green. That happened once already, to the whole plan catalogue.
func serveAPIOptions(
	history store.Store, cfg *config.Config, provs api.ProviderSet,
	channels api.Notifications, dispatcherOff bool,
) api.Options {
	return api.Options{
		History:   history,
		Tokens:    history,
		Sessions:  history,
		Plans:     history,
		Schedules: history,
		Providers: provs,
		Config:    cfg,
		Weights:   report.DefaultWeights(),
		UI:        ui.FS(),

		// The channel configuration comes from this side because sealing a
		// webhook URL needs the master key; the delivery history comes
		// straight from the store, because the dispatcher writes that table
		// and the API only reports it.
		Notifications: channels,
		Deliveries:    history,
		// Reported rather than inferred: from inside the API a channel that
		// is enabled and a channel nobody drains look identical, and the
		// difference is the whole reason somebody opens that page.
		NotifyDispatcherOff: dispatcherOff,
	}
}

// isLoopbackAddr reports whether addr binds to loopback only.
//
// Anything it cannot parse counts as public: the consequence of being wrong
// that way is being asked for a token, and the consequence of being wrong the
// other way is an unauthenticated API on a public interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// cliProviders adapts the CLI's provider wiring to what the API needs.
//
// It lives here rather than in internal/api on purpose: unsealing a provider
// secret needs the master key, and keeping that on this side of the interface
// is what stops the API package from importing crypto at all.
//
// Clients are built once and reused: proxmox.New builds an HTTP client, and
// doing that per request would throw away every connection.
type cliProviders struct {
	a *app

	mu sync.Mutex
	hv map[string]core.HypervisorProvider
	bp map[string]core.BackupProvider
}

var _ api.ProviderSet = (*cliProviders)(nil)

// Entries lists the configured providers with their secrets redacted. The
// redaction happens here so that no caller can forget it.
func (p *cliProviders) Entries() []config.Provider {
	cfg, err := p.a.config()
	if err != nil {
		return nil
	}
	out := make([]config.Provider, 0, len(cfg.Providers))
	for _, entry := range cfg.Providers {
		out = append(out, entry.Redacted())
	}
	return out
}

func (p *cliProviders) Hypervisor(id string) (core.HypervisorProvider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.hv[id]; ok {
		return client, nil
	}
	client, _, err := p.a.hypervisor(id)
	if err != nil {
		return nil, err
	}
	if p.hv == nil {
		p.hv = map[string]core.HypervisorProvider{}
	}
	p.hv[id] = client
	return client, nil
}

func (p *cliProviders) Backups(id string) (core.BackupProvider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.bp[id]; ok {
		return client, nil
	}
	client, _, err := p.a.backups(id)
	if err != nil {
		return nil, err
	}
	if p.bp == nil {
		p.bp = map[string]core.BackupProvider{}
	}
	p.bp[id] = client
	return client, nil
}
