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

**Composition lane (ADR 0059).** `offload_compose_video` / `compose-video` renders
designed HTML/CSS motion graphics to video with **HyperFrames**: title cards, lower thirds, kinetic
type and alpha overlays. It is CPU-class: software GL and CPU encode, with no GPU lease. It is
pinned, env-scrubbed and machine-gated. See
[Composition (HyperFrames)](#composition-hyperframes).

## Questions this doc answers

- What happens to the GPU during a render, and what state is the machine left in?
- How does the composition lane stay local, pinned and off the GPU?
- Which models are bound, and where is that configured?
- What are the image-editing operations, and how are they invoked?
- Which of the three edit-shaped routes fits a given change?
- When is batching worth it?
- Why is FLUX not an option?
- How does a non-commercial model (Qwen-Image-2.1) ship, and how is every result it makes tagged?
- Which card does a ComfyUI route render on, and how is that pinned per binding?

## Scope

The generation verbs and MCP tools, the GPU lock and zero-warm lifecycle, warm batch mode, the
inpainting route, the generative instruction-edit route, the edit-operation pack, per-machine
model bindings, named license-tagged families, the per-binding ComfyUI launch profile, and the
CPU-class composition lane (HyperFrames).

## Non-scope

- Arbitrary caller-supplied graphs and node provisioning → the run-graph flow at
  [../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md)
- Serving text tiers → [setup-installer.md](setup-installer.md)
- VRAM footprint measurement for fleet dispatch → [fleet-node.md](fleet-node.md)

## Key concepts

**GPU Lock** — a single-slot, cross-process lock; only one GPU-heavy job runs at a time per machine.
**Zero-Warm** — no GPU residency persists between jobs. **Warm Batch** — an opt-in session where the
checkpoint loads once for N renders. **Op** — one image-editing operation inside `edit-image`.
**Named family** — an opt-in image or edit binding beside the node's default one, selected per
request by `family` and carrying its own license (ADR 0058). **Launch profile** — the ComfyUI launch
flags a binding needs (`comfy_cuda_device`, `comfy_dynamic_vram`, `comfy_extra_args`).

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

**One submission layer, CLI-preferred.** Every ComfyUI runner submits, polls, and retrieves
through `render/comfy-submit.mjs` (0.56.0; before it, six runners each carried their own copy of
the raw `POST /prompt` → poll `/history` → `GET /view` block, and only `comfy-render.mjs` had the
dead-server watchdog). Submission goes through the vendored
[`comfyui-pp-cli`](printed-clis.md) when a binary is resolvable — `COMFYUI_PP_CLI` env (loud-fail
if wrong), a local `tools/comfyui/bin` build, or PATH — which buys the idempotent submission
lease (an identical in-flight graph attaches instead of double-rendering; a stale or unverifiable
lease force-resubmits, preserving the raw path's always-POST behavior), typed
accept/reject/partial-accept outcomes with `node_errors` verbatim, a durable run row, and, after
the render, the authoritative `execution_start -> execution_success` timing (one `timing …`
stdout line; the server log's "Prompt executed" line is never parsed by either side). No binary →
the raw HTTP path, byte-identical to the pre-0.56.0 runners (per-run `client_id`, same POST
body); CLI-mode local pre-POST failures also fall back to raw, loudly. Polling never goes through
the CLI: the loop needs the dead-server watchdog (`COMFY_DEAD_SEC`, default 240 s — abort and
release the GPU slot when the server stops answering) and the suspend/resume fence, which the
CLI's `wait` does not provide; `/view` bytes are fetched raw for exact file fidelity. All six
runners now share that hardened loop.

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
falls back to the raw prompt, records the reason, and renders anyway. The no-new-quotes rule is
also STATED in the system prompt (0.62.1), not just enforced: with only the span rule stated,
Gemma-class refiners quoted the prompt's subject itself and the guard rejected nearly every
span-less refinement (measured: gemma-4-12b 2% -> 87% refine rate with the sentence). Span guarding is computed in
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
`flatten_design`, and `instantiate_design` (the last two drive GIMP — how GIMP 3.2 is driven headless, what its batch mode, PDB, GEGL filters and export procedures really do on this build, and every measured trap, is documented in [`skill/gimp/reference/`](../../skill/gimp/reference/README.md)). `finish` is delivery sharpening
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

**Upscale** (`upscale-image` / `offload_upscale_image`) enlarges a still with an ESRGAN-family model
through the core ComfyUI nodes (`UpscaleModelLoader` → `ImageUpscaleWithModel`; `render/wf-upscale.mjs`,
runner `render/comfy-upscale.mjs`). The model is per-machine: `upscale_model` names a ComfyUI
`upscale_models/` filename, and when it is empty the route binds `videogen_upscale_model` — the video
route's post-decode upscaler is the same kind of file, so a box that upscales video upscales stills
with no extra key (`config.EffectiveUpscaleModel`; the pipeline gate and `offload_status` both read
it). With no size request the output is whatever the model produces (its own factor). `scale` is
the overall factor relative to the SOURCE and is made exact: the runner measures the source header
(PNG/JPEG/WebP, no decode) and pins `round(src × scale)` with `ImageScale` (crop disabled), so the
result does not depend on what the model's filename claims — a `2xLexicaRRDBNet` and a
`4x-UltraSharp` both return exactly 2× for `scale: 2`. Only when the header cannot be read does the
builder fall back to `ImageScaleBy` at `scale / nativeFactor(filename)` (`4x-UltraSharp` → 4,
`RealESRGAN_x2plus` → 2, unknown → 4), and the runner says so on stderr. `width`+`height` pin the
size yourself and win over `scale`. The pipeline gates before any GPU work — a half-given or
negative size, a size above ComfyUI's 16384 limit, a non-positive `scale`, a `method` outside the
five core resamplers, a `scale` whose result would exceed that limit, or an absolute / parent-escaping
`model` override (subfolder names such as `ESRGAN/4x.pth` are valid ComfyUI names and pass) all defer
with the offending value named — and the runner pre-flights the same builder before taking the slot
(ComfyUI's `scale_by` range 0.01–8 is enforced there too). **After the render the written file is
verified**: its header must be readable (PNG/JPEG/WebP, plus GIF through the decoder the binary
already registers — an unreadable file defers rather than returning a size-less success), and when a
size was requested its dimensions must match within 2 px or the call defers naming produced-vs-
expected. Both sides measure the same three advertised source formats (PNG/JPEG/WebP — Go has its own
header reader mirroring `image-size.mjs`), so for those the runner pins the size and a mismatch means
the renderer did not honor it, which the defer says. A GIF source is the one shape Go measures and the
runner cannot: the runner falls back to the model's filename factor, Go still expects `src × scale`,
and the two agree only when that guess was right — a wrong guess is caught as a mismatch whose defer
names the fallback and the fix (pin the size, or use PNG/JPEG/WebP). The result carries
`width`/`height` read from the file and `factor`, the measured output/source ratio — or `factor_x` /
`factor_y` when a pinned size is non-uniform.
`ImageUpscaleWithModel` tiles on OOM by itself, so there is no tile knob. It synthesizes detail, so
it is an enlargement tool, not a faithful restore — an exact resample is `edit-image`'s `resize`.
`upscale_script` ships as a default (the runner is generic, like `run_graph_script`); the model is a
binding, and since 0.78.0 every image-capable tier seeds `upscale_model: 4x-UltraSharp.pth` (the
three tiers with no image route stay unseeded and defer). A per-request `model` override (a name
relative to `upscale_models/`) works even on a box that binds none. The runner is the ComfyUI graph,
so an sd.cpp-primary box without ComfyUI defers with the `comfyui` prereq missing — an sd.cpp arm
(stable-diffusion.cpp has native ESRGAN support) is a documented follow-up.

**Generative instruction edit** (`offload_edit_image_generative`, MCP-only — no CLI verb) is the
third edit-shaped route, for changes that are global or diffuse and have no drawable region: "make
it snowing heavily", "turn the leather into fur". A Qwen-Image-Edit-class model reads the source
through its own vision encoder and re-renders the whole frame, so fine detail outside the intended
change will shift — prefer inpainting whenever a mask is possible. The route is bound per machine by
the `gen_edit_*` keys (named `gen_edit`, not `edit`, because `edit_*` is the deterministic PIL
route). Since 0.132.5 every ≥16 GB ComfyUI tier seeds them (and `blackwell-8` in its RAM layer):
`gen_edit_script` `render/comfy-edit.mjs`, `gen_edit_unet` `qwen-image-edit-2511-Q5_1.gguf`,
`gen_edit_preset` `lightning8`. A box without that seed defers until it binds `gen_edit_script`
and `gen_edit_unet`. `gen_edit_preset` pairs steps+cfg+LoRA as a matched
triple (`full` | `lightning8`, the default | `lightning4`), because a Lightning LoRA at full
steps/cfg produces mush and the base at 4 steps produces noise, and either renders "successfully".
The builder itself defaults neither steps nor cfg — it throws if either is missing
(`render/wf-qwen-image-edit.mjs`) — but the runner fills a missing half straight from the preset
(`render/comfy-edit.mjs`), so that throw is unreachable here and **a half-override is silently
completed rather than rejected**. This route therefore has NO pair guard, unlike the qwen-image
GENERATION route, which exits 2 on a half-override. The same applies at the binding layer:
`gen_edit_steps` and `gen_edit_cfg` are independent keys emitted independently by the harness, so
binding one alone silently runs at the preset's other half. Set both or neither, and prefer
switching preset over hand-setting either. Lightning is applied as a **LoRA**, never a
pre-merged checkpoint, so it composes with any quantisation (GGUF bindings load via
`UnetLoaderGGUF`).

**Output resolution follows the source, within 0.9-2.0 MP.** The graph scales the input once, and
that scaled image is what the sampler denoises — so the scaler's output size *is* the edit's output
size. It is `ImageScaleToTotalPixels` at a `resolution_steps: 16` snap (Qwen-Image's 8x VAE stride x
DiT patch size 2; without the snap the latent needs padding and edits come back soft and subtly
warped). The runner measures the source file — PNG, JPEG or WebP headers, no decode — and targets its
actual megapixels, so a source inside the band renders at exactly its own size: 2048x1024 in,
2048x1024 out. `gen_edit_megapixels` overrides the target when every edit on a machine should land on
one fixed size; an unreadable header falls back to the ceiling, the non-destructive direction.

The band is deliberate at both ends. The 2.0 ceiling bounds VRAM and time on a seat running under a
~15 GB unet, so a 24 MP phone photo comes down instead of taking the box out. The 0.9 floor scales a
small source *up* onto the model's working canvas — which is what the previous node did anyway, and
what both official templates do; they normalise rather than preserve. 0.9 specifically, because it
sits just under the whole 1-MP-class grid (1536x640 is the lowest at 0.9375, then 1216x832 at 0.965,
1344x768 and 1152x896 at 0.984, 1024x1024 at 1.0), so every one of those keeps scale factor 1.0. A
floor of 1.0 would have quietly stretched a 1344x768 source to 1360x768. The floor also keeps the
arithmetic out of two holes found by replaying ComfyUI's own formula over real files: a 97x53
thumbnail resolves to 0.0049 MP, under the node's declared 0.01 minimum, and a pathological aspect
ratio can snap a dimension to 0 — a graph ComfyUI cannot execute.

This replaced the shipped template's `FluxKontextImageScale` (fixed 0.44.0-0.48.0). That node takes
no size argument — it hard-snaps to the nearest-aspect entry of Flux-Kontext's 17-entry table, whose
largest entry is 1024x1024, so **every** edit came back at ~1 MP or less (a 2048x1024 source
returned 1456x720) with no configuration that could raise it. It bought the graph nothing:
`TextEncodeQwenImageEditPlus` rescales its own inputs internally (384x384 for the vision tokens,
~1 MP snapped to 8 for the reference latents), so the pre-scaler only ever fed the canvas.

The three edit-shaped routes in one line each: `edit_image` = deterministic PIL ops (exact, CPU, no
GPU lock); `inpaint_image` = re-denoise inside a mask you supply (everything outside the mask is
preserved in INTENT, but is NOT pixel-identical: `grow_mask` dilates + feathers the mask by 16 px in
latent space by default, and the whole frame is VAE round-tripped on decode — there is no
composite-back node in the graph; pass `grow_mask: 0` for the tightest mask);
`edit_image_generative` = maskless instruction edit (the whole frame re-renders).

**Per-box device/launch seams (J4).** Three env knobs decouple shared code from CUDA-box
assumptions, all default-preserving: `COMFY_COMPUTE_DEVICE` overrides the DisTorch2 loaders'
`compute_device` in the Wan graph (was hardcoded `cuda:0`); `COMFY_EXTRA_ARGS` appends verbatim
flags to the managed ComfyUI launch (whitespace-split — a flag VALUE containing spaces is
inexpressible, fine for ComfyUI-style flags); `TTS_DEVICE` overrides the
Chatterbox worker's torch device auto-pick. Since the launch profile (below), `COMFY_EXTRA_ARGS`
can also come from the config (`comfy_extra_args`), and the harness sets `COMFY_CUDA_DEVICE` /
`COMFY_DYNAMIC_VRAM` per binding.

## Data and state

Rendered outputs land in the configured media directory or a caller-supplied `out_dir`. Footprint
observations are recorded as a side effect of successful renders — see
[fleet-node.md](fleet-node.md).

## Interfaces and entry points

CLI verbs `generate-image` (`--family`, `--transparent`), `inpaint-image`, `generate-video`,
`generate-audio`, `generate-svg`, `edit-image`, `media`, `run-graph`, `compose-video`; the matching
`offload_*` MCP tools (`family` / `transparent` on `offload_generate_image`; `family` / `images` /
`transparent` on `offload_edit_image_generative`). The generative instruction edit is MCP-only
(`offload_edit_image_generative`); ad-hoc runs use `render/comfy-edit.mjs` directly. The
composition lane is also a fleet task (`compose-video`), restricted to vetted templates.

## Dependencies

A local ComfyUI installation (`comfy_dir`), the model files named by the bindings below, GIMP for the
design ops, and the Node renderer scripts under `render/`. The composition lane needs Node >= 22,
the pinned HyperFrames install (`setup/hyperframes/` lockfile), its pinned chrome-headless-shell,
and ffmpeg + ffprobe.

## Downstream effects

The GPU lock is machine-wide: a long render blocks vision tasks, which defer with `gpu_busy` rather
than queueing. Consumers of the media outputs — notably the creative pipeline workflows — depend on
the output envelope shape.

## Invariants and assumptions

1. **Zero-warm by default.** Nothing GPU-resident survives a job.
2. **The CPU memory stack is never unloaded** by the free step.
3. Only one GPU-heavy job at a time, per machine.
   **Corollary — never post a graph straight to ComfyUI (`:8188`).** Every render enters
   through this system so it takes the machine-wide lease; a direct POST is unprotected
   rather than refused (see [gpu-lease](gpu-lease.md) and
   [ADR 0018](../architecture/decisions/0018-machine-wide-fenced-gpu-lease.md)). This covers
   any tool that drives ComfyUI directly — the official Comfy-MCP server, and this repo's own
   `tools/comfyui` CLI, whose supported posture is as the harness's submit backend (where the
   lease is already held) rather than standalone. Use `run-graph` for graphs the templates do
   not cover, or `gpu reserve --class media` to hold the lease by hand.
4. **No FLUX-family model is ever added** — see
   [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md). The binding reason
   is the non-commercial licence, not VRAM; a bigger card does not reopen it.

## Model bindings

Bound per machine through flat config keys, so the same code serves different hardware:

| Concern | Keys |
|---|---|
| Image | `imagegen_family`, `imagegen_ckpt`, `imagegen_vae`, `imagegen_steps/cfg/sampler/scheduler`, `imagegen_preset/clip/lora/lora_strength/shift` (qwen-image knobs), `imagegen_pool_vvram_gb/pool_compute/pool_donor` (krea2 pooled loading), `imagegen_refiner_model/refiner_timeout_sec` (opt-in prompt refiner) |
| Inpaint | `inpaint_ckpt`, `inpaint_vae`, `inpaint_steps/cfg/sampler/scheduler` |
| Generative edit | `gen_edit_script`, `gen_edit_unet`, `gen_edit_preset` (`full`/`lightning8`/`lightning4`), `gen_edit_clip/vae/lora/lora_strength`, `gen_edit_steps/cfg/sampler/scheduler`, `gen_edit_megapixels` (0 = follow the source, held within 0.9-2.0), `gen_edit_timeout_sec` |
| Upscale | `upscale_script` (shipped default `render/comfy-upscale.mjs`), `upscale_model` (ComfyUI `upscale_models/` filename; empty = `videogen_upscale_model`), `upscale_timeout_sec` (600) |
| Video | `videogen_family` (`""`/`wan22` = Wan 2.2; `ltx25` = LTX-2.5 joint-AV), `videogen_unet_high`, `videogen_unet_low`, `videogen_text_encoder`, `videogen_upscale_model` (Wan keys); `videogen_transformer`, `videogen_video_vae`, `videogen_audio_vae`, `videogen_latent_upscaler`, `videogen_fps`, `videogen_pool_vvram_gb/pool_compute/pool_donor` (LTX-2.5 keys) |
| Audio | `voicegen_*`, `musicgen_script`; `tts_endpoint` / `tts_model` (default `tts-1`) / `tts_voice` / `tts_api_key` (0.113.25: an OpenAI-compatible speech SERVER for `generate_audio kind=voice` — `voice: endpoint`, or the default on a box with no `voicegen_script`; `internal/ttsclient` POSTs `/v1/audio/speech`, writes the WAV atomically, defers naming the server's words on any non-audio answer, takes no media lease because the server owns its GPU; e.g. VoiceStudio on `http://127.0.0.1:3900`) |
| Qwen-Image-2.1 (family `qwen-image-2.1`) | `imagegen_ckpt` + `imagegen_clip` + `imagegen_vae` (all three REQUIRED — the builder has no defaults), `imagegen_schedule` (`official` default / `comfy`), `imagegen_steps/cfg` (both or neither; official 40 / 1.0); edit: `gen_edit_family: "qwen-image-2.1"`, `gen_edit_unet/clip/vae`, `gen_edit_resolution` (0 = 1024), `gen_edit_cache_device` (`auto`/`gpu`/`cpu`/`off`) |
| Named families + license (ADR 0058) | `imagegen_families`, `gen_edit_families` (name → overlay + `license` + `commercial_use`); `imagegen_license`/`imagegen_commercial_use`, `gen_edit_license`/`gen_edit_commercial_use` (the default binding's own tag, both or neither) |
| ComfyUI | `comfy_dir`, per-task `*_script` and `*_timeout_sec`; launch profile `comfy_cuda_device`, `comfy_dynamic_vram` (`on`/`off`/`""`), `comfy_extra_args` |
| Composition (HyperFrames) | `compose_script` (`render/compose-hyperframes.mjs`), `hyperframes_dir` (the pinned install), `hyperframes_browser_path` (the pinned chrome-headless-shell), `compose_timeout_sec` (1800), `compose_workers` (`""`/`auto` or 1-24), `compose_cache_dir` (`""` = `<media_dir>/.compose-cache`), `compose_quality` (`high`); `ffmpeg_path` (ffprobe beside it). All three route keys empty = NOT CONFIGURED; the installer binds them |

Hardware profiles seed these. The single-card 16 GB tiers (`blackwell-16`, `ampere-16`, `volta-16`)
and the 8 GB tiers' RAM layer bind **HiDream-O1** via `imagegen_family` — the official graph for that
DiT, never the generic SDXL graph; the pooled and 32 GB-class Blackwell tiers (`blackwell-2x16`,
`-3x16`, `-32`, `-48`, `-72`) bind **Krea 2 Turbo** (below). No tier seeds a non-commercial family.
The Wan 2.2 video tiers bind **Wan 2.2 Q8_0** experts with an fp16 text encoder. **RealVisXL** is the SDXL-class inpainting default. The 8 GB
tiers stay SDXL-class for image generation until O1 on 8 GB is verified on real hardware.

**LTX-2.5** (`videogen_family: "ltx25"`) is the measured 32 GB-class video seat (2026-08-12
three-way, bound 2026-08-14, behavior-proven 2026-08-15): the 22B distilled int8 DiT renders
1920×1088-class output with a **jointly generated soundtrack** through the official template's
two-pass recipe (half-res base pass + ×2 latent spatial upscale + refine, fixed distilled
sigmas, dual CFG 1/1) — vs Wan 2.2's silent 1280×720 @ 16 fps in 1134 s. **The dual-16 GB
binding is POOLED at 1280×704 (operator decision 2026-08-15: the pool doctrine outranks
resolution).** The measured constraint: the int8 file upcasts to bf16 at compute —
**39.11 GB loaded** — which exceeds a 2×16 GB physical pool, so at 1920×1088 DisTorch
pooling OOMs at every `virtual_vram_gb` (weights fit above ~25, but full-resolution stage-2
activations then don't). At 1280×704 with `videogen_pool_vvram_gb: 30` the pooled render is
behavior-proven (5 s + joint audio in ~190 s, both cards loaded, zero OOM); pooled serving
requires the `--disable-dynamic-vram` launch flag (MultiGPU #191), same as the krea2 image
seat. Full-resolution 1920×1088 remains available as a per-deployment STREAMING alternative
(pool keys unset, dynamic VRAM on — measured ~210 s and 1.65–2.3× faster for bf16 graphs).
The builder deliberately drops the template's gemma4_e2b prompt-enhancer branch (the
harness's own planner does prompt expansion) and pairs the convrot transformer with the
**conv** video VAE. Wan 2.2 stays available per-request via `model: "wan"`.

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

**Krea 2 Turbo** is the 32GB-class *generation* seat (operator blind-verdict 2026-08-14 — it
won both decided bake-off pairs against Qwen-Image-2512 bf16, including the text-rendering
probe): `imagegen_family: "krea2"` selects its model-correct graph, which rides the Qwen-Image
stack (shared `qwen_image_vae`, Qwen3-VL encoder via `CLIPLoader type "krea2"`, default
`qwen3vl_4b_bf16.safetensors`) but with its OWN template — **no** `ModelSamplingAuraFlow`
shift node, the regular `EmptyLatentImage`, and the turbo recipe **baked into the weights**
(8 steps / cfg 1.0 / euler / simple; no LoRA branch, no `full` preset — many steps at high
cfg burns a distilled model out). The same two-key binding rule as qwen-image applies
(`imagegen_vae` must not be `"builtin"`; steps/cfg together or neither). **Pooling (the
32GB-pool doctrine):** `imagegen_pool_vvram_gb` > 0 loads the DiT through
`UNETLoaderDisTorch2MultiGPU` in RATIO mode, donating that many GiB from
`imagegen_pool_compute` (default `cuda:0`) to `imagegen_pool_donor` (default `cuda:1`); the
byte-expert allocation string is deliberately unused (its reservation half is a structural
no-op — the node reads only the post-`#` segment expert mode leaves empty). Pooled
safetensor serving additionally requires launching ComfyUI with `--disable-dynamic-vram`
until ComfyUI-MultiGPU #191 lands — carried per-box by the `COMFY_EXTRA_ARGS` launch seam,
never by shared code. **Windows multi-GPU visibility (ComfyUI >= 0.34):** upstream hides every CUDA device but the first on Windows unless devices are explicitly selected; `ensureComfy` restores full visibility for the spawned child via `cudaVisibleEnv()` (env-based; multi-GPU spawns also get `--disable-pinned-memory` per upstream guidance) whenever the operator has not already scoped devices — without this, every pooled graph fails prompt validation with `donor_device: 'cuda:1' not in ['cpu', 'cuda:0']`. Zero/empty pool keys render single-GPU (the small-fleet shape).

The recommended **≥16 GB image-*edit* primitive is Qwen-Image-Edit-2511** (Apache-2.0). Since the
generative-edit route landed (0.44.0) it is a first-class `gen_edit_*` config binding — set
`gen_edit_unet` to the 2511 file and the harness drives it through `render/comfy-edit.mjs` /
`render/wf-qwen-image-edit.mjs`; since 0.132.5 the ≥16 GB ComfyUI tiers seed it (see above).
Callers with their own graphs can still reach the model through
[run-graph](../flows/run-graph-manifest-satisfaction.md) with the model set declared in the node
manifest (e.g. the creative-marketing-pipelines scene-swap). **Pin a `_1` GGUF quant
(`Q4_1`/`Q5_1`), never a `_K_` one:** 2511 K-quants fail `UnetLoaderGGUF` with
`cannot reshape array` even on byte-perfect files (city96/ComfyUI-GGUF #247). Measured on
`blackwell-16` (<node-b>, then a single RTX 5060 Ti 16 GB) 2026-07-19: Q5_1 (15.4 GB) + fp8 encoder fits 16 GB with block-swap, composite peak
15,757 MiB. FLUX-family models remain prohibited
([ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md)).

## Named families, launch profiles and license tags (ADR 0058)

A node has ONE default image binding (`imagegen_*`) and ONE default edit binding (`gen_edit_*`).
`imagegen_families` and `gen_edit_families` add **named** bindings beside them. A request selects one
with `family` — `offload_generate_image` / `offload_edit_image_generative`, `generate-image --family`,
the fleet `image-gen` payload — and a request without it renders exactly what it rendered before
families existed. This is the only way a model whose license forbids commercial use ships in this
repository ([ADR 0058](../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md),
amending the reasoning of [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md)
for named, tagged families): never a default, never a seed default, always an explicit per-request
opt-in whose result carries its license.

**An overlay is a complete binding.** It is a JSON object of the route's own keys — image:
`imagegen_*`, `sdcpp_*`, `comfy_*`; edit: `gen_edit_*`, `comfy_*` — plus two REQUIRED meta keys,
`license` (string) and `commercial_use` (bool). Resolution (`config.ResolveImageFamily` /
`ResolveEditFamily`) starts from the node's config, **clears every model-binding key** of that route
(checkpoint, family, VAE, text encoder, LoRA, preset, sampler knobs, pool keys, sdcpp model files),
keeps the route keys (script, engine, timeout, reserve) and the launch keys, then applies the
overlay. So a family never inherits the default's LoRA or pool by accident. The config load refuses:
an overlay key outside those prefixes or not a config key (typo), a forbidden key (`*_families`,
`*_license`, the prompt refiner), a missing/empty `license`, a missing `commercial_use`, a family name
outside `[a-z0-9._-]`, or a name that collides with the default binding's own family. Every family is
also checked by the same binding-trap warnings as the default, with its name in front.

**Every result is tagged.** Results carry `family` (the default binding's own name for an unnamed
request), and `license` + `commercial_use` whenever the binding declares them; a `commercial_use:
false` result also carries `license_note` ("research/evaluation use only under <license>; not for
commercial work"). The ledger row carries `license` (`ledger.Entry.License`; absent = UNKNOWN, never
"safe"). `offload_status` lists `media.image_families` / `media.edit_families` — name, graph family,
engine, checkpoint, license, commercial_use (null = undeclared) and the route verdict — and
`/fleet/health` publishes `image_families` with the same license flags. A warm batch
(`generate-image --batch`) always renders the default binding, so every batch item — and the batch
payload's top level — carries that binding's `family` and, when declared, its `license`,
`commercial_use` and `license_note`. `width`/`height` in a
`generate_image` or `edit_image_generative` result are **measured** from the written file
(`imagegen.OutputSize`), not echoed from the request.

**Unknown family, unsupported flag.** An unknown `family` defers with the list this node serves.
`transparent` is honoured only by the `qwen-image-2.1` graph on ComfyUI (the only RGBA VAE); any other
binding defers rather than render opaque. `images` (multi-reference) needs a 2.1 edit family and is
capped at 10 images in all, target included; a 2511 `preset` on a 2.1 edit defers by name. The render
runner closes the same gap one layer down: `comfy-render.mjs --family` is a closed set, and an
unknown value exits 2 before any GPU work — it used to render the generic SDXL graph silently.
`generate-image --batch` renders the default binding only; `--family` with `--batch` is refused.

**`doctor` sees the whole family.** `media.routes` gains `generate_image:<name>` and
`edit_image_generative:<name>`: CONFIGURED only when the family's script resolves AND every model file
its graph opens sits in the class directory the loader reads (resolved under the family's own
`comfy_dir`); the verdict leads with `NON-COMMERCIAL (<license>)` for a research family. The `comfyui
model bindings` section also resolves the files a binding's graph loads WITHOUT a key naming them — a
preset's Lightning LoRA (`qwen-image` `lightning4`, 2511 `lightning8`/`lightning4`) and a builder's
default text encoder and VAE — so an edit route bound to preset `lightning8` with its LoRA on no models
root is a `MISSING` row instead of a green doctor and a failed render.

### Qwen-Image-2.1 (`render/wf-qwen-image-21.mjs`)

A 7B single-stream DiT with a Qwen3-VL-8B encoder and a new 4-channel (RGBA) VAE — not a variant of
Qwen-Image 2512, whose graph cannot drive it. Needs **ComfyUI ≥ v0.37.0** (the nodes arrived in PR
#16400; master ≥ `95539f56` adds the KV-cache placement fix #16429). Weights: **Qwen Research License**
(non-commercial) — bind it only as a named family with `"license": "Qwen Research License",
"commercial_use": false`. The download set is in `setup/SETUP-AGENT.md`.

- **T2I graph:** `UNETLoader` + `CLIPLoader(type "qwen_image")` + `VAELoader` → `TextEncodeQwenImage21`
  → sampler → `VAEDecode` (4-channel) → `SplitImageWithAlpha` (opaque RGB, the default) → `SaveImage`.
  `EmptyLatentImage` (the sampler reshapes it to the model's 64-channel /16 latent); no
  `ModelSamplingAuraFlow`, no SD3 latent. Width/height snap DOWN to /32 (floor 256); default 2048×2048,
  40 steps, cfg 1.0, euler — the official recipe. `--ckpt`, `--clip` and `--vae` are all required; a
  builtin VAE, a `.gguf` UNET (no GGUF loader is wired; 2.1 GGUFs need the leejet ComfyUI-GGUF fork)
  and pool flags (v1 is single-card) are refused.
- **Schedules (`imagegen_schedule`):** `official` (default) = the model repo's diffusers
  `FlowMatchEulerDiscreteScheduler` — dynamic mu from the target token count on the 256→8192 line
  (extrapolated past it: 2048² → mu 1.3129), exponential time shift, `shift_terminal` 0.02 — computed
  in JS and fed through `ManualSigmas` + `SamplerCustomAdvanced` (`BasicGuider` at cfg 1, `CFGGuider`
  otherwise). Golden-tested to 1e-6 against the real diffusers scheduler
  (`render/testdata/qwen-image-21-sigmas.golden.json`, generator beside it). Values are printed
  fixed-point because `ManualSigmas`' parser has no exponent support. `comfy` = `KSampler(euler,
  simple)` with ComfyUI's fixed model shift (0.69 at every size). ComfyUI #16447 contests which looks
  better at 2K; the binding picks.
- **Transparency:** `transparent: true` wraps the prompt in the official RGBA template ("This is an
  RGBA image with transparency. … The image has alpha channel and the background is transparent.")
  and keeps the alpha channel; the default splits it off, so an ordinary prompt never hands a
  partially-transparent PNG to a compositor.
- **Edit graph (`gen_edit_family: "qwen-image-2.1"`):** `LoadImage` per image → `TextEncodeQwenImage21`
  with `vae` and `images.image_1..N` (image_1 = the edit target; references are `<image2>`…`<image10>`
  in the prompt) → `QwenImage21Cache(device, dtype)` on the model path → `KSampler` on the encoder's
  own latent (output 2, on the target's grid) → decode. `comfy-edit.mjs --ref` repeats, each staged
  into `<COMFY_DIR>/input` and removed afterwards. `gen_edit_resolution` (default 1024; 0 keeps each
  image's size) and `gen_edit_cache_device` (`gpu`/`off` stay out of the host-RAM prefetch path that
  ComfyUI #16443 aborts in on dynamic-VRAM edits) are its knobs; the 2511-only flags (preset, LoRA,
  megapixels) are refused on it.
- **Footprint key:** `qwen-image-2.1` renders are bucketed by the precision in the DiT's filename
  (`bf16`, `int8`, `nvfp4`, …) — the peaks differ about 2×.
- **Known open upstream defects (2026-09-22):** #16435 (grid-dependent noise on some edit grids) and
  #16443 (dynamic-VRAM edit abort, fix PR #16450 open). Both are why 2511 stays the default edit seat.

### Launch profile (`comfy_cuda_device`, `comfy_dynamic_vram`, `comfy_extra_args`)

The harness launches ComfyUI on demand (`render/comfy-lifecycle.mjs`). Before this, every launch
took its flags from the process's `COMFY_EXTRA_ARGS` alone, with every card visible and no device
flag, so every single-card graph rendered on ComfyUI's default device — on a mixed box its
**fastest** card, which on the three-card tier is the display card. A binding now carries a profile,
handed to the runner as env:

| key | env | launch effect |
|---|---|---|
| `comfy_cuda_device` | `COMFY_CUDA_DEVICE` | `--cuda-device <n>` (index or comma list, in **ComfyUI's device order** — the order the `*_pool_*` `cuda:N` keys use, fastest-first unless `CUDA_DEVICE_ORDER` says otherwise, NOT nvidia-smi's PCI order). It hides every other card. Never `--default-device`: that only reorders the visible list and silently re-maps what every pool key means. |
| `comfy_dynamic_vram` | `COMFY_DYNAMIC_VRAM` | `on` strips `--disable-dynamic-vram` from the extra args (a bf16 DiT larger than one card streams instead of partial-offloading); `off` adds it (DisTorch2 pooled seats need it off, MultiGPU #191); `""` leaves the args alone |
| `comfy_extra_args` | `COMFY_EXTRA_ARGS` | verbatim extra flags; `""` = inherit the process env, as before. A `--cuda-device`/`--default-device` here loses to `comfy_cuda_device` |

**Where the pin applies.** `comfy_cuda_device` is applied ONLY to the single-card routes — image
generation when the binding does not pool, generative edit, upscale, inpaint, animate and music. It is
never applied to a pooled image or video seat (the pool keys name its cards, and the blackwell-3x16
video pool must COMPUTE on `cuda:0`, the MultiGPU #220 exception a pin would hide), and never to
`run-graph` (the caller's graph owns its placement; it still gets the launch-wide keys). The
`COMFY_CUDA_DEVICE` / `COMFY_DYNAMIC_VRAM` env is always set (empty when unbound), so a value in the
operator's shell can never pin a route whose binding did not ask for it. A family overlay may carry
its own `comfy_*` keys: the recipe planned for 2.1 on a three-card box is `comfy_cuda_device "2"` +
`comfy_dynamic_vram "on"` on the family only, so the pooled default keeps `--disable-dynamic-vram`
(UNMEASURED until the family's arms run). The config load warns on `comfy_dynamic_vram "on"` or a
`comfy_cuda_device` together with pool keys.

**A running ComfyUI that contradicts the profile is never reused silently.** Before reusing an
instance already listening, the runner reads `GET /system_stats` `system.argv`. If a device pin or a
dynamic-VRAM setting is requested and the argv does not honour it: a harness-launched instance
(fingerprint: pid + exact argv, `render/comfy-ownership.mjs`) whose owner is gone is stopped and
relaunched with the right flags; anything else fails with a greppable
`COMFY-PROFILE-MISMATCH: …` line and the render defers — a foreign instance is never killed. An
unreadable argv with a profile requested is a mismatch, never a pass.

**blackwell-3x16 seeds `comfy_cuda_device: "2"`** — `cuda:2` in ComfyUI's order is the 5060 Ti at
PCI B5:00.0 (nvidia-smi index 2). `cuda:0` is the 5070 Ti display card and is excluded by rule;
between the two 5060 Tis the B5:00.0 card is the operator's choice (2026-09-22) because it cools
far better (the 17:00.0 card, `cuda:1`, reached 82 °C under a 2.1 render). And `TestTripleBlackwellNeverSchedulesOntoTheDisplayCard` fails a tier that
seeds a single-card ComfyUI route without a non-display pin.

## Error handling

Failures return typed Defers rather than crashing: a busy GPU lock defers with a distinct reason, a
render error defers with detail. The batch path records per-job failures and continues.

## Security and privacy notes

Generation runs local. `run-graph` executes caller-supplied graphs and provisions caller-specified
node packs, which is a trusted-caller interface by design — see
[ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md) for what
protects the environment from it. `compose_video`'s `html` and `project_dir` inputs are the same
kind of trusted-caller interface: the page runs in a Chrome without a sandbox. The fleet door
therefore accepts vetted templates only. See [Security](#security) below.

**Licenses.** A non-commercial family's output is tagged (`license`, `commercial_use: false`,
`license_note`) and its ledger row carries the license, but the tag is informational: nothing stops
a caller from republishing the file. Do not route brand or client work to a family whose
`commercial_use` is false, and read an absent license as UNKNOWN
([ADR 0058](../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md)).

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

**Model files behind the ComfyUI routes (0.130.4, register F-31).** A route can be `CONFIGURED` while
the model NAME it hands the graph is absent or in the wrong place, and until 0.130.4 that surfaced only
as a graph rejection at render time. `doctor` now prints a `comfyui model bindings` section: every
configured model name (`imagegen_ckpt`, `videogen_unet_high`, `gen_edit_unet`, `upscale_model`, …) is
resolved against the class directory its loader node opens — `checkpoints` for `CheckpointLoaderSimple`,
`diffusion_models` (alias `unet`) for the UNET loaders, `vae`, `text_encoders` (alias `clip`),
`clip_vision`, `loras`, `upscale_models`, `latent_upscale_models` — across `<comfy_dir>/models` and every
`extra_model_paths.yaml` root exactly as ComfyUI's `folder_paths` reads them. Verdicts: `FOUND`, `MISSING`
(no class directory holds it), `MISPLACED` (present only under a class the graph never loads from). A
subfolder name resolves as the loader opens it; a file that moved one level is still reported in its
class. `MISSING`/`MISPLACED` fail doctor; a box with no models root prints no section.

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
- [`render/comfy-lifecycle.mjs`](../../render/comfy-lifecycle.mjs) — cold start, warm flag, the
  launch profile (`launchFlags`, `reuseVerdict`, `COMFY-PROFILE-MISMATCH`)
- [`render/comfy-ownership.mjs`](../../render/comfy-ownership.mjs) — the harness-launch fingerprint
- [`render/comfy-render.mjs`](../../render/comfy-render.mjs) — the image family switch (closed
  `KNOWN_FAMILIES`; unknown `--family` exits 2)
- [`render/wf-qwen-image-21.mjs`](../../render/wf-qwen-image-21.mjs) — the Qwen-Image-2.1 T2I and
  multi-reference edit graphs and the official sigma schedule
  ([golden fixture](../../render/testdata/qwen-image-21-sigmas.golden.json))
- [`render/comfy-generate.mjs`](../../render/comfy-generate.mjs) — single and batch render
- [`render/comfy-edit.mjs`](../../render/comfy-edit.mjs) /
  [`render/wf-qwen-image-edit.mjs`](../../render/wf-qwen-image-edit.mjs) — the generative edit
  lifecycle and its graph builder
- [`render/sdcpp-generate.mjs`](../../render/sdcpp-generate.mjs) — the sdcpp engine (flag mapping
  to the pinned sd.cpp CLI lives here)
- [`render/edit_image.py`](../../render/edit_image.py) — the edit ops
- [`internal/pipeline/inpaint_autotext.go`](../../internal/pipeline/inpaint_autotext.go) — auto-text
  localization and its validation envelope
- [`internal/imagegen/`](../../internal/imagegen/), [`internal/gpugen/`](../../internal/gpugen/)
- [`internal/mediacap/mediacap.go`](../../internal/mediacap/mediacap.go) — derived capability, one
  source for both `doctor` and `offload_status`
- [`internal/mediacap/families.go`](../../internal/mediacap/families.go) — per-family route verdicts,
  preset/builder-implied model files, the family rows `offload_status` publishes
- [`internal/config/families.go`](../../internal/config/families.go) — family overlay validation and
  resolution, license notes (ADR 0058)
- [`render/compose-hyperframes.mjs`](../../render/compose-hyperframes.mjs) — the composition runner:
  env allowlist, subcommand allowlist, `--json` everywhere, lint → check → render → ffprobe gate,
  typed `COMPOSE-FAIL` classes
- [`render/compose-templates/`](../../render/compose-templates/README.md) — the vetted templates and
  the shared, locally declared font kit
- [`internal/pipeline/composevideo.go`](../../internal/pipeline/composevideo.go) — `runComposeVideo`:
  the compose slot, the runner env allowlist (`gpugen.Spec.EnvExact`), typed defers
- [`internal/fleetnode/compose_task.go`](../../internal/fleetnode/compose_task.go) — the
  template-only `compose-video` fleet task
- [`setup/hyperframes/`](../../setup/hyperframes/package.json) — the pinned lockfile the installers
  install from

## Related docs

- [../flows/zero-warm-generation.md](../flows/zero-warm-generation.md)
- [../architecture/decisions/0009-zero-warm-gpu-lifecycle.md](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md)
- [../architecture/decisions/0011-flux-family-license-prohibition.md](../architecture/decisions/0011-flux-family-license-prohibition.md)
- [../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md](../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md)
- [../architecture/decisions/0059-external-cli-media-tool-runs-cpu-class-pinned-env-scrubbed.md](../architecture/decisions/0059-external-cli-media-tool-runs-cpu-class-pinned-env-scrubbed.md)

## Re-encoding ops and `ffmpeg_video_encoder` (0.113.10)

Of the `offload_media` ops only two re-encode video: `trim` with `reencode=true` (exact cuts) and `convert`
when video is kept. Without a setting they use ffmpeg's container default — `libx264` on the CPU. The config
key `ffmpeg_video_encoder` (e.g. `"h264_nvenc"`) moves those two draft/QA paths to the GPU's NVENC block,
which frees CPU threads and does not touch CUDA cores, so a re-encode no longer competes with the seats
for the processor. Stream-copy ops (`trim` default, `concat`, `mux_audio`), `extract_frames` and `probe` are
unaffected. NVENC at a given bitrate trails a slow x264 preset, so final deliverables should stay on the CPU
encoder unless a side-by-side viewing says otherwise; an ffmpeg without the encoder fails the op loudly.

## Composition (HyperFrames)

`offload_compose_video` (CLI `compose-video`, fleet task `compose-video`) turns an HTML/CSS
composition into video with [HyperFrames](https://github.com/heygen-com/hyperframes) (npm
`hyperframes`, Apache-2.0). HyperFrames serves the page locally, seeks headless Chrome one frame at a
time and pipes the frames through ffmpeg. The same inputs therefore produce the same frames: this is
designed, text-exact motion graphics, not generation. Use it for title cards, lower thirds, kinetic
type, stat cards and captions on word timings. With `webm` (VP9 `yuva420p`) or `mov`
(ProRes 4444) it produces alpha overlays that lay over LTX b-roll or talking-head footage.

**Class: CPU.** The lane renders in software GL (`--no-browser-gpu`,
`PRODUCER_BROWSER_GPU_MODE=software`) with CPU encode, so it takes **no GPU lease and no
`withGpuSlot`**. A media lease would make load-triggering text admissions wait
([ADR 0026](../architecture/decisions/0026-text-load-admissions-wait-for-the-media-lease.md)) for
work that never touches a card. One composition runs at a time per process, on its own compose slot
(not `mediaSlot`). A second call waits `gpu_wait_ms` and then defers `compose_busy`. On the fleet,
`compose-video` is exempt from the text concurrency cap for the same reason `accel` is.

**Inputs: exactly one.**

| input | meaning | fleet |
|---|---|---|
| `template` + `variables` | a vetted template under `render/compose-templates/` (shipped: `title-card`, `lower-third`) plus values for its declared variables | yes, the only form |
| `html` | an inline single-file composition, staged as the work dir's `index.html` | no |
| `project_dir` (+ `composition`) | a local composition directory, rendered in place | no |

A template's variables are typed: `string`, `color`, `number`, `boolean` or `enum`. The runner
merges them into the declared defaults in its own copy of the page, so `lint` and `check` judge the
caller's real text and colors. An undeclared or mistyped value defers `BAD_INPUT`. `duration` is a
template parameter: HyperFrames reads a composition's total length from source and never from a
variable, so the runner rewrites the root's `data-duration`.

**Pipeline** (`render/compose-hyperframes.mjs`):

1. `lint --json`, which must report `errorCount` 0.
2. `check --json --no-browser-gpu`, which must exit 0. It covers runtime errors, layout, motion
   and WCAG contrast.
3. `render --batch <one row> --json`. Only `--batch` makes `--json` produce the manifest. The call
   carries `--format`, `--quality`, `--workers`, `--no-browser-gpu`, `--strict`,
   `--strict-variables` and `--no-best-effort`.
4. An ffprobe gate on codec, size, fps, duration (±1 frame), alpha for `webm`/`mov`/`png-sequence`,
   and an audio stream when the page carries `<audio>`.
5. With `snapshots`, `snapshot --at … --describe false --json`, whose frames are saved next to the
   output.

`strict: false` reports lint and check findings without failing on them. The payload is what
ffprobe measured, not what was requested.

**Failure classes.** Every failure is a `deferred:true` whose reason starts
`compose_video: <CLASS>:`. The runner prints the same class as `COMPOSE-FAIL: <CLASS>: <detail>`
and writes it into its result file.

| class | cause |
|---|---|
| `BAD_INPUT` | the request broke a rule (inputs, enums, template name, variables), decided before any spawn |
| `LINT_ERRORS` / `CHECK_FAILED` | the composition failed its own gates |
| `RENDER_FAILED` | a failed row, or an output that failed the ffprobe gate |
| `BROWSER_MISSING` / `FFMPEG_MISSING` / `CLI_MISSING` | the pinned Chrome, ffmpeg/ffprobe or the pinned CLI is absent |
| `SPAWN_EBUSY` | an antivirus lock on ffmpeg ([hyperframes#4058](https://github.com/heygen-com/hyperframes/issues/4058)); retried once, then this class |
| `DISK_HEADROOM` | the frame-storage gate ([#4060](https://github.com/heygen-com/hyperframes/issues/4060)); move `compose_cache_dir` to a larger drive |
| `TIMEOUT` | `compose_timeout_sec` elapsed; the process tree is killed |

**Measured on the reference box** (36 threads, Windows, software GL confirmed on the running
Chrome's command line, quality `high`, 2026-09-22). Render time is HyperFrames' own `renderTimeMs`.
The whole call, with lint, check and the ffprobe gate, took 27-38 s.

| composition | frames | 1 worker | `auto` |
|---|---|---|---|
| `title-card` 1080p mp4 | 150 | 16.4 s | 15.3 s, 14.6 s |
| `lower-third` 1080p mp4 | 150 | 14.4 s | 17.5 s |
| `lower-third` 1080p webm with alpha | 150 | 22.0 s | 18.1 s |

An earlier session on the same box measured `title-card` at 21.2 s (1 worker) and 24-25 s (`auto`),
and `lower-third` webm at 30.4 s. Neither worker setting wins consistently on a 150-frame card, so
`compose_workers` defaults to HyperFrames' own sizing. Determinism held in both sessions:

- three `title-card` renders (1 worker, `auto`, `auto`) were byte-identical files with identical
  `framemd5` over all 150 frames;
- the `lower-third` mp4 renders were byte-identical;
- the two webm files differed in container bytes, but their decoded `yuva420p` frames were
  identical (`framemd5`).

### Security

- **Compositions are trusted code.** HyperFrames' Chrome launches with `--no-sandbox` and site
  isolation disabled. `html` and `project_dir` are accepted only from the local MCP and CLI doors, the
  same trusted-caller posture as `run-graph`. The fleet door is not token-gated
  ([ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)), so it refuses
  both at ack time and renders only the node's vetted templates. Template variables reach the page
  as text (`data-var-text`) and as sanitized CSS custom properties, never as markup.
- **No cloud path is wired.** The runner allows only `lint`, `check`, `render`, `snapshot`,
  `browser ensure|path` and `--version`. `init`, `skills`, `cloud`, `lambda`, `cloudrun`, `capture`,
  `upgrade` and `publish` are refused before any spawn, and so are `--gpu`, `--browser-gpu` and
  `--docker`. `snapshot` always carries `--describe false`, because describe calls Gemini
  ([ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md)).
- **Allowlisted environment, twice.** The CLI gets PATH, the Windows system variables, temp, home and
  the app-data dirs. The runner sets `HYPERFRAMES_NO_TELEMETRY=1`, `DO_NOT_TRACK=1`,
  `HYPERFRAMES_NO_UPDATE_CHECK=1`, `HYPERFRAMES_NO_AUTO_INSTALL=1`, `HYPERFRAMES_SKIP_SKILLS=1`,
  `PRODUCER_BROWSER_GPU_MODE=software` and the pinned ffmpeg, ffprobe, browser and cache paths.
  Nothing else passes: no `*_API_KEY` or token (`capture` would prefer `OPENROUTER_API_KEY`),
  `NODE_OPTIONS` or `GPU_LEASE_*`. The Go side applies the same allowlist to the runner itself
  (`gpugen.Spec.EnvExact`).
- **`--json` on every call.** It is the only switch that skips the CLI's npm-registry and GitHub
  update checks. `HYPERFRAMES_NO_UPDATE_CHECK=1` alone stops the self-install, not the pings.
- **No stray state.** The cwd is a fresh, empty work dir, so the CLI's `./.env` autoload finds
  nothing and an `ffmpeg.exe` planted in a cwd can never win the binary scan. HOME is
  `<hyperframes_dir>/home`, so HyperFrames' own state (`~/.hyperframes` and the managed Chrome cache)
  stays harness-owned and nothing it writes reaches the operator's `~/.claude`.
- **Pinned and verified.** The install is `npm ci --ignore-scripts` from the committed lockfile
  (`setup/hyperframes/`, `hyperframes` exact). `npm audit signatures` is fatal on failure.
  `npm rebuild esbuild` runs the one postinstall the CLI needs. `browser ensure` fetches the CLI's
  pinned chrome-headless-shell, and `hyperframes_browser_path` binds it explicitly, because the CLI's
  own lookup prefers a newer build in `~/.cache/puppeteer`. The installer never runs
  `npm install -g`: a global HyperFrames self-upgrades in a detached process. The runner refuses an
  install whose version is not its own pin (`PINNED_VERSION`, held equal to the lockfile by a test)
  with `CLI_MISSING`, because every guard here was read in the pinned source. `acceptance` runs
  that check as the node's identity.
- **Offline renders.** Vetted templates reference no URL, and every font family they use is declared
  with `@font-face` from the shared kit. An undeclared family makes the compiler request the Google
  Fonts CSS API, with the page's character set in the query.
