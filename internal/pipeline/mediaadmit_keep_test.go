package pipeline

// A call that resumed a place in line and is now WAITING for its card (in this process, for the
// card's slot) is present, but a token's life is its last poll and the wait polls nothing: after
// the grace the place read as absent and later callers and other processes' waiters skipped it, so
// a caller that did everything right lost its turn while it stood in it. The wait refreshes the
// place, and nothing else does; a place that was never resumed is not invented.

import (
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func shortKeepInterval(t *testing.T, every time.Duration) {
	t.Helper()
	old := placeKeepEvery
	placeKeepEvery = every
	t.Cleanup(func() { placeKeepEvery = old })
}

// polledSamples collects the distinct last-poll stamps a token shows while fn runs.
func polledSamples(m *gpulease.Manager, id string, fn func()) map[int64]bool {
	seen := map[int64]bool{}
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, tk := range m.Tokens() {
				if tk.ID == id {
					mu.Lock()
					seen[tk.PolledMs] = true
					mu.Unlock()
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	fn()
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	return seen
}

func TestAResumedPlaceIsKeptAliveWhileItsCallWaitsForTheCardSlot(t *testing.T) {
	shortKeepInterval(t, 15*time.Millisecond)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	pinned := leaseIDOf(admitUUIDC)
	// The card has no lease record, but another job in THIS process holds its slot: the resumed
	// call waits for it in-process, polling nothing.
	if !mediaSlots.tryTake([]string{pinned}) {
		t.Fatal("could not hold the card's slot")
	}
	defer mediaSlots.release([]string{pinned})
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{pinned}}, time.Now().Add(-5*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	samples := polledSamples(f.m, tok.ID, func() {
		f.await(f.image(map[string]any{"waiter_token": tok.ID}))
	})
	if len(samples) < 4 {
		t.Fatalf("the place was re-asserted %d time(s) during a %d ms wait; a waiting call must keep its place live", len(samples)-1, f.cfg.GPUWaitMs)
	}
}

// A call that never held a place does not conjure one by waiting.
func TestAFirstTimeCallWaitingForTheSlotCreatesNoPlaceUntilItGivesUp(t *testing.T) {
	shortKeepInterval(t, 15*time.Millisecond)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	pinned := leaseIDOf(admitUUIDC)
	if !mediaSlots.tryTake([]string{pinned}) {
		t.Fatal("could not hold the card's slot")
	}
	defer mediaSlots.release([]string{pinned})
	ch := f.image(nil)
	time.Sleep(100 * time.Millisecond)
	if n := len(f.m.Tokens()); n != 0 {
		t.Fatalf("%d token(s) while a first-time call was still inside its window: a place is left only when the call gives up", n)
	}
	if r := f.await(ch); r.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want the queued answer at the end of the window, got %q", r.Meta.ErrClass)
	}
}

// Once the call is served its place is spent, and the keeper must not bring it back.
func TestAServedResumedCallLeavesNoPlaceBehind(t *testing.T) {
	shortKeepInterval(t, time.Millisecond) // ticks inside the grant itself, so a keeper left running would revive the place
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	pinned := leaseIDOf(admitUUIDC)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{pinned}}, time.Now().Add(-5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	f.letRunnersGo()
	if r := f.await(f.image(map[string]any{"waiter_token": tok.ID})); !r.OK {
		t.Fatalf("the card is free, the call must be served: %q %s", r.Meta.ErrClass, r.Reason)
	}
	time.Sleep(60 * time.Millisecond) // several keeper ticks, were it still running
	if n := len(f.m.Tokens()); n != 0 {
		t.Fatalf("%d token(s) after the call was served: the keeper revived a spent place", n)
	}
}

// The same for a call that holds the whole node.
func TestAResumedWholeNodePlaceIsKeptAliveWhileItsCallWaits(t *testing.T) {
	shortKeepInterval(t, 15*time.Millisecond)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	if !mediaSlots.tryTake(nil) { // another whole-node job in this process
		t.Fatal("could not hold the whole node's slot")
	}
	defer mediaSlots.release(nil)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "run-graph"}, time.Now().Add(-5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	samples := polledSamples(f.m, tok.ID, func() {
		f.await(f.graph(map[string]any{"waiter_token": tok.ID}))
	})
	if len(samples) < 4 {
		t.Fatalf("the whole-node place was re-asserted %d time(s) during a %d ms wait", len(samples)-1, f.cfg.GPUWaitMs)
	}
}

// Stopping the keeper stops it: nothing touches the token afterwards, so a call that has moved on
// (served, or gone back to the caller) does not keep a place alive by leaving a goroutine behind.
func TestAStoppedKeeperTouchesNothing(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	shortKeepInterval(t, time.Millisecond)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{leaseIDOf(admitUUIDC)}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stop := keepPlace(f.m, tok.ID)
	time.Sleep(25 * time.Millisecond)
	stop()
	stop() // idempotent
	polled := func() int64 {
		for _, tk := range f.m.Tokens() {
			if tk.ID == tok.ID {
				return tk.PolledMs
			}
		}
		t.Fatal("the token vanished")
		return 0
	}
	before := polled()
	if before == tok.PolledMs {
		t.Fatal("the keeper never refreshed the place while it ran")
	}
	time.Sleep(40 * time.Millisecond)
	if after := polled(); after != before {
		t.Errorf("the place was touched %d ms after the keeper was stopped", after-before)
	}
	if stop := keepPlace(f.m, ""); stop == nil {
		t.Error("keeping no place still returns a stop func")
	} else {
		stop()
	}
}
