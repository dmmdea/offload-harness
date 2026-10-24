---
status: Accepted
date: "2026-10-24"
---

# A live driver-fault repeat demotes a card's placement

## Context

Since 0.184.0 the harness reads the NVIDIA driver's own fault records (Windows `nvlddmkm`, Linux `Xid`) as facts. Register
C-83-B kept them out of every decision: a driver fault and a kill look alike in the log, so an event never moves a lease
verdict, a gate or a placement. 0.199.0 added `gpu faults`, which groups the records into incidents and applies NVIDIA
NVSentinel's RepeatedXIDErrorOnSameGPU rule: one fault class in 5 or more bursts on one card within 24 hours. NVSentinel
answers that rule with `CONTACT_SUPPORT`. 0.200.0 publishes the reading per card as `gpu_faults` on every fleet node's
`/fleet/health`, leaves bursts at a restart or sleep transition out of the rule, and retains incidents beyond the OS log.

A repeat is a card where something keeps resetting the engines, and a reset ends the contexts on that card. On Windows a
TDR resets the adapter. Work placed there can be cut off part way, then deferred and placed again, and the time is lost.
The reference box's repeat (2026-10-11, 5 bursts of `nvlddmkm 153` on one card in 82 minutes) ran while no harness work
was on that card. The cause was outside the harness, and a job placed there would still have been reset.

The operator accepted the routing change on 2026-10-24, answering the proposal "publish each card's fault summary on the
fleet wire so placement avoids a card with a live repeat flag" with "Next routing work: ok".

## Decision

**1. A repeat is live while its last burst is within 24 hours** of the delegator's clock (`faultRepeatLive`). This is the
rule's own window. A card that has been quiet for a day stops being demoted without anyone clearing it.

**2. Remote nodes are demoted, never excluded.** The new ranking key, `faultDemoted`, sits right after saturation in
`betterRemote` and between saturation and the free-card tier in `fitPickWith`. A node loses on this key when every agent
seat the contract could run on there sits on a card with a live repeat:
- The seats come from the node's published layer rows (`placement.RemoteSeatCards`), as for a lease that holds a card
  (ADR 0094).
- One seat off such cards keeps the node clean, because the placement table falls back to that seat.
- Where nothing narrows the seats, any live repeat demotes. That covers a vision or text lane with no contract, a node with
  no rows, a long-context contract, and a layer the node does not declare.
- A demoted node still takes work when nothing better is left. A node that publishes no `gpu_faults` (one release behind,
  no reading yet, no NVIDIA card) is never demoted.
- A demoted node that lost is narrated as `faulted (<class> x<bursts> on <bus>, last burst <time>)` in the placement
  reason.

**3. The local seat reads busy for the deal** when a live repeat sits under every local seat of some subtask, read from the
summary fleet-serve writes (`<state root>/gpu/faults-summary.json`). The seats are the box's own agent chain, resolved
against the summary's card table (`placement.LocalSeatCards`). The rule applies to route=auto's joint reading and to
route=spread's local slot, and only when a remote could take the work. It never applies to route=local and never on a box
with no remotes. It is not part of the seat's load reading, so the capacity wait reads the seat afresh and an overflow
subtask may still come back to it: demoted, not excluded. A missing or unreadable summary reads not faulted.

**4. It is cause-agnostic.** The rule does not ask whether harness work caused the faults. A reset ends every context on
the card whoever caused it, and the incident that motivated this rule had no harness work on the card.

**5. Facts stay facts everywhere else.** The amendment to C-83-B is this one: a live repeat may DEMOTE PLACEMENT. Driver
events still never move a lease verdict, a gate, an exclusion, a drain or a release. `gpu faults` and `gpu_faults` remain
readings, and the per-event views (the lease's `driver_events`) are unchanged.

## Consequences

- A node with a card that keeps resetting stops receiving delegated work while a clean node can take it. When the card is
  quiet for 24 hours, it competes again on the usual keys.
- The key is discrete and strong. A demoted node loses to any clean node that is not saturated, whatever its expected
  completion time. That is the intent: a probable reset outweighs a faster seat.
- The delegator's view is up to 40 minutes old (the node refreshes every 10 minutes and publishes a reading for up to 40).
  A repeat that just formed can miss one placement, and one that just went stale can demote for that long.
- The local reading follows fleet-serve's summary file. On a box with no fleet-serve there is no file and no local
  demotion.
- No setting turns this off. The rule only reorders candidates, never refuses work, and a card that keeps resetting is
  not a place anyone wants work sent. If that changes, a setting can be added without changing the rule.

## Alternatives considered

- **Exclude the card or node.** Rejected. The repeat is a reading of a log, not a verdict on the card, and an exclusion
  would turn a one-card fleet away from its own work. Demotion keeps the work flowing.
- **A seconds penalty in the expected-completion arithmetic.** Rejected. A reset costs the whole job and its retry, not a
  number of seconds, and a soft term would let a fast faulting seat outrank a slower clean one.
- **Count only incidents that harness jobs ran into** (`gpu faults --jobs`). Rejected. The ledger does not see callers that
  reach the seats directly, and the motivating repeat had no harness job on the card.
- **Act on every incident, not only on the repeat rule.** Rejected. A single burst is routine (two of the reference box's
  19 incidents in 30 days were restart teardown), and NVSentinel's rule is the established threshold.

## Related code

- `internal/delegate/faultdemotion.go`: `faultDemoted`, `liveRepeats`, `seatsOnFaultedCards`, `localFaulted`.
- `internal/delegate/gate.go` (`betterRemote`) and `internal/delegate/run.go` (`fitPickWith`, `readAutoLocalSlot`, the
  spread local slot): where the key and the local reading sit.
- `internal/delegate/placement_reason.go` (`oneWordVerdictWith`): the `faulted` reason.
- `internal/placement/leasecontract.go` (`LocalSeatCards`, `RemoteSeatCards`).
- `internal/gpuactivity/faultstore.go` (`FaultSummary`) and `gpu_faults_health.go`: the reading this rule consumes.

## Related docs

- [gpu-lease.md: `gpu faults`](../../systems/gpu-lease.md#gpu-faults-the-fault-history-as-incidents-with-known-errors) and
  [its retention and summary](../../systems/gpu-lease.md#retention-and-the-fleet-nodes-summary).
- [FLEET-NODE.md: `gpu_faults`](../../FLEET-NODE.md#driver-fault-summary-gpu_faults).
- [ADR 0094](0094-a-lease-names-its-cards-a-card-has-one-holder.md): the seat-to-card rule this reuses.
- NVIDIA NVSentinel's RepeatedXIDErrorOnSameGPU rule (github.com/NVIDIA/NVSentinel).
