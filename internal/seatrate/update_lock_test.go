package seatrate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// updateThroughContention is Update that does not give up on contention. Update returns ErrLocked
// when it could not take the store's lock within its lockWait (2 s), by design: a run is never blocked
// by the store, the caller skips that one observation and the next run records again. That is the
// right answer for a run and the wrong one for a test that asks whether twelve writers who all get in
// are all kept. Twelve writers queued behind one lock file take longer than 2 s on a saturated host,
// and ErrLocked leaves the store untouched, so asking again cannot double-count. Every OTHER error is
// returned as it is: a failure that is not contention must not be hidden by a retry, and a lock create
// that is refused window after window (see retryContention) is not contention either.
func updateThroughContention(path string, fn func(*Store)) error {
	return retryContention(func() error { return Update(path, fn) }, 5*time.Minute) // only a lock that never frees reaches this
}

// maxDeniedWindows is how many lock windows in a row may end in a refused create before
// retryContention reports the refusal. One window can end that way by chance under heavy contention on
// Windows (the last attempt of the window landed on a delete still pending, the other writers' work),
// so a single one is not a verdict. A directory that cannot be written ends every window that way and
// surfaces after maxDeniedWindows x lockWait instead of after the whole contention deadline.
const maxDeniedWindows = 3

// retryContention calls update until it returns something other than ErrLocked, for at most `within`.
// An ErrLocked that also carries a permission denial (Update's answer to a lock create refused for the
// whole window) counts toward maxDeniedWindows, and any other ErrLocked starts the count again.
func retryContention(update func() error, within time.Duration) error {
	deadline := time.Now().Add(within)
	denied := 0
	for {
		err := update()
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			return err
		}
		if !errors.Is(err, fs.ErrPermission) {
			denied = 0
			continue
		}
		if denied++; denied >= maxDeniedWindows {
			return err
		}
	}
}

// lockOpens swaps the lock-open seam for one that answers the first `deny` calls with err and every
// later call with the real exclusive create, and returns the number of calls made so far. The seam is
// restored when the test ends.
func lockOpens(t *testing.T, deny int, err error) *atomic.Int32 {
	t.Helper()
	orig := openLock
	var calls atomic.Int32
	openLock = func(name string) (*os.File, error) {
		if int(calls.Add(1)) <= deny {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return orig(name)
	}
	t.Cleanup(func() { openLock = orig })
	return &calls
}

// On Windows, deleting a file whose handle is still open marks it delete-PENDING, and an exclusive
// create issued in that window fails with ACCESS_DENIED rather than "already exists". Update used to
// return that raw, so the run that finished while another run's lock was being deleted lost its rate
// sample (and the serialise gate failed one run in five, 2026-09-17). The denial is contention: ask
// again inside the lock window.
func TestUpdateRetriesALockOpenDeniedWhileADeleteIsPending(t *testing.T) {
	path := Path(t.TempDir())
	calls := lockOpens(t, 3, fs.ErrPermission)
	ran := 0
	err := Update(path, func(s *Store) {
		ran++
		s.Observe("agent-pool", 30, 0, time.Now())
	})
	if err != nil {
		t.Fatalf("a denied lock open while a delete is pending must be retried, got %v", err)
	}
	if ran != 1 {
		t.Fatalf("fn ran %d times, want exactly once", ran)
	}
	if n := calls.Load(); n != 4 {
		t.Fatalf("the lock was opened %d times, want 4 (three denials, then the create)", n)
	}
	s, lerr := Load(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := s.Get("agent-pool").Samples; got != 1 {
		t.Fatalf("samples = %d, want the one observation stored", got)
	}
	if _, serr := os.Stat(path + ".lock"); serr == nil {
		t.Fatal("lock file left behind")
	}
}

// A denial that never clears is a directory this process cannot write, not a delete in flight. It
// surfaces after the lock window, as ErrLocked (the caller logs and skips, as for a held lock) that
// also wraps the open error, so the real cause is never hidden behind "locked by another writer".
func TestUpdateReportsAPersistentDenialAsLockedAndNamesBothCauses(t *testing.T) {
	path := Path(t.TempDir())
	calls := lockOpens(t, 1<<30, fs.ErrPermission)
	ran := false
	start := time.Now()
	err := Update(path, func(*Store) { ran = true })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a persistent denial returned nil")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("a persistent denial must be ErrLocked so the caller skips the sample as it does for a held lock, got %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("a persistent denial must still name the open error (fs.ErrPermission), got %v", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != path+".lock" {
		t.Errorf("the open error (a *fs.PathError naming the lock file) is not in the chain: %v", err)
	}
	if elapsed < lockWait/2 || elapsed > lockWait+time.Second {
		t.Errorf("returned after %s, want about lockWait (%s): retried until the window ends, not forever", elapsed, lockWait)
	}
	if calls.Load() < 5 {
		t.Errorf("the lock was opened only %d times in %s: the denial was not polled", calls.Load(), elapsed)
	}
	if ran {
		t.Error("fn ran without the lock")
	}
	for _, f := range []string{path, path + ".lock"} {
		if _, serr := os.Stat(f); serr == nil {
			t.Errorf("%s exists after a refused update", filepath.Base(f))
		}
	}
}

// Guard: a lock another live writer holds is still ErrLocked at the end of the window, and it is not
// reported as a permission problem.
func TestUpdateStillReturnsErrLockedForALiveLock(t *testing.T) {
	path := Path(t.TempDir())
	if err := os.WriteFile(path+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ran := false
	start := time.Now()
	err := Update(path, func(*Store) { ran = true })
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("live lock: err = %v, want ErrLocked", err)
	}
	if errors.Is(err, fs.ErrPermission) {
		t.Errorf("a held lock is not a permission problem: %v", err)
	}
	if d := time.Since(start); d > lockWait+time.Second {
		t.Errorf("a live lock held the caller for %s, want about lockWait (%s)", d, lockWait)
	}
	if ran {
		t.Error("fn ran without the lock")
	}
	if _, serr := os.Stat(path + ".lock"); serr != nil {
		t.Errorf("another writer's live lock was removed: %v", serr)
	}
}

// Guard: only contention is retried. Any other failure to open the lock (a vanished directory, a full
// disk) is returned at once and unchanged, not polled for two seconds and not reworded.
func TestUpdateReturnsAnyOtherLockOpenErrorAtOnce(t *testing.T) {
	path := Path(t.TempDir())
	boom := errors.New("no space left on device")
	calls := lockOpens(t, 1<<30, boom)
	start := time.Now()
	err := Update(path, func(*Store) { t.Error("fn ran without the lock") })
	if !errors.Is(err, boom) || errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want the open error itself and not ErrLocked", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the lock was opened %d times, want 1 (not contention, not retried)", n)
	}
	if d := time.Since(start); d > lockWait/2 {
		t.Errorf("took %s: a non-contention error was waited on", d)
	}
}

// Guard for the deadline: a stale lock that cannot be removed (on Windows a handle another process
// holds open blocks the delete) must wait out the lock window and end as ErrLocked. The takeover used
// to retry at once whether or not the removal worked, so such a lock spun the caller at full speed
// with no deadline.
func TestUpdateWithAStaleLockItCannotRemoveEndsAtTheDeadline(t *testing.T) {
	path := Path(t.TempDir())
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	var removals atomic.Int32
	orig := removeLock
	removeLock = func(name string) error {
		removals.Add(1)
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
	}
	t.Cleanup(func() { removeLock = orig })
	done := make(chan error, 1)
	go func() { done <- Update(path, func(*Store) { t.Error("fn ran without the lock") }) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("err = %v, want ErrLocked at the deadline", err)
		}
	case <-time.After(lockWait + 4*time.Second):
		t.Fatalf("Update is still spinning on a stale lock it cannot remove, long after the lock window (%d removals)", removals.Load())
	}
	// One removal per poll at most: about lockWait/lockPoll, never the millions of a spin.
	if n := removals.Load(); n < 2 || n > 4*int32(lockWait/lockPoll) {
		t.Errorf("%d removal attempts in a %s window polled every %s: want about one per poll", n, lockWait, lockPoll)
	}
}
