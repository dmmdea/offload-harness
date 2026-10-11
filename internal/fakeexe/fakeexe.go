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
// A copy has a price the link did not: the first start of a newly written executable is slow where a
// scanner inspects it (measured on a development host: 1.7 to 2 s for the first start, 30 ms for the second),
// and a test that gives the whole call a short deadline would pay it inside its budget. So Install on
// Windows starts the stand-in once, with a file in its own directory as the only argument, before it
// returns. That asks one thing of a package's stand-in: started under the tool's name it treats its last
// argument as the file to write, as the stand-ins here do, and whatever else it does is harmless at that moment.
//
// It is a separate package so a production package never imports "testing" (the shape
// internal/gpuprobe/smitest has).
package fakeexe

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Dir makes a new empty directory for stand-in executables and removes it when the test ends. It is not
// t.TempDir: the executables are removed here, with a bounded retry on Windows, so testing's own TempDir
// cleanup never meets one.
func Dir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lo-fakeexe-*")
	if err != nil {
		t.Fatalf("fakeexe: %v", err)
	}
	t.Cleanup(func() { removeAll(t, dir) })
	return dir
}

// Install puts a stand-in for the tool called name in dir and returns its path. The name is the tool's
// name without a suffix ("ffmpeg"); ".exe" is added on Windows, where a file must have it to be run.
func Install(t testing.TB, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("fakeexe: the test binary's path: %v", err)
	}
	dst := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(strings.ToLower(dst), ".exe") {
			dst += ".exe"
		}
	} else if os.Link(self, dst) == nil {
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

// warm starts the stand-in once and waits for it, so that the slow first start of a new executable is not
// paid inside a test's own time budget. What it exits with does not matter, and a start that takes longer
// than 30 s is abandoned.
func warm(exe string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, exe, filepath.Join(filepath.Dir(exe), warmupFile)).Run()
}

// warmupFile is the file the warm-up start is told to write, in the stand-in's own directory.
const warmupFile = "warm-up.out"

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

// removeAll removes dir. On Windows a refusal is retried for up to 5 s with a growing pause, because a
// just-written executable can stay held for a moment by a scanner; what is still there after that is
// reported, as testing's own TempDir cleanup would.
func removeAll(t testing.TB, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	pause := 10 * time.Millisecond
	for {
		err := os.RemoveAll(dir)
		if err == nil {
			return
		}
		if runtime.GOOS != "windows" || time.Now().After(deadline) {
			t.Errorf("fakeexe: removing %s: %v", dir, err)
			return
		}
		time.Sleep(pause)
		if pause < 500*time.Millisecond {
			pause *= 2
		}
	}
}
