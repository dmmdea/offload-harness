package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Even with a handle that shares delete, an open can fail while a rename replaces the file. The rename marks the old entry for
// deletion before the new one takes its name, and for that instant an open of the name fails with ERROR_ACCESS_DENIED (the file is
// delete-pending), whoever opens it and however. Measured on a loaded Windows host with the shared open in place: 5 such failures in
// about 124,000 reads of a heartbeat that Renew rewrote in a loop, in 4 of 4 runs under 30 busy-loop processes, none in about 720,000
// reads on a quiet host. The instant ends in milliseconds, so the read waits it out (the same bound the claim removal uses). A read that
// gave up on it answered "no heartbeat" for a live lease, which is how a lease past its window was handed to a second job.
//
// The seam fails the open the way the system does (a permission error) for the first n calls and counts them; the Windows test
// beside this one (readretry_windows_test.go) shows that a delete-pending file really fails that way.

// failingOpens makes the shared open fail with err for its first n calls, then lets it through. It returns the count of opens made.
func failingOpens(t *testing.T, n int, err error) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	old := openSharedFile
	openSharedFile = func(path string) (*os.File, error) {
		if int(calls.Add(1)) <= n {
			return nil, &fs.PathError{Op: "open", Path: path, Err: err}
		}
		return old(path)
	}
	t.Cleanup(func() { openSharedFile = old })
	return &calls
}

// TestAReadThatMeetsAnAccessDeniedOpenWaitsForTheFile: three refusals, then the file. Every reader of a replaced lease file must get
// the new content, not the refusal and not the fallback.
func TestAReadThatMeetsAnAccessDeniedOpenWaitsForTheFile(t *testing.T) {
	dir := t.TempDir()
	meta := &Meta{Epoch: 7, RenewedAtMs: 1000}
	hb := filepath.Join(dir, "hb."+strconv.FormatUint(meta.Epoch, 10))
	if err := os.WriteFile(hb, []byte("424242"), 0o666); err != nil {
		t.Fatal(err)
	}
	asText := func(b []byte, err error) string {
		if err != nil {
			return "error: " + err.Error()
		}
		return string(b)
	}
	readers := map[string]func() string{
		"readSharedFile": func() string { return asText(readSharedFile(hb)) },
		"readRecordFile": func() string { return asText(readRecordFile(hb)) },
		"heartbeatAt":    func() string { return strconv.FormatInt(heartbeatAt(dir, meta), 10) },
	}
	for name, read := range readers {
		calls := failingOpens(t, 3, os.ErrPermission)
		if got := read(); got != "424242" {
			t.Errorf("%s after three access-denied opens = %q, want the file's content 424242", name, got)
		}
		if got := calls.Load(); got != 4 {
			t.Errorf("%s made %d opens, want 4 (three refused, one through)", name, got)
		}
	}
}

// TestAnAccessDeniedOpenThatNeverEndsIsGivenUpOnAfterABoundedWait: a file that stays unreadable is reported as it was, after
// removeAttempts retries and no more, so a status call is slowed by a bounded time and not hung.
func TestAnAccessDeniedOpenThatNeverEndsIsGivenUpOnAfterABoundedWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.json")
	if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	calls := failingOpens(t, 1<<30, os.ErrPermission)
	start := time.Now()
	_, err := readSharedFile(path)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("a file that stays unreadable returned %v, want the permission error", err)
	}
	if got, want := calls.Load(), int64(removeAttempts+1); got != want {
		t.Errorf("made %d opens, want %d (the first and %d retries)", got, want, removeAttempts)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("giving up took %v, want a bounded wait of about %v", took, time.Duration(removeAttempts)*removePause)
	}
}

// TestAFailureThatIsNotAnAccessDeniedIsNotRetried: a missing file is an answer ("no heartbeat yet", "lease gone") and any other
// failure is not known to pass; neither is waited on.
func TestAFailureThatIsNotAnAccessDeniedIsNotRetried(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.json")
	calls := failingOpens(t, 0, nil)
	if _, err := readSharedFile(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file returned %v, want ErrNotExist", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a missing file was opened %d times, want 1", got)
	}

	present := filepath.Join(t.TempDir(), "1.json")
	if err := os.WriteFile(present, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("not a permission error")
	calls = failingOpens(t, 1<<30, boom)
	if _, err := readSharedFile(present); !errors.Is(err, boom) {
		t.Fatalf("an unrelated failure returned %v, want it as it was", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("an unrelated failure was retried: %d opens, want 1", got)
	}
}
