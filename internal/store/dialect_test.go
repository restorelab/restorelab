package store

import "testing"

func TestRebindPostgresNumbersPlaceholders(t *testing.T) {
	got := rebind(dialectPostgres, `INSERT INTO runs (id, state) VALUES (?, ?)`)
	want := `INSERT INTO runs (id, state) VALUES ($1, $2)`
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
}

func TestRebindSQLiteLeavesQueryAlone(t *testing.T) {
	query := `INSERT INTO runs (id, state) VALUES (?, ?)`
	if got := rebind(dialectSQLite, query); got != query {
		t.Fatalf("rebind = %q, want it unchanged", got)
	}
}

// A question mark inside a string literal is data, not a placeholder. The
// prefix match in resolveRunID uses a literal, so this is not hypothetical.
func TestRebindIgnoresQuestionMarksInsideLiterals(t *testing.T) {
	got := rebind(dialectPostgres, `SELECT ? WHERE name = 'what? really'`)
	want := `SELECT $1 WHERE name = 'what? really'`
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
}

func TestRebindNumbersPastNine(t *testing.T) {
	query := `VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	got := rebind(dialectPostgres, query)
	want := `VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
}

func TestRebindHandlesTheLiteralUsedForPrefixMatching(t *testing.T) {
	got := rebind(dialectPostgres, `SELECT id FROM runs WHERE id = ? OR id LIKE ? || '%' LIMIT 2`)
	want := `SELECT id FROM runs WHERE id = $1 OR id LIKE $2 || '%' LIMIT 2`
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
}
