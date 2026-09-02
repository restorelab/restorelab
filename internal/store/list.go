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
SELECT id, plan_name, plan_id, source_workload_id, source_name, state, result,
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
	if f.NotTerminal {
		marks, targs := terminalList()
		clauses = append(clauses, "state NOT IN ("+marks+")")
		args = append(args, targs...)
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
		r, err := scanRunSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanRunSummary reads one row of the summary column list.
//
// It is shared by ListRuns and LastRuns so the two queries cannot disagree
// about what a summary is: they select the same columns, in the same order,
// and one function reads them.
func scanRunSummary(rows *sql.Rows) (RunSummary, error) {
	var (
		r                  RunSummary
		planID             sql.NullString
		sourceName, result sql.NullString
		state, startedAt   string
		completedAt        sql.NullString
		rtoMS              sql.NullInt64
		rtoTargetMS        sql.NullInt64
		cleanupDone        int
	)
	if err := rows.Scan(&r.ID, &r.PlanName, &planID, &r.SourceWorkloadID, &sourceName,
		&state, &result, &startedAt, &completedAt, &rtoMS, &rtoTargetMS, &cleanupDone); err != nil {
		return r, err
	}

	r.PlanID = planID.String
	r.SourceName = sourceName.String
	r.State = core.RunState(state)
	r.Result = core.RunResult(result.String)
	r.RTO = time.Duration(rtoMS.Int64) * time.Millisecond
	r.RTOTarget = time.Duration(rtoTargetMS.Int64) * time.Millisecond
	r.CleanupDone = intToBool(cleanupDone)

	var err error
	if r.StartedAt, err = parseTime(startedAt); err != nil {
		return r, fmt.Errorf("store: run %s has an unreadable started_at: %w", r.ID, err)
	}
	if r.CompletedAt, err = parseNullTime(nullable(completedAt)); err != nil {
		return r, fmt.Errorf("store: run %s has an unreadable completed_at: %w", r.ID, err)
	}
	return r, nil
}

// lastRunsSQL keeps only the newest run of each workload.
//
// ROW_NUMBER is the one shape both engines take: SQLite has had window
// functions since 3.25 and PostgreSQL far longer. The ordering repeats
// ListRuns' (started_at DESC, id DESC) on purpose - two drills of the same
// workload can start in the same microsecond, and "the last one" has to mean
// the same thing in both queries.
//
// The %s is a list of "?" placeholders, built from the id count and nothing
// else. No caller value ever reaches the query text.
const lastRunsSQL = `
SELECT id, plan_name, plan_id, source_workload_id, source_name, state, result,
	started_at, completed_at, rto_ms, rto_target_ms, cleanup_done
FROM (
	SELECT id, plan_name, plan_id, source_workload_id, source_name, state, result,
		started_at, completed_at, rto_ms, rto_target_ms, cleanup_done,
		ROW_NUMBER() OVER (
			PARTITION BY source_workload_id
			ORDER BY started_at DESC, id DESC
		) AS rn
	FROM runs
	WHERE source_workload_id IN (%s)
) ranked
WHERE rn = 1`

// LastRuns returns each workload's most recent run.
func (s *sqlStore) LastRuns(ctx context.Context, workloadIDs []string) (map[string]RunSummary, error) {
	out := map[string]RunSummary{}
	// No question, no query: "IN ()" is a syntax error on both engines, and
	// an empty map is the right answer anyway.
	if len(workloadIDs) == 0 {
		return out, nil
	}

	marks := make([]string, len(workloadIDs))
	args := make([]any, len(workloadIDs))
	for i, id := range workloadIDs {
		marks[i] = "?"
		args[i] = id
	}

	rows, err := s.query(ctx, fmt.Sprintf(lastRunsSQL, strings.Join(marks, ", ")), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		r, err := scanRunSummary(rows)
		if err != nil {
			return nil, err
		}
		out[r.SourceWorkloadID] = r
	}
	return out, rows.Err()
}
