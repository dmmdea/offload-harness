package gpulease

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A queued request was decided against the operator's presence and the desktop floor when it joined
// the line. The line can be hours long, so the decision is put again at the grant: DesktopRefusals is
// the rule, Options.GrantCheck is where it is asked, and a refusal is ErrGrantRefused, never a lease.

func TestDesktopRefusalsNamesTheDisplayCardOnceTheOperatorIsBack(t *testing.T) {
	in := openedDisplayInput(2, 4) // card 1 is the display card, opened while the operator was away
	ids := []string{"gpu-bbbb0000"}
	if got := DesktopRefusals(in, ids); len(got) != 0 {
		t.Fatalf("operator away and the floor kept: nothing to refuse, got %+v", got)
	}
	in.AllowDisplay = false // the operator is back at the desk
	got := DesktopRefusals(in, ids)
	if len(got) != 1 || got[0].ID != "gpu-bbbb0000" || got[0].Reason != ReasonDisplay {
		t.Fatalf("at the desk the display card is refused as %q, got %+v", ReasonDisplay, got)
	}
}

func TestDesktopRefusalsHoldsTheFloorAtTheGrantAsAtTheEnqueue(t *testing.T) {
	in := openedDisplayInput(6, 4) // 12 free - 6 = 6 left: fine at the enqueue
	ids := []string{"gpu-bbbb0000"}
	if got := DesktopRefusals(in, ids); len(got) != 0 {
		t.Fatalf("floor kept, got %+v", got)
	}
	in.Cards[1].VRAMFreeGiB = 9 // a game took 3 GiB while the request waited: 9 - 6 = 3 < 4
	got := DesktopRefusals(in, ids)
	if len(got) != 1 || got[0].Reason != ReasonVRAM || !strings.Contains(got[0].Detail, "desktop floor") {
		t.Fatalf("a card under its floor at the grant is refused on VRAM naming the floor, got %+v", got)
	}
}

func TestDesktopRefusalsRefusesAnUnknownDisplayCardAtTheDeskAndLeavesOtherCardsAlone(t *testing.T) {
	in := baseInput()
	in.Cards = unknownDisplayCards()
	got := DesktopRefusals(in, []string{"gpu-aaaa0000", "gpu-cccc0000"})
	if len(got) != 2 || got[0].Reason != ReasonDisplayUnknown {
		t.Fatalf("with the display unknown and the operator not known away, every chosen card is refused: %+v", got)
	}
	// A card that is not the display card is judged by the allocator at pick time, never here.
	in2 := baseInput() // card 1 is the display card; the request chose card 0
	in2.FootprintGiB = 15
	in2.DisplayFloorGiB = 4
	if got := DesktopRefusals(in2, []string{"gpu-aaaa0000"}); len(got) != 0 {
		t.Fatalf("the floor is the display card's alone, got %+v", got)
	}
}

// A card a live lease holds is that lease's to refuse (ErrHeld). It is not read as a desktop refusal:
// the holder's footprint is on it.
func TestDesktopRefusalsLeavesAHeldCardToTheClaim(t *testing.T) {
	in := openedDisplayInput(9, 4) // would be under the floor
	in.Claimed = map[string]bool{"gpu-bbbb0000": true}
	if got := DesktopRefusals(in, []string{"gpu-bbbb0000"}); len(got) != 0 {
		t.Fatalf("a claimed card is the claim's to refuse, got %+v", got)
	}
}

// Acquire asks GrantCheck once the card is free and the waiter is at the front, and a refusal ends the
// wait with ErrGrantRefused without taking the card.
func TestAcquireRefusesTheGrantWhenTheCheckSaysTheCardNoLongerQualifies(t *testing.T) {
	m := scopedRealClock(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "holder", Devices: []string{card1}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	var heldByOtherWhenAsked atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, aerr := m.Acquire(ClassMedia, Options{
			Reason: "queued", Devices: []string{card1}, Wait: 10 * time.Second, WaitOut: true,
			GrantCheck: func() error {
				asked.Add(1)
				// The check runs with the cards granted to this request, so the claim it sees is its own.
				if info := m.InspectFor([]string{card1}); !info.Held || info.Reason != "queued" {
					heldByOtherWhenAsked.Store(true)
				}
				return errors.New("operator at the desk")
			},
		})
		done <- aerr
	}()
	waitAllRegistered(t, m, 1, 3*time.Second)
	time.Sleep(150 * time.Millisecond) // several poll ticks while the holder is still on the card
	if n := asked.Load(); n != 0 {
		t.Fatalf("the check is made at the grant, never while the holder still has the card: asked %d times", n)
	}
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case aerr := <-done:
		var refused *ErrGrantRefused
		if !errors.As(aerr, &refused) || !strings.Contains(refused.Reason, "operator at the desk") {
			t.Fatalf("want ErrGrantRefused carrying the check's reason, got %v", aerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refused grant must end the wait")
	}
	if heldByOtherWhenAsked.Load() {
		t.Error("the check must run with the cards granted to this request, not before the claim and not while another lease held them")
	}
	if info := m.InspectFor([]string{card1}); info.Held {
		t.Fatalf("a refused grant must not leave the card claimed: %+v", info)
	}
}

// A check that passes grants as before, and no check is the behaviour before the option existed.
func TestAcquireGrantsWhenTheCheckPasses(t *testing.T) {
	m := scopedRealClock(t)
	var asked atomic.Int32
	l, err := m.Acquire(ClassMedia, Options{
		Reason: "queued", Devices: []string{card1}, Wait: 5 * time.Second, WaitOut: true,
		GrantCheck: func() error { asked.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if asked.Load() == 0 {
		t.Error("the check must be asked before the grant")
	}
	l2, err := m.Acquire(ClassMedia, Options{Reason: "no check", Devices: []string{card2}, Wait: 5 * time.Second})
	if err != nil {
		t.Fatalf("no GrantCheck is the old behaviour: %v", err)
	}
	_ = l2.Release()
}

func TestVetGrantIsNilWithoutACheckAndKeepsAGrantTheCheckPasses(t *testing.T) {
	m := scopedRealClock(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "r", Devices: []string{card1}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if err := m.VetGrant(l, Options{}); err != nil {
		t.Fatalf("no check is no refusal: %v", err)
	}
	if err := m.VetGrant(l, Options{GrantCheck: func() error { return nil }}); err != nil {
		t.Fatalf("a passing check keeps the grant: %v", err)
	}
	if !m.InspectFor([]string{card1}).Held {
		t.Fatal("a grant the check passed stays held")
	}
}

// A refused grant is given back. If giving it back fails the cards are still held, which is not the
// same answer as "refused": the caller must hear that, not go on as if the cards were free.
func TestVetGrantThatCannotReleaseSaysSoAndIsNotARefusal(t *testing.T) {
	check := func() error { return errors.New("operator at the desk") }
	released := 0
	verr := vetGrant(check, func() error { released++; return nil })
	var refused *ErrGrantRefused
	if !errors.As(verr, &refused) || released != 1 {
		t.Fatalf("a refused grant is released once and reported as ErrGrantRefused: %v (released %d)", verr, released)
	}
	verr = vetGrant(check, func() error { return errors.New("sharing violation") })
	if verr == nil || errors.As(verr, &refused) {
		t.Fatalf("a grant that could not be given back is not a clean refusal: %v", verr)
	}
	if !strings.Contains(verr.Error(), "operator at the desk") || !strings.Contains(verr.Error(), "still held") || !strings.Contains(verr.Error(), "sharing violation") {
		t.Fatalf("say why it was refused, that the cards are still held, and why: %v", verr)
	}
	if err := vetGrant(func() error { return nil }, func() error { t.Error("a passing check releases nothing"); return nil }); err != nil {
		t.Fatal(err)
	}
}
