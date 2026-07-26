package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestHelperSleep is the standard Go helper-process trick: it is a no-op unless the
// parent asks for it, and then it just stays alive long enough to be killed.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("LO_HELPER_SLEEP") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

// spawnHelper starts this test binary as a live child process.
func spawnHelper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleep", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "LO_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	return cmd
}

// alive reports whether pid still refers to a running process, by the same signal-0
// probe used elsewhere. On Windows FindProcess+Signal(0) is unreliable, so this waits
// on the handle instead.
func exited(cmd *exec.Cmd, within time.Duration) bool {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// detachHolder spawns a hidden child that owns the lease and MUST reap it on every path
// that does not report success — a surviving child takes the card as soon as the winner
// releases and holds it for the whole --for window, while the operator was told the
// reservation failed.
//
// This asserts the platform fact the guard depends on: Process.Release() invalidates the
// handle, so a Kill() issued afterwards fails and the child lives on. The guard was
// present and dead for exactly that reason.
func TestReleasedHandleCannotReapTheChild(t *testing.T) {
	cmd := spawnHelper(t)

	// CAPTURE THE PID BEFORE Release(). Release sets Process.Pid to -1, and on Unix
	// os.FindProcess(-1) succeeds, so killing through it issues kill(-1, SIGKILL) —
	// "signal every process this user may signal", i.e. the test runner and everything
	// else on the machine. Windows hides that: OpenProcess(-1) fails, so FindProcess
	// returns an error and the kill is skipped.
	pid := cmd.Process.Pid
	if pid <= 0 {
		t.Fatalf("helper has no usable pid: %d", pid)
	}

	proc := cmd.Process
	if err := proc.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := proc.Kill(); err == nil {
		reapByPID(t, pid)
		t.Skip("this platform still kills through a released handle; the ordering fix is a no-op here")
	}
	// The child outlived a Kill that reported failure — which is the whole point.
	// Clean it up out of band, by the pid we saved rather than the invalidated handle.
	reapByPID(t, pid)
}

// reapByPID kills a process by pid, refusing any non-positive value: on Unix those are
// process-GROUP and broadcast selectors, never a single child.
func reapByPID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		t.Fatalf("refusing to signal pid %d — that is a broadcast, not a process", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
	// The parent never Wait()s a released process, so on Unix the corpse lingers as a
	// zombie until this test binary exits and init reaps it. Harmless and short-lived.
}

// And the fix: with the handle retained, Kill actually reaps the child.
func TestRetainedHandleReapsTheChild(t *testing.T) {
	cmd := spawnHelper(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill through a retained handle failed: %v — detachHolder's cleanup would leak a holder", err)
	}
	if !exited(cmd, 10*time.Second) {
		t.Fatal("the child survived Kill; a failed reservation would leave a holder on the card")
	}
}
