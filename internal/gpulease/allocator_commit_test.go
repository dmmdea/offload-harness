package gpulease

// The card allocator applies the grant's host-RAM rule before it picks cards (allocator.go), and the grant's rule keeps three
// commit states apart: nothing declared, a commit nobody stated (held to physical RAM, with what running leases have yet to touch)
// and a STATED commit (held to the commit limit, with what running leases have yet to be charged). The allocator's input carries
// the stated figure (HostNeedCommitGiB above 0) and the pending in both quantities; these tests are the ones that fail when it
// reads either wrong, so it cannot pick cards the grant is about to refuse (or refuse cards the grant would give).

import (
	"errors"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// 100 GiB of RAM, a 160 GiB commit limit, 8 GiB headroom: the line for a commit nobody stated is 92, for a stated one 152.
func commitFilterInput(committed float64) AllocInput {
	in := baseInput()
	in.HostMem = gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: committed, CommitLimitGiB: 160}
	in.HostHeadroomGiB = 8
	return in
}

func hostRefusal(t *testing.T, in AllocInput) *NoCardsError {
	t.Helper()
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) || none.HostReason == "" {
		t.Fatalf("want a refusal on host RAM, got %v", err)
	}
	return none
}

func TestTheAllocatorJudgesAStatedCommitAsItsOwnFigure(t *testing.T) {
	// A commit nobody stated is the touched figure on the physical line: 50 + 10 <= 92.
	in := commitFilterInput(50)
	in.HostNeedGiB = 10
	if _, err := Allocate(in); err != nil {
		t.Fatalf("10 GiB declared once fits (50 + 10 <= 92): %v", err)
	}
	// The same working set with a commit stated at 110 does not fit the commit limit (50 + 110 > 152): refused on the commit.
	in.HostNeedCommitGiB = 110
	if none := hostRefusal(t, in); none.HostImpossible {
		t.Fatalf("a commit that waiting can cure is not impossible: %+v", none)
	}
	// A commit stated BELOW the working set is taken as stated: 60 touched of 90 available, 5 committed on 140 of a 152 bound fits,
	// where the 60 declared once would be 200 against 92.
	in = commitFilterInput(140)
	in.HostNeedGiB, in.HostNeedCommitGiB = 60, 5
	if _, err := Allocate(in); err != nil {
		t.Fatalf("a stated commit of 5 fits the commit limit (140 + 5 <= 152): %v", err)
	}
	in.HostNeedCommitGiB = 0
	hostRefusal(t, in)
}

func TestTheAllocatorCountsWhatRunningLeasesHaveYetToBeChargedAgainstAStatedCommit(t *testing.T) {
	in := commitFilterInput(50)
	in.HostNeedGiB, in.HostNeedCommitGiB = 10, 10 // stated, equal to the working set
	if _, err := Allocate(in); err != nil {
		t.Fatalf("a stated 10 on 50 committed fits: %v", err)
	}
	// 100 GiB of commit the running leases have yet to be charged (and nothing yet to touch): 50 + 10 + 100 > 152.
	in.HostPending = gpuprobe.HostLoad{CommitGiB: 100}
	hostRefusal(t, in)
	// The same pending is invisible to a commit nobody stated, which is held to what running leases have yet to TOUCH.
	in.HostNeedCommitGiB = 0
	if _, err := Allocate(in); err != nil {
		t.Fatalf("a commit nobody stated is held to the pending touch (none here): %v", err)
	}
	in.HostPending = gpuprobe.HostLoad{TouchGiB: 40}
	hostRefusal(t, in) // 50 + 10 + 40 > 92
}
