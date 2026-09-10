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
   the command turns each printed line into a notification. `gpulease.Acquire`'s existing
   short-circuit stands: a text holder whose declared window outlasts `--wait` is answered at
   once, with the window, and every refusal names the flag that would have queued.

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
