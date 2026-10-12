package clean

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScratchComesFromTestingTempDir uses scratch space the way a hygienic test does:
// the folder is removed when the test ends.
func TestScratchComesFromTestingTempDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
