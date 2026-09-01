package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/restorelab/restorelab/internal/core"
)

// DO NOTHING rather than DO UPDATE: an event is a fact that happened at a
// point in time. Writing the same seq twice means a retry, and the first
// write is the one that recorded the truth.
const insertEventSQL = `
INSERT INTO run_events (run_id, seq, at, state, step, status, message, check_result, err)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO NOTHING`

// AppendEvent records one line of the progress stream.
//
// ev.Seq is assigned by the caller, not by a database sequence: the order
// must be the engine's emission order, not the order writes happened to land
// - two things that diverge the moment a write is retried.
func (s *sqlStore) AppendEvent(ctx context.Context, runID string, ev Event) error {
	checkJSON, err := encodeJSON(ev.Check)
	if err != nil {
		return fmt.Errorf("store: encode check of event %d: %w", ev.Seq, err)
	}
	return s.exec(ctx, insertEventSQL,
		runID, ev.Seq, formatTime(ev.At), string(ev.State),
		nullString(ev.Step), nullString(string(ev.Status)),
		nullString(ev.Message), nullString(checkJSON), nullString(ev.Err),
	)
}

const selectEventsSQL = `
SELECT seq, at, state, step, status, message, check_result, err
FROM run_events WHERE run_id = ? AND seq > ? ORDER BY seq`

// Events returns a run's events after afterSeq, in order. Phase B's SSE
// replays from here when a browser reconnects mid-drill.
func (s *sqlStore) Events(ctx context.Context, runID string, afterSeq int64) ([]Event, error) {
	rows, err := s.query(ctx, selectEventsSQL, runID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			ev                 Event
			at, state          string
			step, status       sql.NullString
			message, checkJSON sql.NullString
			errText            sql.NullString
		)
		if err := rows.Scan(&ev.Seq, &at, &state, &step, &status, &message, &checkJSON, &errText); err != nil {
			return nil, err
		}

		ev.State = core.RunState(state)
		ev.Step = step.String
		ev.Status = core.StepStatus(status.String)
		ev.Message = message.String
		ev.Err = errText.String
		if ev.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("store: event %d of run %s has an unreadable timestamp: %w", ev.Seq, runID, err)
		}
		if checkJSON.Valid && checkJSON.String != "" {
			var c core.CheckResult
			if err := decodeJSON([]byte(checkJSON.String), &c); err != nil {
				return nil, fmt.Errorf("store: event %d of run %s has an unreadable check: %w", ev.Seq, runID, err)
			}
			ev.Check = &c
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}
