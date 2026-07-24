---
status: Accepted
date: "2026-07-24"
---

# GPU memory is a resolved provider; UMA samples Dedicated+Shared

## Context

The fleet node was NVIDIA-shaped in three places: the startup gate refused to serve without a
working `nvidia-smi`; the health sampler shelled the same query; and `gpu_vendor`/`gpu_arch` were
re-derived from NVIDIA product-name regexes. A working AMD/Vulkan box — the `amd-rdna3` tier this
harness now ships first-class — could not join the fleet at all, and the refusal blamed the GPU
brand rather than the missing measurement.

Separately, per-render footprint sampling ([ADR 0008](0008-pdh-primary-vram-sampling.md)) summed
the PDH `\GPU Process Memory(pid_*)\Dedicated Usage` tree. On a unified-memory iGPU, allocations
land in **Shared** system memory and the Dedicated counter reads ~0 — so on exactly the tier that
most needs measured footprints, they would silently never record (the J-audit's "invisible"
break).

## Decision

1. **The GPU memory source is a resolved provider, not an assumption.** `fleet-serve` tries
   `nvidia-smi` first (a working NVIDIA node is byte-identical in behavior), then the
   **windows-generic WDDM provider**: capacity from the display-class registry
   (`HardwareInformation.qwMemorySize` — the same source `detect.ps1` uses), usage from the
   `\GPU Adapter Memory(*)` PDH counters (vendor-agnostic WDDM facilities). The gate refuses only
   when **no memory source works** — "no working GPU memory source", never "no NVIDIA GPU".
2. **Vendor/arch come from the installer's manifest**, not product-name regexes: the
   `installed.json` `profile` already encodes what `detect.ps1` classified
   (`amd-rdna3` → `amd`/`rdna3`). Unknown stays `"unknown"` — honest, never guessed.
3. **The UMA memory model changes both compositions.** Capacity: an iGPU's carve-out is not its
   capacity; the provider advertises carve-out + the WDDM shared budget (~half of system RAM).
   Usage: adapter Dedicated + Shared. Whether a box is UMA comes from the profile
   (`amd-rdna3`/`amd-gcn` yes, discrete classes no), with a small-carve-out heuristic only when
   no manifest exists.
4. **Per-render sampling gains `fleet_sampler: "pdh-shared"`** — the same process-tree sampler
   summing Dedicated + Shared Usage. The `amd-rdna3` config_seed sets it; discrete boxes keep the
   Dedicated tree (Shared is noise there). ADR 0008's PDH-primary decision stands; this extends
   its counter set for unified memory.

## Consequences

- An AMD/Vulkan box fleet-serves and advertises honest numbers; the dispatcher needs no changes
  (the contract already speaks GiB and vendor/arch strings).
- UMA totals are a *budget*, not exclusive VRAM — the dispatcher's margin logic already treats
  free GiB as advisory, and the receipt loop (Juan's box) validates the projections.
- The nvidia-smi path stays the first choice everywhere it works, so every existing node's
  behavior and numbers are unchanged.
- Off-Windows non-NVIDIA boxes still cannot fleet-serve (no generic provider there) — the gate
  message says which sources were tried; a Linux provider is a future seam, not this decision.

## Alternatives considered

- **DXGI/D3DKMT queries for the shared budget.** More precise than RAM/2, but needs COM/D3D
  bindings; the WDDM budget is documented ≈ RAM/2 and the receipt loop measures reality anyway.
- **Making `auto` UMA-aware at runtime.** Rejected: the profile already knows, and config-seeded
  explicitness beats runtime guessing (`auto` stays exactly what ADR 0008 shipped).
- **A vendor tool per GPU (rocm-smi, xpu-smi).** Rejected for the same reason nvidia-smi was the
  problem: per-vendor tools multiply failure modes; WDDM counters are one vendor-agnostic source.

## Related docs

- [0008-pdh-primary-vram-sampling.md](0008-pdh-primary-vram-sampling.md) — extended, not replaced
- [../../FLEET-NODE.md](../../FLEET-NODE.md) — operator guide (provider resolution, sampler modes)
- [../../systems/fleet-node.md](../../systems/fleet-node.md)
