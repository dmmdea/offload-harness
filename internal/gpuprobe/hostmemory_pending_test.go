package gpuprobe

import "testing"

// The not-yet-loaded part a commit is counted with depends on whose commit it is. A commit nobody stated keeps the line it always
// had, whose pending figure is what running leases have yet to TOUCH (their declaration less what their processes occupy). A
// stated commit is counted with what they have yet to be CHARGED (their declared commit less their private bytes). A lane whose
// card allocations are charged to commit already has little yet to be charged and much yet to touch, which is the case where
// the two differ: 50 committed of 100 (limit 160), 90 available, a lease of 10 asked for beside running leases.
func TestAnUnstatedCommitCountsThePendingTouchAndAStatedOneThePendingCommit(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 50, CommitLimitGiB: 160}
	unstated := HostLoad{TouchGiB: 10, CommitGiB: 10}
	stated := HostLoad{TouchGiB: 10, CommitGiB: 10, CommitStated: true}

	// 40 GiB yet to touch, none yet to be charged: 50 + 10 + 40 = 100 > 92 for the unstated commit; 50 + 10 + 0 fits the limit.
	touchOnly := HostLoad{TouchGiB: 40}
	if got := hostRAMAdmitsLoad(mem, true, true, unstated, touchOnly, 8); got.OK || !got.CommitShort || got.TouchShort || !near(got.CommitProjectedGiB, 100) || !near(got.CommitPendingGiB, 40) {
		t.Errorf("an unstated commit counts the 40 yet to touch: ok=%v commitShort=%v touchShort=%v projected %.1f pending %.1f (%s)", got.OK, got.CommitShort, got.TouchShort, got.CommitProjectedGiB, got.CommitPendingGiB, got.Why)
	}
	if got := hostRAMAdmitsLoad(mem, true, true, stated, touchOnly, 8); !got.OK || !near(got.CommitProjectedGiB, 60) {
		t.Errorf("a stated commit does not count what is yet to touch: ok=%v projected %.1f (%s)", got.OK, got.CommitProjectedGiB, got.Why)
	}

	// 100 GiB yet to be charged, none yet to touch: 50 + 10 + 100 = 160 > 152 for the stated commit; the unstated one is blind to it.
	chargeOnly := HostLoad{CommitGiB: 100}
	if got := hostRAMAdmitsLoad(mem, true, true, stated, chargeOnly, 8); got.OK || !got.CommitShort || !near(got.CommitProjectedGiB, 160) || !near(got.CommitPendingGiB, 100) {
		t.Errorf("a stated commit counts the 100 yet to be charged: ok=%v commitShort=%v projected %.1f pending %.1f (%s)", got.OK, got.CommitShort, got.CommitProjectedGiB, got.CommitPendingGiB, got.Why)
	}
	if got := hostRAMAdmitsLoad(mem, true, true, unstated, chargeOnly, 8); !got.OK || !near(got.CommitProjectedGiB, 60) {
		t.Errorf("an unstated commit does not count what is yet to be charged: ok=%v projected %.1f (%s)", got.OK, got.CommitProjectedGiB, got.Why)
	}
}
