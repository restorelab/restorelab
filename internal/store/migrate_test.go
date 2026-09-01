package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Temporary until Task 4 provides the real helper.
func openTestSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsArePairedAcrossDialects(t *testing.T) {
	sqlite, err := loadMigrations("sqlite")
	if err != nil {
		t.Fatalf("loadMigrations(sqlite): %v", err)
	}
	postgres, err := loadMigrations("postgres")
	if err != nil {
		t.Fatalf("loadMigrations(postgres): %v", err)
	}

	if len(sqlite) == 0 {
		t.Fatal("no sqlite migrations found: the embed pattern is probably wrong")
	}
	if len(sqlite) != len(postgres) {
		t.Fatalf("sqlite has %d migrations, postgres has %d: every migration must exist in both dialects",
			len(sqlite), len(postgres))
	}
	for i := range sqlite {
		if sqlite[i].Number != postgres[i].Number || sqlite[i].Name != postgres[i].Name {
			t.Errorf("migration %d differs: sqlite has %d_%s, postgres has %d_%s",
				i, sqlite[i].Number, sqlite[i].Name, postgres[i].Number, postgres[i].Name)
		}
	}
}

func TestMigrationsAreNumberedFromOneWithoutGaps(t *testing.T) {
	migrations, err := loadMigrations("sqlite")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for i, m := range migrations {
		if m.Number != i+1 {
			t.Fatalf("migration at position %d is numbered %d: numbers must run from 1 without gaps", i, m.Number)
		}
	}
}

func TestApplyMigrationsIsIdempotent(t *testing.T) {
	db := openTestSQLite(t)

	first, err := applyMigrations(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("first applyMigrations: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first run applied nothing")
	}

	second, err := applyMigrations(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("second applyMigrations: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second run applied %v, want nothing: migrations must be idempotent", second)
	}
}

func TestPendingMigrationsOnAFreshDatabase(t *testing.T) {
	db := openTestSQLite(t)

	pending, err := pendingMigrations(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("pendingMigrations: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("a fresh database must report pending migrations")
	}

	if _, err := applyMigrations(context.Background(), db, "sqlite"); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	pending, err = pendingMigrations(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("pendingMigrations after apply: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("after applying, %d migrations still pending", len(pending))
	}
}
