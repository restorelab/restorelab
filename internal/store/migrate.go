package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationFS embed.FS

// Migration is one numbered schema change, in one dialect. The same number
// and name must exist in both dialects; a test enforces it, because two
// engines drifting apart is the standing risk of supporting both.
type Migration struct {
	Number int
	Name   string
	SQL    string
}

// loadMigrations reads a dialect's migrations, ordered by number. File names
// are "<number>_<name>.sql", e.g. "0001_runs.sql".
func loadMigrations(dialect string) ([]Migration, error) {
	entries, err := migrationFS.ReadDir(path.Join("migrations", dialect))
	if err != nil {
		return nil, fmt.Errorf("store: read migrations for %s: %w", dialect, err)
	}

	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numText, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q is not named <number>_<name>.sql", e.Name())
		}
		number, err := strconv.Atoi(numText)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has a non-numeric prefix: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile(path.Join("migrations", dialect, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		out = append(out, Migration{Number: number, Name: name, SQL: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// ensureMigrationsTable creates the bookkeeping table. The statement is valid
// in both dialects.
func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		number     integer PRIMARY KEY,
		name       text    NOT NULL,
		applied_at text    NOT NULL
	)`)
	return err
}

// appliedNumbers reads which migrations this database already has.
func appliedNumbers(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT number FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		applied[n] = true
	}
	return applied, rows.Err()
}

// pendingMigrations lists what this database is missing, in order.
func pendingMigrations(ctx context.Context, db *sql.DB, dialect string) ([]Migration, error) {
	if err := ensureMigrationsTable(ctx, db); err != nil {
		return nil, err
	}
	all, err := loadMigrations(dialect)
	if err != nil {
		return nil, err
	}
	applied, err := appliedNumbers(ctx, db)
	if err != nil {
		return nil, err
	}

	var pending []Migration
	for _, m := range all {
		if !applied[m.Number] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// applyMigrations brings the database up to date and reports which numbers it
// applied. Each migration runs in its own transaction: a failure leaves the
// schema at the last complete step rather than half-applied.
func applyMigrations(ctx context.Context, db *sql.DB, dialect string) ([]int, error) {
	pending, err := pendingMigrations(ctx, db, dialect)
	if err != nil {
		return nil, err
	}

	var applied []int
	for _, m := range pending {
		if err := applyOne(ctx, db, Dialect(dialect), m); err != nil {
			return applied, fmt.Errorf("store: migration %04d_%s: %w", m.Number, m.Name, err)
		}
		applied = append(applied, m.Number)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db *sql.DB, dialect Dialect, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// No-op once Commit has succeeded; it only matters on the error returns.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		rebind(dialect, `INSERT INTO schema_migrations (number, name, applied_at) VALUES (?, ?, ?)`),
		m.Number, m.Name, formatTime(nowUTC()),
	); err != nil {
		return err
	}
	return tx.Commit()
}
