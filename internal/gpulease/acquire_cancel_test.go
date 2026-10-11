package gpulease

// A queued Acquire could not be ended from outside: it polled until its Wait ran out. The `gpu reserve`
// wrapper stands in that line for up to --wait 8h, and a console close or a kill during it had nothing to
// stop: the process was ended with its waiter record in the line (pruned later, once its pid was found
// gone). Options.Cancel ends the wait at the next poll with ErrCancelled, unregistering the waiter through
// Acquire's own defer and claiming nothing.

import (
	"errors"
	"testing"
	"time"
)

func TestACancelledAcquireLeavesTheLineAndClaimsNothing(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "holder", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	defer func() { _ = holder.Release() }()

	cancel := make(chan struct{})
	res := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "waiter", Wait: time.Minute, WaitOut: true, Cancel: cancel})
		if aerr == nil {
			_ = l.Release()
		}
		res <- aerr
	}()
	waitAllRegistered(t, m, 1, 3*time.Second)
	closedAt := time.Now()
	close(cancel)
	select {
	case aerr := <-res:
		if !errors.Is(aerr, ErrCancelled) {
			t.Fatalf("a cancelled wait must end with ErrCancelled, got %v", aerr)
		}
		if d := time.Since(closedAt); d > 3*time.Second {
			t.Errorf("the wait ended %s after the cancel; it must end at the next poll (about %s)", d, acquirePollInterval)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled wait kept waiting")
	}
	if n := len(m.Waiters()); n != 0 {
		t.Errorf("the cancelled waiter must leave the line, %d record(s) remain", n)
	}
	if info := m.Inspect(); !info.Held || info.Reason != "holder" {
		t.Errorf("the cancel must not touch the holder's lease: %+v", info)
	}
}

// A cancel that is already closed when Acquire is called registers nothing and asks for nothing, even on a free card:
// the caller was told to stop before it began.
func TestACancelledAcquireOnAFreeCardTakesNothing(t *testing.T) {
	m := realClockManager(t)
	cancel := make(chan struct{})
	close(cancel)
	l, err := m.Acquire(ClassText, Options{Reason: "late", Wait: time.Minute, Cancel: cancel})
	if !errors.Is(err, ErrCancelled) || l != nil {
		if l != nil {
			_ = l.Release()
		}
		t.Fatalf("an Acquire cancelled before it began must return ErrCancelled and no lease, got lease=%v err=%v", l != nil, err)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("nothing may be claimed: %+v", info)
	}
	if n := len(m.Waiters()); n != 0 {
		t.Errorf("nothing may be registered: %d waiter(s)", n)
	}
}

// Without Cancel the wait is what it was: a nil channel never fires, and a free card is taken at once.
func TestAcquireWithoutACancelIsUnchanged(t *testing.T) {
	m := realClockManager(t)
	l, err := m.Acquire(ClassText, Options{Reason: "plain", Wait: time.Second})
	if err != nil {
		t.Fatalf("a free card without a cancel: %v", err)
	}
	_ = l.Release()
}
