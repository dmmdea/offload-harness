//go:build !windows

package gpugen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func pidGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		// a zombie child of this test process still answers signal 0: treat it as gone
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && strings.Contains(string(b), ") Z") {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// A runner started with OwnProcessGroup that times out has its WHOLE group signalled, so a
// grandchild (the engine) is dead when Generate returns, not left holding the GPU.
func TestOwnProcessGroupTimeoutKillsTheGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gc.pid")
	out := filepath.Join(dir, "out")
	start := time.Now()
	_, err := Generate(context.Background(), Spec{
		Exe: "sh", Script: "-c", Args: []string{"sleep 60 & echo $! > " + pidFile + "; wait"},
		Out: out, Timeout: 700 * time.Millisecond, SkipFreeComfy: true, OwnProcessGroup: true,
	})
	if err == nil || ClassifyErr(err) != "timeout" {
		t.Fatalf("want a timeout, got %v (class %q)", err, ClassifyErr(err))
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("the kill took %v", time.Since(start))
	}
	if !pidGone(readPid(t, pidFile)) {
		t.Error("the grandchild survived the group kill")
	}
}

// A runner that ignores SIGTERM (or is blocked) is SIGKILLed with its group after the grace.
func TestOwnProcessGroupEscalatesToSIGKILL(t *testing.T) {
	old := termGrace
	termGrace = 300 * time.Millisecond
	t.Cleanup(func() { termGrace = old })
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gc.pid")
	_, err := Generate(context.Background(), Spec{
		Exe: "sh", Script: "-c", Args: []string{"trap '' TERM; (trap '' TERM; sleep 60) & echo $! > " + pidFile + "; while true; do sleep 1; done"},
		Out: filepath.Join(dir, "out"), Timeout: 500 * time.Millisecond, SkipFreeComfy: true, OwnProcessGroup: true,
	})
	if err == nil {
		t.Fatal("want a timeout")
	}
	if !pidGone(readPid(t, pidFile)) {
		t.Error("a TERM-ignoring grandchild must be SIGKILLed after the grace")
	}
}

// Context cancellation (a client cancel, the harness shutting down) takes the same path as a timeout.
func TestOwnProcessGroupCancelKillsTheGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gc.pid")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ {
			if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		cancel()
	}()
	_, err := Generate(ctx, Spec{
		Exe: "sh", Script: "-c", Args: []string{"sleep 60 & echo $! > " + pidFile + "; wait"},
		Out: filepath.Join(dir, "out"), Timeout: time.Minute, SkipFreeComfy: true, OwnProcessGroup: true,
	})
	if err == nil {
		t.Fatal("a cancelled run must fail")
	}
	if !pidGone(readPid(t, pidFile)) {
		t.Error("the grandchild survived the cancel")
	}
}

// Without OwnProcessGroup nothing changes: the child is not a group leader, killTree falls
// back to the direct kill (the browse lane and every pre-existing lane).
func TestKillTreeOnANonLeaderKillsTheProcessItself(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if err := killTree(cmd.Process); err != nil {
		t.Fatalf("killTree: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a non-leader process must be killed directly")
	}
}

func TestSetProcessGroupMakesTheChildItsOwnLeader(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		t.Fatalf("pgid = %d (err %v), want the child's own pid %d", pgid, err, cmd.Process.Pid)
	}
}

// TST24: the production grace is 5 s (the tests shorten the var, so a typo in the default would
// otherwise go unseen) - it must be shorter than the runner's own wait for its engine (3 s) is long
// enough, and longer than that wait.
func TestTermGraceDefaultIsFiveSeconds(t *testing.T) {
	if termGrace != 5*time.Second {
		t.Fatalf("termGrace = %v, want 5s: the runner waits up to 3 s for its engine after a SIGTERM, so the grace must exceed that", termGrace)
	}
}

// TST24: the SIGKILL escalation fires only while the process is still alive. An exited and reaped
// process may have had its pid recycled by a stranger's group, so a late SIGKILL of -pid would hit it.
func TestKillTreeDoesNotEscalateAgainstAnExitedProcess(t *testing.T) {
	old, oldFn := termGrace, killGroupFn
	termGrace = 300 * time.Millisecond
	var mu sync.Mutex
	var sent []syscall.Signal
	killGroupFn = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		sent = append(sent, sig)
		mu.Unlock()
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { termGrace, killGroupFn = old, oldFn })

	cmd := exec.Command("sh", "-c", `trap 'exit 0' TERM; while true; do sleep 0.1; done`)
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	time.Sleep(300 * time.Millisecond) // let sh install its trap
	if err := killTree(cmd.Process); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the process did not exit on SIGTERM")
	}
	time.Sleep(3 * termGrace) // well past the grace: the escalation timer has fired by now
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Fatalf("signals sent to the group = %v, want only SIGTERM (no SIGKILL to an exited process's group)", sent)
	}
}
