package gpulease

// F82, the hold bound (the incident of 2026-10-14). A lease that cleared the seat and now asks the host for its RAM
// holds its cards for the whole wait. The CLI bounds that hold; what the library owes it is the record: while the wait
// runs the lease's record says when the bound releases it (Meta.HostRAMHoldUntilMs), so `gpu status`, a different process,
// can tell the operator. The stamp is there only while the wait is: it goes when the figure is admitted and when the wait
// ends without admission (a lease that stays held after a wait that --wait ended must not go on claiming a bound), and a
// wait that has no bound stamps nothing.

import (
	"testing"
	"time"
)

// waitForHoldStamp polls the record until the bound is stamped on it, as a reader in another process would.
func waitForHoldStamp(t *testing.T, m *Manager, epoch uint64) Info {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec := recordOf(t, m, epoch); !rec.HostRAMHoldUntil.IsZero() {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a wait with a bound never stamped the moment the bound releases the lease on the record, where another process can read it")
	return Info{}
}

func TestTheAwaitStampsWhenTheHoldBoundReleasesTheLeaseWhileItWaitsAndClearsItOnAdmission(t *testing.T) {
	m, f := ramScoped(t, 70) // 70 + 30 > 92: the host has no room until the test says so
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if rec := recordOf(t, m, l.Epoch()); !rec.HostRAMHoldUntil.IsZero() {
		t.Fatalf("no wait has begun, so no bound is stamped: %v", rec.HostRAMHoldUntil)
	}

	until := time.Now().Add(20 * time.Second).Truncate(time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: until, HoldUntil: until}) }()
	rec := waitForHoldStamp(t, m, l.Epoch())
	if d := rec.HostRAMHoldUntil.Sub(until); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("the stamp is the bound the caller gave (%s), got %s", until, rec.HostRAMHoldUntil)
	}
	if rec.HostRAMWaitGiB != 30 || rec.HostRAMGiB != 0 {
		t.Errorf("while it waits the lease still declares nothing and still carries the figure: wait %.1f, declared %.1f", rec.HostRAMWaitGiB, rec.HostRAMGiB)
	}

	f.setCommit(40) // the room appears: 40 + 30 <= 92
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the host has the room now: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not see the room")
	}
	rec = recordOf(t, m, l.Epoch())
	if !rec.HostRAMHoldUntil.IsZero() {
		t.Errorf("an admitted lease is no longer waiting, so no bound is running: %v", rec.HostRAMHoldUntil)
	}
	if rec.HostRAMGiB != 30 || rec.HostRAMWaitGiB != 0 {
		t.Errorf("admitted: declared %.1f, wait %.1f", rec.HostRAMGiB, rec.HostRAMWaitGiB)
	}
}

// A wait that ends without admission (its deadline, here) leaves the lease still asking and no bound running.
func TestAWaitThatEndsRefusedClearsTheBoundItStamped(t *testing.T) {
	m, _ := ramScoped(t, 70)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	until := time.Now().Add(300 * time.Millisecond)
	asHostRAM(t, l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: until, HoldUntil: until}))
	rec := recordOf(t, m, l.Epoch())
	if !rec.HostRAMHoldUntil.IsZero() {
		t.Errorf("the wait is over, so the bound it stamped is not running any more: %v", rec.HostRAMHoldUntil)
	}
	if rec.HostRAMWaitGiB != 30 {
		t.Errorf("the lease still asks for its figure: wait %.1f", rec.HostRAMWaitGiB)
	}
}

// A wait with no bound (the caller passes no HoldUntil: --ram-hold-max 0) stamps nothing, at any moment of the wait.
func TestAWaitWithoutABoundStampsNothing(t *testing.T) {
	m, _ := ramScoped(t, 70)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	done := make(chan error, 1)
	go func() { done <- l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: time.Now().Add(400 * time.Millisecond)}) }()
	for i := 0; i < 20; i++ {
		if rec := recordOf(t, m, l.Epoch()); !rec.HostRAMHoldUntil.IsZero() {
			t.Fatalf("a wait with no bound stamps no bound: %v", rec.HostRAMHoldUntil)
		}
		time.Sleep(15 * time.Millisecond)
	}
	asHostRAM(t, <-done)
}
