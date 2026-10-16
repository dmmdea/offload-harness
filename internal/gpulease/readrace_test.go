package gpulease

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten reproduces the Windows read-side flake of the lease record. A lease
// record is replaced in place while the lease is held: at the grant (granting, then active), and by every Restamp (the host-RAM
// hold, the tree the holder recorded, the holder's note). The writer renames a fresh file over the record (renameReplacing). A
// reader that opens the record at that instant gets ERROR_SHARING_VIOLATION on Windows, because the rename holds the old entry
// without FILE_SHARE_DELETE. The writer's side of that race is retried (renamesafe_test.go); the reader's was not, so Leases() and
// the fence every render child asks (EpochIsCurrent) answered "no lease" for a lease that was held, and a media route's own test
// saw "want one live lease while the render runs, got []" on a loaded box (TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere).
//
// The lease is held for the whole test and never released while a reader runs, so any answer other than "one live lease, and
// current" is a read that lost the record to the rewrite. Bounded: a fixed number of rewrites, and the readers spin until the writer
// is done, so there is no sleep to wait on. Measured on Windows before the retry, under four spinning readers: Leases() missed
// between 1 read in 30 and 1 in 60, and EpochIsCurrent about 1 in 15.
func TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten(t *testing.T) {
	m := scopedRealClock(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	epoch := l.Epoch()

	const rewrites = 300
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var leaseReads, leaseMisses, fenceReads, fenceMisses atomic.Int64
	spin := func(reads, misses *atomic.Int64, held func() bool) {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reads.Add(1)
			if !held() {
				misses.Add(1)
			}
		}
	}
	for i := 0; i < 2; i++ {
		readers.Add(2)
		go spin(&leaseReads, &leaseMisses, func() bool { return len(m.Leases()) == 1 })
		go spin(&fenceReads, &fenceMisses, func() bool { return m.EpochIsCurrent(epoch) })
	}
	stopReaders := func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		readers.Wait()
	}
	defer stopReaders()

	for i := 0; i < rewrites; i++ {
		if err := l.Restamp(func(mt *Meta) { mt.Reason = "rewrite-" + strconv.Itoa(i) }); err != nil {
			t.Fatalf("rewrite %d of the held lease's record: %v", i, err)
		}
	}
	stopReaders()

	if leaseReads.Load() == 0 || fenceReads.Load() == 0 {
		t.Fatalf("the readers never ran (Leases %d reads, EpochIsCurrent %d reads): the test proved nothing", leaseReads.Load(), fenceReads.Load())
	}
	if n := leaseMisses.Load(); n > 0 {
		t.Errorf("Leases() did not report one live lease on %d of %d reads while the held lease's record was rewritten %d times", n, leaseReads.Load(), rewrites)
	}
	if n := fenceMisses.Load(); n > 0 {
		t.Errorf("EpochIsCurrent(%d) called the held lease not current on %d of %d reads while its record was rewritten %d times", epoch, n, fenceReads.Load(), rewrites)
	}
}
