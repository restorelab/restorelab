package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/api"
	"github.com/restorelab/restorelab/internal/config"
	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// The secrets this test proves never reach a response body.
const (
	leakDBPassword      = "hunter2correcthorse"
	leakProviderSecret  = "rlsec:v1:averysecretsealedvalue"
	leakProviderTokenID = "restorelab@pve!drills-rw"
)

// apiFixture is a real SQLite history, a real server and a real HTTP client.
type apiFixture struct {
	server *httptest.Server
	secret string
	runID  string
	store  store.Store
}

// readOnlyProviders is a provider set whose destructive methods fail the test.
type readOnlyProviders struct {
	t *testing.T
}

func (p readOnlyProviders) Entries() []config.Provider {
	return []config.Provider{{
		ID: "pve", Kind: "proxmox", Roles: []string{"hypervisor", "backup"},
		Endpoint: "https://cluster.invalid:8006",
		TokenID:  leakProviderTokenID, TokenSecret: leakProviderSecret,
	}}
}

func (p readOnlyProviders) Hypervisor(string) (core.HypervisorProvider, error) {
	return &e2eFleet{t: p.t}, nil
}

func (p readOnlyProviders) Backups(string) (core.BackupProvider, error) {
	return e2eBackups{}, nil
}

type e2eFleet struct{ t *testing.T }

func (f *e2eFleet) ID() string                 { return "pve" }
func (f *e2eFleet) Kind() string               { return "proxmox" }
func (f *e2eFleet) Ping(context.Context) error { return nil }

func (f *e2eFleet) ListNodes(context.Context) ([]core.Node, error) {
	return []core.Node{{ID: "pve1", Online: true}}, nil
}

func (f *e2eFleet) ListWorkloads(context.Context) ([]core.Workload, error) {
	return []core.Workload{{ID: "110", Name: "linux-test", Kind: core.WorkloadKindVM,
		Node: "pve1", PowerState: core.PowerStateRunning}}, nil
}

func (f *e2eFleet) GetWorkload(_ context.Context, id string) (*core.Workload, error) {
	if id != "110" {
		return nil, core.ErrNotFound
	}
	return &core.Workload{ID: "110", Name: "linux-test", Node: "pve1"}, nil
}

func (f *e2eFleet) GetStatus(context.Context, string) (*core.WorkloadStatus, error) {
	return &core.WorkloadStatus{ID: "110", PowerState: core.PowerStateRunning, AgentReady: true}, nil
}

func (f *e2eFleet) AllocateWorkloadID(context.Context) (string, error) {
	f.t.Fatal("the read-only API allocated a workload id")
	return "", nil
}

func (f *e2eFleet) Restore(context.Context, core.Backup, core.RestoreOptions) (*core.RestoreJob, error) {
	f.t.Fatal("the read-only API called Restore")
	return nil, nil
}

func (f *e2eFleet) WaitForJob(context.Context, *core.RestoreJob) (*core.TaskState, error) {
	f.t.Fatal("the read-only API waited on a job")
	return nil, nil
}

func (f *e2eFleet) Start(context.Context, string) error {
	f.t.Fatal("the read-only API started a workload")
	return nil
}

func (f *e2eFleet) Stop(context.Context, string) error {
	f.t.Fatal("the read-only API stopped a workload")
	return nil
}

func (f *e2eFleet) Delete(context.Context, string) error {
	f.t.Fatal("the read-only API deleted a workload")
	return nil
}

type e2eBackups struct{}

func (e2eBackups) ID() string                 { return "pve" }
func (e2eBackups) Kind() string               { return "proxmox" }
func (e2eBackups) Ping(context.Context) error { return nil }

func (e2eBackups) ListBackups(context.Context, string) ([]core.Backup, error) {
	return []core.Backup{{ID: "local:backup/vzdump-qemu-110.vma.zst", WorkloadID: "110",
		CreatedAt: time.Now().Add(-2 * time.Hour), SizeBytes: 337 << 20}}, nil
}

func (e2eBackups) GetLatestBackup(context.Context, string) (*core.Backup, error) {
	backups, _ := e2eBackups{}.ListBackups(context.Background(), "110")
	return &backups[0], nil
}

// newAPIFixture records a drill in a real database and serves it.
func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	ctx := context.Background()

	history, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { history.Close() })

	run := &core.RecoveryRun{
		ID:               "0aca8405-4e80-4ac9-8bdd-057a56dc0281",
		PlanName:         "adhoc-110",
		ProviderID:       "pve",
		SourceWorkloadID: "110",
		SourceName:       "linux-test",
		TempWorkloadID:   "9001",
		Node:             "pve1",
		State:            core.RunSuccess,
		Result:           core.ResultSuccess,
		StartedAt:        time.Now().Add(-time.Hour),
		CompletedAt:      time.Now().Add(-time.Hour).Add(28 * time.Second),
		RTO:              28 * time.Second,
		RTOTarget:        5 * time.Minute,
		CleanupDone:      true,
	}
	if err := history.CreateRun(ctx, run, "name: adhoc-110\n"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := history.SaveStep(ctx, run.ID, 0, core.Step{
		Name: "restore", State: core.RunRestoring, Status: core.StepDone,
		Duration: 4 * time.Second,
	}); err != nil {
		t.Fatalf("SaveStep: %v", err)
	}
	if err := history.SaveCheck(ctx, run.ID, 0, core.CheckResult{
		Name: "ssh is up", Type: "command", Status: core.CheckPass, Attempts: 1,
	}); err != nil {
		t.Fatalf("SaveCheck: %v", err)
	}
	if err := history.AppendEvent(ctx, run.ID, store.Event{
		Seq: 1, At: run.StartedAt, State: core.RunRestoring, Step: "restore",
		Status: core.StepRunning, Message: "restoring",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	secret, record, err := api.NewToken("e2e", time.Now())
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := history.CreateToken(ctx, record); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	cfg := config.New()
	cfg.Database.URL = "postgres://restorelab:" + leakDBPassword + "@db.internal:5432/history"
	cfg.Defaults.Provider = "pve"

	srv := httptest.NewServer(api.New(api.Options{
		History:   history,
		Tokens:    history,
		Providers: readOnlyProviders{t: t},
		Config:    cfg,
	}))
	t.Cleanup(srv.Close)

	return &apiFixture{server: srv, secret: secret, runID: run.ID, store: history}
}

// get performs an authenticated GET and returns the status and body.
func (f *apiFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.secret)

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestARecordedDrillIsReadableOverHTTP(t *testing.T) {
	f := newAPIFixture(t)

	status, body := f.get(t, "/api/v1/recovery-runs")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if !strings.Contains(body, "linux-test") {
		t.Fatalf("the listing does not mention the recorded drill: %s", body)
	}

	// The short id works, exactly as `runs show` accepts one.
	status, body = f.get(t, "/api/v1/recovery-runs/"+f.runID[:8])
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if doc["run_id"] != f.runID {
		t.Errorf("run_id = %v, want %q", doc["run_id"], f.runID)
	}

	status, body = f.get(t, "/api/v1/recovery-runs/"+f.runID+"/events")
	if status != http.StatusOK || !strings.Contains(body, "restoring") {
		t.Fatalf("events = %d %s", status, body)
	}
}

func TestTheHTMLReportOpensInABrowser(t *testing.T) {
	f := newAPIFixture(t)

	status, body := f.get(t, "/api/v1/recovery-runs/"+f.runID+"/report?format=html")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	for _, want := range []string{"<html", "linux-test", "</html>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the HTML report has no %q", want)
		}
	}
}

func TestConfidenceIsAnsweredFromTheRealHistory(t *testing.T) {
	f := newAPIFixture(t)

	status, body := f.get(t, "/api/v1/workloads/110/confidence")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	var c struct {
		Score          *int     `json:"score"`
		Tested         bool     `json:"tested"`
		Reasons        []string `json:"reasons"`
		RunsConsidered int      `json:"runs_considered"`
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !c.Tested || c.Score == nil {
		t.Fatalf("confidence = %s, want a score from the recorded drill", body)
	}
	if c.RunsConsidered != 1 {
		t.Errorf("runs_considered = %d, want 1", c.RunsConsidered)
	}
}

func TestHealthIsTheOnlyUnauthenticatedEndpoint(t *testing.T) {
	f := newAPIFixture(t)
	client := f.server.Client()

	resp, err := client.Get(f.server.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health without a token = %d, want 200", resp.StatusCode)
	}

	for _, path := range []string{
		"/api/v1/recovery-runs",
		"/api/v1/recovery-runs/" + f.runID,
		"/api/v1/recovery-runs/" + f.runID + "/report",
		"/api/v1/workloads",
		"/api/v1/workloads/110/backups",
		"/api/v1/workloads/110/confidence",
		"/api/v1/providers",
		"/api/v1/doctor",
	} {
		resp, err := client.Get(f.server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, resp.StatusCode)
		}
	}
}

// TestNoResponseEverCarriesASecret is the server-side counterpart of
// TestProviderDetailsNeverLeaksSecrets: it walks every endpoint, in the happy
// case and in the error case, and fails if a body carries anything sealed.
func TestNoResponseEverCarriesASecret(t *testing.T) {
	f := newAPIFixture(t)

	paths := []string{
		"/api/v1/health",
		"/api/v1/recovery-runs",
		"/api/v1/recovery-runs?limit=abc",
		"/api/v1/recovery-runs?cursor=!!!",
		"/api/v1/recovery-runs?since=last%20tuesday",
		"/api/v1/recovery-runs/" + f.runID,
		"/api/v1/recovery-runs/zzzzzzzz",
		"/api/v1/recovery-runs/" + f.runID + "/events",
		"/api/v1/recovery-runs/" + f.runID + "/events?after=x",
		"/api/v1/recovery-runs/" + f.runID + "/report?format=pdf",
		"/api/v1/workloads",
		"/api/v1/workloads/110",
		"/api/v1/workloads/404",
		"/api/v1/workloads/110/backups",
		"/api/v1/workloads/110/confidence",
		"/api/v1/workloads?provider=nope",
		"/api/v1/providers",
		"/api/v1/doctor",
		"/api/v1/nope",
	}

	forbidden := []string{
		f.secret,            // the caller's own API token
		leakDBPassword,      // the history database password
		leakProviderSecret,  // the sealed provider secret
		leakProviderTokenID, // the provider token id: half a credential
	}

	for _, path := range paths {
		_, body := f.get(t, path)
		for _, secret := range forbidden {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s leaked %q:\n%s", path, secret, body)
			}
		}
	}
}

func TestTheAPINeverWrites(t *testing.T) {
	f := newAPIFixture(t)
	client := f.server.Client()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, err := http.NewRequest(method, f.server.URL+"/api/v1/recovery-runs", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+f.secret)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s /recovery-runs was accepted: B1 has no write path", method)
		}
	}
}
