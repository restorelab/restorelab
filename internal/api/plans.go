package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/store"
)

// errBadVersion is what ?version= is refused with.
//
// It is a value rather than a formatted string so the message stays identical
// wherever the guard is read, and so the handler can answer a 400 without
// having to phrase the explanation itself.
var errBadVersion = errors.New(
	"version must be a positive integer: it is the version you expect this plan to be at")

// planDTO is a stored plan on the wire.
//
// YAML carries the document verbatim, which is what makes a round trip
// through this API lossless: a plan exported and re-imported is the same
// bytes, comments included.
type planDTO struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	WorkloadID  string    `json:"workload_id"`
	ProviderID  string    `json:"provider_id,omitempty"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	YAML        string    `json:"yaml,omitempty"`
}

// newPlanDTO renders a stored plan. withDocument is false in a listing: a
// catalogue of fifty plans must not ship fifty documents to draw a table.
func newPlanDTO(p store.Plan, withDocument bool) planDTO {
	dto := planDTO{
		ID: p.ID, Name: p.Name, Description: p.Description,
		WorkloadID: p.WorkloadID, ProviderID: p.ProviderID,
		Version: p.Version, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if withDocument {
		dto.YAML = p.YAML
	}
	return dto
}

// problemForPlan is problemFor with the two errors that name a thing: a
// reference that resolves to nothing, and one that resolves to several.
//
// problemFor speaks about runs there, because until now those were the only
// things this API resolved by reference. Telling someone their plan request
// found "no such recovery run" would send them looking at the wrong table.
// Everything else - the conflicts, the invalid document, the missing
// database - reads the same for both, and stays in one place.
func problemForPlan(err error) Problem {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return newProblem("not-found", "No such plan", http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrAmbiguous):
		return newProblem("ambiguous-id", "Ambiguous plan id", http.StatusConflict,
			"that prefix matches more than one plan: give a few more characters, or use the name")
	default:
		return problemFor(err)
	}
}

// handleListPlans serves the catalogue, ordered by name.
//
// It reuses page[T] so a client never meets two shapes of listing, but the
// envelope carries no cursor: the catalogue is dozens of rows on a stable
// ordering, and a keyset over that would be ceremony.
func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}

	plans, err := catalog.List(r.Context(), s.plans, store.PlanFilter{
		WorkloadID: r.URL.Query().Get("workload"),
		Limit:      limit,
	})
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}

	out := page[planDTO]{Items: []planDTO{}}
	for _, p := range plans {
		out.Items = append(out.Items, newPlanDTO(p, false))
	}
	writeJSON(w, r, out)
}

// handleGetPlan serves one plan, as JSON or as the document itself.
//
// ?format=yaml exists so `plan show` and a plain curl can write a file
// straight from the response, without a client having to pull one string out
// of a JSON object and unescape it.
func (s *Server) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	p, err := catalog.Get(r.Context(), s.plans, r.PathValue("ref"))
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}

	if r.URL.Query().Get("format") == "yaml" {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, p.YAML)
		return
	}
	writeJSON(w, r, newPlanDTO(*p, true))
}

// handleCreatePlan stores a new plan.
//
// The body is the plan document itself rather than an envelope around it.
// yaml.v3 reads JSON as YAML, so a dashboard can post JSON without this
// project inventing and maintaining a second schema for the same thing.
func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	document, ok := readDocument(w, r)
	if !ok {
		return
	}

	// Creating means creating: a POST onto a name that exists is a conflict,
	// not a silent replacement of somebody else's plan.
	p, _, err := catalog.Save(r.Context(), s.plans, document, catalog.CreateOnly)
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}

	w.Header().Set("Location", "/api/v1/plans/"+p.ID)
	writeJSONStatus(w, r, http.StatusCreated, newPlanDTO(*p, true))
}

// handleUpdatePlan replaces a plan and bumps its version.
//
// The plan is read before the body is: a PUT onto a plan that does not exist
// is a 404 about the URL, and answering that before parsing anything keeps a
// bad reference from being reported as a bad document.
func (s *Server) handleUpdatePlan(w http.ResponseWriter, r *http.Request) {
	expected, err := parseExpectedVersion(r.URL.Query().Get("version"))
	if err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}

	current, err := catalog.Get(r.Context(), s.plans, r.PathValue("ref"))
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}

	document, ok := readDocument(w, r)
	if !ok {
		return
	}

	// A PUT that renames the plan would leave the old name behind as a second
	// row, which is a rename nobody asked for. Refuse it: renaming is
	// deleting one plan and creating another, and that is worth saying out
	// loud rather than doing quietly.
	//
	// The check happens here, before anything is written, and that is the
	// whole point: deciding it from Save's result would mean deciding it
	// after Save had already created the second plan, so the refusal would
	// arrive with the damage already done.
	name, err := catalog.Name(document)
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}
	if name != current.Name {
		writeProblem(w, r, newProblem("rename-not-supported",
			"A plan cannot be renamed in place", http.StatusConflict,
			"the document names "+name+", but this URL is "+current.Name+
				"; delete the old plan and create the new one"))
		return
	}

	p, _, err := catalog.Save(r.Context(), s.plans, document, expected)
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}
	writeJSON(w, r, newPlanDTO(*p, true))
}

// handleDeletePlan removes a plan. Its runs keep their name and snapshot.
//
// No condition, and none needed: a drill in flight executes the snapshot
// taken when it was queued, never the catalogue row, so deleting a plan
// cannot disturb an execution or rewrite a report.
func (s *Server) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	if _, err := catalog.Delete(r.Context(), s.plans, r.PathValue("ref")); err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validatedDTO is a plan document's meaning, on the wire.
type validatedDTO struct {
	Valid       bool   `json:"valid"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	WorkloadID  string `json:"workload_id"`
	ProviderID  string `json:"provider_id,omitempty"`
	// NormalizedYAML is the document with its defaults applied. An editor
	// shows it as "here is what this actually says": the difference between
	// a field left out and a field left out *meaning something*.
	NormalizedYAML string `json:"normalized_yaml"`

	// ProofLevel and ProofSummary say what this plan would establish if
	// every check passed. The editor shows them beside the validation tick,
	// because "valid" and "worth running" are different questions and only
	// the first one used to have an answer here.
	ProofLevel   string `json:"proof_level,omitempty"`
	ProofSummary string `json:"proof_summary,omitempty"`
}

// handleValidatePlan reports whether a document is a valid plan, storing
// nothing.
//
// It exists for the dashboard's editor. Routing it through catalog.Validate
// rather than reimplementing the rules in a client is the same argument that
// put catalog between the API and the store: internal/plan is the only
// definition of a valid plan, and a second one in TypeScript would drift from
// it at the first field added on this side.
func (s *Server) handleValidatePlan(w http.ResponseWriter, r *http.Request) {
	document, ok := readDocument(w, r)
	if !ok {
		return
	}

	v, err := catalog.Validate(document)
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}

	writeJSON(w, r, validatedDTO{
		Valid:          true,
		Name:           v.Name,
		Description:    v.Description,
		WorkloadID:     v.WorkloadID,
		ProviderID:     v.ProviderID,
		NormalizedYAML: v.Normalised,
		ProofLevel:     v.ProofLevel,
		ProofSummary:   v.ProofSummary,
	})
}

// readDocument reads a plan document from a request body, capped.
//
// Capped by the same maxRequestBody the trigger endpoint uses: a plan is a
// few hundred bytes, and anything past the cap is not a request but a way to
// make the server allocate. An empty body is refused here rather than left to
// the parser, which would call it "EOF".
func readDocument(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	document, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeBadRequest(w, r, "the body could not be read")
		return nil, false
	}
	if len(document) == 0 {
		writeBadRequest(w, r, "the body must carry the plan document")
		return nil, false
	}
	return document, true
}

// parseExpectedVersion reads the optional ?version= guard.
//
// Absent means "overwrite whatever is there", which is what a `plan apply`
// from a CI pipeline means: it has no idea what the current version is and
// has no business guessing. Present and unparseable is refused rather than
// ignored - a guard that silently does nothing is worse than no guard.
func parseExpectedVersion(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errBadVersion
	}
	return n, nil
}
