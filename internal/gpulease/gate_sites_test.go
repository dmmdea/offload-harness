package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The sites that decide from a read or a stat of a file the writer replaces by a rename go through the gate too (gate.go): the keeper's
// look at the record, the test for a claim in progress, the waiters' and yield files, and the ComfyUI launch markers (which Node
// restamps through a staged .tmp file). Each test makes the rename end at a chosen look, or never, and counts what the site did.

// stageContent is stageBeside with the staged file's content.
func stageContent(t *testing.T, c *gateClock, path string, content []byte, age time.Duration) {
	t.Helper()
	staged := stageBeside(t, c, path, age)
	if err := os.WriteFile(staged, content, 0o666); err != nil {
		t.Fatal(err)
	}
	at := c.now().Add(-age)
	if err := os.Chtimes(staged, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestKeeperSignOfLifeWaitsForARecordThatIsBeingInstalled(t *testing.T) {
	c := onGateClock(t)
	m := scopedRealClock(t)
	record := m.metaPath()
	stageBeside(t, c, record, 0)
	endRenameAtStat(t, record, 3)
	if sign := m.keeperSignOfLife(0, record); sign.IsZero() {
		t.Fatal("keeperSignOfLife found no sign of life in a record that was being replaced when it looked: a keeper that is alive reads as gone")
	}
}

func TestAClaimBeingInstalledReadsAsAClaimInProgressAndNotAsDebris(t *testing.T) {
	t.Run("the rename ends", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		stageBeside(t, c, m.metaPath(), 0)
		endRenameAtStat(t, m.metaPath(), 3)
		if !m.claimIsFresh() {
			t.Error("claimIsFresh = false for a claim whose rename ended a moment ago, want true")
		}
	})
	t.Run("the rename never ends", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		stuckBeside(t, c, m.metaPath())
		if !m.claimIsFresh() {
			t.Error("claimIsFresh = false for a claim that cannot be examined because a rename may be installing it: it would be called debris and removed")
		}
	})
	t.Run("nothing is there", func(t *testing.T) {
		onGateClock(t)
		m := scopedRealClock(t)
		if m.claimIsFresh() {
			t.Error("claimIsFresh = true with no claim at all, want false")
		}
	})
}

func TestAWaiterRecordBeingRefreshedIsReadAndStatedAfterTheRenameEnds(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "4242.1.json")
	stageBeside(t, c, path, 0)
	endRenameAtOpen(t, path, 3)
	if b, err := readWaiterFile(path); err != nil || string(b) != "42" {
		t.Fatalf("readWaiterFile = %q, %v while the record was being refreshed, want its content once the rename ends", b, err)
	}

	other := filepath.Join(t.TempDir(), "4243.1.json")
	stageBeside(t, c, other, 0)
	endRenameAtStat(t, other, 3)
	if fi, err := statWaiterFile(other); err != nil || fi == nil {
		t.Fatalf("statWaiterFile = %v, %v while the record was being refreshed, want the file once the rename ends", fi, err)
	}
}

// A wait that ran out is not repeated by the retry loop around it: the loop retries an error that may pass, and this one just had its
// whole budget.
func TestAWaiterRecordWhoseRenameNeverEndsIsGivenUpOnOnceNotRetriedInALoop(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "4242.1.json")
	stuckBeside(t, c, path)
	log := recordOpens(t, "", nil)
	_, err := readWaiterFile(path)
	if !errors.Is(err, errRenameInFlight) || os.IsNotExist(err) {
		t.Fatalf("readWaiterFile = %v, want errRenameInFlight, which is not a confirmed absence (the caller would prune the record)", err)
	}
	if got, want := log.total(), int(absentBudget/absentPause)+1; got != want {
		t.Errorf("the record was opened %d times, want %d (one wait, then the answer; no retry loop around it)", got, want)
	}
}

func TestReadYieldWaitsForTheRequestToBeInstalled(t *testing.T) {
	c := onGateClock(t)
	path := YieldFilePath(t.TempDir(), 7)
	b, err := json.Marshal(YieldRequest{Epoch: 7, AskedMs: 5})
	if err != nil {
		t.Fatal(err)
	}
	stageContent(t, c, path, b, 0)
	endRenameAtOpen(t, path, 3)
	req, ok := ReadYield(filepath.Dir(path), 7)
	if !ok || req.Epoch != 7 {
		t.Fatalf("ReadYield = %+v, %v while the request was being installed, want it once the rename ends", req, ok)
	}
}

// The ask's first look at the request file is outside the epoch lock. One that meets the file mid-rename must wait for it and find it,
// not go on to the lock to ask again.
func TestAnAskThatMeetsTheRequestFileMidRenameWaitsForItInsteadOfGoingOnToAskAgain(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(12 * time.Minute)
	r.waiter(yieldWaiterA)
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	c := onGateClock(t)

	if err := os.Rename(r.yieldFile(), stagingPath(r.yieldFile())); err != nil {
		t.Fatal(err)
	}
	at := c.now()
	if err := os.Chtimes(stagingPath(r.yieldFile()), at, at); err != nil {
		t.Fatal(err)
	}
	endRenameAtStat(t, r.yieldFile(), 3)
	var reachedTheLock atomic.Int64
	yieldBeforeLockHook = func() { reachedTheLock.Add(1) }
	t.Cleanup(func() { yieldBeforeLockHook = nil })

	b := r.m.NewYieldAsker(r.l.Epoch())
	for i := 0; i < 3; i++ {
		if _, asked, err := b.Tick(); err != nil || asked {
			t.Fatalf("tick %d of a second asker = asked %v, %v: the epoch had been asked", i+1, asked, err)
		}
	}
	if got := reachedTheLock.Load(); got != 0 {
		t.Errorf("the second asker went on to the epoch lock %d times to ask an epoch whose request was being installed, want 0", got)
	}
}

// The ComfyUI launch markers are restamped by Node through a staged .tmp file and read by Go in two places: the lease reader's tree walk
// (tree.go) and the scope inference (infer.go). Both go through the shared open.
func TestTheLaunchMarkerReadersGoThroughTheSharedOpen(t *testing.T) {
	onGateClock(t)
	dir := t.TempDir()
	keyed := filepath.Join(dir, ".offload-launch-k.json")
	plain := filepath.Join(dir, ".offload-launch.json")
	for _, p := range []string{keyed, plain} {
		if err := os.WriteFile(p, []byte(`{"pid":4242,"key":"k"}`), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	log := recordOpens(t, "", nil)
	if got, unknown := readLaunchMarkers(dir); len(got) != 2 || unknown {
		t.Fatalf("readLaunchMarkers = %+v (unknown %v), want both markers and nothing unknown", got, unknown)
	}
	if _, ok := LaunchMarkerReader(dir)(); !ok {
		t.Fatal("LaunchMarkerReader found no marker")
	}
	for _, p := range []string{keyed, plain} {
		if got := log.perPath[p]; got == 0 {
			t.Errorf("%s was never opened through the shared open: a plain read of it fails while Node renames over it, and waits for nothing", filepath.Base(p))
		}
	}
}
