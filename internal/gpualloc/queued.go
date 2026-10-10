package gpualloc

import (
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// QueuedClaims lists the cards callers are queued for ahead of a new arrival: live waiters and
// the live tokens (a place held for a caller who may come back). They are not free for a newcomer,
// who queues behind them (FIFO). A queued whole-node request is a barrier for every card. ownToken
// is the caller's own place, never counted against it.
//
// WHY THE ALLOCATOR READS IT (register D-1xx-3, 2026-10-09). The lease directory shows only what is
// HELD, so a card that a registered waiter is queued for, and that nobody holds at this instant,
// reads as free to the allocator. A request that picks such a card and claims it through the gated
// Acquire is told the waiter is ahead (ErrStillQueued) and has to pick again; a request that never
// asked the line would have won the card ahead of the waiter, which is the starvation this closes.
// Both doors that allocate (the reserve verb and the media admission) add this set to the cards the
// allocator may not treat as free, so they choose a card nobody is queued for when one exists and
// queue FIFO on the busy set when none does, instead of finding out at the claim.
//
// Merge the result into AllocInput.Claimed (Need.Claimed before BuildInput, or the map BuildInput
// returned): a card in it is "not free right now", and the allocator still lists it among the cards
// a queued request can be given.
func QueuedClaims(m *gpulease.Manager, cards []gpuprobe.Card, ownToken string) map[string]bool {
	out := map[string]bool{}
	add := func(devs []string) {
		if len(devs) == 0 {
			for _, c := range cards {
				out[c.LeaseID()] = true
			}
			return
		}
		for _, d := range devs {
			out[d] = true
		}
	}
	for _, w := range m.Waiters() {
		if ownToken != "" && w.Token == ownToken {
			continue
		}
		add(w.Devices)
	}
	for _, t := range m.Tokens() {
		if t.ID == ownToken || !m.TokenLive(t) {
			continue
		}
		add(t.Devices)
	}
	return out
}
