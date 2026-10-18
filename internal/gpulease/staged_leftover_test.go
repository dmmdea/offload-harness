package gpulease

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A staged file with no live name beside it is one of two things: a write that may still be installing (a fresh staged file: the
// rename has not ended), or the leftover of a writer that died. The scans that work from a LISTING count the first as "cannot be
// told", which keeps a holder alive and a record held, and must not count the second: nothing sweeps a dead writer's heartbeat
// leftover, so one counted would hold an unreadable whole-node record until somebody removed the file by hand. What tells them apart
// is the age of the staged file against stagingFresh (settled.go). The boundary is pinned on both sides.

var stagedLeftoverAges = []struct {
	name string
	age  time.Duration
	live bool // a rename that may still be installing
}{
	{"a staged file just under the bound may still be installing", stagingFresh - time.Second, true},
	{"a staged file just over the bound is a dead writer's leftover", stagingFresh + time.Second, false},
	{"a staged file ten minutes old is a dead writer's leftover", 10 * time.Minute, false},
}

// An unreadable whole-node record whose only heartbeat is a staged file with no live name: a first renew that died after it staged
// the heartbeat (or a rename that is still installing it). Past the bound the holder is not shown to be alive and the node is free.
func TestAnUnreadableWholeNodeRecordWhoseOnlyHeartbeatIsAStagedLeftoverFollowsTheAgeOfThatFile(t *testing.T) {
	for _, tc := range stagedLeftoverAges {
		t.Run(tc.name, func(t *testing.T) {
			clk := onGateClock(t)
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
			ageFile(t, rec, 10*time.Minute) // past the heartbeat TTL: the record says nothing
			restore := UseReadFault(func(p string) error {
				if p == rec {
					return &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
				}
				return nil
			})
			t.Cleanup(restore)
			hb := m.heartbeatPath(l.Epoch())
			if err := os.Remove(hb); err != nil {
				t.Fatal(err)
			}
			stageBeside(t, clk, hb, tc.age)

			if _, unknown := m.keeperEvidence(0, rec); unknown != tc.live {
				t.Errorf("keeperEvidence(0) with a staged heartbeat %v old and no live name: unknown=%v, want %v", tc.age, unknown, tc.live)
			}
			other := asPID(m, os.Getpid()+1)
			w, werr := other.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour})
			switch {
			case tc.live && werr == nil:
				_ = w.Release()
				t.Errorf("a second whole-node lease (epoch %d) was granted over an unreadable record whose heartbeat may still be installing", w.Epoch())
			case !tc.live && werr != nil:
				t.Errorf("an unreadable whole-node record whose only heartbeat is a dead writer's old staged leftover is debris and the node is free: %v", werr)
			case !tc.live:
				_ = w.Release()
			}
		})
	}
}

// The card-record twin: a staged card record with no live name. `gpu release` with no epoch must not refuse over a dead writer's
// leftover for a lease that does not exist, and must not release the one lease it can see while a record may be installing.
func TestAnOldStagedCardRecordWithNoLiveNameIsNotARecordThatCannotBeRead(t *testing.T) {
	for _, tc := range stagedLeftoverAges {
		t.Run(tc.name, func(t *testing.T) {
			clk := onGateClock(t)
			m, _ := scopedManager(t)
			a := mustDevices(t, m, card0)
			ghost := a.Epoch() + 50
			stageBeside(t, clk, epochRecordPath(m.leaseDir(), ghost), tc.age)

			bad := m.unreadableCardRecords()
			if tc.live {
				if len(bad) != 1 || bad[0] != ghost {
					t.Errorf("unreadableCardRecords with a staged card record %v old and no live name = %v, want [%d]", tc.age, bad, ghost)
				}
				if target, err := m.ReleaseTarget(0); err == nil {
					t.Errorf("ReleaseTarget(0) = (%d, nil) with a record that may still be installing, want a refusal", target)
				}
				return
			}
			if len(bad) != 0 {
				t.Errorf("unreadableCardRecords with a staged card record %v old and no live name = %v, want none: it is a dead writer's leftover", tc.age, bad)
			}
			if target, err := m.ReleaseTarget(0); err != nil || target != a.Epoch() {
				t.Errorf("ReleaseTarget(0) = (%d, %v) with an old staged leftover beside the one live lease, want (%d, nil)", target, err, a.Epoch())
			}
		})
	}
}

// A staged heartbeat beside a PRESENT live name is a renew in progress by a holder whose live file reads on its own: the instant
// says nothing the live file does not, and it is not a heartbeat that cannot be told.
func TestAStagedHeartbeatBesideItsLiveNameIsNotAHeartbeatThatCannotBeTold(t *testing.T) {
	clk := onGateClock(t)
	m := scopedRealClock(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if err := l.Renew(); err != nil {
		t.Fatal(err)
	}
	stageBeside(t, clk, m.heartbeatPath(l.Epoch()), 0) // a renew that has staged its next heartbeat and not yet renamed it
	sign, unknown := m.keeperEvidence(0, m.metaPath())
	if unknown {
		t.Errorf("keeperEvidence(0) with a staged heartbeat beside a live one that reads: unknown=true, want false: the live file tells")
	}
	if sign.IsZero() {
		t.Errorf("the live heartbeat is a sign of life and was not noted")
	}
}

// The refusal for an unreadable record says what it rests on. A sign of life within the heartbeat TTL is a heartbeat or a write; an
// UNKNOWN heartbeat is no sign at all, the holder is only not shown to be gone, and what ends that is not the heartbeat going stale.
func TestTheRefusalForAnUnreadableRecordSaysWhatItRestsOn(t *testing.T) {
	refusal := func(t *testing.T, m *Manager) string {
		t.Helper()
		other := asPID(m, os.Getpid()+1)
		// A whole-node grant is refused over a card lease whose record cannot be read and whose holder is alive (keptUnreadable).
		l, err := other.TryAcquire(ClassMedia, Options{Reason: "whole node", TTL: time.Hour})
		if err == nil {
			_ = l.Release()
			t.Fatalf("a whole-node lease (epoch %d) was granted over a live card holder whose record cannot be read", l.Epoch())
		}
		var held *ErrHeld
		if !errors.As(err, &held) {
			t.Fatalf("the refusal is %v, want *ErrHeld", err)
		}
		return held.Info.Reason
	}
	const signOfLife = "a heartbeat or a write within the heartbeat TTL"

	t.Run("a fresh heartbeat is a sign of life and the sentence says so", func(t *testing.T) {
		m, _, _, _ := unreadableRecordFixture(t)
		if reason := refusal(t, m); !strings.Contains(reason, signOfLife) {
			t.Errorf("the holder gave a heartbeat a moment ago, the reason must name it: %q", reason)
		}
	})
	t.Run("an unknown heartbeat is not a sign of life and the sentence does not claim one", func(t *testing.T) {
		m, _, _, _ := unreadableRecordFixture(t)
		withHeartbeatFaults(t, errHBBoom, errHBBoom) // the heartbeat can be neither read nor examined: the record is 10 minutes old
		reason := refusal(t, m)
		if strings.Contains(reason, signOfLife) {
			t.Errorf("no sign of life was seen (the heartbeat cannot be told), and the reason says one was: %q", reason)
		}
		for _, want := range []string{"cannot be told", "gpu release"} {
			if !strings.Contains(reason, want) {
				t.Errorf("the reason must say the heartbeat cannot be told and what ends it (%q): %q", want, reason)
			}
		}
	})
}
