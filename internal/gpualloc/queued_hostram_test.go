package gpualloc

// G6 of the P0 plan at the allocator: QueuedClaims treats every waiter's cards as spoken for, except that a
// waiter which waits ONLY on host RAM does not hold its cards against a request that declares none
// (gpulease.Waiter.HoldsItsCardsAgainst). The gate lets that request pass, so the allocator must not steer it away
// from a card that is free to it, or tell a call that cannot wait that the card is promised to somebody.

import (
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func TestQueuedClaimsLeavesAHostRAMWaitersCardFreeToARequestThatDeclaresNone(t *testing.T) {
	cards := threeCards()
	m := scratchManager(t)
	var commit atomic.Uint64
	setCommit := func(gib float64) { commit.Store(math.Float64bits(gib)) }
	setCommit(90) // 90 + 30 > 100 - 8: the waiter below is short of memory, its card is free
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 100 - math.Float64frombits(commit.Load())/2, CommitUsedGiB: math.Float64frombits(commit.Load()), CommitLimitGiB: 160}, true
	})
	t.Cleanup(restore)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Waits on host RAM until the test lets the host recover (the cleanup below), then takes the card and leaves.
		if l, err := m.Acquire(gpulease.ClassMedia, gpulease.Options{Reason: "krea2", TTL: time.Hour, Devices: []string{cards[0].LeaseID()},
			HostRAMGiB: 30, Wait: 10 * time.Second, WaitOut: true}); err == nil {
			_ = l.Release()
		}
	}()
	t.Cleanup(func() {
		setCommit(0)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the host-RAM waiter never ended")
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if ws := m.Waiters(); len(ws) == 1 && ws[0].WaitingFor == gpulease.WaitHostRAM {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want one waiter on host RAM, got %+v", m.Waiters())
		}
	}

	if got := QueuedClaims(m, cards, "", true); !got[cards[0].LeaseID()] {
		t.Errorf("a request that declares host RAM queues behind the waiter, so its card is not free to it: %v", got)
	}
	if got := QueuedClaims(m, cards, "", false); got[cards[0].LeaseID()] {
		t.Errorf("a request that declares none passes a waiter that waits only on host RAM, so its card is free to it: %v", got)
	}
}
