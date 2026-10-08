---
status: Accepted
date: "2026-10-07"
---

# The capacity wait runs to the call's deadline, patience is the time the call has left, and a refusal's cooldown yields to a free worker

## Context

On 2026-10-07 a delegation session pinned `route:"local"` on `offload_research` and 32 of 78 pages ended
`not started: the call ended first`; the rerun without the pin landed pages on every node. The diagnosis that
followed (six readers and a skeptic over the whole placement path, then the delegator ledger) found the harness
used the fleet only when a caller opted in, and that where it did wait for capacity the wait ended long before the
call did. The top cause of a deferred delegate row in the day's ledger, 85 of 281, was
`capacity wait: no node had room within 2m0s`, for a subtask that a node freeing a minute later would have taken,
inside a call that has 1,500 s.

The operator's decision, quoted verbatim (2026-10-07):

> The offload harness must be fully built and wired to smartly and dynamically rout work utilizing the full
> capabilities of the Cluster's GPUs and systems

> this needs to be a super SMART, DYNAMIC, ADAPTABLE, OPTIMIZED, EFFICIENT AND FULLY PARALLEL SYSTEM THAT MAKES THE
> MOST OUT OF THE HARDWARE AVAILABLE WHILE DELIVERING THE HIGHEST QUALITY OUTPUT POSSIBLE

It is read as: placement uses every node, sized by speed among the seats that are already quality-adequate. Quality
adequacy (the tier matrix, eligibility, delegator-side acceptance) is never relaxed to raise utilization, and nothing
in this ADR touches it. This ADR is the first of a series; it changes only how long a subtask that no node can take
right now is held before it is deferred.

The two decisions that built the wait named this change and held it back for one reason.
[ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) (Consequences): "a wait that runs to the call's
deadline is a separate change, and the two ship in that order because an open-ended wait can outlive the client's
abort". [ADR 0065](0065-the-whole-call-has-a-deadline-below-the-clients-abort.md) gave the two MCP delegation doors
that deadline, called it "the precondition of the next placement change (the delegator's capacity wait running until a
call deadline instead of a fixed TTL)", and listed the wait under "Not solved here". The precondition has held since
0.156.0.

## Decision

1. **A call that has a whole-call deadline waits for capacity until that deadline, less a reserve.** The two MCP
   delegation doors (`agent_delegate`, `offload_research`) own a deadline (`agent_call_deadline_sec`, default 1,500 s,
   measured from handler entry). A subtask that every node able to run it has refused for capacity (a 503 or 429), or
   whose only placement is a seat a text lease reserves or fences, or that a deal or the process gate sent to the wait,
   now stays in the delegator's queue until `deadline - callWaitReserve`, re-reading the fleet's health every tick and
   landing on the first node that frees. Band-0 production work never defers for capacity while the call still has time
   to place it, once every subtask of the call has started (decision 9: a wait that holds a run slot somebody else needs
   keeps its old bound). The wait is credited to the contract's budget exactly as before: it is never charged to `timeout_sec`.

2. **What `agent_placement_wait_sec` means now.** It is the wait of a call that has **no** deadline: the `delegate` CLI
   verb, and `agent_run` and `offload_ask` with a route (whose doors carry no deadline, ADR 0065). 0 keeps the built-in
   120 s; a positive value is that many seconds. **Under a deadline the key does not bound the wait of the last
   subtask to start**, in either direction, so an operator who set it to 120 s when that was the default is not left with
   the old behaviour on the doors that matter; it still bounds the wait of a subtask that holds a run slot while others
   have not started (decision 9). To shorten the wait of the MCP doors, shorten `agent_call_deadline_sec`. A negative value is the
   operator's off switch and stays one for every call: a deadline does not switch the wait back on.

3. **Three waits keep their configured bound under a deadline, and the lease wait does not outlast the call.**
   - A composite decision's eviction wait (`decided`, ADR 0039): its expiry RUNS the contract on the decided seat, which
     would start with only the reserve left and be cut.
   - The verification retry's wait for a busy seat (`awaitRetrySeat`, ADR 0063 decision 10): an optional second opinion
     on an answer the subtask already holds, and holding that answer for the rest of the call to improve on it would cost
     the caller more than the retry can return.
   - A wait that holds a run slot while subtasks of the call have not started (decision 9).

   The bound those waits keep is the one the wait had before this ADR: the larger of `agent_placement_wait_sec` and
   `agent_lease_wait_sec`, as it always was. A wait that the **call** bounds is different: it ends at the horizon even
   when `agent_lease_wait_sec` is longer, because a wait past the call's deadline cannot help anyone, the call is over
   before it ends. (The first version of this decision let a longer lease wait win over the horizon; that is a wait
   the deadline would cut with no reserve, the failure decision 4 exists to prevent.)

4. **The reserve.** The wait ends `callWaitReserve` (15 s) before the deadline and dispatches nothing inside it. It is the
   least execution budget a dispatch is ever handed (`minRetrySec`, 10 s), one poll to read the answer (`pollEvery`, 3 s)
   and a little slack. A job placed in the call's last seconds can only be cut by the deadline and then runs on in its
   node with nobody waiting for it, the ghost [ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) exists to
   stop creating. The last look of a TTL wait, which falls after its final sleep, is not taken by a wait bounded by the
   call. A call with no more than the reserve left does not wait at all: its subtasks end at once as a `capacity` defer
   that says so. That is neither the operator's off switch (`placement refused`, wait disabled) nor a wait that ran.

5. **The end of the wait says what ended it.** The capacity defer keeps its class (`capacity`) and prefix, and reads
   `capacity wait: no node had room before the call's deadline (waited …; bounded by the call's deadline, not by
   agent_placement_wait_sec: the call had … left when the wait began, less the … a placed job needs to run in; …) —
   re-run later or add a node`. It no longer tells the caller to raise a setting that never bounded the wait. The
   holder-naming deferral of a reserved seat words the same fact. A wait that is still running when the deadline passes
   (an attempt in flight, a probe the deadline cut) is the deadline's cut, unchanged from ADR 0065 decision 2.

6. **Sheddable work is still shed at once.** `priority: -1` never waits, with or without a deadline.

7. **Patience is the time the call has left.** How long a job may wait to START on a node (ADR 0063 decision 5) was the
   contract's poll budget, 660-1,260 s for a `timeout_auto` contract, longer than the call can still honour late in it. A
   node with a 500 s backlog passed the backlog gate, the job was dealt, the call ended first, and a job the node had
   already started kept running there with nobody waiting for it (17 call-deadline cuts in one day, 11 of them with the job
   still on a node). The patience is now `min(poll budget, time left on the call - reserve)` at every site that reads
   it: the auto deal, the spread deal, re-placement, the capacity wait's tick and the one-word verdict that narrates them
   (the narration takes the gate's own function, so the two cannot diverge). The reserve is part of the clamp because a job
   that would start inside it is as useless as one placed inside it. The clamp is never zero, since a zero patience reads
   as "no bound"; a call with nothing left to start a job in lets through only a node that starts at once, and a node that
   publishes no ETA is still no opinion. It stays a feasibility refusal in the class of `feasibleFinal` and the backlog gate
   (ADR 0063 decision 5): it never ranks seats by speed. The reason prints the call's arithmetic after the gate's own
   (`... past the 30 s this contract will wait for a start; the call's deadline is 45 s away, less the 15 s reserved for a
   placed job to run in, and the contract's own poll budget is 21m0s`), so the number is never read as the contract's when
   the call set it. A call with no deadline is unchanged.

8. **A refusal's cooldown yields to a free worker.** A 503's `Retry-After` is the node's own estimate, at the moment it
   refused, of when a worker frees ([ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) decision 1 turns it
   into a per-node cooldown for the run). The wait re-reads health every tick and kept the node out until the hint ended
   all the same: a node that drained a minute into a 300 s hint stayed unused for the rest of the wait while the subtask
   deferred, and with decision 1 the wait is now long enough for that to matter. A health read taken **after** the refusal
   that **proves** a free worker lifts the cooldown, in the capacity wait and in re-placement: the node publishes its
   worker ceiling, fewer jobs run than that, none waits in its backlog, and it does not report itself saturated
   (`freeWorkerProven`). The proof is positive evidence only. It is the second proof `provablyStartsNow` accepts and not
   the first: a node whose queue is empty because it publishes no counters (an older node) proves nothing about its
   workers, and "unknown is never a yes" is the rule every capacity reader here follows. The read must postdate the latest
   refusal in force: the wait asks the probe memo for a snapshot no older than it (`fleetReadForWait`), so a snapshot a
   sibling's probe took just before the refusal is never reused to judge it, and re-placement passes the instant it began
   (`withRoomAfter`). A node that refuses again after a lift is **firm** for the rest of the run: its counters did not
   predict its refusals (a stale VRAM snapshot, say), its `Retry-After` stands, and it costs one early ask, not one per tick
   of a 24-minute wait. The placement reason of the dispatch a lift allowed says so
   (`[its Retry-After cooldown (4m41s left) was lifted: its health, read after the refusal, shows 1 of 4 worker(s) running
   and none queued]`). It says so only of the dispatch the lift allowed: a node that refused again after the lift, or that
   is asked after the hint would have ended on its own, is not narrated as lifted (`cooldowns.liftNarration`). Unknown,
   saturated and backlogged readings keep the cooldown, as before.

9. **A wait does not hold a run slot to the horizon while subtasks of the call have not started.** A call runs
   `runConcurrency` (4) subtasks at a time and a waiting subtask holds its slot. *(Amended by
   [0076](0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md):
   the 4 is the floor of a width that follows the call's deal. The rule below is stated in terms of
   `callDeadline.unstarted()`, not of the number, and holds at any width.)* If every subtask waited to the horizon,
   four of them on a saturated fleet would hold all four slots for the rest of the call: the other four of an
   eight-subtask call, and every later chunk of a batched `offload_research`, would not start until the waiters ended, and
   would then start with less than the reserve left and be cut. The wait meant to stop capacity defers would have made
   "never started" ones. So the call-bound wait belongs to the **last subtask to start**, the one with nothing behind it:
   while the call has subtasks that have not started (`callDeadline.unstarted()`: the ones waiting for a slot and the ones
   of later chunks, counted over the whole batched call), a wait keeps the bound it had before this ADR (decision 3),
   unless the call's own horizon comes sooner. The capacity defer that ends such a wait says why a call with time left
   stopped waiting (`N subtask(s) of the call had not started, so this wait could not hold its run slot to the call's
   deadline`) and keeps naming `agent_placement_wait_sec`, which is the bound that applied.

10. **Nothing starts inside the reserve.** The launch loop starts no subtask once the call has no more left than the
    reserve (`callDeadline.noRoom`), whether the subtask waited a long time for a slot or belongs to a later chunk: a job
    started there could only be cut and would run on in its node with nobody waiting for it, decision 4's reason applied to
    the start of a subtask and not only to its wait. It ends as a `capacity` defer, `not started: the call's deadline left
    no room (the call had … left, no more than the … a placed job needs to run in)`, and not as the call-deadline cut,
    whose prefix means the deadline passed and which it had not. A call that never had more than the reserve (a deadline
    configured at 10 s) has nothing to take the reserve out of, and keeps starting its subtasks as before.

## Consequences

- A fully saturated fleet holds the last subtask of a call to the horizon, about 24 minutes of a default call, where it
  used to defer a subtask after 2. That is the point of the change, and it has three costs. The subtasks ahead of the
  last still wait only their TTL (decision 9), so a saturated fleet does not hold four run slots for the whole call: a
  call is as wide as it was, and longer by the last subtask's wait; a wider run limit is its own change. A subtask whose
  only places in line are leases that outlast the call holds the call to the horizon too, then ends as the
  holder-naming deferral (the holder and the soonest end are in it); the wait is not shortened by a guess at when a
  holder will release, since a lease's declared end is an upper bound that `gpu release` routinely beats. And a node that
  keeps answering 503 while its health reports free capacity is asked about once per cooldown, not once per tick: the
  Retry-After cooldown is what paces those re-asks, and decision 8 lifts it at most once per node per run.
- The call still ends before the client's abort. The horizon is `deadline - 15 s`, the deadline is 1,500 s from handler
  entry, and the live check stays "call wall <= deadline + 30 s".
- A config that pins `agent_placement_wait_sec` to a positive number no longer shortens the wait of the last subtask to
  start on the MCP doors, and the earlier ones keep it. There is no per-call way to ask for the old bound for the last
  one; the knob for it is `agent_call_deadline_sec`. A longer `agent_lease_wait_sec` no longer extends a wait past the
  call's horizon.
- The last `callWaitReserve` of a call starts nothing (decision 10): a research call whose later chunk begins that late
  returns those pages as `capacity` defers at once, where they used to begin and be cut.
- Tests that pinned the CUT of a wait by the deadline (ADR 0065) set the reserve to zero to keep pinning it; with the
  production reserve the wait ends first, as a capacity defer, and the cut remains for an attempt in flight.
- `Summary.Waited` and `results[].capacity_wait_sec` now report waits of minutes. Nothing else on the wire changes: the
  defer's class, prefix and `place_keeping` are as before.

## Alternatives considered

- **Release the run slot while a subtask waits.** Rejected for now: the waiters would no longer bound the call's width,
  so the subtasks behind them would start at once and dispatch in the same ticks, every one of them reading the same
  health and competing for the same room, and the process gate, the deal's headroom and the Retry-After pacing were all
  sized for `runConcurrency` open attempts. Capping the wait of a slot-holder at its old bound changes one number and
  keeps all of that. *(The width itself is sized from the deal since ADR 0076, which is the "wider run limit is its own
  change" the Consequences name; releasing the slot of a waiter stays rejected.)*

- **Keep the configured value as a ceiling and only change the default.** Rejected: a live config that pins the value
  (the historical default written out) would keep the defect, and there is no way to tell it from a deliberate choice.
  The operator's lever for the doors that matter is the call deadline.
- **Make the configured value a floor under a deadline.** Rejected: a floor longer than the call is the wait the deadline
  cuts, with no reserve; the reserve is exactly the guard that matters near the end of a call.
- **A reserve sized from the node's `min_turn_sec` or the contract's wall.** Rejected for now: `etaFor` is capped at the
  wall and priced on a wall-fitted final (the diagnosis' F06, which needs its own ADR 0050 amendment), so a per-node
  reserve built on it would inherit that ([ADR 0079](0079-the-ranking-eta-is-not-capped-at-the-wall.md) has since ended the cap and the wall fit; building a
  reserve on the new eta is a separate decision). A flat reserve is a floor, not a promise that the job finishes.
- **End the wait early when every place in line has a known end past the call.** Rejected: a lease's declared end and a
  node's estimate are upper bounds, and "provably hopeless" is not provable. The defer carries the soonest end
  (`retry_after_sec`) either way.
- **Let a wait outlive the call and return what is placed.** Rejected: it is the failure ADR 0065 exists to prevent.
- **Lift a cooldown whenever the node's queue reads empty (`provablyStartsNow`).** Rejected: an older node and every fake
  publish a queue depth of 0 and nothing else, so it would re-ask every refusing node on the next tick and make the
  cooldown decorative. The proof has to name a worker.
- **Lift it on any later health read.** Rejected: the memoised snapshot a sibling took just before the refusal still
  advertises the room the node denied; the read has to postdate the refusal.
- **No firmness: lift every time the proof holds.** Rejected: with the wait now as long as the call, a node whose counters
  disagree with its admission would be asked once per tick for 24 minutes, each ask a refused dispatch with its own
  telemetry rows.

## Related code

- `internal/delegate/run.go`: `capacityWaitFor`, `placementTTL`, `awaitCapacity`, `capacityDefer`, `reservedDefer`,
  `awaitRetrySeat`, `cooldowns` (`hold`, `lift`, `newestHold`), `fleetReadForWait`, `withRoomAfter`, `replacementNode`
- `internal/delegate/gate.go`: `freeWorkerProven`
- `internal/delegate/calldeadline.go`: `timeLeft`, `waitHorizon`, `callWaitReserve`, `patience`, `begin`, `unstarted`,
  `noRoom`, `unlaunched`
- `internal/delegate/placement_reason.go`: `patienceFn`, `oneWordVerdictWith`, `placementVerdictLine`
- `internal/config/config.go`: `AgentPlacementWaitSec`, `PlacementWait`, `DefaultPlacementWait`
- `internal/delegate/callwait_test.go`, `internal/delegate/callwait_slots_test.go`,
  `internal/delegate/patience_call_test.go`, `internal/delegate/cooldown_lift_test.go`

## Related docs

- [../../systems/fleet-node.md](../../systems/fleet-node.md), "The capacity wait" and "What bounds the wait"
- [0063-placement-holds-instead-of-sleeping-or-refusing.md](0063-placement-holds-instead-of-sleeping-or-refusing.md),
  which built the wait and named this change
- [0065-the-whole-call-has-a-deadline-below-the-clients-abort.md](0065-the-whole-call-has-a-deadline-below-the-clients-abort.md),
  which gave the call its deadline
- [../../OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md), `agent_placement_wait_sec` and `agent_call_deadline_sec`
- The same diagnosis' other fixes shipped with this one, each recorded where its subject lives: the spread deal's rate for an unmeasured seat
  ([0057](0057-placement-scores-the-cards-the-harness-can-use.md), F04), one number for a new job's wait ([0050](0050-placement-ranks-adequate-seats-by-expected-completion.md)
  amendment, F10), `remotes` that may only narrow `delegate_remotes` ([0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md)
  amendment), and in `fleet-node.md` the unlimited run cap (F11) and the eta-first mechanical order (F12).
