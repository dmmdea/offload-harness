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
