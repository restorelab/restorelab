package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

const insertRunSQL = `
INSERT INTO runs (
	id, plan_name, plan_snapshot, plan_id, plan_version,
	provider_id, backup_provider_id,
	source_workload_id, source_name, temp_workload_id, temp_name, node,
	backup, state, result, started_at, completed_at,
	rto_ms, rto_target_ms, cleanup_done, err
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateRun records a run that has just started, with the plan exactly as it
// was at that moment. Plans become editable later; a report must keep saying
// what was actually checked, not what the plan says today.
//
// plan_id and plan_version are provenance beside that snapshot: they say
// which catalogue entry this drill came from, while the snapshot says what it
// actually executed. They are written here and nowhere else - UpdateRun does
// not carry them, because where a run came from cannot change afterwards.
func (s *sqlStore) CreateRun(ctx context.Context, run *core.RecoveryRun, planYAML string) error {
	backupJSON, err := encodeJSON(run.Backup)
	if err != nil {
		return fmt.Errorf("store: encode backup for run %s: %w", run.ID, err)
	}
	return s.exec(ctx, insertRunSQL,
		run.ID, run.PlanName, planYAML,
		nullString(run.PlanID), nullInt(run.PlanVersion),
		run.ProviderID, nullString(run.BackupProviderID),
		run.SourceWorkloadID, nullString(run.SourceName), nullString(run.TempWorkloadID),
		nullString(run.TempName), nullString(run.Node),
		nullString(backupJSON), string(run.State), nullString(string(run.Result)),
		formatTime(run.StartedAt), formatNullTime(run.CompletedAt),
		run.RTO.Milliseconds(), run.RTOTarget.Milliseconds(),
		boolToInt(run.CleanupDone), nullString(run.Err),
	)
}

const updateRunSQL = `
UPDATE runs SET
	source_name = ?, temp_workload_id = ?, temp_name = ?, node = ?, backup = ?,
	state = ?, result = ?, completed_at = ?,
	rto_ms = ?, rto_target_ms = ?, cleanup_done = ?, err = ?
WHERE id = ?`

// UpdateRun overwrites the fields that change as a run progresses. The id,
// the source workload and the plan snapshot are deliberately not among them.
//
// source_name is: an ad-hoc drill knows only the workload's id when it
// starts, and learns its name from the provider on the way. Leaving it out
// meant the history showed a bare "110" where the terminal had shown
// "linux-test (110)".
func (s *sqlStore) UpdateRun(ctx context.Context, run *core.RecoveryRun) error {
	backupJSON, err := encodeJSON(run.Backup)
	if err != nil {
		return fmt.Errorf("store: encode backup for run %s: %w", run.ID, err)
	}
	return s.exec(ctx, updateRunSQL,
		nullString(run.SourceName),
		nullString(run.TempWorkloadID), nullString(run.TempName), nullString(run.Node),
		nullString(backupJSON), string(run.State), nullString(string(run.Result)),
		formatNullTime(run.CompletedAt),
		run.RTO.Milliseconds(), run.RTOTarget.Milliseconds(),
		boolToInt(run.CleanupDone), nullString(run.Err),
		run.ID,
	)
}

const setTempWorkloadSQL = `
UPDATE runs SET temp_workload_id = ?, node = ? WHERE id = ?`

// SetTempWorkload records the temporary workload a run has just created, as
// soon as it exists. See the Store interface for why this writes only these
// two columns.
//
// An unknown runID is not an error: like UpdateRun, this is a best-effort
// write against a run the caller believes already exists, made from an event
// rather than a full run - there is nothing more specific to report, and
// nothing here should ever become a reason to abort a drill.
func (s *sqlStore) SetTempWorkload(ctx context.Context, runID, tempWorkloadID, node string) error {
	return s.exec(ctx, setTempWorkloadSQL, nullString(tempWorkloadID), nullString(node), runID)
}

const selectRunSQL = `
SELECT id, plan_name, plan_id, plan_version, provider_id, backup_provider_id,
	source_workload_id, source_name, temp_workload_id, temp_name, node,
	backup, state, result, started_at, completed_at,
	rto_ms, rto_target_ms, cleanup_done, err
FROM runs WHERE id = ?`

// resolveRunID turns an id or a unique prefix into a full id.
//
// Accepting a prefix is what makes `runs show 0aca8405` work; refusing an
// ambiguous one is what keeps it honest. An exact match is looked up first
// and wins outright: if someone types a whole id that also happens to be a
// prefix of another, they meant the one they typed.
func (s *sqlStore) resolveRunID(ctx context.Context, idOrPrefix string) (string, error) {
	var exact string
	err := s.queryRow(ctx, `SELECT id FROM runs WHERE id = ?`, idOrPrefix).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// LIMIT 2 is all we need: one row means unique, two means ambiguous, and
	// there is no reason to drag back a thousand.
	rows, err := s.query(ctx, `SELECT id FROM runs WHERE id LIKE ? || '%' ORDER BY id LIMIT 2`, idOrPrefix)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrNotFound, idOrPrefix)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%w: %s", ErrAmbiguous, idOrPrefix)
	}
}

// GetRun loads a run with its whole timeline.
func (s *sqlStore) GetRun(ctx context.Context, idOrPrefix string) (*core.RecoveryRun, error) {
	id, err := s.resolveRunID(ctx, idOrPrefix)
	if err != nil {
		return nil, err
	}

	var (
		run                         core.RecoveryRun
		planID                      sql.NullString
		planVersion                 sql.NullInt64
		backupProvider, sourceName  sql.NullString
		tempID, tempName, node      sql.NullString
		backupJSON, result, errText sql.NullString
		startedAt                   string
		completedAt                 sql.NullString
		rtoMS, rtoTargetMS          sql.NullInt64
		cleanupDone                 int
		state                       string
	)

	err = s.queryRow(ctx, selectRunSQL, id).Scan(
		&run.ID, &run.PlanName, &planID, &planVersion, &run.ProviderID, &backupProvider,
		&run.SourceWorkloadID, &sourceName, &tempID, &tempName, &node,
		&backupJSON, &state, &result, &startedAt, &completedAt,
		&rtoMS, &rtoTargetMS, &cleanupDone, &errText,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, idOrPrefix)
	}
	if err != nil {
		return nil, err
	}

	// Both provenance columns read back as NULL for an ad-hoc drill, and for
	// a run whose stored plan has since been deleted: ON DELETE SET NULL
	// clears the link and leaves the rest of the row exactly as it was.
	run.PlanID = planID.String
	run.PlanVersion = int(planVersion.Int64)
	run.BackupProviderID = backupProvider.String
	run.SourceName = sourceName.String
	run.TempWorkloadID = tempID.String
	run.TempName = tempName.String
	run.Node = node.String
	run.State = core.RunState(state)
	run.Result = core.RunResult(result.String)
	run.Err = errText.String
	run.CleanupDone = intToBool(cleanupDone)
	run.RTO = time.Duration(rtoMS.Int64) * time.Millisecond
	run.RTOTarget = time.Duration(rtoTargetMS.Int64) * time.Millisecond

	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, fmt.Errorf("store: run %s has an unreadable started_at: %w", id, err)
	}
	if run.CompletedAt, err = parseNullTime(nullable(completedAt)); err != nil {
		return nil, fmt.Errorf("store: run %s has an unreadable completed_at: %w", id, err)
	}
	if backupJSON.Valid && backupJSON.String != "" {
		var b core.Backup
		if err := decodeJSON([]byte(backupJSON.String), &b); err != nil {
			return nil, fmt.Errorf("store: run %s has an unreadable backup: %w", id, err)
		}
		run.Backup = &b
	}

	if run.Steps, err = s.loadSteps(ctx, id); err != nil {
		return nil, err
	}
	if run.Checks, err = s.loadChecks(ctx, id); err != nil {
		return nil, err
	}
	return &run, nil
}

// nullString maps "" to SQL NULL. An empty string and "not recorded" are the
// same thing for every column here, and NULL keeps that visible to anyone
// reading the database by hand.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullable adapts a scanned sql.NullString for parseNullTime.
func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
