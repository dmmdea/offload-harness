package gpulease

// The read gate: how long a reader of a lease file waits when the live name is absent and a staged replacement stands beside it.
//
// settled.go says when an absence is an answer. This file is what a READER does about an absence that is not yet one: on Windows a
// rename that succeeded can leave the live name absent for tens of milliseconds, with the staged file still beside it, so the read
// waits for the rename to end instead of answering "not there". Windows only, on the same switch as the wait for a refused open
// (deniedOpenMayClear): on a platform where rename(2) is atomic there is nothing to wait for, and the read answers at once.

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// absentBudget is how long a reader waits, from the first time it meets a fresh staged file beside an absent name, for the rename to end.
// A deadline and never a count of pauses. Measured on the host that showed the defect: absent for 11 to 75 ms as the reader sees it, one
// rename that took 119 ms to return; 250 ms is twice the longest, and it is paid only when a fresh staged file stands beside the name.
// A variable so a test can shorten it.
var absentBudget = 250 * time.Millisecond

// absentPause is the pause between looks, the same as the wait for a refused open.
const absentPause = removePause

// absentMemory is how long a name whose wait ran out is read without a new wait (see rememberAbsentExpiry).
var absentMemory = 5 * time.Second

// gateNow and gateSleep are the clock and the pause of the gate, as variables so a test can run the wait on a clock it moves.
var (
	gateNow   = time.Now
	gateSleep = time.Sleep
)

// gatePauses counts the pauses the gate has taken waiting for a rename to end, since the process started, and gateExpiries the waits
// that ran out the whole budget: test seams and not metrics. readRetries (hostram.go) counts the pauses for a refused open, a different
// wait, and a missing file adds to neither.
var (
	gatePauses   atomic.Int64
	gateExpiries atomic.Int64
)

// gateWatch is one read's wait for a rename to end. The zero value is a read that has not met one yet; the deadline runs from the first
// time it does.
type gateWatch struct {
	first    time.Time // when the read first met a fresh staged file, or a name the look found and the open did not
	rechecks int       // looks at the name that found it there after a read that did not
}

// gateVerdict is what a read that found its name absent learns from the file beside it.
type gateVerdict int

const (
	// gateNoStaging: the file is never replaced by a rename (a card claim), so there is nothing beside it to look for and absent is the answer.
	gateNoStaging gateVerdict = iota
	// gateClear: no fresh staged file stands beside the name. The caller looks at the name once, unconditionally: the rename may have
	// ended between its read and the look at the staged file, and the name is then back.
	gateClear
	// gateWaited: a fresh staged file stands beside the name, a rename may be installing it, and the gate has paused: look again.
	gateWaited
	// gateExpired: the budget ran out. The caller answers an I/O failure (errRenameInFlight), never an absence.
	gateExpired
)

// afterAbsentRead is called by a read that found its name absent, with the gate on.
func (w *gateWatch) afterAbsentRead(path string) gateVerdict {
	if stagingPath(path) == "" {
		return gateNoStaging
	}
	if !stagedInFlight(path) {
		forgetAbsentExpiry(path) // the staged file is gone: whatever was stuck has ended
		return gateClear
	}
	if absentExpiryRemembered(path) {
		return gateExpired // this name outlasted a whole wait a moment ago
	}
	if epochHoldExhausted(path) {
		lockCapHits.Add(1)
		return gateExpired // the hold of the epoch lock this read is part of has spent what the gate may take of it
	}
	now := gateNow()
	if w.first.IsZero() {
		w.first = now
	}
	if now.Sub(w.first) >= absentBudget {
		gateExpiries.Add(1)
		rememberAbsentExpiry(path)
		return gateExpired
	}
	gatePauses.Add(1)
	gateSleep(absentPause)
	return gateWaited
}

// nameIsThere is called when the look at the name found it there although the read of it found it absent: the rename ended between the
// two. The caller reads again at once; if that keeps happening the same deadline ends it.
func (w *gateWatch) nameIsThere(path string) gateVerdict {
	now := gateNow()
	if w.first.IsZero() {
		w.first = now
	}
	if now.Sub(w.first) >= absentBudget {
		gateExpiries.Add(1)
		return gateExpired
	}
	if epochHoldExhausted(path) {
		lockCapHits.Add(1)
		return gateExpired
	}
	if w.rechecks > 0 {
		gatePauses.Add(1)
		gateSleep(absentPause)
	}
	w.rechecks++
	return gateWaited
}

// The gate time one hold of the epoch lock may take. A second acquirer spins for the lock for epochLockAttempts x epochLockPause (2 s) and
// then gives up on a lock held by a live process, and a hold reads the heartbeat of every lease: with a fresh staged leftover beside each
// of N of them and nothing remembered, the hold was N budgets long, past the spin from the ninth. The memory of a wait that ran out
// (below) makes it one wait per file, which is not a bound; this is: once a hold has run for lockGateCap on the gate's clock, every
// further read it makes of a lease file with a fresh staged file beside an absent name answers the I/O failure at once, with no wait.
// That is the answer a wait that ran out gives, and every decision made under the lock reads it as held (unknown is never stale), so
// nothing is decided differently: the wait would only have turned "unknown" into a heartbeat that is fresh. The cap applies to the reads
// of the lease directory the hold is of, by whoever makes them in this process, and never to a read outside a hold.
//
// One second is half the spin, and leaves the rest of the hold (a few file operations) and the wait for a refused open (readSharedFile,
// remembered after the first wait) well inside it.
var lockGateCap = 1 * time.Second

var (
	holdsMu    sync.Mutex
	epochHolds = map[string]time.Time{} // lease directory -> when the hold of its epoch lock in progress began, on the gate's clock
	holdCount  atomic.Int64             // len(epochHolds), so a read outside any hold takes no lock
)

// lockCapHits counts the reads the cap answered at once: a test seam and not a metric.
var lockCapHits atomic.Int64

// beginEpochHold records that this process holds the epoch lock of the lease directory dir from now. The returned function ends the hold.
func beginEpochHold(dir string) (end func()) {
	holdsMu.Lock()
	epochHolds[dir] = gateNow()
	holdCount.Store(int64(len(epochHolds)))
	holdsMu.Unlock()
	return func() {
		holdsMu.Lock()
		delete(epochHolds, dir)
		holdCount.Store(int64(len(epochHolds)))
		holdsMu.Unlock()
	}
}

// epochHoldExhausted says whether a hold of the epoch lock of the lease directory that path is in has run for lockGateCap.
func epochHoldExhausted(path string) bool {
	if holdCount.Load() == 0 {
		return false
	}
	holdsMu.Lock()
	defer holdsMu.Unlock()
	now := gateNow()
	for dir, began := range epochHolds {
		if strings.HasPrefix(path, dir+string(os.PathSeparator)) && now.Sub(began) >= lockGateCap {
			return true
		}
	}
	return false
}

// The memory of a wait that ran out. A staged file that stays fresh beside an absent name (a writer that died a moment ago) is not a
// rename in flight, and the wait for it must be paid once for the name and not once for each read of it: a grant reads each record about
// five times and each heartbeat about seven inside one hold of the epoch lock, so a budget per read held it for 1.75 s for a single such
// heartbeat, and eight of them for far longer than the 2 s a second acquirer's spin allows. For absentMemory after a wait ran out, a read
// of the same name that still finds a fresh staged file beside it answers the same I/O failure at once. The memory is only a way to skip
// the wait: it ends when the staged file is gone, when a read of the name succeeds, and when its time is up. It is a different thing from
// refusedFiles (hostram.go), which is for a lasting permission fault, and an absence is never fed to that.
var (
	absentMu      sync.Mutex
	absentExpired = map[string]time.Time{}
	absentCount   atomic.Int64 // len(absentExpired), so the common read (nothing remembered) takes no lock
)

func rememberAbsentExpiry(path string) {
	now := gateNow()
	absentMu.Lock()
	defer absentMu.Unlock()
	for p, until := range absentExpired { // drop what has run out, so names that are never read again do not pile up
		if !now.Before(until) {
			delete(absentExpired, p)
		}
	}
	absentExpired[path] = now.Add(absentMemory)
	absentCount.Store(int64(len(absentExpired)))
}

func absentExpiryRemembered(path string) bool {
	if absentCount.Load() == 0 {
		return false
	}
	absentMu.Lock()
	defer absentMu.Unlock()
	until, ok := absentExpired[path]
	if ok && !gateNow().Before(until) {
		delete(absentExpired, path)
		absentCount.Store(int64(len(absentExpired)))
		return false
	}
	return ok
}

// forgetAbsentExpiry is called when a read of the name succeeds and when no fresh staged file stands beside it.
func forgetAbsentExpiry(path string) {
	if absentCount.Load() == 0 {
		return
	}
	absentMu.Lock()
	defer absentMu.Unlock()
	delete(absentExpired, path)
	absentCount.Store(int64(len(absentExpired)))
}
