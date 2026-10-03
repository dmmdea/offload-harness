---
status: Accepted
date: "2026-09-09"
---

# 0039 — A held card is a place in line, and a cleared seat stays cleared

Release: 0.115.2

## Context

ADR 0018 made the GPU lease machine-wide and fenced so a text measurement and a media
render stop destroying each other; ADR 0026 made text-load admissions wait out a *media*
holder. Both are about the harness's own call sites. The one ingress an operator's session
uses to put its own work on the card — `local-offload gpu reserve` — was left calling
`TryAcquire`: a held card came back as an **error**.

The consequence was behavioural, not mechanical. On 2026-09-09, for the tenth time, a
session declined GPU work outright: *"the harness is live — it pins models to these cards
by UUID and can spawn on any of them mid-segment, which is exactly what voided two 5070 Ti
runs last time."* Each clause was true, and each had a fix that already existed: the fleet
routes delegations around a held card (0.113.14), `--drain --unload-seat` empties the seat
(0.113.16), and the pipeline has queued renders behind each other since ADR 0018. Two gaps
made the refusal look reasonable:

1. **The reservation verb did not queue.** A session that ran `gpu reserve` into a holder
   got `GPU held by text (pid …)` and stopped. Nothing in the message said the machine had a
   queue.
2. **A text lease did not stop loads.** ADR 0026 gated media only, reasoning that a text
   holder "unloads nothing, so a switch underneath it costs a measurement, not the machine".
   That stopped being true the day `--unload-seat` shipped: the holder *did* clear the cards,
   and the next interactive text call made llama-swap pull a multi-GB model straight back
   onto them, mid-run. That is the "spawn on any of them mid-segment" the session feared,
   and it voided the two runs it cited.

And no instruction surface — `gpu status`, `offload_status`, the operator's global rules —
told a session what to do with a held card other than not use it.

## Decision

1. **`gpu reserve` queues.** `--wait` (default **8 h**; `0` restores fail-fast) is how long
   the wrapper and the detached holder stand in line behind a current holder. The detached
   form's hidden child (`gpu hold`) is the process that queues, so the pid `reserved:` names
   is the one that took the card; the parent reports success only once that is true, or the
   child's exit when the line did not move in time. The wait prints exactly two stderr lines
   (`queued behind …`, `acquired after …`) — never one per poll, because the session wrapping
   the command turns each printed line into a notification. `gpulease.Acquire`'s
   declared-window short-circuit is **disabled** for a reservation (`Options.WaitOut`): it is
   right for a tool call with a 90 s budget and wrong for a caller that would otherwise give
   the job up, because holders release before their declared window as a rule (the wrapper
   form releases when its command ends). The first live proof of this change found exactly
   that: a `--wait 2m` waiter behind a `--for 3m` holder was refused at once, and the holder
   released six seconds later. Every refusal names the flag that would have kept queueing.

2. **`--unload-seat` implies `--exclusive`, and an exclusive text lease gates loads.** The
   lease record gains `exclusive`; `blocksLoad` in the admission gate treats
   `text && exclusive` exactly as media. A load under it rides its `cascade_remote_lanes`
   lane when one serves the model, otherwise waits the caller's own budget and returns a
   `LeaseError` naming the holder. Plain text leases keep ADR 0026's behaviour. `Inspect`
   and `infoFrom` become one builder so the stamp cannot reach `ErrHeld` and miss the gate.

3. **Every "held" surface ends with the queue command.** `gpulease.QueueHint` is one string
   shared by `gpu status`, the CLI refusals and `offload_status`, which now publishes the
   LOCAL lease under `gpu_lease` (held/class/reason/expiry/exclusive + `queue_with`).

4. **The operating rule, stated where sessions read it:** never refuse or defer GPU work
   because a card looks busy — reserve it, and the machine queues it. Delegations already
   route to the other nodes meanwhile.

## Consequences

- A session with a bench, a render or a training run to place has one command and one
  outcome: it runs, after whoever is ahead of it. "Busy" stops being a reason to write
  work off, which is what "the harness must carry over half of our total work" requires.
- Under an exclusive text hold, an interactive text call on the same box without a remote
  lane waits its own budget and fails naming the holder. That is the honest outcome — the
  cards are genuinely spoken for — and it is loud, so an operator configures a lane rather
  than discovering a voided measurement hours later. Boxes that never take exclusive holds
  see no change.
- An 8-hour default wait is a long time for a process to sit; it is also exactly how long a
  session would otherwise have spent not doing the work. `--wait 0` is there for the caller
  that genuinely wants an answer now.

## Extended 2026-10-02: a card is a place in line, so the agent lane takes a free one (register C-86, plan P4)

A held card is a place in line, and with card-scoped leases a lease holds some cards, not the box. The delegator therefore
asks whether the contract has a free local card before it asks whether the box is busy: `delegate.LeaseForContract` narrows the
local lease to the seats the contract could run on (`placement.AgentChain`, the list the placement table walks), and the local
box is busy for a contract only when **every** local agent seat it could use sits on a held card. The placement table gains
row 5c: when the home layer's agent seat fits the contract and its cards are held, the next declared layer that is not opt-in
and not dormant, whose agent seat fits the window and whose cards are free, takes it (on the three-card tier a card-2 render
moves the flagship lane to the single layer's card-0 seat). With none free the contract keeps the home seat and **queues at its
gate**, never a refusal, and the reason says the delegator may route it to another node. A contract that names its layer runs
there or waits there. See [GPU lease](../../systems/gpu-lease.md), "Consumers read a seat's cards, not the node". Pinned by
`TestAgentHomeFallsBackToNonIntersectingLayer`, `TestNoFallbackQueuesAndRoutesRemote`, `TestPlacementKeepsLocalSeatOnFreeCard`
and `TestLocalBusyFalseWhenAFreeCardServesTheSeat`.

## Alternatives considered

- **Keep fail-fast, document the queue.** Rejected: the same documentation has existed
  since 0.113.14 in `docs/systems/gpu-lease.md` and did not reach the session at the moment
  it read `GPU held by`. The behaviour has to be the default, and the hint has to be in the
  error.
- **Gate loads under every text lease.** Rejected for ADR 0026's reason — a plain text
  holder that left the tier resident loses nothing to a switch, while every interactive call
  on the box would lose the length of an eval. The stamp targets the case that actually
  voided runs.
- **Have the parent (not the hidden child) queue in `--detach`.** Rejected: the parent
  exits; a lease taken by a pid that is gone reads as reclaimable, and the whole point of
  `--detach` is a holder pid that is observable for the window's length.

## Related code

- `gpu_cmd.go` — `--wait`, `--exclusive`, `acquireQueued`, `heldHint`, the detach parent's
  queue loop, `gpu hold --wait --exclusive`
- `internal/gpulease/gpulease.go` — `Meta.Exclusive`, `Info.Exclusive`, `Options.Exclusive`,
  `QueueHint`, the single `infoFrom` builder, `ErrHeld` with the declared window
- `internal/modelaffinity/gpuwait.go` — `blocksLoad`
- `internal/mcpserver/mcpserver.go` — `localLeaseView`

## Related docs

- [docs/systems/gpu-lease.md](../../systems/gpu-lease.md) — "A held card is a place in line",
  "Exclusive text holds"
- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md), [ADR 0026](0026-text-load-admissions-wait-for-the-media-lease.md)
- `docs/OPERATOR-GUIDE.md` — the delegate section's lease paragraphs

## Amendment 2026-10-03 (GPU routing P13b, the place in line reaches the media tools)

The Decision made the *reservation verb* queue. It left the other ingress, a media tool call, with a
bounded wait and then a refusal: `gpu_wait_ms` (90 s), then `gpu_busy`, which sent the caller back
to the end of a line it could not see, or away. That is the same defect one door over, and it sat on
top of a second one: a media call took the whole-node lease even when it used one card.

On a host that leases cards a media call now (1) asks the allocator for **one card**, or for the card
an explicit `comfy_cuda_device` names (a hard constraint, never re-picked), and holds a lease on it,
running in the ComfyUI instance bound to that card, so two calls run on two cards at once; and (2) when it
has waited its window with no card it answers with a **place-keeping token** (`gpu_queued`, a capacity
defer carrying `waiter_token`, `queue_position` and `eta_s`) instead of `gpu_busy`; the caller re-sends the
request with the token and resumes the place it left. A token has no process behind it, so its life is its
last poll: it holds its place for 30 s, is then skipped by every waiter (a whole-node barrier included) so
an absent client never blocks the line, and can be resumed for 10 minutes. A host that does not lease cards
keeps `gpu_busy` exactly as before. A call that holds the whole node leaves a token too on a host that
leases cards. Only a call that came through a door able to send the token back (the MCP server) leaves
one: the CLI verbs, the fleet dispatch and the image batch keep `gpu_busy`, because a token nobody can
claim would only hold a card back from the next caller for the grace.

What is deliberately not scoped: the **default** ComfyUI instance (port 8188) is one process for the box,
so a job that uses it holds the whole node, or (a pooled route) cards that every other such job on a box
of at most three cards must also hold, so two of them cannot run at once; on a larger box they hold the whole
node. A `run-graph` with several declared devices holds the whole node too: its graph is the caller's and the
default instance sees every card, so a lease on some of them would not confine it. A pin that cannot be turned into a card (the box declares
no `gpu_comfy_order`) keeps the whole node and today's `--cuda-device`; nothing is guessed from an index.
The tokens live in their own directory because an older binary prunes any waiter whose process has stopped
polling; on a host that mixes versions an older binary can take a card ahead of a token holder, which costs
the holder its place and never exclusivity.

Related code: `internal/pipeline/mediaadmit.go`, `mediaslots.go`, `internal/gpulease/tokens.go`,
`internal/gpualloc`, `internal/comfyinst`. Related docs: [media-generation.md](../../systems/media-generation.md)
("Per-card media admission"), [gpu-lease.md](../../systems/gpu-lease.md) ("A place in line for a caller that
cannot stay").
