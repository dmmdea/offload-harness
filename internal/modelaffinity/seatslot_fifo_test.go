package modelaffinity

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// Register C-60 (0.143.0): waiters must never block each other. Every run
// registers in phase admission BEFORE it reaches the seat gate, and the gate
// used to count every other registered run — so with the seat's slot FREE,
// two waiters each saw the other and both waited out their budgets (92 local
// runs refused this way on 2026-09-29). The first in line must get the free
// slot; the second must wait for it; neither may count one behind it.
func TestAwaitSeatSlotWaitersDoNotBlockEachOther(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	first, err := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission})
	if err != nil {
		t.Fatal(err)
	}
	defer first.End()
	time.Sleep(5 * time.Millisecond) // a later registration time
	second, err := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission})
	if err != nil {
		t.Fatal(err)
	}
	defer second.End()

	// cap 1, nothing running, two waiters: the FIRST in line is admitted at once.
	start := time.Now()
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", first.ID(), 1, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("the first waiter must take the free slot: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("the first waiter waited although the slot was free")
	}
	// The second is behind the first (which now holds the slot): it waits,
	// and is admitted the moment the first ends.
	first.Phase(gpuactivity.PhaseRunning)
	go func() {
		time.Sleep(150 * time.Millisecond)
		first.End()
	}()
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", second.ID(), 1, time.Now().Add(3*time.Second)); err != nil {
		t.Fatalf("the second waiter must be admitted when the first ends: %v", err)
	}
}

// A later waiter never counts toward an earlier one's wait, and a run past
// admission always does.
func TestAwaitSeatSlotCountsRunningRunsAndEarlierWaitersOnly(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	running, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseRunning})
	defer running.End()
	time.Sleep(5 * time.Millisecond)
	early, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission})
	defer early.End()
	time.Sleep(5 * time.Millisecond)
	late, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract", Phase: gpuactivity.PhaseAdmission})
	defer late.End()

	// cap 2: early sees 1 running + 0 earlier waiters = 1 < 2 -> admitted.
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", early.ID(), 2, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("early waiter: %v", err)
	}
	// late sees 1 running + 1 earlier waiter = 2 -> refused at a short deadline.
	err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", late.ID(), 2, time.Now().Add(200*time.Millisecond))
	if !IsSeatSlotError(err) {
		t.Fatalf("the late waiter must wait behind the running run and the earlier waiter, got %v", err)
	}
}
