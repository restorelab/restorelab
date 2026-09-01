package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDefaultsToSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")

	s, err := Open(context.Background(), Config{DefaultPath: dbPath})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if !strings.HasPrefix(s.Describe(), "sqlite") {
		t.Fatalf("Describe = %q, want it to start with sqlite", s.Describe())
	}
}

func TestOpenHonoursAnExplicitSQLiteURL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "elsewhere.db")

	s, err := Open(context.Background(), Config{URL: "sqlite://" + filepath.ToSlash(dbPath)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if !strings.Contains(s.Describe(), "elsewhere.db") {
		t.Fatalf("Describe = %q, want it to name the file we asked for", s.Describe())
	}
}

func TestOpenRejectsAnUnknownScheme(t *testing.T) {
	_, err := Open(context.Background(), Config{URL: "mysql://localhost/restorelab"})
	if err == nil {
		t.Fatal("Open must refuse a scheme it cannot serve")
	}
	if !strings.Contains(err.Error(), "mysql") {
		t.Errorf("the error should name the scheme, got: %v", err)
	}
}

func TestOpenRejectsAStringThatIsNotAURL(t *testing.T) {
	_, err := Open(context.Background(), Config{URL: "/var/lib/restorelab/history.db"})
	if err == nil {
		t.Fatal("Open must refuse a bare path: the scheme is what picks the engine")
	}
}

func TestOpenWithNoURLAndNoDefaultPathIsAnError(t *testing.T) {
	if _, err := Open(context.Background(), Config{}); err == nil {
		t.Fatal("Open must refuse a config that names nowhere to write")
	}
}

// Open and Migrate must agree on which database they are talking about; a
// `db migrate` that migrated something else would be worse than no command.
func TestOpenAndMigrateResolveToTheSameDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	cfg := Config{DefaultPath: dbPath}
	ctx := context.Background()

	applied, err := Migrate(ctx, cfg)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("Migrate applied nothing to a fresh database")
	}

	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Already migrated, so a second Migrate has nothing to do.
	again, err := Migrate(ctx, cfg)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second Migrate applied %v, want nothing: Open and Migrate disagree on the target", again)
	}
}

// A password in the URL must never reach an error message.
func TestOpenNeverLeaksThePasswordOnFailure(t *testing.T) {
	_, err := Open(context.Background(), Config{
		URL: "postgres://user:hunter2@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1",
	})
	if err == nil {
		t.Skip("something answered on that port; nothing to assert")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error leaked the password: %v", err)
	}
}

func TestDescribeNeverLeaksThePassword(t *testing.T) {
	got := Describe(Config{URL: "postgres://restorelab:hunter2@db.example.com:5432/restorelab"})
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Describe leaked the password: %q", got)
	}
	if !strings.Contains(got, "db.example.com") {
		t.Errorf("Describe = %q, want it to still name the host", got)
	}
}

func TestDescribeNamesTheEmbeddedFile(t *testing.T) {
	got := Describe(Config{DefaultPath: "/home/x/.restorelab/history.db"})
	if !strings.Contains(got, "sqlite") || !strings.Contains(got, "history.db") {
		t.Fatalf("Describe = %q, want it to name the engine and the file", got)
	}
}
