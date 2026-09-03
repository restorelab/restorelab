package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

// scheduledPlanDTO is one scheduled plan, and what is about to happen to it.
type scheduledPlanDTO struct {
	PlanID     string `json:"plan_id"`
	Name       string `json:"name"`
	WorkloadID string `json:"workload_id"`
	Schedule   string `json:"schedule"`
	Timezone   string `json:"timezone,omitempty"`

	// NextSlotAt is when this plan drills next, or null when its schedule
	// cannot be read. A caller must be able to tell "nothing is coming"
	// apart from "we could not work it out", so the two are different
	// fields rather than one empty string.
	NextSlotAt *time.Time `json:"next_slot_at"`
	// Error explains a null NextSlotAt. A plan whose document or cron no
	// longer parses is reported rather than dropped from the listing: it is
	// a plan somebody believes is scheduled, and silence is what leaves a
	// machine untested for months.
	Error string `json:"error,omitempty"`

	LastSlot *slotDTO `json:"last_slot,omitempty"`
}

// slotDTO is one decided slot.
type slotDTO struct {
	PlanID    string    `json:"plan_id"`
	PlanName  string    `json:"plan_name,omitempty"`
	SlotAt    time.Time `json:"slot_at"`
	DecidedAt time.Time `json:"decided_at"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
}

func newSlotDTO(s store.Slot, planName string) slotDTO {
	return slotDTO{
		PlanID:    s.PlanID,
		PlanName:  planName,
		SlotAt:    s.SlotAt,
		DecidedAt: s.DecidedAt,
		Outcome:   string(s.Outcome),
		Reason:    s.Reason,
		RunID:     s.RunID,
	}
}

// handleGetSchedule lists the plans that drill themselves, and when next.
//
// A plan with no schedule is absent: most plans have none, and padding the
// listing with them would bury the handful that matter.
func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	rows, err := catalog.List(r.Context(), s.plans, store.PlanFilter{})
	if err != nil {
		writeProblem(w, r, problemForPlan(err))
		return
	}

	now := s.now()
	items := make([]scheduledPlanDTO, 0, len(rows))
	for _, row := range rows {
		dto, scheduled := s.scheduledPlan(r, row, now)
		if scheduled {
			items = append(items, dto)
		}
	}
	writeJSON(w, r, page[scheduledPlanDTO]{Items: items})
}

// scheduledPlan renders one catalogue row, and reports whether it is
// scheduled at all.
//
// A row whose document or cron cannot be read counts as scheduled: somebody
// wrote a schedule into it, and the answer they need is the error - not an
// absence they will read as "this plan is fine, it just is not scheduled".
func (s *Server) scheduledPlan(r *http.Request, row store.Plan, now time.Time) (scheduledPlanDTO, bool) {
	dto := scheduledPlanDTO{
		PlanID:     row.ID,
		Name:       row.Name,
		WorkloadID: row.WorkloadID,
	}

	parsed, err := plan.Parse([]byte(row.YAML))
	if err != nil {
		// Unparseable: there is no schedule field to read, so the only
		// honest thing is to say the document is broken. It is included
		// because a broken plan that was scheduled has stopped drilling.
		dto.Error = "the stored plan is not valid: " + err.Error()
		return dto, true
	}

	dto.Schedule = parsed.Schedule
	dto.Timezone = parsed.ScheduleTimezone

	sched, err := plan.ParseSchedule(parsed.Schedule, parsed.ScheduleTimezone)
	if err != nil {
		dto.Error = err.Error()
		return dto, true
	}
	if sched == nil {
		return dto, false
	}

	// A read failure here is not worth a 500 for the whole listing: the
	// plan's schedule is still worth reporting, and the missing piece is one
	// column of one row.
	last, err := s.schedules.LastSlot(r.Context(), row.ID)
	if err == nil {
		slot := newSlotDTO(*last, row.Name)
		dto.LastSlot = &slot
	} else if !errors.Is(err, store.ErrNotFound) {
		dto.Error = "the last slot could not be read: " + err.Error()
	}

	after := now
	if last != nil && last.SlotAt.After(after) {
		after = last.SlotAt
	}
	next := sched.Next(after)
	dto.NextSlotAt = &next
	return dto, true
}

// handleListSlots reports the slots the scheduler has decided.
//
// Skipped slots are included, and that is the point of the endpoint: "why was
// this machine not tested" is a question the dashboard has to be able to
// answer, and it cannot from the run history alone - a slot that was skipped
// produced no run.
func (s *Server) handleListSlots(w http.ResponseWriter, r *http.Request) {
	f := store.SlotFilter{}

	// A workload rather than a plan: the machine is what somebody is looking
	// at when they ask why it was not tested, and it may be covered by more
	// than one plan.
	if workload := r.URL.Query().Get("workload"); workload != "" {
		f.WorkloadID = workload
	}

	if ref := r.URL.Query().Get("plan"); ref != "" {
		row, err := catalog.Get(r.Context(), s.plans, ref)
		if err != nil {
			writeProblem(w, r, problemForPlan(err))
			return
		}
		f.PlanID = row.ID
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeBadRequest(w, r, "limit must be a positive whole number")
			return
		}
		f.Limit = n
	}

	slots, err := s.schedules.ListSlots(r.Context(), f)
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}

	// One lookup per plan, not per slot: a page of slots for one plan would
	// otherwise be fifty identical queries for the same name.
	names := map[string]string{}
	items := make([]slotDTO, 0, len(slots))
	for _, slot := range slots {
		name, ok := names[slot.PlanID]
		if !ok {
			if row, err := s.plans.GetPlan(r.Context(), slot.PlanID); err == nil {
				name = row.Name
			}
			names[slot.PlanID] = name
		}
		items = append(items, newSlotDTO(slot, name))
	}
	writeJSON(w, r, page[slotDTO]{Items: items})
}
