package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// operateSecret is a token that may write. testSecret deliberately may not:
// the pair is what makes the 403 tests mean anything.
const operateSecret = "rl_OPERATEOPERATEOPERATEOPERATEOPERATEOPERATEOP"

// operatingServer wires a server that knows two tokens - the read-only
// testSecret and operateSecret - over the history and providers given.
func operatingServer(t *testing.T, history *fakeHistory, providers ProviderSet) *Server {
	t.Helper()
	opts := Options{
		History: history,
		Config:  &config.Config{Defaults: config.Defaults{Provider: "pve"}},
		Now:     func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
	}
	if providers != nil {
		opts.Providers = providers
	}
	s, tokens := newTestServer(t, opts)
	tokens.byHash[HashToken(operateSecret)] = store.APIToken{
		ID: "tok-operate", Name: "ops", Hash: HashToken(operateSecret),
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Scopes:    []string{store.ScopeOperate},
	}
	return s
}

// triggerServer wires a server that can both queue drills and hold a
// catalogue, knowing all three tokens.
//
// It is deliberately not operatingServer with an extra argument: the ad-hoc
// tests above are the proof that the ad-hoc path did not change when the
// stored-plan path was added, and they are only proof for as long as they
// keep calling what they always called.
func triggerServer(t *testing.T) (*Server, *fakeHistory, *fakePlans) {
	t.Helper()
	history, plans := newFakeHistory(), newFakePlans()
	s, tokens := newTestServer(t, Options{
		History:   history,
		Plans:     plans,
		Providers: fakeProviders{hv: testFleet(t), bp: backupSource{}},
		Config:    &config.Config{Defaults: config.Defaults{Provider: "pve"}},
		Now:       func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
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
	return s, history, plans
}

// post sends a write request under a chosen token. `do` has neither a body
// nor a choice of secret, on purpose: until now nothing needed either.
func post(s *Server, secret, target, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, target, nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// strictProviders fails the test the moment anything asks it for a provider.
// It is how "the provider was never called at all" is proven rather than
// asserted.
type strictProviders struct{ t *testing.T }

func (p strictProviders) Entries() []config.Provider {
	p.t.Error("the API listed providers when it should not have looked at all")
	return nil
}

func (p strictProviders) Hypervisor(string) (core.HypervisorProvider, error) {
	p.t.Error("the API reached for a hypervisor before checking the reserved id range")
	return nil, nil
}

func (p strictProviders) Backups(string) (core.BackupProvider, error) {
	p.t.Error("the API reached for a backup provider when it should not have")
	return nil, nil
}

// cleanableFleet is the read-only fleet with the one destructive call the
// cleanup endpoint is allowed to make, and only that one.
type cleanableFleet struct {
	*fleet
	deleted []string
}

func (c *cleanableFleet) Delete(_ context.Context, id string) error {
	c.deleted = append(c.deleted, id)
	return nil
}

// The distinction a write API must not get wrong: a valid token that may not
// do this is 403. Answering 401 would tell a caller its credentials are
// broken when they are fine, and send it to regenerate a token that works.
func TestAReadTokenCannotTrigger(t *testing.T) {
	s := operatingServer(t, newFakeHistory(), fakeProviders{hv: testFleet(t)})

	rec := post(s, testSecret, "/api/v1/recovery-runs", `{"workload_id":"110"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST with a read token = %d, want 403: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q", got, problemContentType)
	}

	if rec := do(s, http.MethodGet, "/api/v1/recovery-runs"); rec.Code != http.StatusOK {
		t.Fatalf("GET with the same token = %d, want 200: the token is fine, the action was not", rec.Code)
	}
}

func TestTriggeringQueuesARunAndReturnsIt(t *testing.T) {
	history := newFakeHistory()
	s := operatingServer(t, history, fakeProviders{hv: testFleet(t), bp: backupSource{}})

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"workload_id":"110"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	var dto runSummaryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a run: %v", err)
	}
	if dto.ID == "" {
		t.Fatal("the response carries no run id")
	}
	if want := "/api/v1/recovery-runs/" + dto.ID; rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}

	if len(history.queued) != 1 {
		t.Fatalf("the store holds %d queued runs, want 1", len(history.queued))
	}
	q := history.queued[0]
	if q.run.ID != dto.ID {
		t.Errorf("queued run id = %q, want %q", q.run.ID, dto.ID)
	}
	if q.run.State != core.RunQueued {
		t.Errorf("queued run state = %q, want QUEUED", q.run.State)
	}
	if q.run.SourceWorkloadID != "110" {
		t.Errorf("queued run workload = %q, want 110", q.run.SourceWorkloadID)
	}
	if !strings.Contains(q.plan, "110") {
		t.Errorf("the plan snapshot does not mention the workload:\n%s", q.plan)
	}
	// The fleet fails the test on every mutating call; reaching here at all
	// is the assertion that nothing was asked of the provider.
}

func TestAnUnusableDrillIsRefusedBeforeItIsQueued(t *testing.T) {
	history := newFakeHistory()
	s := operatingServer(t, history, fakeProviders{hv: testFleet(t)})

	for name, body := range map[string]string{
		"no workload_id": `{}`,
		"nonsense check": `{"workload_id":"110","checks":["not-a-check"]}`,
	} {
		rec := post(s, operateSecret, "/api/v1/recovery-runs", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}

	if len(history.queued) != 0 {
		t.Fatalf("the queue holds %d runs, want 0: an unusable drill was queued", len(history.queued))
	}
}

func TestOneDrillPerWorkloadAtATime(t *testing.T) {
	history := newFakeHistory()
	history.add(core.RecoveryRun{
		ID: "run-in-flight", PlanName: "adhoc-110", SourceWorkloadID: "110",
		State: core.RunQueued, StartedAt: time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
	})
	s := operatingServer(t, history, fakeProviders{hv: testFleet(t)})

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"workload_id":"110"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "run-in-flight") {
		t.Errorf("the 409 does not name the run already in flight: %s", rec.Body)
	}
	if len(history.queued) != 0 {
		t.Fatalf("a second drill was queued for the same workload")
	}
}

func TestCancellingAQueuedRunSettlesIt(t *testing.T) {
	history := newFakeHistory()
	history.add(core.RecoveryRun{
		ID: "run-queued", PlanName: "adhoc-110", SourceWorkloadID: "110",
		State: core.RunQueued, StartedAt: time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
	})
	s := operatingServer(t, history, nil)

	rec := post(s, operateSecret, "/api/v1/recovery-runs/run-queued/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var dto runSummaryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a run: %v", err)
	}
	if dto.State != string(core.RunCancelled) {
		t.Errorf("state = %q, want CANCELLED", dto.State)
	}
	if history.byID["run-queued"].State != core.RunCancelled {
		t.Errorf("the store still says %q", history.byID["run-queued"].State)
	}
}

func TestCancellingARunningRunIsAccepted(t *testing.T) {
	history := newFakeHistory()
	history.add(core.RecoveryRun{
		ID: "run-running", PlanName: "adhoc-110", SourceWorkloadID: "110",
		State: core.RunRestoring, StartedAt: time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
	})
	s := operatingServer(t, history, nil)

	rec := post(s, operateSecret, "/api/v1/recovery-runs/run-running/cancel", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the worker will notice, the drill is not over yet: %s",
			rec.Code, rec.Body)
	}
	if len(history.cancelAsked) != 1 || history.cancelAsked[0] != "run-running" {
		t.Errorf("cancellation asked for %v, want [run-running]", history.cancelAsked)
	}
	if history.byID["run-running"].State != core.RunRestoring {
		t.Errorf("the API settled a running drill itself: state = %q", history.byID["run-running"].State)
	}
}

func TestCancellingAnAlreadySettledRunIsRefused(t *testing.T) {
	history := newFakeHistory()
	history.add(core.RecoveryRun{
		ID: "run-done", PlanName: "adhoc-110", SourceWorkloadID: "110",
		State: core.RunSuccess, Result: core.ResultSuccess,
		StartedAt:   time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 1, 11, 5, 0, 0, time.UTC),
	})
	s := operatingServer(t, history, nil)

	rec := post(s, operateSecret, "/api/v1/recovery-runs/run-done/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: a drill that is already over cannot be cancelled again: %s",
			rec.Code, rec.Body)
	}
	if history.byID["run-done"].State != core.RunSuccess {
		t.Errorf("cancelling a settled run must not touch its state: %q", history.byID["run-done"].State)
	}
}

func TestCleanupRefusesAnIdOutsideTheReservedRange(t *testing.T) {
	s := operatingServer(t, newFakeHistory(), strictProviders{t: t})

	rec := post(s, operateSecret, "/api/v1/cleanup/110", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	// strictProviders errors the test if anything reached for a provider, so
	// a clean run here is the assertion that the cluster was never called.
}

func TestCleanupRemovesAWorkloadInTheReservedRange(t *testing.T) {
	hv := &cleanableFleet{fleet: testFleet(t)}
	s := operatingServer(t, newFakeHistory(), fakeProviders{hv: hv})

	rec := post(s, operateSecret, "/api/v1/cleanup/9001", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(hv.deleted) != 1 || hv.deleted[0] != "9001" {
		t.Fatalf("deleted = %v, want [9001]", hv.deleted)
	}
}

func TestACleanupNeedsTheOperateScope(t *testing.T) {
	s := operatingServer(t, newFakeHistory(), strictProviders{t: t})

	if rec := post(s, testSecret, "/api/v1/cleanup/9001", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestTheQueueShowsOnlyWhatHasNotSettled(t *testing.T) {
	history := newFakeHistory()
	history.add(core.RecoveryRun{
		ID: "run-done", PlanName: "adhoc-110", SourceWorkloadID: "110",
		State: core.RunSuccess, Result: core.ResultSuccess,
		StartedAt:   time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 1, 10, 5, 0, 0, time.UTC),
	})
	history.add(core.RecoveryRun{
		ID: "run-waiting", PlanName: "adhoc-120", SourceWorkloadID: "120",
		State: core.RunRestoring, StartedAt: time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
	})
	history.leases["run-waiting"] = fakeLease{
		owner:   "worker-a",
		expires: time.Date(2026, 9, 1, 11, 2, 0, 0, time.UTC),
	}
	s := operatingServer(t, history, nil)

	rec := do(s, http.MethodGet, "/api/v1/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var p page[queueEntryDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("the queue holds %d entries, want 1: %s", len(p.Items), rec.Body)
	}
	if p.Items[0].ID != "run-waiting" {
		t.Errorf("queue holds %q, want run-waiting", p.Items[0].ID)
	}
	if p.Items[0].Worker != "worker-a" {
		t.Errorf("worker = %q, want worker-a: the queue must say who holds a drill", p.Items[0].Worker)
	}
}

// --- triggering by naming a stored plan ---------------------------------------

func TestTriggerByPlanQueuesTheStoredPlan(t *testing.T) {
	s, history, _ := triggerServer(t)
	seeded := seedPlan(t, s, validPlanYAML) // name: web-tier, workload 110

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"web-tier"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	if len(history.queued) != 1 {
		t.Fatalf("the store holds %d queued runs, want 1", len(history.queued))
	}
	q := history.queued[0]
	if q.run.PlanName != "web-tier" {
		t.Errorf("plan name = %q, want web-tier", q.run.PlanName)
	}
	if q.run.PlanID != seeded.ID || q.run.PlanVersion != 1 {
		t.Errorf("provenance = %q/v%d, want %q/v1",
			q.run.PlanID, q.run.PlanVersion, seeded.ID)
	}
	// The snapshot is the defaulted plan, not the document as written: the
	// document says nothing about the network, and what the worker executes
	// must say "isolated" out loud.
	if !strings.Contains(q.plan, "network: isolated") {
		t.Errorf("the snapshot is not the defaulted plan:\n%s", q.plan)
	}

	// The response carries the provenance too, so a caller that queued a
	// drill knows which plan it will execute without reading it back.
	var dto runSummaryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a run: %v", err)
	}
	if dto.PlanID != seeded.ID {
		t.Errorf("plan_id = %q, want %q", dto.PlanID, seeded.ID)
	}
	// The fleet fails the test on every mutating call; reaching here at all
	// is the assertion that nothing was asked of the provider.
}

// The body of a trigger-by-plan says nothing about a workload, so the id the
// run carries can only have come from the stored document.
func TestTriggerByPlanTakesTheWorkloadFromThePlan(t *testing.T) {
	s, history, _ := triggerServer(t)
	seedPlan(t, s, withName("db-tier", "104"))

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"db-tier"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	q := history.queued[0]
	if q.run.SourceWorkloadID != "104" {
		t.Errorf("workload = %q, want 104: it comes from the plan", q.run.SourceWorkloadID)
	}
	// The plan names proxmox-main; the configured default is pve. A drill
	// that ignored the plan's provider would restore onto the wrong cluster.
	if q.run.ProviderID != "proxmox-main" {
		t.Errorf("provider = %q, want proxmox-main: the plan names it", q.run.ProviderID)
	}
}

// A plan resolved by id rather than by name is the same plan: the catalogue
// takes either, and a dashboard that only ever saw an id must not have to
// look up a name to launch a drill.
func TestTriggerByPlanAcceptsAnID(t *testing.T) {
	s, history, _ := triggerServer(t)
	seeded := seedPlan(t, s, validPlanYAML)

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"`+seeded.ID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if history.queued[0].run.PlanID != seeded.ID {
		t.Errorf("plan_id = %q, want %q", history.queued[0].run.PlanID, seeded.ID)
	}
}

// Merging a plan with an override would make "what does this plan do" a
// question with two answers. The refusal has to name the fields, or whoever
// gets it has to guess which half of their body was the problem.
func TestTriggerRefusesAPlanMixedWithAdhocFields(t *testing.T) {
	s, history, _ := triggerServer(t)
	seedPlan(t, s, validPlanYAML)

	for name, body := range map[string][]string{
		"workload_id":        {`{"plan":"web-tier","workload_id":"110"}`, "workload_id"},
		"an override":        {`{"plan":"web-tier","network":"vmbr0"}`, "network"},
		"a boolean override": {`{"plan":"web-tier","skip_startup":true}`, "skip_startup"},
		"a list override":    {`{"plan":"web-tier","checks":["ping"]}`, "checks"},
	} {
		rec := post(s, operateSecret, "/api/v1/recovery-runs", body[0])
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body)
			continue
		}
		var p Problem
		decodePlan(t, rec, &p)
		if !strings.Contains(p.Detail, body[1]) || !strings.Contains(p.Detail, "plan") {
			t.Errorf("%s: detail = %q, want it to name both %q and plan", name, p.Detail, body[1])
		}
	}

	if len(history.queued) != 0 {
		t.Fatalf("the queue holds %d runs, want 0: a mixed request was queued", len(history.queued))
	}
}

func TestTriggerByAnUnknownPlanIs404(t *testing.T) {
	s, history, _ := triggerServer(t)

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"nothing-here"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	// It must talk about plans: "No such recovery run" would send whoever
	// read it looking at the wrong table entirely.
	var p Problem
	decodePlan(t, rec, &p)
	if p.Title != "No such plan" {
		t.Errorf("title = %q, want %q", p.Title, "No such plan")
	}
	if len(history.queued) != 0 {
		t.Errorf("the queue holds %d runs, want 0", len(history.queued))
	}
}

// A plan valid when it was written can stop parsing between two releases.
// The caller must hear that as a problem with the plan, not as a drill that
// fails halfway through for no stated reason - so it is a 400, before
// anything is queued, and the detail names the field.
func TestTriggerByAStoredPlanThatNoLongerParsesIsA400(t *testing.T) {
	s, history, plans := triggerServer(t)
	seeded := seedPlan(t, s, validPlanYAML)

	// Written straight into the catalogue: the API validates on the way in,
	// so the only way to hold a document this release refuses is to have
	// stored it under a release that accepted it.
	row := plans.stored[seeded.ID]
	row.YAML = validPlanYAML + "typo_here: 3\n"
	plans.stored[seeded.ID] = row

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"web-tier"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var p Problem
	decodePlan(t, rec, &p)
	if !strings.Contains(p.Detail, "typo_here") {
		t.Errorf("detail = %q, want it to name the offending field", p.Detail)
	}
	if len(history.queued) != 0 {
		t.Errorf("the queue holds %d runs, want 0", len(history.queued))
	}
}

// The lock is on the workload, not on how the drill was asked for: naming a
// plan must not be a way around it.
func TestAWorkloadAlreadyInADrillIs409EvenThroughAPlan(t *testing.T) {
	s, history, _ := triggerServer(t)
	seedPlan(t, s, validPlanYAML)
	history.add(core.RecoveryRun{
		ID: "run-in-flight", PlanName: "adhoc-110", SourceWorkloadID: "110",
		State: core.RunQueued, StartedAt: time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
	})

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"web-tier"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "run-in-flight") {
		t.Errorf("the 409 does not name the run already in flight: %s", rec.Body)
	}
	if len(history.queued) != 0 {
		t.Errorf("a second drill was queued for the same workload")
	}
}

// Naming a plan to execute it is still operating, not managing: the token
// that may launch drills may launch this one, and the token that may rewrite
// the catalogue may not launch anything.
func TestTriggeringByPlanNeedsOperateNotManage(t *testing.T) {
	s, history, _ := triggerServer(t)
	seedPlan(t, s, validPlanYAML)

	if rec := post(s, manageSecret, "/api/v1/recovery-runs", `{"plan":"web-tier"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("with a manage token = %d, want 403: %s", rec.Code, rec.Body)
	}
	if rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"web-tier"}`); rec.Code != http.StatusCreated {
		t.Fatalf("with an operate token = %d, want 201: %s", rec.Code, rec.Body)
	}
	if len(history.queued) != 1 {
		t.Errorf("the queue holds %d runs, want 1", len(history.queued))
	}
}

// A deployment with no catalogue answers 503 rather than 404: telling a
// caller its plan does not exist would send it to create one, and the
// creation would fail for the same reason.
func TestTriggerByPlanWithoutACatalogueIs503(t *testing.T) {
	s := operatingServer(t, newFakeHistory(), fakeProviders{hv: testFleet(t)})

	rec := post(s, operateSecret, "/api/v1/recovery-runs", `{"plan":"web-tier"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
}

// adhocFields is a hand-written list beside a struct, and the two can drift:
// a field added to triggerRequest and forgotten there would be accepted
// alongside a plan and then silently ignored, which is the worst of the three
// possible outcomes - worse than a 400, worse than honouring it.
//
// The struct is the source of truth, so the test reads it rather than a
// second list: adding a field to triggerRequest without teaching adhocFields
// about it fails here, on the day it is added.
func TestEveryAdhocFieldIsReportedAsAConflict(t *testing.T) {
	typ := reflect.TypeOf(triggerRequest{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" || name == "plan" {
			continue
		}

		var req triggerRequest
		value := reflect.ValueOf(&req).Elem().Field(i)
		switch value.Kind() {
		case reflect.String:
			value.SetString("something")
		case reflect.Bool:
			value.SetBool(true)
		case reflect.Slice:
			value.Set(reflect.ValueOf([]string{"ping"}))
		default:
			t.Fatalf("%s is a %s: teach this test how to populate one", name, value.Kind())
		}

		if got := req.adhocFields(); !slices.Contains(got, name) {
			t.Errorf("a request setting only %q reports %v: adhocFields does not know that field, "+
				"so it would be accepted next to a plan and then ignored", name, got)
		}
	}

	// And the converse: an empty request conflicts with nothing, or naming a
	// plan alone would be refused.
	if got := (triggerRequest{Plan: "web-tier"}).adhocFields(); len(got) != 0 {
		t.Errorf("a request naming only a plan reports %v, want nothing", got)
	}
}
