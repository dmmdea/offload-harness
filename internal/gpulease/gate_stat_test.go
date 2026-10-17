package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// The stat sites (statRecord and the callers that decide from a stat) wait for a rename the way the reads do (gate_test.go): the same
// helper, statSettled, the same bound, the same memory, and the same failure class when the wait runs out.

// endRenameAtStat makes the staged file take the live name just before the nth stat of path. It returns the count of stats of path.
func endRenameAtStat(t *testing.T, path string, n int) *atomic.Int64 {
	t.Helper()
	staged := stagingPath(path)
	var stats atomic.Int64
	old := statFile
	statFile = func(p string) (os.FileInfo, error) {
		if p == path && int(stats.Add(1)) == n {
			if err := os.Rename(staged, path); err != nil {
				t.Errorf("ending the rename: %v", err)
			}
		}
		return old(p)
	}
	t.Cleanup(func() { statFile = old })
	return &stats
}

func TestAStatOfAnAbsentNameWithAFreshStagedFileWaitsForTheRenameToEnd(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "e", "7.json")
	stageBeside(t, c, path, 0)
	stats := endRenameAtStat(t, path, 4)
	fi, err := statSettled(path)
	if err != nil || fi == nil {
		t.Fatalf("statSettled = %v, %v while the rename of the record was in flight, want the file once it ends", fi, err)
	}
	if got := stats.Load(); got != 4 {
		t.Errorf("the name was looked at %d times, want 4 (three that met the rename, one after it)", got)
	}
	if got := gatePauses.Load(); got != 3 {
		t.Errorf("the gate paused %d times, want 3", got)
	}
}

func TestAStatOfARenameThatNeverEndsGivesUpAsAFailureThatCannotBeExaminedNotAnAbsence(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "e", "7.json")
	stuckBeside(t, c, path)
	_, err := statSettled(path)
	if !errors.Is(err, errRenameInFlight) || errors.Is(err, fs.ErrNotExist) || !isIOFailure(err) {
		t.Fatalf("statSettled = %v, want an I/O-class errRenameInFlight that is not a not-exist error", err)
	}
	if got, want := gatePauses.Load(), int64(absentBudget/absentPause); got != want {
		t.Errorf("the gate paused %d times before giving up, want %d", got, want)
	}
	if got := gateExpiries.Load(); got != 1 {
		t.Errorf("gateExpiries = %d, want 1", got)
	}
}

// statRecord is what Restamp, ReleaseTarget, RecordState and the release use to tell a card lease from a whole-node one, and every one of
// them reads an error that is not "absent" as "cannot be examined". It goes through statSettled.
func TestStatRecordWaitsForARenameToEndAndReportsOneThatNeverEndsAsAnErrorNotAnAbsence(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "e", "7.json")
	stageBeside(t, c, path, 0)
	endRenameAtStat(t, path, 3)
	if err := statRecord(path); err != nil {
		t.Fatalf("statRecord = %v while the rename of the record was in flight, want nil once it ends", err)
	}

	stuck := filepath.Join(t.TempDir(), "e", "8.json")
	stuckBeside(t, c, stuck)
	if err := statRecord(stuck); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("statRecord = %v for a record whose rename never ends, want an error that is not an absence", err)
	}
}

// The heartbeat reader reads and then looks at the file (heartbeatAt): a name that outlasted the wait in the read must not cost a second
// wait in the look.
func TestAReadThatGaveUpAndTheLookThatFollowsItPayOneWaitBetweenThem(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	stuckBeside(t, c, path)
	if _, err := readSharedFile(path); !errors.Is(err, errRenameInFlight) {
		t.Fatalf("readSharedFile = %v, want errRenameInFlight", err)
	}
	pauses := gatePauses.Load()
	if _, err := statSettled(path); !errors.Is(err, errRenameInFlight) {
		t.Fatalf("statSettled = %v, want errRenameInFlight", err)
	}
	if got := gatePauses.Load(); got != pauses {
		t.Errorf("the look after the read paused %d more times, want none: the name had just outlasted a whole wait", got-pauses)
	}
	if got := gateExpiries.Load(); got != 1 {
		t.Errorf("gateExpiries = %d, want 1", got)
	}

	// and so does the reader of the heartbeat as a whole: its read and its look are one wait
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	hb := m.heartbeatPath(l.Epoch())
	stuckBeside(t, c, hb)
	gatePauses.Store(0)
	meta, err := readEpochRecord(m.leaseDir(), l.Epoch())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, known := heartbeatAt(m.leaseDir(), meta); known {
			t.Fatalf("heartbeatAt known = true for a heartbeat whose rename never ends, want unknown")
		}
	}
	if got, want := gatePauses.Load(), int64(absentBudget/absentPause); got != want {
		t.Errorf("three reads of an in-flight heartbeat paused %d times, want %d (one budget)", got, want)
	}
}
