package fakeexe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// probeEnv makes TestNestedProbe do its work. Only the child that TestAStandInThatRunsTestsRefusesToInstallAnother
// starts sets it, and that child is started with -test.run for the probe alone.
const probeEnv = "LO_FAKEEXE_TEST_PROBE"

// The test binary answers as the stand-in tool when it is started under one of the names this file's tests
// install ("ffmpeg", "ffprobe", "standin"), so every stand-in these tests make is a leaf: it writes its last
// argument when it is given one, prints, and exits. Nothing here starts a stand-in under a name this does
// not answer for, except the one test that does it on purpose with the probe above and nothing else to run.
func init() {
	if warmStart(os.Getenv) {
		// This package's own init has already exited a warm-up start; reaching here means that guard broke.
		// Exit loudly instead of running tests, so that the test that watches for it fails and nothing recurses.
		fmt.Fprintln(os.Stderr, "BUG: a warm-up start reached the test package's init")
		os.Exit(3)
	}
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	switch stem {
	case "ffmpeg", "ffprobe", "standin":
		if len(os.Args) > 1 {
			_ = os.WriteFile(os.Args[len(os.Args)-1], []byte("ran"), 0o644)
		}
		os.Stdout.WriteString("stand-in ran")
		os.Exit(0)
	}
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// A stand-in runs, and its directory is gone when its test ends (the cleanup is the point: a stand-in
// that Windows will not unlink fails here after the retry window, as it failed testing's own cleanup).
func TestAStandInRunsAndItsDirectoryIsRemovedWhenItsTestEnds(t *testing.T) {
	var exe string
	t.Run("use", func(t *testing.T) {
		exe = Place(t, "standin")
		out, err := exec.Command(exe).CombinedOutput()
		if err != nil || string(out) != "stand-in ran" {
			t.Fatalf("running %s: %q, %v, want the stand-in's answer", exe, out, err)
		}
	})
	if _, err := os.Stat(filepath.Dir(exe)); !os.IsNotExist(err) {
		t.Errorf("the stand-in's directory %q is still there after its test ended (stat err %v)", filepath.Dir(exe), err)
	}
}

// The folder of a helper call is inside the calling test's own temp directory (the one testing makes for
// t.TempDir and removes when the test ends), not a folder of its own in the shared temp root: a test run that
// is killed before its cleanups run (a timeout, a guard, a crash) then leaves it inside the run's temp root, and
// a harness that deletes that root deletes it. A helper that made its own folder in the temp root
// (MkdirTemp("", ...)) leaves one per call there whenever a run is killed. (This cannot be seen by pointing
// TMP elsewhere: testing fixes its temp base when the binary starts.) The test's directory is also gone,
// with the helper's folders, once the test has ended.
func TestAHelperCallStaysInsideTheTestsOwnTempDirAndIsGoneWhenItsTestEnds(t *testing.T) {
	var home, dir, place string
	t.Run("use", func(t *testing.T) {
		home = filepath.Dir(t.TempDir()) // the test's own temp directory; its folders are numbered 001, 002, ...
		dir = Dir(t)
		Install(t, dir, "ffmpeg")
		Install(t, dir, "ffprobe")
		place = Place(t, "standin")
		for _, d := range []string{dir, filepath.Dir(place)} {
			if filepath.Dir(d) != home {
				t.Errorf("the helper's folder %q is not inside the test's temp directory %q: a killed run would leave it in %q",
					d, home, filepath.Dir(d))
			}
		}
		if dir == filepath.Dir(place) {
			t.Errorf("Dir and Place returned the same folder %q", dir)
		}
	})
	for _, p := range []string{dir, filepath.Dir(place), home} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%q is still there after the test that used the helper ended (stat err %v)", p, err)
		}
	}
}

// On Windows the stand-in must not be the running test binary under another name: a link to it can never
// be unlinked while the test process lives, on a host where Windows enforces that. This holds on every
// Windows host, including those where unlinking such a link happens to work.
func TestOnWindowsAStandInIsACopyAndNotALinkOfTheRunningBinary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("off Windows the stand-in is a hard link, which unlinks fine")
	}
	exe := Place(t, "standin")
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
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		got := Install(t, dir, name)
		if want := filepath.Join(dir, exeName(name)); got != want {
			t.Errorf("Install(%q) = %q, want %q", name, got, want)
		}
		if fi, err := os.Stat(got); err != nil || fi.Size() == 0 {
			t.Errorf("stand-in %q: stat %v, err %v", got, fi, err)
		}
	}
}

// On Windows Install starts the stand-in once before it returns (the slow first start of a new executable
// is then not paid inside a test's own time budget), and the executable runs afterwards.
func TestOnWindowsInstallStartsTheStandInOnce(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("off Windows the stand-in is a hard link to a binary that has already run")
	}
	before := warmed.Load()
	exe := Place(t, "standin")
	if got := warmed.Load() - before; got != 1 {
		t.Errorf("Install made %d warm-up starts, want 1", got)
	}
	out, err := exec.Command(exe).CombinedOutput()
	if err != nil || string(out) != "stand-in ran" {
		t.Fatalf("the stand-in does not run after Install's warm-up: %q, %v", out, err)
	}
}

// copyTo puts a copy of the running test binary in a fresh directory under name and returns its path.
func copyTo(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), exeName(name))
	if err := copyFile(self, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// A warm-up start ends in this package's init: no output, status 0, no test run, whatever the stand-in's
// own dispatcher would have done, so a package whose dispatcher does not know the name cannot recurse
// through the warm-up. If the guard broke, the start would reach this file's init, which exits with 3 and
// says so (it does not run tests), so this test fails instead of starting a chain of test runs.
func TestAWarmUpStartExitsBeforeAnyTestRuns(t *testing.T) {
	// A name no dispatcher in this file answers for: only the warm-up guard can end this start quietly.
	exe := copyTo(t, "unanswered")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.v")
	cmd.Env = append(os.Environ(), warmEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	err := cmd.Run()
	if err != nil || out.Len() != 0 {
		t.Fatalf("a warm-up start ended with %v and printed %q, want status 0 and nothing", err, out.String())
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("a warm-up start took %v", d)
	}
}

// The predicates of the two guards, without starting anything.
func TestTheGuardsRecogniseTheirOwnMarkers(t *testing.T) {
	getenv := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if warmStart(getenv(map[string]string{})) {
		t.Error("a process without warmEnv is not a warm-up start")
	}
	if !warmStart(getenv(map[string]string{warmEnv: "1"})) {
		t.Error("a process with warmEnv is a warm-up start")
	}
	a, b := t.TempDir(), t.TempDir()
	list := a + string(os.PathListSeparator) + "no-such-dir" + string(os.PathListSeparator) + b
	if !startedFrom(list, a) || !startedFrom(list, b) {
		t.Error("a process running from a listed directory is a stand-in")
	}
	if startedFrom(list, t.TempDir()) || startedFrom("", a) || startedFrom(list, filepath.Join(a, "missing")) {
		t.Error("a process running from an unlisted or missing directory is not a stand-in")
	}
	if got := addDir(addDir("", a), b); got != a+string(os.PathListSeparator)+b {
		t.Errorf("addDir built %q", got)
	}
	if got := addDir(a, a); got != a {
		t.Errorf("addDir listed a directory twice: %q", got)
	}
}

// Install records the directory it installs into, which is what a stand-in started later finds in its
// environment.
func TestInstallRecordsItsDirectoryForTheRecursionGuard(t *testing.T) {
	dir := Dir(t)
	Install(t, dir, "ffmpeg")
	Install(t, dir, "ffprobe")
	got := filepath.SplitList(os.Getenv(dirsEnv))
	if len(got) == 0 || got[len(got)-1] != dir {
		t.Errorf("%s = %q, want it to end with %q", dirsEnv, os.Getenv(dirsEnv), dir)
	}
	n := 0
	for _, d := range got {
		if d == dir {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%q is listed %d times in %q", dir, n, os.Getenv(dirsEnv))
	}
}

// TestNestedProbe is the body of the child TestAStandInThatRunsTestsRefusesToInstallAnother starts. On its
// own it does nothing.
func TestNestedProbe(t *testing.T) {
	if os.Getenv(probeEnv) == "" {
		t.Skip("only the child that the refusal test starts runs this")
	}
	Dir(t) // a stand-in that got here must be ended by this call
	fmt.Println("PROBE-SURVIVED")
}

// A stand-in that the package's dispatcher did not answer for runs tests. The child below is exactly that,
// kept harmless by -test.run (only the probe above can run, and it only calls Dir): it finds itself running
// from a directory the parent recorded and must be ended with status 97 and its explanation, not carry on.
func TestAStandInThatRunsTestsRefusesToInstallAnother(t *testing.T) {
	exe := copyTo(t, "unanswered")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNestedProbe$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), probeEnv+"=1", dirsEnv+"="+filepath.Dir(exe))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != nestedExit {
		t.Fatalf("the child ended with %v, want status %d; output:\n%s", err, nestedExit, out.String())
	}
	if strings.Contains(out.String(), "PROBE-SURVIVED") || !strings.Contains(out.String(), "refusing to install another stand-in") {
		t.Errorf("the child's output is not the refusal:\n%s", out.String())
	}
}

// removeAll retries a refusal on Windows for a bounded time. A file that is open without delete sharing
// (what Go's os.Open does, and what a scanner or a just-exited image looks like to a delete) refuses the
// removal until it is closed.
func TestOnWindowsRemoveAllRetriesAHeldFileAndIsBounded(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("off Windows an open file can be unlinked, so there is nothing to retry")
	}
	hold := func(t *testing.T) (dir string, f *os.File) {
		dir = t.TempDir()
		p := filepath.Join(dir, "held.exe")
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		return dir, f
	}

	dir, f := hold(t)
	time.AfterFunc(400*time.Millisecond, func() { f.Close() })
	start := time.Now()
	if err := removeAll(dir, 5*time.Second); err != nil {
		t.Fatalf("a file released after 400 ms was not removed within the window: %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 4*time.Second {
		t.Errorf("removal took %v, want it to wait for the release and not for the whole window", d)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%q is still there (stat err %v)", dir, err)
	}

	dir, f = hold(t)
	defer f.Close()
	start = time.Now()
	if err := removeAll(dir, 300*time.Millisecond); err == nil {
		t.Fatal("a file that is still held was reported removed")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("a refused removal retried for %v, want it bounded by the window", d)
	}
}
