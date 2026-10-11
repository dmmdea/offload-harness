package fakeexe

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test binary answers as the stand-in tool when it is started under the name "f67standin" (so the
// stand-in is exercised for real: it is run, then removed). Like the stand-ins it models, it writes its last
// argument, when it is given one.
func init() {
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	if stem != "f67standin" {
		return
	}
	if len(os.Args) > 1 {
		_ = os.WriteFile(os.Args[len(os.Args)-1], []byte("ran"), 0o644)
	}
	os.Stdout.WriteString("stand-in ran")
	os.Exit(0)
}

// A stand-in runs, and its directory is gone when its test ends (the cleanup is the point: a stand-in
// that Windows will not unlink fails here after the retry window, as it failed testing's own cleanup).
func TestAStandInRunsAndItsDirectoryIsRemovedWhenItsTestEnds(t *testing.T) {
	var exe string
	t.Run("use", func(t *testing.T) {
		exe = Place(t, "f67standin")
		out, err := exec.Command(exe).CombinedOutput()
		if err != nil || string(out) != "stand-in ran" {
			t.Fatalf("running %s: %q, %v, want the stand-in's answer", exe, out, err)
		}
	})
	if _, err := os.Stat(filepath.Dir(exe)); !os.IsNotExist(err) {
		t.Errorf("the stand-in's directory %q is still there after its test ended (stat err %v)", filepath.Dir(exe), err)
	}
}

// On Windows the stand-in must not be the running test binary under another name: a link to it can never
// be unlinked while the test process lives, on a host where Windows enforces that. This holds on every
// Windows host, including those where unlinking such a link happens to work.
func TestOnWindowsAStandInIsACopyAndNotALinkOfTheRunningBinary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("off Windows the stand-in is a hard link, which unlinks fine")
	}
	exe := Place(t, "f67standin")
	if !strings.HasSuffix(exe, ".exe") {
		t.Errorf("stand-in %q has no .exe suffix on Windows", exe)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, b) {
		t.Fatalf("stand-in %q is a link to the running test binary %q", exe, self)
	}
	if a.Size() != b.Size() {
		t.Errorf("stand-in is %d bytes, the test binary %d: not a full copy", b.Size(), a.Size())
	}
}

// Install keeps the tool's name as given (plus .exe on Windows) and several stand-ins share a directory.
func TestInstallNamesTheFileAfterTheTool(t *testing.T) {
	dir := Dir(t)
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		got := Install(t, dir, name)
		if want := filepath.Join(dir, name+ext); got != want {
			t.Errorf("Install(%q) = %q, want %q", name, got, want)
		}
		if fi, err := os.Stat(got); err != nil || fi.Size() == 0 {
			t.Errorf("stand-in %q: stat %v, err %v", got, fi, err)
		}
	}
}

// On Windows Install starts the stand-in once before it returns (the slow first start of a new executable
// is then not paid inside a test's own time budget); the start is told to write a file in the stand-in's
// directory, which is how this test sees that it happened.
func TestOnWindowsInstallStartsTheStandInOnce(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("off Windows the stand-in is a hard link to a binary that has already run")
	}
	exe := Place(t, "f67standin")
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), warmupFile)); err != nil {
		t.Errorf("Install did not start the stand-in: %v", err)
	}
}
