package gpuprobe

import (
	"strings"
	"testing"
)

// A commit that nobody STATED is judged against the line it always was: committed + the declared figure against physical RAM
// less the headroom. Only a commit the caller stated (gpu reserve --ram-commit, or a caller that measured one) is judged against
// the OS commit limit. Every caller that declares one figure (the media admission, the seat warm-back, a bare
// `gpu reserve --ram N`) states no commit, so it keeps the protection it had.
//
// The readings are the ones the two-condition rule was made for (referenceWindowsHost: 127.7 GiB RAM, 171.7 GiB commit limit,
// 70.5 committed, 75.6 available). A coder-model arm touches 27.8 GiB and is charged 58.1 GiB.

// The coder arm declared as ONE figure, twice: refused both while the first arm is still loading and once it has loaded.
func TestTwoCoderArmsDeclaredAsOneFigureAreRefusedOnTheCommitCharge(t *testing.T) {
	arm := HostLoad{TouchGiB: 27.8, CommitGiB: 27.8} // no commit stated

	// Arm 1 granted and not yet loaded: its declaration is pending.
	pending := HostLoad{TouchGiB: 27.8, CommitGiB: 27.8}
	got := hostRAMAdmitsLoad(referenceWindowsHost, true, true, arm, pending, 8)
	if got.OK || !got.CommitShort || got.TouchShort {
		t.Fatalf("arm 2 with arm 1 pending, one figure each: want refused on the commit charge alone (70.5 + 27.8 + 27.8 = 126.1 > 127.7 - 8), got ok=%v commitShort=%v touchShort=%v (%s)",
			got.OK, got.CommitShort, got.TouchShort, got.Why)
	}
	if !near(got.CommitProjectedGiB, 126.1) || !near(got.CommitBoundGiB, 119.7) {
		t.Errorf("the refusal must carry the arithmetic it rests on: projected %.1f bound %.1f, want 126.1 against 119.7", got.CommitProjectedGiB, got.CommitBoundGiB)
	}
	for _, w := range []string{"waiting for host RAM", "needs 27.8 GiB", "committed 70.5 of 127.7 GiB physical", "+27.8 GiB still to load", "8.0 GiB headroom"} {
		if !strings.Contains(got.Why, w) {
			t.Errorf("why = %q, want it to contain %q", got.Why, w)
		}
	}

	// Arm 1 loaded: 128.6 committed (70.5 + 58.1), 47.8 available (75.6 - 27.8). Nothing is pending.
	loaded := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 47.8, CommitUsedGiB: 128.6, CommitLimitGiB: 171.7}
	got = hostRAMAdmitsLoad(loaded, true, true, arm, HostLoad{}, 8)
	if got.OK || !got.CommitShort || got.TouchShort {
		t.Fatalf("arm 2 once arm 1 has loaded: want refused on the commit charge alone (128.6 + 27.8 = 156.4 > 119.7), got ok=%v commitShort=%v touchShort=%v (%s)",
			got.OK, got.CommitShort, got.TouchShort, got.Why)
	}
	if !near(got.CommitProjectedGiB, 156.4) || !near(got.CommitBoundGiB, 119.7) {
		t.Errorf("projected %.1f bound %.1f, want 156.4 against 119.7", got.CommitProjectedGiB, got.CommitBoundGiB)
	}
}

// A STATED commit is judged against the commit limit. The same two arms are admitted when their stated commits fit it, and
// refused when they do not, whatever the touched set says.
func TestTwoCoderArmsAreAdmittedOnlyOnStatedCommitsThatFitTheLimit(t *testing.T) {
	stated := func(touch, commit float64) HostLoad { return HostLoad{TouchGiB: touch, CommitGiB: commit, CommitStated: true} }

	// Arm 1 alone, charged 58.1: 70.5 + 58.1 = 128.6 <= 171.7 - 8.
	if got := hostRAMAdmitsLoad(referenceWindowsHost, true, true, stated(27.8, 58.1), HostLoad{}, 8); !got.OK || !near(got.CommitProjectedGiB, 128.6) || !near(got.CommitBoundGiB, 163.7) {
		t.Fatalf("arm 1 with its measured commit stated fits the limit (128.6 <= 163.7): ok=%v projected %.1f bound %.1f (%s)", got.OK, got.CommitProjectedGiB, got.CommitBoundGiB, got.Why)
	}
	// Arm 2 while arm 1 is pending, both stated at the measured 58.1: 70.5 + 58.1 + 58.1 = 186.7 > 163.7.
	two := hostRAMAdmitsLoad(referenceWindowsHost, true, true, stated(27.8, 58.1), stated(27.8, 58.1), 8)
	if two.OK || !two.CommitShort || two.TouchShort || !near(two.CommitProjectedGiB, 186.7) {
		t.Fatalf("two arms at their measured commit do not fit the limit: ok=%v commitShort=%v touchShort=%v projected %.1f (%s)", two.OK, two.CommitShort, two.TouchShort, two.CommitProjectedGiB, two.Why)
	}
	// Arm 2 once arm 1 has loaded (128.6 committed): a stated commit of 30 fits (158.6 <= 163.7), 58.1 does not.
	loaded := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 47.8, CommitUsedGiB: 128.6, CommitLimitGiB: 171.7}
	if got := hostRAMAdmitsLoad(loaded, true, true, stated(27.8, 30), HostLoad{}, 8); !got.OK {
		t.Errorf("arm 2 with a stated commit of 30 fits the limit after arm 1 loaded: %s", got.Why)
	}
	if got := hostRAMAdmitsLoad(loaded, true, true, stated(27.8, 58.1), HostLoad{}, 8); got.OK || !got.CommitShort {
		t.Errorf("arm 2 with a stated commit of 58.1 does not fit: ok=%v commitShort=%v (%s)", got.OK, got.CommitShort, got.Why)
	}
}

// The flag is the whole difference: the same figures, stated, are judged against the limit and admitted; unstated, they are
// judged against physical RAM and refused. A stated commit that equals the touched figure is still stated.
func TestAStatedCommitEqualToTheTouchedFigureIsStillStated(t *testing.T) {
	pending := HostLoad{TouchGiB: 27.8, CommitGiB: 27.8}
	stated := hostRAMAdmitsLoad(referenceWindowsHost, true, true, HostLoad{TouchGiB: 27.8, CommitGiB: 27.8, CommitStated: true}, pending, 8)
	if !stated.OK || !near(stated.CommitBoundGiB, 163.7) {
		t.Fatalf("27.8 / 27.8 stated: judged against the 171.7 limit, 126.1 <= 163.7, want admitted: ok=%v bound %.1f (%s)", stated.OK, stated.CommitBoundGiB, stated.Why)
	}
	unstated := hostRAMAdmitsLoad(referenceWindowsHost, true, true, HostLoad{TouchGiB: 27.8, CommitGiB: 27.8}, pending, 8)
	if unstated.OK || !near(unstated.CommitBoundGiB, 119.7) {
		t.Fatalf("27.8 / 27.8 unstated: judged against physical RAM less the headroom, 126.1 > 119.7, want refused: ok=%v bound %.1f", unstated.OK, unstated.CommitBoundGiB)
	}
}

// The band the commit limit had opened for a single figure is closed again: at the reference reading a single figure d is
// admitted by the available memory up to 67.6 GiB, and by the old line only up to 49.2.
func TestTheBandThatTheCommitLimitOpenedForASingleFigureIsRefusedAgain(t *testing.T) {
	if got := hostRAMAdmits(referenceWindowsHost, true, true, 49, 0, 8); !got.OK {
		t.Fatalf("49 GiB declared once fits the old line (70.5 + 49 = 119.5 <= 119.7): %s", got.Why)
	}
	for _, d := range []float64{49.5, 55, 60, 67} {
		got := hostRAMAdmits(referenceWindowsHost, true, true, d, 0, 8)
		if got.OK || !got.CommitShort || got.TouchShort {
			t.Errorf("%.1f GiB declared once: want refused on the commit charge alone (the available memory has room), got ok=%v commitShort=%v touchShort=%v", d, got.OK, got.CommitShort, got.TouchShort)
		}
	}
	if got := hostRAMAdmits(referenceWindowsHost, true, true, 68, 0, 8); got.OK || !got.TouchShort {
		t.Errorf("68 GiB declared once leaves 7.6 of the 8 GiB headroom of the available memory: want refused on it, got ok=%v touchShort=%v", got.OK, got.TouchShort)
	}
}

// On a reading whose commit limit is not enforced (Linux, vm.overcommit_memory 0 or 1) a STATED commit is not judged at all, but
// an unstated one still is: the figure the caller always declared keeps the line it always had.
func TestAnUnstatedCommitIsJudgedWhereTheLimitIsNotEnforced(t *testing.T) {
	soft := HostMemory{PhysicalGiB: 128, AvailableGiB: 100, CommitUsedGiB: 100, CommitLimitGiB: 192, CommitLimitSoft: true}
	if got := hostRAMAdmitsLoad(soft, true, true, HostLoad{TouchGiB: 30, CommitGiB: 30}, HostLoad{}, 8); got.OK || !got.CommitShort {
		t.Errorf("unstated 30 on 100 committed of 128 physical: 130 > 120, want refused on the commit charge, got ok=%v commitShort=%v (%s)", got.OK, got.CommitShort, got.Why)
	}
	if got := hostRAMAdmitsLoad(soft, true, true, HostLoad{TouchGiB: 30, CommitGiB: 30, CommitStated: true}, HostLoad{}, 8); !got.OK || got.CommitApplies {
		t.Errorf("a stated commit is not judged against a limit the OS does not enforce: ok=%v applies=%v (%s)", got.OK, got.CommitApplies, got.Why)
	}
}

// The incident of 2026-10-09 is refused whatever the box has available: the commit charge of two lanes, one figure each, is
// over the old line on any baseline above 54.2 GiB committed (a knife-edge on the available memory alone, at 72 GiB).
func TestTheTwoLaneIncidentIsRefusedWhateverIsAvailable(t *testing.T) {
	lane := HostLoad{TouchGiB: 32.75, CommitGiB: 32.75}
	for _, avail := range []float64{72, 83, 100} {
		mem := HostMemory{PhysicalGiB: 128, AvailableGiB: avail, CommitUsedGiB: 56, CommitLimitGiB: 188}
		if first := hostRAMAdmitsLoad(mem, true, true, lane, HostLoad{}, 8); !first.OK {
			t.Errorf("available %.0f: the first lane fits the idle box: %s", avail, first.Why)
		}
		if second := hostRAMAdmitsLoad(mem, true, true, lane, lane, 8); second.OK {
			t.Errorf("available %.0f: the second lane is refused while the first is loading (56 + 65.5 = 121.5 > 120), got admitted", avail)
		}
	}
}
