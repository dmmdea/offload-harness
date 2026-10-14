package gpulease

// Review round 3, items B and C (the library halves).
//
// C: Lease.Check answered "the lease is gone" for ANY failure to read the record, so the hidden holder could not tell a verdict
// (gone, fenced out) from a read that failed for a moment or for good (an access error, a sharing violation) and let go of a live
// lease in both. A failed READ is its own error now.
//
// B: a holder whose own release failed marks the record, best effort, so `gpu status` does not promise a release by a holder that
// has gone.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckSaysTheRecordCannotBeReadOnlyForAFailedRead(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	path := epochRecordPath(m.leaseDir(), l.Epoch())
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		_ = os.Remove(path)
		if err := os.WriteFile(path, saved, 0o666); err != nil {
			t.Fatal(err)
		}
	}

	// The record is there as a directory: it exists and cannot be read as a file. A failed read, not a verdict.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o777); err != nil {
		t.Fatal(err)
	}
	err = l.Check()
	if err == nil || !IsRecordUnreadable(err) {
		t.Errorf("a record that cannot be read is a failed read, which may pass: %v", err)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("epoch %d", l.Epoch())) {
		t.Errorf("the error names the epoch: %v", err)
	}
	_ = os.Remove(path)

	// Absent: the lease is gone, a verdict.
	err = l.Check()
	if err == nil || IsRecordUnreadable(err) || !strings.Contains(err.Error(), "gone") {
		t.Errorf("an absent record is \"the lease is gone\": %v", err)
	}

	// Corrupt bytes: not a record, a verdict too (retrying cannot make them one).
	if err := os.WriteFile(path, []byte("{not json"), 0o666); err != nil {
		t.Fatal(err)
	}
	err = l.Check()
	if err == nil || IsRecordUnreadable(err) {
		t.Errorf("corrupt bytes are not a failed read: %v", err)
	}
	restore()
	if err := l.Check(); err != nil {
		t.Fatalf("the lease is fine again: %v", err)
	}
}

func TestOnlyAFileSystemFailureThatIsNotAbsenceIsAnIOFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"access denied", &fs.PathError{Op: "open", Path: "r", Err: errors.New("access denied")}, true},
		{"absent", &fs.PathError{Op: "open", Path: "r", Err: fs.ErrNotExist}, false},
		{"not json", fmt.Errorf("invalid character"), false},
		{"nil", nil, false},
	} {
		if got := isIOFailure(tc.err); got != tc.want {
			t.Errorf("%s: isIOFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAHolderThatGaveUpLeavesAMarkerOnTheRecordThatStatusCanRead(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if rec := recordOf(t, m, l.Epoch()); !rec.HolderGaveUp.IsZero() {
		t.Fatalf("no holder has given up yet: %v", rec.HolderGaveUp)
	}
	before := time.Now()
	if err := m.MarkHolderGaveUp(l.Epoch(), "access denied: "+strings.Repeat("x", 500)); err != nil {
		t.Fatal(err)
	}
	rec := recordOf(t, m, l.Epoch())
	if rec.HolderGaveUp.IsZero() || rec.HolderGaveUp.Before(before.Add(-time.Second)) {
		t.Errorf("the marker carries the moment the holder gave up: %v", rec.HolderGaveUp)
	}
	if !strings.HasPrefix(rec.HolderGaveUpWhy, "access denied") || len(rec.HolderGaveUpWhy) > 300 {
		t.Errorf("the marker carries the reason, clipped: %d bytes %q", len(rec.HolderGaveUpWhy), rec.HolderGaveUpWhy)
	}
	if err := m.MarkHolderGaveUp(l.Epoch()+50, "x"); err == nil {
		t.Error("a lease that is not on the record cannot be marked")
	}
}

// The same distinction on a whole-node lease, whose fence is meta.json.
func TestCheckOfAWholeNodeLeaseSaysTheRecordCannotBeReadOnlyForAFailedRead(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if l.v2 {
		t.Skip("this manager grants card-scoped leases only")
	}
	defer func() { _ = l.Release() }()
	path := m.metaPath()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o777); err != nil { // exists, cannot be read as a file
		t.Fatal(err)
	}
	err = l.Check()
	_ = os.Remove(path)
	if !IsRecordUnreadable(err) {
		t.Errorf("a whole-node record that cannot be read is a failed read: %v", err)
	}
	if err := l.Check(); err == nil || IsRecordUnreadable(err) {
		t.Errorf("an absent record is a verdict: %v", err)
	}
	if err := os.WriteFile(path, saved, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Errorf("the lease is fine again: %v", err)
	}
}
