package gpulease

// Review round 3, items E and K.
//
// K: the stamp of a wait's end (and the detached reserve's push-out of it) is a bound for a lease that still ASKS for the host. On an
// admitted lease it names a bound nothing is running, so the stamp is a no-op there.
//
// E: in the detached form the hidden holder is the only thing that releases the lease if the reserve dies, and it can enforce only
// what is on the record. A stamp that cannot be written used to cost one warning and leave the holder with no bound. With
// RequireStamp the stamp is tried a few times, and if it still has not landed the wait fails (*ErrHoldStamp) before the host is
// looked at.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAStampOnAnAdmittedLeaseIsANoOp(t *testing.T) {
	m, _ := ramScoped(t, 10)
	admitted, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admitted.Release() }()
	until := time.Now().Add(10 * time.Minute)
	if err := m.StampHostRAMHold(admitted.Epoch(), until); err != nil {
		t.Fatal(err)
	}
	if rec := recordOf(t, m, admitted.Epoch()); !rec.HostRAMHoldUntil.IsZero() {
		t.Errorf("an admitted lease is no longer waiting for the host, so no bound is stamped on it: %v", rec.HostRAMHoldUntil)
	}

	m2, _ := ramScoped(t, 10)
	waiting, err := m2.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waiting.Release() }()
	if err := m2.StampHostRAMHold(waiting.Epoch(), until); err != nil {
		t.Fatal(err)
	}
	if rec := recordOf(t, m2, waiting.Epoch()); rec.HostRAMHoldUntil.Sub(until).Abs() > time.Millisecond {
		t.Errorf("a lease that still asks for the host carries the bound: want %s, got %v", until, rec.HostRAMHoldUntil)
	}
}

func fastStampRetries(t *testing.T) {
	t.Helper()
	oldT, oldP := stampTries, stampRetryPause
	stampTries, stampRetryPause = 4, 5*time.Millisecond
	t.Cleanup(func() { stampTries, stampRetryPause = oldT, oldP })
}

func TestARequiredStampThatNeverLandsFailsTheWaitBeforeTheHostIsLookedAt(t *testing.T) {
	fastStampRetries(t)
	m, f := ramScoped(t, 70)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	var calls atomic.Int32
	restore := UseStampFault(func(uint64) error { calls.Add(1); return errors.New("disk full") })
	defer restore()
	var reports atomic.Int32
	reads := f.reads.Load()
	until := time.Now().Add(time.Minute)
	err = l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: until, HoldUntil: until, RequireStamp: true, OnStampError: func(error) { reports.Add(1) }})
	var stampErr *ErrHoldStamp
	if !errors.As(err, &stampErr) {
		t.Fatalf("a stamp that never lands fails a wait that requires it with *ErrHoldStamp: %v", err)
	}
	if stampErr.Tries < 3 || int(calls.Load()) != stampErr.Tries || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("it tried a few times (%d reported, %d made) and the error says why: %v", stampErr.Tries, calls.Load(), err)
	}
	if int(reports.Load()) != stampErr.Tries {
		t.Errorf("every failed try is reported to the caller, which says it once: %d reports for %d tries", reports.Load(), stampErr.Tries)
	}
	if f.reads.Load() != reads {
		t.Errorf("the wait ended before the host was looked at: %d reading(s) taken", f.reads.Load()-reads)
	}
	if rec := recordOf(t, m, l.Epoch()); rec.HostRAMWaitGiB != 30 {
		t.Errorf("the failed wait leaves the lease as it was, still asking: %.1f", rec.HostRAMWaitGiB)
	}
}

func TestARequiredStampThatLandsOnALaterTryDoesNotFailTheWait(t *testing.T) {
	fastStampRetries(t)
	m, f := ramScoped(t, 70)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	var calls atomic.Int32
	restore := UseStampFault(func(uint64) error {
		if calls.Add(1) <= 2 {
			return errors.New("access denied")
		}
		return nil
	})
	defer restore()
	until := time.Now().Add(30 * time.Second)
	done := make(chan error, 1)
	go func() {
		done <- l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: until, HoldUntil: until, RequireStamp: true})
	}()
	rec := waitForHoldStamp(t, m, l.Epoch())
	if rec.HostRAMHoldUntil.Sub(until).Abs() > time.Millisecond {
		t.Errorf("the third try landed the bound: %v", rec.HostRAMHoldUntil)
	}
	f.setCommit(40)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the stamp landed and the host has the room: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not see the room")
	}
}
