package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/catalog"
	"github.com/restorelab/restorelab/internal/store"
)

const validYAML = `# the web tier
name: web-tier
description: nightly drill
workload:
  provider: proxmox-main
  id: "110"
checks:
  - type: tcp
    port: 22
`

// memStore is the slice of the store the catalogue uses, backed by a map.
type memStore struct {
	byID  map[string]store.Plan
	calls int
}

func newMemStore() *memStore { return &memStore{byID: map[string]store.Plan{}} }

func (m *memStore) CreatePlan(_ context.Context, p store.Plan) error {
	m.calls++
	for _, existing := range m.byID {
		if existing.Name == p.Name {
			return store.ErrDuplicate
		}
	}
	m.byID[p.ID] = p
	return nil
}

func (m *memStore) UpdatePlan(_ context.Context, p store.Plan, expected int) error {
	m.calls++
	current, ok := m.byID[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	if expected > 0 && current.Version != expected {
		return store.ErrVersionConflict
	}
	p.Version = current.Version + 1
	p.CreatedAt = current.CreatedAt
	m.byID[p.ID] = p
	return nil
}

func (m *memStore) GetPlan(_ context.Context, ref string) (*store.Plan, error) {
	for _, p := range m.byID {
		if p.Name == ref || p.ID == ref {
			found := p
			return &found, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memStore) ListPlans(_ context.Context, _ store.PlanFilter) ([]store.Plan, error) {
	var out []store.Plan
	for _, p := range m.byID {
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) DeletePlan(_ context.Context, ref string) error {
	for id, p := range m.byID {
		if p.Name == ref || p.ID == ref {
			delete(m.byID, id)
			return nil
		}
	}
	return store.ErrNotFound
}

func TestSaveCreatesAndDerivesColumns(t *testing.T) {
	s := newMemStore()
	got, created, err := catalog.Save(context.Background(), s, []byte(validYAML), 0)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !created {
		t.Error("Save reported an update; nothing was there to update")
	}
	if got.Name != "web-tier" {
		t.Errorf("Name = %q, want web-tier", got.Name)
	}
	if got.WorkloadID != "110" || got.ProviderID != "proxmox-main" {
		t.Errorf("derived = %q/%q, want 110/proxmox-main", got.WorkloadID, got.ProviderID)
	}
	if got.Description != "nightly drill" {
		t.Errorf("Description = %q, want %q", got.Description, "nightly drill")
	}
	if got.YAML != validYAML {
		t.Errorf("YAML was rewritten:\n%s", got.YAML)
	}
	if !strings.Contains(got.YAML, "# the web tier") {
		t.Error("the comment did not survive: the document must be stored verbatim")
	}
	if got.ID == "" {
		t.Error("a stored plan needs an id")
	}
}

func TestSaveUpdatesWhenTheNameExists(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	if _, _, err := catalog.Save(ctx, s, []byte(validYAML), 0); err != nil {
		t.Fatalf("Save: %v", err)
	}

	changed := strings.Replace(validYAML, `id: "110"`, `id: "104"`, 1)
	got, created, err := catalog.Save(ctx, s, []byte(changed), 0)
	if err != nil {
		t.Fatalf("Save (update): %v", err)
	}
	if created {
		t.Error("Save reported a creation over a name that already existed")
	}
	if got.Version != 2 {
		t.Errorf("Version = %d, want 2", got.Version)
	}
	if got.WorkloadID != "104" {
		t.Errorf("WorkloadID = %q, want 104: the derived columns must follow the text", got.WorkloadID)
	}
}

func TestSaveRefusesAnInvalidPlanWithoutWriting(t *testing.T) {
	s := newMemStore()
	_, _, err := catalog.Save(context.Background(), s, []byte("name: broken\nworkload:\n  id: \"\"\n"), 0)
	if err == nil {
		t.Fatal("Save accepted a plan with no workload id")
	}
	if !errors.Is(err, catalog.ErrInvalid) {
		t.Errorf("Save = %v, want it to wrap ErrInvalid", err)
	}
	if s.calls != 0 {
		t.Errorf("the store was written %d times; an invalid plan must never reach it", s.calls)
	}
}

func TestSaveRefusesUnknownFields(t *testing.T) {
	s := newMemStore()
	_, _, err := catalog.Save(context.Background(), s, []byte(validYAML+"typo_here: 3\n"), 0)
	if !errors.Is(err, catalog.ErrInvalid) {
		t.Errorf("Save with an unknown field = %v, want ErrInvalid", err)
	}
}

func TestSavePropagatesAVersionConflict(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	if _, _, err := catalog.Save(ctx, s, []byte(validYAML), 0); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, err := catalog.Save(ctx, s, []byte(validYAML), 1); err != nil {
		t.Fatalf("Save (expected 1): %v", err)
	}
	if _, _, err := catalog.Save(ctx, s, []byte(validYAML), 1); !errors.Is(err, store.ErrVersionConflict) {
		t.Errorf("Save (stale expected) = %v, want ErrVersionConflict", err)
	}
}

// CreateOnly is what a POST carries: a name that already exists is a
// conflict, never a quiet replacement of somebody else's plan.
func TestSaveWithCreateOnlyRefusesATakenName(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	if _, _, err := catalog.Save(ctx, s, []byte(validYAML), catalog.CreateOnly); err != nil {
		t.Fatalf("Save (create only): %v", err)
	}
	_, _, err := catalog.Save(ctx, s, []byte(validYAML), catalog.CreateOnly)
	if !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("Save (create only, taken name) = %v, want ErrDuplicate", err)
	}
}

func TestResolveReturnsBothTheRowAndTheParsedPlan(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	if _, _, err := catalog.Save(ctx, s, []byte(validYAML), 0); err != nil {
		t.Fatalf("Save: %v", err)
	}

	row, parsed, err := catalog.Resolve(ctx, s, "web-tier")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if row.Name != "web-tier" {
		t.Errorf("row.Name = %q", row.Name)
	}
	if parsed.Workload.ID != "110" || len(parsed.Checks) != 1 {
		t.Errorf("parsed plan = %+v, want the workload and its one check", parsed)
	}
	// Defaults must have been applied: that is what makes the parsed plan
	// executable rather than merely readable.
	if parsed.Restore.Network != "isolated" {
		t.Errorf("Restore.Network = %q, want the isolated default", parsed.Restore.Network)
	}
	if parsed.Restore.Timeout == 0 {
		t.Error("Restore.Timeout is zero: ApplyDefaults did not run")
	}
}

func TestResolveReportsAStoredPlanThatNoLongerParses(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	s.byID["id"] = store.Plan{
		ID: "id", Name: "rotten", WorkloadID: "110",
		YAML: "name: rotten\nnot_a_field: 1\n", Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if _, _, err := catalog.Resolve(ctx, s, "rotten"); !errors.Is(err, catalog.ErrInvalid) {
		t.Errorf("Resolve on a rotten plan = %v, want ErrInvalid", err)
	}
}

func TestGetAndDeletePassErrNotFoundThrough(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	if _, err := catalog.Get(ctx, s, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if _, err := catalog.Delete(ctx, s, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
}

// Delete names what it removed, so a deletion made through an id prefix can
// be confirmed by something an operator recognises.
func TestDeleteReportsTheNameItRemoved(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	saved, _, err := catalog.Save(ctx, s, []byte(validYAML), 0)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	name, err := catalog.Delete(ctx, s, saved.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if name != "web-tier" {
		t.Errorf("Delete reported %q, want web-tier", name)
	}
	if _, err := catalog.Get(ctx, s, "web-tier"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
}
