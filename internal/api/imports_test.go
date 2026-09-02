package api

// The boundary that keeps the master key out of the package that serves HTTP.
//
// internal/api never imports internal/crypto or internal/providers directly.
// Everything it needs from them arrives through an interface the CLI
// implements - ProviderSet for the live clients, Setup for first-run
// provisioning - so a handler cannot reach a sealed secret even by accident.
//
// The test reads this package's own import statements on purpose. Transitively
// this package does reach both, through config, diag and worker; a test
// written against `go list -deps` would be red on the day it was written, and
// the next person would weaken it until it passed.

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAPIDoesNotImportCryptoOrProviders(t *testing.T) {
	forbidden := func(path string) bool {
		return path == "github.com/restorelab/restorelab/internal/crypto" ||
			strings.HasPrefix(path, "github.com/restorelab/restorelab/internal/providers")
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found: this test is looking in the wrong place")
	}

	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", name, spec.Path.Value, err)
			}
			if forbidden(path) {
				t.Errorf("%s imports %s.\n"+
					"internal/api must reach crypto and providers only through an "+
					"interface the CLI implements - see ProviderSet and Setup.",
					name, path)
			}
		}
	}
}
