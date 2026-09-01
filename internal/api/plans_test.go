package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

// manageSecret is a token that may write the catalogue and nothing else.
// Together with testSecret (read) and operateSecret (operate) it is what
// makes the scope tests mean something: three tokens, three sets of rights,
// and no implication between the last two.
const manageSecret = "rl_MANAGEMANAGEMANAGEMANAGEMANAGEMANAGEMANAGEMA"

// validPlanYAML is a plan a human wrote: a leading comment, quoted ids, and
// no defaults spelled out. The comment is load-bearing - a test asserts it
// comes back, which is what proves the document is stored verbatim.
const validPlanYAML = `# the web tier, restored nightly
name: web-tier
description: nightly drill
workload:
  provider: proxmox-main
  id: "110"
checks:
  - type: tcp
    port: 22
`

// --- test double --------------------------------------------------------------

// fakePlans is the catalogue slice of the store, backed by a map.
//
// It mirrors the real store closely enough for catalog.Save to behave the way
// it does against SQLite: a duplicate name is refused, an update bumps the
// version in the store rather than in the caller, and created_at survives a
// rewrite. Getting any of those wrong here would make the handler tests pass
// against a store that does not exist.
type fakePlans struct {
	stored map[string]store.Plan
	// err, when set, is what every method answers. It is how the 503 case is
	// driven: a deployment with no usable history database.
	err error
}

func newFakePlans() *fakePlans { return &fakePlans{stored: map[string]store.Plan{}} }

func (f *fakePlans) CreatePlan(_ context.Context, p store.Plan) error {
	if f.err != nil {
		return f.err
	}
	for _, existing := range f.stored {
		if existing.Name == p.Name {
			return store.ErrDuplicate
		}
	}
	f.stored[p.ID] = p
	return nil
}

func (f *fakePlans) UpdatePlan(_ context.Context, p store.Plan, expected int) error {
	if f.err != nil {
		return f.err
	}
	current, ok := f.stored[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	if expected > 0 && current.Version != expected {
		return store.ErrVersionConflict
	}
	// The store owns the version and the creation instant; a caller that
	// supplied either would be describing the row rather than writing it.
	p.Version = current.Version + 1
	p.CreatedAt = current.CreatedAt
	f.stored[p.ID] = p
	return nil
}

// GetPlan resolves a name first, then an id, exactly as the store does.
func (f *fakePlans) GetPlan(_ context.Context, ref string) (*store.Plan, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, p := range f.stored {
		if p.Name == ref {
			found := p
			return &found, nil
		}
	}
	if p, ok := f.stored[ref]; ok {
		return &p, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakePlans) ListPlans(_ context.Context, filter store.PlanFilter) ([]store.Plan, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []store.Plan
	for _, p := range f.stored {
		if filter.WorkloadID != "" && p.WorkloadID != filter.WorkloadID {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	limit := filter.Limit
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakePlans) DeletePlan(_ context.Context, ref string) error {
	if f.err != nil {
		return f.err
	}
	for id, p := range f.stored {
		if p.Name == ref || p.ID == ref {
			delete(f.stored, id)
			return nil
		}
	}
	return store.ErrNotFound
}

// --- helpers ------------------------------------------------------------------

// planServer wires a server over a catalogue, knowing all three tokens.
//
// operate is here even though no plan route needs it: the point of the scope
// tests is that a token which may trigger drills still cannot write the
// catalogue, and that needs the token to exist and be valid.
func planServer(t *testing.T) (*Server, *fakePlans) {
	t.Helper()
	plans := newFakePlans()
	s, tokens := newTestServer(t, Options{
		Plans: plans,
		Now:   func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
	})
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tokens.byHash[HashToken(operateSecret)] = store.APIToken{
		ID: "tok-operate", Name: "ops", Hash: HashToken(operateSecret),
		CreatedAt: created, Scopes: []string{store.ScopeOperate},
	}
	tokens.byHash[HashToken(manageSecret)] = store.APIToken{
		ID: "tok-manage", Name: "catalogue", Hash: HashToken(manageSecret),
		CreatedAt: created, Scopes: []string{store.ScopeManage},
	}
	return s, plans
}

// send is `post` generalised to a method: the catalogue is the first surface
// with a PUT and a DELETE on it, and `do` chooses neither the token nor a
// body.
func send(s *Server, method, secret, target, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// seedPlan writes a plan through the API, so a test that needs one starts
// from a row the handlers themselves produced rather than one hand-built to
// match what they expect.
func seedPlan(t *testing.T, s *Server, document string) planDTO {
	t.Helper()
	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", document)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seeding a plan: status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var dto planDTO
	decodePlan(t, rec, &dto)
	return dto
}

// decodePlan reads a JSON body, failing the test with the body itself when it
// is not what was expected - which is nearly always a problem document.
func decodePlan(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("body is not the JSON this test expects: %v\n%s", err, rec.Body)
	}
}

// withName returns validPlanYAML under another name and workload, so a
// listing has something to order and filter.
func withName(name, workload string) string {
	doc := strings.Replace(validPlanYAML, "name: web-tier", "name: "+name, 1)
	return strings.Replace(doc, `id: "110"`, `id: "`+workload+`"`, 1)
}

// --- reading ------------------------------------------------------------------

func TestListPlansIsOrderedByNameAndCarriesNoDocuments(t *testing.T) {
	s, _ := planServer(t)
	seedPlan(t, s, withName("c-plan", "110"))
	seedPlan(t, s, withName("a-plan", "104"))
	seedPlan(t, s, withName("b-plan", "110"))

	rec := do(s, http.MethodGet, "/api/v1/plans")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var p page[planDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 3 {
		t.Fatalf("the listing holds %d plans, want 3: %s", len(p.Items), rec.Body)
	}
	if p.Items[0].Name != "a-plan" || p.Items[2].Name != "c-plan" {
		t.Errorf("listing = %q…%q, want a-plan…c-plan", p.Items[0].Name, p.Items[2].Name)
	}
	// Fifty plans in a table must not mean fifty documents on the wire.
	if p.Items[0].YAML != "" {
		t.Errorf("the listing shipped a document:\n%s", p.Items[0].YAML)
	}
	if p.NextCursor != "" {
		t.Errorf("next_cursor = %q, want none: the catalogue is not paginated", p.NextCursor)
	}
}

func TestListPlansFiltersByWorkload(t *testing.T) {
	s, _ := planServer(t)
	seedPlan(t, s, withName("a-plan", "104"))
	seedPlan(t, s, withName("b-plan", "110"))

	rec := do(s, http.MethodGet, "/api/v1/plans?workload=110")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var p page[planDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 || p.Items[0].Name != "b-plan" {
		t.Fatalf("filtered listing = %s, want only b-plan", rec.Body)
	}
}

func TestGetPlanReturnsTheDocumentVerbatim(t *testing.T) {
	s, _ := planServer(t)
	created := seedPlan(t, s, validPlanYAML)

	rec := do(s, http.MethodGet, "/api/v1/plans/web-tier")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var got planDTO
	decodePlan(t, rec, &got)
	if got.ID != created.ID {
		t.Errorf("id = %q, want %q", got.ID, created.ID)
	}
	if got.YAML != validPlanYAML {
		t.Errorf("the document was rewritten:\n%s", got.YAML)
	}
	if got.WorkloadID != "110" || got.ProviderID != "proxmox-main" {
		t.Errorf("derived = %q/%q, want 110/proxmox-main", got.WorkloadID, got.ProviderID)
	}
	if got.Description != "nightly drill" {
		t.Errorf("description = %q, want %q", got.Description, "nightly drill")
	}
}

func TestGetPlanCanReturnTheDocumentNaked(t *testing.T) {
	s, _ := planServer(t)
	seedPlan(t, s, validPlanYAML)

	rec := do(s, http.MethodGet, "/api/v1/plans/web-tier?format=yaml")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml", got)
	}
	// Byte for byte: this response is meant to be redirected into a file.
	if rec.Body.String() != validPlanYAML {
		t.Errorf("body =\n%s\nwant\n%s", rec.Body, validPlanYAML)
	}
}

func TestGetAnUnknownPlanIs404(t *testing.T) {
	s, _ := planServer(t)

	rec := do(s, http.MethodGet, "/api/v1/plans/nothing-here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q", got, problemContentType)
	}
}

// --- writing ------------------------------------------------------------------

func TestCreatePlanAnswers201WithALocation(t *testing.T) {
	s, plans := planServer(t)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", validPlanYAML)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	var got planDTO
	decodePlan(t, rec, &got)
	if got.Version != 1 || got.Name != "web-tier" {
		t.Errorf("plan = %q v%d, want web-tier v1", got.Name, got.Version)
	}
	if want := "/api/v1/plans/" + got.ID; rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if got.YAML != validPlanYAML {
		t.Errorf("the created plan does not carry its document back:\n%s", got.YAML)
	}
	if len(plans.stored) != 1 {
		t.Fatalf("the catalogue holds %d plans, want 1", len(plans.stored))
	}
}

func TestCreatePlanRefusesATakenName(t *testing.T) {
	s, plans := planServer(t)
	seedPlan(t, s, validPlanYAML)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", validPlanYAML)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: a POST must not replace somebody else's plan: %s",
			rec.Code, rec.Body)
	}
	if len(plans.stored) != 1 {
		t.Errorf("the catalogue holds %d plans, want 1", len(plans.stored))
	}
}

func TestCreatePlanRefusesAnInvalidDocument(t *testing.T) {
	s, plans := planServer(t)

	for name, document := range map[string]string{
		// The workload id is what a drill restores; a plan without one names
		// nothing.
		"no workload id": "name: broken\nworkload:\n  provider: proxmox-main\n  id: \"\"\n",
		// KnownFields: a typo must not be stored as a field nobody reads.
		"unknown field": validPlanYAML + "typo_here: 3\n",
		"not yaml":      "name: [unclosed\n",
	} {
		rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", document)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}

	// The detail must name what is wrong, not merely say that something is.
	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans",
		"name: broken\nworkload:\n  provider: proxmox-main\n  id: \"\"\n")
	var p Problem
	decodePlan(t, rec, &p)
	if !strings.Contains(p.Detail, "workload.id") {
		t.Errorf("detail = %q, want it to name workload.id", p.Detail)
	}

	if len(plans.stored) != 0 {
		t.Fatalf("the catalogue holds %d plans, want 0: an invalid document was stored", len(plans.stored))
	}
}

func TestCreatePlanRefusesAnEmptyBody(t *testing.T) {
	s, _ := planServer(t)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// JSON is a subset of YAML, so a dashboard can post the plan as JSON and this
// project does not have to invent a second schema for the same object.
func TestCreatePlanAcceptsJSON(t *testing.T) {
	s, _ := planServer(t)
	document := `{"name":"web-tier","workload":{"provider":"proxmox-main","id":"110"}}`

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", document)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	var got planDTO
	decodePlan(t, rec, &got)
	if got.Name != "web-tier" || got.WorkloadID != "110" {
		t.Errorf("plan = %q/%q, want web-tier/110", got.Name, got.WorkloadID)
	}
	if got.YAML != document {
		t.Errorf("the JSON document was rewritten:\n%s", got.YAML)
	}
}

func TestUpdatePlanReplacesAndBumpsTheVersion(t *testing.T) {
	s, plans := planServer(t)
	created := seedPlan(t, s, validPlanYAML)

	changed := strings.Replace(validPlanYAML, `id: "110"`, `id: "104"`, 1)
	rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/web-tier", changed)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var got planDTO
	decodePlan(t, rec, &got)
	if got.Version != 2 {
		t.Errorf("version = %d, want 2", got.Version)
	}
	if got.ID != created.ID {
		t.Errorf("id = %q, want the plan to keep %q across a rewrite", got.ID, created.ID)
	}
	// The derived columns follow the text; the text is what carries authority.
	if got.WorkloadID != "104" {
		t.Errorf("workload_id = %q, want 104", got.WorkloadID)
	}
	if len(plans.stored) != 1 {
		t.Errorf("the catalogue holds %d plans, want 1: an update created a second row", len(plans.stored))
	}
}

func TestUpdatePlanRefusesAStaleExpectedVersion(t *testing.T) {
	s, _ := planServer(t)
	seedPlan(t, s, validPlanYAML)

	// Bring it to v2, then send the guard the first reader would have held.
	if rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/web-tier", validPlanYAML); rec.Code != http.StatusOK {
		t.Fatalf("first update: status = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/web-tier?version=1", validPlanYAML)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: the plan moved under the caller: %s", rec.Code, rec.Body)
	}
}

func TestUpdatePlanAcceptsTheCurrentVersionAsAGuard(t *testing.T) {
	s, _ := planServer(t)
	seedPlan(t, s, validPlanYAML)

	rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/web-tier?version=1", validPlanYAML)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got planDTO
	decodePlan(t, rec, &got)
	if got.Version != 2 {
		t.Errorf("version = %d, want 2", got.Version)
	}
}

func TestUpdatePlanRefusesAnUnreadableVersionGuard(t *testing.T) {
	s, _ := planServer(t)
	seedPlan(t, s, validPlanYAML)

	for _, raw := range []string{"nought", "0", "-1"} {
		rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/web-tier?version="+raw, validPlanYAML)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?version=%s: status = %d, want 400: %s", raw, rec.Code, rec.Body)
		}
	}
}

func TestUpdateAnUnknownPlanIs404(t *testing.T) {
	s, plans := planServer(t)

	rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/nothing-here", validPlanYAML)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	// A PUT is not a create: the URL named a plan that is not there.
	if len(plans.stored) != 0 {
		t.Errorf("the catalogue holds %d plans, want 0", len(plans.stored))
	}
}

func TestDeletePlanAnswers204AndTheGetThen404s(t *testing.T) {
	s, plans := planServer(t)
	seedPlan(t, s, validPlanYAML)

	rec := send(s, http.MethodDelete, manageSecret, "/api/v1/plans/web-tier", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a 204 carried a body: %s", rec.Body)
	}
	if len(plans.stored) != 0 {
		t.Errorf("the catalogue holds %d plans, want 0", len(plans.stored))
	}

	if rec := do(s, http.MethodGet, "/api/v1/plans/web-tier"); rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", rec.Code)
	}
}

func TestDeleteAnUnknownPlanIs404(t *testing.T) {
	s, _ := planServer(t)

	rec := send(s, http.MethodDelete, manageSecret, "/api/v1/plans/nothing-here", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// --- scopes -------------------------------------------------------------------

func TestAReadTokenCannotWriteTheCatalogue(t *testing.T) {
	s, plans := planServer(t)
	seedPlan(t, s, validPlanYAML)

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/plans", validPlanYAML},
		{http.MethodPut, "/api/v1/plans/web-tier", validPlanYAML},
		{http.MethodDelete, "/api/v1/plans/web-tier", ""},
	} {
		rec := send(s, c.method, testSecret, c.target, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a read token = %d, want 403: %s",
				c.method, c.target, rec.Code, rec.Body)
		}
	}

	// The token is fine; the action was not. A 401 here would send a caller
	// to regenerate credentials that work.
	if rec := do(s, http.MethodGet, "/api/v1/plans"); rec.Code != http.StatusOK {
		t.Errorf("GET with the same token = %d, want 200", rec.Code)
	}
	if len(plans.stored) != 1 {
		t.Errorf("the catalogue holds %d plans, want 1: a read token changed it", len(plans.stored))
	}
}

// operate does not imply manage. A dashboard given the right to launch a
// drill has no business rewriting the definition of what it launches.
func TestAnOperateTokenCannotWriteTheCatalogue(t *testing.T) {
	s, plans := planServer(t)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/plans", validPlanYAML)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seeding: status = %d, want 201: %s", rec.Code, rec.Body)
	}

	for _, c := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/plans", withName("other", "104")},
		{http.MethodPut, "/api/v1/plans/web-tier", validPlanYAML},
		{http.MethodDelete, "/api/v1/plans/web-tier", ""},
	} {
		rec := send(s, c.method, operateSecret, c.target, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with an operate token = %d, want 403: %s",
				c.method, c.target, rec.Code, rec.Body)
		}
	}
	if len(plans.stored) != 1 {
		t.Errorf("the catalogue holds %d plans, want 1: an operate token changed it", len(plans.stored))
	}
}

// And the other way round: manage does not imply operate. Deciding what a
// drill is and launching one are two different powers.
func TestAManageTokenCannotTriggerADrill(t *testing.T) {
	s, _ := planServer(t)

	rec := send(s, http.MethodPost, manageSecret, "/api/v1/recovery-runs", `{"workload_id":"110"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /recovery-runs with a manage token = %d, want 403: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q", got, problemContentType)
	}

	// Reading is implied by every token, so the manage token still sees the
	// catalogue - which is what makes the 403 above a statement about the
	// action rather than about the token.
	if rec := send(s, http.MethodGet, manageSecret, "/api/v1/plans", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /plans with a manage token = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestTheCatalogueNeedsAToken(t *testing.T) {
	s, _ := planServer(t)

	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/plans"},
		{http.MethodGet, "/api/v1/plans/web-tier"},
		{http.MethodPost, "/api/v1/plans"},
		{http.MethodPut, "/api/v1/plans/web-tier"},
		{http.MethodDelete, "/api/v1/plans/web-tier"},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", c.method, c.target, rec.Code)
		}
	}
}

// --- no history ---------------------------------------------------------------

// A deployment with no usable history database answers 503 on all five
// routes: an empty catalogue would be a lie, and a caller told "no plans"
// would create the one it thinks is missing.
func TestTheCatalogueIs503WithoutAHistoryDatabase(t *testing.T) {
	s, plans := planServer(t)
	plans.err = store.ErrNoHistory

	for _, c := range []struct {
		method, secret, target, body string
	}{
		{http.MethodGet, testSecret, "/api/v1/plans", ""},
		{http.MethodGet, testSecret, "/api/v1/plans/web-tier", ""},
		{http.MethodPost, manageSecret, "/api/v1/plans", validPlanYAML},
		{http.MethodPut, manageSecret, "/api/v1/plans/web-tier", validPlanYAML},
		{http.MethodDelete, manageSecret, "/api/v1/plans/web-tier", ""},
	} {
		rec := send(s, c.method, c.secret, c.target, c.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503: %s", c.method, c.target, rec.Code, rec.Body)
		}
	}
}

// A Server built without a catalogue must answer, not panic: store.Noop
// stands in, and it refuses everything with ErrNoHistory.
func TestAServerWithNoCatalogueConfiguredIs503(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := do(s, http.MethodGet, "/api/v1/plans")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
}

// A PUT whose document names another plan is the rename this API does not do.
// The point of the test is not the 409, it is that nothing was written on the
// way to it: refusing after having already created the second plan would be a
// rename performed and then denied.
func TestUpdatePlanRefusesARenameWithoutWritingAnything(t *testing.T) {
	s, plans := planServer(t)
	seedPlan(t, s, validPlanYAML)

	rec := send(s, http.MethodPut, manageSecret, "/api/v1/plans/web-tier",
		withName("something-else", "110"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if len(plans.stored) != 1 {
		t.Fatalf("the catalogue holds %d plans, want 1: the refused rename wrote one", len(plans.stored))
	}
	for _, p := range plans.stored {
		if p.Name != "web-tier" || p.Version != 1 {
			t.Errorf("stored plan = %q v%d, want web-tier v1 untouched", p.Name, p.Version)
		}
	}
}

// A reference that matches nothing must talk about plans. problemFor answers
// "No such recovery run" there, which would send a reader looking at the
// wrong table entirely.
func TestAnUnknownPlanIsA404AboutPlans(t *testing.T) {
	s, _ := planServer(t)

	rec := send(s, http.MethodGet, testSecret, "/api/v1/plans/nothing", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	var p Problem
	decodePlan(t, rec, &p)
	if p.Title != "No such plan" {
		t.Errorf("title = %q, want %q", p.Title, "No such plan")
	}
}
