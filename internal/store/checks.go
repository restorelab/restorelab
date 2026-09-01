package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

const upsertCheckSQL = `
INSERT INTO run_checks (run_id, seq, name, type, status, started_at, completed_at, duration_ms, attempts, message, details)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO UPDATE SET
	name = excluded.name, type = excluded.type, status = excluded.status,
	started_at = excluded.started_at, completed_at = excluded.completed_at,
	duration_ms = excluded.duration_ms, attempts = excluded.attempts,
	message = excluded.message, details = excluded.details`

func (s *sqlStore) SaveCheck(ctx context.Context, runID string, seq int, check core.CheckResult) error {
	details, err := encodeJSON(check.Details)
	if err != nil {
		return fmt.Errorf("store: encode details of check %q: %w", check.Name, err)
	}
	return s.exec(ctx, upsertCheckSQL,
		runID, seq, check.Name, check.Type, string(check.Status),
		formatNullTime(check.StartedAt), formatNullTime(check.CompletedAt),
		check.Duration.Milliseconds(), check.Attempts,
		nullString(check.Message), nullString(details),
	)
}

const selectChecksSQL = `
SELECT name, type, status, started_at, completed_at, duration_ms, attempts, message, details
FROM run_checks WHERE run_id = ? ORDER BY seq`

func (s *sqlStore) loadChecks(ctx context.Context, runID string) ([]core.CheckResult, error) {
	rows, err := s.query(ctx, selectChecksSQL, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var checks []core.CheckResult
	for rows.Next() {
		var (
			check                  core.CheckResult
			status                 string
			startedAt, completedAt sql.NullString
			message, details       sql.NullString
			durationMS, attempts   sql.NullInt64
		)
		if err := rows.Scan(&check.Name, &check.Type, &status, &startedAt, &completedAt,
			&durationMS, &attempts, &message, &details); err != nil {
			return nil, err
		}

		check.Status = core.CheckStatus(status)
		check.Message = message.String
		check.Attempts = int(attempts.Int64)
		check.Duration = time.Duration(durationMS.Int64) * time.Millisecond
		if check.StartedAt, err = parseNullTime(nullable(startedAt)); err != nil {
			return nil, fmt.Errorf("store: check %q of run %s has an unreadable started_at: %w", check.Name, runID, err)
		}
		if check.CompletedAt, err = parseNullTime(nullable(completedAt)); err != nil {
			return nil, fmt.Errorf("store: check %q of run %s has an unreadable completed_at: %w", check.Name, runID, err)
		}
		if details.Valid && details.String != "" {
			if err := decodeJSON([]byte(details.String), &check.Details); err != nil {
				return nil, fmt.Errorf("store: check %q of run %s has unreadable details: %w", check.Name, runID, err)
			}
		}
		checks = append(checks, check)
	}
	return checks, rows.Err()
}
