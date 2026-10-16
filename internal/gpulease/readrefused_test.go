package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The wait readSharedFile takes for a refused open (readretry_test.go) is for a rename in flight, which passes in milliseconds. A
// file that is STILL refused when the wait ends is not that: it is a lasting fault (a deny-read ACL on a lease file, a foreign
// delete that never lets go of its handle). The wait must be paid once for such a file, not once per read of it, because a grant
// reads the same record five times and the same heartbeat seven (the sweep, the unreadable scan and the verify each go over them)
// inside one hold of the epoch lock. At 20 pauses a read that is about 100 ms x 5 = 500 ms per refused record, under the lock: eight
// refused files held it for 4.4 s, and a second acquirer, which only waits for the lock, gave up after the 2 s the spin allows.
//
// The refusal here is the seam's (a permission error that never ends, for chosen paths), and the counts are the pin: pauses taken
// (readRetries) and opens made per path. The time is only logged.

// refusingPaths makes the shared open fail for good with a permission error for the given paths, and lets every other path through.
// Install it before recordOpens, which counts what reaches it.
func refusingPaths(t *testing.T, paths ...string) {
	t.Helper()
	refused := map[string]bool{}
	for _, p := range paths {
		refused[p] = true
	}
	old := openSharedFile
	openSharedFile = func(path string) (*os.File, error) {
		if refused[path] {
			return nil, &fs.PathError{Op: "open", Path: path, Err: os.ErrPermission}
		}
		return old(path)
	}
	t.Cleanup(func() { openSharedFile = old })
}

// heldThreeLeases holds cards 0..2 in a scratch root, each renewed so that its heartbeat file exists, and returns the records and
// heartbeats of the three.
func heldThreeLeases(t *testing.T) (m *Manager, records, heartbeats []string) {
	t.Helper()
	m = scopedRealClock(t)
	for _, c := range []string{card0, card1, card2} {
		l := mustDevices(t, m, c)
		t.Cleanup(func() { _ = l.Release() })
		if err := l.Renew(); err != nil {
			t.Fatal(err)
		}
		records = append(records, epochRecordPath(m.leaseDir(), l.Epoch()))
		heartbeats = append(heartbeats, m.heartbeatPath(l.Epoch()))
	}
	for _, p := range append(append([]string{}, records...), heartbeats...) {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the held lease has no %s: %v", filepath.Base(p), err)
		}
	}
	return m, records, heartbeats
}

// grantWithRefusedFiles asks for a fourth card while the given files of the three held leases are refused for good, and checks the
// pauses the whole grant took against one bounded wait per refused file.
func grantWithRefusedFiles(t *testing.T, refuse func(records, heartbeats []string) []string) {
	t.Helper()
	waitOutDeniedOpens(t)
	m, records, heartbeats := heldThreeLeases(t)
	paths := refuse(records, heartbeats)
	refusingPaths(t, paths...)
	log := recordOpens(t, m.epochLockPath(), nil)
	readRetries.Store(0)

	start := time.Now()
	l4, err := m.TryAcquire(ClassMedia, Options{Reason: "test", Devices: []string{card3}, TTL: time.Hour})
	took := time.Since(start)
	if l4 != nil {
		defer l4.Release()
	}
	pauses := int(readRetries.Load())
	t.Logf("grant of card3 with %d refused file(s): err=%v, %d pauses, %d opens in all (%d under the lock), %v", len(paths), err, pauses, log.total(), sumValues(log.underLock), took)
	for _, p := range paths {
		t.Logf("  %s: %d opens, %d of them under the lock", filepath.Base(p), log.perPath[p], log.underLock[p])
	}

	if pauses == 0 {
		t.Fatalf("the grant took no pause: the refused files were not read, so the test does not observe the wait")
	}
	if want := len(paths) * removeAttempts; pauses > want {
		t.Errorf("the grant took %d pauses for %d refused file(s), want at most %d (one bounded wait of %d for each file, not one for each read of it)",
			pauses, len(paths), want, removeAttempts)
	}
}

func TestALastingRefusalOfOneRecordIsWaitedOutOnceByAGrant(t *testing.T) {
	grantWithRefusedFiles(t, func(records, _ []string) []string { return records[1:2] })
}

func TestALastingRefusalOfOneHeartbeatIsWaitedOutOnceByAGrant(t *testing.T) {
	grantWithRefusedFiles(t, func(_, heartbeats []string) []string { return heartbeats[1:2] })
}

func TestEveryRecordAndHeartbeatThatIsLastingRefusedIsWaitedOutOnceByAGrant(t *testing.T) {
	grantWithRefusedFiles(t, func(records, heartbeats []string) []string {
		return append(append([]string{}, records...), heartbeats...)
	})
}

// TestAFileRefusedForGoodIsWaitedOutOnceNotOnEveryRead: the same count one level down, with no grant. The first read of a file that
// stays refused takes the whole wait; every read of it after that is one open, answered at once.
func TestAFileRefusedForGoodIsWaitedOutOnceNotOnEveryRead(t *testing.T) {
	waitOutDeniedOpens(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	if err := os.WriteFile(path, []byte("424242"), 0o666); err != nil {
		t.Fatal(err)
	}
	calls := failingOpens(t, 1<<30, os.ErrPermission)
	readRetries.Store(0)

	const reads = 5
	for i := 0; i < reads; i++ {
		before := calls.Load()
		pausesBefore := readRetries.Load()
		if _, err := readSharedFile(path); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("read %d of a refused file returned %v, want the permission error", i+1, err)
		}
		if i == 0 {
			if got := calls.Load() - before; got != int64(removeAttempts+1) {
				t.Errorf("the first read made %d opens, want %d (the first and %d retries)", got, removeAttempts+1, removeAttempts)
			}
			continue
		}
		if got := calls.Load() - before; got != 1 {
			t.Errorf("read %d of a file that had already outlasted its wait made %d opens, want 1", i+1, got)
		}
		if got := readRetries.Load() - pausesBefore; got != 0 {
			t.Errorf("read %d of a file that had already outlasted its wait took %d pauses, want 0", i+1, got)
		}
	}
	if got := readRetries.Load(); got != int64(removeAttempts) {
		t.Errorf("%d reads of one refused file took %d pauses in all, want %d (one wait)", reads, got, removeAttempts)
	}
}
