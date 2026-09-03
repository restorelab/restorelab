package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/store"
)

// fakeSchedules is the slot table, in a map.
type fakeSchedules struct {
	slots []store.Slot
	// workloadOf stands in for the join onto plans that the real ListSlots
	// does: plan id to the workload its plan covers.
	workloadOf map[string]string
	err        error
}

func (f *fakeSchedules) LastSlot(_ context.Context, planID string) (*store.Slot, error) {
	if f.err != nil {
		return nil, f.err
	}
	var newest *store.Slot
	for i := range f.slots {
		if f.slots[i].PlanID != planID {
			continue
		}
		if newest == nil || f.slots[i].SlotAt.After(newest.SlotAt) {
			newest = &f.slots[i]
		}
	}
	if newest == nil {
		return nil, store.ErrNotFound
	}
	return newest, nil
}

func (f *fakeSchedules) ListSlots(_ context.Context, filter store.SlotFilter) ([]store.Slot, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []store.Slot
	for _, s := range f.slots {
		if filter.PlanID != "" && s.PlanID != filter.PlanID {
			continue
		}
		// The real store resolves this with a join onto plans; the fake is
		// told which plan covers which workload by the test.
		if filter.WorkloadID != "" && f.workloadOf[s.PlanID] != filter.WorkloadID {
			continue
		}
		out = append(out, s)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// scheduleServer is planServer with a slot table attached.
func scheduleServer(t *testing.T) (*Server, *fakePlans, *fakeSchedules) {
	t.Helper()
	plans := newFakePlans()
	schedules := &fakeSchedules{workloadOf: map[string]string{}}
	s, tokens := newTestServer(t, Options{
		Plans:     plans,
		Schedules: schedules,
		Now:       func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
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
	return s, plans, schedules
}

// withSchedule returns a plan document carrying a cron expression.
func withSchedule(name, workload, schedule string) string {
	doc := withName(name, workload)
	return doc + "schedule: \"" + schedule + "\"\nschedule_timezone: UTC\n"
}

func TestScheduleNeedsAToken(t *testing.T) {
	s, _, _ := scheduleServer(t)
	for _, path := range []string{"/api/v1/schedule", "/api/v1/schedule/slots"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", path, rec.Code)
		}
	}
}

func TestScheduleListsOnlyScheduledPlans(t *testing.T) {
	s, _, _ := scheduleServer(t)
	seedPlan(t, s, withSchedule("nightly", "110", "0 3 * * *"))
	seedPlan(t, s, withName("manual-only", "104"))

	rec := do(s, http.MethodGet, "/api/v1/schedule")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var p page[scheduledPlanDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 {
		t.Fatalf("the listing holds %d plans, want 1 - only the scheduled one: %s", len(p.Items), rec.Body)
	}
	got := p.Items[0]
	if got.Name != "nightly" || got.Schedule != "0 3 * * *" {
		t.Fatalf("listing = %+v, want the nightly plan and its cron", got)
	}
	if got.NextSlotAt == nil {
		t.Fatal("next_slot_at is null for a valid schedule")
	}
	// Noon on 2026-09-03, daily at 03:00 UTC: tomorrow morning.
	want := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	if !got.NextSlotAt.Equal(want) {
		t.Fatalf("next_slot_at = %v, want %v", got.NextSlotAt, want)
	}
	if got.LastSlot != nil {
		t.Fatalf("last_slot = %+v, want absent for a plan never scheduled", got.LastSlot)
	}
}

// A plan whose cron cannot be read must be reported, not dropped. Dropping it
// reads as "this plan is not scheduled", which is exactly the wrong answer
// for a plan that somebody scheduled and that has silently stopped drilling.
func TestScheduleReportsAPlanWithAnUnreadableCron(t *testing.T) {
	s, plans, _ := scheduleServer(t)
	dto := seedPlan(t, s, withSchedule("nightly", "110", "0 3 * * *"))

	// Edited in the database rather than through the API, which refuses it.
	row := plans.stored[dto.ID]
	row.YAML = strings.Replace(row.YAML, `"0 3 * * *"`, `"every tuesday"`, 1)
	plans.stored[dto.ID] = row

	rec := do(s, http.MethodGet, "/api/v1/schedule")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - one bad plan must not take the endpoint down: %s", rec.Code, rec.Body)
	}

	var p page[scheduledPlanDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 {
		t.Fatalf("the listing holds %d plans, want 1: %s", len(p.Items), rec.Body)
	}
	if p.Items[0].NextSlotAt != nil {
		t.Fatalf("next_slot_at = %v, want null for an unreadable cron", p.Items[0].NextSlotAt)
	}
	if p.Items[0].Error == "" {
		t.Fatal("a plan with an unreadable cron carries no error to explain itself")
	}
}

func TestScheduleCarriesTheLastSlot(t *testing.T) {
	s, _, schedules := scheduleServer(t)
	dto := seedPlan(t, s, withSchedule("nightly", "110", "0 3 * * *"))

	slotAt := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	schedules.slots = []store.Slot{{
		PlanID: dto.ID, SlotAt: slotAt, DecidedAt: slotAt,
		Outcome: store.SlotQueued, RunID: "run-1",
	}}

	rec := do(s, http.MethodGet, "/api/v1/schedule")
	var p page[scheduledPlanDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 || p.Items[0].LastSlot == nil {
		t.Fatalf("last_slot is absent: %s", rec.Body)
	}
	if p.Items[0].LastSlot.RunID != "run-1" {
		t.Fatalf("last_slot.run_id = %q, want run-1", p.Items[0].LastSlot.RunID)
	}
}

func TestSlotsCarryTheReasonASlotWasSkipped(t *testing.T) {
	s, _, schedules := scheduleServer(t)
	dto := seedPlan(t, s, withSchedule("nightly", "110", "0 3 * * *"))

	slotAt := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	schedules.slots = []store.Slot{{
		PlanID: dto.ID, SlotAt: slotAt, DecidedAt: slotAt.Add(6 * time.Hour),
		Outcome: store.SlotSkipped,
		Reason:  "the slot was 6h0m late, past the 2h grace period",
	}}

	rec := do(s, http.MethodGet, "/api/v1/schedule/slots")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var p page[slotDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 {
		t.Fatalf("the listing holds %d slots, want 1: %s", len(p.Items), rec.Body)
	}
	// The reason is the whole reason this endpoint exists.
	if !strings.Contains(p.Items[0].Reason, "grace period") {
		t.Fatalf("reason = %q, want it to explain the skip", p.Items[0].Reason)
	}
	if p.Items[0].Outcome != string(store.SlotSkipped) {
		t.Fatalf("outcome = %q, want skipped", p.Items[0].Outcome)
	}
	// And the plan's name, so a dashboard need not resolve ids itself.
	if p.Items[0].PlanName != "nightly" {
		t.Fatalf("plan_name = %q, want nightly", p.Items[0].PlanName)
	}
}

func TestSlotsFilterByPlan(t *testing.T) {
	s, _, schedules := scheduleServer(t)
	one := seedPlan(t, s, withSchedule("nightly", "110", "0 3 * * *"))
	two := seedPlan(t, s, withSchedule("weekly", "104", "0 4 * * 0"))

	at := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	schedules.slots = []store.Slot{
		{PlanID: one.ID, SlotAt: at, DecidedAt: at, Outcome: store.SlotQueued, RunID: "run-1"},
		{PlanID: two.ID, SlotAt: at, DecidedAt: at, Outcome: store.SlotQueued, RunID: "run-2"},
	}

	rec := do(s, http.MethodGet, "/api/v1/schedule/slots?plan=nightly")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var p page[slotDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 || p.Items[0].RunID != "run-1" {
		t.Fatalf("filtered listing = %+v, want only the nightly plan's slot", p.Items)
	}
}

func TestSlotsForAnUnknownPlanIs404(t *testing.T) {
	s, _, _ := scheduleServer(t)
	rec := do(s, http.MethodGet, "/api/v1/schedule/slots?plan=no-such-plan")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestSlotsRefusesANonsenseLimit(t *testing.T) {
	s, _, _ := scheduleServer(t)
	for _, raw := range []string{"0", "-3", "many"} {
		rec := do(s, http.MethodGet, "/api/v1/schedule/slots?limit="+raw)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s = %d, want 400", raw, rec.Code)
		}
	}
}

// A machine can be covered by more than one plan, so the dashboard asks about
// the machine and the API resolves the plans.
func TestSlotsFilterByWorkload(t *testing.T) {
	s, _, schedules := scheduleServer(t)
	one := seedPlan(t, s, withSchedule("nightly", "110", "0 3 * * *"))
	two := seedPlan(t, s, withSchedule("other", "104", "0 4 * * *"))

	at := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	schedules.slots = []store.Slot{
		{PlanID: one.ID, SlotAt: at, DecidedAt: at, Outcome: store.SlotSkipped, Reason: "late"},
		{PlanID: two.ID, SlotAt: at, DecidedAt: at, Outcome: store.SlotSkipped, Reason: "late"},
	}

	schedules.workloadOf[one.ID] = "110"
	schedules.workloadOf[two.ID] = "104"

	rec := do(s, http.MethodGet, "/api/v1/schedule/slots?workload=110")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var p page[slotDTO]
	decodePlan(t, rec, &p)
	if len(p.Items) != 1 || p.Items[0].PlanID != one.ID {
		t.Fatalf("filtered listing = %+v, want only workload 110's slot", p.Items)
	}
}
