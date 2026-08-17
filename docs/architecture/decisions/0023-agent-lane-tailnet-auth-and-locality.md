---
status: Accepted
date: "2026-08-16"
---

# The agent lane is tailnet-only, bearer-gated, quality-first-placed, and hop-limited

Decision provenance: made by the operator-approved multi-node delegation plan (2026-08-16,
five-persona council reshape applied); this ADR records those already-made decisions per the
ownership rule in the index README.

## Context

The fleet contract ([systems/fleet-node.md](../../systems/fleet-node.md)) carried media render
jobs between self-hosted nodes on the operator's tailnet. 0.63.0 adds an **agent lane**: a fleet
node can now execute a self-contained delegation contract with its own local `agent.Build` loop
and return a versioned result, so a weaker second box does meaningful sub-agent work while the
primary GPU is busy.

That raises four forces the media lane never had to resolve:

1. **Never-cloud** ([ADR 0001](0001-defer-never-cloud-fallback.md)) forbids sending work to any
   remote model. A second machine the operator owns is not that — but the boundary needs a
   precise, enforced definition or it erodes one config edit at a time.
2. The agent lane drives a coding-agent loop on caller-supplied contracts. Unauthenticated, that
   is an RCE-class surface; the media lane's tokenless posture cannot simply be inherited.
3. The remote seat is deliberately **weaker** than the local one. Output from it must never
   degrade delivered quality (operator constraint, verbatim: "not a race about speed, its all
   about delivered quality and precision with efficiency").
4. A delegated agent that could itself delegate would turn a 2-node fleet into an unbounded
   recursion surface.

## Decision

### 1. Never-cloud carve-out — stated, not weakened

A self-hosted fleet node on the operator's tailnet is **local infrastructure**, not a cloud
model. ADR 0001 stands unchanged: the harness still never falls back to a third-party hosted
model. What delegation may reach is enforced structurally, in two layers
(`internal/netguard/tailnet.go`):

- **`TailnetURL`** vets every remote base URL at config load and at intake — allowed hosts are
  loopback, `100.64.0.0/10` IP literals (the Tailscale CGNAT block), dotless MagicDNS short
  names, and hostnames under the operator's **own** tailnet DNS zone (`houseTailnetSuffix`).
  A generic `.ts.net` rule was rejected: it would accept any tailnet's Funnel-published
  hostname — a public-internet endpoint wearing a tailnet-looking name.
- **`SafeDialContext` / `SafeTransport`** re-check at **every dial**: a hostname that passed the
  shape check is resolved, any answer outside loopback/CGNAT is discarded, and the connection
  goes to the vetted IP **literal** — never back through the name. This is the resolve-and-pin
  guard: a DNS answer that drifts to a public address after validation (classic rebinding) dies
  at the socket. The fleet ingress path bans hostnames outright to avoid exactly this class;
  seat endpoints exist to name MagicDNS hosts, so this lane pins instead of banning.

The same guard pair covers the Phase-A `seat_endpoints` config (per-model remote completion
bases) and the `cascade_remote_lanes` config (busy-aware cascade failover bases, roast
delta 7): values are vetted at load, naming the offending key, and again at every dial.

### 2. Transport: tailnet WireGuard; bearer token on the agent lane only (v1)

No TLS is added in v1 — the tailnet is WireGuard-encrypted end to end, and layering TLS inside
it buys nothing against the threat model (a compromised tailnet identity already holds the
bearer token).

Authentication scope v1 is **the agent lane only**: `POST /fleet/dispatch` with
`task_type:"agent"` and `GET /fleet/jobs/{id}` for jobs an agent dispatch created require
`Authorization: Bearer <fleet_auth_token>` (SHA-256 both sides, then constant-time compare —
the hash makes the comparison length-independent). With no token configured, a **non-loopback**
listener refuses agent dispatches outright (403 at ack time), and `agent` is withheld from the
node's advertised task types — an unsafe lane is refused loudly, never silently open.

Two weaknesses are **accepted, knowingly, for a 2-node personal fleet**:

- **Shared identity** — one token fleet-wide; a node cannot be distinguished or individually
  revoked.
- **No rotation** — the token changes only by editing every config and restarting.

Both are acceptable at this scale and would not be at fleet scale; revisit before a third-party
node ever joins.

**Media endpoints stay tokenless in v1.** The deployed media dispatcher and its clients predate
the token and must not start seeing 401s mid-fleet; whole-fleet token enforcement is a recorded
follow-up for a coordinated whole-fleet deploy window.

### 3. Quality-first placement: the hard gate, and idle-local-always-wins

Placement (`internal/delegate/gate.go`) is a pure function with one governing rule: **an idle
local node always runs the work**. `Place` never load-balances for speed — delegation exists to
keep work flowing while the local GPU is spoken for (machine-wide lease held), never to chase
throughput on a weaker seat. A remote node is even *eligible* only when every hard-gate
condition holds: operator opt-in (`agent_enabled`), roster-**verified** seat residency, the
contract's token estimate plus the loop reserve provably fitting the advertised context
ceiling, an `output_schema` present, and requester depth 0. No eligible remote means
queued-local, regardless of how busy local is.

**Weak output is quarantined by construction.** The only thing that crosses back is the typed
`AgentWireResult` — final text, optional schema-validated `structured`, counters, stop reason.
The remote transcript has no field to travel in, so remote reasoning cannot leak into the
caller's context by policy lapse; the type system forbids it.

**Delegator-side acceptance closes the wrong-valid-schema hole.** Grammar-constrained decoding
guarantees *shape*, not *content* — the constraint-tax finding from the delegation plan's
research pass: a small model decoding under a grammar pays for the constraint with content
quality, so a schema-valid result is not evidence of a correct result. Free-prose acceptance
criteria cannot be evaluated by anything, so v1 redefines `acceptance` as a machine-checkable
DSL (`contains:` / `not_contains:` / `regex:` / `min_items:<field>:<n>` / `nonempty:<field>`,
`internal/core/agentwire.go`) **evaluated by the delegator before merge**. A result that fails
a check is reported `failed_verification`, never a success. Unfalsifiable checks (empty
substrings, `min_items:f:0`) are parse errors, not no-ops, so a contract cannot count as
verifiable while verifying nothing.

### 4. Hop limit via receiver-derived depth

A remotely-executed agent must never delegate onward. The wire `depth` field is **advisory**:
the receiving node derives `effectiveDepth = max(1, wireDepth)` before the pipeline sees the
contract (`fleetnode.buildAgentRun`), so no downstream reader can trust a wire claim of
"origin". The placement gate separately requires the **requester's** depth to be 0. In v1 the
limit also holds structurally — `agent.Build` registers no delegate tool for any caller — and
the derived depth is carried so that when an in-loop delegate tool lands (v2), its
depth-0-only registration keys off the derived value, not the wire.

### 5. The N-node door

Nothing above is 2-node-specific. **Any future node — the operator's laptop node or a new box —
joins the agent lane by config alone**: set `fleet_agent_enabled: true`, the shared
`fleet_auth_token`, and the seat's `agent_ctx_tokens` on that node, and add its base URL to the
delegator's remotes. No code change; the placement gate picks it up from its own
`/fleet/health` advertisement.

### 6. Remote completion lanes (`seat_endpoints`, `cascade_remote_lanes`) are tokenless-on-tailnet — accepted, noted

Phase A lets any model seat resolve to a remote llama-swap base over the tailnet — statically
(`seat_endpoints`: that seat is always remote) or busy-aware (`cascade_remote_lanes`, roast
delta 7: the daily cascade's calls ride a lane only while the local machine-wide GPU lease is
held and the lane's roster verifiably serves the SAME model — quality-identical failover,
fail-closed to local, logged per rerouted call). llama-swap itself has no authentication, so
these lanes carry no credential — accepted on the same WireGuard-transport grounds as above,
and noted here so the gap is a recorded decision rather than an oversight. The lanes still
ride the full dial-time tailnet guard — including the lane's own **roster probe**, which reaches
llama-swap through `swapclient.FetchRosterGuarded` rather than the plain reader. (The plain
`swapclient.New` builds its client on Go's default transport, proxy and all: correct for this
node's own loopback llama-swap, and the one hole in "re-checked at every dial" while the lane
probe used it — `TailnetURL` admits a dotless MagicDNS name on shape alone, so only the per-dial
check can prove where that name still resolves.)

### 7. `delegate_subtask` (the in-loop tool) is parked to v2

v1 delegation surfaces are the MCP `agent_delegate` tool (registration gated on
`agent_delegation_enabled`, so `tools/list` is byte-identical when off) and the `delegate` CLI
verb. The placement gate ships now because those surfaces consume it; handing the delegate
capability to the agent loop itself is deliberately deferred until the delegation corpus from
real use says what the loop actually needs.

## Consequences

- The never-cloud boundary is now enforced at three points (config load, intake, every dial)
  instead of asserted in prose; a fat-fingered or hostile config edit pointing a seat at the
  public internet fails loudly at the earliest point that can catch it.
- A tokenless agent lane is impossible beyond loopback — misconfiguration fails closed at ack
  time and in advertisement, at the cost of one more config key to set per worker node.
- Placement can be conservative to a fault: an unadvertised context ceiling
  (`agent_ctx_tokens: 0`) or a not-yet-probed roster reads as ineligible, so a
  freshly-restarted worker briefly attracts no work. That is the intended failure direction —
  a wrong local placement costs queueing; a wrong remote placement costs quality.
- The shared token means a compromised node compromises the lane fleet-wide until every config
  is rotated by hand. Accepted at 2 nodes; recorded as the first thing to revisit at N > 2 or
  any non-personal deployment.
- Media traffic stays observably byte-identical through 0.63.0 (pinned by test), at the cost of
  a two-posture auth story until the whole-fleet enforcement window.

## Alternatives considered

- **TLS on the fleet listeners** — rejected for v1: the tailnet already encrypts transport, and
  certificate management across personal nodes adds operational surface with no added
  protection against the actual threat (a compromised peer inside the tailnet).
- **Generic `.ts.net` hostname allowance** — rejected: Tailscale Funnel publishes hostnames
  under `.ts.net` to the public internet, so the generic rule would launder a public endpoint
  through the tailnet check.
- **Per-node tokens / rotation** — deferred, not rejected: correct at fleet scale, overhead
  without benefit at 2 personal nodes.
- **Prompt-level hop limiting** ("do not delegate") — rejected outright: the limit must be
  structural (tool absent, depth derived server-side), because a prompt is advice to exactly
  the class of model the gate exists to distrust.
- **Trusting the wire `depth` field** — rejected: the receiving node derives its own; a buggy
  or hostile delegator must not be able to mint an "origin" contract on a worker.
- **Free-prose acceptance criteria** — rejected: nothing can evaluate them before merge, and
  the schema-valid-but-wrong failure mode is exactly the weak-seat risk the gate exists for.

## Related code

- [`internal/netguard/tailnet.go`](../../../internal/netguard/tailnet.go) — `TailnetURL`,
  `SafeDialContext`, `SafeTransport`
- [`internal/fleetnode/auth.go`](../../../internal/fleetnode/auth.go),
  [`internal/fleetnode/server.go`](../../../internal/fleetnode/server.go) — bearer check, the
  ack-time and poll-time gates, health advertisement
- [`internal/fleetnode/tasks.go`](../../../internal/fleetnode/tasks.go) — `agentTaskConfigured`,
  `buildAgentRun` (depth derivation, schema requirement)
- [`internal/core/agentwire.go`](../../../internal/core/agentwire.go) — contract, result,
  acceptance DSL
- [`internal/delegate/gate.go`](../../../internal/delegate/gate.go),
  [`internal/delegate/run.go`](../../../internal/delegate/run.go) — placement, delegator-side
  verification
- [`internal/pipeline/agenttask.go`](../../../internal/pipeline/agenttask.go) — node-side
  execution and the defer shapes

## Related docs

- [ADR 0001](0001-defer-never-cloud-fallback.md) — the never-cloud rule this carve-out preserves
- [ADR 0005](0005-loopback-only-serve.md) — the listen-side guard the dial-side guard mirrors
- [systems/fleet-node.md](../../systems/fleet-node.md) — the agent task's wire contract
- [systems/coding-agent.md](../../systems/coding-agent.md) — the loop a contract executes on
- [../../FLEET-NODE.md](../../FLEET-NODE.md), [../../OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md)
  — operator enablement
