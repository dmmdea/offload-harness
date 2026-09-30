package seatrate

import (
	"path/filepath"
	"testing"
	"time"
)

// Only a solo run's prefill sample moves the seat's prefill rate (ADR 0066,
// register C-66). A sample timed while other requests shared the seat is what a
// shared seat gives one request: folded in, the published rate follows the
// traffic the run happened to meet (one fleet seat's prefill_tok_s swung 255-1321 tok/s in
// one day) and every later allowance, ceiling and ETA is sized from it.
func TestObservePrefillIgnoresConcurrentSamples(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	now := time.Now()
	if !s.ObservePrefillLoad("seat", 20000, 10000, 1, now) { // solo: 2,000 tok/s
		t.Fatal("a solo sample must be recorded")
	}
	if got := s.Get("seat").PrefillTokS; got != 2000 {
		t.Fatalf("PrefillTokS = %v, want 2000", got)
	}
	// The same prompt timed at load 3 took three times as long: 666 tok/s.
	if s.ObservePrefillLoad("seat", 20000, 30000, 3, now) {
		t.Fatal("a load-3 sample must not be recorded")
	}
	if got := s.Get("seat").PrefillTokS; got != 2000 {
		t.Fatalf("a load-3 sample moved PrefillTokS to %v, want it to stay 2000", got)
	}
	if s.ObservePrefillLoad("seat", 20000, 30000, 2, now) {
		t.Fatal("a load-2 sample must not be recorded")
	}
	// Load 0 is "never observed": nothing vouched that the seat was the run's own,
	// so it is refused like a shared one — the published rate is only ever a
	// KNOWN solo run's. (A run's load used to default to 1, which made a run that
	// could not look at all — no registry, no engine gauges — teach the store.)
	if s.ObservePrefillLoad("seat", 10000, 10000, 0, now) {
		t.Fatal("an unobserved load must not be recorded: unknown is not solo")
	}
	if s.ObservePrefillLoad("seat", 10000, 10000, -1, now) {
		t.Fatal("a nonsense load must not be recorded")
	}
	if got := s.Get("seat").PrefillTokS; got != 2000 {
		t.Fatalf("an unobserved sample moved PrefillTokS to %v, want it to stay 2000", got)
	}
	// A known solo run (load 1) folds in: 1,000 tok/s -> EWMA 0.3*1000 + 0.7*2000 = 1700.
	if !s.ObservePrefillLoad("seat", 10000, 10000, 1, now) {
		t.Fatal("a known solo sample must be recorded")
	}
	if got := s.Get("seat").PrefillTokS; got < 1699 || got > 1701 {
		t.Fatalf("PrefillTokS = %v, want the EWMA 1700", got)
	}
	// ObservePrefill is the solo call, unchanged.
	if !s.ObservePrefill("seat", 10000, 10000, now) {
		t.Fatal("ObservePrefill is the solo call")
	}
	// A shared seat with no rate yet stays unmeasured rather than learning a wrong one.
	fresh := &Store{Seats: map[string]Seat{}}
	if fresh.ObservePrefillLoad("other", 20000, 30000, 4, now) || fresh.Get("other").PrefillTokS != 0 {
		t.Fatalf("a concurrent first sample must leave the rate unknown, got %v", fresh.Get("other").PrefillTokS)
	}
}

// A load ends at one instant for every run waiting on it: two observations that
// end together are ONE load, and the store keeps the longest measurement of it —
// the run that began waiting first, closest to the load's real start.
func TestObserveColdLoadMergesOneLoadSeenByTwoRuns(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	end := time.Now()
	// The run that joined the load late saw 12.4 s of it...
	if !s.ObserveColdLoad("agent-pool", 12.4, end) {
		t.Fatal("first observation must be recorded")
	}
	// ...and the run that began waiting at the load's start saw all 234.6 s,
	// ending two seconds apart.
	if !s.ObserveColdLoad("agent-pool", 234.6, end.Add(2*time.Second)) {
		t.Fatal("a longer measurement of the same load must replace the shorter one")
	}
	got := s.Get("agent-pool")
	if len(got.ColdLoads) != 1 || got.ColdLoads[0] != 234.6 || got.ColdLoadSec != 234.6 {
		t.Fatalf("cold_loads=%v cold_load_sec=%v, want ONE entry of 234.6 (the shorter view of the same load must not be a second load)", got.ColdLoads, got.ColdLoadSec)
	}
	// A run whose view of that load was shorter, arriving after, changes nothing.
	if s.ObserveColdLoad("agent-pool", 200, end.Add(3*time.Second)) {
		t.Fatal("a shorter view of an already-measured load must not change the store")
	}
	if got := s.Get("agent-pool"); len(got.ColdLoads) != 1 || got.ColdLoads[0] != 234.6 {
		t.Fatalf("cold_loads = %v", got.ColdLoads)
	}
	// A genuinely later load is another entry.
	if !s.ObserveColdLoad("agent-pool", 203.5, end.Add(30*time.Minute)) {
		t.Fatal("a load 30 minutes later is a different load")
	}
	got = s.Get("agent-pool")
	if len(got.ColdLoads) != 2 || got.ColdLoads[1] != 203.5 || got.ColdLoadSec != 234.6 {
		t.Fatalf("cold_loads=%v cold_load_sec=%v, want [234.6 203.5] and the slowest kept", got.ColdLoads, got.ColdLoadSec)
	}
}

// The window still holds the last five loads and the wall is sized from the
// slowest of them.
func TestObserveColdLoadKeepsTheLastFiveLoads(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	base := time.Now()
	for i := 0; i < 7; i++ {
		if !s.ObserveColdLoad("seat", float64(100+i), base.Add(time.Duration(i)*time.Hour)) {
			t.Fatalf("load %d not recorded", i)
		}
	}
	got := s.Get("seat")
	if len(got.ColdLoads) != coldLoadWindow || got.ColdLoads[0] != 102 || got.ColdLoadSec != 106 {
		t.Fatalf("cold_loads=%v cold_load_sec=%v, want the last five (102..106) and 106 the slowest", got.ColdLoads, got.ColdLoadSec)
	}
}

// Garbage in, no change out — and the merge state survives the file.
func TestObserveColdLoadRejectsNonsenseAndRoundTrips(t *testing.T) {
	var nilStore *Store
	if nilStore.ObserveColdLoad("seat", 100, time.Now()) {
		t.Fatal("a nil store records nothing")
	}
	s := &Store{Seats: map[string]Seat{}, path: filepath.Join(t.TempDir(), FileName)}
	if s.ObserveColdLoad("", 100, time.Now()) || s.ObserveColdLoad("seat", 0, time.Now()) || s.ObserveColdLoad("seat", -5, time.Now()) {
		t.Fatal("an empty seat name or a non-positive load must not be recorded")
	}
	end := time.Now().Truncate(time.Second)
	s.ObserveColdLoad("seat", 180, end)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(s.path)
	if err != nil {
		t.Fatal(err)
	}
	// After a reload, a second observer of the same load is still merged.
	if !back.ObserveColdLoad("seat", 240, end.Add(time.Second)) {
		t.Fatal("the reloaded store must still merge the same load")
	}
	if got := back.Get("seat"); len(got.ColdLoads) != 1 || got.ColdLoads[0] != 240 {
		t.Fatalf("cold_loads = %v, want the merge to survive a save and load", got.ColdLoads)
	}
}
