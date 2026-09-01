package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
	"github.com/restorelab/restorelab/internal/version"
)

// History is the slice of the drill history this API uses.
//
// It is declared here rather than taken as a store.Store because that is the
// interface the consumer needs. A handler cannot advance a run even by
// accident, and the tests run against a map.
type History interface {
	ListRuns(ctx context.Context, f store.Filter) ([]store.RunSummary, error)
	GetRun(ctx context.Context, idOrPrefix string) (*core.RecoveryRun, error)
	Events(ctx context.Context, runID string, afterSeq int64) ([]store.Event, error)

	// Queueing, and only queueing. The API never claims, never leases and
	// never writes a run's progress: those belong to the worker, and an
	// interface that offered them here would be an invitation to execute a
	// drill inside an HTTP handler.
	Enqueue(ctx context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error
	RequestCancel(ctx context.Context, runID string, at time.Time) (bool, error)
	ActiveRunForWorkload(ctx context.Context, workloadID string) (string, error)

	// RunLease is the read half of the lease, and the only reason /queue can
	// say which worker holds which drill. Its write half - ClaimRun,
	// RenewLease - is deliberately not here.
	RunLease(ctx context.Context, runID string) (owner string, expires time.Time, err error)
}

// Plans is the catalogue slice of the store this API uses.
//
// Narrow on purpose, like History: a handler cannot reach a run through it,
// and the tests run against a map. It is the same set of methods catalog.Store
// declares, because the handlers reach the store only through that package -
// declaring it here rather than aliasing keeps this package's dependency on
// catalog to the calls themselves.
type Plans interface {
	CreatePlan(ctx context.Context, p store.Plan) error
	UpdatePlan(ctx context.Context, p store.Plan, expected int) error
	GetPlan(ctx context.Context, ref string) (*store.Plan, error)
	ListPlans(ctx context.Context, f store.PlanFilter) ([]store.Plan, error)
	DeletePlan(ctx context.Context, ref string) error
}

// TokenStore is what authentication needs, and nothing more: it cannot create
// or revoke a token, only recognise one.
type TokenStore interface {
	TokenByHash(ctx context.Context, hash string) (*store.APIToken, error)
	TouchToken(ctx context.Context, id string, at time.Time) error
}

// SessionStore is what the session routes need, and nothing more.
//
// DeleteExpiredSessions is deliberately absent: the sweep rides along with
// CreateSession, so the API has no reason to be able to empty this table.
type SessionStore interface {
	CreateSession(ctx context.Context, s store.Session, now time.Time) error
	SessionByHash(ctx context.Context, hash string, now time.Time) (*store.Session, *store.APIToken, error)
	DeleteSession(ctx context.Context, hash string) error
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

	// Plans is the stored-plan catalogue. It is separate from History because
	// it is a different table with a different scope guarding it: a
	// deployment can serve drills without ever letting anyone write a plan
	// over HTTP simply by leaving this nil, and the routes then answer 503
	// rather than pretending the catalogue is empty.
	Plans Plans

	// Sessions backs the dashboard's cookie. Nil is a deployment with no
	// usable history database: the session routes then answer 503, the same
	// way the catalogue does, rather than pretending a login failed.
	Sessions SessionStore

	// UI is the compiled dashboard, or nil for an API-only deployment.
	//
	// An fs.FS rather than a concrete type: this package serves what it is
	// handed and never learns where the bytes came from. internal/cli passes
	// ui.FS(); the tests pass an fstest.MapFS.
	UI fs.FS

	// Weights tunes the confidence score. The zero value means
	// report.DefaultWeights().
	Weights report.ConfidenceWeights
	// Now is the clock. Tests replace it; everything else leaves it nil.
	Now func() time.Time
	// TouchInterval is how often a token's last_used_at is written at most.
	// Zero means DefaultTouchInterval.
	TouchInterval time.Duration
}

// Server is the HTTP API: the read surface, and the writes that queue work
// for a worker to execute.
type Server struct {
	history   History
	tokens    TokenStore
	providers ProviderSet
	plans     Plans
	sessions  SessionStore
	ui        fs.FS
	cfg       *config.Config
	weights   report.ConfidenceWeights
	now       func() time.Time
	touch     *touchThrottle
	mux       *http.ServeMux

	// The event stream's rhythm. Fields rather than the constants they are
	// built from so that a test can drive the loop instead of waiting a real
	// second for every pass. They are set once, in New, and only read
	// afterwards - a stream never writes them.
	ssePoll      time.Duration
	sseHeartbeat time.Duration

	// stopping is closed once, by Drain, when the process starts shutting
	// down. It is the only way an open event stream can learn that it is
	// about to be cut: see Drain. Created in New and never reassigned, so
	// every reader races only against the single close, which is what a
	// channel is for.
	stopping chan struct{}
	drained  sync.Once
}

// New builds a Server with its routes wired.
func New(opts Options) *Server {
	s := &Server{
		history:   opts.History,
		tokens:    opts.Tokens,
		providers: opts.Providers,
		plans:     opts.Plans,
		sessions:  opts.Sessions,
		ui:        opts.UI,
		cfg:       opts.Config,
		weights:   opts.Weights,
		now:       opts.Now,

		ssePoll:      ssePoll,
		sseHeartbeat: sseHeartbeat,
		stopping:     make(chan struct{}),
	}
	if s.now == nil {
		s.now = time.Now
	}
	// A nil catalogue is a deployment with no usable history database, not a
	// reason to panic inside a handler. store.Noop answers ErrNoHistory on
	// all five methods, which problemFor already turns into the 503 the rest
	// of the API gives in the same situation.
	if s.plans == nil {
		s.plans = store.Noop{}
	}
	// Same reasoning for the session table, and the same double: a login
	// against a deployment with no history database answers 503 rather than
	// panicking inside the handler that was about to record the session.
	if s.sessions == nil {
		s.sessions = store.Noop{}
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

// Draining is implemented by a handler that holds responses open for longer
// than a request takes to answer, and can be asked to let them go.
//
// It exists because http.Server.Shutdown does not interrupt in-flight
// requests, it waits for them - and an event stream is an in-flight request
// that ends when the drill ends, minutes later. Without this, one connected
// dashboard turns every clean stop into a full grace period followed by
// context.DeadlineExceeded.
type Draining interface {
	// Drain tells long-lived responses that the server is going away. It is
	// safe to call more than once and never blocks.
	Drain()
}

var _ Draining = (*Server)(nil)

// Drain closes every event stream this server is holding open.
//
// It only signals; each stream returns from its own goroutine after writing a
// last frame that says the connection ended, not the run. Calling it twice is
// harmless, and a Server that has been drained stays drained: a Server is
// built per process, and a process that started stopping does not un-stop.
func (s *Server) Drain() {
	s.drained.Do(func() { close(s.stopping) })
}

// routes wires the surface.
//
// The router is net/http's ServeMux: since Go 1.22 it matches on method and
// path variables, which is the entirety of what a dozen routes need. A
// third-party router would be a dependency bought for nothing.
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

	// The session. POST authenticates nothing beforehand: it is what creates
	// the credential every other route checks.
	mux.HandleFunc("POST /api/v1/session", s.handleCreateSession)
	mux.Handle("GET /api/v1/session", s.authed(s.handleGetSession))
	mux.Handle("DELETE /api/v1/session", s.authed(s.handleDeleteSession))

	// The catalogue. Reading it is a read; writing it needs `manage`, which
	// no other scope implies: deciding what a drill is and launching one are
	// two different powers.
	mux.Handle("GET /api/v1/plans", s.authed(s.handleListPlans))
	mux.Handle("GET /api/v1/plans/{ref}", s.authed(s.handleGetPlan))
	// Validation is a write scope even though it writes nothing: it is the
	// plan editor's own route, and the editor is exactly the thing `manage`
	// names.
	mux.Handle("POST /api/v1/plans/validate", s.requireScope(store.ScopeManage, s.handleValidatePlan))
	mux.Handle("POST /api/v1/plans", s.requireScope(store.ScopeManage, s.handleCreatePlan))
	mux.Handle("PUT /api/v1/plans/{ref}", s.requireScope(store.ScopeManage, s.handleUpdatePlan))
	mux.Handle("DELETE /api/v1/plans/{ref}", s.requireScope(store.ScopeManage, s.handleDeletePlan))

	mux.Handle("GET /api/v1/providers", s.authed(s.handleProviders))
	mux.Handle("GET /api/v1/doctor", s.authed(s.handleDoctor))

	// The write surface. Every one of these writes a row and returns; not one
	// of them touches a cluster, except /cleanup, which goes through
	// worker.Cleanup so that the only package holding a destructive provider
	// call stays the one with the guards and the tests for it.
	mux.Handle("GET /api/v1/queue", s.authed(s.handleQueue))
	mux.Handle("POST /api/v1/recovery-runs", s.requireScope(store.ScopeOperate, s.handleTriggerRun))
	mux.Handle("POST /api/v1/recovery-runs/{id}/cancel", s.requireScope(store.ScopeOperate, s.handleCancelRun))
	mux.Handle("POST /api/v1/cleanup/{vmid}", s.requireScope(store.ScopeOperate, s.handleCleanup))

	// Anything else: the dashboard, or - for a path under /api/ that matched
	// no route, and for an API-only deployment - a problem document, not
	// net/http's plain-text 404. A client that parses problem+json must not
	// have to special-case the one response that is not.
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, newProblem("no-such-route", "No such endpoint", http.StatusNotFound,
		"this API serves /api/v1; see docs/api.md"))
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

// writeJSON renders v as a 200.
//
// Every read route answers 200 or a problem document, so the status is not a
// parameter here: it is one on writeJSONStatus, which the write routes use
// because 201 and 202 carry meaning a 200 would throw away.
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	writeJSONStatus(w, r, http.StatusOK, v)
}

// writeJSONStatus renders v as the response body under an explicit status.
//
// It marshals before writing anything: encoding straight into the
// ResponseWriter would emit the header and then fail halfway through the
// body, which a client cannot tell from a truncated network read.
func writeJSONStatus(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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
		// Before Shutdown, not after: Shutdown waits for in-flight requests
		// and an event stream is one that would not end until its drill did.
		// Telling the streams first is what keeps a stop from costing the
		// whole grace period and then reporting DeadlineExceeded.
		if d, ok := h.(Draining); ok {
			d.Drain()
		}
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
