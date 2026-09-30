---
status: Accepted
date: "2026-09-30"
---

# Placement holds instead of sleeping or refusing: a 503 re-places at once, the queue budget follows the node's ETA, and overload is counted per process

## Context

On 2026-09-29 one delegating session sent 648 of the day's 836 delegated attempts to the fleet, with a
median of 21 and up to 37 open at once, into about 13 execution slots. The delegator had no overload policy: every capacity
signal turned into "sleep, or wait a bounded time, then refuse", and every bound was per Run, not per
process.

- A `503 queue full` made the delegator sleep the node's `Retry-After` (the node clamps it to 300 s) on
  the node that had just refused, retry that node once, and charge the sleep to the contract's budget.
  232 refusals cost about 16 caller-hours asleep while other nodes had room. Re-placement then reused the
  run-start snapshot for `route=spread`, so it kept choosing nodes the run's own siblings had filled.
- The queue deadline was a fixed five minutes whatever the node said. A node whose recent jobs took
  444 s behind one worker was handed work that could not start in five minutes; the delegator gave up at
  the deadline, the node ran the job anyway, and the abandoned run held a worker for a caller nobody was
  waiting for. In the hour before the diagnosis those runs held 38 to 96 percent of the slow nodes' wall
  time, and their finished walls fed the node's own `Retry-After` estimate: a loop.
- A capacity defer from the local seat (`seat busy`, no step ever ran) was called re-placeable in four
  comments and consumed by nothing, so it was terminal. 26 of the 52 jobs re-placed onto the local seat
  after a remote refusal died in its line.
- `spread` dealt by rotation (`i mod len(nodes)`) with no count of a node's free slots. The two slowest
  nodes took half of the day's first placements and returned a verified result for fewer than one job in
  five.
- Nothing remembered that a page had failed: at the first count 301 distinct research pages had taken 713
  jobs, and the worst page had taken 24 attempts.

The operator's rules that bound the fix: a busy node or seat is a place in line, never a reason to refuse
(INV-4); and no seat decision carries a wall-time term except as a feasibility refusal that names its
arithmetic, or an ordering key among seats that already passed the adequacy gate (INV-5 and its rider).

## Decision

1. **A dispatch 503 returns at once.** `runRemote` no longer sleeps. The refusal re-places the subtask on a
   node that has room. The node's `Retry-After` becomes a per-node cooldown for the run (jittered once,
   when the refusal happens, so dispatchers do not all re-ask together), and only the capacity wait
   consumes it: the wait credits the time it idles, so it is never charged to the contract, and asks the
   node again when the cooldown ends. A refusal that carries no hint cools the node for
   `refusalCooldown`. A hint at or above the delegator's own queue ceiling (`maxQueuedWait`, 300 s) is
   capped at that ceiling, and the ceiling holds after the jitter (the excess is folded back under it, so
   the re-asks stay spread and a node is never held out for more than 300 s): the comparison is the
   header's number against the delegator's constant, never the node's wording. A wait that ends with a
   node still cooling down says so in its defer.
2. **Re-placement reads the fleet again, for every route.** A snapshot taken before the refusal is never
   reused; one taken after it is (the 2 s probe memo), but siblings that refuse together each read the
   fleet once, the cost the Consequences name. A candidate must be eligible,
   have room by its own advertisement, be able to start the job inside the caller's patience (5 below),
   not be in cooldown, and not have a full process gate (7 below); all of it is judged against the wall the
   re-placement will actually carry (what the first attempt left of the budget), not the contract's
   original one. With no candidate the subtask goes to the capacity wait, whatever kind of refusal started
   the chain: a node the filters held out only for being busy (no room, a cooldown, a backlog past the
   patience, a full process gate) and a full local run-cap line are places in line, while a node the
   contract can never run on is not. The local seat is the last resort only when its run-cap line has a
   free slot ahead of a newcomer (the registry count the seat's own first-come-first-served gate uses); a
   full seat is a line the subtask could not leave.
3. **A local-leg capacity defer is re-placeable.** A defer of class `capacity` with zero steps, produced
   by the local run, joins the refusals a node may answer differently. `route=local` waits in place (the
   seat's own line is the wait), and so does a delegator with no remotes. The time the local run already
   waited in line is credited back. A capacity defer a REMOTE node files after acking a job is an observed
   terminal answer and is never re-placed: nothing after a 202 ever is. If nothing else has room, the
   local seat's defer is published as the defer it is, never as a `placement refused` failure. The defer
   alone is no reason to wait: only an eligible node that is merely busy is worth waiting for, so a defer
   beside a remote that can never run the contract is published at once.
4. **The queue budget follows the node's ETA.** How long the delegator waits for a job to START is
   `clamp(1.5 x etaStart + 30 s, 60 s, patience)`, where `etaStart` is the node's own
   `queue_wait_estimate_sec` or the arithmetic over its jobs and recent wall, and patience is the
   contract's poll budget. It replaces the fixed five minutes. The delegator reads the node's ETA again
   when a job is first seen sitting in the backlog and extends the wait if the snapshot was too short,
   because a `spread` run's snapshot can be minutes old; a read that fails is counted and logged, tried
   again on the next queued poll (three tries at most) and named in the queue-deadline message if it never
   succeeded. A node that publishes no ETA is no opinion and keeps the wait that always applied,
   `min(patience, maxQueuedWait)`; so does a node that publishes an impossible (negative) one.
5. **The backlog gate.** `startsWithinPatience` holds a node out of the deal, the re-placement candidates
   and the capacity wait when its ETA to start a new job exceeds the caller's patience. It is a placement
   feasibility refusal in the same class as `feasibleFinal`: it prints its arithmetic ("a new job would wait
   ~444 s to start (1 running + 0 queued - 1 worker + 1 = 1 ahead x 443.6 s recent wall / 1 worker), past
   the 360 s this contract will wait for a start", a 300 s wall plus the 60 s grace), it never ranks seats by speed, and it is not a pass
   rule. A held-out node is read again every tick and is never refused for good.
6. **The spread deal counts capacity.** No node is dealt more subtasks in one run than its headroom
   (`max_concurrent_jobs - jobs_running`, no floor; an unpublished ceiling is unlimited). A node at its
   headroom leaves the rotation; the fit order, the cycle and the one-subtask-per-seat-per-cycle
   invariant are untouched. The overflow is handed to the capacity wait. The local seat is counted the
   same way: its room is `fleet_max_concurrent_jobs` minus the runs already registered on it (read once per
   deal), and a deal that has spent it takes the seat out of the rotation, so the overflow waits for the
   first node that frees, the seat included, instead of piling into its own line. The count binds only
   while some remote could run the contract: with none, the seat's own line is the only queue there is. A
   subtask handed to the wait this way is neither a lease nor a refusal: it takes the local seat back only
   once the seat stops reading busy by the deal's own reading, with the wait off it is a capacity defer, it
   is not counted as a replacement, and the deal names every node it passed over with its arithmetic,
   a remote that failed its health probe included.
7. **A process-wide in-flight gate.** The delegator counts, per node, the dispatches this process holds
   open across every concurrent Run, and does not send one that would take a node past its published
   admission ceiling (`max_queue_depth`). The subtask waits in line for the first node that frees. The
   count ends with a terminal answer or with the delegator giving up on the job (a queue deadline, a
   cancel), and not a moment later: what the delegator then does with a finished answer on its own host
   (re-packing one the node could not, which can take minutes) is not the node's work and holds no slot
   on it. A turn-away sends nothing, so it is no refusal and is not counted as a replacement.
8. **A per-page retry cap.** A research page whose last three issues (the original and two re-issues) all
   failed after a seat ran them is backed off for 15 minutes with a contract-class defer that says so. An
   issue counts as failed only when a seat ran it and produced no verified digest: a failed verification,
   an abstention, a budget defer or a node's own job error. A full node, a lease, a bad token, a dead
   node, a cancel and a queue deadline never count. A success forgets the page; so does the time. Only research digests are keyed (by the page's content, not
   its file name).
9. **Research digests are routed by contract shape.** A contract from a research door with one context
   page and an output schema is mechanical work by construction; the words of the caller's question
   ("architecture", "why", "how", "compare", "trace") no longer send it to the roomiest seat first. Window
   adequacy still gates every seat, and every other contract is classified by its goal as before.
10. **The second chance queues for a busy seat.** A verification retry whose seat is running another job
    waits for it (the placement wait, credited) instead of being skipped, so the two runs still never share
    the seat. It is skipped only when the seat stays busy for the whole wait or the wait is off, and a
    caller's cancel is reported as a cancel. The wait never places the retry on the seat that took the
    first attempt: the retry's premise is a different seat.

## Consequences

- Refusals become latency in the capacity wait, which is credited and re-reads the fleet every tick. The
  wait is still bounded by `agent_placement_wait_sec` (120 s by default); a wait that runs to the call's
  deadline is a separate change, and the two ship in that order because an open-ended wait can outlive the
  client's abort.
- The delegator stops creating the abandoned runs that fed the loop, but it does not clean up the ones
  already on a node by itself: taking a never-started job back is
  [ADR 0064](0064-a-delegator-takes-back-what-it-has-not-started.md)'s change, and against a node that
  predates it the queue budget and the gate are the whole defence.
- The two decisions meet at the queue deadline. The instant is this ADR's (the ETA-derived budget); what the
  delegator does there is ADR 0064's: it asks the node to take the job back, once. A confirmed withdrawal is
  filed as a capacity refusal, so it enters this machinery like a 503: the node goes on the run's cooldown
  for `refusalCooldown` (the withdrawal carries no `Retry-After`), the subtask is re-placed against the wall
  the first attempt left with the time the job sat queued credited back (once, in `placements.noteRefusal`),
  and with no candidate it waits in line. It is the one job re-placed after a `202`, because it is the one
  job the node says it never ran; a capacity defer a remote files after a `202` still never moves. A
  withdraw the node does not confirm changes nothing, and the row says why after the budget note.
- One extra health read per queued job (up to three when the node does not answer it), and one per
  re-placement, siblings that refuse together included; no read for a job that starts at once.
- `maxQueuedWait` stays as the ceiling for a node with no ETA, as the yardstick for a `Retry-After`, and
  for the pull-queue lane, which has no health view to derive a budget from.
- The process gate and the page cap are process-wide state. Tests that use them key on unique bases and
  page texts; there is no reset switch.
- A stale ETA can still misjudge a node between the read and the dispatch. The 503 and the re-placement
  are the net for that, as they were.

## Alternatives considered

- **Keep the courtesy sleep and credit it.** Rejected: it still sleeps on a node that just refused while
  another node has room, and the wait cannot see a node that frees during it.
- **Rely on the node to withdraw abandoned jobs.** Needed, but it does not stop the delegator creating
  them, and old nodes keep today's behaviour until they are upgraded.
- **Prefer the fastest node.** Rejected: quality outranks speed. Capacity and backlog decide only
  whether a node can take the job in the time the caller gave it, and the order among the nodes that
  can is unchanged.
- **Per-Run bounds only.** Rejected: the overload came from many Runs in one process.
- **Cap a page after its first failure, or forever.** Rejected: a page can fail for a reason that passes,
  so three failures back it off for a while and a success forgets it.

## Related code

- `internal/delegate/run.go`: `runRemote`, `placeAndRun`, `replacementNode`, `awaitCapacity`,
  `noteCooldown`, `capacityDeferRefusal`, `placeSpreadWith`, `awaitRetrySeat`
- `internal/delegate/eta.go`: `etaStartFor`, `startsWithinPatience`, `queueBudgetFor`
- `internal/delegate/gate.go`: `hasRoomWithin`; `internal/delegate/autopoll.go`: `pollBudgetFor`
- `internal/delegate/processgate.go`: the process gate and the page cap
- `internal/delegate/fit.go`: `digestShaped`

## Related docs

- [../../systems/fleet-node.md](../../systems/fleet-node.md), the capacity wait, the spread deal and the
  queue deadline
- [0050-placement-ranks-adequate-seats-by-expected-completion.md](0050-placement-ranks-adequate-seats-by-expected-completion.md),
  whose `Retry-After` courtesy retry (item 7) this replaces
- [0061-liveness-judges-the-seat-engine-busy-hold.md](0061-liveness-judges-the-seat-engine-busy-hold.md)
