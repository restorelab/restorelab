package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/adhoc"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
	"github.com/restorelab/restorelab/internal/worker"
)

// maxRequestBody caps what a write endpoint will read. A drill is described
// in a few hundred bytes; anything past this is not a request, it is a way to
// make the server allocate.
const maxRequestBody = 64 << 10

// triggerRequest is the body of POST /recovery-runs: the smallest terms that
// still describe a drill.
type triggerRequest struct {
	WorkloadID  string   `json:"workload_id"`
	Provider    string   `json:"provider,omitempty"`
	Backup      string   `json:"backup,omitempty"`
	Checks      []string `json:"checks,omitempty"`
	Network     string   `json:"network,omitempty"`
	Node        string   `json:"node,omitempty"`
	Storage     string   `json:"storage,omitempty"`
	Pool        string   `json:"pool,omitempty"`
	RTOTarget   string   `json:"rto_target,omitempty"`
	SkipStartup bool     `json:"skip_startup,omitempty"`
}

// handleTriggerRun queues a drill.
//
// It writes one row and calls no provider. The plan is built and validated
// here, synchronously, so a request that cannot become a drill is a 400 and
// never a queued row somebody has to explain later.
func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	var req triggerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeBadRequest(w, r, "the body is not the JSON this endpoint expects")
		return
	}
	if req.WorkloadID == "" {
		writeBadRequest(w, r, "workload_id is required: it names what to recovery-test")
		return
	}

	opts := adhoc.Options{
		WorkloadID:  req.WorkloadID,
		ProviderID:  req.Provider,
		Backup:      req.Backup,
		Checks:      req.Checks,
		Network:     req.Network,
		Node:        req.Node,
		Storage:     req.Storage,
		Pool:        req.Pool,
		SkipStartup: req.SkipStartup,
	}
	if req.RTOTarget != "" {
		d, err := time.ParseDuration(req.RTOTarget)
		if err != nil {
			writeBadRequest(w, r, "rto_target must be a duration such as 5m")
			return
		}
		opts.RTOTarget = d
	}
	if opts.ProviderID == "" && s.cfg != nil {
		opts.ProviderID = s.cfg.Defaults.Provider
	}

	p, err := adhoc.Plan(opts)
	if err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}

	// One drill per workload at a time. Two concurrent drills of the same
	// workload would restore the same backup twice, and a dashboard that
	// double-clicks must not queue two of them.
	active, err := s.history.ActiveRunForWorkload(r.Context(), req.WorkloadID)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	if active != "" {
		writeProblem(w, r, newProblem("already-running", "This workload already has a drill in flight",
			http.StatusConflict,
			fmt.Sprintf("run %s is queued or running for workload %s", active, req.WorkloadID)))
		return
	}

	planYAML, err := yaml.Marshal(p)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	run := &core.RecoveryRun{
		ID:               uuid.NewString(),
		PlanName:         p.Name,
		ProviderID:       opts.ProviderID,
		SourceWorkloadID: req.WorkloadID,
		State:            core.RunQueued,
		RTOTarget:        time.Duration(p.RTOTarget),
	}
	if err := s.history.Enqueue(r.Context(), run, string(planYAML), s.now()); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	w.Header().Set("Location", "/api/v1/recovery-runs/"+run.ID)
	writeJSONStatus(w, r, http.StatusCreated, newQueuedDTO(run, s.now()))
}

// handleCancelRun asks a drill to stop.
//
// The two answers are genuinely different states of the world, and the status
// says which. 200: the run was still queued, nothing had been created
// anywhere, and it is over. 202: a worker is executing it, has been told, and
// will tear down the temporary workload on its way out - the drill is not
// over yet, and a caller that read 200 here would report a machine gone that
// still exists.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.resolveRun(w, r)
	if !ok {
		return
	}

	settled, err := s.history.RequestCancel(r.Context(), run.ID, s.now())
	if err != nil {
		writeProblem(w, r, s.cancelProblem(r, run.ID, err))
		return
	}

	// Read the run back rather than describe it from here: the store decided
	// what happened, and a body assembled from what we hoped would happen is
	// how an API starts lying about its own writes.
	current, err := s.history.GetRun(r.Context(), run.ID)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	status := http.StatusAccepted
	if settled {
		status = http.StatusOK
	}
	writeJSONStatus(w, r, status, newRunDTO(current))
}

// cancelProblem maps a refused cancellation.
//
// RequestCancel refuses a run that has already settled, and says so without a
// sentinel error to match on. So the run is read back instead of the message
// parsed: a drill that is already over is the caller's mistake and a 409,
// anything else is ours and a 500.
func (s *Server) cancelProblem(r *http.Request, runID string, err error) Problem {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAmbiguous) ||
		errors.Is(err, store.ErrNoHistory) {
		return problemFor(err)
	}
	if current, gerr := s.history.GetRun(r.Context(), runID); gerr == nil && current.State.Terminal() {
		return newProblem("already-settled", "This drill is already over", http.StatusConflict,
			fmt.Sprintf("run %s is already %s", current.ID, current.State))
	}
	return problemFor(err)
}

// defaultTempIDMin and defaultTempIDMax are the reserved range a drill's
// temporary workload is allocated in when the provider entry does not narrow
// it. They mirror the provider's own defaults on purpose: this gate is the
// early one, so that a mistyped id never becomes a request to the cluster at
// all, and the provider still refuses independently before it deletes
// anything.
const (
	defaultTempIDMin = 9000
	defaultTempIDMax = 9999
)

// tempIDRange is the reserved range configured for a provider.
//
// It reads the configuration rather than asking the ProviderSet, because the
// point of the check is to answer before anything is asked of a provider at
// all.
func (s *Server) tempIDRange(providerID string) (int, int) {
	if s.cfg == nil {
		return defaultTempIDMin, defaultTempIDMax
	}
	if providerID == "" {
		providerID = s.cfg.Defaults.Provider
	}
	for _, p := range s.cfg.Providers {
		if p.ID != providerID || p.TempIDMin <= 0 || p.TempIDMax <= 0 {
			continue
		}
		return p.TempIDMin, p.TempIDMax
	}
	return defaultTempIDMin, defaultTempIDMax
}

// cleanupResponse says what was removed.
type cleanupResponse struct {
	WorkloadID string `json:"workload_id"`
	Removed    bool   `json:"removed"`
}

// handleCleanup destroys a temporary workload a drill left behind.
//
// It is the one endpoint that ends in a destructive call, and it makes that
// call through worker.Cleanup: the only package holding a mutating provider
// method stays the one that carries the guards and the tests for them. The
// reserved range is checked here, before a provider is even looked up, so
// that a mistyped production id is a 400 rather than a question asked of the
// cluster.
func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	providerID := r.URL.Query().Get("provider")

	low, high := s.tempIDRange(providerID)
	n, err := strconv.Atoi(vmid)
	if err != nil || n < low || n > high {
		writeBadRequest(w, r, fmt.Sprintf(
			"refusing to clean up %s: RestoreLab only ever creates workloads in its reserved range %d-%d, so anything outside it is not one of ours",
			strconv.Quote(vmid), low, high))
		return
	}

	hv, err := s.providers.Hypervisor(providerID)
	if err != nil {
		writeProblem(w, r, problemForUpstream(err))
		return
	}
	if err := worker.Cleanup(r.Context(), hv, vmid); err != nil {
		writeProblem(w, r, problemForUpstream(err))
		return
	}

	writeJSON(w, r, cleanupResponse{WorkloadID: vmid, Removed: true})
}

// handleQueue serves what is waiting and what is running.
//
// It is a listing, not a new source of truth: the same rows /recovery-runs
// serves, filtered on the states that have not settled, plus the lease that
// says which worker holds each one.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}

	// Scanned newest-first and filtered here rather than asked of the store,
	// because store.Filter carries one state and the queue spans nine. Doing
	// it in Go means the definition of "not settled" is core.RunState.Terminal
	// itself, so a state added there can never be silently missing from the
	// queue. The scan is bounded: a run that has not settled started, at the
	// latest, when it was queued, so it is among the newest rows there are.
	runs, err := s.history.ListRuns(r.Context(), store.Filter{Limit: maxPageSize})
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	out := page[queueEntryDTO]{Items: []queueEntryDTO{}}
	for _, run := range runs {
		if run.State.Terminal() {
			continue
		}
		out.Items = append(out.Items, s.newQueueEntryDTO(r, run))
		if len(out.Items) == limit {
			break
		}
	}
	writeJSON(w, r, out)
}

// queueEntryDTO is one waiting or running drill, with the lease over it.
type queueEntryDTO struct {
	runSummaryDTO
	// Worker is the worker holding this run, empty while it is still waiting
	// for one.
	Worker string `json:"worker,omitempty"`
	// LeaseExpiresAt is when that hold lapses. A lease in the past means the
	// worker stopped renewing and reconciliation has not swept the run yet.
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

// newQueueEntryDTO renders one queue row.
//
// A lease that cannot be read leaves the entry without a worker rather than
// failing the listing: the queue is what an operator looks at when something
// has gone wrong, and it must still render then.
func (s *Server) newQueueEntryDTO(r *http.Request, run store.RunSummary) queueEntryDTO {
	dto := queueEntryDTO{runSummaryDTO: newRunSummaryDTO(run)}
	owner, expires, err := s.history.RunLease(r.Context(), run.ID)
	if err != nil {
		return dto
	}
	dto.Worker = owner
	if !expires.IsZero() {
		at := expires
		dto.LeaseExpiresAt = &at
	}
	return dto
}

// newRunDTO renders a full run the way the listing renders a summary, so a
// write answers in the shape a read already speaks.
func newRunDTO(run *core.RecoveryRun) runSummaryDTO {
	return newRunSummaryDTO(store.RunSummary{
		ID:               run.ID,
		PlanName:         run.PlanName,
		SourceWorkloadID: run.SourceWorkloadID,
		SourceName:       run.SourceName,
		State:            run.State,
		Result:           run.Result,
		StartedAt:        run.StartedAt,
		CompletedAt:      run.CompletedAt,
		RTO:              run.RTO,
		RTOTarget:        run.RTOTarget,
		CleanupDone:      run.CleanupDone,
	})
}

// newQueuedDTO renders a run that has only just been queued: it has no
// progress to report yet, and its start time is the moment it was queued.
func newQueuedDTO(run *core.RecoveryRun, at time.Time) runSummaryDTO {
	dto := newRunDTO(run)
	dto.StartedAt = at
	return dto
}
