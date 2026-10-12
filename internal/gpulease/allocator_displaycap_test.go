package gpulease

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The display card is allocatable up to a share of its total VRAM, at all times (A-RAM-15, the operator's rule
// of 2026-10-11: "the display card may be used up to 60 percent at all times unless I say I need it"). The share
// is AllocInput.DisplayCapPct; the VRAM in use now, the desktop's own included, plus the job's declared footprint
// must stay at or under it. No presence term: the operator's presence opens nothing and closes nothing here.

// capCards is the canned card table of the reference three-card box: card 1 is the display card with 2.6 GiB in
// use of 15.9, so a 60 percent cap allows jobs up to 9.54 - 2.6 = 6.94 GiB there.
func capCards() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: "GPU-aaaa0000", NvidiaIndex: 0, Name: "T", VRAMTotalGiB: 15.9, VRAMFreeGiB: 15.5, ComfyOrder: -1},
		{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Name: "T", VRAMTotalGiB: 15.9, VRAMFreeGiB: 13.3, Display: true, ComfyOrder: -1},
		{UUID: "GPU-cccc0000", NvidiaIndex: 2, Name: "T", VRAMTotalGiB: 15.9, VRAMFreeGiB: 15.9, ComfyOrder: -1},
	}
}

// capInput asks for one card with the pair seat loaded on cards 0 and 2, so the display card is the one with no
// resident seat and sorts first whenever it is allocatable.
func capInput(footprint float64, capPct int) AllocInput {
	in := baseInput()
	in.Cards = capCards()
	in.DisplayCapPct = capPct
	in.FootprintGiB = footprint
	in.Resident = pairResident()
	return in
}

func skipOf(a Allocation, id string) Skip {
	for _, s := range a.Skipped {
		if s.ID == id {
			return s
		}
	}
	return Skip{}
}

func TestAllocatorOpensTheDisplayCardWithinTheCapWithoutPresence(t *testing.T) {
	a, err := Allocate(capInput(6, 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-bbbb0000" {
		t.Fatalf("2.6 GiB in use of 15.9 and a 6 GiB job make 8.6 GiB, under the 9.54 GiB cap: the display card (no resident seat) is the pick, got %v (skipped %+v)", a.Devices, a.Skipped)
	}
	if s := skipOf(a, "gpu-bbbb0000"); s.Reason != "" {
		t.Errorf("the display card was taken, so it is not skipped: %+v", s)
	}
}

func TestAllocatorRefusesTheDisplayCardPastTheCapAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		footprint float64
		wantTaken bool
	}{
		{6, true},
		{6.9, true},   // 2.6 + 6.9 = 9.5 <= 9.54
		{6.94, true},  // exactly the cap
		{6.95, false}, // a hair over
		{7.5, false},  // the plan's number: 10.1 GiB against 9.54
		{13.3, false}, // fits the free VRAM, not the cap
	} {
		a, err := Allocate(capInput(tc.footprint, 60))
		if err != nil {
			t.Fatalf("footprint %v: %v", tc.footprint, err)
		}
		if len(a.Devices) != 1 {
			t.Fatalf("footprint %v: one card was asked for, got %v", tc.footprint, a.Devices)
		}
		if taken := a.Devices[0] == "gpu-bbbb0000"; taken != tc.wantTaken {
			t.Errorf("footprint %v: display card taken = %v, want %v (picked %v, skipped %+v)", tc.footprint, taken, tc.wantTaken, a.Devices, a.Skipped)
		}
		if tc.wantTaken {
			continue
		}
		s := skipOf(a, "gpu-bbbb0000")
		if s.Reason != ReasonVRAM || !s.Display {
			t.Errorf("footprint %v: the display card must be skipped as %q and marked as the display card, got %+v", tc.footprint, ReasonVRAM, s)
		}
		for _, want := range []string{"60 %", "cap", "desktop floor"} {
			if !strings.Contains(s.Detail, want) {
				t.Errorf("footprint %v: the skip must name the cap (%q missing): %q", tc.footprint, want, s.Detail)
			}
		}
	}
}

func TestAllocatorUndeclaredFootprintStaysRefusedOnTheDisplayCard(t *testing.T) {
	a, err := Allocate(capInput(0, 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("a job that declares no footprint cannot be shown to stay under the cap: %v", a.Devices)
	}
	s := skipOf(a, "gpu-bbbb0000")
	if s.Reason != ReasonVRAM || !strings.Contains(s.Detail, "--vram") {
		t.Fatalf("the refusal is on VRAM and names the flag that supplies the footprint, got %+v", s)
	}
}

func TestAllocatorCapZeroKeepsTheDisplayCardClosed(t *testing.T) {
	a, err := Allocate(capInput(6, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("a cap of 0 is the operator saying he needs the card: %v", a.Devices)
	}
	s := skipOf(a, "gpu-bbbb0000")
	if s.Reason != ReasonDisplay || !strings.Contains(s.Detail, "gpu_display_cap_pct") {
		t.Fatalf("the closed card is skipped as %q and the skip names the key that closes it, got %+v", ReasonDisplay, s)
	}
}

func TestAllocatorDisplayUnknownStaysClosedUnderTheCap(t *testing.T) {
	for _, capPct := range []int{0, 60, 100} {
		in := baseInput()
		in.Cards = unknownDisplayCards()
		in.DisplayCapPct = capPct
		in.FootprintGiB = 2
		in.Min, in.Max = 1, 3
		_, err := Allocate(in)
		var none *NoCardsError
		if !errors.As(err, &none) {
			t.Fatalf("cap %d: a reading that cannot say where the monitor is allocates nothing, got %v", capPct, err)
		}
		if len(none.Skipped) != 3 {
			t.Fatalf("cap %d: skipped = %+v, want all three cards", capPct, none.Skipped)
		}
		for _, s := range none.Skipped {
			if s.Reason != ReasonDisplayUnknown {
				t.Errorf("cap %d: card %s skipped as %q, want %q", capPct, s.ID, s.Reason, ReasonDisplayUnknown)
			}
		}
		if len(none.Waitable) != 0 {
			t.Errorf("cap %d: waiting does not make the display known, but %v were offered to queue on", capPct, none.Waitable)
		}
	}
}

// The cap needs the card's total VRAM; a reading without it cannot be held to a share of it.
func TestAllocatorDisplayCardWithoutATotalStaysClosedUnderTheCap(t *testing.T) {
	in := capInput(1, 60)
	in.Cards[1].VRAMTotalGiB = 0
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("a share of an unknown total is unknown: the display card must not be taken, got %v", a.Devices)
	}
	if s := skipOf(a, "gpu-bbbb0000"); s.Reason != ReasonVRAM || !strings.Contains(s.Detail, "total") {
		t.Fatalf("the skip says the total VRAM is not known, got %+v", s)
	}
}

// The cap is the display card's alone: a pair card a job fills to the last GiB is allocatable, as it always was.
func TestTheCapTouchesOnlyTheDisplayCard(t *testing.T) {
	in := capInput(15.5, 60)
	in.Resident = nil
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] != "gpu-aaaa0000" {
		t.Fatalf("card 0 (15.5 free, 15.5 needed) must still be allocatable under a 60 percent cap that applies to the display card only: %v, skipped %+v", a.Devices, a.Skipped)
	}
}

// A declared display_floor_gib (the display layer's absolute floor) still holds when it is the stricter bound.
func TestADeclaredFloorHoldsAboveTheCapWhenItIsStricter(t *testing.T) {
	in := capInput(3, 60)
	in.DisplayFloorGiB = 12 // 13.3 free - 3 = 10.3 < 12, though 2.6 + 3 is far under the cap
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("a 12 GiB floor is stricter than the cap's 6.36 GiB: %v", a.Devices)
	}
	if s := skipOf(a, "gpu-bbbb0000"); s.Reason != ReasonVRAM || !strings.Contains(s.Detail, "12.0") {
		t.Fatalf("the skip names the floor that bound, got %+v", s)
	}
	in.DisplayFloorGiB = 4 // the layer's seeded 4 GiB is looser than the cap: the cap decides
	in.FootprintGiB = 7.5
	if a, _ = Allocate(in); a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("a 4 GiB floor must not loosen the cap: 7.5 GiB still passes it, got %v", a.Devices)
	}
}

// A request for several cards takes the display card only where it clears the cap, like any other pick.
func TestAMultiCardRequestTakesTheDisplayCardOnlyWhereItClearsTheCap(t *testing.T) {
	in := capInput(6, 60)
	in.Min, in.Max = 3, 3
	in.Resident = nil
	a, err := Allocate(in)
	if err != nil || len(a.Devices) != 3 {
		t.Fatalf("a 6 GiB job clears the cap on the display card, so three cards qualify: %v %v", a.Devices, err)
	}
	in = capInput(7.5, 60)
	in.Min, in.Max = 3, 3
	in.Resident = nil
	_, err = Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) || none.Have != 2 {
		t.Fatalf("7.5 GiB passes the cap on no more than two cards: want a NoCardsError with 2 allocatable, got %v", err)
	}
}

// A queued request may wait on a card a live lease holds only if the card would take the job once the lease
// ends. The display card that would not clear the cap is never worth waiting for.
func TestAHeldDisplayCardThatCouldNotClearTheCapIsNotQueuedOn(t *testing.T) {
	for _, tc := range []struct {
		footprint  float64
		wantQueued bool
	}{
		{6, true},
		{7.5, false},
	} {
		in := capInput(tc.footprint, 60)
		in.Min, in.Max = 3, 3
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

// The grant puts the rule again from fresh readings (Options.GrantCheck -> DesktopRefusals): the card chosen at
// the enqueue is given back when the VRAM in use has moved past the cap while the request waited, or when the
// operator closed the card (cap 0) in the meantime.
func TestDesktopRefusalsAppliesTheCapAtGrantTime(t *testing.T) {
	ids := []string{"gpu-bbbb0000"}
	in := capInput(6, 60)
	if got := DesktopRefusals(in, ids); len(got) != 0 {
		t.Fatalf("within the cap at the enqueue: nothing to refuse, got %+v", got)
	}
	in.Cards[1].VRAMFreeGiB = 9 // a game took the card to 6.9 GiB in use: 6.9 + 6 = 12.9 > 9.54
	got := DesktopRefusals(in, ids)
	if len(got) != 1 || got[0].Reason != ReasonVRAM || !strings.Contains(got[0].Detail, "cap") || !strings.Contains(got[0].Detail, "60 %") {
		t.Fatalf("a card past the cap at the grant is refused on VRAM naming the cap, got %+v", got)
	}
	in = capInput(6, 0)
	got = DesktopRefusals(in, ids)
	if len(got) != 1 || got[0].Reason != ReasonDisplay {
		t.Fatalf("a closed card (cap 0) is refused as %q at the grant, got %+v", ReasonDisplay, got)
	}
	in = capInput(6, 60)
	in.Cards = unknownDisplayCards()
	if got = DesktopRefusals(in, []string{"gpu-aaaa0000"}); len(got) != 1 || got[0].Reason != ReasonDisplayUnknown {
		t.Fatalf("an unknown display stays refused at the grant whatever the cap, got %+v", got)
	}
}

// A display card that was never part of the reading is not special: the cap is about the card the screen is on.
func TestACapWithNoDisplayCardInTheReadingChangesNothing(t *testing.T) {
	in := capInput(15, 10)
	in.Cards[1].Display = false
	in.Resident = nil
	a, err := Allocate(in)
	if err != nil || len(a.Devices) != 1 {
		t.Fatalf("with no display card the cap has nothing to hold: %v %v", a.Devices, err)
	}
}

// A job that lands exactly on the cap is admitted. The share of a total is not exact in floating point
// (10 x 40 / 100 is fine, but 6.1 - 2.1 is 3.9999999999999996, under 4), so the comparison holds a tolerance far
// below anything a card reports: 3.9 GiB in use of 10 plus a declared 2.1 is exactly 60 percent.
func TestAJobThatLandsExactlyOnTheCapIsAdmittedDespiteFloatError(t *testing.T) {
	in := capInput(2.1, 60)
	in.Cards[1].VRAMTotalGiB, in.Cards[1].VRAMFreeGiB = 10, 6.1
	a, err := Allocate(in)
	if err != nil || len(a.Devices) != 1 || a.Devices[0] != "gpu-bbbb0000" {
		t.Fatalf("3.9 in use + 2.1 declared = 6.0 of 10 is exactly the 60 percent cap: want the display card, got %v %v (skipped %+v)", a.Devices, err, a.Skipped)
	}
	in = capInput(2.2, 60) // a tenth of a GiB over
	in.Cards[1].VRAMTotalGiB, in.Cards[1].VRAMFreeGiB = 10, 6.1
	if a, err = Allocate(in); err != nil || a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("0.1 GiB over the cap is over: %v %v", a.Devices, err)
	}
}

// A call placed from a card table the caller could not refresh keeps the screen's card closed (the cap is judged
// on VRAM in use now, which a stale table misstates), and the skip says that: the cap is what the operator
// configured, and the card is closed by the table, not by a gpu_display_cap_pct of 0 nobody wrote.
func TestAStaleTableClosesTheDisplayCardAndTheSkipNamesTheTable(t *testing.T) {
	in := capInput(2, 60)
	in.DisplayStale = true
	a, err := Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] == "gpu-bbbb0000" {
		t.Fatalf("a stale table keeps the display card closed whatever the cap: %v", a.Devices)
	}
	s := skipOf(a, "gpu-bbbb0000")
	if s.Reason != ReasonDisplay || !s.Display {
		t.Fatalf("the closed card is skipped as %q and marked as the display card, got %+v", ReasonDisplay, s)
	}
	if !strings.Contains(s.Detail, "could not be re-read") || strings.Contains(s.Detail, "gpu_display_cap_pct is 0") {
		t.Errorf("the skip names the table, not a cap of 0: %q", s.Detail)
	}
	// The grant-time check puts the same rule again, in the same words.
	refused := DesktopRefusals(in, []string{"gpu-bbbb0000"})
	if len(refused) != 1 || refused[0].Reason != ReasonDisplay || !strings.Contains(refused[0].Detail, "could not be re-read") {
		t.Errorf("the grant-time check refuses the display card for the same reason: %+v", refused)
	}
	// The operator's switch outranks it: with the cap at 0 the skip names the key, whatever the table.
	in.DisplayCapPct = 0
	a, err = Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if s = skipOf(a, "gpu-bbbb0000"); !strings.Contains(s.Detail, "gpu_display_cap_pct is 0") {
		t.Errorf("a cap of 0 is named even when the table is stale too: %q", s.Detail)
	}
}

// A cap of 100 with no declared display_floor_gib is the whole card (the operator giving it): nothing is held
// back, so there is no share to hold a footprint to, and a job that declares none (every media call) is given
// the card as it would be given any other. This is the one place the footprint requirement stops, and it is
// deliberate (the key's doc comment, the --cards help, ADR 0039 and the CHANGELOG say so); the three neighbours
// below pin where it does not reach: one point under the whole card, a declared floor, and a footprint that
// does not fit the free VRAM.
func TestACapOf100WithNoDeclaredFloorTakesAJobThatDeclaresNoFootprint(t *testing.T) {
	taken := func(footprint float64, capPct int, floor float64) bool {
		in := capInput(footprint, capPct)
		in.DisplayFloorGiB = floor
		a, err := Allocate(in)
		if err != nil {
			t.Fatal(err)
		}
		return len(a.Devices) == 1 && a.Devices[0] == "gpu-bbbb0000"
	}
	if !taken(0, 100, 0) {
		t.Error("the whole card, no floor declared: a job that declares no footprint is given it (it sorts first, nothing resident)")
	}
	if taken(0, 99, 0) {
		t.Error("one point under the whole card holds a share back, and a job that declares no footprint cannot be held to it")
	}
	if taken(0, 100, 4) {
		t.Error("a declared display_floor_gib is held back even at 100, so an undeclared footprint cannot be shown to keep it")
	}
	if taken(14, 100, 0) {
		t.Error("the whole card is not more than the card has free: 14 GiB does not fit 13.3 free")
	}
}
