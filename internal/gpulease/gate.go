package gpulease

// The read gate: how long a reader of a lease file waits when the live name is absent and a staged replacement stands beside it.
//
// settled.go says when an absence is an answer. This file is what a READER does about an absence that is not yet one: on Windows a
// rename that succeeded can leave the live name absent for tens of milliseconds, with the staged file still beside it, so the read
// waits for the rename to end instead of answering "not there". Windows only, on the same switch as the wait for a refused open
// (deniedOpenMayClear): on a platform where rename(2) is atomic there is nothing to wait for, and the read answers at once.

import (
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
