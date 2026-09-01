package store

// The stored-plan half of the conformance suite. Same rule as the rest of it:
// one body of tests, played against both engines. A query that passes on one
// side and fails on the other is fixed in the shared query, never by writing a
// second one.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// samplePlan builds a stored plan with every field populated, so a column the
// implementation forgot to write shows up as a mismatch rather than as a zero
// value nobody notices.
func samplePlan(id, name string) Plan {
	at := time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC)
	return Plan{
		ID:          id,
		Name:        name,
		Description: "the web tier, restored nightly",
		WorkloadID:  "110",
		ProviderID:  "proxmox-main",
		YAML:        "# written by a human\nname: " + name + "\n",
		Version:     1,
		CreatedAt:   at,
		UpdatedAt:   at,
	}
}

// PlanConformance exercises the stored-plan CRUD against one engine.
func PlanConformance(t *testing.T, open OpenFunc) {
	t.Run("create then get round trips every field", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		want := samplePlan("2f1a4c76-0b1e-4d2a-9a51-1d0f8c2b3e44", "web-tier")

		if err := s.CreatePlan(ctx, want); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		got, err := s.GetPlan(ctx, "web-tier")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if got.ID != want.ID || got.Name != want.Name || got.Description != want.Description {
			t.Errorf("identity = %q/%q/%q, want %q/%q/%q",
				got.ID, got.Name, got.Description, want.ID, want.Name, want.Description)
		}
		if got.WorkloadID != want.WorkloadID || got.ProviderID != want.ProviderID {
			t.Errorf("derived = %q/%q, want %q/%q",
				got.WorkloadID, got.ProviderID, want.WorkloadID, want.ProviderID)
		}
		if got.YAML != want.YAML {
			t.Errorf("YAML = %q, want %q (the text is stored verbatim)", got.YAML, want.YAML)
		}
		if got.Version != 1 {
			t.Errorf("Version = %d, want 1", got.Version)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Errorf("timestamps = %v/%v, want %v/%v",
				got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
		}
	})

	t.Run("a duplicate name is refused", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		if err := s.CreatePlan(ctx, samplePlan("id-one", "web-tier")); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		err := s.CreatePlan(ctx, samplePlan("id-two", "web-tier"))
		if !errors.Is(err, ErrDuplicate) {
			t.Fatalf("CreatePlan with a taken name = %v, want ErrDuplicate", err)
		}
	})

	t.Run("a reference resolves by name, id, then id prefix", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("abcdef01-0000-4000-8000-000000000001", "web-tier")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		for _, ref := range []string{"web-tier", p.ID, "abcdef01"} {
			got, err := s.GetPlan(ctx, ref)
			if err != nil {
				t.Fatalf("GetPlan(%q): %v", ref, err)
			}
			if got.ID != p.ID {
				t.Errorf("GetPlan(%q).ID = %q, want %q", ref, got.ID, p.ID)
			}
		}
		if _, err := s.GetPlan(ctx, "nothing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetPlan(unknown) = %v, want ErrNotFound", err)
		}
	})

	t.Run("an ambiguous id prefix is refused", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		if err := s.CreatePlan(ctx, samplePlan("dup00001-aaaa", "one")); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		if err := s.CreatePlan(ctx, samplePlan("dup00001-bbbb", "two")); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		if _, err := s.GetPlan(ctx, "dup00001"); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("GetPlan(ambiguous prefix) = %v, want ErrAmbiguous", err)
		}
	})

	t.Run("update bumps the version and rewrites the derived columns", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("id-update", "web-tier")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		p.WorkloadID = "104"
		p.YAML = "name: web-tier\nworkload:\n  id: \"104\"\n"
		p.UpdatedAt = p.CreatedAt.Add(time.Hour)
		if err := s.UpdatePlan(ctx, p, 0); err != nil {
			t.Fatalf("UpdatePlan: %v", err)
		}

		got, err := s.GetPlan(ctx, "web-tier")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if got.Version != 2 {
			t.Errorf("Version = %d, want 2", got.Version)
		}
		if got.WorkloadID != "104" {
			t.Errorf("WorkloadID = %q, want %q", got.WorkloadID, "104")
		}
		if !got.CreatedAt.Equal(p.CreatedAt) {
			t.Errorf("CreatedAt = %v, want it unchanged at %v", got.CreatedAt, p.CreatedAt)
		}
	})

	t.Run("a stale expected version is refused", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("id-conflict", "web-tier")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		if err := s.UpdatePlan(ctx, p, 1); err != nil {
			t.Fatalf("UpdatePlan(expected 1): %v", err)
		}
		err := s.UpdatePlan(ctx, p, 1)
		if !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("UpdatePlan(stale expected) = %v, want ErrVersionConflict", err)
		}
		if err := s.UpdatePlan(ctx, samplePlan("id-missing", "gone"), 1); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdatePlan(unknown plan) = %v, want ErrNotFound", err)
		}
	})

	t.Run("listing is ordered by name and filtered by workload", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		for _, spec := range []struct{ id, name, workload string }{
			{"id-c", "c-plan", "110"},
			{"id-a", "a-plan", "104"},
			{"id-b", "b-plan", "110"},
		} {
			p := samplePlan(spec.id, spec.name)
			p.WorkloadID = spec.workload
			if err := s.CreatePlan(ctx, p); err != nil {
				t.Fatalf("CreatePlan(%s): %v", spec.name, err)
			}
		}

		all, err := s.ListPlans(ctx, PlanFilter{})
		if err != nil {
			t.Fatalf("ListPlans: %v", err)
		}
		if len(all) != 3 || all[0].Name != "a-plan" || all[2].Name != "c-plan" {
			t.Fatalf("ListPlans = %v, want a-plan, b-plan, c-plan", names(all))
		}

		some, err := s.ListPlans(ctx, PlanFilter{WorkloadID: "110"})
		if err != nil {
			t.Fatalf("ListPlans(workload): %v", err)
		}
		if len(some) != 2 || some[0].Name != "b-plan" || some[1].Name != "c-plan" {
			t.Fatalf("ListPlans(110) = %v, want b-plan, c-plan", names(some))
		}
	})

	t.Run("deleting a plan unlinks its runs and keeps them whole", func(t *testing.T) {
		s := open(t)
		ctx := context.Background()
		p := samplePlan("id-delete", "web-tier")
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		run := sampleRun("0aca8405-4e80-4ac9-8bdd-057a56dc0281")
		run.PlanID = p.ID
		run.PlanVersion = 1
		if err := s.CreateRun(ctx, run, "name: web-tier\n"); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		if err := s.DeletePlan(ctx, "web-tier"); err != nil {
			t.Fatalf("DeletePlan: %v", err)
		}
		if _, err := s.GetPlan(ctx, "web-tier"); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetPlan after delete = %v, want ErrNotFound", err)
		}

		got, err := s.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun after deleting its plan: %v", err)
		}
		if got.PlanID != "" {
			t.Errorf("PlanID = %q, want it cleared by ON DELETE SET NULL", got.PlanID)
		}
		if got.PlanName != run.PlanName || got.State != run.State {
			t.Errorf("the run changed beyond its plan link: %q/%v", got.PlanName, got.State)
		}

		if err := s.DeletePlan(ctx, "web-tier"); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeletePlan(twice) = %v, want ErrNotFound", err)
		}
	})
}

// names renders a listing for a failure message.
func names(plans []Plan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Name)
	}
	return out
}
