package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
	"github.com/restorelab/restorelab/internal/version"
)

// History is the slice of the drill history this API reads.
//
// It is declared here rather than taken as a store.Store because that is the
// interface the consumer needs: three methods, all read-only. A handler
// cannot write a run even by accident, and the tests run against a map.
type History interface {
	ListRuns(ctx context.Context, f store.Filter) ([]store.RunSummary, error)
	GetRun(ctx context.Context, idOrPrefix string) (*core.RecoveryRun, error)
	Events(ctx context.Context, runID string, afterSeq int64) ([]store.Event, error)
}

// TokenStore is what authentication needs, and nothing more: it cannot create
// or revoke a token, only recognise one.
type TokenStore interface {
	TokenByHash(ctx context.Context, hash string) (*store.APIToken, error)
	TouchToken(ctx context.Context, id string, at time.Time) error
}

// ProviderSet hands the API live provider clients.
//
// The CLI implements it. That is deliberate: unsealing a provider secret
// needs the master key, and keeping that knowledge on the other side of an
// interface is what stops this package from importing crypto or
// internal/providers at all.
//
// The methods return core's own provider interfaces rather than narrower
// ones: /doctor needs the capability assertions (core.NetworkValidator, the
// Proxmox storage inspector) that a narrowed interface would hide. The cost
// is that a fake has to implement the destructive methods too - and that is
// how the tests prove none of them is ever called.
type ProviderSet interface {
	// Entries lists the configured providers, secrets already redacted.
	Entries() []config.Provider
	// Hypervisor returns the compute provider with this id, or the configured
	// default when id is empty.
	Hypervisor(id string) (core.HypervisorProvider, error)
	// Backups returns the backup provider with this id, or the default.
	Backups(id string) (core.BackupProvider, error)
}

// Options configures a Server. History, Tokens and Providers are required;
// the rest have working defaults.
type Options struct {
	History   History
	Tokens    TokenStore
	Providers ProviderSet
	Config    *config.Config

	// Weights tunes the confidence score. The zero value means
	// report.DefaultWeights().
	Weights report.ConfidenceWeights
	// Now is the clock. Tests replace it; everything else leaves it nil.
	Now func() time.Time
	// TouchInterval is how often a token's last_used_at is written at most.
	// Zero means DefaultTouchInterval.
	TouchInterval time.Duration
}

// Server is the read-only HTTP API.
type Server struct {
	history   History
	tokens    TokenStore
	providers ProviderSet
	cfg       *config.Config
	weights   report.ConfidenceWeights
	now       func() time.Time
	touch     *touchThrottle
	mux       *http.ServeMux
}

// New builds a Server with its routes wired.
func New(opts Options) *Server {
	s := &Server{
		history:   opts.History,
		tokens:    opts.Tokens,
		providers: opts.Providers,
		cfg:       opts.Config,
		weights:   opts.Weights,
		now:       opts.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.weights == (report.ConfidenceWeights{}) {
		s.weights = report.DefaultWeights()
	}
	interval := opts.TouchInterval
	if interval <= 0 {
		interval = DefaultTouchInterval
	}
	s.touch = newTouchThrottle(interval)
	s.mux = s.routes()
	return s
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// routes wires the surface.
//
// The router is net/http's ServeMux: since Go 1.22 it matches on method and
// path variables, which is the entirety of what a dozen read-only routes
// need. A third-party router would be a dependency bought for nothing.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	mux.Handle("GET /api/v1/recovery-runs", s.authed(s.handleListRuns))
	mux.Handle("GET /api/v1/recovery-runs/{id}", s.authed(s.handleGetRun))
	mux.Handle("GET /api/v1/recovery-runs/{id}/events", s.authed(s.handleRunEvents))
	mux.Handle("GET /api/v1/recovery-runs/{id}/report", s.authed(s.handleRunReport))

	mux.Handle("GET /api/v1/workloads", s.authed(s.handleListWorkloads))
	mux.Handle("GET /api/v1/workloads/{id}", s.authed(s.handleGetWorkload))
	mux.Handle("GET /api/v1/workloads/{id}/backups", s.authed(s.handleWorkloadBackups))
	mux.Handle("GET /api/v1/workloads/{id}/confidence", s.authed(s.handleWorkloadConfidence))

	mux.Handle("GET /api/v1/providers", s.authed(s.handleProviders))
	mux.Handle("GET /api/v1/doctor", s.authed(s.handleDoctor))

	// Anything else: a problem document, not net/http's plain-text 404. A
	// client that parses problem+json must not have to special-case the one
	// response that is not.
	mux.HandleFunc("/", s.handleUnknown)
	return mux
}

func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, newProblem("no-such-route", "No such endpoint", http.StatusNotFound,
		"this API serves GET requests under /api/v1; see docs/api.md"))
}

// handleHealth answers liveness without a token.
//
// It says nothing about the deployment: no database location, no provider
// name, no configuration. Anyone who can reach the port can read this.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, map[string]string{
		"status":  "ok",
		"version": version.String(),
	})
}

// writeJSON renders v as the response body.
//
// It marshals before writing anything: encoding straight into the
// ResponseWriter would emit a 200 header and then fail halfway through the
// body, which a client cannot tell from a truncated network read.
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// shutdownGrace is how long in-flight requests get to finish.
const shutdownGrace = 10 * time.Second

// Serve runs h on addr until ctx is cancelled, then stops gracefully.
//
// A drill can take minutes, but nothing in B1 does: ten seconds is generous
// for a read-only surface, and a stop that never completes is worse than one
// that cuts a listing short.
func Serve(ctx context.Context, addr string, h http.Handler, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logf("shutting down, finishing in-flight requests (%s)", shutdownGrace)
		// Detached from ctx on purpose: we are here *because* ctx is done, so
		// a context derived from it would already be cancelled and Shutdown
		// would drop every in-flight request instead of letting it finish.
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		//nolint:contextcheck // the grace period cannot inherit the cancelled ctx, see above
		if err := srv.Shutdown(stopCtx); err != nil {
			return err
		}
		return nil
	}
}
