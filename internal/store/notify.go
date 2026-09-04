package store

// The notification half of the store: which runs have been considered, what
// a workload's story was before this run, and the queue of messages waiting
// to be posted.
//
// Nothing here diverges per engine. The claim is a plain conditional UPDATE
// rather than a row lock, so neither postgresClaimSuffix nor
// sqliteClaimSuffix applies, and every statement goes through
// s.exec/s.query/s.execCount so rebind handles the placeholders.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// claimRunForNotifySQL is the whole concurrency story of this feature.
//
// One statement, one winner: the WHERE clause makes every later caller update
// zero rows. A dispatcher that dies right after claiming has already recorded
// that this run was considered, which is the conservative outcome - silence
// about one run, rather than the same message twice.
const claimRunForNotifySQL = `
UPDATE runs SET notified_at = ? WHERE id = ? AND notified_at IS NULL`

// ClaimRunForNotify takes responsibility for deciding whether a run is worth
// announcing.
func (s *sqlStore) ClaimRunForNotify(ctx context.Context, runID string, at time.Time) (bool, error) {
	n, err := s.execCount(ctx, claimRunForNotifySQL, formatTime(at.UTC()), runID)
	if err != nil {
		return false, fmt.Errorf("store: claim run %s for notification: %w", runID, err)
	}
	return n == 1, nil
}

// unnotifiedRunsSQL lists terminal runs nobody has claimed yet.
//
// The columns and their order are lastRunsSQL's, so scanRunSummary reads this
// too. Writing a second scanner is the thing that comment forbids: two
// scanners are two chances for a query to disagree about what a summary is.
//
// The %s is the terminal state list, built from terminalStates and nothing
// else. No caller value ever reaches the query text.
//
// COALESCE(completed_at, started_at) rather than completed_at alone, and that
// is not a flourish. A run whose worker died is settled by SetState, which
// writes the state and nothing else, so a terminal run can genuinely carry no
// completion time - and the two engines sort NULLs at opposite ends. Without
// the COALESCE the backlog would read back in a different order on SQLite and
// on PostgreSQL, which is the exact class of divergence this package is
// arranged to prevent.
const unnotifiedRunsSQL = `
SELECT id, plan_name, plan_id, source_workload_id, source_name, state, result,
	started_at, completed_at, rto_ms, rto_target_ms, cleanup_done, proof_level
FROM runs
WHERE notified_at IS NULL AND state IN (%s)
ORDER BY COALESCE(completed_at, started_at) ASC, id ASC
LIMIT ?`

// UnnotifiedRuns lists terminal runs nobody has claimed yet, oldest first.
//
// Oldest first is the point of the ordering: a channel unreachable for an
// hour reads its backlog in the order things happened, rather than being told
// the ending before the middle.
func (s *sqlStore) UnnotifiedRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	marks, args := terminalList()
	args = append(args, limit)

	rows, err := s.query(ctx, fmt.Sprintf(unnotifiedRunsSQL, marks), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list unnotified runs: %w", err)
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

// previousStorySQL finds the newest earlier run of a workload that reached a
// verdict.
//
// "Reached a verdict" is result IS NOT NULL AND result <> ”: a cancelled run
// and an inconclusive one both carry an empty result, persisted as NULL, and
// neither is a claim about whether the backup restores. That emptiness is
// written by recovery.markCancelled and recovery.markInconclusive, and this
// clause is what reads it back correctly. The <> ” half is not redundant
// with the NULL check: nullString maps "" to NULL on the way in, but a row
// written by an older build or by hand can still hold an empty string.
//
// The ordering repeats ListRuns' (started_at DESC, id DESC) so that "before
// this run" means the same thing here as everywhere else in the product.
const previousStorySQL = `
SELECT id, plan_name, plan_id, source_workload_id, source_name, state, result,
	started_at, completed_at, rto_ms, rto_target_ms, cleanup_done, proof_level
FROM runs
WHERE source_workload_id = ?
	AND (started_at < ? OR (started_at = ? AND id < ?))
	AND result IS NOT NULL AND result <> ''
ORDER BY started_at DESC, id DESC
LIMIT 1`

// precedingStateSQL reads the state of the run immediately before this one,
// whatever that run concluded.
//
// It is deliberately previousStorySQL without the verdict clause. The two
// questions look at two different runs, and answering them in one method is
// what keeps them consistent: a caller assembling them from two calls could
// see a run land in between and get a baseline and a flag describing
// different histories.
const precedingStateSQL = `
SELECT state
FROM runs
WHERE source_workload_id = ?
	AND (started_at < ? OR (started_at = ? AND id < ?))
ORDER BY started_at DESC, id DESC
LIMIT 1`

// PreviousStory returns the workload's baseline before this run, and whether
// the run immediately before it reached no verdict.
func (s *sqlStore) PreviousStory(ctx context.Context, workloadID string, before Position) (*RunSummary, bool, error) {
	at := formatTime(before.StartedAt)

	prev, err := s.previousGradedRun(ctx, workloadID, at, before.ID)
	if err != nil {
		return nil, false, err
	}

	var state string
	err = s.queryRow(ctx, precedingStateSQL, workloadID, at, at, before.ID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		// No earlier run at all. Nothing was attempted, so nothing was
		// unevaluable: this is an answer, not a failure.
		return prev, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read the run before %s of workload %s: %w",
			before.ID, workloadID, err)
	}

	// INCONCLUSIVE only, never CANCELLED. A cancelled drill was stopped by a
	// human who already knows; it says nothing about whether the workload can
	// be seen, and reading it as a loss of visibility would announce a
	// situation nobody is in.
	return prev, core.RunState(state) == core.RunInconclusive, nil
}

// previousGradedRun runs previousStorySQL and scans at most one row.
//
// It goes through s.query rather than s.queryRow because scanRunSummary reads
// a *sql.Rows, which is what keeps it the single scanner for this column
// list.
func (s *sqlStore) previousGradedRun(ctx context.Context, workloadID, at, id string) (*RunSummary, error) {
	rows, err := s.query(ctx, previousStorySQL, workloadID, at, at, id)
	if err != nil {
		return nil, fmt.Errorf("store: read the story of workload %s: %w", workloadID, err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return nil, rows.Err()
	}
	r, err := scanRunSummary(rows)
	if err != nil {
		return nil, err
	}
	return &r, rows.Err()
}

const deliveryColumns = `id, run_id, channel_id, kind, state, attempts, next_at,
	status, error, payload, created_at, sent_at`

// insertDeliverySQL refuses a second message to the same channel about the
// same run without reading the driver's error.
//
// This is insertPlanSQL's mechanism, for insertPlanSQL's reason: a UNIQUE
// violation does not have the same shape under modernc.org/sqlite and under
// pgx, and parsing either message would be a second dialect difference to
// maintain. The unique index stays underneath as the real defence against a
// race.
//
// The two CASTs are what an INSERT ... SELECT costs on PostgreSQL: a bare
// parameter in a SELECT list carries no type, resolves to text, and then
// cannot be assigned to an integer column. insertPlanSQL never hit this
// because every value it binds is text. Saying the type here is cheaper than
// splitting the statement.
const insertDeliverySQL = `
INSERT INTO notification_deliveries (` + deliveryColumns + `)
SELECT ?, ?, ?, ?, ?, CAST(? AS integer), ?, CAST(? AS integer), ?, ?, ?, ?
WHERE NOT EXISTS (
	SELECT 1 FROM notification_deliveries WHERE run_id = ? AND channel_id = ?
)`

// CreateDelivery records one message to send.
func (s *sqlStore) CreateDelivery(ctx context.Context, d Delivery) error {
	n, err := s.execCount(ctx, insertDeliverySQL,
		d.ID, d.RunID, d.ChannelID, d.Kind, string(d.State), d.Attempts,
		formatNullTime(d.NextAt), d.Status, nullString(d.Err), d.Payload,
		formatTime(d.CreatedAt), formatNullTime(d.SentAt),
		d.RunID, d.ChannelID)
	if err != nil {
		return fmt.Errorf("store: record delivery %s for run %s: %w", d.ID, d.RunID, err)
	}
	if n == 0 {
		return ErrDuplicate
	}
	return nil
}

// dueDeliveriesSQL lists what may be attempted now.
//
// "next_at IS NULL OR next_at <= ?" rather than next_at alone: a delivery
// created with no schedule is due immediately, and a NULL compared against a
// timestamp is neither true nor false, so it would sit in the table forever
// with nobody noticing.
//
// The ordering breaks its tie on the id for the reason every ordering in this
// package does: two deliveries can be scheduled for the same instant, and a
// paged read that cannot order them would show one twice.
const dueDeliveriesSQL = `
SELECT ` + deliveryColumns + `
FROM notification_deliveries
WHERE state = ? AND (next_at IS NULL OR next_at <= ?)
ORDER BY next_at ASC, id ASC
LIMIT ?`

// DueDeliveries lists pending deliveries whose next attempt time has arrived.
func (s *sqlStore) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := s.query(ctx, dueDeliveriesSQL, string(DeliveryPending), formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list due deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const settleDeliverySQL = `
UPDATE notification_deliveries SET
	state = ?, attempts = ?, next_at = ?, status = ?, error = ?, sent_at = ?
WHERE id = ?`

// SettleDelivery writes the outcome of an attempt.
//
// It overwrites the attempt columns and leaves run_id, channel_id, kind,
// payload and created_at alone: what the message is about cannot change
// between attempts, and a retry has to send what the first attempt tried to
// send.
func (s *sqlStore) SettleDelivery(ctx context.Context, d Delivery) error {
	n, err := s.execCount(ctx, settleDeliverySQL,
		string(d.State), d.Attempts, formatNullTime(d.NextAt), d.Status,
		nullString(d.Err), formatNullTime(d.SentAt), d.ID)
	if err != nil {
		return fmt.Errorf("store: settle delivery %s: %w", d.ID, err)
	}
	if n == 0 {
		// Not a silent success. A dispatcher that believes it recorded an
		// outcome it never wrote would find the delivery due again and post
		// the same message a second time.
		return fmt.Errorf("%w: delivery %s", ErrNotFound, d.ID)
	}
	return nil
}

// scanDelivery reads one row of deliveryColumns.
//
// It mirrors scanRunSummary and exists for the same reason: CreateDelivery
// and DueDeliveries must not be able to disagree about what a delivery is.
func scanDelivery(rows *sql.Rows) (Delivery, error) {
	var (
		d                       Delivery
		state, createdAt        string
		nextAt, errText, sentAt sql.NullString
	)
	// attempts and status are NOT NULL with a default of 0, so they scan
	// straight into ints: unlike the run columns, there is no "not recorded"
	// value to keep distinct from zero.
	if err := rows.Scan(&d.ID, &d.RunID, &d.ChannelID, &d.Kind, &state, &d.Attempts,
		&nextAt, &d.Status, &errText, &d.Payload, &createdAt, &sentAt); err != nil {
		return d, err
	}

	d.State = DeliveryState(state)
	d.Err = errText.String

	var err error
	if d.NextAt, err = parseNullTime(nullable(nextAt)); err != nil {
		return d, fmt.Errorf("store: delivery %s has an unreadable next_at: %w", d.ID, err)
	}
	if d.CreatedAt, err = parseTime(createdAt); err != nil {
		return d, fmt.Errorf("store: delivery %s has an unreadable created_at: %w", d.ID, err)
	}
	if d.SentAt, err = parseNullTime(nullable(sentAt)); err != nil {
		return d, fmt.Errorf("store: delivery %s has an unreadable sent_at: %w", d.ID, err)
	}
	return d, nil
}

// lastDeliveriesSQL keeps only the newest delivery of each channel.
//
// ROW_NUMBER for the reason lastRunsSQL uses it: it is the one shape both
// engines take. The ordering repeats that query's tie-break on the id, so two
// deliveries written in the same microsecond resolve the same way here as
// they do for runs.
//
// The %s is a list of "?" placeholders built from the id count and nothing
// else. No caller value ever reaches the query text.
const lastDeliveriesSQL = `
SELECT ` + deliveryColumns + `
FROM (
	SELECT ` + deliveryColumns + `,
		ROW_NUMBER() OVER (
			PARTITION BY channel_id
			ORDER BY created_at DESC, id DESC
		) AS rn
	FROM notification_deliveries
	WHERE channel_id IN (%s)
) ranked
WHERE rn = 1`

// LastDeliveries returns each named channel's most recent delivery.
func (s *sqlStore) LastDeliveries(ctx context.Context, channelIDs []string) (map[string]Delivery, error) {
	out := map[string]Delivery{}
	// No question, no query: "IN ()" is a syntax error on both engines, and
	// an empty map is the right answer anyway.
	if len(channelIDs) == 0 {
		return out, nil
	}

	marks := make([]string, len(channelIDs))
	args := make([]any, len(channelIDs))
	for i, id := range channelIDs {
		marks[i] = "?"
		args[i] = id
	}

	rows, err := s.query(ctx, fmt.Sprintf(lastDeliveriesSQL, strings.Join(marks, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("store: last deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out[d.ChannelID] = d
	}
	return out, rows.Err()
}
