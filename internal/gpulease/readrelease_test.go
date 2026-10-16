package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The wait that readSharedFile takes for a refused open (readretry_test.go) is for a file that is there and passes in milliseconds.
// It must never be paid for a file that is not there. A released lease has no record, no claim and no heartbeat, and every status
// call, every fence and every sweep asks about them: if each such ask cost the wait (about 100 ms), a lease that had ended would
// slow down every reader behind it, and the readers that run under the epoch lock would hold it for the sum.
//
// These tests count, for each ask, how many times each file is OPENED and how many pauses the wait took (readRetries), with the
// wait switched ON on every platform (waitOutDeniedOpens), so a read that retried a missing file would show as 21 opens and 20
// pauses. Counts are the pin; the time is a loose bound beside them (a mutant that waits on every missing file costs 100 ms each).

// openLog is what recordOpens saw: opens per path, those made while the epoch lock file existed, and how many failed: refused with a
// permission error (the failure the wait is for), or in some other way than the file being absent (a missing file is an ordinary answer).
type openLog struct {
	mu        sync.Mutex
	perPath   map[string]int
	underLock map[string]int
	refused   int
	unusual   int
}

// recordOpens wraps the shared open: it counts every open per path, notes the ones made while the file at lockPath exists (the epoch
// lock is held, by this test's one goroutine), and runs before(path) ahead of each open when given (a test's way to make a file
// vanish between the listing that named it and the read of it). lockPath "" notes nothing.
func recordOpens(t *testing.T, lockPath string, before func(path string)) *openLog {
	t.Helper()
	log := &openLog{perPath: map[string]int{}, underLock: map[string]int{}}
	old := openSharedFile
	openSharedFile = func(path string) (*os.File, error) {
		if before != nil {
			before(path)
		}
		held := false
		if lockPath != "" {
			_, serr := os.Stat(lockPath)
			held = serr == nil
		}
		f, err := old(path)
		log.mu.Lock()
		log.perPath[path]++
		if held {
			log.underLock[path]++
		}
		switch {
		case err == nil || errors.Is(err, os.ErrNotExist):
		case errors.Is(err, os.ErrPermission):
			log.refused++
		default:
			log.unusual++
		}
		log.mu.Unlock()
		return f, err
	}
	t.Cleanup(func() { openSharedFile = old })
	return log
}

func (l *openLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.perPath {
		n += c
	}
	return n
}

// most is the largest number of times one path was opened, and which.
func (l *openLog) most() (int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	worst, which := 0, ""
	for p, c := range l.perPath {
		if c > worst {
			worst, which = c, p
		}
	}
	return worst, which
}

// mostMissing is most, over the paths that do not exist now: a file that is there may be read more than once by one ask (heartbeatAt
// is called by both the listing and the judge of the same lease); a file that is not there must cost one open per ask.
func (l *openLog) mostMissing() (int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	worst, which := 0, ""
	for p, c := range l.perPath {
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if c > worst {
			worst, which = c, p
		}
	}
	return worst, which
}

func (l *openLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perPath = map[string]int{}
	l.underLock = map[string]int{}
	l.refused, l.unusual = 0, 0
}

func (l *openLog) describe(dir string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var rows []string
	for p, c := range l.perPath {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			rel = p
		}
		rows = append(rows, filepath.ToSlash(rel)+" x"+strconv.Itoa(c))
	}
	sort.Strings(rows)
	return strings.Join(rows, ", ")
}

// TestAReleasedLeaseReadsAbsentAtOnceAndNoReadWaitsForIt: release a lease in a scratch root, then ask every reader about it. Each
// answers "absent" (no lease, not current, no heartbeat, no sign of life), opens each missing file at most once, and takes no pause.
func TestAReleasedLeaseReadsAbsentAtOnceAndNoReadWaitsForIt(t *testing.T) {
	waitOutDeniedOpens(t) // the wait is on whatever the build: a read that waited on a missing file would show
	m := scopedRealClock(t)
	l := mustDevices(t, m, card0)
	epoch := l.Epoch()
	if err := l.Renew(); err != nil { // a live lease has a heartbeat file; the release must take it away
		t.Fatal(err)
	}
	meta, err := readEpochRecord(m.leaseDir(), epoch)
	if err != nil {
		t.Fatalf("the record of the held lease: %v", err)
	}
	if len(m.Leases()) != 1 || !m.EpochIsCurrent(epoch) {
		t.Fatalf("before the release the lease is not seen as held: Leases() = %v", epochsOf(m.Leases()))
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{epochRecordPath(m.leaseDir(), epoch), m.heartbeatPath(epoch), cardClaimPath(m.leaseDir(), card0)} {
		if _, serr := os.Stat(gone); !os.IsNotExist(serr) {
			t.Fatalf("after Release %s is still there (%v)", gone, serr)
		}
	}

	log := recordOpens(t, "", nil)
	asks := []struct {
		name string
		ask  func() (answer string)
	}{
		{"Leases()", func() string {
			if got := m.Leases(); len(got) != 0 {
				return "a lease: " + describeEpochs(got)
			}
			return ""
		}},
		{"EpochIsCurrent", func() string {
			if m.EpochIsCurrent(epoch) {
				return "current"
			}
			return ""
		}},
		{"Inspect()", func() string {
			if info := m.Inspect(); info.Held {
				return "held"
			}
			return ""
		}},
		{"heartbeatAt", func() string {
			if got := heartbeatAt(m.leaseDir(), meta); got != meta.RenewedAtMs {
				return "a heartbeat other than the stamp"
			}
			return ""
		}},
		{"lastHeartbeat", func() string {
			if got := m.lastHeartbeat(meta); got != meta.RenewedAtMs {
				return "a heartbeat other than the stamp"
			}
			return ""
		}},
		{"keeperSignOfLife", func() string {
			if got := m.keeperSignOfLife(epoch, epochRecordPath(m.leaseDir(), epoch)); !got.IsZero() {
				return "a sign of life at " + got.String()
			}
			return ""
		}},
	}
	all := time.Now()
	for _, a := range asks {
		log.reset()
		readRetries.Store(0)
		start := time.Now()
		answer := a.ask()
		took := time.Since(start)
		if answer != "" {
			t.Errorf("%s of a released lease answered %q, want absent", a.name, answer)
		}
		if worst, which := log.most(); worst > 1 {
			t.Errorf("%s opened %s %d times, want at most 1: a missing file is an answer, not a refusal to wait out (opens: %s)", a.name, which, worst, log.describe(m.leaseDir()))
		}
		if n := readRetries.Load(); n != 0 {
			t.Errorf("%s waited %d times for a file that is not there, want 0", a.name, n)
		}
		t.Logf("%-17s %d opens (%s), %d pauses, %v", a.name, log.total(), log.describe(m.leaseDir()), readRetries.Load(), took)
	}
	// The sum of the six asks stays far under what one wait on one missing file costs on top of everything else (about 100 ms each).
	if took := time.Since(all); took > 2*time.Duration(removeAttempts)*removePause {
		t.Errorf("the six asks about a released lease took %v: a missing file is being waited on (one wait is about %v)", took, time.Duration(removeAttempts)*removePause)
	}
}

func describeEpochs(infos []Info) string {
	var parts []string
	for _, e := range epochsOf(infos) {
		parts = append(parts, strconv.FormatUint(e, 10))
	}
	return strings.Join(parts, ",")
}

// TestAMissingOrDeletedRecordCostsOneOpenAndNoWaitUnderTheEpochLock: the reads that run while the epoch lock is held (a grant's sweep,
// its check against the other leases, the release) cost one open per file that is gone, whatever the number of records. Three
// leases are held and a fourth card is asked for; the first read of each record is made to find it deleted, as it is when the holder
// releases between the sweep's listing and its read. Each deleted record is opened once, not waited on.
func TestAMissingOrDeletedRecordCostsOneOpenAndNoWaitUnderTheEpochLock(t *testing.T) {
	waitOutDeniedOpens(t)
	m := scopedRealClock(t)
	var held []*Lease
	for _, c := range []string{card0, card1, card2} {
		held = append(held, mustDevices(t, m, c))
	}
	epochs := map[string]bool{}
	for _, l := range held {
		epochs[epochRecordPath(m.leaseDir(), l.Epoch())] = true
	}

	deleted := map[string]bool{}
	lockPath := m.epochLockPath()
	log := recordOpens(t, lockPath, func(path string) {
		if !epochs[path] || deleted[path] {
			return
		}
		if _, err := os.Stat(lockPath); err != nil {
			return // only the reads made under the lock are the subject
		}
		deleted[path] = true
		if err := os.Remove(path); err != nil {
			t.Errorf("deleting %s between the listing and the read: %v", path, err)
		}
	})
	readRetries.Store(0)
	start := time.Now()
	l4, err := m.TryAcquire(ClassMedia, Options{Reason: "test", Devices: []string{card3}, TTL: time.Hour})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a grant of a free card while three records vanish under the sweep: %v", err)
	}
	defer l4.Release()

	if len(deleted) != len(epochs) {
		t.Fatalf("the sweep read %d of the %d records: the test did not delete them under the lock", len(deleted), len(epochs))
	}
	if len(log.underLock) == 0 {
		t.Fatal("no open was made while the epoch lock was held: the test does not observe the locked reads")
	}
	for path := range epochs {
		if log.underLock[path] != 1 {
			t.Errorf("the deleted record %s was opened %d times under the lock, want exactly 1", filepath.Base(path), log.underLock[path])
		}
	}
	if n := readRetries.Load(); n != 0 {
		t.Errorf("%d pauses were taken under the lock for records that were gone, want 0", n)
	}
	if took > 2*time.Duration(removeAttempts)*removePause {
		t.Errorf("the grant took %v with three records deleted under it: a deleted record is being waited on (one wait is about %v, three would be %v)",
			took, time.Duration(removeAttempts)*removePause, 3*time.Duration(removeAttempts)*removePause)
	}
	t.Logf("grant of card3 with three records deleted under the sweep: %d opens in all (%s), %d of them under the lock, %d pauses, %v",
		log.total(), log.describe(m.leaseDir()), sumValues(log.underLock), readRetries.Load(), took)
}

func sumValues(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// TestReleasingUnderTheEpochLockReadsNoRecordMoreThanOnce: the release path and the listing readers, over N released leases' worth of
// missing records, make no read that grows by a wait. releaseV2 removes a lease's files under the lock; every ask after it finds the
// files gone. Leases() over the remaining records opens each present record once and each missing one once.
func TestReleasingUnderTheEpochLockReadsNoRecordMoreThanOnce(t *testing.T) {
	waitOutDeniedOpens(t)
	m := scopedRealClock(t)
	var held []*Lease
	for _, c := range []string{card0, card1, card2, card3} {
		held = append(held, mustDevices(t, m, c))
	}
	log := recordOpens(t, m.epochLockPath(), nil)
	readRetries.Store(0)

	start := time.Now()
	for _, l := range held[:3] {
		if err := l.Release(); err != nil {
			t.Fatal(err)
		}
	}
	releaseTook := time.Since(start)
	if n := readRetries.Load(); n != 0 {
		t.Errorf("releasing three leases took %d pauses, want 0", n)
	}
	if worst, which := log.mostMissing(); worst > 1 {
		t.Errorf("releasing three leases opened the missing %s %d times, want at most 1 (opens: %s)", filepath.Base(which), worst, log.describe(m.leaseDir()))
	}

	log.reset()
	start = time.Now()
	got := m.Leases()
	listTook := time.Since(start)
	if len(got) != 1 || got[0].Epoch != held[3].Epoch() {
		t.Fatalf("Leases() after three releases = %v, want only epoch %d", epochsOf(got), held[3].Epoch())
	}
	// two asks name the heartbeat of the one live lease (the listing and the judge), and it was never renewed, so it is missing: two
	// opens, each answered at once. A wait on a missing file would show as 21.
	if worst, which := log.mostMissing(); worst > 2 {
		t.Errorf("Leases() opened the missing %s %d times, want at most 2 (opens: %s)", filepath.Base(which), worst, log.describe(m.leaseDir()))
	}
	if n := readRetries.Load(); n != 0 {
		t.Errorf("Leases() took %d pauses, want 0", n)
	}
	if releaseTook+listTook > 2*time.Duration(removeAttempts)*removePause {
		t.Errorf("three releases and a listing took %v: a missing file is being waited on", releaseTook+listTook)
	}
	t.Logf("three releases %v; Leases() with one record left: %d opens (%s), %v", releaseTook, log.total(), log.describe(m.leaseDir()), listTook)
}
