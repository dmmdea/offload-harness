---
status: Accepted
date: "2026-09-03"
---

# 0034 — Fleet overview is a read-only page served by the delegator, from data nodes already publish

- Deciders: operator, harness session (2026-09-03 PAIR gap analysis)
- Related: [0023](0023-agent-lane-tailnet-auth-and-locality.md) (tailnet-only, bearer-gated agent
  lane), [0013](0013-nodes-advertise-raw-footprint.md) (nodes advertise raw facts; the dispatcher
  owns interpretation)

## Context

A 2026-09-03 gap analysis compared this harness's fleet to the PAIR project's operator surface —
PAIR ships live per-node cards (GPU/CPU/RAM sparklines, seat residency, served models, a jobs feed)
that this harness's operator had no equivalent of; the only way to see fleet state was `curl
/fleet/health` node by node or read the delegation log by hand.

PAIR's operator page was the part worth porting. Its other mechanisms were not, because this
harness's existing fleet design already covers the same ground more strongly:

- PAIR's mDNS-style **discovery/pairing** exists to let nodes join an untrusted network dynamically.
  This harness's roster is a static, operator-declared `delegate_remotes` list on a Tailscale-only
  network — a stronger trust boundary than runtime discovery, not a weaker one, and adopting
  discovery would be a regression.
- PAIR's **mTLS and port takeover** solve a problem this harness does not have: nodes already sit
  behind the tailnet's own encryption and a bearer-token gate on the one route that carries a
  payload (the agent job dispatch, per ADR 0023). Layering a second certificate scheme on top adds
  operational surface without closing a gap.
- PAIR's engine binding is coarse: it treats GPU **utilization as a primary scheduling signal**,
  which PAIR's own README documents as suiting only a fleet of near-identical machines. This
  harness's fleet is deliberately heterogeneous (workstation, laptop, mini-PC, differing VRAM and
  context ceilings), and utilization alone says nothing about whether a contract fits a seat.

What remained genuinely missing was the **view**: one place to see every node's health, jobs, and
recent errors at a glance, without hand-curling each one.

## Decision

1. Ship the view as a **read-only overview page served by the delegator**, not by any node. The
   delegator already holds (or can cheaply poll) every node's health and jobs, and already owns the
   delegation-log corpus that PAIR has no equivalent of — one page here covers the whole fleet;
   a node-side page would need one per box and would need to learn about siblings it currently
   knows nothing about.
2. The page (`fleet-ui`) and its terminal client (`top`) call **only existing or newly-additive
   `GET` routes** — `/fleet/health`, the new `/fleet/jobs`, and the delegator's own on-disk
   delegation-log. It never dispatches, drains, or otherwise mutates anything. `/fleet/jobs` is
   deliberately payload-free and unauthenticated: it is metadata only, so there is nothing on that
   route the agent lane's bearer gate exists to protect.
3. The only new health fields are **additive**: `gpu_util_pct` + `gpu_util_known` (always present;
   the busiest device, PAIR's multi-GPU rule adopted deliberately), `host_cpu_pct` +
   `host_ram_used_gb` + `host_ram_total_gb` (omitempty, a background sampler's cached read), and
   `served_models` (omitempty, the cached llama-swap roster). `schema_version` stays 1; a node that
   never upgrades emits a byte-identical payload minus these keys.
4. GPU utilization is adopted **only as a tie-breaker**, the fourth and lowest-priority key in
   `betterRemote` — consulted only after capacity, provable-free-slot, and queue depth are all tied,
   and only between two nodes that both publish `gpu_util_known: true`. This is the direct rejection
   of PAIR's primary-signal usage: on this fleet, "which idle seat is best" is answered by whether
   the contract fits and whether a slot is provably free, never by which card happens to be least
   busy at poll time.
5. `served_models`, when a node publishes it, becomes part of the hard placement gate
   (`seatServed`): a node whose roster does not name its own advertised `agent_seat` is ineligible.
   An unpublished roster (pre-0.113.0 node, or a cold cache) stays unknown and is never a refusal —
   this is capability information becoming stricter only where the node has actually said something
   false-sounding, never where it has said nothing.
6. `fleet-ui` runs as a **fifth process the operator starts by hand** — no scheduler, no service, no
   auto-start (house rule: no unattended schedulers/watchdogs). It binds loopback by default and
   refuses `0.0.0.0`/`[::]` outright; `--listen-trusted-network` is required to bind a tailnet
   address, mirroring `fleet-serve`'s own posture.

## Consequences

- No new trust surface: every route the page depends on is `GET`, metadata-only or already
  bearer-gated, and the page's own bind defaults to loopback with the same refusal logic
  `fleet-serve` already uses.
- No node-side UI or new node dependency — a node that never upgrades past 0.112.0 still works with
  everything except the new fields, which read as absent/unknown rather than breaking the poller.
- One more process to remember to start (`fleet-ui`) — accepted deliberately rather than made a
  service, per the no-unattended-schedulers rule.
- Placement gets measurably pickier in exactly two ways (`served_models` eligibility, the fourth
  ranking key) and both are additive refinements of the existing hard-gate/ranking design in
  `internal/delegate/gate.go`, not a new scheduling model.
- The fourth ranking key's preorder is **honestly incomplete on a mixed fleet**: a tie between a node
  that publishes `gpu_util_known` and one that does not resolves by roster order, not by a synthesized
  comparison. This is documented, not hidden, and the remedy is operator-side (upgrade every node),
  not a code-side coercion of unknown into a number.

## Alternatives considered

- **Adopt PAIR wholesale (discovery, pairing, mTLS, port takeover).** Rejected: PAIR's trust model
  solves problems this harness's tailnet + static roster + bearer gate already solve more strongly;
  adopting it would be a regression in exchange for compatibility with a scheduler this harness does
  not run.
- **A node-side page on every box.** Rejected: N pages to check instead of one, and every node would
  need to learn about its siblings — duplicated state the delegator already holds for free by virtue
  of being the thing that already polls health for placement.
- **GPU utilization as a primary ranking key**, matching PAIR's own scheduler. Rejected: PAIR's own
  README states this design suits only a fleet of similar machines: this fleet is heterogeneous by
  design (a workstation, a laptop, a mini-PC with very different context ceilings), and a contract's
  fit and a slot's provable availability are the signals that actually determine whether work
  succeeds there — utilization only ever decides what capacity and queue depth left tied.

## Related code

- `internal/fleetview/` — the poller, types, embedded page, and `RenderTop`.
- `fleet_ui_cmd.go`, `top_cmd.go`, `fleet_smoke_cmd.go` — the three new verbs.
- `internal/delegate/gate.go` — `seatServed`, `betterRemote`'s fourth key.
- `internal/fleetnode/server.go` — `healthPayload`'s new fields, `handleJobs`.
- `internal/hostsample/hostsample.go` — the background host CPU/RAM sampler.

## Related docs

- [systems/fleet-overview.md](../../systems/fleet-overview.md)
- [systems/fleet-node.md](../../systems/fleet-node.md)
- [0023](0023-agent-lane-tailnet-auth-and-locality.md)
