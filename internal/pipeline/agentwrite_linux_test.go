//go:build linux

package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteDoorRefusesASymlinkEscape is the LINUX half of the confinement
// proof. A symlink is this OS's escape shape: a write_root that IS one, and a
// write THROUGH one nested inside the write root, both have to be refused — the
// first because the write tools would otherwise open their os.Root on the wrong
// tree, the second because os.Root's traversal is what stops it.
//
// The Windows twin is in agentwrite_windows_test.go and uses a real `mklink /J`
// junction spawn; each file is skipped on the other OS by its build tag.
//
// Note on the mechanism, stated rather than implied: this door confines
// IN-PROCESS writes with os.Root, not with Landlock. Landlock (and the Windows
// job object) cage a CHILD PROCESS, and the write door spawns none — the
// sandbox package stays what it has always been, the cage for run_shell / run,
// neither of which this door grants.
func TestWriteDoorRefusesASymlinkEscape(t *testing.T) {
	read := t.TempDir()
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary.txt")
	if err := os.WriteFile(canary, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(read, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if _, derr := openWriteDoor(read, writeContract("escape")); derr == nil {
		t.Fatal("a symlink pointing OUT of the read root was accepted as write_root — the write tools would then be confined to the wrong tree")
	} else if !strings.Contains(derr.Error(), "OUTSIDE") && !strings.Contains(derr.Error(), "write_root") {
		t.Errorf("refusal %q does not say what was wrong", derr.Error())
	}

	got, rerr := os.ReadFile(canary)
	if rerr != nil || string(got) != "untouched\n" {
		t.Fatalf("the file outside the read root changed: %q (%v)", string(got), rerr)
	}
}

// TestWriteDoorRefusesASymlinkNestedInsideTheWriteRoot: the symlink does not
// have to BE the write root to be an escape. os.Root refuses to traverse one,
// so the write must be refused rather than land outside the tree.
func TestWriteDoorRefusesASymlinkNestedInsideTheWriteRoot(t *testing.T) {
	read := t.TempDir()
	outside := t.TempDir()

	door, err := openWriteDoor(read, writeContract("work"))
	if err != nil {
		t.Fatal(err)
	}
	if lerr := os.Symlink(outside, filepath.Join(door.root, "out")); lerr != nil {
		t.Skipf("cannot create a symlink here: %v", lerr)
	}

	tools, terr := buildWriteToolsForTest(door)
	if terr != nil {
		t.Fatal(terr)
	}
	res, werr := tools.write(t, `{"path":"out/landed.txt","content":"escaped"}`)
	if werr == nil && !strings.Contains(res, "NOT performed") {
		if _, serr := os.Stat(filepath.Join(outside, "landed.txt")); serr == nil {
			t.Fatal("a write followed a symlink OUT of write_root and landed outside the tree")
		}
		t.Fatalf("the write through a symlink was not refused: %q", res)
	}
}

// TestVerifyWriteRootInsideRefusesASymlink exercises the door's write-root
// verification directly, rather than only through openWriteDoor where MkdirAll
// happens to refuse first: a guard whose only coverage is another guard firing
// before it is an untested guard.
func TestVerifyWriteRootInsideRefusesASymlink(t *testing.T) {
	read := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(read, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	_ = link
	root, oerr := os.OpenRoot(read)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer root.Close()

	if err := verifyWriteRootInside(root, "escape"); err == nil {
		t.Fatal("a symlink pointing OUTSIDE the read root was accepted as a write root")
	}
	if merr := os.Mkdir(filepath.Join(read, "ordinary"), 0o755); merr != nil {
		t.Fatal(merr)
	}
	if err := verifyWriteRootInside(root, "ordinary"); err != nil {
		t.Errorf("an ordinary directory inside the root was refused: %v", err)
	}
	if werr := os.WriteFile(filepath.Join(read, "afile"), []byte("x"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if err := verifyWriteRootInside(root, "afile"); err == nil {
		t.Error("a FILE was accepted as a write root")
	}
}
