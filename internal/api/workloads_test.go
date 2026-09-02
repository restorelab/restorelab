package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// fleet is a read-only hypervisor. Every destructive method fails the test:
// B1 has no write path, and this is how that is proven rather than asserted.
type fleet struct {
	t *testing.T

	workloads []core.Workload
	status    map[string]*core.WorkloadStatus
	err       error
}

func (f *fleet) ID() string                 { return "pve" }
func (f *fleet) Kind() string               { return "proxmox" }
func (f *fleet) Ping(context.Context) error { return f.err }

func (f *fleet) ListNodes(context.Context) ([]core.Node, error) {
	return []core.Node{{ID: "pve1", Online: true}}, f.err
}

func (f *fleet) ListWorkloads(context.Context) ([]core.Workload, error) {
	return f.workloads, f.err
}

func (f *fleet) GetWorkload(_ context.Context, id string) (*core.Workload, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.workloads {
		if f.workloads[i].ID == id {
			return &f.workloads[i], nil
		}
	}
	return nil, core.ErrNotFound
}

func (f *fleet) GetStatus(_ context.Context, id string) (*core.WorkloadStatus, error) {
	if f.err != nil {
		return nil, f.err
	}
	if st, ok := f.status[id]; ok {
		return st, nil
	}
	return nil, core.ErrNotFound
}

func (f *fleet) AllocateWorkloadID(context.Context) (string, error) {
	f.t.Fatal("the read-only API allocated a workload id")
	return "", nil
}

func (f *fleet) Restore(context.Context, core.Backup, core.RestoreOptions) (*core.RestoreJob, error) {
	f.t.Fatal("the read-only API called Restore")
	return nil, nil
}

func (f *fleet) WaitForJob(context.Context, *core.RestoreJob) (*core.TaskState, error) {
	f.t.Fatal("the read-only API waited on a restore job")
	return nil, nil
}

func (f *fleet) Start(context.Context, string) error {
	f.t.Fatal("the read-only API started a workload")
	return nil
}

func (f *fleet) Stop(context.Context, string) error {
	f.t.Fatal("the read-only API stopped a workload")
	return nil
}

func (f *fleet) Delete(context.Context, string) error {
	f.t.Fatal("the read-only API deleted a workload")
	return nil
}

// backupSource answers backup lookups.
type backupSource struct {
	backups []core.Backup
	err     error
}

func (b backupSource) ID() string                 { return "pve" }
func (b backupSource) Kind() string               { return "proxmox" }
func (b backupSource) Ping(context.Context) error { return b.err }

func (b backupSource) ListBackups(context.Context, string) ([]core.Backup, error) {
	return b.backups, b.err
}

func (b backupSource) GetLatestBackup(context.Context, string) (*core.Backup, error) {
	if b.err != nil {
		return nil, b.err
	}
	if len(b.backups) == 0 {
		return nil, core.ErrNoBackup
	}
	return &b.backups[0], nil
}

func testFleet(t *testing.T) *fleet {
	return &fleet{
		t: t,
		workloads: []core.Workload{
			{ID: "110", Name: "linux-test", Kind: core.WorkloadKindVM, Node: "pve1",
				PowerState: core.PowerStateRunning, CPUCores: 2, MemoryBytes: 2 << 30},
			{ID: "9001", Name: "restorelab-110", Node: "pve1", Managed: true},
			{ID: "9000", Name: "windows-template", Node: "pve1", Template: true},
		},
		status: map[string]*core.WorkloadStatus{
			"110": {ID: "110", PowerState: core.PowerStateRunning, AgentReady: true, IPs: []string{"10.10.10.5"}},
		},
	}
}

func TestListingWorkloadsHidesTemplatesAndDrillLeftovers(t *testing.T) {
	s, _ := newTestServer(t, Options{Providers: fakeProviders{hv: testFleet(t)}})

	rec := do(s, http.MethodGet, "/api/v1/workloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var p page[workloadDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 || p.Items[0].ID != "110" {
		t.Fatalf("items = %+v, want only workload 110", p.Items)
	}

	// The temporary workloads a drill creates are an artefact, not something
	// anyone would drill - but they must be reachable when asked for.
	rec = do(s, http.MethodGet, "/api/v1/workloads?temporary=true")
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 2 {
		t.Fatalf("with temporary=true, items = %+v, want 110 and 9001", p.Items)
	}
}

func TestGettingOneWorkloadCarriesItsLiveStatus(t *testing.T) {
	s, _ := newTestServer(t, Options{Providers: fakeProviders{hv: testFleet(t)}})

	rec := do(s, http.MethodGet, "/api/v1/workloads/110")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var w workloadDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
		t.Fatalf("body is not a workload: %v", err)
	}
	if w.Status == nil || !w.Status.AgentReady {
		t.Fatalf("status = %+v, want the agent reported ready", w.Status)
	}
}

func TestAnUnknownWorkloadIsA404(t *testing.T) {
	s, _ := newTestServer(t, Options{Providers: fakeProviders{hv: testFleet(t)}})

	if rec := do(s, http.MethodGet, "/api/v1/workloads/404"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTheClusterRefusingUsIsA502(t *testing.T) {
	broken := testFleet(t)
	broken.err = core.ErrUnauthorized
	s, _ := newTestServer(t, Options{Providers: fakeProviders{hv: broken}})

	rec := do(s, http.MethodGet, "/api/v1/workloads")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: our provider token being refused is not the caller's fault", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Fatal("no problem document")
	}
}

func TestBackupsOfAWorkload(t *testing.T) {
	backups := []core.Backup{
		{ID: "local:backup/vzdump-qemu-110-2026_09_01.vma.zst", WorkloadID: "110",
			CreatedAt: time.Now().Add(-3 * time.Hour), SizeBytes: 337 << 20, Verified: core.VerificationNone},
	}
	s, _ := newTestServer(t, Options{Providers: fakeProviders{hv: testFleet(t), bp: backupSource{backups: backups}}})

	rec := do(s, http.MethodGet, "/api/v1/workloads/110/backups")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var p page[any]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("got %d backups, want 1", len(p.Items))
	}
}

func TestConfidenceOfANeverTestedWorkloadIsNullNotZero(t *testing.T) {
	s, _ := newTestServer(t, Options{
		Providers: fakeProviders{hv: testFleet(t), bp: backupSource{}},
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads/110/confidence")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var c confidenceDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("body is not a confidence: %v", err)
	}
	if c.Tested {
		t.Fatal("tested = true for a workload with no runs")
	}
	if c.Score != nil {
		t.Fatalf("score = %v, want null: a never-tested workload is not a zero, it is a dash", *c.Score)
	}
}

func TestConfidenceIsComputedFromTheStoredHistory(t *testing.T) {
	h := newFakeHistory()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	h.runs = []store.RunSummary{
		{ID: "r1", SourceWorkloadID: "110", State: core.RunSuccess, Result: core.ResultSuccess,
			StartedAt: now.Add(-2 * time.Hour), CompletedAt: now.Add(-2 * time.Hour).Add(time.Minute),
			RTO: 28 * time.Second, RTOTarget: 5 * time.Minute, CleanupDone: true},
	}
	backups := []core.Backup{{ID: "b1", WorkloadID: "110", CreatedAt: now.Add(-3 * time.Hour)}}

	s, _ := newTestServer(t, Options{
		History:   h,
		Providers: fakeProviders{hv: testFleet(t), bp: backupSource{backups: backups}},
		Now:       func() time.Time { return now },
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads/110/confidence")

	var c confidenceDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("body is not a confidence: %v", err)
	}
	if !c.Tested {
		t.Fatal("tested = false with a run on record")
	}
	if c.Score == nil || *c.Score != 100 {
		t.Fatalf("score = %v, want 100: a fresh successful drill within its RTO", c.Score)
	}
	if c.RunsConsidered != 1 {
		t.Errorf("runs_considered = %d, want 1", c.RunsConsidered)
	}
	if c.LastRunID != "r1" {
		t.Errorf("last_run_id = %q, want r1", c.LastRunID)
	}
}

func TestConfidenceSurvivesAWorkloadWithNoBackup(t *testing.T) {
	h := newFakeHistory()
	h.runs = []store.RunSummary{{ID: "r1", SourceWorkloadID: "110",
		State: core.RunSuccess, Result: core.ResultSuccess, StartedAt: time.Now().Add(-time.Hour)}}
	s, _ := newTestServer(t, Options{
		History:   h,
		Providers: fakeProviders{hv: testFleet(t), bp: backupSource{err: core.ErrNoBackup}},
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads/110/confidence")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: no backup is a penalty, not an error", rec.Code)
	}
	var c confidenceDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("body is not a confidence: %v", err)
	}
	if c.Score == nil || *c.Score == 100 {
		t.Fatalf("score = %v, want a penalty for having nothing to restore", c.Score)
	}
}

func TestNoProviderConfiguredIsA503(t *testing.T) {
	s, _ := newTestServer(t, Options{Providers: fakeProviders{err: errors.New("no hypervisor provider configured")}})

	rec := do(s, http.MethodGet, "/api/v1/workloads")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestWorkloadListingCarriesTheLastDrill(t *testing.T) {
	history := newFakeHistory()
	history.lastRuns = map[string]store.RunSummary{
		"110": {
			ID:               "run-110",
			SourceWorkloadID: "110",
			State:            core.RunSuccess,
			Result:           core.ResultSuccess,
			StartedAt:        time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		},
	}
	s, _ := newTestServer(t, Options{
		History:   history,
		Providers: fakeProviders{hv: testFleet(t)},
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var p page[workloadDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("items = %+v, want one", p.Items)
	}
	got := p.Items[0]
	if got.LastRunID != "run-110" {
		t.Errorf("last_run_id = %q, want run-110", got.LastRunID)
	}
	if got.LastRunState != string(core.RunSuccess) || got.LastRunResult != string(core.ResultSuccess) {
		t.Errorf("last_run state/result = %q/%q, want SUCCESS/SUCCESS", got.LastRunState, got.LastRunResult)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("last_run_at = %v, want the run's start", got.LastRunAt)
	}
	// It asked once, for the whole page, and not once per row.
	if history.lastRunsCalls != 1 {
		t.Errorf("LastRuns was called %d times, want exactly 1", history.lastRunsCalls)
	}
}

// A workload nobody has ever drilled must arrive with no last-drill keys at
// all: "never tested" is an answer the UI renders differently from a score of
// zero, and an empty string in the JSON would read as a drill that happened.
func TestWorkloadNeverDrilledCarriesNoLastRunKeys(t *testing.T) {
	s, _ := newTestServer(t, Options{
		History:   newFakeHistory(),
		Providers: fakeProviders{hv: testFleet(t)},
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads")
	if body := rec.Body.String(); strings.Contains(body, "last_run") {
		t.Errorf("body carries a last_run key for a workload never drilled: %s", body)
	}
}

// The inventory is the answer; its drill history is a bonus. A database that
// cannot be read must not turn a working listing into a 500.
func TestWorkloadListingSurvivesAnUnreadableHistory(t *testing.T) {
	history := newFakeHistory()
	history.lastRunsErr = errors.New("database is locked")
	s, _ := newTestServer(t, Options{
		History:   history,
		Providers: fakeProviders{hv: testFleet(t)},
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// The detail screen needs the same fields the listing has, and it must get
// them from the same call rather than from a second code path.
func TestWorkloadDetailCarriesTheLastDrill(t *testing.T) {
	history := newFakeHistory()
	history.lastRuns = map[string]store.RunSummary{
		"110": {ID: "run-110", SourceWorkloadID: "110", State: core.RunFailed,
			StartedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
	}
	s, _ := newTestServer(t, Options{
		History:   history,
		Providers: fakeProviders{hv: testFleet(t)},
	})

	rec := do(s, http.MethodGet, "/api/v1/workloads/110")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var dto workloadDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("body is not a workload: %v", err)
	}
	if dto.LastRunID != "run-110" || dto.LastRunState != string(core.RunFailed) {
		t.Errorf("last run = %q/%q, want run-110/FAILED", dto.LastRunID, dto.LastRunState)
	}
	// A run that failed has no result graded onto it here; the key must be
	// absent rather than empty, which is what omitempty gives.
	if dto.LastRunResult != "" {
		t.Errorf("last_run_result = %q, want empty", dto.LastRunResult)
	}
}
