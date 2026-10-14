package placement

import (
	"strings"
	"testing"
)

// A twin that is already loaded is held to the card the driver reports for the monitor, as the admission
// guard is: the watcher in fleet-serve asks ResidentVerdict, and a display_device of "auto" must read the
// derived card there, not the string.
func TestResidentVerdictReadsTheDerivedCardForAnAutoLayer(t *testing.T) {
	l, _ := autoLayerAndSeat(t, "0")
	l.Guards = []string{"display_floor", "presence"}

	// The monitor is on 0 and card 0 is under the floor; card 1, the seeded pin, has room.
	f := admitting()
	f.free["0"], f.free["1"] = 3, 15
	live := f.live()
	live.ScreenCards = screenAt("0")
	ok, reasons := ResidentVerdict(l, live)
	joined := strings.Join(reasons, " | ")
	if ok || !strings.Contains(joined, "free 3.0 GiB < floor 4.0 GiB on display device auto (index 0)") {
		t.Fatalf("a resident twin under the floor on the monitor's card is a violation naming index 0, got ok=%v %q", ok, joined)
	}

	// The floor kept on the monitor's card: clean, whatever card 1 holds.
	f.free["0"], f.free["1"] = 6, 1
	live = f.live()
	live.ScreenCards = screenAt("0")
	if ok, reasons := ResidentVerdict(l, live); !ok {
		t.Fatalf("6.0 GiB free on the monitor's card keeps the floor, got %v", reasons)
	}

	// The monitor moves: the same numbers now read card 1, which is under the floor.
	live.ScreenCards = screenAt("1")
	ok, reasons = ResidentVerdict(l, live)
	if ok || !strings.Contains(strings.Join(reasons, " "), "(index 1)") {
		t.Fatalf("the verdict follows the monitor to index 1, got ok=%v %v", ok, reasons)
	}
}
