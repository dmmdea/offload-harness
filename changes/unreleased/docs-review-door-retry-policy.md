### Changed - the review lane's page states what each defer gets: what is retried, what is waited for, and what a cold seat costs

- **What was open.** `docs/systems/mcp-server.md` described `offload_review_diff`'s fence handling and its filters but never said
  what the door does with a defer, so a caller could not tell a capacity defer it should re-call from an abstention it should not,
  and nothing named the waits and the one retry that already surround the door. The page also still said that, with the fleet
  declining, the local leg waits out the lease and then defers. It stopped doing that when the in-run fence check landed: while the
  fence holds, the local leg defers `capacity` at once, naming the holder and its declared expiry, and only a fence taken after that
  check meets the cordon wait.
- **The policy, written down.** A block closing the lane's section: the door has no retry loop and no backoff of its own, and a retry
  never goes to the same seat. It lists the four bounded things that already exist around it so nobody adds a fifth (the admission
  budget, the in-run contention ladder, the fleet's capacity wait, the fleet's one retry), then one row per defer shape with its class,
  what the door does, whether it retries, and what the caller does: a foreign fence (offered once to the fleet, then deferred at once), a
  fence taken after the door looked, a full run line, a seat unfit to answer, a failed stack, an abstention, a ceiling and a refused
  call. A cold seat is stated as not a defer, with the reason the lane neither keeps a seat warm nor warms one ahead of a call.
  A plain text reservation is stated as not a fence, so the review runs on the seat.
- **Where the code is behind the policy.** The page says so, stated as not built: a local-leg defer that is a fact about
  the seat or its cards is not offered once to the fleet the way a fence seen on entry is, and the review's result carries no admission
  block. No code changed.
- **What ends a review.** The block states it from the node's own rules. A stall is `infrastructure` (reason `stalled:`, not retried
  by the delegator) and sits in the row for a seat unfit to answer. The wall is the run's expectation and not a deadline, and a run
  still producing is ended only by the liveness ceiling, max(3 x the seat's estimated run time, 2 x the wall, the 1800 s floor) and
  never above 4 h, which files `budget` or, when the run only sat behind the seat's other work, `capacity`. The ceiling row names its
  real stoppers (the ceiling, the step budget, a completion cut at its token budget, a re-pack the clock skipped or cut), and the
  patience bullet says the call has no fixed upper bound at the wall: the admission budget, the wait in the seat's run line and then
  the run to its ceiling. The 30 to 90 s figure is `offload_ask`'s and is not quoted for a review.
- **The retired fallthrough wait, everywhere the page said it.** The salvage paragraph still said a deferred fleet review is
  salvaged "instead of falling through to a local wait behind the very lease that fenced it". With the in-run check that leg defers at
  once, so the page now says so there too, and ADR 0041 carries a dated amendment on its 2026-09-15 note.
- **Tests.** `review_policy_docs_test.go`, red before the page changed: the policy block must exist with one row per defer shape and
  the bounds it quotes taken from the constants they come from (the admission budget, the contention budget, the placement wait, the
  wall ceiling, and the liveness ceiling's floor and cap); the ceiling row must lead with `budget`, name the ceiling, the step budget
  and the `capacity` filing, and say nothing of a stall; the unfit-seat row must carry `stalled:`; the patience bullet must not bound
  the call at the wall; and neither retired sentence about the fenced fallthrough waiting may be on the page.
