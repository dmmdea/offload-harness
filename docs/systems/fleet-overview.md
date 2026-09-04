# Fleet overview

## Purpose

A read-only operator page that answers "what is my fleet doing right now?" in one place, ported
from the PAIR project's operator surface (2026-09-03 gap analysis: PAIR's discovery/pairing/mTLS and
port takeover were not worth adopting — the harness's tailnet-only bind and static roster are
already stronger — but its live operator view was). The delegator polls every configured node's
`/fleet/health` and `/fleet/jobs`, folds in its own delegation-log corpus, and serves the result as
one card per node plus a cluster jobs feed and an errors feed — in a browser (`fleet-ui`) or a
terminal (`top`). See [ADR 0034](../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md)
for why this stayed additive and read-only rather than adopting PAIR wholesale.

## Questions this doc answers

- What does the page show, and where does each number come from?
- Why is a node's utilization showing `n/a`?
- Why does the page show a job a node no longer lists?
- Why did a node stop being placed on after an upgrade?
- Why is the page read-only and unauthenticated, and why is that safe?
- What is `fleet-smoke`, and why does its token live only in a context doc?
- How does `top` differ from `fleet-ui`, and what does it need to run?

## Scope

`internal/fleetview` (the poller, the Overview/Node/Error/Point types, the embedded page and its
JSON API), the `fleet-ui`, `top`, and `fleet-smoke` verbs, and the health/jobs fields on the node
side that feed them (`gpu_util_pct`, `gpu_util_known`, `host_cpu_pct`, `host_ram_used_gb`,
`host_ram_total_gb`, `served_models`, `GET /fleet/jobs`) together with their two new placement
consequences in `internal/delegate/gate.go`.

## Non-scope

- The node contract itself (routes, job state machine, VRAM sampling, the agent task wire shape) →
  [fleet-node.md](fleet-node.md). This doc only covers the additive health/jobs fields fleet-overview
  reads and the fields fleet-overview does not need.
- Placement mechanics beyond the fourth ranking key → [fleet-node.md](fleet-node.md)'s "Placement
  routes and the retry" section and `internal/delegate/gate.go` itself, which this doc points to
  rather than restates.
- Writing to a node. Every route this page reads is a `GET`; it never dispatches, drains, or
  otherwise mutates fleet state.
- Historical metrics beyond the in-memory sparkline window — there is no database, no export, and no
  retention past process restart.

## Key concepts

**Node** (`fleetview.Node`) — one polled node's current state: reachability, GPU/VRAM/host numbers,
agent-lane advertisement, queue counters, a bounded sparkline history, and its most recent jobs.

**Overview** (`fleetview.Overview`) — the full snapshot served as JSON at `/api/overview`: every
node, the errors feed, and the delegation-log rows, as of one poll tick (`At`).

**Point** (`fleetview.Point`) — one bounded history sample (GPU util, CPU, VRAM free, RAM used) used
to draw a node's sparklines.

**Errors taxonomy** (`fleetview.Error`) — three sources, two severities:

| Source | Severity | Meaning |
|---|---|---|
| `probe` | `error` | The node's `/fleet/health` fetch failed this tick (unreachable, timeout, non-200). |
| `job` | `error` | A job in that node's `/fleet/jobs` feed reached state `error`. |
| `delegation` | `warn` | A row in the delegation-log corpus was deferred, or ran but failed acceptance. |

**`fleet-smoke` contract** — the harness's version of PAIR's "Test traffic" button: one grounded
contract (harness default step budget, 60 s cap) dispatched to every configured node with
`route=remote` forced.

## How the system works

`fleet-ui` (`fleet_ui_cmd.go`) starts an `internal/fleetview.Poller` against a roster — explicit
`--remote` flags, else `cfg.DelegateRemotes` plus this box's own `fleet-serve` listener when that is
bound beyond loopback (`fleetUIRemotes`) — and serves `internal/fleetview.NewHandler(p)` on
`127.0.0.1:18813` by default.

**Poll cadence and history.** `Poller.Run` probes immediately, then on `--interval` (default 5 s).
Each tick (`Poller.tick`) fans out to every configured base **in parallel**, each with its own
5 s (`probeTimeout`) budget — never one shared deadline for the whole roster, so one slow node
cannot stall the others' cards. Every reachable node's `/fleet/health` reading appends one `Point` to
its history, capped at `--history` points (default 120; roughly 10 minutes at the default interval).

**Dedupe.** `seenErr` (a `map[string]bool`) stops the same failure from re-appearing on every tick:
a probe failure key (`"probe|<base>"`) is set on the first failure of a streak and cleared the moment
the node answers again, so the *next* streak reports once more. A job-error key
(`"<node_id>|<job_id>"`) and a delegation-row key (`"deleg|<job_id>"`) are each set once, the first
time that row is seen in an `error`/deferred/failed state.

**Prune rules.** After every tick, `pruneSeenErrLocked` drops job and delegation dedupe keys whose
underlying row has fallen out of the retained window (a node's current `Jobs` slice, or the last
`maxDelegations` — 100 — corpus rows) — so if the same job id is ever re-dispatched after its old
row aged out, its error reports again rather than staying silently suppressed forever. Probe keys are
left alone; `fold` already clears those on the node's next successful answer. `errors` itself is
capped at `maxErrors` (200), oldest dropped first.

**A failed `/fleet/jobs` fetch is distinguished from an empty one.** `tick` fetches health and jobs
independently per node; when the jobs fetch fails (timeout, 5xx, or a pre-0.113.0 node's permanent
404 on the route), `fold`'s `jobsOK=false` path leaves that node's `n.Jobs` exactly as the last
successful fetch set it — it never overwrites a still-populated list with an empty decode of a failed
response — and skips that node's job-key pruning that tick too, since `pruneSeenErrLocked` derives
validity from `n.Jobs`, which is unchanged.

**Deep-copy on read.** `Poller.Snapshot()` returns a full copy: node structs are copied by value, and
every `[]map[string]any` field (`gpu_devices`, `jobs`, `delegations`) is deep-cloned recursively
(`cloneAnyMap`/`cloneAnyValue`) so a caller mutating the returned `Overview` can never race the
poller's own goroutine mutating the same maps on the next tick.

**The delegation-log fold** (`errors.go`) reads today's and yesterday's day-sharded corpus files
(`BaseDir()/delegation-log/YYYY-MM-DD.jsonl`), keeps the last `maxDelegations` rows, and loosely
decodes only `delegationRowFields` (`ts`, `job_id`, `node`, `seat`, `placement_reason`, `deferred`,
`defer_class`, `acceptance_pass`, `wall_ms`, `error`) — a decode narrow enough that a future corpus
column can never break it, and deliberately excludes `contract` (and any field pulled from it, such
as `goal`): `/api/overview` is unauthenticated, and contract content has no business on the one route
this package serves with no auth at all. A missing corpus file (fresh install, a delegator that has
never run) is not an error. Every row that is `deferred` or failed acceptance becomes one `Error`
(severity `warn`, source `delegation`), deduped on `"deleg|<job_id>"`.

**The embedded page** (`internal/fleetview/server.go` + `ui.html`) serves three routes: `GET /{$}`
(the page itself, embedded via `go:embed`), `GET /api/overview` (the live `Overview` as JSON,
`Cache-Control: no-store`), and `GET /healthz`. The handler never issues an outbound request of its
own — all polling lives in the `Poller` it only reads from.

**`top`** (`top_cmd.go` + `internal/fleetview/topmodel.go`) is a pure client of a running `fleet-ui`:
a Bubble Tea program that polls `<ui>/api/overview` on its own `--interval` and renders
`RenderTop`, the same node table plus JOBS/ERRORS feeds as the page, in plain text for a headless
box. It needs no config file, no netguard bind check, and no delegator state of its own — point
`--ui` at the delegator's tailnet `fleet-ui` address.

## Important flows

**A tick, end to end:** `Poller.tick` fires every node's health+jobs probe concurrently →
`fold` updates that node's `Node` struct and appends new errors under `p.mu` → once every goroutine
returns, `foldDelegationLog` reads the corpus and appends any new deferred/failed rows →
`pruneSeenErrLocked` bounds `seenErr`. A browser or `top` client only ever reads the result of the
*last completed* tick through `Snapshot()`; it never blocks on or triggers a probe itself.

**`fleet-smoke`, one grounded contract per node** (harness default step budget, 60 s cap;
`fleet_smoke_cmd.go`): for each configured base, `smokeContract` builds an `AgentContract` whose
only context doc carries a token
(`PONG-<node-hint>`) nowhere else in the prompt, with acceptance `contains:<token>` — so a seat that
merely echoes the goal text cannot pass (the delegation skill's parrot rule). `MaxSteps` is left at
`0`, which `PrepareContract` fills in as the harness default (`core.AgentMaxStepsDefault`, 12), not a
hand-picked smaller cap: earlier smoke runs pinned at 1 step and then 3 steps both deferred on real
seats, because any seat that plans or tool-calls before its final reply needs more than a
hand-picked minimum — the real cost control is `TimeoutSec` (60 s), not the step count. `delegate.Run`
is called with `route="remote"` forced against that one base, so the row proves the node actually ran
the work rather than falling back to local. The table's verdict is `PASS` only when the contract
neither errored nor deferred, passed acceptance, and its `PlacementReason` actually starts with
`route=remote` — landing back on local (an ineligible or unreachable node) is a `FAIL`, not a
silent local pass. `fleet-smoke` exits non-zero unless every row is `PASS`.

## Data and state

All state is in-memory and process-lifetime only: `Poller.nodes` (by base), `Poller.errors` (bounded
200), `Poller.delegations` (bounded 100, refreshed every tick from the on-disk corpus), and
`Poller.seenErr` (bounded by roughly `nodes*jobsPerNode + maxDelegations + len(bases)`, per
`pruneSeenErrLocked`'s doc comment). Nothing is persisted by this package; restarting `fleet-ui`
loses history and the errors feed (which is why the delegation-log fold re-reads the corpus fresh
each tick rather than caching it — the corpus itself is the durable record, not this page).

## Interfaces and entry points

- `fleet-ui` verb (`fleet_ui_cmd.go`) — starts the poller and serves the page + JSON API.
- `top` verb (`top_cmd.go`) — terminal client of a running `fleet-ui`.
- `fleet-smoke` verb (`fleet_smoke_cmd.go`) — one-shot grounded contract per node, table output.
- `GET /` — the embedded overview page.
- `GET /api/overview` — the live `Overview` snapshot as JSON.
- `GET /healthz` — plain liveness check.
- Node-side additions this page depends on: `GET /fleet/health` (additive fields listed below) and
  `GET /fleet/jobs` (new route) — both documented in [fleet-node.md](fleet-node.md).

## Dependencies

`internal/config` (the roster and `FleetAuthToken`), `internal/netguard` (`SafeTransport`, the
loopback/bind checks both `fleet-ui` and `fleetUIRemotes` use), `internal/delegate` for
`fleet-smoke` only (`fleetview` itself deliberately does not import `internal/delegate` — see the
package doc comment in `overview.go`: that decoder discards the graph fields this view needs, and
pulling in the whole delegator would drag in placement/dispatch/ledger just to read telemetry), and
the Bubble Tea library (`github.com/charmbracelet/bubbletea`) for `top`.

## Downstream effects

None on `tools/list`, on dispatch, or on any existing node-side behavior — every field this page
reads is additive and `omitempty` (except `gpu_util_pct`/`gpu_util_known`, always-present) on
payloads nodes already emit. The one behavioral change outside this package is placement
(`internal/delegate/gate.go`): `served_models` publication is now a hard-gate input
(`seatServed`), and GPU utilization is a new, lowest-priority ranking key (`betterRemote`'s fourth
key). Both are described under "Placement" below and owned by `gate.go`, not this package.

## Invariants and assumptions

- This page never dispatches, drains, or writes to any node — every request it issues is a `GET`.
- A poll failure never crashes the page: `fold` records the error and keeps serving the previous
  state for that node.
- `Snapshot()` is always a consistent, fully-copied view — no caller can observe a partially-updated
  node or race the poller's next tick.
- `gpu_util_pct`/`gpu_util_known` are always present on `/fleet/health`; every other new field
  (`host_cpu_pct`, `host_ram_used_gb`, `host_ram_total_gb`, `served_models`) is `omitempty`, and
  `host_ram_total_gb`'s presence is the signal to trust the other two host fields (see
  [fleet-node.md](fleet-node.md)).
- **Placement (ADR 0034).** GPU utilization is a **tie-breaker only** — the fourth of four ranking
  keys in `betterRemote`, consulted only after capacity, provable-free-slot, and queue depth are all
  tied, and only between two nodes that both publish `gpu_util_known: true`. It is comparable **only**
  within that subset: on a mixed fleet, a tie between a node that publishes utilization and one that
  does not resolves by roster order (the single left-to-right scan in `Place`) **by design** — the
  same tie-break every earlier key already leaves open when it runs out of information. The remedy,
  when this matters operationally, is to upgrade every node so `gpu_util_known` is uniformly true and
  the ordering becomes total again — there is no code-side fix for a preorder that is honestly
  incomplete.
- **Placement (`served_models` eligibility).** `seatServed` (`internal/delegate/gate.go`) treats an
  **unpublished** roster (`len(ServedModels) == 0` — a pre-0.113.0 node, or a probe that has not
  landed yet) as unknown and never a refusal. A node that *does* publish a roster is only eligible
  when that roster names its own advertised `agent_seat` (case-insensitive) — so a node whose roster
  omits the seat it claims to run is correctly excluded, not silently trusted.

## Error handling

A probe failure (`herr != nil` in `Poller.fold`) marks the node unreachable, records `ProbeError`,
and appends one `Error{Source: "probe", Severity: "error"}` — deduped, so a downed node produces one
entry per outage, not one per tick. A jobs-fetch failure alone does not mark the node unreachable
(health can succeed while jobs fails); it just freezes that node's job list, per "Important flows"
above. Every error the page renders is a fact about the fleet, never an internal panic surfaced to
the operator — nothing in this package calls `panic` in a request path.

## Security and privacy notes

**Read-only by construction:** every route this page or its poller calls is a `GET`, and the page
itself exposes no form, button, or mutation of any kind — an operator watching it cannot accidentally
dispatch or drain anything from here.

**Unauthenticated on purpose, and safe because of what it carries.** `GET /fleet/jobs` (node side) is
deliberately unauthenticated for id/task/model/state/timestamps — metadata only, and never a job's
`payload` or result, so there is nothing there the per-job bearer gate (`handleJob`) exists to
protect. Its `error` string is the one exception: an AGENT row's error text can echo contract content
(the goal, a tool-result fragment) the way a media row's never does, so `handleJobs` applies the SAME
bearer check `handleJob` uses — when a token is configured and the request carries no valid bearer, an
agent row's `error` is omitted while every other field on every row (media rows included) stays as it
was. `fleet-ui`'s own page and `/api/overview` are equally unauthenticated for the fields they carry:
the aggregate adds only delegator-local placement/defer metadata on top of what the node routes already
publish (`internal/fleetview/errors.go`'s `delegationRowFields` — timing, placement, pass/fail, never
`contract` or anything pulled from it, such as a goal) — never contract text, and never more sensitive
than the per-node routes it reads.

**Tailnet-only bind, enforced twice.** `fleet-ui` refuses to bind `0.0.0.0`/`[::]`/a bare `:port`
outright, and refuses any non-loopback address unless `--listen-trusted-network` is passed — the same
posture as `fleet-serve`. The default is `127.0.0.1:18813`; the flag exists for an operator who wants
to check the fleet from another tailnet box, never for a public bind.

## Observability and debugging

- `curl http://127.0.0.1:18813/healthz` confirms `fleet-ui` is up.
- `curl http://127.0.0.1:18813/api/overview | jq .` is the fastest way to see exactly what the page
  sees, without a browser.
- A node card stuck on "unreachable" — check `n.ProbeError` in the JSON, or the ERRORS feed's
  `probe`-source rows, for the literal dial/timeout/status error.
- A node's utilization column reading `n/a` — see "Common pitfalls" below.
- `fleet-smoke --json` gives the same PASS/FAIL/DEFER rows as the table, machine-readable, for
  scripting a fleet health check.

## Testing notes

`internal/fleetview` covers the poller's dedupe/prune/bound behavior, the deep-copy guarantee on
`Snapshot`, the failed-vs-empty-jobs-fetch distinction, and `RenderTop`'s rendering (age formatting,
truncation, cross-node job sort) independent of a TTY. `internal/delegate/gate_test.go` covers
`seatServed` and the fourth ranking key, including the mixed-known/unknown tie case. Validate a real
deploy with `fleet-smoke` (proves every node's agent lane actually accepts and executes remote work)
and by loading `fleet-ui` in a browser or running `top` against it.

## Common pitfalls

- **"Why is a node's utilization showing `n/a`?"** Either the node predates 0.113.0 (no
  `gpu_util_known` field at all — decodes as `false`), or its GPU snapshot source cannot report
  utilization (only `nvidia-smi`-derived snapshots populate it; the ADAPTER-level WDDM fallback path
  does not). It is never a placement penalty by itself — see the invariant above.
- **"Why does the page show a job the node no longer lists?"** It doesn't, for long: a node's job
  entries are evicted after a TTL (one hour, `fleetnode`'s job-store janitor), and the next
  successful `/fleet/jobs` fetch drops it from that node's `Jobs` slice, which in turn lets
  `pruneSeenErrLocked` drop its dedupe key. Between two ticks after eviction it can still be visible
  if the poller hasn't re-fetched yet — a one-tick staleness, not a leak.
- **"Why did a node stop being placed on after I upgraded it?"** Check whether its `served_models`
  list now omits its own `agent_seat`. As of 0.113.0 the node publishes `served_models` as CANONICAL
  ids AND every alias (`swapclient.Roster.Names`) — the fix for the earlier alias-blind gate defect,
  where an agent seat bound by alias (the normal shape: `agent-pool` -> `qwen3.8-27b-vllm`,
  `offload-e4b` -> `gemma-4-e4b`) could publish only its canonical id and read as not-serving-its-own-
  seat forever. So a genuinely stopped node usually means a real roster alias mismatch (the seat's
  llama-swap alias changed but the config's `agent_model`/roster reference did not follow) —
  `seatServed` refuses it correctly, because the node is telling the truth about what it can currently
  serve. A pre-0.113.0 node/delegator pairing on either side of the upgrade is unaffected: an
  unpublished `served_models` roster (empty/absent) reads as UNKNOWN, never a refusal, so an old node
  talking to a new delegator (or a new node talking to an old delegator that ignores the field) keeps
  its prior placement behavior exactly.
- Treating `queue_depth` alone as a load signal on this page the way an older mental model might —
  read `jobs_running`/`jobs_queued` beside it, as [fleet-node.md](fleet-node.md) explains; this page
  shows both, not the ambiguous combined number alone.
- Assuming `fleet-smoke`'s cheap contract is testing a small step budget — it is not; the cheapness
  is the 60 s timeout, and `MaxSteps` is deliberately the harness's normal default (see "Important
  flows").

## Source map

- [`internal/fleetview/overview.go`](../../internal/fleetview/overview.go) — the `Overview`/`Node`/
  `Error`/`Point` types (JSON tags are exact — the embedded page reads them verbatim)
- [`internal/fleetview/poller.go`](../../internal/fleetview/poller.go) — poll cadence, per-node
  parallel probing, dedupe (`seenErr`), pruning, the deep-copy `Snapshot`
- [`internal/fleetview/errors.go`](../../internal/fleetview/errors.go) — the delegation-log corpus
  fold into the errors feed
- [`internal/fleetview/server.go`](../../internal/fleetview/server.go) +
  [`internal/fleetview/ui.html`](../../internal/fleetview/ui.html) — the embedded page and its
  `/api/overview` JSON route
- [`internal/fleetview/topmodel.go`](../../internal/fleetview/topmodel.go) — `RenderTop` (the pure,
  tested rendering core) and `topModel` (the Bubble Tea client of a running `fleet-ui`)
- [`fleet_ui_cmd.go`](../../fleet_ui_cmd.go) — the `fleet-ui` verb, bind refusal, roster resolution
- [`top_cmd.go`](../../top_cmd.go) — the `top` verb
- [`fleet_smoke_cmd.go`](../../fleet_smoke_cmd.go) — the `fleet-smoke` verb, its contract, and its
  table renderer
- [`internal/delegate/gate.go`](../../internal/delegate/gate.go) — `betterRemote`'s fourth key,
  `seatServed`
- [`internal/fleetnode/server.go`](../../internal/fleetnode/server.go) — `healthPayload`'s new
  fields, `handleJobs`
- [`internal/hostsample/hostsample.go`](../../internal/hostsample/hostsample.go) — the background
  host CPU/RAM sampler behind `host_cpu_pct`/`host_ram_used_gb`/`host_ram_total_gb`

## Related docs

- [ADR 0034](../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md)
- [fleet-node.md](fleet-node.md) — the node contract this page reads: health fields, job semantics,
  the agent lane
- [FLEET-NODE.md](../FLEET-NODE.md) — operator guide for running a node
