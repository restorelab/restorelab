package store

// The conformance suite for what hangs off a run: its steps, its check
// results, its progress events, and the listing. Split from conformance.go so
// neither file grows past what fits in one head.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// newConformanceRun creates a run for a sub-suite to attach things to.
func newConformanceRun(t *testing.T, s Store, id string) *core.RecoveryRun {
	t.Helper()
	run := sampleRun(id)
	if err := s.CreateRun(context.Background(), run, "name: x\n"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// StepsAndChecksConformance exercises the timeline attached to a run.
func StepsAndChecksConformance(t *testing.T, open OpenFunc) {
	t.Run("steps come back in order with their durations", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "cccccccc-0000-0000-0000-000000000000")

		steps := []core.Step{
			{Name: "discover_backup", State: core.RunDiscoveringBackup, Status: core.StepDone, Duration: 445 * time.Millisecond, Message: "using backup X"},
			{Name: "restore", State: core.RunRestoring, Status: core.StepDone, Duration: 4700 * time.Millisecond},
			{Name: "cleanup", State: core.RunCleaningUp, Status: core.StepSkipped, Message: "KeepWorkload requested"},
		}
		for i, step := range steps {
			if err := s.SaveStep(ctx, run.ID, i, step); err != nil {
				t.Fatalf("SaveStep %d: %v", i, err)
			}
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Steps) != len(steps) {
			t.Fatalf("got %d steps, want %d", len(got.Steps), len(steps))
		}
		for i, want := range steps {
			if got.Steps[i].Name != want.Name {
				t.Errorf("step %d name = %q, want %q (order must be preserved)", i, got.Steps[i].Name, want.Name)
			}
			if got.Steps[i].Duration != want.Duration {
				t.Errorf("step %d duration = %v, want %v", i, got.Steps[i].Duration, want.Duration)
			}
			if got.Steps[i].Status != want.Status {
				t.Errorf("step %d status = %v, want %v", i, got.Steps[i].Status, want.Status)
			}
			if got.Steps[i].Message != want.Message {
				t.Errorf("step %d message = %q, want %q", i, got.Steps[i].Message, want.Message)
			}
		}
	})

	t.Run("saving the same seq twice replaces it", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "cccccccc-1111-0000-0000-000000000000")

		if err := s.SaveStep(ctx, run.ID, 0, core.Step{Name: "restore", State: core.RunRestoring, Status: core.StepRunning}); err != nil {
			t.Fatalf("SaveStep running: %v", err)
		}
		if err := s.SaveStep(ctx, run.ID, 0, core.Step{Name: "restore", State: core.RunRestoring, Status: core.StepDone, Duration: time.Second}); err != nil {
			t.Fatalf("SaveStep done: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Steps) != 1 {
			t.Fatalf("got %d steps, want 1: a step is upserted at its seq, not appended", len(got.Steps))
		}
		if got.Steps[0].Status != core.StepDone {
			t.Errorf("status = %v, want done: the second write must win", got.Steps[0].Status)
		}
	})

	t.Run("checks keep their details", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "cccccccc-2222-0000-0000-000000000000")

		check := core.CheckResult{
			Name:     "COMMAND",
			Type:     "command",
			Status:   core.CheckPass,
			Duration: 902 * time.Millisecond,
			Attempts: 3,
			Message:  `exit 0, stdout "active"`,
			Details: map[string]any{
				"exit_code": float64(0),
				"argv":      []any{"/bin/sh", "-c", "systemctl is-active ssh"},
			},
		}
		if err := s.SaveCheck(ctx, run.ID, 0, check); err != nil {
			t.Fatalf("SaveCheck: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Checks) != 1 {
			t.Fatalf("got %d checks, want 1", len(got.Checks))
		}
		if got.Checks[0].Attempts != check.Attempts {
			t.Errorf("Attempts = %d, want %d", got.Checks[0].Attempts, check.Attempts)
		}
		if got.Checks[0].Type != check.Type {
			t.Errorf("Type = %q, want %q", got.Checks[0].Type, check.Type)
		}
		if got.Checks[0].Message != check.Message {
			t.Errorf("Message = %q, want %q", got.Checks[0].Message, check.Message)
		}
		if got.Checks[0].Details["exit_code"] != float64(0) {
			t.Errorf("Details lost the exit code: %v", got.Checks[0].Details)
		}
	})

	t.Run("a run with no timeline reads back empty, not broken", func(t *testing.T) {
		s := open(t)
		run := newConformanceRun(t, s, "cccccccc-3333-0000-0000-000000000000")

		got, err := s.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Steps) != 0 || len(got.Checks) != 0 {
			t.Fatalf("got %d steps and %d checks, want none", len(got.Steps), len(got.Checks))
		}
	})
}

// EventsConformance exercises the progress stream. Phase B's SSE replays from
// it on reconnection, so ordering and the "after this seq" cursor matter more
// than they appear to in phase A.
func EventsConformance(t *testing.T, open OpenFunc) {
	t.Run("events come back in seq order", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "dddddddd-0000-0000-0000-000000000000")

		at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
		// Written out of order on purpose: seq decides, not arrival.
		for _, seq := range []int64{3, 1, 2} {
			ev := Event{
				Seq:     seq,
				At:      at.Add(time.Duration(seq) * time.Second),
				State:   core.RunRestoring,
				Step:    "restore",
				Status:  core.StepRunning,
				Message: "restoring",
			}
			if err := s.AppendEvent(ctx, run.ID, ev); err != nil {
				t.Fatalf("AppendEvent %d: %v", seq, err)
			}
		}

		got, err := s.Events(ctx, run.ID, 0)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d events, want 3", len(got))
		}
		for i, want := range []int64{1, 2, 3} {
			if got[i].Seq != want {
				t.Errorf("event %d has seq %d, want %d", i, got[i].Seq, want)
			}
		}
		if got[0].Step != "restore" || got[0].Status != core.StepRunning {
			t.Errorf("event 0 = %+v, want step restore in status running", got[0])
		}
	})

	t.Run("afterSeq skips what the caller already has", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "eeeeeeee-0000-0000-0000-000000000000")

		for seq := int64(1); seq <= 5; seq++ {
			if err := s.AppendEvent(ctx, run.ID, Event{Seq: seq, At: nowUTC(), State: core.RunRestoring}); err != nil {
				t.Fatalf("AppendEvent %d: %v", seq, err)
			}
		}

		got, err := s.Events(ctx, run.ID, 3)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 {
			t.Fatalf("Events(after 3) returned %d events, want seq 4 and 5: %+v", len(got), got)
		}
	})

	t.Run("a check event keeps its result", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "ffffffff-0000-0000-0000-000000000000")

		check := &core.CheckResult{Name: "COMMAND", Type: "command", Status: core.CheckPass, Message: "exit 0"}
		if err := s.AppendEvent(ctx, run.ID, Event{Seq: 1, At: nowUTC(), State: core.RunRunningChecks, Check: check}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}

		got, err := s.Events(ctx, run.ID, 0)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 1 || got[0].Check == nil {
			t.Fatalf("the check result did not survive: %+v", got)
		}
		if got[0].Check.Name != "COMMAND" || got[0].Check.Status != core.CheckPass {
			t.Errorf("check = %+v, want COMMAND/pass", got[0].Check)
		}
	})

	t.Run("an event without a check reads back with none", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "ffffffff-1111-0000-0000-000000000000")

		if err := s.AppendEvent(ctx, run.ID, Event{Seq: 1, At: nowUTC(), State: core.RunRestoring}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		got, err := s.Events(ctx, run.ID, 0)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(got) != 1 || got[0].Check != nil {
			t.Fatalf("got %+v, want one event with no check", got)
		}
	})
}

// ListConformance exercises the history listing.
func ListConformance(t *testing.T, open OpenFunc) {
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	seed := func(t *testing.T, s Store) {
		t.Helper()
		ctx := context.Background()
		specs := []struct {
			id       string
			workload string
			state    core.RunState
			result   core.RunResult
			offset   time.Duration
		}{
			{"11111111-0000-0000-0000-000000000000", "110", core.RunSuccess, core.ResultSuccess, 0},
			{"22222222-0000-0000-0000-000000000000", "104", core.RunFailed, core.ResultFailed, time.Hour},
			{"33333333-0000-0000-0000-000000000000", "110", core.RunSuccess, core.ResultDegraded, 2 * time.Hour},
		}
		for _, spec := range specs {
			run := sampleRun(spec.id)
			run.SourceWorkloadID = spec.workload
			run.State = spec.state
			run.Result = spec.result
			run.StartedAt = base.Add(spec.offset)
			run.CompletedAt = run.StartedAt.Add(30 * time.Second)
			if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
				t.Fatalf("CreateRun %s: %v", spec.id, err)
			}
		}
	}

	t.Run("most recent first", func(t *testing.T) {
		s := open(t)
		seed(t, s)

		got, err := s.ListRuns(context.Background(), Filter{})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d runs, want 3", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].StartedAt.Before(got[i].StartedAt) {
				t.Fatalf("run %d started before run %d: the listing must be newest first", i-1, i)
			}
		}
		if got[0].SourceName == "" || got[0].RTO == 0 {
			t.Errorf("the summary lost fields the listing shows: %+v", got[0])
		}
	})

	t.Run("filter by workload", func(t *testing.T) {
		s := open(t)
		seed(t, s)

		got, err := s.ListRuns(context.Background(), Filter{WorkloadID: "110"})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d runs for workload 110, want 2", len(got))
		}
		for _, r := range got {
			if r.SourceWorkloadID != "110" {
				t.Errorf("got a run for workload %q", r.SourceWorkloadID)
			}
		}
	})

	t.Run("filter by result", func(t *testing.T) {
		s := open(t)
		seed(t, s)

		got, err := s.ListRuns(context.Background(), Filter{Result: core.ResultFailed})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].SourceWorkloadID != "104" {
			t.Fatalf("got %+v, want the single failed run for workload 104", got)
		}
	})

	t.Run("filter by state", func(t *testing.T) {
		s := open(t)
		seed(t, s)

		got, err := s.ListRuns(context.Background(), Filter{State: core.RunFailed})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d runs in state FAILED, want 1", len(got))
		}
	})

	// This is the test that the fixed-width timestamp layout underwrites: the
	// comparison happens on text, and it must still mean "later than".
	t.Run("filter by since", func(t *testing.T) {
		s := open(t)
		seed(t, s)

		got, err := s.ListRuns(context.Background(), Filter{Since: base.Add(90 * time.Minute)})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d runs since 90 minutes in, want 1", len(got))
		}
		if !got[0].StartedAt.Equal(base.Add(2 * time.Hour)) {
			t.Errorf("got the run started at %v, want the one at %v", got[0].StartedAt, base.Add(2*time.Hour))
		}
	})

	t.Run("limit is honoured", func(t *testing.T) {
		s := open(t)
		seed(t, s)

		got, err := s.ListRuns(context.Background(), Filter{Limit: 2})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d runs, want 2", len(got))
		}
	})

	t.Run("an empty store lists nothing without erroring", func(t *testing.T) {
		s := open(t)

		got, err := s.ListRuns(context.Background(), Filter{})
		if err != nil {
			t.Fatalf("ListRuns on an empty store: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d runs, want none", len(got))
		}
	})

	t.Run("paging with a cursor walks the whole history exactly once", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()

		// Cinq runs, une minute d'écart, du plus ancien au plus récent.
		var ids []string
		for i := 0; i < 5; i++ {
			id := fmt.Sprintf("aaaaaaa%d-0000-0000-0000-000000000000", i)
			run := sampleRun(id)
			run.StartedAt = base.Add(time.Duration(i) * time.Minute)
			run.CompletedAt = run.StartedAt.Add(30 * time.Second)
			if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
				t.Fatalf("CreateRun %s: %v", id, err)
			}
			ids = append(ids, id)
		}

		seen := map[string]int{}
		var after *Position
		for page := 0; page < 10; page++ {
			got, err := s.ListRuns(ctx, Filter{Limit: 2, After: after})
			if err != nil {
				t.Fatalf("ListRuns page %d: %v", page, err)
			}
			if len(got) == 0 {
				break
			}
			for _, r := range got {
				seen[r.ID]++
			}
			last := got[len(got)-1]
			after = &Position{StartedAt: last.StartedAt, ID: last.ID}

			// Un drill qui démarre pendant qu'on pagine : avec un OFFSET, il
			// décalerait toute la suite et ferait sauter une ligne.
			if page == 0 {
				fresh := sampleRun("bbbbbbbb-0000-0000-0000-000000000000")
				fresh.StartedAt = base.Add(time.Hour)
				fresh.CompletedAt = fresh.StartedAt.Add(30 * time.Second)
				if err := s.CreateRun(ctx, fresh, "name: x\n"); err != nil {
					t.Fatalf("CreateRun fresh: %v", err)
				}
			}
		}

		for _, id := range ids {
			switch seen[id] {
			case 0:
				t.Errorf("run %s was skipped while paging", id)
			case 1:
			default:
				t.Errorf("run %s came back %d times while paging", id, seen[id])
			}
		}
	})

	t.Run("two runs started at the same instant both come back", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()

		// Même started_at : sans l'id comme second critère de tri, le curseur
		// en perdrait un.
		for _, id := range []string{
			"cccccccc-0000-0000-0000-000000000001",
			"cccccccc-0000-0000-0000-000000000002",
		} {
			run := sampleRun(id)
			run.StartedAt = base
			if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
				t.Fatalf("CreateRun %s: %v", id, err)
			}
		}

		first, err := s.ListRuns(ctx, Filter{Limit: 1})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(first) != 1 {
			t.Fatalf("got %d runs, want 1", len(first))
		}
		second, err := s.ListRuns(ctx, Filter{Limit: 1, After: &Position{
			StartedAt: first[0].StartedAt, ID: first[0].ID,
		}})
		if err != nil {
			t.Fatalf("ListRuns page 2: %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("second page has %d runs, want 1: the tie on started_at lost a row", len(second))
		}
		if second[0].ID == first[0].ID {
			t.Fatal("the second page repeated the first row")
		}
	})

	t.Run("the summary carries the RTO target", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("dddddddd-0000-0000-0000-000000000000")
		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		got, err := s.ListRuns(ctx, Filter{})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if got[0].RTOTarget != run.RTOTarget {
			t.Errorf("RTOTarget = %v, want %v: the confidence score reads it from the listing",
				got[0].RTOTarget, run.RTOTarget)
		}
	})
}
