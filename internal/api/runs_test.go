package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
	"github.com/restorelab/restorelab/internal/store"
)

// seedRuns fills a fake history with n runs, newest first, one minute apart.
func seedRuns(h *fakeHistory, n int) {
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := string(rune('a'+i)) + "0000000-0000-0000-0000-000000000000"
		started := base.Add(-time.Duration(i) * time.Minute)
		h.runs = append(h.runs, store.RunSummary{
			ID:               id,
			PlanName:         "adhoc-110",
			SourceWorkloadID: "110",
			SourceName:       "linux-test",
			State:            core.RunSuccess,
			Result:           core.ResultSuccess,
			StartedAt:        started,
			CompletedAt:      started.Add(28 * time.Second),
			RTO:              28 * time.Second,
			RTOTarget:        5 * time.Minute,
			CleanupDone:      true,
		})
		h.byID[id] = &core.RecoveryRun{
			ID: id, PlanName: "adhoc-110", SourceWorkloadID: "110", SourceName: "linux-test",
			State: core.RunSuccess, Result: core.ResultSuccess,
			StartedAt: started, CompletedAt: started.Add(28 * time.Second),
			RTO: 28 * time.Second, RTOTarget: 5 * time.Minute, CleanupDone: true,
			Steps: []core.Step{{
				Name: "restore", State: core.RunRestoring, Status: core.StepDone,
				StartedAt: started, CompletedAt: started.Add(4 * time.Second), Duration: 4 * time.Second,
			}},
			Checks: []core.CheckResult{{
				Name: "ssh is up", Type: "command", Status: core.CheckPass, Attempts: 1,
			}},
		}
	}
}

func TestListingRunsPagesWithACursor(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 5)
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var first page[runSummaryDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(first.Items))
	}
	if first.NextCursor == "" {
		t.Fatal("no next_cursor with three runs left")
	}
	if first.Items[0].RTO == "" || first.Items[0].RTOSeconds == 0 {
		t.Error("the RTO is missing from the listing")
	}

	rec = do(s, http.MethodGet, "/api/v1/recovery-runs?limit=2&cursor="+first.NextCursor)
	var second page[runSummaryDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("second page is not a page: %v", err)
	}
	if len(second.Items) != 2 {
		t.Fatalf("second page has %d items, want 2", len(second.Items))
	}
	if second.Items[0].ID == first.Items[1].ID {
		t.Fatal("the second page repeated the first page's last row")
	}
}

func TestTheLastPageHasNoCursor(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 2)
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs?limit=10")

	var p page[runSummaryDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if p.NextCursor != "" {
		t.Errorf("next_cursor = %q on the last page, want it absent", p.NextCursor)
	}
}

func TestAnEmptyListingIsAnEmptyArrayNotNull(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs")

	if got := rec.Body.String(); !strings.Contains(got, `"items":[]`) {
		t.Fatalf("body = %s, want an empty array: a null would break every client that iterates", got)
	}
}

func TestABadCursorIsARequestError(t *testing.T) {
	s, _ := newTestServer(t, Options{})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs?cursor=!!!")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestABrokenHistoryIsA503(t *testing.T) {
	h := newFakeHistory()
	h.err = store.ErrNoHistory
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestGettingARunAcceptsAPrefix(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 3)
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs/a000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if doc["schema"] != "restorelab.recovery-run/v1" {
		t.Errorf("schema = %v, want the report document schema", doc["schema"])
	}
	if doc["run_id"] == nil {
		t.Error("the document has no run_id")
	}
	if _, ok := doc["steps"]; !ok {
		t.Error("the full run has no steps")
	}
}

func TestAnUnknownRunIsA404AndAnAmbiguousOneA409(t *testing.T) {
	h := newFakeHistory()
	h.byID["abc11111-0000-0000-0000-000000000000"] = &core.RecoveryRun{ID: "abc11111-0000-0000-0000-000000000000"}
	h.byID["abc22222-0000-0000-0000-000000000000"] = &core.RecoveryRun{ID: "abc22222-0000-0000-0000-000000000000"}
	s, _ := newTestServer(t, Options{History: h})

	if rec := do(s, http.MethodGet, "/api/v1/recovery-runs/zzzz"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown prefix = %d, want 404", rec.Code)
	}
	if rec := do(s, http.MethodGet, "/api/v1/recovery-runs/abc"); rec.Code != http.StatusConflict {
		t.Errorf("ambiguous prefix = %d, want 409", rec.Code)
	}
}

func TestTheHTMLReportIsServedAsHTML(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs/a000/report?format=html")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Error("the HTML report has no html element")
	}
}

func TestAnUnknownReportFormatIsRefused(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs/a000/report?format=pdf")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestEventsComeBackInOrderAndCanBeResumed(t *testing.T) {
	h := newFakeHistory()
	seedRuns(h, 1)
	id := h.runs[0].ID
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for seq := int64(1); seq <= 3; seq++ {
		h.events[id] = append(h.events[id], store.Event{
			Seq: seq, At: at.Add(time.Duration(seq) * time.Second),
			State: core.RunRestoring, Step: "restore", Status: core.StepRunning,
			Message: "restoring",
		})
	}
	s, _ := newTestServer(t, Options{History: h})

	rec := do(s, http.MethodGet, "/api/v1/recovery-runs/a000/events")
	var all page[eventDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(all.Items) != 3 {
		t.Fatalf("got %d events, want 3", len(all.Items))
	}
	if all.Items[0].Seq != 1 {
		t.Errorf("first seq = %d, want 1: events are ordered by the engine's emission order", all.Items[0].Seq)
	}

	rec = do(s, http.MethodGet, "/api/v1/recovery-runs/a000/events?after=2")
	var rest page[eventDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &rest); err != nil {
		t.Fatalf("body is not a page: %v", err)
	}
	if len(rest.Items) != 1 || rest.Items[0].Seq != 3 {
		t.Fatalf("resuming after seq 2 gave %+v, want only seq 3", rest.Items)
	}
}

// A listing carries the provenance too, not only the full run: a dashboard
// grouping drills by plan reads the listing, and would otherwise have to
// fetch every run to find out which plan produced it.
func TestAListingCarriesThePlanItCameFrom(t *testing.T) {
	h := newFakeHistory()
	// add prepends, the way the real listing orders: the last one added is
	// the most recent, and comes back first.
	h.add(core.RecoveryRun{
		ID: "adhoc-run", PlanName: "adhoc-104",
		SourceWorkloadID: "104", State: core.RunSuccess,
		StartedAt: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	})
	h.add(core.RecoveryRun{
		ID: "run-from-a-plan", PlanName: "web-tier", PlanID: "plan-id", PlanVersion: 2,
		SourceWorkloadID: "110", State: core.RunSuccess,
		StartedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	})
	s, _ := newTestServer(t, Options{History: h})

	rec := send(s, http.MethodGet, testSecret, "/api/v1/recovery-runs", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var out page[runSummaryDTO]
	decodePlan(t, rec, &out)
	if len(out.Items) != 2 {
		t.Fatalf("listed %d runs, want 2", len(out.Items))
	}
	if out.Items[0].PlanID != "plan-id" {
		t.Errorf("PlanID = %q, want plan-id", out.Items[0].PlanID)
	}
	// An ad-hoc drill has no plan, and omitempty must leave the field out
	// rather than report an empty one.
	if out.Items[1].PlanID != "" {
		t.Errorf("an ad-hoc run reports PlanID %q, want none", out.Items[1].PlanID)
	}
	if !strings.Contains(rec.Body.String(), `"plan_id":"plan-id"`) {
		t.Errorf("the wire form does not carry plan_id:\n%s", rec.Body)
	}
}
