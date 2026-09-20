---
status: Accepted
date: "2026-09-20"
---

# The Linux GPU memory provider reads amdgpu sysfs; the generic source is named per OS

## Context

[ADR 0014](0014-gpu-memory-provider-and-uma-sampling.md) made the fleet node's GPU memory source a
resolved provider — `nvidia-smi`, else the Windows-generic WDDM source — and recorded that
"off-Windows non-NVIDIA boxes still cannot fleet-serve (no generic provider there); a Linux
provider is a future seam". That seam became the blocker the day the first Linux AMD node
arrived: binxarn (Ryzen 5 5625U / Vega 7, Ubuntu 26.04, 2026-09-20) detected into the MEASURED
`amd-gcn` tier, rendered and served its llama-swap config, and then `fleet-serve` refused with
`no working GPU memory source: nvidia-smi (…); windows-generic (… requires WDDM)`.

The amdgpu kernel driver already publishes what the provider needs, per card, under
`/sys/class/drm/card*/device`: `mem_info_vram_total/used` (the carve-out) and
`mem_info_gtt_total/used` (the GTT pool — system memory the GPU may map, which on an APU is the
shared budget ADR 0014 approximates with RAM/2 on Windows).

## Decision

1. **`linux-amdgpu` is the generic memory source on Linux.** `AmdgpuSysfsProbe(root, uma)` reads
   the first AMD card exposing `mem_info_vram_total`; the UMA composition matches ADR 0014
   (capacity = carve-out + shared budget, usage = dedicated + shared) with the driver's own GTT
   pool as the budget. A discrete AMD card (`uma=false`) reports VRAM alone, like nvidia-smi does
   for NVIDIA. A zero total is a failed probe; used is clamped at total.
2. **The generic source is named by the caller.** `ResolveProviderNamed` takes a
   `GenericProvider{Probe, Source}` so the serve banner and the gate error say which source was
   tried (`linux-amdgpu` vs `windows-generic`). `ResolveProvider` keeps its signature and its
   `windows-generic` label — every existing node and test is byte-identical.
3. **Vendor/arch still come from the manifest** (`installed.json` profile), never from a product
   string; the `cpu` profile still gets no generic source on any OS.
4. **`AmdgpuSysfsDeviceProbe`** lists every AMD card with the same composition, so a per-device
   `gpu_devices[]` is available on Linux the way it is with nvidia-smi.

## Consequences

- A Linux AMD node fleet-serves and advertises honest numbers; the dispatcher needs no change.
- The advertised UMA total is the driver's GTT budget (15.1 GiB on a 30 GiB box), which is a
  *budget* the OS shares with everything else — as with the Windows composition, the
  dispatcher's free-GiB margin stays advisory.
- The probe has no build tag: it reads plain files under an injected root, so it is unit-tested
  on every OS with a fake `/sys/class/drm` tree; `main.go` wires it on `linux` only.
- The 2 s health sampler runs the same single-file reads; there is no external tool to fail
  transiently, which removes ADR 0014's "provider selection binds a transient failure" edge for
  this source.

## Alternatives considered

- **`rocm-smi` / `amd-smi`.** Rejected for ADR 0014's own reason: a vendor tool multiplies
  failure modes, and neither is installed by default — sysfs is always there when amdgpu is.
- **Vulkan `VK_EXT_memory_budget` queries.** More precise for the serving process, but needs a
  Vulkan binding in the Go node; the driver's sysfs numbers are what the per-render sampler
  will read anyway.

## Related docs

- [0014-gpu-memory-provider-and-uma-sampling.md](0014-gpu-memory-provider-and-uma-sampling.md) — extended, not replaced
- [../../FLEET-NODE.md](../../FLEET-NODE.md) — provider resolution
- `internal/fleetnode/vram_linux_amdgpu.go` — the source
