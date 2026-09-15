//go:build windows

package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteDoorRefusesAWindowsJunctionEscape is the WINDOWS half of the
// confinement proof, and it is deliberately a REAL SPAWN of the real OS tool:
// a directory junction (a reparse point) is the Windows escape that a
// hand-rolled string check misses, and it cannot be faked — mklink's junction
// is a filesystem object with kernel semantics, and a mocked one would prove
// nothing about the door.
//
// `cmd /c mklink /J` needs no administrator rights (unlike a symbolic link),
// which is why the junction and not the symlink is the shape a non-privileged
// process on this OS would actually use to climb out.
//
// The Linux twin of this test is in agentwrite_linux_test.go and uses a
// symlink; each file is skipped on the other OS by its build tag.
func TestWriteDoorRefusesAWindowsJunctionEscape(t *testing.T) {
	read := t.TempDir()
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary.txt")
	if err := os.WriteFile(canary, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(read, "escape")
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput()
	if err != nil {
		t.Skipf("mklink /J unavailable here (%v): %s", err, out)
	}
	// Prove the junction actually REDIRECTS before relying on it. Go's Lstat
	// reports a junction as a plain directory (it behaves like one), so the
	// only honest check is behavioural: write through the link and see the
	// bytes land on the other side. If they do not, this test would prove
	// nothing and must say so rather than pass.
	probe := filepath.Join(link, "probe.txt")
	if perr := os.WriteFile(probe, []byte("probe"), 0o644); perr != nil {
		t.Skipf("cannot write through the junction (%v) — nothing to prove", perr)
	}
	if _, serr := os.Stat(filepath.Join(outside, "probe.txt")); serr != nil {
		t.Skipf("mklink reported success but %s does not redirect (%v) — nothing to prove", link, serr)
	}
	if rerr := os.Remove(probe); rerr != nil {
		t.Fatal(rerr)
	}

	// The door must refuse the junction itself as a write_root...
	if _, derr := openWriteDoor(read, writeContract("escape")); derr == nil {
		t.Fatal("a junction pointing OUT of the read root was accepted as write_root — the write tools would then be confined to the wrong tree")
	} else if !strings.Contains(derr.Error(), "OUTSIDE") && !strings.Contains(derr.Error(), "write_root") {
		t.Errorf("refusal %q does not say what was wrong", derr.Error())
	}

	// ...and nothing outside may be touched on the way to finding that out.
	got, rerr := os.ReadFile(canary)
	if rerr != nil || string(got) != "untouched\n" {
		t.Fatalf("the file outside the read root changed: %q (%v)", string(got), rerr)
	}
}

// TestWriteDoorRefusesAJunctionNESTEDInsideTheWriteRoot: the junction does not
// have to BE the write root to be an escape — one sitting inside it is a path
// the write tools could follow out. os.Root rejects reparse points during
// traversal, so a write through it must fail rather than land outside.
func TestWriteDoorRefusesAJunctionNestedInsideTheWriteRoot(t *testing.T) {
	read := t.TempDir()
	outside := t.TempDir()

	door, err := openWriteDoor(read, writeContract("work"))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(door.root, "out")
	if out, lerr := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); lerr != nil {
		t.Skipf("mklink /J unavailable here (%v): %s", lerr, out)
	}

	tools, terr := buildWriteToolsForTest(door)
	if terr != nil {
		t.Fatal(terr)
	}
	res, werr := tools.write(t, `{"path":"out/landed.txt","content":"escaped"}`)
	if werr == nil && !strings.Contains(res, "NOT performed") {
		if _, serr := os.Stat(filepath.Join(outside, "landed.txt")); serr == nil {
			t.Fatal("a write followed a junction OUT of write_root and landed outside the tree")
		}
		t.Fatalf("the write through a junction was not refused: %q", res)
	}
}

// TestVerifyWriteRootInsideRefusesAJunction exercises the door's write-root
// verification directly, rather than only through openWriteDoor where MkdirAll
// happens to refuse first: a guard whose only coverage is another guard firing
// before it is an untested guard.
//
// It is also the regression test for the implementation this replaced, which
// compared filepath.EvalSymlinks'd paths — and passed a junction escape,
// because Go reports a junction as an ordinary directory and EvalSymlinks does
// not follow it. That version was green on Linux and inert on Windows.
func TestVerifyWriteRootInsideRefusesAJunction(t *testing.T) {
	read := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(read, "escape")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable here (%v): %s", err, out)
	}
	probe := filepath.Join(link, "probe.txt")
	if perr := os.WriteFile(probe, []byte("probe"), 0o644); perr != nil {
		t.Skipf("cannot write through the junction (%v) — nothing to prove", perr)
	}
	if _, serr := os.Stat(filepath.Join(outside, "probe.txt")); serr != nil {
		t.Skipf("the junction does not redirect (%v) — nothing to prove", serr)
	}

	root, oerr := os.OpenRoot(read)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer root.Close()

	if err := verifyWriteRootInside(root, "escape"); err == nil {
		t.Fatal("a junction that redirects OUTSIDE the read root was accepted as a write root")
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
