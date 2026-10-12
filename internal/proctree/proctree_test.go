package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// alive is the lease package's own liveness rule, so a test and the reclaim rule never disagree
// about what a running process is.
func alive(pid int) bool { return gpulease.PIDAlive(pid) }

// The helper process of these tests is this test binary, run in one of two roles. A "parent"
// starts a "sleeper" (its grandchild from the wrapper's point of view), writes the sleeper's pid
// to a file and sleeps; a "sleeper" only sleeps. The extra words follow a non-flag word so the
// testing package leaves them alone.
func TestHelperProcess(t *testing.T) {
	role := os.Getenv("PROCTREE_HELPER")
	if role == "" {
		t.Skip("helper process only")
	}
	if role == "parent" || role == "leaver" {
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
		hideChild(child)
		child.Env = append(os.Environ(), "PROCTREE_HELPER=sleeper")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(os.Getenv("PROCTREE_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		if role == "leaver" {
			os.Exit(0) // leaves its child running: the child's parent is gone
		}
	}
	time.Sleep(90 * time.Second)
}

// treeFixture starts a parent that starts a sleeping grandchild under containment, and returns
// the tree, the parent's pid and the grandchild's pid once the grandchild exists.
func treeFixture(t *testing.T, opts Options) (tr *Tree, parent, grand int) {
	t.Helper()
	return roleFixture(t, "parent", opts)
}

// roleFixture is treeFixture for a helper role ("parent" stays; "leaver" exits at once and leaves
// its child running).
func roleFixture(t *testing.T, role string, opts Options) (tr *Tree, parent, grand int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=120s")
	hideChild(cmd)
	cmd.Env = append(os.Environ(), "PROCTREE_HELPER="+role, "PROCTREE_PIDFILE="+pidFile)
	tr, err := Start(cmd, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The wrapper reaps its command (cmd.Wait); without it a killed command stays a zombie and
	// still answers a liveness probe.
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = tr.Terminate()
		_ = tr.Close()
		_ = cmd.Process.Kill()
		<-reaped
		if grand > 0 {
			kill9(grand)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if n, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && n > 0 {
				return tr, cmd.Process.Pid, n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the helper never reported its grandchild")
	return nil, 0, 0
}

func kill9(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func waitGone(t *testing.T, what string, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still running", what, pid)
}

// A started command's whole tree is listed by the containment, grandchild included.
func TestPIDsListsTheGrandchild(t *testing.T) {
	tr, parent, grand := treeFixture(t, Options{})
	if !tr.Contained() {
		t.Fatalf("this platform contains the tree, and did not: %s", tr.Warning())
	}
	pids, err := tr.PIDs()
	if err != nil {
		t.Fatal(err)
	}
	have := map[int]bool{}
	for _, p := range pids {
		have[p] = true
	}
	if !have[parent] || !have[grand] {
		t.Fatalf("members = %v, want the command (%d) and its grandchild (%d)", pids, parent, grand)
	}
}

// THE POINT OF THE JOB OBJECT / PROCESS GROUP: stopping the tree stops the grandchild, which a
// Kill of the direct child never did (the grandchild kept the card).
func TestTerminateKillsTheWholeTree(t *testing.T) {
	tr, parent, grand := treeFixture(t, Options{})
	if err := tr.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	waitGone(t, "the grandchild", grand, 15*time.Second)
	waitGone(t, "the command", parent, 15*time.Second)
	if pids, _ := tr.PIDs(); len(pids) != 0 {
		t.Fatalf("members after Terminate = %v", pids)
	}
}

// Containment is opt-in to die with the wrapper: by default letting go of the tree leaves the
// job running (a wrapper that dies must not take a long render with it, which is what makes the
// tree-orphan rule possible); with the option, letting go ends it.
func TestDieWithWrapperIsOptIn(t *testing.T) {
	if runtime.GOOS != "windows" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		hideChild(cmd)
		if _, err := Start(cmd, Options{DieWithWrapper: true}); err != ErrDieWithWrapperUnsupported {
			t.Fatalf("a platform that cannot honour die-with-wrapper must refuse it loudly, got %v", err)
		}
		tr, _, grand := treeFixture(t, Options{})
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
		if !alive(grand) {
			t.Fatal("closing the containment ended the tree by default")
		}
		return
	}
	tr, parent, grand := treeFixture(t, Options{})
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if !alive(parent) || !alive(grand) {
		t.Fatal("closing the containment ended the tree by default (it must not: the wrapper is not a single point of failure)")
	}
	tr2, parent2, grand2 := treeFixture(t, Options{DieWithWrapper: true})
	if err := tr2.Close(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, "the command", parent2, 15*time.Second)
	waitGone(t, "the grandchild", grand2, 15*time.Second)
}

// TerminateExcept spares exactly what it is told to spare (a process another lease took over)
// and kills the rest.
func TestTerminateExceptSparesTheProtectedAndKillsTheRest(t *testing.T) {
	tr, parent, grand := treeFixture(t, Options{})
	left, err := tr.TerminateExcept(map[int]bool{grand: true})
	if err != nil {
		t.Fatalf("TerminateExcept: %v", err)
	}
	waitGone(t, "the command", parent, 15*time.Second)
	if !alive(grand) {
		t.Fatal("the protected grandchild was killed")
	}
	if len(left) != 1 || left[0] != grand {
		t.Fatalf("left = %v, want [%d]", left, grand)
	}
}

// A member whose parent has exited is still a member: the command leaves its child running and
// exits, so the child is reparented, but it is still in the job (Windows) or the group (Unix) and
// is found and stopped with the tree. Walking parent links alone would lose it.
func TestTerminateKillsAMemberWhoseParentExited(t *testing.T) {
	tr, parent, grand := roleFixture(t, "leaver", Options{})
	waitGone(t, "the leaving command", parent, 15*time.Second)
	pids, err := tr.PIDs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pids {
		if p == grand {
			found = true
		}
	}
	if !found {
		t.Fatalf("the orphaned grandchild %d is still a member of the tree, members = %v", grand, pids)
	}
	if err := tr.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	waitGone(t, "the orphaned grandchild", grand, 15*time.Second)
}
