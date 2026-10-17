package gpulease

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

// A heartbeat that cannot be read says nothing about the holder, so it must not make a lease stale.
//
// The reclaim rule needs a stale heartbeat AND a lapsed window. Once a window has lapsed (a `gpu reserve` command that outran its
// --for, a detached hold past its window) the heartbeat is the only thing that keeps the lease, and the holder replaces it every 15 s
// with a rename over the live name, WITHOUT the epoch lock. On Windows a successful rename can leave the live name absent for tens of
// milliseconds while the staged file beside it still exists. A reader that took that for "this holder never renewed" fell back to
// the stamp written at acquisition, which is hours old by then, so a live lease read as stale, an acquirer's probe said the card was
// free, its locked sweep deleted the live lease, and the card was granted to a second job while the first still ran on it.
//
// These tests model the episode on disk (the live name renamed away, the staged file left in place), so they hold whatever the reader
// does about the wait, and they are the same on every platform: a heartbeat in that state is unknown, and unknown is not stale.

// errHBBoom is an I/O failure that is neither "absent" nor a permission error: what a reader gets when a wait for a rename gave up.
var errHBBoom = errors.New("heartbeat could not be examined")

// overdueLease takes a lease with a 50 ms window, moves the clock 10 minutes on (past the window, and the acquisition stamp is far
// older than the heartbeat TTL) and renews it, so the heartbeat file holds the current time and the record's own stamp is stale. The
// lease is a card lease when devices are named and a whole-node one when none are.
func overdueLease(t *testing.T, m *Manager, now *time.Time, devs ...string) *Lease {
	t.Helper()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "long job", Devices: devs, TTL: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("taking the lease: %v", err)
	}
	*now = now.Add(10 * time.Minute)
	if err := l.Renew(); err != nil {
		t.Fatalf("renewing the lease past its window: %v", err)
	}
	rec := leaseRecordOf(t, m, l)
	if now.UnixMilli()-rec.RenewedAtMs <= m.heartbeatTTL.Milliseconds() || now.UnixMilli() <= rec.ExpiresAtMs {
		t.Fatalf("setup: the lease must be past its window with a stale acquisition stamp (now %d, renewed_at %d, expires_at %d)", now.UnixMilli(), rec.RenewedAtMs, rec.ExpiresAtMs)
	}
	return l
}

// leaseRecordOf reads the record of a lease, a card lease's or a whole-node one's.
func leaseRecordOf(t *testing.T, m *Manager, l *Lease) *Meta {
	t.Helper()
	if l.v2 {
		rec, err := readEpochRecord(m.leaseDir(), l.Epoch())
		if err != nil {
			t.Fatalf("the record of epoch %d: %v", l.Epoch(), err)
		}
		return rec
	}
	rec, err := m.readMeta()
	if err != nil {
		t.Fatalf("the record of the whole-node lease: %v", err)
	}
	return rec
}

// inFlight puts the heartbeat in the state a reader meets mid-rename: the live name absent and the staged file beside it, just written.
func inFlight(t *testing.T, hb string) {
	t.Helper()
	if err := os.Rename(hb, hb+".tmp"); err != nil {
		t.Fatalf("staging the heartbeat aside: %v", err)
	}
	fresh := time.Now()
	if err := os.Chtimes(hb+".tmp", fresh, fresh); err != nil {
		t.Fatal(err)
	}
}

// refusedAsHeld asks for the same cards (or the whole node) as another process and wants *ErrHeld. A grant is released again.
func refusedAsHeld(t *testing.T, m *Manager, what string, devs ...string) {
	t.Helper()
	other := asPID(m, os.Getpid()+1)
	l, err := other.TryAcquire(ClassMedia, Options{Reason: "second job", Devices: devs, TTL: time.Hour})
	if err == nil {
		_ = l.Release()
		t.Errorf("the lease of a live holder was granted to a second job (epoch %d) %s", l.Epoch(), what)
		return
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Errorf("the second job was refused with %v %s, want *ErrHeld", err, what)
	}
}

// TestAnInFlightHeartbeatNeverMakesAnOverdueLiveLeaseReclaimable is the reclaim injection, for every pairing of a whole-node lease and
// a card lease: the holder's window has lapsed, its acquisition stamp is minutes old, and its heartbeat is mid-rename. A second
// acquirer must be refused, the holder must still be listed and not fenced out, and the holder's next renewal must still work.
func TestAnInFlightHeartbeatNeverMakesAnOverdueLiveLeaseReclaimable(t *testing.T) {
	cases := []struct {
		name         string
		holder, next []string
	}{
		{"card lease, second job wants the same card", []string{card0}, []string{card0}},
		{"whole-node lease, second job wants the whole node", nil, nil},
		{"whole-node lease, second job wants a card", nil, []string{card0}},
		{"card lease, second job wants the whole node", []string{card0}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, now := scopedManager(t)
			l := overdueLease(t, m, now, c.holder...)
			defer l.Release()
			wantRecord := m.metaPath()
			if len(c.holder) > 0 {
				wantRecord = epochRecordPath(m.leaseDir(), l.Epoch())
			}
			if _, err := os.Stat(wantRecord); err != nil {
				t.Fatalf("setup: the lease has no record at %s: %v", wantRecord, err)
			}
			hb := m.heartbeatPath(l.Epoch())
			// Control: with the heartbeat in place the lease is held and refused to a second job.
			refusedAsHeld(t, m, "although its heartbeat is fresh (control)", c.next...)
			if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != l.Epoch() {
				t.Fatalf("control: Leases() = %v with the heartbeat in place, want [%d]", got, l.Epoch())
			}

			inFlight(t, hb)
			if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != l.Epoch() {
				t.Errorf("Leases() = %v while the holder's heartbeat is mid-rename, want [%d]", got, l.Epoch())
			}
			refusedAsHeld(t, m, "because its heartbeat was mid-rename", c.next...)
			if err := l.Check(); err != nil {
				t.Errorf("the holder was fenced out: %v", err)
			}

			// The rename ends: the holder's next renewal works and the lease is still its own.
			_ = os.Remove(hb + ".tmp")
			if err := l.Renew(); err != nil {
				t.Errorf("the holder's next renewal: %v", err)
			}
			if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != l.Epoch() {
				t.Errorf("Leases() = %v after the rename ended, want [%d]", got, l.Epoch())
			}
		})
	}
}

// withHeartbeatFaults makes the heartbeat files fail the way a reader sees them when a wait for a rename has given up: the open fails
// with an I/O error, and so does the stat if statErr is set. Other files are untouched.
func withHeartbeatFaults(t *testing.T, openErr, statErr error) {
	t.Helper()
	isHB := func(path string) bool {
		return strings.HasPrefix(filepath.Base(path), "hb.") && !strings.HasSuffix(path, ".tmp")
	}
	oldOpen := openSharedFile
	if openErr != nil {
		openSharedFile = func(path string) (*os.File, error) {
			if isHB(path) {
				return nil, &fs.PathError{Op: "open", Path: path, Err: openErr}
			}
			return oldOpen(path)
		}
	}
	restoreStat := func() {}
	if statErr != nil {
		restoreStat = UseStatFault(func(path string) error {
			if isHB(path) {
				return &fs.PathError{Op: "stat", Path: path, Err: statErr}
			}
			return nil
		})
	}
	t.Cleanup(func() { openSharedFile = oldOpen; restoreStat() })
}

// TestAHeartbeatThatCannotBeReadOrExaminedIsUnknownAndUnknownIsNotStale: the read fails with an I/O error (not absent, not a
// permission error) and so does the look at the file. Nothing is known about the holder, so nothing may be taken from it.
func TestAHeartbeatThatCannotBeReadOrExaminedIsUnknownAndUnknownIsNotStale(t *testing.T) {
	for _, c := range []struct {
		name string
		devs []string
	}{{"card lease", []string{card0}}, {"whole-node lease", nil}} {
		t.Run(c.name, func(t *testing.T) {
			m, now := scopedManager(t)
			l := overdueLease(t, m, now, c.devs...)
			defer l.Release()
			withHeartbeatFaults(t, errHBBoom, errHBBoom)
			refusedAsHeld(t, m, "because its heartbeat could be neither read nor examined", c.devs...)
			if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != l.Epoch() {
				t.Errorf("Leases() = %v while the heartbeat can be neither read nor examined, want [%d]", got, l.Epoch())
			}
		})
	}
}

// TestAnUnreadableHeartbeatIsAgedByItsFileTime: the content cannot be read but the file can be examined, so its modification time
// stands in for the heartbeat. Renew writes the staged file and renames it over the live name, so a heartbeat renewed a moment ago
// has a fresh time and an unreadable one that nobody renews ages out like any other.
func TestAnUnreadableHeartbeatIsAgedByItsFileTime(t *testing.T) {
	for _, c := range []struct {
		name string
		devs []string
	}{{"card lease", []string{card0}}, {"whole-node lease", nil}} {
		t.Run(c.name+", just renewed", func(t *testing.T) {
			m, now := scopedManager(t)
			l := overdueLease(t, m, now, c.devs...)
			defer l.Release()
			if err := os.Chtimes(m.heartbeatPath(l.Epoch()), *now, *now); err != nil {
				t.Fatal(err)
			}
			withHeartbeatFaults(t, errHBBoom, nil)
			refusedAsHeld(t, m, "although its heartbeat file was written a moment ago", c.devs...)
		})
		t.Run(c.name+", not renewed for ten minutes", func(t *testing.T) {
			m, now := scopedManager(t)
			l := overdueLease(t, m, now, c.devs...)
			defer l.Release()
			old := now.Add(-10 * time.Minute)
			if err := os.Chtimes(m.heartbeatPath(l.Epoch()), old, old); err != nil {
				t.Fatal(err)
			}
			withHeartbeatFaults(t, errHBBoom, nil)
			next, err := asPID(m, os.Getpid()+1).TryAcquire(ClassMedia, Options{Reason: "second job", Devices: c.devs, TTL: time.Hour})
			if err != nil {
				t.Fatalf("a heartbeat nobody has renewed for ten minutes must still let a lapsed lease go: %v", err)
			}
			_ = next.Release()
		})
	}
}

// TestAnEmptyOrUnparseableHeartbeatIsAgedByItsFileTimeNotByTheAcquisitionStamp: a torn write (the in-place fallback of Renew, or a
// truncate before the write) leaves a heartbeat file whose content says nothing. A fresh one is a holder that has just renewed; an old
// one is a holder that has not for ten minutes.
func TestAnEmptyOrUnparseableHeartbeatIsAgedByItsFileTimeNotByTheAcquisitionStamp(t *testing.T) {
	for _, content := range []string{"", "garbage", "12ab"} {
		for _, devs := range [][]string{{card0}, nil} {
			kind := "whole-node lease"
			if len(devs) > 0 {
				kind = "card lease"
			}
			name := kind + ", content " + strconv.Quote(content)
			t.Run(name+", written a moment ago", func(t *testing.T) {
				m, now := scopedManager(t)
				l := overdueLease(t, m, now, devs...)
				defer l.Release()
				hb := m.heartbeatPath(l.Epoch())
				if err := os.WriteFile(hb, []byte(content), 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(hb, *now, *now); err != nil {
					t.Fatal(err)
				}
				refusedAsHeld(t, m, "although its heartbeat file was written a moment ago", devs...)
				if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != l.Epoch() {
					t.Errorf("Leases() = %v with a torn but fresh heartbeat, want [%d]", got, l.Epoch())
				}
			})
			t.Run(name+", ten minutes old", func(t *testing.T) {
				m, now := scopedManager(t)
				l := overdueLease(t, m, now, devs...)
				defer l.Release()
				hb := m.heartbeatPath(l.Epoch())
				if err := os.WriteFile(hb, []byte(content), 0o666); err != nil {
					t.Fatal(err)
				}
				old := now.Add(-10 * time.Minute)
				if err := os.Chtimes(hb, old, old); err != nil {
					t.Fatal(err)
				}
				next, err := asPID(m, os.Getpid()+1).TryAcquire(ClassMedia, Options{Reason: "second job", Devices: devs, TTL: time.Hour})
				if err != nil {
					t.Fatalf("a corrupt heartbeat nobody has renewed for ten minutes must still let a lapsed lease go: %v", err)
				}
				_ = next.Release()
			})
		}
	}
}

// TestAHeartbeatThatIsSettledAbsentStillFallsBackToTheAcquisitionStamp: a holder that never renewed (no heartbeat file, and no staged
// file that a rename could still be installing) is judged by the stamp from the acquisition, as before. So is a staged file left by a
// writer that died long ago: it is not a rename in flight.
func TestAHeartbeatThatIsSettledAbsentStillFallsBackToTheAcquisitionStamp(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, hb string)
	}{
		{"never renewed", func(t *testing.T, hb string) { _ = os.Remove(hb) }},
		{"a staged file left by a writer that died long ago", func(t *testing.T, hb string) {
			if err := os.Rename(hb, hb+".tmp"); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Minute)
			if err := os.Chtimes(hb+".tmp", old, old); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, devs := range [][]string{{card0}, nil} {
			kind := "whole-node lease"
			if len(devs) > 0 {
				kind = "card lease"
			}
			t.Run(kind+", "+c.name, func(t *testing.T) {
				m, now := scopedManager(t)
				l := overdueLease(t, m, now, devs...)
				defer l.Release()
				c.setup(t, m.heartbeatPath(l.Epoch()))
				next, err := asPID(m, os.Getpid()+1).TryAcquire(ClassMedia, Options{Reason: "second job", Devices: devs, TTL: time.Hour})
				if err != nil {
					t.Fatalf("a lapsed lease whose holder never renewed must let go: %v", err)
				}
				_ = next.Release()
			})
		}
	}
}

// TestAnUnknownHeartbeatDoesNotChangeTheOtherHalvesOfTheRule: the guards that already hold stay, and they do not depend on the
// heartbeat: a lease inside its declared window, or younger than the heartbeat TTL, is held whatever the heartbeat looks like; and a
// lease whose holder is gone is reclaimed whatever the heartbeat looks like (the wrapper-gone half of the rule).
func TestAnUnknownHeartbeatDoesNotChangeTheOtherHalvesOfTheRule(t *testing.T) {
	t.Run("inside its window", func(t *testing.T) {
		m, now := scopedManager(t)
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "long job", Devices: []string{card0}, TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		*now = now.Add(10 * time.Minute)
		if err := l.Renew(); err != nil {
			t.Fatal(err)
		}
		inFlight(t, m.heartbeatPath(l.Epoch()))
		refusedAsHeld(t, m, "inside its declared window", card0)
	})
	t.Run("younger than the heartbeat TTL", func(t *testing.T) {
		m, now := scopedManager(t)
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "long job", Devices: []string{card0}, TTL: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		*now = now.Add(time.Minute)
		if err := l.Renew(); err != nil {
			t.Fatal(err)
		}
		inFlight(t, m.heartbeatPath(l.Epoch()))
		refusedAsHeld(t, m, "younger than the heartbeat TTL", card0)
	})
	for _, devs := range [][]string{{card0}, nil} {
		kind := "whole-node lease"
		if len(devs) > 0 {
			kind = "card lease"
		}
		t.Run(kind+", a recycled pid with an unreadable heartbeat is reclaimed by the wrapper-gone half", func(t *testing.T) {
			m, now := scopedManager(t)
			l := overdueLease(t, m, now, devs...)
			defer l.Release()
			inFlight(t, m.heartbeatPath(l.Epoch()))
			// The pid is alive but it is a different process now.
			m.procStart = func(int) (int64, bool) { return 9999, true }
			next, err := asPID(m, os.Getpid()+1).TryAcquire(ClassMedia, Options{Reason: "second job", Devices: devs, TTL: time.Hour})
			if err != nil {
				t.Fatalf("a lease whose holder's pid was recycled must be reclaimed whatever its heartbeat looks like: %v", err)
			}
			_ = next.Release()
		})
	}
}

// TestARenewedHeartbeatCarriesTheFreshFileTimeOfItsStagedFile: Renew writes a staged file and renames it over the live name, so the
// heartbeat that is in place after a renewal has the time of that write, never the age of the file it replaced. A renewed heartbeat
// whose content cannot be read is aged by this time (TestAnUnreadableHeartbeatIsAgedByItsFileTime), so it must be fresh.
func TestARenewedHeartbeatCarriesTheFreshFileTimeOfItsStagedFile(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	hb := m.heartbeatPath(l.Epoch())
	if err := l.Renew(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}
	if err := l.Renew(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(hb)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(fi.ModTime()); age > 10*time.Second || age < -10*time.Second {
		t.Errorf("the heartbeat renewed a moment ago has a file time %v away from now, want the time of the renewal", age)
	}
}
