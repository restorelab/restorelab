package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

const selectRunsPrefix = `
SELECT id, plan_name, source_workload_id, source_name, state, result,
	started_at, completed_at, rto_ms, rto_target_ms, cleanup_done
FROM runs`

// ListRuns returns run summaries, most recent first.
//
// The WHERE clause is assembled from constant fragments and every value
// travels as a bound parameter, so no user input ever reaches the query text.
func (s *sqlStore) ListRuns(ctx context.Context, f Filter) ([]RunSummary, error) {
	var clauses []string
	var args []any

	if f.WorkloadID != "" {
		clauses = append(clauses, "source_workload_id = ?")
		args = append(args, f.WorkloadID)
	}
	if f.State != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, string(f.State))
	}
	if f.Result != "" {
		clauses = append(clauses, "result = ?")
		args = append(args, string(f.Result))
	}
	if !f.Since.IsZero() {
		// The fixed-width timestamp layout is what makes this string
		// comparison a chronological one.
		clauses = append(clauses, "started_at >= ?")
		args = append(args, formatTime(f.Since))
	}
	if f.After != nil {
		// Le tri est (started_at DESC, id DESC) ; « après » veut donc dire
		// strictement plus petit dans cet ordre lexicographique à deux
		// composantes. Le second terme n'est pas un détail : deux drills
		// peuvent démarrer dans la même microseconde, et sans l'id l'un des
		// deux disparaîtrait de la pagination.
		clauses = append(clauses, "(started_at < ? OR (started_at = ? AND id < ?))")
		at := formatTime(f.After.StartedAt)
		args = append(args, at, at, f.After.ID)
	}

	query := selectRunsPrefix
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	query += " ORDER BY started_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []RunSummary
	for rows.Next() {
		var (
			r                  RunSummary
			sourceName, result sql.NullString
			state, startedAt   string
			completedAt        sql.NullString
			rtoMS              sql.NullInt64
			rtoTargetMS        sql.NullInt64
			cleanupDone        int
		)
		if err := rows.Scan(&r.ID, &r.PlanName, &r.SourceWorkloadID, &sourceName,
			&state, &result, &startedAt, &completedAt, &rtoMS, &rtoTargetMS, &cleanupDone); err != nil {
			return nil, err
		}

		r.SourceName = sourceName.String
		r.State = core.RunState(state)
		r.Result = core.RunResult(result.String)
		r.RTO = time.Duration(rtoMS.Int64) * time.Millisecond
		r.RTOTarget = time.Duration(rtoTargetMS.Int64) * time.Millisecond
		r.CleanupDone = intToBool(cleanupDone)
		if r.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("store: run %s has an unreadable started_at: %w", r.ID, err)
		}
		if r.CompletedAt, err = parseNullTime(nullable(completedAt)); err != nil {
			return nil, fmt.Errorf("store: run %s has an unreadable completed_at: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
