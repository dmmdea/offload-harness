//go:build linux

package gpulease

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The Linux half of readrace_test.go. On Windows a reader can meet the instant a rename has marked the old entry for deletion, and
// readSharedFile waits it out. On Linux rename(2) is atomic: the name always opens, to the old file or to the new one, and a reader's
// open descriptor never holds a rename up. So concurrent rename-over of a record and of its heartbeat, with readers in a loop, must
// give no miss, and the wait must never trigger: not one refused open, not one pause (readRetries). This pins both, and fails if
// the platform gate on the wait (sharedOpenMeetsDeletePending) ever lets a wait run here, or if a writer here stops being a rename.
//
// The writers are the real ones: Restamp rewrites e/<epoch>.json by rename-over (under the epoch lock), Renew rewrites hb.<epoch> the
// same way. The readers are the three the fence and the sweep use: Leases(), EpochIsCurrent and the heartbeat read. The lease is held
// for the whole test, so any answer other than "one lease, current, heartbeat read" is a miss.
func TestOnLinuxConcurrentRenameOverOfARecordAndItsHeartbeatIsNeverMissedAndNeverWaitedFor(t *testing.T) {
	if deniedOpenMayClear {
		t.Fatal("deniedOpenMayClear is true on Linux: a refused open would be waited on here, where nothing is ever delete-pending")
	}
	m := scopedRealClock(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	epoch := l.Epoch()
	if err := l.Renew(); err != nil { // the heartbeat file exists from here on, so a failed read of it is a miss
		t.Fatal(err)
	}
	meta, err := readEpochRecord(m.leaseDir(), epoch)
	if err != nil {
		t.Fatal(err)
	}
	hb := m.heartbeatPath(epoch)

	log := recordOpens(t, "", nil)
	readRetries.Store(0)
	gatePauses.Store(0)

	const (
		rewrites  = 3000
		readPause = 50 * time.Microsecond
	)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var leaseReads, leaseMisses, fenceReads, fenceMisses, hbReads, hbMisses atomic.Int64
	poll := func(reads, misses *atomic.Int64, ok func() bool) {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reads.Add(1)
			if !ok() {
				misses.Add(1)
			}
			time.Sleep(readPause)
		}
	}
	for i := 0; i < 2; i++ {
		readers.Add(3)
		go poll(&leaseReads, &leaseMisses, func() bool { return len(m.Leases()) == 1 })
		go poll(&fenceReads, &fenceMisses, func() bool { return m.EpochIsCurrent(epoch) })
		go poll(&hbReads, &hbMisses, func() bool {
			b, rerr := readSharedFile(hb)
			if rerr != nil {
				return false
			}
			_, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			return perr == nil
		})
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

	var writers sync.WaitGroup
	var werr atomic.Value
	writers.Add(2)
	go func() {
		defer writers.Done()
		for i := 0; i < rewrites; i++ {
			if err := l.Restamp(func(mt *Meta) { mt.Reason = "rewrite-" + strconv.Itoa(i) }); err != nil {
				werr.Store("Restamp: " + err.Error())
				return
			}
		}
	}()
	go func() {
		defer writers.Done()
		for i := 0; i < rewrites; i++ {
			if err := l.Renew(); err != nil {
				werr.Store("Renew: " + err.Error())
				return
			}
		}
	}()
	writers.Wait()
	stopReaders()
	if e := werr.Load(); e != nil {
		t.Fatalf("a writer failed: %v (on Linux a rename-over never fails)", e)
	}

	if leaseReads.Load() == 0 || fenceReads.Load() == 0 || hbReads.Load() == 0 {
		t.Fatalf("the readers never ran (Leases %d, EpochIsCurrent %d, heartbeat %d reads): the test proved nothing", leaseReads.Load(), fenceReads.Load(), hbReads.Load())
	}
	t.Logf("%d record and %d heartbeat rename-overs; misses: Leases() %d of %d, EpochIsCurrent %d of %d, heartbeat %d of %d; %d opens, %d refused, %d failed otherwise (a missing meta.json is not a failure), %d pauses",
		rewrites, rewrites, leaseMisses.Load(), leaseReads.Load(), fenceMisses.Load(), fenceReads.Load(), hbMisses.Load(), hbReads.Load(), log.total(), log.refused, log.unusual, readRetries.Load())
	if n := leaseMisses.Load(); n > 0 {
		t.Errorf("Leases() did not report the held lease on %d of %d reads while its record was rewritten", n, leaseReads.Load())
	}
	if n := fenceMisses.Load(); n > 0 {
		t.Errorf("EpochIsCurrent(%d) called the held lease not current on %d of %d reads while its record was rewritten", epoch, n, fenceReads.Load())
	}
	if n := hbMisses.Load(); n > 0 {
		t.Errorf("the heartbeat could not be read on %d of %d reads while Renew rewrote it", n, hbReads.Load())
	}
	if log.refused != 0 || log.unusual != 0 {
		t.Errorf("%d opens of a lease file were refused and %d failed otherwise on Linux, want 0 of each: rename(2) is atomic, so no open can meet the swap (a missing meta.json is the one ordinary failure and is not counted)", log.refused, log.unusual)
	}
	if n := readRetries.Load(); n != 0 {
		t.Errorf("readSharedFile paused %d times on Linux, want 0: the wait for a refused open must never trigger where no rename can cause one", n)
	}
	if n := gatePauses.Load(); n != 0 {
		t.Errorf("the gate paused %d times on Linux, want 0: nothing is absent mid-rename where rename(2) is atomic", n)
	}

	// the heartbeat reader's value is the fallback stamp only when the file cannot be read: it must be a real renewal
	if got, known := heartbeatAt(m.leaseDir(), meta); !known || got < meta.RenewedAtMs {
		t.Errorf("heartbeatAt = %d (known %v), before the record's stamp %d or unknown", got, known, meta.RenewedAtMs)
	}
}
