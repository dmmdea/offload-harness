---
status: Accepted
date: "2026-07-18"
---

# Nodes advertise the raw observed footprint; the dispatcher owns all margin

## Context

A fleet node measures the VRAM peak of each render and advertises it in `/fleet/health`'s
`model_footprints[]` so a dispatcher can decide whether a job fits. [ADR 0008](0008-pdh-primary-vram-sampling.md)
had the node **pad** that value — `vram_peak_gb = round(observed × 1.2, 0.1)` — as headroom against
sampler variance.

The dispatcher, independently, applies its **own** margin when it decides placement (a multiplier
plus a fixed reservation). With the node padding by ×1.2 and the dispatcher then applying its own
×1.2 + fixed offset, the effective margin compounded: `observed × 1.2 × 1.2 + offset`. That
double-inflation pushed the advertised-plus-margin cost of the large models past a 16 GB node's
capacity, making wan2.2 and hidream **unroutable** on the 16 GB tier even though they run there
comfortably in reality.

Two components each holding "the" margin means neither owns it, and the product is wrong.

## Decision

**A node advertises the raw observed peak** — `vram_peak_gb = round(observed, 0.1)`, no padding — and
**the dispatcher owns all routing margin.**

The node's job is to report what a render actually cost on this machine, as measured (max-kept,
positive-only, PDH-sampled per ADR 0008). The dispatcher's job is to decide how much headroom to
require on top of that measured cost, because margin is a *placement* policy, not a *measurement*
property — it depends on how tightly the dispatcher wants to pack nodes, which the node cannot know.

This is CONTRACT v2.1: the `vram_peak_gb` field's meaning changes from "padded footprint" to "raw
measured peak." Consuming dispatchers apply their margin to the raw value.

## Consequences

- The ×1.2×1.2 double-inflation is gone; the dispatcher's single margin becomes the only margin, and
  wan2.2/hidream route on the 16 GB tier as they should.
- The `vram_peak_gb` contract field now means **raw measured peak**. A dispatcher that was relying on
  the node's ×1.2 for safety must apply adequate margin itself — but since the dispatcher already
  applied its own margin, in practice this just removes the node's redundant contribution.
- Measurement and margin are cleanly separated: the node measures, the dispatcher pads. Each concern
  has exactly one owner.
- This supersedes the padding decision in ADR 0008. The rest of ADR 0008 (PDH-primary per-process
  sampling, `nvidia-smi` for live health capacity, the mtime cross-process merge, Afterburner as a
  validation companion) is unchanged.

## Alternatives considered

- **Keep the node ×1.2 and reduce the dispatcher's margin to compensate.** Rejected: it keeps margin
  split across two components, so the two must stay coordinated forever. Any future change to either
  side silently re-breaks the total. Single ownership is the fix.
- **Node advertises raw, dispatcher advertises nothing (no margin at all).** Rejected: some margin is
  genuinely needed against sampler variance and concurrent load; removing it entirely would
  over-pack. The margin should exist — it should just live in one place, the dispatcher, which sees
  the whole fleet's load.
- **Make the node's padding factor configurable.** Rejected: it is the same split-ownership problem
  with an extra knob. The dispatcher already has the margin knob.

## Related code

- [`internal/fleetnode/footprints.go`](../../../internal/fleetnode/footprints.go) — `Record` now stores
  the raw rounded peak
- [`internal/fleetnode/footprints_test.go`](../../../internal/fleetnode/footprints_test.go),
  [`internal/pipeline/pipeline_footprint_test.go`](../../../internal/pipeline/pipeline_footprint_test.go)
  — assertions updated to the raw values

## Related docs

- [0008-pdh-primary-vram-sampling.md](0008-pdh-primary-vram-sampling.md) — the sampling ADR whose
  padding this revises
- [../../systems/fleet-node.md](../../systems/fleet-node.md), [../../FLEET-NODE.md](../../FLEET-NODE.md)
