package gpuprobe

import (
	"math"
	"strings"
	"testing"
)

// A figure that is not a finite number of GiB compares against nothing: every ordered comparison with NaN is false, and Go's
// max(NaN, 0) is NaN, so a declaration of NaN used to fall through every "never" and "short" test and be admitted. The CLI refuses
// such a flag, but the rule is the one place every caller reaches (the media admission, the warm-back, the card allocator, the
// hidden holder), so it refuses too.

func TestARuleCallerThatDeclaresAFigureThatIsNotFiniteIsRefused(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for _, tc := range []struct {
		name string
		need HostLoad
	}{
		{"a touched set of NaN", HostLoad{TouchGiB: nan, CommitGiB: 10}},
		{"a commit of NaN", HostLoad{TouchGiB: 10, CommitGiB: nan}},
		{"a stated commit of NaN", HostLoad{TouchGiB: 10, CommitGiB: nan, CommitStated: true}},
		{"a touched set of NaN with no commit", HostLoad{TouchGiB: nan}},
		{"a commit of NaN with no touched set", HostLoad{CommitGiB: nan}},
		{"a touched set of +Inf", HostLoad{TouchGiB: inf, CommitGiB: 10}},
		{"a stated commit of +Inf", HostLoad{TouchGiB: 10, CommitGiB: inf, CommitStated: true}},
		{"a commit of -Inf", HostLoad{TouchGiB: 10, CommitGiB: -inf}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostRAMAdmitsLoad(referenceWindowsHost, true, true, tc.need, HostLoad{}, 8)
			if got.OK || !got.Impossible {
				t.Fatalf("want refused as impossible (no state of the box admits a figure that is not a number), got ok=%v impossible=%v (%s)", got.OK, got.Impossible, got.Why)
			}
			if !strings.Contains(got.Why, "finite") {
				t.Errorf("the refusal must say the figure is not a finite number of GiB: %q", got.Why)
			}
			// Even where the platform has no reader the rule cannot judge a declaration like that, and does not admit it.
			if no := hostRAMAdmitsLoad(HostMemory{}, false, false, tc.need, HostLoad{}, 8); no.OK {
				t.Errorf("a platform with no reader admits what it cannot judge, but not a figure that is not a number: %+v", no)
			}
		})
	}
}

// The other inputs the comparisons rest on: the part still to load of the running leases, the headroom and the reading itself. A
// lease waits on them like on an unreadable reading, and the sentence says why.
func TestARuleInputThatIsNotFiniteHoldsTheLeaseBack(t *testing.T) {
	nan := math.NaN()
	need := HostLoad{TouchGiB: 10, CommitGiB: 10}
	for name, tc := range map[string]struct {
		mem      HostMemory
		pending  HostLoad
		headroom float64
	}{
		"a pending touched set of NaN": {referenceWindowsHost, HostLoad{TouchGiB: nan}, 8},
		"a pending commit of NaN":      {referenceWindowsHost, HostLoad{CommitGiB: nan}, 8},
		"a headroom of NaN":            {referenceWindowsHost, HostLoad{}, nan},
		"an available memory of NaN":   {HostMemory{PhysicalGiB: 127.7, AvailableGiB: nan, CommitUsedGiB: 70.5, CommitLimitGiB: 171.7}, HostLoad{}, 8},
		"a commit charge of NaN":       {HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: nan, CommitLimitGiB: 171.7}, HostLoad{}, 8},
		"a commit limit of NaN":        {HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 70.5, CommitLimitGiB: nan}, HostLoad{}, 8},
		"a physical RAM of NaN":        {HostMemory{PhysicalGiB: nan, AvailableGiB: 75.6, CommitUsedGiB: 70.5, CommitLimitGiB: 171.7}, HostLoad{}, 8},
	} {
		t.Run(name, func(t *testing.T) {
			got := hostRAMAdmitsLoad(tc.mem, true, true, need, tc.pending, tc.headroom)
			if got.OK || !got.Unreadable || got.Impossible {
				t.Fatalf("want the lease to wait as on an unreadable reading, got ok=%v unreadable=%v impossible=%v (%s)", got.OK, got.Unreadable, got.Impossible, got.Why)
			}
			if !strings.Contains(got.Why, "finite") {
				t.Errorf("the sentence must say a figure is not a finite number: %q", got.Why)
			}
		})
	}
}

// The verdict of a reading with a figure that is not a number is unknown, not OK.
func TestTheVerdictOfAReadingThatIsNotFiniteIsUnknown(t *testing.T) {
	nan := math.NaN()
	for name, mem := range map[string]HostMemory{
		"physical":  {PhysicalGiB: nan, AvailableGiB: 75.6, CommitUsedGiB: 70.5, CommitLimitGiB: 171.7},
		"available": {PhysicalGiB: 127.7, AvailableGiB: nan, CommitUsedGiB: 70.5, CommitLimitGiB: 171.7},
		"committed": {PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: nan, CommitLimitGiB: 171.7},
		"limit":     {PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 70.5, CommitLimitGiB: nan},
	} {
		if v := mem.Verdict(8); v != HostUnknown {
			t.Errorf("a reading whose %s is NaN: verdict %s, want unknown", name, v)
		}
	}
	if v := referenceWindowsHost.Verdict(math.NaN()); v != HostUnknown {
		t.Errorf("a headroom of NaN: verdict %s, want unknown", v)
	}
	if v := referenceWindowsHost.Verdict(8); v != HostOK {
		t.Errorf("the reference reading stays OK, got %s", v)
	}
}

// A load whose figure is NaN declares something: "declares nothing" is the answer that skips the rule altogether, and the rule is
// where such a figure is refused.
func TestALoadWithAFigureThatIsNotANumberDeclaresHostRAM(t *testing.T) {
	for name, l := range map[string]HostLoad{
		"a touched set of NaN": {TouchGiB: math.NaN()},
		"a commit of NaN":      {CommitGiB: math.NaN()},
		"a touched set of Inf": {TouchGiB: math.Inf(1)},
	} {
		if !l.Declares() {
			t.Errorf("%s: Declares() = false, want true: the rule must see it", name)
		}
	}
	if (HostLoad{}).Declares() || (HostLoad{TouchGiB: -1, CommitGiB: -1}).Declares() {
		t.Error("a load of nothing (or of negative figures, which are clamped to nothing) declares nothing")
	}
}
