package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/restorelab/restorelab/internal/core"
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

// errNotImplemented marks the methods tasks 5 to 7 still have to fill in. A
// grep for it is the check that none survive.
var errNotImplemented = errors.New("store: not implemented yet")

func (s *sqlStore) CreateRun(context.Context, *core.RecoveryRun, string) error {
	return errNotImplemented
}
func (s *sqlStore) UpdateRun(context.Context, *core.RecoveryRun) error { return errNotImplemented }
func (s *sqlStore) SaveStep(context.Context, string, int, core.Step) error {
	return errNotImplemented
}
func (s *sqlStore) SaveCheck(context.Context, string, int, core.CheckResult) error {
	return errNotImplemented
}
func (s *sqlStore) AppendEvent(context.Context, string, Event) error { return errNotImplemented }
func (s *sqlStore) GetRun(context.Context, string) (*core.RecoveryRun, error) {
	return nil, errNotImplemented
}
func (s *sqlStore) ListRuns(context.Context, Filter) ([]RunSummary, error) {
	return nil, errNotImplemented
}
func (s *sqlStore) Events(context.Context, string, int64) ([]Event, error) {
	return nil, errNotImplemented
}
