package gpulease

// Round 4, item 1. Inspect() and Leases() read through the reader, which treats a record that EXISTS but cannot be read as absent
// (legacy returns nil on any read error, v2 skips it). Every decision that a lease is gone must come from the record itself:
// Manager.RecordState says absent, held or unreadable, and unreadable is HELD. Reproduced on Windows and elsewhere by replacing
// the record with a directory that cannot be removed as a file.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// ITEM 3. AwaitHostRAM and the restamps turned a record they could not read into "the lease is gone" and dropped the real cause. A
// failed read is not a verdict: the wait looks again a few times before it says gone, and an unreadable record is reported as that,
// with the cause wrapped.
func TestAnUnreadableRecordIsNotTheLeaseIsGoneInRestampAndInTheWait(t *testing.T) {
	asleaseRecord := func(m *Manager, form string, l *Lease) string {
		if form == "card lease" {
			return epochRecordPath(m.leaseDir(), l.Epoch())
		}
		return m.metaPath()
	}
	for _, form := range []string{"card lease", "whole-node lease"} {
		t.Run(form+"/restamp", func(t *testing.T) {
			m, _ := ramScoped(t, 70)
			opts := Options{Reason: "film", TTL: time.Hour, HostRAMWaitGiB: 30}
			if form == "card lease" {
				opts.Devices = []string{card0}
			}
			l, err := m.TryAcquire(ClassMedia, opts)
			if err != nil {
				t.Fatal(err)
			}
			if form == "whole-node lease" && l.v2 {
				t.Skip("this manager grants card-scoped leases only")
			}
			restore := replaceWithDirectory(t, asleaseRecord(m, form, l))
			err = m.Restamp(l.Epoch(), func(mt *Meta) { mt.Reason = "x" })
			if err == nil || !IsRecordUnreadable(err) || strings.Contains(err.Error(), "is gone") {
				t.Errorf("a record that cannot be read is not \"the lease is gone\", and the cause stays reachable: %v", err)
			}
			restore()
			_ = l.Release()
			err = m.Restamp(l.Epoch(), func(mt *Meta) { mt.Reason = "x" })
			if err == nil || !strings.Contains(err.Error(), "the lease is gone") || !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an absent record is gone, with its cause wrapped: %v", err)
			}
		})
	}

	t.Run("the wait reports an unreadable record as that", func(t *testing.T) {
		m, _ := ramScoped(t, 70)
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
		if err != nil {
			t.Fatal(err)
		}
		restore := replaceWithDirectory(t, epochRecordPath(m.leaseDir(), l.Epoch()))
		defer func() { restore(); _ = l.Release() }()
		start := time.Now()
		err = l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: time.Now().Add(time.Minute)})
		if err == nil || !IsRecordUnreadable(err) || strings.Contains(err.Error(), "is gone") {
			t.Errorf("the wait over a record it cannot read says so and keeps the cause: %v", err)
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("it re-checks a few times and ends: %s", took)
		}
	})

	t.Run("the wait looks again before it says gone", func(t *testing.T) {
		m, _ := ramScoped(t, 70)
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Release() }()
		restore := replaceWithDirectory(t, epochRecordPath(m.leaseDir(), l.Epoch()))
		time.AfterFunc(150*time.Millisecond, restore) // the record is readable again before the third look
		err = l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: time.Now().Add(1500 * time.Millisecond)})
		asHostRAM(t, err) // the host never had the room; not "the lease is gone"
	})
}

// Restamp picks between a card lease's record and a whole-node lease's by looking at e/<epoch>.json. A look that FAILS for any reason
// but the file being absent (an access error while it is replaced) is no evidence of a whole-node lease: the dispatch must go to
// the card path, which reads the record and says "cannot be read" when it cannot, and never "the lease is gone" for a lease it
// could not even look at.
func TestRestampOfACardLeaseWhoseRecordCannotBeExaminedIsNeverTheLeaseIsGone(t *testing.T) {
	m, _ := ramScoped(t, 70)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	rec := epochRecordPath(m.leaseDir(), l.Epoch())
	defer UseStatFault(func(p string) error {
		if p == rec {
			return &fs.PathError{Op: "CreateFile", Path: p, Err: os.ErrPermission}
		}
		return nil
	})()

	stamp := func(reason string) func(*Meta) { return func(mt *Meta) { mt.Reason = reason } }
	for name, run := range map[string]func(reason string) error{
		"Restamp": func(reason string) error { return m.Restamp(l.Epoch(), stamp(reason)) },
		"restampLocked": func(reason string) error {
			return m.withEpochLock(func() error { return m.restampLocked(l.Epoch(), stamp(reason)) })
		},
	} {
		err := run(name)
		if err != nil && (strings.Contains(err.Error(), "is gone") || !IsRecordUnreadable(err)) {
			t.Errorf("%s over a record that could not be examined must not call the lease gone: %v", name, err)
		}
		if err == nil {
			// The card path read the record itself, and it is readable: the stamp lands.
			if got, rerr := readEpochRecord(m.leaseDir(), l.Epoch()); rerr != nil || got.Reason != name {
				t.Errorf("%s reported success but the record was not stamped: %v %+v", name, rerr, got)
			}
		}
	}
}

// Round 5, item 2. A record that EXISTS and cannot be READ (an I/O failure, not corrupt bytes) is held while its keeper is alive:
// the acquirer's sweep leaves the record and its claims alone while the holder's heartbeat is fresh, and sweeps them once that
// liveness is gone. A corrupt record is still debris after the grace window, as before.
func TestAnUnreadableCardRecordWhoseHolderIsAliveIsNotSweptAndItsClaimsStay(t *testing.T) {
	m := scopedRealClock(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Renew(); err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	rec := epochRecordPath(m.leaseDir(), epoch)
	old := time.Now().Add(-time.Minute) // older than the grace a claim in progress gets
	if err := os.Chtimes(rec, old, old); err != nil {
		t.Fatal(err)
	}
	defer UseReadFault(func(p string) error {
		if p == rec {
			return &fs.PathError{Op: "open", Path: p, Err: os.ErrPermission}
		}
		return nil
	})()

	other, err := m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, Devices: []string{card0}})
	if err == nil {
		_ = other.Release()
		t.Fatal("a second lease was granted on a card whose holder is alive and whose record merely cannot be read")
	}
	if _, serr := os.Stat(rec); serr != nil {
		t.Errorf("the record of a live holder was swept: %v", serr)
	}
	if len(m.ClaimsOf(epoch)) == 0 {
		t.Error("the claims of a live holder were swept")
	}
	// A whole-node request is refused too: the lease is invisible to the reader, but it is there.
	if w, werr := m.TryAcquire(ClassMedia, Options{Reason: "whole node", TTL: time.Hour}); werr == nil {
		_ = w.Release()
		t.Error("a whole-node lease was granted over a card lease whose holder is alive and whose record cannot be read")
	}

	// The keeper's liveness goes: its heartbeat is stale and the record has not been written for minutes. Now it is debris.
	stale := time.Now().Add(-10 * time.Minute)
	if err := os.WriteFile(m.heartbeatPath(epoch), []byte(strconv.FormatInt(stale.UnixMilli(), 10)), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(m.heartbeatPath(epoch), stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rec, stale, stale); err != nil {
		t.Fatal(err)
	}
	next, err := m.TryAcquire(ClassMedia, Options{Reason: "after", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatalf("once its keeper's heartbeat is stale the unreadable record is debris and the card is free: %v", err)
	}
	_ = next.Release()
}
