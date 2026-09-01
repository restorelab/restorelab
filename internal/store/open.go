package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config says where the drill history lives.
type Config struct {
	// URL selects the engine by scheme: "postgres://..." or "sqlite://path".
	// Empty means the embedded default at DefaultPath.
	URL string
	// DefaultPath is the SQLite file used when URL is empty, normally
	// ~/.restorelab/history.db.
	DefaultPath string
}

// Open returns the store the configuration asks for.
//
// With no URL it is SQLite at DefaultPath, created and migrated on the spot.
// That default is the point of the whole design: telling an operator to stand
// up a PostgreSQL before they can see whether their RTO is degrading is the
// kind of friction connect, doctor and network create exist to remove.
func Open(ctx context.Context, cfg Config) (Store, error) {
	dialect, dsn, err := resolve(cfg)
	if err != nil {
		return nil, err
	}
	if dialect == dialectSQLite {
		return OpenSQLite(ctx, dsn)
	}
	return OpenPostgres(ctx, dsn)
}

// Migrate brings a configured database's schema up to date and reports which
// migrations it applied.
func Migrate(ctx context.Context, cfg Config) ([]int, error) {
	dialect, dsn, err := resolve(cfg)
	if err != nil {
		return nil, err
	}

	var db *sql.DB
	if dialect == dialectSQLite {
		if dir := filepath.Dir(dsn); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("store: create directory for %s: %w", dsn, err)
			}
		}
		db, err = sql.Open("sqlite", sqliteDSN(dsn))
	} else {
		db, err = sql.Open("pgx", dsn)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", redactDSNError(dsn, err))
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("store: reach database: %w", redactDSNError(dsn, err))
	}
	return applyMigrations(ctx, db, string(dialect))
}

// resolve turns a Config into the engine and the connection string to use.
//
// Open and Migrate must agree on this exactly - a `db migrate` that migrated
// a different database from the one being written to would be worse than no
// command at all - so it lives in one place.
func resolve(cfg Config) (Dialect, string, error) {
	if cfg.URL == "" {
		if cfg.DefaultPath == "" {
			return "", "", fmt.Errorf("store: no database URL and no default path")
		}
		return dialectSQLite, cfg.DefaultPath, nil
	}

	scheme, rest, ok := strings.Cut(cfg.URL, "://")
	if !ok {
		return "", "", fmt.Errorf("store: %q is not a database URL: expected postgres://... or sqlite://path", cfg.URL)
	}

	switch scheme {
	case "sqlite", "file":
		if rest == "" {
			return "", "", fmt.Errorf("store: %q names no file: expected sqlite:///path/to/history.db", cfg.URL)
		}
		return dialectSQLite, rest, nil
	case "postgres", "postgresql":
		return dialectPostgres, cfg.URL, nil
	default:
		return "", "", fmt.Errorf("store: unsupported database scheme %q: RestoreLab speaks sqlite and postgres", scheme)
	}
}

// Describe renders a configured location for display, without its password.
// It answers `db status` before a connection has been attempted.
func Describe(cfg Config) string {
	dialect, dsn, err := resolve(cfg)
	if err != nil {
		return "(misconfigured)"
	}
	if dialect == dialectSQLite {
		return "sqlite " + dsn
	}
	return "postgres " + RedactDSN(dsn)
}
