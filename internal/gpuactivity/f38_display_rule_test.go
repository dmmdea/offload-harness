package gpuactivity

// F38 follow-up (2026-10-10): the note names a card "the display card" by display_active alone (a card
// driving a screen right now). The allocator's placement rule, which `gpu cards` prints, also counts a card
// whose monitor is attached with the screen asleep (display_attached). The two differ on exactly that card,
// and this pins what the status note says about it so that the comment, the changelog and the docs, which
// state the difference, stay true: it is "outside this lease's cards", not the display card, and
// screenCards (the placement rule's reading of the same View) does mark it.

import (
	"strings"
	"testing"
)

func TestAMonitorsCardWithItsScreenAsleepIsOutsideTheLeaseNotTheDisplayCardHere(t *testing.T) {
	cards := f38Cards(60, 0, 0, "") // no card is driving a screen right now
	cards[0].DisplayAttached = true // card 0 has the monitor, asleep
	h := f38Holder(strings.ToLower(f38CardC))
	h.Progress = &ProgressState{State: "unknown", Problem: "does not exist"}
	v := View{Held: true, Holder: h, Seat: SeatState{Name: "agent-pool"}, GPUs: cards}
	if !screenCards(v)[strings.ToLower(f38CardA)] {
		t.Fatalf("the placement rule must mark the monitor's card: %v", screenCards(v))
	}
	if displayCards(v)[f38CardA] {
		t.Fatalf("display_active alone must not mark a card whose screen is asleep: %v", displayCards(v))
	}
	_, note := f38Assess(h, cards)
	if strings.Contains(note, "display card") {
		t.Errorf("display_active is off everywhere, so no card is named the display card:\n%s", note)
	}
	if !strings.Contains(note, "60% on card 0") || !strings.Contains(note, "outside this lease's cards") {
		t.Errorf("the busy card the lease does not hold is named for what the status can say of it:\n%s", note)
	}
}
