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
- **Tests.** `review_policy_docs_test.go`, red before the page changed: the policy block must exist with one row per defer shape and
  the bounds it quotes taken from the constants they come from (the admission budget, the contention budget, the placement wait and the
  wall ceiling), and the retired sentence about the fenced fallthrough waiting must be gone.
