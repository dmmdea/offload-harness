package gpulease

import (
	"io/fs"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows"
)

// The deterministic form of TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten: the read of the lease's record fails with the
// error Windows returns when it meets a rename-over in flight (the seam stands in for the failing read; the error is the one
// measured under the concurrent rewrite), for the first few attempts, then succeeds as the real read does once the rename is done.
// A reader that gives up on the first failure reads the held lease as absent.
func TestATransientSharingViolationOnTheRecordReadIsRetriedNotReadAsAbsent(t *testing.T) {
	m := scopedRealClock(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	rec := epochRecordPath(m.leaseDir(), l.Epoch())

	const transient = 3
	var failed atomic.Int32
	defer UseReadFault(func(p string) error {
		if p == rec && failed.Add(1) <= transient {
			return &fs.PathError{Op: "open", Path: p, Err: windows.ERROR_SHARING_VIOLATION}
		}
		return nil
	})()

	got := m.Leases()
	if len(got) != 1 {
		t.Fatalf("Leases() = %+v after %d sharing violations on the record read: want the one held lease (a violation is a rename in flight, not an absent record)", got, failed.Load())
	}
	if n := failed.Load(); n < transient {
		t.Fatalf("the seam failed only %d reads of the record: the test did not inject the fault", n)
	}
}
