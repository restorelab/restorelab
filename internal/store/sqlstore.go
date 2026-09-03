package store

import (
	"context"
	"database/sql"
)

// sqlStore implements Store over database/sql.
//
// There is exactly one of these for both engines: the query set is shared and
// rebind is the only thing that differs. Two implementations would be two
// things to keep in step, which is the failure mode supporting two engines
// invites.
type sqlStore struct {
	db       *sql.DB
	dialect  Dialect
	describe string
}

var _ Store = (*sqlStore)(nil)

func (s *sqlStore) Describe() string { return s.describe }
func (s *sqlStore) Close() error     { return s.db.Close() }

// exec runs a statement, rebinding placeholders for the dialect.
func (s *sqlStore) exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, rebind(s.dialect, query), args...)
	return err
}

// query runs a SELECT, rebinding placeholders for the dialect.
func (s *sqlStore) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, rebind(s.dialect, q), args...)
}

// queryRow runs a single-row SELECT, rebinding placeholders for the dialect.
func (s *sqlStore) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, rebind(s.dialect, q), args...)
}

// execCount runs a statement and reports how many rows it changed. It is what
// lets "revoke a token that is not there" be an error rather than a silent
// success.
func (s *sqlStore) execCount(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := s.db.ExecContext(ctx, rebind(s.dialect, query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// withTx runs fn inside a transaction, rolling back on any error.
//
// It is the only transactional path in the package, and it exists for
// ClaimSlot: writing a schedule slot and queueing the run it decided on must
// be one atomic act. Split them, and a process that dies in between either
// drills a slot twice or forgets it - and the first of those restores a
// production clone a second time.
//
// On SQLite the transaction is immediate (_txlock=immediate on the DSN), so
// two writers contend at BEGIN, where busy_timeout applies, rather than
// halfway through with no way to back out.
func (s *sqlStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		// The rollback error is deliberately dropped: fn's error is the
		// diagnosis, and a failing rollback would replace it with its own
		// consequence.
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// execTx runs a statement inside a transaction and reports how many rows it
// changed, rebinding placeholders for the dialect the way exec does.
func (s *sqlStore) execTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, rebind(s.dialect, query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
