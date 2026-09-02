package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
)

// confidenceHistoryDepth is how many past drills the score looks at. Twenty
// is enough for a failure rate to mean something and few enough to stay one
// query.
const confidenceHistoryDepth = 20

// workloadDTO describes one workload on the cluster.
type workloadDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Node    string `json:"node,omitempty"`
	Cluster string `json:"cluster,omitempty"`

	Tags []string `json:"tags,omitempty"`

	CPUCores    int   `json:"cpu_cores"`
	MemoryBytes int64 `json:"memory_bytes"`
	DiskBytes   int64 `json:"disk_bytes"`

	PowerState string `json:"power_state"`
	Template   bool   `json:"template"`

	// Managed marks a workload RestoreLab created for a drill.
	Managed       bool   `json:"managed"`
	RecoveryRunID string `json:"recovery_run_id,omitempty"`

	// LastRunID, LastRunAt, LastRunState and LastRunResult describe the
	// workload's most recent drill, all absent when it has never had one.
	//
	// LastRunAt is when that drill started, not when it finished: a drill
	// still running has no completion time, and "last tested" has to have an
	// answer while it is in flight. The state is here as well as the result
	// because a run still going and a run that was cancelled both carry an
	// empty result, and they are not the same news.
	LastRunID     string     `json:"last_run_id,omitempty"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastRunState  string     `json:"last_run_state,omitempty"`
	LastRunResult string     `json:"last_run_result,omitempty"`

	Status *workloadStatusDTO `json:"status,omitempty"`
}

// workloadStatusDTO is the live state of a workload, when it could be read.
type workloadStatusDTO struct {
	PowerState  string   `json:"power_state"`
	UptimeSecs  float64  `json:"uptime_seconds"`
	AgentReady  bool     `json:"agent_ready"`
	IPs         []string `json:"ips,omitempty"`
	CPUUsage    float64  `json:"cpu_usage"`
	MemoryBytes int64    `json:"memory_bytes"`
}

func newWorkloadDTO(w core.Workload) workloadDTO {
	return workloadDTO{
		ID:            w.ID,
		Name:          w.Name,
		Kind:          string(w.Kind),
		Node:          w.Node,
		Cluster:       w.Cluster,
		Tags:          w.Tags,
		CPUCores:      w.CPUCores,
		MemoryBytes:   w.MemoryBytes,
		DiskBytes:     w.DiskBytes,
		PowerState:    string(w.PowerState),
		Template:      w.Template,
		Managed:       w.Managed,
		RecoveryRunID: w.RecoveryRunID,
	}
}

// applyLastRun copies a workload's most recent drill onto its DTO.
func applyLastRun(dto *workloadDTO, run store.RunSummary) {
	dto.LastRunID = run.ID
	dto.LastRunState = string(run.State)
	dto.LastRunResult = string(run.Result)
	if !run.StartedAt.IsZero() {
		at := run.StartedAt
		dto.LastRunAt = &at
	}
}

// withLastRuns fills in the last-drill fields of a page of workloads.
//
// Best-effort, exactly like the backup lookup in the confidence handler: an
// inventory that cannot be listed is a failure, but an inventory whose drill
// history cannot be read is still an inventory. The rows then arrive without
// their last-drill keys - the same shape a workload nobody has ever drilled
// has - and the screen showing them says "never tested", which is the honest
// thing to say when the history cannot be reached.
func (s *Server) withLastRuns(ctx context.Context, items []workloadDTO) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	last, err := s.history.LastRuns(ctx, ids)
	if err != nil {
		return
	}
	for i := range items {
		if run, ok := last[items[i].ID]; ok {
			applyLastRun(&items[i], run)
		}
	}
}

func newWorkloadStatusDTO(s *core.WorkloadStatus) *workloadStatusDTO {
	if s == nil {
		return nil
	}
	return &workloadStatusDTO{
		PowerState:  string(s.PowerState),
		UptimeSecs:  s.Uptime.Seconds(),
		AgentReady:  s.AgentReady,
		IPs:         s.IPs,
		CPUUsage:    s.CPUUsage,
		MemoryBytes: s.MemoryBytes,
	}
}

// confidenceDTO is the Recovery Confidence answer.
type confidenceDTO struct {
	WorkloadID string `json:"workload_id"`
	// Score is null when the workload has never been tested. A UI renders
	// that as "--", never as 0%: "we have no idea" and "we know it is bad"
	// are different answers.
	Score   *int     `json:"score"`
	Tested  bool     `json:"tested"`
	Reasons []string `json:"reasons"`

	LastRunID      string `json:"last_run_id,omitempty"`
	RunsConsidered int    `json:"runs_considered"`
}

// hypervisor resolves the provider a request asked for, answering the request
// itself when it cannot.
func (s *Server) hypervisor(w http.ResponseWriter, r *http.Request) (core.HypervisorProvider, bool) {
	id := r.URL.Query().Get("provider")
	hv, err := s.providers.Hypervisor(id)
	if err != nil || hv == nil {
		s.writeProviderProblem(w, r, id, err)
		return nil, false
	}
	return hv, true
}

// backups resolves the backup provider a request asked for.
func (s *Server) backups(w http.ResponseWriter, r *http.Request) (core.BackupProvider, bool) {
	id := r.URL.Query().Get("provider")
	bp, err := s.providers.Backups(id)
	if err != nil || bp == nil {
		s.writeProviderProblem(w, r, id, err)
		return nil, false
	}
	return bp, true
}

// writeProviderProblem tells a named provider that does not exist (the
// caller's mistake, 404) from having none usable at all (ours, 503).
func (s *Server) writeProviderProblem(w http.ResponseWriter, r *http.Request, id string, err error) {
	detail := "no provider is configured for this operation"
	if err != nil {
		detail = err.Error()
	}
	if id != "" {
		writeProblem(w, r, newProblem("no-such-provider", "No such provider",
			http.StatusNotFound, fmt.Sprintf("no usable provider %q: %s", id, detail)))
		return
	}
	writeProblem(w, r, newProblem("provider-unavailable", "No usable provider",
		http.StatusServiceUnavailable, detail))
}

// handleListWorkloads serves GET /api/v1/workloads.
//
// It queries the cluster on every call, with no cache: a cache would bring an
// invalidation question nobody has needed yet, and the day it is needed it
// will be an informed decision rather than a default.
func (s *Server) handleListWorkloads(w http.ResponseWriter, r *http.Request) {
	hv, ok := s.hypervisor(w, r)
	if !ok {
		return
	}

	workloads, err := hv.ListWorkloads(r.Context())
	if err != nil {
		writeProblem(w, r, problemForUpstream(err))
		return
	}

	includeTemporary := r.URL.Query().Get("temporary") == "true"
	out := page[workloadDTO]{Items: []workloadDTO{}}
	for _, workload := range workloads {
		if workload.Template {
			continue
		}
		if workload.Managed && !includeTemporary {
			continue
		}
		out.Items = append(out.Items, newWorkloadDTO(workload))
	}
	s.withLastRuns(r.Context(), out.Items)
	writeJSON(w, r, out)
}

// handleGetWorkload serves GET /api/v1/workloads/{id}.
func (s *Server) handleGetWorkload(w http.ResponseWriter, r *http.Request) {
	hv, ok := s.hypervisor(w, r)
	if !ok {
		return
	}

	workload, err := hv.GetWorkload(r.Context(), r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, problemForUpstream(err))
		return
	}

	dto := newWorkloadDTO(*workload)
	// The live status is a bonus, not the answer: a stopped workload has no
	// agent to ask, and that must not turn a description into an error.
	if status, err := hv.GetStatus(r.Context(), workload.ID); err == nil {
		dto.Status = newWorkloadStatusDTO(status)
	}

	// One workload, one id: the same call the listing makes, asking about a
	// page of one. There is no second code path to keep in step.
	items := []workloadDTO{dto}
	s.withLastRuns(r.Context(), items)
	dto = items[0]

	writeJSON(w, r, dto)
}

// handleWorkloadBackups serves GET /api/v1/workloads/{id}/backups.
func (s *Server) handleWorkloadBackups(w http.ResponseWriter, r *http.Request) {
	bp, ok := s.backups(w, r)
	if !ok {
		return
	}

	backups, err := bp.ListBackups(r.Context(), r.PathValue("id"))
	if err != nil && !errors.Is(err, core.ErrNoBackup) {
		writeProblem(w, r, problemForUpstream(err))
		return
	}

	// No backup is an empty list, not a 404: the workload exists, and "you
	// have nothing to restore" is the answer, not the absence of one.
	out := page[*report.BackupDTO]{Items: []*report.BackupDTO{}}
	for i := range backups {
		// Aged against the server's own clock rather than a wall clock read
		// inside the renderer: a handler whose output moves while its
		// injected clock stands still cannot be captured, and the golden
		// fixtures need this one to hold still.
		out.Items = append(out.Items, report.NewBackupDTOAt(&backups[i], s.now()))
	}
	writeJSON(w, r, out)
}

// handleWorkloadConfidence serves GET /api/v1/workloads/{id}/confidence.
//
// This is the endpoint the tool exists for - "how much can I count on this
// restore?" - and it only became possible when phase A gave report.Score a
// history to read. It had been written and wired to nothing since the start.
func (s *Server) handleWorkloadConfidence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	runs, err := s.history.ListRuns(r.Context(), store.Filter{
		WorkloadID: id,
		Limit:      confidenceHistoryDepth,
	})
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	in := report.ConfidenceInput{Now: s.now()}
	if len(runs) > 0 {
		in.LastRun = runFromSummary(runs[0])
		for _, r := range runs[1:] {
			in.History = append(in.History, runFromSummary(r))
		}
	}

	// The backup is looked up best-effort: a cluster that cannot be reached
	// costs the "no backup" penalty and says so in the reasons, which is far
	// more useful than refusing to answer at all.
	if bp, err := s.providers.Backups(r.URL.Query().Get("provider")); err == nil && bp != nil {
		if backup, err := bp.GetLatestBackup(r.Context(), id); err == nil && backup != nil {
			in.LatestBackupAt = backup.CreatedAt
		}
	}

	score := report.Score(in, s.weights)
	dto := confidenceDTO{
		WorkloadID:     id,
		Tested:         score.Tested,
		Reasons:        score.Reasons,
		RunsConsidered: len(runs),
	}
	if score.Tested {
		value := score.Score
		dto.Score = &value
		dto.LastRunID = runs[0].ID
	}
	if dto.Reasons == nil {
		dto.Reasons = []string{}
	}
	writeJSON(w, r, dto)
}

// runFromSummary rebuilds exactly as much of a run as report.Score reads:
// its result, its state, when it finished and its RTO against target.
//
// Loading the full runs instead would mean one query per run plus every step
// and check row, to grade a workload on five fields.
func runFromSummary(s store.RunSummary) *core.RecoveryRun {
	return &core.RecoveryRun{
		ID:               s.ID,
		SourceWorkloadID: s.SourceWorkloadID,
		State:            s.State,
		Result:           s.Result,
		StartedAt:        s.StartedAt,
		CompletedAt:      s.CompletedAt,
		RTO:              s.RTO,
		RTOTarget:        s.RTOTarget,
		CleanupDone:      s.CleanupDone,
	}
}
