package gpuactivity

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Register C-82. Begin named a run "<pid>-<UnixNano>". On a coarse clock (Windows reads the clock in
// 0.5-1 ms steps) two runs begun in the same tick of one process got the SAME id, so the second record
// overwrote the first and the registry undercounted the runs on a seat. That count is what the local run cap
// (modelaffinity.AwaitSeatSlot) and the seat-load figure read, and it is why the delegate spread deal's
// "counts what is already registered on the local seat" test failed on a third of its runs on Windows.
//
// The clock is pinned to ONE instant here, which is the worst case a coarse clock produces: every Begin of
// the tick reads the same value.

// fixedClock registers every Begin of reg at one instant and returns it, so a List at that instant sees
// each record fresh (a record stamped in the past would read as stale and be swept).
func fixedClock(reg *Registry) time.Time {
	fixed := time.Now()
	reg.now = func() time.Time { return fixed }
	return fixed
}

// Two runs begun at the same instant are two runs: two ids, two records on disk, two runs listed.
func TestTwoRunsBegunAtTheSameInstantGetDistinctIDsAndRecords(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	fixed := fixedClock(reg)
	a, err := reg.Begin(Run{Seat: "seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.End()
	b, err := reg.Begin(Run{Seat: "seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.End()
	if a.ID() == b.ID() {
		t.Fatalf("two runs begun at the same instant share the id %q: the second record overwrites the first", a.ID())
	}
	files, err := filepath.Glob(filepath.Join(reg.Dir(), "run-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("%d record file(s) on disk for two runs: %v", len(files), files)
	}
	if got := reg.OnSeat(fixed, "seat"); len(got) != 2 {
		t.Fatalf("the registry lists %d run(s) on the seat, want the 2 that were begun: %+v", len(got), got)
	}
}

// A burst of runs begun in one tick, from several goroutines (a spread deal launching its legs at once),
// is all registered: the sequence that keeps ids apart is process-wide and safe to take concurrently.
func TestManyRunsBegunConcurrentlyInOneTickAreAllRegistered(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	fixed := fixedClock(reg)
	const n = 48
	handles := make([]*Handle, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			handles[i], errs[i] = reg.Begin(Run{Seat: "seat", Kind: "contract"})
		}(i)
	}
	wg.Wait()
	ids := map[string]bool{}
	for i, h := range handles {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		defer h.End()
		ids[h.ID()] = true
	}
	if len(ids) != n {
		t.Fatalf("%d runs begun in one tick got %d distinct ids", n, len(ids))
	}
	files, err := filepath.Glob(filepath.Join(reg.Dir(), "run-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != n {
		t.Fatalf("%d record files on disk for %d runs", len(files), n)
	}
	if got := reg.OnSeat(fixed, "seat"); len(got) != n {
		t.Fatalf("the registry lists %d of the %d runs begun", len(got), n)
	}
}

// The id stays readable: "<pid>-<unix nanos>-<seq>", the record "run-<id>.json" beside it. Nothing parses
// the id, but a person reading the registry directory (or a `gpu status` line) has to be able to tell whose
// record a file is and when it began.
func TestRunIDKeepsItsReadableShapeAndNamesItsRecord(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	fixed := fixedClock(reg)
	h, err := reg.Begin(Run{Seat: "seat", Kind: "agent_run"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	parts := strings.Split(h.ID(), "-")
	if len(parts) != 3 {
		t.Fatalf("id %q is not <pid>-<unix nanos>-<seq>", h.ID())
	}
	if parts[0] != strconv.Itoa(os.Getpid()) || parts[1] != strconv.FormatInt(fixed.UnixNano(), 10) {
		t.Fatalf("id %q does not lead with this process's pid and the begin time (%d, %d)", h.ID(), os.Getpid(), fixed.UnixNano())
	}
	if seq, err := strconv.ParseInt(parts[2], 10, 64); err != nil || seq < 1 {
		t.Fatalf("id %q has no positive sequence: %q", h.ID(), parts[2])
	}
	if _, err := os.Stat(filepath.Join(reg.Dir(), "run-"+h.ID()+".json")); err != nil {
		t.Fatalf("the record is not named run-<id>.json: %v", err)
	}
	if got := reg.List(fixed); len(got) != 1 || got[0].ID != h.ID() {
		t.Fatalf("the listed record's id is %+v, want %q", got, h.ID())
	}
}

// modelaffinity.AwaitSeatSlot breaks a same-millisecond tie in the seat's line with `r.ID < selfID`, a STRING
// comparison. Runs of one process begun in the same tick differ only in the sequence, so the sequence has to
// sort as text in the order it counts, or a run begun later would line up ahead of one begun earlier whenever
// the count crosses a power of ten (unpadded, "10" sorts before "9").
func TestRunIDsOfOneTickSortInTheOrderTheyWereBegun(t *testing.T) {
	now := time.Unix(0, 1_790_000_000_000_000_000)
	prev := ""
	for seq := int64(1); seq <= 1200; seq++ {
		id := runID(4242, now, seq)
		if prev != "" && id <= prev {
			t.Fatalf("id %q (sequence %d) does not sort after %q (sequence %d): the seat's line would invert", id, seq, prev, seq-1)
		}
		prev = id
	}
	if got, want := runID(4242, now, 7), "4242-1790000000000000000-000007"; got != want {
		t.Fatalf("runID = %q, want %q", got, want)
	}
}
