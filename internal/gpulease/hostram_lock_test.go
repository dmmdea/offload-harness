package gpulease

// F57: a card-scoped grant ran its whole host-RAM check inside the machine-wide epoch lock: the host's counters AND, for
// every live lease that declared a need, a walk of its holder's process tree with a memory read per process (the reviewer
// measured about 26 ms median and 63 ms worst for one holder, 27.6 ms for six children, on every grant attempt and every
// waiter poll). With several declaring holders and waiters the lock is held for about 100 ms a round against a 2 s spin
// for everyone else who wants an epoch, and the comment at the call said "a few syscalls inside a critical section that is
// already file I/O". The readings are now taken BEFORE the lock, and only compared inside it: the decision still sees the
// leases live at the instant this grant writes its own record (their declared needs come from the records read under the
// lock; only the machine's counters and the per-holder resident figures are a little older, and an older resident figure
// is a LOWER one, so the not-yet-loaded part it leaves is larger: the stale direction is the conservative one).

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func TestTheHostReadingsAreTakenBeforeTheEpochLockAndOnlyComparedInside(t *testing.T) {
	m, f := ramScoped(t, 40)
	// A live lease that declared RAM, so the check needs a process-tree read for its holder.
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	f.setHeld(os.Getpid(), 5)

	locked := func() bool { _, serr := os.Stat(m.epochLockPath()); return serr == nil }
	var memIn, memOut, treeIn, treeOut atomic.Int64
	base := m.hostMem
	m.hostMem = func() (gpuprobe.HostMemory, bool) {
		if locked() {
			memIn.Add(1)
		} else {
			memOut.Add(1)
		}
		return base()
	}
	baseTree := m.workload
	m.workload = func(pid int) (float64, bool) {
		if locked() {
			treeIn.Add(1)
		} else {
			treeOut.Add(1)
		}
		return baseTree(pid)
	}

	l, err := m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 20})
	if err != nil {
		t.Fatalf("40 committed + 20 declared + 15 still to load is under 92: %v", err)
	}
	defer func() { _ = l.Release() }()

	if memIn.Load() != 0 || treeIn.Load() != 0 {
		t.Errorf("work done under the epoch lock: %d host-memory read(s), %d process-tree read(s); both belong before it", memIn.Load(), treeIn.Load())
	}
	if memOut.Load() < 1 || treeOut.Load() < 1 {
		t.Errorf("the readings must still be taken (before the lock): %d host-memory, %d process-tree", memOut.Load(), treeOut.Load())
	}
}

// The decision is still made under the lock, on the leases live then: a grant that lands between this grant's reading and its
// turn is counted (its record is read inside the lock), so two grants that read the same headroom cannot both take it.
func TestAGrantThatLandsAfterTheReadingIsStillCountedInsideTheLock(t *testing.T) {
	m, f := ramScoped(t, 60) // 60 committed of 100, 8 headroom: one 20 GiB declaration fits, two do not
	// The first racer is granted AFTER the second has read the host (the hook runs between the second's readings and its lock).
	var first *Lease
	var ferr error
	m.afterHostReadHook = func() {
		m.afterHostReadHook = nil // once: the first grant's own reading must not run it again
		first, ferr = m.TryAcquire(ClassMedia, Options{Reason: "first", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 20})
		f.setCommit(60) // its weights have not loaded: committed memory has not moved, only its declaration is on record
	}
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 20})
	if first == nil {
		t.Fatalf("the hook between the second grant's reading and its lock never ran (ferr=%v): the grant must read the host before it takes the lock", ferr)
	}
	if ferr != nil {
		t.Fatalf("the first grant fits an empty line: %v", ferr)
	}
	defer func() { _ = first.Release() }()
	asHostRAM(t, err) // 60 + 20 + the 20 the first has yet to load > 92: refused, because the first's record is read under the lock
}
