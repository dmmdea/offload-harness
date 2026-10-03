package gpulease

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDescendantsOfFollowsParentLinks(t *testing.T) {
	table := []TreeProc{
		{PID: 10, PPID: 1}, {PID: 11, PPID: 10}, {PID: 12, PPID: 11}, {PID: 13, PPID: 10},
		{PID: 20, PPID: 1}, {PID: 21, PPID: 20}, // another tree
	}
	got := descendantsOf(10, table, func(int) (int64, bool) { return 0, false })
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	if len(pids) != 4 || pids[0] != 10 {
		t.Fatalf("want the root and its three descendants, root first, got %v", pids)
	}
	for _, p := range pids {
		if p == 20 || p == 21 {
			t.Fatalf("a process of another tree leaked in: %v", pids)
		}
	}
	// A root that is no longer in the table is still reported: the wrapper is the evidence's
	// anchor even when the snapshot raced its exit.
	if got := descendantsOf(99, table, func(int) (int64, bool) { return 0, false }); len(got) != 1 || got[0].PID != 99 {
		t.Fatalf("an absent root is reported alone, got %+v", got)
	}
}

func TestDescendantsOfRejectsARecycledParentID(t *testing.T) {
	// Process 12 names parent 11, but 12 began BEFORE 11: the id 11 was recycled, 12 is
	// not this tree's.
	table := []TreeProc{{PID: 10, PPID: 1}, {PID: 11, PPID: 10}, {PID: 12, PPID: 11}}
	starts := map[int]int64{10: 1000, 11: 2000, 12: 1500}
	got := descendantsOf(10, table, func(pid int) (int64, bool) { v, ok := starts[pid]; return v, ok })
	for _, p := range got {
		if p.PID == 12 {
			t.Fatalf("a child older than the parent it names belongs to a recycled id: %+v", got)
		}
	}
}

func TestDescendantsOfSurvivesACorruptCycle(t *testing.T) {
	table := []TreeProc{{PID: 10, PPID: 12}, {PID: 11, PPID: 10}, {PID: 12, PPID: 11}}
	done := make(chan []TreeProc, 1)
	go func() { done <- descendantsOf(10, table, func(int) (int64, bool) { return 0, false }) }()
	select {
	case got := <-done:
		if len(got) != 3 {
			t.Fatalf("each process once, got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cycle in the table looped forever")
	}
}

// The child the real-OS test starts is this test binary, which sleeps. Its extra flags follow a
// non-flag word so the testing package leaves them alone (an unknown flag would exit it at once).
func TestHelperSleepsForTheProcessTreeTest(t *testing.T) {
	if os.Getenv("GPULEASE_TREE_HELPER") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(30 * time.Second)
}

// The real process table, read-only: a child this test starts shows up in its parent's tree
// with the command line it was started with. This is the only place a ComfyUI `--cuda-device`
// can be read for a lease whose wrapper is an old binary.
func TestProcessTreeReadsAChildsCommandLine(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleepsForTheProcessTreeTest", "treeprobe", "--cuda-device", "3", "--tree-probe-marker")
	cmd.Env = append(os.Environ(), "GPULEASE_TREE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	var found TreeProc
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		procs, err := ProcessTree(os.Getpid())
		if err != nil {
			t.Skipf("no process-table reader here: %v", err)
		}
		for _, p := range procs {
			if p.PID == cmd.Process.Pid {
				found = p
			}
		}
		if found.PID != 0 && found.Cmdline != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if found.PID == 0 {
		t.Fatalf("the child %d is not in this process's tree", cmd.Process.Pid)
	}
	if !strings.Contains(found.Cmdline, "--tree-probe-marker") || !strings.Contains(found.Cmdline, "--cuda-device 3") {
		t.Fatalf("command line of pid %d = %q, want the flags it was started with", found.PID, found.Cmdline)
	}
	if found.PPID != os.Getpid() {
		t.Errorf("parent = %d, want %d", found.PPID, os.Getpid())
	}
	if found.StartMs == 0 {
		t.Error("the start time is part of the record (it is what keeps a recycled pid out of the tree)")
	}
	// And the evidence rule reads a card out of exactly that line.
	args := splitCommandLine(found.Cmdline)
	if got, ok := cudaDeviceFlag(args); !ok || got != "3" {
		t.Fatalf("--cuda-device in %q = %q, %v", found.Cmdline, got, ok)
	}
}

func TestSplitCommandLineHonoursQuotesAndKeepsBackslashes(t *testing.T) {
	got := splitCommandLine(`"C:\Program Files\Python\python.exe" main.py --cuda-device 2 --name 'a b'`)
	want := []string{`C:\Program Files\Python\python.exe`, "main.py", "--cuda-device", "2", "--name", "a b"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
	if got := splitCommandLine("   "); len(got) != 0 {
		t.Fatalf("blank line has no args, got %q", got)
	}
}

// A Linux start identity is clock ticks since boot (USER_HZ 100), not Unix milliseconds: the
// recycled-pid check converts it with the boot time, so the comparison with the lease's start is
// in one unit.
func TestTicksToUnixMsConvertsBootRelativeStarts(t *testing.T) {
	// Booted at Unix second 1_000_000; a process that started 2.5 s after boot (250 ticks).
	if got := ticksToUnixMs(1_000_000, 250); got != 1_000_002_500 {
		t.Fatalf("ticksToUnixMs = %d, want 1000002500", got)
	}
	if got := ticksToUnixMs(1_000_000, 0); got != 1_000_000_000 {
		t.Fatalf("a process started at boot is the boot time, got %d", got)
	}
}
