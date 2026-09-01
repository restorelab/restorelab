package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
