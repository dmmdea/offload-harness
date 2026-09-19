package modelaffinity

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// Register C-42: with cap 1 and one run registered on the seat, a second run
// waits until the first ends, and never counts its own record.
func TestAwaitSeatSlotWaitsForARegisteredRunToEnd(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	first, err := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := reg.Begin(gpuactivity.Run{Seat: "SEAT", Kind: "agent_run"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.End()
	go func() {
		time.Sleep(150 * time.Millisecond)
		first.End()
	}()
	start := time.Now()
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", second.ID(), 1, time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("the slot must free when the first run ends: %v", err)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > 3*time.Second {
		t.Fatalf("waited %s; expected roughly the first run's remaining life", took)
	}
	// Alone on the seat now: no wait at all.
	start = time.Now()
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", second.ID(), 1, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("a run alone on the seat must not wait")
	}
}

// A seat that stays full past the deadline is a capacity refusal that names
// the seat, the cap and the runs ahead — and nothing is refused when the gate
// is disabled (cap 0 or no reader).
func TestAwaitSeatSlotFailsAtTheDeadlineAndIsOffWithoutACap(t *testing.T) {
	reg := gpuactivity.OpenAt(t.TempDir())
	a, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract"})
	defer a.End()
	b, _ := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract"})
	defer b.End()
	err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", "", 2, time.Now().Add(120*time.Millisecond))
	if !IsSeatSlotError(err) {
		t.Fatalf("a full seat must fail as a seat-slot error, got %v", err)
	}
	if e := err.(*SeatSlotError); e.Ahead != 2 || e.Cap != 2 || e.Seat != "seat" {
		t.Fatalf("error must name the seat, cap and runs ahead: %+v", e)
	}
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "seat", "", "", 0, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("cap 0 disables the gate: %v", err)
	}
	if err := AwaitSeatSlot(context.Background(), nil, "seat", "", "", 1, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("no reader disables the gate: %v", err)
	}
	// Runs on another seat do not count.
	if err := AwaitSeatSlot(context.Background(), reg.OnSeat, "other", "", "", 1, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("another seat's runs must not count: %v", err)
	}
}
