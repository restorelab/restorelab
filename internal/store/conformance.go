package store

// This file holds the conformance suite: one body of tests, written against
// the Store interface, run against every engine.
//
// It lives outside _test.go so both engines' test files can call it, and it
// is the main defence against the two engines drifting apart. A behaviour
// that differs between SQLite and PostgreSQL shows up here, not in a user's
// history six months from now. When one of these fails on one engine only,
// the fix is always to correct the shared query - never to write a second one.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// OpenFunc gives the conformance suite a fresh, empty store.
type OpenFunc func(t *testing.T) Store

// sampleRun builds a run with every field populated, so a column the
// implementation forgot to write shows up as a mismatch rather than as a
// zero value nobody notices.
//
// PlanID and PlanVersion are the one exception, and deliberately so: plan_id
// is a foreign key, and both engines enforce it - foreign_keys is on in the
// SQLite DSN. A sample carrying an id no row in plans holds would fail every
// insert in this suite. A test that wants provenance creates the plan first
// and sets the two fields itself.
func sampleRun(id string) *core.RecoveryRun {
	started := time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC)
	return &core.RecoveryRun{
		ID:               id,
		PlanName:         "adhoc-110",
		ProviderID:       "proxmox-main",
		BackupProviderID: "proxmox-main",
		SourceWorkloadID: "110",
		SourceName:       "linux-test",
		TempWorkloadID:   "9001",
		TempName:         "restorelab-110-20260901",
		Node:             "pve1",
		State:            core.RunSuccess,
		Result:           core.ResultSuccess,
		StartedAt:        started,
		CompletedAt:      started.Add(28 * time.Second),
		RTO:              28 * time.Second,
		RTOTarget:        5 * time.Minute,
		CleanupDone:      true,
		ProofLevel:       core.ProofService,
	}
}

// RunConformance exercises writing and reading back a single run.
func RunConformance(t *testing.T, open OpenFunc) {
	t.Run("create then get round trips every field", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		want := sampleRun("0aca8405-4e80-4ac9-8bdd-057a56dc0281")

		// The plan has to exist before the run can point at it: plan_id is a
		// foreign key on both engines. That is the point of writing it this
		// way round - a run claiming a provenance no catalogue entry backs is
		// a state the database refuses, not one we have to check for.
		p := samplePlan("6b1d5a90-3c7e-4f11-8a02-9e4d7c6b5a03", "provenance-plan")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		want.PlanID = p.ID
		want.PlanVersion = 3

		if err := s.CreateRun(ctx, want, "name: adhoc-110\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, want.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}

		if got.ID != want.ID || got.PlanName != want.PlanName {
			t.Errorf("ID/PlanName = %q/%q, want %q/%q", got.ID, got.PlanName, want.ID, want.PlanName)
		}
		if got.PlanID != want.PlanID || got.PlanVersion != want.PlanVersion {
			t.Errorf("provenance = %q/v%d, want %q/v%d",
				got.PlanID, got.PlanVersion, want.PlanID, want.PlanVersion)
		}
		if got.ProviderID != want.ProviderID || got.BackupProviderID != want.BackupProviderID {
			t.Errorf("provider ids = %q/%q, want %q/%q",
				got.ProviderID, got.BackupProviderID, want.ProviderID, want.BackupProviderID)
		}
		if got.SourceWorkloadID != want.SourceWorkloadID || got.SourceName != want.SourceName {
			t.Errorf("source = %q/%q, want %q/%q",
				got.SourceWorkloadID, got.SourceName, want.SourceWorkloadID, want.SourceName)
		}
		if got.TempWorkloadID != want.TempWorkloadID || got.TempName != want.TempName || got.Node != want.Node {
			t.Errorf("temp workload = %q/%q on %q, want %q/%q on %q",
				got.TempWorkloadID, got.TempName, got.Node, want.TempWorkloadID, want.TempName, want.Node)
		}
		if got.State != want.State || got.Result != want.Result {
			t.Errorf("State/Result = %v/%v, want %v/%v", got.State, got.Result, want.State, want.Result)
		}
		if !got.StartedAt.Equal(want.StartedAt) {
			t.Errorf("StartedAt = %v, want %v (nanoseconds must survive)", got.StartedAt, want.StartedAt)
		}
		if !got.CompletedAt.Equal(want.CompletedAt) {
			t.Errorf("CompletedAt = %v, want %v", got.CompletedAt, want.CompletedAt)
		}
		if got.RTO != want.RTO || got.RTOTarget != want.RTOTarget {
			t.Errorf("RTO = %v (target %v), want %v (target %v)", got.RTO, got.RTOTarget, want.RTO, want.RTOTarget)
		}
		if !got.CleanupDone {
			t.Error("CleanupDone = false, want true")
		}
		if got.ProofLevel != want.ProofLevel {
			t.Errorf("ProofLevel = %q, want %q", got.ProofLevel, want.ProofLevel)
		}
	})

	// The level is what the confidence score caps on, and the score is
	// graded from summaries rather than from full runs - so a level that
	// round-trips through GetRun but not through the listing would leave
	// every workload uncapped on the one screen that shows them all.
	t.Run("the proof level survives the listing as well as the run", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("7c2e1f00-1111-4222-8333-944455556666")
		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		list, err := s.ListRuns(ctx, Filter{WorkloadID: run.SourceWorkloadID})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(list) != 1 || list[0].ProofLevel != run.ProofLevel {
			t.Fatalf("ListRuns gave %+v, want one row proving %s", list, run.ProofLevel)
		}

		last, err := s.LastRuns(ctx, []string{run.SourceWorkloadID})
		if err != nil {
			t.Fatalf("LastRuns: %v", err)
		}
		if last[run.SourceWorkloadID].ProofLevel != run.ProofLevel {
			t.Errorf("LastRuns proof level = %q, want %q",
				last[run.SourceWorkloadID].ProofLevel, run.ProofLevel)
		}
	})

	// A run written before the column existed reads back as unrecorded, not
	// as NONE. The two are different claims: the score caps on the second
	// and refuses to draw any conclusion from the first, so a store that
	// blurred them would mark down every workload in an existing install.
	t.Run("a run with no proof level reads back unrecorded", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("8d3f2011-2222-4333-8444-955566667777")
		run.ProofLevel = core.ProofUnknown
		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.ProofLevel != core.ProofUnknown {
			t.Errorf("ProofLevel = %q, want it to stay unrecorded", got.ProofLevel)
		}
		if got.ProofLevel == core.ProofNone {
			t.Error("an unrecorded level came back as NONE: that is a claim nobody made")
		}
	})

	// The level moves as the run learns things, so it has to be among the
	// fields UpdateRun overwrites - the queued row starts at NONE and the
	// worker raises it.
	t.Run("update raises the proof level", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("9e405122-3333-4444-8555-966677778888")
		run.ProofLevel = core.ProofNone
		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		run.ProofLevel = core.ProofData
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.ProofLevel != core.ProofData {
			t.Errorf("ProofLevel = %q, want DATA", got.ProofLevel)
		}
	})

	t.Run("update overwrites mutable fields", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("11111111-2222-3333-4444-555555555555")
		run.State = core.RunRestoring
		run.Result = ""
		run.CompletedAt = time.Time{}
		run.CleanupDone = false

		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		run.State = core.RunFailed
		run.Result = core.ResultFailed
		run.Err = "restore refused by the hypervisor"
		run.CompletedAt = run.StartedAt.Add(time.Minute)
		run.CleanupDone = true
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.State != core.RunFailed || got.Result != core.ResultFailed {
			t.Errorf("State/Result = %v/%v, want FAILED/FAILED", got.State, got.Result)
		}
		if got.Err != run.Err {
			t.Errorf("Err = %q, want %q", got.Err, run.Err)
		}
		if !got.CleanupDone {
			t.Error("CleanupDone = false, want true after the update")
		}
	})

	// An ad-hoc drill starts knowing only the workload id and learns the name
	// from the provider on the way. If the update does not carry it, the
	// history shows a bare id where the terminal showed a name.
	t.Run("a name discovered during the run is stored", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("7777dddd-0000-0000-0000-000000000000")
		run.SourceName = ""

		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		run.SourceName = "linux-test"
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.SourceName != "linux-test" {
			t.Errorf("SourceName = %q, want %q", got.SourceName, "linux-test")
		}

		listed, err := s.ListRuns(ctx, Filter{})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(listed) != 1 || listed[0].SourceName != "linux-test" {
			t.Errorf("the listing lost the name: %+v", listed)
		}
	})

	t.Run("the stored backup survives", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("bbbbbbbb-0000-0000-0000-000000000000")
		run.Backup = &core.Backup{
			ID:        "local:backup/vzdump-qemu-110-2026_09_01-00_52_46.vma.zst",
			SizeBytes: 353370112,
			CreatedAt: time.Date(2026, 9, 1, 0, 52, 46, 0, time.UTC),
		}

		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Backup == nil {
			t.Fatal("the backup was lost")
		}
		if got.Backup.ID != run.Backup.ID || got.Backup.SizeBytes != run.Backup.SizeBytes {
			t.Errorf("backup = %+v, want %+v", got.Backup, run.Backup)
		}
	})

	t.Run("a run with no backup reads back with none", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("cafecafe-0000-0000-0000-000000000000")
		run.Backup = nil

		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Backup != nil {
			t.Errorf("Backup = %+v, want nil", got.Backup)
		}
	})

	t.Run("a zero CompletedAt stays zero", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("99999999-8888-7777-6666-555555555555")
		run.CompletedAt = time.Time{}

		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if !got.CompletedAt.IsZero() {
			t.Errorf("CompletedAt = %v, want the zero time: an unfinished run has no end", got.CompletedAt)
		}
	})

	t.Run("get by unique prefix", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		run := sampleRun("abcdef01-0000-0000-0000-000000000000")
		if err := s.CreateRun(ctx, run, "name: x\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		got, err := s.GetRun(ctx, "abcdef")
		if err != nil {
			t.Fatalf("GetRun by prefix: %v", err)
		}
		if got.ID != run.ID {
			t.Errorf("ID = %q, want %q", got.ID, run.ID)
		}
	})

	t.Run("an ambiguous prefix is refused, not guessed", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		for _, id := range []string{
			"aaaa1111-0000-0000-0000-000000000000",
			"aaaa2222-0000-0000-0000-000000000000",
		} {
			if err := s.CreateRun(ctx, sampleRun(id), "name: x\n"); err != nil {
				t.Fatalf("CreateRun %s: %v", id, err)
			}
		}
		if _, err := s.GetRun(ctx, "aaaa"); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("GetRun error = %v, want ErrAmbiguous", err)
		}
	})

	t.Run("an exact id wins over a prefix that also matches", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		// "abc" is both a full id here and a prefix of the other.
		for _, id := range []string{"abc", "abcdef"} {
			if err := s.CreateRun(ctx, sampleRun(id), "name: x\n"); err != nil {
				t.Fatalf("CreateRun %s: %v", id, err)
			}
		}
		got, err := s.GetRun(ctx, "abc")
		if err != nil {
			t.Fatalf("GetRun: %v, want the exact match to win", err)
		}
		if got.ID != "abc" {
			t.Errorf("ID = %q, want %q", got.ID, "abc")
		}
	})

	t.Run("an unknown id reports not found", func(t *testing.T) {
		s := open(t)
		if _, err := s.GetRun(context.Background(), "nothing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetRun error = %v, want ErrNotFound", err)
		}
	})
}
