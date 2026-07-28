package fleetnode

import (
	"strings"
	"testing"
	"time"
)

// TestReclaimableNeverClaimsCapacityWeDoNotHold is the safety property. Over-
// promising costs a failed job; under-promising costs a scheduling opportunity.
// With nothing of ours loaded the answer is a measured zero, whatever the card
// happens to be holding for someone else.
func TestReclaimableNeverClaimsCapacityWeDoNotHold(t *testing.T) {
	got := Reclaimable(ReclaimInputs{UsedGiB: 6.5, OursLoaded: false, BaselineGiB: 1.0, HaveBaseline: true})
	if !got.Known || got.ReclaimableGiB != 0 {
		t.Fatalf("nothing loaded must be a KNOWN zero, got %+v", got)
	}
	if !strings.Contains(got.Source, "baseline") {
		t.Errorf("the source must explain that the in-use memory is unreclaimable: %q", got.Source)
	}
}

// TestReclaimableIsUnknownBeforeAnIdleObservation: guessing here would be worse
// than saying nothing — the dispatcher can fall back to free VRAM, but it cannot
// recover from a confident wrong number.
func TestReclaimableIsUnknownBeforeAnIdleObservation(t *testing.T) {
	got := Reclaimable(ReclaimInputs{UsedGiB: 9.0, OursLoaded: true, HaveBaseline: false})
	if got.Known {
		t.Fatalf("no baseline yet must be UNKNOWN, got %+v", got)
	}
	if !strings.Contains(got.Source, "unknown") {
		t.Errorf("the source must say it is unknown and why: %q", got.Source)
	}
}

// TestReclaimableExcludesTheSharedDesktop is the measured workstation's case: a
// card holding 6.5 GiB of desktop plus 3.1 GiB of our models can deliver 3.1, not
// 9.6 (total) and not 6.7 (free).
func TestReclaimableExcludesTheSharedDesktop(t *testing.T) {
	got := Reclaimable(ReclaimInputs{UsedGiB: 9.6, OursLoaded: true, BaselineGiB: 6.5, HaveBaseline: true})
	if !got.Known {
		t.Fatal("a known baseline plus loaded models must produce a known answer")
	}
	if diff := got.ReclaimableGiB - 3.1; diff > 0.001 || diff < -0.001 {
		t.Errorf("reclaimable = %.3f, want 3.1 (used 9.6 minus the 6.5 GiB desktop baseline)", got.ReclaimableGiB)
	}
}

// TestReclaimableFloorsAtZeroBelowBaseline: something outside the harness can
// release memory, which must never produce a negative advertisement.
func TestReclaimableFloorsAtZeroBelowBaseline(t *testing.T) {
	got := Reclaimable(ReclaimInputs{UsedGiB: 4.0, OursLoaded: true, BaselineGiB: 6.5, HaveBaseline: true})
	if !got.Known || got.ReclaimableGiB != 0 {
		t.Fatalf("below-baseline usage must floor at a known zero, got %+v", got)
	}
	if !strings.Contains(got.Source, "below") {
		t.Errorf("the source must explain the floor: %q", got.Source)
	}
}

// TestBaselineOnlyMovesWhenIdle: if a busy sample could move the baseline, our own
// loaded models would be absorbed into the "unreclaimable" figure and the node
// would advertise less and less capacity the longer it stayed warm.
func TestBaselineOnlyMovesWhenIdle(t *testing.T) {
	var b BaselineTracker
	now := time.Now()
	b.Observe(6.5, false, now) // idle: recorded
	b.Observe(12.0, true, now) // busy: must be ignored
	base, _, ok := b.Baseline()
	if !ok || base != 6.5 {
		t.Fatalf("baseline = %.1f (ok=%v), want the idle observation 6.5", base, ok)
	}
	v := b.Verdict(12.0, true)
	if !v.Known || v.ReclaimableGiB != 5.5 {
		t.Errorf("verdict = %+v, want 5.5 GiB reclaimable", v)
	}
	// A later idle sample re-measures the machine (the desktop's usage changes).
	b.Observe(7.0, false, now.Add(time.Minute))
	if base, _, _ := b.Baseline(); base != 7.0 {
		t.Errorf("baseline = %.1f, want the newer idle observation 7.0", base)
	}
}

// TestBaselineStartsUnknown: a fresh process has measured nothing, and must say so
// rather than defaulting to zero (which would advertise the whole card).
func TestBaselineStartsUnknown(t *testing.T) {
	var b BaselineTracker
	if _, _, ok := b.Baseline(); ok {
		t.Fatal("a fresh tracker must have no baseline")
	}
	if v := b.Verdict(9.0, true); v.Known {
		t.Errorf("verdict before any observation must be unknown, got %+v", v)
	}
}
