//go:build !windows

package nodeswap

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A --restart-command on Linux runs through the POSIX shell. Until 0.143.0 it
// ran through `powershell`: "restart command failed: exec: "powershell":
// executable file not found in $PATH" on two Linux nodes, 2026-09-29.
func TestPlatformCommandRunsThroughTheShellOffWindows(t *testing.T) {
	out, err := runPlatformCommand(context.Background(), 10*time.Second, "echo restarted && echo $((1+1))")
	if err != nil {
		t.Fatalf("restart command failed: %v (%s)", err, out)
	}
	if !strings.Contains(out, "restarted") || !strings.Contains(out, "2") {
		t.Fatalf("output = %q, want the shell to have run both commands", out)
	}
}

// fakeProc builds a /proc-shaped tree: one dir per pid with an `exe` symlink
// and a NUL-separated `cmdline`.
func fakeProc(t *testing.T, root string, pid int, exe, cmdline string) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(d, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.ReplaceAll(cmdline, " ", "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Verification finds the restarted node by its executable — through a
// symlinked install path too — and never counts a process still running a
// replaced (deleted) image or another binary.
func TestFindRunningByExeProcFindsTheRestartedNode(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opt", "offload", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bin, "local-offload")
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "usr-local-bin-local-offload")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(bin, "llama-server")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(dir, "proc")
	fakeProc(t, proc, 4242, target, "/opt/offload/bin/local-offload fleet-serve --listen 192.0.2.10:18811")
	fakeProc(t, proc, 4243, target+" (deleted)", "/opt/offload/bin/local-offload fleet-serve")
	fakeProc(t, proc, 4244, other, "/opt/offload/bin/llama-server -m x.gguf")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil { // non-numeric entries are skipped
		t.Fatal(err)
	}

	for _, asked := range []string{target, link} {
		got, err := findRunningByExeProc(proc, asked)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].PID != 4242 || !strings.Contains(got[0].CommandLine, "fleet-serve --listen") {
			t.Fatalf("asked %s: got %+v, want exactly pid 4242 with its command line", asked, got)
		}
	}
}

// A node run by another user (EACCES on its /proc/<pid>/exe) is matched by
// its world-readable cmdline; when such an entry names no absolute
// executable and nothing else matched, the finder errors instead of reporting
// "no node" — an empty result there rolls a good swap back.
func TestFindRunningByExeProcSeesAnotherUsersNode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "local-offload")
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(dir, "proc")
	fakeProc(t, proc, 5100, target, target+" fleet-serve --listen 192.0.2.10:18811")
	fakeProc(t, proc, 5101, target, "local-offload fleet-serve")
	denied := map[string]bool{filepath.Join(proc, "5100", "exe"): true, filepath.Join(proc, "5101", "exe"): true}
	orig := procReadlink
	t.Cleanup(func() { procReadlink = orig })
	procReadlink = func(p string) (string, error) {
		if denied[p] {
			return "", &os.PathError{Op: "readlink", Path: p, Err: os.ErrPermission}
		}
		return orig(p)
	}

	got, err := findRunningByExeProc(proc, target)
	if err != nil {
		t.Fatalf("a denied exe with an absolute argv[0] must still match: %v", err)
	}
	if len(got) != 1 || got[0].PID != 5100 {
		t.Fatalf("got %+v, want exactly pid 5100 matched by its cmdline", got)
	}

	// Only the relative-argv[0] entry left: it cannot be ruled out, so the
	// finder must not say "no node".
	if err := os.RemoveAll(filepath.Join(proc, "5100")); err != nil {
		t.Fatal(err)
	}
	got, err = findRunningByExeProc(proc, target)
	if err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("got %+v, %v; want an error naming the unreadable entries", got, err)
	}
}

// platformDeps wires the verification finder off Windows (the holder finder
// stays empty — a Linux swap stops nothing).
func TestPlatformDepsWireAVerificationFinderOffWindows(t *testing.T) {
	var d Deps
	platformDeps(&d)
	if d.FindRunningByExe == nil {
		t.Fatal("platformDeps must set FindRunningByExe off Windows: verification after a restart cannot find the node otherwise")
	}
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc on this OS (darwin/BSD): the fleet's Linux nodes are the target")
	}
	self, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable")
	}
	got, err := d.FindRunningByExe(self)
	if err != nil {
		t.Fatalf("FindRunningByExe: %v", err)
	}
	found := false
	for _, p := range got {
		if p.PID == os.Getpid() {
			found = true
		}
	}
	if !found {
		t.Fatalf("the test binary itself (pid %d) was not found running %s: %+v", os.Getpid(), self, got)
	}
}
