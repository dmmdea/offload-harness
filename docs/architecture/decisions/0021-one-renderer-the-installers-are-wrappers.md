---
status: Accepted
date: "2026-07-28"
---

# One renderer: the installers are wrappers, not renderers

## Context

Section 3 of the first-class-install plan said `install.ps1` and `install.sh` would become
"fetch + place + service-register wrappers" around `local-offload install …`, with
"nothing that decides anything" living in a wrapper. `install.sh` was written that way.
`install.ps1` never was: it kept its own token substitution, its own 26B removal, its own
GPU-env injection and its own off-matrix default table — a **second renderer**, 68 KB of
PowerShell, drifting against the Go one.

The drift was not theoretical. It shipped:

| version | what the PowerShell renderer produced | real llama-swap verdict |
|---|---|---|
| 0.36.0 | matrix templates with the 26B model block removed but its `m26` var left behind, on any tier that drops the 26B | **REJECTED** — `matrix: var key "m26" references unknown model "gemma4-26b-a4b"` |
| 0.37.0 | the above, plus literal `__SEATS_RESIDENT__` / `__SEATS_SWAPPABLE__` / `__M26_ALT__` in matrix set expressions, on **every** tier | **REJECTED** |

Both verified against llama-swap v242, not inferred. Live nodes were spared only because
Step 6 skips when a config already exists — a *fresh* Windows install would not have
started. The same gap made a seat-declaring tier refuse outright, which is why `ampere-6`
could not be installed on Windows at all and `ampere-8` could not declare its seats.

Two behaviours also lived ONLY in PowerShell, so a Linux install of the same tier silently
did without them:

- the Blackwell CUDA env (`CUDA_VISIBLE_DEVICES=0`, `CUDA_MODULE_LOADING=LAZY`), applied by
  an `if ($profileId -match '^blackwell-')` branch;
- the RAM gate on the 26B — `cpu_moe` puts EVERY expert in RAM, so it is dropped on a
  low/min RAM tier. The Go renderer had no ram-tier input at all, so Linux served the 26B
  on boxes with no RAM path for it.

## Decision

**`install.ps1` Step 6 delegates to `local-offload install render`. There is one renderer.**

- Everything that decided anything moved into the binary or the tier table: `gpu_env`
  became a **tier field** (so it is hardware-class-driven, not name-prefix-driven, and
  applies on every OS); the off-matrix backend defaults became `--fallback-backend`; the
  RAM gate became `--ram-tier`, implemented once from install.ps1's own documented rules.
- The wrapper still owns what only it knows: the resolved `ram_tier`, the physical-core
  count, and the install paths. It passes them as flags.
- The renderer is resolved, not assumed: `$env:OFFLOAD_HARNESS_EXE`, else the installed
  exe, else a `go build` into a TEMP dir. Step 7 is what builds the *installed* copy, and
  Step 6 runs before it.

**`-RenderOnly` is no longer build-free**, and its header says so. It remains
side-effect-free with respect to the install tree, which is the property the tests rely
on. `render.tests.ps1` builds the renderer once and points every case at it.

## How it was proven safe

A behaviour-preservation gate, not a claim: every profile × backend × ram-tier was
rendered by the OLD PowerShell renderer and by the delegated one — 34 configs — and
compared line by line ignoring comments. **Zero unexplained differences across all 32
comparable pairs.** The only differences were the two bugs above (unsubstituted tokens,
dangling `m26` var), each confirmed as a llama-swap *rejection* on the old output and an
*acceptance* on the new.

That gate is also what caught three regressions in the delegation before it merged: the
renderer classifying the LOCAL machine when asked for off-matrix defaults; `--cpu-moe`
emitted without its `-ngl 999`; and `blackwell-8` missing the CUDA env because the tier
field had been added to the other four Blackwell tiers only.

## Consequences

- A fresh Windows install renders a config llama-swap accepts. It did not, in 0.36.0/0.37.0.
- `ampere-8` now declares its measured vision + STT seats, and `ampere-6` installs on
  Windows. The tier table finally reaches both operating systems.
- The Blackwell CUDA env and the 26B RAM gate now apply on Linux too — the same tier gets
  the same treatment, which is the whole point of a tier.
- `install.ps1`'s renderer internals are dead and were removed. `Remove-26bFromYaml` and
  `Add-GpuEnvToYaml` no longer have a caller in the render path.
- ~~**`install.sh` still passes no `--ram-tier`**, so the Linux gate is inert until it
  does.~~ **CLOSED in 0.39.0:** `hwdetect.RAMTier` ports detect.ps1's thresholds, `install
  detect` emits `ram_tier` on the verdict, and `install.sh` passes it to BOTH `install
  seed` and `install render` — refusing to install if detect returns none, rather than
  silently running with both gates off. That also closed a second, quieter half of the
  same gap: `install seed` never received a ram tier either, so the RAM-gated media seed
  (`config_seed_ram_mid_high`) had never applied on Linux at all.
- `render.tests.ps1` needs Go on PATH. It always ran from a checkout, so this is not new
  in practice, and it is stated in the header.

## Alternatives rejected

- **Teach the PowerShell renderer about matrix and seats.** That is the drift, restated:
  two renderers, two sets of bugs, and the tier table reaching one OS first every time.
- **Move Step 7 (build) ahead of Step 6 for everyone.** Heavier than needed — the renderer
  only has to EXIST, not be the installed artifact, so resolving one is enough.
- **Keep the PowerShell path as a fallback for off-matrix boxes.** That preserves a second
  renderer for exactly the case with the least test coverage.
