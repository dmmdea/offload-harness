package gpulease

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten reproduces the Windows read-side flake of the lease record. A lease
// record is replaced in place while the lease is held: at the grant (granting, then active), and by every Restamp (the host-RAM
// hold, the tree the holder recorded, the holder's note). The writer renames a fresh file over the record (renameReplacing). While
// that rename runs, the system holds a handle on the old entry with DELETE access, and a reader that opens the record with a share
// mode that lacks FILE_SHARE_DELETE (os.ReadFile) fails with ERROR_SHARING_VIOLATION. The writer's side of that race is retried
// (renamesafe_test.go); the reader's was not, so Leases() and the fence every render child asks (EpochIsCurrent) answered "no
// lease" for a lease that was held, and a media route's own test saw "want one live lease while the render runs, got []" on a
// loaded host (TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere).
//
// The lease is held for the whole test and never released while a reader runs, so any answer other than "one live lease, and
// current" is a read that lost the record to the rewrite. Bounded: a fixed number of rewrites, and the readers poll until the
// writer is done. Measured on Windows with the reader as it was, two readers of each kind: Leases() missed 24 to 51 reads in about
// 2,000 to 3,000 and EpochIsCurrent 70 to 95 in about the same, per run of 150 rewrites.
//
// The readers pause between reads. A reader that re-opens the record without a gap keeps a handle on it nearly always, and a
// rename cannot replace an entry that has one open (renameReplacing gives up after 2 s), so on a loaded host the writer starved:
// that is the writer's business and not the subject, so a starved rewrite is tried again.
//
// A few stray misses are allowed (strayMisses). With the shared open in, a run under 30 busy-loop processes showed one miss in about
// 8,000 to 26,000 reads in 2 runs of about 80, which this change does not explain; the defect misses dozens to hundreds per run, so
// the allowance cannot hide it (the mutant, os.Open in place of OpenShared, fails every run by a factor of more than ten).
func TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten(t *testing.T) {
	m := scopedRealClock(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	epoch := l.Epoch()

	const (
		rewrites    = 150
		strayMisses = 3
		readPause   = 100 * time.Microsecond
	)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var leaseReads, leaseMisses, fenceReads, fenceMisses atomic.Int64
	poll := func(reads, misses *atomic.Int64, held func() bool) {
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
			time.Sleep(readPause)
		}
	}
	for i := 0; i < 2; i++ {
		readers.Add(2)
		go poll(&leaseReads, &leaseMisses, func() bool { return len(m.Leases()) == 1 })
		go poll(&fenceReads, &fenceMisses, func() bool { return m.EpochIsCurrent(epoch) })
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
		var err error
		for try := 0; try < 5; try++ {
			if err = l.Restamp(func(mt *Meta) { mt.Reason = "rewrite-" + strconv.Itoa(i) }); err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("rewrite %d of the held lease's record: %v", i, err)
		}
	}
	stopReaders()

	if leaseReads.Load() == 0 || fenceReads.Load() == 0 {
		t.Fatalf("the readers never ran (Leases %d reads, EpochIsCurrent %d reads): the test proved nothing", leaseReads.Load(), fenceReads.Load())
	}
	if n := leaseMisses.Load(); n > strayMisses {
		t.Errorf("Leases() did not report one live lease on %d of %d reads while the held lease's record was rewritten %d times", n, leaseReads.Load(), rewrites)
	}
	if n := fenceMisses.Load(); n > strayMisses {
		t.Errorf("EpochIsCurrent(%d) called the held lease not current on %d of %d reads while its record was rewritten %d times", epoch, n, fenceReads.Load(), rewrites)
	}
}
