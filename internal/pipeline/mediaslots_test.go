package pipeline

import (
	"sync"
	"testing"
	"time"
)

// The in-process half of media mutual exclusion is a SET of per-card slots, not one slot: two
// media jobs on two different cards run at once, one job that needs the whole node waits for
// every card, and waiters are served in arrival order. takeMediaSlot/releaseMediaSlot keep
// their meaning (the whole-node form) for the callers that never named a card.

func TestAWholeNodeSlotExcludesEveryCard(t *testing.T) {
	s := &slotSet{}
	if !s.tryTake(nil) {
		t.Fatal("a free set must give the whole node")
	}
	if s.tryTake([]string{"a"}) {
		t.Fatal("a card must not be given while the whole node is held")
	}
	if s.tryTake(nil) {
		t.Fatal("the whole node is held once")
	}
	s.release(nil)
	if !s.tryTake([]string{"a"}) {
		t.Fatal("a card must be free once the whole node is released")
	}
}

func TestTwoDifferentCardsAreHeldAtOnce(t *testing.T) {
	s := &slotSet{}
	if !s.tryTake([]string{"a"}) || !s.tryTake([]string{"b"}) {
		t.Fatal("two disjoint cards must be held together")
	}
	if s.tryTake([]string{"a"}) {
		t.Fatal("one card has one holder")
	}
	if s.tryTake([]string{"b", "c"}) {
		t.Fatal("a set that overlaps a held card must wait")
	}
	s.release([]string{"a"})
	if !s.tryTake([]string{"a", "c"}) {
		t.Fatal("a released card is free again, with its set-mates")
	}
}

func TestTheWholeNodeWaitsForEveryCardToDrain(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	s.tryTake([]string{"b"})
	got := make(chan bool, 1)
	go func() { got <- s.take(nil, 5*time.Second) }()
	waitQueued(t, s, 1)
	s.release([]string{"a"})
	select {
	case <-got:
		t.Fatal("the whole node was granted while card b is still held")
	case <-time.After(50 * time.Millisecond):
	}
	s.release([]string{"b"})
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the whole node must be granted once every card is free")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the whole-node waiter was never woken")
	}
}

// A waiter that needs the whole node is a barrier: a later job asking for a free card queues
// behind it instead of starving it forever.
func TestAWholeNodeWaiterIsABarrierForLaterWaiters(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	order := make(chan string, 2)
	go func() {
		if s.take(nil, 5*time.Second) {
			order <- "whole"
			time.Sleep(30 * time.Millisecond)
			s.release(nil)
		}
	}()
	waitQueued(t, s, 1)
	go func() {
		if s.take([]string{"b"}, 5*time.Second) {
			order <- "card-b"
			s.release([]string{"b"})
		}
	}()
	waitQueued(t, s, 2)
	select {
	case got := <-order:
		t.Fatalf("%s was granted while card a is still held", got)
	case <-time.After(50 * time.Millisecond):
	}
	s.release([]string{"a"})
	first, second := <-order, <-order
	if first != "whole" || second != "card-b" {
		t.Fatalf("served %s then %s, want the earlier waiter (the whole node) first", first, second)
	}
}

// Disjoint backfill: a waiter that wants cards nobody holds is not held up by a waiter ahead of
// it that wants a held card.
func TestADisjointWaiterPassesAWaitingOverlap(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	done := make(chan bool, 1)
	go func() { done <- s.take([]string{"a", "b"}, 5*time.Second) }()
	waitQueued(t, s, 1)
	if !s.take([]string{"c"}, time.Second) {
		t.Fatal("card c is free and nobody ahead of this waiter wants it")
	}
	s.release([]string{"c"})
	s.release([]string{"a"})
	if ok := <-done; !ok {
		t.Fatal("the overlapping waiter must be served once card a is released")
	}
}

func TestATimedOutWaiterLeavesNothingBehind(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	start := time.Now()
	if s.take([]string{"a"}, 40*time.Millisecond) {
		t.Fatal("card a is held: the wait must time out")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("returned before the bounded wait")
	}
	if s.queued() != 0 {
		t.Fatalf("%d waiter(s) linger after a timeout", s.queued())
	}
	s.release([]string{"a"})
	if !s.tryTake([]string{"a"}) {
		t.Fatal("the card must be free: the timed-out waiter must not have been granted it")
	}
	// A wait of zero is one try, and does not jump a queue.
	if s.take([]string{"a"}, 0) {
		t.Fatal("a zero wait on a held card is a refusal")
	}
}

func TestHeldReportsTheCardsInUse(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"b"})
	s.tryTake([]string{"a"})
	h := s.held()
	if len(h) != 2 || !h["a"] || !h["b"] {
		t.Fatalf("held = %v", h)
	}
	if s.wholeHeld() {
		t.Fatal("no whole-node holder")
	}
	s.release([]string{"a"})
	s.release([]string{"b"})
	s.tryTake(nil)
	if !s.wholeHeld() {
		t.Fatal("the whole node is held")
	}
}

func TestReleasingASlotNobodyHoldsIsHarmless(t *testing.T) {
	s := &slotSet{}
	s.release([]string{"a"})
	s.release(nil)
	if !s.tryTake([]string{"a"}) {
		t.Fatal("a stray release must not corrupt the set")
	}
}

// The legacy pair keeps its meaning: whole-node, the form the pipeline-job route and the
// composition tests use.
func TestTheLegacySlotFunctionsAreTheWholeNodeForm(t *testing.T) {
	if !takeMediaSlot(0) {
		t.Fatal("the process-wide set must start free")
	}
	if mediaSlots.tryTake([]string{"a"}) {
		t.Fatal("a card must not be given while takeMediaSlot holds the whole node")
	}
	releaseMediaSlot()
	if !mediaSlots.tryTake([]string{"a"}) {
		t.Fatal("releaseMediaSlot must release the whole node")
	}
	mediaSlots.release([]string{"a"})
}

// No two holders ever share a card, whatever the interleaving.
func TestNoCardEverHasTwoSlotHolders(t *testing.T) {
	s := &slotSet{}
	cards := []string{"a", "b", "c"}
	var mu sync.Mutex
	inUse := map[string]int{}
	whole := 0
	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				var ids []string
				switch (g + i) % 4 {
				case 0:
					ids = nil
				case 1:
					ids = []string{cards[(g+i)%3]}
				case 2:
					ids = []string{cards[(g+i)%3], cards[(g+i+1)%3]}
				default:
					ids = []string{cards[g%3]}
				}
				if !s.take(ids, 5*time.Second) {
					t.Errorf("a bounded wait of 5 s timed out: a slot was never released")
					return
				}
				mu.Lock()
				if ids == nil {
					whole++
					if whole > 1 || len(inUse) > 0 {
						t.Errorf("the whole node granted beside others: whole=%d cards=%v", whole, inUse)
					}
				} else {
					if whole > 0 {
						t.Errorf("a card granted beside the whole node")
					}
					for _, id := range ids {
						inUse[id]++
						if inUse[id] > 1 {
							t.Errorf("card %s has %d holders", id, inUse[id])
						}
					}
				}
				mu.Unlock()
				time.Sleep(time.Duration((g+i)%3) * time.Millisecond)
				mu.Lock()
				if ids == nil {
					whole--
				} else {
					for _, id := range ids {
						inUse[id]--
						if inUse[id] == 0 {
							delete(inUse, id)
						}
					}
				}
				mu.Unlock()
				s.release(ids)
			}
		}(g)
	}
	wg.Wait()
}

func waitQueued(t *testing.T, s *slotSet, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.queued() < n {
		if time.Now().After(deadline) {
			t.Fatalf("waiters never queued: have %d, want %d", s.queued(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A waiter that gives up must not leave the waiters behind it parked: the ones it alone was
// blocking run at once.
func TestAGivenUpWaiterFreesTheOnesItWasBlocking(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	first := make(chan bool, 1)
	go func() { first <- s.take([]string{"a", "b"}, 150*time.Millisecond) }()
	waitQueued(t, s, 1)
	second := make(chan bool, 1)
	go func() { second <- s.take([]string{"b"}, 10*time.Second) }()
	waitQueued(t, s, 2)
	if ok := <-first; ok {
		t.Fatal("card a is held: the first waiter must time out")
	}
	select {
	case ok := <-second:
		if !ok {
			t.Fatal("the second waiter must be granted card b")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("card b is free and the waiter that blocked it is gone, yet the second waiter is still parked")
	}
}

// The instant the timer fires a release can grant the slots to the waiter. They are then its
// own: reporting a timeout would leak them for good.
func TestAGrantInTheInstantOfTheTimeoutIsKept(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	s.onTimeout = func() { s.release([]string{"a"}) } // the holder lets go just as the timer fires
	if !s.take([]string{"a"}, 20*time.Millisecond) {
		t.Fatal("the slot was granted as the timer fired: the call must report that it holds it")
	}
	if s.tryTake([]string{"a"}) {
		t.Fatal("the caller holds card a: nobody else may take it")
	}
}

// takeAny serves a job running under a lease its parent holds on SEVERAL cards: it runs on
// whichever of them is free, and waits for the first to free when none is.

func TestTakeAnyPicksTheFirstFreeCard(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	got, ok := s.takeAny([]string{"a", "b", "c"}, 0)
	if !ok || got != "b" {
		t.Fatalf("picked %q ok=%v, want b: the first candidate that is free", got, ok)
	}
	if s.tryTake([]string{"b"}) {
		t.Fatal("the picked card is held")
	}
}

func TestTakeAnyWaitsForTheFirstCardToFree(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	s.tryTake([]string{"b"})
	type res struct {
		id string
		ok bool
	}
	got := make(chan res, 1)
	go func() { id, ok := s.takeAny([]string{"a", "b"}, 5*time.Second); got <- res{id, ok} }()
	waitQueued(t, s, 1)
	s.release([]string{"b"})
	select {
	case r := <-got:
		if !r.ok || r.id != "b" {
			t.Fatalf("got %+v, want card b, the one that freed", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter was never woken")
	}
	if s.tryTake([]string{"b"}) {
		t.Fatal("card b is held by the waiter")
	}
	if !s.tryTake([]string{"c"}) {
		t.Fatal("a card outside the candidates is untouched")
	}
}

func TestTakeAnyTimesOutWithNothingHeld(t *testing.T) {
	s := &slotSet{}
	s.tryTake([]string{"a"})
	if id, ok := s.takeAny([]string{"a"}, 30*time.Millisecond); ok {
		t.Fatalf("took %q although the only candidate is held", id)
	}
	if s.queued() != 0 {
		t.Fatal("a timed-out waiter lingers")
	}
	s.release([]string{"a"})
	if !s.tryTake([]string{"a"}) {
		t.Fatal("the card must be free: the timed-out waiter must not have been granted it")
	}
	if id, ok := (&slotSet{}).takeAny(nil, time.Second); ok || id != "" {
		t.Fatal("no candidates, no card")
	}
}

// The whole-node holder excludes it, and an earlier waiter for a card keeps its place.
func TestTakeAnyRespectsTheWholeNodeAndTheQueue(t *testing.T) {
	s := &slotSet{}
	s.tryTake(nil)
	if _, ok := s.takeAny([]string{"a", "b"}, 0); ok {
		t.Fatal("no card while the whole node is held")
	}
	s.release(nil)
	s.tryTake([]string{"a"})
	first := make(chan bool, 1)
	go func() { first <- s.take([]string{"b"}, 5*time.Second) }() // not free? it is: b is free, so this returns at once
	if !<-first {
		t.Fatal("setup: b is free")
	}
	// b is now held, a is held: a queued waiter for a, then takeAny over a and c: c is free and
	// nobody ahead wants it.
	queued := make(chan bool, 1)
	go func() { queued <- s.take([]string{"a"}, 5*time.Second) }()
	waitQueued(t, s, 1)
	id, ok := s.takeAny([]string{"a", "c"}, time.Second)
	if !ok || id != "c" {
		t.Fatalf("took %q ok=%v, want c: a is held and wanted by the waiter ahead", id, ok)
	}
	s.release([]string{"a"})
	if !<-queued {
		t.Fatal("the earlier waiter for a must be served when a frees")
	}
}
