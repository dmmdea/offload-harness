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

**An `accepted` job can also leave the queue without ever running (ADR 0064).** Two terminal `error`
records say so, and both mean "nothing ran, so re-placing the work cannot double-run it":

- `withdrawn: ...` — the job's delegator asked for it back. `DELETE /fleet/jobs/{id}` (agent bearer, like
  the poll) takes back a job that is still `accepted`, under the same mutex the scheduler claims under,
  so a claim and a withdraw cannot both succeed. A running or finished job is never touched (`409` with
  its state); a repeat answers `200` again, as does a request for a job the node already took back itself
  (reaped, or marked never-started at shutdown); a media job is `405`; a node without the route answers
  `405`/`404`, which a delegator reads as "no withdraw here".
- `reaped: ...` — nobody polled the job for `fleet_poll_lease_sec` (default 60 s; negative = off; a
  value under 15 is raised to 15). Only an `accepted` agent job a delegator PUSHED is ever reaped: the
  scheduler's claim scan skips it at once and a ticker takes it, so a ghost never starts in the gap
  between ticks. A running job, a job the pull queue claimed, and media and vision jobs are never
  reaped. Authorized polls, authorized duplicate dispatches and parked long polls keep a job alive; the
  unauthenticated jobs feed, a poll that failed the bearer gate and a duplicate dispatch without the
  bearer (`401`, whatever `task_type` it declares) do not.

The node counts both routes since the process started (`jobs_withdrawn`, `jobs_reaped`; see the health
table), logs one line per withdraw and one per reap pass, and `fleet-serve` prints the lease in force at
start-up. A drain's `not started: ...` marks are a third route to a job that never ran and are counted in
neither.

A run that finishes more than a lease after its poller last looked is an abandoned run, and its wall no
longer feeds `recent_agent_wall_sec` (or the Retry-After built from it). Why all of this exists: every
delegator give-up used to leave the job on the node to run for nobody, which took 43 % and 59 % of two
nodes' agent runs on 2026-09-29. See [ADR 0064](../architecture/decisions/0064-a-delegator-takes-back-what-it-has-not-started.md)
and [Taking a job back](../FLEET-NODE.md#taking-a-job-back-withdraw-and-the-poll-lease).

**`accepted` is a real waiting state (0.100.0).** Accepting a job used to start it, so `accepted`
lasted microseconds and the node had no queue at all — just an unbounded pile of concurrent
executions that one config key happened to cap. A dispatch is now *admitted* to a FIFO, and a single
scheduler goroutine claims jobs only while an execution slot is free. Two independent limits fall
out of that:

- **`fleet_max_queue_depth`** (default 2x `fleet_max_concurrent_jobs`, i.e. 8 with the default 4
  workers — register S-04/C-25) — the admission ceiling on `accepted` + `running`, i.e. on
  `queue_depth`. Exceeding it is the only thing that produces `503 queue full`. The refusal boundary
  itself is unchanged in meaning from 0.99.0; only the default resolution changed, from a flat 32
  regardless of worker count to a multiple of the concurrency the node actually has. A node admitting
  32 deep behind 4 workers could pile up 28 jobs with no hope of starting inside any wall a caller
  would wait out — 236 measured contracts died at the delegator's 5-minute queue deadline having
  never started, 75% of them while another node sat idle.
- **`fleet_max_concurrent_jobs`** (default 4) — how many admitted jobs execute at once. Exceeding it
  never refuses anything; the job waits in `accepted`. This is the limit that protects the single
  llama-swap endpoint and the GPU behind it.

Both read `0` as "use the built-in default" and a negative value as "unlimited". A **busy node is not
a full node** — that distinction is the entire point of the split.

**A `queue full` 503 carries `Retry-After` (register S-04).** The refusal is a wait the node
PUBLISHES. The delegator reads it as a per-node cooldown that only its capacity wait consumes: the
503 itself returns at once and re-places the subtask on a node with room, and nothing ever sleeps on
the node that refused (ADR 0063). It is never a dead end: `Retry-After` (seconds) = `ceil(excess x recent_agent_wall_sec / max(1,
max_concurrent_jobs))`, where `excess = capped_backlog - max_concurrent_jobs` — **the CAPPED
backlog only** (`Jobs.CountsCapped`, the same set `max_concurrent_jobs` actually bounds), never the
wire's all-task-types `queue_depth`. An uncapped job (a render, an stt, a pipeline route) never
waits behind `max_concurrent_jobs`, so dividing the all-jobs depth by it would inflate the estimate
for a node whose agent slots are genuinely idle behind unrelated media load — the same class of bug
`saturation.score` was already fixed for (S-17) and this release's own `IdleSlot` fix (S-20)
repeats the lesson of.

The header is bounded to `[5, 300]` — never "retry at once" (the queue IS full) and never an
unbounded promise — but the CLAMP IS SAID OUT LOUD rather than disguised as a precise number, so the
three cases read differently:

- No `recent_agent_wall_sec` sample (a fresh node, or one that has never finished an agent job):
  the flat `30`, worded as a default — `(no recent completions yet — retry in 30 s)` — never as a
  measurement.
- A genuine estimate inside `[5, 300]`: `(~N s until a worker frees, from recent completions)`.
- An estimate that would exceed 300s: the header still caps at 300, but the text says
  `(>=300 s until a worker frees, from recent completions)` rather than presenting the cap as if it
  were the precise answer — an undisguised clamp invites every waiter to retry in lockstep at the
  same instant.

The message's existing prefix (`queue full (…): retry later, or raise fleet_max_queue_depth`) is
unchanged, byte for byte — the delegator quotes it — with one of the three clauses above appended.
This is deliberately **not** sized from `seat_rate.min_turn_sec`: that number is a max-final RETRY
floor for one seat and has no relationship to how deep this node's backlog is.

`/fleet/health` publishes **`queue_wait_estimate_sec`** (float, omitted when a worker is free or no
wall sample exists) — the node's own number, so the delegator reads it before ever being refused,
not only after: it ranks seats (W-11), derives the queue budget of a job sent to the node, and gates
the deal and the capacity wait on it (ADR 0063, "The queue budget and the backlog gate" below). It is
the SAME `excess x recent_agent_wall_sec / max(1, max_concurrent_jobs)`
formula and the SAME capped-backlog-only `excess`, computed off the node's CURRENT capped backlog —
but it is **RAW, not clamped to `[5, 300]`**: Retry-After is an HTTP retry contract that must stay a
small, boundable promise, while the health field is a delegator's own placement signal, where a
genuine 600s estimate is more useful reported honestly than floored to 300 or hidden. Reading the
two fields together: **absent `queue_wait_estimate_sec` with a present `recent_agent_wall_sec`
means genuinely 0** (a worker is free right now); **both absent means unknown** (no wall sample
exists yet to estimate from, not that the wait is zero). A job still in its ADMISSION phase
(`jobs_admitting`) counts toward this estimate exactly like any other running job: the worker slot
is genuinely taken, even though `saturation.score` excludes admitting jobs (no card is busy yet).

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
> healthy node can legitimately sit near its `max_queue_depth` (7 of 8 at the default). Placement is unaffected — lower is still better, and the
> delegator's tie-break compares like with like across nodes — but an operator reading it cold will
> misjudge it. Read `jobs_running` / `jobs_queued` beside it.

Dequeue and `accepted` → `running` happen in one critical section, which is what makes the drain
distinction below trustworthy: a job still `accepted` when shutdown begins provably never started.

**Duplicate dispatch is idempotent, with one deliberate exception.** Re-dispatching a job id that is
`accepted`, `running`, or `done` re-acks `202` and does **not** start a second run. A job in `error`
returns `409`. For an agent or vision job on a node with a `fleet_auth_token`, all of it is answered only to
a caller carrying the bearer, whatever `task_type` the duplicate declares (`401` otherwise): the job's own
record decides.

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
Dedicated+Shared usage) — a global sampler polling every two seconds either way. On Linux the
generic source is the amdgpu sysfs probe ([ADR 0053](../architecture/decisions/0053-linux-amdgpu-gpu-memory-provider.md)),
or, for a unified-memory SoC tier with no VRAM counter (`rockchip-rk3588`), `/proc/meminfo`
less the operator's `uma_reserve_gib` (`fleetnode.MeminfoUMAProbe`: capacity `MemTotal − reserve`,
free `MemAvailable − reserve` clamped to `[0, capacity]`). There is no
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
[ADR 0005](../architecture/decisions/0005-loopback-only-serve.md). Since 0.144.1 the flag permits one
specific address only: an all-interfaces address (empty host, `0.0.0.0`, `[::]`) and an address that
does not parse are refused with or without it (`netguard.AllInterfaces`, shared by fleet-serve,
local-agent and fleet-ui). The Linux unit's `Restart=on-failure` + `RestartSec=15` then covers the
boot race where `tailscale ip -4` prints nothing yet: the start fails and retries until the tailnet
address exists, instead of binding every interface.

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
   is the shared `admit` path. A seat whose runtime cannot do one of the three tasks narrows the lane
   with the config key `vision_tasks` (0.153.0, written by the tier's media seat: the RK3588 NPU seat
   serves `vqa` and `ocr`, never `assess_image`): the node refuses any other task at ack time with a
   `400` naming the allowed set, publishes the list in health as `vision_tasks` (additive, omitempty,
   lane-gated like `vision_model`), and a delegator skips the node for a task it does not list —
   absent means all three, so a node that predates the field is unchanged. Details:
   [FLEET-NODE.md](../FLEET-NODE.md#the-vision-task-post-fleetvision),
   [ADR 0040](../architecture/decisions/0040-vision-work-travels-to-a-node-with-an-idle-card.md),
   [ADR 0062](../architecture/decisions/0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md).
10. The text lane (`POST /fleet/text`, 0.154.0) runs ONE classify or extract on this node's own pipeline and
    ships DARK. `text` is in `supported_task_types` and `text_tasks` in health exactly when
    `TextLaneAdmissible` holds: the tier's media seat declared at least one text task (config `text_tasks`,
    written by the seat, never by `config_seed`; no shipped tier declares any) and the listener is safely
    reachable (the vision lane's rule). It rides the same bearer gate on dispatch and poll, takes dispatch's
    1 MiB body, goes through the shared `admit` path, stores the node's FULL `core.Result` (a defer is a
    `done` job saying `deferred: true`) and is concurrency-capped. The node refuses a task outside
    `text_tasks` at ack time with a `400` naming the set, and always refuses summarize and triage. A delegator
    places it only on a node whose health lists `text` and the task (`delegate.PlaceText`); a node that
    predates the lane is never picked. Details:
    [FLEET-NODE.md](../FLEET-NODE.md#the-text-task-post-fleettext), [ADR 0069](../architecture/decisions/0069-an-unconstrained-seat-runs-classify-and-extract-from-the-prompt-and-the-text-lane-ships-dark.md).
11. The cascade chat lane (`POST /fleet/chat`, register C-41b) is advertised — `chat_lane` in
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
  The refusal returns at once (ADR 0063): nothing sleeps on the node that refused, its `Retry-After`
  only cools that node for the capacity wait, and the fleet is read again before every re-placement.
- Assuming re-placement makes a saturated fleet free. Every placement spends from the contract's own
  `timeout_sec`, and when nobody takes the job the subtask fails with `placement refused: …`.
- Expecting a duplicate dispatch to return an error. Only `error` jobs do.
- Binding with `:18811` and expecting it to work as loopback.
- Treating Afterburner as required.
- Sizing a queue wait, a Retry-After, or any admission refusal from
  `seat_rate.min_turn_sec`. That number is a max-final RETRY floor for one seat, not a measure of
  backlog depth — use `recent_agent_wall_sec` / `queue_wait_estimate_sec` instead (register S-04).
- Assuming any queued job blocks `Jobs.IdleSlot()` (health's `saturation.idle_slot`, the shed rule
  for `priority: -1` dispatches). Only a CAPPED queued job does (register S-20) — an uncapped one
  (a render, an stt, a pipeline route) never contends for a capped execution slot, mirroring
  `claimLocked`'s own skip.
- Computing `Retry-After` / `queue_wait_estimate_sec` from the wire's `queue_depth` (all task
  types). Both are sized from the CAPPED backlog only (`Jobs.CountsCapped`) — the same distinction
  `IdleSlot` makes above — or a pile of unrelated uncapped media/stt/pipeline work inflates the
  estimate for a node whose agent slots are genuinely idle.
- Reading `queue_wait_estimate_sec` as bounded the same way `Retry-After` is. It is not: the health
  field is the RAW estimate (useful past 300s to a delegator making its own routing decision), and
  only the HTTP header is clamped to `[5, 300]`.

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

**Overdue (GPU routing P1).** The lease block also carries `overdue`, true only while the lease is HELD and its declared window has already
ended (omitted otherwise, so an unchanged lease publishes the same bytes). A held lease means its holder is alive and heartbeating, and a
`--for` window is not a ceiling for a wrapper holder, so a lease can outlive what it asked for. Before this the remaining time was clamped to
zero first, which is below every threshold, so such a lease read `busy: false` and the node looked free for exactly the lease that was
demonstrably still using the cards. The rule is now `busy = held AND (remaining > fleet_busy_lease_sec OR overdue)`: the short-lease rule
(a render under the threshold never makes the node a non-target) is the first clause and is unchanged; a negative `fleet_busy_lease_sec` still
turns the whole duration rule off, so an operator who disabled it does not get overdue-busy back, though `overdue` itself is still published
because it is a fact, not a verdict. `remaining_sec` is omitted for an overdue lease (its zero is `omitempty`). The delegator decodes it as
`NodeView.LeaseOverdue`, **not** as `LeaseBusy`: a busy non-text lease is a hard exclusion, and an abandoned lease must rank the node last
without making it unroutable for longer than it was before. `betterRemote`'s lease key now has three rungs (clean, long lease, overdue), and
the node-side gate still queues whatever arrives. The overdue busy is **not** folded into the node's own refusing flag (the one behind
`saturation.high` and `idle_slot`): a media lease does not make dispatch turn work away, so an overdue node keeps advertising the room it has,
and the delegator's capacity wait (`hasRoom`) still asks it when it is the only remote; only a long lease inside its window and a text lease
set `saturation.high`, as before. A lease record with no declared end (a missing, zero or negative `expires_at_ms`, which a hand-edited, foreign or
truncated record can carry) reads as an unset end in `gpulease.Info`, so it is never overdue against 1970. A delegator one release behind reads the overdue lease's `busy: true` as it reads any busy
lease; that is the intended fleet-wide meaning of "the card is not free". The fleet deploy never reads this verdict: its wait-idle step takes
job counts from health and the lease from the lease directory (`gpulease` Inspect), pinned by `healthwire_compat_test.go`.

**Free cards (GPU routing P1).** The delegator decodes the node's `gpu_devices[]` (it dropped it before) and ranks nodes on per-card truth
instead of the one utilisation scalar. A card is **free** when it is not a display card, its utilisation is known and under 15 % (the line
`gpu status` draws for "the cards are busy under the lease"; an unknown is never idle) and it has the VRAM, which is the placement seat's
published footprint split across the cards it spans, or a quarter of the card when no footprint is published. A layer whose seat spans N cards
needs N free ones. A warm seat fills its own card, so a seat the node says is loaded (health `seat_loaded`, or the layer row's `loaded`) vouches
for the VRAM of as many idle cards as it spans: a node serving from a resident seat does not lose to a cold node for being warm. It vouches for
VRAM only, never for utilisation, and a node that does not say whether its seat is loaded gets no credit. Nothing matches a seat pin to a card by index (a CUDA index and nvidia-smi's PCI order can differ), so every card is a
candidate. `betterRemote` gains a per-node tier after the ETA key: a node with a free card beats one whose cards are all busy, and a node that
published no per-card truth sits between them, so an older node is neither credited nor blamed. `route=auto/remote` spends a node's free cards
as it deals subtasks (a node with two free cards is preferred for its first two, not its third) and `route=spread` uses the tier as the first
key inside a deal cycle, so the one-subtask-per-seat-per-cycle invariant is untouched. This is ranking, never a gate: `max_concurrent_jobs`
stays the hard ceiling, a node with no free card still takes work (it queues on the node), and `offload_status` shows `free_cards` and
`cards_total` per node. Which cards a LEASE holds, and what that means for a contract, is the next paragraph.

**Per-card lease truth (GPU routing P7).** With card-scoped leases several leases are live at once, each on its own cards, and the one `lease`
block described only the lowest epoch. A render on one card of three therefore read as the whole node spoken for, and a delegator routed nothing
to the other two. `/fleet/health` now carries `leases[]` beside the block, one entry per live lease, omitted when none is held:
`{"epoch","class","devices":[lower-cased GPU UUIDs],"scope":"declared|inferred|whole-node","until","remaining_sec","busy","overdue","exclusive","draining","orphaned","stalled","verdict"}`.
`devices` ABSENT means the whole node and every reader must take it as every card; `scope` says where the cards came from (an inferred scope is the
evidence rule's reading of a lease an older binary wrote, off unless `gpu_legacy_scope_inference` is on); `verdict` is the most escalated of
`held-stalled`, `held-orphaned`, `held-overdue`, else `held`, where `held` is the absence of news and never a claim that the holder is working. Each
seat row of `layers[]` gains `device_ids`, the lease ids of the cards its pin names, resolved on the node against its own card table, so a reader
never guesses whether a bare `"0"` is a CUDA index or a PCI one; a pin the node cannot place publishes none, which reads as every card.

*Old readers.* The singular `lease` block, `lease_exclusive` and `lease_draining` stay, and are now the WORST across the live leases (class `text` if
any lease is, `busy`, `orphaned`, `stalled` if any is, the longest `remaining_sec` and `until`, exclusive/draining if any is); `pid` and
`reason` stay the lowest epoch's. `overdue` is the one field that folds the other way: every reader of the singular fields (a delegator one release
behind, and in the current binary the text and vision remotes, the MCP door's fleet view and `leasedLanes`) computes `LeaseBusy = busy AND NOT overdue`,
so the block says `overdue` only when no live lease is a long hold of its own (busy and not overdue). "Overdue if ANY lease is" would let an abandoned
lease on one card hide a live long render on another: the block would read busy-and-overdue, which every one of those readers takes as not busy, ranked
last, never fenced. Beside a live long lease the block is therefore busy and not overdue, which is what that lease alone says; with every busy lease
abandoned it is busy and overdue, as for one. A delegator one release behind reads only those, so it is never told less than is true: with a short media
render on one card and an exclusive reservation on another it sees a text, busy, exclusive hold. With ONE lease the block is byte for byte what it was.
A new delegator reading a node that publishes no `leases[]` (an older node) takes that block as the whole node, which is today's rule.

*The node's own gates follow the cards.* `saturation.high` and `idle_slot` close the node only when the leases that refuse new work (a text
reservation, or a lease long enough to be `busy` and not overdue) are whole-node or together hold every card the node publishes; a node that
cannot enumerate its cards cannot show one is left and stays closed. `/fleet/dispatch` refuses a TEXT reservation per contract: an agent
contract is turned away only when every seat it could run on (`placement.AgentChain`: the home seat, then each layer's agent seat in turn)
sits on a card some text reservation holds, because the placement table falls back to a seat whose cards are free; every text lease counts, not
only the lowest epoch. Every other task type keeps the whole-node refusal (its cards are chosen later: unknown is every card), and a media lease
still never refuses a dispatch, only fences at the seat's own pre-check.

*The delegator's fence.* `NodeView.Leases` carries the entries. `leaseFenceReason` fences a node only for a contract whose seats ALL sit on a card a
fencing lease (exclusive, draining, or busy and not overdue and not a plain text reservation) holds, reading the seats off the node's own layer rows
(`placement.RemoteSeatCards`, the same chain rule the local box applies to itself); `leaseDemotionRank` ranks by the leases that stand between THIS
contract and its seats, so an overdue lease on a card the contract does not need says nothing, and one on its cards ranks the node last and never
excludes it. A node with no layer rows, a long-context contract, a layer the node does not declare and a seat whose cards cannot be placed all read as
every card. The vision and text lanes carry no contract, so they keep the whole-node reading. The reason names the lease and how many cards it holds
(`busy: media lease 7 on 1 card`).

*The wasted local leg.* A lease this process does not hold that fences every local seat of a contract's chain turns the local run away at the
seat's own pre-check after the delegator has spent an attempt, a ledger row and an intent record on it. The delegator now reads the same verdict first
(`ForeignFence` over the chain, `fencedLocal`) at every site that used to fall back to the local seat: route=auto when nothing else could take it, the
auto deal, the spread rotation, the re-placement's last resort and the capacity wait's local tick. It waits in line instead, and the fence clearing runs the
contract locally. It applies only when there is another node to wait for (a delegator with no remotes has no leg to save, and the pre-check is the fast
honest answer) and never to `route=local`; the holder's own child is exempt, as at the pre-check. A fallback must not hide a failure: when the remotes
were failing their health probe at the placement, that class rides the wait and is stamped on whatever ends it (the capacity defer, the fenced defer, a
local run after the fence cleared), so a fleet that has been down for a week still counts as `Summary.Infrastructure` and exits non-zero, as the local
fall-through it replaces did. A remote that answered while the subtask waited clears it (the fleet was not dead).

*A wait that ends in a kept place.* When the capacity wait ends with nothing having taken the work, the defer (class `capacity`, unchanged) names the places
the subtask stood in: `results[].place_keeping` lists `{node, on, detail, eta_sec}` for each node it stood behind (`lease` on its cards, `queue` for its own
refusal or no room, `backlog`, `cooldown`) and `retry_after_sec` is the soonest known end, with the same facts in the reason. The durable token that resumes a
place across calls belongs to media admission (plan P13); this is the delegator's half, the facts a re-call is placed by.

*Deploy and reclaim.* A binary swap touches the executable and the processes it may stop, not a card (the table in [node-swap.md](node-swap.md)), so a standalone deploy's
lease wait stays every lease unless the operator names the cards it touches (`node-swap --cards`, resolved against the card table, and still holding for a lease
a process the deploy would stop holds: that stop ends it whatever its cards); the fleet-serve wait is a job count, which is one node-wide number.
`fleet_reclaim`'s idle baseline stays whole-node on purpose: it records the headline card's used VRAM when nothing of the harness is loaded, its llama-swap half already
reads every card, and the error it exists to avoid is recording a baseline over a loaded model, which narrowing the lease half could only make more likely.
`fleet-ui` and `top` show one tile per card with its holder ([fleet-overview.md](fleet-overview.md)).

**What the lease is doing (plan P8, ADR 0070).** The lease block also carries `orphaned` and `stalled`, each absent unless
true and the worst across the node's live leases: an attended lease whose owner has been gone past `gpu_orphan_grace_min`,
a lease whose progress file stopped moving inside its stall window. A lease whose declared window ended while its holder
still renews is the block's existing expiry-based `overdue` key (the routing change), deliberately not published a second
time from the standing: one source for one wire key. They describe the lease and refuse nothing; a node one release behind
omits them, which decodes to false. Ranking such a node behind a healthy one is the delegator's call.

**Terms (plan P9).** The lease block also carries `expired`, absent unless true and read across every live lease (an
expired sibling does not hide behind a healthy lower epoch): the holder's own tick found the lease's term ended and not
renewable (its owner gone, its owner not shown present and no progress advancing, nothing running under it, or its cards
could not be read) and labelled it. An expired lease is still HELD: `busy` and
`overdue` stay true beside it (the Busy rule is `remaining > threshold OR overdue`, unchanged), nothing is reclaimed or
killed, and the key is a fact for the delegator and the operator, never a refusal. A lease that was renewed has a later
`until` and no `expired`. A node one release behind omits the key, which decodes to false; the delegator does not read it
yet.

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
than 60 s never removed — and runs a scan every `fleet_store_prune_every_sec` seconds (default 60; 0.130.1 — the job tick cannot fire under a GPU lease, and that is when the store fills fastest), after every `fleet_store_prune_every_jobs` completed jobs (default 8), on any health
poll whose last status was above the high mark, and once at start. At most one scan runs at a time; a health poll never walks
the directory itself. The root must carry a `.storesteward` marker file (written by the steward into an empty root; a populated root without it is
refused at start — a mistyped `fleet_store_root` can never become an oldest-first purge of some other tree); every removed page is
one journal line. A missing root fails `fleet-serve` at start; a scan or prune error is carried in `store.error`, never
hidden. Why it exists: LMCache's fs_native eviction counts only pages the running MP server wrote and the seat wrapper prunes
at seat start only, so <node-c>'s store went 28 → 99 GB against a 100 GB quota in 75 minutes of real fan-out (2026-09-06).

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

**Holds, never sleeps or refuses (ADR 0063).** Six changes to the wait and what feeds it:

- **A 503 never sleeps.** `runRemote` used to sleep the node's `Retry-After` (up to the 300 s the node clamps it
  to) on the node that refused, retry it once and charge the sleep to the contract. Now the refusal returns at once
  and `noteCooldown` files the hint as a per-node cooldown for the run, jittered once when the refusal happens; a
  refusal with no hint cools the node for `refusalCooldown`, and a hint at or above `maxQueuedWait` (compared as a
  number with the delegator's own constant, never the node's wording) is capped there, after the jitter (the excess is
  folded back under the ceiling, so a node is held out for at most 300 s). Only the wait consumes the cooldown, and
  the wait credits what it idles; a wait that ends with a node still cooling down names it in its defer.
- **Re-placement re-reads the fleet** for every route (`fetchViewsSince`: a memoised snapshot counts only when it was
  taken after the refusal, so a snapshot that predates the refusal is never reused; siblings that refuse together
  each read once). Candidates pass `withRoom`: eligible, room by their own advertisement, a start inside the
  caller's patience, no cooldown, a process gate with a free slot — all judged against the wall the re-placement
  will actually carry (what the first attempt left of the budget). With none left the subtask goes to the wait,
  whatever kind of refusal started the chain: `withRoom` lists every ELIGIBLE node it held out only for being busy,
  and any such node, like a full local run-cap line, marks the subtask wait-worthy; a node the contract can never
  run on does not (waiting for it would be a wait for nothing).
- **The local seat is a candidate only with a free slot ahead** in its run-cap line (`localSlotAhead`: the registered
  runs on the seat against `fleet_max_concurrent_jobs`, the count the seat's first-come-first-served gate uses). A
  local-leg capacity defer (class `capacity`, zero steps) is re-placeable (`capacityDeferRefusal`); `route=local`, or
  no remotes, waits in place, and a remote's defer after a `202` never moves. The wait the local run already spent in
  line is credited. With nothing else free the defer is published as a defer, not as `placement refused`; the defer
  alone does not make the subtask wait (only an eligible node that is merely busy does).
- **The queue budget and the backlog gate.** A job's wait to START is `clamp(1.5 x etaStart + 30 s, 60 s, patience)`
  (`queueBudgetFor`), where `etaStart` is `queue_wait_estimate_sec` or the arithmetic over the node's jobs and recent
  wall, patience is the contract's poll budget (`pollBudgetFor`), and a node that publishes no ETA keeps
  `min(patience, 5 min)`. It is read again when the job is first seen queued. `startsWithinPatience` holds a node out
  of the deal, of re-placement and of the wait when its ETA exceeds the patience: a placement FEASIBILITY refusal that
  prints its arithmetic (`backlog (a new job would wait ~444 s to start (1 running + 0 queued - 1 worker(s) + 1 = 1
  ahead x 443.6 s recent wall / 1 worker(s)), past the 360 s this contract will wait for a start)`, a 300 s wall plus the 60 s grace), in the same class
  as `feasibleFinal`, never a speed preference. A held-out node is read again every tick. A negative
  `queue_wait_estimate_sec` is a node bug and is no opinion (`estimateKnown`), never a confident zero. The refresh
  read is tried again on the next queued poll when it fails (three tries at most), logged, and named in the
  queue-deadline message if it never succeeded. At that deadline the delegator asks the node to take the job back
  (ADR 0064, above): a confirmation re-places the subtask, the node cools like any capacity refusal and the time
  the job sat queued is credited back; anything else leaves the deadline a failure whose text ends with why the
  withdraw was not confirmed.
- **The spread deal counts capacity.** No node is dealt more subtasks per run than `max_concurrent_jobs -
  jobs_running` (no floor; an unpublished ceiling is unlimited); a node at its headroom leaves the rotation, the fit
  order and the cycle are untouched, and the overflow goes to the wait. The local seat is counted too
  (`localRunCapRoom`: `fleet_max_concurrent_jobs` minus the runs registered on the planner seat, read once per deal;
  only while some remote could run the contract). The overflow subtask (`PlacedResult.overflow`) is neither a lease
  nor a refusal: it takes the seat back only once the seat stops reading busy by the deal's own reading
  (`localStillBusy`; a seat whose busy probe failed or was ambiguous still reads busy there, on `spread` and `auto` alike, although the deal itself treats it as idle, register C-88, 2026-10-01), is a capacity defer when the wait is off, and is not counted as a replacement. The deal names
  every node it passed over with its arithmetic, and a remote that failed its health probe keeps the run flagged.
- **A process-wide gate and a page cap** (`processgate.go`). The delegator counts, per node, the dispatches the
  process holds open across every concurrent Run and does not send one past the node's `max_queue_depth`; the subtask
  waits for the first node that frees (a turn-away is no refusal). A research page whose last three issues all
  failed after a seat ran them (a failed verification, an abstention, a budget defer or a node's own job error;
  never a full node, a lease, a bad token, a cancel, a queue deadline or the whole call's deadline, ADR 0065)
  is backed off for 15 minutes with a
  `contract`-class defer, and a success forgets it. An issue that was retried counts by either attempt: when its
  first attempt stands as a seat-down defer (ADR 0066), the retry seat's own failed run counts, and a retry the whole call's deadline cut counts for nothing (ADR 0065).

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
| `jobs_withdrawn` | int, omitted when 0 | Jobs a delegator took back out of this node's backlog (`DELETE /fleet/jobs/{id}`) since the process started. The call that flipped a job counts once; a repeat does not. ADR 0064. |
| `jobs_reaped` | int, omitted when 0 | Accepted agent jobs the poll-lease reaper took because nobody was polling them, since the process started. A drain's never-started marks count in neither. ADR 0064. |
| `seat_loaded` | bool, omitted when unread | llama-swap's `/running` says the agent seat is loaded. Served from the 30 s residency cache; a read older than two windows or never taken waits (bounded by `residencyWaitBound`, 1.5 s) for a fresh `/running` before answering; an agent contract that completed a call on this seat writes the loaded state straight into the cache (0.128.2, below). |
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

**The residency cache is stale-while-revalidate with a bound (0.128.2).** The residency refresh
(`agent_seat_resident`, `served_models`, `seat_loaded`/`seat_starting`) runs at most once per 30 s window,
and a health read past that window used to serve the previous answer unconditionally while refreshing
behind it — so the FIRST read after any quiet period reported the seat as it was before the last job,
however long ago. The 0.128.1 slot census showed the cost: the delegator's eta charged `cold 28` for a
seat whose admission wait was 6 ms (`chosen eta 326 s (cold 28 + 298 gen)`), because the only health read
between the smoke that warmed the seat and the census was the census's own, two minutes later, and it got
the pre-smoke answer. Now a read inside one more window (30–60 s old) keeps that shape; a read older than
that, or one on a never-probed node, waits for the refresh it kicked, bounded by `residencyWaitBound`
(1.5 s — sized for a responsive llama-swap, whose three refresh legs answer in milliseconds, and under every
health client's budget: the accelerator router's 2 s, the fleet UI poller's 5 s, the delegator's 15 s), and
serves whatever is published when the wait ends — the previous answer if llama-swap is slow or hung, said
once per refresh cycle on the node's log. Separately, the dispatch path reads the one fact a finished agent
contract proves about the advertised seat — `steps > 0` on a result whose `seat` IS `agent_seat`, i.e. at
least one completed call on it — and writes `seat_loaded:true` / `seat_starting:false` straight into the
cache, no probe. Nothing else counts: a defer that never reached the seat (roster-unserved, config,
contract, a composite guard) finishes as a SUCCESS-shaped job with zero steps (`agenttask`'s `OK` is true
for every terminal outcome, defers included) and proves nothing; a job error proves nothing either way; a
contract that ran on another layer's seat proves nothing about this one. That write never touches
`agent_seat_resident` (roster-verified, always) or the cache's age, so the roster keeps refreshing on its
own 30 s cadence under sustained traffic; a probe that STARTED before the write keeps the job's seat facts
when it lands and still publishes its roster facts. Pinned by `residency_stale_test.go`: fresh beyond the
band, previous inside it, bounded and logged once on a hung `/running`, a completed call written with no
probe while a zero-step defer and a foreign-seat result write nothing, no wait across back-to-back finishes
on a slow `/running`, an in-flight probe keeping the job's seat facts, and the pull-queue door writing it
too. Both doors write it — the push dispatch (`handleDispatch`'s run closure) and the pull-queue claim loop
(`claimOne`'s) — because each hand-writes its own run sequence and a write landed on one alone is the
drift class the register's "agent doors" entry records. On a composite node only the default layer's seat
(`agent_seat`) has this fast path; another layer's seat is written by no job and read by the probe alone. A
result that cannot be decoded for the write is logged once per process (it is this node's own producer, so
that is a wire-shape drift, not a normal outcome) and the cache falls back to `/running` only.

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

**The memory stack is protected by name, in both modes.** The house rule gives every model a
300 s idle ttl, so a mem0 stack member is neither a `ttl: -1/0` seat nor in the keep-set, yet
it is what `gpu reserve --unload-seat` and the render helper keep resident. Reclaim keeps the
same set, `memory_stack` (the default set when empty), matched on the canonical id ignoring
case and padding, before the keep-set is consulted and even when no keep-set could be read
(register C-94). A non-member at ttl 300 is still reclaimable.

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
| `auto` (default) | `gate.Place`: an idle local seat always wins; remotes are considered only while the local seat is busy — since PR-5 (W-01) that reading is `probeLocalBusy`'s own in-flight count at or past `FleetConcurrencyLimit()`, or a load in progress, OR'd with the GPU lease, read ONCE per Run — and only the ones passing the hard gate (agent lane on, seat resident, contract fits the advertised ctx, output_schema present, origin hop, and — PR-5's W-05, as corrected in 0.128.1 — the seat can produce one tool step and a minimal answer inside the contract's own effective wall). No eligible remote → queued-local. A contract that names a `layer` the delegator's own box does not declare is not idle-local work (register A-108): the roster is read for it although nothing is busy, and it goes to the best remote that declares the layer; with none it defers naming the layer. See "Expected-completion ranking and the joint deal" below for how `auto`/`remote` now place a WHOLE Run's subtasks in one pass. |
| `spread` (0.80.0, fit-scored 0.99.0) | one `Run` fetches every remote's health ONCE, then deals the subtasks across the local seat AND every remote that passes the hard gate for that subtask. The deal is computed for the WHOLE run in one pass before dispatch, and within each cycle of `len(nodes)` slots every eligible seat takes at most one subtask — so an N-contract fan-out genuinely runs on N seats at the same time, and the fit score can reorder a cycle but never collapse it (see "Fit-scored remote slots" below). The local rotation slot is never contested by shape: with the local seat IDLE, slot 0 is always the local seat (pinned by `TestDealSpreadKeepsSubtaskZeroLocal`, `TestDealSpreadSameShapedFanOutReachesEverySeat` and `TestRunSpreadDealsAcrossLocalAndEveryEligibleRemote`) and a 2-contract spread with an eligible remote is still guaranteed one local + one remote — the pair shape. It IS contested by load (0.113.20, `agent_spread_local_slot`): a local seat already holding a request at deal time loses its slots to the best-fit eligible remote with room — see "The local slot under load" below. It is contested by the contract's layer too (register A-108): a subtask that names a layer the delegator's own box does not declare never takes the local slot — see "The local slot and a named layer" below. Per-subtask eligibility means a contract failing the gate (no `output_schema`, over-size) silently takes the local slot instead; `results[].placement` names where each landed and, for a remote, which shape the fit score read. Measured before spread existed: `auto` put four concurrent contracts on one box, `remote` put four on the other one. No eligible remote → every subtask runs local and the reason says so. Since ADR 0063 the deal also counts each remote's headroom (never more subtasks than `max_concurrent_jobs - jobs_running`; the overflow goes to the capacity wait) and holds out a node that cannot start the job inside the caller's patience. |
| `local` | forced in-process, no network. |
| `remote` | forced fleet node; with no eligible remote the subtask DEFERS loudly. |

Remotes come from the call's `remotes` argument, else from the config's `delegate_remotes` (tailnet URLs). A call's own list REPLACES the config list; it does not merge. A box with `delegate_remotes` set therefore fans out without the caller naming nodes — fleet membership is configuration, not per-call knowledge.

**Fit-scored remote slots (0.99.0).** `spread` used to deal the remote slots blind: `k := i % len(nodes)` and nothing more. Across heterogeneous seats that sends mechanical triage to the biggest seat and cross-file reasoning to the smallest one with equal probability. `internal/delegate/fit.go` now infers the contract's coarse SHAPE from its own goal text and scores the eligible seats:

- **Shape** (`inferKind`) is one of `mechanical` (extraction, listing, counting, filtering, digesting) or `reasoning` (explanation, causation, cross-file interaction, tracing, comparison). It is decided by an ORDERED deterministic pre-filter — a quantity rule, then an explanation rule, then a mechanical-verb rule — and the order is load-bearing: the quantity rule is what stops a bare `how ` pattern reading "how many files changed" as reasoning. No model call is involved; a placement is reproducible from the recorded contract alone.
- **A research page digest is `mechanical` by its shape, whatever the goal says (register C-76, ADR 0063).** A contract from a research door (`offload_research`, `cli:research`) with exactly one context page and an output schema is decided from that structure (`digestShaped`, rule `research-digest`), not from the words of the caller's question: "architecture", "why", "how", "compare" and "trace" sent every such digest to the roomiest seat first. Window adequacy still gates every seat, and any other contract is read from its goal as above.
- **A goal no rule matches is `mechanical`** — the CHEAP seat. This harness exists to move grunt work off the expensive seat, so ambiguity falls toward cheap, never toward capable. A wrong cheap placement costs a retry (which the engine already runs on a different seat); a wrong expensive placement costs the capable seat, which is the resource being protected. The unmatched branch is the seam a better fallback would plug into — a shape carried on the contract, or one decided per fan-out and reused — never a per-subtask model round-trip.
- **Score** (`scoreFit`) ranks a seat by its ADVERTISED `agent_ctx_tokens`, the only capability number nodes publish: reasoning takes the roomiest **adequate** seat, mechanical takes the **smallest adequate** seat so the roomier one stays free. *Adequate* is not a slogan — it is `adequate()`, `est_tokens + specReserve <= agent_ctx_tokens`, the same arithmetic the hard gate uses (they share the function, so they cannot drift). An unadvertised ceiling is never adequate: unknown is not a capacity, and a seat that published no number must not win the mechanical contest by looking like the smallest on the roster.
- **Fit chooses WITHIN a cycle, never a free re-pick.** This is the load-bearing constraint: a subtask takes the best-fitting seat *among those not yet dealt in the current cycle*, and a local slot reshuffles the deck. Without it the smallest seat wins every mechanical slot and the roomiest wins every reasoning slot — measured on a `{local, node-b 131k, node-a 32k, node-c 32k}` roster, an unconstrained re-pick put 8 mechanical subtasks on `local 2 / node-a 4 / node-c 2 / node-b 0` and 8 reasoning subtasks on `local 2 / node-b 6 / node-a 0 / node-c 0`, which is precisely the stacking `spread` exists to remove. With the cycle constraint both deal `2/2/2/2` — mechanical dispatching the small seats first, reasoning the roomiest first.
- **The deal is joint, and it has to be.** No per-subtask function of (index, own shape, roster) can hold the invariant: distinctness inside a cycle forces the slot-to-seat map to be a bijection for each shape, and distinctness inside a MIXED-shape cycle then forces the two shapes' bijections to be identical — i.e. forces the shape to have no effect at all. Fit scoring and one-per-seat therefore coexist only when the deal can see its siblings, so `dealSpread` computes every subtask's placement in one ordered pass before dispatch. That also keeps placement deterministic and free of shared mutable state (the goroutines read the deal, they never build it).
- **Ties keep the rotation** (the comparison is strict), so an all-equal roster deals exactly as it did before fit scoring existed.
- **The cycle is keyed on the DIAL BASE, not the node id (2026-09-17, S-12).** Every other exclusion in the delegator (the tried set, the re-placement exclusions, the refusal cooldown, the quarantine) keys on the base, and a `node_id` is neither unique nor guaranteed to be published. Two remotes advertising an empty id — or the same id, which register C-19 shows does drift — shared one entry, so the second remote of a cycle found its key already taken, the cycle was reshuffled, and the fit score handed the SAME seat both subtasks while the other one idled. The base is what the dispatcher actually dials, so it is the only key that can mean "this seat already has one".
- **The local rotation slot is never contested by shape; it is contested by load (0.113.20).** With the local seat idle, subtask 0 lands local whatever its shape — a single-subtask spread is the riskiest case for a shape heuristic, and one regex match must not send a whole run off-box. The same holds for every later local slot, because the fit score ranks by advertised ceiling and the local seat advertises none in a delegator run; scoring it would mean inventing a number for it. Widening the contest to the local slot is a small change once the local seat advertises a ceiling of its own.
- **The local slot under load (0.113.20, operator decision 2026-09-06).** The deal reads the local seat's in-flight count ONCE when it is computed — the same reader the drain uses (`internal/seatload`: vLLM `num_requests_running` + `waiting`, or a llama-server's processing `/slots`, through llama-swap, alias-aware) — and when the seat already holds a request, every local slot (`i mod len == 0`, subtask 0 included) goes to the best-fit eligible remote WITH ROOM (`hasRoom`; a sheddable run needs an idle slot) instead; the remotes' one-per-seat-per-cycle invariant is unchanged and the reason names the count (`…; local seat busy: 3 in flight`). With no remote that has room the slot stays local and the reason says `local seat busy … no remote with room`. An idle seat keeps every slot it had, so a lone session is dealt exactly as before; a text lease still removes the local seat in both modes. `agent_spread_local_slot: "always"` restores the unconditional local slot. Why: K delegating sessions each dealt 3 of every 8 subtasks to the same local seat while the remotes idled — the K×8 gate's remaining tail after <node-c> seat replacement (first-local subtask 155–189 s under K=3 vs 91–105 s for its siblings; K=2 wall 1.55× K=1 against a 1.5× bound). A probe that fails deals as idle and logs why: the rule is an optimisation of the deal, never a gate.
- **The local slot and a named layer (register A-108).** A subtask whose contract names a `layer` (register A-100) is dealt the local slot only if the delegator's own box declares that layer (`cfg.layers`, read by `runner.localServesLayer`; a view that carries rows is honoured too). Before this the deal never looked: on a box that declares no layers, every local rotation slot a layer-naming subtask fell on (slot 1 of 2 in a two-node spread; six in the 2026-09-19 ledger) ended as `layer fast requested; this box declares no layers` while the one node that declared the layer sat idle, and `route=auto`'s idle-local shortcut did the same with no spread at all, because `Place` carries the rule and neither deal calls `Place`. Now the local slot leaves the rotation for that subtask, exactly as a text lease takes it out, and the subtask deals among the remotes that declare the layer (`remoteEligible` already refuses every other one by the layer rule). Capacity keeps its meaning: when every remote that declares the layer is dealt to its headroom or holds a backlog past the caller's patience, the subtask is a place in line for them (the capacity wait; reason `every remote that can take layer fast is already dealt to its headroom or holds a backlog past the caller's patience (...); this box declares no such layer`), never a defer from the seat, and the wait never offers it the local seat. With no node declaring the layer it defers naming it, and the placement reason carries the fleet's verdict (`layer fast requested and no answering remote declares it`) instead of the defensive "placement and gate disagree" line. The same predicate gates every other place the local seat is chosen: `route=auto`'s idle-local shortcut (and the fleet read that feeds it: an idle box reads the roster only when some contract names a layer it does not declare), the capacity wait's local candidate and its closing lease defer, a re-placement's local last resort (a refusal that was a place in line waits for the node that declares the layer; one that was not, a 409 for instance, fails at once naming the layer even while a text lease holds the seat: that lease frees a seat that could never run the contract, so it is not waited on, whereas a box that declares the layer keeps the lease wait), and a verification retry (another node that declares the layer, or no retry). A contract that names no layer, and a box that declares the layer, deal exactly as before (`TestSpreadKeepsTheLocalSlotForContractsThatNameNoLayer`, `TestSpreadHandsTheLocalSlotToABoxThatDeclaresTheLayer`, `TestAutoHandsAnIdleBoxThatDeclaresTheLayerItsOwnContract`, and, for the lease wait, `TestALeaseWaitStaysForABoxThatDeclaresTheLayer`). The refusal that frees nothing, under a lease, is pinned by `TestANamedLayerItsOnlyNodeRefusedForGoodFailsAtOnceUnderAnyLease`.

**Retry on a different seat (0.80.0).** A subtask whose first attempt came back `failed_verification` (the acceptance DSL caught a wrong answer) or an honest `abstention` is re-run ONCE on a different node when one is available — local → the best eligible remote, remote → local — under a fresh job id. The published result is the BETTER attempt (a success beats any failure; otherwise the first attempt stands) and carries `retried_on` + `retry_note`; the summary carries `retried` / `retry_recovered`. Measured motivation: on the same four digest contracts the 27B seat and the 4B seat each missed a different one, and neither miss was silent thanks to acceptance — the retry is what turns "caught" into "recovered". Transport failures and infrastructure/config/contract defers are NOT retried: a broken box or a bad contract does not get better on another seat. The retry lives **inside the subtask's `timeout_sec`** — it gets whatever budget the first attempt left, and is skipped (the result carries a `retry_note` saying so) when less than the retry floor remains — 10 s by default, raised by the delegator's `agent_retry_min_sec` (0.115.9, register D-46: a cold vLLM load plus one turn at `max_tokens` on the retry seat; 300 on the reference box) — so `timeout_sec` stays the wall ceiling the caller was told it is. Two more skips, each named in `retry_note` (0.115.9): a first attempt that ended on an **empty final** (`stop_reason` `reasoning_starved` / `empty`, 0.115.8) is never retried — the shape is the seat's completion budget, not a wrong answer another seat corrects; and the retry never lands on a seat that is **already running another job** — a remote the delegator can prove would QUEUE the retry (`!provablyStartsNow`: no free worker, or a backlog ahead of it), or the local seat with requests in flight — which would only halve both runs' tok/s — a skip that is now a WAIT (ADR 0063, register C-76): the retry queues for the seat for up to the placement wait (credited to its budget) and runs the moment the seat has room, and is skipped only when the seat stays busy for the whole wait or the wait is off (`retry_note` then says how long it waited). A seat-down re-placement (ADR 0066, the `seat down:` row below) is exempt from both: it is not checked for a busy seat, so it is neither held nor skipped for one, because a remote node's own queue is the line (a full node answers 503 and the dispatch is re-placed at once, or waits in the bounded, credited capacity wait when no node has room, ADR 0063). The local seat has no such line: while its run-cap line has no free slot ahead of a newcomer the retry goes to an untried remote first, and to the local seat only when there is none. Under the whole-call deadline ([ADR 0065](../architecture/decisions/0065-the-whole-call-has-a-deadline-below-the-clients-abort.md)) a seat-down re-placement is an attempt like any other: the credit it carries lengthens the retry's budget on the contract's clock, never the call's, so one still running or waiting for capacity at the deadline is cut with a `budget` row of its own, the published result stays the first attempt (the seat-down defer produced in time) and the cut rides in its `retry_note`, and a `seat down:` defer produced after the deadline is the deadline's budget defer and is not re-placed. The remote threshold was `jobs_running > 0` until 2026-09-17, i.e. zero rather than the node's own ceiling, so a four-worker box with one job in flight refused every cross-seat retry although three workers were idle; register D-46 shipped the rule and not the threshold. It is the same ceiling-aware predicate the placement gate ranks on, and it stays conservative — an unpublished ceiling still reads as busy. The re-placement floor after a REFUSED dispatch (no seat time spent) stays at 10 s.

**The retry never lands on a FENCED or RESERVED local seat (0.117.7, register D-94; reservations, register C-81).** Before choosing the local
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
minutes. A plain (non-exclusive, non-draining) text reservation is NOT a fence: the affinity gate
admits the load, so nothing refuses the dial. It is a RESERVATION, though, and a retry never runs on it
(register C-81), as a first placement on `route=auto` or `route=spread` does not. `Reserved` already keeps
the local seat out of FIRST placement (below) and out of `replacementNode`'s last resort, and
`alternativeNode` asks it as well, right after the
fence; before C-81 it read only the fence, so a verification retry or a seat-down re-issue (ADR 0066)
whose first attempt had run on a fleet node was dialled onto cards a measurement had reserved. With the
seat reserved the retry goes to the best untried remote exactly as it does for a fence (the placement
reason says `the local seat is reserved (…)` and names the holder). With none it is skipped at once, from
the lease record, and the `retry_note` names the holder and says why: `the local seat is reserved (gpu
lease class=text …) and no other node is eligible; a retry placed there would run on the cards the holder
reserved`. The clause `the local seat is reserved (…)` is the sentence `replacementNode` ends a refused
re-placement with for a reserved seat (a test keeps the two identical), and the flow after it differs. A refused
re-placement goes to the capacity wait, which holds the subtask until the lease clears or a node frees (bounded
by `agent_placement_wait_sec`, or `agent_lease_wait_sec` when that is longer). A retry has no such wait: it is
skipped, as it is for a fence (D-94), the first attempt's result stands, and the note names the holder. Its one
wait is for a busy seat it was already placed on (ADR 0063, decision 10), not for a seat no placement may
take. The holder's own child
(`GPU_LEASE_EPOCH` equal to the held epoch) is exempt, as it is for a first placement. The note says `and
no other node is eligible` only for a read of the fleet that finished: when the whole call's deadline (ADR 0065) ended that read,
the note leads with `call deadline reached before a retry node was chosen` and names the fence (or the reservation) behind it, and the
claim about the other nodes is left out.

FIRST placement already consults the lease and always did: `route=auto` defers to the capacity wait
when `Reserved(LocalLease(...))` holds (`TestRunAutoReservedLocalDefersNamingTheHolder`) and
`route=spread` drops the local seat out of the deal (`TestRunSpreadReservedLocalDealsRemotesOnly`).
A MEDIA lease is deliberately excluded there — renders are arbitrated at the model-affinity gate
(ADR 0026) — which is why it fences a retry but not a first placement: a retry runs on the leftovers
of `timeout_sec` and cannot afford to spend them queueing behind a render.

### Expected-completion ranking and the joint deal (route=auto/remote, PR-5)

[ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md) and the
operator-signed INV-5 rider it cites (register Part A of the harness master plan, 2026-09-17: "a seat decision
may carry a wall-time term only as (i) a feasibility floor computed from the contract's fitted final … and (ii)
an ordering key among seats that have already passed the capability/adequacy gate, with power-of-two-choices
among near-ties") reshaped `auto`/`remote` placement on the same "quality-first, then honest quantities"
principle `spread`'s fit score already used. Nothing here widens who is eligible — `remoteEligible` still decides that — it changes how the
survivors are RANKED and how many of one Run's subtasks one node can take.

- **One joint deal, not N independent picks (W-06, register S-11/S-13).** `RunWith` now fetches the fleet ONCE
  and computes every subtask's `auto`/`remote` placement in one ordered pass (`dealAutoRemote`) before any
  dispatch goroutine starts — the exact invariant `spread`'s `dealSpread` already held, extended here because
  siblings used to probe the fleet and call `Place` independently, within milliseconds of each other, and could
  read the same free slot on the same node before any of them had actually dispatched. Per-node **headroom** —
  `max_concurrent_jobs − jobs_running − (subtasks this deal has already committed to it)` — is checked before a
  node is even ranked: a node at 0 headroom gets NOTHING, no floor, and the next-best eligible candidate is
  tried. Three outcomes per subtask: a resolved node; `capacityWait` (something was eligible but every one of
  them is at headroom right now — routed to the existing `awaitCapacity`, which watches for room to free exactly
  as it already does for a 503 or a held lease); or `noRemote` (nothing in the fleet could ever take it —
  unrelated to headroom, the pre-PR-5 "no eligible remote" outcome, unchanged).
- **A feasibility floor that asks only for a minimum viable final, never the seat's worst case (W-05, register
  S-03/S-05).** A seat whose published `seat_rate` implies it cannot produce one tool step plus a minimal
  64-token final within the contract's own EFFECTIVE wall — `timeout_sec` as given, or `seatrate.AutoWallFor`'s
  own sizing for a `timeout_auto` contract — is excluded, naming the arithmetic ("one step and a 64-token answer
  need 42 s at 5.4 tok/s, the wall is 20 s"). No think block, no structured re-pack and NO cold load enter that
  question: admission pays the cold load outside the wall (D-64 warms the seat on the admission budget), so a
  wall shorter than the load is not infeasible. This is deliberately NOT the seat's published `min_turn_sec`
  (its max-final worst case), and since 0.128.1 it is no longer a fit of the configured final against
  `seatrate.FinalBudgetFloor` either: 0.128.0 shipped that rule and it refused a cold <node-a> a 60 s smoke
  contract the seat completes in ~25 s ("fitted final 0 < floor 1024") while printing a 4,682 s eta for the
  <node-c> on the same wall. The INV-5 rider permits a wall-time term as a refusal only "below a minimum viable
  final"; how much of the configured final the wall actually buys is the eta's business (next bullet). An
  unpublished rate is no opinion — eligible, exactly as before.
- **Expected-completion ranking among quality-adequate seats (W-11, register S-02).** Once a seat is past the
  hard gate, `betterRemote` orders survivors by an ETA — cold load (charged only when `seat_loaded` is KNOWN
  false; a load in progress counts half) plus the node's own queue wait plus the generation time for the
  final FITTED to the whole wall (0.128.1: not the wall minus cold, since admission pays the load outside it) at
  the seat's measured rate, capped at the wall so an eta never exceeds cold + queue + wall — with the axis
  depending on the contract's inferred shape exactly as
  `spread`'s fit score already does: reasoning-shaped work still ranks the roomiest window first, eta only as
  its tie-break; mechanical work now ranks the fastest expected completion first (replacing the old
  "smallest adequate seat wins" rule), window as its tie-break. Two candidates whose etas are close are a near-tie, decided by a
  deterministic **power-of-two-choices draw** seeded from the wire job id (or a fresh seed on the two
  re-placement/retry selection paths, which have no job id in hand yet) — so K independent dispatchers spread
  across near-tied seats instead of all converging on the single fastest one. Since 0.132.3 that draw is
  bounded JITTER on each seat's own eta, derived from the seed and the seat's node id, rather than one coin
  flipped for the pair: a coin hashed from the seed alone answered the same whichever way round it was asked,
  so two near-tied seats each beat the other and the roster's order decided the winner (ADR 0057). A seat that
  publishes no rate is ranked on the fleet's median published rate, keeping its own backlog and cold load;
  window decides only when no node on the roster publishes a rate; `PlaceVision` gets the same eta tie-break
  after its own existing keys (no generation term — a vision judgment is one call, not an agent loop).
- **A quiet lease demotes instead of excluding (W-14, register S-15).** `LeaseBusy` alone — a node's DECLARED
  reservation window, not a measurement of the cards — used to hard-exclude a remote exactly like a held TEXT
  lease. Now only an EXCLUSIVE or DRAINING hold, or a busy lease that is not a plain text reservation (a media
  render, which can never be exclusive/draining), still excludes; a plain, non-exclusive, non-draining text
  lease the node calls busy stays ELIGIBLE and is ranked last among eligible remotes instead — a 6-hour
  reservation over idle cards no longer routes work away from a node that could run it.
- **`placement_reason` names every reachable remote with a one-word verdict (item 8, register D-105).** The
  resolved reason (`route=auto → node-a (headroom); node-a: chosen eta 41 s (cold 15 + 26 gen); node-b: slow
  (one step and a 64-token answer need 646 s at 0.3 tok/s, the wall is 300 s); node-c: cap (4/4 running, headroom 0, dealt 0)`) keeps the existing `route=remote`/`route=spread`
  prefixes byte-identical (`fleet_smoke_cmd.go` parses them) and appends one clause per node from the vocabulary
  `chosen | queue | backlog | cap | slow | lease | cold | probe | unfit(ctx) | noschema` (`backlog`: ADR 0063), in gate order.
- **Long-poll and a courtesy Retry-After retry (item 7, register D-106).** The delegator's poll now sends
  `GET /fleet/jobs/<id>?wait=12`; a node at 0.127+ blocks the connection until the job finishes or 12 s elapse
  (bounded under the client's own 15 s per-poll timeout), and an older node ignores the parameter and answers
  at once — the fallback IS the parameter being a no-op, not a second code path. The courtesy retry this item
  added (a dispatch 503's own `Retry-After` honored with a bounded sleep and one retry of the SAME node, inside the
  dispatch call) was replaced by ADR 0063: a 503 returns at once, the subtask is re-placed on a node with room, and
  the hint becomes a per-node cooldown that only the capacity wait honours.
- **`offload_status` reports the same in-flight signal (item 9, W-31).** Each fleet node row and the local seat
  entry publish `in_flight` (a job-registry count — `jobs_running − jobs_admitting` remotely, the seat's own
  gauge locally — never GPU utilization and never a lease alone) and a one-word `verdict`: `busy` (in-flight >
  0) | `held-idle` (a lease is held, nothing running) | `loaded-idle` | `cold` | `unknown`. **Scope: this PR
  wires the vocabulary into `offload_status` only.** `gpu status` and `fleet-ui`/`top` keep their existing
  `gpuactivity`-based verdicts unchanged; the operator's original ask ("one-word verdict vocabulary
  everywhere") spans all three surfaces, and the other two are a follow-up, not something this PR claims to
  have done.
  - **`jobs_admitting` is a DISPLAY and queue-estimate signal, not a headroom exemption.** `in_flight` and
    `verdict` above subtract it (a job still in admission holds no card, so it must not count as "busy" for
    a human reading the status), and it is exactly what the node's own `queue_wait_estimate_sec` (preferred by
    `queueWaitFor` whenever published — see "Expected-completion ranking" above) is computed from. W-06's
    HEADROOM key (`max_concurrent_jobs − jobs_running`) deliberately does NOT subtract it: an admitting job has
    already claimed one of the node's `max_concurrent_jobs` workers and WILL occupy the card once its cordon/
    pre-flight/cold-load/coherence-probe sequence finishes, so dealing another subtask onto that same worker
    slot would over-commit it. The two readings of the same field are intentional, not an inconsistency: one
    answers "is the card doing work right now" (display), the other "is this worker slot free to claim"
    (placement).

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
| `timeout_sec` | int | The wall: what the run is EXPECTED to fit in, over probe + build + loop + re-pack (its clock starts after admission). It is not a deadline: since 0.131.0 (ADR 0055) a run ends on a stall or on the liveness ceiling (`ceiling_sec`: `max(3 × estimate, 2 × wall, 1800 s)`), and since register C-80 the structured re-pack alone is held to the wall plus 30 s of slack, by a token budget and a deadline check before each attempt (see `repack_ms` below). Default 300, clamped to 900. Since 0.126.0 (register D-03) a caller who names none gets the default PLUS `timeout_auto`, and the node sizes the wall itself — see the next row. |
| `timeout_auto` | bool | 0.126.0 (register D-03). Stamped by intake when the caller named no `timeout_sec`: the executing node sizes the wall from its seat's measured rate (the same estimate published as `wall_estimate_sec`), clamped to 300..900, runs under it and reports it as `wall_sec`. A seat with no rate yet runs the default. Never set next to a caller's own `timeout_sec`; never set on a retry or re-placement (its wall is what is left). The delegator's **budget** (the retry remainder, the re-placement ledger) still holds the 900 s cap open, but since register D-116 its **poll clock** does not: it is sized from what the target node advertises (`seat_rate` + `seat_budget`) by the same arithmetic the node sizes its wall with, raised to the node's own `wall_sec` the moment a running poll publishes one, and only a node advertising no rate is polled to the cap — see [the poll deadline](#job-protocol-delegator--node). An older node ignores the field and runs the default. |
| `thinking` | string | Optional (0.115.8). The planner think-block policy on the executing seat: `auto` (empty; falls back to the node's `agent_thinking`, then auto) thinks every step and re-issues an EMPTY final once with thinking off at 4× the step budget; `off` renders every planner call in non-thinking mode (`chat_template_kwargs: {"enable_thinking": false}`, the re-pack's knob) — for grounded extraction on a thinking seat that spends its budget in the think block; `on` never sends the kwarg (a template that rejects it). Any other string is a 400 naming `thinking`. |
| `write_root` | string | Optional (0.122.0, register D-06). Opens the WRITE door: a directory RELATIVE to the run's read root (this node's materialized context dir) the seat may create and change files under. Must be relative and non-escaping — no `..`, no absolute or volume-qualified path, no `.git` segment, no reserved Windows device name, no trailing space or dot — validated on every platform, because the delegator and the node can be different operating systems. Refused unless this node's config says `agent_allow_write: true`: an ack-time 400 on the fleet path (so the delegator re-places), `defer_class: "write"` on the in-process local path. See [The write door](../FLEET-NODE.md#the-write-door-agent_allow_write-default-off). |
| `depth` | int | **Advisory on the wire**: the node derives `max(1, depth)` for anything that arrives over the fleet wire, so a wire claim of "origin" is never trusted. The delegator's placement gate separately requires the requester's depth to be 0 (hop limit 1). |

Context docs are materialized to a job-scoped dir under `pipeline-jobs/` (the same
sweep-at-startup discipline as pipeline jobs) and removed when the job ends. A delegator's
own in-process local runs (`pipeline.RunAgentContract`, inside the MCP server and the
`delegate` and `research` commands) keep their context in the same root, in `agent-local-*`
dirs that carry an owner marker (`.owner`, the writing process's id, beside `context/`
and never inside it). The startup sweep removes only what is orphaned: a marked dir is kept
while its owner process is alive and the dir is under `jobdir.MaxRunLifetime` (24 h), an
unmarked `agent-local-*` dir (a delegator older than the marker wrote it) is kept until it
is that old, and any other unmarked dir is `fleet-serve`'s own and goes. A `fleet-serve`
restart therefore never takes the context out from under a local run in flight on the same
box (register C-78). A removal for age alone, which can reach a run that outlived the bound,
is logged dir by dir; a dir the sweep cannot inspect is kept and reported, never counted as
a run in flight. The marker's process id means something only in the process-id space that
wrote it, so keep one base dir per machine: sharing one between a Windows host and a WSL
distribution, a container and its host, or two machines would read a live owner as exited
(the machine-wide GPU lease and its activity registry presume the same of their records).

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
| `stop_reason` | string | The loop's stop reason: `done`, `budget` (step budget), `error`, `unparsed_tool_call`, and since 0.115.8 `reasoning_starved` (the final completion spent its budget on hidden reasoning twice in a row — under `auto`, thinking on then off; under `on`, twice thinking on) or `empty` (the seat closed with nothing twice). Since 0.115.19 (register D-89) the LAST step of a multi-step run is a FORCED FINAL step — no tools offered, an answer-now turn, the final completion budget, thinking off unless the seat is pinned `on` — so `done` can carry `stop_note` "forced final answer …" (the seat answered when its step budget ran out), and `budget` now means it answered even that step with a tool call, which is never executed. The last two always arrive DEFERRED with an empty `output`; nothing re-packs them. Since 0.125.0 (register D-114) also `tool_call_cut`: the seat's TOOL-CALL ARGUMENT was cut by the completion budget - the engine refuses to parse the half-written JSON (llama.cpp: HTTP 500 "Failed to parse tool call arguments as JSON ... invalid string: missing closing quote"), or the completion arrives carrying a tool call whose arguments are a fragment. Since 0.140.2 that second shape is read from the ARGUMENTS, not the finish reason alone: vLLM reports a call cut at the cap as `finish_reason: tool_calls`, so an argument that does not parse is a cut when the finish reason is `length`, when `completion_tokens` reached the step budget, or when the JSON ends mid-value. The client also never hands invalid tool-call arguments back to the engine (they go out as `{}`), which is what made vLLM answer the next request with HTTP 400 `Unterminated string`. The same budget test (finish `length`, or `completion_tokens` at the call's budget) decides `reasoning_starved` versus `empty` and a flagged cut answer. The loop re-issues that SAME step ONCE at the final budget; a second cut ends the run on this reason with an empty `output` and `defer_class: budget` - never `infrastructure`, which is how a ~3 KB single-call write at a 1,024-token step budget used to be filed against the box. |
| `stop_note` | string | 0.115.8: the one-line evidence behind a `reasoning_starved` / `empty` stop — finish reason, reasoning vs completion tokens, which wire key the seat used (`reasoning` on vLLM, `reasoning_content` on llama.cpp). Since 0.115.19 also on a forced final step: "forced final answer …" on a `done`, and the tool-call evidence on a `budget`. Since 0.125.0 also on `tool_call_cut`: both budgets and how far into the argument the engine got ("tool-call argument cut at the completion budget twice (step 1024 tok, re-issued at 4096 tok; partial argument 2847 chars) …"), which is the same line the defer `reason` carries. Absent on every other stop. |
| `output_truncated` | bool | 0.115.8: the final answer ended on `finish_reason: length` — a correct PARTIAL, still re-packed and still the caller's to use, but not the whole. |
| `response_shape` | string | 0.115.13 (register D-45): the seat's OBSERVED answer shape on this run — `reasoning_key=reasoning\|reasoning_content\|none reasoning_tokens=reported\|unreported tool_calls_parsed=N completions=N`. The pin says what the seat is; this says how it answered (the 2026-09-04 tool-parser mismatch and the 2026-09-10 reasoning-key blind spot were both invisible without it). Absent when no completion ran. |
| `seat_tok_s` | float | 0.115.21 (register D-03): this run's effective decode rate — completion tokens per second of call wall over the completions that generated ≥ 1,024 tokens (tool-call completions are prefill-dominated and excluded). 0/absent = no qualifying completion. Also the ledger row's `tok_per_s`. |
| `wall_sec` | int | 0.126.0 (register D-03): the wall the node sized this run to — a `timeout_auto` contract, the estimate below clamped to 300..900; `wall_note` is prefixed `auto wall (timeout_auto):`. Stamped before admission, so a defer at the cordon carries it too. Absent when the contract named its own `timeout_sec`, or when the seat had no rate yet and the wire default ran. |
| `wall_estimate_sec` | int | 0.115.21: the wall the node estimated the contract needed on this seat BEFORE the loop ran — cold load + (thinking auto ? one think block : 0) + (steps − 1) × (128 tok + 6 s prefill) + final budget, at the seat's remembered rate (`seat-rates.json`, else `agent_seat_tok_s`). Absent when no rate is known. Never changes the wall. |
| `min_turn_sec` | int | 0.115.21: cold load + one turn at the final budget — the least wall a retry is worth on this seat; since 0.117.2 it includes the re-pack term for a contract with an `output_schema`. The delegator's retry floor is `max(agent_retry_min_sec, the RETRY seat's min_turn)` — a remote node's health `seat_rate` at its `seat_budget`, the local seat's store — and only falls back to this first-attempt value when the retry seat published none (register D-46). |
| `wall_note` | string | 0.115.21: the estimate's arithmetic (`wall 600 s is BELOW the estimate 733 s for agent-pool: cold load 210 s + one think block 4096 tok (137 s) + 11 tool steps × (128 tok + 6 s prefill) (113 s) + final 8192 tok (273 s) at 30.0 tok/s (store, 5 samples); min_turn 484 s`), or why there is none (`no decode-rate sample for … yet`). |
| `final_budget_fit` / `budget_note` | int / string | 0.122.1 (register D-95): the final-answer completion budget the run actually opened at, once the REMAINING wall was taken into account — never above the configured rule (4× the step budget, cap 8,192), floored at 1,024 — and the arithmetic behind the narrowing. Both absent when the seat has no measured rate or the configured budget fitted as it was. |
| `final_reissue` | string | Register D-95. `list_cap` is the only shape: a final answer cut at the completion budget on a SCHEMA contract is re-issued once, thinking off, at the same (fitted) budget, with the schema's own list caps spelled out ("cap every list at N items … keep every string under 200 characters"), before the run falls through to the re-pack. Present whether or not the re-issue then succeeded, so a first cut and a second are distinguishable without opening `calls[]`. Gated on the wall still holding one more turn AT THE FITTED BUDGET (`final_budget_fit` above) — since 2026-09-17 (register S-06/W-07): the gate used to size that one turn from the CONFIGURED final (up to 8,192 tokens) plus a full re-pack term, which below ~9 tok/s exceeded the 900 s wall cap and could never fire at all — the #1 measured defer fleet-wide ("re-pack skipped: the final answer was cut at the completion budget", 48 rows). |
| `repack_ms` / `repack_attempts` / `repack_note` | int / int / string | 0.115.23 (register D-91): how long the structured re-pack ran, how many seat completions it spent (two grammar attempts + the chat lane at most), and why it stopped or was skipped. A `length`-cut final answer (`output_truncated`) is never re-packed — the run abstains at once with `output failed schema: re-pack skipped …` and the partial in `output` — and no attempt starts with under a tenth of the wall (capped at 45 s) left (a 12 KB re-pack is a ~190 s re-generation on the 4B; three of them spent 690 s into a 900 s wall on 2026-09-10). Since 2026-09-17 (register D-85/D-108, S-21/W-19) the lane-probe closures behind the re-pack's client (a cascade lane's residency cache and local-busy check) are built ONCE per re-pack call and shared across every attempt instead of rebuilt fresh each time — before this, each of up to three attempts re-paid a live `/v1/models` + `/running` + per-model gauge read of the LOCAL seat before spending a token (measured fleet-wide: 1,301 rows, median 18 s, p90 109 s, max 581 s, 15.13 h total). Each attempt's own transport timeout is also bounded by `min(seat allowance, remaining wall / attempts still owed a turn)`, so one slow attempt can no longer sit on its full per-call allowance while the retry and the chat fallback are still due — for a caller with no liveness monitor. **Under the monitor (every node run, register C-66, PR-5/PR-12) the context owns the deadline and there is no transport bound at all:** the re-pack STREAMS, so every delta is progress and its stall allowance bounds silence, sized `max(120 s, 1.5 × expected answer tokens ÷ the seat's decode rate + 30 s)` (an unknown rate keeps the flat 120 s); a request the busy hold (ADR 0061) keeps is no longer cut a third of the way through what is left of the run and re-sent from the back of the engine's queue; and `repack_attempts` counts the requests actually SENT (it read 3 for the one real request of a stalled re-pack, on 119 of 119 stalled rows since 2026-09-20: anything that read `repack_attempts == 3` as exhaustion must change). The job record shows phase `repack` with its allowance and the streamed tokens. A stalled re-pack's reason names the arithmetic, or the flat bound and why it is the flat bound (no measured rate for the seat, or an unknown size). A seat that refuses streamed requests is asked again as JSON and remembered as JSON-only for 30 minutes; on it the re-pack is one non-streamed answer bounded by its allowance alone, and `repack_note` says `streaming refused by this seat` (on a success too) so the streaming fix not applying to that seat is never silent. When the DELEGATOR re-packed a failed one, `repack_note` reads `rescued on <seat> (…) after the node's re-pack failed: <the node's reason>`. **Bounded by the wall, and not resent blindly (register C-80, ADR 0055 item 9).** The wall is the run's expectation and its ceiling the only deadline, and nothing used to compare what a re-pack attempt asked for with the time left: on 2026-09-30 a seat at about 5.6 tok/s ended its loop 219 s into a 600 s wall and ran three attempts for 1,670 s on a 2,782-byte answer. Now, before EACH attempt the node takes the time left before `min(the run's ceiling, the wall's end + 30 s)` (the 30 s is the liveness slack, the padding the re-pack's own stall allowance already carries) and the seat's decode rate (the seat-rates store, else this run's observed rate, else the rate this run's own completions measured, else `agent_seat_tok_s`; with no known rate there is no arithmetic, and a FAILED re-pack then says `re-pack time bound off: no decode rate is known for this seat …` in `repack_note`). An attempt whose `max_tokens` fits is sent as sized; one that does not is sent with the tokens the time buys (`clamped_from` in its record), down to the answer's own size (`len/3 + 64` tokens); under that it is skipped, and so is every later attempt: `repack_note` reads `re-pack skipped: N s left to the wall + 30 s grace at R tok/s buys M tokens < the answer's K (rate: …)`. A skip with no attempt before it is a `budget` defer, reason `structured re-pack skipped: …`, with the finished answer in `output` and `schema_miss` set, so the delegator re-packs it on the doors that wire the rescue (`agent_delegate`, `offload_research`, the `delegate` and `research` verbs; a deferred `offload_ask` carries the answer as `output`, flagged `schema_miss`, and a review defer is bare because its filters never saw the raw prose); a skip after a failed attempt is a note on that attempt's own verdict. A loop that ends after the wall plus its grace therefore never gets a node-side re-pack, whatever the seat's speed or the answer's size, and its structured result depends on that rescue (one grammar completion on the delegator's own agent seat). An attempt the time left narrowed and the seat cut at that narrowed budget, with a tail that is no runaway, is the clock's verdict as well: a `budget` defer under the same `structured re-pack ` prefix, never an abstention (which the delegator retries on another node); a runaway tail stays an abstention. It is a token budget and a deadline check, never a transport timeout: a request in flight is not cut and the busy hold (ADR 0061) is unchanged. A grammar attempt cut at `max_tokens` is resent at the 8,192-token cap only when the budget was the problem: the budget is under the cap, the tail of what the seat wrote is not degenerate (only whitespace, or a block of up to 64 bytes repeated back from the end of the output across at least half of its last 256 bytes, or 32 bytes for whitespace alone, or a block of lines repeated), the answer needs more tokens than the budget held at the bytes per token the seat wrote (a token per three bytes at least), the wall did not set the budget (a clamped attempt is never resent), and the time left buys what the answer needs (a resend that would carry fewer tokens than the request just judged too small cannot finish, so it is not sent and the note carries the arithmetic); otherwise the loop goes straight to the chat lane, and the note says what was seen (`the output was degenerate (…)`, `the 1024-token budget was too small`, `the budget is already the 8192-token cap`, or `inside the … budget: the output ran on past it`). Both prompts name each field's type and a list's item type (the grammar lane names the types its grammar enforces: every array is a list of strings and an object a string; the chat lane and a vLLM seat's schema lane name the schema's own), the grammar's whitespace is bounded (the rule of llama.cpp's own `json.gbnf`: empty, one space, or one newline and at most twenty spaces or tabs; it reaches llama.cpp grammar lanes only, a vLLM seat's `structured_outputs` carries no whitespace control), and the scalar coercion turns a bare JSON number into the string a string field or list item asks for, keeping its text, and reads array items both ways, saying which fields it re-typed (`coerced to the schema's types: numbers (3 items)`) in the producing attempt's `why` and in `repack_note`. **Unverified:** that whitespace was what the incident's seat wrote was not confirmed (the request was not replayed) and the bounded repetition was not compiled against a live seat when this was written; the `head` and `tail` in `repack_attempts_detail` confirm or refute the first, the first grammar request on a live seat the second. The bound holds at request boundaries: the rate is tokens per second of call wall (prefill included) and a request in flight is never cut, so an optimistic rate can let one request overrun the wall plus its grace. Every attempt is on the wire as `repack_attempts_detail`. |
| `repack_attempts_detail` | list | Register C-80, additive and omitted when empty (a delegator that predates it ignores it). One record per structured re-pack attempt, in order: `attempt` (1-based; a skipped attempt carries the number it would have had), `lane` (`grammar`: a GBNF grammar on the completion route; `json_schema`: a vLLM seat's `structured_outputs`; `chat`: the grammar-free chat route), `max_tokens` (the budget the request carried; absent when skipped), `clamped_from` (the budget the time left narrowed it from), `tokens_out` (what the seat generated, whether or not the answer was usable), `finish_reason`, `ms`, `head` and `tail` (the first and last 80 bytes of what it wrote, whitespace kept; an answer of 160 bytes or less is all `head`), `skipped` and `why` (why it was skipped, or how a sent attempt failed; empty on the one that produced the object as the seat wrote it, and `coerced to the schema's types: …` on one whose scalars the node re-typed). `tokens_out`, `head` and `tail` are what the seat generated and wrote whether or not the answer was usable: for an attempt that died mid-stream (a stall, the ceiling, a dropped connection) they are the deltas heard (a lower bound) and what had arrived. `repack_attempts` still counts only the requests actually sent. The row an `agent_delegate` caller reads (`results[].repack_attempts_detail`) carries the first four records. |
| `calls` | `[{step, max_tokens, finish_reason, completion_tokens, reasoning_tokens, content_chars, reasoning_chars, tool_calls, thinking_off, reasoning_key, forced_final, ms}]` | 0.115.8 (register D-47): one entry per planner completion (0.115.19: `forced_final` marks the forced final step's call, D-89), on every result shape, set before the defer branches — the arithmetic a starvation diagnosis needs without transcript bytes. `reasoning_tokens` is vLLM's `usage.completion_tokens_details.reasoning_tokens` (0 = not reported). A pre-0.115.8 node emits none. Since 0.125.0 (register D-99) the DELEGATOR's published row `results[].calls` carries the LAST eight of these records (`omitempty`), so a caller reads them from `agent_delegate` / the CLI directly instead of from this endpoint with the fleet token. |
| `admission_wait_sec` | float | Everything spent BEFORE the wall started, as one number: the cordon wait, the llama-swap **swap pre-flight**, the seat's cold-load warm-up, the coherence probe and — since the S-24 fix — the served-window probe. All five draw on ONE budget (`agent_admission_wait_sec`, 0 = `core.AgentAdmissionSecDefault` = 300 s, −1 = off), so the ceiling is the budget and not the sum of five of them. Omitted when zero: nothing was swapping, the seat was already resident and the window read instantly. A job sits in state `running` for this whole window, which is why the delegator's poll bound carries a matching admission allowance. |
| `admission_note` | string | What admission DID or could not settle, `; `-joined across the steps that had something to say: `cold load Ns outside the wall`, `budget spent while <model>:<state> (proceeding into the wall)`, `running probe failed (proceeding): …`. Since the S-24 fix the warm-up also speaks from its **no-op** exits — `warm-up could not read /running (proceeding; the seat may still be cold)`, `no admission budget left for the warm-up …` — because "could not read" and "the seat is ready" used to be reported identically, and a still-cold seat reached the wall looking warm. Empty = every step settled cleanly. |
| `ctx_window_note` | string | WHICH window the loop budgeted against and WHERE it came from — the live probe, this box's `agent_ctx_tokens`, or the conservative 8,192 fallback (`agent.ResolveContextTokens`). Both doors discarded this line until the S-24 fix, so a run that silently compacted at 8,192 on a 131,072-token seat was indistinguishable on the wire from a correct one, and the only symptom was a task that compacted for no reason. When the fallback was caused by the probe running out of ADMISSION budget, the same sentence also appears in `admission_note`, because there the fix is a cold-start problem and not a window one. |
| `coherence_note` | string | Register D-118: the post-warm SEAT COHERENCE probe’s verdict — one ≤ 96-token completion, charged to `admission_wait_sec`, asking the freshly loaded seat to call `read_file` and answer DONE. `coherence probe: tool call parsed in Ns` (the seat is sane), `… answered in text without a tool call …` / `… inconclusive (…); proceeding` (fail-open), or `seat incoherent at warm: …`, which is also a `deferred` `infrastructure` result. Absent when the probe did not run (`agent_coherence_probe` `off`, a warm seat under the default `cold`, or a pre-D-118 node). |
| `seat_recoveries` / `seat_down_wait_sec` | int / float | ADR 0066, register C-72: how many times the run's seat went down under it and came back — counted when the re-issued call's first byte arrives, never for a re-issue that recovered nothing (the loop holds its transcript, so nothing done so far was lost) — and the wall spent waiting on a downed seat, the whole episode included the time llama-swap held the re-issued call while it started the seat. On a `seat down: …` defer `seat_down_wait_sec` is what the delegator credits back to the retry's budget (plus `admission_wait_sec`, capped at one contract wall). Omitted when zero; a result from a node without ADR 0066 reads as "not measured". |
| `deferred` | bool | True = the node ran and honestly could not complete the contract. **A defer is a success shape at the job level**: the job lands `done`, never `error` — `error` is reserved for internal wiring bugs (mirrors the cascade's defer semantics). |
| `reason` | string | Why it deferred (shapes below). |
| `schema_miss` | bool | Register C-66, PR-4. Set with `deferred` when the agent loop FINISHED: `output` holds its complete answer (never a cut one) and only the structuring failed — the re-pack stalled, was unreachable, was cut or answered a shape the schema refused. It tells the delegator it may re-pack the answer itself (`rescue`, [coding-agent](coding-agent.md#delegation-surfaces)) instead of counting a finished loop as lost work; the defer itself is unchanged, so an older delegator reads what it always did. A node that predates the field omits it and the delegator recognizes the same shape from `stop_reason: done` plus the reason prefix `structured re-pack ` or `output failed schema`. `omitempty`. |
| `defer_class` | string | The machine-branchable WHY: `abstention` \| `budget` \| `infrastructure` \| `config` \| `contract`. Additive and `omitempty` — a pre-0.65 node emits no class, and readers must treat empty as *unknown*, never as abstention. |
| `wall_ms` | int | Node-observed wall time. |
| `tokens_out` | int | Completion tokens the run generated: the loop's, plus the re-pack's. Every re-pack attempt counts, a failed one included (register C-80; before it, only an attempt that produced the object was added), and an attempt that died mid-stream counts the deltas it had streamed. |
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
| `agent seat "…" is not in the endpoint's served roster` | `config` | A *positive* roster miss. An unreachable or empty roster proceeds instead (logged), letting the loop's first call surface the real transport error. When the seat is not the config's own planner seat the reason ends with its source: ` - the seat was chosen by placement (layer "…", role "…"), not by this config's agent_model "…"` for a placed run, or `set by the caller or delegator` for an override (register C-95: a config copy that kept the node's `layers` deferred on a layer seat it never named). |
| unknown-profile message naming the valid profiles | `config` | The contract asked for a profile this build does not have. |
| `building agent: …` / `agent loop: …` | `infrastructure` | Build or planner failure — nothing was learned about the task. Carve-out since 2026-09-17 (register S-22/W-16): a PARENT cancellation mid-loop is its own shape below, not this one. |
| `seat incoherent at warm: …` | `infrastructure` | Register D-118. The post-warm coherence probe asked the freshly loaded seat for one `read_file` call and got the NaN shape back: ≥ 20 identical non-whitespace bytes in a row, an unparsed tool-call marker with no parsed call, or nothing at all at the 96-token cap AND no hidden reasoning reported (a thinking seat cut inside its think block proceeds with a `cut inside the think block` note — the loop calls that same completion reasoning starvation, not a broken seat). It fires BEFORE the wall starts, so the contract spent seconds, and it is one of the `infrastructure` defers the delegator retries on another node (`delegate.IncoherentSeatDefer`; the others are `seat warm-up failed:` and `seat down:`) — the fault is a property of that seat, and what the node's admission spent before it (the cold load that triggered the probe) is credited back to the subtask's `timeout_sec` ledger so the retry floor does not refuse the retry. A broken verdict is also remembered for 10 minutes per endpoint+seat, so the next WARM contract on the still-resident seat defers on it without spending a probe. Still a broken stack: an operator has to fix the box. |
| `seat down: …` | `infrastructure` | ADR 0066, register C-72. The seat's engine went down under the run and did not come back inside the recovery wait: **wedged** (its counters stayed flat for the flat bound while llama-swap listed it ready — the text carries the engine-flat arithmetic and the engine's running/waiting gauges) or **died** (it left llama-swap's `/running` inside a hold, or a call failed like a dead seat and a fresh read shows the seat starting, stopping, unlisted or refusing connections). Before it is filed the node cancels only the call in flight, waits for the seat under the cold-load hold as one bounded episode (a seat llama-swap does not list is started by the re-issue, and a start that fails is retried at the poll until the bound) and re-issues the step (`seat_recoveries`); the text says when the seat "did not come back after N start attempt(s) (waited Ns)" and what it last looked like, or when a seat that was seen serving "went down again" under the re-issued step. The delegator re-places this defer on another node (it is the third retryable `infrastructure` defer, after `seat incoherent at warm:` and `seat warm-up failed:`) and credits back the wait. A seat lost during the structured re-pack files the same prefix with `(during the structured re-pack)` appended and the finished answer in `output`, flagged `schema_miss`: a delegator that can re-pack the answer itself does that first and re-places the contract only when it cannot (a seat lost there reports no wait of its own unless the loop had already waited out an earlier outage, so the wall a failed rescue spent is credited back to the re-placement too). A thrash stays `stalled: …`. A node without ADR 0066 files the same outage as `stalled: …`. The delegator re-places the defer even when the dead seat's own `min_turn_sec` exceeds the budget and even when the alternative node is at its ceiling (the node's queue is the line: a 503 is re-placed at once, and a fleet with no room holds the contract in the bounded capacity wait of ADR 0063); while the local seat's run-cap line has no free slot the retry goes to an untried remote rather than join it; when there is no other node the `retry_note` says so (or says that the call's deadline had passed before a node was chosen). Under a whole-call deadline (ADR 0065) the re-placement is cut like any other attempt, and a `seat down:` defer that is produced after the deadline is the deadline's budget defer (its reason quotes `the run itself reported infrastructure: seat down: …`), not re-placed. |
| `seat not serving: llama-swap answered HTTP <5xx> …` | `infrastructure` | ADR 0066. A 5xx from llama-swap that outlived the busy-seat budget while the seat was NOT known to be down (a start that failed, a health check that timed out): NOT contention, so `concurrencyLimit` is not the knob. While the seat reads starting or unlisted the same failure is a `seat down:` instead and never spends the contention budget, in the loop and in the structured re-pack alike. `seat contended: …` is now a 429 only. |
| `wall timeout after <N>s` | `budget` | The CALLER's deadline fired (`wall timeout after <N>s (the caller's deadline, not this node's ceiling)`), in the loop **or** in the re-pack; the node's own run ends on a stall or on its ceiling instead (ADR 0055). Its own shape so the delegator can size future contracts off it. |
| `step budget exhausted (<n> steps)` (since 0.115.19 `: <stop_note>` appended when the forced final step got a tool call) | `budget` | The loop burned `max_steps` with no final answer; `output` is empty, so there is nothing to re-pack. Since 0.115.19 the last step already asked for the answer with no tools offered, so this shape now means the seat answered even that step with a tool call (never executed; `stop_note` carries it). |
| `empty final answer after <n> steps and <t> completion tokens: <stop_note>` | `budget` (stop `reasoning_starved`) / `abstention` (stop `empty`) | 0.115.8. The loop's final completion carried no visible content on the first attempt AND on its one re-issue with thinking off at the final budget. `budget` when the completion budget went to the think block (vLLM `reasoning` / `reasoning_tokens ≥ 0.9 × completion`, or `finish_reason: length`), `abstention` when the seat had room and said nothing. Never re-packed: before 0.115.8 this shape reached the re-pack as `""`, came back as a schema-valid all-empty object, failed acceptance, and was retried cross-seat with the wall's leftovers (2026-09-10: 20,526 tokens on <node-b> 27B for zero visible characters). |
| `output failed schema: …` | `abstention` | The re-pack's FINAL, DECISIVE attempt (the last of up to three it ran: two grammar completions then the chat fallback — register D-108, PR #366 correctness review) reached the seat and the answer was unusable — a validation failure, a **non-429 4xx** (the seat refusing *this* request: context length exceeded, an uncompilable grammar), or a 200 carrying zero choices. All three are the box answering; the fix is a smaller context or a flatter schema, not an operator. Any EARLIER attempt's own failure — including a genuine transport failure or a self-imposed cutoff — rides in the message as a note (`attempt N/3 (bound …): …`), never as what decides the class: only the LAST attempt that ran does that. |
| `structured re-pack unreachable: …` | `infrastructure` | The re-pack's FINAL, DECISIVE attempt could not REACH the seat, or the seat was not what answered: a dial/transport failure, a **5xx**, a **429** (llama-server does not rate-limit — a 429 means something in FRONT of it answered), or a **body that could not be read or parsed** (`llama-server response body unusable: …`). The last shape covers a proxy or captive portal returning HTML with a 200, and a connection dropped mid-body: both happen AFTER the request succeeds, so no `*url.Error` / `net.Error` exists to catch them, and all three used to be filed as abstentions at exit 0. Split from the schema shape deliberately — filed under `output failed schema:` a llama-swap outage reads as a model that cannot follow a schema. **No longer sticky across every attempt** (changed 2026-09-17, register D-108, PR #366 correctness review): a 5xx on an EARLIER attempt followed by a wrong-shape answer on the FINAL one now files as `output failed schema:` (abstention) — the seat did, in the end, answer — while a genuine 5xx/429/dial-refusal on the FINAL attempt still files here regardless of what earlier attempts did. A self-imposed per-attempt-bound cutoff (`client.Timeout` firing on a request this box deliberately narrowed) is explicitly EXCLUDED from this shape even on the final attempt — see `structured re-pack attempt N/3 cut by its …` below — because a timeout is never proof the box is broken. **This is the one broken-stack shape that carries a POPULATED `output`**: the agent loop already finished, so its prose is preserved (`agenttask.go` sets `wire.Output` before the re-pack, and every failure branch below keeps it, so the CALLER still receives the loop's answer — delegator-side acceptance does not read it, since acceptance runs only when `deferred` is false) while `structured` stays absent. It is still counted into `summary.lost_to_stack` — a contract with an `output_schema` asked for a mechanically checked deliverable, and unchecked prose is not one, unless the delegator's rescue re-packed it (`schema_miss`, above), in which case it is a success and is not counted. |
| `seat warm-up failed: …` | `infrastructure` | Register C-76 (R-05a), only with `agent_warm_failure_defer: true`. The admission warm-up request was refused with a **5xx** and there is positive evidence the seat's process did not start: llama-swap's answer is not one of its busy shapes (429, 503 `process is not ready`, a 500 of its own, a health check that timed out, an empty 502 — a seat PEERS hold is a place in line, never a refusal), `/running` lists no row for the seat and nothing else is mid-swap, and `/running` could be read (an engine that exits at start answers 500 `upstream command exited prematurely`). Deferred within about one poll interval, before any wall exists and with the loop never started, where admission used to proceed into the wall and spend all of it on a seat that could not answer. The status is in `admission_note`. The delegator gives the defer one retry on another node (like the coherence defer), crediting the admission the node spent. With the flag off (the default, the audit mode of the house security standard) the run proceeds as it always did and `admission_note` says `this run would have deferred at once`, so would-be defers can be counted first. A 404 (the name is unknown here) and a timeout still proceed with the note, and so does a 5xx while `/running` reads the seat ready, starting or stopping. A non-200 answer that loaded nothing is no longer reported as an attempted load (the coherence probe is not asked, no cold load is recorded against the seat). The `agent_run` door keeps proceeding on a 5xx. |
| `structured re-pack attempt <n>/3 cut by its <bound> share of the wall` | `budget` | 2026-09-17 (register D-108, PR #366 correctness review). The re-pack's FINAL, DECISIVE attempt was ended by its OWN per-attempt bound (`min(seat allowance, remaining wall / attempts still owed a turn)`) — indistinguishable on the wire from a real hang (both surface as a `*url.Error` whose `Timeout()` is true, or wrap `context.DeadlineExceeded`), but never evidence the box is broken: filing a self-imposed cutoff as `structured re-pack unreachable:` (infrastructure) recreates, one arm over, the exact defect this PR already fixes for a canceled parent context (S-22/W-16). A dial-refused or connection-reset failure has NEITHER shape and still files as `structured re-pack unreachable:`. Any earlier attempts' own failures ride in the message as notes, same as the other two shapes. |
| `structured re-pack skipped: <N> s left to the wall + <G> s grace at <R> tok/s buys <M> tokens < the answer's <K>` | `budget` | Register C-80. The finished answer's re-pack was never sent (or not sent again): the time left before the wall's end plus the liveness slack, at the seat's decode rate, could not buy even the answer's own size in tokens. Nothing was asked of the seat, so it is not an abstention (which the delegator retries on another node) and not infrastructure; the answer rides in `output` with `schema_miss`, and the delegator re-packs it before it counts the contract lost, on the doors that wire the rescue (`agent_delegate`, `offload_research`, the `delegate` and `research` verbs). When an earlier attempt had already failed, its own verdict stands and the skip is a note inside it. |
| `structured re-pack attempt <n>/3 (…): re-pack truncated at <T> tokens (… the time left set this budget: max_tokens <B> clamped to <T>: …)` | `budget` | Register C-80. The LAST attempt was cut at a budget the time left had narrowed, and its tail was no runaway: the clock decided how much the seat was given, not the seat and not the schema. Filed as an abstention it would be retried on another node, a failure charged to a seat that was never given room to finish. The answer rides in `output` with `schema_miss`, as for the skip above. A cut with a degenerate tail (whitespace, a loop) stays an `abstention`: that one is the seat's. |
| `this node does not open the write door: agent_allow_write is false …` | `write` | 0.122.0, register D-06. The contract asked for `write_root` and this node has not opted in. Its own class because it is neither a broken stack (nothing is wrong with the box) nor an unplaceable contract (another node may have opted in) — the delegator's right reflex is to re-place. On the FLEET path this never appears: `buildAgentRun` refuses at ack with a 400 so the re-placement happens there; the defer is the in-process local path, which has no ack hop. |
| `write door: … over the N-file cap` / `… over the N-byte cap` | `write` | The run finished and its write set is past a door cap. **No diff is published** — a truncated or partial patch applies as silent damage. The fix is a smaller leg, not an operator. |
| `agent loop: canceled (the parent context ended — the caller gave up, or this box is draining)` | `budget` | 2026-09-17 (register S-22/W-16). The PARENT context was canceled mid-LOOP — the mirror of the re-pack's own arm below, added to the loop branch which fell through to the generic `agent loop: …` infrastructure shape instead: the failed request looks exactly like a dial refusal (a `*url.Error`), so an operator used to be told to fix a box that never misbehaved (12 `agent loop: context canceled` rows filed as broken hardware). Nothing on the box failed. **The reason names no single cause, on purpose (PR #366 review):** on the FLEET NODE path the only thing that ever cancels this context is `fleetnode.Jobs.DrainAndStop` on a node drain — never a caller abandoning a poll — and that path's own mark (`state=error, err="interrupted"`) is written BEFORE the cancel that releases the run; `finish()` is write-once against an already-terminal job, so **this defer's words never reach a delegator polling a drained node at all** — the delegator reads `interrupted`, not `budget` (proven in `internal/fleetnode/jobs_test.go`, `TestJobsDrainDiscardsALateAgentBudgetDefer`). Only the LOCAL in-process path (`RunAgentContract`, no `Jobs` store in front of it) can ever surface this string to a human, and there a cancel genuinely is the caller's own context ending — hence the wording names both possibilities rather than asserting the one that is false on the fleet path. |
| `canceled during the structured re-pack (the caller's context ended)` | `budget` | The PARENT context was canceled mid-re-pack (the delegator abandoned the poll, the node is shutting down). It arrives as a `*url.Error` exactly like a dial refusal, so it used to read as broken infrastructure — but nothing on the box failed. |

More shapes originate on the **delegator**, not the node:

- `queue deadline after <d>: the node accepted the job but never started it …` — a **FAILURE**
  (`summary.failed`), not a defer. The node admitted the job and every poll answered `accepted`:
  it sat in the backlog and was never given one of the node's concurrency slots. Introduced with
  the node-side queue in 0.100.0, because until then `accepted` lasted microseconds and this state
  could not persist. Two properties make it safe: **queued time is credited back** to the
  execution deadline (a job that waits and then runs is never penalised for the wait — the
  contract's `timeout_sec` is a budget for work), and the wait itself is **bounded** by the node's own ETA
  (`clamp(1.5 x etaStart + 30 s, 60 s, timeout_sec + grace)`, ADR 0063; `min(timeout_sec + grace, 5 minutes)` for
  a node that publishes no ETA). It is a failure rather than a defer on purpose — a defer
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
never arrived, published separately for exactly this reason), and only when nothing succeeded
(a partial result is a successful call, C-75, ADR 0065), rather than on the whole of
`summary.infrastructure` — see [coding-agent](coding-agent.md#delegation-surfaces). The counted
set includes the `structured re-pack unreachable` shape, whose `output` is populated: what was
lost is the schema-checked deliverable, not the bytes — unless the delegator's rescue (`schema_miss`,
above) re-packed it, in which case it is a success and is not counted.

### Auth (v1 scope: the agent lane — joined by the vision lane in 0.116.0)

`fleet_auth_token`, when set, bearer-gates exactly two lanes: agent dispatches (and the vision
and text lanes' `POST /fleet/vision` and `POST /fleet/text`, which ride the same rule through
`tokenGated`), and
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

**Because media dispatch is tokenless, a remote media task never picks the node's output path.**
The builders in `internal/fleetnode/tasks.go` and `compose_task.go` drop a caller's `out`, and
run-graph's `out_dir`. The pipeline then writes `<media_dir>/<task>-<hash8>.<ext>` (run-graph:
`<media_dir>`). Results come back by bare name through `GET /fleet/media/{name}`, which serves only
files directly inside `media_dir`, so no working caller ever depended on a path outside it. A stray
key is ignored, not refused. `TestRemoteMediaTaskOutNeverReachesThePipeline` tries absolute,
`..`, UNC, drive-relative, existing-file and bare-name targets against every writer.
`TestEveryFleetWriterIsCoveredByTheOutRule` fails when a new file-writing task is not in that
table. The local MCP and CLI doors keep `out`, because they are trusted callers on the box.

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
| `agent_seat_resident` | Roster-**verified**: a cached probe of llama-swap's `/v1/models` (alias-aware) saw the seat. The cache refreshes in the background at most once per 30 s; the handler blocks on llama-swap only for a too-stale, never-probed or invalidated cache, and then for at most `residencyWaitBound` (1.5 s) — see the seat-state section above (0.128.2). |
| `served_models` (0.113.0) | The same cached probe's full roster name list — **canonical ids AND every alias** (`swapclient.Roster.Names`, not `IDs` alone) — omitted/empty on a cold cache or a failed fetch (unknown, never a stale list). `internal/delegate/gate.go`'s `seatServed` uses this to check the roster actually names `agent_seat`, a stronger check than `agent_seat_resident` alone: a node can be roster-resident under one alias while its `served_models` list shows a different one after a rename. Publishing aliases too matters because an agent seat is normally bound BY alias (`agent-pool` -> `qwen3.8-27b-vllm`, `offload-e4b` -> `gemma-4-e4b`); an id-only list would have made a correctly-served alias seat read as unserved. A pre-0.113.0 node/delegator pairing is unaffected: an unpublished (empty/absent) `served_models` reads as UNKNOWN, never a refusal. |
| `tiers` / `layers` (0.123.2, composite only) | What this node IS (every tier it is a complete instance of) and what it can PLACE ON: one row per device layer with the declared seats, each seat's `served` flag from the same cached roster, and the node's own `admissible`/`reason` verdict from its guards. Absent on a plain node; built from cached reads only, so health still never probes. See [composite-tier.md](composite-tier.md). |

Residency **fails closed** twice over: until the first probe lands the answer is `false` (since
0.128.2 the first read WAITS for that probe, bounded, so on a responsive box it is already `true`;
`false` is what a probe that misses the bound leaves behind), and a
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
2b. **The local run cap** (`modelaffinity.AwaitSeatSlot`, register C-42, 0.130.1). `fleet_max_concurrent_jobs`
   caps the jobs the fleet SENDS to this node ("queue full" 503); nothing capped the runs the box starts on
   its OWN seat, and the run registry gated nothing by itself — so sixteen could land on one seat and spend
   their walls inside the engine's queue. Both locally-started doors now take this wait: the MCP `agent_run`
   handler and the pipeline's contract runner (`internal/pipeline/agenttask.go` — a delegation's local leg and
   every fleet job alike). It waits, inside the same admission budget, while the registered runs on the seat
   (this run's own record excluded) number `FleetConcurrencyLimit` (default 4) or more; a slot that never frees
   is a capacity defer (`seat busy: …`, re-placeable), never a refusal, and `admission_note` reads `held at the
   seat cap for the admission budget`. `fleet_max_concurrent_jobs: -1` disables both caps.
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

## The media-job door, artifacts and honest advertisement (ADR 0072)

Three additions to the media tasks, all additive on the wire.

**The media-job door.** `POST /fleet/media-job` (task `media-job`) takes one `image-gen`, `video-gen`, `animate`,
`audio-gen` or `run-graph` job together with the input files it reads, from a holder of the fleet token. The body is
`{job_id, task_type, payload, bundle?, bundle_sha256?, inputs?}`: `payload` is the inner task's payload exactly as
`/fleet/dispatch` takes it, `bundle` is base64 of a gzip-tar of the files, and `inputs` maps a payload field to a bare name
in the bundle. The fields that may be files are `video-gen.still`, `animate.ref`, `animate.driver` and `audio-gen.clone`.

- It is open only when `fleet_media_inputs` is true, `fleet_auth_token` is set and a media task is bound
  (`config.MediaInputsAdmissible`; health lists `media-job` only while one inner task is also runnable). Closed, it answers
  403; without the bearer, 401; both before a byte of the body is read. `media-job` is token-gated, so the same task over
  `/fleet/dispatch` needs the bearer too, and its jobs are masked from tokenless polls and feeds.
- The body is capped at `fleet_media_inputs_max_mb` (default 256, compressed) in base64 plus 64 KiB (413 over it), is JSON
  only, and unknown fields are a 400. A token holder gets a 15-minute read and write window; a writer that cannot carry the
  extended deadlines is logged once per process per route. The body is held once while the job is admitted (the bundle is
  base64-decoded straight out of it into the one decoded copy, and the admission closure keeps only the job id and task type),
  so a running or queued job pins neither the body nor the bundle: the extracted directory is the only copy.
- The bundle's sha256 must match; it is extracted into `<media_dir>/fleet-inputs/in-*` (regular files only, confined names,
  byte caps; a symlink or a traversal name is refused); each `inputs` value must be a regular file directly in that
  directory; and its first bytes must match the field's kind: image PNG, JPEG or WebP; video MP4/MOV or WebM/MKV; audio WAV,
  FLAC, MP3, OGG or M4A. A payload that names a node-local path in a file field, in any casing (`Still` fills `still` in the
  builders), is refused: this door carries bytes, never paths; a shipped file replaces every spelling of its field. The inner task is built by the same builder `/fleet/dispatch` uses, with each field rewritten to the extracted path.
- The directory is removed when the job ends and on every refusal. `fleet-serve` removes `in-*` directories older than the
  longest media timeout plus an hour at startup. A node-side failure (a disk that filled) is a 500, not "refused".

**Artifacts.** When a media job finishes, its stored `data` gains `artifacts: [{name, bytes, sha256}]`, one per output the
result names (`image_path`, `video_path`, `audio_path`, run-graph `outputs`) that is a regular file directly inside
`media_dir`. A path outside it, a symlink or a file that cannot be hashed is left out and never fails the job. The poll
returns it with the rest of `data`, so the machine that fetches `GET /fleet/media/{name}` can verify the bytes.

**Honest advertisement.** `video-gen`, `animate`, `audio-gen` and `run-graph` are advertised, and admitted, only while
`internal/mediacap` reads the matching route as CONFIGURED: `generate_video`, `animate_character`, `run_graph`, and for
audio any of `generate_audio:voice`, `generate_audio:voice:endpoint` and `generate_audio:music`. A bound script over a
missing weight, VAE or custom node is BOUND-BUT-MISSING and the task drops out; restoring the file brings it back. One
predicate (`taskConfiguredFor`) serves health and dispatch, so a node never lists what it would refuse. `image-gen` keeps
`ImageGenAdvertisable`. `/fleet/health` gains `media_routes: [{route, engine, state}]` (every task route, not the shared
prerequisites; the `detail` paths stay on the node), and `supported_task_types` and `loadable_model_families` are derived per
request from the same reading, which is cached for at most 60 seconds per config and read once per health request and once
per admission (the node keys its cache once, at construction; the pull claim loop re-derives its task list on every claim). A
media task the node binds whose route is not CONFIGURED is refused at admission with a `503` naming the route and its
state (`task_type "video-gen" is bound on this node but its route is not ready: generate_video BOUND-BUT-MISSING ...`), which
every delegator re-places; a task that is not bound at all keeps the `400 unsupported task_type`. A delegator reads `media_routes` through
`delegate.NodeView` (`MediaRoutes`, `RouteState`); an absent field is unknown, never "no route".

## Source map

- [`internal/fleetnode/server.go`](../../internal/fleetnode/server.go) — routes, payloads, duplicate
  semantics, agent-lane auth gates, agent health advertisement
- [`internal/fleetnode/leases.go`](../../internal/fleetnode/leases.go) — `leases[]`, the folded singular block, the
  node-level closed reading and the per-contract text-reservation gate (GPU routing P7)
- [`internal/placement/leasecontract.go`](../../internal/placement/leasecontract.go) — which leases stand between a
  contract and its seats: the chain reading the delegator, the node and a remote's rows share
- [`internal/fleetnode/auth.go`](../../internal/fleetnode/auth.go) — the bearer credential check
- [`internal/fleetnode/media_job.go`](../../internal/fleetnode/media_job.go) — the media-job door: the body, the
  bundle checks, the magic-byte sniff, the sweep of orphaned input directories
- [`internal/fleetnode/media_artifacts.go`](../../internal/fleetnode/media_artifacts.go) — `artifacts` on a finished
  media job
- [`internal/fleetnode/media_ready.go`](../../internal/fleetnode/media_ready.go) — the cached mediacap reading behind
  the honest advertisement and `media_routes`
- [`internal/mediaremote/`](../../internal/mediaremote/) — the client that places a media job on a node
- [`internal/fleetnode/jobs.go`](../../internal/fleetnode/jobs.go) — state machine, the admit-then-
  schedule queue and its concurrency limit, eviction, drain, the agent job marker
- [`internal/fleetnode/tasks.go`](../../internal/fleetnode/tasks.go) — `agentTaskConfigured`,
  `buildAgentRun` (contract decode, depth derivation, context materialization),
  `SweepOrphanedPipelineJobs` (the startup sweep and its ownership rules)
- [`internal/jobdir/`](../../internal/jobdir/) — the owner marker of a delegator-side job dir under
  `pipeline-jobs/` (`.owner`, the writer's process id), the `agent-local-` name prefix and
  `MaxRunLifetime`; `pipeline.RunAgentContract` writes it and the sweep reads it (leaf, standard
  library only)
- [`internal/delegate/run.go`](../../internal/delegate/run.go) — the delegator: placement, re-placement, the
  capacity wait, the spread deal, the dispatch and poll loop (ADR 0063)
- [`internal/delegate/eta.go`](../../internal/delegate/eta.go) — expected-completion ranking, the backlog gate
  and the ETA-derived queue budget
- [`internal/delegate/processgate.go`](../../internal/delegate/processgate.go) — the process-wide in-flight
  gate and the per-page retry cap
- [`internal/core/agentwire.go`](../../internal/core/agentwire.go) — contract, result, acceptance DSL
- [`internal/pipeline/agenttask.go`](../../internal/pipeline/agenttask.go) — node-side execution,
  structured re-pack, defer shapes
- [`internal/pipeline/coherence.go`](../../internal/pipeline/coherence.go) — the post-warm seat
  coherence probe (register D-118) both agent doors run
- [`internal/fleetnode/footprints.go`](../../internal/fleetnode/footprints.go) — padding, merge,
  persistence
- [`internal/fleetnode/vram.go`](../../internal/fleetnode/vram.go),
  [`vram_windows.go`](../../internal/fleetnode/vram_windows.go) — the two sampling paths
- [`internal/fleetnode/vram_uma_meminfo.go`](../../internal/fleetnode/vram_uma_meminfo.go) — the
  linux-meminfo memory provider of a unified-memory SoC tier (`rockchip-rk3588`)
- [`internal/gpuprobe/`](../../internal/gpuprobe/) — the nvidia-smi command + per-device parser and
  the host free-RAM reader (leaf; fleetnode's `GPUDevice`/`ParseSmiMemoryDevices`/`HeadlineDevice`
  alias it)
- [`fleet_reclaim.go`](../../fleet_reclaim.go) — `oursLoaded` / `anyReclaimable`: the keep-set
  and memory-stack classification above, over `pkg/llamaswap`'s `Running()` + `IsProtected()`
- [`main.go`](../../main.go) — `fleet-serve` / `fleet-measure` verbs

## Related docs

- [../FLEET-NODE.md](../FLEET-NODE.md) — operator guide
- [../flows/fleet-job-lifecycle.md](../flows/fleet-job-lifecycle.md)
- [../architecture/decisions/0072-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md](../architecture/decisions/0072-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md)
- [../architecture/decisions/0008-pdh-primary-vram-sampling.md](../architecture/decisions/0008-pdh-primary-vram-sampling.md)
- [fleet-overview.md](fleet-overview.md) — the delegator-side operator page that reads these health
  and jobs fields
- [../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md](../architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md)
