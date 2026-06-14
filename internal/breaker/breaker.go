// Package breaker provides a per-tier circuit breaker with a sliding-window failure
// counter, hand-rolled with no external dependencies.
//
// States:
//
//	CLOSED     — normal operation; Allow() returns true.
//	OPEN       — tripped; Allow() returns false until cooldown elapses.
//	HALF-OPEN  — cooldown elapsed; exactly ONE probe is allowed through; a
//	             successful Record closes the breaker, a failed one re-opens it.
//
// The sliding window tracks the last windowSize outcomes (bool ring buffer).
// The breaker trips to OPEN when failures within that window reach failureThreshold.
package breaker

import (
	"sync"
	"time"
)

// state is the circuit state.
type state int

const (
	stateClosed   state = iota
	stateOpen     state = iota
	stateHalfOpen state = iota
)

func (s state) String() string {
	switch s {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker is a single-tier circuit breaker.
type Breaker struct {
	mu               sync.Mutex
	failureThreshold int
	windowSize       int
	cooldown         time.Duration
	now              func() time.Time

	// ring buffer: outcomes[head % windowSize] is the oldest slot.
	outcomes []bool // true = success, false = failure
	count    int    // number of recorded outcomes (capped at windowSize)
	head     int    // index of the next slot to write

	failures int // cached failure count in the current window

	st        state
	openedAt  time.Time // when OPEN was entered (for cooldown)
	probeOut  bool      // true when a HALF-OPEN probe has been dispatched
}

// New creates a Breaker.
//   - failureThreshold: number of failures in the sliding window that trips OPEN.
//   - windowSize: size of the outcome ring buffer.
//   - cooldown: how long the breaker stays OPEN before transitioning to HALF-OPEN.
func New(failureThreshold, windowSize int, cooldown time.Duration) *Breaker {
	return newWithClock(failureThreshold, windowSize, cooldown, time.Now)
}

// newWithClock is the internal constructor that accepts an injectable clock.
func newWithClock(failureThreshold, windowSize int, cooldown time.Duration, now func() time.Time) *Breaker {
	return &Breaker{
		failureThreshold: failureThreshold,
		windowSize:       windowSize,
		cooldown:         cooldown,
		now:              now,
		outcomes:         make([]bool, windowSize),
	}
}

// Allow returns true if a call may proceed.
// While OPEN it returns false; in HALF-OPEN exactly one probe is allowed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.st {
	case stateClosed:
		return true

	case stateOpen:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			// Transition to HALF-OPEN.
			b.st = stateHalfOpen
			b.probeOut = true
			return true
		}
		return false

	case stateHalfOpen:
		if !b.probeOut {
			// A probe was allowed but has not been recorded yet — allow it once.
			b.probeOut = true
			return true
		}
		// Another caller: deny while waiting for the probe result.
		return false
	}
	return false
}

// Record registers the outcome of a call.
// success=false counts as a trip-eligible infra failure.
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.st {
	case stateHalfOpen:
		b.probeOut = false
		if success {
			b.trip(true)
		} else {
			b.reOpen()
		}
		return

	case stateOpen:
		// Stale record after re-open — ignore.
		return
	}

	// CLOSED: push into sliding window.
	b.pushOutcome(success)
	if !success && b.failures >= b.failureThreshold {
		b.reOpen()
	}
}

// State returns "closed", "open", or "half-open".
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Lazily advance OPEN → HALF-OPEN on State() queries too.
	if b.st == stateOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		b.st = stateHalfOpen
	}
	return b.st.String()
}

// pushOutcome adds a new outcome to the ring buffer and updates the failure count.
func (b *Breaker) pushOutcome(success bool) {
	slot := b.head % b.windowSize

	// If the window is full, evict the outcome at this slot.
	if b.count == b.windowSize {
		if !b.outcomes[slot] {
			b.failures--
		}
	} else {
		b.count++
	}

	b.outcomes[slot] = success
	if !success {
		b.failures++
	}
	b.head = (b.head + 1) % b.windowSize
}

// reOpen transitions to OPEN, recording the open timestamp.
func (b *Breaker) reOpen() {
	b.st = stateOpen
	b.openedAt = b.now()
}

// trip transitions out of HALF-OPEN (success=true → CLOSED; success=false → OPEN).
func (b *Breaker) trip(success bool) {
	if success {
		// Reset window and go CLOSED.
		b.outcomes = make([]bool, b.windowSize)
		b.count = 0
		b.head = 0
		b.failures = 0
		b.st = stateClosed
	} else {
		b.reOpen()
	}
}

// ---------------------------------------------------------------------------
// Group — lazily creates one Breaker per tier string.
// ---------------------------------------------------------------------------

// Group manages a set of Breakers keyed by tier name.
type Group struct {
	mu               sync.Mutex
	failureThreshold int
	windowSize       int
	cooldown         time.Duration
	now              func() time.Time
	breakers         map[string]*Breaker
}

// NewGroup creates a Group; all per-tier Breakers share the same configuration.
func NewGroup(failureThreshold, windowSize int, cooldown time.Duration) *Group {
	return newGroupWithClock(failureThreshold, windowSize, cooldown, time.Now)
}

func newGroupWithClock(failureThreshold, windowSize int, cooldown time.Duration, now func() time.Time) *Group {
	return &Group{
		failureThreshold: failureThreshold,
		windowSize:       windowSize,
		cooldown:         cooldown,
		now:              now,
		breakers:         make(map[string]*Breaker),
	}
}

func (g *Group) get(tier string) *Breaker {
	g.mu.Lock()
	b, ok := g.breakers[tier]
	if !ok {
		b = newWithClock(g.failureThreshold, g.windowSize, g.cooldown, g.now)
		g.breakers[tier] = b
	}
	g.mu.Unlock()
	return b
}

// Allow delegates to the per-tier Breaker.
func (g *Group) Allow(tier string) bool { return g.get(tier).Allow() }

// Record delegates to the per-tier Breaker.
func (g *Group) Record(tier string, success bool) { g.get(tier).Record(success) }

// State delegates to the per-tier Breaker.
func (g *Group) State(tier string) string { return g.get(tier).State() }
