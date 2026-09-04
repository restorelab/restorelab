package store

// The captured-value half of the store: what a drill measured, and the
// history the next drill is compared against.
//
// Nothing here diverges per engine. Every statement goes through
// s.exec/s.query so rebind handles the placeholders, and the one type the
// schema names that the rest of the package does not use, double precision,
// is spelled the same way on both.

import (
	"context"
	"fmt"
)

// upsertCheckValueSQL records one reading, replacing any previous reading of
// the same name by the same check of the same run.
//
// The replacement is the point rather than a convenience. A check that
// retries runs its command again, and two rows for one drill would put the
// same night into the drift window twice, moving a median that is supposed to
// describe five separate nights.
//
// It is upsertCheckSQL's shape, VALUES with an ON CONFLICT, and that avoids
// the trap insertDeliverySQL had to work around: a bare parameter in a SELECT
// list carries no type and resolves to text on PostgreSQL, which then cannot
// be assigned to a double precision column. In a VALUES list the parameter
// takes the target column's type, so no CAST is needed here.
const upsertCheckValueSQL = `
INSERT INTO check_values (run_id, check_seq, name, value)
VALUES (?, ?, ?, ?)
ON CONFLICT (run_id, check_seq, name) DO UPDATE SET value = excluded.value`

// SaveCheckValue records one number a check read out of the restored
// workload.
func (s *sqlStore) SaveCheckValue(ctx context.Context, runID string, checkSeq int,
	name string, value float64) error {
	if err := s.exec(ctx, upsertCheckValueSQL, runID, checkSeq, name, value); err != nil {
		return fmt.Errorf("store: record value %q of check %d of run %s: %w",
			name, checkSeq, runID, err)
	}
	return nil
}

// capturedValuesSQL reads a workload's history of one capture, newest first.
//
// The join through runs is what keeps the workload off check_values: a value
// belongs to a check of a run, and the run already knows whose it is. The
// join through run_checks is what turns a check_seq into the name a plan
// wrote, so renumbering the checks of a plan does not silently repoint a
// history at a different measurement.
//
// "Reached a verdict" is the pair of conditions
//
//	result IS NOT NULL AND result <> ''
//
// exactly as previousStorySQL spells it, and for the same reason: an
// INCONCLUSIVE run and a CANCELLED one both carry an empty result, persisted
// as NULL, and neither is evidence about the workload in either direction.
// Letting one into the window would let a night nobody could evaluate move the
// median the next night is graded against.
//
// It is written as a block rather than inline because gofmt rewrites a bare
// pair of apostrophes into a curly quote in doc comment prose, which would
// turn this SQL into typography.
//
// The empty-string half is not redundant with the NULL
// check: nullString maps "" to NULL on the way in, but a row written by an
// older build or by hand can still hold an empty string.
//
// The ordering repeats ListRuns' (started_at DESC, id DESC) so that "the last
// five" means the same thing here as everywhere else in the product, and so
// that two runs started in the same microsecond cannot swap places between
// two reads of the same window.
const capturedValuesSQL = `
SELECT cv.value
FROM check_values cv
JOIN runs r ON r.id = cv.run_id
JOIN run_checks rc ON rc.run_id = cv.run_id AND rc.seq = cv.check_seq
WHERE r.source_workload_id = ?
	AND rc.name = ?
	AND cv.name = ?
	AND r.result IS NOT NULL AND r.result <> ''
ORDER BY r.started_at DESC, r.id DESC
LIMIT ?`

// CapturedValues returns what previous drills of this workload measured under
// this check and this capture name, most recent first.
//
// A workload with no history is an empty slice and no error. A first drill has
// nothing to compare against, and that is what a first drill is.
func (s *sqlStore) CapturedValues(ctx context.Context, workloadID, checkName, valueName string,
	limit int) ([]float64, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}

	rows, err := s.query(ctx, capturedValuesSQL, workloadID, checkName, valueName, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read the history of %q on check %q of workload %s: %w",
			valueName, checkName, workloadID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// runCheckValuesSQL reads every value one run measured.
//
// No join and no verdict filter: this is the report's question, not drift's.
// A run that could not be evaluated still measured what it measured, and
// hiding the numbers from the person reading the run would be hiding the one
// piece of evidence the drill did produce.
const runCheckValuesSQL = `
SELECT check_seq, name, value FROM check_values WHERE run_id = ? ORDER BY check_seq, name`

// RunCheckValues returns everything one run measured, keyed by check seq then
// by capture name.
func (s *sqlStore) RunCheckValues(ctx context.Context, runID string) (map[int]map[string]float64, error) {
	out := map[int]map[string]float64{}

	rows, err := s.query(ctx, runCheckValuesSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("store: read the values of run %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			seq   int
			name  string
			value float64
		)
		if err := rows.Scan(&seq, &name, &value); err != nil {
			return nil, err
		}
		if out[seq] == nil {
			out[seq] = map[string]float64{}
		}
		out[seq][name] = value
	}
	return out, rows.Err()
}
