---
status: Accepted
date: "2026-08-27"
---

# 0028 — Delegation durability is a push-side intent ledger, not a pull queue

## Context

The consolidated-pull-queue decision document (2026-08-26) laid out three ways
to finish the "send several jobs and lose nothing" requirement after builds
0.100.0–0.102.0 made push placement correct: **A** — keep push, add
delegator-death durability; **B** — invert to a pull queue hosted in a fleet
node (claim/ack endpoints, durable store, lease TTLs); **C** — a cloud-hosted
queue. The one gap A leaves is "nodes take jobs dynamically from one queue";
the one gap it closes is the only one observed at current volume: a delegator
that dies mid-poll orphans work a node may still finish.

## Decision

Operator-approved 2026-08-27: **do A now; judge B against the delegation
scoreboard, not against the design document.** Contention arguments for B
cannot be evaluated at near-zero delegation volume.

Mechanics (`internal/delegate/intent.go`): every ACKED remote dispatch appends
an intent line to `<state-root>/delegate-intent.jsonl` before polling begins;
terminal answers observed by the dispatching process close the entry; the
orphanable exits (cancellation, owned-job poll deadline, queued give-up) leave
it OPEN. A once-per-process background pass re-polls open entries and files
finished results under `<state-root>/delegate-recovered/<job>.json`, closing
entries a node positively denies (restarted stores) or that age past 48 h.
A nil ledger is inert — durability is an addition, never a new dispatch
failure mode.

## Consequences

- Delegator death no longer loses remote work a node completes; recovery is a
  poll, never a re-run (the node's duplicate-job path re-acks idempotently).
- The queue remains N per-delegator queues that shed well; genuinely dynamic
  cross-node take stays unbuilt until the scoreboard shows contention.
- Re-open trigger for B: sustained multi-session delegation volume with
  measured node imbalance, or a real incident A's recovery cannot cover.

## Amendment (2026-09-30, see ADR 0064)

The decision stands: running work stays recoverable. Three corrections follow from the first live
measurement of the recovery pass. It closed 61 of 61 intents on 2026-09-29 (36 on an HTTP 401, 25 on a 404)
and recovered none: a 401 is a fact about the recovering process's credentials, so an intent now stays open
on it (one log line per pass); every intent event carries the unix second and the pid of the process that
wrote it; and a delegator that gives up on a job the node has NOT started now asks the node to take it back
(`DELETE /fleet/jobs/{id}`), closing the intent as `withdrawn` only when the node confirms. A job that has
started is still never touched, so the orphanable exits for running work leave their intents open exactly as
before. [ADR 0064](0064-a-delegator-takes-back-what-it-has-not-started.md) records the change.
