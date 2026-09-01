package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openTestSQLite gives a throwaway, unmigrated database on disk. t.TempDir is
// removed for us, so nothing leaks between tests.
func openTestSQLite(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// newTestStore gives a ready-to-use Store backed by a throwaway SQLite file.
// It is what the conformance suite runs against on every `go test ./...`.
func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenSQLiteCreatesAndMigrates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "history.db")

	s, err := OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("the database file was not created: %v", err)
	}
	if s.Describe() == "" {
		t.Fatal("Describe must name the engine and the file")
	}
}

// Opening the same file twice must not re-run migrations or fail.
func TestOpenSQLiteTwiceOnTheSameFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	ctx := context.Background()

	first, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("first OpenSQLite: %v", err)
	}
	first.Close()

	second, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("second OpenSQLite: %v", err)
	}
	second.Close()
}

// A fresh database has nothing to lose, so no backup is written. The copy
// only matters when there is existing history.
func TestOpenSQLiteWritesNoBackupOnAFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")

	s, err := OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(dbPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("a backup was written for a database that had nothing in it")
	}
}

// The ON DELETE CASCADE clauses in the schema are inert unless foreign_keys
// is on, and SQLite leaves it off by default. This proves the DSN turns it on.
func TestSQLiteEnforcesForeignKeys(t *testing.T) {
	db := openTestSQLite(t)
	ctx := context.Background()
	if _, err := applyMigrations(ctx, db, "sqlite"); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	_, err := db.ExecContext(ctx,
		`INSERT INTO run_steps (run_id, seq, name, state, status) VALUES ('nope', 0, 'restore', 'RESTORING', 'done')`)
	if err == nil {
		t.Fatal("inserting a step for a run that does not exist must be rejected: foreign_keys is off")
	}
}

// WAL is what lets `runs list` read while a drill is writing.
func TestSQLiteUsesWAL(t *testing.T) {
	db := openTestSQLite(t)

	var mode string
	if err := db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}
