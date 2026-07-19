---
status: Accepted
date: "2026-07-18"
---

# Per-process PDH counters as the primary footprint source

## Context

A fleet dispatcher decides which node can take a job by comparing the job's expected VRAM cost
against a node's free VRAM. That requires knowing what a render actually costs — per model family,
measured, not guessed.

Measuring it on Windows is harder than it sounds. Consumer GeForce cards with a display attached run
under **WDDM**, where NVML's per-process VRAM accounting returns N/A. `nvidia-smi` can therefore only
report *global* memory on these machines. Global memory includes the desktop compositor, the browser,
and anything else on the box — so attributing it to our render is wrong by an unpredictable margin.

## Decision

Two different questions get two different sources, and conflating them is the mistake this ADR
exists to prevent.

**Live node capacity — `nvidia-smi` only.** The `vram_total_gb` and `vram_free_gb` fields in the
fleet health payload come from a global sampler polling
`nvidia-smi --query-gpu=memory.total,memory.used` every two seconds. There is no PDH path here, and
no fallback: a sampling failure keeps the last good snapshot rather than publishing zeros, and the
health endpoint returns 503 once that snapshot ages past 30 seconds. Reporting stale-but-true beats
reporting a confident zero, and refusing to answer beats reporting stale-and-unmarked.

**Per-render footprint measurement — PDH primary, global-delta fallback.** On Windows the sampler
reads the `\GPU Process Memory(*)\Dedicated Usage` performance counter, enumerates instances, and
sums only those belonging to the render's own process tree. This is the only per-process option that
works under WDDM. Elsewhere — and when explicitly configured — a global-delta sampler subtracts a
baseline captured before the render loaded anything.

**Advertised footprints are max-kept.** A new observed peak ratchets the footprint up to the worst
observation rather than averaging toward optimism. Only successful renders with a positive peak are
recorded, because a failed run's peak may be partial.

> **Revised 2026-07-18 by [ADR 0013](0013-nodes-advertise-raw-footprint.md).** This ADR originally
> had the node pad the advertised value by ×1.2 (`round(observed × 1.2, 0.1)`). That was reversed:
> the node now advertises the **raw** observed peak and the dispatcher owns all routing margin —
> node ×1.2 on top of the dispatcher's margin double-inflated footprints. The PDH/sampling core of
> this ADR is unchanged; only the padding is superseded.

**Footprints merge across processes by file mtime.** `fleet-measure` run while `fleet-serve` is
already serving would otherwise be invisible to the running node; a stat-and-reload before each read
merges the other process's records, comparing raw observations rather than padded values.

**MSI Afterburner is a validation companion, never a dependency.** The harness imports nothing from
it, reads nothing from it, and every fleet feature works without it. It is used once at bring-up to
eyeball the per-process plot against measured values; agreement within 15% means the PDH path is
trustworthy on that machine, and worse than that is the documented signal to set
`fleet_sampler: "global"`.

## Consequences

- Footprints reflect our job's real cost on the machine that will run it, not a number copied from a
  spec sheet.
- Footprints are the raw observed peak (see the ADR 0013 revision note above); the dispatcher applies
  the routing margin, so a node never double-inflates its own footprint.
- **The sampler config value is not validated.** The selection predicate is `!= "global"`, so `"pdh"`
  and `"auto"` behave identically, and `"pdh"` on a non-Windows host silently yields the global-delta
  sampler. A typo in this field selects PDH rather than erroring.
- A machine whose PDH counters disagree with reality has a documented escape hatch, and a documented
  procedure for discovering that it needs one.
- The counter set is known to report bogus values for the desktop compositor instance. This is
  harmless here because the tree-sum excludes it, but anyone reading raw counter output should expect
  it.

## Alternatives considered

- **NVML per-process accounting.** Rejected because it does not work: WDDM makes it return N/A on
  exactly the hardware this runs on.
- **Global `nvidia-smi` for footprints too.** Rejected as the primary: it charges our job for the
  desktop's memory. Kept as the fallback, where a before/after delta makes it defensible.
- **Depending on Afterburner's shared-memory export.** Rejected: a third-party GUI application must
  not be a runtime dependency of a headless node. It earns its place as a cross-check instead.
- **Advertising raw observed peaks.** Rejected: a sampler polling at 500 ms will miss transients, and
  under-reporting VRAM causes a dispatcher to overcommit a node.

## Related code

- [`internal/fleetnode/vram_windows.go`](../../../internal/fleetnode/vram_windows.go) — PDH
  enumeration and process-tree filtering
- [`internal/fleetnode/vram.go`](../../../internal/fleetnode/vram.go) — global sampler, keep-last
  behavior
- [`internal/fleetnode/footprints.go`](../../../internal/fleetnode/footprints.go) — padding,
  max-keep, mtime merge, atomic persistence
- [`internal/pipeline/pipeline.go`](../../../internal/pipeline/pipeline.go) — sampler selection

## Related docs

- [../../FLEET-NODE.md](../../FLEET-NODE.md) — the operator-facing validation procedure
- [../../systems/fleet-node.md](../../systems/fleet-node.md)
