package gpuactivity

// F34 (2026-10-10): `gpu status` flagged "possible device-trespass" on the workstation's display card for
// ordinary desktop load. trespassOf exempted the display card through displayCards, which reads display_active
// alone; on the reference box display_active reads Disabled on every card whenever no lit screen or game is
// initialised, while display_attached stays Yes on the monitor's card (gpuprobe/display.go, measured 2026-10-03).
// So the card the card table marks "display" was never exempt, and 23% of desktop load on it, with a lease
// holding another card, raised the flag. The exemption now also reads the placement rule (screenCards), the
// rule the F18 desktop-process fold already uses. These tests keep the display_attached-only reading that the
// older fixture (trio, which sets display_active on the display card) never exercised.

import (
	"strings"
	"testing"
	"time"
)

// attachedTrio is trio with the monitor on card 1 and nothing driving a screen right now: display_active reads
// Disabled everywhere, display_attached is Yes on card 1 only.
func attachedTrio(u0, u1, u2 int) []GPU {
	g := trio(u0, u1, u2)
	g[1].DisplayActive = false
	g[1].DisplayAttached = true
	return g
}

func TestTrespassExemptsTheMonitorsCardWhoseDisplayActiveReadsDisabled(t *testing.T) {
	now := time.Now()
	idle := SeatState{Name: "agent-pool"}
	for _, c := range []struct {
		name string
		gpus []GPU
	}{
		{"23% of desktop load on the monitor's card, the lease's card busy", attachedTrio(0, 23, 90)},
		{"23% of desktop load on the monitor's card, the lease's card quiet", attachedTrio(0, 23, 0)},
		// The named limit: the exemption is the card table's display marking, so an UNLEASED heavy job on the
		// display card is not flagged either (the desktop's use and a stray job look the same from here).
		{"an unleased heavy job on the monitor's card (the named limit)", attachedTrio(0, 95, 90)},
	} {
		v := View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: c.gpus}
		if !screenCards(v)[strings.ToLower(tCard1)] || displayCards(v)[tCard1] {
			t.Fatalf("%s: the fixture must be display_attached only (screenCards marks card 1, displayCards does not): %v / %v", c.name, screenCards(v), displayCards(v))
		}
		if got := trespassOf(v); len(got) != 0 {
			t.Errorf("%s: the display card must not be flagged, got %+v", c.name, got)
		}
		if _, note := Assess(v); strings.Contains(note, "device-trespass") {
			t.Errorf("%s: the note must not say device-trespass: %s", c.name, note)
		}
		if m := v.Map(); m["device_trespass"] != nil {
			t.Errorf("%s: no trespass, no key: %v", c.name, m)
		}
	}
}

// The exemption is the display card's alone: a card that is not marked display, busy and unleased, is still
// flagged, with the monitor's card busy beside it (and not named).
func TestTrespassStillFlagsAnUnleasedCardThatIsNotTheMonitors(t *testing.T) {
	now := time.Now()
	v := View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: SeatState{Name: "agent-pool"}, GPUs: attachedTrio(23, 23, 90)}
	got := trespassOf(v)
	if len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("trespass = %+v, want card 0 only (card 1 is the monitor's, card 2 is the lease's)", got)
	}
	verdict, note := Assess(v)
	if verdict != VerdictHeldWorking {
		t.Errorf("the lease's own card is busy: held-working, got %q (%s)", verdict, note)
	}
	if !strings.Contains(note, "possible device-trespass: 23% on card 0 (") {
		t.Errorf("card 0 must be flagged:\n%s", note)
	}
	if strings.Contains(note, "on card 1") {
		t.Errorf("the monitor's card must not be named as a trespass:\n%s", note)
	}
	// The same card with nothing marking any display is flagged just as before.
	plain := View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: SeatState{Name: "agent-pool"}, GPUs: trio(23, 0, 90)}
	plain.GPUs[1].DisplayActive = false
	if got := trespassOf(plain); len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("trespass = %+v, want card 0 only", got)
	}
}

// The two readings are a union, not a swap: on a box whose every non-leased card is display-ish (one lit, one only
// attached) the placement rule has no non-display card left to keep a work card (its single-card guard marks
// nothing), while display_active alone still marks the lit one. The lit card stays exempt, as it was before F34.
func TestTrespassKeepsTheLitDisplayCardExemptWhereThePlacementRuleMarksNothing(t *testing.T) {
	gpus := []GPU{
		{Index: 0, UUID: tCard0, Name: "Card A", UtilPct: 40, UtilKnown: true, MemTotalMiB: 16000, DisplayActive: true},
		{Index: 1, UUID: tCard1, Name: "Card B", UtilPct: 0, UtilKnown: true, MemTotalMiB: 16000, DisplayAttached: true},
	}
	v := View{At: time.Now(), Held: true, Holder: scopedHolder(strings.ToLower(tCard1)), Seat: SeatState{Name: "agent-pool"}, GPUs: gpus}
	if len(screenCards(v)) != 0 || !displayCards(v)[tCard0] {
		t.Fatalf("the fixture must have the placement rule mark nothing while display_active marks card 0: %v / %v", screenCards(v), displayCards(v))
	}
	if got := trespassOf(v); len(got) != 0 {
		t.Fatalf("the lit display card stays exempt, got %+v", got)
	}
}
