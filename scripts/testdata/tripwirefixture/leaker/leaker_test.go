package leaker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLeavesThreeFolders is the defect: three folders in the OS temp dir, none removed. The tripwire reports them as
// "tripwire-leak-*" with a count of three.
func TestLeavesThreeFolders(t *testing.T) {
	for i := 0; i < 3; i++ {
		dir, err := os.MkdirTemp("", "tripwire-leak-")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("left behind"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRemovesItsFolder is the control: a folder made the same way and removed on every path leaves no entry, so the count
// above is exactly the three that were left.
func TestRemovesItsFolder(t *testing.T) {
	dir, err := os.MkdirTemp("", "tripwire-clean-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
}
