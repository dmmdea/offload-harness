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

**Vulkan device selection is auto, not pinned to 0 (found 2026-09-23).** `GGML_VK_VISIBLE_DEVICES`
used to default to `"0"` whenever unset, on the assumption that device 0 is the render GPU. On a
box with an enabled integrated GPU, ggml-Vulkan can enumerate the iGPU FIRST (measured: Vulkan0 =
Intel UHD 630, Vulkan1 = an RTX 5060) — every render then ran on the iGPU with no error, just
100×+ slower. `render/sdcpp-generate.mjs` now runs `<sdcpp_bin> --list-devices` and picks the first
discrete (NVIDIA/AMD) adapter when the env is unset, falling back to device 0 only when none is
found or the probe fails; an explicit `GGML_VK_VISIBLE_DEVICES` in the environment always wins and
is never second-guessed. `local-offload doctor` reports which device an sdcpp render will actually
use and prints a `WARN` line when that device is an integrated GPU
(`internal/mediacap/sdcppdevice.go`, mirroring the same resolution order in Go) — this is a live
subprocess probe, so it is invoked directly from `doctor`, not from `mediacap.Routes` (which stays
a pure config/filesystem derivation for `offload_status`/`acceptance`).

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
- How is a named opt-in family (Qwen-Image-2.1) selected, and what does its result carry?
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
**Zero-Warm** — no GPU residency persists between jobs, except the memory stack (the mem0 embedder and reranker stay resident, register C-87). **Warm Batch** — an opt-in session where the
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
  small (a few hundred MiB each) and, on the reference box, pinned to the utility card, not the render card. An earlier unload-all implementation tore that memory stack down
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

**A server that answers but cannot work ends the wait (register C-83, 0.158.1).** On 2026-10-01 a
sticky CUDA error on a 16 GB render card killed ComfyUI's prompt worker while its HTTP server kept
answering: `/history/<id>` returned `200 {}` for 48 minutes, which the loop read as a slow render,
and the media lease held all three cards with nothing running on any of them. The loop now does two
things about that:
- **The wait budget is wall-clock time.** `COMFY_WAIT_SEC` used to count polls, so slow `/history`
  answers stretched 1,500 s to 2,894 s. A suspend still moves the deadline by the time slept.
- **A health probe runs while the prompt is unlisted.** Once the prompt has been missing from
  `/history` for 15 s (or `/history` has answered with an error status instead),
  `GET /system_stats` runs every 15 s. It calls into CUDA, so a broken context
  answers it with an HTTP error, and two such answers in a row end the wait. Every runner submits
  only after `/system_stats` answered OK, so an error there is a change, not an endpoint that never
  worked. A probe that gets no answer at all counts for nothing, because silence is the dead-server
  watchdog's question.

Both that probe and the dead-server watchdog raise a *server unusable* error. `comfy-render.mjs`
exits **3** on it (1 for any other failure, 2 for a caller mistake). A `--no-lifecycle` child also
exits 3 when its parent's server answers `/system_stats` with an error three times while it waits
for it, and when the server never answers at all.

**Picking the produced file from `/history` never trusts node-id order alone (found 2026-09-23).**
`comfy-output.mjs`'s `firstOutputFile()` scans the `/history` outputs object for the first node
carrying a file descriptor; JS enumerates integer-like object keys in ASCENDING NUMERIC order
regardless of insertion order. The native `LoadVideo` node echoes a UI preview of its own input
into its own `outputs` entry — the same shape as a real result — and `wf-wan-animate2.mjs`'s
`LoadVideo` node ("240") sorts before its own `SaveVideo` node ("246"), so `animate_character`
always returned the raw driver video unmodified: full render time elapsed, exit 0, "WROTE `<out>`"
printed, and the delivered file byte-identical to the input. `firstOutputFile` now takes the
caller's own API-format graph as an optional second argument and skips any node whose `class_type`
starts with `Load` — a loader never legitimately produces the result. Every runner
(`comfy-animate.mjs`, `comfy-video.mjs`, `comfy-edit.mjs`, `comfy-inpaint.mjs`, `comfy-music.mjs`,
`comfy-upscale.mjs`) passes its graph. Audited every other `wf-*.mjs` builder: only WAN-Animate-2
uses a `LoadVideo` node; every other video lane's `LoadImage` does not register a preview entry in
ComfyUI's execution outputs. `allOutputsByNode` (the `run_graph` lane, which addresses a specific
node id from its own manifest rather than guessing) was not affected.

**Warm batch.** `generate-image --batch` takes a jobs file and runs N renders in one session. The
only behavioral change is omitting ComfyUI's `--cache-none`, so the checkpoint loads once; teardown
still happens exactly once, at the batch boundary. A failed render is recorded and the batch
continues, one JSONL result line per job, and the script exits 0 (the Go side reads per-job
status). The exception is a server that became unusable, where the child exits 3. Then the failed
job and every later job get a row, the later ones with an `error` that starts `not run: ComfyUI
became unusable at job N/M`. The batch exits non-zero and its teardown frees the card and the
lease, instead of failing every remaining job against the same server (C-83: 3 min each, after a
48-minute wait on the first). A failed job's `error` carries the child's own `RENDER FAILED:` reason
rather than only `comfy-render exited N`. The inpaint batch (`comfy-inpaint.mjs --batch`) stops the
same way on an unusable server, and still stops after `COMFY_BATCH_MAX_CONSEC_FAIL` (3) consecutive
failures; its `_row: "aborted"` line now carries `reason`. **The default single-render path is
unchanged.**

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

`crop`'s (and `composite`'s/`text`'s) `x`/`y` are anchored coordinates, and `0` is a legitimate,
common value — a crop or paste at the image origin. `EditOp.X`/`Y` (`internal/mediaops/editimage.go`)
carry no `omitempty` for exactly that reason: an `int` field with `omitempty` drops an explicit zero
from the JSON entirely, and `render/edit_image.py`'s worker then saw a missing `"x"` key and raised
`pipeline failed: 'x'` — every crop/composite/text anchored at `0,0` failed outright (found 2026-09-23).
The Python side also defaults safely (`op.get("x") or 0`) as defense in depth. `Width`/`Height` keep
`omitempty`: `0` is never a valid dimension (`ValidateOps` rejects it), so there is no zero-vs-absent
ambiguity to protect against there.

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
`gen_edit_preset` `lightning8`. `blackwell-8` seeds `qwen_image_edit_2511_fp8mixed.safetensors` for
`gen_edit_unet` instead (register A-120): it passed on the 8 GB reference box but was not timed there, and the builder
picks its loader by file extension, so the one key is the whole switch. A box without that seed defers
until it binds `gen_edit_script` and `gen_edit_unet`. `gen_edit_preset` pairs steps+cfg+LoRA as a matched
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

CLI verbs `generate-image` (`--family`, `--transparent`), `inpaint-image`, `generate-video`
(every `offload_generate_video` option, `--fast` included),
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
2. **The memory stack is never unloaded** by the free step or by `gpu reserve --unload-seat` (register C-87, 2026-10-01). It is an exception to item 1: mem0 never yields to a lease, and the stack is small and, on the reference box, off the render card.
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
| Video | `videogen_family` (`""`/`wan22` = Wan 2.2; `ltx25` = LTX-2.5 joint-AV), `videogen_unet_high`, `videogen_unet_low`, `videogen_text_encoder`, `videogen_upscale_model`, `videogen_wan_virtual_vram_gb` (Wan keys); `videogen_transformer`, `videogen_video_vae`, `videogen_audio_vae`, `videogen_latent_upscaler`, `videogen_fps`, `videogen_pool_vvram_gb/pool_compute/pool_donor` (LTX-2.5 keys) |
| Audio | `voicegen_*`, `musicgen_script`; `tts_endpoint` / `tts_model` (default `tts-1`) / `tts_voice` / `tts_api_key` (0.113.25: an OpenAI-compatible speech SERVER for `generate_audio kind=voice` — `voice: endpoint`, or the default on a box with no `voicegen_script`; `internal/ttsclient` POSTs `/v1/audio/speech`, writes the WAV atomically, defers naming the server's words on any non-audio answer, takes no media lease because the server owns its GPU; e.g. VoiceStudio on `http://127.0.0.1:3900`) |
| Qwen-Image-2.1 (family `qwen-image-2.1`) | `imagegen_ckpt` + `imagegen_clip` + `imagegen_vae` (all three REQUIRED — the builder has no defaults), `imagegen_schedule` (`official` default / `comfy`), `imagegen_steps/cfg` (both or neither; official 40 / 1.0); edit: `gen_edit_family: "qwen-image-2.1"`, `gen_edit_unet/clip/vae`, `gen_edit_resolution` (0 = 1024), `gen_edit_cache_device` (`auto`/`gpu`/`cpu`/`off`) |
| Named families + license (ADR 0058) | `imagegen_families`, `gen_edit_families` (name → overlay + `license` + `commercial_use`); `imagegen_license`/`imagegen_commercial_use`, `gen_edit_license`/`gen_edit_commercial_use` (the default binding's own tag, both or neither) |
| ComfyUI | `comfy_dir`, per-task `*_script` and `*_timeout_sec`; launch profile `comfy_cuda_device`, `comfy_dynamic_vram` (`on`/`off`/`""`), `comfy_extra_args` |
| Composition (HyperFrames) | `compose_script` (`render/compose-hyperframes.mjs`), `hyperframes_dir` (the pinned install), `hyperframes_browser_path` (the pinned chrome-headless-shell), `compose_timeout_sec` (1800), `compose_workers` (`""`/`auto` or 1-24), `compose_cache_dir` (`""` = `<media_dir>/.compose-cache`), `compose_quality` (`high`); `ffmpeg_path` (ffprobe beside it). All three route keys empty = NOT CONFIGURED; the installer binds them |

Hardware profiles seed these. The single-card 16 GB tiers (`blackwell-16`, `ampere-16`, `volta-16`)
and the 8 GB tiers' RAM layer bind **HiDream-O1** via `imagegen_family` — the official graph for that
DiT, never the generic SDXL graph; the pooled and 32 GB-class Blackwell tiers (`blackwell-2x16`,
`-3x16`, `-32`, `-48`, `-72`) bind **Krea 2 Turbo** (below). No tier makes a named family its default; `blackwell-8`'s RAM
layer seeds `qwen-image-2.1` as an opt-in family, for image generation and for the instruction edit.
The Wan 2.2 video tiers bind **Wan 2.2 Q8_0** experts with an fp16 text encoder, except `blackwell-8`: its RAM layer
binds the fp8_scaled pair on `videogen_wan_loader: "native"` and leaves the text encoder to the builder default
(register A-120: 2,029 s against 2,939 s for the Q8_0 pair at 832x480x81 on the reference box, same still, prompt and
seed). **RealVisXL** is the SDXL-class inpainting default. The 8 GB
tiers stay SDXL-class for image generation until O1 on 8 GB is verified on real hardware.

**The Wan DisTorch2 split is per card** (`videogen_wan_virtual_vram_gb`, default 7 = the builder's
value). Both Wan experts load through DisTorch2 with `virtual_vram_gb` GiB parked in system RAM; the card
holds the rest. That is the GGUF path: `videogen_wan_loader` (empty or `auto` by default) picks the loader per
expert by file extension, a `.safetensors` expert goes through the core `UNETLoader` with no split at all (the
value is inert there), and `native` forces that loader and refuses a `.gguf` expert by name, so a tier that
seeds `native` (`blackwell-8`) goes back to GGUF experts by changing three keys, both experts and the loader.
It was a constant 7 in `render/wf-wan22-i2v.mjs`, and on an 8 GB card under the driver's
"Prefer No Sysmem Fallback" policy 7 asks ~8.4 GB of a 15.4 GB Q8_0 expert and OOMs, while 11 got through
the load and hung the card (<node-e> reg3b/reg3c, 2026-09-22). Each node sets the value it MEASURED; the
pipeline passes it as `--wan-vvram-gb`. It is not the LTX-2.5 pool key: that one borrows VRAM from a
donor card, this one parks weights in RAM. The runner also asks the running ComfyUI for every node class
the graph names before submitting (`render/comfy-nodes.mjs`), so a missing pack is a one-line
`MISSING_NODE` defer naming the class and pack, with nothing POSTed.

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
seat. **Text encoder / both VAEs (fixed 2026-09-24, same defect and fix as krea2 above):**
when pooled they load through `CLIPLoaderMultiGPU`/`VAELoaderMultiGPU` pinned to
`videogen_pool_donor`, never the stock nodes' `"default"` device. Full-resolution 1920×1088 remains available as a per-deployment STREAMING alternative
(pool keys unset, dynamic VRAM on — measured ~210 s and 1.65–2.3× faster for bf16 graphs).
The builder deliberately drops the template's gemma4_e2b prompt-enhancer branch (the
harness's own planner does prompt expansion) and pairs the convrot transformer with the
**conv** video VAE. Wan 2.2 stays available per-request via `model: "wan"`.

**ComfyUI-MultiGPU archival (2026-09-30) — pin + mirror.** Upstream `pollockjj/ComfyUI-MultiGPU`
archives 2026-09-30 (issue #223: final code state v2.6.4, no successor endorsed; full research
in `Ecosystem/Benchmarks and Optimizations/2026-09-22-qwen-image-21-hyperframes/research/comfyui-multigpu-archival-2026-09-24.md`).
Neither native Dynamic VRAM nor `SelectModelDevice`/"MultiGPU Work Units" reproduce this pack's
donor+compute `virtual_vram_gb` weight-sharding (needed by the Wan GGUF lane and the pooled
krea2/LTX-2.5 seats), so the harness stays on MultiGPU for those and pins rather than migrates
blind. **Pinned commit: `ed1ffaef7cec1a66f35106c6a4c7a40927c2dc83`** — upstream v2.6.4's last
code commit (`b51c99a525e9607e43545ee2a8b7694c74a4775a`) plus one already-deployed local fix
(`fix(p2p): platform-aware cudart load + fail-closed P2P on Windows/WDDM`, operator, 2026-09-01 —
the first-stage fix for upstream issue #220's `libcudart.so`-on-Windows crash; the second-stage
`illegal memory access` in the int8 dispatch path itself, hit only when `compute_device` is a
*non-default* CUDA device, remains open and unfixable upstream — this is why the pooled LTX-2.5
seat keeps `compute cuda:0`). **Fallback source once upstream goes read-only:** private mirror
`https://github.com/dmmdea/ComfyUI-MultiGPU-mirror` (full `git clone --mirror`, all branches/tags,
plus this pinned commit). `render/comfy-nodes.mjs` (`NODE_PACKS`) and
`internal/mediacap/routeneeds.go` (`nodePacks`/`packHint`) both carry the pin+mirror note in the
operator-facing `MISSING_NODE` hint, so a box missing the pack is told where to get the frozen,
patched code, not just the (soon read-only) upstream URL. All four fleet Windows/Linux ComfyUI
installs (<node-b>, <node-e>, <node-a>, <node-c>) are aligned to this commit (2026-09-24) — see the
per-node rev table and the A/B seat-switch decision in
`Ecosystem/Benchmarks and Optimizations/2026-09-22-qwen-image-21-hyperframes/infra/multigpu-archival-actions-2026-09-24.md`
(operator's Drive, not this repo). Re-check the archive-coordination thread (#223)
and `city96/ComfyUI-GGUF#427` (GGUF+DynamicVRAM, open/stalled) periodically before investing
further in this pack; if a fork gains independent commits and traction, re-evaluate.

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
never by shared code. **Text encoder / VAE device (fixed 2026-09-24):** the stock
`CLIPLoader`/`VAELoader` nodes' `device` input only ever offers `"default"`/`"cpu"`, and
`"default"` is ComfyUI's own fastest-first device pick, entirely independent of
`imagegen_pool_compute`/`imagegen_pool_donor` — on the 3x16 reference tier that IS the
display card (measured A/B, `multigpu-archival-actions-2026-09-24.md`: the ~12 GiB
Qwen3-VL-4B encoder loaded there on every pooled render regardless of which two cards the
DiT's own pool keys named). When pooled, `wf-krea2.mjs` now routes both through
ComfyUI-MultiGPU's `CLIPLoaderMultiGPU`/`VAELoaderMultiGPU`, pinned to
`imagegen_pool_donor` — never the unnamed default. Unpooled builds are unaffected (the
plain nodes, `device: "default"`, are correct there: `comfy_cuda_device` already pins the
whole process to one card). **Windows multi-GPU visibility (ComfyUI >= 0.34):** upstream hides every CUDA device but the first on Windows unless devices are explicitly selected; `ensureComfy` restores full visibility for the spawned child via `cudaVisibleEnv()` (env-based; multi-GPU spawns also get `--disable-pinned-memory` per upstream guidance) whenever the operator has not already scoped devices — without this, every pooled graph fails prompt validation with `donor_device: 'cuda:1' not in ['cpu', 'cuda:0']`. Zero/empty pool keys render single-GPU (the small-fleet shape).

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
keeps the route keys (script, engine, timeout, reserve; the timeout is the node's own `imagegen_timeout_sec` or `gen_edit_timeout_sec`, so a family that needs longer sets its own) and the launch keys, then applies the
overlay. So a family never inherits the default's LoRA or pool by accident. The config load refuses:
an overlay key outside those prefixes or not a config key (typo), a forbidden key (`*_families`,
`*_license`, the prompt refiner), a missing/empty `license`, a missing `commercial_use`, a family name
outside `[a-z0-9._-]`, or a name that collides with the default binding's own family. Every family is
also checked by the same binding-trap warnings as the default, with its name in front.

**Video families carry the pair too (CT-47).** A `videogen_families[name]` entry may declare `license`
and `commercial_use`, both or neither (the load refuses one without the other, naming the family). It is
the video analogue of `imagegen_license` / `imagegen_commercial_use`, with two differences: nothing
requires it, because video family selection was never a licensing gate (every family stays reachable
through `model`), and it describes the MODEL, so `ResolveVideoFamilyBinding` reads it from the family's
own entry even for this box's default family, whose weights still come from the flat `videogen_*` keys.
A `generate_video` result then carries `license` and `commercial_use`, tagged with the family that
rendered (an override request gets the overridden family's pair, not the seat's), its ledger row carries
`license`, `offload_status` lists both under each `media.video_family_bindings` row (null when
undeclared), and `local-offload doctor` prints a `license:` line under a family that declares one. No
warning text rides along (operator order 2026-10-01). A family that declares none adds nothing and
reads as UNKNOWN. No value ships as a default: the repo's only sourced statements of the video
licences are the names and conditions in `render/templates-license-map.json` (read from the hosting
cards and digests, not from the licence texts in full), and its `conditional` class is not a bool.

**Every result is tagged.** Results carry `family` (the default binding's own name for an unnamed
request), and `license` + `commercial_use` whenever the binding declares them (no warning sentence
rides along: `license_note` was removed by operator order on 2026-10-01). The ledger row carries `license` (`ledger.Entry.License`; absent = UNKNOWN, never
"safe"). `offload_status` lists `media.image_families` / `media.edit_families` — name, graph family,
engine, checkpoint, license, commercial_use (null = undeclared) and the route verdict — and
`/fleet/health` publishes `image_families` with the same license flags — including on a node with NO
default image binding at all, opt-in-family-only (e.g. an sdcpp tier that only serves
`qwen-image-2.1`): `image-gen` is advertised and admitted whenever the default binding OR any named
family is configured (`config.ImageGenAdvertisable`), and `image_families`/`loadable_model_families`
list only the families that are genuinely bound — never a phantom default row or an invented `sdxl`
label for a route this node does not serve. A dispatch naming no `family` on such a node still gets
the plain "no image-gen route configured" defer (same as any unconfigured node), never a silent
render of one family as if it were the default. A warm batch
(`generate-image --batch`) always renders the default binding, so every batch item — and the batch
payload's top level — carries that binding's `family` and, when declared, its `license`
and `commercial_use`. `width`/`height` in a
`generate_image` or `edit_image_generative` result are **measured** from the written file
(`imagegen.OutputSize`), not echoed from the request.

**Unknown family, unsupported flag.** An unknown `family` defers with the list this node serves.
`transparent` is honoured only by the `qwen-image-2.1` family (the only one with an RGBA VAE) —
on EITHER engine: the ComfyUI graph and sd.cpp's own build of the model carry the identical VAE, so
`SupportsTransparentImage` follows the family, not the engine. On sdcpp, `transparent` has no CLI
flag of its own — the runner (`render/sdcpp-generate.mjs`) wraps the prompt in the official RGBA
template instead (the same constant the ComfyUI builder uses) and leaves the written PNG's alpha
channel untouched; the DEFAULT (no `transparent`) instead flattens the output to opaque RGB after
sd-cli writes it, because sd.cpp's qwen-image-2.1 VAE emits RGBA unconditionally regardless of the
prompt (`render/png-alpha.mjs`, a dependency-free channel-drop — never a composite onto a
background, matching `SplitImageWithAlpha`'s own behavior). Any other binding defers rather than
render opaque. `images` (multi-reference) needs a 2.1 edit family and is
capped at 10 images in all, target included; a 2511 `preset` on a 2.1 edit defers by name. The render
runner closes the same gap one layer down: `comfy-render.mjs --family` is a closed set, and an
unknown value exits 2 before any GPU work — it used to render the generic SDXL graph silently.
`generate-image --batch` renders the default binding only; `--family` with `--batch` is refused.

**`doctor` sees the whole family.** `media.routes` gains `generate_image:<name>` and
`edit_image_generative:<name>`: CONFIGURED only when the family's script resolves AND every model file
its graph opens sits in the class directory the loader reads (resolved under the family's own
`comfy_dir`); the verdict names the family's license (`license <name>`). The `comfyui
model bindings` section also resolves the files a binding's graph loads WITHOUT a key naming them — a
preset's Lightning LoRA (`qwen-image` `lightning4`, 2511 `lightning8`/`lightning4`) and a builder's
default text encoder and VAE — so an edit route bound to preset `lightning8` with its LoRA on no models
root is a `MISSING` row instead of a green doctor and a failed render.

### Qwen-Image-2.1 (`render/wf-qwen-image-21.mjs`)

A 7B single-stream DiT with a Qwen3-VL-8B encoder and a new 4-channel (RGBA) VAE — not a variant of
Qwen-Image 2512, whose graph cannot drive it. Needs **ComfyUI ≥ v0.37.0** (the nodes arrived in PR
#16400; master ≥ `95539f56` adds the KV-cache placement fix #16429). Bind it as a named family
(overlay example: `docs/OPERATOR-GUIDE.md`). The download set is in `setup/SETUP-AGENT.md`.

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
  partially-transparent PNG to a compositor. The same contract holds on the sdcpp engine
  (`render/sdcpp-generate.mjs` reuses `rgbaPrompt`/`RGBA_PROMPT_PREFIX`/`RGBA_PROMPT_SUFFIX` from
  this file rather than duplicating the template): sd.cpp's qwen-image-2.1 VAE always emits RGBA,
  so the runner flattens the default case to opaque RGB itself (`render/png-alpha.mjs`) instead of
  refusing transparency or shipping a partially-transparent PNG as the "opaque" result.
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

### ACE-Step 1.5 music (`render/wf-acestep.mjs`, `render/comfy-music.mjs`, `render/audio-qa.mjs`)

`offload_generate_audio kind=music` / CLI `generate-audio` renders through the ACE-Step v1.5
**split** stack (UNETLoader DiT + `DualCLIPLoader(type "ace")` qwen 0.6b/4b encoders + VAELoader —
NOT the retired v1 all-in-one checkpoint): `TextEncodeAceStepAudio1.5` (tags/lyrics/bpm/duration/
keyscale + the `generate_audio_codes` LLM planner) → `ConditioningZeroOut` (negative) →
`EmptyAceStep1.5LatentAudio` → `ModelSamplingAuraFlow` → `KSampler` → `VAEDecodeAudio` →
`SaveAudio` (FLAC). Every default (cfg 1.0, steps 8, shift 3.0, `generate_audio_codes` true,
`cfg_scale` 2.0, temperature 0.85, top_p 0.9) is verified against the official
`audio_ace_step1_5_xl_turbo` Comfy-Org template (`comfyui_workflow_templates` package) field for
field — the harness does not diverge from the template on any shared parameter.

**Known defect (F-35 regression follow-up, root-caused 2026-09-23): short INSTRUMENTAL renders
reliably go dead partway through.** Measured on 4 independent 15s renders under the harness's own
default instrumental path (empty lyrics, `generate_audio_codes: true`) — the two original defect
clips plus two fresh reproductions, one with plain empty lyrics and one with an `"[instrumental]"`
lyric tag (which does NOT fix it) — every one has real content for roughly the first 9-11s then
drops to a clean, highly periodic near-silent tail for the remainder (measured amplitude ≈
-60 to -70 dBFS, no NaN/Inf — not garbage, a genuinely quiet signal). Root cause is upstream, in
`comfy/ldm/ace/ace_step15.py`'s `AceStepConditionGenerationModel.prepare_condition`: instrumental
content is driven entirely by `lm_hints` derived from the `generate_audio_codes` LLM's
autoregressive output (`comfy/text_encoders/ace15.py`'s `sample_manual_loop_no_classes`); with no
lyrics to plan a song structure against, the planner's own output evidently degrades to a
near-constant low-energy code for the tail well before the requested duration. Turning
`generate_audio_codes` **off** is not a workaround either — by source-code trace (not independently
re-measured live: the confirmation render hit unrelated GPU-lease contention from a concurrent
session and was not retried) `is_covers` then falls back to the pure silence-latent reference
(`get_silence_latent`, `comfy/model_base.py`'s `ACEStep15.extra_conds`) for the WHOLE clip — i.e.
disabling the planner does not recover real content, it trades a partial dead tail for total
silence. Separately, true peak was measured at a literal 0.0 dBFS (clipping-level) on two of the
three renders with no loudness normalization anywhere in the pipeline — an independent defect from
the dead air.

**Root cause, refined 2026-09-23 (0.140.3):** `comfy/text_encoders/ace15.py`'s ACE-Step LM always
emits exactly `duration*5` audio codes (min=max) and reliably plans the song to end 2-6s BEFORE the
requested duration, filling the remainder with silence codes — a hard cut, not a fade, matching the
`-60` to `-70` dBFS tail measured above. Per-second RMS on 30s requests (same prompt): seed 7 ->
real music to 27.99s then 2.01s silence; seed 759155896809805 -> music to 24.89s then 5.11s silence
(17% of the clip). Asking for MORE than the target duration avoids the defect: at 36s requested, all
three seeds tried put music across the whole requested span (seed 7 to 31.11s; seed
759155896809805 to 30.24s; seed 11 to ~34s then a natural fade). **So a parameter change DOES fix
this** — over-rendering, not a graph shape change, which is why the earlier "no parameter/graph
change fixes this" conclusion held only while duration itself was held fixed.

**The fix has two layers: over-render + trim (0.140.3), then the 0.139.3 post-render QA gate
(`render/audio-qa.mjs`) as defense-in-depth**, both run from `comfy-music.mjs`'s `generate()`:
0. **Over-render + trim.** When the graph is built from args (not a verbatim `--graph` passthrough)
   and ffmpeg/ffprobe resolve, `generate()` asks `buildAceStep` for
   `renderSeconds = seconds + max(6, ceil(0.2*seconds))` (`computeRenderSeconds`) instead of the
   requested seconds — both `TextEncodeAceStepAudio1.5`'s `duration` and
   `EmptyAceStep1.5LatentAudio`'s `seconds` get the over-length value, so the LM plans against a
   longer song. The produced file is then trimmed back to exactly the requested seconds with a 1.0s
   fade-out on the cut (`audio-qa.mjs`'s `trimToSeconds`, same FLAC container) BEFORE step 1 below
   ever measures it. ffmpeg unavailable, or a caller-supplied `--graph` (its duration is opaque to
   `buildGraphFromArgs`), renders the requested seconds exactly, with no trim — same as before
   0.140.3.
1. Measure trailing/leading silence (`ffmpeg silencedetect`, -45 dB / 0.5 s) and true peak +
   integrated loudness (`ffmpeg ebur128=peak=true`) in one combined ffmpeg pass, plus duration via
   ffprobe. `assessDeadAir()` flags dead air when trailing OR leading silence exceeds 1.0 s, or more
   than 10% of the clip is silent.
2. On a flagged render: re-render exactly ONCE with a freshly minted seed (`rewireSeed()` rewrites
   every node's `inputs.seed`, id-agnostic so it also works on a caller-supplied `--graph`), over-
   rendering and trimming the same way as step 0 — the LM planner's output does vary by seed (the
   two reproduction renders' silence onset differed by ~0.5s despite fixed inputs), so a retry
   sometimes lands on a seed whose planned content happens to fill the duration.
3. Still dead air after the retry: `generate()` first removes the stray output file (best-effort,
   `cleanupFailedDeadAirOutput()`) — `renderOnce` always writes ComfyUI's raw `SaveAudio` bytes
   (unconditionally FLAC) straight to `out`, whatever extension the caller requested; step 0's
   `applyTrim` re-mixes it to match that extension when it runs and succeeds, but not on a `--graph`
   passthrough (`renderSeconds` never set) and not when its own ffmpeg call fails (logged, never
   fatal). Left in place, that stray file is either genuinely mismatched-container bytes ("FLAC bytes
   in a .wav name", found 2026-09-23) or a correctly-muxed file that still failed content QA —
   neither belongs at the caller's path, so cleanup removes it unconditionally rather than only in the
   narrower mismatched-container case. The render then fails with an error tagged `DEAD_AIR:` (mapped
   to `gpugen.ClassifyErr`'s `dead_air` class), which `runGenerateAudio`
   (`internal/pipeline/pipeline.go`) turns into a typed defer — never a silently-shipped dead clip,
   and never a leftover file at the requested path.
4. The accepted render is always loudness-normalized (`ffmpeg loudnorm`, single-pass, target -14
   LUFS integrated / -1 dBTP true peak — the common streaming convention, with headroom below 0
   dBFS so a downstream lossy re-encode's peak overshoot cannot clip), independent of the dead-air
   verdict.

ffmpeg/ffprobe are resolved via `$FFMPEG_PATH` (threaded from `Pipeline.genEnv()` — the same
per-machine `ffmpeg_path` config `internal/audioio` already uses for transcribe), which resolves a
bare name (the shipped `ffmpeg_path` default) through PATH via `internal/mediaops.ResolveBinary`
before threading it, then a PATH probe fallback on the JS side. **Either missing is now a fatal,
typed `FFMPEG_UNAVAILABLE` error raised by `main()` before the GPU lock or ComfyUI are ever touched
(F-38, 2026-09-24), not a silent skip** — the pre-fix behavior (degrade to a no-op skip and ship the
raw, unverified render) let the gate go silently inert fleet-wide on every node that never set an
explicit `ffmpeg_path`, since the config default `"ffmpeg"` is a bare name that `existsSync()`
always read as missing (reproduced identically on <node-c> and <node-a>). `gpugen.ClassifyErr`
maps `FFMPEG_UNAVAILABLE` to the `ffmpeg_unavailable` class, mirroring `dead_air` below.

**A `gpugen`-killed timeout is a typed `timeout`, not a generic failure (found 2026-09-23).**
`gpugen.Generate` tree-kills the child on its context timeout (`killTree`); on Windows that is
`taskkill /T /F`, which terminates through `TerminateProcess` reporting exit code 1 — `cmd.Wait()`
then returned a plain `"exit status 1"` containing none of `ClassifyErr`'s recognized substrings
(`timeout`/`deadline`/`killed`/`signal:`), so a cold ACE-Step retry killed at `audiogen_timeout_sec`
surfaced as an indistinguishable generic failure instead of a timeout. `Generate` now checks its
OWN derived context's `DeadlineExceeded` directly — authoritative regardless of what the OS reports
for the child's exit code — and folds "timeout"/"deadline exceeded" into the returned error text, so
`ClassifyErr(gerr) == "timeout"` is reliable for every `gpugen`-based lane (image, video, audio), not
just the one that happened to reproduce it.

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

### Per-card ComfyUI instances (plan P13a, render layer)

One ComfyUI per card lets two media jobs run on two cards at once instead of queueing on one. The
render layer now knows how to run, find, reuse and stop such an instance. **Nothing sets it yet:**
the lease admission that picks a card, the per-card in-process slot and the waiting-place token are
the next phase (P13b), so a box behaves exactly as before until a caller exports the instance env.

**The default instance is unchanged.** With no instance env the key is empty and everything is as it
was: one ComfyUI on its default port, the argv `launchFlags` always built, the `.offload-launch.json`
marker with the same four fields, `offload-comfyui.log`, the same spawn env, no bind check. A test
pins each of those against literals taken from the code before the change.

**What names an instance.** Only an explicit `COMFY_INSTANCE` key (letters, digits, `-`, `_`, up to
48), or the card the instance is bound to (`COMFY_CARD_UUID`, key derived from the head of the
uuid). A port on its own (`--api` / `COMFY_API`) does not make an instance: it keeps its old
behaviour. A keyed instance owns:

| thing | keyed instance | default instance |
|---|---|---|
| endpoint | `--api` / `COMFY_API`, else `COMFY_PORT_BASE` (default the first port after the default one) plus `COMFY_INSTANCE_INDEX`; never the default port | the default port |
| launch flags | `--port`, `--output-directory <comfy dir>/instances/<key>/output`, `--temp-directory <comfy dir>/instances/<key>`, before the extra args | none of these |
| launch marker | `.offload-launch-<key>.json`: pid, the spawning process, argv, profile, key, port and the lease epoch (`GPU_LEASE_EPOCH`) when launched under a lease | `.offload-launch.json`, unchanged |
| console log | `offload-comfyui-<key>.log`, rotated on its own | `offload-comfyui.log` |
| input directory | shared; runners stage under names unique across processes (clock, process id, random suffix, sequence number: `render/comfy-input.mjs`) | shared |

A `--port`, `--output-directory` or `--temp-directory` in `COMFY_EXTRA_ARGS` is dropped (with a
`COMFY-INSTANCE-WARN:` line) for a keyed instance, because the last occurrence wins and would put it
back on a shared port or directory.

**Why one folder per instance, outside `temp` and `output`.** ComfyUI appends `/temp` to the value of
`--temp-directory` itself, so the temp directory a keyed instance really uses is
`<comfy dir>/instances/<key>/temp`. The default instance deletes `<comfy dir>/temp` on every start
(assets disabled, the harness default), and the harness launches the default instance per job, so a
keyed temp directory nested under `<comfy dir>/temp` would be wiped whenever the default instance
started. A test pins that no keyed directory is inside the default instance's `temp` or `output`
tree, and that the directories the launcher creates are the ones ComfyUI really uses.

**Pin by uuid, never by index.** A card-bound instance gets `CUDA_VISIBLE_DEVICES=<GPU uuid>` in its
child env and **no** `--cuda-device` flag (and a `--cuda-device` / `--default-device` in the extra
args is dropped). `--cuda-device N` counts in CUDA's default fastest-first order, which is not the
driver's: on the three-card tier index 0 is the display card, so any index taken from the driver or
from a lease lands on the wrong card. The uuid names one card in every ordering; measured on that
tier (2026-10-02), the process sees exactly one device, of that uuid's model. A device index
(`COMFY_CUDA_DEVICE`) together with an instance key, whether the key is an explicit `COMFY_INSTANCE`
or a card in `COMFY_CARD_UUID`, is refused (`COMFY-INSTANCE-CONFLICT`): the render layer never takes
an index for a keyed instance. The legacy `comfy_cuda_device` path is untouched for the default
instance. The Go side blanks the index whenever a card is named (`imagegen.ComfyLaunch.Env`,
`gpugen.instanceEnv`).

**An explicit key with no card is not pinned.** `COMFY_INSTANCE` on its own gives an instance its
own port, directories, marker and log, but nothing binds it to a card: it sees every card its
environment shows, the display card included, and the launcher logs a `COMFY-INSTANCE-WARN:` line
saying so. That is the operator's own choice (a side-by-side instance, a test); an instance meant to
run on a card is created by naming the card (`COMFY_CARD_UUID`). A `CUDA_VISIBLE_DEVICES` in the
environment, or a `--cuda-device` the operator wrote in `COMFY_EXTRA_ARGS`, counts as the operator's
own pin and is passed through without the warning (a card-bound instance drops the latter).

**Reuse needs proof of ownership.** A ComfyUI that already answers on a keyed instance's port is
reused only when it is shown to be that instance, because whatever answers could be another key's
instance, another launcher's ComfyUI or a foreign one, none of which uses this instance's
directories, marker or log. The proof is the keyed launch marker: this key and this port, a live
pid, the exact recorded argv as `GET /system_stats` reports it, and that argv carrying the instance's
own `--port` and `--output-directory`. It is required whether or not a card pin was asked for, so two
explicit keys that resolve to the same port do not share one ComfyUI. Anything short of it is refused
(`COMFY-PROFILE-MISMATCH`) and never stopped.

**Reuse by uuid.** `GET /system_stats` lists device names, not uuids, and two cards of one model are
indistinguishable, so a card pin cannot be read off a running process. `reuseVerdict` proves it from
the keyed launch marker as above and, in addition, that the marker records this uuid. A ComfyUI the
harness did not launch (no marker proving the pid and the exact argv) is refused and never stopped,
whatever its argv. One the marker proves is the harness's own, but that was launched on another card,
without a pin, or with an argv carrying `--cuda-device`, is stopped and relaunched on the right card
when its spawner is gone, and refused while another process still holds it.

**Bind check.** Before launching a keyed instance whose endpoint did not answer as ComfyUI, the
runner tries to bind the port on every address ComfyUI will listen on: `127.0.0.1` unless its
`--listen` flag (in `COMFY_EXTRA_ARGS`) says otherwise, each address of a comma list, or every
interface for a bare `--listen`. It does not probe the host the runner spells in its api, because
that is where a client reaches ComfyUI, not where ComfyUI binds (`localhost` can resolve to `::1`
first and miss a holder on `127.0.0.1`). If something holds the port the launch fails with
`COMFY-PORT-TAKEN`; the holder is never signalled or killed. The default instance does not bind-check
(that would change its failure mode).

**Ports and other launchers.** A keyed instance's port comes from `--api` / `COMFY_API`, else
`COMFY_PORT_BASE` (default 8189) plus `COMFY_INSTANCE_INDEX`; the index is assigned by whatever
launches the instance (the admission phase, P13b), not derived from a card's ordinal, and
`COMFY_PORT_BASE` moves the whole range. The harness reserves 8189 to 8192 on a host for this, so a
hand-started set of per-card ComfyUI processes on the same ports (an older stopgap launcher that
numbers them by card ordinal and uses its own output directories) is never reused or stopped: it
fails the ownership proof above and is refused with `COMFY-PROFILE-MISMATCH`. The two launchers must
not run in one lease window until that stopgap is retired. The host's record of the ports it listens
on must name the harness as the owner of that range in the change that first sets the instance env.

**Runners.** Every render runner (`comfy-generate`, `-render`, `-video`, `-edit`, `-inpaint`,
`-animate`, `-upscale`, `-music`, `-run-graph`) resolves its endpoint through `comfyApi(flags.api)` and
hands it to `withGpuSlot`. For a **keyed** instance `withGpuSlot` launches, frees (`POST /free`) and
tails the log of that instance (before this, the launch and the post-run free ignored `--api` and used
the environment's endpoint). For an **unkeyed** instance nothing changes about the launch:
`ensureComfy` is still called without an api (an unkeyed launch has no `--port`, so it can only start
the default endpoint), and the post-run `/free` now goes to the endpoint the runner talked to. With no
`--api` that is the same endpoint as before, so the calls are the old ones in value; with `--api`
naming another port on an unkeyed instance, the launch still uses the environment's endpoint while the
free follows `--api`. `run-graph` starts and frees its own ComfyUI through the same rule
(`instanceDeps`), and an env that cannot name an instance is a typed `RUN_ERROR` defer in its result
file (exit 0), like every other `run-graph` failure. The Go side carries the optional endpoint and
card in `imagegen.ComfyLaunch{API, CardUUID}` and `gpugen.Spec{ComfyAPI, CardUUID}`; the post-run
`/free` goes to the instance that ran.

**Verifying a pin on Windows.** The driver reports no per-process rows for graphics-mode (WDDM)
cards, so confirm that an instance is on its card by per-card memory deltas, not process ids.

| line | meaning |
|---|---|
| `COMFY-INSTANCE-INVALID: …` | a key, uuid, port base or index that cannot name an instance, or a keyed instance on the default port |
| `COMFY-INSTANCE-CONFLICT: …` | an instance key (explicit, or a card uuid) and a device index both given |
| `COMFY-INSTANCE-WARN: …` | extra args that would override the instance's pin, port or directories were dropped, or an instance with no card pin was launched |
| `COMFY-PORT-TAKEN: …` | the instance's port is held, on an address ComfyUI will listen on, by something that is not ComfyUI; nothing was launched or killed |
| `COMFY-PROFILE-MISMATCH: …` | a ComfyUI answers on the instance's port but is not shown to be that instance (or is on the wrong card); refused, and stopped only when the harness's own marker proves it is the harness's and its spawner is gone |

## Error handling

Failures return typed Defers rather than crashing: a busy GPU lock defers with a distinct reason, a
render error defers with detail. The batch path records per-job failures and continues, except when ComfyUI became unusable: the batch then stops at that job and exits non-zero (register C-83, 0.158.1; see the Warm batch paragraph above).

## Security and privacy notes

Generation runs local. `run-graph` executes caller-supplied graphs and provisions caller-specified
node packs, which is a trusted-caller interface by design — see
[ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md) for what
protects the environment from it. `compose_video`'s `html` and `project_dir` inputs are the same
kind of trusted-caller interface: the page runs in a Chrome without a sandbox. The fleet door
therefore accepts vetted templates only. See [Security](#security) below.

**Licenses.** A family that declares a license tags its output (`license`, `commercial_use`) and
its ledger row; the tag is informational and
enforces nothing. An absent license
reads as UNKNOWN
([ADR 0058](../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md)).

## Capability is derived, never declared

`internal/mediacap` answers "what can this box actually render?" from the bindings themselves and
the files they name — the same gates the pipeline routes on. Three verdicts per route:

| Verdict | Meaning | Is it a fault? |
|---|---|---|
| `CONFIGURED` | bound, and every file it names exists — for a render-script route, everything the script loads too | no |
| `NOT CONFIGURED` | no binding on this box; the task defers by design | no |
| `BOUND-BUT-MISSING` | the config names a file that is not there, or the route's graph/worker loads something that is not | **yes** — the task defers at call time |

Both reporting surfaces read from it: `local-offload doctor`'s media section (a
`BOUND-BUT-MISSING` route exits non-zero) and the MCP `offload_status` tool's `media.routes`.

**What a render-script route loads (<node-e> parity audit, 2026-09-23).** `generate_video`,
`animate_character` and both `generate_audio` kinds used to be `CONFIGURED` as soon as their script
existed; on the 8 GB reference box that was three green rows over routes that failed when called. The
verdict now covers what the script loads (`internal/mediacap/routeneeds.go`):

- **Model files** of the route's graph: every file the config binds AND the builder default it falls
  back to when a key is unset (`videogen_text_encoder` unset still loads umt5; the Wan VAE has no key
  at all). The video set follows `videogen_family` exactly as the runner dispatches (Wan 2.2, `ltx25`,
  `h3`, `hunyuan`); animate checks the four WAN-Animate-2 files, music the four ACE-Step files. Files
  only a per-request mode loads (Wan `fast=true`'s lightx2v LoRAs) are named when absent, never a
  failure. `TestRouteNeedsMirrorTheRenderBuilders` parses the `render/wf-*.mjs` defaults so the two
  cannot drift.
- **Custom-node classes** the graph names: `VHS_VideoCombine` (ComfyUI-VideoHelperSuite) for Wan and
  Hunyuan, the DisTorch2 loaders (ComfyUI-MultiGPU, plus ComfyUI-GGUF for a `.gguf` expert), and
  `UnetLoaderGGUF` (ComfyUI-GGUF) for a `.gguf` image/edit UNET. `doctor` asks a ComfyUI that is
  already running (`GET /object_info/<class>` on `COMFY_API`, default `127.0.0.1:8188`), which also
  sees a pack that failed to import; when nothing answers it decides from `<comfy_dir>/custom_nodes`
  on disk (an active pack directory with `__init__.py`, matched by name or by its marker class) and
  says which it used. `offload_status` and `acceptance` use the disk check only. Nothing starts
  ComfyUI or loads a model.
- **Voice**: the python `render/tts.mjs` will spawn (`TTS_PY`, else `<repo>/.tts-venv`, else `python`
  on PATH), the `chatterbox` and `torch` packages in its site-packages, and the Chatterbox weights in
  the Hugging Face hub cache (`HF_HUB_CACHE`, else `HF_HOME/hub`, else `~/.cache/huggingface/hub`).
  These resolve from the environment of the process asking; a server started with another `TTS_PY` or
  `HF_HOME` resolves its own.

Every missing item is listed on the route's line, so one doctor pass is the whole fix list.

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

`render/compose-hyperframes.test.mjs` also pins the shipped compose templates: the offline, deterministic and
declared-variable contract, the ported templates' provenance, the retained licence texts by hash, the residue gate
(no creator names, brand palette, placeholder copy or CDN host), the no-scale-above-1 guard for the ports, the
catalog and the hard bans of the house skill (`skill/hyperframes-compose/SKILL.md`) and the lists in this doc.
`render/captions-groups.test.mjs` covers the caption helper, and
`internal/mcpserver/composevideo_test.go` fails when a shipped template is missing from the tool description.

`crossplatform_lint_test.go` (repo root) is the gate on the resolution rules above: a runner that
probes a Windows venv interpreter without a POSIX one, a drive-letter literal in shared Go with no
`runtime.GOOS` branch, or a `.exe` in a tier `config_seed` fails CI. The two `amd-rdna3*` seeds are
recorded as known offenders with their reason rather than silently skipped — a NEW one fails.

## Common pitfalls

- Assuming the free step unloads everything — it deliberately preserves the memory stack.
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
  (per-instance marker files)
- [`render/comfy-input.mjs`](../../render/comfy-input.mjs) — staging a runner's inputs under names
  unique across processes
- [`render/comfy-instance.test.mjs`](../../render/comfy-instance.test.mjs) — per-card instances: the
  default instance pinned to its old behaviour, and the keyed one
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
- [`render/compose-templates/_third_party/`](../../render/compose-templates/_third_party/hyperframes-student-kit/PROVENANCE.md)
  — the licence texts of third-party template sources, kept verbatim, with a record of what was taken
- [`render/captions-groups.mjs`](../../render/captions-groups.mjs) — the pure caption grouping helper
  (`offload_transcribe` segments to the `captions-bar` `words_json`)
- [`skill/hyperframes-compose/`](../../skill/hyperframes-compose/SKILL.md) — the house skill for the compose lane:
  the hard bans on the HyperFrames CLI, which tool to pick, the template catalog, the verification loop and the
  captions flow (the canonical copy; installing it into an agent's skills folder is the operator's step)
- [`internal/pipeline/composevideo.go`](../../internal/pipeline/composevideo.go) — `runComposeVideo`:
  the compose slot, the runner env allowlist (`gpugen.Spec.EnvExact`), typed defers
- [`internal/fleetnode/compose_task.go`](../../internal/fleetnode/compose_task.go) — the
  template-only `compose-video` fleet task
- [`setup/hyperframes/`](../../setup/hyperframes/package.json) — the pinned lockfile the installers
  install from
- [`render/templates-catalog.mjs`](../../render/templates-catalog.mjs) — the read-only workflow
  templates catalog: active-node walk, model files, API flag, licence class and gate, directory-aware
  readiness, node snapshots, candidate diff
- [`render/templates-license-map.json`](../../render/templates-license-map.json) — the harness-owned
  licence map, keyed by Hugging Face repo id
- [`render/testdata/templates-catalog/`](../../render/testdata/templates-catalog/README.md) — its
  fixtures: trimmed upstream templates (MIT) and hand-written edge cases

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
| `template` + `variables` | a vetted template under `render/compose-templates/` (shipped: `title-card`, `lower-third`, `stat-card`, `section-title`, `callout-label`, `checklist-card`, `captions-bar`) plus values for its declared variables | yes, the only form |
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
| `LINT_ERRORS` / `CHECK_FAILED` | the composition failed its own gates (since HyperFrames 0.8.108 this includes text that spills out of its box, `layout/text_box_overflow`, which 0.8.61 rendered clipped; `strict: false` accepts it) |
| `RENDER_FAILED` | a failed row, or an output that failed the ffprobe gate |
| `BROWSER_MISSING` / `FFMPEG_MISSING` / `CLI_MISSING` | the pinned Chrome, ffmpeg/ffprobe or the pinned CLI is absent |
| `SPAWN_EBUSY` | an antivirus lock on ffmpeg ([hyperframes#4058](https://github.com/heygen-com/hyperframes/issues/4058)); retried once, then this class |
| `DISK_HEADROOM` | the frame-storage gate ([#4060](https://github.com/heygen-com/hyperframes/issues/4060)): above one worker every frame is stored (8.3 MB at 1080p, refused past 90 % of the free space), so render a long clip with `workers` 1, which streams; else move `compose_cache_dir` to a larger drive |
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

**Kit-derived templates and `captions-bar`** (same box, quality `high`, 2026-09-30). Render time is again
HyperFrames' own `renderTimeMs`; the whole gated call took 34-50 s.

| composition | frames | 1 worker | `auto` |
|---|---|---|---|
| `stat-card` 1080p mp4 | 180 | 21.2 s | 18.5 s |
| `section-title` 1080p mp4 | 180 | 21.2 s | 17.8 s |
| `checklist-card` 1080p mp4 | 210 | 33.3 s | 25.8 s |
| `callout-label` 1080p webm with alpha | 180 | 22.0 s | 22.7 s |
| `callout-label` 1080p mov (ProRes 4444) | 180 | 23.1 s | 19.9 s |
| `captions-bar` 1080p webm with alpha | 240 | 23.3 s | 23.7 s |
| `captions-bar` 1080p webm with alpha, 300 s of captions | 9,017 | 1,038 s | refused `DISK_HEADROOM` (33 GB free) |

`checklist-card` is the slowest per frame (23.8 s per 150 frames at one worker) because four blurred rows and
a title enter together. Each of them, rendered at 1, 2, 4 and 6 workers and at `auto` twice, gave 15 of 15
identical pairs of decoded frames. That check found a real defect on the first pass: two of the cards animated a
scale above 1 (a halo breathing out to 1.06, a ring popping to 1.1), and their frames differed between worker
counts (80 of 180 and 172 of 210 frames, by a few pixels of gradient; two runs at 4 workers disagreed with each
other too). Blur was not the cause (the same cards with the blur removed still differed) and opacity in place of the
scale fixed both. The ports and `captions-bar` therefore never scale above 1, and a test guards that. The older
`title-card` still drifts one large glow out to 1.12 while it translates, and it was measured too (quality `high`,
150 frames, 1, 2, 4 and 6 workers and `auto` twice): 15 of 15 identical pairs. So a scale above 1 is not always
worker-dependent, and the two defects are pinned down only as far as the controlled variants go; the contract is in
the [templates README](../../render/compose-templates/README.md).

### Captions from a transcript

`render/captions-groups.mjs` is a pure helper that turns `offload_transcribe`'s `<base>.segments.json` (the
`{word, start, end, probability}` array `internal/sttclient` writes: whisper-server's tokens, not words, so a
word-initial token keeps its leading space and a continuation such as an apostrophe suffix, the digits after a
currency sign or punctuation has none, and the helper merges those back into words first) into the `words_json`
variable of the `captions-bar` template. It groups words a few at a time (`punchy` 3, `conversational` 5, `calm` 6; a group also ends at
a sentence end, at a pause of 0.15 s or more, or past 42 characters), holds each group 0.3 s past its last word and
never past the next group's start, and packs the groups into chunks that fit the template, which takes at most
16,000 characters and 600 s. The helper cuts at 300 s by default (the reason is measured below);
`--chunk-sec` sets another cap up to the template's 600 s. The first chunk keeps absolute time; each later one is
rebased to 0 and carries the `offset_sec` to lay it at.

A 16 KB string variable was measured before the template was designed: a 15,968-character list (476 groups)
passed `lint`, `check` and `--strict-variables` and arrived intact in the page. The design that followed was
measured too. One CSS-animated element per group is flat up to 80 groups in the page (24 s for 240 frames) and slows
from about 150 (34 s), and a 374-group, 12.9 KB list had written about 290 of its 450 frames after ten minutes, when
it was stopped. The template therefore holds one element and derives the frame from the time of HyperFrames'
`hf-seek` event, which renders those 450 frames in 39.6 s (draft quality, clean CPU). A full chunk is long, though:
a 300 s chunk (9,017 frames) was refused `DISK_HEADROOM` at `auto` workers with 33 GB free (HyperFrames stores every
frame above one worker and budgets 8.3 MB each) and rendered at one worker in 1,038 s (115 ms a frame, on a busy box),
so a 600 s chunk would pass the 1,800 s default of `compose_timeout_sec`, which is why the default cut is 300 s and not
the template's 600 s. Render chunks at `workers` 1 (the captions-bar README, "Long chunks"). `offload_media` has no
overlay operation, so a chunk's `webm` or `mov` overlay is laid over the footage with ffmpeg's `overlay` filter or in
an editor; the overlay itself is silent.

### Security

- **Compositions are trusted code.** HyperFrames' Chrome launches with `--no-sandbox` and site
  isolation disabled. `html` and `project_dir` are accepted only from the local MCP and CLI doors, the
  same trusted-caller posture as `run-graph`. The fleet door is not token-gated
  ([ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)), so it refuses
  both at ack time and renders only the node's vetted templates. It also ignores a caller's `out`:
  the node writes `<media_dir>/compose-<hash8>.<ext>`, the same rule every fleet media task follows
  ([fleet-node.md](fleet-node.md)). Template variables reach the page
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
  pinned chrome-headless-shell from `storage.googleapis.com`, and `hyperframes_browser_path` binds it
  explicitly, because the CLI's own lookup prefers a newer build in `~/.cache/puppeteer`. That
  `ensure` spawn alone carries the node flag `--dns-result-order=ipv4first` (not env — the allowlist
  above stays exact): plain `dns.lookup`'s default order can hand back an unreachable IPv6 address
  first with no fast failover, hanging the download on a box with a dead IPv6 route to that host even
  though a plain `curl` recovers in seconds; the existing per-op deadline (`--timeout-sec`, default
  900s) still bounds the call and fails it typed `TIMEOUT`/`BROWSER_MISSING` either way. The installer never runs
  `npm install -g`: a global HyperFrames self-upgrades in a detached process. The runner refuses an
  install whose version is not its own pin (`PINNED_VERSION`, held equal to the lockfile by a test)
  with `CLI_MISSING`, because every guard here was read in the pinned source. `acceptance` runs
  that check as the node's identity.
- **Offline renders.** Vetted templates reference no URL, and every font family they use is declared
  with `@font-face` from the shared kit. An undeclared family makes the compiler request the Google
  Fonts CSS API, with the page's character set in the query.

## Remote routing and the media-job door (ADR 0072)

`offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio` and
`offload_run_graph` (CLI `generate-image`, `generate-video`, `generate-audio`, `run-graph`) take `route` and
`remotes` (`--route`, repeatable `--remote`). A machine with no lane for the job, or a caller who names a node, can
render on a fleet node and get the output back hash-verified. The other media tools (`offload_edit_image`,
`offload_generate_svg`, `offload_media`, `offload_upscale_image`, the inpaint and generative-edit routes) stay local.

**Where a job runs.**

- `local` always runs here. It never touches the network.
- `auto` (the default) runs here when this machine has the lane, and the call is then byte-identical to one made before
  the route existed. "Has the lane" is read from the files, not the binding: `mediacap` derives the route (the script,
  the weights its graph loads, the custom nodes it names), so a default config, which binds every script, does not make a
  thin client look like a render box. With no lane here, `auto` goes to a node from `delegate_remotes`; with no lane and
  no fleet configured it runs here and returns the pipeline's own deferral, as before.
- `remote` always goes to a node. `remotes` narrows the nodes for one call; each must already be in `delegate_remotes`,
  and the call is refused before any probe otherwise.

**Which node.** The client reads each candidate's `/fleet/health` and keeps the nodes that list the task (and `media-job`
when input files travel), report every route the task needs as CONFIGURED when they report routes at all (a node that
predates `media_routes` is unknown, not refused), and do not hold a TEXT lease (they would answer 503). Among those, a node
with no held lease ranks first, then the shorter queue (queued plus running), then config order. Every miss is named in the
defer, with `defer_class` `capacity`.

**What travels.** A job with no input file goes through `POST /fleet/dispatch`. A job with a still (`offload_generate_video`),
a reference and driver (`offload_animate_character`) or a clone sample (`offload_generate_audio`) packs the files into a
bundle (each under its field name plus its extension, copied into a temp directory and packed with `composebundle`) and goes
through `POST /fleet/media-job` ([fleet-node.md](fleet-node.md#the-media-job-door-artifacts-and-honest-advertisement-adr-0072)).
`run-graph` carries its graph and manifest inline, so it never needs the door. The payload uses the field names the node's
builders decode; `out` and `out_dir` never travel (`out_dir` is where the fetched outputs land here). Three request fields cannot ride the fleet task and defer by name
(`defer_class` `contract`) instead of being dropped: `refine=false`, `tts_voice` and `transformer`.

**What comes back.** The client polls `/fleet/jobs/{id}` every 2 seconds inside a budget when the caller gave no deadline
(image 2 h, video and animate 6 h, audio 1 h, run-graph 2 h), then fetches every output the result names by bare name from
`/fleet/media`. Nothing lands until every file is downloaded and its sha256 equals the one the node published in
`artifacts`: a mismatch deletes what was fetched, leaves any file already at `out` untouched, and defers as
`infrastructure`. The primary output goes to the caller's `out` when given, the rest into this machine's `media_dir` (or, for
`run_graph`, the caller's `out_dir`, created if missing); no fetched file ever replaces one that already exists except the
caller's own `out`: each output is staged under a unique temp name, the primary takes the node's file name when it is free,
every other output is prefixed with the remote job id, and a failure part-way removes every temp and names the files that
already landed. The result's paths are rewritten to the local copies and it gains `node`, `remote_job_id` and, when the node published no
artifacts (an older node), `unverified: true`. `meta.node` names the node and `meta.placement` reads `remote: forced` or
`remote: no <lane> lane on this machine`. A node's 503 or 429 is a `capacity` defer, a 400 or 413 a `contract` defer, a 401
or 403 (on the dispatch, the poll or the fetch) a `config` defer, a call whose own deadline passed a `budget` defer naming the
node and the remote job (which may still hold its card), and a transport failure an `infrastructure` defer. An input file this
machine cannot read is `contract`; this machine's own temp directory, disk or packer failing is `infrastructure`. A defer the node itself returned (a render that
deferred) comes back as the node sent it, with `meta.node`.

## Comfy workflow templates catalog (phase A)

`render/templates-catalog.mjs` is a read-only catalog of the ComfyUI workflow templates a node
carries: the `comfyui-workflow-templates` package that ComfyUI itself pins, a checkout of the upstream
repository, or a `pip download` extract of a candidate version. It lists and classifies. It never runs a
template, downloads a model, installs a package, changes a config or touches the network, and the only
file it writes is the one named by `--out`. There is no MCP tool, config key, fleet task or route for it
yet. A template is a UI-format graph and `run-graph` takes API-format graphs only (the tool description
says so, and `preflight-graph-file.mjs` reads each node's `class_type`), so running one needs a converter
that this phase does not contain.

**Class: none.** No GPU slot, no lease, no ComfyUI process. It is one dependency-free file (`node:`
builtins only), so the whole file can be piped to `node --input-type=module - snapshot --comfy-dir <tree>`
on any node and run there with nothing deployed. A piped file has no licence map beside it, so its other
verbs count every repo as unknown unless they are given `--license-map FILE`.

**What it derives, per template.** Every count carries its basis: the package it was computed on
(`installed package`, `package extract` or `repo checkout at <commit>`, with the wheel versions). The
installed package, a candidate and the upstream HEAD give different numbers, and a figure without its basis
is not a figure.

| field | rule |
|---|---|
| active nodes | subgraph instances are expanded; a node is active only if its mode is not 2 (muted) or 4 (bypassed) and every instance around it is active; a self-instantiating or dangling subgraph is flagged, never hidden |
| model files | `properties.models` of active nodes (class, URL, hash), plus loader widget files nobody annotated; files that only bypassed nodes reference are reported apart and not required |
| `kind` | `api` (paid partner nodes) beats `custom_nodes` (a node pack core does not carry) beats `local` (core nodes only) |
| API flag | three signals, each reported: a node type in the node's own `comfy_api_nodes` ids (read from `node_id="X"`, a `NODE_ID = "X"` class attribute and the first argument of `_cloud_schema`), the `api_` name prefix, the index's `openSource: false`; any one is enough, and the summary counts the templates where they disagree |
| parameter surface | the `proxyWidgets` of its subgraph instances, the widgets a caller could override |
| licence | the worst class among the repos its active nodes download from, from the harness's own map |
| gate | `blocked`, `ack_required` or `open`, below |

**Licence map and gate.** `render/templates-license-map.json` is keyed by Hugging Face repo id. Each entry
has a class (`permissive`, `conditional`, `non_commercial`, or an explicit `unknown` for a repo whose licence
nobody has read), the source URL, the date and the basis, and a repo that is not listed is `unknown`. An entry
whose basis begins `inferred from` was copied from the sibling repo it names, not read: it carries no source URL
or date, and the summary's evidence range ignores it. A template takes the worst class of its repos (non_commercial >
conditional > unknown > permissive), so an unlisted repo never hides a known restriction, and a file with no
Hugging Face source counts as unknown. The boolean `commercial_use` of
[ADR 0058](../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md)
(status Proposed) cannot say `conditional`, which is why the map has a third class. The map's 50 entries
were seeded from repo-level licence tags digested on 2026-09-30 and none was re-fetched when it was written;
a tag is not a per-file licence, and the conditions text comes from digests of licence texts that were not
read in full. The gate: a paid API template is `blocked` (it spends money,
[ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md)); a FLUX-family template is
`blocked` ([ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md)), meaning a FLUX
name, a repo id containing "flux" or under the model maker's organisation, or a map flag, with the shared
text-encoder repo exempt by its entry. A repo whose id merely contains "flux" does not count when the template
takes only text encoders from it (a text encoder is another maker's model wherever it is hosted): the gate warns
instead, while any other class from that repo, a file with no class and the maker's own organisation still bar.
Non-commercial, conditional and unknown licences are `ack_required` (an explicit acknowledgement that names the
class); permissive or weightless is `open`. A FLUX-named weights file from another repo is only a warning. `list`
hides blocked templates unless `--include-hidden`; naming `--kind api` lists the paid API ones without it, and
the FLUX-family ones stay hidden.

**Readiness is directory-aware.** A file is usable by a template only if ComfyUI would offer it to the
loader that reads it, and that is a question about directories, not names: a VAE loader lists the `vae`
class and nothing else. The check reproduces ComfyUI 0.37.0's `folder_paths.py`, `utils/extra_config.py`
and the lines of `main.py` (`apply_custom_paths`) that register directories: default directories per class,
three of them dual (`text_encoders` also reads `clip/`, `diffusion_models` also reads `unet/`, `controlnet`
also reads `t2i_adapter/`); the yaml keys `unet` and `clip` mapped to their classes; `base_path` joined to
each entry, an absolute entry winning, `is_default` first; the output directory's `checkpoints`, `clip`,
`vae`, `diffusion_models` and `loras` added last (`--output-directory` moves it, `--base-directory` moves it
and the models directory); and a class listing that is recursive, follows links, skips `.git` and keeps only
the extensions its core loaders accept, by path relative to the directory (so `sub/name` is not `name`, and a
`.gguf` is not offered by a core loader). A requirement is `present` (or `present_class_unknown`: a loader
file nobody annotated, met by its exact name in any class) or says why not: `missing`, `wrong_class` (and
where the file is), `in_subfolder`, `case_mismatch`, `extension_not_listed`, `class_unregistered`.
`--mode basename-only` is the older shortcut (any file with that name, anywhere), kept so its numbers can be
reproduced and compared. Only `local` templates are evaluated. A snapshot records the node's ComfyUI version
and the hashes of `folder_paths.py` and `utils/extra_config.py`; `readiness` compares the hashes with the
0.37.0 files and reports `rules_check` (advisory: the node is scored either way, and the directory-aware text
table prints a `rules:` line under a node whose files differ).

**Verbs** (`node render/templates-catalog.mjs <verb>`): `summary`, `list`, `catalog` (the full catalog as
JSON), `snapshot` (one node's package versions and fingerprint, API node ids, resolved model roots, model
files, and hashes of the two ComfyUI rule files with line endings normalised; it exits 2 on a directory that
is not a ComfyUI tree), `readiness` (per node and on any node, `--mode directory-aware|basename-only|both`)
and `diff` (what a candidate package adds, removes and changes, and which templates it would break on each
node; `--candidate-basis` says what the candidate directory is). Sources are `--templates-dir` or `--comfy-dir`.

**Invariants.**

- No count without its basis (`stamp.label`).
- Read-only: a test fails if the file imports anything but `node:` builtins, opens the network, spawns a
  process or writes anywhere but `--out`.
- A catalog speaks only for a node whose installed json package is the same version; `readiness` refuses
  otherwise, and `--candidate` answers anyway and labels it.
- An unlisted repo is `unknown`, never `permissive`.

**Testing.** `node --test render/templates-catalog.test.mjs` runs on trimmed real templates (MIT, see
`NOTICE`) and hand-written edge fixtures in `render/testdata/templates-catalog/`. With
`TEMPLATES_CATALOG_DIR` set to an installed package's `templates` directory (and
`TEMPLATES_CATALOG_COMFY_DIR` to its ComfyUI tree) one more test reproduces the counts measured on the
0.1.94 package: 566 entries, 326 API, 240 local, 235 of them needing weights, 5 with none. It is skipped
otherwise.

**Known gaps.** The catalog is data; nothing consumes it yet. Per-repo licence texts are still to be read
for the 83 repos (of the 133 that the installed package's local templates download from) that the map does not list. Six templates name a FLUX-named
weights file (mostly a FLUX.2 VAE) without being FLUX; they are reported, not decided. Two templates
(`image_ideogram4_t2i` and its `_int8` variant) take a FLUX.2 VAE from a FLUX-named repo and are `blocked` by the
repo signal; whether ADR 0011 reaches a reused FLUX VAE is the operator's call. The
run-graph satisfier's presence check (`modelCandidates` in `manifest-satisfy.mjs`) looks only where the
manifest path's own class is registered, so on a node with no `extra_model_paths.yaml` it does not find a
file under `models/unet` for a `models/diffusion_models/...` path that ComfyUI itself lists; that is a
follow-up for the models phase, not part of this one. The directory rules are ComfyUI 0.37.0's: a node whose
rule files differ is scored with them anyway and `rules_check` says so, and `main.py` is not hashed, so a change
confined to it is not seen. An `extra_model_paths.yaml` entry that is rooted but has no drive letter
(`/models/x`) under a drive-lettered `base_path` resolves onto that drive under Windows path join and is left as
written here (measured against ComfyUI's own loader, not on a live file).
