package gpuprobe

import "testing"

// What NEAR does and does not hold back. The word is the room against any of the lines inside the headroom, but a lease is judged
// only on the lines that apply to it: one that states its commit is judged on the commit limit, not on the physical-RAM line that
// holds a commit nobody stated. So a host that reads NEAR on the physical line alone still admits a lease that states its commit,
// and the docs must not say every declaring lease waits.
func TestAHostThatReadsNearOnThePhysicalCommitLineStillAdmitsALeaseThatStatesItsCommit(t *testing.T) {
	// 125 committed is inside the 8 GiB headroom of 127.7 GiB physical RAM (119.7), 46.7 under the 171.7 GiB limit's 163.7, and 60
	// GiB is available.
	mem := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 60, CommitUsedGiB: 125, CommitLimitGiB: 171.7}
	if v := mem.Verdict(8); v != HostNear {
		t.Fatalf("the fixture is wrong: want NEAR, got %s", v)
	}
	stated := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 10, CommitGiB: 10, CommitStated: true}, HostLoad{}, 8)
	if !stated.OK {
		t.Errorf("a lease that states its commit is judged on the limit (135 of 163.7) and the available memory (50 left): want admitted, got %+v (%s)", stated, stated.Why)
	}
	unstated := hostRAMAdmits(mem, true, true, 10, 0, 8)
	if unstated.OK || !unstated.CommitShort {
		t.Errorf("a lease that states no commit is held to physical RAM (135 of 119.7): want refused on the commit, got %+v (%s)", unstated, unstated.Why)
	}
}

// NEAR on the available memory holds back every lease that declares a working set, stated commit or not.
func TestAHostThatReadsNearOnTheAvailableMemoryHoldsBackALeaseThatStatesItsCommit(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 5, CommitUsedGiB: 40, CommitLimitGiB: 171.7}
	if v := mem.Verdict(8); v != HostNear {
		t.Fatalf("the fixture is wrong: want NEAR, got %s", v)
	}
	stated := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 10, CommitGiB: 10, CommitStated: true}, HostLoad{}, 8)
	if stated.OK || !stated.TouchShort {
		t.Errorf("5 GiB available is inside the headroom: want refused on the working set, got %+v (%s)", stated, stated.Why)
	}
}
