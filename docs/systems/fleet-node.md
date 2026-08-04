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

## Scope

The HTTP contract surface, the job state machine and its idempotency semantics, VRAM sampling,
footprint measurement and persistence, and node startup and drain.

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
| `GET /fleet/health` | Node identity, GPU vendor and architecture, live total and free VRAM, supported task types, loadable model families, measured footprints, queue depth |
| `POST /fleet/dispatch` | Submit a job; returns `202` with an ack |
| `GET /fleet/jobs/{id}` | Poll job state and result |

The dispatch envelope is parsed **strictly** — unknown fields are rejected, and the body is capped.
Several contract-reserved fields are accepted and ignored, so the contract can grow without the node
needing to change first.

**Job states are `accepted` → `running` → `done` | `error`.** Terminal states are write-once: a late
completion cannot overwrite a finished job. Terminal entries are evicted after a TTL by a periodic
janitor, and `queue_depth` counts only non-terminal jobs.

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

## Security and privacy notes

The contract is unauthenticated and assumes a trusted network — in practice a tailnet. That
assumption is acknowledged by the explicit `--listen-trusted-network` flag. Node identity defaults to
the hostname, so operator documentation uses placeholders rather than real names.

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
PDH instance parser. `fleet_verbs_test.go` covers parameter resolution and the bind guard.

## Common pitfalls

- Believing the per-process PDH tree supplies health's VRAM numbers. It does not — that is the resolved provider (`nvidia-smi`, or the ADAPTER-level WDDM counters on the generic path).
- Expecting `queued` as a state. The first state is `accepted`.
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
three model loads. Seats llama-swap reports with `ttl 0` (no auto-unload) are therefore
treated as part of the baseline. Without that rule a correctly configured node — which
never reaches "nothing loaded" — would report `unknown` forever.

**Unknown is published as absence.** Before any idle baseline has been observed, both
numbers are OMITTED and only `vram_reclaim_source` is sent, explaining why. A consumer
falls back to `vram_free_gb`. Over-promising costs a failed job; under-promising costs a
scheduling opportunity, so the rule is deliberately asymmetric: the node never claims
reclaim capacity while holding nothing.

`harness_version` ships in the same payload — node/repo drift used to be found by hand,
and a node several releases behind gets debugged against known-fixed bugs.

## Source map

- [`internal/fleetnode/server.go`](../../internal/fleetnode/server.go) — routes, payloads, duplicate
  semantics
- [`internal/fleetnode/jobs.go`](../../internal/fleetnode/jobs.go) — state machine, eviction, drain
- [`internal/fleetnode/footprints.go`](../../internal/fleetnode/footprints.go) — padding, merge,
  persistence
- [`internal/fleetnode/vram.go`](../../internal/fleetnode/vram.go),
  [`vram_windows.go`](../../internal/fleetnode/vram_windows.go) — the two sampling paths
- [`main.go`](../../main.go) — `fleet-serve` / `fleet-measure` verbs

## Related docs

- [../FLEET-NODE.md](../FLEET-NODE.md) — operator guide
- [../flows/fleet-job-lifecycle.md](../flows/fleet-job-lifecycle.md)
- [../architecture/decisions/0008-pdh-primary-vram-sampling.md](../architecture/decisions/0008-pdh-primary-vram-sampling.md)
