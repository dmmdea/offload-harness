// Package fakeexe is test support for tests that need an executable named like a tool (ffmpeg, ffprobe)
// which is the running test binary itself, re-executed as the stand-in (the helper-process trick: the
// package's init or TestMain sees the tool's name in os.Args[0] and answers as the tool).
//
// It exists because four packages each carried the same helper, and it hard-linked the test binary to
// the tool's name. On Windows that cannot be cleaned up: a link names the same file as the running test
// binary, and Windows refuses to unlink any name of a file a live process has mapped as its image, so
// testing's TempDir cleanup failed with "unlinkat ...\ffmpeg.exe: Access is denied" on a host where the
// refusal applies (measured there: a link that nothing had ever run was still locked after 20 s, a
// byte copy was removed at once; on another Windows host both removed fine). A retry cannot help, because
// the lock lasts as long as the test process does. So on Windows the stand-in is always a copy, and
// elsewhere a hard link (free, and unlinking one is fine), falling back to a copy.
//
// Where the stand-ins live. Dir is the calling test's own t.TempDir, so a test run that is killed before
// its cleanups run leaves them inside the run's temp root (which a harness deletes), never as a folder of
// their own in the shared temp directory. Dir also registers its own removal, with a bounded retry on
// Windows (a just-run executable can stay held for a moment by a scanner). Cleanups run last-in first-out
// and Dir registers its after t.TempDir's, so the retry runs before testing's own TempDir removal, which
// then finds nothing to refuse.
//
// A copy has a price the link did not: the first start of a newly written executable is slow where a
// scanner inspects it (measured on a development host: 1.7 to 2 s for the first start, 30 ms for the second),
// and a test that gives the whole call a short deadline would pay it inside its budget. So Install on
// Windows starts the stand-in once before it returns. That start is a leaf by construction: it carries
// warmEnv, and this package's init exits the process the moment it sees it, before any package's own init
// or any test can run. It therefore asks nothing of a package's stand-in dispatcher.
//
// The recursion guard. A stand-in is the test binary, so a stand-in started under a name the package's
// dispatcher does not recognise runs the whole test suite instead of answering as the tool, and every test
// that installs a stand-in would start another one: a fork bomb, one directory and one process per level
// (a runaway of this kind has happened: thousands of copies in minutes). Install records every directory it
// puts a stand-in in, in dirsEnv, which child processes inherit. A process that finds itself running from
// one of those directories is a stand-in; if it reaches Dir or Install it is running tests, and it exits
// with nestedExit at once, so the depth of any such chain is two.
//
// It is a separate package so a production package never imports "testing" (the shape
// internal/gpuprobe/smitest has).
package fakeexe

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// warmEnv is set on the warm-up start. A process that finds it set exits at once (see init).
	warmEnv = "LO_FAKEEXE_WARM"
	// dirsEnv lists (a path list) the directories whose stand-ins this run has installed. Children inherit it.
	dirsEnv = "LO_FAKEEXE_DIRS"
	// nestedExit is the exit status of a stand-in that reached Dir or Install.
	nestedExit = 97
	// removeWindow is how long a directory's removal is retried on Windows.
	removeWindow = 5 * time.Second
)

// warmed counts the warm-up starts this process has made (the tests read it).
var warmed atomic.Int64

// nested is true in a process that is running from a directory this package installed a stand-in in: the
// test binary re-executed as a tool. It is read once, before any test sets dirsEnv for its own children.
var nested bool

func init() {
	if warmStart(os.Getenv) {
		os.Exit(0)
	}
	if self, err := os.Executable(); err == nil {
		nested = startedFrom(os.Getenv(dirsEnv), filepath.Dir(self))
	}
}

// warmStart reports whether this process is the warm-up start of a stand-in.
func warmStart(getenv func(string) string) bool { return getenv(warmEnv) != "" }

// startedFrom reports whether dir is one of the directories in list (a path list).
func startedFrom(list, dir string) bool {
	want, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for _, d := range filepath.SplitList(list) {
		if fi, err := os.Stat(d); err == nil && os.SameFile(fi, want) {
			return true
		}
	}
	return false
}

// refuseNested ends a process that is a stand-in running tests. See the recursion guard above.
func refuseNested() {
	if !nested {
		return
	}
	fmt.Fprintln(os.Stderr, "fakeexe: this process is a stand-in started from a directory fakeexe installed it in, and it is running tests, "+
		"so the package's init did not answer as the tool for the name it was started under; refusing to install another stand-in")
	os.Exit(nestedExit)
}

// Dir returns a new empty directory for stand-in executables. It is inside the calling test's own temp
// directory (t.TempDir) and is removed, with a bounded retry on Windows, when the test ends.
func Dir(t testing.TB) string {
	t.Helper()
	refuseNested()
	dir := t.TempDir()
	// Registered after t.TempDir's own removal, so it runs before it (last in, first out).
	t.Cleanup(func() {
		if err := removeAll(dir, removeWindow); err != nil {
			t.Errorf("fakeexe: removing %s: %v", dir, err)
		}
	})
	return dir
}

// Install puts a stand-in for the tool called name in dir and returns its path. The name is the tool's
// name without a suffix ("ffmpeg"); ".exe" is added on Windows, where a file must have it to be run.
func Install(t testing.TB, dir, name string) string {
	t.Helper()
	refuseNested()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("fakeexe: the test binary's path: %v", err)
	}
	dst := filepath.Join(dir, name)
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(dst), ".exe") {
		dst += ".exe"
	}
	// Recorded before the stand-in can be started by anything, for the recursion guard.
	t.Setenv(dirsEnv, addDir(os.Getenv(dirsEnv), dir))
	if runtime.GOOS != "windows" && os.Link(self, dst) == nil {
		return dst
	}
	if err := copyFile(self, dst); err != nil {
		t.Fatalf("fakeexe: copying the test binary to %s: %v", dst, err)
	}
	if runtime.GOOS == "windows" {
		warm(dst)
	}
	return dst
}

// addDir appends dir to a path list, once.
func addDir(list, dir string) string {
	for _, d := range filepath.SplitList(list) {
		if d == dir {
			return list
		}
	}
	if list == "" {
		return dir
	}
	return list + string(os.PathListSeparator) + dir
}

// warm starts the stand-in once and waits for it, so that the slow first start of a new executable is not
// paid inside a test's own time budget. The start exits in this package's init (warmEnv), so what the
// stand-in's own dispatcher would do does not matter; what it exits with does not either, and a start that
// takes longer than 30 s is abandoned.
func warm(exe string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), warmEnv+"=1")
	warmed.Add(1)
	_ = cmd.Run()
}

// Place is Install into a directory of its own (see Dir).
func Place(t testing.TB, name string) string {
	t.Helper()
	return Install(t, Dir(t), name)
}

// copyFile streams src to a new executable file at dst (a test binary is tens of MB; it is not read whole).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeAll removes dir. On Windows a refusal is retried for up to window with a growing pause, because a
// just-written executable can stay held for a moment by a scanner; what is still there after that is
// returned, as testing's own TempDir cleanup would report it.
func removeAll(dir string, window time.Duration) error {
	deadline := time.Now().Add(window)
	pause := 10 * time.Millisecond
	for {
		err := os.RemoveAll(dir)
		if err == nil {
			return nil
		}
		if runtime.GOOS != "windows" || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(pause)
		if pause < 500*time.Millisecond {
			pause *= 2
		}
	}
}
