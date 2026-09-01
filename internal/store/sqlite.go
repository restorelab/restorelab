package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure Go, no cgo: the binary stays static
)

// OpenSQLite opens the embedded history database, creating it if needed, and
// brings its schema up to date.
//
// Migrations are applied automatically here, unlike PostgreSQL. This file
// belongs to the tool: asking the operator to run "db migrate" after every
// upgrade would be pure friction, and it is exactly the command one forgets
// until a run fails. The file is copied aside first, so a migration that goes
// wrong is recoverable.
func OpenSQLite(ctx context.Context, dbPath string) (Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create directory for %s: %w", dbPath, err)
		}
	}

	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}

	// One connection. SQLite serialises writers anyway, and a pool only
	// creates lock contention against ourselves.
	db.SetMaxOpenConns(1)

	pending, err := pendingMigrations(ctx, db, string(dialectSQLite))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: inspect schema of %s: %w", dbPath, err)
	}
	if len(pending) > 0 {
		// Back up only when this is an upgrade of a database that already
		// holds history. Creating one is not: SQLite makes the file the
		// moment we connect, so "the file exists" would be true on a first
		// run too, and a history.db.bak appearing on the day of install
		// looks alarming for nothing.
		applied, err := appliedNumbers(ctx, db)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: inspect schema of %s: %w", dbPath, err)
		}
		if len(applied) > 0 {
			if err := backupBeforeMigrate(dbPath); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("store: back up %s before migrating: %w", dbPath, err)
			}
		}
		if _, err := applyMigrations(ctx, db, string(dialectSQLite)); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	return &sqlStore{db: db, dialect: dialectSQLite, describe: "sqlite " + dbPath}, nil
}

// sqliteDSN carries the PRAGMAs the schema depends on.
//
//   - WAL: a "runs list" must not block a drill that is writing.
//   - busy_timeout: two commands started at once wait instead of failing.
//   - foreign_keys: SQLite disables them by default, and the schema's
//     ON DELETE CASCADE clauses rely on them.
//   - synchronous NORMAL: enough under WAL, and the journal is not the
//     critical data here - the cluster is.
func sqliteDSN(dbPath string) string {
	return dbPath + "?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"
}

// backupBeforeMigrate copies the database aside before the schema changes.
// Cheap insurance on a file the user cannot regenerate: the drill history is
// the one thing here that exists nowhere else.
func backupBeforeMigrate(dbPath string) error {
	data, err := os.ReadFile(dbPath)
	if os.IsNotExist(err) {
		return nil // nothing to lose
	}
	if err != nil {
		return err
	}
	return os.WriteFile(dbPath+".bak", data, 0o600)
}
