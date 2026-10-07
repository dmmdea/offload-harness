package placement

import (
	"strings"
	"testing"
)

// A seat on a box that pins by GPU UUID (the board re-enumerates indices) names its card by UUID. The
// floor's arithmetic has to know whether that seat is the one on the display card: comparing the UUID
// string with the card's index never matched, so such a seat read as "pinned off the display card" and
// was judged as free - 0, the check the display footprint exists to replace. The UUID is resolved
// through the probe, and one that cannot be resolved counts as on the card.

func TestAUUIDPinnedSeatOnTheDisplayCardPaysItsFootprintAgainstTheFloor(t *testing.T) {
	l, s := displayLayerAndSeat(t) // display_device "1", floor 4, footprint 6.2
	s.Device = "GPU-bbbb2222"
	f := admitting()
	f.index = map[string]string{"GPU-bbbb2222": "1", "GPU-aaaa1111": "0"}

	if ok, reason, _ := LayerAdmissible(l, s, f.live(), nil); !ok {
		t.Fatalf("15 free - 6.2 = 8.8 over the floor: %q", reason)
	}
	f.free = map[string]float64{"0": 2, "1": 9, "2": 3}
	ok, reason, guard := LayerAdmissible(l, s, f.live(), nil)
	if ok || guard != "display_floor" || !strings.Contains(reason, "footprint 6.2") {
		t.Fatalf("9 free - 6.2 = 2.8 under the floor: the UUID-pinned seat is on the card and pays its footprint, got ok=%v %q", ok, reason)
	}
}

func TestAUUIDPinnedSeatOffTheDisplayCardPaysNothing(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	s.Device = "GPU-aaaa1111" // card 0
	f := admitting()
	f.index = map[string]string{"GPU-bbbb2222": "1", "GPU-aaaa1111": "0"}
	f.free = map[string]float64{"0": 2, "1": 9, "2": 3}
	if ok, reason, _ := LayerAdmissible(l, s, f.live(), nil); !ok {
		t.Fatalf("a seat on card 0 puts nothing on the display card (9 free >= 4): %q", reason)
	}
}

func TestAUUIDPinThatCannotBeResolvedIsTreatedAsOnTheDisplayCard(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	s.Device = "GPU-zzzz9999" // in no probe
	f := admitting()
	f.index = map[string]string{"GPU-bbbb2222": "1"}
	f.free = map[string]float64{"0": 2, "1": 9, "2": 3}
	if ok, reason, _ := LayerAdmissible(l, s, f.live(), nil); ok || !strings.Contains(reason, "footprint 6.2") {
		t.Fatalf("an unresolvable pin cannot be shown to be off the card: it pays the footprint, got ok=%v %q", ok, reason)
	}
	s.DisplayFootprintGiB = 0
	if ok, reason, _ := LayerAdmissible(l, s, f.live(), nil); ok || !strings.Contains(reason, "display footprint undeclared") {
		t.Fatalf("and with no footprint declared it refuses before any arithmetic, got ok=%v %q", ok, reason)
	}
	// No reader at all is the same.
	f.index = nil
	if ok, reason, _ := LayerAdmissible(l, s, f.live(), nil); ok || !strings.Contains(reason, "display footprint undeclared") {
		t.Fatalf("no probe to resolve a UUID is no proof the seat is off the card, got ok=%v %q", ok, reason)
	}
}
