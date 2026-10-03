package gpulease

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func fourCards() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: "GPU-aaaa0000", NvidiaIndex: 0, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15, ComfyOrder: -1},
		{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15, ComfyOrder: -1},
		{UUID: "GPU-cccc0000", NvidiaIndex: 2, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15, ComfyOrder: -1},
		{UUID: "GPU-dddd0000", NvidiaIndex: 3, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15, ComfyOrder: -1},
	}
}

// A queued `--cards N` takes the cards that are free RIGHT NOW first and tops the set up
// from the claimed ones that would fit: a card that is idle must not be thrown away while
// the request waits for busy ones (the operator's order: a lease on one card when it could
// use three is unacceptable).
func TestAllocatorWaitableListsFreeCardsBeforeClaimedOnes(t *testing.T) {
	in := AllocInput{
		Cards: fourCards(), Min: 2, Max: 2, HostFreeOK: true, HostFreeGiB: 64,
		Claimed: map[string]bool{"gpu-aaaa0000": true, "gpu-bbbb0000": true, "gpu-cccc0000": true},
	}
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("three of four cards are claimed and two are wanted: %v", err)
	}
	want := []string{"gpu-dddd0000", "gpu-aaaa0000", "gpu-bbbb0000", "gpu-cccc0000"}
	if !reflect.DeepEqual(none.Waitable, want) {
		t.Fatalf("Waitable = free cards first, then claimed ones in allocation order:\n got %v\nwant %v", none.Waitable, want)
	}
	if none.Have != 1 {
		t.Errorf("Have counts the cards that are free now: %d", none.Have)
	}
}

// A card that cannot be taken for a reason that is not a live lease is never offered as a
// card to wait for: waiting does not fix a display card, a quarantine, a foreign process or
// a VRAM shortfall.
func TestAllocatorWaitableExcludesCardsThatWaitingDoesNotFree(t *testing.T) {
	cards := fourCards()
	cards[1].Display = true
	cards[3].VRAMFreeGiB = 1
	in := AllocInput{
		Cards: cards, Min: 2, Max: 2, HostFreeOK: true, HostFreeGiB: 64, FootprintGiB: 8,
		Claimed:     map[string]bool{"gpu-aaaa0000": true},
		Quarantined: map[string]bool{"gpu-cccc0000": true},
	}
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("want NoCardsError: %v", err)
	}
	// Only card a (claimed, would fit) is waitable: b is the display card, c is
	// quarantined, d has no room.
	if want := []string{"gpu-aaaa0000"}; !reflect.DeepEqual(none.Waitable, want) {
		t.Fatalf("Waitable = %v, want %v", none.Waitable, want)
	}
}
