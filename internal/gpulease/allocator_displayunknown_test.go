package gpulease

// A card of a reading that could not say which card the monitor is on (gpuprobe.Card.DisplayUnknown)
// is not handed out: display_attached is the only signal marking the operator's screen while it
// sleeps, and "unknown" is never "not the monitor". It is not a card to wait on either (waiting
// for a lease to end does not tell us which card the monitor is on), so a caller polling for
// cards sees it clear on the next good reading.

import (
	"errors"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func unknownDisplayCards() []gpuprobe.Card {
	cards := allocCards()
	for i := range cards {
		cards[i].Display = false // the degraded reading flags nothing...
		cards[i].DisplayUnknown = true
	}
	return cards
}

func TestAllocatorDoesNotAutoAssignACardWhenTheDisplayIsUnknown(t *testing.T) {
	in := baseInput()
	in.Cards = unknownDisplayCards()
	in.Min, in.Max = 1, 3
	_, err := Allocate(in)
	var none *NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("a reading that cannot say where the monitor is must not allocate: err = %v", err)
	}
	if len(none.Skipped) != 3 {
		t.Fatalf("skipped = %+v, want all three cards", none.Skipped)
	}
	for _, s := range none.Skipped {
		if s.Reason != ReasonDisplayUnknown {
			t.Errorf("card %s skipped as %q, want %q", s.ID, s.Reason, ReasonDisplayUnknown)
		}
	}
	if len(none.Waitable) != 0 {
		t.Errorf("waiting for a lease does not make the display known, but %v were offered to queue on", none.Waitable)
	}
}

// The operator being away is the one override the display rule has, and it covers this case too.
func TestAllocatorAllowsAnUnknownDisplayCardWhenTheOperatorIsAway(t *testing.T) {
	in := baseInput()
	in.Cards = unknownDisplayCards()
	in.AllowDisplay = true
	got, err := Allocate(in)
	if err != nil || len(got.Devices) != 1 {
		t.Fatalf("with the operator away an unknown display is not a reason to refuse: %+v %v", got, err)
	}
}

// A good reading marks nothing, so the allocator is exactly what it was.
func TestAllocatorIgnoresTheMarkOnAKnownReading(t *testing.T) {
	in := baseInput()
	got, err := Allocate(in)
	if err != nil || len(got.Devices) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}
