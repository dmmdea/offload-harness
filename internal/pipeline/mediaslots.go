package pipeline

import (
	"sync"
	"time"
)

// slotSet is the IN-PROCESS half of GPU mutual exclusion, one slot per CARD (plan P13).
//
// The file claim serializes across processes. An INHERITED lease has no claim to contend on:
// every job under `gpu reserve -- <cmd>` already holds the same one, so without this,
// concurrency inside one process is unarbitrated. That is fine for the one-shot command the
// wrapper was written for and wrong for a long-running server: fleet-serve runs Pipeline.Run
// inline in a net/http handler goroutine, so two dispatches under one reservation would both
// proceed, spawn two ComfyUI instances on one card, and race the unload election so one render
// runs with models still resident, precisely the condition the lease exists to prevent.
//
// It used to be ONE slot (a channel of capacity one) for the whole process, which kept two
// media jobs on two different cards from ever running together. Now a job takes the slots of
// the cards it will use:
//
//   - a set of card ids is held by one job at a time, and two jobs on disjoint cards hold
//     theirs at once;
//   - the whole node (ids nil: a job that names no card, run-graph, a pooled route, a host
//     with card-scoped leases off) conflicts with every card, so it waits for all of them and
//     they wait for it, exactly as the single slot behaved;
//   - waiters are served in arrival order among those that conflict; a waiter whose cards are
//     not wanted by anyone ahead of it is not held up (disjoint backfill), and a waiter that
//     needs the whole node is a barrier for the ones behind it, so it cannot starve.
//
// A waiter BLOCKS instead of polling (no timers, no wakeups, no file reads: it is handed its
// slots the moment the holders release), and the wait is bounded, which a Mutex cannot be.
//
// LOCK ORDER IS ALWAYS slot -> file lease, never the reverse, so the two cannot deadlock.
type slotSet struct {
	mu    sync.Mutex
	whole bool
	cards map[string]bool
	q     []*slotWaiter
	// onTimeout is a test seam: it runs when a waiter's timer has fired, before the waiter takes
	// the lock to withdraw. A release in that window is the race the grant check below covers.
	onTimeout func()
}

type slotWaiter struct {
	ids     []string // nil = the whole node; for an any-waiter, the candidates
	any     bool     // wants ONE of ids, not all of them
	picked  string   // the card an any-waiter was given
	ready   chan struct{}
	granted bool
}

// mediaSlots is the process-wide set every media route takes its slots from.
var mediaSlots = &slotSet{}

// conflict reports whether two requests contend: the whole node (nil) contends with everything.
func conflict(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// freeLocked reports whether ids conflict with nothing held right now.
func (s *slotSet) freeLocked(ids []string) bool {
	if s.whole {
		return false
	}
	if len(ids) == 0 {
		return len(s.cards) == 0
	}
	for _, id := range ids {
		if s.cards[id] {
			return false
		}
	}
	return true
}

// blockedLocked reports whether a waiter ahead of position `upto` contends with ids.
func (s *slotSet) blockedLocked(ids []string, upto int) bool {
	for i := 0; i < upto && i < len(s.q); i++ {
		if conflict(s.q[i].ids, ids) {
			return true
		}
	}
	return false
}

func (s *slotSet) grabLocked(ids []string) {
	if len(ids) == 0 {
		s.whole = true
		return
	}
	if s.cards == nil {
		s.cards = map[string]bool{}
	}
	for _, id := range ids {
		s.cards[id] = true
	}
}

// tryTake takes ids (nil = the whole node) if they are free and nobody ahead is waiting for
// them. It never waits.
func (s *slotSet) tryTake(ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, ok := s.availableLocked(&slotWaiter{ids: ids}, len(s.q)); ok {
		s.grabLocked(got)
		return true
	}
	return false
}

// availableLocked says which slots w could take right now, given the holders and the waiters
// ahead of position upto. A waiter for a set takes exactly that set; an any-waiter takes the
// first candidate that is free and wanted by nobody ahead.
func (s *slotSet) availableLocked(w *slotWaiter, upto int) ([]string, bool) {
	if !w.any {
		if s.freeLocked(w.ids) && !s.blockedLocked(w.ids, upto) {
			return w.ids, true
		}
		return nil, false
	}
	for _, c := range w.ids {
		one := []string{c}
		if s.freeLocked(one) && !s.blockedLocked(one, upto) {
			return one, true
		}
	}
	return nil, false
}

// take is tryTake that waits at most `wait` for the slots, in arrival order. Reports false on
// timeout, which the caller turns into the same clean defer a busy card produces.
func (s *slotSet) take(ids []string, wait time.Duration) bool {
	return s.waitFor(&slotWaiter{ids: append([]string(nil), ids...)}, wait)
}

// takeAny takes ONE of the candidate cards, whichever is free first, waiting at most `wait`. It
// serves a job that runs under a lease its parent holds on several cards: any of them will do.
func (s *slotSet) takeAny(candidates []string, wait time.Duration) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	w := &slotWaiter{ids: append([]string(nil), candidates...), any: true}
	if !s.waitFor(w, wait) {
		return "", false
	}
	return w.picked, true
}

func (s *slotSet) waitFor(w *slotWaiter, wait time.Duration) bool {
	s.mu.Lock()
	if got, ok := s.availableLocked(w, len(s.q)); ok {
		s.grabLocked(got) // free: no timer allocated at all in the common case
		w.granted = true
		if w.any {
			w.picked = got[0]
		}
		s.mu.Unlock()
		return true
	}
	if wait <= 0 {
		s.mu.Unlock()
		return false
	}
	w.ready = make(chan struct{})
	s.q = append(s.q, w)
	s.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ready:
		return true
	case <-timer.C:
		if s.onTimeout != nil {
			s.onTimeout()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if w.granted { // granted in the instant the timer fired: it is ours
			return true
		}
		for i, x := range s.q {
			if x == w {
				s.q = append(s.q[:i], s.q[i+1:]...)
				break
			}
		}
		// Waiters behind this one may have been blocked only by it.
		s.dispatchLocked()
		return false
	}
}

// release gives the slots back (nil = the whole node) and hands them to the waiters that can
// now run. Releasing what nobody holds is harmless.
func (s *slotSet) release(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		s.whole = false
	} else {
		for _, id := range ids {
			delete(s.cards, id)
		}
	}
	s.dispatchLocked()
}

// dispatchLocked grants, in arrival order, every waiter whose slots are free and not wanted by
// a waiter ahead of it. Granting only ever makes later waiters MORE blocked, so one pass is
// enough.
func (s *slotSet) dispatchLocked() {
	for i := 0; i < len(s.q); {
		w := s.q[i]
		if got, ok := s.availableLocked(w, i); ok {
			s.grabLocked(got)
			w.granted = true
			if w.any {
				w.picked = got[0]
			}
			close(w.ready)
			s.q = append(s.q[:i], s.q[i+1:]...)
			continue
		}
		i++
	}
}

// held is the cards in use right now. The admission path treats them as claimed even when no
// lease record shows them (a job inheriting its parent's lease has none of its own).
func (s *slotSet) held() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.cards))
	for id := range s.cards {
		out[id] = true
	}
	return out
}

// wholeHeld reports whether a whole-node holder is in.
func (s *slotSet) wholeHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.whole
}

// queued is how many waiters are parked.
func (s *slotSet) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.q)
}

// takeMediaSlot claims the whole node in-process, waiting at most wait. Reports false on
// timeout, which the caller turns into the same clean defer a busy card produces. It is the
// form of every caller that never named a card (the pipeline-job route, the batch route, a
// whole-node lease).
func takeMediaSlot(wait time.Duration) bool { return mediaSlots.take(nil, wait) }

func releaseMediaSlot() { mediaSlots.release(nil) }
