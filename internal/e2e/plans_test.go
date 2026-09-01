package e2e

// Phase B3, assembled onto the same rig as B2: a plan is written down and
// stored over HTTP, a drill is launched by naming it, the worker executes that
// drill against the simulated cluster, and the run keeps saying what it did
// after the plan it came from has been deleted.
//
// Everything here goes through the product's own path - POST /plans writes the
// catalogue, POST /recovery-runs resolves the name and queues a row, a worker
// claims it and restores into the isolated bridge. The one exception is the
// second database handle below, and it exists precisely because no endpoint
// renders what it reads.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/plan"
)

// storedPlanName is what the catalogue knows this drill as, and what the
// trigger names instead of describing a workload.
const storedPlanName = "postgres-nightly"

// --- reading what no endpoint renders ----------------------------------------

// rawHistory is a second handle on the database the fixture's store writes to.
//
// Two of a run's columns have no representation on the wire: plan_version,
// which is provenance nobody has yet needed to render, and plan_snapshot,
// which is read exactly once, by the worker that claims the run. Reading the
// snapshot through the product would mean claiming the run - taking it away
// from the worker whose execution is the point of this test - so the honest
// alternative is to read the row, and to say so.
type rawHistory struct {
	driver string
	dsn    string
}

// provenance returns a run's plan link, the version behind it, and the plan
// snapshot it executed.
//
// An unlinked run is not an error: plan_id is NULL once its plan is deleted,
// and that NULL is the thing this file most wants to observe.
func (h rawHistory) provenance(t *testing.T, runID string) (planID string, version int, snapshot string) {
	t.Helper()

	db, err := sql.Open(h.driver, h.dsn)
	if err != nil {
		t.Fatalf("open a second handle on the history: %v", err)
	}
	defer func() { _ = db.Close() }()

	// The one query in this package written twice, and for the dullest of
	// reasons: the store's rebind is unexported and a placeholder is not
	// worth exporting it for.
	query := `SELECT plan_id, plan_version, plan_snapshot FROM runs WHERE id = ?`
	if h.driver == "pgx" {
		query = strings.Replace(query, "?", "$1", 1)
	}

	var (
		id, snap sql.NullString
		v        sql.NullInt64
	)
	if err := db.QueryRowContext(context.Background(), query, runID).Scan(&id, &v, &snap); err != nil {
		t.Fatalf("read the provenance of run %s: %v", runID, err)
	}
	return id.String, int(v.Int64), snap.String
}

// --- the document ------------------------------------------------------------

// storedPlanDocument writes the plan an operator would keep in a git
// repository, for the workload the simulated cluster actually holds.
//
// The ids come from the fixture's own constants rather than being typed out
// again: a plan naming a workload the fake does not have would fail somewhere
// deep in the engine, which is a long way from the mistake. The command is
// pulled off queueCheck for the same reason - it has to be the one the fake
// guest agent was told to answer.
//
// extraRestore is how the second version differs from the first, so that the
// version recorded against a run cannot quietly be a hard-coded 1.
func storedPlanDocument(description, extraRestore string) string {
	return fmt.Sprintf(`# The nightly drill for the production database.
#
# The comment is deliberate: a stored plan keeps the document exactly as it
# was written, and this line is what proves it survived the round trip.
name: %s
description: %s
workload:
  provider: %s
  id: %q
backup:
  strategy: latest
  max_age: 26h
restore:
  node: %s
%sstartup:
  timeout: 30s
checks:
  - type: command
    name: PostgreSQL
    run: %s
    expect: active
rto_target: 10m
`, storedPlanName, description, queueProvider, sourceVMID, node, extraRestore,
		strings.TrimPrefix(queueCheck, "cmd:"))
}

// --- reading the API ---------------------------------------------------------

// planBody is the stored plan as the API renders it.
type planBody struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	WorkloadID  string `json:"workload_id"`
	ProviderID  string `json:"provider_id"`
	Version     int    `json:"version"`
	YAML        string `json:"yaml"`
}

// queuedBody is the run summary a trigger answers with.
type queuedBody struct {
	ID               string `json:"id"`
	PlanName         string `json:"plan_name"`
	PlanID           string `json:"plan_id"`
	SourceWorkloadID string `json:"source_workload_id"`
	State            string `json:"state"`
}

// decodeBody unmarshals a response, reporting the body when it cannot: a
// decode error against a problem document is otherwise unreadable.
func decodeBody[T any](t *testing.T, what, body string) T {
	t.Helper()

	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%s is not the JSON this test expects: %v\n%s", what, err, body)
	}
	return out
}

// runListing returns the listing entry for one run, as a bare map.
//
// A map rather than a struct because the assertion that matters is about the
// whole entry: deleting a plan must change one field and leave every other
// byte alone, and a struct would only compare the fields this test happened to
// think of.
func (f *queueFixture) runListing(t *testing.T, runID string) map[string]any {
	t.Helper()

	status, body := f.get(t, "/api/v1/recovery-runs?workload="+sourceVMID)
	if status != http.StatusOK {
		t.Fatalf("GET /recovery-runs = %d: %s", status, body)
	}

	listing := decodeBody[struct {
		Items []map[string]any `json:"items"`
	}](t, "the runs listing", body)

	for _, item := range listing.Items {
		if item["id"] == runID {
			return item
		}
	}
	t.Fatalf("run %s is not in the listing: %s", runID, body)
	return nil
}

// --- the test ----------------------------------------------------------------

// A plan is stored, edited, launched by name, executed, and then deleted -
// and the run it produced says exactly the same thing afterwards.
//
// The deletion at the end is the assertion the whole phase exists for. A drill
// executes the snapshot taken when it was queued, never the catalogue row, so
// a plan that is edited or removed months later cannot rewrite what a report
// says happened.
func TestAStoredPlanDrillsAndOutlivesItsOwnDeletion(t *testing.T) {
	f := newQueueFixture(t, nil)

	// --- 1. the plan is written into the catalogue over HTTP ------------------

	v1 := storedPlanDocument("restore last night's backup and prove the service comes back", "")

	status, body := f.do(t, http.MethodPost, "/api/v1/plans", f.manage, v1)
	if status != http.StatusCreated {
		t.Fatalf("POST /plans = %d: %s", status, body)
	}
	stored := decodeBody[planBody](t, "the created plan", body)

	if stored.ID == "" {
		t.Fatal("a stored plan came back without an id: nothing could ever point at it")
	}
	if stored.Version != 1 {
		t.Errorf("Version = %d, want 1 for a plan that has just been created", stored.Version)
	}
	if stored.WorkloadID != sourceVMID || stored.ProviderID != queueProvider {
		t.Errorf("derived columns = %q/%q, want %q/%q: they must follow the document",
			stored.WorkloadID, stored.ProviderID, sourceVMID, queueProvider)
	}
	if stored.YAML != v1 {
		t.Errorf("the document was rewritten on its way into the catalogue:\n%s", stored.YAML)
	}

	// A token that may launch a drill has no business deciding what a drill
	// is. The two scopes are separate, and this is the path that proves it.
	if status, body := f.do(t, http.MethodPost, "/api/v1/plans", f.operate, v1); status != http.StatusForbidden {
		t.Errorf("POST /plans with an operate token = %d: %s\nwant 403", status, body)
	}

	// --- 2. it is edited, so the version recorded below means something -------

	v2 := storedPlanDocument("restore last night's backup, on a smaller clone",
		"  cpu_limit: 2\n  memory_limit: 4096\n")

	status, body = f.do(t, http.MethodPut, "/api/v1/plans/"+storedPlanName, f.manage, v2)
	if status != http.StatusOK {
		t.Fatalf("PUT /plans/%s = %d: %s", storedPlanName, status, body)
	}
	edited := decodeBody[planBody](t, "the updated plan", body)
	if edited.Version != 2 {
		t.Fatalf("Version = %d after an edit, want 2", edited.Version)
	}
	if edited.ID != stored.ID {
		t.Errorf("the edit moved the plan from %s to %s: an update must keep its identity",
			stored.ID, edited.ID)
	}

	// --- 3. a drill is launched by naming it ---------------------------------

	status, body = f.do(t, http.MethodPost, "/api/v1/recovery-runs", f.operate,
		fmt.Sprintf(`{"plan":%q}`, storedPlanName))
	if status != http.StatusCreated {
		t.Fatalf("POST /recovery-runs {\"plan\":%q} = %d: %s", storedPlanName, status, body)
	}
	queued := decodeBody[queuedBody](t, "the queued run", body)

	if queued.ID == "" {
		t.Fatal("the API queued a drill without telling the caller its id")
	}
	if queued.State != string(core.RunQueued) {
		t.Errorf("state = %q, want QUEUED: no worker exists yet", queued.State)
	}
	if queued.PlanID != stored.ID {
		t.Errorf("plan_id = %q, want %q: the run must point at the plan it was launched from",
			queued.PlanID, stored.ID)
	}
	if queued.PlanName != storedPlanName {
		t.Errorf("plan_name = %q, want %q", queued.PlanName, storedPlanName)
	}
	// The workload was never in the request: it came off the plan, which is
	// the whole point of launching a drill by name.
	if queued.SourceWorkloadID != sourceVMID {
		t.Errorf("source_workload_id = %q, want %q from the plan's own workload",
			queued.SourceWorkloadID, sourceVMID)
	}

	// And the reverse of the scope check above: writing the catalogue is not
	// permission to launch what it describes.
	if status, body := f.do(t, http.MethodPost, "/api/v1/recovery-runs", f.manage,
		fmt.Sprintf(`{"plan":%q}`, storedPlanName)); status != http.StatusForbidden {
		t.Errorf("POST /recovery-runs with a manage token = %d: %s\nwant 403", status, body)
	}

	// --- 4. the worker executes it against the simulated cluster -------------

	f.startWorker(t, "worker-stored-plan", nil)
	run := waitSettled(t, f.history, queued.ID)

	if run.State != core.RunSuccess || run.Result != core.ResultSuccess {
		t.Fatalf("state/result = %s/%s (%s), want SUCCESS", run.State, run.Result, run.Err)
	}
	if len(run.Checks) != 1 || !run.Checks[0].OK() {
		t.Errorf("checks = %+v, want the plan's one in-guest check to have passed", run.Checks)
	}
	if run.Checks[0].Name != "PostgreSQL" {
		t.Errorf("check name = %q, want the name the document gave it", run.Checks[0].Name)
	}
	if !run.CleanupDone {
		t.Error("CleanupDone = false: the temporary workload was not torn down")
	}
	if hasVM(f.pve, tempVMID) {
		t.Error("the temporary workload is still on the cluster after a successful drill")
	}
	if !hasVM(f.pve, sourceVMID) {
		t.Fatal("the SOURCE workload was destroyed - this is the worst possible bug")
	}
	if n := countRestores(f.pve); n != 1 {
		t.Errorf("the cluster saw %d restores, want exactly 1", n)
	}
	assertNoDestructiveCallOnSource(t, f.pve)
	assertHardened(t, f.pve)
	assertRestoreParams(t, f.pve)

	// --- 5. the provenance the run carries -----------------------------------

	if run.PlanID != stored.ID {
		t.Errorf("run.PlanID = %q, want %q", run.PlanID, stored.ID)
	}
	if run.PlanVersion != 2 {
		t.Errorf("run.PlanVersion = %d, want 2: the version that was current when it was queued",
			run.PlanVersion)
	}

	rowPlanID, rowVersion, snapshot := f.raw.provenance(t, queued.ID)
	if rowPlanID != stored.ID || rowVersion != 2 {
		t.Errorf("the row carries %q/v%d, want %q/v2", rowPlanID, rowVersion, stored.ID)
	}

	// --- 6. the snapshot describes the plan that actually ran ----------------

	executed, err := plan.Parse([]byte(snapshot))
	if err != nil {
		t.Fatalf("the snapshot the worker executed does not parse: %v\n%s", err, snapshot)
	}

	if executed.Name != storedPlanName || executed.Workload.ID != sourceVMID {
		t.Errorf("the snapshot describes %q/%q, want %q/%q",
			executed.Name, executed.Workload.ID, storedPlanName, sourceVMID)
	}
	// The two limits only exist in v2. Finding them here is what says the
	// drill ran the edited plan rather than the one it replaced.
	if executed.Restore.CPULimit != 2 || executed.Restore.MemoryLimitMB != 4096 {
		t.Errorf("snapshot limits = %d/%d, want 2/4096 from version 2 of the plan",
			executed.Restore.CPULimit, executed.Restore.MemoryLimitMB)
	}
	if executed.Restore.Node != node {
		t.Errorf("snapshot node = %q, want %q", executed.Restore.Node, node)
	}
	// Defaults the document never mentioned: the snapshot is the defaulted
	// plan, so what the worker parses is executable rather than merely
	// readable, and a default that changes in a later release cannot
	// retroactively change what this drill did.
	if executed.Restore.Network != "isolated" {
		t.Errorf("snapshot network = %q, want the isolated default", executed.Restore.Network)
	}
	if executed.Restore.Timeout == 0 {
		t.Error("snapshot restore timeout is zero: the defaults were not applied before it was taken")
	}
	if !executed.Cleanup.CleanupAlways() {
		t.Error("snapshot cleanup.always is false: the default must survive into the snapshot")
	}
	if !executed.Startup.WaitsForAgent() {
		t.Error("snapshot does not wait for the guest agent, yet its only check runs inside the guest")
	}
	if executed.Startup.WaitsForIP() {
		t.Error("snapshot waits for an address it has no check to use")
	}
	// The check's free-form params have to survive the round trip: a command
	// check whose "run" was dropped would reach the engine with nothing to
	// run, and every drill launched by name would fail.
	if len(executed.Checks) != 1 {
		t.Fatalf("the snapshot holds %d checks, want 1", len(executed.Checks))
	}
	if got := executed.Checks[0].Params["run"]; got != strings.TrimPrefix(queueCheck, "cmd:") {
		t.Errorf("snapshot check run = %v, want the command the document gave", got)
	}
	// And it is not the document. plan_yaml is what was written; plan_snapshot
	// is what ran, and conflating them is exactly the bug this separation
	// exists to prevent.
	if strings.Contains(snapshot, "# The nightly drill") {
		t.Error("the snapshot is the stored document verbatim; it must be the defaulted plan")
	}

	// --- 7. the plan is deleted, and the run does not move -------------------

	before, beforeBody := f.mustGetRun(t, queued.ID)
	listedBefore := f.runListing(t, queued.ID)
	if listedBefore["plan_id"] != stored.ID {
		t.Fatalf("the listing does not link the run to its plan: %v", listedBefore)
	}

	if status, body := f.do(t, http.MethodDelete, "/api/v1/plans/"+storedPlanName, f.operate, ""); status != http.StatusForbidden {
		t.Errorf("DELETE /plans with an operate token = %d: %s\nwant 403", status, body)
	}
	if status, body := f.do(t, http.MethodDelete, "/api/v1/plans/"+storedPlanName, f.manage, ""); status != http.StatusNoContent {
		t.Fatalf("DELETE /plans/%s = %d: %s", storedPlanName, status, body)
	}
	if status, _ := f.do(t, http.MethodGet, "/api/v1/plans/"+storedPlanName, f.manage, ""); status != http.StatusNotFound {
		t.Errorf("GET the deleted plan = %d, want 404", status)
	}

	// The report loses its link to the plan and nothing else. plan_id is the
	// one field ON DELETE SET NULL is allowed to reach, and this is where a
	// reader would see it: the name, the version that ran, the timeline, the
	// checks and the verdict all stay exactly as they were, because the plan
	// a run executed is copied into the run, never referenced.
	after, afterBody := f.mustGetRun(t, queued.ID)
	if before["plan_id"] != stored.ID {
		t.Errorf("the report did not carry plan_id before the deletion: %v", before["plan_id"])
	}
	if _, ok := after["plan_id"]; ok {
		t.Errorf("the report still links a deleted plan: %v", after["plan_id"])
	}
	if after["plan_version"] != before["plan_version"] {
		t.Errorf("plan_version changed with the deletion: %v then %v",
			before["plan_version"], after["plan_version"])
	}
	delete(before, "plan_id")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("deleting the plan changed more than the link:\nbefore: %s\nafter:  %s", beforeBody, afterBody)
	}

	// The listing is the one place the link shows, so it is the one place
	// anything is allowed to change: plan_id goes, and nothing else moves.
	listedAfter := f.runListing(t, queued.ID)
	if _, ok := listedAfter["plan_id"]; ok {
		t.Errorf("plan_id survived the deletion of the plan: %v", listedAfter)
	}
	delete(listedBefore, "plan_id")
	if !reflect.DeepEqual(listedBefore, listedAfter) {
		t.Errorf("deleting the plan changed the run beyond its plan link:\nbefore: %v\nafter:  %v",
			listedBefore, listedAfter)
	}

	// And in the row itself: the link is gone, the version and the snapshot
	// stay. A report can still say which version of which plan produced it,
	// and can still show what that plan asked for, with the plan long gone.
	afterPlanID, afterVersion, afterSnapshot := f.raw.provenance(t, queued.ID)
	if afterPlanID != "" {
		t.Errorf("plan_id = %q, want it cleared by ON DELETE SET NULL", afterPlanID)
	}
	if afterVersion != 2 {
		t.Errorf("plan_version = %d after the deletion, want 2: provenance is not the link", afterVersion)
	}
	if afterSnapshot != snapshot {
		t.Error("the snapshot changed when the plan was deleted: a report must be immutable")
	}

	// Nothing in any of that touched the cluster.
	if n := countRestores(f.pve); n != 1 {
		t.Errorf("the cluster saw %d restores by the end, want 1", n)
	}
	if !hasVM(f.pve, sourceVMID) {
		t.Fatal("the SOURCE workload was destroyed")
	}
}

// mustGetRun returns the full run document the API renders, as a bare map,
// alongside the raw body for a failure message.
//
// The backup's age is dropped on the way out. It is a function of now rather
// than of the stored row - the renderer measures it against the clock every
// time - so two renderings of the same finished run legitimately differ
// there, and comparing it would make this test fail for the one reason that
// has nothing whatever to do with plans.
func (f *queueFixture) mustGetRun(t *testing.T, runID string) (map[string]any, string) {
	t.Helper()

	status, body := f.get(t, "/api/v1/recovery-runs/"+runID)
	if status != http.StatusOK {
		t.Fatalf("GET /recovery-runs/%s = %d: %s", runID, status, body)
	}

	doc := decodeBody[map[string]any](t, "the run document", body)
	if backup, ok := doc["backup"].(map[string]any); ok {
		delete(backup, "age_seconds")
		delete(backup, "age")
	}
	return doc, body
}
