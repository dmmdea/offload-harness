# Media generation

## Purpose

Image, video, audio, and SVG generation plus image editing — everything that drives a local ComfyUI
instance, a stable-diffusion.cpp binary, or a generation script, under a GPU lifecycle that leaves
the machine usable afterwards.

**Two image engines (J2, 2026-07-24).** `imagegen_engine` selects the `generate_image` backend
per machine: `""`/`"comfy"` = the ComfyUI path this doc mostly describes (unchanged default);
`"sdcpp"` = **stable-diffusion.cpp** via `render/sdcpp-generate.mjs` — a single native Vulkan
binary, spawn-per-job under the same GPU lock, zero-warm by construction (the process exits and
the memory is gone; no `/free`, no Python). The sdcpp path exists for the AMD/Vulkan tier
(`amd-rdna3*` profiles seed it with the Apache-2.0 Z-Image-Turbo GGUF set — see
`setup/SETUP-AGENT.md`, Media tier) but runs on any Vulkan GPU. Go-side, the runner is a thin
`gpugen.Spec` with `SkipFreeComfy:true` — the shape the TTS path proved; the runner owns the
mapping from the harness's generic flags to sd.cpp's CLI, so a pin bump fixes flag drift in one
`.mjs` file, never in Go. `sd-server` (OpenAI/A1111-compatible, ships in the same pinned zip) is
the recorded warm-swap upgrade path — deliberately not wired yet.

## Questions this doc answers

- What happens to the GPU during a render, and what state is the machine left in?
- Which models are bound, and where is that configured?
- What are the image-editing operations, and how are they invoked?
- When is batching worth it?
- Why is FLUX not an option?

## Scope

The generation verbs and MCP tools, the GPU lock and zero-warm lifecycle, warm batch mode, the
inpainting route, the edit-operation pack, and per-machine model bindings.

## Non-scope

- Arbitrary caller-supplied graphs and node provisioning → the run-graph flow at
  [../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md)
- Serving text tiers → [setup-installer.md](setup-installer.md)
- VRAM footprint measurement for fleet dispatch → [fleet-node.md](fleet-node.md)

## Key concepts

**GPU Lock** — a single-slot, cross-process lock; only one GPU-heavy job runs at a time per machine.
**Zero-Warm** — no GPU residency persists between jobs. **Warm Batch** — an opt-in session where the
checkpoint loads once for N renders. **Op** — one image-editing operation inside `edit-image`.

## How the system works

Every GPU-heavy job runs inside `withGpuSlot`, which owns the render-side lifecycle: **fence**
against the lease it was handed, free the llama-swap tiers (once per lease, after draining),
cold-start ComfyUI, run the job, then tear down — `/free` and kill the ComfyUI process. Teardown is
idempotent and also runs on SIGINT/SIGTERM, so an interrupt does not leak ComfyUI.

`withGpuSlot` does **not** acquire or release. The harness takes the machine-wide lease in Go and
threads `GPU_LEASE_DIR` / `GPU_LEASE_EPOCH` / `GPU_LEASE_CLASS` down; a GPU job started with no
lease refuses rather than grabbing the card. Arbitration, staleness, fencing and reclaim all live
in one place — see [GPU lease](gpu-lease.md) and
[ADR 0018](../architecture/decisions/0018-machine-wide-fenced-gpu-lease.md). A busy card queues the
job for a bounded window (`gpu_wait_ms`, 90 s) and then defers with the holder's detail.

Two details of the free step are easy to get wrong:

- **It frees per model, not everything.** The always-loaded embedding and reranker models are
  CPU-only and hold zero GPU VRAM. An earlier unload-all implementation tore that memory stack down
  on every generation job for no VRAM benefit; the keep-set now protects it.
- **ComfyUI is only killed if the harness started it.** An already-running instance is left alone.

Full rationale in [ADR 0009](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md).

**Warm batch.** `generate-image --batch` takes a jobs file and runs N renders in one session. The
only behavioral change is omitting ComfyUI's `--cache-none`, so the checkpoint loads once; teardown
still happens exactly once, at the batch boundary. A failed render is recorded and the batch
continues, one JSONL result line per job. **The default single-render path is unchanged.**

**Prompt refiner (opt-in).** When `imagegen_refiner_model` names a llama-swap text model,
`generate_image` first expands the raw prompt with concrete photographic detail (lighting,
composition, materials, mood, lens vocabulary) on the free local text tier — before the render and
before the media lease, so the text call never contends with the render. One decision point
(`internal/pipeline/refiner.go`) serves the single ComfyUI path, the sdcpp engine, and warm batch.
It is fail-safe by construction: any refiner problem — transport error, timeout
(`imagegen_refiner_timeout_sec`, default 30; a deadline hit is annotated "cold model swap?"),
truncated or empty output, output shorter than the input, a prompt already over the ~200-token
refiner budget (skipped up front), a dropped/altered `"double-quoted"` span, or **added** quoted
text (a whole-output quote wrap is stripped first; net-new quotes beyond that are rejected) —
falls back to the raw prompt, records the reason, and renders anyway. Span guarding is computed in
normalized-quote space (curly `“”` count as `"`); an odd quote count drops the trailing quote
before pairing, so a trailing inch mark never pairs into a bogus span (a LEADING stray still can —
that mis-pair falls back safely, and the distinct `altered (glyphs/whitespace)` vs `dropped`
reasons make it diagnosable). Batches carry a refiner circuit breaker: after 3 consecutive
transport/timeout-class failures the remaining jobs skip the refiner (marked
`refiner disabled after N consecutive failures`) instead of stalling timeout-by-timeout before the
first render. Empty model = OFF and the path is byte-identical (pinned by test). Results carry
`refined`/`refined_prompt`/`refine_fallback` only when configured (batch items always say
`refined` true/false then, and the batch summary counts `refine_fallbacks`); `refine=false`
(MCP/CLI `--refine=false`/per-batch-job) renders the prompt verbatim. Output paths derive from the
raw prompt with the `refine` knob stripped from the hash, so re-runs keep reusing one file.

**Image editing** is one verb (`edit-image` / `offload_edit_image`) carrying an `ops` list, not a
family of verbs. The full op set (validated in `mediaops.ValidateOps`) is `crop`, `resize`,
`convert`, `composite`, `text`, `mask_boxes`, `grade`, `lut_cube`, `perspective_composite`, `finish`,
`flatten_design`, and `instantiate_design` (the last two drive GIMP). `finish` is delivery sharpening
and should come **last** — sharpening before a resize is undone by the resampling — but this is a
caller convention that the validator documents, not an ordering it enforces (mask and rendition
chains may legitimately follow). `renditions` is a top-level parameter, not an op: it re-runs the
pipeline once per export target.

**Inpainting** (`inpaint-image` / `offload_inpaint_image`) takes a mask, or builds one from
`mask_boxes`. `--auto-text` localizes rendered-text regions with the vision model and inpaints them.
It is **active**, not gated: the always-defer gate was removed on 2026-07-17 after a grounding
evaluation passed 3/3. Its safety envelope is validation rather than a gate — an unparseable answer,
no boxes, or absurd boxes covering more than 60% of the image all error out so the caller defers,
with the manual `mask_boxes` workflow named. It never silently repaints unverified regions.

**Per-box device/launch seams (J4).** Three env knobs decouple shared code from CUDA-box
assumptions, all default-preserving: `COMFY_COMPUTE_DEVICE` overrides the DisTorch2 loaders'
`compute_device` in the Wan graph (was hardcoded `cuda:0`); `COMFY_EXTRA_ARGS` appends verbatim
flags to the managed ComfyUI launch (whitespace-split — a flag VALUE containing spaces is
inexpressible, fine for ComfyUI-style flags); `TTS_DEVICE` overrides the
Chatterbox worker's torch device auto-pick.

## Data and state

Rendered outputs land in the configured media directory or a caller-supplied `out_dir`. Footprint
observations are recorded as a side effect of successful renders — see
[fleet-node.md](fleet-node.md).

## Interfaces and entry points

CLI verbs `generate-image`, `inpaint-image`, `generate-video`, `generate-audio`, `generate-svg`,
`edit-image`, `media`, `run-graph`; the matching `offload_*` MCP tools.

## Dependencies

A local ComfyUI installation (`comfy_dir`), the model files named by the bindings below, GIMP for the
design ops, and the Node renderer scripts under `render/`.

## Downstream effects

The GPU lock is machine-wide: a long render blocks vision tasks, which defer with `gpu_busy` rather
than queueing. Consumers of the media outputs — notably the creative pipeline workflows — depend on
the output envelope shape.

## Invariants and assumptions

1. **Zero-warm by default.** Nothing GPU-resident survives a job.
2. **The CPU memory stack is never unloaded** by the free step.
3. Only one GPU-heavy job at a time, per machine.
4. **No FLUX-family model is ever added** — see
   [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md). The binding reason
   is the non-commercial licence, not VRAM; a bigger card does not reopen it.

## Model bindings

Bound per machine through flat config keys, so the same code serves different hardware:

| Concern | Keys |
|---|---|
| Image | `imagegen_family`, `imagegen_ckpt`, `imagegen_vae`, `imagegen_steps/cfg/sampler/scheduler`, `imagegen_preset/clip/lora/lora_strength/shift` (qwen-image knobs), `imagegen_refiner_model/refiner_timeout_sec` (opt-in prompt refiner) |
| Inpaint | `inpaint_ckpt`, `inpaint_vae`, `inpaint_steps/cfg/sampler/scheduler` |
| Video | `videogen_unet_high`, `videogen_unet_low`, `videogen_text_encoder`, `videogen_upscale_model` |
| Audio | `voicegen_*`, `musicgen_script` |
| ComfyUI | `comfy_dir`, per-task `*_script` and `*_timeout_sec` |

Hardware profiles seed these. Tiers at 16 GB and above bind **HiDream-O1 bf16** via
`imagegen_family` — the official graph for that DiT, never the generic SDXL graph — and **Wan 2.2
Q8_0** experts with an fp16 text encoder. **RealVisXL** is the SDXL-class inpainting default. The 8 GB
tiers stay SDXL-class for image generation until O1 on 8 GB is verified on real hardware.

**Qwen-Image 2512** (Apache-2.0) is the prompt-adherence *generation* alternative at ≥16 GB:
`imagegen_family: "qwen-image"` selects its model-correct graph — SD3-class 16-channel latent
(`EmptySD3LatentImage`, never the SDXL latent), `ModelSamplingAuraFlow` shift, split
text-encoder/VAE files, and `ConditioningZeroOut` for an empty negative — with `imagegen_ckpt`
carrying the UNET filename (`.gguf` or `.safetensors`; the `_1`-quant rule below applies to 2512
GGUFs the same as 2511). **Binding it on a seeded box takes TWO key changes, not one:** every
≥16 GB profile seeds `imagegen_vae: "builtin"` for HiDream, and the qwen-image route fails loud
on that value (the UNET file carries no VAE weights) — set `imagegen_vae` to
`qwen_image_vae.safetensors` or clear it. If binding `imagegen_steps`/`imagegen_cfg`, set both
or neither: the route rejects a half-override of the preset pairing. It is not the seeded
default seat: `imagegen_family` is a per-machine binding, and ad-hoc renders reach the family
through the render CLIs (`comfy-render.mjs`/`comfy-generate.mjs --family qwen-image`; presets
`full` = 50 steps/cfg 4 and `lightning4` = 4 steps/cfg 1 + Lightning LoRA, both
template-verified). The `--preset/--clip/--lora/--lora-strength/--shift` flags are bound by
`imagegen_preset` / `imagegen_clip` / `imagegen_lora` / `imagegen_lora_strength` /
`imagegen_shift`, so a harness-driven `qwen-image` seat can run the `lightning4` recipe
directly (an empty `imagegen_lora` means unset, not LoRA-stripping — bind preset `full` for a
LoRA-free run). On a qwen-image seat with no `imagegen_cfg` bound, a per-request `steps`
override alone trips the route's steps/cfg pair guard by design (exit 2, loud) — callers on
that seat pick a preset instead of overriding steps.

The recommended **≥16 GB image-*edit* primitive is Qwen-Image-Edit-2511** (Apache-2.0). It is a
model-matrix *designation*, not a config binding — image editing at that tier runs through
[run-graph](../flows/run-graph-manifest-satisfaction.md) with the model set declared in the caller's
node manifest (e.g. the creative-marketing-pipelines scene-swap), so no edit checkpoint is seeded into
`config.json`. **Pin a `_1` GGUF quant (`Q4_1`/`Q5_1`), never a `_K_` one:** 2511 K-quants fail
`UnetLoaderGGUF` with `cannot reshape array` even on byte-perfect files (city96/ComfyUI-GGUF #247).
Measured on `ampere-16` 2026-07-19: Q5_1 (15.4 GB) + fp8 encoder fits 16 GB with block-swap, composite
peak 15,757 MiB. FLUX-family models remain prohibited
([ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md)).

## Error handling

Failures return typed Defers rather than crashing: a busy GPU lock defers with a distinct reason, a
render error defers with detail. The batch path records per-job failures and continues.

## Security and privacy notes

Generation runs local. `run-graph` executes caller-supplied graphs and provisions caller-specified
node packs, which is a trusted-caller interface by design — see
[ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md) for what
protects the environment from it.

## Capability is derived, never declared

`internal/mediacap` answers "what can this box actually render?" from the bindings themselves and
the files they name — the same gates the pipeline routes on. Three verdicts per route:

| Verdict | Meaning | Is it a fault? |
|---|---|---|
| `CONFIGURED` | bound, and every file it names exists | no |
| `NOT CONFIGURED` | no binding on this box; the task defers by design | no |
| `BOUND-BUT-MISSING` | the config names a file that is not there | **yes** — the task defers at call time |

Both reporting surfaces read from it: `local-offload doctor`'s media section (a
`BOUND-BUT-MISSING` route exits non-zero) and the MCP `offload_status` tool's `media.routes`.
Neither states an engine as a constant any more. That mattered on a real node: `offload_status`
hardcoded `"image_engine": "ComfyUI (local)"` and shipped it to an autonomous planner on a box whose
`imagegen_engine` is `sdcpp` and which has no ComfyUI at all, while `doctor` — checking model
aliases only — stayed green as `generate_image` deferred on a render script that was not on disk.
A capability map a planner acts on is worse wrong than absent.

Relative script bindings are resolved against the **executable's** directory (`gpugen.ResolveScript`'s
rule, shared via `ResolveScriptIn`), so the verdict answers the same question the runner will ask.
`node` and `comfy_dir` are reported as prereq rows, and only when a bound route actually needs them —
an sdcpp-only box is never told it is missing ComfyUI. Model-alias routes (vision/STT) are
deliberately absent: their reachability is a live `/v1/models` question that doctor's alias diff
already answers.

## Cross-platform engine resolution

The engines are resolved the same way on every OS, because a tier is a hardware class and not an
operating system:

| What | Rule | Where |
|---|---|---|
| Render script (`render/*.mjs`) | relative → against the **executable's** dir | `gpugen.ResolveScript` / `ResolveScriptIn` |
| ComfyUI venv python | `COMFY_PY`, else `.venv/Scripts/python.exe`, `venv/Scripts/python.exe`, `python_embeded/python.exe`, `.venv/bin/python`, `venv/bin/python`, else `python` (Windows) / `python3` | `render/comfy-lifecycle.mjs` `resolveComfyPy`, shared with `comfy-run-graph.mjs` |
| ComfyUI install dir | `COMFY_DIR` / `comfy_dir`, else `C:/ComfyUI` on Windows and **unbound** elsewhere | `config.DefaultComfyDir` + `resolveComfyDir` |
| Executable binding (`node_path`, `ffmpeg_path`, `sdcpp_bin`) | stat when it is a path, PATH lookup when it is a bare name | `mediacap.binaryPresent` |

Windows candidates are probed **first**, so Windows resolution is byte-identical to what it always
was. This exists because it was not always so: the venv probe was Windows-only and `comfy_dir`
defaulted to `C:/ComfyUI` everywhere, which made ComfyUI unlaunchable on Linux nodes while their
`/fleet/health` still advertised every ComfyUI-backed task. An unbound `comfy_dir` is now
NOT CONFIGURED (a legitimate machine) rather than a path that cannot exist, and `ensureComfy`
refuses with that reason instead of spawning into a bad cwd.

## Observability and debugging

Look at the lock directory first when jobs will not start — a leaked lock blocks everything on the
machine. ComfyUI's own logs cover render failures. `fleet-measure` prints observed VRAM peaks per
task. `local-offload doctor` prints the derived media routes above before it probes the endpoint,
so a broken binding surfaces even when llama-swap is down.

## Testing notes

`render/*.test.mjs` (run with `node --test` from the repo root) covers the lock, lifecycle, batch
semantics, and output parsing. Go-side coverage sits in `internal/pipeline/` for the media dispatch
and defer paths, and `internal/mediacap/` for the derived verdicts.

`crossplatform_lint_test.go` (repo root) is the gate on the resolution rules above: a runner that
probes a Windows venv interpreter without a POSIX one, a drive-letter literal in shared Go with no
`runtime.GOOS` branch, or a `.exe` in a tier `config_seed` fails CI. The two `amd-rdna3*` seeds are
recorded as known offenders with their reason rather than silently skipped — a NEW one fails.

## Common pitfalls

- Assuming the free step unloads everything — it deliberately preserves the CPU memory stack.
- Treating `grade` or `finish` as verbs. They are ops inside `edit-image`.
- Using `perspective` — the op is `perspective_composite`.
- Assuming the pipeline reorders ops for you — it does not; `finish` should be placed last by the
  caller, and the validator does not enforce it.
- Expecting `--auto-text` to defer always. That gate was removed after its evaluation passed.
- Expecting concurrency on one machine. Concurrency is a fleet concern.

## Source map

- [`render/gpu-lock.mjs`](../../render/gpu-lock.mjs) — slot, free step, teardown
- [`render/comfy-lifecycle.mjs`](../../render/comfy-lifecycle.mjs) — cold start, warm flag
- [`render/comfy-generate.mjs`](../../render/comfy-generate.mjs) — single and batch render
- [`render/sdcpp-generate.mjs`](../../render/sdcpp-generate.mjs) — the sdcpp engine (flag mapping
  to the pinned sd.cpp CLI lives here)
- [`render/edit_image.py`](../../render/edit_image.py) — the edit ops
- [`internal/pipeline/inpaint_autotext.go`](../../internal/pipeline/inpaint_autotext.go) — auto-text
  localization and its validation envelope
- [`internal/imagegen/`](../../internal/imagegen/), [`internal/gpugen/`](../../internal/gpugen/)
- [`internal/mediacap/mediacap.go`](../../internal/mediacap/mediacap.go) — derived capability, one
  source for both `doctor` and `offload_status`

## Related docs

- [../flows/zero-warm-generation.md](../flows/zero-warm-generation.md)
- [../architecture/decisions/0009-zero-warm-gpu-lifecycle.md](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md)
- [../architecture/decisions/0011-flux-family-license-prohibition.md](../architecture/decisions/0011-flux-family-license-prohibition.md)
