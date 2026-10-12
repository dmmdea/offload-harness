package gpualloc

// F58 (c): the allocator's pre-filter applies the grant's host-RAM rule before it picks a card, and the rule's
// third term is the part of the leases ALREADY GRANTED that has not loaded yet (gpuprobe.HostRAMAdmits: committed + need +
// pending). BuildInput feeds it from Manager.HostRAMPending. Deleting that one line (HostPendingGiB) left every test green,
// so the allocator would pick a card for a job the grant is about to refuse because a lane granted a moment earlier was
// still loading its weights: the very second-lane-into-the-gap the term exists to stop.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func TestBuildInputCarriesWhatGrantedLeasesHaveYetToLoadAndTheAllocatorUsesIt(t *testing.T) {
	cards := threeCards()
	m := scratchManager(t)
	// The grant below reads the process-wide host reading: roomy, so the holder is admitted.
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return roomyHostMemory, true })
	t.Cleanup(restore)
	// A lane already granted on card 0 that declared 40 GiB and has loaded nothing yet (no process below this test's own).
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "krea2", TTL: time.Hour, Devices: []string{cards[0].LeaseID()}, HostRAMGiB: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	// The allocator's own host: 100 GiB, 30 committed. A 40 GiB need alone fits (30 + 40 <= 100 - 8); with the 40 GiB the
	// holder has yet to load it does not (30 + 40 + 40 > 92).
	deps := hostDeps(cards)
	deps.HostMemory = func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 85, CommitUsedGiB: 30, CommitLimitGiB: 160}, true
	}
	cfg := config.Config{}
	in, err := BuildInput(context.Background(), m, cfg, Need{RAMGiB: 40}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if in.HostPendingGiB < 39 || in.HostPendingGiB > 40.001 {
		t.Fatalf("HostPendingGiB = %.2f, want the 40 GiB the granted lease has not loaded (Manager.HostRAMPending)", in.HostPendingGiB)
	}
	in.Min, in.Max = 1, 1
	_, aerr := gpulease.Allocate(in)
	var none *gpulease.NoCardsError
	if !errors.As(aerr, &none) || none.HostReason == "" {
		t.Fatalf("with 40 GiB still to load the allocator must refuse on host RAM, got %v", aerr)
	}

	// Control: with nothing pending the same request is allocated, so the refusal above is the pending term and no other.
	_ = holder.Release()
	in2, err := BuildInput(context.Background(), m, cfg, Need{RAMGiB: 40}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if in2.HostPendingGiB != 0 {
		t.Fatalf("no lease is granted any more, so nothing is pending: %.2f", in2.HostPendingGiB)
	}
	in2.Min, in2.Max = 1, 1
	if _, err := gpulease.Allocate(in2); err != nil {
		t.Fatalf("a 40 GiB need fits a host with 30 committed and nothing pending: %v", err)
	}
}
