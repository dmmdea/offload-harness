//go:build windows

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A job object that refuses the child (an enclosing job that cannot be nested, an access
// failure) must not fail the job it wraps: the command runs uncontained, the tree says so, and
// the wrapper falls back to what it can enumerate itself.
func TestACommandThatCannotBeAssignedToAJobStillRunsAndSaysSo(t *testing.T) {
	orig := assignProcess
	assignProcess = func(job, proc windows.Handle) error { return syscall.Errno(5) }
	t.Cleanup(func() { assignProcess = orig })

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
	hideChild(cmd)
	cmd.Env = append(os.Environ(), "PROCTREE_HELPER=parent", "PROCTREE_PIDFILE="+pidFile)
	tr, err := Start(cmd, Options{})
	if err != nil {
		t.Fatalf("an uncontainable command must still start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if b, rerr := os.ReadFile(pidFile); rerr == nil {
			if p, perr := os.FindProcess(atoi(string(b))); perr == nil {
				_ = p.Kill()
			}
		}
	})
	if tr.Contained() || !strings.Contains(tr.Warning(), "could not be put in a job object") {
		t.Fatalf("contained=%v warning=%q: it must say it runs uncontained", tr.Contained(), tr.Warning())
	}
	if tr.Mechanism() != "none" {
		t.Fatalf("mechanism = %q", tr.Mechanism())
	}
	// It was resumed: the suspended child ran far enough to report its own child.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never ran: a child that could not be assigned was left suspended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := tr.Terminate(); err != ErrNotContained {
		t.Fatalf("Terminate on an uncontained tree = %v, want ErrNotContained", err)
	}
	if _, err := tr.PIDs(); err != ErrNotContained {
		t.Fatalf("PIDs on an uncontained tree = %v, want ErrNotContained", err)
	}
}

// KILL_ON_JOB_CLOSE is set exactly when asked for.
func TestKillOnCloseIsSetOnlyWhenAskedFor(t *testing.T) {
	for _, want := range []bool{false, true} {
		tr, _, _ := treeFixture(t, Options{DieWithWrapper: want})
		if got := tr.KillsOnClose(); got != want {
			t.Fatalf("DieWithWrapper=%v: the job kills on close = %v", want, got)
		}
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
