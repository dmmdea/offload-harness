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
