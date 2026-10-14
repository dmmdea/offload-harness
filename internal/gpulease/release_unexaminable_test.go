package gpulease

// Review round 3 (D): ReleaseByEpoch answered (false, nil), "no lease was held", whenever the lease's record could not even be
// examined, for any reason: a stat that fails with "access denied" while the record is being replaced is not an absent record.
// The caller then reported nothing wrong, and the cards sat held. Only an absent record is "nothing held"; anything else is an
// error, because the lease may well be there.

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAReleaseWhoseRecordCannotBeExaminedIsAnErrorNotNothingHeld(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	restore := UseStatFault(func(string) error { return errors.New("access denied") })
	released, err := m.ReleaseByEpoch(l.Epoch())
	restore()
	if err == nil || released || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("a record that cannot be examined is an error that names why, not \"nothing was held\": released=%v err=%v", released, err)
	}
	if !m.Inspect().Held {
		t.Fatal("the lease is still there: the failed examination released nothing")
	}

	// An absent record is still nothing to release, and the real stat of an epoch that is no lease says so.
	if released, err := m.ReleaseByEpoch(l.Epoch() + 90); released || err != nil {
		t.Errorf("an epoch that is no lease is nothing held, not a failure: released=%v err=%v", released, err)
	}
	restore = UseStatFault(func(string) error { return fs.ErrNotExist })
	defer restore()
	if released, err := m.ReleaseByEpoch(l.Epoch()); released || err != nil {
		t.Errorf("a record reported absent is nothing held: released=%v err=%v", released, err)
	}
}

// The whole-node half of the same masking: a record that cannot be READ (other than absent or corrupt) is not "nothing held".
func TestAWholeNodeReleaseWhoseRecordCannotBeReadIsAnErrorNotNothingHeld(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if l.v2 {
		t.Skip("this manager grants card-scoped leases only")
	}
	meta := m.metaPath()
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(meta, 0o777); err != nil { // a record that exists and cannot be read as a file
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(meta) }()
	released, err := m.ReleaseByEpoch(l.Epoch())
	if err == nil || released {
		t.Fatalf("a record that exists and cannot be read is an error, not \"nothing was held\": released=%v err=%v", released, err)
	}
}
