---
status: Accepted
date: "2026-11-25"
---

# 0105 — A refused single-shot call holds its place in both lines

## Context

On 2026-11-25 nine `transcribe` calls on route `auto` ran while a GPU lease held this box's cards. The one fleet node
took four of them and refused four with `503 queue full (4 jobs accepted+running, limit 4): retry later ... (~183 s until
a worker frees, from recent completions)`. The four left the fleet's line at once: `sttremote.Run` on route auto called the
node once, discarded the attempt on the first error and ran the call on this box, where the lease was. The ninth ended in
`gpu busy: gpu-lease timeout after 1m29.82s`. Nothing asked the fleet again, although the node had said, in words, when a
worker would free.

The four single-shot lanes (`sttremote`, `visionremote`, `textremote`, `forecastremote`) all had this shape: pick a node,
dispatch once, and on any error either defer (route `remote`) or fall to the local run (route `auto`). The agent contracts
and the media lanes already wait ([ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md),
[ADR 0073](0073-the-capacity-wait-runs-to-the-calls-deadline.md)); these four did not. The standing rule is that a busy card
is a place in line, never a reason to refuse.

## Decision

1. **One loop, `internal/lanehold`, owns the placement half of the four lanes.** Pick a node and dispatch until a node
   accepts, this box's own side frees, or the bound ends, whichever comes first. The poll-and-run half of a call stays in each
   lane. The package knows no lane types: a lane hands it one `Try` (a pick plus a dispatch), and the error `Try` returns says,
   through a small interface, whether a place may free and which node just refused.
2. **What waits.** A dispatch refused with 503 or 429, and "no node is eligible" when at least one miss was temporary (card
   leased, queue full, cooling after a refusal). Everything else is returned at once, as before: any other status, a dial error,
   no remotes configured, a node without the door, a file over every node's cap. Every message and defer class is kept; a
   refused dispatch still ends in `dispatch <url>: status 503:` and so still files as `queue_full`. A call that reached the
   bound reads `...; waited 41s in line` after the last error, typed so the lane still reads its class.
3. **Between tries.** The roster is read again every 3 s (the delegator's own capacity-wait interval), or at the earliest
   cooldown end, or at the bound. A node that refused is left alone for 10 s whatever its health says (an upload is megabytes and
   is not re-sent into a node that just refused), stretched once by up to 30 % so nine callers refused on one tick do not return
   on one tick. After that a node that publishes its room (a queue ceiling, or a saturation verdict) is tried as soon as its
   fresh health is not saturated, and skipped while it is; a node that publishes neither waits out its `Retry-After`, read from
   the refusal's header. A node whose health reads full is a temporary miss and is not dispatched to at all (it used to be
   uploaded to only to be refused).
4. **Where it applies.** Route `remote`: the loop with nowhere else to go, and at the bound the same capacity defer. Route
   `auto`: only when this box's own side is fenced by a lease (stt `localBusy`, vision and text `localBusy`, forecast
   `localBlocked`). The loop then also asks that trigger before each next try: when it reads free, the fleet attempt is
   discarded and the call runs here, placed `local: card freed after 41s in line, <the fleet's last answer>`. A call a node takes
   after waiting is placed `remote: local gpu busy, waited 41s in line`. The vision lane's other trigger, a seat that is serving
   someone, is not a fence: the engine's own queue is the place in line there, so after one failed try the call runs here at
   once, as before. A forecast on a box with no forecast model has no local side and holds for the fleet alone. Route `local` is
   untouched. While the call holds on route auto it registers a seat waiter for the local model
   (`gpulease.RegisterSeatWaiter`, the place a blocked admission takes), refreshed each tick and dropped when the loop ends, so a
   chained media acquire does not jump it.
5. **The bound, with no new key.** stt: `stt_request_timeout_sec` (1800 s when unset) less the call's run budget
   (`budgetFor`, a base plus half the audio's length), never below 0. Vision, text and forecast: the lane's own `Budget`. A
   caller deadline cuts it to at most half of what is left. `agent_placement_wait_sec < 0`, the operator's off switch for waiting
   in line, makes it 0: one try, and a node that reads full is still tried, byte for byte as before. The run half gets its own
   full budget from the moment a node accepts (it used to share one deadline with the placement).
6. **The node says when an stt job is queued.** While an stt job waits at the node's stt gate it publishes
   `progress.phase = "queued"` on the job, and `"running"` once it holds a slot; a job that finds a free slot publishes nothing. The
   asker's poll does not charge the time a job spent `queued` to its run budget, bounded overall by `stt_request_timeout_sec`.
   An older node publishes no phase and is charged for every second, as before. No other wire change; the node's `Retry-After`
   formula is untouched.
7. **A log line only when the reason changes**, on stderr, and one at the end: `transcribe: waiting in line: node-c refused it;
   this box's card is leased`, then `transcribe: placed on node-c after 41 s in line` or `transcribe: this box's card freed
   after 41 s in line`. No per-tick output.
8. **The ledger keeps the mapping.** A call that ended on a probe that found the nodes full or cooling reads
   `no fleet node is eligible for the ... probed node-c (node-c): queue full (4 of 4 jobs)`. That is the queue-full a 503 would
   have filed, so `internal/ledger` files it as `queue_full` ahead of the `no fleet node is eligible` prefix.

## Consequences

- The nine calls of the incident hold their places: the four refused return to the node, one at a time as workers free, or
  run here the moment the lease ends, and none is lost to a node's first 503.
- A call that waits and is then accepted has its full run budget. A long queue at the node no longer eats the stt run budget.
- A refused attempt's PAIR card closes quiet (`RemoteAttribution.Bounce`) and the next dispatch opens the next card, so a
  call that waited shows one card per node it was sent to.
- A single-shot call can now take up to the bound longer than before to give up: up to 5 minutes for vision, text and forecast,
  and up to the unused part of `stt_request_timeout_sec` for transcription. `agent_placement_wait_sec: -1` restores one try.
- A recording long enough that its run budget reaches `stt_request_timeout_sec` (about 54 minutes of audio at the defaults) has no
  wait in line: the bound is 0. Raising `stt_request_timeout_sec` gives it one.

## Alternatives considered

- **A retry inside each lane.** Four copies of one loop is how the lanes drifted before (the shared roster read, [ADR 0074](0074-every-fleet-client-admits-the-same-roster-under-a-configured-list-of-tailnet-zones.md)).
- **Reusing the delegator's capacity wait.** It is built around subtasks, contracts and a run ledger; a single-shot call has
  none of them, and the lanes must not import the delegator's runner.
- **Waiting on this box's card only.** That is the old `gpu_wait_ms` wait, which is exactly what ended at 1m29s while the fleet's
  line moved.

## Related code

- [`internal/lanehold`](../../../internal/lanehold/lanehold.go), the loop, the cooldowns, the bound and the seat waiter
- [`internal/sttremote`](../../../internal/sttremote/sttremote.go), [`internal/visionremote`](../../../internal/visionremote/visionremote.go),
  [`internal/textremote`](../../../internal/textremote/textremote.go), [`internal/forecastremote`](../../../internal/forecastremote/forecastremote.go)
- [`internal/fleetnode/stt_upload.go`](../../../internal/fleetnode/stt_upload.go) (`enterSTT`), the node's queued and running phases
- [`internal/delegate`](../../../internal/delegate/gate.go) (`Saturated`, `ParseRetryAfterSeconds`), the readings the loop shares with the delegator

## Related docs

- [0072](0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md), the upload door
- [0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) and [0073](0073-the-capacity-wait-runs-to-the-calls-deadline.md), the delegator's wait this one follows
- [../../systems/fleet-node.md](../../systems/fleet-node.md), "The single-shot lanes read the same roster", and [../../systems/delegation-client.md](../../systems/delegation-client.md)
