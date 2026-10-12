package seatrate

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// deniedWindow is what Update returns when the lock create was refused for its whole window: ErrLocked
// and the open error side by side.
func deniedWindow() error {
	return fmt.Errorf("%w: the lock file could not be created within %s: %w", ErrLocked, lockWait,
		&fs.PathError{Op: "open", Path: "store.lock", Err: fs.ErrPermission})
}

// A lock create that is refused window after window is a directory nobody can write, not contention. The
// helper used to retry it (it is ErrLocked) for its whole five minutes, so a test on an unwritable
// directory sat silent until the go test timeout and then failed with a denial it could have named in
// seconds.
func TestRetryContentionReportsADenialThatEveryWindowEndsWith(t *testing.T) {
	calls := 0
	err := retryContention(func() error {
		calls++
		if calls > 50 {
			return nil // what the old helper reached after retrying for minutes
		}
		return deniedWindow()
	}, time.Minute)
	if !errors.Is(err, fs.ErrPermission) || !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want the refusal (ErrLocked beside the permission denial)", err)
	}
	if calls != maxDeniedWindows {
		t.Errorf("update was called %d times, want %d (one per lock window, then the verdict)", calls, maxDeniedWindows)
	}
}

// Guard: contention is still waited out, however long it lasts, and one window that happens to end in a
// denial among contention windows is not a verdict (the count is of windows in a row).
func TestRetryContentionKeepsRetryingContention(t *testing.T) {
	script := []error{ErrLocked, deniedWindow(), deniedWindow(), ErrLocked, deniedWindow(), deniedWindow(), ErrLocked}
	for i := 0; i < 40; i++ {
		script = append(script, ErrLocked)
	}
	script = append(script, nil)
	calls := 0
	err := retryContention(func() error {
		e := script[calls]
		calls++
		return e
	}, time.Minute)
	if err != nil {
		t.Fatalf("err = %v, want nil: contention windows are asked again until the store is free", err)
	}
	if calls != len(script) {
		t.Errorf("update was called %d times, want %d", calls, len(script))
	}
}

// Guard: any error that is not ErrLocked is returned at once and unchanged, and so is ErrLocked once the
// contention deadline has passed.
func TestRetryContentionReturnsOtherErrorsAtOnceAndGivesUpAtItsDeadline(t *testing.T) {
	boom := errors.New("disk full")
	calls := 0
	if err := retryContention(func() error { calls++; return boom }, time.Minute); !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err = %v after %d calls, want the error itself after 1", err, calls)
	}
	start := time.Now()
	err := retryContention(func() error { return ErrLocked }, 50*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Errorf("err = %v, want ErrLocked at the deadline", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a 50 ms contention deadline took %s", d)
	}
}

// captureLog sends the standard logger to a buffer until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// A lock that cannot be removed (on Windows a scanner holding it open without delete sharing) makes every
// writer in the next lockStale skip its sample with only "store locked by another writer" to show for it.
// The save itself landed, so Update still succeeds; the failed release is said once, naming the lock and
// the cause.
func TestUpdateSaysWhenItCannotRemoveItsLock(t *testing.T) {
	path := Path(t.TempDir())
	lock := path + ".lock"
	orig := removeLock
	removeLock = func(name string) error {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
	}
	t.Cleanup(func() { removeLock = orig })
	out := captureLog(t)
	if err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) }); err != nil {
		t.Fatalf("a lock that cannot be released must not fail an update whose save landed: %v", err)
	}
	s, lerr := Load(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := s.Get("agent-pool").Samples; got != 1 {
		t.Fatalf("samples = %d, want the one observation stored", got)
	}
	line := out.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, lock) || !strings.Contains(line, fs.ErrPermission.Error()) {
		t.Errorf("want one log line naming the lock %s and the cause, got %q", lock, line)
	}
	if _, serr := os.Stat(lock); serr != nil {
		t.Errorf("the seam refused the removal, so the lock must still be there: %v", serr)
	}
}

// Guard: a release that works, or finds the lock already gone, says nothing.
func TestUpdateSaysNothingWhenItsLockIsReleasedOrAlreadyGone(t *testing.T) {
	out := captureLog(t)
	path := Path(t.TempDir())
	if err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	orig := removeLock
	removeLock = func(name string) error {
		_ = orig(name)
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	t.Cleanup(func() { removeLock = orig })
	if err := Update(path, func(s *Store) { s.Observe("agent-pool", 40, 0, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("a clean release logged %q", out.String())
	}
}
