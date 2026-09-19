package storesteward

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestEveryTicksOnTimeAndStopsWithTheContext (0.130.1, register B-48/B-16): the
// job tick cannot fire while a GPU lease keeps fleet jobs off the node, and that
// is exactly when a bench arm or the pair seat writes ~1 GB/min of pages — on
// 2026-09-18 the dataset went 53 → 71 GB (ENOSPC) between two job ticks. The
// time tick calls Tick on a clock, and a non-positive period is a disabled tick.
func TestEveryTicksOnTimeAndStopsWithTheContext(t *testing.T) {
	var n atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Every(ctx, 5*time.Millisecond, func() { n.Add(1) }); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n.Load() < 3 {
		t.Fatalf("time tick fired %d times in 2 s at a 5 ms period", n.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Every did not return after the context was cancelled")
	}
	// Disabled: a non-positive period returns at once without ticking.
	var m atomic.Int32
	Every(context.Background(), 0, func() { m.Add(1) })
	if m.Load() != 0 {
		t.Fatalf("a zero period must not tick, got %d", m.Load())
	}
}
