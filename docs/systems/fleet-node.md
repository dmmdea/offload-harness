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

`fleet-serve` exposes three routes:

| Route | Purpose |
|---|---|
| `GET /fleet/health` | Node identity, GPU vendor and architecture, live total and free VRAM, supported task types, loadable model families, measured footprints, queue depth, and the node's job capacity (running / queued / both limits) |
| `POST /fleet/dispatch` | Submit a job; returns `202` with an ack |
| `GET /fleet/jobs/{id}` | Poll job state and result |

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
only its current depth.

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
memory.used`, one line per GPU — `nvidiaSmiMemoryDevices`/`fleetnode.ParseSmiMemoryDevices`) on
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
bind `cuda:0` to a different index). This is the fix for a real mis-report found live on a 2×16 GiB
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
- Assuming a `503 queue full` sheds the job to a sibling node. **It does not** — `internal/delegate`
  treats any non-`202` as terminal and does not re-place the subtask. That shedding behaviour is the
  media dispatcher's, in a different repository.
- Expecting a duplicate dispatch to return an error. Only `error` jobs do.
- Binding with `:18811` and expecting it to work as loopback.
- Treating Afterburner as required.

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
| `spread` (0.80.0, fit-scored 0.99.0) | one `Run` fetches every remote's health ONCE, then deals the subtasks across the local seat AND every remote that passes the hard gate for that subtask. The deal is computed for the WHOLE run in one pass before dispatch, and within each cycle of `len(nodes)` slots every eligible seat takes at most one subtask — so an N-contract fan-out genuinely runs on N seats at the same time, and the fit score can reorder a cycle but never collapse it (see "Fit-scored remote slots" below). The local rotation slot is never contested, so slot 0 is always the local seat (pinned by `TestDealSpreadKeepsSubtaskZeroLocal`, `TestDealSpreadSameShapedFanOutReachesEverySeat` and `TestRunSpreadDealsAcrossLocalAndEveryEligibleRemote`) and a 2-contract spread with an eligible remote is still guaranteed one local + one remote — the pair shape. Per-subtask eligibility means a contract failing the gate (no `output_schema`, over-size) silently takes the local slot instead; `results[].placement` names where each landed and, for a remote, which shape the fit score read. Measured before spread existed: `auto` put four concurrent contracts on one box, `remote` put four on the other one. No eligible remote → every subtask runs local and the reason says so. |
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
- **The local rotation slot is never contested.** Subtask 0 lands local whatever its shape — a single-subtask spread is the riskiest case for a shape heuristic, and one regex match must not send a whole run off-box. The same holds for every later local slot, because the fit score ranks by advertised ceiling and the local seat advertises none in a delegator run; scoring it would mean inventing a number for it. Widening the contest to the local slot is a small change once the local seat advertises a ceiling of its own.

**Retry on a different seat (0.80.0).** A subtask whose first attempt came back `failed_verification` (the acceptance DSL caught a wrong answer) or an honest `abstention` is re-run ONCE on a different node when one is available — local → the best eligible remote, remote → local — under a fresh job id. The published result is the BETTER attempt (a success beats any failure; otherwise the first attempt stands) and carries `retried_on` + `retry_note`; the summary carries `retried` / `retry_recovered`. Measured motivation: on the same four digest contracts the 27B seat and the 4B seat each missed a different one, and neither miss was silent thanks to acceptance — the retry is what turns "caught" into "recovered". Transport failures and infrastructure/config/contract defers are NOT retried: a broken box or a bad contract does not get better on another seat. The retry lives **inside the subtask's `timeout_sec`** — it gets whatever budget the first attempt left, and is skipped (the result carries a `retry_note` saying so) when fewer than 10 s remain — so `timeout_sec` stays the wall ceiling the caller was told it is.

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
| `acceptance` | `[string]` | Machine-checkable checks, parsed at validation and **evaluated by the delegator**, never the node: `contains:<s>`, `not_contains:<s>`, `regex:<re>`, `min_items:<field>:<n>` (n ≥ 1), `nonempty:<field>`. Unfalsifiable shapes (empty substrings, a zero minimum) are parse errors. Text verbs read `output`, falling back to the raw `structured` bytes when `output` is empty; field verbs require `structured` and fail closed without it. |
| `profile` | string | Agent task profile; empty = `research`. An unknown name defers loudly, naming the valid set. |
| `max_steps` | int | Loop step budget. Default 12, clamped to 12 — an over-ask is clamped, not rejected. |
| `timeout_sec` | int | Wall ceiling, enforced node-side as a context deadline over probe + build + loop + re-pack. Default 300, clamped to 900. |
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
| `structured` | object | Present iff `output_schema` was given AND the re-pack validated: after the loop, one grammar-constrained completion on the same seat re-packs `output` into the schema, with one retry before deferring. The re-pack call sends `chat_template_kwargs: {"enable_thinking": false}` — it is a mechanical shape transformation, not a reasoning step, and on a THINKING seat the grammar-constrained output otherwise lands in `reasoning_content` while `content` comes back empty, failing both attempts and discarding a finished answer. Harmless on non-thinking templates (measured identical output with and without). |
| `steps` | int | Steps consumed. |
| `stop_reason` | string | The loop's stop reason. |
| `deferred` | bool | True = the node ran and honestly could not complete the contract. **A defer is a success shape at the job level**: the job lands `done`, never `error` — `error` is reserved for internal wiring bugs (mirrors the cascade's defer semantics). |
| `reason` | string | Why it deferred (shapes below). |
| `defer_class` | string | The machine-branchable WHY: `abstention` \| `budget` \| `infrastructure` \| `config` \| `contract`. Additive and `omitempty` — a pre-0.65 node emits no class, and readers must treat empty as *unknown*, never as abstention. |
| `wall_ms` | int | Node-observed wall time. |
| `tokens_out` | int | Re-pack completion tokens, when a structured result was produced. |
| `harness_version` / `harness_build_sha256` | string | A1 config pinning (0.81.0), REQUEST side: the node's compiled-in version and the exact binary's self-SHA-256. Request construction (per-call temperature, the re-pack's `enable_thinking:false`, profile toolsets) is code, and a version string alone cannot pin code identity — two checkouts can both claim the same version while one carries uncommitted changes. Stamped only when the seat demonstrably served (the loop completed); absent on pre-loop defers and on pre-0.81 nodes. **Absent = unknown — refuse to pair**, never a value. |
| `seat_config_sha256` / `seat_config_basis` | string | A1 config pinning, SERVER side: a stable hash over a closed field set from the seat's live `/upstream/{model}/props` (build, weights path, quant, `n_ctx`, slots, server sampler defaults, `reasoning_format`, chat-template hash, modalities), plus a one-line human-readable basis for "what changed?". The probe runs post-loop against a resident seat and gives up in 3 s — it must never cold-start a model as a telemetry side effect — so an evicted seat honestly leaves the pin absent. The per-request `seed` default is deliberately excluded. |
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
| `wall timeout after <N>s` | `budget` | The `timeout_sec` deadline fired, in the loop **or** in the re-pack; its own shape so the delegator can size future contracts off it. |
| `step budget exhausted (<n> steps)` | `budget` | The loop burned `max_steps` with no final answer; `output` is empty, so there is nothing to re-pack. |
| `output failed schema: …` | `abstention` | The re-pack reached the seat and the answer was unusable after its one retry — a validation failure, a **non-429 4xx** (the seat refusing *this* request: context length exceeded, an uncompilable grammar), or a 200 carrying zero choices. All three are the box answering; the fix is a smaller context or a flatter schema, not an operator. |
| `structured re-pack unreachable: …` | `infrastructure` | The re-pack could not REACH the seat, or the seat is not what answered: a dial/transport failure, a **5xx**, a **429** (llama-server does not rate-limit — a 429 means something in FRONT of it answered), or a **body that could not be read or parsed** (`llama-server response body unusable: …`). The last shape covers a proxy or captive portal returning HTML with a 200, and a connection dropped mid-body: both happen AFTER the request succeeds, so no `*url.Error` / `net.Error` exists to catch them, and all three used to be filed as abstentions at exit 0. Split from the schema shape deliberately — filed under `output failed schema:` a llama-swap outage reads as a model that cannot follow a schema. The flag is sticky across the retry: a 5xx followed by a wrong-shape retry stays `infrastructure`, because a transport failure that happened at all is the operator's signal. **This is the one broken-stack shape that carries a POPULATED `output`**: the agent loop already finished, so its prose is preserved (`agenttask.go` sets `wire.Output` before the re-pack, and every failure branch below keeps it, so the CALLER still receives the loop's answer — delegator-side acceptance does not read it, since acceptance runs only when `deferred` is false) while `structured` stays absent. It is still counted into `summary.lost_to_stack` — a contract with an `output_schema` asked for a mechanically checked deliverable, and unchecked prose is not one. |
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

### Auth (v1 scope: the agent lane only)

`fleet_auth_token`, when set, bearer-gates exactly two things: agent dispatches, and
`/fleet/jobs/{id}` polls of jobs an agent dispatch created (the job record carries an agent
marker, written atomically at creation and evicted with the record). The comparison hashes both
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

Residency **fails closed** twice over: until the first probe lands the answer is `false`, and a
probe *failure* publishes `false` rather than keeping the last good answer — advertising a seat
off a stale success while llama-swap is down would route agent work at a node that cannot run
it, whereas `false` only costs a conservative local placement. The failure is still cached for
a full TTL window, so a dead endpoint is probed once per window, not hammered per request.

### Job protocol (delegator ↔ node)

- The **delegator mints the job id**: `agd-` + 24 hex chars from `crypto/rand`, minted before
  placement so even a local run correlates its telemetry.
- Dispatch is the normal envelope (`job_id`, `task_type: "agent"`, `payload` = the contract);
  the ack is the standard `202`. A transport-level failure is retried **once with the same id**
  — if the first POST actually landed, the store's duplicate path re-acks `202` idempotently,
  so the retry can never buy a second run (the same 202-reack semantics the media lane has
  always had). A non-202 answer is a refusal, not doubt, and is surfaced without retry.
- The delegator polls `/fleet/jobs/{id}` every 3 s. A poll `404` is the lost-ack shape (the
  node never saw, or evicted, the job): re-dispatch the same id, bounded at 2 re-dispatches — a
  node that keeps forgetting the job is broken, and re-POSTing forever would re-run the
  contract on every node restart.
- **Poll deadline** = the contract's `timeout_sec` + 60 s grace. Past it the delegator stops
  polling — the node may still finish server-side; the job id in the telemetry line lets an
  operator reconcile by hand. The outcome depends on whether the node ever ANSWERED about the
  job: a node that answered gets an honest `poll deadline …` defer (it acked and never reached
  a terminal state), while a node that never answered — dial refused, connection dropped,
  unparseable body — is a **failure**, because a delegator that manufactures a defer stamped
  with a silent node's id and seat is inventing a report nobody on that node ever made. Every
  poll failure is logged, and the last one is quoted in the reason.

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
- [`internal/fleetnode/footprints.go`](../../internal/fleetnode/footprints.go) — padding, merge,
  persistence
- [`internal/fleetnode/vram.go`](../../internal/fleetnode/vram.go),
  [`vram_windows.go`](../../internal/fleetnode/vram_windows.go) — the two sampling paths
- [`fleet_reclaim.go`](../../fleet_reclaim.go) — `oursLoaded` / `anyReclaimable`: the keep-set
  classification above, over `pkg/llamaswap`'s `Running()` + `IsProtected()`
- [`main.go`](../../main.go) — `fleet-serve` / `fleet-measure` verbs

## Related docs

- [../FLEET-NODE.md](../FLEET-NODE.md) — operator guide
- [../flows/fleet-job-lifecycle.md](../flows/fleet-job-lifecycle.md)
- [../architecture/decisions/0008-pdh-primary-vram-sampling.md](../architecture/decisions/0008-pdh-primary-vram-sampling.md)
