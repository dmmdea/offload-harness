package gpulease

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// A record that EXISTS and cannot be READ is held while its keeper is alive (keeper.go), and the keeper's liveness is its heartbeat.
// The heartbeat is replaced by a rename over the live name WITHOUT the epoch lock, so under the lock that sweeps and grants it is the
// one lease file that can be in flight. A heartbeat that cannot be read or examined is UNKNOWN, as it is for the reclaim rule
// (heartbeatAt), and unknown is no evidence that the holder is gone: reading it as "no sign of life" had the sweep remove the record
// and the claims of a live holder (a record that is old, a heartbeat mid-rename) and the next grant take its cards.

// unreadableRecordFixture takes a card lease and renews it, makes its record old and unreadable (an I/O failure that is not absence) and
// returns the pieces a test needs.
func unreadableRecordFixture(t *testing.T) (m *Manager, l *Lease, rec, hb string) {
	t.Helper()
	m = scopedRealClock(t)
	var err error
	l, err = m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if err := l.Renew(); err != nil {
		t.Fatal(err)
	}
	rec = epochRecordPath(m.leaseDir(), l.Epoch())
	hb = m.heartbeatPath(l.Epoch())
	old := time.Now().Add(-10 * time.Minute) // past the heartbeat TTL and the grace of a claim in progress: the record says nothing
	if err := os.Chtimes(rec, old, old); err != nil {
		t.Fatal(err)
	}
	restore := UseReadFault(func(p string) error {
		if p == rec {
			return &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
		}
		return nil
	})
	t.Cleanup(restore)
	return m, l, rec, hb
}

func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestAnUnreadableRecordWhoseHeartbeatIsUnknownIsHeldAndNotSwept(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, m *Manager, hb string)
	}{
		{"the heartbeat is mid-rename", func(t *testing.T, m *Manager, hb string) { inFlight(t, hb) }},
		{"the heartbeat can be neither read nor examined", func(t *testing.T, m *Manager, hb string) {
			withHeartbeatFaults(t, errHBBoom, errHBBoom)
		}},
		{"the heartbeat cannot be read but its file was written a moment ago", func(t *testing.T, m *Manager, hb string) {
			withHeartbeatFaults(t, errHBBoom, nil)
		}},
		{"the heartbeat is torn but its file was written a moment ago", func(t *testing.T, m *Manager, hb string) {
			if err := os.WriteFile(hb, []byte("12ab"), 0o666); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, l, rec, hb := unreadableRecordFixture(t)
			epoch := l.Epoch()
			c.arrange(t, m, hb)

			other, err := m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, Devices: []string{card0}})
			if err == nil {
				_ = other.Release()
				t.Errorf("a second lease (epoch %d) was granted on the card of a live holder whose record cannot be read, while %s", other.Epoch(), c.name)
			}
			if _, serr := os.Stat(rec); serr != nil {
				t.Errorf("the record of a live holder was swept while %s: %v", c.name, serr)
			}
			if len(m.ClaimsOf(epoch)) == 0 {
				t.Errorf("the claims of a live holder were swept while %s", c.name)
			}
			if w, werr := m.TryAcquire(ClassMedia, Options{Reason: "whole node", TTL: time.Hour}); werr == nil {
				_ = w.Release()
				t.Errorf("a whole-node lease was granted over a live card lease whose record cannot be read, while %s", c.name)
			}
		})
	}
}

// What stays as it was: a heartbeat that can be examined is aged by its file's time, so a dead keeper is still found out, and one the
// holder never wrote is still no sign.
func TestAnUnreadableRecordWhoseKeeperIsGoneIsStillDebris(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, m *Manager, hb string)
	}{
		{"the heartbeat was never written", func(t *testing.T, m *Manager, hb string) {
			if err := os.Remove(hb); err != nil {
				t.Fatal(err)
			}
		}},
		{"the heartbeat is unreadable and its file is old", func(t *testing.T, m *Manager, hb string) {
			ageFile(t, hb, 10*time.Minute)
			withHeartbeatFaults(t, errHBBoom, nil)
		}},
		{"the heartbeat is torn and its file is old", func(t *testing.T, m *Manager, hb string) {
			if err := os.WriteFile(hb, []byte("12ab"), 0o666); err != nil {
				t.Fatal(err)
			}
			ageFile(t, hb, 10*time.Minute)
		}},
		{"the heartbeat holds a time from minutes ago", func(t *testing.T, m *Manager, hb string) {
			stale := time.Now().Add(-10 * time.Minute)
			if err := os.WriteFile(hb, []byte(strconv.FormatInt(stale.UnixMilli(), 10)), 0o666); err != nil {
				t.Fatal(err)
			}
			ageFile(t, hb, 10*time.Minute)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _, _, hb := unreadableRecordFixture(t)
			c.arrange(t, m, hb)
			next, err := m.TryAcquire(ClassMedia, Options{Reason: "after", TTL: time.Hour, Devices: []string{card0}})
			if err != nil {
				t.Fatalf("an unreadable record whose keeper is gone (%s) is debris and the card is free: %v", c.name, err)
			}
			_ = next.Release()
		})
	}
}

// The same holds for a whole-node lease, whose record is meta.json. Its keeper cannot name the epoch of the record it cannot read, so it
// looks at every heartbeat in the lease directory (keeperEvidence, epoch 0), and that is a LISTING of the directory. A heartbeat whose
// rename is installing it has its live name absent from the listing and only its staged file in it, so a scan that skips staged names
// finds no heartbeat at all, and reads the holder as gone. A listing taken in that instant must be told as unknown, as a launch marker's is
// (tree.go).
func TestAnUnreadableWholeNodeRecordWhoseHeartbeatIsMidRenameIsHeldAndNotSwept(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, m *Manager, hb string)
	}{
		{"the heartbeat is mid-rename: its live name is not listed and its staged file stands in its place", func(t *testing.T, m *Manager, hb string) { inFlight(t, hb) }},
		{"the heartbeat is listed but can be neither read nor examined", func(t *testing.T, m *Manager, hb string) {
			withHeartbeatFaults(t, errHBBoom, errHBBoom)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := scopedRealClock(t)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Release() })
			if err := l.Renew(); err != nil {
				t.Fatal(err)
			}
			rec := m.metaPath()
			ageFile(t, rec, 10*time.Minute) // past the heartbeat TTL and the grace of a claim in progress: the record says nothing
			restore := UseReadFault(func(p string) error {
				if p == rec {
					return &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
				}
				return nil
			})
			t.Cleanup(restore)
			c.arrange(t, m, m.heartbeatPath(l.Epoch()))

			other := asPID(m, os.Getpid()+1)
			if w, werr := other.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour}); werr == nil {
				_ = w.Release()
				t.Errorf("a second whole-node lease (epoch %d) was granted over a live whole-node holder (epoch %d) whose record cannot be read, while %s", w.Epoch(), l.Epoch(), c.name)
			}
			if _, serr := os.Stat(rec); serr != nil {
				t.Errorf("the record of a live whole-node holder was swept while %s: %v", c.name, serr)
			}
			if cl, cerr := other.TryAcquire(ClassMedia, Options{Reason: "card", TTL: time.Hour, Devices: []string{card0}}); cerr == nil {
				_ = cl.Release()
				t.Errorf("a card lease (epoch %d) was granted over a live whole-node holder (epoch %d) whose record cannot be read, while %s", cl.Epoch(), l.Epoch(), c.name)
			}
		})
	}
}
