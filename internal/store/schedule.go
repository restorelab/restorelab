package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/restorelab/restorelab/internal/core"
)

const slotColumns = `plan_id, slot_at, decided_at, outcome, reason, run_id`

// insertSlotSQL refuses an already decided slot without reading the driver's
// error, the way insertPlanSQL refuses a taken name: a primary key violation
// does not have the same shape under modernc.org/sqlite and under pgx, and
// parsing either message would be a dialect difference to maintain. The
// primary key stays underneath as the real defence against a race - this
// clause only decides which error the caller sees.
const insertSlotSQL = `
INSERT INTO schedule_slots (` + slotColumns + `)
SELECT ?, ?, ?, ?, ?, ?
WHERE NOT EXISTS (
	SELECT 1 FROM schedule_slots WHERE plan_id = ? AND slot_at = ?
)`

// ClaimSlot records a slot decision and queues its run in one transaction.
func (s *sqlStore) ClaimSlot(ctx context.Context, slot Slot, run *core.RecoveryRun, planYAML string) error {
	if slot.Outcome == SlotQueued && run == nil {
		return errors.New("store: a queued slot needs the run it queued")
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		slotAt := formatTime(slot.SlotAt.UTC())
		decidedAt := formatTime(slot.DecidedAt.UTC())

		// The run is written first, because schedule_slots.run_id is a
		// foreign key into runs and both engines enforce it.
		//
		// The order is not what makes this safe - the transaction is. A
		// duplicate slot below rolls this insert back, so a run queued here
		// either has a slot accounting for it or never existed at all.
		// Getting that wrong is how a scheduler ends up with a drill nobody
		// scheduled, which is the same production clone restored twice.
		if slot.Outcome == SlotQueued {
			if _, err := s.execTx(ctx, tx, enqueueSQL,
				run.ID, run.PlanName, planYAML,
				nullString(run.PlanID), nullInt(run.PlanVersion),
				run.ProviderID, nullString(run.BackupProviderID),
				run.SourceWorkloadID, nullString(run.SourceName), string(core.RunQueued),
				decidedAt, decidedAt, run.RTOTarget.Milliseconds(),
			); err != nil {
				return err
			}
		}

		n, err := s.execTx(ctx, tx, insertSlotSQL,
			slot.PlanID, slotAt, decidedAt,
			string(slot.Outcome), nullString(slot.Reason), nullString(slot.RunID),
			slot.PlanID, slotAt)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrDuplicate
		}
		return nil
	})
}

const selectLastSlotSQL = `
SELECT ` + slotColumns + ` FROM schedule_slots
WHERE plan_id = ? ORDER BY slot_at DESC LIMIT 1`

// LastSlot returns the most recent slot decided for a plan.
func (s *sqlStore) LastSlot(ctx context.Context, planID string) (*Slot, error) {
	slot, err := scanSlot(s.queryRow(ctx, selectLastSlotSQL, planID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return slot, nil
}

// ListSlots returns decided slots, most recent first.
func (s *sqlStore) ListSlots(ctx context.Context, f SlotFilter) ([]Slot, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}

	// The columns are qualified because the workload filter joins plans in.
	// One query rather than one per plan: a machine covered by three plans
	// is still one question.
	query := `SELECT s.plan_id, s.slot_at, s.decided_at, s.outcome, s.reason, s.run_id
FROM schedule_slots s`
	var args []any
	var where []string
	if f.WorkloadID != "" {
		query += ` JOIN plans p ON p.id = s.plan_id`
		where = append(where, `p.workload_id = ?`)
		args = append(args, f.WorkloadID)
	}
	if f.PlanID != "" {
		where = append(where, `s.plan_id = ?`)
		args = append(args, f.PlanID)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY s.slot_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Slot
	for rows.Next() {
		slot, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *slot)
	}
	return out, rows.Err()
}

// rowScanner is what *sql.Row and *sql.Rows both satisfy, so one scan
// function serves LastSlot and ListSlots and there is only one place that
// knows the column order.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSlot(sc rowScanner) (*Slot, error) {
	var (
		slot              Slot
		slotAt, decidedAt string
		outcome           string
		reason, runID     *string
	)
	if err := sc.Scan(&slot.PlanID, &slotAt, &decidedAt, &outcome, &reason, &runID); err != nil {
		return nil, err
	}

	var err error
	if slot.SlotAt, err = parseTime(slotAt); err != nil {
		return nil, err
	}
	if slot.DecidedAt, err = parseTime(decidedAt); err != nil {
		return nil, err
	}
	slot.Outcome = SlotOutcome(outcome)
	if reason != nil {
		slot.Reason = *reason
	}
	if runID != nil {
		slot.RunID = *runID
	}
	return &slot, nil
}
