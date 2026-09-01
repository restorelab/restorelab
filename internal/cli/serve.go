package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/spf13/cobra"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
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

	if opts.noListen {
		fmt.Fprintf(a.out, "  history: %s\n", history.Describe())
		<-ctx.Done()
		return nil
	}

	srv := api.New(api.Options{
		History:   history,
		Tokens:    history,
		Plans:     history,
		Providers: provs,
		Config:    cfg,
		Weights:   report.DefaultWeights(),
	})

	fmt.Fprintf(a.out, "%s listening on http://%s/api/v1\n", a.ok(), opts.listen)
	fmt.Fprintf(a.out, "  history: %s\n", history.Describe())
	if opts.noWorker {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorYellow,
			"no worker here: queued drills wait for the worker you said runs elsewhere"))
	}
	if isLoopbackAddr(opts.listen) {
		fmt.Fprintf(a.out, "  %s\n", a.paint(colorDim,
			"loopback only; use --listen and a token to expose it, behind TLS"))
	}

	return api.Serve(ctx, opts.listen, srv, func(format string, args ...any) {
		fmt.Fprintf(a.err, format+"\n", args...)
	})
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
