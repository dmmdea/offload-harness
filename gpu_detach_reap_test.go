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
	proc := cmd.Process
	if err := proc.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := proc.Kill(); err == nil {
		t.Skip("this platform still kills through a released handle; the ordering fix is a no-op here")
	}
	// The child outlived a Kill that reported failure. Clean it up out of band.
	if p, err := os.FindProcess(cmd.Process.Pid); err == nil {
		_ = p.Kill()
	}
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
