package plan

import (
	"os"
	"path/filepath"
	"testing"
)

// The shipped example plans double as documentation, so a change to the plan
// schema that breaks them must break the build.
func TestExamplePlansAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "plans")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no example plans found")
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			p, err := Load(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !p.Cleanup.CleanupAlways() {
				t.Error("example plans must always clean up after themselves")
			}
			if p.Restore.Network != "isolated" {
				t.Errorf("example plan restores onto %q, want the isolated network", p.Restore.Network)
			}
		})
	}
}
