package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// --- test doubles -----------------------------------------------------------

// enqueued is one row Enqueue was handed: the run, the plan snapshot, and
// when. Keeping the plan lets a test assert what a handler built without a
// database anywhere near it.
type enqueued struct {
	run  core.RecoveryRun
	plan string
	at   time.Time
}

// fakeLease is what a worker holds over a run.
type fakeLease struct {
	owner   string
	expires time.Time
}

// fakeHistory is the API's view of the store, in memory.
type fakeHistory struct {
	runs   []store.RunSummary
	byID   map[string]*core.RecoveryRun
	events map[string][]store.Event
	err    error

	// The queue half. queued records every Enqueue in order; cancelAsked
	// records the runs a cancellation was asked of but not settled - the
	// worker's half of the job, which no handler does.
	queued      []enqueued
	cancelAsked []string
	leases      map[string]fakeLease

	// The last-drill half. lastRunsCalls counts the calls, so a test can
	// prove the listing asks once for the page and not once per row.
	lastRuns      map[string]store.RunSummary
	lastRunsErr   error
	lastRunsCalls int
}

func newFakeHistory() *fakeHistory {
	return &fakeHistory{
		byID:   map[string]*core.RecoveryRun{},
		events: map[string][]store.Event{},
		leases: map[string]fakeLease{},
	}
}

// add records a run in both views the fake keeps, newest first, exactly as
// the real listing orders them.
func (f *fakeHistory) add(run core.RecoveryRun) {
	stored := run
	f.byID[run.ID] = &stored
	f.runs = append([]store.RunSummary{{
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
	}}, f.runs...)
}

// setState keeps the two views in step.
func (f *fakeHistory) setState(runID string, state core.RunState, at time.Time) {
	if run, ok := f.byID[runID]; ok {
		run.State = state
		run.CompletedAt = at
	}
	for i := range f.runs {
		if f.runs[i].ID == runID {
			f.runs[i].State = state
			f.runs[i].CompletedAt = at
		}
	}
}

func (f *fakeHistory) LastRuns(_ context.Context, ids []string) (map[string]store.RunSummary, error) {
	f.lastRunsCalls++
	if f.lastRunsErr != nil {
		return nil, f.lastRunsErr
	}
	out := map[string]store.RunSummary{}
	for _, id := range ids {
		if run, ok := f.lastRuns[id]; ok {
			out[id] = run
		}
	}
	return out, nil
}

func (f *fakeHistory) ListRuns(_ context.Context, filter store.Filter) ([]store.RunSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = store.DefaultListLimit
	}
	var out []store.RunSummary
	for _, r := range f.runs {
		if filter.WorkloadID != "" && r.SourceWorkloadID != filter.WorkloadID {
			continue
		}
		if filter.State != "" && r.State != filter.State {
			continue
		}
		if filter.Result != "" && r.Result != filter.Result {
			continue
		}
		if filter.NotTerminal && r.State.Terminal() {
			continue
		}
		if filter.After != nil {
			after := filter.After
			// Runs come back newest first, so "after the cursor" means
			// strictly older, with the id breaking ties.
			isAfterCursor := r.StartedAt.Before(after.StartedAt) ||
				(r.StartedAt.Equal(after.StartedAt) && r.ID < after.ID)
			if !isAfterCursor {
				continue
			}
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeHistory) GetRun(_ context.Context, idOrPrefix string) (*core.RecoveryRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	if run, ok := f.byID[idOrPrefix]; ok {
		return run, nil
	}
	var found *core.RecoveryRun
	for id, run := range f.byID {
		if len(idOrPrefix) < len(id) && id[:len(idOrPrefix)] == idOrPrefix {
			if found != nil {
				return nil, store.ErrAmbiguous
			}
			found = run
		}
	}
	if found == nil {
		return nil, store.ErrNotFound
	}
	return found, nil
}

func (f *fakeHistory) Events(_ context.Context, runID string, afterSeq int64) ([]store.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []store.Event
	for _, e := range f.events[runID] {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// Enqueue records a run to be executed later. It never runs anything: that
// is the whole point of the interface the API declares.
func (f *fakeHistory) Enqueue(_ context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error {
	if f.err != nil {
		return f.err
	}
	queuedRun := *run
	queuedRun.StartedAt = at
	f.queued = append(f.queued, enqueued{run: queuedRun, plan: planYAML, at: at})
	f.add(queuedRun)
	return nil
}

// RequestCancel mirrors the store: a queued run settles here and now, a run
// already in flight can only be asked.
func (f *fakeHistory) RequestCancel(_ context.Context, runID string, at time.Time) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	run, ok := f.byID[runID]
	if !ok {
		return false, store.ErrNotFound
	}
	if run.State.Terminal() {
		return false, fmt.Errorf("%w: run %s is %s", store.ErrAlreadySettled, runID, run.State)
	}
	if run.State == core.RunQueued {
		f.setState(runID, core.RunCancelled, at)
		return true, nil
	}
	f.cancelAsked = append(f.cancelAsked, runID)
	return false, nil
}

func (f *fakeHistory) ActiveRunForWorkload(_ context.Context, workloadID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	for _, r := range f.runs {
		if r.SourceWorkloadID == workloadID && !r.State.Terminal() {
			return r.ID, nil
		}
	}
	return "", nil
}

func (f *fakeHistory) RunLease(_ context.Context, runID string) (string, time.Time, error) {
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	if _, ok := f.byID[runID]; !ok {
		return "", time.Time{}, store.ErrNotFound
	}
	l := f.leases[runID]
	return l.owner, l.expires, nil
}

// fakeTokens accepts exactly the tokens it was given.
type fakeTokens struct {
	byHash  map[string]store.APIToken
	err     error
	touched int
}

func (f *fakeTokens) TokenByHash(_ context.Context, hash string) (*store.APIToken, error) {
	if f.err != nil {
		return nil, f.err
	}
	tok, ok := f.byHash[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &tok, nil
}

func (f *fakeTokens) TouchToken(context.Context, string, time.Time) error {
	f.touched++
	return nil
}

// fakeProviders hands out one hypervisor and one backup provider.
type fakeProviders struct {
	hv      core.HypervisorProvider
	bp      core.BackupProvider
	entries []config.Provider
	err     error
}

func (f fakeProviders) Entries() []config.Provider { return f.entries }

func (f fakeProviders) Hypervisor(string) (core.HypervisorProvider, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hv, nil
}

func (f fakeProviders) Backups(string) (core.BackupProvider, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bp, nil
}

// --- helpers ----------------------------------------------------------------

const testSecret = "rl_TESTTESTTESTTESTTESTTESTTESTTESTTESTTESTTES"

// newTestServer wires a server with one valid token.
func newTestServer(t *testing.T, opts Options) (*Server, *fakeTokens) {
	t.Helper()
	tokens := &fakeTokens{byHash: map[string]store.APIToken{
		HashToken(testSecret): {ID: "tok-1", Name: "test", Hash: HashToken(testSecret),
			CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}}
	if opts.History == nil {
		opts.History = newFakeHistory()
	}
	if opts.Tokens == nil {
		opts.Tokens = tokens
	}
	if opts.Providers == nil {
		opts.Providers = fakeProviders{}
	}
	return New(opts), tokens
}

// do sends an authenticated request and returns the recorder.
func do(s *Server, method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer "+testSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// --- tests ------------------------------------------------------------------

func TestHealthNeedsNoToken(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`status = %v, want "ok"`, body["status"])
	}
	// /health is unauthenticated: it must not describe the deployment.
	if _, ok := body["history"]; ok {
		t.Error("/health described the database to an unauthenticated caller")
	}
}

func TestEverythingElseNeedsAToken(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	for _, path := range []string{
		"/api/v1/recovery-runs",
		"/api/v1/recovery-runs/abcd",
		"/api/v1/workloads",
		"/api/v1/providers",
		"/api/v1/doctor",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != problemContentType {
			t.Errorf("GET %s Content-Type = %q, want %q", path, got, problemContentType)
		}
	}
}

func TestAnUnknownPathIsAProblemDocument(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := do(s, http.MethodGet, "/api/v1/nope")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q: even a 404 speaks problem+json", got, problemContentType)
	}
}

func TestAWrongMethodIsRefused(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	// B1 has no write path at all. POST must not quietly fall through to the
	// GET handler.
	rec := do(s, http.MethodPost, "/api/v1/recovery-runs")

	if rec.Code == http.StatusOK {
		t.Fatal("POST /recovery-runs was accepted: B1 has no write path")
	}
}
