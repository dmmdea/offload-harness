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
func TestTheFirstScanReusesTheLaneReadingOnlyWhileItIsFresh(t *testing.T) {
	prev := verdictFresh
	verdictFresh = 250 * time.Millisecond
	t.Cleanup(func() { verdictFresh = prev })

	probesAfterTheFirstScan := func(t *testing.T, identityWait time.Duration) int {
		t.Helper()
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
		answerOf(t, runDoor(e.cfg, e.lr, e.req("a red door", nil), AtOnce))
		probes, _ := e.lr.counts()
		return probes
	}

	if got := probesAfterTheFirstScan(t, 0); got != 1 {
		t.Errorf("a reading as fresh as the ticket is reused by the first scan: probes %d, want 1 (the call's own)", got)
	}
	if got := probesAfterTheFirstScan(t, 600*time.Millisecond); got != 2 {
		t.Errorf("a reading older than %s by the time the ticket exists is asked again: probes %d, want 2", verdictFresh, got)
	}
}
