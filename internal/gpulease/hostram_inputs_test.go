package gpulease

// The inputs of the two host-memory conditions, pinned one by one. Each test below goes red under a mutant of exactly the
// input it names (the touched figure the commit is read from, the commit half of a held reading, the commit a whole-node
// grant is judged on, the quantity a held figure is subtracted from), because a condition that is right for the common
// case can still be fed the wrong number.

import (
	"bufio"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// A commit smaller than the touched set is a declaration the CLI accepts and takes as stated: mapped files count in the
// working set and not in the commit. 90 GiB are available (60 touched leave 30), the commit charge 140 + 5 = 145 fits the
// bound of 152; read as 60 (the larger figure) the same lease would be 200 and refused. The record keeps the smaller commit
// and the pending commit of the lease is that figure, not the touched one.
func TestACommitBelowTheTouchedSetIsTakenAsStated(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(90, 140))
	if chk := m.HostRAMCheckLoad(gpuprobe.HostLoad{TouchGiB: 60, CommitGiB: 5}); !chk.OK {
		t.Fatalf("the read-only check takes the commit as stated: %+v", chk)
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "mapped", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 60, HostRAMCommitGiB: 5})
	if err != nil {
		t.Fatalf("60 touched of 90 available and 5 committed on 140 of a 152 bound fit both conditions: %v", err)
	}
	defer func() { _ = l.Release() }()
	info := m.InspectFor([]string{card0})
	if info.HostRAMGiB != 60 || info.HostRAMCommitGiB != 5 {
		t.Fatalf("the record must carry the smaller commit as stated: touched %.1f commit %.1f", info.HostRAMGiB, info.HostRAMCommitGiB)
	}
	if got := m.HostRAMPendingCommit(); got != 5 {
		t.Fatalf("nothing of the lease is charged yet, so its pending commit is the 5 it declared, got %.1f", got)
	}

	// The same lease declaring one figure declares it for both, and the commit condition then refuses it: 140 + 60 > 152.
	m2, _ := commitScoped(t, commitHostMem(90, 140))
	if _, err := m2.TryAcquire(ClassMedia, Options{Reason: "one figure", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 60}); !asHostRAM(t, err).CommitShort {
		t.Fatalf("a single figure of 60 is a commit of 60: %v", err)
	}
}

// The whole-node grant judges the commit too, not only the touched set (the card-scoped grant has its own test in
// hostram_commit_test.go). 10 GiB touched fit the 80 available; 130 + 60 of commit do not fit a bound of 152. The
// refusal spends no epoch.
func TestAWholeNodeGrantIsRefusedOnItsCommitAlthoughItsTouchedSetFits(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 130))
	before := epochNow(t, m)
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "whole", TTL: time.Hour, HostRAMGiB: 10, HostRAMCommitGiB: 60})
	short := asHostRAM(t, err)
	if !short.CommitShort || short.TouchShort || short.Impossible {
		t.Fatalf("want a refusal on commit alone, got commitShort=%v touchShort=%v impossible=%v (%v)", short.CommitShort, short.TouchShort, short.Impossible, err)
	}
	if got := epochNow(t, m); got != before {
		t.Fatalf("a refused whole-node grant spent an epoch (%d -> %d)", before, got)
	}
}

// Each quantity is subtracted from its own declaration: a lane that declared 10 touched and 60 committed, whose processes
// hold 3 resident and 20 private, has 7 left to touch and 40 left to be charged. (The rule that private bytes must not be
// subtracted from a touched-set declaration, nor resident bytes from a commit one, at the unit where it is applied.)
func TestPendingSubtractsEachQuantityFromItsOwnDeclaration(t *testing.T) {
	live := []Info{{Epoch: 1, PID: 100, HostRAMGiB: 10, HostRAMCommitGiB: 60}}
	held := func(Info) (gpuprobe.HostLoad, bool) { return gpuprobe.HostLoad{TouchGiB: 3, CommitGiB: 20}, true }
	if got, want := pendingLoad(live, held), (gpuprobe.HostLoad{TouchGiB: 7, CommitGiB: 40}); got != want {
		t.Fatalf("pending = %+v, want %+v", got, want)
	}
}

// And through the grant: lane a declared 20 GiB to touch; its processes hold 5 resident but 20 private (committed, not yet
// touched: the shape of a lane still loading its weights), so 15 GiB are still to be touched. Available is 35 with 8 of
// headroom, so a second lane declaring 15 needs 15 + 15 = 30 against 27 and is refused on the touched set. Reading the
// private bytes as what lane a has loaded would call it fully loaded and admit the second lane into the gap.
func TestASecondLaneIsRefusedWhileTheFirstLanesWorkingSetIsStillLoading(t *testing.T) {
	m, f := commitScoped(t, commitHostMem(40, 0))
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "lane a", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 20})
	if err != nil {
		t.Fatalf("lane a fits an idle box: %v", err)
	}
	defer func() { _ = a.Release() }()
	f.mu.Lock()
	f.mem.AvailableGiB = 35
	f.mu.Unlock()
	f.setHeld(os.Getpid(), 5)
	f.setHeldPrivate(os.Getpid(), 20)
	b, err := m.TryAcquire(ClassMedia, Options{Reason: "lane b", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 15})
	if err == nil {
		_ = b.Release()
		t.Fatal("lane b was admitted with 15 GiB declared, 15 GiB of lane a still to touch and 35 - 8 = 27 available")
	}
	if short := asHostRAM(t, err); !short.TouchShort || short.PendingGiB != 15 {
		t.Fatalf("want a refusal on the touched set with 15 pending, got touchShort=%v pending=%.1f (%v)", short.TouchShort, short.PendingGiB, err)
	}
}

// The commit half of a tree-orphan lease's held figure (membersHeld, the recorded members when the holder is dead): what the
// members and everything below them hold privately is what the lease's declared commit is reduced by, as the working set is
// reduced by what they hold resident. A child holding 96 MiB reads at least that in each half.
func TestMembersHeldCarriesBothHalves(t *testing.T) {
	if _, ok := privateBytes(os.Getpid()); !ok {
		t.Skip("no process-memory reader on this platform")
	}
	const mib = 96
	cmd := winexec.Command(os.Args[0])
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
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
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
	load, ok := membersHeld([]TreeMember{{PID: cmd.Process.Pid}})
	if !ok {
		t.Fatal("the member could not be read")
	}
	floor := float64(mib) * 0.9 / 1024
	if load.TouchGiB < floor || load.CommitGiB < floor {
		t.Fatalf("a member holding %d MiB: held touched %.3f GiB, commit %.3f GiB, want at least %.3f of each", mib, load.TouchGiB, load.CommitGiB, floor)
	}
}
