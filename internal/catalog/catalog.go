// Package catalog is the stored-plan CRUD: it validates a plan document with
// internal/plan, derives the columns a listing needs, and hands the result to
// internal/store.
//
// It exists because the two ends refuse the job. store must not parse YAML -
// validation inside the persistence layer is the mixing this codebase has
// avoided throughout - and plan must not know about a database, which is what
// keeps it usable everywhere. The API and the CLI both write plans; without
// this package that logic would exist twice, and the two copies would drift.
// It is internal/adhoc's argument, made again: a plan written over HTTP and a
// plan written from a terminal must be the same plan.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/restorelab/restorelab/internal/plan"
	"github.com/restorelab/restorelab/internal/store"
)

// ErrInvalid wraps every refusal that comes from the document itself: bad
// YAML, an unknown field, or a plan Validate rejects.
//
// It is distinct from the store's errors on purpose. The API turns this into
// a 400 (the caller sent something wrong) and a store error into a 4xx or a
// 503 about our own state; conflating them would send someone hunting for a
// database problem that does not exist.
var ErrInvalid = errors.New("catalog: the plan is not valid")

// Store is the slice of store.Store the catalogue uses. Declared here so the
// tests can drive a map and so no caller can reach a run through it.
type Store interface {
	CreatePlan(ctx context.Context, p store.Plan) error
	UpdatePlan(ctx context.Context, p store.Plan, expected int) error
	GetPlan(ctx context.Context, ref string) (*store.Plan, error)
	ListPlans(ctx context.Context, f store.PlanFilter) ([]store.Plan, error)
	DeletePlan(ctx context.Context, ref string) error
}

// CreateOnly asks Save to refuse a name that already exists rather than
// replace what carries it. It is what a POST means, as opposed to a PUT.
const CreateOnly = -1

// Validated is what a plan document means, without it being stored.
type Validated struct {
	Name        string
	Description string
	WorkloadID  string
	ProviderID  string
	// Normalised is the document with every default applied, rendered back
	// as YAML. It is what an editor shows as "here is what this actually
	// says" - the difference between a plan that omits a field and a plan
	// whose omitted field means something.
	Normalised string

	// ProofLevel is what this plan would establish if every one of its checks
	// passed, and ProofSummary says it in a sentence. They are here so an
	// editor can answer "what will this actually prove" while the plan is
	// still being written - which is the only moment improving it is free.
	ProofLevel   string
	ProofSummary string
}

// derive reads the indexed facts out of a parsed plan.
//
// Save and Validate both need them, and a second copy of this list would
// drift from the first the day a column is added - the failure this codebase
// has already met once, in adhocFields.
func derive(parsed *plan.Plan) Validated {
	return Validated{
		Name:         parsed.Name,
		Description:  parsed.Description,
		WorkloadID:   parsed.Workload.ID,
		ProviderID:   parsed.Workload.Provider,
		ProofLevel:   string(parsed.ProvenLevel()),
		ProofSummary: parsed.ProofSummary(),
	}
}

// Validate parses a plan document and reports what it means, writing nothing.
//
// It exists for an editor that has to answer "is this valid" before an
// operator commits to saving. Going through the same parse Save uses is the
// point: internal/plan stays the only definition of a valid plan, and a
// client never has to reimplement one.
//
// Nothing is written. No row, no reserved name, no version.
func Validate(document []byte) (*Validated, error) {
	parsed, err := parse(document)
	if err != nil {
		return nil, err
	}

	normalised, err := yaml.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("catalog: render the validated plan: %w", err)
	}

	v := derive(parsed)
	v.Normalised = string(normalised)
	return &v, nil
}

// Save writes a plan document, creating it or replacing the one that already
// carries its name. It reports whether the plan was created.
//
// expected is the version the caller believes is current: 0 means "whatever
// is there", CreateOnly means "there must be nothing there". The document is
// validated before anything is written, so a document that cannot become a
// plan is never a row somebody has to explain later. It is stored verbatim:
// comments and key order survive, and the plan each run actually executed is
// kept separately, in that run's snapshot.
func Save(ctx context.Context, s Store, document []byte, expected int) (*store.Plan, bool, error) {
	parsed, err := parse(document)
	if err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	d := derive(parsed)
	row := store.Plan{
		ID:          uuid.NewString(),
		Name:        d.Name,
		Description: d.Description,
		WorkloadID:  d.WorkloadID,
		ProviderID:  d.ProviderID,
		YAML:        string(document),
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	existing, err := s.GetPlan(ctx, parsed.Name)
	created := false
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := s.CreatePlan(ctx, row); err != nil {
			return nil, false, err
		}
		created = true
	case err != nil:
		return nil, false, err
	case expected == CreateOnly:
		return nil, false, store.ErrDuplicate
	default:
		row.ID = existing.ID
		guard := expected
		if guard < 0 {
			guard = 0
		}
		if err := s.UpdatePlan(ctx, row, guard); err != nil {
			return nil, false, err
		}
	}

	// Read the row back rather than describe it from here: the store owns the
	// version, and a value assembled from what we hoped it would do is how a
	// catalogue starts lying about its own writes.
	saved, err := s.GetPlan(ctx, row.ID)
	if err != nil {
		return nil, false, err
	}
	return saved, created, nil
}

// Name reports the name a plan document carries, writing nothing.
//
// It exists for the one caller that has to decide before it writes: a PUT
// carries both a URL naming the plan to replace and a document naming a
// plan, and those two can disagree. Finding that out from Save would mean
// finding it out after Save has already created the second plan - the
// rename nobody asked for, done and then refused.
func Name(document []byte) (string, error) {
	parsed, err := parse(document)
	if err != nil {
		return "", err
	}
	return parsed.Name, nil
}

// Get returns one stored plan by name, id or unique id prefix.
func Get(ctx context.Context, s Store, ref string) (*store.Plan, error) {
	return s.GetPlan(ctx, ref)
}

// Resolve returns a stored plan together with the executable plan its
// document describes, defaults applied and validated.
//
// A stored plan can stop parsing between two releases - a field removed, a
// value no longer accepted - and the caller needs to hear that as a problem
// with the plan, not as a mysterious failure halfway through a drill.
func Resolve(ctx context.Context, s Store, ref string) (*store.Plan, *plan.Plan, error) {
	row, err := s.GetPlan(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := parse([]byte(row.YAML))
	if err != nil {
		return nil, nil, fmt.Errorf("stored plan %q: %w", row.Name, err)
	}
	return row, parsed, nil
}

// List returns the catalogue.
func List(ctx context.Context, s Store, f store.PlanFilter) ([]store.Plan, error) {
	return s.ListPlans(ctx, f)
}

// Delete removes a plan and reports the name of what it removed. Its runs
// keep their own name and their snapshot; only the link goes.
//
// The name is worth the extra read. A reference can be an id prefix, so
// "restorelab plan delete abcd1234" would otherwise confirm the deletion of
// "abcd1234" - an operator would have no way to see, from the confirmation
// alone, which plan they just lost. Resolving first also means the deletion
// itself is by id: whatever the prefix matched is what goes, and a plan
// created between the two calls cannot make the prefix mean something else.
func Delete(ctx context.Context, s Store, ref string) (string, error) {
	row, err := s.GetPlan(ctx, ref)
	if err != nil {
		return "", err
	}
	if err := s.DeletePlan(ctx, row.ID); err != nil {
		return "", err
	}
	return row.Name, nil
}

// parse is plan.Parse with the refusal marked, so callers can tell a bad
// document from a bad database.
//
// Both errors are wrapped, not just the sentinel: the caller branches on
// ErrInvalid to pick a status code, but whoever reads the message still needs
// the parser's own words - which field, which line - and a caller that wants
// to inspect that error rather than print it keeps the ability to.
func parse(document []byte) (*plan.Plan, error) {
	p, err := plan.Parse(document)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return p, nil
}
