//go:build windows

package proctree

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// A job object that refuses the child (an enclosing job that cannot be nested, an access
// failure) must not fail the job it wraps: the command runs uncontained, the tree says so, and
// the wrapper falls back to what it can enumerate itself.
func TestACommandThatCannotBeAssignedToAJobStillRunsAndSaysSo(t *testing.T) {
	orig := assignProcess
	assignProcess = func(job, proc windows.Handle) error { return syscall.Errno(5) }
	t.Cleanup(func() { assignProcess = orig })

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := winexec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
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

// The command is created SUSPENDED and resumed only once it is inside the job, so it cannot spawn
// a process that escapes the job first. The assignment is slowed down here and the child must not
// have run at all by the time it happens.
func TestTheChildDoesNotRunUntilItIsInsideTheJob(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ranEarly := false
	orig := assignProcess
	assignProcess = func(job, proc windows.Handle) error {
		time.Sleep(900 * time.Millisecond) // a running child would have started its own child by now
		if _, err := os.Stat(pidFile); err == nil {
			ranEarly = true
		}
		return orig(job, proc)
	}
	t.Cleanup(func() { assignProcess = orig })
	cmd := winexec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
	hideChild(cmd)
	cmd.Env = append(os.Environ(), "PROCTREE_HELPER=parent", "PROCTREE_PIDFILE="+pidFile)
	tr, err := Start(cmd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tr.Terminate()
		_ = tr.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if ranEarly {
		t.Fatal("the command ran before it was inside the job object: whatever it spawned first would have escaped")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			return // and it runs once resumed
		}
		if time.Now().After(deadline) {
			t.Fatal("the command was never resumed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Only a member of the job is ever ended by a protected stop: a process that is not in it (a pid
// reused since the list was read) is left alone.
func TestKillMemberLeavesAProcessThatIsNotInTheJob(t *testing.T) {
	foreign := winexec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
	hideChild(foreign)
	foreign.Env = append(os.Environ(), "PROCTREE_HELPER=sleeper")
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = foreign.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = foreign.Process.Kill(); <-reaped })
	job, err := NewJob(JobLimits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Close() })
	if err := job.killMember(foreign.Process.Pid); err != nil {
		t.Fatalf("killMember: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !alive(foreign.Process.Pid) {
		t.Fatal("a process that is not in the job was ended")
	}
}

// Disarming the kill-on-close is what lets a spared member outlive the wrapper's handle: the job stops killing on close,
// the tree is still listed, and closing the handle ends nothing.
func TestDisarmKillOnCloseLetsTheJobOutliveItsHandle(t *testing.T) {
	tr, parent, grand := treeFixture(t, Options{DieWithWrapper: true})
	if !tr.KillsOnClose() {
		t.Fatal("setup: a DieWithWrapper job kills on close")
	}
	if err := tr.DisarmKillOnClose(); err != nil {
		t.Fatalf("DisarmKillOnClose: %v", err)
	}
	if tr.KillsOnClose() {
		t.Fatal("the job still kills on close after it was disarmed")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if !alive(parent) || !alive(grand) {
		t.Fatalf("closing a disarmed job ended its processes (command alive=%v, grandchild alive=%v)", alive(parent), alive(grand))
	}
}

// A job that never killed on close has nothing to disarm, and an uncontained tree is not an error.
func TestDisarmKillOnCloseIsANoOpWhereNothingKills(t *testing.T) {
	tr, _, _ := treeFixture(t, Options{})
	if err := tr.DisarmKillOnClose(); err != nil {
		t.Fatalf("a job that does not kill on close: %v", err)
	}
	var none Tree
	if err := none.DisarmKillOnClose(); err != nil {
		t.Fatalf("an uncontained tree: %v", err)
	}
}

// The caller that asked for --die-with-wrapper asked for a guarantee, and a command that could not be put in the job has none:
// it is refused (and the suspended child stopped), exactly as when the job object cannot be created at all. Running it
// uncontained with a warning would leave a render alive after the wrapper dies, which is what the flag exists to prevent.
func TestACommandThatCannotBeAssignedIsRefusedWhenDieWithWrapperWasAsked(t *testing.T) {
	orig := assignProcess
	assignProcess = func(job, proc windows.Handle) error { return syscall.Errno(5) }
	t.Cleanup(func() { assignProcess = orig })

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := winexec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
	hideChild(cmd)
	cmd.Env = append(os.Environ(), "PROCTREE_HELPER=parent", "PROCTREE_PIDFILE="+pidFile)
	tr, err := Start(cmd, Options{DieWithWrapper: true})
	if err == nil {
		_ = tr.Terminate()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		t.Fatalf("a command that cannot be contained was started although --die-with-wrapper was asked for (warning %q)", tr.Warning())
	}
	if !strings.Contains(err.Error(), "die-with-wrapper") {
		t.Errorf("the refusal names the flag it could not honour: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("setup: the child was never started, so the test proves nothing about stopping it")
	}
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the suspended child was left behind after the refusal")
	}
	if _, serr := os.Stat(pidFile); serr == nil {
		t.Error("the refused command ran far enough to start its own child")
	}
}
