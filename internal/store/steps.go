package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// ON CONFLICT DO UPDATE is accepted by SQLite 3.24+ and by PostgreSQL alike,
// so one statement serves both. A step is written twice - once running, once
// settled - and the second write must replace the first, not sit beside it.
const upsertStepSQL = `
INSERT INTO run_steps (run_id, seq, name, state, status, started_at, completed_at, duration_ms, message, err, details)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO UPDATE SET
	name = excluded.name, state = excluded.state, status = excluded.status,
	started_at = excluded.started_at, completed_at = excluded.completed_at,
	duration_ms = excluded.duration_ms, message = excluded.message,
	err = excluded.err, details = excluded.details`

func (s *sqlStore) SaveStep(ctx context.Context, runID string, seq int, step core.Step) error {
	details, err := encodeJSON(step.Details)
	if err != nil {
		return fmt.Errorf("store: encode details of step %q: %w", step.Name, err)
	}
	return s.exec(ctx, upsertStepSQL,
		runID, seq, step.Name, string(step.State), string(step.Status),
		formatNullTime(step.StartedAt), formatNullTime(step.CompletedAt),
		step.Duration.Milliseconds(), nullString(step.Message),
		nullString(step.Err), nullString(details),
	)
}

const selectStepsSQL = `
SELECT name, state, status, started_at, completed_at, duration_ms, message, err, details
FROM run_steps WHERE run_id = ? ORDER BY seq`

func (s *sqlStore) loadSteps(ctx context.Context, runID string) ([]core.Step, error) {
	rows, err := s.query(ctx, selectStepsSQL, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []core.Step
	for rows.Next() {
		var (
			step                      core.Step
			state, status             string
			startedAt, completedAt    sql.NullString
			message, errText, details sql.NullString
			durationMS                sql.NullInt64
		)
		if err := rows.Scan(&step.Name, &state, &status, &startedAt, &completedAt,
			&durationMS, &message, &errText, &details); err != nil {
			return nil, err
		}

		step.State = core.RunState(state)
		step.Status = core.StepStatus(status)
		step.Message = message.String
		step.Err = errText.String
		step.Duration = time.Duration(durationMS.Int64) * time.Millisecond
		if step.StartedAt, err = parseNullTime(nullable(startedAt)); err != nil {
			return nil, fmt.Errorf("store: step %q of run %s has an unreadable started_at: %w", step.Name, runID, err)
		}
		if step.CompletedAt, err = parseNullTime(nullable(completedAt)); err != nil {
			return nil, fmt.Errorf("store: step %q of run %s has an unreadable completed_at: %w", step.Name, runID, err)
		}
		if details.Valid && details.String != "" {
			if err := decodeJSON([]byte(details.String), &step.Details); err != nil {
				return nil, fmt.Errorf("store: step %q of run %s has unreadable details: %w", step.Name, runID, err)
			}
		}
		steps = append(steps, step)
	}
	return steps, rows.Err()
}
