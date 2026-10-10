package gpulease

// The gated no-wait attempt (register D-1xx-3, 2026-10-09) and what its review found around
// it: whom a refused request names, whether a seat admission holds back cards it has nothing
// to do with, how long a waiter that declared a long wait is kept, and what a request that
// could not take a place in line says. The headline tests are in queue_fifo_test.go; these pin
// the edges the pre-ship review of D-1xx-3 closed.

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stderrOf runs fn with os.Stderr redirected and returns what it wrote.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() {
			os.Stderr = orig
			_ = w.Close()
		}()
		fn()
	}()
	return <-done
}

// A refused request names the waiter that was actually AHEAD of it: older, and wanting a card
// it wants (the rule isFrontOfQueue applies). The first other waiter in the listing is neither
// in general. Here an older waiter holds card a, a younger one card b, and a newcomer asks for
// card b: the one in its way is the card-b waiter, and the card-a waiter has nothing to do
// with it.
func TestARefusedRequestNamesTheWaiterAheadOnItsOwnCards(t *testing.T) {
	m := scopedRealClock(t)
	const cardA, cardB = "card-a", "card-b"
	olderOnA, unregA := m.registerWaiter(ClassText, Options{Reason: "older waiter on card a", Devices: []string{cardA}})
	defer unregA()
	time.Sleep(5 * time.Millisecond)
	onB, unregB := m.registerWaiter(ClassSeat, Options{Reason: "waiter on card b", Devices: []string{cardB}})
	defer unregB()
	if olderOnA.path == "" || onB.path == "" {
		t.Fatal("failed to register the queued waiters — test cannot proceed")
	}
	time.Sleep(5 * time.Millisecond)

	_, err := m.Acquire(ClassMedia, Options{Reason: "render on b", TTL: time.Hour, Devices: []string{cardB}, Wait: 120 * time.Millisecond})
	if err == nil {
		t.Fatal("a request on card b won the card ahead of the waiter registered for it")
	}
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued, got: %v", err)
	}
	if !strings.Contains(err.Error(), "waiter on card b") {
		t.Fatalf("the refusal must name the waiter on the request's own card: %v", err)
	}
	if strings.Contains(err.Error(), "older waiter on card a") {
		t.Fatalf("the refusal named a waiter on a disjoint card as the one in the way: %v", err)
	}
}

// Nor does it name a waiter BEHIND the caller. Here the caller is held back by a live place-
// keeping token that arrived before it, and a waiter younger than the caller sits on the same
// card: that younger waiter is not what the caller is waiting for.
func TestARefusedRequestDoesNotNameAWaiterBehindIt(t *testing.T) {
	m := scopedRealClock(t)
	const cardA = "card-a"
	if _, err := m.LeaveToken(ClassMedia, Options{Reason: "place held for a caller who left", Devices: []string{cardA}}, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("leave token: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var err error
	go func() {
		defer wg.Done()
		_, err = m.Acquire(ClassMedia, Options{Reason: "caller held back by the token", TTL: time.Hour, Devices: []string{cardA}, Wait: 400 * time.Millisecond})
	}()
	waitAllRegistered(t, m, 1, 2*time.Second)
	time.Sleep(5 * time.Millisecond) // the younger waiter's SinceMs strictly follows the caller's
	younger, unreg := m.registerWaiter(ClassSeat, Options{Reason: "younger waiter behind the caller", Devices: []string{cardA}})
	defer unreg()
	if younger.path == "" {
		t.Fatal("failed to register the younger waiter — test cannot proceed")
	}
	wg.Wait()
	if err == nil || !errors.Is(err, ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued behind the token, got: %v", err)
	}
	if strings.Contains(err.Error(), "younger waiter behind the caller") {
		t.Fatalf("the refusal named a waiter that arrived AFTER the caller: %v", err)
	}
	if !strings.Contains(err.Error(), "a place held for another caller") {
		t.Fatalf("the refusal must name the place that was actually ahead: %v", err)
	}
}

// If whoever was ahead has left by the time the refusal is worded, the answer is still a place-
// in-line one (ErrStillQueued, retry), not a claim that the card is held.
func TestARefusalWithNoOneLeftAheadIsStillAPlaceInLineAnswer(t *testing.T) {
	m := scopedRealClock(t)
	self := Waiter{PID: os.Getpid(), Class: ClassMedia, SinceMs: time.Now().UnixMilli(), Devices: []string{"card-a"}, path: "not-on-disk.json"}
	err := queueTimeoutErr(m, self, 0)
	var held *ErrHeld
	if errors.As(err, &held) || !errors.Is(err, ErrStillQueued) {
		t.Fatalf("expected a plain ErrStillQueued, got %T: %v", err, err)
	}
}

// The words of the refusal are true of what the request did. A request that made one attempt
// and was refused on the spot did not "give up waiting"; one that stood in line for its window
// did, and says for how long.
func TestTheRefusalWordsMatchWhetherTheRequestWaited(t *testing.T) {
	m := realClockManager(t)
	ahead, unreg := m.registerWaiter(ClassText, Options{Reason: "waiter ahead"})
	defer unreg()
	if ahead.path == "" {
		t.Fatal("failed to register the queued waiter — test cannot proceed")
	}
	time.Sleep(5 * time.Millisecond)

	_, err := m.Acquire(ClassMedia, Options{Reason: "no wait", TTL: time.Hour})
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued, got: %v", err)
	}
	if strings.Contains(err.Error(), "gave up") || strings.Contains(err.Error(), "waiting") {
		t.Fatalf("a refusal on the spot must not claim it waited: %v", err)
	}
	if !strings.Contains(err.Error(), "waiter ahead") {
		t.Fatalf("the refusal must name who is ahead: %v", err)
	}

	_, err = m.Acquire(ClassMedia, Options{Reason: "waited", TTL: time.Hour, Wait: 150 * time.Millisecond})
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued after the window, got: %v", err)
	}
	if !strings.Contains(err.Error(), "gave up after waiting 150ms") {
		t.Fatalf("a request that stood in line must say how long: %v", err)
	}
}

// A blocked seat admission waits for ITS seat's cards, and registers its place there. A fresh
// claim on a free card the seat has nothing to do with is not held back by it (disjoint
// backfill, which the gate promises); a claim on the seat's own card yields to it.
func TestASeatWaiterOnOneCardDoesNotStopAClaimOnAFreeCard(t *testing.T) {
	m := scopedRealClock(t)
	// RegisterSeatWaiter builds its own Manager with the real process-start stamp; the stub
	// realClockManager carries would read that record as a recycled pid and prune it.
	m.procStart = processStart
	const cardA, cardB = "card-a", "card-b"
	refresh, unregister := RegisterSeatWaiter(m.leaseDir(), "load seat on card a", []string{cardA})
	defer unregister()
	refresh()
	time.Sleep(5 * time.Millisecond)

	if lease, err := m.Acquire(ClassMedia, Options{Reason: "render on a", TTL: time.Hour, Devices: []string{cardA}}); err == nil {
		_ = lease.Release()
		t.Fatal("a no-wait claim on the seat's own card must yield to the seat's waiter")
	} else if !errors.Is(err, ErrStillQueued) || !strings.Contains(err.Error(), "load seat on card a") {
		t.Fatalf("expected ErrStillQueued naming the seat waiter, got: %v", err)
	}
	lease, err := m.Acquire(ClassMedia, Options{Reason: "render on b", TTL: time.Hour, Devices: []string{cardB}})
	if err != nil {
		t.Fatalf("a no-wait claim on a free card the seat does not sit on must be granted: %v", err)
	}
	_ = lease.Release()
}

// A seat whose cards cannot be named registers as the whole node: unknown is every card, the
// direction of every doubt in the text-load gate. It holds back a claim on any card.
func TestASeatWaiterWithNoCardsNamedIsTheWholeNode(t *testing.T) {
	m := scopedRealClock(t)
	m.procStart = processStart
	_, unregister := RegisterSeatWaiter(m.leaseDir(), "load a seat that cannot be placed", nil)
	defer unregister()
	time.Sleep(5 * time.Millisecond)

	lease, err := m.Acquire(ClassMedia, Options{Reason: "render on b", TTL: time.Hour, Devices: []string{"card-b"}})
	if err == nil {
		_ = lease.Release()
		t.Fatal("a seat that cannot be placed is the whole node: a claim on any card queues behind it")
	}
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued, got: %v", err)
	}
}

// A device list that does not normalize is not a set of cards: it registers as the whole node,
// never as a record naming nothing the claims use.
func TestASeatWaiterWithAMalformedCardListIsTheWholeNode(t *testing.T) {
	m := scopedRealClock(t)
	m.procStart = processStart
	_, unregister := RegisterSeatWaiter(m.leaseDir(), "load with a bad card list", []string{"card-a", " "})
	defer unregister()
	ws := m.Waiters()
	if len(ws) != 1 || len(ws[0].Devices) != 0 {
		t.Fatalf("a blank card id must register the whole node, got %+v", ws)
	}
}

// A waiter that declared a wait is kept for it. The debris cap used to be a flat 12 hours from
// arrival, so a `gpu reserve --wait 20h` that was alive and heartbeating was reaped at hour 12
// and its refresh re-created a record the next reader reaped again: out of the line for the
// last eight hours of a wait it was told it had. A record that declared nothing is judged as
// before, and one that outlives its own deadline by more than the slack is debris.
func TestAWaiterIsKeptForItsOwnDeclaredWaitPastTheDefaultCap(t *testing.T) {
	m, now := newTestManager(t)
	long, unregLong := m.registerWaiter(ClassText, Options{Reason: "declared 20h", Wait: 20 * time.Hour})
	defer unregLong()
	flat, unregFlat := m.registerWaiter(ClassSeat, Options{Reason: "declared nothing"})
	defer unregFlat()
	if long.path == "" || flat.path == "" {
		t.Fatal("failed to register the waiters — test cannot proceed")
	}
	if long.DeadlineMs == 0 || flat.DeadlineMs != 0 {
		t.Fatalf("the declared wait must be recorded (and only when declared): long=%d flat=%d", long.DeadlineMs, flat.DeadlineMs)
	}
	reasons := func() map[string]bool {
		out := map[string]bool{}
		for _, w := range m.Waiters() {
			out[w.Reason] = true
		}
		return out
	}

	*now = now.Add(13 * time.Hour) // past the flat 12 h cap, inside the declared 20 h
	m.refreshWaiter(long)          // the heartbeat a live Acquire loop keeps up
	got := reasons()
	if !got["declared 20h"] {
		t.Fatalf("a live waiter that declared --wait 20h was reaped at hour 13: %v", got)
	}
	if got["declared nothing"] {
		t.Fatalf("a waiter that declared no wait is still debris past the flat cap: %v", got)
	}

	*now = now.Add(9 * time.Hour) // hour 22: past the declared deadline (20 h) plus the hour of slack
	m.refreshWaiter(long)
	if got := reasons(); got["declared 20h"] {
		t.Fatalf("a waiter that outlived its own declared deadline by more than the slack is debris: %v", got)
	}
}

// The record carries the wait across a refresh: refreshWaiter rewrites the file from the Waiter,
// and a refresh that dropped the field would make the declared wait last until the first beat.
func TestARefreshKeepsTheDeclaredDeadline(t *testing.T) {
	m := realClockManager(t)
	w, unreg := m.registerWaiter(ClassText, Options{Reason: "declared 20h", Wait: 20 * time.Hour})
	defer unreg()
	if w.path == "" {
		t.Fatal("failed to register the waiter — test cannot proceed")
	}
	m.refreshWaiter(w)
	ws := m.Waiters()
	if len(ws) != 1 || ws[0].DeadlineMs != w.DeadlineMs || ws[0].DeadlineMs == 0 {
		t.Fatalf("the refreshed record must carry the declared deadline (%d): %+v", w.DeadlineMs, ws)
	}
}

// A request that cannot take a place in line says so, once. The gate is fail-soft by design (a
// record that was never written answers "front of the queue" so a bookkeeping fault never
// refuses GPU work), which made the loss silent: the request claimed the card as a bare claim
// would, ahead of waiters it should have queued behind, and nothing showed it. The claim still
// succeeds; the warning is printed once per process, not once per call.
func TestARequestThatCannotRegisterSaysSoOnceAndStillClaims(t *testing.T) {
	m := realClockManager(t)
	// A regular file where the waiters directory should be: MkdirAll fails on it.
	if err := os.WriteFile(m.waitersDir(), []byte("not a directory"), 0o666); err != nil {
		t.Fatalf("setup: %v", err)
	}
	unregisteredWarned.Store(false)
	t.Cleanup(func() { unregisteredWarned.Store(false) })

	out := stderrOf(t, func() {
		for _, reason := range []string{"first", "second"} {
			lease, err := m.Acquire(ClassMedia, Options{Reason: reason, TTL: time.Hour})
			if err != nil {
				t.Errorf("a request whose place in line cannot be recorded must still claim a free card: %v", err)
				return
			}
			_ = lease.Release()
		}
	})
	if n := strings.Count(out, "could not record this request's place in line"); n != 1 {
		t.Fatalf("expected the warning exactly once for two requests, got %d: %q", n, out)
	}
	if !strings.Contains(out, "without queueing behind earlier waiters") {
		t.Fatalf("the warning must say what the loss means: %q", out)
	}
}
