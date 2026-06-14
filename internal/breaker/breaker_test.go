package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a monotonically advanceable clock for deterministic tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// helpers -------------------------------------------------------------------

func newBreaker(threshold, window int, cooldown time.Duration, clk *fakeClock) *Breaker {
	return newWithClock(threshold, window, cooldown, clk.Now)
}

func newGroup(threshold, window int, cooldown time.Duration, clk *fakeClock) *Group {
	return newGroupWithClock(threshold, window, cooldown, clk.Now)
}

// ---------------------------------------------------------------------------
// CLOSED → OPEN transition
// ---------------------------------------------------------------------------

func TestTripToOpen(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(3, 5, 10*time.Second, clk)

	if got := b.State(); got != "closed" {
		t.Fatalf("want closed, got %s", got)
	}
	if !b.Allow() {
		t.Fatal("should allow in closed state")
	}

	// Record 2 failures — below threshold.
	b.Record(false)
	b.Record(false)
	if b.State() != "closed" {
		t.Fatal("should still be closed after 2 failures (threshold=3)")
	}

	// Third failure trips.
	b.Record(false)
	if b.State() != "open" {
		t.Fatalf("want open after 3 failures, got %s", b.State())
	}
	if b.Allow() {
		t.Fatal("Allow() must return false while OPEN within cooldown")
	}
}

// ---------------------------------------------------------------------------
// OPEN → HALF-OPEN after cooldown
// ---------------------------------------------------------------------------

func TestOpenToHalfOpen(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(2, 4, 5*time.Second, clk)

	b.Record(false)
	b.Record(false) // trip
	if b.State() != "open" {
		t.Fatal("expected open")
	}

	// Before cooldown expires: still open.
	clk.Advance(4 * time.Second)
	if b.Allow() {
		t.Fatal("Allow() must be false before cooldown elapses")
	}

	// Exactly at cooldown: half-open.
	clk.Advance(1 * time.Second) // total = 5s = cooldown
	if !b.Allow() {
		t.Fatal("Allow() must return true once cooldown elapsed (probe)")
	}
	if b.State() != "half-open" {
		t.Fatalf("want half-open, got %s", b.State())
	}

	// A second Allow() while probe is outstanding must be denied.
	if b.Allow() {
		t.Fatal("second Allow() while probe outstanding must return false")
	}
}

// ---------------------------------------------------------------------------
// HALF-OPEN: successful probe → CLOSED
// ---------------------------------------------------------------------------

func TestHalfOpenSuccessCloses(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(2, 4, 5*time.Second, clk)

	b.Record(false)
	b.Record(false)
	clk.Advance(5 * time.Second)

	b.Allow() // dispatch probe
	b.Record(true)

	if b.State() != "closed" {
		t.Fatalf("want closed after successful probe, got %s", b.State())
	}
	if !b.Allow() {
		t.Fatal("Allow() should return true in closed state")
	}
}

// ---------------------------------------------------------------------------
// HALF-OPEN: failed probe → OPEN (re-opens immediately)
// ---------------------------------------------------------------------------

func TestHalfOpenFailureReopens(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(2, 4, 5*time.Second, clk)

	b.Record(false)
	b.Record(false)
	clk.Advance(5 * time.Second)

	b.Allow() // dispatch probe
	b.Record(false)

	if b.State() != "open" {
		t.Fatalf("want open after failed probe, got %s", b.State())
	}
	// Cooldown restarted — must still block.
	if b.Allow() {
		t.Fatal("Allow() must be false immediately after re-open")
	}
}

// ---------------------------------------------------------------------------
// Sliding window eviction: old failures age out
// ---------------------------------------------------------------------------

func TestSlidingWindowEvictsOldFailures(t *testing.T) {
	clk := newFakeClock()
	// threshold=3, window=4
	b := newBreaker(3, 4, 10*time.Second, clk)

	// Fill window with 2 failures + 2 successes.
	b.Record(false)
	b.Record(false)
	b.Record(true)
	b.Record(true)
	// Window: [F, F, T, T] → 2 failures, below threshold.
	if b.State() != "closed" {
		t.Fatal("should still be closed")
	}

	// Adding a success evicts the first F; window: [F, T, T, T] → 1 failure.
	b.Record(true)
	if b.State() != "closed" {
		t.Fatal("should be closed after old failure evicted")
	}

	// Add 2 more failures: window becomes [T, T, F, F] then [T, F, F, F] — trips.
	b.Record(false)
	b.Record(false)
	// After 6th record window slots (size 4):
	// slot sequence written: 0=F,1=F,2=T,3=T,0=T,1=F,2=F → current window [T,F,F,T?]
	// let's just check it trips after 3 failures visible in the window.
	// Actually let's record one more to make failures unambiguously >= 3.
	b.Record(false)
	if b.State() != "open" {
		t.Fatalf("want open, got %s (failures should have reached threshold)", b.State())
	}
}

// ---------------------------------------------------------------------------
// Successes interspersed never trip
// ---------------------------------------------------------------------------

func TestSuccessesNeverTrip(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(3, 5, 10*time.Second, clk)

	for i := 0; i < 20; i++ {
		b.Record(true)
		if b.State() != "closed" {
			t.Fatalf("should never open on success-only stream, step %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Group isolates tiers
// ---------------------------------------------------------------------------

func TestGroupIsolatesToTiers(t *testing.T) {
	clk := newFakeClock()
	g := newGroup(2, 4, 5*time.Second, clk)

	// Trip tier-A.
	g.Record("tier-A", false)
	g.Record("tier-A", false)

	if g.State("tier-A") != "open" {
		t.Fatal("tier-A should be open")
	}
	if g.State("tier-B") != "closed" {
		t.Fatal("tier-B should be unaffected")
	}
	if g.Allow("tier-B") != true {
		t.Fatal("tier-B should still allow calls")
	}
}

// ---------------------------------------------------------------------------
// Group lazy creation
// ---------------------------------------------------------------------------

func TestGroupLazyCreation(t *testing.T) {
	clk := newFakeClock()
	g := newGroup(3, 5, 10*time.Second, clk)

	// New tier starts closed.
	if g.State("new-tier") != "closed" {
		t.Fatal("new tier should start closed")
	}
	if !g.Allow("another-tier") {
		t.Fatal("new tier should allow")
	}
}

// ---------------------------------------------------------------------------
// State() lazy-advances OPEN → HALF-OPEN
// ---------------------------------------------------------------------------

func TestStateLazyAdvance(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(1, 2, 3*time.Second, clk)

	b.Record(false) // trip
	if b.State() != "open" {
		t.Fatal("should be open")
	}

	clk.Advance(3 * time.Second)
	// State() should lazily transition without an Allow() call.
	if got := b.State(); got != "half-open" {
		t.Fatalf("want half-open via State(), got %s", got)
	}
}

// ---------------------------------------------------------------------------
// Full cycle: CLOSED → OPEN → HALF-OPEN → CLOSED
// ---------------------------------------------------------------------------

func TestFullCycle(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(2, 3, 10*time.Second, clk)

	// Two failures trip.
	b.Record(false)
	b.Record(false)
	if b.State() != "open" {
		t.Fatal("step 1: want open")
	}

	// Cooldown.
	clk.Advance(10 * time.Second)

	// Probe.
	if !b.Allow() {
		t.Fatal("step 2: want probe allowed")
	}
	if b.State() != "half-open" {
		t.Fatal("step 2: want half-open")
	}

	// Successful probe → closed.
	b.Record(true)
	if b.State() != "closed" {
		t.Fatal("step 3: want closed")
	}

	// Normal operation again.
	if !b.Allow() {
		t.Fatal("step 4: want allow in closed")
	}
}

// ---------------------------------------------------------------------------
// Re-open after failed half-open probe, then successful recovery
// ---------------------------------------------------------------------------

func TestReOpenThenRecover(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(1, 2, 5*time.Second, clk)

	b.Record(false) // trip
	clk.Advance(5 * time.Second)
	b.Allow()      // probe
	b.Record(false) // fail → re-open

	if b.State() != "open" {
		t.Fatal("should be open after failed probe")
	}

	// Second cooldown.
	clk.Advance(5 * time.Second)
	if !b.Allow() {
		t.Fatal("second probe should be allowed")
	}
	b.Record(true) // success → closed

	if b.State() != "closed" {
		t.Fatalf("want closed after recovery, got %s", b.State())
	}
}

// ---------------------------------------------------------------------------
// Concurrent safety smoke-test
// ---------------------------------------------------------------------------

func TestConcurrentSafety(t *testing.T) {
	b := New(10, 20, 1*time.Second)
	var wg sync.WaitGroup
	var allowed atomic.Int64

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if b.Allow() {
				allowed.Add(1)
				b.Record(i%3 != 0) // 1/3 failures
			}
		}(i)
	}
	wg.Wait()
	// Just verify no race / panic; state can be anything.
	_ = b.State()
}

// ---------------------------------------------------------------------------
// Group concurrent safety
// ---------------------------------------------------------------------------

func TestGroupConcurrentSafety(t *testing.T) {
	g := NewGroup(5, 10, 1*time.Second)
	tiers := []string{"gemma4-e2b", "offload-e4b", "gemma4-26b-a4b"}
	var wg sync.WaitGroup

	for _, tier := range tiers {
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(tier string, i int) {
				defer wg.Done()
				if g.Allow(tier) {
					g.Record(tier, i%4 != 0)
				}
				_ = g.State(tier)
			}(tier, i)
		}
	}
	wg.Wait()
}
