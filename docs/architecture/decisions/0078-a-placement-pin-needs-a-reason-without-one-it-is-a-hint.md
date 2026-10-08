---
status: Accepted
date: "2026-10-07"
---

# ADR 0078 — A placement pin needs a reason; without one it is a hint

## Context

On 2026-10-07 a delegation session passed `route:"local"` to a 78-page `offload_research` call. 46 ledger rows read
`route=local forced`, and 32 pages ended `not started: the call ended first`: the whole fan-out queued on one seat while
four fleet nodes had free workers. A spread rerun of the same call landed pages on four nodes. `local` and `remote`
were hard pins: the engine obeyed them whatever the fleet looked like, and nothing recorded why a caller had narrowed
placement.

The operator's decision, quoted verbatim (2026-10-07):

> The offload harness must be fully built and wired to smartly and dynamically rout work utilizing the full
> capabilities of the Cluster's GPUs and systems

> this needs to be a super SMART, DYNAMIC, ADAPTABLE, OPTIMIZED, EFFICIENT AND FULLY PARALLEL SYSTEM THAT MAKES THE
> MOST OUT OF THE HARDWARE AVAILABLE WHILE DELIVERING THE HIGHEST QUALITY OUTPUT POSSIBLE

It is read as in [ADR 0073](0073-the-capacity-wait-runs-to-the-calls-deadline.md), the first of this series: placement uses
every node, sized by speed among the seats that are already quality-adequate, and adequacy is never relaxed to raise
utilization. Applied to the caller: a caller may not narrow placement with no reason recorded. A pin is not wrong in itself.
A benchmark has to run on the seat it measures, and credentials must not leave the box. But a pin is a decision, and a
decision with a reason can be recorded, counted and audited, where one without is only a way to leave most of the fleet idle.
This ADR changes what a `local` or `remote` route means; it touches no eligibility rule, and it amends what
[ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) and
[ADR 0076](0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md)
say of `route=local`, and decision 6 of [ADR 0040](0040-vision-work-travels-to-a-node-with-an-idle-card.md), each noted where the
text is.

Two facts about the code shaped it. The engine reads its route at three dozen sites (`r.route`: the deal, the capacity wait, the
re-placement fallback, the retry, the lease and fence reads), and every one of them already behaves correctly for `auto`, so a hint
is one decision at intake (the run applies `auto`) and not three dozen edits. And not every caller can name a reason:
`fleet-smoke` names one node and must measure that node; the review lane's fenced-seat fallthrough passes `route=remote`
precisely so that nothing runs on the fenced local seat; `offload_ask` and `agent_run` pass their caller's route through. The
reasons are for the surfaces where a session decides.

## Decision

1. **A closed set of reasons.** `pin_reason` is one of:

   | pin_reason | the caller's reason | valid with |
   |---|---|---|
   | `privacy` | the material may not leave this box (credentials, unreleased or brand-isolated data) | `local` |
   | `locality` | the work needs something only this box has: a local service, files the contract cannot carry inline, this box's own seat as the thing under comparison | `local` |
   | `measurement` | the call measures a specific place: a benchmark, a bake, a seat A/B | `local`, `remote` |
   | `operator` | the operator named this placement for this call | `local`, `remote` |

   A closed set, not free text: it can be grouped and counted, and a model that has to pick one has to say what kind of reason it has.

2. **It is checked at intake, before any placement.** A value outside the set, a reason with the wrong route (`privacy` with
   `remote`) and a reason with a route that is not a pin (`auto`, `spread`, `queue`, and `offload_research`'s default `spread`) are
   refused as bad input, with an error that lists the valid set (`CheckPinReason`). Only a pin can have a reason. The doors check
   first, before they spend anything: `agent_delegate` before a `context_paths` file is read, `offload_research` before a page is
   fetched, the `delegate` and `research` verbs before the config is loaded. The MCP doors answer in the deferred shape every other
   bad input has. The engine checks again, in `runWith` right after the route is recognised.

3. **A reasoned pin is authoritative to the end of the subtask.** Its first placement is what `local` and `remote` always were. Its
   second chance now obeys it too, which is new: a `route=remote` retry used to be able to land on the local seat, so a node's wrong
   answer under a reasoned remote pin now ends `failed_verification` where the seat used to recover it. A `local` pin is never retried on another node, and a `remote` pin's retry after a failed
   verification, an abstention, an admission defer or a seat-down defer goes to another fleet node or is not run, and never lands on
   this box's seat (`runner.remotePinned`, asked first in `alternativeNode`; the `retry_note` names the pin when no other node exists
   and when the retry ran, while a skip for the budget floor, an empty final, the call deadline or a busy retry seat keeps its own
   note). The guard is keyed on the
   reason and not on the route: the bare remote route of a caller with no reason channel (`fleet-smoke`, the review lane,
   `offload_ask`, `agent_run`) and a reasonless remote hint keep the `remote -> local` retry that `alternativeNode` documents as
   accepted, and a route-keyed guard would have changed every one of them. Its reason is on
   every ledger row (`pin_reason`) and on every result (`results[].pin_reason`). A call that carries a browse grant (`allow_browse`,
   [ADR 0060](0060-opt-in-browse-lane-drives-the-operators-browser.md)) is pinned whatever it says: the browse tool drives THIS machine's
   own browser, which is why the door admits it only on `route: local`, and a hint could have placed it on another node's. The grant is
   the reason, so a bare `local` on such a call is a pin under the implied reason `locality`, and its rows and results say so.

4. **A reasonless `local` is a hint, placed as `auto` places it ([ADR 0076](0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md)).**
   The idle local seat wins the first `room` subtasks of its run-cap line; the overflow goes through the unchanged `remoteEligible`
   gate, the backlog gate and the headroom count to the remotes with room, and with none to the capacity wait. A call that fits the
   seat's line stays local and reads no node's health, which is what the caller hinted. A seat a text lease reserves or a lease
   fences, one at its run cap, one loading, and one whose load would unload another loaded vLLM seat is not idle, and the work goes
   to the fleet or waits in line for it. A failed verification may be retried on another node, which a pin could not (`route local
   places nothing on another node`).

5. **A reasonless `remote` is a hint, placed fleet-first.** The deal treats the local seat as unavailable by preference, as
   `route=remote` always did, so a remote with room wins even while the seat is idle. Where `remote` would have deferred for lack of
   an eligible remote with room, the local seat may run the subtask instead, if it can:
   - no remote can run the contract at all: the seat runs it at once, and the reason says no remote was eligible (it used to defer
     `route=remote: no eligible remote`). A box with no agent seat (a delegation client) has no seat to run it, so there the hint ends
     as the same defer a remote pin gets, carrying the fleet's verdict, and counts as no override;
   - every eligible remote is at its headroom, or holds a backlog past the caller's patience: the deal gives the seat what its
     run-cap line takes, counted like a remote's headroom, in the deal and not in the wait. The wait would start the overflow only
     after a dealt subtask finished (the overflow counts nothing in the width, ADR 0076), and with the wait switched off it would
     defer while the seat idled;
   - a dispatch is refused: the re-placement falls back to the seat as it does for `auto`;
   - a dispatch is turned away by the process gate (the dealt node is full for this process, which says nothing about the
     others): the capacity wait reads the fleet once before it gives the subtask the idle seat, so a node with room wins and
     a fleet with none hands it to the seat on the next tick. Every other way into the wait (the deal's overflow, a
     refusal, a lease) has just established that no remote has room, and keeps the seat's place in line at once. With the wait
     switched off (`agent_placement_wait_sec` negative), or for a sheddable run (priority -1), which never waits, there is no tick
     to read the fleet in: the idle seat takes the subtask at once, under the guards below, instead of the capacity defer or the
     shed a remote pin gets (`hintSeatWithoutAWait`).

   The fallback is the seat, never a lease. A seat a text lease reserves or a lease fences, and one another vLLM seat occupies, is
   never a fallback: the subtask waits in line for it or for a node, and a wait that ends with nothing placed defers naming the
   holder. Nor is a seat whose run-cap line is full or that is loading, in the deal and in the gate case. With no remote able to run
   the contract at all, though, the seat's own run-cap line is the only queue there is ([ADR 0076](0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md);
   `replacementNode` and `awaitCapacity` say the same), so a full or loading seat takes the subtask at once, behind its in-flight
   work, exactly as route `auto` does ("queued-local beats ineligible-remote"), and only a lease, a fence or an occupant, the three
   things that loading the seat would break, hold it back. A contract that names a layer this box does not declare is not the
   seat's work either.

6. **The result and the row say what happened.**
   - Every result's placement reason opens with a clause in plain words: `route=local was a hint (no pin_reason), honoured: placed
     on the local seat`, `route=local was a hint (no pin_reason), overridden: placed on <node>`, and for a subtask no node took,
     `...: placed as route=auto, and no node took it`; for `remote`, `...honoured: placed on <node>` and `...overridden: placed on
     the local seat`. The reason placement chose differently (a spent run-cap line, a busy seat, a lease, no eligible remote) is the
     note the clause is prefixed to. The clause opens the note, and does not close it, because the ledger keeps 120 bytes of a placement.
   - A subtask the call's deadline gave up on while a seat or a node was still running it says `...and where it was running is not
     known`, and the tally counts it as a hint and no override; "no node took it" is for a subtask that was never started or was refused.
   - A ledger row of a call that came through a door that offers `pin_reason` with route `local` or `remote` carries `route_asked` (what
     the caller named) and `pin_reason` (empty for a hint). `route` stays the route the engine applied, as it always was, so a hint's
     row says `route=auto`. A reader that wanted the pins finds them by `pin_reason`, and the hints by `route_asked` without one. The dispatch
     marker carries the same two columns. Rows of every other call are byte-identical.
   - `offload_status`'s fleet block has a `pins` object: since this server started (`started_at`, and the label says so), in
     subtasks, `reasoned` by reason (all four keys always present), `hints`, `hints_overridden`, and `unreasoned` (decision 7).
     The accounting is the server's own (`delegate.PinTally`, handed to the engine by each door), counted once per published result,
     so a retried or re-placed subtask is one and a hint is judged by where its published result was placed. Nothing scans the ledger.

7. **Only the doors that offer `pin_reason` opt in** (`RunOptions.PinNeedsReason`): `agent_delegate`, `offload_research`, and the
   `delegate` and `research` verbs. The zero value keeps today's authoritative pin, so the three callers above are untouched:
   `fleet-smoke` still measures the node it names; the review lane's fallthrough still never runs on the fenced seat (a hint would
   park it in a capacity wait, which that path must not do); `offload_ask` and `agent_run` still pass their route through. The ones
   that run inside the MCP server and reach the engine (the review lane's fallthrough, and the remote route of `offload_ask` and
   `agent_run`) hand the server's tally over, so `offload_status` counts their pins as `unreasoned`. An explicit `route:"local"` on
   `offload_ask` or `agent_run` runs on this box's seat without reaching the engine and is counted nowhere, so for that route the gap
   is still a belief.

8. **The tools say it.** The `route` text of both tools no longer calls `local` "force in-process": it says a pin needs a
   `pin_reason`, what a hint is, and that each result says whether it was honoured or overridden. `pin_reason` is an `enum` of the
   closed set, with each reason's meaning and routes. The operator scripts that measure one seat (`parallel-sessions-gate.ps1`,
   `write-door-gate.ps1`) and the `contracts/` examples pass `--pin-reason measurement`.

9. **The lane doors' `auto` route reads the local seat too (amends ADR 0040 decision 6).** The vision lane sent an `auto` call to a
   fleet node only while the machine-wide GPU lease was held. A seat serving other requests, with no lease anywhere, still queued the
   next image behind them while a node with an idle vision seat sat free. `visionremote` now also asks the delegator's own question of
   the seat the task would run on (`delegate.LocalSeatBusy`, through the one `probeSeatBusy` that the delegator reads its agent seat
   with): a request in flight, a load or unload in progress, or a load that would unload another loaded vLLM seat reads busy, and a
   seat that cannot be read reads idle. The seat is the one the pipeline picks for the task (`ocr` has its own binding when the machine
   has one). The lease is read first and keeps its words (`remote: local gpu busy`); a busy seat places `remote: local vision seat
   busy (<what>)`, and with no eligible node `local: vision seat busy (<what>), <why>`. A box with no `delegate_remotes` has no node to
   choose, so it does not read the seat. The threshold is the seat's own, any request in flight, which is the spread deal's reading; it
   is not the agent run cap. `fleet_max_concurrent_jobs` bounds the agent loops this
   harness registered on the agent seat (`localRunCapRoom`, `localSlotAhead`), and a single-shot vision call is not one of them (only
   `agent_run` and the agent contract register runs), so that line cannot see the lane's load and the cap means nothing for a vision
   seat. **Not changed:** STT (ADR 0072 defines its own trigger, `WouldBlockUpstream`); compose (it chooses by capability, not load);
   and the text lane (0.154.0). The text lane has no seat to read from the door: `classify` enters the cascade on the triage rung unless
   the learned router, the kNN pre-filter or the degraded list skips it, which the pipeline decides per request from the input's
   features, `extract` enters on the workhorse, and both climb. The door runs before the pipeline and cannot name the seat whose load
   matters, and the workhorse that its lease reading is scoped to (a question about cards) is the seat `classify` mostly does not use.
   Reading a guessed seat would be an invented signal, so the lane stays as it was. It ships dark besides: no shipped tier declares a
   text task.

## Consequences

- A session that passes `route:"local"` out of habit, as every MCP caller could until now, gets a hint. A call that fits the idle
  seat's run-cap line still runs there; a wider one runs the line there and the rest on the fleet. A call that must run on this box
  says why with a `pin_reason`. A hint is not a privacy boundary: material that must not leave the box is `privacy`, and a call that
  forgets to say so may be placed on a fleet node (the operator's own tailnet; the delegation lane never dials anything else, ADR 0001).
- The ledger's `route` column means "applied" more than it did: a hint's row says `auto`. The incident's rows would have read
  `route_asked=local`, no `pin_reason`, and `placement` opening `route=local was a hint (no pin_reason), overridden: placed on <node>`.
- A local hint reads the local seat's load and the run registry, where a pin read nothing (`readAutoLocalSlot`: milliseconds, up to 4 s
  when llama-swap hangs, failing open). A call that fits the line still reads no node.
- With the capacity wait switched off (`agent_placement_wait_sec` negative), the overflow of a local hint that no remote has room for
  is a capacity defer, as it is for `auto`; it used to queue behind the seat as a pin. A remote hint's overflow goes to the idle seat in
  the deal and does not depend on the wait.
- A pin and a hint place the verification retry differently: a pinned `local` call has no other node to retry on, a pinned `remote`
  call retries on another fleet node or not at all, and a hint may retry on the seat or on a node.
- The default `offload_status` answer grows by the `pins` block, 856 bytes on the status fixture (the golden moved, and only by that
  block); the brief carries the fleet block, so it grows by the same. The block is absent when delegation is off, since no door can
  pin then.
- `unreasoned` is a measure of a gap, not a fix for it: `offload_ask`, `agent_run` and the review lane can still narrow placement with no
  reason, and the explicit local route of the first two is not even counted. The decision is to name them rather than to widen the change.
- Known gaps, none a regression against a pin or `auto`: a local hint on a box with no agent seat ends as the config defer `no agent seat
  resolvable`, as `auto` and a local pin do; a reasoned remote pin whose retry is refused by its node with the capacity wait on ends as a
  capacity defer whose `retried_on` names this box's node id, although nothing ran on the seat (the capacity defer's existing
  convention); and the opencode plugin's delegate digest still reads a placement with no hint clause by substring, so a `route=auto`
  overflow placed on a node, whose placement text mentions the local seat's run-cap line, is counted as local. A typed where-it-ran
  field on the result is the fix for the last one.

## Alternatives considered

- **Treat every bare route as a hint in the engine.** Rejected: `fleet-smoke` would stop measuring the node it names, the review lane
  would run on the fenced seat it uses `route=remote` to avoid, and `offload_ask` / `agent_run` have no field to give a reason with.
  The opt-in puts the rule where a session decides.
- **Refuse a reasonless `local` or `remote`.** Rejected: it breaks every existing caller, and the operator's wording is that such a
  route "becomes a hint placement may override".
- **Free-text reasons.** Rejected: nothing to group, count or audit, and a model writes anything.
- **A process-wide tally.** Rejected: the window is "since this server started", which is the server's, a test server would inherit
  the previous test's counts, and the status golden could not pin the block.
- **Count overrides where an attempt finishes.** Rejected: `finish` runs once per attempt, so a retried subtask counted twice. The
  count is taken over the published results.
- **The hint clause at the end of the placement note.** Rejected: the ledger cuts a placement to 120 bytes, and the clause is the
  part that must survive.
- **A fourth route, `remote-first`.** Rejected: it is `auto` with the seat dealt as unavailable, which the engine already does for
  `route=remote`; a new route name would be a new thing to keep consistent at every site.
- **Key the retry guard on the route.** Rejected: the bare remote route (`fleet-smoke`, the review lane, `offload_ask`, `agent_run`)
  keeps `remote -> local` by design, and a guard on `route == "remote"` turned ten existing tests red. The reason is what makes a
  pin one this box obeys to the end, so the guard asks for it.
- **A reason for "the local seat is the better model".** Not added; the closed set stays four. Placement chooses among seats that are
  already adequate for the contract (the capability gate, `remoteEligible`, and the contract's own acceptance checks) and ranks them by
  speed; it does not rank them by a judgment of model quality, which would be a signal nothing measures. A caller's belief that one
  adequate seat answers better than another is not a reason that can be checked, recorded or counted, and a reason that cannot is how a
  pin became a way to leave the fleet idle. The cases where the seat is the point already have a reason: `measurement` for a seat under
  comparison, `operator` for an operator's decision, `locality` for something only this box has. A caller who wants the strongest seat
  for judgment-heavy work names no reason, gets a hint, and the acceptance checks decide whether the answer stands (a failed one is
  retried on another node).
- **Hint counters in the call summary** (`hints`, `hints_overridden` beside the placement counts of an `agent_delegate` or research call)
  and an operator-guide line saying that a caller who wants the strongest seat gives no reason and gets a hint. Deferred, not rejected:
  each result's placement clause and `offload_status.fleet.pins` carry the same facts today.
- **Count the explicit local route of `offload_ask` and `agent_run` as unreasoned.** Deferred, not rejected: it is a tally increment in two
  handlers for a gap this change names, and the texts say what is counted until it is done.
- **Leave a remote hint's overflow to the capacity wait.** Rejected, for the reasons in decision 5.
- **Read the cascade's workhorse as the text lane's seat.** Rejected in decision 9.

## Related code

- `internal/delegate/pin.go`: the closed set, `CheckPinReason`, `resolvePin`, `remoteHint`, the hint clause (`stampPin`,
  `hintClause`, `placedWhere`), `PinTally`
- `internal/delegate/run.go`: `runWith` (intake, the remote hint's forcing, the stamp and the tally), `dealAutoRemoteRoom` (the seat's
  fallback in the deal), `awaitCapacity`, `attempt`, `record`, `probeSeatBusy`
- `internal/delegate/seatbusy.go`, `internal/visionremote/visionremote.go`: the lane doors' seat reading
- `internal/delegate/wire.go`, `internal/delegate/telemetry.go`, `internal/ledger/ledger.go`: `pin_reason`, `route_asked`
- `internal/mcpserver/mcpserver.go`: both tools' schemas and text, `handleAgentDelegate`, `handleResearch`, `agentDelegateOptions`,
  `pinsView`; `main.go`: `--pin-reason` and `cliPinOptions`
- `internal/delegate/pin_test.go`, `pin_retry_test.go`, `pin_gate_test.go`, `pin_fallback_test.go`, `internal/delegate/seatbusy_test.go`,
  `internal/mcpserver/pin_reason_test.go`, `internal/visionremote/seat_busy_test.go`, `delegate_cli_test.go`,
  `integrations/opencode/test/plugin.test.ts`

## Related docs

- [../../systems/fleet-node.md](../../systems/fleet-node.md), "Placement routes and the retry" and "A placement pin needs a reason"
- [../../OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md), "Delegate subtasks across fleet nodes"
- [0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md](0076-a-calls-width-is-sized-from-its-deal-a-batch-of-up-to-16-is-one-deal-and-auto-counts-the-local-run-cap-line.md),
  the placement a local hint gets
- [0063-placement-holds-instead-of-sleeping-or-refusing.md](0063-placement-holds-instead-of-sleeping-or-refusing.md), the headroom
  count, the process gate and the capacity wait the hints use
- [0040-vision-work-travels-to-a-node-with-an-idle-card.md](0040-vision-work-travels-to-a-node-with-an-idle-card.md) and
  [0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md](0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md),
  the lane doors
