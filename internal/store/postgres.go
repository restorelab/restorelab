package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, so the query set stays shared
)

// ErrSchemaBehind says the database is missing migrations. The caller falls
// back to Noop and names the command that fixes it, rather than writing into
// a schema that cannot hold the data.
var ErrSchemaBehind = errors.New("store: database schema is behind")

// OpenPostgres connects to a PostgreSQL database and refuses to use it if its
// schema is behind.
//
// Migrations are NOT applied here, unlike SQLite. A PostgreSQL database is
// shared and may serve several instances; migrating someone else's schema as
// a side effect of running a CLI would be rude. `restorelab db migrate` is
// the deliberate act.
func OpenPostgres(ctx context.Context, dsn string) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open postgres: %w", redactDSNError(dsn, err))
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: reach postgres: %w", redactDSNError(dsn, err))
	}

	pending, err := pendingMigrations(ctx, db, string(dialectPostgres))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: inspect postgres schema: %w", redactDSNError(dsn, err))
	}
	if len(pending) > 0 {
		_ = db.Close()
		return nil, fmt.Errorf("%w: %d migration(s) pending, run `restorelab db migrate`", ErrSchemaBehind, len(pending))
	}

	return &sqlStore{db: db, dialect: dialectPostgres, describe: "postgres " + RedactDSN(dsn)}, nil
}

// postgresClaimSuffix is the PostgreSQL half of the only query this project
// writes twice.
//
// FOR UPDATE SKIP LOCKED locks the row this transaction is about to take and
// makes every other worker skip it rather than queue behind it. It is why
// several workers can drain the queue at full speed without ever colliding,
// and why the claim needs no advisory lock and no retry loop.
const postgresClaimSuffix = " FOR UPDATE SKIP LOCKED"

// RedactDSN strips the password from a connection string so it can be shown
// to a user or written to a log.
//
// A PostgreSQL URL routinely carries a password, and nothing in this package
// may leak it - not into an error, not into `doctor`, not into a log line.
// An unparseable string says nothing at all rather than risk echoing one.
func RedactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(connection string)"
	}
	return u.Redacted()
}

// redactDSNError keeps a driver error from carrying the password: some
// drivers quote the whole connection string back at you.
func redactDSNError(dsn string, err error) error {
	if err == nil || dsn == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, dsn) {
		// The driver may also quote just the password, without the rest of
		// the URL around it. Scrub that too.
		if pw, ok := dsnPassword(dsn); ok && strings.Contains(msg, pw) {
			return errors.New(strings.ReplaceAll(msg, pw, "xxxxx"))
		}
		return err
	}
	return errors.New(strings.ReplaceAll(msg, dsn, RedactDSN(dsn)))
}

// dsnPassword pulls the password out of a connection string, if it has one.
func dsnPassword(dsn string) (string, bool) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "", false
	}
	pw, ok := u.User.Password()
	if !ok || pw == "" {
		return "", false
	}
	return pw, true
}
