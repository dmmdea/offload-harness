package gpulease

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A waiter refused by a lease whose owner is long gone must be told which lease, what is
// wrong with it, for how long, what is running and the exact command: "held by pid N" sent
// readers away for ten rounds of the same incident.
func TestErrHeldNamesAnOrphanedLeaseAndTheTakeoverCommand(t *testing.T) {
	fakeProcs(t) // pid 777 is dead
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Command: "python film.py --spec film.json",
		Owner: Owner{Session: "sess-A", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	m.ObserveOwner(m.Inspect()) // the first observer stamps the moment the owner went
	*now = now.Add(40 * time.Minute)
	_ = l.Renew()

	_, err = m.TryAcquire(ClassMedia, Options{Reason: "another render"})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("want ErrHeld, got %v", err)
	}
	msg := held.Error()
	for _, want := range []string{"held-orphaned", "session sess-A", "gone for 40m0s", "grace", "film.py", "local-offload gpu takeover --epoch " + strconv.FormatUint(held.Info.Epoch, 10), "nothing reclaims or kills it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	// A healthy lease's refusal is unchanged: no takeover advice for a lease that is fine.
	m2, _ := newTestManager(t)
	l2, err := m2.TryAcquire(ClassMedia, Options{Reason: "healthy", TTL: time.Hour, Owner: Owner{PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l2.Release() }()
	_, err = m2.TryAcquire(ClassMedia, Options{Reason: "other"})
	if !errors.As(err, &held) || strings.Contains(held.Error(), "takeover") || strings.Contains(held.Error(), "held-") {
		t.Fatalf("a lease inside the grace is not escalated: %v", err)
	}
}

func TestExplainHeldHonoursTheInstalledGrace(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	m.ObserveOwner(info)
	*now = now.Add(20 * time.Minute)
	_ = l.Renew()

	if got := m.ExplainHeld(m.Inspect(), 0); !strings.Contains(got, "held-orphaned") {
		t.Fatalf("20 minutes gone is past the 15 minute default: %q", got)
	}
	SetDefaultOrphanGrace(time.Hour)
	t.Cleanup(func() { SetDefaultOrphanGrace(0) })
	if got := m.ExplainHeld(m.Inspect(), 0); got != "" {
		t.Fatalf("with a one hour grace installed, 20 minutes is inside it: %q", got)
	}
	// An explicit grace beats the installed one.
	if got := m.ExplainHeld(m.Inspect(), 10*time.Minute); !strings.Contains(got, "held-orphaned") {
		t.Fatalf("explicit grace: %q", got)
	}
	SetDefaultOrphanGrace(-1)
	if orphanGraceOrDefault(0) != DefaultOrphanGrace {
		t.Fatal("a non-positive grace restores the default")
	}
}

func TestExplainHeldNamesAnOverdueAndAStalledLease(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "r", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	*now = now.Add(3 * time.Hour)
	_ = l.Renew()
	got := m.ExplainHeld(m.Inspect(), 0)
	if !strings.Contains(got, "held-overdue") || !strings.Contains(got, "2h0m0s") || !strings.Contains(got, "still renewing") {
		t.Fatalf("overdue: %q", got)
	}
	if m.ExplainHeld(Info{}, 0) != "" {
		t.Fatal("a free lease needs no explanation")
	}
}

// FORMATTING A REFUSAL IS A READ. ErrHeld.Error() used to derive the lease's standing the
// stamping way: it wrote the orphan marker under the epoch lock. A waiter, the text gate's
// refusal and every %v of an ErrHeld inherited that write, and one formatted while another
// process held the epoch lock spun the lock's full wait (about two seconds) for nothing.
// Waiters read the marker a status surface recorded; only `gpu status`, offload_status and
// the fleet health write it.
func TestErrHeldFormattingNeverWritesAndNeverTakesTheEpochLock(t *testing.T) {
	fakeProcs(t) // pid 777 is dead
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Command: "python film.py",
		Owner: Owner{Session: "sess-A", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	*now = now.Add(40 * time.Minute) // the owner is long gone, and nobody has looked yet
	_ = l.Renew()

	// (1) formatting writes nothing.
	_, err = m.TryAcquire(ClassMedia, Options{Reason: "another render"})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("want ErrHeld, got %v", err)
	}
	_ = held.Error()
	_ = m.ExplainHeld(m.Inspect(), 0)
	if marks := orphanMarks(t, m); len(marks) != 0 {
		t.Fatalf("formatting a refusal must not stamp the orphan marker: %v", marks)
	}

	// (2) formatting while the epoch lock is held by another process returns at once.
	if err := os.WriteFile(m.epochLockPath(), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(m.epochLockPath())
	start := time.Now()
	_ = held.Error()
	_ = m.ExplainHeld(m.Inspect(), 0)
	if d := time.Since(start); d > 750*time.Millisecond {
		t.Fatalf("formatting a refusal must not wait on the epoch lock (took %s)", d)
	}
}

// A refusal still reads the marker another observer recorded: the sentence is useful for an
// orphan whenever any status surface has seen it.
func TestErrHeldReadsTheMarkerAStatusCallRecorded(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "sess-A", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	_ = m.Standing(m.Inspect(), 0) // a status surface: the first observer stamps
	*now = now.Add(40 * time.Minute)
	_ = l.Renew()
	if got := m.ExplainHeld(m.Inspect(), 0); !strings.Contains(got, "held-orphaned") || !strings.Contains(got, "gone for 40m0s") {
		t.Fatalf("the waiter must read the recorded moment: %q", got)
	}
	if n := len(orphanMarks(t, m)); n != 1 {
		t.Fatalf("reading must neither add nor clear the marker, got %d", n)
	}
}
