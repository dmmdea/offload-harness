package modelaffinity

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// Register C-60 under real concurrency: N goroutines race for ONE slot, launched
// in the REVERSE of their registration order. They are admitted strictly in
// registration order and never two at once. (The pre-C-60 count and a LIFO
// count both fail this and the older sequential tests.)
func TestAwaitSeatSlotAdmitsConcurrentWaitersOneAtATimeInRegistrationOrder(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	running, err := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseRunning})
	if err != nil {
		t.Fatal(err)
	}
	const n = 3
	hs := make([]*gpuactivity.Handle, n)
	for i := range hs {
		time.Sleep(5 * time.Millisecond) // distinct registration milliseconds
		if hs[i], err = reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var order []int
	var inSlot, maxInSlot atomic.Int64
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := n - 1; i >= 0; i-- {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if errs[i] = AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", hs[i].ID(), 1, time.Now().Add(20*time.Second)); errs[i] != nil {
				return
			}
			cur := inSlot.Add(1)
			for m := maxInSlot.Load(); cur > m && !maxInSlot.CompareAndSwap(m, cur); m = maxInSlot.Load() {
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			hs[i].Phase(gpuactivity.PhaseRunning) // past the gate: holds the slot
			time.Sleep(60 * time.Millisecond)
			inSlot.Add(-1)
			hs[i].End()
		}(i)
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	early := len(order)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("%d waiters were admitted while the running run held the slot", early)
	}
	running.End()
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("waiter %d was refused with the slot free in turn: %v", i, e)
		}
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("admission order = %v, want registration order", order)
		}
	}
	if maxInSlot.Load() != 1 {
		t.Fatalf("max concurrently admitted = %d, want 1 (cap 1)", maxInSlot.Load())
	}
}

// Two waiters registered in the SAME millisecond (a burst of spread legs
// starting together): the id breaks the tie, so exactly one is ahead of the
// other. Mutant: dropping `|| (r.StartedAtMs == selfStart && r.ID < selfID)`
// admits both at cap 1 (the existing tests sleep 5 ms between registrations and
// never reach this branch).
func TestAwaitSeatSlotBreaksASameMillisecondTieByID(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	const ms = int64(1_900_000_000_000)
	a, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission, StartedAtMs: ms})
	b, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission, StartedAtMs: ms})
	defer a.End()
	defer b.End()
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for _, h := range []*gpuactivity.Handle{a, b} {
		wg.Add(1)
		go func(h *gpuactivity.Handle) {
			defer wg.Done()
			if AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", h.ID(), 1, time.Now().Add(400*time.Millisecond)) == nil {
				admitted.Add(1)
			}
		}(h)
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted = %d of 2 same-millisecond waiters at cap 1, want exactly 1", admitted.Load())
	}
}

// A waiter that gives up and ends its registration lets the next in line
// through; an UNREGISTERED caller (its own record not listed) counts every
// waiter, the conservative reading. Mutant for the second half: dropping
// `!found ||` in count().
func TestAwaitSeatSlotAnAbandonedWaiterDoesNotStrandTheQueue(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	running, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseRunning})
	defer running.End()
	time.Sleep(5 * time.Millisecond)
	w1, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission})
	time.Sleep(5 * time.Millisecond)
	w2, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission})
	defer w2.End()
	// cap 2: w1 sees only the running run (1 < 2) and is admitted; w2 sees running + w1 ahead (2) and waits.
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", w1.ID(), 2, time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("w1: %v", err)
	}
	done2 := make(chan error, 1)
	go func() {
		done2 <- AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", w2.ID(), 2, time.Now().Add(20*time.Second))
	}()
	select {
	case err := <-done2:
		t.Fatalf("w2 was admitted behind w1 and the running run: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	w1.End() // w1 abandons
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("w2: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("w2 never moved up after w1 ended")
	}
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", "not-registered", 1, time.Now().Add(150*time.Millisecond)); !IsSeatSlotError(err) {
		t.Fatalf("an unregistered caller must count every waiter, got %v", err)
	}
}
