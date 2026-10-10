package gpulease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A queued Acquire says so for as long as it waits, and says nothing once it
// returns — the holder's release path reads this to decide whether a warm-back
// is worth anything (register D-124).
func TestAQueuedAcquireIsListedAsAWaiterUntilItReturns(t *testing.T) {
	m, _ := newTestManager(t)
	holder, err := m.TryAcquire(ClassText, Options{Reason: "arm A", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if ws := m.Waiters(); len(ws) != 0 {
		t.Fatalf("nobody is queued yet, got %+v", ws)
	}
	done := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "arm B", TTL: time.Hour, Wait: 5 * time.Second, WaitOut: true})
		if aerr == nil {
			_ = l.Release()
		}
		done <- aerr
	}()
	deadline := time.Now().Add(2 * time.Second)
	var seen []Waiter
	for time.Now().Before(deadline) {
		if seen = m.Waiters(); len(seen) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(seen) != 1 || seen[0].PID != os.Getpid() || seen[0].Class != ClassText || seen[0].Reason != "arm B" {
		t.Fatalf("the queued acquire must be listed with its pid, class and reason: %+v", seen)
	}
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	if aerr := <-done; aerr != nil {
		t.Fatalf("the waiter must acquire once the holder releases: %v", aerr)
	}
	if ws := m.Waiters(); len(ws) != 0 {
		t.Fatalf("a waiter that returned must not stay listed: %+v", ws)
	}
}

// A waiter record whose process is gone is debris: pruned on read, never a
// reason to defer a warm forever.
func TestWaitersPrunesDeadAndMalformedRecords(t *testing.T) {
	m, _ := newTestManager(t)
	dir := m.waitersDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	dead, _ := json.Marshal(Waiter{PID: 2147483000, Class: ClassText, SinceMs: time.Now().UnixMilli()})
	if err := os.WriteFile(filepath.Join(dir, "2147483000.1.json"), dead, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	live, _ := json.Marshal(Waiter{PID: os.Getpid(), Class: ClassMedia, SinceMs: time.Now().UnixMilli()})
	if err := os.WriteFile(filepath.Join(dir, "live.json"), live, 0o666); err != nil {
		t.Fatal(err)
	}
	ws := m.Waiters()
	if len(ws) != 1 || ws[0].PID != os.Getpid() {
		t.Fatalf("only the live waiter survives a read: %+v", ws)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dead and malformed records must be removed, %d left", len(entries))
	}
}

// The warm-owed marker names the seat and survives until it is cleared.
func TestSeatWarmOwedMarkerRoundTrips(t *testing.T) {
	m, _ := newTestManager(t)
	if got := m.SeatWarmOwed(); got != "" {
		t.Fatalf("nothing is owed yet, got %q", got)
	}
	if err := m.MarkSeatWarmOwed(""); err == nil {
		t.Fatal("an empty seat name must be refused")
	}
	if err := m.MarkSeatWarmOwed("qwen38-27b-gsq-vllm"); err != nil {
		t.Fatal(err)
	}
	if got := m.SeatWarmOwed(); got != "qwen38-27b-gsq-vllm" {
		t.Fatalf("marker: %q", got)
	}
	m.ClearSeatWarmOwed()
	if got := m.SeatWarmOwed(); got != "" {
		t.Fatalf("cleared marker still reads %q", got)
	}
}

// ClearSeatWarmOwedIfSeat is the compare-and-delete the status verb clears a stale marker with:
// it removes the marker only while it still names the seat the caller observed, so a fresh
// marker a new lease wrote for another seat between the read and the clear survives.
func TestClearSeatWarmOwedIfSeat(t *testing.T) {
	m, _ := newTestManager(t)
	if m.ClearSeatWarmOwedIfSeat("seat") {
		t.Fatal("there is no marker, so nothing was cleared")
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	if m.ClearSeatWarmOwedIfSeat("other-seat") {
		t.Fatal("a marker for another seat must not be cleared")
	}
	if got := m.SeatWarmOwed(); got != "seat" {
		t.Fatalf("a refused clear left the marker %q, want it untouched", got)
	}
	if !m.ClearSeatWarmOwedIfSeat("SEAT") {
		t.Fatal("the seat name compares case-insensitively, as the warm path does")
	}
	if got := m.SeatWarmOwed(); got != "" {
		t.Fatalf("the cleared marker still reads %q", got)
	}
	if m.ClearSeatWarmOwedIfSeat("seat") {
		t.Fatal("a second clear finds nothing to remove")
	}
	// An empty marker reads as "no seat owed"; a blank seat name must not match it and delete it.
	if err := os.WriteFile(m.seatWarmOwedPath(), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if m.ClearSeatWarmOwedIfSeat("  ") || m.ClearSeatWarmOwedIfSeat("") {
		t.Fatal("a blank seat name clears nothing")
	}
	if _, err := os.Stat(m.seatWarmOwedPath()); err != nil {
		t.Fatalf("a refused clear left the empty marker file in place: %v", err)
	}
}

// The marker carries the time it was stamped, in whole seconds, and MarkSeatWarmOwedAt sets it.
func TestSeatWarmOwedKeepsItsStamp(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	seat, stamped := m.seatWarmOwedRecord()
	if seat != "seat" || !stamped.Equal(*now) {
		t.Fatalf("the marker records the clock's time: seat=%q stamped=%v want %v", seat, stamped, *now)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 987_000_000, time.UTC)
	if err := m.MarkSeatWarmOwedAt("seat", at); err != nil {
		t.Fatal(err)
	}
	if _, stamped = m.seatWarmOwedRecord(); !stamped.Equal(at.Truncate(time.Second)) {
		t.Fatalf("the stamp is kept in whole seconds: %v", stamped)
	}
}

// ClearSeatWarmOwedIfStale is the clear `gpu status` uses: it removes the marker only when it
// names the seat, is provably older than the readings the caller decided from, and no lease is
// live. Every leg is load-bearing, so each is pinned alone.
func TestClearSeatWarmOwedIfStale(t *testing.T) {
	began := time.Date(2026, 7, 25, 12, 0, 10, 500_000_000, time.UTC) // the readings began at :10.5
	stampedAt := func(t *testing.T, m *Manager, at time.Time) {
		t.Helper()
		if err := m.MarkSeatWarmOwedAt("seat", at); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("no marker", func(t *testing.T) {
		m, _ := newTestManager(t)
		if m.ClearSeatWarmOwedIfStale("seat", began) {
			t.Fatal("there is nothing to clear")
		}
	})
	t.Run("an older marker for the seat goes, compared case-insensitively", func(t *testing.T) {
		m, _ := newTestManager(t)
		stampedAt(t, m, began.Add(-time.Hour))
		if !m.ClearSeatWarmOwedIfStale("SEAT", began) || m.SeatWarmOwed() != "" {
			t.Fatalf("a marker from an hour before the readings is stale (owed=%q)", m.SeatWarmOwed())
		}
	})
	t.Run("another seat's marker stays", func(t *testing.T) {
		m, _ := newTestManager(t)
		stampedAt(t, m, began.Add(-time.Hour))
		if m.ClearSeatWarmOwedIfStale("other-seat", began) || m.SeatWarmOwed() != "seat" {
			t.Fatal("the marker names a different seat")
		}
	})
	t.Run("a blank seat clears nothing", func(t *testing.T) {
		m, _ := newTestManager(t)
		stampedAt(t, m, began.Add(-time.Hour))
		if m.ClearSeatWarmOwedIfStale("  ", began) || m.SeatWarmOwed() != "seat" {
			t.Fatal("a blank seat name matches nothing")
		}
	})
	// The stamp is whole seconds: a marker written at :09.8 reads :09 and one written at :10.2
	// reads :10. The first is older than :10.5 whatever its fraction; the second may or may not
	// be, and a marker that cannot be shown to be older stays.
	t.Run("the stamp's second must have ended before the readings began", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			at   time.Time
			want bool
		}{
			{"written at :09.8", began.Add(-700 * time.Millisecond), true},
			{"written at :10.2, before the readings but in their second", began.Add(-300 * time.Millisecond), false},
			{"written at :10.7, after the readings began", began.Add(200 * time.Millisecond), false},
			{"written at the instant the readings began", began, false},
		} {
			m, _ := newTestManager(t)
			stampedAt(t, m, tc.at)
			if got := m.ClearSeatWarmOwedIfStale("seat", began); got != tc.want {
				t.Errorf("%s: cleared=%v want %v", tc.name, got, tc.want)
			}
			if gone := m.SeatWarmOwed() == ""; gone != tc.want {
				t.Errorf("%s: marker gone=%v want %v", tc.name, gone, tc.want)
			}
		}
	})
	t.Run("a marker with no usable stamp stays", func(t *testing.T) {
		for _, body := range []string{"seat\n", "seat not-a-time\n"} {
			m, _ := newTestManager(t)
			if err := os.WriteFile(m.seatWarmOwedPath(), []byte(body), 0o666); err != nil {
				t.Fatal(err)
			}
			if m.ClearSeatWarmOwedIfStale("seat", began) || m.SeatWarmOwed() != "seat" {
				t.Errorf("%q cannot be shown to be older than the readings, so it stays", body)
			}
		}
	})
	t.Run("a live lease keeps it", func(t *testing.T) {
		m, _ := newTestManager(t)
		stampedAt(t, m, began.Add(-time.Hour))
		l, err := m.TryAcquire(ClassText, Options{Reason: "holder", TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if m.ClearSeatWarmOwedIfStale("seat", began) || m.SeatWarmOwed() != "seat" {
			t.Fatal("a lease is live: the marker is its debt")
		}
		if err := l.Release(); err != nil {
			t.Fatal(err)
		}
		if !m.ClearSeatWarmOwedIfStale("seat", began) {
			t.Fatal("once the card is free the same marker is stale")
		}
	})
}
