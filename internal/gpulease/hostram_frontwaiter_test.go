package gpulease

// G6 of the P0 plan: a front waiter that waits ONLY on the host's memory must not stop a request that adds none.
//
// The line is FIFO among waiters whose cards conflict, and a waiter the host refuses stays at the front for its
// whole --wait (eight hours by default), so before this a 0 GiB request for the same card (a text bench, a render
// that fits the card), or ANY request when the waiter asked for the whole node, queued behind a card that stood
// idle, and was answered "which has not claimed the card". The rule these tests pin:
//
//   - a waiter that waits only on host RAM (WaitingFor == WaitHostRAM: its cards are free, the memory is not) does
//     not hold back a request that declares NO host RAM. That request adds nothing to what the waiter is short of,
//     so it cannot make the shortage worse, and the card would stand idle otherwise;
//   - a request that DOES declare host RAM stays behind it, as it would behind any waiter: its memory competes with
//     the waiter's, and what the refusal says names that;
//   - the cost is stated, not hidden: the request that passes takes the card the waiter wanted, so when the memory
//     recovers the waiter waits for that card. It is bounded because the waiter stops being "waiting only on
//     memory" the moment its cards are held (ErrHeld), and from then on it is an ordinary front waiter nothing
//     passes: the delay is at most the lease of whoever passed, never a stream of them.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type ramAcquired struct {
	l   *Lease
	err error
}

// ramWaiter is a request waiting on the host's memory, started by startRAMWaiter.
type ramWaiter struct {
	done     chan ramAcquired
	consumed atomic.Bool
}

// result waits for the request to end (the test took its outcome, so the cleanup does not wait for it again).
func (w *ramWaiter) result(d time.Duration) (ramAcquired, bool) {
	select {
	case r := <-w.done:
		w.consumed.Store(true)
		return r, true
	case <-time.After(d):
		return ramAcquired{}, false
	}
}

// startRAMWaiter starts a request that declares need GiB of host RAM for devs (none = the whole node) and waits
// for it, and returns once its record says it waits on the host's memory. The test ends it: the host recovers
// and the lease it was finally granted is released.
func startRAMWaiter(t *testing.T, m *Manager, f *fakeHost, need float64, devs ...string) *ramWaiter {
	t.Helper()
	w := &ramWaiter{done: make(chan ramAcquired, 1)}
	go func() {
		l, err := m.Acquire(ClassMedia, Options{Reason: "ram waiter", TTL: time.Hour, Devices: devs, HostRAMGiB: need, Wait: 10 * time.Second, WaitOut: true})
		w.done <- ramAcquired{l, err}
	}()
	t.Cleanup(func() {
		if w.consumed.Load() {
			return
		}
		f.setCommit(0) // the host recovers, so the waiter (if still waiting) is granted and the goroutine ends
		if r, ok := w.result(15 * time.Second); !ok {
			t.Error("the RAM waiter goroutine never ended")
		} else if r.l != nil {
			_ = r.l.Release()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ws := m.Waiters(); len(ws) == 1 && ws[0].WaitingFor == WaitHostRAM {
			return w
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the waiter never registered as waiting on host RAM: %+v", m.Waiters())
	return nil
}

// A request for the SAME card that declares nothing passes a waiter that waits only on memory: the card is idle,
// and the request adds no memory to what the waiter is short of.
func TestARequestThatDeclaresNoHostRAMPassesAWaiterThatWaitsOnlyOnMemory(t *testing.T) {
	m, f := ramScoped(t, 85) // 85 + 30 > 92: the waiter is short; the card is free
	startRAMWaiter(t, m, f, 30, card0)
	time.Sleep(5 * time.Millisecond) // the request arrives AFTER the waiter

	quiet, err := m.Acquire(ClassMedia, Options{Reason: "declares nothing", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatalf("a request that adds no memory must pass a waiter that waits only on memory, got %v", err)
	}
	_ = quiet.Release()
}

// A request that DECLARES host RAM stays behind the waiter, and the sentence it is refused with names what the
// waiter in front is waiting for (not the generic "has not claimed the card").
func TestARequestThatDeclaresHostRAMStaysBehindAWaiterOnMemoryAndNamesIt(t *testing.T) {
	m, f := ramScoped(t, 85)
	startRAMWaiter(t, m, f, 30, card0)
	time.Sleep(5 * time.Millisecond)

	_, err := m.Acquire(ClassMedia, Options{Reason: "also declares", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 1})
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("a request that declares host RAM competes with the waiter's memory and queues behind it, got %v", err)
	}
	if want := "which is waiting for host RAM (needs 30.0 GiB) and has not claimed the card"; !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal must name what the waiter in front is waiting for (%q), got: %v", want, err)
	}
}

// THE HEAD-OF-LINE FINDING (review, 2026-10-10). A whole-node request blocked on host RAM stood first in line for
// its whole --wait and stopped everything, a 0 GiB text bench on a free card included. A waiter that waits only
// on memory holds nothing, whatever cards either side wants.
func TestAWholeNodeHostRAMWaiterDoesNotStopARequestThatDeclaresNoHostRAM(t *testing.T) {
	m, f := ramScoped(t, 85)
	startRAMWaiter(t, m, f, 30) // no devices: the whole node
	if ws := m.Waiters(); len(ws) != 1 || len(ws[0].Devices) != 0 {
		t.Fatalf("want one whole-node waiter on host RAM, got %+v", ws)
	}
	time.Sleep(5 * time.Millisecond)

	bench, err := m.Acquire(ClassText, Options{Reason: "kv bench, declares no host RAM", TTL: time.Hour, Devices: []string{card1}})
	if err != nil {
		t.Fatalf("a 0 GiB request on a free card must pass a whole-node waiter that waits only on host RAM: %v", err)
	}
	_ = bench.Release()

	_, err = m.Acquire(ClassMedia, Options{Reason: "also declares", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 5})
	if !errors.Is(err, ErrStillQueued) || !strings.Contains(err.Error(), "waiting for host RAM (needs 30.0 GiB)") {
		t.Fatalf("a request that declares host RAM stays behind the whole-node waiter and says why, got %v", err)
	}
}

// THE BOUND. The waiter that was passed is served right after the request that passed it, and nothing else
// passes it meanwhile: once its card is held it is no longer "waiting only on memory", so a later request that
// declares nothing queues behind it like any other, and when the passer releases, the waiter gets the card first.
func TestThePassedWaiterIsServedRightAfterThePasserAndNothingElsePassesItMeanwhile(t *testing.T) {
	m, f := ramScoped(t, 85)
	waiter := startRAMWaiter(t, m, f, 30, card0)
	time.Sleep(5 * time.Millisecond)

	passer, err := m.Acquire(ClassMedia, Options{Reason: "passes", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatalf("the first request that declares nothing passes: %v", err)
	}
	f.setCommit(40) // the host recovers while the passer holds the card

	// The waiter now finds its card held, and stops being a waiter on memory.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ws := m.Waiters()
		if len(ws) == 1 && ws[0].WaitingFor == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the waiter never read as waiting for the card once the passer held it: %+v", ws)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// A later request that declares nothing arrives now, and waits its turn behind the waiter.
	later := make(chan ramAcquired, 1)
	go func() {
		l, err := m.Acquire(ClassMedia, Options{Reason: "later", TTL: time.Hour, Devices: []string{card0}, Wait: 10 * time.Second, WaitOut: true})
		later <- ramAcquired{l, err}
	}()
	t.Cleanup(func() {
		select {
		case r := <-later:
			if r.l != nil {
				_ = r.l.Release()
			}
		case <-time.After(15 * time.Second):
			t.Error("the later request never ended")
		}
	})
	for deadline = time.Now().Add(5 * time.Second); len(m.Waiters()) < 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the later request never registered: %+v", m.Waiters())
		}
	}

	_ = passer.Release()
	select {
	case r := <-waiter.done:
		waiter.consumed.Store(true)
		if r.err != nil {
			t.Fatalf("the waiter is served when the passer releases: %v", r.err)
		}
		defer func() { _ = r.l.Release() }()
	case r := <-later:
		if r.l != nil {
			_ = r.l.Release()
		}
		t.Fatal("a later request took the card ahead of the waiter it had just been queued behind")
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter was not served after the passer released the card")
	}
}

// The unit under both gates: who a waiter blocks, and whose cards it holds.
func TestBlocksArrivalAndHoldsItsCardsAgainst(t *testing.T) {
	ramWaiter := Waiter{Devices: []string{card0}, WaitingFor: WaitHostRAM, HostRAMGiB: 30}
	plain := Waiter{Devices: []string{card0}}
	whole := Waiter{WaitingFor: WaitHostRAM, HostRAMGiB: 30}
	cases := []struct {
		name    string
		w       Waiter
		devices []string
		ram     float64
		want    bool
	}{
		{"a waiter on memory blocks a declaring request on its card", ramWaiter, []string{card0}, 5, true},
		{"but not one that declares nothing", ramWaiter, []string{card0}, 0, false},
		{"nor a declaring request on another card", ramWaiter, []string{card1}, 5, false},
		{"a plain waiter blocks a request that declares nothing", plain, []string{card0}, 0, true},
		{"and one that declares host RAM", plain, []string{card0}, 5, true},
		{"a whole-node waiter on memory blocks a declaring request on any card", whole, []string{card1}, 5, true},
		{"but not one that declares nothing", whole, []string{card1}, 0, false},
		{"a whole-node request is blocked by a waiter on a card when it declares host RAM", ramWaiter, nil, 5, true},
		{"and passes it when it declares nothing", ramWaiter, nil, 0, false},
	}
	for _, c := range cases {
		if got := c.w.BlocksArrival(c.devices, c.ram); got != c.want {
			t.Errorf("%s: BlocksArrival = %v, want %v", c.name, got, c.want)
		}
	}
	if !ramWaiter.HoldsItsCardsAgainst(true) || ramWaiter.HoldsItsCardsAgainst(false) || !plain.HoldsItsCardsAgainst(false) || !plain.HoldsItsCardsAgainst(true) {
		t.Error("a waiter on memory holds its cards against a declaring request alone; any other waiter holds them against everyone")
	}
}
