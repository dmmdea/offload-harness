package gpulease

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The staged files whose writers hold the epoch lock are debris, at any age, to whoever holds the lock: a writer that has the lock is
// not running while another process has it, so a staged file found then is the leftover of a writer that died (a restamp of meta.json
// stages as meta.json.restamp, a card record as e/<epoch>.json.tmp). Left alone, one costs every reader that finds its live name
// absent a wait for a rename that will never end. The heartbeat is the exception: Renew replaces hb.<epoch> WITHOUT the lock, so
// hb.<epoch>.tmp is a rename that may be landing at this instant, and is never swept.

// leftoverAges are the two ages of a leftover that matter: one the gate still reads as a rename in flight (under stagingFresh) and
// one it reads as a dead writer's. The sweep does not look at the age.
var leftoverAges = map[string]time.Duration{"fresh": 0, "old": time.Hour}

// plantStaged writes the staged file of live with the age given, on the gate's clock.
func plantStaged(t *testing.T, c *gateClock, live string, age time.Duration) string {
	t.Helper()
	return stageBeside(t, c, live, age)
}

func eRecordPath(m *Manager, epoch uint64) string { return epochRecordPath(m.leaseDir(), epoch) }

// A whole-node grant sweeps the restamp file a dead writer left beside meta.json, in the same critical section as the claim.
func TestAWholeNodeGrantSweepsTheRestampFileAWriterLeft(t *testing.T) {
	for name, age := range leftoverAges {
		t.Run(name, func(t *testing.T) {
			c := onGateClock(t)
			m, _ := newTestManager(t)
			if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
				t.Fatal(err)
			}
			restamp := plantStaged(t, c, m.metaPath(), age)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
			if err != nil {
				t.Fatalf("TryAcquire = %v", err)
			}
			defer l.Release()
			if exists(restamp) {
				t.Errorf("meta.json.restamp (%s) is still there after a grant that held the epoch lock", name)
			}
		})
	}
}

// A card grant sweeps both: the restamp file of the whole-node record and the staged card records.
func TestACardGrantSweepsTheStagedRecordsAndTheRestampFileWritersLeft(t *testing.T) {
	for name, age := range leftoverAges {
		t.Run(name, func(t *testing.T) {
			c := onGateClock(t)
			m := scopedRealClock(t)
			if err := os.MkdirAll(m.v2Dir(), 0o777); err != nil {
				t.Fatal(err)
			}
			restamp := plantStaged(t, c, m.metaPath(), age)
			staged7 := plantStaged(t, c, eRecordPath(m, 7), age)
			staged8 := plantStaged(t, c, eRecordPath(m, 8), age)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "card", Devices: []string{card0}, TTL: time.Hour})
			if err != nil {
				t.Fatalf("TryAcquire = %v", err)
			}
			defer l.Release()
			for _, p := range []string{restamp, staged7, staged8} {
				if exists(p) {
					t.Errorf("%s is still there after a grant that held the epoch lock (%s)", filepath.Base(p), name)
				}
			}
		})
	}
}

// The whole-node grant's second critical section (the verification that no card lease is live) sweeps too, and the reads it makes are
// not delayed by what it swept: meta.json is read after the restamp file beside it is gone, so no wait is paid for it.
func TestTheWholeNodeVerificationSweepsLeftoversBeforeItReadsAnything(t *testing.T) {
	c := onGateClock(t)
	m, _ := newTestManager(t)
	if err := os.MkdirAll(m.v2Dir(), 0o777); err != nil {
		t.Fatal(err)
	}
	restamp := plantStaged(t, c, m.metaPath(), 0)
	staged := plantStaged(t, c, eRecordPath(m, 7), 0)
	gatePauses.Store(0)
	if err := m.verifyNoDeviceLeases(1); err != nil {
		t.Fatalf("verifyNoDeviceLeases = %v", err)
	}
	for _, p := range []string{restamp, staged} {
		if exists(p) {
			t.Errorf("%s is still there after the verification that held the epoch lock", filepath.Base(p))
		}
	}
	if n := gatePauses.Load(); n != 0 {
		t.Errorf("the verification paused %d times for a rename in flight, want none: the leftover it waited for is a dead writer's and was there to sweep", n)
	}
}

// GUARDS, which pass before the sweep exists and must keep passing: the heartbeat's staged file is a rename that may be landing (Renew
// does not take the lock), and the live files are not staged files.
func TestTheSweepOfLeftoversLeavesTheHeartbeatsStagedFileAndTheLiveFiles(t *testing.T) {
	for name, age := range leftoverAges {
		t.Run(name, func(t *testing.T) {
			c := onGateClock(t)
			m := scopedRealClock(t)
			held := mustDevices(t, m, card1)
			defer held.Release()
			hbStaged := plantStaged(t, c, m.heartbeatPath(held.Epoch()), age)
			otherHb := plantStaged(t, c, m.heartbeatPath(99), age)

			l, err := m.TryAcquire(ClassMedia, Options{Reason: "card", Devices: []string{card0}, TTL: time.Hour})
			if err != nil {
				t.Fatalf("a card grant beside a held card = %v", err)
			}
			_ = l.Release()
			_ = m.verifyNoDeviceLeases(1) // runs the sweep; the live card lease makes the answer a refusal, which is not the subject

			for _, p := range []string{hbStaged, otherHb} {
				if !exists(p) {
					t.Errorf("%s was swept: Renew writes it without the epoch lock, so it may be a rename landing at this instant", filepath.Base(p))
				}
			}
			if rec := eRecordPath(m, held.Epoch()); !exists(rec) {
				t.Errorf("%s, the live record of the held lease, is gone", filepath.Base(rec))
			}
		})
	}
}
