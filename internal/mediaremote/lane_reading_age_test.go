package mediaremote

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// THE READING THE FIRST SCAN REUSES IS AS OLD AS THE LANE QUESTION, NOT AS THE TICKET. A call asks the lane, then (for an image) waits on
// the fingerprints of this machine's weight files (F59 put the identity after the lane question, and each file can take up to 20 s),
// then makes the ticket. The ticket handed its first scan the reading as "fresh" by the time the ticket was made, so a lane that
// freed during the identity wait was still taken for held, and the ticket waited one more scan or went to an idle node. The age
// of the reading is the age of the question.
//
// The first scan is run by hand on a queue with no scanner, and the freshness window is chosen for what each half asserts. The test
// used to fail on loaded machines (about a third of 160 runs of it alone on one gate host and 4 to 8 % on another, at the 0.181.0
// release commit as at this branch): the lane runner counts EVERY question, and a live scanner asks again. It did so within ~40 ms
// of the first scan on the tests' rhythm and at once on any rhythm (TestANewScannerLooksAtItsFirstTicketOnce), so a count read after
// the call returned was 2 or 3 whenever the test goroutine was slower than the scanner. A clock-based window has the same
// shape: 250 ms between the question and the scan is 250 ms on an idle machine and not on a loaded one, so the half that asserts
// "reused" gives the reading a minute, and the half that asserts "asked again" needs the reading OLDER than its window, which a
// slower machine only makes truer.
func TestTheFirstScanReusesTheLaneReadingOnlyWhileItIsFresh(t *testing.T) {
	// probesAfterTheFirstScan makes the ticket for a call whose lane is busy and runs its first scan, and says how many times the
	// lane was asked in all: the call's own question and, when the first scan could not reuse that reading, the scan's.
	probesAfterTheFirstScan := func(t *testing.T, fresh, identityWait time.Duration) int {
		t.Helper()
		prev := verdictFresh
		verdictFresh = fresh
		t.Cleanup(func() { verdictFresh = prev })
		q, _ := manual(t)
		e := newQEnv(t, 1, true)
		if identityWait > 0 {
			var first atomic.Bool
			fp := localFingerprints
			swapLocalReaders(t, func(ctx context.Context, path string, size int64) string {
				if first.CompareAndSwap(false, true) {
					time.Sleep(identityWait) // the first file is slow to read: a cold cache
				}
				return fp(ctx, path, size)
			}, localPeek)
		}
		admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
		q.cycle(context.Background())
		probes, _ := e.lr.counts()
		return probes
	}

	if got := probesAfterTheFirstScan(t, time.Minute, 0); got != 1 {
		t.Errorf("a reading as fresh as the ticket is reused by the first scan: probes %d, want 1 (the call's own)", got)
	}
	const window = 250 * time.Millisecond
	if got := probesAfterTheFirstScan(t, window, 600*time.Millisecond); got != 2 {
		t.Errorf("a reading older than %s by the time the ticket exists is asked again: probes %d, want 2", window, got)
	}
}

// A SCANNER THE TICKET STARTED LOOKS AT IT ONCE, AND THE NEXT TIME WHEN THE RHYTHM SAYS. register starts the scanner for the process's
// first ticket and also wakes it, and the wake-up outlived the scan it was for: the new scanner's first scan took the ticket, the
// kick was still in the channel, and the scanner scanned the same ticket again at once. The second scan found the reading the first
// had reused gone and asked the lane again (one more card-table read) within milliseconds of the first, which is the double
// question the reuse exists to save. The rhythm here is the production one; a
// question inside 300 ms of the first scan can only have come from the wake-up.
func TestANewScannerLooksAtItsFirstTicketOnce(t *testing.T) {
	prevFresh, prevEvery := verdictFresh, scanEvery
	verdictFresh, scanEvery = time.Minute, 10*time.Second
	t.Cleanup(func() { verdictFresh, scanEvery = prevFresh, prevEvery })
	freshQueue(t)
	e := newQEnv(t, 1, true)

	answerOf(t, runDoor(e.cfg, e.lr, e.req("a red door", nil), AtOnce))
	time.Sleep(300 * time.Millisecond)
	if probes, _ := e.lr.counts(); probes != 1 {
		t.Errorf("a scanner the first ticket started scans it once, and the rhythm brings the next scan: probes %d within 300 ms, want 1 (the call's own; the first scan reuses it)", probes)
	}
}

// ... AND A SCANNER THAT IS ASLEEP STILL WAKES FOR A TICKET MADE WHILE IT SLEEPS. The cure above wakes only a scanner that was not
// started by the registration, and this pins the other half: with the rhythm at 10 s a second ticket's call must get its first
// scan now, not when the timer fires (8 s at the soonest), because the call waits for that scan to answer where it stands.
func TestATicketMadeWhileTheScannerSleepsIsScannedAtOnce(t *testing.T) {
	prev := scanEvery
	scanEvery = 10 * time.Second
	t.Cleanup(func() { scanEvery = prev })
	freshQueue(t)
	e := newQEnv(t, 1, true)

	answerOf(t, runDoor(e.cfg, e.lr, e.req("a red door", nil), AtOnce)) // starts the scanner, which now sleeps for about 10 s
	begin := time.Now()
	answerOf(t, runDoor(e.cfg, e.lr, e.req("a blue door", nil), AtOnce))
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("a ticket made while the scanner sleeps is scanned at once (its call answers where it stands): the call took %s, the scanner's rhythm is 8 s at the soonest", took.Round(10*time.Millisecond))
	}
}
