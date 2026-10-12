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

// renameDenials swaps the rename seam for one that answers the first `deny` calls with err (as the
// *os.LinkError os.Rename returns) and every later call with the real rename, and returns the number of
// calls made so far. The seam is restored when the test ends.
func renameDenials(t *testing.T, deny int, err error) *atomic.Int32 {
	t.Helper()
	orig := renameStore
	var calls atomic.Int32
	renameStore = func(oldpath, newpath string) error {
		if int(calls.Add(1)) <= deny {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { renameStore = orig })
	return &calls
}

// onlyTheStoreIsLeft fails the test when anything but the store file is in its directory: no lock file
// and no temp file survive an Update, whatever it returned.
func onlyTheStoreIsLeft(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("%s left behind next to the store", e.Name())
		}
	}
}

// The store is read without the lock, and on Windows a rename over a file a reader has open fails with
// ACCESS_DENIED although nothing is wrong. D-120 made Update wait out a denied lock create; the rename
// that publishes the save is the same race one step later, and Update used to hand its error to the
// caller, which logs "seat-rates store not updated" and drops the run's sample. The denial is ephemeral:
// ask again inside the window.
func TestUpdateRetriesARenameDeniedWhileAReaderHoldsTheStoreOpen(t *testing.T) {
	path := Path(t.TempDir())
	calls := renameDenials(t, 3, fs.ErrPermission)
	ran := 0
	err := Update(path, func(s *Store) {
		ran++
		s.Observe("agent-pool", 30, 0, time.Now())
	})
	if err != nil {
		t.Fatalf("a rename denied while a reader holds the store open must be retried, got %v", err)
	}
	if ran != 1 {
		t.Fatalf("fn ran %d times, want exactly once (the retry is of the rename, not of the update)", ran)
	}
	if n := calls.Load(); n != 4 {
		t.Fatalf("the rename was tried %d times, want 4 (three denials, then the rename)", n)
	}
	s, lerr := Load(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := s.Get("agent-pool").Samples; got != 1 {
		t.Fatalf("samples = %d, want the one observation stored", got)
	}
	onlyTheStoreIsLeft(t, path)
}

// The corrupt-store branch of Update saves through the same rename: its replacement is not lost either.
func TestUpdateRetriesTheRenameThatReplacesACorruptStore(t *testing.T) {
	path := Path(t.TempDir())
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := renameDenials(t, 2, fs.ErrPermission)
	err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) })
	if err == nil || errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want the corrupt-store notice and not the denial", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("the rename was tried %d times, want 3", n)
	}
	s, lerr := Load(path)
	if lerr != nil {
		t.Fatalf("the replacement store is not readable: %v", lerr)
	}
	if got := s.Get("agent-pool").Samples; got != 1 {
		t.Fatalf("samples = %d, want the one observation stored", got)
	}
}

// A denial that never clears (a store another account owns, an immutable file) ends after the window,
// names the rename, is not reported as lock contention, and leaves the previous store and no temp file.
func TestUpdateReportsAPersistentRenameDenialAfterTheWindow(t *testing.T) {
	path := Path(t.TempDir())
	if err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	calls := renameDenials(t, 1<<30, fs.ErrPermission)
	ran := 0
	start := time.Now()
	err := Update(path, func(s *Store) {
		ran++
		s.Observe("agent-pool", 40, 0, time.Now())
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a persistent rename denial returned nil")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("the denial must stay in the chain, got %v", err)
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("a refused rename is not lock contention, and must not be reported as ErrLocked: %v", err)
	}
	var le *os.LinkError
	if !errors.As(err, &le) || le.Op != "rename" || le.New != path {
		t.Errorf("the error must name the rename onto the store (a *os.LinkError for %s): %v", path, err)
	}
	if elapsed < lockWait/2 || elapsed > lockWait+time.Second {
		t.Errorf("returned after %s, want about lockWait (%s): retried until the window ends, not forever", elapsed, lockWait)
	}
	if calls.Load() < 5 {
		t.Errorf("the rename was tried only %d times in %s: the denial was not polled", calls.Load(), elapsed)
	}
	if ran != 1 {
		t.Errorf("fn ran %d times, want once", ran)
	}
	s, lerr := Load(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := s.Get("agent-pool").Samples; got != 1 {
		t.Errorf("samples = %d: the refused save changed the store", got)
	}
	onlyTheStoreIsLeft(t, path)
}

// Guard: only a denial that can clear is retried. Any other failure of the rename (a full disk, a
// vanished directory) is returned at once and unchanged.
func TestUpdateReturnsAnyOtherRenameErrorAtOnce(t *testing.T) {
	path := Path(t.TempDir())
	boom := errors.New("no space left on device")
	calls := renameDenials(t, 1<<30, boom)
	start := time.Now()
	err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the rename error itself", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the rename was tried %d times, want 1 (not a denial, not retried)", n)
	}
	if d := time.Since(start); d > lockWait/2 {
		t.Errorf("took %s: a rename error that cannot clear was waited on", d)
	}
	onlyTheStoreIsLeft(t, path)
}

// The real race, with no seam: a reader holds the store open for a while, as the unlocked Load of the
// delegate, the fleet node and the pipeline does, and Update saves meanwhile. On Windows the bare rename
// fails at once with "Access is denied" (the sample was lost); with the retry the save lands as soon as
// the reader lets go. Elsewhere a rename is never blocked and this is a plain round trip.
func TestUpdateSucceedsWhileAReaderHoldsTheStoreOpen(t *testing.T) {
	path := Path(t.TempDir())
	if err := Update(path, func(s *Store) { s.Observe("agent-pool", 30, 0, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	release := time.AfterFunc(300*time.Millisecond, func() { _ = held.Close() })
	defer release.Stop()
	if err := Update(path, func(s *Store) { s.Observe("agent-pool", 40, 0, time.Now()) }); err != nil {
		t.Fatalf("an Update that lands while a reader has the store open lost its sample: %v", err)
	}
	s, lerr := Load(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := s.Get("agent-pool").Samples; got != 2 {
		t.Fatalf("samples = %d, want both observations stored", got)
	}
	onlyTheStoreIsLeft(t, path)
}
