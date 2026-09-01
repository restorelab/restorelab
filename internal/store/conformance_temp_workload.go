package store

// The conformance suite for SetTempWorkload: the write that happens the
// moment a temporary workload exists on the cluster, well before the run is
// done. It lives outside a _test.go so both engines' test files can call it,
// same as the rest of the conformance suite.

import (
	"context"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// TempWorkloadConformance exercises SetTempWorkload.
func TempWorkloadConformance(t *testing.T, open OpenFunc) {
	t.Run("write then read", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := newConformanceRun(t, s, "aaaabbbb-0000-0000-0000-000000000000")

		if err := s.SetTempWorkload(ctx, run.ID, "9099", "private-other"); err != nil {
			t.Fatalf("SetTempWorkload: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.TempWorkloadID != "9099" || got.Node != "private-other" {
			t.Errorf("TempWorkloadID/Node = %q/%q, want %q/%q",
				got.TempWorkloadID, got.Node, "9099", "private-other")
		}
	})

	// The test that matters: a run that dies right after this write must
	// still show its original state, result and started_at intact, and a
	// later UpdateRun must not have lost what SetTempWorkload recorded.
	t.Run("it writes only its two columns, and survives the update that follows", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("aaaabbbb-1111-0000-0000-000000000000")
		run.State = core.RunRestoring
		run.Result = ""
		run.TempWorkloadID = ""
		run.Node = ""
		run.CompletedAt = time.Time{}

		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		if err := s.SetTempWorkload(ctx, run.ID, "9100", "pve1"); err != nil {
			t.Fatalf("SetTempWorkload: %v", err)
		}

		afterSet, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun after SetTempWorkload: %v", err)
		}
		if afterSet.State != core.RunRestoring {
			t.Errorf("State = %v, want %v: SetTempWorkload must not touch it", afterSet.State, core.RunRestoring)
		}
		if afterSet.Result != "" {
			t.Errorf("Result = %q, want empty: SetTempWorkload must not touch it", afterSet.Result)
		}
		if !afterSet.StartedAt.Equal(run.StartedAt) {
			t.Errorf("StartedAt = %v, want %v: SetTempWorkload must not touch it", afterSet.StartedAt, run.StartedAt)
		}
		if afterSet.TempWorkloadID != "9100" || afterSet.Node != "pve1" {
			t.Errorf("TempWorkloadID/Node = %q/%q, want %q/%q",
				afterSet.TempWorkloadID, afterSet.Node, "9100", "pve1")
		}

		// The run finishes; UpdateRun writes the whole mutable row from the
		// in-memory run, which by now also carries the temp workload.
		run.State = core.RunSuccess
		run.Result = core.ResultSuccess
		run.TempWorkloadID = "9100"
		run.Node = "pve1"
		run.CompletedAt = run.StartedAt.Add(30 * time.Second)
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun after UpdateRun: %v", err)
		}
		if got.TempWorkloadID != "9100" || got.Node != "pve1" {
			t.Errorf("TempWorkloadID/Node did not survive UpdateRun: got %q/%q, want %q/%q",
				got.TempWorkloadID, got.Node, "9100", "pve1")
		}
		if got.State != core.RunSuccess || got.Result != core.ResultSuccess {
			t.Errorf("State/Result = %v/%v, want SUCCESS/SUCCESS", got.State, got.Result)
		}
	})

	// An unknown run id is not an error: SetTempWorkload is a best-effort
	// write like UpdateRun, not a user-facing lookup like RevokeToken. The
	// caller only has an event, no way to tell "the run never existed" from
	// "the run existed and this call is racing a slow CreateRun", and it must
	// never turn a database hiccup into a reason to abort a drill.
	t.Run("an unknown run id is not an error", func(t *testing.T) {
		s := open(t)
		err := s.SetTempWorkload(context.Background(), "does-not-exist", "9100", "pve1")
		if err != nil {
			t.Fatalf("SetTempWorkload on an unknown run id = %v, want nil", err)
		}
	})
}
