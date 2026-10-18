package gpuprobe

import (
	"strings"
	"testing"
)

// A commit limit the OS enforces can sit BELOW physical RAM: Linux with vm.overcommit_memory = 2 and the default ratio caps
// the commit at about half the RAM plus the swap, so a swapless box has a limit near half its RAM (Windows cannot: its limit is
// physical RAM plus the page file). A commit nobody stated is held to physical RAM less the headroom, and on such a reading that
// line lies further out than the one the kernel enforces, so it is held to the nearer of the two: the commit limit less the
// headroom. Otherwise a caller that declares one figure (the media admission, the seat warm-back, a bare gpu reserve --ram N) is
// admitted past a limit the kernel refuses allocations at, while the verdict of the same reading says OVER.

// The reading: 100 GiB of RAM, 90 available, 70 GiB committed against a strict 60 GiB limit.
var strictBelowPhysical = HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 70, CommitLimitGiB: 60}

func TestACommitNobodyStatedIsHeldToAnEnforcedLimitThatSitsBelowPhysicalRAM(t *testing.T) {
	if v := strictBelowPhysical.Verdict(8); v != HostOver {
		t.Fatalf("the fixture is wrong: 70 committed against a 60 GiB enforced limit reads %s, want OVER", v)
	}
	unstated := hostRAMAdmitsLoad(strictBelowPhysical, true, true, HostLoad{TouchGiB: 10, CommitGiB: 10}, HostLoad{}, 8)
	if unstated.OK || !unstated.CommitShort || unstated.TouchShort {
		t.Fatalf("10 GiB declared once on a reading that is OVER on its enforced limit: want refused on the commit, got ok=%v commitShort=%v touchShort=%v (%s)",
			unstated.OK, unstated.CommitShort, unstated.TouchShort, unstated.Why)
	}
	if !near(unstated.CommitBoundGiB, 52) || !near(unstated.CommitProjectedGiB, 80) {
		t.Errorf("the check must carry the arithmetic it decided on: bound %.1f projected %.1f, want the 60 GiB limit less the 8 headroom (52) against 70 + 10 (80)", unstated.CommitBoundGiB, unstated.CommitProjectedGiB)
	}
	// The refusal names the line it rests on: here the commit limit, not the physical RAM that lies further out.
	for _, w := range []string{"committed 70.0 of 60.0 GiB commit limit", "8.0 GiB headroom"} {
		if !strings.Contains(unstated.Why, w) {
			t.Errorf("why = %q, want it to contain %q", unstated.Why, w)
		}
	}
	if strings.Contains(unstated.Why, "physical,") || strings.Contains(unstated.Why, "held to physical RAM") {
		t.Errorf("why = %q names the physical line, which is not the one that refused", unstated.Why)
	}

	// A commit the caller stated is judged on the same line on this reading.
	stated := hostRAMAdmitsLoad(strictBelowPhysical, true, true, HostLoad{TouchGiB: 10, CommitGiB: 10, CommitStated: true}, HostLoad{}, 8)
	if stated.OK || !near(stated.CommitBoundGiB, 52) {
		t.Errorf("a stated commit on the same reading: want refused against 52, got ok=%v bound %.1f", stated.OK, stated.CommitBoundGiB)
	}
}

// The single-figure callers, on a reading a hair over the limit and on one the commit has room under.
func TestAOneFigureCallerIsRefusedAtAnEnforcedLimitBelowPhysicalRAM(t *testing.T) {
	over := HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 61, CommitLimitGiB: 60}
	if got := hostRAMAdmits(over, true, true, 10, 0, 8); got.OK || !got.CommitShort {
		t.Errorf("one figure of 10 on 61 committed against a 60 GiB enforced limit: want refused on the commit, got ok=%v commitShort=%v (%s)", got.OK, got.CommitShort, got.Why)
	}
	// 128 GiB of RAM, a strict 64 GiB limit, 60 committed, 100 available: `gpu reserve --ram 40` is admitted by the physical line
	// (60 + 40 = 100 <= 120) and by the available memory (100 - 40 = 60 >= 8), and the kernel refuses allocations past 64.
	half := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 60, CommitLimitGiB: 64}
	if got := hostRAMAdmits(half, true, true, 40, 0, 8); got.OK || !got.CommitShort || got.TouchShort {
		t.Errorf("--ram 40 on 60 committed against a strict 64 GiB limit: want refused on the commit alone, got ok=%v commitShort=%v touchShort=%v (%s)", got.OK, got.CommitShort, got.TouchShort, got.Why)
	}
	// Exactly at the line (the limit less the headroom: 56) is admitted, a quarter GiB past it is not.
	if got := hostRAMAdmits(HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 16, CommitLimitGiB: 64}, true, true, 40, 0, 8); !got.OK || !near(got.CommitBoundGiB, 56) {
		t.Errorf("16 + 40 = 56 sits on the bound: want admitted against 56, got ok=%v bound %.1f (%s)", got.OK, got.CommitBoundGiB, got.Why)
	}
	if got := hostRAMAdmits(HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 16.25, CommitLimitGiB: 64}, true, true, 40, 0, 8); got.OK || !got.CommitShort {
		t.Errorf("a quarter GiB past the bound: want refused, got ok=%v (%s)", got.OK, got.Why)
	}
}

// What the leases already granted have yet to touch counts on the same line.
func TestWhatRunningLeasesHaveYetToTouchCountsAgainstTheEnforcedLimitBelowPhysicalRAM(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 20, CommitLimitGiB: 64}
	if got := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 20, CommitGiB: 20}, HostLoad{}, 8); !got.OK {
		t.Fatalf("20 + 20 = 40 <= 56: want admitted, got %s", got.Why)
	}
	if got := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 20, CommitGiB: 20}, HostLoad{TouchGiB: 20}, 8); got.OK || !got.CommitShort || !near(got.CommitProjectedGiB, 60) {
		t.Errorf("20 + 20 + 20 pending = 60 > 56: want refused with projected 60, got ok=%v projected %.1f (%s)", got.OK, got.CommitProjectedGiB, got.Why)
	}
}

// A commit above the line it is judged against, less the headroom, can never be admitted: on this reading the line is the limit.
func TestACommitAboveAnEnforcedLimitBelowPhysicalRAMIsImpossible(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 10, CommitLimitGiB: 64}
	got := hostRAMAdmitsLoad(mem, true, true, HostLoad{TouchGiB: 10, CommitGiB: 60}, HostLoad{}, 8)
	if got.OK || !got.Impossible || !got.CommitShort {
		t.Fatalf("60 GiB of commit against a 64 GiB limit less 8 (56): want impossible, got ok=%v impossible=%v commitShort=%v (%s)", got.OK, got.Impossible, got.CommitShort, got.Why)
	}
	for _, w := range []string{"host RAM can never admit this", "it declares 60.0 GiB of commit", "the 64.0 GiB commit limit", "leaves 56.0 GiB"} {
		if !strings.Contains(got.Why, w) {
			t.Errorf("why = %q, want it to contain %q", got.Why, w)
		}
	}
}

// What stays as it was: a limit that lies further out than physical RAM (Windows) and a limit nobody enforces (Linux modes 0 and 1)
// leave the unstated commit on the physical line.
func TestTheUnstatedCommitLineIsPhysicalRAMWhereTheLimitIsFurtherOutOrNotEnforced(t *testing.T) {
	windows := HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 70, CommitLimitGiB: 170}
	got := hostRAMAdmits(windows, true, true, 30, 0, 8)
	if got.OK || !near(got.CommitBoundGiB, 92) {
		t.Errorf("limit above physical: the unstated commit stays on 100 - 8 = 92 (70 + 30 = 100 refused), got ok=%v bound %.1f (%s)", got.OK, got.CommitBoundGiB, got.Why)
	}
	if !strings.Contains(got.Why, "committed 70.0 of 100.0 GiB physical") {
		t.Errorf("why = %q, want the physical line named", got.Why)
	}
	soft := HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 40, CommitLimitGiB: 60, CommitLimitSoft: true}
	got = hostRAMAdmits(soft, true, true, 30, 0, 8)
	if !got.OK || !near(got.CommitBoundGiB, 92) {
		t.Errorf("a limit the OS does not enforce is no line, even when it sits below physical RAM: want admitted against 92, got ok=%v bound %.1f (%s)", got.OK, got.CommitBoundGiB, got.Why)
	}
}
