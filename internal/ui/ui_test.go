package ui

import (
	"io/fs"
	"testing"
)

// Le contrat minimal : FS() rend un système de fichiers utilisable, même
// quand le dashboard n'a pas été compilé. C'est ce qui permet à
// `go build ./...` et à `go test ./...` de tourner sans Node.
func TestFSIsUsableWithoutABuild(t *testing.T) {
	f := FS()
	if f == nil {
		t.Fatal("FS() returned nil")
	}
	if _, err := fs.ReadDir(f, "."); err != nil {
		t.Fatalf("the embedded dist is not readable: %v", err)
	}
}

// Built dit la vérité dans les deux cas, et ne panique dans aucun.
func TestBuiltMatchesThePresenceOfIndexHTML(t *testing.T) {
	_, err := fs.Stat(FS(), "index.html")
	if got, want := Built(), err == nil; got != want {
		t.Fatalf("Built() = %v, want %v (index.html present: %v)", got, want, err == nil)
	}
}
