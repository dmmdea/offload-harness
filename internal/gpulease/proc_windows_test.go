//go:build windows

package gpulease

import (
	"os"
	"os/exec"
	"testing"
)

// The cross-security-context case, pinned.
//
// PID 4 is the Windows System process: always present, and never openable by a normal
// user token. It is the cheapest deterministic stand-in for the real motivating case —
// a llama-swap registered as a SYSTEM service holding the card while a user-session
// process asks for it.
//
// This test FAILS against the previous implementation (os.FindProcess, which requests
// PROCESS_QUERY_INFORMATION and gets ACCESS_DENIED, reported as "no such process").
// That false "dead" reading hits Reclaimable's provably-gone fast path, which bypasses
// both the heartbeat and the declared window — so a user-context acquirer would take
// the GPU straight out from under a live SYSTEM holder.
func TestInaccessibleSystemProcessReadsAsAlive(t *testing.T) {
	if pidAlive(4) {
		return // correct: present-but-not-inspectable is ALIVE
	}
	// If we are running elevated, pid 4 may be genuinely openable and readable; only
	// treat it as a failure when the process really does exist.
	out, err := exec.Command("tasklist", "/FI", "PID eq 4", "/NH").Output()
	if err == nil && len(out) > 0 {
		t.Fatal("the System process (pid 4) read as DEAD; a cross-context holder would be " +
			"reclaimed immediately via the provably-gone fast path")
	}
	t.Skip("could not confirm pid 4 exists in this environment")
}

// The ordinary contract still has to hold: a live child is alive, and once it exits it
// is dead. Without this, "treat failures as alive" could silently degrade into "always
// alive", which would strand the GPU behind crashed holders forever.
func TestLiveChildIsAliveAndExitedChildIsDead(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "pause")
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		t.Skipf("could not spawn a probe child: %v", err)
	}
	pid := cmd.Process.Pid
	if !pidAlive(pid) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("a running child (pid %d) read as dead", pid)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if pidAlive(pid) {
		t.Fatalf("an exited child (pid %d) still reads as alive; crashed holders would never be reclaimed", pid)
	}
}

func TestNonPositivePidIsNeverAlive(t *testing.T) {
	if pidAlive(0) || pidAlive(-1) {
		t.Fatal("a non-positive pid must never read as alive")
	}
	if !pidAlive(os.Getpid()) {
		t.Fatal("our own process read as dead")
	}
}
