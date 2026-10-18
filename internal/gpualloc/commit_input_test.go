package gpualloc

import (
	"context"
	"errors"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A commit the job STATES reaches the allocator's input as its own figure, and the allocator judges it as the grant does. 100 GiB
// of RAM, a 160 GiB commit limit, 50 committed, 90 available, the default 8 GiB headroom: a commit nobody stated is held to 92, a
// stated one to 152, so 10 GiB declared once fits and 10 GiB with a commit stated at 110 does not.
func TestBuildInputCarriesAStatedCommitAndTheAllocatorJudgesIt(t *testing.T) {
	cards := threeCards()
	m := scratchManager(t)
	deps := hostDeps(cards)
	deps.HostMemory = func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 50, CommitLimitGiB: 160}, true
	}
	cfg := config.Config{}

	in, err := BuildInput(context.Background(), m, cfg, Need{RAMGiB: 10, RAMCommitGiB: 110}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if in.HostNeedGiB != 10 || in.HostNeedCommitGiB != 110 {
		t.Fatalf("the input carries the working set and the stated commit: touched %v commit %v", in.HostNeedGiB, in.HostNeedCommitGiB)
	}
	in.Min, in.Max = 1, 1
	_, aerr := gpulease.Allocate(in)
	var none *gpulease.NoCardsError
	if !errors.As(aerr, &none) || none.HostReason == "" {
		t.Fatalf("a stated commit of 110 on 50 committed does not fit the commit limit: want a host refusal, got %v", aerr)
	}

	// Control: the same working set with no commit stated is held to the physical line and fits.
	one, err := BuildInput(context.Background(), m, cfg, Need{RAMGiB: 10}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if one.HostNeedCommitGiB != 0 {
		t.Fatalf("no commit stated reaches the input as none: %v", one.HostNeedCommitGiB)
	}
	one.Min, one.Max = 1, 1
	if _, err := gpulease.Allocate(one); err != nil {
		t.Fatalf("10 GiB declared once fits (50 + 10 <= 92): %v", err)
	}

	// The flag itself: a commit of 60 stated fits the limit (50 + 60 = 110 <= 152) and the same 60 not stated does not fit physical
	// RAM (110 > 92), so the allocator admits it only if it knows the commit was stated. The 110 above is refused either way.
	stated, err := BuildInput(context.Background(), m, cfg, Need{RAMGiB: 10, RAMCommitGiB: 60}, deps)
	if err != nil {
		t.Fatal(err)
	}
	stated.Min, stated.Max = 1, 1
	if _, err := gpulease.Allocate(stated); err != nil {
		t.Fatalf("a stated commit of 60 on 50 committed fits the commit limit (110 <= 152) and is admitted only as a stated one: %v", err)
	}
	unstated := stated
	unstated.HostNeedCommitGiB = 0
	unstated.HostNeedGiB = 60
	if _, err := gpulease.Allocate(unstated); err == nil {
		t.Fatal("the same 60 GiB declared once is held to physical RAM (50 + 60 = 110 > 92) and must be refused: the fixture no longer tells a stated commit from an unstated one")
	}
}
