package seatrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The cold-load window (ADR 0066, register C-66): one load seen by several runs is
// ONE entry, however late each run reports, and two real loads are never one.

// The flagship died ten times on 2026-09-29 with a median up-time of 6.7 minutes, so
// two real loads end minutes apart — never seconds. The merge window is ten seconds:
// a wider one would swallow the second load into the first.
func TestLoadsSevenMinutesApartAreTwoLoads(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	t0 := time.Now()
	s.ObserveColdLoad("flagship", 234.6, t0)
	s.ObserveColdLoad("flagship", 203.5, t0.Add(7*time.Minute))
	if got := s.Get("flagship"); len(got.ColdLoads) != 2 || got.ColdLoads[0] != 234.6 || got.ColdLoads[1] != 203.5 {
		t.Fatalf("cold_loads = %v, want [234.6 203.5]: a load that ended 7 minutes after another is not the same load", got.ColdLoads)
	}
	// The edge of the window itself: 10 s is the same load, 11 s is not.
	edge := &Store{Seats: map[string]Seat{}}
	edge.ObserveColdLoad("seat", 100, t0)
	edge.ObserveColdLoad("seat", 90, t0.Add(coldLoadMergeWindow)) // a shorter view, exactly at the window: merged
	edge.ObserveColdLoad("seat", 80, t0.Add(coldLoadMergeWindow+2*time.Second+time.Nanosecond))
	if got := edge.Get("seat"); len(got.ColdLoads) != 2 {
		t.Fatalf("cold_loads = %v, want the observation past the window to be its own load", got.ColdLoads)
	}
}

// A run that reports late ended EARLIER than the newest entry. Only an observation
// that ends within the window of an entry — on either side — is a view of that load.
func TestAnObservationThatEndedAnHourBeforeTheNewestIsNotMergedIntoIt(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	t0 := time.Now()
	s.ObserveColdLoad("seat", 234.6, t0)
	s.ObserveColdLoad("seat", 178, t0.Add(-time.Hour)) // an older load, reported late
	got := s.Get("seat")
	if len(got.ColdLoads) != 2 || got.ColdLoads[0] != 178 || got.ColdLoads[1] != 234.6 {
		t.Fatalf("cold_loads = %v, want [178 234.6] in the order the loads happened", got.ColdLoads)
	}
	// ...while one that ended 5 s before an entry IS the same load.
	s2 := &Store{Seats: map[string]Seat{}}
	s2.ObserveColdLoad("seat", 234.6, t0)
	s2.ObserveColdLoad("seat", 12.4, t0.Add(-5*time.Second))
	if got := s2.Get("seat"); len(got.ColdLoads) != 1 || got.ColdLoads[0] != 234.6 {
		t.Fatalf("cold_loads = %v: an observer that ended 5 s earlier saw the same load", got.ColdLoads)
	}
}

// The newest end anchors the merge for a later observer of the SECOND load, however
// many loads came before it, and an earlier-ending observer never drags the anchor
// backwards.
func TestALateObserverOfTheSecondLoadMergesIntoIt(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	t0 := time.Now()
	s.ObserveColdLoad("seat", 200, t0)
	s.ObserveColdLoad("seat", 250, t0.Add(30*time.Minute))               // a second load
	s.ObserveColdLoad("seat", 240, t0.Add(30*time.Minute+2*time.Second)) // another run's shorter view of it
	if got := s.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoads[1] != 250 {
		t.Fatalf("cold_loads = %v, want [200 250]: the second load's late observer must merge into it", got.ColdLoads)
	}
	s2 := &Store{Seats: map[string]Seat{}}
	end := t0.Add(time.Hour)
	s2.ObserveColdLoad("seat", 250, end)
	s2.ObserveColdLoad("seat", 300, end.Add(-5*time.Second)) // began waiting earlier, so it measured more: it replaces the entry
	s2.ObserveColdLoad("seat", 90, end.Add(8*time.Second))   // ends 8 s after the newest end: the same load
	if got := s2.Get("seat"); len(got.ColdLoads) != 1 || got.ColdLoads[0] != 300 {
		t.Fatalf("cold_loads = %v, want ONE entry of 300: the merge point moved backwards to the earlier observer", got.ColdLoads)
	}
}

// Runs report when THEY end, not when the load did: with a flagship that died every
// ~7 minutes and runs of 10+ minutes, out-of-order completion is the normal case. A
// run that waited on an OLDER load and finishes after a newer one was recorded must
// find its own load, or the window fills with copies and short tails and the real
// measurements are pushed out (seat-rates.json: [200 210 2], then [178 200 3 3 3]).
func TestALateReporterOfAnOlderLoadMergesIntoItsOwnLoad(t *testing.T) {
	t0 := time.Now()
	s := &Store{Seats: map[string]Seat{}}
	s.ObserveColdLoad("seat", 200, t0)
	s.ObserveColdLoad("seat", 210, t0.Add(10*time.Minute)) // another load after an engine death
	if s.ObserveColdLoad("seat", 2, t0.Add(time.Second)) { // a long run that also joined the first load, finishing late
		t.Fatal("a short view of the first load changed the store")
	}
	if got := s.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoads[0] != 200 || got.ColdLoads[1] != 210 {
		t.Fatalf("cold_loads = %v, want [200 210]: the late reporter's tail is the FIRST load, not a third", got.ColdLoads)
	}
	// A late reporter that measured MORE of the older load replaces it in place.
	if !s.ObserveColdLoad("seat", 250, t0.Add(2*time.Second)) {
		t.Fatal("a longer view of the first load must replace its measurement")
	}
	if got := s.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoads[0] != 250 || got.ColdLoads[1] != 210 || got.ColdLoadSec != 250 {
		t.Fatalf("cold_loads = %v cold_load_sec = %v, want [250 210] and the slowest kept", got.ColdLoads, got.ColdLoadSec)
	}
	// Tails of five different loads, reported late, never displace the real ones.
	tails := &Store{Seats: map[string]Seat{}}
	tails.ObserveColdLoad("seat", 178, t0)
	tails.ObserveColdLoad("seat", 271, t0.Add(20*time.Minute))
	for i := 0; i < 5; i++ {
		tails.ObserveColdLoad("seat", 3, t0.Add(time.Duration(i%2*20)*time.Minute+time.Second)) // joiners of those two loads
	}
	if got := tails.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoadSec != 271 {
		t.Fatalf("cold_loads = %v cold_load_sec = %v, want the two real loads and 271 the slowest", got.ColdLoads, got.ColdLoadSec)
	}
}

// An observation that sits inside the merge window of TWO entries (whose ends are 15 s
// apart, so they were recorded as two loads) is a view of the NEAREST one.
func TestAnObservationBetweenTwoEntriesMergesIntoTheNearest(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	t0 := time.Now()
	s.ObserveColdLoad("seat", 100, t0)
	s.ObserveColdLoad("seat", 200, t0.Add(15*time.Second)) // 15 s later: another load
	if !s.ObserveColdLoad("seat", 300, t0.Add(8*time.Second)) {
		t.Fatal("a longer view of the nearer load must replace it")
	}
	if got := s.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoads[0] != 100 || got.ColdLoads[1] != 300 {
		t.Fatalf("cold_loads = %v, want [100 300]: 8 s from the first end and 7 s from the second, so the second", got.ColdLoads)
	}
}

// The merge anchor follows a longer, later view: an entry's end moves to the latest end
// that replaced its measurement, so the run that finishes a few seconds after THAT still
// finds the load.
func TestTheMergeAnchorFollowsALongerLaterView(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	t0 := time.Now()
	s.ObserveColdLoad("seat", 100, t0)
	s.ObserveColdLoad("seat", 150, t0.Add(8*time.Second)) // longer, and ending later: the entry's end is now t0+8s
	if s.ObserveColdLoad("seat", 90, t0.Add(16*time.Second)) {
		t.Fatal("a shorter view ending 8 s after the entry's latest end changed the store")
	}
	if got := s.Get("seat"); len(got.ColdLoads) != 1 || got.ColdLoads[0] != 150 {
		t.Fatalf("cold_loads = %v, want ONE entry of 150: the anchor did not follow the later end", got.ColdLoads)
	}
}

// A load older than everything in a full window has already left it: reporting it
// late must not push a newer load out.
func TestAnObservationOlderThanAFullWindowIsDropped(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	base := time.Now()
	for i := 0; i < coldLoadWindow; i++ {
		s.ObserveColdLoad("seat", float64(100+i), base.Add(time.Duration(i)*time.Hour))
	}
	if s.ObserveColdLoad("seat", 500, base.Add(-2*time.Hour)) {
		t.Fatal("a load older than a full window was recorded")
	}
	if got := s.Get("seat"); len(got.ColdLoads) != coldLoadWindow || got.ColdLoads[0] != 100 || got.ColdLoadSec != 104 {
		t.Fatalf("cold_loads = %v cold_load_sec = %v: the late report disturbed the window", got.ColdLoads, got.ColdLoadSec)
	}
	// One inside the window's span goes in its place in end order and the OLDEST leaves.
	if !s.ObserveColdLoad("seat", 300, base.Add(90*time.Minute)) {
		t.Fatal("a load inside the window's span was not recorded")
	}
	got := s.Get("seat")
	want := []float64{101, 300, 102, 103, 104}
	if len(got.ColdLoads) != len(want) {
		t.Fatalf("cold_loads = %v, want %v", got.ColdLoads, want)
	}
	for i := range want {
		if got.ColdLoads[i] != want[i] {
			t.Fatalf("cold_loads = %v, want %v (end order, the oldest forgotten)", got.ColdLoads, want)
		}
	}
	if got.ColdLoadSec != 300 {
		t.Fatalf("cold_load_sec = %v, want 300", got.ColdLoadSec)
	}
}

// An observation with no end time cannot merge: it is appended as the newest and
// stays its own entry.
func TestAnObservationWithNoEndTimeNeverMerges(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	s.ObserveColdLoad("seat", 100, time.Time{})
	s.ObserveColdLoad("seat", 90, time.Time{})
	s.ObserveColdLoad("seat", 80, time.Now())
	if got := s.Get("seat"); len(got.ColdLoads) != 3 {
		t.Fatalf("cold_loads = %v, want three entries: an unknown end merges with nothing", got.ColdLoads)
	}
	// ...and it is the NEWEST entry, after loads of known end.
	known := &Store{Seats: map[string]Seat{}}
	known.ObserveColdLoad("seat", 100, time.Now())
	known.ObserveColdLoad("seat", 90, time.Time{})
	if got := known.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoads[0] != 100 || got.ColdLoads[1] != 90 {
		t.Fatalf("cold_loads = %v, want [100 90]: an observation with no end time is appended as the newest", got.ColdLoads)
	}
}

// Store's zero value is documented as empty and usable.
func TestZeroValueStoreTakesAColdLoad(t *testing.T) {
	var s Store
	if !s.ObserveColdLoad("seat", 100, time.Now()) {
		t.Fatal("a zero-value Store must record a cold load")
	}
	if got := s.Get("seat"); len(got.ColdLoads) != 1 {
		t.Fatalf("cold_loads = %v", got.ColdLoads)
	}
}

// A merge that measures no more than the entry changes nothing, never writes
// through a snapshot a caller still holds, and stamps the seat's update time.
func TestColdLoadMergeDetails(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	t0 := time.Now()
	s.ObserveColdLoad("seat", 100, t0)
	if s.ObserveColdLoad("seat", 100, t0.Add(time.Second)) {
		t.Fatal("an equal measurement of the same load changed the store")
	}
	snap := s.Get("seat") // a caller's snapshot shares the slices' backing arrays
	if !s.ObserveColdLoad("seat", 150, t0.Add(2*time.Second)) {
		t.Fatal("a longer measurement must replace the shorter one")
	}
	if snap.ColdLoads[0] != 100 || !snap.ColdLoadEnds[0].Equal(t0) {
		t.Fatalf("the merge wrote through a snapshot the caller held: %v %v", snap.ColdLoads, snap.ColdLoadEnds)
	}
	if got := s.Get("seat"); !got.Updated.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("Updated = %v, want the end of the newest observation", got.Updated)
	}
	// A late report of an older load must not move Updated backwards.
	s.ObserveColdLoad("seat", 7, t0.Add(-time.Hour))
	if got := s.Get("seat"); !got.Updated.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("Updated = %v after a late report of an older load, want it unchanged", got.Updated)
	}
}

// The ends are part of the file: a reloaded store still merges a second observer of
// the same load, and a file written before the ends were kept still loads (its
// entries have no end and merge with nothing; new observations carry theirs).
func TestColdLoadEndsSurviveTheFileAndOldFilesStillLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := &Store{Seats: map[string]Seat{}, path: path}
	end := time.Now().Truncate(time.Second)
	s.ObserveColdLoad("seat", 180, end)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.ObserveColdLoad("seat", 240, end.Add(time.Second)) {
		t.Fatal("the reloaded store must still merge the same load")
	}
	if got := back.Get("seat"); len(got.ColdLoads) != 1 || got.ColdLoads[0] != 240 || len(got.ColdLoadEnds) != 1 {
		t.Fatalf("cold_loads = %v ends = %v, want the merge to survive a save and load", got.ColdLoads, got.ColdLoadEnds)
	}

	legacy := filepath.Join(dir, "legacy.json")
	old := map[string]any{"seats": map[string]any{"seat": map[string]any{
		"tok_s": 30, "cold_load_sec": 234.6, "cold_loads": []float64{12.4, 234.6}, "samples": 3, "updated": end,
	}}}
	raw, _ := json.Marshal(old)
	if err := os.WriteFile(legacy, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := Load(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := ls.Get("seat"); len(got.ColdLoads) != 2 || got.ColdLoadSec != 234.6 {
		t.Fatalf("a file without ends did not load: %+v", got)
	}
	if !ls.ObserveColdLoad("seat", 199, end) {
		t.Fatal("a new observation on a legacy window was not recorded")
	}
	got := ls.Get("seat")
	if len(got.ColdLoads) != 3 || got.ColdLoads[2] != 199 || len(got.ColdLoadEnds) != 3 || !got.ColdLoadEnds[2].Equal(end) || got.ColdLoadSec != 234.6 {
		t.Fatalf("cold_loads = %v ends = %v: the new entry must carry its end beside the two of unknown end", got.ColdLoads, got.ColdLoadEnds)
	}
}

// The older path (Observe, for a caller that only knows the wall it waited) keeps the
// ends aligned with an unknown end, and the window still trims to five.
func TestObserveKeepsTheColdLoadEndsAligned(t *testing.T) {
	s := &Store{Seats: map[string]Seat{}}
	now := time.Now()
	for i := 0; i < coldLoadWindow+2; i++ {
		s.Observe("seat", 0, float64(50+i), now)
		if got := s.Get("seat"); len(got.ColdLoadEnds) != len(got.ColdLoads) {
			t.Fatalf("after %d observation(s): %d loads but %d ends: the ends must stay aligned", i+1, len(got.ColdLoads), len(got.ColdLoadEnds))
		}
	}
	got := s.Get("seat")
	if len(got.ColdLoads) != coldLoadWindow || len(got.ColdLoadEnds) != coldLoadWindow || got.ColdLoads[0] != 52 || got.ColdLoadSec != 56 {
		t.Fatalf("cold_loads = %v ends = %v cold_load_sec = %v", got.ColdLoads, got.ColdLoadEnds, got.ColdLoadSec)
	}
	for i, e := range got.ColdLoadEnds {
		if !e.IsZero() {
			t.Fatalf("end %d = %v: a caller that cannot say when the load ended records no end", i, e)
		}
	}
}

// A window whose ends do not line up with its loads (a hand-edited file, a writer
// that lost one) has NO known ends: attributing the ends it has to the wrong
// entries would merge a new load into a stranger.
func TestAMisalignedWindowHasNoKnownEnds(t *testing.T) {
	t0 := time.Now()
	s := &Store{Seats: map[string]Seat{"seat": {
		ColdLoads:    []float64{12.4, 234.6},
		ColdLoadEnds: []time.Time{t0}, // one end for two loads
		ColdLoadSec:  234.6,
	}}}
	if !s.ObserveColdLoad("seat", 50, t0) {
		t.Fatal("an observation on a misaligned window was not recorded")
	}
	got := s.Get("seat")
	if len(got.ColdLoads) != 3 || got.ColdLoads[0] != 12.4 || got.ColdLoads[1] != 234.6 || got.ColdLoads[2] != 50 {
		t.Fatalf("cold_loads = %v, want the observation as its own entry: the lone end must not be attributed to the first load", got.ColdLoads)
	}
	if len(got.ColdLoadEnds) != 3 {
		t.Fatalf("ends = %v, want them aligned again", got.ColdLoadEnds)
	}
}
