package gpulease

// A tree-orphan's host RAM (plan P10 follow-up). A lease whose wrapper died keeps its claim while a recorded member lives, and
// the host-RAM guard still has to tell how much of its declared need has LOADED. It used to measure the processes below the
// lease's holder pid, which for a tree-orphan is the dead wrapper: the members of the job were reparented away (Linux) or
// reached only by the dead pid's parent links (Windows, until the pid is reused), so the figure was ~0 and the whole declared
// need was charged again as "still to load", on top of the commit the running job already is. Every later grant that declares
// RAM then waited behind a message naming host RAM for as long as the orphan lived.

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// startHoldChild starts the test binary as a process that holds mib MiB resident and returns it once it says so.
func startHoldChild(t *testing.T, mib int) *exec.Cmd {
	t.Helper()
	if _, ok := privateBytes(os.Getpid()); !ok {
		t.Skip("no private-memory reader on this platform")
	}
	cmd := exec.Command(os.Args[0])
	hideProc(cmd)
	cmd.Env = append(os.Environ(), "GPULEASE_HELPER_HOLD_MIB="+strconv.Itoa(mib))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("helper said %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the memory-holding child never became ready")
	}
	return cmd
}

func TestTreeOrphanDeclaredNeedIsNotChargedAgainAsUnloaded(t *testing.T) {
	m, _ := scopedManager(t)
	newFakeHost(10).arm(m)
	m.workload = nil // measure real processes: the point is which processes are measured
	const declared = 0.5
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "wrapper", Devices: []string{card0}, TTL: time.Hour, HostRAMGiB: declared})
	if err != nil {
		t.Fatal(err)
	}

	child := startHoldChild(t, 96)
	start, _ := m.procStart(child.Process.Pid) // the Manager's own reading of a start, whatever seam it runs on
	if err := l.RecordTree([]TreeMember{{PID: child.Process.Pid, StartMs: start}}); err != nil {
		t.Fatal(err)
	}
	// The wrapper died: its pid is one nothing holds, and only the job it started lives.
	const deadWrapper = 1_900_000_001
	rewriteHolder(t, m, l.Epoch(), deadWrapper)
	liveSet(t, child.Process.Pid)

	live := m.Leases()
	if len(live) != 1 || !live[0].TreeOrphan {
		t.Fatalf("setup: the lease is a tree-orphan: %+v", live)
	}
	pending := m.HostRAMPending()
	if pending > declared-0.05 {
		t.Fatalf("pending = %.3f GiB of a declared %.1f: the 96 MiB the orphan's member already holds resident was charged as still to load", pending, declared)
	}
	if pending <= 0 {
		t.Fatalf("pending = %.3f GiB: a 96 MiB member cannot have loaded 0.5 GiB", pending)
	}
	// The grant path reads the holders early (before the epoch lock) and compares inside it: the same figure.
	r := m.readHost(1)
	chk := m.hostRAMCheckWith(1, live, r)
	if chk.PendingGiB > declared-0.05 {
		t.Fatalf("the grant's check charged %.3f GiB pending for the orphan, want the same measured figure (< %.2f)", chk.PendingGiB, declared-0.05)
	}
}
