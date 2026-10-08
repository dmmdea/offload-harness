---
status: Accepted
date: "2026-10-07"
---

# ADR 0076 — A call's width is sized from its deal; a batch of up to 16 is one deal; auto counts the local run-cap line

## Context

The 2026-10-07 diagnosis of the placement path found the harness used the fleet only when a caller opted in, and that even
then one call never had more than four jobs in flight. The semaphore in `RunWith` was the constant `runConcurrency = 4`.
The joint deal ([ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md) decision 4, [ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md)
decision 6) commits every subtask to a place with a free slot, and with 4 nodes of 4 slots it can commit two to each; only four
goroutines ran, so each node saw about one. `RunBatched`, which `offload_research` uses for its 12 pages, cut the list into
chunks of eight and ran them strictly one after the other, so the second chunk waited for the slowest page of the first and a
12-page call used at most four of the fleet's sixteen slots. The comment on the constant gave a reason ("small enough that a
local fallback burst cannot stampede the one GPU") that predates the local seat's run-cap line, which now bounds exactly that.

The second defect is the one the wider call would have made bite. `route=auto` dealt every subtask to an idle local seat
without counting what it had committed to it: 8 subtasks, a run cap of 4 and one remote with 4 free slots made 8 local and 0
remote (the spread deal on the same inputs made 4 and 4). While the width was four the cap and the width happened to be equal;
at eight the overflow stood in the seat's own FIFO for up to the run's wall while an eligible remote idled, and concurrent
callers (several sessions, or parallel tool calls in one) each read the seat idle at their own start.

The operator's decision, quoted verbatim (2026-10-07):

> The offload harness must be fully built and wired to smartly and dynamically rout work utilizing the full
> capabilities of the Cluster's GPUs and systems

> this needs to be a super SMART, DYNAMIC, ADAPTABLE, OPTIMIZED, EFFICIENT AND FULLY PARALLEL SYSTEM THAT MAKES THE
> MOST OUT OF THE HARDWARE AVAILABLE WHILE DELIVERING THE HIGHEST QUALITY OUTPUT POSSIBLE

It is read as in [ADR 0073](0073-the-capacity-wait-runs-to-the-calls-deadline.md), the first of this series: placement uses every
node, sized by speed among the seats that are already quality-adequate, and adequacy (the tier matrix, `remoteEligible`,
delegator-side acceptance) is never relaxed to raise utilization. This ADR changes how many of a call's subtasks run at once,
how a list of pages is dealt, and how many an idle local seat is dealt; it touches no eligibility rule. It amends what
[ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md) (the `runConcurrency` siblings of its Consequences, and
the idle seat of its decisions 1 and 4), [ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) (decision 6, which
counted the local seat's line for the spread deal only) and [ADR 0032](0032-a-peer-held-seat-is-waited-for-not-deferred.md)
(decision 3, the sequential chunks) say, each noted where the text is. It keeps ADR 0073's decisions 9 and 10 as written
(decision 5 below) and notes there that the 4 in them is now the floor.

## Decision

1. **A call is as wide as its deal.** The semaphore is `dealParallelism()`: the sum, over the subtasks the deal committed to a
   place, of what each place takes at once, never less than `runConcurrency` (4):
   - a remote that **publishes** `max_concurrent_jobs` counts every subtask dealt to it. The deal already holds that count to
     the node's headroom (`max_concurrent_jobs - jobs_running - dealt`), and the process gate holds the dispatches this process
     has open to the node's admission ceiling (ADR 0063 decision 7);
   - a remote that publishes none counts at most `runConcurrency` **per node**. An unpublished ceiling is unknown and never a
     limit (`headroom`), so the deal can send such a node every subtask, and the bound the constant gave a call is the only
     evidence it can take them. The 4 is per node and not global: two such nodes give 8, not 4. A call whose only remote
     publishes none is held to 4 on that node, as it was;
   - the local seat counts `min(dealt, run-cap room)`. Past its room a local run waits in the seat's own FIFO
     (`pipeline/agenttask.go`, registers C-42 and C-60), and holding a run slot for that wait is what the constant bounded;
   - a subtask the deal gave no place counts nothing: the overflow handed to the capacity wait (`capacityWait`), and a local
     slot a text lease reserves (`reserved`).

   A call with no deal to read, `route=local`, has `runConcurrency`; `route=queue` hands its subtasks to the pull holders and
   returns before any semaphore exists (ADR 0030), so it has no width. The width is logged when it is not 4.
   The deal is read-only by the time the semaphore is built, so the width is a function of one snapshot.

2. **Why the 4 can go.** The reason on the constant was a burst on the one GPU. The local seat's run-cap line now bounds the
   runs started on it, from every process, first come first served (`AwaitSeatSlotReporting`, `fleet_max_concurrent_jobs`,
   default 4), and the deal counts that line (decision 4); a remote is bounded by the ceiling it publishes and the deal's
   headroom count, and a node past either answers `503` and the subtask is re-placed (ADR 0063). This is a reading of the code:
   the delegate tests drive a fake `LocalRunner`, so no test in this repository exercises the seat's FIFO.

3. **A batch of up to 16 is one deal.** `RunBatched` deals up to `MaxBatchSubtasks` (16) subtasks as ONE joint deal, through an
   internal `runWith` that takes the subtask bound as a parameter. A longer list is consecutive deals of 16, in order, with the
   summaries added up and `Summary.Batches` counting the deals (1 for a call of up to 16; it was 2 for 9 to 12 pages).
   `offload_research` sends at most 12 pages, so one research call is one deal across every node. **The doors that cap a call at
   eight keep doing so**: `agent_delegate` and the `delegate` CLI verb call `RunWith`, which still refuses a ninth subtask, and
   the tool's input schema (`maxItems` 8) is unchanged. The call deadline (ADR 0065) already spans every chunk of a batched call; it is untouched.

4. **`route=auto` counts the idle seat's line.** The deal reads `localRunCapRoom()` once per call and counts every subtask it
   gives an idle seat against it, as the spread deal counts the local seat (ADR 0063 decision 6). The idle seat still wins the
   first `room` subtasks (ADR 0050 decision 1; `gate.go`'s "an idle local node always runs the work"). The rest are dealt as a
   busy seat's are: through the ranking over the unchanged `remoteEligible` gate, the backlog gate and the headroom count, to
   the remotes with room; with none having room, to the capacity wait, which places the subtask on the first node that frees,
   the seat too (`localSlotAhead`). Only while no remote could run the contract at all is the seat's own line the only queue
   there is, and the seat keeps the work, as the spread deal does. A subtask moved off the seat says so in its placement reason
   (`the idle local seat's run-cap line is spent by this deal (4 of 4 free slot(s) dealt; ...)`). Because the overflow needs the
   roster, an idle seat's call reads the fleet's health when it has more subtasks than the seat's line takes
   (`len(subtasks) > room`); a call that fits the line reads none, as before.

5. **R1's rules hold at any width.** ADR 0073 decisions 9 and 10 are stated in terms of `callDeadline.unstarted()` and the
   reserve, not of the number 4, and nothing about them moved. A wait that holds a run slot while a subtask of the call has not
   started keeps its TTL, and only the last subtask to start waits to the call's horizon; the launch loop starts nothing inside
   the reserve; the call-deadline unwind is the same. A wider semaphore means fewer subtasks are ever behind a wait, which is
   the point: the overflow, which counts nothing in the width (decision 1), starts behind the dealt subtasks as their slots
   free, and the last of them waits to the horizon.

## Consequences

- A 12-page research call opens up to 12 jobs at once, each node at most what it publishes and the local seat at most its
  room. The barrier between chunks is gone: no page waits for the slowest page of another.
- A node sees its whole share at once instead of staggered. The nets for a snapshot that is stale by construction are the
  ones that were there for the old width: the node's own admission (`503 queue full`), the process gate, and re-placement.
- A call wider than the idle seat's line sends the overflow to the fleet where it used to queue it locally. Work that used to
  stay on the box now travels when the box cannot start it. The first `room` subtasks of an idle box stay on it.
- Concurrent callers see each other through the run registry: the second caller's `localRunCapRoom` counts the runs the first
  has registered on the seat. A run registers when it starts, so a window remains in which two callers read the same room; the
  seat's FIFO is the net for that window, as it was.
- `Summary.Batches` is 1 for a research call of 9 to 12 pages where it was 2. Nothing else on the wire changes.
- The width is no longer a property tests can assume. The tests that pinned the eight-subtask chunk boundary moved to 16, and
  the ones that pinned "four at a time" are the ones that have no deal (`route=local`) or a node that publishes no ceiling.

## Alternatives considered

- **Count the overflow in the width, so every subtask starts at once and waits in the capacity wait together.** Rejected for
  this change: every one of them would then read `unstarted() == 0` and wait to the horizon, and each worker that frees would be
  raced for by all the waiters. Binding a waiter to a worker that is free is its own change (the diagnosis' M2); this one only
  stops the semaphore from being narrower than the deal.
- **A larger constant (8 or 16).** Rejected: it is not a function of the fleet. Eight against one old node over-commits it, and
  eight against a fleet of sixteen slots under-uses it. The deal is the only place that knows what was committed where.
- **The sum of every node's free slots, whether or not the deal used them.** Rejected: a slot the deal gave this call is the
  only slot the call may count on, and a node the deal passed over (a backlog past the caller's patience, an inadequate seat)
  is not capacity for it.
- **One global bound of 4 for all the nodes that publish no ceiling.** Rejected: it would hold a fleet of older nodes at the
  old width for no reason the old width had. The per-node reading keeps each such node at the bound it always had.
- **Release the run slot while a subtask waits** (ADR 0073's alternative). Still rejected, for ADR 0073's reasons.
- **Raise `MaxSubtasks` (the door's bound) to 16.** Out of scope: it changes the `agent_delegate` tool schema and what a model
  may send in one call. The batch bound is internal to `RunBatched`, whose one caller is bounded at 12 pages by its own schema.
- **Leave `route=auto` alone and rely on the width cap.** Rejected: the cap only coincided with the run cap at 4. At any wider
  call the overflow queues in the seat's FIFO while a remote with headroom idles, and the diagnosis measured the shape.

## Related code

- `internal/delegate/run.go`: `RunWith`, `runWith`, `RunBatched`, `dealParallelism`, `dealAutoRemote`, `dealAutoRemoteRoom`,
  `placeAutoRemote`, `anyRemoteEligible`, `dealSpread` (`dealRoom`), `localRunCapRoom`, the launch loop
- `internal/delegate/calldeadline.go`: `unstarted`, `noRoom` (unchanged; the rules this width must keep)
- `internal/pipeline/agenttask.go`, `internal/modelaffinity/seatslot.go`: the local run-cap line (`AwaitSeatSlotReporting`)
- `internal/delegate/deal_width_test.go`, `internal/delegate/deal_local_cap_test.go`, `internal/delegate/run_batched_test.go`,
  `internal/delegate/callwait_slots_test.go`

## Related docs

- [../../systems/fleet-node.md](../../systems/fleet-node.md), "How wide a call is, and how a list of pages is dealt"
- [0050-placement-ranks-adequate-seats-by-expected-completion.md](0050-placement-ranks-adequate-seats-by-expected-completion.md),
  the joint deal and the idle seat
- [0063-placement-holds-instead-of-sleeping-or-refusing.md](0063-placement-holds-instead-of-sleeping-or-refusing.md), the headroom
  count and the process gate the width relies on
- [0073-the-capacity-wait-runs-to-the-calls-deadline.md](0073-the-capacity-wait-runs-to-the-calls-deadline.md), whose decisions 9
  and 10 this width keeps
- [../../OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md), "Parallel sessions on one llama-swap" and "Delegate subtasks across fleet nodes"
