package gpuprobe

import (
	"reflect"
	"testing"
)

// ScreenCardIndexes names the cards the driver says drive a monitor, so the display_floor guard can
// contradict a declared display card on positive evidence. It answers nothing when the reading cannot
// say, because a guard that "knew" the screen's card from no evidence would refuse a correct config.
func TestScreenCardIndexes(t *testing.T) {
	three := []Device{
		{Index: 0, UUID: "GPU-aaaa1111"},
		{Index: 1, UUID: "GPU-bbbb2222", DisplayAttached: true},
		{Index: 2, UUID: "GPU-cccc3333"},
	}
	if got := ScreenCardIndexes(three); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("monitor attached to card 1: got %v", got)
	}

	// The board reordered on a power loss: the card the monitor is on is now enumerated as 0, and
	// the screen is lit, so display_active carries it.
	moved := []Device{
		{Index: 0, UUID: "GPU-bbbb2222", DisplayActive: true},
		{Index: 1, UUID: "GPU-aaaa1111"},
		{Index: 2, UUID: "GPU-cccc3333"},
	}
	if got := ScreenCardIndexes(moved); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the monitor's card re-enumerated as 0: got %v", got)
	}

	// Two cards drive a display (a second monitor): both are named, in the probe's order.
	two := []Device{
		{Index: 0, UUID: "GPU-aaaa1111", DisplayAttached: true},
		{Index: 1, UUID: "GPU-bbbb2222"},
		{Index: 2, UUID: "GPU-cccc3333", DisplayActive: true},
	}
	if got := ScreenCardIndexes(two); !reflect.DeepEqual(got, []string{"0", "2"}) {
		t.Fatalf("two screen cards: got %v", got)
	}

	// Nothing drives a display (headless, or the screen is on an iGPU): the reading says nothing.
	if got := ScreenCardIndexes([]Device{{Index: 0, UUID: "GPU-aaaa1111"}, {Index: 1, UUID: "GPU-bbbb2222"}}); got != nil {
		t.Fatalf("no card drives a display: got %v, want nil", got)
	}

	// A reading taken after a transient failure carries no display_attached, so it cannot tell the
	// monitor's card from the others. Even a card that reads display_active is not evidence then.
	unknown := []Device{
		{Index: 0, UUID: "GPU-aaaa1111", AttachedUnknown: true},
		{Index: 1, UUID: "GPU-bbbb2222", DisplayActive: true, AttachedUnknown: true},
	}
	if got := ScreenCardIndexes(unknown); got != nil {
		t.Fatalf("an unknown display_attached must say nothing, got %v", got)
	}

	if got := ScreenCardIndexes(nil); got != nil {
		t.Fatalf("no devices, no answer: %v", got)
	}
}
