package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// escapesRoot matches os.Root's error text, so a Go release that rewords it fails here.
func TestEscapesRootMatchesOSRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for _, name := range []string{"../x", "out/x"} {
		if _, err := r.Open(name); !escapesRoot(err) {
			t.Errorf("Open(%q) = %v, not recognized as escaping the root", name, err)
		}
	}
	if _, err := r.Open("missing"); escapesRoot(err) {
		t.Errorf("a missing file was taken for an escape: %v", err)
	}
}
