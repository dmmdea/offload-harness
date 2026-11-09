package mediaremote

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// probeRetryPause is how long the asker waits before it asks again a node whose /fleet/health read timed out. A node whose cards are
// all busy answers its health slowly; one timeout is not "this node does not serve the task", and a batch that read it so slept its
// whole longest step (batchWaitSteps) before it looked again. Chosen, not measured. A variable so a test can shorten it.
var probeRetryPause = 2500 * time.Millisecond

// probeRosterRetrying is rosterprobe.Probe with one retry for the members whose read ran out of time: after probeRetryPause those are
// asked once more (their negative verdict forgotten first, or the cache would replay the timeout), and the second answer replaces the
// first. A member that was refused, answered with a status, was served from the memo or the negative cache, or failed any other way is
// not asked again; nor is anything once ctx ends.
func probeRosterRetrying(ctx context.Context, remotes []string, token string, timeout time.Duration) []rosterprobe.Reading {
	rs := rosterprobe.Probe(ctx, remotes, token, timeout)
	var at []int
	var again []string
	for i, r := range rs {
		if r.Err != nil && r.Refused == nil && r.Source == rosterprobe.SourceProbe && probeTimedOut(r.Err) {
			at = append(at, i)
			again = append(again, r.Base)
		}
	}
	if len(at) == 0 || ctx.Err() != nil {
		return rs
	}
	if !sleepCtx(ctx, probeRetryPause) {
		return rs
	}
	for _, b := range again {
		rosterprobe.Default.Forget(b)
	}
	second := rosterprobe.Probe(ctx, again, token, timeout)
	for k, i := range at {
		if k < len(second) {
			rs[i] = second[k]
		}
	}
	return rs
}

// probeTimedOut says a health read failed by running out of time (its own bound), as opposed to being refused or unroutable.
func probeTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
