package gpulease

// Round 4, item 1. Inspect() and Leases() read through the reader, which treats a record that EXISTS but cannot be read as absent
// (legacy returns nil on any read error, v2 skips it). Every decision that a lease is gone must come from the record itself:
// Manager.RecordState says absent, held or unreadable, and unreadable is HELD. Reproduced on Windows and elsewhere by replacing
// the record with a directory that cannot be removed as a file.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// replaceWithDirectory puts a non-empty directory where the file was: it exists, cannot be read as a file, and cannot be removed by
// a release that removes files.
func replaceWithDirectory(t *testing.T, path string) (restore func()) {
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

func TestRecordStateSaysAbsentHeldOrUnreadableAndUnreadableIsNeverGone(t *testing.T) {
	for _, form := range []string{"card lease", "whole-node lease"} {
		t.Run(form, func(t *testing.T) {
			m, _ := ramScoped(t, 10)
			opts := Options{Reason: "film", TTL: time.Hour}
			if form == "card lease" {
				opts.Devices = []string{card0}
			}
			l, err := m.TryAcquire(ClassMedia, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Release() }()
			if form == "whole-node lease" && l.v2 {
				t.Skip("this manager grants card-scoped leases only")
			}
			path := m.metaPath()
			if form == "card lease" {
				path = epochRecordPath(m.leaseDir(), l.Epoch())
			}

			if st, err := m.RecordState(l.Epoch()); st != RecordHeld || err != nil {
				t.Errorf("a readable record is held: %v %v", st, err)
			}
			if st, err := m.RecordState(l.Epoch() + 77); st != RecordAbsent || err != nil {
				t.Errorf("an epoch no record names is absent: %v %v", st, err)
			}

			restore := replaceWithDirectory(t, path)
			st, err := m.RecordState(l.Epoch())
			if st != RecordUnreadable || !IsRecordUnreadable(err) {
				t.Errorf("a record that exists and cannot be read is unreadable, never gone: %v %v", st, err)
			}
			if form == "card lease" {
				// The reader hides it, which is the defect: Inspect says free while the record is there.
				if m.Inspect().Held {
					t.Log("the reader still sees the lease (fine: the point is RecordState does not depend on it)")
				}
			}

			// A whole-node lease's Release returns the failure; it used to answer nil, "already gone".
			if form == "whole-node lease" {
				relErr := l.Release()
				if relErr == nil || !IsRecordUnreadable(relErr) || !strings.Contains(relErr.Error(), "releasing lease") {
					t.Errorf("Release over a record it cannot read returns a wrapped *RecordUnreadableError, not nil: %v", relErr)
				}
				var ru *RecordUnreadableError
				if !errors.As(relErr, &ru) || ru.Epoch != l.Epoch() {
					t.Errorf("the error names the epoch: %v", relErr)
				}
			}
			restore()
			if st, err := m.RecordState(l.Epoch()); st != RecordHeld || err != nil {
				t.Errorf("the record is readable again: %v %v", st, err)
			}
			_ = l.Release()
			if st, _ := m.RecordState(l.Epoch()); st != RecordAbsent {
				t.Errorf("a released lease is absent: %v", st)
			}
		})
	}
}
