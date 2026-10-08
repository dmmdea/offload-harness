# render/testdata — iGPU engine logs

The fixtures the no-CPU guard (`render/igpu-engine.mjs`, `createLogGuard`) is pinned to.
Everything here is a REAL capture from the amd-gcn reference node (Vega 7 iGPU through
RADV, Ubuntu, 32 GB UMA), scrubbed of local paths (`/models/...`, `/work/...`, `/opt/...`
are placeholders), except the `*-derived.log` files, which are derived and say so
in their first line.

sd.cpp's log record has two shapes and the fixtures hold both (see "The two sd.cpp record
shapes" below): the `sdcpp-*` captures are master-929 (`3f8527a`), the `sdcpp945-*` ones
master-945 (`a1ded76`).

| file | what it is | the guard's verdict |
|---|---|---|
| `sdcpp-video-healthy.log` | stable-diffusion.cpp `3f8527a` (master-929), FastWan2.2 TI2V-5B I2V 832x480x49, full VAE | passes |
| `sdcpp-video-tae.log` | same model, tiny autoencoder decode (`--taesd`) | passes |
| `sdcpp945-video-healthy.log` | stable-diffusion.cpp `a1ded76` (master-945), FastWan2.2 TI2V-5B I2V 832x480x17, 3 steps, tiled full VAE (overlap 0.25); prompt "waves crash on the rocks around the lighthouse, golden hour" | passes |
| `sdcpp945-video-tae.log` | same model and prompt, tiny autoencoder decode (`--taesd`, `taew2_2`) | passes |
| `sdcpp945-vace-healthy.log` | master-945, Wan2.1 VACE 1.3B (fp16 safetensors) 288x512x17, 20 steps, a depth control video and a reference image; prompt "a clay figure of a bearded man in a plaid shirt waves hello, stop-motion clay style, warm evening light" | passes |
| `sdcpp-vace-device-lost.log` | Wan2.1 VACE 1.3B at 15,600 tokens: the GPU reset in the first step | `GPU_RESET` |
| `da3-healthy.log` | depth-anything.cpp `14f7461`, one frame | passes |
| `audiocpp-voice-clone.log` | audio.cpp v0.9.0 chatterbox voice clone | passes |
| `audiocpp091-voice-clone.log` | audio.cpp v0.9.1 chatterbox voice clone (7.36 s of audio); text "Every clip on this node now renders on the integrated graphics, and nothing runs on the processor. This is the first voice test." | passes |
| `audiocpp091-music.log` | audio.cpp v0.9.1 ace_step, 30 s of audio, built WITH the planner prefill patch (`setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch` applies to v0.9.1 unchanged): the planner is on `Vulkan0` and no line says `buffer_name CPU`; prompt "warm acoustic guitar and soft piano, gentle documentary underscore, 90 bpm, instrumental" | passes |
| `audiocpp-music-host-prefill.log` | audio.cpp v0.9.0 ace_step: a genuinely captured CPU placement (`ace_step.planner.weights.buffer_name CPU`) | `CPU_PLACEMENT` |
| `sdcpp-video-cpu-derived.log` | derived from `sdcpp-video-healthy.log`: every params and compute line on CPU | `CPU_PLACEMENT` |
| `sdcpp-video-cpu-compute-derived.log` | derived from `sdcpp-video-healthy.log`: only the diffusion stage's compute buffer on CPU | `CPU_PLACEMENT` |
| `sdcpp-video-cpu-plan-derived.log` | derived from `sdcpp-video-healthy.log`: only the auto-fit plan's DiT line on CPU (`-> compute CPU, params RAM`) | `CPU_PLACEMENT` |
| `sdcpp-video-tae-cpu-derived.log` | derived from `sdcpp-video-tae.log`: every params and compute line on CPU | `CPU_PLACEMENT` |
| `sdcpp945-video-cpu-derived.log` | derived from `sdcpp945-video-healthy.log`: every params and compute line on CPU | `CPU_PLACEMENT` |
| `sdcpp945-video-cpu-compute-derived.log` | derived from `sdcpp945-video-healthy.log`: only the diffusion stage's compute buffer on CPU | `CPU_PLACEMENT` |
| `sdcpp945-video-cpu-plan-derived.log` | derived from `sdcpp945-video-healthy.log`: only the auto-fit plan's DiT line on CPU (`-> compute CPU, params RAM`) | `CPU_PLACEMENT` |
| `sdcpp945-video-tae-cpu-derived.log` | derived from `sdcpp945-video-tae.log`: every params and compute line on CPU | `CPU_PLACEMENT` |
| `sdcpp945-vace-cpu-compute-derived.log` | derived from `sdcpp945-vace-healthy.log`: only the diffusion stage's compute buffer on CPU | `CPU_PLACEMENT` |

## The two sd.cpp record shapes

The same message, as the two releases print it (real lines from the fixtures):

```
master-929  [VERBOSE] ggml_runner.cpp:1019 - Wan2.2-TI2V-5B compute buffer size: 478.17 MB(VRAM) on Vulkan0 (peak across 1 segment)
master-945  [V] Wan2.2-TI2V-5B compute buffer size: 192.53 MB(VRAM) on Vulkan0 (peak across 1 segment) --- ggml_runner.cpp:1019
```

- **Level tag.** `[VERBOSE]` / `[INFO   ]` / `[WARN   ]` / `[ERROR  ]` (padded to 7) became `[V]` / `[I]` /
  `[W]` / `[E]` (and `[D]`), unpadded (sd.cpp #2104).
- **Source.** `file.cpp:N - ` in front of the message (the line number padded to 4) moved behind it, as
  ` --- file.cpp:N` (#2104 moved it; #2106 made the separator ` --- `, it was ` - ` in a build
  between them, if one was cut, which no capture here covers: the guard reads that shape from the upstream commits alone).
- **Records of several lines** carry the tag on the FIRST line and the source on the LAST: the parameter
  dumps open with `[V] SDCliParams {` and close with `} --- main.cpp:699`; `System Info:` ends its second
  line with ` --- main.cpp:698`. The lines in between have neither.
- **Prompt echoes.** `parse '...'` and `split prompt "..."` print the prompt on one line with its newlines
  escaped (`\n`, `\r` as two characters; #2106). No fixture has a multi-line prompt, so the escaping is
  read from the upstream commit and pinned with synthetic lines, and whether the dumps escape a newline
  is not known: the guard skips a dump block whole either way.
- **Not changed.** ggml's own lines (`ggml_vulkan: ...`, `load_backend: ...`) are bare in both, and so are
  the progress bars (carriage returns, kept byte for byte: `render/testdata/*.log` is `-text`).
- **The auto-fit plan** (`[I]     DiT          params   5162 MiB, compute reserve  2048 MiB -> compute
  Vulkan0, params Vulkan0`) is in both releases' logs; the guard takes the DiT line on `Vulkan<N>` as
  evidence of the diffusion stage and `-> compute CPU` of any component as a placement.

`normalizeSdLine` in `render/igpu-engine.mjs` cuts one head and one tail, so the guard's shapes read the
message and the same tests run over the fixtures of both releases.

## Why the sd.cpp negatives are derived

No sd.cpp run on the CPU was captured: the node's operator forbids running a model on its
CPU, even to make a fixture. Each negative is a healthy log with the device substituted
in exactly the line shapes sd.cpp prints (the params-buffer line, the compute-buffer line, the
auto-fit plan line). The substitution touches only the device words, so one script derives the
negatives of both record shapes. `derive-cpu-fixtures.mjs` does the
substitution and the node test re-derives each file and fails if a checked-in file
drifts from its source.

## What the CPU backend prints (checked in the source, not assumed)

Both format strings were read at stable-diffusion.cpp commit `3f8527a` (and the ggml it
pins, commit `89c4413`); the master-945 logs print the same message texts (checked against
the `sdcpp945-*` fixtures, the source was not re-read at `a1ded76`), only the record around
them changed:

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
