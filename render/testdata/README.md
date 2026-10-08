# render/testdata — iGPU engine logs

The fixtures the no-CPU guard (`render/igpu-engine.mjs`, `createLogGuard`) is pinned to.
Everything here is a REAL capture from the amd-gcn reference node (Vega 7 iGPU through
RADV, Ubuntu, 32 GB UMA), scrubbed of local paths (`/models/...`, `/work/...`, `/opt/...`
are placeholders), except the three `*-derived.log` files, which are derived and say so
in their first line.

| file | what it is | the guard's verdict |
|---|---|---|
| `sdcpp-video-healthy.log` | stable-diffusion.cpp `3f8527a`, FastWan2.2 TI2V-5B I2V 832x480x49, full VAE | passes |
| `sdcpp-video-tae.log` | same model, tiny autoencoder decode (`--taesd`) | passes |
| `sdcpp-vace-device-lost.log` | Wan2.1 VACE 1.3B at 15,600 tokens: the GPU reset in the first step | `GPU_RESET` |
| `da3-healthy.log` | depth-anything.cpp `14f7461`, one frame | passes |
| `audiocpp-voice-clone.log` | audio.cpp v0.9.0 chatterbox voice clone | passes |
| `audiocpp-music-host-prefill.log` | audio.cpp v0.9.0 ace_step: a genuinely captured CPU placement (`ace_step.planner.weights.buffer_name CPU`) | `CPU_PLACEMENT` |
| `sdcpp-video-cpu-derived.log` | derived from `sdcpp-video-healthy.log`: every params and compute line on CPU | `CPU_PLACEMENT` |
| `sdcpp-video-cpu-compute-derived.log` | derived from `sdcpp-video-healthy.log`: only the diffusion stage's compute buffer on CPU | `CPU_PLACEMENT` |
| `sdcpp-video-tae-cpu-derived.log` | derived from `sdcpp-video-tae.log`: every params and compute line on CPU | `CPU_PLACEMENT` |

## Why the sd.cpp negatives are derived

No sd.cpp run on the CPU was captured: the node's operator forbids running a model on its
CPU, even to make a fixture. Each negative is a healthy log with the device substituted
in exactly the two line shapes sd.cpp prints. `derive-cpu-fixtures.mjs` does the
substitution and the node test re-derives each file and fails if a checked-in file
drifts from its source.

## What the CPU backend prints (checked in the source, not assumed)

Both format strings were read at stable-diffusion.cpp commit `3f8527a` (and the ggml it
pins, commit `89c4413`):

- `src/model_manager.cpp:490`:
  `"model manager prepared params backend buffers (%6.2f MB, %zu tensors, %zu blocks, %s) on %s"`,
  with `%s` #1 = `RAM` when `ggml_backend_buft_is_host()` else `VRAM`, and `%s` #2 =
  `ggml_backend_buft_name(buft)`. ggml's CPU buffer type returns the literal `"CPU"`
  (`src/ggml-backend.cpp`, `ggml_backend_cpu_buffer_type_get_name`); a Vulkan buffer type is
  `"Vulkan0"`, and the pinned host staging type is `"Vulkan_Host"` (`ggml-vulkan.cpp`,
  `ggml_backend_vk_host_buffer_type_name`).
- `src/core/ggml_runner.cpp:1019`:
  `"%s compute buffer size: %.2f MB(%s) on %s (peak across %zu segment%s)"`, with `%s` #2 =
  `RAM` when `sd_backend_is_cpu()` else `VRAM`, and `%s` #3 = `ggml_backend_name(backend)`.
  ggml's CPU backend returns the literal `"CPU"` (`src/ggml-cpu/ggml-cpu.cpp`,
  `ggml_backend_cpu_get_name`); Vulkan returns `"Vulkan<N>"`.

So a CPU placement reads `... MB(RAM) on CPU` (compute) and `..., RAM) on CPU` (params): the
name is `CPU`, not `CPU0` or `cpu`. The guard matches `on CPU\d*` case-insensitively
anyway, and treats `Vulkan_Host` as neither evidence nor a placement (`Vulkan\d+` needs a
digit). Params resting in host RAM with the compute on Vulkan is the sanctioned overflow, so
a params line `on CPU` alone is not a failure; a compute buffer `on CPU` is, and so is a run
with no diffusion-stage compute buffer on Vulkan at all.

Other real shapes the guard keys on: `[WARN] loading CPU backend` and `Using CPU backend`
(`ggml_extend_backend.cpp`, the no-GPU fallback), `auto-fit: no GPU memory budget available;
using CPU` (`backend_fit.cpp:446`) and the plan line `-> compute <backend>, params <where>`
(`backend_fit.cpp:346`). `Initializing backend: CPU` (a params host) and
`load_backend: loaded CPU backend` (ggml always registers its CPU backend) are not
placements.

## screen-parity-table.json

One list of backend values and extra-args elements for BOTH screens: `internal/config`
(`CPUBackendRefusal`, `ScreenExtraArgs`) and `render/igpu-engine.mjs` (`refuseCpuBackend`,
`screenExtraArgs`) must agree on every row, and both test files read this file. It pins the
empty per-module parts (`vulkan0,`, `,vulkan0`, `vulkan0&`) and the Unicode spaces
(`--threads=<NBSP>cpu`) that the Go screen once let through.

## clock-preload.cjs

A test-only `node --require` preload for `render/igpu-deadline.test.mjs`: it busy-waits before the
runner starts, moves the clock an hour ahead after an exact `spawn` / `spawnSync` (matched by a
needle in its command line), and records the `timeout` option of every `spawnSync`. Never used by
a runner.

## token-cap-table.json

The shared inputs and expected latent-token counts for the token cap. `render/igpu-engine.test.mjs`
(`latentTokens`) and `internal/config` (`LatentTokens`) both read it, so the Go and Node
formulas cannot drift apart.
