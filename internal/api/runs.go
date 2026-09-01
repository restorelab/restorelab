package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/report"
	"github.com/restorelab/restorelab/internal/store"
)

// runSummaryDTO is one row of the runs listing.
//
// It is a hand-maintained wire type, never core.RecoveryRun marshalled
// directly, for the same reason report.Document is: a refactor of the domain
// model must not silently change what consumers parse. Durations are emitted
// twice - a float to compare, a string to display - exactly as the report
// document does.
type runSummaryDTO struct {
	ID       string `json:"id"`
	PlanName string `json:"plan_name"`
	// PlanID is the stored plan this drill came from, absent when the drill
	// was described in the request rather than named. It is omitempty rather
	// than always present because "" would read as a plan whose id is empty,
	// and because a plan deleted later leaves its runs with no id at all -
	// they keep their name and their snapshot, which is what a report needs.
	PlanID           string `json:"plan_id,omitempty"`
	SourceWorkloadID string `json:"source_workload_id"`
	SourceName       string `json:"source_name,omitempty"`

	State  string `json:"state"`
	Result string `json:"result,omitempty"`

	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`

	RTOSeconds float64 `json:"rto_seconds"`
	RTO        string  `json:"rto"`

	RTOTargetSeconds float64 `json:"rto_target_seconds,omitempty"`
	RTOExceeded      bool    `json:"rto_exceeded"`

	CleanupDone bool `json:"cleanup_done"`
}

func newRunSummaryDTO(r store.RunSummary) runSummaryDTO {
	dto := runSummaryDTO{
		ID:               r.ID,
		PlanName:         r.PlanName,
		PlanID:           r.PlanID,
		SourceWorkloadID: r.SourceWorkloadID,
		SourceName:       r.SourceName,
		State:            string(r.State),
		Result:           string(r.Result),
		StartedAt:        r.StartedAt,
		RTOSeconds:       r.RTO.Seconds(),
		RTO:              report.FormatDuration(r.RTO),
		RTOTargetSeconds: r.RTOTarget.Seconds(),
		RTOExceeded:      r.RTOTarget > 0 && r.RTO > r.RTOTarget,
		CleanupDone:      r.CleanupDone,
	}
	// A run still going has no completion time. null says that; the zero
	// instant would read as "completed in 1970".
	if !r.CompletedAt.IsZero() {
		completed := r.CompletedAt
		dto.CompletedAt = &completed
	}
	return dto
}

// eventDTO is one line of a run's progress stream.
type eventDTO struct {
	Seq     int64            `json:"seq"`
	At      time.Time        `json:"at"`
	State   string           `json:"state"`
	Step    string           `json:"step,omitempty"`
	Status  string           `json:"status,omitempty"`
	Message string           `json:"message,omitempty"`
	Check   *report.CheckDTO `json:"check,omitempty"`
	Err     string           `json:"error,omitempty"`
}

func newEventDTO(e store.Event) eventDTO {
	dto := eventDTO{
		Seq:     e.Seq,
		At:      e.At,
		State:   string(e.State),
		Step:    e.Step,
		Status:  string(e.Status),
		Message: e.Message,
		Err:     e.Err,
	}
	if e.Check != nil {
		check := report.NewCheckDTO(*e.Check)
		dto.Check = &check
	}
	return dto
}

// handleListRuns serves GET /api/v1/recovery-runs.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}

	filter := store.Filter{
		WorkloadID: q.Get("workload"),
		State:      core.RunState(strings.ToUpper(q.Get("state"))),
		Result:     core.RunResult(strings.ToUpper(q.Get("result"))),
		Limit:      limit + 1, // one extra row is how we know there is a next page
	}

	if since := q.Get("since"); since != "" {
		at, err := parseSince(since, s.now())
		if err != nil {
			writeBadRequest(w, r, err.Error())
			return
		}
		filter.Since = at
	}

	if cursor := q.Get("cursor"); cursor != "" {
		pos, err := decodeCursor(cursor)
		if err != nil {
			writeBadRequest(w, r, err.Error())
			return
		}
		filter.After = &pos
	}

	runs, err := s.history.ListRuns(r.Context(), filter)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	out := page[runSummaryDTO]{Items: []runSummaryDTO{}}
	if len(runs) > limit {
		last := runs[limit-1]
		out.NextCursor = encodeCursor(store.Position{StartedAt: last.StartedAt, ID: last.ID})
		runs = runs[:limit]
	}
	for _, run := range runs {
		out.Items = append(out.Items, newRunSummaryDTO(run))
	}
	writeJSON(w, r, out)
}

// resolveRun loads the run a path variable names, answering the request when
// it cannot.
//
// The id may be a prefix, exactly as `runs show` accepts one: an exact match
// wins, an ambiguous prefix is a 409 rather than a guess.
func (s *Server) resolveRun(w http.ResponseWriter, r *http.Request) (*core.RecoveryRun, bool) {
	run, err := s.history.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		p := problemFor(err)
		if errors.Is(err, store.ErrNotFound) {
			p.Detail = scrubSecrets("no recorded drill matches " + strconv.Quote(r.PathValue("id")))
		}
		writeProblem(w, r, p)
		return nil, false
	}
	return run, true
}

// handleGetRun serves GET /api/v1/recovery-runs/{id}.
//
// The body is report.Document, the same schema `--format json` writes to a
// file and `/report?format=json` returns. One run, one wire shape, wherever
// it is read from.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.resolveRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, r, report.NewDocument(run))
}

// handleRunEvents serves GET /api/v1/recovery-runs/{id}/events.
//
// One endpoint, two representations, chosen by Accept: a dashboard follows
// the run live over text/event-stream, a script loops over the JSON page. The
// JSON shape is the one B1 shipped and must stay byte for byte what its
// clients already parse.
//
// ?after=<seq> resumes the JSON page; the stream resumes on Last-Event-ID.
// Both are the same replay, because both are the same Events(runID, afterSeq)
// query over the seq stored in run_events.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.resolveRun(w, r)
	if !ok {
		return
	}

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamEvents(w, r, run)
		return
	}

	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeBadRequest(w, r, "after must be a sequence number")
			return
		}
		after = n
	}

	events, err := s.history.Events(r.Context(), run.ID, after)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	out := page[eventDTO]{Items: []eventDTO{}}
	for _, e := range events {
		out.Items = append(out.Items, newEventDTO(e))
	}
	writeJSON(w, r, out)
}

// handleRunReport serves GET /api/v1/recovery-runs/{id}/report?format=json|html.
func (s *Server) handleRunReport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "html" {
		writeBadRequest(w, r, "format must be json or html")
		return
	}

	run, ok := s.resolveRun(w, r)
	if !ok {
		return
	}

	if format == "json" {
		writeJSON(w, r, report.NewDocument(run))
		return
	}

	// The HTML report is rendered into memory first: report.HTML writing
	// straight to the ResponseWriter would commit a 200 before a template
	// error could be reported as anything at all.
	var buf strings.Builder
	if err := report.HTML(&buf, run); err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(buf.String()))
}
