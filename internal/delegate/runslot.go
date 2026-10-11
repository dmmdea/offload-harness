package delegate

import (
	"context"
	"sync/atomic"
	"time"
)

// runSlot is one subtask's place in its call's width (RunWith's semaphore): the number of
// subtasks a call has open at once, sized from its deal (ADR 0076). The launcher takes the place
// before it starts the subtask, and the subtask gives it back when it ends. A capacity wait that
// has to outlast its TTL gives its place up and takes one again only to dispatch (ADR 0073,
// amended by the 2026-10-10 amendment): the places stay what bounds the dispatches a call has open
// at once, while a subtask that is waiting for a node to free no longer keeps a sibling that has
// not started out of the call.
//
// One goroutine owns a runSlot (the subtask's), so its methods are not safe for two callers; held
// is atomic only so a test may read it.
type runSlot struct {
	sem  chan struct{}
	held atomic.Bool
}

// newRunSlot is the place the launcher has just taken on sem.
func newRunSlot(sem chan struct{}) *runSlot {
	s := &runSlot{sem: sem}
	s.held.Store(true)
	return s
}

// holds reports whether the subtask has its place now.
func (s *runSlot) holds() bool { return s != nil && s.held.Load() }

// release gives the place back so a subtask that has not started can take it. It reports whether
// it released one.
func (s *runSlot) release() bool {
	if s == nil || !s.held.CompareAndSwap(true, false) {
		return false
	}
	<-s.sem
	return true
}

// acquire takes the place back, waiting for one to free until `until` or until ctx ends. blocked
// says the call had to wait for it, which means everything the caller read before the wait is old.
// ok is false when the wait ended first. A subtask that already holds its place gets it at once.
func (s *runSlot) acquire(ctx context.Context, until time.Time) (ok, blocked bool) {
	if s == nil || s.held.Load() {
		return true, false
	}
	select {
	case s.sem <- struct{}{}:
		s.held.Store(true)
		return true, false
	default:
	}
	timer := time.NewTimer(max(time.Until(until), 0))
	defer timer.Stop()
	select {
	case s.sem <- struct{}{}:
		s.held.Store(true)
		return true, true
	case <-ctx.Done():
		return false, true
	case <-timer.C:
		return false, true
	}
}

// finish gives the place back when the subtask ends, whichever way it ended.
func (s *runSlot) finish() { s.release() }

type runSlotKey struct{}

// withRunSlot carries the subtask's place down to the capacity wait, the only code that gives it
// up and takes it back.
func withRunSlot(ctx context.Context, s *runSlot) context.Context {
	return context.WithValue(ctx, runSlotKey{}, s)
}

// runSlotFrom is the subtask's place, nil for a caller that has none (a direct call of the wait, a
// path outside RunWith): there is then nothing to give up and the wait is bounded as it always was.
func runSlotFrom(ctx context.Context) *runSlot {
	s, _ := ctx.Value(runSlotKey{}).(*runSlot)
	return s
}
