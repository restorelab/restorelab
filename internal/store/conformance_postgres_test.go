package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The conformance suite against PostgreSQL.
//
// It is skipped unless RESTORELAB_TEST_DATABASE_URL points at a database we
// may create and drop schemas in, so `go test ./...` stays green on a bare
// machine — which is a hard requirement of this project.
//
// Run it with:
//
//	docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test --name rl-pg postgres:17
//	RESTORELAB_TEST_DATABASE_URL='postgres://postgres:test@localhost:5432/postgres?sslmode=disable' \
//	  go test ./internal/store/ -run Postgres -v
//
// When a test passes on SQLite and fails here, that is the divergence this
// whole arrangement exists to catch: fix the shared query, never write a
// second one.
func newTestPostgresStore(t *testing.T) Store {
	t.Helper()

	dsn := os.Getenv("RESTORELAB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESTORELAB_TEST_DATABASE_URL is not set; skipping the PostgreSQL conformance run")
	}

	// Each sub-test gets its own schema, dropped afterwards, so tests cannot
	// see each other's rows.
	schema := "rl_test_" + sanitiseSchemaName(t.Name())
	ctx := context.Background()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})

	scoped := dsn
	if strings.Contains(dsn, "?") {
		scoped += "&search_path=" + schema
	} else {
		scoped += "?search_path=" + schema
	}

	// Apply the migrations by hand: OpenPostgres deliberately refuses to, and
	// that refusal is itself worth exercising below.
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatalf("open scoped connection: %v", err)
	}
	if _, err := applyMigrations(ctx, db, string(dialectPostgres)); err != nil {
		db.Close()
		t.Fatalf("applyMigrations: %v", err)
	}
	db.Close()

	s, err := OpenPostgres(ctx, scoped)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// sanitiseSchemaName turns a Go test name into something PostgreSQL accepts
// as a bare identifier.
func sanitiseSchemaName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func TestPostgresRunConformance(t *testing.T) { RunConformance(t, newTestPostgresStore) }

func TestPostgresStepsAndChecksConformance(t *testing.T) {
	StepsAndChecksConformance(t, newTestPostgresStore)
}

func TestPostgresEventsConformance(t *testing.T) { EventsConformance(t, newTestPostgresStore) }

func TestPostgresListConformance(t *testing.T) { ListConformance(t, newTestPostgresStore) }

func TestPostgresTokensConformance(t *testing.T) { TokensConformance(t, newTestPostgresStore) }

func TestPostgresSessionsConformance(t *testing.T) {
	SessionsConformance(t, newTestPostgresStore)
}

func TestPostgresPlanConformance(t *testing.T) { PlanConformance(t, newTestPostgresStore) }

func TestPostgresTempWorkloadConformance(t *testing.T) {
	TempWorkloadConformance(t, newTestPostgresStore)
}

func TestPostgresLastRunsConformance(t *testing.T) {
	LastRunsConformance(t, newTestPostgresStore)
}

func TestPostgresQueueWriteConformance(t *testing.T) {
	QueueWriteConformance(t, newTestPostgresStore)
}

func TestPostgresQueueClaimConformance(t *testing.T) {
	QueueClaimConformance(t, newTestPostgresStore)
}

// A PostgreSQL database is shared: RestoreLab must refuse to write into a
// schema it does not fully recognise, rather than migrate it behind the
// operator's back.
func TestPostgresRefusesABehindSchema(t *testing.T) {
	dsn := os.Getenv("RESTORELAB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESTORELAB_TEST_DATABASE_URL is not set")
	}

	schema := "rl_test_" + sanitiseSchemaName(t.Name())
	ctx := context.Background()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})

	scoped := dsn
	if strings.Contains(dsn, "?") {
		scoped += "&search_path=" + schema
	} else {
		scoped += "?search_path=" + schema
	}

	_, err = OpenPostgres(ctx, scoped)
	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("OpenPostgres on an unmigrated schema = %v, want ErrSchemaBehind", err)
	}
	if !strings.Contains(err.Error(), "db migrate") {
		t.Errorf("the error should name the command that fixes it, got: %v", err)
	}
}

// Redaction is not optional: a PostgreSQL URL routinely carries a password,
// and it must never reach a log, an error, or `doctor`.
func TestRedactDSNRemovesThePassword(t *testing.T) {
	got := RedactDSN("postgres://restorelab:hunter2@db.example.com:5432/restorelab")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("RedactDSN leaked the password: %q", got)
	}
	if !strings.Contains(got, "db.example.com") {
		t.Fatalf("RedactDSN = %q, want it to still name the host", got)
	}
	if !strings.Contains(got, "restorelab") {
		t.Fatalf("RedactDSN = %q, want it to still name the user and database", got)
	}
}

func TestRedactDSNOnAnUnparseableStringSaysNothing(t *testing.T) {
	got := RedactDSN("postgres://user:hunter2@%%%/db")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("RedactDSN leaked the password from an unparseable string: %q", got)
	}
}

// Some drivers quote the connection string back at you inside the error.
func TestRedactDSNErrorScrubsTheDriverMessage(t *testing.T) {
	dsn := "postgres://restorelab:hunter2@db.example.com:5432/restorelab"

	whole := redactDSNError(dsn, fmt.Errorf("cannot connect to %s", dsn))
	if strings.Contains(whole.Error(), "hunter2") {
		t.Fatalf("the whole-DSN case leaked the password: %v", whole)
	}

	justPassword := redactDSNError(dsn, fmt.Errorf(`password authentication failed for "hunter2"`))
	if strings.Contains(justPassword.Error(), "hunter2") {
		t.Fatalf("the bare-password case leaked the password: %v", justPassword)
	}
}
