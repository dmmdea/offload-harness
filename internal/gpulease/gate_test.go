package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gate.go: a reader that meets a lease file whose live name is absent with a fresh staged file beside it waits for the rename to end.
// Windows only. The tests run the gate on every platform (waitOutDeniedOpens) on a clock that only a pause moves, and make the rename
// end at a chosen open, so the counts are exact and nothing sleeps.

// gateClock is the gate's clock for a test: it moves when the gate pauses and not otherwise.
type gateClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *gateClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *gateClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// onGateClock switches the gate on whatever the build, runs it on a clock the test controls, and zeroes the counters.
func onGateClock(t *testing.T) *gateClock {
	t.Helper()
	waitOutDeniedOpens(t)
	c := &gateClock{t: time.Now()}
	oldNow, oldSleep := gateNow, gateSleep
	gateNow, gateSleep = c.now, c.sleep
	gatePauses.Store(0)
	gateExpiries.Store(0)
	readRetries.Store(0)
	t.Cleanup(func() { gateNow, gateSleep = oldNow, oldSleep })
	return c
}

// stageBeside writes the staged file a writer leaves beside path, dated age before the gate's clock, and returns its name.
func stageBeside(t *testing.T, c *gateClock, path string, age time.Duration) string {
	t.Helper()
	staged := stagingPath(path)
	if staged == "" {
		t.Fatalf("%s has no staged name", path)
	}
	if err := os.MkdirAll(filepath.Dir(staged), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("42"), 0o666); err != nil {
		t.Fatal(err)
	}
	at := c.now().Add(-age)
	if err := os.Chtimes(staged, at, at); err != nil {
		t.Fatal(err)
	}
	return staged
}

// endRenameAtOpen makes the staged file take the live name just before the nth open of path: the rename ends. It returns the count of
// opens of path.
func endRenameAtOpen(t *testing.T, path string, n int) *atomic.Int64 {
	t.Helper()
	staged := stagingPath(path)
	var opens atomic.Int64
	old := openSharedFile
	openSharedFile = func(p string) (*os.File, error) {
		if p == path && int(opens.Add(1)) == n {
			if err := os.Rename(staged, path); err != nil {
				t.Errorf("ending the rename: %v", err)
			}
		}
		return old(p)
	}
	t.Cleanup(func() { openSharedFile = old })
	return &opens
}

// statLog records the stats the gate makes through statFile.
type statLog struct {
	mu    sync.Mutex
	paths []string
}

func countStats(t *testing.T) *statLog {
	t.Helper()
	l := &statLog{}
	old := statFile
	statFile = func(p string) (os.FileInfo, error) {
		l.mu.Lock()
		l.paths = append(l.paths, p)
		l.mu.Unlock()
		return old(p)
	}
	t.Cleanup(func() { statFile = old })
	return l
}

func (l *statLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.paths)
}

func (l *statLog) of(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range l.paths {
		if p == path {
			n++
		}
	}
	return n
}

func TestAReadOfAnAbsentNameWithAFreshStagedFileWaitsForTheRenameToEnd(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	stageBeside(t, c, path, 0)
	opens := endRenameAtOpen(t, path, 4)
	b, err := readSharedFile(path)
	if err != nil || string(b) != "42" {
		t.Fatalf("readSharedFile = %q, %v while the rename of the heartbeat was in flight, want the new content 42", b, err)
	}
	if got := opens.Load(); got != 4 {
		t.Errorf("the name was opened %d times, want 4 (three that met the rename, one after it)", got)
	}
	if got := gatePauses.Load(); got != 3 {
		t.Errorf("the gate paused %d times, want 3 (one between each pair of opens)", got)
	}
	if got := readRetries.Load(); got != 0 {
		t.Errorf("the wait for a refused open paused %d times for a name that was absent, want 0", got)
	}
}

func TestTheWaitCoversTheWholeNodeRecordThroughItsRestampFileAndOnlyThat(t *testing.T) {
	c := onGateClock(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.json")
	stageBeside(t, c, path, 0)
	endRenameAtOpen(t, path, 3)
	if b, err := readSharedFile(path); err != nil || string(b) != "42" {
		t.Fatalf("readSharedFile(meta.json) = %q, %v while meta.json.restamp stood beside it, want the new content 42", b, err)
	}
	// A .tmp beside meta.json is not its staged name (the epoch counter's is): nothing is in flight, so no wait.
	other := t.TempDir()
	gatePauses.Store(0)
	if err := os.WriteFile(filepath.Join(other, "meta.json.tmp"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readSharedFile(filepath.Join(other, "meta.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readSharedFile(meta.json) with only a .tmp beside it = %v, want a not-exist error", err)
	}
	if got := gatePauses.Load(); got != 0 {
		t.Errorf("the gate paused %d times for a .tmp beside meta.json, want 0", got)
	}
}

func TestARenameThatNeverEndsIsGivenUpOnAtTheBudgetAsAnIOFailureNotAnAbsence(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "e", "7.json")
	stageBeside(t, c, path, 0)
	opens := endRenameAtOpen(t, path, 1<<30)
	_, err := readSharedFile(path)
	if err == nil {
		t.Fatal("readSharedFile returned no error for a name that never came back")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readSharedFile = %v: the wait ran out and the reader called the file gone; Check would then call the lease gone", err)
	}
	if !errors.Is(err, errRenameInFlight) || !isIOFailure(err) {
		t.Fatalf("readSharedFile = %v, want an I/O-class errRenameInFlight", err)
	}
	want := int(absentBudget / absentPause)
	if got := int(gatePauses.Load()); got != want {
		t.Errorf("the gate paused %d times before giving up, want %d (the budget %v at %v a pause)", got, want, absentBudget, absentPause)
	}
	if got := opens.Load(); got != int64(want+1) {
		t.Errorf("the name was opened %d times, want %d", got, want+1)
	}
	if got := gateExpiries.Load(); got != 1 {
		t.Errorf("gateExpiries = %d, want 1", got)
	}
}

// The rename can end between the open that met nothing and the look at the staged file: the staged file is then gone too, and a
// reader that stopped there would answer "absent" for a name that is back.
func TestAReadThatMeetsTheRenameEndingBetweenTheLooksReadsTheFile(t *testing.T) {
	onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	if err := os.WriteFile(path, []byte("42"), 0o666); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int64
	old := openSharedFile
	openSharedFile = func(p string) (*os.File, error) {
		if opens.Add(1) == 1 {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist} // the name was not there yet
		}
		return old(p)
	}
	t.Cleanup(func() { openSharedFile = old })
	b, err := readSharedFile(path)
	if err != nil || string(b) != "42" {
		t.Fatalf("readSharedFile = %q, %v, want the content of the file that is there on the second look", b, err)
	}
	if got := opens.Load(); got != 2 {
		t.Errorf("the name was opened %d times, want 2", got)
	}
	if got := gatePauses.Load(); got != 0 {
		t.Errorf("the gate paused %d times for a name that was already back, want 0", got)
	}
}

func TestAMissingNameWithNoFreshStagedFileCostsOneOpenAndNoPause(t *testing.T) {
	for _, c := range []struct {
		name string
		prep func(t *testing.T, clk *gateClock, path string)
	}{
		{"nothing beside it", func(t *testing.T, clk *gateClock, path string) {}},
		{"a staged file older than the freshness bound (a writer that died)", func(t *testing.T, clk *gateClock, path string) {
			stageBeside(t, clk, path, stagingFresh+2*time.Second)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := onGateClock(t)
			path := filepath.Join(t.TempDir(), "hb.7")
			c.prep(t, clk, path)
			log := recordOpens(t, "", nil)
			_, err := readSharedFile(path)
			if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errRenameInFlight) {
				t.Fatalf("readSharedFile = %v, want a plain not-exist error", err)
			}
			if got := log.total(); got != 1 {
				t.Errorf("the missing name was opened %d times, want 1", got)
			}
			if got := gatePauses.Load(); got != 0 {
				t.Errorf("the gate paused %d times for a name nothing is replacing, want 0", got)
			}
		})
	}
}

// A card claim is created once and removed, never renamed over: nothing is in flight beside it, whatever stands there.
func TestAClaimIsNeverWaitedForAndCostsNoStat(t *testing.T) {
	clk := onGateClock(t)
	path := filepath.Join(t.TempDir(), "cards", "gpu-test-0.claim")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", []byte("1"), 0o666); err != nil {
		t.Fatal(err)
	}
	at := clk.now()
	_ = os.Chtimes(path+".tmp", at, at)
	stats := countStats(t)
	log := recordOpens(t, "", nil)
	if _, err := readSharedFile(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readSharedFile(claim) = %v, want a not-exist error", err)
	}
	if got := log.total(); got != 1 {
		t.Errorf("the claim was opened %d times, want 1", got)
	}
	if got := stats.total(); got != 0 {
		t.Errorf("the missing claim cost %d stats, want 0", got)
	}
	if got := gatePauses.Load(); got != 0 {
		t.Errorf("the gate paused %d times for a claim, want 0", got)
	}
}

// Off Windows (the gate is off) a missing name is answered at once, with no stat and no pause: rename(2) is atomic, nothing is in flight.
func TestWithTheGateOffAMissingNameIsAnsweredAtOnce(t *testing.T) {
	clk := onGateClock(t)
	deniedOpenMayClear = false // the switch the gate shares with the wait for a refused open; onGateClock restored it for the cleanup
	path := filepath.Join(t.TempDir(), "hb.7")
	stageBeside(t, clk, path, 0)
	stats := countStats(t)
	log := recordOpens(t, "", nil)
	if _, err := readSharedFile(path); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errRenameInFlight) {
		t.Fatalf("readSharedFile = %v with the gate off, want a plain not-exist error", err)
	}
	if got := log.total(); got != 1 {
		t.Errorf("the name was opened %d times, want 1", got)
	}
	if got := stats.total(); got != 0 {
		t.Errorf("a read with the gate off cost %d stats, want 0", got)
	}
	if got := gatePauses.Load(); got != 0 {
		t.Errorf("a read with the gate off paused %d times, want 0", got)
	}
}

// A name the look finds but the open does not (the open keeps answering "not there" for a file that is) is read again only until the
// same deadline: it ends as an I/O failure and not as a spin, and not as an absence for a file that is there.
func TestANameTheLookFindsButTheOpenDoesNotIsReadAgainOnlyUntilTheDeadline(t *testing.T) {
	onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	if err := os.WriteFile(path, []byte("42"), 0o666); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int64
	old := openSharedFile
	openSharedFile = func(p string) (*os.File, error) {
		if opens.Add(1) > 1000 {
			return nil, errors.New("runaway: the read is still looping") // ends a loop with no deadline as a failure and not as a hang
		}
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	t.Cleanup(func() { openSharedFile = old })
	_, err := readSharedFile(path)
	if !errors.Is(err, errRenameInFlight) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readSharedFile = %v, want an I/O-class errRenameInFlight after the deadline (opens: %d)", err, opens.Load())
	}
	if got, want := gatePauses.Load(), int64(absentBudget/absentPause); got != want {
		t.Errorf("the read paused %d times, want %d (the first look is free, the rest are paced to the deadline)", got, want)
	}
	if got := gateExpiries.Load(); got != 1 {
		t.Errorf("gateExpiries = %d, want 1", got)
	}
}
