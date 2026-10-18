package gpulease

import (
	"os"
	"testing"
)

// `gpu release` with no epoch ends "whatever is held", and decides that from a LISTING of the card records: it releases the lease when
// exactly one is live and refuses when several are. A card record whose rename is installing it has its live name absent from the
// listing and only its staged file in it, so a listing taken in that instant does not show the lease. With two leases held the one in
// flight was invisible and the other was released, the very lease the operator had not meant; with one held the answer was "nothing is
// held". A record that cannot be listed is not a lease that is not there (unreadableCardRecords).

// inFlightRecord puts a card lease's record in the state a listing meets mid-rename: the live name absent and the staged file beside it,
// just written.
func inFlightRecord(t *testing.T, m *Manager, l *Lease) {
	t.Helper()
	rec := epochRecordPath(m.leaseDir(), l.Epoch())
	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagingPath(rec), data, 0o666); err != nil {
		t.Fatal(err)
	}
}

func TestReleasingWhateverIsHeldIsRefusedWhileACardRecordIsMidRename(t *testing.T) {
	t.Run("two card leases, one of them mid-rename: the other is not the one to release", func(t *testing.T) {
		onGateClock(t)
		m, _ := scopedManager(t)
		a, b := mustDevices(t, m, card0), mustDevices(t, m, card1)
		inFlightRecord(t, m, b)
		if target, err := m.ReleaseTarget(0); err == nil {
			t.Errorf("ReleaseTarget(0) = (%d, nil) with the record of lease %d mid-rename, want a refusal: which lease is meant is not known", target, b.Epoch())
		}
		if released, err := m.ReleaseByEpoch(0); err == nil {
			t.Errorf("ReleaseByEpoch(0) = (%v, nil) with the record of lease %d mid-rename, want a refusal", released, b.Epoch())
		}
		if _, err := os.Stat(epochRecordPath(m.leaseDir(), a.Epoch())); err != nil {
			t.Errorf("the record of lease %d was removed by a release of whatever is held while the record of lease %d was mid-rename: %v", a.Epoch(), b.Epoch(), err)
		}
	})
	t.Run("one card lease, mid-rename: it is not nothing", func(t *testing.T) {
		onGateClock(t)
		m, _ := scopedManager(t)
		a := mustDevices(t, m, card0)
		inFlightRecord(t, m, a)
		if target, err := m.ReleaseTarget(0); err == nil && target == 0 {
			t.Errorf("ReleaseTarget(0) = (0, nil) with the only lease's record mid-rename, want a refusal: something is held")
		}
		if released, err := m.ReleaseByEpoch(0); err == nil && !released {
			t.Errorf("ReleaseByEpoch(0) = (false, nil) with the only lease's record mid-rename, want a refusal: something is held")
		}
	})
	t.Run("the record is back: the lease it names is the one released", func(t *testing.T) {
		onGateClock(t)
		m, _ := scopedManager(t)
		a, b := mustDevices(t, m, card0), mustDevices(t, m, card1)
		inFlightRecord(t, m, b)
		_ = os.Rename(stagingPath(epochRecordPath(m.leaseDir(), b.Epoch())), epochRecordPath(m.leaseDir(), b.Epoch()))
		if _, err := m.ReleaseTarget(0); err == nil {
			t.Errorf("ReleaseTarget(0) with two live card leases succeeded, want the refusal that asks for --epoch")
		}
		if released, err := m.ReleaseByEpoch(a.Epoch()); err != nil || !released {
			t.Errorf("ReleaseByEpoch(%d) = (%v, %v), want the named lease released", a.Epoch(), released, err)
		}
	})
}

// A named epoch whose record cannot be examined is still that lease: ReleaseTarget names it (the release reports what became of it), so
// the caller that stops the instances kept under a lease before releasing it is not told "nothing is held".
func TestReleaseTargetOfANamedEpochWhoseRecordIsMidRenameNamesIt(t *testing.T) {
	onGateClock(t)
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	inFlightRecord(t, m, a)
	if target, err := m.ReleaseTarget(a.Epoch()); err != nil || target != a.Epoch() {
		t.Errorf("ReleaseTarget(%d) = (%d, %v) with the record mid-rename, want (%d, nil)", a.Epoch(), target, err, a.Epoch())
	}
}
