package main

// What the wrapper records as its command's tree (gpu_tree.go members). The record is what a reader holds a dead wrapper's
// claim on, so a process in it that is not the job keeps a card claimed for a job that is gone.

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/proctree"
)

// sleeper starts the test binary as a process that sleeps (TestHelperLongSleep), window-less, and reaps it at cleanup.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLongSleep$", "-test.timeout=120s")
	hideWindow(cmd)
	cmd.Env = append(os.Environ(), "LO_LONG_SLEEP=1")
	return cmd
}

func membersContain(ms []gpulease.TreeMember, pid int) bool {
	for _, m := range ms {
		if m.PID == pid {
			return true
		}
	}
	return false
}

// On Windows a contained command's job lists exactly what runs in it, the descendants that outlived their parent included, and
// the parent-link walk of the process table adds only what the job does not hold: the console host the OS starts for a command
// whose wrapper has no console is one, and it can outlive the job, keeping a dead wrapper's claim for a process that is not the
// job. Where the walk is a second opinion (Unix, or a command that runs uncontained) it still counts.
func TestRecordedTreeIsTheJobsExactListOnWindowsAndTheTableWalkElsewhere(t *testing.T) {
	cmd := sleeper(t)
	tree, err := proctree.Start(cmd, proctree.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Terminate(); _ = tree.Close(); _, _ = cmd.Process.Wait() })
	if !tree.Contained() {
		t.Skip("the command runs uncontained here, so the table walk is the only source")
	}
	outside := sleeper(t) // a process the job does not hold
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outside.Process.Kill(); _, _ = outside.Process.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, ok := gpulease.ProcessStart(outside.Process.Pid); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("no process start reader on this platform")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The process table says the extra process is below the command (a console host is, by parent link).
	table := func(root int) ([]gpulease.TreeProc, error) {
		return []gpulease.TreeProc{{PID: root}, {PID: outside.Process.Pid, PPID: root}}, nil
	}

	for _, tc := range []struct {
		goos        string
		wantOutside bool
	}{
		{"windows", false}, // the job is exact
		{"linux", true},    // the walk is a second opinion where a process can leave its group
	} {
		r := &treeRecorder{tree: tree, walk: table, goos: tc.goos}
		got := r.members()
		if !membersContain(got, cmd.Process.Pid) {
			t.Fatalf("%s: the command itself is always recorded: %+v", tc.goos, got)
		}
		if has := membersContain(got, outside.Process.Pid); has != tc.wantOutside {
			t.Errorf("%s: a process the table shows below the command but the job does not hold is recorded = %v, want %v (%+v)", tc.goos, has, tc.wantOutside, got)
		}
	}
}
