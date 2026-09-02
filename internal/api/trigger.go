package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/adhoc"
	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
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
	// Plan names a stored plan, by name or id. It is exclusive of every
	// ad-hoc field below: a drill either runs a plan somebody wrote down, or
	// it is described here, and merging the two would make "what does this
	// plan do" a question with no single answer.
	Plan string `json:"plan,omitempty"`

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

// adhocFields lists the ad-hoc fields this request set, so a body that mixes
// a plan with them can say exactly what it mixed.
//
// Naming them matters more than it looks: a caller that posts a plan plus a
// forgotten workload_id from an older script needs to be told which key to
// remove, not merely that two ways of describing a drill met.
func (r triggerRequest) adhocFields() []string {
	var set []string
	for name, used := range map[string]bool{
		"workload_id": r.WorkloadID != "", "provider": r.Provider != "",
		"backup": r.Backup != "", "checks": len(r.Checks) > 0,
		"network": r.Network != "", "node": r.Node != "",
		"storage": r.Storage != "", "pool": r.Pool != "",
		"rto_target": r.RTOTarget != "", "skip_startup": r.SkipStartup,
	} {
		if used {
			set = append(set, name)
		}
	}
	// Map iteration is random, and a 400 whose wording changes between two
	// identical requests is a message nobody can grep for.
	sort.Strings(set)
	return set
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

	p, stored, ok := s.planForTrigger(w, r, req)
	if !ok {
		return
	}

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
	if providerID == "" && s.cfg != nil {
		providerID = s.cfg.Defaults.Provider
	}

	// One drill per workload at a time. Two concurrent drills of the same
	// workload would restore the same backup twice, and a dashboard that
	// double-clicks must not queue two of them.
	active, err := s.history.ActiveRunForWorkload(r.Context(), workloadID)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	if active != "" {
		writeProblem(w, r, newProblem("already-running", "This workload already has a drill in flight",
			http.StatusConflict,
			fmt.Sprintf("run %s is queued or running for workload %s", active, workloadID)))
		return
	}

	// The snapshot is the defaulted plan re-marshalled, for a stored plan as
	// much as for an ad-hoc one: what the worker executes is this text, so
	// there is one shape of snapshot and it is never the catalogue row that
	// runs. Deleting or editing the plan afterwards cannot change what this
	// drill did.
	planYAML, err := yaml.Marshal(p)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	run := &core.RecoveryRun{
		ID:               s.newID(),
		PlanName:         p.Name,
		ProviderID:       providerID,
		SourceWorkloadID: workloadID,
		State:            core.RunQueued,
		RTOTarget:        time.Duration(p.RTOTarget),
	}
	// Provenance, and only when there is any: an ad-hoc drill came from
	// nowhere but this request, and a plan_id invented for it would point at
	// a row that does not exist.
	if stored != nil {
		run.PlanID = stored.ID
		run.PlanVersion = stored.Version
	}
	if err := s.history.Enqueue(r.Context(), run, string(planYAML), s.now()); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	w.Header().Set("Location", "/api/v1/recovery-runs/"+run.ID)
	writeJSONStatus(w, r, http.StatusCreated, newQueuedDTO(run, s.now()))
}

// planForTrigger builds the plan a request asks for, from the catalogue or
// from the ad-hoc description, and reports which stored plan it came from.
//
// It is the only place the two ways of asking for a drill differ. Everything
// past it - the conflict check, the snapshot, the row - exists once, so a
// drill launched by name and one described in the body cannot drift apart.
// It answers the request itself on refusal and reports false; a nil stored
// plan means the drill was described rather than named.
func (s *Server) planForTrigger(w http.ResponseWriter, r *http.Request, req triggerRequest) (*plan.Plan, *store.Plan, bool) {
	if req.Plan == "" {
		p, ok := s.adhocPlanForTrigger(w, r, req)
		return p, nil, ok
	}

	if mixed := req.adhocFields(); len(mixed) > 0 {
		writeBadRequest(w, r, "a request either names a plan or describes a drill, not both: "+
			"drop "+strings.Join(mixed, ", ")+", or drop \"plan\"")
		return nil, nil, false
	}

	// problemForPlan rather than problemFor: a reference that matches nothing
	// here is a missing plan, and a 404 reading "No such recovery run" would
	// send whoever got it looking at the wrong table. A stored plan that no
	// longer parses arrives wrapped in catalog.ErrInvalid and comes out a
	// 400 naming the field, which is what it is - the document is wrong, not
	// the database.
	row, parsed, err := catalog.Resolve(r.Context(), s.plans, req.Plan)
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return nil, nil, false
	}
	return parsed, row, true
}

// adhocPlanForTrigger builds the plan a body describes in place.
//
// This is the path that existed before the catalogue did, moved behind a
// function and otherwise untouched: the defaults, the refusals and their
// wording are the ones ad-hoc drills have always had.
func (s *Server) adhocPlanForTrigger(w http.ResponseWriter, r *http.Request, req triggerRequest) (*plan.Plan, bool) {
	if req.WorkloadID == "" {
		writeBadRequest(w, r, "workload_id is required: it names what to recovery-test")
		return nil, false
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
			return nil, false
		}
		opts.RTOTarget = d
	}
	if opts.ProviderID == "" && s.cfg != nil {
		opts.ProviderID = s.cfg.Defaults.Provider
	}

	p, err := adhoc.Plan(opts)
	if err != nil {
		writeBadRequest(w, r, err.Error())
		return nil, false
	}
	return p, true
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
		writeProblem(w, r, problemFor(err))
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

// tempIDRange is the reserved range configured for a provider.
//
// It reads the configuration rather than asking the ProviderSet, because the
// point of the check is to answer before anything is asked of a provider at
// all. Falling back to core.DefaultTempIDMin/Max on an unconfigured provider
// mirrors the provider's own defaults on purpose: this gate is the early
// one, so that a mistyped id never becomes a request to the cluster at all,
// and the provider still refuses independently before it deletes anything.
func (s *Server) tempIDRange(providerID string) (int, int) {
	if s.cfg == nil {
		return core.DefaultTempIDMin, core.DefaultTempIDMax
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
	return core.DefaultTempIDMin, core.DefaultTempIDMax
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

	// Filtered by the store, not in Go: the set of states that have not
	// settled lives in one place there (terminalStates, kept in step with
	// core.RunState.Terminal by a test), so a run in flight cannot be pushed
	// out of the page by finished runs that merely happen to be more recent.
	runs, err := s.history.ListRuns(r.Context(), store.Filter{NotTerminal: true, Limit: limit})
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	out := page[queueEntryDTO]{Items: []queueEntryDTO{}}
	for _, run := range runs {
		out.Items = append(out.Items, s.newQueueEntryDTO(r, run))
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
		PlanID:           run.PlanID,
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
