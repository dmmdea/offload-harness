package gpualloc

import (
	"time"

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
//
// declaresHostRAM is whether the asker declares any host RAM: a waiter that waits ONLY on host RAM does
// not hold its cards against one that declares none (gpulease.Waiter.HoldsItsCardsAgainst, G6 of the P0
// plan), so those cards are free to it, as the gated claim will find them; without this the allocator
// would steer a request that passes such a waiter away from an idle card, and a call that cannot wait would
// be told the card is "promised to callers ahead" by a waiter it can pass.
func QueuedClaims(m *gpulease.Manager, cards []gpuprobe.Card, ownToken string, declaresHostRAM bool) map[string]bool {
	return QueuedClaimsBefore(m, cards, ownToken, declaresHostRAM, time.Time{})
}

// QueuedClaimsBefore is QueuedClaims for a caller that RESUMES a place in line that arrived at since: only the
// waiters and places that arrived strictly before it are ahead of it, because the lease queue itself serves a
// resumed place by arrival time (a caller behind it does not hold the card against it, and a tie is not counted: the
// answer errs toward free). A zero since is a new arrival, for whom everyone in line is ahead (QueuedClaims). It
// exists for the ticket queue: with two places on one idle card each read the other as in its way, and neither
// would ever be started (P0 plan S2).
func QueuedClaimsBefore(m *gpulease.Manager, cards []gpuprobe.Card, ownToken string, declaresHostRAM bool, since time.Time) map[string]bool {
	ahead := func(sinceMs int64) bool { return since.IsZero() || sinceMs < since.UnixMilli() }
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
		if !w.HoldsItsCardsAgainst(declaresHostRAM) || !ahead(w.SinceMs) {
			continue
		}
		add(w.Devices)
	}
	for _, t := range m.Tokens() {
		if t.ID == ownToken || !m.TokenLive(t) || !t.HoldsItsCardsAgainst(declaresHostRAM) || !ahead(t.SinceMs) {
			continue
		}
		add(t.Devices)
	}
	return out
}
