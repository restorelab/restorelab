package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

// terminalStates lists the states a run never leaves. It is written out
// rather than derived from core.RunState.Terminal() because it goes into
// SQL: the database cannot call a Go method, and a state added to one and
// not the other is exactly the kind of drift a queue turns into a stuck run.
// The test QueueStatesMatchCoreTerminal keeps them in step.
var terminalStates = []core.RunState{
	core.RunSuccess, core.RunFailed, core.RunCancelled, core.RunCleanupFailed,
}

// terminalList renders terminalStates as a bound-parameter list.
func terminalList() (string, []any) {
	marks := make([]string, len(terminalStates))
	args := make([]any, len(terminalStates))
	for i, s := range terminalStates {
		marks[i] = "?"
		args[i] = string(s)
	}
	return strings.Join(marks, ", "), args
}

const enqueueSQL = `
INSERT INTO runs (
	id, plan_name, plan_snapshot, plan_id, plan_version,
	provider_id, backup_provider_id,
	source_workload_id, source_name, state, started_at, queued_at,
	rto_ms, rto_target_ms, cleanup_done
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0)`

// Enqueue records a run to be executed later.
//
// started_at is set to the queue time rather than left null: the whole
// history is ordered by it, and a row that sorted last until a worker picked
// it up would be invisible in the listing that is supposed to show it
// waiting.
//
// The provenance is written here for the same reason CreateRun writes it: a
// queued drill knows where it came from at the moment it is queued, and the
// worker that picks it up later has no way to find out. It is never read on
// the way to execution - the snapshot is - it only has to survive.
func (s *sqlStore) Enqueue(ctx context.Context, run *core.RecoveryRun, planYAML string, at time.Time) error {
	return s.exec(ctx, enqueueSQL,
		run.ID, run.PlanName, planYAML,
		nullString(run.PlanID), nullInt(run.PlanVersion),
		run.ProviderID, nullString(run.BackupProviderID),
		run.SourceWorkloadID, nullString(run.SourceName), string(core.RunQueued),
		formatTime(at), formatTime(at), run.RTOTarget.Milliseconds(),
	)
}

// SetState writes the run's state and nothing else - and never over a state
// the run has already settled into.
//
// The guard is not defensive programming, it is a false alarm this product
// cannot afford. Two workers sweeping the same interrupted run both call
// Delete: the winner removes the workload and records FAILED, the loser is
// refused by the provider and would write CLEANUP_FAILED with an ORPHANED
// WORKLOAD message over it. Nothing unsafe happens - nothing is re-run and
// nothing extra is deleted - but that message is the loudest alarm the
// product has, and an alarm that cries wolf is one an operator learns to
// ignore. The first writer decides.
func (s *sqlStore) SetState(ctx context.Context, runID string, state core.RunState) error {
	marks, args := terminalList()
	args = append([]any{string(state), runID}, args...)
	return s.exec(ctx, `UPDATE runs SET state = ? WHERE id = ? AND state NOT IN (`+marks+`)`, args...)
}

const setRunErrorSQL = `UPDATE runs SET err = ? WHERE id = ?`

// SetRunError writes the run's error message and nothing else.
//
// Like SetState and SetTempWorkload, an unknown run id is not an error: this
// is a best-effort write made from something other than a full run, and
// nothing here should ever become a reason to abort or retry.
func (s *sqlStore) SetRunError(ctx context.Context, runID, message string) error {
	return s.exec(ctx, setRunErrorSQL, nullString(message), runID)
}

// RequestCancel asks a run to stop, settling it immediately when nothing has
// started.
//
// The two cases are genuinely different. A queued run nobody has claimed has
// created nothing on any cluster: it can be marked CANCELLED here, and no
// worker ever needs to hear about it. A run being executed can only be asked;
// the worker notices and cancels the engine's context, and the temporary
// workload is destroyed on the way out.
func (s *sqlStore) RequestCancel(ctx context.Context, runID string, at time.Time) (bool, error) {
	settled, err := s.execCount(ctx,
		`UPDATE runs SET state = ?, completed_at = ?, cancel_requested_at = ?
		 WHERE id = ? AND state = ? AND lease_owner IS NULL`,
		string(core.RunCancelled), formatTime(at), formatTime(at), runID, string(core.RunQueued))
	if err != nil {
		return false, err
	}
	if settled == 1 {
		return true, nil
	}

	marks, args := terminalList()
	args = append([]any{formatTime(at), runID}, args...)
	asked, err := s.execCount(ctx,
		`UPDATE runs SET cancel_requested_at = ? WHERE id = ? AND state NOT IN (`+marks+`)`,
		args...)
	if err != nil {
		return false, err
	}
	if asked == 1 {
		return false, nil
	}

	// Neither settled nor asked: the run is finished, or it does not exist.
	var state string
	err = s.queryRow(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrNotFound, runID)
	}
	if err != nil {
		return false, err
	}
	return false, fmt.Errorf("%w: run %s is %s", ErrAlreadySettled, runID, state)
}

const cancelRequestedSQL = `SELECT cancel_requested_at FROM runs WHERE id = ?`

// CancelRequested reports whether a cancellation was asked for.
func (s *sqlStore) CancelRequested(ctx context.Context, runID string) (bool, error) {
	var at sql.NullString
	err := s.queryRow(ctx, cancelRequestedSQL, runID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrNotFound, runID)
	}
	if err != nil {
		return false, err
	}
	return at.Valid && at.String != "", nil
}

// ActiveRunForWorkload returns this workload's queued or running drill.
//
// It is what makes "one drill per workload at a time" enforceable: two
// concurrent drills of the same workload would restore the same backup twice,
// and a dashboard that double-clicks would queue two of them.
func (s *sqlStore) ActiveRunForWorkload(ctx context.Context, workloadID string) (string, error) {
	marks, args := terminalList()
	args = append([]any{workloadID}, args...)

	var id string
	err := s.queryRow(ctx,
		`SELECT id FROM runs WHERE source_workload_id = ? AND state NOT IN (`+marks+`)
		 ORDER BY started_at LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// queuedRunColumns is what a worker needs to execute a run and nothing more.
// It is shared by the claim and by StaleRuns so the two can never scan a
// different shape.
const queuedRunColumns = `id, plan_name, provider_id, backup_provider_id,
	source_workload_id, plan_snapshot, queued_at`

const selectClaimableSQL = `
SELECT ` + queuedRunColumns + `
FROM runs
WHERE state = ? AND lease_owner IS NULL
ORDER BY queued_at
LIMIT 1`

const takeLeaseSQL = `
UPDATE runs SET lease_owner = ?, lease_expires_at = ?
WHERE id = ? AND lease_owner IS NULL`

// claimSelect is the one point where the query set diverges between the two
// engines. The rationale for each half lives next to the engine it belongs
// to: postgresClaimSuffix and sqliteClaimSuffix.
func (s *sqlStore) claimSelect() string {
	if s.dialect == dialectPostgres {
		return selectClaimableSQL + postgresClaimSuffix
	}
	return selectClaimableSQL + sqliteClaimSuffix
}

// scanQueuedRun reads one row of queuedRunColumns.
func scanQueuedRun(sc interface{ Scan(...any) error }) (QueuedRun, error) {
	var (
		q                        QueuedRun
		backupProvider, queuedAt sql.NullString
	)
	if err := sc.Scan(&q.ID, &q.PlanName, &q.ProviderID, &backupProvider,
		&q.SourceWorkloadID, &q.PlanSnapshot, &queuedAt); err != nil {
		return QueuedRun{}, err
	}
	q.BackupProviderID = backupProvider.String

	at, err := parseNullTime(nullable(queuedAt))
	if err != nil {
		return QueuedRun{}, fmt.Errorf("store: run %s has an unreadable queued_at: %w", q.ID, err)
	}
	q.QueuedAt = at
	return q, nil
}

// ClaimRun takes ownership of the oldest queued run.
//
// The WHERE clause is the whole invariant of this phase: only a run whose
// lease_owner is null can be claimed, and the claim sets it. A run whose
// worker died therefore stays unclaimable forever - reconciliation fails it,
// and nothing can revive it. A drill destroys and recreates a machine; it is
// not idempotent, and replaying an interrupted one is worse than losing it.
//
// The transaction is opened the same way on both engines. What differs is
// what that transaction means: an immediate (write-locking) transaction on
// SQLite, thanks to the DSN, and a row lock taken by claimSelect on
// PostgreSQL.
func (s *sqlStore) ClaimRun(ctx context.Context, owner string, lease time.Duration, now time.Time) (*QueuedRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// A no-op once Commit has succeeded; it only matters on the error paths.
	defer func() { _ = tx.Rollback() }()

	q, err := s.claimLocked(ctx, tx, owner, lease, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return q, nil
}

// claimLocked is the half of the claim both engines share: read the oldest
// claimable row inside the transaction, then take it.
func (s *sqlStore) claimLocked(ctx context.Context, tx *sql.Tx, owner string,
	lease time.Duration, now time.Time) (*QueuedRun, error) {

	row := tx.QueryRowContext(ctx, rebind(s.dialect, s.claimSelect()), string(core.RunQueued))
	q, err := scanQueuedRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoWork
	}
	if err != nil {
		return nil, err
	}

	res, err := tx.ExecContext(ctx, rebind(s.dialect, takeLeaseSQL),
		owner, formatTime(now.Add(lease)), q.ID)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		// Someone took it between the select and the update. Not an error:
		// the caller asks again.
		return nil, ErrNoWork
	}
	return &q, nil
}

const renewLeaseSQL = `
UPDATE runs SET lease_expires_at = ? WHERE id = ? AND lease_owner = ?`

// RenewLease extends a lease its caller must already hold.
func (s *sqlStore) RenewLease(ctx context.Context, runID, owner string, until time.Time) error {
	n, err := s.execCount(ctx, renewLeaseSQL, formatTime(until), runID, owner)
	if err != nil {
		return err
	}
	if n != 1 {
		// Losing a lease is not a detail: it means another process believes
		// it owns this drill, and the caller must stop touching the cluster.
		return fmt.Errorf("store: run %s is not leased by %s", runID, owner)
	}
	return nil
}

const finishLeaseSQL = `UPDATE runs SET lease_expires_at = NULL WHERE id = ?`

// FinishLease clears the expiry of a finished run, keeping its owner.
//
// Keeping lease_owner is deliberate twice over: which worker ran a drill is
// part of its history, and a cleared owner would make the run claimable
// again.
func (s *sqlStore) FinishLease(ctx context.Context, runID string) error {
	return s.exec(ctx, finishLeaseSQL, runID)
}

const runLeaseSQL = `SELECT lease_owner, lease_expires_at FROM runs WHERE id = ?`

// RunLease reports who holds a run and until when.
//
// It is the read half of the lease, and it exists so that nothing outside
// this package has to know the two columns are called lease_owner and
// lease_expires_at. StaleRuns cannot stand in for it: it deliberately skips
// terminal runs, so it can never say anything about a drill that finished.
func (s *sqlStore) RunLease(ctx context.Context, runID string) (string, time.Time, error) {
	var owner, expires sql.NullString
	err := s.queryRow(ctx, runLeaseSQL, runID).Scan(&owner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, fmt.Errorf("%w: %s", ErrNotFound, runID)
	}
	if err != nil {
		return "", time.Time{}, err
	}

	at, err := parseNullTime(nullable(expires))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: run %s has an unreadable lease expiry: %w", runID, err)
	}
	return owner.String, at, nil
}

// StaleRuns lists the runs whose worker stopped renewing: it was claimed, it
// has not finished, and its lease has run out. They are never re-run, only
// failed.
func (s *sqlStore) StaleRuns(ctx context.Context, now time.Time) ([]QueuedRun, error) {
	marks, args := terminalList()
	args = append(args, formatTime(now))

	rows, err := s.query(ctx, `
		SELECT `+queuedRunColumns+`
		FROM runs
		WHERE lease_owner IS NOT NULL
			AND state NOT IN (`+marks+`)
			AND lease_expires_at IS NOT NULL
			AND lease_expires_at < ?
		ORDER BY queued_at`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []QueuedRun
	for rows.Next() {
		q, err := scanQueuedRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}
