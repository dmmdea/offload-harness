package modelaffinity

// A blocked text-load admission takes a place in the lease queue (register D-1xx-2) so a
// chained media claim cannot win the gap before its next poll. That place is on the cards the
// admission is waiting for: the seat's own. Registered as the whole node it held back every
// fresh claim on every card, including a free card the seat has nothing to do with, which is
// the disjoint backfill the queue promises (pre-ship review of D-1xx-3, 2026-10-09).

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// seatWaiterOf blocks an admission of model behind a media lease on card 0, waits for the
// ClassSeat entry it registers, and returns that entry. The admission is released by the
// caller dropping the lease (the test's cleanup does it).
func seatWaiterOf(t *testing.T, m *gpulease.Manager, model string) gpulease.Waiter {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Admit(ctx, "http://seat-reg-"+t.Name(), model, 5*time.Second)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, w := range m.Waiters() {
			if w.Class == gpulease.ClassSeat {
				return w
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("a blocked admission of %q never registered a ClassSeat waiter", model)
	return gpulease.Waiter{}
}

func TestABlockedAdmissionQueuesOnItsSeatsCardsNotTheWholeNode(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard0) // seat-card0 and seat-pair sit on card 0; seat-card2 does not

	cases := []struct {
		model string
		want  []string
	}{
		{"seat-card0", []string{idCard0}},
		{"seat-pair", []string{idCard0, idCard2}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			w := seatWaiterOf(t, m, tc.model)
			if !reflect.DeepEqual(w.Devices, tc.want) {
				t.Fatalf("the admission waits for %v, so it must queue on %v, not %v", tc.model, tc.want, w.Devices)
			}
		})
	}
}

// The point of naming the cards: a fresh claim on a free card the seat is not on goes through
// while the admission waits, and a claim on the seat's own card queues behind it.
func TestASeatsWaiterDoesNotHoldBackAClaimOnAnotherCard(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard0)
	w := seatWaiterOf(t, m, "seat-card0")
	if !reflect.DeepEqual(w.Devices, []string{idCard0}) {
		t.Fatalf("setup: the seat waiter is on %v, want card 0", w.Devices)
	}
	time.Sleep(5 * time.Millisecond) // the claim below arrives strictly after the waiter

	lease, err := m.Acquire(gpulease.ClassMedia, gpulease.Options{Reason: "render on the free card", Devices: []string{idCard2}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("a no-wait claim on card 2 must go through while a seat on card 0 waits: %v", err)
	}
	_ = lease.Release()
}

// A seat that cannot be placed (no pin declared for it) queues as the whole node: unknown is
// every card, the direction of every doubt in this gate.
func TestAnUnplaceableSeatQueuesAsTheWholeNode(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard0)
	w := seatWaiterOf(t, m, "mystery-seat")
	if len(w.Devices) != 0 {
		t.Fatalf("a seat with no declared pin must queue on the whole node, got %v", w.Devices)
	}
}

// SeatCards reads the pins and the card table the gate already uses, and says nothing it
// cannot resolve.
func TestSeatCardsResolvesPinsAndFailsClosed(t *testing.T) {
	armSeatScope(t, tripleBoxPins)
	if got := SeatCards("seat-card2"); !reflect.DeepEqual(got, []string{idCard2}) {
		t.Fatalf("SeatCards(seat-card2) = %v, want card 2", got)
	}
	if got := SeatCards("seat-triple"); len(got) != 3 {
		t.Fatalf("SeatCards(seat-triple) = %v, want all three cards", got)
	}
	if got := SeatCards("mystery-seat"); got != nil {
		t.Fatalf("an undeclared model has no cards to name, got %v", got)
	}
	armSeatScope(t, map[string][]string{"seat-bad-pin": {"7"}})
	if got := SeatCards("seat-bad-pin"); got != nil {
		t.Fatalf("a pin the card table cannot place has no cards to name, got %v", got)
	}
}
