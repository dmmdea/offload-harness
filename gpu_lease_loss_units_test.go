package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Unit tests of the helpers the C-59 fix added: the cancellable drain, the
// re-queue budget and the detach form's truthful error. The behaviour they
// serve is pinned end to end in gpu_lease_loss_test.go.

// The context ends a drain at once and leaves the seat alone: this is what the
// wrapper cancels when its lease is lost.
func TestMaintainSeatStopsAtOnceWhenItsContextIsCancelled(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.inflight.Store(1)
	cfgPath, m := handoffFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "drainer", TTL: time.Hour, Draining: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err = maintainSeatCtx(ctx, loadCfgPath(cfgPath), holder.Restamp, true, time.Now().Add(time.Minute), true, true, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled drain must return the context's error, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the drain outlived its context by %s", took)
	}
	if f.unloads.Load() != 0 {
		t.Fatalf("a cancelled drain must not unload the seat: unloads=%d", f.unloads.Load())
	}
}

// The queue budget a reserve has left after losing its lease: what remains of
// --wait, never under the drain floor, and still fail-fast for --wait 0.
func TestRequeueWaitIsWhatIsLeftOfTheQueueBudget(t *testing.T) {
	if got := requeueWait(time.Now().Add(-time.Hour), 8*time.Hour); got < 7*time.Hour-time.Minute || got > 7*time.Hour+time.Minute {
		t.Errorf("an hour into an 8h budget leaves ~7h, got %s", got)
	}
	if got := requeueWait(time.Now().Add(-9*time.Hour), 8*time.Hour); got != drainFloor {
		t.Errorf("a spent budget still gets the drain floor, got %s", got)
	}
	if got := requeueWait(time.Now().Add(-time.Hour), 0); got != 0 {
		t.Errorf("--wait 0 stays fail-fast, got %s", got)
	}
}

// After a failed drain on the DETACH form the error must not tell the caller
// the lease is still held when it is not: `gpu release` then frees someone
// else's.
func TestDetachedMaintainErrorSaysWhetherTheLeaseIsStillHeld(t *testing.T) {
	_, m := leaseFixture(t)
	cause := errors.New("drain of seat did not finish within 1m0s")
	lease, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Draining: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := detachedMaintainError(m, lease.Epoch(), cause); !errors.Is(got, cause) || !strings.Contains(got.Error(), "the lease is still held") {
		t.Errorf("the holder's own lease: want the still-held remedy, got %v", got)
	}
	// Another holder took the card.
	if _, err := m.ReleaseByEpoch(lease.Epoch()); err != nil {
		t.Fatal(err)
	}
	other, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "someone else", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Release() }()
	got := detachedMaintainError(m, lease.Epoch(), cause)
	if !errors.Is(got, cause) || strings.Contains(got.Error(), "still held") || !strings.Contains(got.Error(), "no longer this reservation's") {
		t.Errorf("another holder's lease: want the no-longer-held message, got %v", got)
	}
	// The card is free.
	_ = other.Release()
	if got := detachedMaintainError(m, lease.Epoch(), cause); strings.Contains(got.Error(), "still held") {
		t.Errorf("a free card: want the no-longer-held message, got %v", got)
	}
}
