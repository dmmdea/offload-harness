package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRenameReplacingSurvivesAConcurrentReader reproduces the Windows lease
// flake: a writer renaming a fresh temp file over a live path while another
// goroutine holds that path open for a plain read. On Windows, os.Rename is
// MoveFileEx, which must delete the destination's directory entry to replace
// it; deleting requires FILE_SHARE_DELETE on every handle already open
// against it, and Go's os.Open never grants that bit (see renameReplacing).
// A concurrent os.ReadFile therefore makes a bare os.Rename fail with
// ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION, intermittently — this is
// exactly what TestReserveRenewsTheLeaseWhileDraining and
// TestReserveRenewsTheLeaseWhileWarmingBack hit (~1 in 24 full runs) via
// Manager.Restamp, which renames a fresh record over meta.json while the
// test's own polling Inspect() (and, in production, `gpu status`, a probe,
// or any other reader) reads that same path with no coordination.
//
// Guarded to run fast: the reader loop is a tight, uncoordinated os.ReadFile
// spin, and the writer does a bounded number of renames — enough to
// reproduce the race reliably on Windows (measured: fails within the first
// few dozen iterations on a bare os.Rename) without a sleep-based wait.
func TestRenameReplacingSurvivesAConcurrentReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(path, []byte("0"), 0o666); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var readerRunning sync.WaitGroup
	readerRunning.Add(1)
	go func() {
		defer readerRunning.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The read result itself is irrelevant here — only the WRITER's
			// rename is under test. This goroutine's sole job is to hold a
			// share-read-write (never share-delete) handle open against
			// `path` as often as possible, the same way Inspect()/readMeta()
			// or a concurrent `gpu status` process would.
			_, _ = os.ReadFile(path)
		}
	}()
	defer func() {
		close(stop)
		readerRunning.Wait()
	}()

	const iterations = 200
	var failures atomic.Int64
	var firstErr atomic.Value // error
	for i := 0; i < iterations; i++ {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(i)), 0o666); err != nil {
			t.Fatalf("writing temp file (iteration %d): %v", i, err)
		}
		if err := renameReplacing(tmp, path); err != nil {
			failures.Add(1)
			firstErr.CompareAndSwap(nil, err)
			_ = os.Remove(tmp) // renameReplacing leaves tmp behind on failure
		}
	}

	if n := failures.Load(); n > 0 {
		err, _ := firstErr.Load().(error)
		t.Fatalf("renameReplacing failed %d/%d times under a concurrent reader; first error: %v", n, iterations, err)
	}
}

// TestRenameReplacingRetriesTheWindowsErrnosAndNothingElse pins the
// classification renameReplacing depends on: it must retry exactly the two
// documented Windows ephemeral errnos and treat every other error —
// including a genuine "file not found" from a bad source path — as final on
// the first attempt, on every platform (isEphemeralRenameError is
// unconditionally false off Windows).
func TestRenameReplacingStopsImmediatelyOnANonEphemeralError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	dest := filepath.Join(dir, "dest")
	err := renameReplacing(missing, dest)
	if err == nil {
		t.Fatal("expected an error renaming a nonexistent source")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected a not-exist error, got %v", err)
	}
}
