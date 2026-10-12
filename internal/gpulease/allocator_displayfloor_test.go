package gpulease

import (
	"errors"
	"strings"
	"testing"
)

// The display card the cap has opened (DisplayCapPct) is still the desktop's. It is allocatable only
// while free VRAM, less what the job puts there, leaves the floor the display layer declares
// (DisplayFloorGiB): the same arithmetic as the layer's display_floor guard, so the two doors onto
// that card cannot disagree about it. These tests open the card to its whole VRAM (cap 100) so the
// declared floor is the only bound in play; allocator_displaycap_test.go holds the cap itself.

// pairResident is the box with the pair seat loaded on cards 0 and 2, so the display card is
// the one with no resident seat and would sort first.
func pairResident() map[string]ResidentInfo {
	return map[string]ResidentInfo{
		"gpu-aaaa0000": {Seats: []string{"agent-pool"}, CostGiB: 15},
		"gpu-cccc0000": {Seats: []string{"agent-pool"}, CostGiB: 15},
	}
}

func openedDisplayInput(footprint, floor float64) AllocInput {
	in := baseInput() // card 1 is the display card with 12 GiB free
	in.DisplayCapPct = 100
	in.FootprintGiB = footprint
	in.DisplayFloorGiB = floor
	in.Resident = pairResident()
	return in
}

func TestAnOpenedDisplayCardIsHeldToTheDesktopFloor(t *testing.T) {
	cases := []struct {
		name      string
		footprint float64
		floor     float64
		wantTaken bool
	}{
		{"9 of 12 free leaves 3, under a 4 floor", 9, 4, false},
		{"6 of 12 free leaves 6, over the floor", 6, 4, true},
		{"8 of 12 free leaves exactly the floor", 8, 4, true},
		{"a hair over the floor's edge", 8.01, 4, false},
		{"no footprint declared cannot be shown to keep the floor", 0, 4, false},
		{"no floor declared is the behaviour before the floor existed", 0, 0, true},
		{"no floor declared, any footprint that fits", 11, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Allocate(openedDisplayInput(tc.footprint, tc.floor))
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Devices) != 1 {
				t.Fatalf("one card was asked for, got %v", a.Devices)
			}
			taken := a.Devices[0] == "gpu-bbbb0000"
			if taken != tc.wantTaken {
				t.Fatalf("display card taken = %v, want %v (picked %v, skipped %+v)", taken, tc.wantTaken, a.Devices, a.Skipped)
			}
			if !tc.wantTaken {
				var s Skip
				for _, x := range a.Skipped {
					if x.ID == "gpu-bbbb0000" {
						s = x
					}
				}
				if s.Reason != ReasonVRAM || !strings.Contains(s.Detail, "desktop floor") {
					t.Errorf("the display card must be skipped as %q naming the floor, got %+v", ReasonVRAM, s)
				}
			}
		})
	}
}

func TestAnUndeclaredFootprintOnTheDisplayCardSaysHowToDeclareIt(t *testing.T) {
	a, err := Allocate(openedDisplayInput(0, 4))
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, s := range a.Skipped {
		if s.ID == "gpu-bbbb0000" {
			detail = s.Detail
		}
	}
	if !strings.Contains(detail, "--vram") {
		t.Fatalf("a refusal for a missing footprint must name the flag that supplies it, got %q", detail)
	}
}

// The floor is the desktop's, so it applies to the card the screen is on and to no other: a
// pair card that a 15 GiB job leaves with 0.5 GiB free is allocatable, as it always was.
func TestTheDesktopFloorTouchesOnlyTheDisplayCard(t *testing.T) {
	in := baseInput()
	in.DisplayCapPct = 100
	in.FootprintGiB = 15
	in.DisplayFloorGiB = 4
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-aaaa0000" {
		t.Fatalf("card 0 (15.5 free, 15 needed) must still be allocatable: %v, skipped %+v", a.Devices, a.Skipped)
	}
}

// A declared floor opens nothing: with the cap at 0 (the operator needs the card) the display card is not
// allocatable at all, floor or no floor.
func TestADeclaredFloorDoesNotOpenAClosedDisplayCard(t *testing.T) {
	in := openedDisplayInput(2, 4)
	in.DisplayCapPct = 0
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := skipReason(a, "gpu-bbbb0000"); got != ReasonDisplay {
		t.Fatalf("with the cap closed the display card is %q, got %q", ReasonDisplay, got)
	}
}

// A queued request may wait on a card a live lease holds only if the card would take the job
// once the lease ends. The display card that would not clear the floor is never worth waiting
// for, so it must not be offered as a card to queue on.
func TestAHeldDisplayCardThatCouldNotClearTheFloorIsNotQueuedOn(t *testing.T) {
	for _, tc := range []struct {
		footprint  float64
		wantQueued bool
	}{
		{6, true},   // 12 - 6 = 6 >= 4: it fits once the holder leaves
		{10, false}, // 12 - 10 = 2 < 4: it never would
	} {
		in := openedDisplayInput(tc.footprint, 4)
		in.Min, in.Max = 3, 3 // all three cards are wanted, so the request cannot be met now
		in.Claimed = map[string]bool{"gpu-bbbb0000": true}
		in.Resident = nil
		_, err := Allocate(in)
		var none *NoCardsError
		if !errors.As(err, &none) {
			t.Fatalf("footprint %v: want a NoCardsError, got %v", tc.footprint, err)
		}
		queued := false
		for _, id := range none.Waitable {
			if id == "gpu-bbbb0000" {
				queued = true
			}
		}
		if queued != tc.wantQueued {
			t.Errorf("footprint %v: display card waitable = %v, want %v (waitable %v)", tc.footprint, queued, tc.wantQueued, none.Waitable)
		}
	}
}

// Order among allocatable cards is "no resident seat first, then the cheapest eviction": a loaded
// display twin makes the display card a card with a seat on it, costed at what the twin holds, so
// it does not sort ahead of a card that costs less to take.
func TestALoadedTwinOnTheDisplayCardIsAnEvictionNotAnEmptyCard(t *testing.T) {
	in := baseInput()
	in.DisplayCapPct = 100
	in.FootprintGiB = 2
	in.DisplayFloorGiB = 4
	in.Min, in.Max = 1, 1
	in.Resident = map[string]ResidentInfo{
		"gpu-aaaa0000": {Seats: []string{"small-seat"}, CostGiB: 3},
		"gpu-bbbb0000": {Seats: []string{"gemma-4-e4b-display"}, CostGiB: 6.2},
		"gpu-cccc0000": {Seats: []string{"small-seat"}, CostGiB: 3},
	}
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-aaaa0000" {
		t.Fatalf("a 3 GiB eviction on card 0 must beat the 6.2 GiB twin on the display card, got %v", a.Devices)
	}
	// The same box with the twin NOT seen: the display card reads as empty and sorts first. This is
	// the ordering the twin's residency exists to change.
	in.Resident = map[string]ResidentInfo{
		"gpu-aaaa0000": {Seats: []string{"small-seat"}, CostGiB: 3},
		"gpu-cccc0000": {Seats: []string{"small-seat"}, CostGiB: 3},
	}
	a, err = Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-bbbb0000" {
		t.Fatalf("with no resident seat seen on it the display card sorts first, got %v", a.Devices)
	}
}
