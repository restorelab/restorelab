package store

// The conformance suite for LastRuns: one query that answers "when was each
// of these machines last drilled", for a whole page of workloads at once. It
// lives outside a _test.go so both engines' test files can call it, same as
// the rest of the suite.

import (
	"context"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// runAt records a finished run for one workload at one instant.
func runAt(t *testing.T, s Store, id, workload string, started time.Time) {
	t.Helper()
	run := sampleRun(id)
	run.SourceWorkloadID = workload
	run.StartedAt = started
	run.CompletedAt = started.Add(30 * time.Second)
	if err := s.CreateRun(context.Background(), run, "name: x\n"); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
}

// LastRunsConformance exercises LastRuns.
func LastRunsConformance(t *testing.T, open OpenFunc) {
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	t.Run("each workload comes back with its newest run and nobody else's", func(t *testing.T) {
		s := open(t)
		runAt(t, s, "11110000-0000-0000-0000-000000000001", "110", base)
		runAt(t, s, "11110000-0000-0000-0000-000000000002", "110", base.Add(time.Hour))
		runAt(t, s, "22220000-0000-0000-0000-000000000001", "104", base.Add(30*time.Minute))

		got, err := s.LastRuns(context.Background(), []string{"110", "104"})
		if err != nil {
			t.Fatalf("LastRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2: %+v", len(got), got)
		}
		if got["110"].ID != "11110000-0000-0000-0000-000000000002" {
			t.Errorf("workload 110 = %s, want the newer run", got["110"].ID)
		}
		if got["104"].ID != "22220000-0000-0000-0000-000000000001" {
			t.Errorf("workload 104 = %s, want its own run", got["104"].ID)
		}
	})

	// "Never drilled" and "drilled and it went badly" are different answers,
	// and a caller that cannot tell them apart will render one as the other.
	t.Run("a workload that has never been drilled is absent, not zero", func(t *testing.T) {
		s := open(t)
		runAt(t, s, "33330000-0000-0000-0000-000000000001", "110", base)

		got, err := s.LastRuns(context.Background(), []string{"110", "999"})
		if err != nil {
			t.Fatalf("LastRuns: %v", err)
		}
		if _, ok := got["999"]; ok {
			t.Errorf("workload 999 is present: %+v", got["999"])
		}
	})

	// An IN () clause is a syntax error on both engines. The empty question
	// must not reach the database at all.
	t.Run("an empty list asks nothing and answers nothing", func(t *testing.T) {
		s := open(t)
		got, err := s.LastRuns(context.Background(), nil)
		if err != nil {
			t.Fatalf("LastRuns(nil): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %+v, want an empty map", got)
		}
	})

	// Two drills can start in the same microsecond. The listing breaks that
	// tie on the id, and "the last one" has to mean the same thing here.
	t.Run("a tie on started_at is broken by id, as the listing breaks it", func(t *testing.T) {
		s := open(t)
		runAt(t, s, "44440000-0000-0000-0000-00000000000a", "110", base)
		runAt(t, s, "44440000-0000-0000-0000-00000000000b", "110", base)

		got, err := s.LastRuns(context.Background(), []string{"110"})
		if err != nil {
			t.Fatalf("LastRuns: %v", err)
		}
		if got["110"].ID != "44440000-0000-0000-0000-00000000000b" {
			t.Errorf("workload 110 = %s, want the higher id", got["110"].ID)
		}
	})

	// The summary a caller gets must be the same shape ListRuns gives, or a
	// consumer would have to know which query produced its row.
	t.Run("the summary carries the fields the listing carries", func(t *testing.T) {
		s := open(t)
		runAt(t, s, "55550000-0000-0000-0000-000000000001", "110", base)

		got, err := s.LastRuns(context.Background(), []string{"110"})
		if err != nil {
			t.Fatalf("LastRuns: %v", err)
		}
		summary := got["110"]
		if summary.SourceWorkloadID != "110" || summary.SourceName != "linux-test" {
			t.Errorf("source = %q/%q, want 110/linux-test", summary.SourceWorkloadID, summary.SourceName)
		}
		if summary.State != core.RunSuccess || summary.Result != core.ResultSuccess {
			t.Errorf("state/result = %v/%v, want SUCCESS/SUCCESS", summary.State, summary.Result)
		}
		if !summary.StartedAt.Equal(base) {
			t.Errorf("StartedAt = %v, want %v", summary.StartedAt, base)
		}
		if summary.RTO != 28*time.Second {
			t.Errorf("RTO = %v, want 28s", summary.RTO)
		}
	})
}
