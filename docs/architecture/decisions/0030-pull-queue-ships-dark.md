---
status: Accepted
date: "2026-08-28"
---

# 0030 — The consolidated pull queue (Option B) ships complete and DARK

## Context

ADR 0028 chose Option A (push-side intent ledger) and parked Option B — the
genuine "one queue, nodes take work dynamically" inversion — against the
delegation scoreboard. The operator then directed Option B built (2026-08-28).
Both directions hold at once by shipping B **dark**: the code is complete and
tested, and an unconfigured fleet runs byte-identically to 0.108.x.

## Decision

`internal/fleetqueue` + the fleet-node claim loop + delegate `route:"queue"`,
inert until three config keys are bound:

- `fleet_queue_host` (holder only) — the always-on node's fleet server also
  hosts the durable queue (bbolt at `<state-root>/fleet-queue.db`) and the
  `/fleet/queue/{submit,claim,ack,nack,jobs/{id}}` routes, bearer-gated by the
  fleet's existing agent-lane rule.
- `fleet_queue_holder` (every participant) — the holder's base URL,
  tailnet-vetted at load.
- `fleet_queue_claim` (worker nodes) — starts the claim loop: pull an eligible
  job, run it through the SAME `BuildRequest` + jobs surface a pushed dispatch
  uses, ack the result; back-pressure by the node's own queue-depth cap.

Semantics: at-least-once. Leases are the contract timeout + 5 min slack;
expiry requeues with history, bounded at 2 requeues then a loud failure; the
first ack wins and late duplicates are ignored. The results route mirrors the
push path's `{state,data,error}` wire so one poller reads both. The delegator
queue route deliberately skips ADR 0028's intent ledger: the holder itself is
the durable record — a dead delegator re-polls the holder and loses nothing.

## Consequences and the enabling bar

- The holder is a SPOF for queue-routed work only; push (`auto`/`spread`)
  remains the default and untouched — the mitigation the decision doc asked
  for is structural, not a fallback mode.
- ENABLING (binding the keys) is a separate operator decision, judged against
  the delegation scoreboard: sustained multi-session volume with measured node
  imbalance. Turning the keys on without that evidence buys queue latency for
  nothing — the same reasoning that parked B in ADR 0028.
- First-enable checklist: holder = the always-on box; token REQUIRED once any
  listener leaves loopback; watch `fleet-queue.db` size and the abandoned-job
  rate before widening use.
