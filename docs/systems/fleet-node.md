# Fleet node

## Purpose

The node side of the compute-fleet contract: `fleet-serve` advertises what this machine can do and
accepts dispatched jobs; `fleet-measure` establishes what those jobs actually cost in VRAM.

This document explains the behavior. For running a node, see [../FLEET-NODE.md](../FLEET-NODE.md).

## Questions this doc answers

- What does a dispatcher see when it asks this node about itself?
- What happens when the same job is dispatched twice?
- Where do the advertised VRAM footprints come from, and how much do I trust them?
- Why does health return 503 sometimes?
- What does an `agent` job carry over the wire, what comes back, and who needs the token?

## Scope

The HTTP contract surface, the job state machine and its idempotency semantics, VRAM sampling,
footprint measurement and persistence, node startup and drain, and the agent task's wire
contract and auth.

## Non-scope

- The dispatcher itself, which lives in its own repository
- How a job actually renders → [media-generation.md](media-generation.md)
- Graph execution details → [../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md)

## Key concepts

**Footprint** — a measured VRAM cost for a model family and task, advertised so a dispatcher can
place work. **Ack** — the node's acceptance of a dispatched job. **Drain** — an orderly shutdown that
stops accepting work while remaining readable.

## How the system works

`fleet-serve` exposes four routes:

| Route | Purpose |
|---|---|
| `GET /fleet/health` | Node identity, GPU vendor and architecture, live total and free VRAM, supported task types, loadable model families, measured footprints, queue depth, the node's job capacity (running / queued / both limits), GPU utilization, host CPU/RAM, and the agent lane's served-models roster (0.113.0) |
| `POST /fleet/dispatch` | Submit a job; returns `202` with an ack |
| `GET /fleet/jobs/{id}` | Poll one job's state and result; `?wait=<seconds>` (≤ 12) long-polls until the job is terminal |
| `GET /fleet/jobs` | Cluster jobs feed: recent jobs across this node, newest first, payload-free (0.113.0) |

The dispatch envelope is parsed **strictly** — unknown fields are rejected, and the body is capped.
Several contract-reserved fields are accepted and ignored, so the contract can grow without the node
needing to change first.

**Job states are `accepted` → `running` → `done` | `error`.** Terminal states are write-once: a late
completion cannot overwrite a finished job. Terminal entries are evicted after a TTL by a periodic
janitor, and `queue_depth` counts only non-terminal jobs.

**`accepted` is a real waiting state (0.100.0).** Accepting a job used to start it, so `accepted`
lasted microseconds and the node had no queue at all — just an unbounded pile of concurrent
executions that one config key happened to cap. A dispatch is now *admitted* to a FIFO, and a single
scheduler goroutine claims jobs only while an execution slot is free. Two independent limits fall
out of that:

- **`fleet_max_queue_depth`** (default 32) — the admission ceiling on `accepted` + `running`, i.e. on
  `queue_depth`. Exceeding it is the only thing that produces `503 queue full`. Unchanged in meaning
  from 0.99.0.
- **`fleet_max_concurrent_jobs`** (default 4) — how many admitted jobs execute at once. Exceeding it
  never refuses anything; the job waits in `accepted`. This is the limit that protects the single
  llama-swap endpoint and the GPU behind it.

Both read `0` as "use the built-in default" and a negative value as "unlimited". A **busy node is not
a full node** — that distinction is the entire point of the split.

Health reports both sides: `queue_depth` (unchanged meaning and shape, for existing readers such as
the delegator's placement tie-break) plus `jobs_running`, `jobs_queued`, `max_concurrent_jobs` and
`max_queue_depth`. Publishing the limits is what lets a delegator see a node's *capacity* rather than
only its current depth — and since 0.101.0 the delegator uses them: a node whose `queue_depth` has
reached its `max_queue_depth` is the node that will answer `503`, so it now ranks below every node
that is not provably full, and a node with a free worker and an empty backlog ranks above one whose
workers are all busy. `queue_depth` still decides everything those two keys do not.

> **`queue_depth`'s meaning is unchanged, but its DISTRIBUTION shifts sharply.** It always counted
> `accepted` + `running`; before 0.100.0 those were all executing, so the number topped out near what
> the box could sustain and a high reading was a real alarm. Now most of it can be backlog, so a
> healthy node can legitimately sit at 31. Placement is unaffected — lower is still better, and the
> delegator's tie-break compares like with like across nodes — but an operator reading it cold will
> misjudge it. Read `jobs_running` / `jobs_queued` beside it.

Dequeue and `accepted` → `running` happen in one critical section, which is what makes the drain
distinction below trustworthy: a job still `accepted` when shutdown begins provably never started.

**Duplicate dispatch is idempotent, with one deliberate exception.** Re-dispatching a job id that is
`accepted`, `running`, or `done` re-acks `202` and does **not** start a second run. A job in `error`
returns `409`.

The asymmetry is intentional and worth understanding before changing it: the dispatcher treats any
non-`202` as a refusal and may send the job elsewhere. If a `done` job answered non-`202`, the
dispatcher would buy a duplicate render somewhere else in the fleet. A *failed* job answering `409` is
a deliberate, explicit refusal — this node tried and could not, so another node legitimately should.

> After TTL eviction a re-dispatched id looks new and will re-render. Documented and accepted.

**Health returns 503** when the VRAM snapshot is missing or older than 30 seconds. Refusing to answer
beats answering with stale numbers a dispatcher would place work against.

`fleet-serve` refuses to start without a working GPU probe — advertising a zero-VRAM node would make
the dispatcher treat the box as broken rather than absent. Shutdown drains before closing the
listener, so pollers can still read final state.

**Drain finishes what it started; it does not start what it never began.** The backlog is dropped the
moment drain begins, in-flight runs get the drain timeout, and every surviving job is then marked
terminal with the honest reason: `error: "interrupted"` for one that was executing, and
`error: "not started: node shut down while this job was queued"` for one that only ever waited.
Collapsing those two would have the node claim it began work it never touched — and they route
differently for a caller, since a never-started job is the cheapest possible thing to re-issue while
an interrupted one may have left partial output behind.

## Data and state

- **Footprints** persist to `~/.local-offload/footprints.json`, written atomically (temp file plus
  rename). A corrupt or missing file opens empty with a log line rather than crashing.
- **Jobs** are in-memory with TTL eviction.
- **VRAM snapshots** are held by a sampler goroutine.

## VRAM sampling — two sources, two purposes

This is the single most misread part of the system. There are two different questions, with two
different answers:

**Live node capacity** (`vram_total_gb`, `vram_free_gb` in health) comes from a **resolved memory
provider** ([ADR 0014](../architecture/decisions/0014-gpu-memory-provider-and-uma-sampling.md)):
`nvidia-smi` where it works, else the windows-generic WDDM source (registry `qwMemorySize` capacity
+ `\GPU Adapter Memory` PDH usage; UMA iGPUs advertise carve-out + the ~RAM/2 shared budget and
Dedicated+Shared usage) — a global sampler polling every two seconds either way. There is no
per-process path here. A sampling failure keeps the last good snapshot rather than publishing
zeros, bounded by the 30-second staleness gate.

**Multi-GPU:** a working `nvidia-smi` node runs a per-device query (`index,uuid,name,memory.total,
memory.used`, one line per GPU — `nvidiaSmiMemoryDevices`/`fleetnode.ParseSmiMemoryDevices`; since
0.116.0 the command and parser live in the leaf `internal/gpuprobe`, and the fleetnode names are
aliases over it so the composite tier's placement guards read cards through the same parser) on
that same 2-second sampler instead of the single-value query, and publishes the full breakdown as
`gpu_devices[]` in health — additive, and **always present when nvidia-smi is the resolved
source, including a single-GPU box** (a one-element array; there is no single-GPU special case —
`chooseSamplerKind` in `main.go` is the exact routing decision, unit-tested in
`fleet_verbs_test.go`). It is omitted only on windows-generic, which has no per-adapter signal to
enumerate. A new field is additive in practice too: the fleet-dispatcher decodes health with a
plain `json.Decoder` and sets `DisallowUnknownFields` nowhere in its `internal/`, so an extra
field on any node — single-GPU included — is silently ignored, not a wire break. The headline
`vram_total_gb`/`vram_free_gb` pair is picked by
`fleetnode.SelectHeadlineDevice`: the config-pinned `primary_gpu_uuid` card when set and present,
else `fleetnode.HeadlineDevice`'s fallback — the device with the **largest total VRAM** (ties
broken by more free VRAM) — never nvidia-smi's own line order, which is PCI bus order and has no
relationship to which device a CUDA app actually computes on (`CUDA_DEVICE_ORDER=FASTEST_FIRST` can
bind `cuda:0` to a different index). This is the fix for a real mis-report found live in 2026-08 on what was then a 2×16 GiB
Blackwell box (<node-b>): the donor card (nvidia-smi index 0, the RTX 5060 Ti) was being advertised as
the fleet's free VRAM while renders ran on the compute card at index 1 (the RTX 5070 Ti), which
could over-admit a second job that then contends or OOMs — and because the two cards are a
near-tie in total VRAM (16311 vs 16303 MiB), even the largest-total fallback still headlines the
wrong one there. **Canonical guidance (CMP tier notes): pin by GPU UUID, never index** —
`primary_gpu_uuid` is the deterministic fix for exactly this case; a UUID is stable across reboots,
reseats, and whatever order nvidia-smi or CUDA choose to enumerate in. A pinned-but-not-found UUID
falls back to the largest-total rule and logs one stderr warning (never silent — a typo'd UUID
must not quietly revert to guessing forever). `ParseSmiMemory` (2-field, first-line) itself is left
unchanged — it has a caller outside this path (the per-process footprint delta sampler below) that
must not silently change behavior.

**Per-render footprints** use **per-process PDH counters as primary**, with a global-delta sampler as
fallback. On Windows the sampler reads `\GPU Process Memory(*)\Dedicated Usage`, enumerates
instances, and sums only the render's own process tree — or Dedicated **plus Shared** in the
`pdh-shared` mode the UMA tier seeds (on an iGPU allocations land in Shared and Dedicated reads ~0). This is the only per-process option available:
consumer cards with a display attached run under WDDM, where NVML per-process accounting returns N/A
and `nvidia-smi` can therefore only see global memory.

Advertised footprints are the **raw max-observed peak**: a new observed peak sets
`vram_peak_gb = round(observed, 0.1)` — the node adds **no** margin; the dispatcher owns all routing
margin ([ADR 0013](../architecture/decisions/0013-nodes-advertise-raw-footprint.md)). Only successful renders with a positive peak are
recorded. Footprints merge across processes by file mtime, so `fleet-measure` run while a node is
serving becomes visible to the running node.

Full reasoning in [ADR 0008](../architecture/decisions/0008-pdh-primary-vram-sampling.md).

> **Configuration caveat:** the sampler selection predicate is "not `global`", so `"pdh"` and
> `"auto"` behave identically and a typo selects PDH. `"pdh"` on a non-Windows host silently yields
> the global-delta sampler.

## Interfaces and entry points

- `local-offload fleet-serve --listen <addr>` — default `127.0.0.1:18811`.
- `local-offload fleet-measure` — runs one minimal render per configured task through the normal
  pipeline, so the passive footprint hook records exactly what fleet jobs will. Voice and run-graph
  are deliberately skipped.

Binding beyond loopback requires `--listen-trusted-network`. Note that `:18811` with an empty host is
treated as non-loopback and refused — see
[ADR 0005](../architecture/decisions/0005-loopback-only-serve.md).

## Dependencies

A resolved GPU memory provider for capacity (`nvidia-smi`, else WDDM registry+PDH), Windows PDH for per-process footprints, the media generation stack for
actually running jobs, `internal/netguard` for the bind guard.

## Downstream effects

Health payload shape is a published contract. Changing a field name or the ack semantics breaks the
dispatcher's placement logic — and the duplicate-ack semantics in particular have fleet-wide cost
implications.

## Invariants and assumptions

1. Terminal job states are write-once.
2. `done` re-acks `202`; only `error` returns `409`.
3. Advertised `vram_peak_gb` is never zero or negative.
4. Health answers 503 rather than serving a stale snapshot.
5. The node refuses to start without a working GPU memory source.
6. An agent dispatch on a non-loopback listener with no `fleet_auth_token` is refused (403), and
   `agent` is withheld from the advertised `supported_task_types`.
7. An agent defer is a `done` job carrying `deferred: true`, never an `error` job.
8. No transcript crosses the fleet wire — the agent result envelope has no field for one.
9. The vision lane (`POST /fleet/vision`, 0.116.0) is advertised — `vision` in
   `supported_task_types`, `vision_model` in health — exactly when `VisionLaneAdmissible` holds
   (a bound `vision_model` and the agent lane's reachability rule), rides the agent lane's bearer
   gate on dispatch and on poll, and stores the node's FULL `core.Result` as the done job's data
   (a defer is a `done` job saying `deferred: true`). Its body cap is `VisionBodyCap`
   (`vision_max_image_bytes` × 4/3 + slack), not dispatch's 1 MiB; everything after the body read
   is the shared `admit` path. Details: [FLEET-NODE.md](../FLEET-NODE.md#the-vision-task-post-fleetvision),
   [ADR 0040](../architecture/decisions/0040-vision-work-travels-to-a-node-with-an-idle-card.md).
10. The cascade chat lane (`POST /fleet/chat`, register C-41b) is advertised — `chat_lane` in
    health, alongside `served_models` — exactly when `ChatLaneAdmissible` holds (a bound
    `endpoint` and the agent lane's reachability rule), and rides the agent lane's bearer gate.
    It is the ONE surface here that is **not a job**: it forwards a single OpenAI chat completion
    synchronously, byte for byte, to this node's own llama-swap and copies the answer (and the
    upstream status, unflattened — the caller's seat-wait loop keys on llama-swap's 429/503) back.
    It serves only what this node's roster serves, alias-aware: `404` for any other model, `503`
    when the roster is unreadable, `502` when the forward itself fails. Body cap `ChatBodyCap`
    (8 MiB — a cascade prompt carries its document). It exists because this node's llama-swap
    binds loopback only, so a delegator cannot reach it directly; the caller's half is
    `llamaclient.FleetLaneGates` (see
    [offload-pipeline.md](offload-pipeline.md#security-and-privacy-notes)).

## Security and privacy notes

The **media** contract is unauthenticated and assumes a trusted network — in practice a tailnet.
That assumption is acknowledged by the explicit `--listen-trusted-network` flag. Since 0.65.0
the **agent lane** is the deliberate exception: bearer-gated when `fleet_auth_token` is set,
refused outright beyond loopback without one, because it executes caller-supplied agent
contracts rather than renders (see [The agent task](#the-agent-task-task_type-agent) and
[ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)). Node
identity defaults to the hostname, so operator documentation uses placeholders rather than real
names.

## Observability and debugging

- `curl <node>/fleet/health` is the fastest check that a node is serving and has fresh numbers.
- `fleet-measure` prints raw records including `observed_peak_gb` and sample counts.
- **MSI Afterburner is a recommended validation companion, never a dependency** — the harness imports
  nothing from it and every feature works without it. Bring-up procedure: compare its per-process plot
  against measured values; agreement within 15% means the PDH path is trustworthy on that machine,
  worse means set `fleet_sampler: "global"`. Procedure in [../FLEET-NODE.md](../FLEET-NODE.md).
- The counter set commonly reports bogus values for the desktop compositor instance. Harmless here —
  the tree-sum excludes it — but expect it in raw counter output.

## Testing notes

`internal/fleetnode/` covers the health golden shape and its 503 paths, the dispatch rejection matrix,
both duplicate-dispatch cases, the job state machine, footprint padding/merge/persistence, and the
PDH instance parser. `queue_test.go` pins the backlog/concurrency split — a job waiting while the
workers are busy, a burst whose peak overlap never exceeds the limit (with an unlimited control arm),
the server admitting while busy and refusing only when full, and drain telling never-started from
interrupted. `healthwire_compat_test.go` is an EXTERNAL test package so it can run the delegator's
real `FetchNodeView` decoder against the real health handler: the additive capacity fields must not
break a reader that has never heard of them. `auth_test.go` pins the agent-lane auth matrix and the media lane's tokenless
bypass; `tasks_agent_test.go` the advertisement gate and contract materialization;
`internal/pipeline/agenttask_test.go` the defer shapes over a fake chat client.
`fleet_verbs_test.go` covers parameter resolution and the bind guard.

## Common pitfalls

- Believing the per-process PDH tree supplies health's VRAM numbers. It does not — that is the resolved provider (`nvidia-smi`, or the ADAPTER-level WDDM counters on the generic path).
- Expecting `queued` as a state. The first state is `accepted` — which, since 0.100.0, is also the
  *waiting* state. A job sitting in `accepted` for a while is a queued job, not a stuck one.
- Reading `fleet_max_queue_depth` as a concurrency limit. It caps the backlog (`queue_depth`);
  `fleet_max_concurrent_jobs` caps execution.
- Assuming a `503 queue full` is the end of that subtask. Since 0.101.0 `internal/delegate`
  **re-places** it on another eligible remote and then on the local seat (bounded: the first choice
  plus `maxRemoteReplacements` = 2 more remotes, then local). What it does NOT re-place is a
  `400`/`401`/`403` — those are about the request, not the node — nor anything after a `202` ack.
- Assuming re-placement makes a saturated fleet free. Every placement spends from the contract's own
  `timeout_sec`, and when nobody takes the job the subtask fails with `placement refused: …`.
- Expecting a duplicate dispatch to return an error. Only `error` jobs do.
- Binding with `:18811` and expecting it to work as loopback.
- Treating Afterburner as required.

## The node's lease and its store (0.113.16)

**`lease`** — published in `/fleet/health` only while the machine-wide GPU lease (docs/systems/gpu-lease.md) is HELD:
`{"held":true,"class":"text|media","pid":N,"reason":"…","until":"RFC3339"}`. Absent = unreserved (or a pre-0.113.16 node;
both read as eligible). A held **text** lease makes this node a non-target twice over: the delegator's gate skips it
(`NodeView.LeasedText`, the remote twin of the local `Reserved()` rule from 0.113.14), and this node's own `/fleet/dispatch`
refuses NEW work with the same re-placeable 503 the queue cap uses — `node leased (gpu lease class=text pid=N reason=… until …)`
— so a delegator of any version places the subtask elsewhere. Known jobs re-acked and result polls are never refused; a media
lease (a render arbitrated on the node itself) never refuses. The node stays up, keeps answering health and finishes what it
holds: a measurement window no longer stops the fleet node to keep foreign digests off the card.

**Duration, not just class (0.113.27).** The lease block also carries `remaining_sec` and `busy` — this node's OWN verdict that its card is
spoken for long enough that a delegator should place elsewhere, for a lease of ANY class, decided by `fleet_busy_lease_sec` (default 120
seconds; negative disables the rule and restores the text-only behaviour). The verdict travels rather than the threshold, so a delegator never
needs a remote box's config; a node one release behind omits both fields, which decode to false and mean exactly what they meant before. The
delegator reads it as `NodeView.LeaseBusy`, `remoteEligible` excludes it exactly as it excludes a text lease, and an operator asking why
nothing landed there sees "long GPU lease held" beside the existing "text lease held". Why: a MEDIA lease never refused, which is correct for
the 20-second render it was designed around and wrong for the harness's longest jobs — anything holding that lease for hours left this node
publishing idle slots while its card was gone, and placement routed work TOWARD it. `refusing` and `idle_slot` are now derived together, so a
node that would turn work away never advertises a free slot; and the saturation SCORE is computed from the CAPPED running set, the same set
`idle_slot` measures, because feeding it the all-jobs count let a node publish `score 1.0` and `idle_slot true` in one payload.

**`serving_config_spec_sha256`** / **`serving_config_state`** (0.123.0, ADR 0043) — the rendered llama-swap config's
provenance. These are **new keys on this existing endpoint**: no new route, no new bind, and every pre-0.123.0
delegator keeps decoding the payload unchanged. The spec hash is the config's identity (the sha256 of the closed
input set it was rendered from: tier, render params, template, tier entry, harness version); the state is this
node's own verdict on it — `MATCH`, `STALE`, `UNSTAMPED` or `HAND-EDITED` — computed by re-rendering from the
node's embedded tier seeds, the same derivation `local-offload audit-yaml --against-render` uses.

Published only when `serving_config_path` names this node's rendered config. There is no safe default — every node
keeps it somewhere else (a top-level `llama-swap/` directory on one Windows box, the install-root stack directory on
another, a service `etc/` directory on the Linux node) — and a guess landing on the wrong file would publish some
other config's provenance as this node's. Both keys are **omitted** when the path is unset or the file cannot be read, so "this
node does not report" stays distinguishable from "this node reports MATCH"; an `UNSTAMPED` file publishes the state
with no hash, because the state is the finding. The verdict is cached on the file's (mtime, size) — health is polled
every few seconds by every delegator and the verdict costs a re-render. The node only READS the file; re-rendering
is `install render`, run by a human.

**`store`** — published only when `fleet_store_root` is configured: the store steward's last status
(`root, used_gb, cap_gb, high_gb, low_gb, files, last_scan, last_prune, last_removed, last_freed_gb, prunes, jobs_since_tick,
error`). The steward (internal/storesteward) keeps a persistent KV page store this node owns on disk under a budget the box
computes — `cap = min(fleet_store_cap_gb, 0.8 × (used + free))`, high 95 %, low 85 %, oldest-first by mtime, files younger
than 60 s never removed — and runs a scan after every `fleet_store_prune_every_jobs` completed jobs (default 8), on any health
poll whose last status was above the high mark, and once at start. At most one scan runs at a time; a health poll never walks
the directory itself. The root must carry a `.storesteward` marker file (written by the steward into an empty root; a populated root without it is
refused at start — a mistyped `fleet_store_root` can never become an oldest-first purge of some other tree); every removed page is
one journal line. A missing root fails `fleet-serve` at start; a scan or prune error is carried in `store.error`, never
hidden. Why it exists: LMCache's fs_native eviction counts only pages the running MP server wrote and the seat wrapper prunes
at seat start only, so the Lenovo's store went 28 → 99 GB against a 100 GB quota in 75 minutes of real fan-out (2026-09-06).

## Bands, tenants, saturation and the capacity wait (0.113.18)

The fleet-flow chapter's L5/L6 (plan `2026-09-02-vllm-27b-seat-and-cache-server-tier.md`): the llm-d shape — a saturation
signal, priority bands, tenant queues, a TTL instead of a timeout — sized for three boxes and one choke point (the harness).

**Every dispatch carries a band and a tenant.** The band rides the envelope's `priority` (contract-reserved since v2,
accepted-and-ignored by every older node, sent only when non-zero); the tenant rides the `X-Offload-Tenant` header — a header
because `dispatchEnvelope` is decoded with `DisallowUnknownFields`, and a new field would `400` on every node one release
behind. Bands (`core.Band*`): `-1` sheddable (measurement / gate traffic), `0` production (the default and what an older
delegator sends), `+1` urgent; anything else is clamped, and a non-integer `priority` reads as 0 (lenient on purpose — it
was ignored before). The tenant is printable ASCII ≤ 96 bytes, else anonymous; the delegator sends `host-pid-start`
(`delegate.DefaultTenant`, `LOCAL_OFFLOAD_TENANT` overrides) — one MCP server = one Claude session = one tenant.

**The store claims by band → tenant → arrival** (`Jobs.claimLocked`): highest effective band first (a sheddable job that has
waited `bandAgingAfter` = 60 s counts as band 0), then the claimable tenant served least recently (`Jobs.served`, a claim
sequence per tenant, pruned by the janitor), then the pending index. Anonymous tenants (older delegators) are one tenant, so
for them the store is exactly the FIFO it was. The uncapped-lane rule is unchanged: a media job never queues behind capped ones.

**The shed rule** (`handleDispatch`, before the queue cap): a band `-1` dispatch is admitted only into an IDLE execution slot
(`Jobs.IdleSlot`: empty backlog and a free capped worker, or unlimited concurrency) and otherwise refused
`503 shed (priority -1): no idle execution slot (…)`. Sheddable work takes idle capacity only — it never queues before OR
behind production work. The delegator re-places the 503 (any version) and, with no idle node anywhere, sheds the subtask at
once: deferred, `defer_class: "capacity"`, `summary.shed`. Media task types are uncapped, so a running render leaves the slot
idle — the rule is about the text seat's lane, which is what the cap protects.

**`saturation`** — always in `/fleet/health`: `{"score":0.5,"high":false,"idle_slot":false}`. `score` =
max(`jobs_running/max_concurrent_jobs`, `queue_depth/max_queue_depth`) over the limits the node publishes (an unpublished limit
contributes nothing); `high` = a new band-0 dispatch would be refused right now (backlog at `max_queue_depth`, draining, or a
held **text** lease — the three refusal states dispatch applies to new work, so the block can never disagree with a
dispatch); `idle_slot` = `Jobs.IdleSlot`. The delegator (`NodeView.Saturation*`) reads `high` as saturated, OR'd with its own
arithmetic (an older node ranks as before), and `hasRoom` uses the block as the capacity wait's "try this one" predicate.
Seat-level counters are deliberately not an input: this handler never probes the seat (a probe of an unloaded seat through
llama-swap LOADS it), and on this fleet the job counters already describe the load the harness itself puts on a seat. The
block is the seam a cached seat sampler would feed later, without a wire change.

**The capacity wait (delegator, `agent_placement_wait_sec`, default 120 s, negative = off).** `placeAndRun` used to end a
refused chain with `placement refused` the moment no untried node was left, and a reserved local seat waited on the LOCAL
lease alone (`agent_lease_wait_sec`) and then deferred — even when a remote had freed in the meantime. Now, when every node
that could run the subtask refused for CAPACITY (503/429; `placements.capacityRefusal`) or the only placement is a reserved
seat, `awaitCapacity` polls every `placementPollInterval` (3 s): the local seat once the lease clears (only reachable when it
WAS reserved — an unreserved, untried local seat is taken by `replacementNode` first), else the best remote whose health says
it has room (`hasRoom`), skipping a node that refused inside the wait for `refusalCooldown` (10 s). A node that passes and
still refuses is one more tick, paced by the poll — never a tight loop. Idle time is credited (`placements.credit`, so
`remaining()` hands the seat that finally takes the work its whole `timeout_sec`); attempts are charged as always. Outcomes:
landed (`summary.waited`, `results[].capacity_wait_sec`, `replacements` counts the refusals) · the wait exhausted with the
local seat still reserved → the holder-naming `infrastructure` deferral of 0.113.14 · exhausted otherwise → deferred, class
`capacity` (not `BrokenStackDefer`: the fleet is healthy, it was not this contract's turn) · sheddable → shed at once, no wait
· wait off → the pre-0.113.18 `placement refused` failure, byte for byte. Also closed: `replacementNode`'s local last resort
now honours a text lease (before, a remote's 503 fell straight onto the reserved cards — the 2026-09-05 incident through a
side door). The wait is bounded by the config key alone; `agent_lease_wait_sec` still applies when it is the longer of the two.

**The health probe itself: concurrent, memoised, negative-cached, bounded inside the wait (register D-106,
2026-09-17).** `fetchViews` — the delegator's read of every configured remote's `/fleet/health`, and the input to
every placement decision — probed the roster SERIALLY at `fetchNodeViewTimeout` (15 s) per remote, with no
cache, on the critical path of every subtask on `route=auto`/`remote`, of every re-placement, of every retry
and of every capacity-wait tick. Only `route=spread` amortised it, with one probe at run start. Measured over
1,977 delegation rows: **46 rows kept work on the local seat because one remote's health probe timed out, and
in 41 of them that remote had zero jobs in flight** — a node was excluded for being slow to answer a cached
read, not for being busy. Four changes, none of which adds a probe:

- **Concurrent.** Every remote is probed at once, each goroutine bounded by `fetchNodeViewTimeout`, so the
  fleet probe costs the SLOWEST remote rather than the sum of all of them. The answers are reassembled in the
  CONFIGURED order, because `views`/`bases`/`probeErrs` are positional (a `NodeView` carries no base) and
  completion order is not an ordering any caller can use.
- **Memoised per Run** (`fetchViewsMemoTTL`, 2 s). The sibling subtasks of one fan-out share one snapshot —
  what `spread` always did, now for every route — and the memo dies with the runner, so no snapshot outlives
  the call that took it. 2 s is deliberately shorter than the SHORTEST GAP BETWEEN TWO TICKS: the capacity
  wait exists to notice a node that just freed, so the memo must never be able to answer one of its ticks.
  The comparison is not against `placementPollInterval` itself — the tick is jittered, so the shortest gap
  is `(1 - jitterFrac) × placementPollInterval` = 2.4 s, and a 2.5 s memo would read as "safely under 3 s"
  while quietly serving ticks from cache. `TestProbeMemoCannotServeACapacityWaitTick` pins the relation over
  the production values, because every capacity-wait test zeroes the memo in order to compress the tick.
- **Negative-cached** (`probeNegativeTTL`, 30 s). A base that failed at the TRANSPORT — dial refused, no
  route, DNS, a reset, the per-base timeout — is skipped rather than re-dialled, and the reason it failed is
  REPLAYED into `probeErrs`, so the placement note still names the node and says what happened to it. Only
  transport failures: a `401`, `404` or `503` is a node that ANSWERED, and skipping it for half a minute
  would turn a momentary refusal into ineligibility.
- **The wait's tick may not outlive the wait** (`probeTickBound` = what is left of the wait). The wait passed
  the RAW run context to its probe while every other call site wrapped it in a remaining-budget one, so a
  probe could outlive the wait it was serving. Nothing TIGHTER belongs here, and both tighter bounds were
  tried and reverted: `2 × placementPollInterval` (6 s) sits below `fetchNodeViewTimeout` (15 s), which is
  this doc's own slow-vs-down boundary, so a remote answering in 6–15 s under load was cancelled on every
  tick, never became a candidate, and was never negative-cached either (a cancellation is not evidence about
  a node) — the wait then expired saying "no node had room", which was false. `min(fetchNodeViewTimeout,
  remaining)` fails differently: the per-base context is DERIVED from the tick's, so the two deadlines land
  on the same instant and the tick's fires first, nothing is ever attributable to a node, and a dead base is
  re-dialled at full cost on every tick (measured at 30 dials across one compressed wait, against 6 after).
  Nothing tighter is needed because the fan-out is concurrent and each goroutine is capped at
  `fetchNodeViewTimeout`: one black-holed remote costs a tick that bound ONCE and is then skipped by the
  30 s negative cache.
- **A tick's probe failures reach the operator.** The tick used to discard `probeErrs` with `_`, so a wait
  spent entirely on remotes that never answered ended as `no node had room … 0 refusal(s)` — an operator told
  to add a node when the nodes they had were failing to answer. Failures are now tallied per base across the
  wait and folded into the capacity defer as their own clause, `; N probe(s) failed during the wait: <base>:
  <reason> (last of 3)`, kept distinct from refusals because a probe failure is not a refusal: nobody
  declined the work.

**The fixed sleeps are jittered (2026-09-17).** `pollEvery`, `placementPollInterval` and `refusalCooldown` are
the same numbers in every dispatcher, so K sessions started within a second of each other re-read health,
re-dispatch and re-ask a refusing node in lockstep for a whole run — the convoy that makes a busy node look
busier than it is. Each sleep is now scaled by a uniform factor in `[0.8, 1.2]`; the cadence's mean is
unchanged, and the jitter is CLAMPED so no sleep can run past the deadline it lives under (the wait's TTL,
the poll deadline). The cooldown is jittered once when the refusal is recorded, not re-rolled per check, so a
node does not flicker in and out of the candidate list.

## What the node says about itself while work is in flight (unreleased)

Three facts the node held and never published, and one it published wrongly. Every input here is a
counter this node already keeps or a number it already reads — nothing new probes a seat, and nothing
new is sampled (register C-05 stands: probing an unloaded seat through llama-swap LOADS it).

| Health field | Type | Meaning |
|---|---|---|
| `jobs_admitting` | int, omitted when 0 | The subset of `jobs_running` whose worker has **not started generating**: it is still in the run's admission phase — cordon → swap pre-flight → warm → coherence probe — which the node budgets up to 300 s for. Counted from this process's own `gpuactivity` records with `phase: "admission"` (ADR 0041), never from the job store, which knows a worker took the job but not what that worker is waiting for. The registry is opened at most once per 2 s and **retried** — a briefly unresolvable state root does not silence the field for the life of the process — and a registry that cannot be opened or listed is logged once, because `0` is a legitimate value and silence would make the two indistinguishable. |
| `seat_loaded` | bool, omitted when unread | llama-swap's `/running` says the agent seat is loaded. |
| `seat_starting` | bool, omitted when unread | …and is still LOADING (llama-swap holds `/upstream/<seat>/…` for the whole load — 4m08s on the 27B TP2 seat, register D-92), so "loaded" is not yet "ready". |
| `lease_exclusive` | bool, omitted when false | The held lease FENCES the cards: no model may be loaded onto them for its duration. |
| `lease_draining` | bool, omitted when false | The held lease is still draining the seat. |
| `recent_agent_wall_sec` | float, omitted when none | Median wall of the last (up to) 8 agent jobs to FINISH here — the completion signal a node with no `seat_rate` sample has no other way to publish. The median, not the mean, so one 900-second outlier does not redefine the node. |

All six are additive and `omitempty`: a node with the agent lane off, no lease and no admission holds
emits a byte-identical payload, and a delegator that has never heard of them decodes exactly what it
decoded before (pinned in `nodetruth_test.go` and `healthwire_compat_test.go`).

**`saturation.score` now excludes admitting jobs from its concurrency numerator**, and only from
there. A job whose worker is cordon-waiting or warming holds a capped slot while the card is idle —
measured: 10 of 47 lease-timeout rows happened on a box with zero agent jobs in flight — so counting
it as utilization told every delegator to route away from an idle node. `saturation.high`,
`saturation.idle_slot` and the DEPTH term are deliberately unchanged: the slot really is taken, a new
dispatch really would queue behind it, and `max_queue_depth` bounds every admitted job whatever its
phase.

**Seat state is read from `/running` only**, through `internal/seatload`'s alias-aware reader
(`seatload.Running`): `/running` lists CANONICAL ids while the harness binds seats by ALIAS, so a
bare-name match reads a loaded seat as absent — the silent 0.113.16–19 drain defect. It rides the
residency refresh's background single-flight (one cycle per 30 s TTL, never one per request).

**Two different failures publish NOTHING, and both are logged.** A `/running` read that ERRORS leaves
both fields absent. So does an AMBIGUOUS one: when the roster GET fails, `seatload` falls back to
matching `/running` by the bare name (better than a refusal), and that fallback cannot see an
alias-bound seat listed under its canonical id — so `Loaded:false` *while `/running` lists models*
means "could not tell", not "idle". Publishing it as `seat_loaded:false` would assert a loaded seat is
idle exactly when the box is busy enough to time out a roster GET.

The predicate is `seatload`'s `Ambiguous` (a failed roster **and** a non-empty `/running`), which is
exactly what `gpu_drain` (`!rd.Loaded && rd.Ambiguous`) and `internal/placement/live.go`
(`err == nil && !rd.Ambiguous`) key on — and it is deliberately narrower than "the roster failed". A
failed roster over an **empty** `/running` is knowable: nothing is loaded on the box at all, so the
seat is not loaded either and no alias resolution is needed to say so, and health publishes
`seat_loaded:false`. Only the unknowable case is withheld: absent ≠ idle, the same rule the VRAM
snapshot and the reclaim verdict follow, with the reason on the node's log so an operator is not left
guessing at two missing keys.

**`GET /fleet/jobs/{id}?wait=<seconds>` is a completion event.** An already-terminal job answers at
once; anything else blocks on the job store's terminal broadcast — which the store has fired all
along — until the job finishes or the wait elapses, then answers with the job's live state. The wait
is capped at `MaxJobWaitSec` = 12 s and **must stay below the delegator's `pollRequestTimeout`**
(15 s, `internal/delegate/run.go`), or every long poll would be cancelled client-side a moment before
the node answered; the pairing is pinned by a test that reads the delegator's own source. Absent the
parameter the route is byte-identical, and the 3 s poll remains the fallback. Measured motivation:
236 queue-deadline rows spent exactly `101 poll(s)` = 300 s of pure polling.

**The blanket `WriteTimeout` (30 s) is a floor, not a ceiling.** Go arms it at header-read for every
handler alike, so it silently truncated the chat lane, whose own budget is `ChatProxyTimeout` = 10
minutes: a forwarded cascade call that generated past 30 s was cut mid-write and read to the caller
as a dead node. The blanket stays — it is what keeps every other route bounded — and the two handlers
that legitimately outlive it extend their OWN deadline per request through
`http.NewResponseController(w).SetWriteDeadline`: the chat lane to `ChatProxyTimeout` + 30 s of
copy-back slack, the long poll to its wait + 2 s. A `ResponseWriter` that cannot carry a deadline
(a recorder, a wrapper that does not unwrap) is not a failure — the handler just runs under the
blanket, as before.

## The acceptance gate (`local-offload acceptance`)

A node must pass this before it is handed work. It is deliberately NOT `doctor`: doctor
STATS the configured files, and both 2026-07-27 fleet failures passed doctor cleanly while
every dispatched job died.

| node | what doctor saw | what actually happened |
|---|---|---|
| Windows | the venv `python.exe` exists and is readable | it is a **uv trampoline** re-execing a base interpreter in ANOTHER account's roaming profile — it stats for everyone, runs only for its owner |
| Linux | the lease directory exists and is readable | it was owned by a different user, so the running identity could not create a lease file |

So every check here **exercises** the capability as the running identity — it runs the
interpreter, it writes to the lease directory — and the report leads with which identity
that was, because in both failures the binary, the config and the files were all correct
and only the account was wrong.

Checks: GPU lease writable (by writing a probe file and removing it) · every bound
interpreter runnable (`node`, `ffmpeg`, the PIL python, `sd-cli`) · derived media routes
carry no `BOUND-BUT-MISSING` · every configured model alias is in the live roster.
Unbound capabilities `SKIP` and never make a node look unready. Exit is non-zero when the
node must not be handed work.

**Capability is identity-dependent, and that is the point.** On the measured Linux node the
same binary and config report the PIL engine as `PASS` for the install owner and `SKIP` for
the service account, because the ComfyUI venv is not visible to the latter. Run the gate as
the identity the service runs as — `sudo -u <svc>` / the scheduled task's principal — or it
answers a question nobody asked. Relative script bindings resolve against the EXECUTABLE's
directory, so run the INSTALLED binary, not a copy in /tmp.

## What a node advertises about its VRAM

`/fleet/health` publishes four VRAM numbers, and only one of them is a safe divisor
for scheduling:

| field | meaning | why it is not enough alone |
|---|---|---|
| `vram_total_gb` | the card's capacity | over-counts every shared card — the measured workstation's desktop plus its always-resident support tier hold ~3 GiB that cannot be reclaimed at any price |
| `vram_free_gb` | free right now | under-counts a WARM node: a loaded, swappable model looks like lost capacity |
| `vram_reclaimable_gb` | what this node can free by unloading its own **swappable** seats | — |
| `vram_schedulable_gb` | `free + reclaimable` — **the number to divide by** | — |

Measured on a 16 GiB workstation, before and after loading one 4 GiB seat:

| | free | reclaimable | schedulable |
|---|---:|---:|---:|
| idle | 12.77 | 0 | **12.77** |
| warm | 8.74 | 4.04 | **12.78** |

Free drops by 4 GiB; schedulable stays flat. That is the property a dispatcher needs —
a warm node must not look full.

**How reclaimable is derived.** Two obvious mechanisms do not work: per-process GPU
memory (`nvidia-smi --query-compute-apps=...,used_memory`) returns `[N/A]` on Windows,
which is exactly the node with the shared desktop; and the footprint store records what a
RENDER task peaks at, not what the text tiers currently hold. So the node measures an
**idle baseline** — used VRAM observed while no swappable seat of ours is loaded and the
GPU lease is free — and reports everything above it as reclaimable. The baseline IS the
unreclaimable share, measured rather than assumed, and it re-measures as the machine
changes.

**Always-resident seats count as baseline, not capacity.** The support tier (embedder +
reranker) is co-resident on purpose; unloading it is what made a single RAG query pay
three model loads. Those seats are therefore treated as part of the baseline. Without
that rule a correctly configured node — which never reaches "nothing loaded" — would
report `unknown` forever.

**Which seats are resident comes from the CONFIG, not from `/running`.** The node used to
read residency off each `/running` row's `ttl` field, and llama-swap misreports it: a seat
configured `ttl: -1` (never unload) is published on `/running` as `ttl: 0` (verified live
on v249 — both support seats read `0` there today). The old rule survived that by accident,
because it also treated `0` as resident, but it got the opposite case wrong: a support seat
given a real TTL was counted as reclaimable, over-stating capacity by the size of an
embedder. The keep-set now comes from `pkg/llamaswap`, which parses the llama-swap YAML
(`ttl: -1` / `ttl: 0` seats, plus their aliases) and never asks the server. On a box where
no llama-swap YAML and no keep-set config can be read at all, the node falls back to the
old `ttl` reading rather than to "nothing is protected" — the permissive answer would fold
a resident embedder into the idle baseline and make that node under-advertise forever.

**Unknown is published as absence.** Before any idle baseline has been observed, both
numbers are OMITTED and only `vram_reclaim_source` is sent, explaining why. A consumer
falls back to `vram_free_gb`. Over-promising costs a failed job; under-promising costs a
scheduling opportunity, so the rule is deliberately asymmetric: the node never claims
reclaim capacity while holding nothing.

`harness_version` ships in the same payload — node/repo drift used to be found by hand,
and a node several releases behind gets debugged against known-fixed bugs.

## The agent task (`task_type: "agent"`)

Since 0.65.0 a node can execute a **delegation contract**: a self-contained sub-agent task it
runs with its own local `agent.Build` loop — read-only over the contract's materialized context
docs, no write/run/fetch/github capability, no delegate tool — and answers with a versioned
result. Decisions and rationale:
[ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md). The
delegator side (placement gate, acceptance evaluation, the `agent_delegate`/`delegate`
surfaces) lives in `internal/delegate` and is summarized in
[coding-agent.md](coding-agent.md#delegation-surfaces).

The task is advertised only when all three hold (`fleetnode.AgentLaneAdmissible`):
`fleet_agent_enabled` is true (explicit operator opt-in — default false, and the health payload
is byte-identical to a pre-0.65 node when off, pinned by test), an agent seat resolves (config
`agent_model`, else the workhorse `model`), and the lane is safely reachable (loopback
listener, or `fleet_auth_token` set).

`AgentLaneAdmissible` is the SINGLE predicate behind both the advertisement (`supported_task_types`
*and* the four `agent_*` health fields) and the ack-time admission. One function rather than two
condition lists, because the delegator reads only the `agent_*` fields: a lane advertised that
dispatch would refuse is a mis-route by construction, and the two lists had already drifted once
(health keyed on `fleet_agent_enabled` alone, so a tokenless non-loopback node advertised a
placeable lane and 403'd everything sent to it).

One predicate is only half the guarantee — it also has to be asked about the same **input**. Both
sides pass the **resolved** listener (`Options.LoopbackListener`, computed by the verb from where
the bind actually landed): health at construction, and dispatch by threading it into
`BuildRequest(ctx, cfg, loopbackListener, taskType, payload)`. `BuildRequest` used to derive its
own answer with `ConfigLoopbackListen(cfg)`, so a node with `fleet_listen: "0.0.0.0:18811"`,
`--listen 127.0.0.1:18811` and no token advertised `agent` from the resolved (loopback) view and
answered `400 unsupported task_type "agent" (supported: )` from the config view — the same
mis-route wearing a 400 instead of a 403. `ConfigLoopbackListen` now survives only for callers
with no listener at all. The dispatch handler's tokenless refusal consults
`AgentLaneSafelyReachable` (condition 3 standing alone) rather than re-implementing it, so the
`403` and the advertisement cannot disagree either.

`AgentLaneAdvertisement == dispatch admission` is asserted by
`TestAgentLaneAdvertisementMatchesAdmission` across the four (listener, token) combinations **and**
the config-vs-resolved mismatch row. The inverse mismatch (config loopback, resolved non-loopback,
tokenless) is not a hole: the auth guard `403`s it before `BuildRequest` runs at all.

### Placement routes and the retry (delegator side)

`delegate.Run` places each subtask per `route`:

| route | placement |
|---|---|
| `auto` (default) | `gate.Place`: an idle local seat always wins; remotes are considered only while the local GPU lease is held, and only the ones passing the hard gate (agent lane on, seat resident, contract fits the advertised ctx, output_schema present, origin hop). No eligible remote → queued-local. |
| `spread` (0.80.0, fit-scored 0.99.0) | one `Run` fetches every remote's health ONCE, then deals the subtasks across the local seat AND every remote that passes the hard gate for that subtask. The deal is computed for the WHOLE run in one pass before dispatch, and within each cycle of `len(nodes)` slots every eligible seat takes at most one subtask — so an N-contract fan-out genuinely runs on N seats at the same time, and the fit score can reorder a cycle but never collapse it (see "Fit-scored remote slots" below). The local rotation slot is never contested by shape: with the local seat IDLE, slot 0 is always the local seat (pinned by `TestDealSpreadKeepsSubtaskZeroLocal`, `TestDealSpreadSameShapedFanOutReachesEverySeat` and `TestRunSpreadDealsAcrossLocalAndEveryEligibleRemote`) and a 2-contract spread with an eligible remote is still guaranteed one local + one remote — the pair shape. It IS contested by load (0.113.20, `agent_spread_local_slot`): a local seat already holding a request at deal time loses its slots to the best-fit eligible remote with room — see "The local slot under load" below. Per-subtask eligibility means a contract failing the gate (no `output_schema`, over-size) silently takes the local slot instead; `results[].placement` names where each landed and, for a remote, which shape the fit score read. Measured before spread existed: `auto` put four concurrent contracts on one box, `remote` put four on the other one. No eligible remote → every subtask runs local and the reason says so. |
| `local` | forced in-process, no network. |
| `remote` | forced fleet node; with no eligible remote the subtask DEFERS loudly. |

Remotes come from the call's `remotes` argument, else from the config's `delegate_remotes` (tailnet URLs). A call's own list REPLACES the config list; it does not merge. A box with `delegate_remotes` set therefore fans out without the caller naming nodes — fleet membership is configuration, not per-call knowledge.

**Fit-scored remote slots (0.99.0).** `spread` used to deal the remote slots blind: `k := i % len(nodes)` and nothing more. Across heterogeneous seats that sends mechanical triage to the biggest seat and cross-file reasoning to the smallest one with equal probability. `internal/delegate/fit.go` now infers the contract's coarse SHAPE from its own goal text and scores the eligible seats:

- **Shape** (`inferKind`) is one of `mechanical` (extraction, listing, counting, filtering, digesting) or `reasoning` (explanation, causation, cross-file interaction, tracing, comparison). It is decided by an ORDERED deterministic pre-filter — a quantity rule, then an explanation rule, then a mechanical-verb rule — and the order is load-bearing: the quantity rule is what stops a bare `how ` pattern reading "how many files changed" as reasoning. No model call is involved; a placement is reproducible from the recorded contract alone.
- **A goal no rule matches is `mechanical`** — the CHEAP seat. This harness exists to move grunt work off the expensive seat, so ambiguity falls toward cheap, never toward capable. A wrong cheap placement costs a retry (which the engine already runs on a different seat); a wrong expensive placement costs the capable seat, which is the resource being protected. The unmatched branch is the seam a better fallback would plug into — a shape carried on the contract, or one decided per fan-out and reused — never a per-subtask model round-trip.
- **Score** (`scoreFit`) ranks a seat by its ADVERTISED `agent_ctx_tokens`, the only capability number nodes publish: reasoning takes the roomiest **adequate** seat, mechanical takes the **smallest adequate** seat so the roomier one stays free. *Adequate* is not a slogan — it is `adequate()`, `est_tokens + specReserve <= agent_ctx_tokens`, the same arithmetic the hard gate uses (they share the function, so they cannot drift). An unadvertised ceiling is never adequate: unknown is not a capacity, and a seat that published no number must not win the mechanical contest by looking like the smallest on the roster.
- **Fit chooses WITHIN a cycle, never a free re-pick.** This is the load-bearing constraint: a subtask takes the best-fitting seat *among those not yet dealt in the current cycle*, and a local slot reshuffles the deck. Without it the smallest seat wins every mechanical slot and the roomiest wins every reasoning slot — measured on a `{local, qube 131k, aorus 32k, lenovo 32k}` roster, an unconstrained re-pick put 8 mechanical subtasks on `local 2 / aorus 4 / lenovo 2 / qube 0` and 8 reasoning subtasks on `local 2 / qube 6 / aorus 0 / lenovo 0`, which is precisely the stacking `spread` exists to remove. With the cycle constraint both deal `2/2/2/2` — mechanical dispatching the small seats first, reasoning the roomiest first.
- **The deal is joint, and it has to be.** No per-subtask function of (index, own shape, roster) can hold the invariant: distinctness inside a cycle forces the slot-to-seat map to be a bijection for each shape, and distinctness inside a MIXED-shape cycle then forces the two shapes' bijections to be identical — i.e. forces the shape to have no effect at all. Fit scoring and one-per-seat therefore coexist only when the deal can see its siblings, so `dealSpread` computes every subtask's placement in one ordered pass before dispatch. That also keeps placement deterministic and free of shared mutable state (the goroutines read the deal, they never build it).
- **Ties keep the rotation** (the comparison is strict), so an all-equal roster deals exactly as it did before fit scoring existed.
- **The cycle is keyed on the DIAL BASE, not the node id (2026-09-17, S-12).** Every other exclusion in the delegator (the tried set, the re-placement exclusions, the refusal cooldown, the quarantine) keys on the base, and a `node_id` is neither unique nor guaranteed to be published. Two remotes advertising an empty id — or the same id, which register C-19 shows does drift — shared one entry, so the second remote of a cycle found its key already taken, the cycle was reshuffled, and the fit score handed the SAME seat both subtasks while the other one idled. The base is what the dispatcher actually dials, so it is the only key that can mean "this seat already has one".
- **The local rotation slot is never contested by shape; it is contested by load (0.113.20).** With the local seat idle, subtask 0 lands local whatever its shape — a single-subtask spread is the riskiest case for a shape heuristic, and one regex match must not send a whole run off-box. The same holds for every later local slot, because the fit score ranks by advertised ceiling and the local seat advertises none in a delegator run; scoring it would mean inventing a number for it. Widening the contest to the local slot is a small change once the local seat advertises a ceiling of its own.
- **The local slot under load (0.113.20, operator decision 2026-09-06).** The deal reads the local seat's in-flight count ONCE when it is computed — the same reader the drain uses (`internal/seatload`: vLLM `num_requests_running` + `waiting`, or a llama-server's processing `/slots`, through llama-swap, alias-aware) — and when the seat already holds a request, every local slot (`i mod len == 0`, subtask 0 included) goes to the best-fit eligible remote WITH ROOM (`hasRoom`; a sheddable run needs an idle slot) instead; the remotes' one-per-seat-per-cycle invariant is unchanged and the reason names the count (`…; local seat busy: 3 in flight`). With no remote that has room the slot stays local and the reason says `local seat busy … no remote with room`. An idle seat keeps every slot it had, so a lone session is dealt exactly as before; a text lease still removes the local seat in both modes. `agent_spread_local_slot: "always"` restores the unconditional local slot. Why: K delegating sessions each dealt 3 of every 8 subtasks to the same local seat while the remotes idled — the K×8 gate's remaining tail after the Lenovo seat replacement (first-local subtask 155–189 s under K=3 vs 91–105 s for its siblings; K=2 wall 1.55× K=1 against a 1.5× bound). A probe that fails deals as idle and logs why: the rule is an optimisation of the deal, never a gate.

**Retry on a different seat (0.80.0).** A subtask whose first attempt came back `failed_verification` (the acceptance DSL caught a wrong answer) or an honest `abstention` is re-run ONCE on a different node when one is available — local → the best eligible remote, remote → local — under a fresh job id. The published result is the BETTER attempt (a success beats any failure; otherwise the first attempt stands) and carries `retried_on` + `retry_note`; the summary carries `retried` / `retry_recovered`. Measured motivation: on the same four digest contracts the 27B seat and the 4B seat each missed a different one, and neither miss was silent thanks to acceptance — the retry is what turns "caught" into "recovered". Transport failures and infrastructure/config/contract defers are NOT retried: a broken box or a bad contract does not get better on another seat. The retry lives **inside the subtask's `timeout_sec`** — it gets whatever budget the first attempt left, and is skipped (the result carries a `retry_note` saying so) when less than the retry floor remains — 10 s by default, raised by the delegator's `agent_retry_min_sec` (0.115.9, register D-46: a cold vLLM load plus one turn at `max_tokens` on the retry seat; 300 on the reference box) — so `timeout_sec` stays the wall ceiling the caller was told it is. Two more skips, each named in `retry_note` (0.115.9): a first attempt that ended on an **empty final** (`stop_reason` `reasoning_starved` / `empty`, 0.115.8) is never retried — the shape is the seat's completion budget, not a wrong answer another seat corrects; and the retry never lands on a seat that is **already running another job** — a remote the delegator can prove would QUEUE the retry (`!provablyStartsNow`: no free worker, or a backlog ahead of it), or the local seat with requests in flight — which would only halve both runs' tok/s. The remote threshold was `jobs_running > 0` until 2026-09-17, i.e. zero rather than the node's own ceiling, so a four-worker box with one job in flight refused every cross-seat retry although three workers were idle; register D-46 shipped the rule and not the threshold. It is the same ceiling-aware predicate the placement gate ranks on, and it stays conservative — an unpublished ceiling still reads as busy. The re-placement floor after a REFUSED dispatch (no seat time spent) stays at 10 s.

**The retry never lands on a FENCED local seat (0.117.7, register D-94).** Before choosing the local
seat for a retry the delegator reads the machine-wide lease and asks `delegate.Fenced` — the
placement-side reading of `modelaffinity.BlocksNewRun`, which it calls rather than restates. Three
holds fence: an **exclusive** text lease (the holder cleared the cards), a **draining** text lease
(the seat is cordoned while in-flight work finishes) and a **media** lease (a render owns the VRAM).
When the seat is fenced the retry goes to the best eligible remote instead — `gate.Place` with the
local node out of contention, so an idle node beats one that would queue — and when nothing else is
eligible the subtask defers AT ONCE with a `retry_note` naming the fence and the holder. It is read
from the lease record, never discovered by dialling: on 2026-09-14 a retry was placed on the local
seat under an exclusive lease, waited the whole `gpu-lease timeout after 5m0s (bound 5m0s)` at the
affinity cordon and then deferred as capacity, while an idle remote sat unused for those five
minutes. A plain (non-exclusive, non-draining) text reservation is NOT a fence here: it already
removes the local seat from FIRST placement (`Reserved`, below), and the affinity gate admits the
load, so refusing a retry on it would refuse work the box can do.

FIRST placement already consults the lease and always did: `route=auto` defers to the capacity wait
when `Reserved(LocalLease(...))` holds (`TestRunAutoReservedLocalDefersNamingTheHolder`) and
`route=spread` drops the local seat out of the deal (`TestRunSpreadReservedLocalDealsRemotesOnly`).
A MEDIA lease is deliberately excluded there — renders are arbitrated at the model-affinity gate
(ADR 0026) — which is why it fences a retry but not a first placement: a retry runs on the leftovers
of `timeout_sec` and cannot afford to spend them queueing behind a render.

### Contract wire shape (`core.AgentContract`)

The dispatch envelope's `payload` for an agent job is one contract. The reader is **tolerant on
unknown fields** — nodes deploy staggered, and a strict decoder would make every additive field
a flag-day upgrade — while `schema_version` skew and the size/count caps are strict. Every
decode error is an ack-time 400 with the decoder's reason.

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | Must be `1`. Any other version is refused at decode — a mismatched peer defers loudly rather than half-understanding a contract. |
| `goal` | string | Required. The self-contained task; the sub-agent sees only this plus the context docs. |
| `context` | `[{name, text}]` | Inline documents: ≤ 16 docs, ≤ 256 KiB total (name+text bytes — a transport bound, not a context-fit promise). Each `name` must be a flat filename because it becomes a file under the job's context dir — see [Context doc names](#context-doc-names) for the exact rules. |
| `output_schema` | object | JSON Schema for the structured result. Must yield at least one grammar-compilable property — a `properties` map of string / number / integer / boolean / string-array / enum fields. **Required for remote execution**: without it the dispatch is refused at ack, because the delegator would have no mechanical check before merging. |
| `acceptance` | `[string]` | Machine-checkable checks, parsed at validation and **evaluated by the delegator**, never the node: `contains:<s>`, `not_contains:<s>`, `regex:<re>`, `min_items:<field>:<n>` (n ≥ 1), `nonempty:<field>`, and (0.122.0) `diff_touches:<path-prefix>` / `diff_max_files:<n>` over the write set. Unfalsifiable shapes (empty substrings, a zero minimum, an empty diff prefix, a negative file bound) are parse errors. Text verbs read `output`, falling back to the raw `structured` bytes when `output` is empty; field verbs require `structured` and fail closed without it. |
| `profile` | string | Agent task profile; empty = `research`. An unknown name defers loudly, naming the valid set. |
| `max_steps` | int | Loop step budget. Default 12, clamped to 12 — an over-ask is clamped, not rejected. |
| `setup_actions` | `[{tool, args}]` | Optional (0.113.24, ADR 0036 P2). ≤ 8 tool calls the node REPLAYS before the model's first turn, through the seat's env rules and dispatch, spending no step; validated at decode (bare tool name, args a JSON object ≤ 4 KiB) — a bad list is a 400 naming `setup_actions`. Whether the tool exists on this seat is answered as an observation. A node with `agent_seed_context_reads: true` prepends one `read_file` per context doc on its own; seeded reads plus the contract's actions are one list clamped to 8 per run (seeded first). A node one release behind IGNORES the field (unknown fields are kept for the mixed fleet) and reports no `setup_ran`. Since 0.115.12 a committed replay feeds the exact-repeat breaker (a model call repeating it byte for byte is refused with "you already have that result" — one copy of the document in the transcript, not two) and a replayed `read_file` that reached EOF ends with a `(complete file: N lines …)` footer instead of a continuation hint. |
| `timeout_sec` | int | Wall ceiling, enforced node-side as a context deadline over probe + build + loop + re-pack. Default 300, clamped to 900. Since 0.126.0 (register D-03) a caller who names none gets the default PLUS `timeout_auto`, and the node sizes the wall itself — see the next row. |
| `timeout_auto` | bool | 0.126.0 (register D-03). Stamped by intake when the caller named no `timeout_sec`: the executing node sizes the wall from its seat's measured rate (the same estimate published as `wall_estimate_sec`), clamped to 300..900, runs under it and reports it as `wall_sec`. A seat with no rate yet runs the default. Never set next to a caller's own `timeout_sec`; never set on a retry or re-placement (its wall is what is left). The delegator's **budget** (the retry remainder, the re-placement ledger) still holds the 900 s cap open, but since register D-116 its **poll clock** does not: it is sized from what the target node advertises (`seat_rate` + `seat_budget`) by the same arithmetic the node sizes its wall with, raised to the node's own `wall_sec` the moment a running poll publishes one, and only a node advertising no rate is polled to the cap — see [the poll deadline](#job-protocol-delegator--node). An older node ignores the field and runs the default. |
| `thinking` | string | Optional (0.115.8). The planner think-block policy on the executing seat: `auto` (empty; falls back to the node's `agent_thinking`, then auto) thinks every step and re-issues an EMPTY final once with thinking off at 4× the step budget; `off` renders every planner call in non-thinking mode (`chat_template_kwargs: {"enable_thinking": false}`, the re-pack's knob) — for grounded extraction on a thinking seat that spends its budget in the think block; `on` never sends the kwarg (a template that rejects it). Any other string is a 400 naming `thinking`. |
| `write_root` | string | Optional (0.122.0, register D-06). Opens the WRITE door: a directory RELATIVE to the run's read root (this node's materialized context dir) the seat may create and change files under. Must be relative and non-escaping — no `..`, no absolute or volume-qualified path, no `.git` segment, no reserved Windows device name, no trailing space or dot — validated on every platform, because the delegator and the node can be different operating systems. Refused unless this node's config says `agent_allow_write: true`: an ack-time 400 on the fleet path (so the delegator re-places), `defer_class: "write"` on the in-process local path. See [The write door](../FLEET-NODE.md#the-write-door-agent_allow_write-default-off). |
| `depth` | int | **Advisory on the wire**: the node derives `max(1, depth)` for anything that arrives over the fleet wire, so a wire claim of "origin" is never trusted. The delegator's placement gate separately requires the requester's depth to be 0 (hop limit 1). |

Context docs are materialized to a job-scoped dir under `pipeline-jobs/` (the same
sweep-at-startup discipline as pipeline jobs) and removed when the job ends.

### Result wire shape (`core.AgentWireResult`)

The **only** thing that crosses back — the remote transcript has no field to travel in, so
remote reasoning is quarantined from the caller's context by construction.

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | `1`. |
| `node_id` | string | Executing node (`fleet_node_id`, else the OS hostname). |
| `seat` | string | The resolved planner model that ran the loop. |
| `output` | string | The loop's final assistant text. Stays populated even when the structured re-pack failed, so the CALLER still receives the loop's answer. It is preserved for the caller, **not** for delegator-side acceptance — acceptance runs only when `deferred` is false, and every re-pack failure branch defers. |
| `structured` | object | Present iff `output_schema` was given AND the result validated: a final answer that already IS the requested object (0.115.12: trimmed to its outermost `{…}`, scalar-coerced) is taken as is with no re-pack call; otherwise, after the loop, one grammar-constrained completion on the same seat re-packs `output` into the schema, with one retry before deferring. Its completion budget scales with the answer it re-packs (0.115.10: `len(output)/3 + 512` tokens, floor 1,024, cap 8,192 — a fixed 1,024 turned every long answer into a JSON prefix filed as "invalid json"); a completion cut at `max_tokens` is named `re-pack truncated at N tokens` and the retry gets the cap. The re-pack call sends `chat_template_kwargs: {"enable_thinking": false}` — it is a mechanical shape transformation, not a reasoning step, and on a THINKING seat the grammar-constrained output otherwise lands in `reasoning_content` while `content` comes back empty, failing both attempts and discarding a finished answer. Harmless on non-thinking templates (measured identical output with and without). |
| `diff` / `diff_files` / `write_note` | string / [string] / string | 0.122.0 (register D-06). Present only on a contract that opened the WRITE door. `diff` is a unified patch of everything that changed under `write_root` during the run, `a/`+`b/` prefixed so `git apply -p1` takes it; `diff_files` lists the touched paths in the order the diff renders them; `write_note` says what the door did when there is no diff to read (this node has not opted in, a cap was broken, the seat wrote nothing). **The harness applies none of it** — reviewing and applying is the caller's job, and that is the whole reason a small seat can be given an implementation leg. The write set is taken from a before/after TREE SNAPSHOT of `write_root`, not from the tool-call ledger: the ledger says `write_file` ran, not what the bytes became, and a file the seat wrote without being asked to is invisible in it. Rendered before every defer branch, so a run that hit its step budget still publishes the partial change it made - including a run that stopped on `tool_call_cut`, where "the seat wrote nothing" is the honest reading: the engine refused the cut argument, so `write_file` never ran (register D-114). |
| `steps` | int | Steps consumed. |
| `stop_reason` | string | The loop's stop reason: `done`, `budget` (step budget), `error`, `unparsed_tool_call`, and since 0.115.8 `reasoning_starved` (the final completion spent its budget on hidden reasoning twice in a row — under `auto`, thinking on then off; under `on`, twice thinking on) or `empty` (the seat closed with nothing twice). Since 0.115.19 (register D-89) the LAST step of a multi-step run is a FORCED FINAL step — no tools offered, an answer-now turn, the final completion budget, thinking off unless the seat is pinned `on` — so `done` can carry `stop_note` "forced final answer …" (the seat answered when its step budget ran out), and `budget` now means it answered even that step with a tool call, which is never executed. The last two always arrive DEFERRED with an empty `output`; nothing re-packs them. Since 0.125.0 (register D-114) also `tool_call_cut`: the seat's TOOL-CALL ARGUMENT was cut by the completion budget - the engine refuses to parse the half-written JSON (llama.cpp: HTTP 500 "Failed to parse tool call arguments as JSON ... invalid string: missing closing quote"), or the completion arrives on `finish_reason: length` carrying a tool call whose arguments are a fragment. The loop re-issues that SAME step ONCE at the final budget; a second cut ends the run on this reason with an empty `output` and `defer_class: budget` - never `infrastructure`, which is how a ~3 KB single-call write at a 1,024-token step budget used to be filed against the box. |
| `stop_note` | string | 0.115.8: the one-line evidence behind a `reasoning_starved` / `empty` stop — finish reason, reasoning vs completion tokens, which wire key the seat used (`reasoning` on vLLM, `reasoning_content` on llama.cpp). Since 0.115.19 also on a forced final step: "forced final answer …" on a `done`, and the tool-call evidence on a `budget`. Since 0.125.0 also on `tool_call_cut`: both budgets and how far into the argument the engine got ("tool-call argument cut at the completion budget twice (step 1024 tok, re-issued at 4096 tok; partial argument 2847 chars) …"), which is the same line the defer `reason` carries. Absent on every other stop. |
| `output_truncated` | bool | 0.115.8: the final answer ended on `finish_reason: length` — a correct PARTIAL, still re-packed and still the caller's to use, but not the whole. |
| `response_shape` | string | 0.115.13 (register D-45): the seat's OBSERVED answer shape on this run — `reasoning_key=reasoning\|reasoning_content\|none reasoning_tokens=reported\|unreported tool_calls_parsed=N completions=N`. The pin says what the seat is; this says how it answered (the 2026-09-04 tool-parser mismatch and the 2026-09-10 reasoning-key blind spot were both invisible without it). Absent when no completion ran. |
| `seat_tok_s` | float | 0.115.21 (register D-03): this run's effective decode rate — completion tokens per second of call wall over the completions that generated ≥ 1,024 tokens (tool-call completions are prefill-dominated and excluded). 0/absent = no qualifying completion. Also the ledger row's `tok_per_s`. |
| `wall_sec` | int | 0.126.0 (register D-03): the wall the node sized this run to — a `timeout_auto` contract, the estimate below clamped to 300..900; `wall_note` is prefixed `auto wall (timeout_auto):`. Stamped before admission, so a defer at the cordon carries it too. Absent when the contract named its own `timeout_sec`, or when the seat had no rate yet and the wire default ran. |
| `wall_estimate_sec` | int | 0.115.21: the wall the node estimated the contract needed on this seat BEFORE the loop ran — cold load + (thinking auto ? one think block : 0) + (steps − 1) × (128 tok + 6 s prefill) + final budget, at the seat's remembered rate (`seat-rates.json`, else `agent_seat_tok_s`). Absent when no rate is known. Never changes the wall. |
| `min_turn_sec` | int | 0.115.21: cold load + one turn at the final budget — the least wall a retry is worth on this seat; since 0.117.2 it includes the re-pack term for a contract with an `output_schema`. The delegator's retry floor is `max(agent_retry_min_sec, the RETRY seat's min_turn)` — a remote node's health `seat_rate` at its `seat_budget`, the local seat's store — and only falls back to this first-attempt value when the retry seat published none (register D-46). |
| `wall_note` | string | 0.115.21: the estimate's arithmetic (`wall 600 s is BELOW the estimate 733 s for agent-pool: cold load 210 s + one think block 4096 tok (137 s) + 11 tool steps × (128 tok + 6 s prefill) (113 s) + final 8192 tok (273 s) at 30.0 tok/s (store, 5 samples); min_turn 484 s`), or why there is none (`no decode-rate sample for … yet`). |
| `repack_ms` / `repack_attempts` / `repack_note` | int / int / string | 0.115.23 (register D-91): how long the structured re-pack ran, how many seat completions it spent (two grammar attempts + the chat lane at most), and why it stopped or was skipped. A `length`-cut final answer (`output_truncated`) is never re-packed — the run abstains at once with `output failed schema: re-pack skipped …` and the partial in `output` — and no attempt starts with under a tenth of the wall (capped at 45 s) left (a 12 KB re-pack is a ~190 s re-generation on the 4B; three of them spent 690 s into a 900 s wall on 2026-09-10). |
| `calls` | `[{step, max_tokens, finish_reason, completion_tokens, reasoning_tokens, content_chars, reasoning_chars, tool_calls, thinking_off, reasoning_key, forced_final, ms}]` | 0.115.8 (register D-47): one entry per planner completion (0.115.19: `forced_final` marks the forced final step's call, D-89), on every result shape, set before the defer branches — the arithmetic a starvation diagnosis needs without transcript bytes. `reasoning_tokens` is vLLM's `usage.completion_tokens_details.reasoning_tokens` (0 = not reported). A pre-0.115.8 node emits none. Since 0.125.0 (register D-99) the DELEGATOR's published row `results[].calls` carries the LAST eight of these records (`omitempty`), so a caller reads them from `agent_delegate` / the CLI directly instead of from this endpoint with the fleet token. |
| `admission_wait_sec` | float | Everything spent BEFORE the wall started, as one number: the cordon wait, the llama-swap **swap pre-flight**, the seat's cold-load warm-up, the coherence probe and — since the S-24 fix — the served-window probe. All five draw on ONE budget (`agent_admission_wait_sec`, 0 = `core.AgentAdmissionSecDefault` = 300 s, −1 = off), so the ceiling is the budget and not the sum of five of them. Omitted when zero: nothing was swapping, the seat was already resident and the window read instantly. A job sits in state `running` for this whole window, which is why the delegator's poll bound carries a matching admission allowance. |
| `admission_note` | string | What admission DID or could not settle, `; `-joined across the steps that had something to say: `cold load Ns outside the wall`, `budget spent while <model>:<state> (proceeding into the wall)`, `running probe failed (proceeding): …`. Since the S-24 fix the warm-up also speaks from its **no-op** exits — `warm-up could not read /running (proceeding; the seat may still be cold)`, `no admission budget left for the warm-up …` — because "could not read" and "the seat is ready" used to be reported identically, and a still-cold seat reached the wall looking warm. Empty = every step settled cleanly. |
| `ctx_window_note` | string | WHICH window the loop budgeted against and WHERE it came from — the live probe, this box's `agent_ctx_tokens`, or the conservative 8,192 fallback (`agent.ResolveContextTokens`). Both doors discarded this line until the S-24 fix, so a run that silently compacted at 8,192 on a 131,072-token seat was indistinguishable on the wire from a correct one, and the only symptom was a task that compacted for no reason. When the fallback was caused by the probe running out of ADMISSION budget, the same sentence also appears in `admission_note`, because there the fix is a cold-start problem and not a window one. |
| `coherence_note` | string | Register D-118: the post-warm SEAT COHERENCE probe’s verdict — one ≤ 96-token completion, charged to `admission_wait_sec`, asking the freshly loaded seat to call `read_file` and answer DONE. `coherence probe: tool call parsed in Ns` (the seat is sane), `… answered in text without a tool call …` / `… inconclusive (…); proceeding` (fail-open), or `seat incoherent at warm: …`, which is also a `deferred` `infrastructure` result. Absent when the probe did not run (`agent_coherence_probe` `off`, a warm seat under the default `cold`, or a pre-D-118 node). |
| `deferred` | bool | True = the node ran and honestly could not complete the contract. **A defer is a success shape at the job level**: the job lands `done`, never `error` — `error` is reserved for internal wiring bugs (mirrors the cascade's defer semantics). |
| `reason` | string | Why it deferred (shapes below). |
| `defer_class` | string | The machine-branchable WHY: `abstention` \| `budget` \| `infrastructure` \| `config` \| `contract`. Additive and `omitempty` — a pre-0.65 node emits no class, and readers must treat empty as *unknown*, never as abstention. |
| `wall_ms` | int | Node-observed wall time. |
| `tokens_out` | int | Re-pack completion tokens, when a structured result was produced. |
| `harness_version` / `harness_build_sha256` | string | A1 config pinning (0.81.0), REQUEST side: the node's compiled-in version and the exact binary's self-SHA-256. Request construction (per-call temperature, the re-pack's `enable_thinking:false`, profile toolsets) is code, and a version string alone cannot pin code identity — two checkouts can both claim the same version while one carries uncommitted changes. Stamped only when the seat demonstrably served (the loop completed); absent on pre-loop defers and on pre-0.81 nodes. **Absent = unknown — refuse to pair**, never a value. |
| `seat_config_sha256` / `seat_config_basis` | string | A1 config pinning, SERVER side: a stable hash over a closed field set from the seat's live `/upstream/{model}/props` (build, weights path, quant, `n_ctx`, slots, server sampler defaults, `reasoning_format`, chat-template hash, modalities), plus a one-line human-readable basis for "what changed?". The probe runs post-loop against a resident seat and gives up in 3 s — it must never cold-start a model as a telemetry side effect — so an evicted seat honestly leaves the pin absent. The per-request `seed` default is deliberately excluded. |
| `trace` / `rules_fired` | array / int | The step trace (0.113.22, ADR 0036): per tool call `{step, tool, status, obs_chars, rule}` (+ `setup` on replayed calls, 0.113.24; + `note`, ≤ 160 bytes of what the model was told, on calls that did NOT commit, 0.113.26 — the rigger's evidence) — what ran, what became of it, how much the model read, which environment rule decided — and the count of rule hits. Set before the defer branches. Omitempty: a pre-0.113.22 node emits neither, and readers must treat absence as "no trace", never as "no calls". |
| `setup_ran` | int | 0.113.24: how many of the run's setup actions (the contract's `setup_actions` plus this node's seeded context reads) EXECUTED before the first model turn — committed or failed; a refused, unknown-tool or over-budget action is not counted and its step-0 `trace` entry (`setup: true`) says why. Absent = no replay happened, which on a node one release behind is the silence to read. |
| `prefill_steps` / `prefill_tokens` / `cache_tokens` / `prefill_ms` | int/float | The run's T2-B prefill accounting, previously node-ledger-only — a REMOTE run's prefill economics never reached the delegator's standing corpus. Budget-stopped runs carry them too (they burn the most steps). Zero/absent = not measured, never "zero prefill". |

### Defer shapes and their classes

Each reason is a distinct, stable string so a delegator (and the delegation ledger) can key on
it; the class is what code branches on, because a reason string is prose and an exit code is not.

| Reason shape | Class | Note |
|---|---|---|
| `no agent seat resolvable (agent_model and model both empty)` | `config` | |
| `agent seat "…" is not in the endpoint's served roster` | `config` | A *positive* roster miss. An unreachable or empty roster proceeds instead (logged), letting the loop's first call surface the real transport error. |
| unknown-profile message naming the valid profiles | `config` | The contract asked for a profile this build does not have. |
| `building agent: …` / `agent loop: …` | `infrastructure` | Build or planner failure — nothing was learned about the task. |
| `seat incoherent at warm: …` | `infrastructure` | Register D-118. The post-warm coherence probe asked the freshly loaded seat for one `read_file` call and got the NaN shape back: ≥ 20 identical non-whitespace bytes in a row, an unparsed tool-call marker with no parsed call, or nothing at all at the 96-token cap AND no hidden reasoning reported (a thinking seat cut inside its think block proceeds with a `cut inside the think block` note — the loop calls that same completion reasoning starvation, not a broken seat). It fires BEFORE the wall starts, so the contract spent seconds, and it is the ONE `infrastructure` defer the delegator retries on another node (`delegate.IncoherentSeatDefer`) — the fault is a property of that seat, and what the node's admission spent before it (the cold load that triggered the probe) is credited back to the subtask's `timeout_sec` ledger so the retry floor does not refuse the retry. A broken verdict is also remembered for 10 minutes per endpoint+seat, so the next WARM contract on the still-resident seat defers on it without spending a probe. Still a broken stack: an operator has to fix the box. |
| `wall timeout after <N>s` | `budget` | The `timeout_sec` deadline fired, in the loop **or** in the re-pack; its own shape so the delegator can size future contracts off it. |
| `step budget exhausted (<n> steps)` (since 0.115.19 `: <stop_note>` appended when the forced final step got a tool call) | `budget` | The loop burned `max_steps` with no final answer; `output` is empty, so there is nothing to re-pack. Since 0.115.19 the last step already asked for the answer with no tools offered, so this shape now means the seat answered even that step with a tool call (never executed; `stop_note` carries it). |
| `empty final answer after <n> steps and <t> completion tokens: <stop_note>` | `budget` (stop `reasoning_starved`) / `abstention` (stop `empty`) | 0.115.8. The loop's final completion carried no visible content on the first attempt AND on its one re-issue with thinking off at the final budget. `budget` when the completion budget went to the think block (vLLM `reasoning` / `reasoning_tokens ≥ 0.9 × completion`, or `finish_reason: length`), `abstention` when the seat had room and said nothing. Never re-packed: before 0.115.8 this shape reached the re-pack as `""`, came back as a schema-valid all-empty object, failed acceptance, and was retried cross-seat with the wall's leftovers (2026-09-10: 20,526 tokens on the Qube 27B for zero visible characters). |
| `output failed schema: …` | `abstention` | The re-pack reached the seat and the answer was unusable after its one retry — a validation failure, a **non-429 4xx** (the seat refusing *this* request: context length exceeded, an uncompilable grammar), or a 200 carrying zero choices. All three are the box answering; the fix is a smaller context or a flatter schema, not an operator. |
| `structured re-pack unreachable: …` | `infrastructure` | The re-pack could not REACH the seat, or the seat is not what answered: a dial/transport failure, a **5xx**, a **429** (llama-server does not rate-limit — a 429 means something in FRONT of it answered), or a **body that could not be read or parsed** (`llama-server response body unusable: …`). The last shape covers a proxy or captive portal returning HTML with a 200, and a connection dropped mid-body: both happen AFTER the request succeeds, so no `*url.Error` / `net.Error` exists to catch them, and all three used to be filed as abstentions at exit 0. Split from the schema shape deliberately — filed under `output failed schema:` a llama-swap outage reads as a model that cannot follow a schema. The flag is sticky across the retry: a 5xx followed by a wrong-shape retry stays `infrastructure`, because a transport failure that happened at all is the operator's signal. **This is the one broken-stack shape that carries a POPULATED `output`**: the agent loop already finished, so its prose is preserved (`agenttask.go` sets `wire.Output` before the re-pack, and every failure branch below keeps it, so the CALLER still receives the loop's answer — delegator-side acceptance does not read it, since acceptance runs only when `deferred` is false) while `structured` stays absent. It is still counted into `summary.lost_to_stack` — a contract with an `output_schema` asked for a mechanically checked deliverable, and unchecked prose is not one. |
| `this node does not open the write door: agent_allow_write is false …` | `write` | 0.122.0, register D-06. The contract asked for `write_root` and this node has not opted in. Its own class because it is neither a broken stack (nothing is wrong with the box) nor an unplaceable contract (another node may have opted in) — the delegator's right reflex is to re-place. On the FLEET path this never appears: `buildAgentRun` refuses at ack with a 400 so the re-placement happens there; the defer is the in-process local path, which has no ack hop. |
| `write door: … over the N-file cap` / `… over the N-byte cap` | `write` | The run finished and its write set is past a door cap. **No diff is published** — a truncated or partial patch applies as silent damage. The fix is a smaller leg, not an operator. |
| `canceled during the structured re-pack (the caller's context ended)` | `budget` | The PARENT context was canceled mid-re-pack (the delegator abandoned the poll, the node is shutting down). It arrives as a `*url.Error` exactly like a dial refusal, so it used to read as broken infrastructure — but nothing on the box failed. |

More shapes originate on the **delegator**, not the node:

- `queue deadline after <d>: the node accepted the job but never started it …` — a **FAILURE**
  (`summary.failed`), not a defer. The node admitted the job and every poll answered `accepted`:
  it sat in the backlog and was never given one of the node's concurrency slots. Introduced with
  the node-side queue in 0.100.0, because until then `accepted` lasted microseconds and this state
  could not persist. Two properties make it safe: **queued time is credited back** to the
  execution deadline (a job that waits and then runs is never penalised for the wait — the
  contract's `timeout_sec` is a budget for work), and the wait itself is **bounded** by
  `min(timeout_sec + grace, 5 minutes)`. It is a failure rather than a defer on purpose — a defer
  is a report about the work and carries the node's id and seat, and a job that never started has
  no such report to make.
- `poll deadline after <d>: node accepted the job but did not reach a terminal state` — class
  `budget`, or `infrastructure` when the last poll answer was unusable (a 5xx / unknown state,
  named in the reason) **or when the node LOST the job at least once** (each loss is named with
  its re-dispatch count: a node that forgets jobs is broken, not slow). Produced **only** when the
  node reported OWNING the job — a `200` whose state is `accepted`/`running`/`done`/`error`.
  Reachability is not ownership: a node that only ever answered `404` (a positive denial that it
  ever held the job) or `503` yields a FAILURE (`summary.failed`), not a defer, because a defer
  asserts the node took the work. An early poll error is RETIRED by a later healthy answer, so one
  503 followed by clean `running` answers ends `budget`, not `infrastructure`.
- The FAILURE that a poll deadline produces names what the node actually did, in three shapes:
  it never answered; it answered **with a 404**, quoted as the denial it is; or it answered and
  neither claimed nor denied the job (a 5xx, or a state this delegator does not know — a newer
  peer's `queued`). The third used to print the 404 sentence, so a node returning only 503s
  published a denial nobody ever made.
- `route=remote: no remote passed the capability gate (none is both agent-enabled and
  roster-resident)` — class `config`; with health-probe failures alongside it the probe errors are
  appended and the class becomes `infrastructure`.
- `route=remote: M of N agent-enabled remote(s) advertise no context ceiling (agent_ctx_tokens is
  unset or 0 on <node ids> …)` — class `config`. `agent_ctx_tokens` is `omitempty` on the wire and
  an operator sets it on the node, so a ceiling nobody advertised is a node verdict, never a
  statement about the caller's contract. The state is produced by **a node running the agent lane
  with `agent_ctx_tokens` unset** — the lane is admitted on `fleet_agent_enabled` + a resolvable
  planner seat + a safely reachable listener, never on a ceiling, and health publishes whatever is
  configured (0 included). It is *not* a peer predating the lane: that peer sends no
  `agent_enabled` either, so it is filtered out before any ceiling is considered. The test is
  **per lane, not fleet-wide**: ANY lane that advertised nothing produces this verdict, and the
  message names those nodes because the fix is on those boxes. A silent lane's ceiling is
  UNKNOWN, not small — it may be a 128k machine — so no ceiling claim may be made *about that
  lane*. (An earlier form keyed off the roomiest ceiling in the fleet, a MAX, so one node with a
  real ceiling supplied one on behalf of every peer that had published none, and that mixed fleet
  still got a quiet `contract` verdict quoting a number the silent node never sent.) When every
  lane that DID advertise a ceiling is too small for the contract, that second cause is appended
  to this reason — scoped to the advertised lanes ("every remote that DID advertise a ceiling tops
  out at N"), so both true causes reach the operator in one run and neither is a claim about the
  silent box.
- `route=remote: all N configured remote(s) failed the health probe: …` — class
  `infrastructure`; `route=remote: no remote fleet nodes are configured …` — class `config`.
- The **contract-side** gate rejections — no `output_schema`, a contract already past the origin
  hop, a token estimate no advertised ceiling can hold — class `contract`, **but only when the
  node side is positively established as fine**: every configured remote answered its health
  probe, at least one offers the agent lane, and EVERY such lane advertises a real ceiling for the
  contract to be too big for (one silent lane is enough to make the class loud). Otherwise
  the class is the loud one and the reason names both causes. The class was introduced in the
  round-3 review (`config` counts as a broken stack, so a caller's own contract mistake exited
  non-zero and accused a healthy node) and the round-4 review found the mirror defect: the
  contract check ran FIRST and short-circuited even a totally dead fleet, so a schemaless contract
  published `{succeeded:1, infrastructure:0}` at exit 0 over a fleet that had been unreachable for
  a week. Absence of evidence about the fleet is never evidence about the contract.

`infrastructure` and `config` defers are counted into the delegator's `summary.infrastructure`
and make `local-offload delegate` exit non-zero (`delegateExitErr` in `main.go`): a broken or
misconfigured node must not read as a successful run. `contract` deliberately is not.
`summary.infrastructure` also counts a **local placement taken while every configured remote was
failing its health probe** (`route=auto`, local GPU busy): the placement is right and the work
runs, but `route=remote` exited non-zero on that identical fleet state while `route=auto`
discarded it, so a fleet down for a week read green forever. That same case is why the MCP tool's
`isError` is NOT the exit code's rule — it fires on `summary.failed` plus `summary.lost_to_stack`
(the defers that LOST a subtask — it delivered no usable result because the contracted output
never arrived, published separately for exactly this reason) rather than on the whole of
`summary.infrastructure` — see [coding-agent](coding-agent.md#delegation-surfaces). The counted
set includes the `structured re-pack unreachable` shape, whose `output` is populated: what was
lost is the schema-checked deliverable, not the bytes.

### Auth (v1 scope: the agent lane — joined by the vision lane in 0.116.0)

`fleet_auth_token`, when set, bearer-gates exactly two lanes: agent dispatches (and the vision
lane's `POST /fleet/vision`, which rides the same rule through `tokenGated`), and
`/fleet/jobs/{id}` polls of jobs those dispatches created (the job record carries an agent
marker — or, for a vision job, the `Gated` marker — written atomically at creation and evicted
with the record). The comparison hashes both
sides with SHA-256 before a constant-time compare, making it length-independent. Wrong or
missing credential → `401` with the standard error envelope (`"error": "unauthorized"`),
checked immediately after the body decode and **before** the job_id validation and the re-ack
lookup, so an unauthorized caller can neither probe the field validators nor learn job
existence.

With **no token configured**, a non-loopback listener refuses agent dispatches outright —
`403 agent lane requires fleet_auth_token on a non-loopback listener` — and
`AgentLaneAdmissible` withholds `agent` from the advertised `supported_task_types` **and** from
the four `agent_*` health fields below, so neither a task-list-driven dispatcher nor a delegator
learns the capability just to eat a 403. Loopback with no token is the
local-MCP trust boundary and stays open. Every media path — media dispatch, media job polls, `/fleet/media/*`, health — ignores
the token entirely, so already-deployed tokenless media clients keep working byte-identically
(pinned by test); whole-fleet enforcement is a recorded follow-up
([ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)).

### Context doc names

A `context[].name` is a future FILENAME on the receiving node, and the delegator and the node
can be different operating systems — so the contract enforces the **strictest** platform's rules
everywhere, on both sides. Rejected at `Validate` (i.e. an ack-time `400`, before any file is
touched):

| Shape | Why |
|---|---|
| empty, or `.` / `..` | not a filename / a directory reference |
| contains `/`, `\`, `:`, or NUL | traversal, a Windows drive/ADS hazard, or a C-string truncation |
| a reserved Windows device name — `CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`, any case, with or without extensions (`nul.md.txt` counts; the stem before the first dot is what matters) | Windows resolves these in every directory: the write **succeeds** and the readback is EMPTY, so the doc vanishes with no error anywhere |
| a trailing space or trailing dot (`notes.md `, `notes.md.`) | Windows strips them, so the name silently becomes a different one |

Duplicates are rejected on a **normalized** key — trailing spaces/dots trimmed, case-folded —
so `notes.md`, `notes.md ` and `Notes.MD` cannot shadow one another. Raw string comparison let
those pairs through as "distinct", and the second write then overwrote the first with nothing
reporting it. `COM10`/`LPT10` and names that merely *start* like a device (`console.md`,
`nullify.go`) are ordinary files and stay legal.

### Health advertisement — four agent fields, fail-closed residency

All four are additive and `omitempty` (`schema_version` stays 1; a lane-off node emits a
byte-identical payload), populated only when `AgentLaneAdmissible` holds — i.e. opted in, a seat
resolves, AND the listener posture is one dispatch will accept:

| Field | Meaning |
|---|---|
| `agent_enabled` | The operator opted this node into the lane. |
| `agent_seat` | The resolved planner seat (`agent_model`, else the workhorse). |
| `agent_ctx_tokens` | The seat's serving ceiling, **from config** (`agent_ctx_tokens`) — never probed on the health cadence, because the live-window probe can cold-start a multi-GB model. `0` = omitted = "ceiling unknown", which the delegator's gate reads as never-fits. |
| `agent_seat_resident` | Roster-**verified**: a cached probe of llama-swap's `/v1/models` (alias-aware) saw the seat. The cache refreshes in the background at most once per 30 s; the handler never blocks on llama-swap. |
| `served_models` (0.113.0) | The same cached probe's full roster name list — **canonical ids AND every alias** (`swapclient.Roster.Names`, not `IDs` alone) — omitted/empty on a cold cache or a failed fetch (unknown, never a stale list). `internal/delegate/gate.go`'s `seatServed` uses this to check the roster actually names `agent_seat`, a stronger check than `agent_seat_resident` alone: a node can be roster-resident under one alias while its `served_models` list shows a different one after a rename. Publishing aliases too matters because an agent seat is normally bound BY alias (`agent-pool` -> `qwen3.8-27b-vllm`, `offload-e4b` -> `gemma-4-e4b`); an id-only list would have made a correctly-served alias seat read as unserved. A pre-0.113.0 node/delegator pairing is unaffected: an unpublished (empty/absent) `served_models` reads as UNKNOWN, never a refusal. |
| `tiers` / `layers` (0.123.2, composite only) | What this node IS (every tier it is a complete instance of) and what it can PLACE ON: one row per device layer with the declared seats, each seat's `served` flag from the same cached roster, and the node's own `admissible`/`reason` verdict from its guards. Absent on a plain node; built from cached reads only, so health still never probes. See [composite-tier.md](composite-tier.md). |

Residency **fails closed** twice over: until the first probe lands the answer is `false`, and a
probe *failure* publishes `false` rather than keeping the last good answer — advertising a seat
off a stale success while llama-swap is down would route agent work at a node that cannot run
it, whereas `false` only costs a conservative local placement. The failure is still cached for
a full TTL window, so a dead endpoint is probed once per window, not hammered per request.

### Health advertisement — GPU utilization and host CPU/RAM (0.113.0, fleet-overview)

Four more fields, added for [fleet-overview](fleet-overview.md)'s per-node cards and its placement
tie-break ([ADR 0034](../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md)):

| Field | Meaning |
|---|---|
| `gpu_util_pct` / `gpu_util_known` | The **busiest device's** utilization (PAIR's multi-GPU rule). **Always present, never `omitempty`** — unlike the agent fields above, `gpu_util_known: false` is itself meaningful (unknown, not idle), so the key must never simply vanish. Known only when at least one device's snapshot reports a valid reading; the `nvidia-smi`-derived snapshot populates it, the ADAPTER-level WDDM fallback path does not. |
| `host_cpu_pct` | Coarse host CPU busy %, from a background sampler (`internal/hostsample`) — two cumulative CPU-time readings a sample interval apart. `omitempty`. |
| `host_ram_used_gb` / `host_ram_total_gb` | Host RAM used/total, same sampler. `omitempty`. |

All three host fields are emitted together only when `internal/hostsample.Sampler.Load()` returns a
`Known` sample — there is no companion `*_known` flag for them, unlike `gpu_util_known`, because
`host_ram_total_gb` is never genuinely `0` when known and therefore serves as the group's own
presence signal: check it (or the key's mere presence) before trusting a `host_cpu_pct` of `0` as a
real reading rather than an absent field. The handler never samples itself — it only reads the
sampler's last cached value, the same pattern the VRAM snapshot already uses.

### `GET /fleet/jobs` — the cluster jobs feed (0.113.0)

`GET /fleet/jobs?limit=N` (default 50, capped at 500) returns `{"jobs": [...]}`, newest first,
**unauthenticated for id/task/model/state/`agent`/timestamps/`wall_ms`** — metadata only, never the
job's `payload` or its result, so there is nothing there the per-job bearer gate (`handleJob`,
`GET /fleet/jobs/{id}`) exists to protect. Its `error` string (truncated to 200 runes) is the one
exception: an `agent: true` row's error text can echo contract content (the goal, a tool-result
fragment) the way a media row's never does, so when `fleet_auth_token` is configured and the request
carries no valid bearer, `handleJobs` omits `error` on every agent row while leaving it — and every
other field on every row, media rows included — unchanged. It exists for
[fleet-overview](fleet-overview.md)'s cross-node JOBS feed and `fleet-ui`'s per-node job lists; a
node that predates 0.113.0 answers this route with a permanent 404, which the poller distinguishes
from a transient failure only by effect (it keeps the node's last-known job list either way) — see
fleet-overview.md's "A failed `/fleet/jobs` fetch is distinguished from an empty one".

### Job protocol (delegator ↔ node)

- The **delegator mints the job id**: `agd-` + 24 hex chars from `crypto/rand`, minted before
  placement so even a local run correlates its telemetry.
- Dispatch is the normal envelope (`job_id`, `task_type: "agent"`, `payload` = the contract);
  the ack is the standard `202`. A transport-level failure is retried **once with the same id**
  — if the first POST actually landed, the store's duplicate path re-acks `202` idempotently,
  so the retry can never buy a second run (the same 202-reack semantics the media lane has
  always had). A non-202 answer is a refusal, not doubt, and is never re-POSTed to the SAME node
  — it goes to the RE-PLACEMENT rule instead (0.101.0), which decides from the status whether
  another node is worth asking. The two are separate mechanisms with separate bounds:
  `dispatchAttempts` (2) is about transport doubt at one node, `maxRemoteReplacements` (2) is
  about finding a different node.
- The delegator polls `/fleet/jobs/{id}` every 3 s. A poll `404` is the lost-ack shape (the
  node never saw, or evicted, the job): re-dispatch the same id, bounded at 2 re-dispatches — a
  node that keeps forgetting the job is broken, and re-POSTing forever would re-run the
  contract on every node restart.
- The poll payload is `{job_id, state, data?, error?}` plus, since register D-116, **`wall_sec`**:
  the wall the RUNNING job is executing under, as the executing lane reported it
  (`core.ReportWall` → `Jobs.SetWall`). Additive and `omitempty` — a lane that reports none
  publishes the pre-D-116 payload, and a delegator too old to read it ignores the field. It
  exists because the node's sized wall used to reach the delegator only on the FINAL result,
  which is exactly the message a node that dies mid-run never sends. Never rewritten once the
  job is terminal: from there the result carries its own `wall_sec`.
  It is reported at the line that OPENS the wall context — **after** admission — so it means
  *the wall has started*, not *a wall was sized*: a job sits in state `running` for its whole
  admission window (the foreign-fence check, the cordon, the swap pre-flight, the cold load, the
  coherence probe and the served-window probe), and the delegator anchors its poll clock on the
  first `wall_sec` it sees. A run that defers during admission therefore publishes no `wall_sec`
  at all, which is correct — no wall ever ran.
- **Poll deadline** = the contract's `timeout_sec` + 60 s grace. Past it the delegator stops
  polling — the node may still finish server-side; the job id in the telemetry line lets an
  operator reconcile by hand. The outcome depends on whether the node ever ANSWERED about the
  job: a node that answered gets an honest `poll deadline …` defer (it acked and never reached
  a terminal state), while a node that never answered — dial refused, connection dropped,
  unparseable body — is a **failure**, because a delegator that manufactures a defer stamped
  with a silent node's id and seat is inventing a report nobody on that node ever made. Every
  poll failure is logged, and the last one is quoted in the reason.
- **An unsized (`timeout_auto`) contract is polled at the node's wall, not at the cap** (register
  D-116). `timeout_sec` on such a contract is only the wire default, and the node decides the real
  wall — so the delegator sizes its clock from that node's advertised `seat_rate` and `seat_budget`
  through the SAME function the node uses (`seatrate.AutoWallFor`; the node's entry point is
  `pipeline.AutoWallFor`, the delegator's is `delegate.autoPollBound`, and a test runs both on one
  contract so they cannot drift **while both are fed the same seat**), clamped to the same 300..900.
  Four bounds, and the deadline message names which one applied: `poll bound: sized from <node>'s
  seat_rate X tok/s (N samples): M s`, `poll bound: the node's own wall M s, from where the node
  started it` (a running poll published `wall_sec`; the node's number and its START are
  authoritative, so the clock is re-anchored there and can only ever be RAISED after), `poll bound:
  cap: no seat rate advertised by <node>`, or `poll bound: cap: the contract runs on <node>'s seat
  <x> and only <agent_seat>'s rate is advertised` — a COMPOSITE placement runs the dispatched
  layer's seat and the node sizes its wall from that seat's rate, which health does not publish, so
  the delegator refuses to size a clock from a seat the run will not use.
  Until a `wall_sec` is observed the bound also carries an **admission allowance** (300 s,
  `core.AgentAdmissionSecDefault`), named in the message as `+ Xs allowed for the node's admission
  before its wall starts`: the node's wall starts only after the cordon, the swap pre-flight, the
  seat's cold load, the coherence probe and the served-window probe, and all of that is spent in
  state `running`, earning no queued credit. Without it an auto contract landing on a cold seat was
  abandoned at the poll deadline while the node was still inside its own wall. The bound also rides the published result as
  `results[].poll_note`, on a green result as much as on a deadline. A contract that names its own
  `timeout_sec` is untouched: `timeout_sec` + grace, no note, the pre-D-116 wording exactly. So is
  the `queue` route, where the claimant is not chosen by the delegator and there is no health view
  to size from — it still polls at the cap.

## Admission: what a run pays before its wall starts

Everything below happens while the job reads `running` and before `core.ReportWall` opens the wall
context. One budget covers all of it (`agent_admission_wait_sec`), and every step reports into
`admission_wait_sec` / `admission_note`. Both agent doors — the fleet contract (`runAgentTask`) and
the MCP `agent_run` handler — run the same steps in the same order, which is the invariant the
`agentrun_admission` suite exists to hold: each drift between them was found in production.

1. **The foreign-fence check** (register S-26). The machine-wide GPU lease is read once,
   through the directory `config.Load` armed for the cordon, and `delegate.ForeignFence` asks whether
   it refuses THIS process's next run — an exclusive text hold, a draining cordon, or a media render
   held by somebody else. If it does, the run defers `capacity` immediately, naming the fence and the
   holder's line (class, pid, declared reason, expiry). Nothing the run can do inside its own budget
   releases another process's lease, so the verdict is on disk before the first poll; the doors used
   to poll that file for the whole budget and reach the same verdict 300 s later (47 rows, 3.92 h, in
   the three days to 2026-09-17) while the delegator sat on the re-placement path that verdict exists
   to trigger. An **inherited** lease (`GPU_LEASE_EPOCH`, i.e. `gpu reserve … -- <session>`) is not a
   fence, and a plain non-fencing reservation still waits at the cordon — ADR 0032's "a peer-held seat
   is waited for" governs every hold whose answer can still change.
2. **The cordon** (`modelaffinity.AwaitRunSlot`, register D-93): the same rule, waited out rather than
   refused, for a hold that arrives between the check above and this line.
3. **The swap pre-flight** (`awaitSeatAdmission`, ADR 0032). llama-swap queues — with no timeout of
   its own — any request that needs a model it is still loading, so a contract that dialled mid-swap
   spent its whole wall inside that queue. This polls `GET /running` while any model is non-`ready`.
   The seat's OWN row ends the wait at once, and since the S-08 fix that row is matched by **alias or
   canonical id**: `/running` names models canonically while the harness binds seats by alias
   (`agent-pool` → `qwen3.8-27b-vllm`), so the fast path was dead on every alias-bound box and a
   READY seat slept the whole budget whenever any other model happened to be mid-swap (406 rows,
   "/running lists the seat under another id"). The roster read that resolves the alias is lazy: it
   happens only when the bare name missed AND the alternative is a sleep.
4. **The cold-load warm-up** (`warmSeat`, register D-64): one GET through
   `/upstream/<seat>/v1/models`, which makes llama-swap swap the seat in and answers only once its
   health check passes. It also speaks from its no-op exits (see `admission_note`).
5. **The coherence probe** (register D-118), on a seat this run cold-loaded — which the warm-up
   reports explicitly, because a sub-tick load measures 0 s and a note is not the same fact.
6. **The served-window probe** (`agent.ProbeServedWindow`, register S-24). It carries a
   ten-minute cold-start budget by design — it is allowed to absorb a load — so running it on the
   WALL context handed a cold seat the contract's own clock and the run was filed as a wall timeout.
   It is now bounded by the admission deadline like everything above it; an exhausted budget leaves
   it a dead context and it falls back to `agent_ctx_tokens`, the same fallback an unanswerable probe
   has always taken — and `ctx_window_note` says so, on every run, so that fallback is never a silent
   one. The seat-residency (roster) check runs just ahead of it so a seat this endpoint does not serve
   is never cold-started by a run that is about to defer.

Every step above that PROBES reports its own failure rather than passing it off as a clean answer —
the alias roster read, the residency read, the warm-up, the window probe. That rule is the point of
the block: "the gate could not tell" and "there was nothing to tell" want different fixes, and a
budget silently spent on the first while the wire reports the second is how each of these defects
survived for months. The alias resolution in particular retries on the next poll inside the same
budget: latching "already tried" on a failed roster read disabled the alias match for the rest of the
wait after ONE transient error, which is S-08 again, intermittently.

## Source map

- [`internal/fleetnode/server.go`](../../internal/fleetnode/server.go) — routes, payloads, duplicate
  semantics, agent-lane auth gates, agent health advertisement
- [`internal/fleetnode/auth.go`](../../internal/fleetnode/auth.go) — the bearer credential check
- [`internal/fleetnode/jobs.go`](../../internal/fleetnode/jobs.go) — state machine, the admit-then-
  schedule queue and its concurrency limit, eviction, drain, the agent job marker
- [`internal/fleetnode/tasks.go`](../../internal/fleetnode/tasks.go) — `agentTaskConfigured`,
  `buildAgentRun` (contract decode, depth derivation, context materialization)
- [`internal/core/agentwire.go`](../../internal/core/agentwire.go) — contract, result, acceptance DSL
- [`internal/pipeline/agenttask.go`](../../internal/pipeline/agenttask.go) — node-side execution,
  structured re-pack, defer shapes
- [`internal/pipeline/coherence.go`](../../internal/pipeline/coherence.go) — the post-warm seat
  coherence probe (register D-118) both agent doors run
- [`internal/fleetnode/footprints.go`](../../internal/fleetnode/footprints.go) — padding, merge,
  persistence
- [`internal/fleetnode/vram.go`](../../internal/fleetnode/vram.go),
  [`vram_windows.go`](../../internal/fleetnode/vram_windows.go) — the two sampling paths
- [`internal/gpuprobe/`](../../internal/gpuprobe/) — the nvidia-smi command + per-device parser and
  the host free-RAM reader (leaf; fleetnode's `GPUDevice`/`ParseSmiMemoryDevices`/`HeadlineDevice`
  alias it)
- [`fleet_reclaim.go`](../../fleet_reclaim.go) — `oursLoaded` / `anyReclaimable`: the keep-set
  classification above, over `pkg/llamaswap`'s `Running()` + `IsProtected()`
- [`main.go`](../../main.go) — `fleet-serve` / `fleet-measure` verbs

## Related docs

- [../FLEET-NODE.md](../FLEET-NODE.md) — operator guide
- [../flows/fleet-job-lifecycle.md](../flows/fleet-job-lifecycle.md)
- [../architecture/decisions/0008-pdh-primary-vram-sampling.md](../architecture/decisions/0008-pdh-primary-vram-sampling.md)
- [fleet-overview.md](fleet-overview.md) — the delegator-side operator page that reads these health
  and jobs fields
- [../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md](../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md)
