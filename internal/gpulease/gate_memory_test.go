package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The wait for a rename that never ends (a staged file left by a writer that died a moment ago) must be paid once for a file, not once
// per read of it. A grant reads each record about five times and each heartbeat about seven inside one hold of the epoch lock, so a
// wait of 250 ms per read would hold the lock for 1.75 s for one such heartbeat, and a second acquirer's spin allows 2 s. The memory of
// a wait that ran out is what bounds it (gate.go): a read of the same name in the next few seconds, with the staged file still fresh,
// answers at once with the same I/O failure.

// stuckBeside is a staged file that stays fresh for the whole test: dated far in the future, which counts as fresh.
func stuckBeside(t *testing.T, c *gateClock, path string) {
	t.Helper()
	stageBeside(t, c, path, -time.Hour)
}

func TestAStuckStagedFileIsWaitedOutOncePerFileAndNotOncePerRead(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	stuckBeside(t, c, path)
	log := recordOpens(t, "", nil)
	const reads = 7
	for i := 1; i <= reads; i++ {
		if _, err := readSharedFile(path); !errors.Is(err, errRenameInFlight) || !isIOFailure(err) {
			t.Fatalf("read %d = %v, want an I/O-class errRenameInFlight", i, err)
		}
	}
	want := int64(absentBudget / absentPause)
	if got := gatePauses.Load(); got != want {
		t.Errorf("%d reads of one name with a stuck staged file paused %d times, want %d (one budget, not one per read)", reads, got, want)
	}
	if got := gateExpiries.Load(); got != 1 {
		t.Errorf("gateExpiries = %d after %d reads, want 1", got, reads)
	}
	if got := log.total(); got != int(want)+reads {
		t.Errorf("%d reads made %d opens, want %d (the first read's %d plus one for each of the other %d)", reads, got, int(want)+reads, int(want)+1, reads-1)
	}
}

func TestTheMemoryOfAnExpiredWaitEndsAfterItsTimeAndWhenTheFileComesBack(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	stuckBeside(t, c, path)
	budgetPauses := int64(absentBudget / absentPause)

	_, _ = readSharedFile(path)
	if got := gatePauses.Load(); got != budgetPauses {
		t.Fatalf("the first read paused %d times, want %d", got, budgetPauses)
	}
	c.sleep(absentMemory - time.Second) // inside the memory
	_, _ = readSharedFile(path)
	if got := gatePauses.Load(); got != budgetPauses {
		t.Errorf("a read inside the memory paused %d more times, want none", got-budgetPauses)
	}
	c.sleep(2 * time.Second) // past it
	_, _ = readSharedFile(path)
	if got := gatePauses.Load(); got != 2*budgetPauses {
		t.Errorf("a read after the memory ran out paused %d times in all, want %d (a new budget)", got, 2*budgetPauses)
	}
	if got := gateExpiries.Load(); got != 2 {
		t.Errorf("gateExpiries = %d, want 2", got)
	}

	// The file comes back: a read of it succeeds and the memory is gone, so the next rename of the same name is waited for.
	if err := os.Remove(stagingPath(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("42"), 0o666); err != nil {
		t.Fatal(err)
	}
	if b, err := readSharedFile(path); err != nil || string(b) != "42" {
		t.Fatalf("readSharedFile = %q, %v after the file came back", b, err)
	}
	if err := os.Rename(path, stagingPath(path)); err != nil {
		t.Fatal(err)
	}
	at := c.now()
	if err := os.Chtimes(stagingPath(path), at, at); err != nil {
		t.Fatal(err)
	}
	endRenameAtOpen(t, path, 3)
	before := gatePauses.Load()
	if b, err := readSharedFile(path); err != nil || string(b) != "42" {
		t.Fatalf("readSharedFile = %q, %v for a new rename after the file came back, want it waited for", b, err)
	}
	if got := gatePauses.Load() - before; got != 2 {
		t.Errorf("the new rename was waited for with %d pauses, want 2", got)
	}
}

// Design point 3: a fresh leftover staged file under a grant costs at most one wait per file per grant, however many times the grant
// reads the file. Three leases are held and never renewed; the heartbeat of each has a staged file beside it and no live name, as it has
// when its holder's first renewal died between the write and the rename.
func TestAFreshLeftoverStagedHeartbeatCostsAGrantAtMostOneWaitPerFile(t *testing.T) {
	c := onGateClock(t)
	m := scopedRealClock(t)
	var held []*Lease
	for _, card := range []string{card0, card1, card2} {
		l := mustDevices(t, m, card)
		held = append(held, l)
		t.Cleanup(func() { _ = l.Release() })
	}
	// The leftovers go in once all three are held: a grant in between would meet (and remember) the earlier ones.
	for _, l := range held {
		stuckBeside(t, c, m.heartbeatPath(l.Epoch()))
	}
	gatePauses.Store(0)
	gateExpiries.Store(0)

	l4, err := m.TryAcquire(ClassMedia, Options{Reason: "test", Devices: []string{card3}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("a grant of a free card while three heartbeats have a fresh leftover beside them: %v", err)
	}
	defer l4.Release()

	files := int64(len(held))
	budgetPauses := int64(absentBudget / absentPause)
	t.Logf("grant of card3 with %d fresh leftovers: %d pauses, %d budgets run out", files, gatePauses.Load(), gateExpiries.Load())
	if gatePauses.Load() == 0 {
		t.Fatal("the grant never met the leftovers: the test does not observe the wait")
	}
	if got := gateExpiries.Load(); got > files {
		t.Errorf("the grant ran out %d budgets for %d files, want at most one each", got, files)
	}
	if got := gatePauses.Load(); got > files*budgetPauses {
		t.Errorf("the grant paused %d times for %d files, want at most %d (one budget of %d for each file, not one for each read of it)", got, files, files*budgetPauses, budgetPauses)
	}
}

// The memory of a wait that ran out ends when the staged file is gone: a name that is simply absent is then absent, at once, and a
// leftover that appears later is a new thing to wait for. Neither end was pinned: only the time and a successful read were.
func TestTheMemoryOfAWaitThatRanOutEndsWhenTheStagedFileIsGone(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	stuckBeside(t, c, path)
	if _, err := readSharedFile(path); !errors.Is(err, errRenameInFlight) {
		t.Fatalf("read of a stuck leftover = %v, want an I/O-class errRenameInFlight", err)
	}
	budget := int64(absentBudget / absentPause)
	if got := gatePauses.Load(); got != budget {
		t.Fatalf("the first read paused %d times, want one budget (%d)", got, budget)
	}

	// The leftover is cleared away: the name is absent and nothing stands beside it.
	if err := os.Remove(stagingPath(path)); err != nil {
		t.Fatal(err)
	}
	_, err := readSharedFile(path)
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errRenameInFlight) {
		t.Errorf("read of an absent name with its leftover gone = %v, want a plain not-exist error: the memory of the wait must end with the file it was for", err)
	}
	if got := gatePauses.Load(); got != budget {
		t.Errorf("the read of the absent name paused %d more times, want none", got-budget)
	}

	// A leftover that appears afterwards is waited for in full.
	stuckBeside(t, c, path)
	if _, err := readSharedFile(path); !errors.Is(err, errRenameInFlight) {
		t.Fatalf("read of a new stuck leftover = %v, want an I/O-class errRenameInFlight", err)
	}
	if got := gatePauses.Load(); got != 2*budget {
		t.Errorf("a new leftover after the old one was cleared away paused %d times in all, want %d: the new one is a new wait, and it was answered from the memory of the old", got, 2*budget)
	}
}
