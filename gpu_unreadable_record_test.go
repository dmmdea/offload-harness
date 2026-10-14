package main

// Round 4, items 1 and 2: a record that EXISTS but cannot be read is a lease that is held, never one that is gone.
//
// Inspect() and Leases() read through the reader, which treats an unreadable record as absent, and the release paths believed it:
// a holder whose own release failed over such a record exited 0 and silently left the cards held, and `gpu release` said "no lease
// is held" over one. Reproduced by replacing the record with a directory that cannot be removed as a file.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// unreadableRecord replaces the file at path with a non-empty directory (it exists, cannot be read, cannot be removed as a file) and
// returns the function that puts the file back.
func unreadableRecord(t *testing.T, path string) (restore func()) {
	t.Helper()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o777); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = os.RemoveAll(path)
		if err := os.WriteFile(path, saved, 0o666); err != nil {
			t.Fatal(err)
		}
	}
}

// ITEM 1. The holder's release over a record it cannot read is a failure it retries and reports; it used to believe Inspect() ("not
// held") and return nil, so the holder exited 0 over a held lease.
func TestTheHoldersReleaseOverARecordItCannotReadIsAFailureNeverSuccess(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	_ = cfgPath
	fastHolderRetries(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	restore := unreadableRecord(t, filepath.Join(m.Dir(), "meta.json"))
	t.Cleanup(func() { restore(); releaseAll(t, m) })

	rerr := releaseHolderLease(m, l)
	if rerr == nil {
		t.Fatal("the record exists and cannot be read: the lease is held, and a release that cannot touch it did not happen")
	}
	if !gpulease.IsRecordUnreadable(rerr) && !strings.Contains(rerr.Error(), "cannot be read") && !strings.Contains(rerr.Error(), "still held") {
		t.Errorf("the failure says why: %v", rerr)
	}

	// The operator's verb takes the same line: with --epoch it never says "no lease is held" over it.
	var verbErr error
	out := captureStdout(t, func() { verbErr = runGPURelease([]string{"--config", cfgPath, "--epoch", "1"}) })
	if verbErr == nil || strings.Contains(out, "no lease is held") {
		t.Errorf("gpu release --epoch over a record it cannot read is an error, not \"no lease is held\" (err=%v, stdout=%q)", verbErr, out)
	}
	var ru *gpulease.RecordUnreadableError
	_ = errors.As(rerr, &ru)
}

// ITEM 2. Bare `gpu release` (no --epoch) on a card-scoped host enumerated the card leases through the reader, which skips a record
// it cannot read, and printed "no lease is held" with exit 0. An entry that exists and cannot be read is an error that names it.
func TestBareGPUReleaseNeverSaysNoLeaseIsHeldOverAnUnreadableCardRecord(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Devices: []string{"gpu-test-0"}})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	restore := unreadableRecord(t, filepath.Join(m.Dir(), "e", "1.json"))
	t.Cleanup(func() { restore(); releaseAll(t, m) })

	var verbErr error
	out := captureStdout(t, func() { verbErr = runGPURelease([]string{"--config", cfgPath}) })
	if verbErr == nil || strings.Contains(out, "no lease is held") {
		t.Fatalf("bare gpu release over a card record it cannot read is an error, not \"no lease is held\" with exit 0 (err=%v, stdout=%q)", verbErr, out)
	}
	for _, want := range []string{"epoch 1", "cannot be read", "--epoch 1"} {
		if !strings.Contains(verbErr.Error(), want) {
			t.Errorf("the error must say %q: %v", want, verbErr)
		}
	}
	_ = epoch
}
