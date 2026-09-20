---
status: Accepted
date: "2026-09-20"
---

# A dual-route node renders a CPU seat family beside its GPU seats, declared per tier

## Context

The operator's direction for the first Linux AMD node (binxarn, Ryzen 5 5625U / Vega 7,
2026-09-20): "wire and test both so the harness can offer both routes — that new AMD tier is a
first-class citizen of the fleet." Both routes were measured on the box (Vulkan E2B pp512 256 /
tg128 22.8 t/s; CPU 109 / 21.5; native CPU build +10 % pp) and the CPU family was hand-spliced
into the live llama-swap config to prove the shape: the tier's chat weights as `<id>-cpu`
entries run by a CPU llama-server build, a second loader macro, and membership in the
interactive set so one model is resident at a time.

A tier is one backend; its template is one backend's flags. Nothing in the renderer could
produce that second family, so the node's config was hand-edited — the state ADR 0043 exists to
end (an UNSTAMPED serving config nobody can regenerate).

## Decision

1. **A tier declares its alternates: `alt_backends` in `profiles.json`** (today only `["cpu"]`;
   `amd-gcn` declares it, measured). The declaration is the permission; nothing renders from it
   on its own.
2. **The renderer produces the CPU family when asked for it:** `install render --llama-bin-cpu
   <dir>` (`Params.AltCPULlamaBin`) appends `offload-e4b-cpu` and `gemma4-e2b-cpu` — the same
   weights files the template serves, the cpu template's exact flags (no `-ngl`, no
   `--flash-attn`, `--threads`), `ttl: 300` — adds the `ldcpu` loader macro on Linux (the host
   template's `${ld}` names the GPU build's dir) and joins the seats to the swappable fragment so
   they land in the interactive set. The flag without the declaration is an error; the
   declaration without the flag prints a note. A cpu tier refuses the flag (its primary route IS
   the CPU).
3. **The manifest and health say what was rendered.** `install.sh --llama-bin-cpu` writes
   `backend` (from the new `install tier-info` verb) and `alt_backends` (only what this install
   rendered) into `installed.json`; `/fleet/health` gains `backends` (primary first, additive,
   omitted when the manifest has no backend). A caller picks the route by seat id — no new
   contract field, no per-request backend switch.
4. **The hand splice is retired by re-rendering:** the rendered CPU entries are byte-identical
   to the spliced ones (checked on binxarn; only the OR-order of the interactive set differs).

## Consequences

- A dual-route box is an installer outcome again (stamped basis, `spec_sha256`), not a hand edit.
- The tier table stays the single source of which boxes may carry a second route; a future
  `alt_backends: ["vulkan"]` on a CUDA tier is the same mechanism with a second family.
- `ParamsBasis` mirrors the new field, so the spec hash covers it.
- Routing by seat id means the delegator needs nothing new to reach the CPU route; a
  contract-level `backend` hint (auto-placement between the two routes by measured latency) is
  a follow-up, not part of this decision.
- Windows renders the family with `llama-server.exe` and no loader macro (self-contained
  builds); tested, not yet measured on a Windows dual-route box.

## Alternatives considered

- **A second llama-swap instance for the CPU route.** Rejected: two residency solvers cannot
  keep one model resident at a time on a shared DDR pool; the matrix is the point.
- **A composite tier (`composes: [amd-gcn, cpu]`, ADR 0039).** Rejected for this shape: the
  composite mechanism places device *layers*; here both routes serve the same weights on the
  same memory, and the choice is per request, not per layer.
- **Letting llama-server pick the backend per request.** llama.cpp loads one backend per
  process; the seat is the unit of backend choice.

## Related docs

- [0014-gpu-memory-provider-and-uma-sampling.md](0014-gpu-memory-provider-and-uma-sampling.md), [0053-linux-amdgpu-gpu-memory-provider.md](0053-linux-amdgpu-gpu-memory-provider.md) — the node side
- [0043-…](README.md) — stamped serving configs
- `internal/servingtmpl/altcpu.go`, `docs/tiers/amd-gcn.md`
