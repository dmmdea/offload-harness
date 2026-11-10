package main

// The hold bound re-queues instead of ending the reserve while --wait has time left.
//
// A lease that unloaded the seat and then waited for the host's RAM used to end, when the hold bound passed, with the bound's
// failure: the lease released, nothing run, exit 1, and the card it had held for the bound's length idle until a human noticed
// and launched the command again (two media lanes on one node: the second sat 10 minutes behind memory the first one never took,
// failed, and its card stayed empty). The bound exists so cards are not held idle behind a figure the host cannot meet; it never
// meant the caller's --wait was over. So while --wait has time left the wrapper lets the cards go, takes its place in the queue
// again (others waiting for the cards go first) and starts over under the new lease, with one line saying so. The reserve fails
// only when --wait is spent, with the same errors as before. The re-queues are not counted against maxLeaseRequeues (the
// ping-pong cap of a lease LOST mid-drain): --wait is their bound.

import (
	"fmt"
	"time"
)

// holdBoundRequeueBudget is the wait a reserve whose hold bound just passed may still spend in the queue: what remains of --wait,
// counted from when the reservation began queueing (the clock requeueWait and drainDeadline use). 0 = none: --wait is spent, or
// it was 0 (fail fast), and the bound's failure stands.
func holdBoundRequeueBudget(queuedAt time.Time, wait time.Duration, now time.Time) time.Duration {
	if wait <= 0 {
		return 0
	}
	if left := queuedAt.Add(wait).Sub(now); left > 0 {
		return left
	}
	return 0
}

// holdBoundRequeueLine is the one line printed when the bound passed and the reserve goes back to the queue.
func holdBoundRequeueLine(e *errHostRAMHoldBound, left time.Duration) string {
	return fmt.Sprintf("gpu reserve: the host RAM hold bound passed (%s) and the host still lacks the room (%s); releasing the cards and queueing again, %s of --wait left (the seat stays cleared meanwhile)",
		roundHold(e.bound), e.short.Error(), left.Round(time.Second))
}
