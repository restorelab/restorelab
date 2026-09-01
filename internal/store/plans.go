package store

import (
	"context"
	"database/sql"
	"errors"
)

const planColumns = `id, name, description, workload_id, provider_id,
	plan_yaml, version, created_at, updated_at`

// insertPlanSQL refuses a taken name without reading the driver's error.
//
// A UNIQUE violation does not have the same shape under modernc.org/sqlite
// and under pgx, and parsing either message would be a second dialect
// difference to maintain. The guard is expressed in SQL instead, and the
// UNIQUE index stays underneath as the real defence against a race.
const insertPlanSQL = `
INSERT INTO plans (` + planColumns + `)
SELECT ?, ?, ?, ?, ?, ?, 1, ?, ?
WHERE NOT EXISTS (SELECT 1 FROM plans WHERE name = ?)`

// CreatePlan records a new plan.
func (s *sqlStore) CreatePlan(ctx context.Context, p Plan) error {
	n, err := s.execCount(ctx, insertPlanSQL,
		p.ID, p.Name, nullString(p.Description), p.WorkloadID, nullString(p.ProviderID),
		p.YAML, formatTime(p.CreatedAt), formatTime(p.UpdatedAt), p.Name)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDuplicate
	}
	return nil
}

const updatePlanSQL = `
UPDATE plans SET
	name = ?, description = ?, workload_id = ?, provider_id = ?,
	plan_yaml = ?, version = version + 1, updated_at = ?
WHERE id = ?`

const updatePlanIfVersionSQL = updatePlanSQL + ` AND version = ?`

// UpdatePlan overwrites a plan and bumps its version.
//
// The increment is done in SQL rather than computed here: two writers cannot
// then produce the same version, whatever they each read beforehand.
func (s *sqlStore) UpdatePlan(ctx context.Context, p Plan, expected int) error {
	id, err := s.resolvePlanRef(ctx, p.ID)
	if err != nil {
		return err
	}

	query, args := updatePlanSQL, []any{
		p.Name, nullString(p.Description), p.WorkloadID, nullString(p.ProviderID),
		p.YAML, formatTime(p.UpdatedAt), id,
	}
	if expected > 0 {
		query = updatePlanIfVersionSQL
		args = append(args, expected)
	}

	n, err := s.execCount(ctx, query, args...)
	if err != nil {
		return err
	}
	if n == 0 {
		// The row exists - resolvePlanRef just said so - so the only thing
		// the WHERE clause can have rejected is the version.
		return ErrVersionConflict
	}
	return nil
}

const selectPlanSQL = `SELECT ` + planColumns + ` FROM plans WHERE id = ?`

// GetPlan resolves a reference and loads the plan behind it.
func (s *sqlStore) GetPlan(ctx context.Context, ref string) (*Plan, error) {
	id, err := s.resolvePlanRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	p, err := scanPlan(s.queryRow(ctx, selectPlanSQL, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// resolvePlanRef turns a name, an id or a unique id prefix into an id.
//
// The name is tried first because it is what a human types, and because a
// name that happens to look like an id should still mean the plan carrying
// it. An id prefix works the way it does for runs, and an ambiguous one is
// refused rather than guessed.
func (s *sqlStore) resolvePlanRef(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", ErrNotFound
	}

	var id string
	err := s.queryRow(ctx, `SELECT id FROM plans WHERE name = ?`, ref).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	err = s.queryRow(ctx, `SELECT id FROM plans WHERE id = ?`, ref).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// LIMIT 2 is all it takes: one row means unique, two mean ambiguous.
	rows, err := s.query(ctx, `SELECT id FROM plans WHERE id LIKE ? || '%' ORDER BY id LIMIT 2`, ref)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return "", err
		}
		ids = append(ids, candidate)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", ErrNotFound
	case 1:
		return ids[0], nil
	default:
		return "", ErrAmbiguous
	}
}

const selectPlansPrefix = `SELECT ` + planColumns + ` FROM plans`

// ListPlans returns the catalogue, ordered by name.
//
// No cursor: a catalogue is dozens of rows on a stable ordering, and a
// keyset over that would be ceremony. The limit is here so a listing can
// never be unbounded.
func (s *sqlStore) ListPlans(ctx context.Context, f PlanFilter) ([]Plan, error) {
	query := selectPlansPrefix
	var args []any
	if f.WorkloadID != "" {
		query += " WHERE workload_id = ?"
		args = append(args, f.WorkloadID)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	query += " ORDER BY name LIMIT ?"
	args = append(args, limit)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// DeletePlan removes a plan; its runs only lose the link.
func (s *sqlStore) DeletePlan(ctx context.Context, ref string) error {
	id, err := s.resolvePlanRef(ctx, ref)
	if err != nil {
		return err
	}
	n, err := s.execCount(ctx, `DELETE FROM plans WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanPlan reads one row in planColumns order, from either a *sql.Row or a
// *sql.Rows: both expose the same Scan signature.
func scanPlan(scan func(...any) error) (*Plan, error) {
	var (
		p                       Plan
		description, providerID sql.NullString
		createdAt, updatedAt    string
	)
	if err := scan(&p.ID, &p.Name, &description, &p.WorkloadID, &providerID,
		&p.YAML, &p.Version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.Description = description.String
	p.ProviderID = providerID.String

	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}
