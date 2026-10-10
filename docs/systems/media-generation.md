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

**iGPU media engines (CT-49).** The video, animate, voice and music lanes also run on a Vulkan-only box (no CUDA, no ROCm) through spawn-per-job native engines (sd.cpp `vid_gen`, sd.cpp VACE with depth-anything.cpp, audio.cpp), under a hard rule that no model runs on CPU. See [iGPU media engines](#igpu-media-engines-video-animate-voice-music-ct-49).

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
- How do video, animate, voice and music run on an iGPU-only box, and how is CPU placement refused?

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
  small (a few hundred MiB each; the larger EmbeddingGemma-2 entry on the tiers that carry it, 1,196 to 1,536 MiB with its projector and 460 to 482 MiB text-only) and, on the reference box, pinned to the utility card, not the render card. An earlier unload-all implementation tore that memory stack down
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

**Outputs are delivered atomically (0.178.0).** Every ComfyUI runner (`comfy-render`, `-edit`,
`-inpaint`, `-animate`, `-upscale`, `-video`, `-music`) used to fetch `/view` and call
`writeFileSync(out, bytes)`, which opens, and so truncates, the target before it writes. On
2026-10-09 the data drive filled to 0 GB in the middle of a 36-picture `comfy-generate --batch`:
the writes failed with `ENOSPC`, 21 zero-byte PNGs were left at the jobs' `out` paths, and the
batch went on to the next job. A zero-byte file passes every `exists()` check, so a skip-existing
builder never re-renders it, and the same call destroys a previous good file at `out` whenever the
new write fails. Now `render/atomic-out.mjs` (the Python workers: `render/atomic_out.py`) stages the
bytes in a sibling of `out` in the same directory (`<out>.partial-<pid>-<n>`) and renames it over
`out` only after the write finished (a POSIX rename, or MoveFileEx with replace on Windows). Any
failure removes the staged file and rethrows the same error, so a failed render leaves **nothing at
`out`** and a good file already there is **left untouched**. An empty payload is refused. A
transient `EBUSY`, `EPERM` or `EACCES` on the rename (an antivirus scanner holding the staged file
on Windows) is retried four times over 750 ms before it counts as a failure. The error names the
output (`ENOSPC: no space left on device, write (writing <out>)`); Node's own message for a failed
write names no path. The same helper delivers `run-graph`'s output files and its result envelope,
`captions-groups.mjs --out`, `sdcpp-generate.mjs`'s engine output and its alpha rewrite (sd-cli
writes a staged sibling, extension last, that is renamed after the rewrite), the cross-volume copy
in `compose-hyperframes.mjs` (`moveInto`, which copied straight onto the destination), the music
runner's trim and loudness swaps (one rename instead of unlink-then-rename, so a failed swap ships
the render as it was), and the Python workers `edit_image.py` and `tts_chatterbox.py`. The igpu
lanes below already worked this way. A runner killed mid-write (a taskkill cannot be caught) can
leave a `*.partial-<pid>-<n>` file; it never carries the output's name. `render/output-writers.test.mjs`
lists every direct file write left in `render/` and why it is not an output, so a new
`writeFileSync(out, ...)` fails a test instead of the next batch.

**Warm batch.** `generate-image --batch` takes a jobs file and runs N renders in one session. The
only behavioral change is omitting ComfyUI's `--cache-none`, so the checkpoint loads once; teardown
still happens exactly once, at the batch boundary. A failed render is recorded and the batch
continues, one JSONL result line per job. The exit code of a `--batch` runner
(`comfy-generate.mjs`, `comfy-inpaint.mjs`) says how it ended: **0** every job rendered; **4** the
batch ran every job and at least one failed (0.178.0; until then it exited 0 and a caller had to grep
the log for `RENDER FAILED`: the rows with `"ok":false` in the results file name the failures, and
the last log line gives the counts); **1** the batch could not run to the end; **2** usage. 4 is not
3 on purpose: 3 is the child's *server unusable* code and `renderExitError` reads it as a verdict.
The Go side (`imagegen.BatchExitJobsFailed`, pinned to `render/batch-jobs.mjs` by a test) treats 4
with a non-empty results file as a finished batch and reads the rows, so `generate-image --batch` still
reports per-job status in its JSON and is not an error for 35 good pictures out of 36; the results
file format did not change. Two failures stop the batch instead of being recorded and passed over,
because every later job would fail the same way. A server that became unusable (the child exits 3):
the failed job and every later job get a row, the later ones with an `error` that starts `not run:
ComfyUI became unusable at job N/M`, and the batch exits non-zero and its teardown frees the card and
the lease, instead of failing every remaining job against the same server (C-83: 3 min each, after a
48-minute wait on the first). A full disk (0.178.0: `ENOSPC`, `EDQUOT` or `EROFS`, read from the
errno where the runner renders in-process and from the child's `RENDER FAILED:` line or ComfyUI's
own `[Errno 28] No space left on device` otherwise): the same stop, with `error` starting `not run:
the disk is full at job N/M, writing <out>`, exit 1 and the error line naming the path. It is
deliberately batch-wide: the output directories of one batch are normally one volume. A failed job's
`error` carries the child's own `RENDER FAILED:` reason rather than only `comfy-render exited N`.
The inpaint batch (`comfy-inpaint.mjs --batch`) stops the same way on an unusable server and on a
full disk (its `_row: "aborted"` line carries `reason`: `server_unusable`, `disk_full` or
`consecutive_failures`, with the count of jobs not attempted), and still stops after
`COMFY_BATCH_MAX_CONSEC_FAIL` (3) consecutive failures; if every job fails it keeps its "systemic"
stop (exit 1) rather than 4. **The default single-render path is unchanged.**

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
render layer now knows how to run, find, reuse and stop such an instance. The admission that picks a card, the per-card in-process slot and the
waiting-place token are P13b ("Per-card media admission", below); a host that does not lease cards behaves
exactly as before.

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
`COMFY_PORT_BASE` (default 8189) plus `COMFY_INSTANCE_INDEX`; the harness's own admission (P13b) gives
the endpoint explicitly as the base plus the card's nvidia-smi index, so a card always has the same port
(the ownership proof refuses to reuse an instance whose launch marker records another one), and
`COMFY_PORT_BASE` moves the whole range. The harness reserves 8189 to 8192 on a host for this, so a
hand-started set of per-card ComfyUI processes on the same ports (an older stopgap launcher that
numbers them by card ordinal and uses its own output directories) is never reused or stopped: it
fails the ownership proof above and is refused with `COMFY-PROFILE-MISMATCH`. The two launchers must
not run in one lease window until that stopgap is retired. The host's record of the ports it listens
on must name the harness as the owner of that range before a host enables card-scoped leases (the Port
Directory is outside this repository: update it with the host's other listeners).

**Runners.** Every render runner (`comfy-generate`, `-render`, `-video`, `-edit`, `-inpaint`,
`-animate`, `-upscale`, `-music`, `-run-graph`) resolves its endpoint through `comfyApi(flags.api)` and
hands it to `withGpuSlot`. For a **keyed** instance `withGpuSlot` launches, frees (`POST /free`) and
tails the log of that instance (before this, the launch and the post-run free ignored `--api` and used
the environment's endpoint). For an **unkeyed** instance on the default endpoint nothing changes:
`ensureComfy` is still called without an api and the post-run `/free` goes to the endpoint the runner talked
to. An **unkeyed** instance on ANOTHER endpoint (`--api` or `COMFY_API` naming a port other than 8188, with
no `COMFY_INSTANCE` or `COMFY_CARD_UUID`) used to be ensured on the default endpoint: an unkeyed launch has
no `--port`, so a blog batch run with `--api http://127.0.0.1:8189` started a stray ComfyUI on 8188 inside its
card lease while every job went to 8189, and the stray made a later card-bound default binding refuse with
`COMFY-PROFILE-MISMATCH`. The runner now hands that endpoint to `ensureComfy`, which reuses it when it
answers and otherwise fails with `COMFY-ENDPOINT-DOWN` (naming the endpoint and the fix: start it yourself,
or key the instance, which makes the runner launch it on its own port), launching nothing; the one launch
that stays is the operator's own, `COMFY_EXTRA_ARGS` carrying that very `--port`. An orphan on that endpoint
that needs replacing is refused before it is stopped when the relaunch could not land there. `run-graph`
starts and frees its own ComfyUI through the same rule (`instanceDeps`, and its free follows the endpoint
it ran on, not 8188), and an env that cannot name an instance is a typed `RUN_ERROR` defer in its result
file (exit 0), like every other `run-graph` failure. The Go side carries the optional endpoint and
card in `imagegen.ComfyLaunch{API, CardUUID}` and `gpugen.Spec{ComfyAPI, CardUUID}`; the post-run
`/free` goes to the instance that ran.

**A kept instance (`--keep-comfy`) is detached, not a pipe-fed child (P13b).** A runner told to keep the
ComfyUI it launched used to never exit: the instance was a non-detached child with piped stdout and stderr, so
the runner's event loop stayed attached to a process meant to outlive it, and its parent saw a hang until a
per-shot timeout killed the tree (and deleted the finished clip: caught on a real film run, 2026-10-03). Now,
and only when the runner keeps the instance (`withGpuSlot` passes `keep` to `ensureComfy`), the instance is
spawned **detached** (its own process group), with its console going to **its own log file** (a descriptor, so
there is no pipe for a runner's exit to close under it: a process still printing into a closed pipe dies with
`EPIPE`), **unref'd** (the runner's loop no longer waits for it) and with its window hidden (a detached console
process opens one on Windows). The non-kept path is byte-for-byte what it was: piped, attached, killed with its
runner. A kept instance that never answers is still killed, since a half-started instance is nobody's.

- **The log is bounded by rotation, not by a capture that stops at 5 MB.** A piped child can be capped as it
  is written; a descriptor handed to another process cannot be truncated under it without a helper process
  to pump the stream, and this harness does not add a long-lived helper for a log. Each launch rotates the
  log (`.1` to `.3`, as before) and cuts the archived copy to its last 5 MB (`trimRotatedLog`), so the disk
  holds at most three 5 MB archives plus the run in progress. The live file of a run is not truncated: the
  instance lives no longer than its lease, which bounds it. `tailComfyLog` (the lines a failure message
  carries) reads only the last 256 KB of the file, so a long-lived instance's log is never read whole.
- **Who stops it.** A kept instance lives no longer than the GPU lease it was launched under; its marker
  records that lease epoch (`leaseEpoch`). The **holder of the lease stops it on release**: `gpu reserve`
  when its wrapped command ends, and the pipeline when its media lease is released
  (`internal/comfyinst`: the marker is matched to the epoch, the instance is shown to be the harness's by pid
  and exact argv on its own endpoint, freed with `POST /free`, then stopped). A kept **default** instance
  (no key) records no lease epoch and is stopped by whoever kept it, as it always was.

**Verifying a pin on Windows.** The driver reports no per-process rows for graphics-mode (WDDM)
cards, so confirm that an instance is on its card by per-card memory deltas, not process ids.

| line | meaning |
|---|---|
| `COMFY-INSTANCE-INVALID: …` | a key, uuid, port base or index that cannot name an instance, or a keyed instance on the default port |
| `COMFY-INSTANCE-CONFLICT: …` | an instance key (explicit, or a card uuid) and a device index both given |
| `COMFY-INSTANCE-WARN: …` | extra args that would override the instance's pin, port or directories were dropped, or an instance with no card pin was launched |
| `COMFY-PORT-TAKEN: …` | the instance's port is held, on an address ComfyUI will listen on, by something that is not ComfyUI; nothing was launched or killed |
| `COMFY-PROFILE-MISMATCH: …` | a ComfyUI answers on the instance's port but is not shown to be that instance (or is on the wrong card); refused, and stopped only when the harness's own marker proves it is the harness's and its spawner is gone |

### Per-card media admission (plan P13b)

On a host that leases cards (`gpu_card_scoped_leases` set **and** a green reader audit, see
[gpu-lease.md](gpu-lease.md)), a generation call no longer takes the whole-node media lease by default.
It asks for what it needs, and the answer decides the lease, the ComfyUI instance and the env its runner
is given:

| the call | asks for | holds | runs in |
|---|---|---|---|
| a single-card route (image, edit, inpaint, upscale, animate, music, un-pooled video) with **no** `comfy_cuda_device` | one card, chosen by the allocator | a lease on that card | the instance bound to that card |
| the same with `comfy_cuda_device` set and resolvable | exactly the card the pin names, and no other | a lease on that card | the instance bound to that card |
| the same with a pin that cannot be resolved (no `gpu_comfy_order`, a comma list, not an index, a position the box does not have) | the whole node, with today's `--cuda-device` | the whole node | the default instance, as before |
| a pooled image or video route | the cards its pool keys name, when the pool has at least two cards and the box has at most three | a lease on those cards | the default instance, **launch unchanged** (every card visible) |
| `run-graph` with ONE declared device | that card | a lease on it | its instance, pinned by uuid |
| `run-graph` with several declared devices, or none, sd.cpp, voice | the whole node | the whole node | the default instance (none for sd.cpp and voice) |
| any call on a host that does not lease cards, or whose card table cannot be read | the whole node | the whole node | exactly as before this change |

**The allocator and the display card.** An unpinned single-card call takes the allocator's card
(`gpu_lease`, "The allocator"): not claimed by a live lease, not promised to a caller waiting in line
(below), not held by another job in this process, with the free VRAM and host RAM, and never the display
card while the operator is at the desk. The display card is the one whose `display_active` reads Enabled
**or** whose `display_attached` reads Yes (`gpuprobe.ScreenCardUUIDs`, the card table's rule); it is auto-assigned only when
`operator_presence` says the operator is away, and then only while the display layer's desktop floor stays free
after the call's footprint (a media call declares none, so an unpinned call never takes it: it picks among the other
cards; `gpu_lease`, "The display card, once the operator is away"). An explicit pin may always name it (the
operator's word). Ties go to a card with no resident seat, then the cheapest eviction, then the lowest id.

**An explicit pin is a hard constraint.** It is never re-picked: the call queues, FIFO, for the card it
names, however many others are free. It still runs in the per-card instance, not in the default one, so
a pinned call never meets another job on port 8188. The pin is in ComfyUI's device order, so turning it
into a card needs the order the box declares (`gpu_comfy_order`); without it the call falls back to the
whole node and says so once in the log. Nothing is ever guessed from an index.

**Why the default instance is not for a partial lease.** One ComfyUI serves one port, one output
directory and one launch marker for the whole box. Two jobs that both use it contend on all three; a
second job whose launch profile differs is refused (`COMFY-PROFILE-MISMATCH`), and one that asks for no
profile silently reuses the first one's instance, on the first one's cards. A job that holds only some
cards must therefore not run in it. Every single-card job gets an instance of its own; the jobs that do
use the default instance hold the whole node, or (pooled routes only) cards that every other such job on a
box of at most three cards also needs, so two of them cannot run at once. On a larger box a pool takes the
whole node, because two disjoint pools could. A `run-graph` with several declared devices is NOT in that
class: its graph is the caller's, the default instance sees every card, and a lease on only some of them would
not keep the graph off the others (a card another call's own instance holds, the display card), so it holds
the whole node; one declared device runs in that card's own instance, which sees no other card.

**What the runner is given.** `GPU_LEASE_DIR/EPOCH/CLASS` as before, plus `GPU_LEASE_DEVICES` (the cards
the lease holds, lease ids) for a card lease; for a per-card instance `COMFY_CARD_UUID` (the driver's GPU
uuid, never an index: `COMFY_CUDA_DEVICE` is blank) and `COMFY_API` (the instance's endpoint: the base
port, 8189, plus the card's nvidia-smi index, so 8189, 8190 and 8191 on the 3-card box; `COMFY_PORT_BASE`
moves the range). The port is stable per card because the render layer refuses to reuse an instance whose
launch marker records another port. And `GPU_LEASE_UNLOAD_MODELS`: the models the runner's unload may
take, which is the llama-swap roster minus the memory stack minus the seats pinned to cards the lease
does not hold (`gpualloc.UnloadModels`, the rule `gpu reserve --devices` uses). Without it the runner
unloads every model off the memory stack and a render on one card empties the seats on the others
(register C-86). The post-run `/free` goes to the endpoint the runner was given.

**In-process slots.** `mediaSlot`, a single slot for the whole process, is now a set with one slot per
card: two jobs on different cards hold theirs at once, a job that holds the whole node conflicts with every
card, waiters are served in arrival order with disjoint backfill, and a whole-node waiter is a barrier. A
process that runs under its parent's lease (`gpu reserve --devices 0,2 -- ...`, `GPU_LEASE_DEVICES`) picks
a free card among the lease's and waits for one when none is; a third call then answers busy, because the
lease is the parent's and there is no queue to hold a place in. A pin outside the parent's cards, or a lease
that holds the whole node, is served by the old path exactly as before.

**A call that cannot get a card keeps its place.** After its `gpu_wait_ms` (90 s) a call that has no card
does not fail with `gpu busy`. It leaves a place-keeping token and answers with a deferred result,
`err_class: gpu_queued`, `defer_class: capacity`, whose data carries `waiter_token`, `queue_position`
(1 = next), `eta_s` (the declared window left on the lease in the way: a ceiling, not a promise),
`devices` (the cards it waits for; empty = the whole node), `held_by` (the leases in the way) and a
`resume` sentence. The caller re-sends the same request with `waiter_token`, and the call resumes the place
it left, with the arrival time it had. A token lives ten minutes after the last time its caller polled it;
for the first 30 seconds of that it holds its place against later callers for the same cards, and after
that it is skipped by everyone (a whole-node barrier included), so a client that wandered off never blocks
the line. A call that RESUMED a place and is now waiting for its card inside this process (the card's slot
is held by another job here) re-asserts its place every ten seconds until it is served, hands it to its lease
wait, or gives it up again, so a caller that is standing in its place never loses it to that grace. A place that
was not resumed is not invented: a first-time call leaves one only when it gives up. Tokens are in `<state>/gpu/tokens`, not among the waiters (an older binary prunes any waiter
whose process stopped polling); a binary that predates them does not honour them, so on a host that mixes
versions it can take a card ahead of a token holder, which costs the holder its place and never
exclusivity. A call that holds the whole node on such a host leaves the same kind of token. A host that
does not lease cards keeps the `gpu busy` answer byte for byte.

**Only a door that can resume leaves a place.** A token is claimed by sending it back, which only an MCP tool
can do (`core.Request.Resumable`, set by the MCP server and by nothing that arrives over the wire). The
`generate-image` CLI verbs, the fleet-node dispatch (the delegator re-places a refused subtask, it never resumes
one) and the image batch get the plain `gpu busy` defer and leave nothing behind: a token nobody can claim would
hold its cards back from every later caller for the 30-second grace, and a node a delegator retries every few
seconds would keep newcomers behind a rolling set of them.

**Who stops the instance.** The grant's release stops the ComfyUI instances kept under the lease
(`internal/comfyinst`) before it releases the lease, so the next holder never finds one on its card, and only
while the lease is still the call's own: a call whose lease was taken away leaves them running and logs which.
A lease that reuses a kept instance (the previous holder died, or was fenced out, and the instance outlived it)
takes it over: its marker is re-stamped with the reusing lease's epoch, so that lease's release stops it. A
runner that does not keep its instance kills it itself, as before.

**A call with no card to take is a place in line too.** The allocator can find no card although the cards
are idle: the host is short of RAM, a transient `nvidia-smi` failure left the monitor's card unknown
(`display-unknown`), every card is the operator's screen or quarantined. That used to leave through the raw
allocator error, classed `gpu_lease_unavailable` (a configuration fault). A call that can resume now keeps a
place on the cards that would qualify but for what is short (the allocator's `Waitable` list, never the whole
node, so it does not become a barrier) with the reason, and resumes it when the host recovers; when no card
could qualify however long it waited (all of them the screen, quarantined, foreign-busy) there is nothing to
hold a place on, and every call gets the plain `gpu_busy` defer carrying the reason.

**Open acceptance of P13 (P13 is not complete until these are closed).** What the review pass could not close
from a worktree, with no GPU and no live lease root:

1. **The seeded `comfy_cuda_device` on the 3x16 tier.** The plan says the seed `"2"` becomes automatic
   allocation. The branch makes an explicit pin a hard constraint, and a config cannot tell the seed from an
   operator's own choice (no provenance), so on a stock 3x16 host every single-card call keeps the whole-node
   lease, and with `gpu_comfy_order` declared the pinned calls serialise on card 2. The tier note says so. Whether
   a pin equal to the seed is treated as unset on card-scoped hosts, or the enable step clears it, is the
   operator's decision; until it is made the complaint this phase set out to fix stays open on that tier's seed.
2. **The allocator is given no VRAM footprint and no RAM need for a media call** (`Need{}`: a footprint of 0 makes
   the fit check always true, and a RAM need of 0 enforces only the headroom floor), so N concurrent instances are
   gated by neither. A measured per-route VRAM peak does exist (the passive footprint store,
   `vram_peak_gb` per family, quant and task), but it cannot simply be passed in: the fit check compares it with a
   card's CURRENT free VRAM, which counts the text seats a media lease would unload, so on a box with a seat
   resident on every card it would refuse cards the lease can take. The allocator first has to credit the
   evictable resident seats (its `Resident` map already carries their cost). The RAM need has no figure at all, and
   the N-instance host-RAM measurement the plan asks for is a live run.
3. **The live two-card acceptance** (two `offload_generate_image` calls on two non-display cards in their own
   instances, confirmed by `nvidia-smi` per-card memory, not pids, under WDDM) and the **UUID-pin spike on a
   non-Windows host**.
4. **A foreign compute process is not skipped on the media path.** `gpu reserve --cards` reads `nvidia-smi`'s
   per-process rows (Linux only: WDDM lists none), but the media path's default `ForeignBusy` reader returns
   nothing, so a Linux host that turns card-scoped leases on would hand a render a card another process is using.
   The reader lives in the root package beside the foreign-load guard; moving it into `internal/gpualloc` is
   the fix, and it matters on a Linux host only.
5. **"Enqueue on the node daemon's job queue" (the plan's spike): not adopted, and why.** The daemon runs media
   jobs inline in the request handler: `concurrencyCapped` is false for `image-gen`, `video-gen`, `animate`,
   `audio-gen` and `run-graph` (a parked media job would hold an execution slot and starve the agent lane), so a
   dispatch to the local daemon would wait in the same per-card slots and leases this admission already waits in
   and add no queue of its own. The place in line is the token.

## iGPU media engines: video, animate, voice, music (CT-49)

A box whose only GPU is a Vulkan iGPU (no CUDA, no ROCm) serves the same four lanes a ComfyUI box does, through the same MCP tools, CLI verbs and fleet task types: `generate_video` (I2V and T2V), `animate_character`, and `generate_audio` kind `voice` (with clone) and kind `music`. `run_graph` stays ComfyUI-only. Every engine is a spawn-per-job native CLI under the existing media lease: the process exits, the memory is gone, nothing is resident, no ComfyUI and no Python. A box that sets none of the keys below behaves byte for byte as before (`TestEveryRouteWithNoEngineKeyKeepsItsExactArgv` pins the four argv shapes, and passes on the unmodified base too).

| Lane | Selected by | Runner | Engine |
|---|---|---|---|
| `generate_video` | a `videogen_families` entry with `"engine": "sdcpp"` | `render/sdcpp-video.mjs` | stable-diffusion.cpp `sd-cli -M vid_gen` (Wan2.2 TI2V-5B GGUF; also drives a Wan2.2 A14B high/low pair where the box has the memory) |
| `animate_character` | `animategen_engine: "sdcpp"` | `render/sdcpp-animate.mjs` | ffmpeg frames, depth-anything.cpp depth PNGs, sd.cpp Wan2.1 VACE 1.3B with the depth directory as `--control-video` |
| `generate_audio` voice | `voicegen_engine: "audiocpp"` | `render/audiocpp-generate.mjs --kind voice` | audio.cpp `audiocpp_cli`, `chatterbox` family, `--task tts` (`clon` with a clone reference) |
| `generate_audio` music | `musicgen_engine: "audiocpp"` | `render/audiocpp-generate.mjs --kind music` | audio.cpp `audiocpp_cli`, `ace_step` family, `--task gen` |

### No model runs on CPU

The operator rule behind these lanes is that nothing runs on the CPU. It is enforced at four layers, each with a test that was seen red once:

1. **Config load.** `config.CPUBackendRefusal` is an allowlist: a backend must be `vulkan` or `vulkanN` (per-module assignments such as `diffusion=vulkan0,vae=vulkan0` are checked value by value). An empty backend, `cpu`, `cpu0`, `best`, `auto` (the binary's own pick is the CPU on a box whose GPU it cannot open), `blas`, `opencl`, `rpc` and a typo such as `vulcan` are all refused. `config.ExtraArgsRefusal` refuses, in every `*_extra_args` list (`sdcpp_extra_args`, `animategen_sdcpp_extra_args`, `animategen_depth_extra_args`, `audiocpp_extra_args`), any element that changes the backend or placement: `--backend` / `-b` (any value), `--params-backend`, `--clip-on-cpu`, `--vae-on-cpu`, `--control-net-cpu`, `--rpc`, `--device` for audio.cpp (the `audiocpp_device` key owns it), any flag named with `cpu` and any `cpu` backend value, because extras are appended after the runner's own `--backend` and would override it. `--offload-to-cpu` is the one cpu-named flag that is allowed, as sanctioned spill: sd.cpp parks the weights in RAM and stages them to the device step by step, and every compute buffer stays on the GPU (the house rule of 2026-10-07 allows RAM as overflow while it truly adds capability, stays bounded and keeps the box stable). Whether a box should use it is a per-seat measurement, not a screen, and on a UMA iGPU box, where "VRAM" is the same memory as RAM, it only adds copies, so the amd-gcn bindings leave it off. The runners never add it themselves (the sd.cpp VACE docs command line shows it, and the runner does not copy it); it reaches sd-cli only when a binding's extra args carry it, and the log guard in layer 4 still kills a run that shows compute on the CPU. `local-offload doctor` loads the config first, so it fails on all of this by name.
2. **mediacap.** A route whose backend is a CPU one is BOUND-BUT-MISSING (doctor FAIL), whatever the config loader said, so an in-process config cannot slip past `offload_status`.
3. **Pipeline.** The same refusals are typed defers before the media lease is taken and before the runner spawns: `meta.err_class` `cpu_backend_refused` for a backend, `extra_args_refused` for an extra-args element.
4. **The runners read their engine's log, and a run passes only on POSITIVE evidence.** Each runner scans every output line while the engine runs, and a run is accepted only with affirmative proof that the work ran on the GPU, per engine. sd-cli needs a non-software `ggml_vulkan: <n> = <device>` line and a diffusion-stage `<module> compute buffer size: ... on Vulkan<N>` line (not the text encoder or VAE; the auto-fit plan's `DiT ... -> compute Vulkan<N>` line is the same evidence) and no compute buffer `on CPU` and no plan line `-> compute CPU`; the pinned format strings (`model_manager.cpp:490`, `ggml_runner.cpp:1019`) print the CPU backend as `CPU`, and `Vulkan_Host` (pinned host memory) is neither evidence nor a placement, so the params-on-host log that `--offload-to-cpu` produces passes while any compute buffer on the CPU is still `CPU_PLACEMENT`. depth-anything.cpp (`da3-cli`) needs `[da3] da::Backend using device: Vulkan<N>` on every frame. audiocpp_cli needs a `<component>.weights.buffer_name Vulkan<N>` line, and ANY `*.weights.buffer_name CPU` line is a placement: upstream audio.cpp loads a second host copy of the ACE-Step planner for the prompt prefill when the backend is Vulkan, so a stock build fails this guard (the real captured log is `render/testdata/audiocpp-music-host-prefill.log`) and the node needs the planner prefill patch, [`setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch`](../../setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch) (see [Voice and music](#voice-and-music-renderaudiocpp-generatemjs)). A line placing compute on the CPU kills the process tree at once with `CPU_PLACEMENT` (`cpu_placement`), and a run that ends without its positive evidence is `CPU_PLACEMENT` too ("no GPU evidence was seen"), never a pass by silence. Params resting in host RAM with the compute on Vulkan is the sanctioned overflow and does not fail. Never scanned for anything: ggml's `load_backend: loaded CPU backend` registration, `Initializing backend: CPU`, and, for sd.cpp only and only on its own record shapes (below), the `SDCliParams` / `SDContextParams` / `SDGenerationParams` dump blocks and the tokenizer echoes (`split prompt ...`, `parse '...'`). **The request's own text never switches the detector off:** every other line is scanned whole, the placement and evidence shapes are anchored at the start of a record (after sd.cpp's record head, `[VERBOSE] file.cpp:N - ` or `[V] `, or audio.cpp's `[TIMING ts=...]` / `[TRACE ts=...]` head), so a prompt, TTS text or lyric that is a fragment of a real placement line (`MB(RAM) on CPU`, `planner.weights.buffer_name CPU`) neither hides it nor, echoed in the middle of some other line, forges one, and a record that is itself a line of the request (`[V] SDCliParams {` as a prompt line) opens no dump block. A line that echoes the request (the text is most of the line, the text of a multi-line prompt also in the one-line spelling sd.cpp prints it in, `\n` for a newline) can only fail to count as POSITIVE evidence, so text that looks like a device or buffer line is not evidence; a device reset is read with the request text taken out. sd.cpp's auxiliary modules (`t5`, `umt5`, `clip*`, `llm`, `vae`/`wan_vae`/`flux_vae`, `tae*`, `taehv`, `control_net`, `esrgan`, ...) are matched by their whole name (`SD_AUX_MODULE`), so a diffusion model called `Wan2.1-Fun-14B-Control` is the diffusion stage. A software Vulkan device (`llvmpipe`, `lavapipe`, `swiftshader`) and `ggml_vulkan: Found 0 Vulkan devices` count as the CPU. The sd-cli and audiocpp_cli verbose flags (`-v`, `--log`) are therefore never optional in the argv. The guard is `createLogGuard` in `render/igpu-engine.mjs` and is pinned by REAL engine logs in `render/testdata/` (the sd.cpp CPU negatives are derived from them, see its README).

**The guard reads both sd.cpp log formats.** sd.cpp changed its log record after master-929 (#2104, #2106). master-929 prints `[VERBOSE] ggml_runner.cpp:1019 - <message>` (long tags padded to 7, the source in front), master-945 prints `[V] <message> --- ggml_runner.cpp:1019` (one-letter tags `[D]` `[V]` `[I]` `[W]` `[E]`, the source behind the message, a prompt echo on one line with its newlines escaped as `\n`). A record of several lines has its tag on the first line and its source on the last, so a parameter dump closes with `} --- main.cpp:699` where it used to close with a bare `}`. Nodes upgrade at different times (a fleet update moves one node and not the others), so both shapes are read for good, and a build between the two commits, which prints ` - ` in front of the source, is read as well (from the upstream commits; no log of it was captured). The rule is one normalisation, `normalizeSdLine`, applied to every sd.cpp line before a shape anchored at the start of a record looks at it: one leading level tag in either spelling (and, after the long one, the `file.cpp:N - ` source prefix) and one trailing ` --- file.cpp:N` tail are cut, the raw line is kept for the error message, and leading whitespace stays (a dump's indented `  }` is not its closing `}`). A dump block closes on a normalised `}`; the plain-words shapes (a CPU backend, `device lost`, `-> compute CPU`) still read the whole raw line, so the request's text hides none of them. An unterminated block ends at the next long-tag record (the old shape's valve) and, in the new shape, only at its close: a closing line the guard does not recognise skips the rest of the log and the run ends `CPU_PLACEMENT` ("no GPU evidence") instead of being passed on a guess. Before the normalisation a healthy master-945 run failed exactly that way (a false `CPU_PLACEMENT` that would have blocked every iGPU video and animate job on a node the day it upgraded sd.cpp). The auto-fit plan block both releases print is read too: the `DiT` line on `Vulkan<N>` is evidence of the diffusion stage (`params RAM` / `params CPU` alone is the sanctioned spill), and `-> compute CPU` for any component is a placement.

Every runner takes `--timeout-sec`; the pipeline passes its timeout minus a 15 s margin (a quarter of the budget under a minute). The runner's deadline counts from ITS OWN process start (the llama-swap drain and every pre-spawn step spend it); every step asks it before it starts (`deadline.enforce`: the frame extraction, each depth frame, the RGB conversion, sd-cli, the frame count, the mp4 encode, the clip check, the audio finalize and each of its steps: the trailing-silence trim, the duration probes, the master or the re-encode, the dead-air gate) and every ffmpeg / ffprobe call carries what is left as its timeout, so it kills its engine's whole process tree before gpugen's own kill; a call killed at its bound is a `timeout`, never `dead_air` or `unmeasurable`. `render/igpu-deadline.test.mjs` pins each of those sites through a test-only clock preload (`render/testdata/clock-preload.cjs`).

### Runner contract: argv shape, binaries, output directory, typed errors

- **Flags first, then `--`, then the positionals.** The pipeline sends `--sd-bin ... --model ... -- <out> [<still>] "<prompt>"` (animate: `-- <out> <ref> <driver> "<prompt>"`; audio: `-- <out> "<text>"`), and every runner's `parseArgs` treats a bare `--` as the end of flag parsing. A prompt, TTS text or lyrics that starts with `--` (a section marker such as `--- Intro ---`) therefore stays a positional instead of being read as a flag that swallows the next token. A hand run may still put the positionals first when none of them starts with `--`.
- **One binary-resolution rule.** The Go side resolves each bound engine binary (`sdcpp_bin`, `animategen_sdcpp_bin`, `animategen_depth_bin`, `audiocpp_bin`) with `mediaops.ResolveBinary` (an explicit path is stat'd, a bare name is looked up on PATH, which is exactly what `doctor` and `offload_status` report) and passes the ABSOLUTE path; a value that resolves to nothing is a defer naming the key, before the lease. The runners refuse a non-absolute `--sd-bin` / `--depth-bin` / `--bin` with `BINARY_NOT_ABSOLUTE`, so a bare `sd-cli` that `doctor` shows CONFIGURED can no longer pass doctor and then fail every call.
- **The output directory is made up front.** The pipeline creates the directory the result lands in before it takes the lease and reports the `MkdirAll` error as a defer (`cannot create the output directory ...`); the runner repeats the check and fails with `OUT_DIR_UNWRITABLE` before it spawns anything, so a render never ends in "cannot write the output" after minutes of GPU time.
- **Backend values are each engine's own.** sd.cpp (`sdcpp_backend`, `animategen_sdcpp_backend`) takes `vulkan` or `vulkanN` (`vulkan0`). audio.cpp (`audiocpp_backend`) takes `vulkan` only, and the device index is the separate `audiocpp_device` key (`--backend vulkan --device 0`); `vulkan0` is not an audio.cpp value, and config load, mediacap, the pipeline and the runner all say so (with the `audiocpp_device` hint) instead of leaving the CLI to reject it at run time. audio.cpp itself also knows `cuda`, `hip`, `rocm` and `metal`, but they are refused here (`cpu_backend_refused`) until a real captured log of their buffer names and an evidence pattern for them exist: the runner proves a run was on the GPU by `<component>.weights.buffer_name Vulkan<N>` lines, so a binding to another backend would read CONFIGURED and then end every call as `CPU_PLACEMENT` "no GPU evidence" even when it ran on that GPU. A non-numeric `audiocpp_device` is its own error (`DEVICE_INVALID`, `err_class` `device_invalid`), not a backend refusal.
- **Output gates.** An engine that exits 0 is not a finished job: a clip is checked for black and frozen output, audio for dead air (below), and a gate fails CLOSED: the dead-air pass must have EXITED 0 AND printed ebur128's `Summary:` block, which ffmpeg prints only at the very end and from which the integrated loudness is read (every per-tick line also carries an `I:`, so a pass killed after a few ticks would otherwise report a loudness and an empty silence list that reads "no silence"); a pass that exited non-zero, died of a signal or printed no Summary (or a build without `silencedetect` / `ebur128`), and a clip whose length ffmpeg could not read, is `UNMEASURABLE` (`unmeasurable`), never "clean" and never described as "no picture". A frame counts as black when 99.9 % of its pixels are (`blackdetect` `pic_th=0.999`, in `render/igpu-qa.mjs`): the 0.98 default rejected legitimate low-key footage, a dark field with a candle flame or the moon in it (0.75 % to 1.5 % of the frame), as `black_clip`, a full failure that is never retried; a pure black clip still fails, and a clip fails when 95 % of it is black. A result is written to a hidden partial file beside its delivery path and renamed onto it only after the gate passed: a failed, rejected or deadline-killed run leaves no partial and never overwrites (or deletes) a good file already at the same path, which is deterministic when the caller names no `out`.
- **Typed failures survive gpugen.** A runner ends every typed failure with one short machine line, `IGPU_CLASS=<class>`, after its (long) human line. `gpugen.Generate` reads the class from the last such line anywhere in the output it captured (256 KiB), never from the 400-byte display tail: the real `GPU_RESET` line is about 470 bytes with its token at the start, so a tail-only classifier read it as a timeout. The error is a `gpugen.RunError` carrying that class, and its reason keeps the runner's labelled line (`GPU_RESET: ...`). `render/testdata/` logs replayed through the real runners, `Generate` and `ClassifyErr` pin every class (`internal/gpugen/igpu_runner_chain_test.go`).

The typed errors a lane can return (`meta.err_class` in parentheses; the runner prints the class name first and `IGPU_CLASS=` last, `gpugen.ClassifyErr` maps it):

| Error | `err_class` | Meaning |
|---|---|---|
| `CPU_PLACEMENT` | `cpu_placement` | a model ran or would run on the CPU, or a clean exit showed no GPU evidence |
| `CPU_BACKEND_REFUSED` | `cpu_backend_refused` | the backend is not an accepted GPU value for that engine |
| `EXTRA_ARGS_REFUSED` | `extra_args_refused` | an extra-args element changes the backend or placement (not `--offload-to-cpu`, which is sanctioned spill) |
| `TOKEN_CAP_EXCEEDED` | `token_cap_exceeded` | the request is over the configured latent-token cap |
| `GPU_RESET` | `gpu_reset` | the engine log reports a lost device (never retried) |
| `ILLEGAL_INSTRUCTION` | `illegal_instruction` | SIGILL / exit 132: the binary uses CPU instructions this CPU lacks |
| `MODEL_INCOMPATIBLE` | `model_incompatible` | sd-cli refused the model file (`model metadata validation failed`) |
| `DEPTH_FRAMES_INVALID` | `depth_frames_invalid` | a control frame is not 8-bit RGB at exactly W x H |
| `BLACK_CLIP` / `FROZEN_CLIP` | `black_clip` / `frozen_clip` | the finished clip is entirely black / entirely frozen |
| `DEAD_AIR` | `dead_air` | the finished audio is silent or has dead air at an edge |
| `BINARY_NOT_ABSOLUTE` | `binary_not_absolute` | a runner was handed a bare or relative engine path |
| `OUT_DIR_UNWRITABLE` | `out_dir_unwritable` | the output directory cannot be created or written |
| `FFMPEG_UNAVAILABLE` | `ffmpeg_unavailable` | ffmpeg / ffprobe could not be resolved |
| `UNMEASURABLE` | `unmeasurable` | the output could not be measured (the dead-air pass exited non-zero, died of a signal or printed no loudness Summary; no duration for the clip check), so it was not checked and is not delivered |
| `ENGINE_CRASHED` | `engine_crashed` | the engine died of SIGSEGV / SIGABRT / SIGBUS / SIGFPE / SIGTRAP: a crash, not a timeout |
| `OUT_OF_MEMORY` | `oom` | ggml `insufficient memory`, sd.cpp `alloc compute buffer failed`, a Vulkan allocation failure, or SIGKILL on a UMA box (the OOM killer) |
| `DEVICE_INVALID` | `device_invalid` | `audiocpp_device` is not a device index (a configuration error, not a backend refusal) |
| a client cancel | `timeout` | the caller cancelled the run: classified like every other media lane's cancel |

### The GPU timeout envelope: GPU_RESET and the token cap

amdgpu's default `lockup_timeout` is 2000 ms: one GPU dispatch that runs longer makes the kernel reset the compute ring (`ring comp_1.2.0 timeout`, then `device wedged, but recovered through reset`). Fused attention cost per step scales with tokens squared times model width, so the request size is the lever. Measured on the reference node: FastWan2.2 TI2V-5B at 832x480x49 on the 16x Wan2.2 VAE is 5,070 latent tokens and runs; Wan2.1 VACE 1.3B at 480x832x33 plus a reference image on the 8x Wan2.1 VAE is 15,600 tokens and lost the device in the first step; 288x512x33 plus a reference is 5,760 and passes: the full run (20 steps, cfg 6.0, euler, tiled VAE decode) exited 0 with no ring reset in 2,939 s wall (conditioning 21 s, control-video VAE encode 199 s, sampling 2,153 s at about 101 to 108 s per step, decode 352 s). So the envelope numbers to seed are **5,760 tokens at model width 1536 passes, 15,600 resets the ring**, and 5,070 tokens at width 3072 (the TI2V-5B) passes. The cost per step scales with tokens squared times width, so the cap is per model: set it per video family and for animate separately, never one number for both.

- `tokens = ceil(W/(stride*2)) * ceil(H/(stride*2)) * (floor((frames-1)/4) + 1 + refLatentFrames)`, stride = the VAE's spatial downsampling (16 for Wan2.2, 8 for Wan2.1), +1 latent frame for the VACE reference image. `config.LatentTokens` and `latentTokens` in `render/igpu-engine.mjs` are twins pinned to one table, `render/testdata/token-cap-table.json`.
- The cap is configured, never assumed: `sdcpp_max_tokens` and `sdcpp_vae_stride` (8 or 16) on a video family, `animategen_sdcpp_max_tokens` and `animategen_sdcpp_vae_stride` for animate. A cap needs its stride; values must be positive; no cap configured means no check and a box without the keys is unchanged.
- The pipeline computes it on the size the runner will render (after the 4k+1 / multiple-of-32 normalization; a video request that names no frames renders the family's `frames`, an animate request `animategen_frames`, and with neither the runner's 49) BEFORE taking the media lease, and refuses with a typed, non-retryable defer (`err_class` `token_cap_exceeded`) that names the tokens, the cap and how to fit (frames that fit at that size, or lower width/height). The runner checks the same formula again before spawning sd-cli.
- When the log still reports `ErrorDeviceLost`, `device lost` or `context is lost`, the runner kills the engine and fails with `GPU_RESET` (`err_class` `gpu_reset`), which names the 2 s lockup timeout and the cap. It is never retried automatically.

### Killing a run: process groups, signals, signal deaths

- **gpugen.** The iGPU lanes start the runner as the leader of its own process group (`Spec.OwnProcessGroup`, non-Windows; Windows keeps `taskkill /T`). A timeout or a cancel SIGTERMs the whole group so the runner can kill its engine and remove its temp dirs, then SIGKILLs the group after a 5 s grace. Other lanes keep their previous kill exactly.
- **The engine stays in the runner's process group** (it is not spawned detached), so gpugen's SIGKILL of that group, the escalation when a runner cannot answer a SIGTERM, takes the engine along; a detached engine used to survive it and keep the iGPU after the lease was released. The runner walks the engine's descendants itself when it kills the tree. `internal/gpugen/igpu_lifecycle_unix_test.go` runs the real runner, SIGSTOPs it, cancels, and asserts the engine is dead.
- **The runners** kill the engine's process tree and remove their temp dirs on SIGTERM, SIGINT and SIGHUP (exit codes 143, 130, 129), and when their parent process disappears (polled once a second), so a runner whose harness was SIGKILLed does not leave the iGPU held. They exit only AFTER the engine tree is gone (bounded wait of 3 s, shorter than gpugen's 5 s grace; zombie-aware), so the media lease is not released while the engine still tears down its Vulkan context and a queued job cannot start a second engine on the same iGPU. A runner that was SIGKILLed itself cannot clean up: its temp dirs carry an owner marker and the next job's `makeTempDir` sweeps the dirs of dead owners. A typed failure is written out before the runner exits (`failAndExit`): a full pipe queues a write and `process.exit()` dropped it.
- **A client cancel** of an iGPU lane classifies as `timeout` like the cancel of every other media lane (the runner answers the SIGTERM with exit 143, which carries none of the words the wording classifier looks for).
- **Deaths are named.** An engine killed by a signal reports it: a crash signal (SIGSEGV, SIGABRT, SIGBUS, SIGFPE, SIGTRAP) is `ENGINE_CRASHED` (`engine_crashed`), never a timeout through the word "killed"; ggml's `insufficient memory (attempted to allocate N MB)` and sd.cpp's `alloc compute buffer failed` in the log make the failure `OUT_OF_MEMORY` (`oom`) whether the engine then exits 1 or aborts; SIGKILL on a UMA iGPU box usually means the kernel OOM killer and classifies as `oom` too; and exit 132 / SIGILL is `ILLEGAL_INSTRUCTION` (`illegal_instruction`): an instruction-set mismatch. The official audio.cpp Ubuntu release binary is built with AVX-512 and dies this way on a CPU without it (it only shows once a model runs; `--list-devices` still works), so build audiocpp_cli on the node from the release tag (`scripts/build_linux.sh --backend vulkan --model-set custom --models chatterbox,ace_step --target audiocpp_cli`) instead of using the release tarball.

### Video (`render/sdcpp-video.mjs`)

```json
"videogen_family": "fastwan",
"videogen_families": {
  "fastwan": {
    "engine": "sdcpp",
    "sdcpp_bin": "/path/to/sd-cli",
    "sdcpp_model": "/path/to/Wan2.2-TI2V-5B-Q8_0.gguf",
    "sdcpp_vae": "/path/to/wan2.2_vae.safetensors",
    "sdcpp_t5xxl": "/path/to/umt5-xxl-encoder-Q8_0.gguf",
    "sdcpp_backend": "vulkan0",
    "sdcpp_extra_args": [],
    "steps": 3, "cfg": 1, "flow_shift": 3, "sampler": "euler",
    "fps": 24, "width": 832, "height": 480, "frames": 49,
    "license": "Apache-2.0", "commercial_use": true
  }
}
```

An sdcpp family may carry any name, including one a ComfyUI family also uses (`wan22`, `ltx25`, `hunyuan`, `h3`): `resolveVideoFamily` matches it by its own name, exactly, its entry wins wholesale (the flat `videogen_*` ComfyUI weight keys never reach sd-cli), and `doctor`, `offload_status` and the pipeline agree on which family a request that names no model resolves to (`config.DefaultVideoSdcppFamily`; an unset `videogen_family` means `wan22`), with exactly one `VideoFamilyBindingRows` row per family. A request's `model` param selects it on a box whose default is ComfyUI. `sdcpp_high_noise_model` adds the Wan2.2 A14B high-noise expert, and `high_noise_cfg`, `high_noise_steps` and `high_noise_sampler` are that expert's own recipe (sd-cli `--high-noise-cfg-scale` / `--high-noise-steps` / `--high-noise-sampling-method`; its default high-noise cfg is 7.0, which doubles the iGPU time of a distilled recipe, so a pair always sets it); they need the high-noise model. Non-default sdcpp families get their own `generate_video:<name>` route in `doctor` and `offload_status`.

**Tiny autoencoder (opt-in).** `sdcpp_tae` binds a tiny autoencoder, and a request with `fast: true` decodes with it (`--taesd`; sd.cpp then also uses it for the I2V encode, log line `using TAE for encoding / decoding`). The verified file is the Apache-2.0 `taew2_2.safetensors` from lightx2v/Autoencoders: 17 frames decode in 2.79 s against 299 s for the tiled full VAE, at SSIM 0.950 against the full VAE with one faint sky smudge. **`lighttaew2_2.safetensors` is broken in this sd.cpp** (frames 8 and 16 decode a different scene, SSIM 0.72): never bind it. The full VAE stays the default. Without the key, `fast` is a no-op on the sdcpp lane and the result's `notes` array says so (a bound but missing file says that instead, and the full VAE decodes); `doctor` lists the TAE file as optional and it never fails the route.

Argv contract (`sdcpp-video.mjs --sd-bin ABS --model ... --vae ... --t5xxl ... --backend ... -- <out.mp4> [<still>] "<prompt>"`; optional `--high-noise-model --high-noise-cfg --high-noise-steps --high-noise-sampler --tae --vae-tile-overlap --frames --width --height --fps --steps --cfg --flow-shift --sampler --seed --negative --max-tokens --vae-stride --timeout-sec --extra-args <json array>`) maps to sd-cli master-929 as `-M vid_gen --diffusion-model [--high-noise-diffusion-model] --vae --t5xxl [-i <still>] -p [-n] [--cfg-scale] [--steps] [--sampling-method] [--flow-shift] [--high-noise-cfg-scale] [--high-noise-steps] [--high-noise-sampling-method] -W -H --video-frames --fps [-s] --backend --diffusion-fa --vae-tiling --vae-tile-overlap 0.25 [--taesd] -v -o <tmp>.webm`, then ffmpeg encodes the webm to H.264 mp4 (yuv420p, CRF 16, the given fps; `FFMPEG_PATH` or PATH). The tile overlap defaults to the measured best decode (17 frames: overlap 0.5 took 364 s, 0.25 took 299 s, 0.25 with `--vae-conv-direct` 424 s, untiled 355 s; `--vae-conv-direct` stays unused on Vulkan). The finished clip is then read back as a clip: ffmpeg `blackdetect` and `freezedetect` over the whole file, and one that is entirely black or entirely frozen (95% or more of its length) fails `BLACK_CLIP` / `FROZEN_CLIP` and is removed, because a Vulkan fp16 or NaN failure exits 0 with no picture in it; a fade or a held frame passes. Frames are normalized to the nearest 4k+1 (ties up, minimum 5, default 49) and width and height are floored to a multiple of 32 (default 832x480); the Go side applies the same rule before it spawns, so the argv the runner receives is the argv sd-cli gets. The result is `{video_path, seed}` plus the family's license pair, like the ComfyUI route.

Measured on the reference iGPU node (Vega 7, UMA, Zen 3 without AVX-512), FastWan TI2V-5B q8_0, 832x480x49, 3 steps, cfg 1: conditioning 15 s, sampling 232 s, full-VAE decode 1,075 s (the TAE above is the opt-in way to cut that).

**The measured sd.cpp release is master-945 (`a1ded76`).** On the measured FastWan run its decoded frames are bit-identical to master-929's (`3f8527a`, which the figures above and the argv below come from) and it runs about 10% faster. master-929 keeps working: the log guard reads both releases' log formats (above), and the fixtures of both stay in `render/testdata/`. Its real healthy logs (`sdcpp945-video-healthy.log`, `sdcpp945-video-tae.log`, `sdcpp945-vace-healthy.log`) pass the guard, and the video log and the VACE log are replayed once each through the real video and animate runners (`render/igpu-runners-main.test.mjs`).

### Animate (`render/sdcpp-animate.mjs`)

Config: `animategen_engine`, `animategen_sdcpp_bin/_model/_vae/_t5xxl/_backend/_tae/_extra_args/_max_tokens/_vae_stride`, `animategen_depth_bin`, `animategen_depth_model`, `animategen_depth_extra_args`, `animategen_steps`, `animategen_cfg`, `animategen_flow_shift`, `animategen_frames` (plus the shared `animategen_width/_height`). `animategen_sdcpp_script` overrides the runner path. `animategen_frames` is the clip length a request that names no `frames` renders (0 = the runner's own 49; a request's `frames` always wins; the ComfyUI animate route ignores the key). It is sized with the geometry and the token cap, because the cap is checked on the frame count the lane will actually render: a box whose cap fits only a short clip names that clip here (the amd-gcn seed: `animategen_frames` 33 at 288x512 is 5,760 tokens against a cap of 5,800, where the runner's own 49 would be 8,064 and every call that omitted `frames` would be refused `token_cap_exceeded`). `animategen_sdcpp_tae` is the same opt-in tiny autoencoder as the video lane (`fast: true`, with the same notes when it is not bound).

**The model must be a `.safetensors` VACE checkpoint.** The two public Wan2.1 VACE 1.3B GGUFs lack `vace_patch_embedding.weight` (1,263 tensors against 1,264) and sd-cli refuses them (`Diffusion model tensor 'model.diffusion_model.vace_patch_embedding.weight' not in model metadata`, `model metadata validation failed`); the Comfy-Org `wan2.1_vace_1.3B_fp16.safetensors` loads. The runner surfaces that refusal as `MODEL_INCOMPATIBLE` naming the model file and the missing tensor, and `doctor` notes a `.gguf` animate model on the route.

The runner extracts the driver's frames with ffmpeg (16 fps, scaled to cover W x H and centre-cropped, the first N = 4k+1 frames; a driver shorter than the request renders the largest 4k+1 it has, and fewer than 5 frames is an error), runs depth-anything.cpp once per frame, and renders `sd-cli -M vid_gen --diffusion-model <vace gguf> --vae --t5xxl -i <ref> --control-video <depth dir> ...`. The frames, depth and output temp directories are removed when the runner finishes, fails, times out, receives SIGTERM/SIGINT/SIGHUP, or finds its parent process gone; a SIGKILL of the runner itself cannot run any cleanup, which is why gpugen signals the runner's whole process group (below) rather than killing node alone.

**The depth step (verified on the node, depth-anything.cpp `14f7461` built with `DA_GGML_VULKAN`).** The argv is `da3-cli depth --model M --input <one frame> --png <out.png> --no-invert`, once per frame. `--input a --input b` is multi-view joint depth, not per-frame, and is never used. `--no-invert` is required: the default writes near=dark and far=bright, `--no-invert` writes near=bright and far=dark, which is the depth-control convention. The CLI has no backend flag (the backends are compile-time), so the step is pinned to the sd backend's Vulkan device through `GGML_VK_VISIBLE_DEVICES` and its log is the evidence it ran on the GPU: `[da3] da::Backend using device: Vulkan<N>` is required on every frame (the first frame is the probe, so a CPU-only build stops after one frame instead of running all N on the CPU), and `offload_weights: ... (N host-only tensors kept on CPU ...)` is a storage line, not a placement. The model is reloaded per frame (about 1.5 s each; 33 frames took 51 s).

**RGB control frames.** `da3-cli` writes a 1-channel PNG at the model's own working size (518x896 for a 288x512 or 480x832 input) and sd-cli refuses 1-channel PNGs (`the number of channels for the input image must be >= 3, but got 1 channels`). So the runner converts every depth PNG to `rgb24` at exactly W x H with ffmpeg into the directory sd-cli reads, then reads every header back (8-bit, colour type 2, W x H, and exactly N files, else `DEPTH_FRAMES_INVALID`).

**The VACE argv** is sd.cpp `docs/wan.md` "V2V with Wan2.1 VACE" at `3f8527a`: `-M vid_gen --diffusion-model <vace> --vae <wan_2.1_vae> --t5xxl <umt5> -i <reference image> --control-video <dir of frames> -W -H --video-frames N --cfg-scale 6.0 --sampling-method euler`. `-i` is the VACE reference image (`-r` is for Flux Kontext and MiniMax-H3 only). That doc page also passes `--offload-to-cpu`; the runner never adds it (on a UMA iGPU it only adds copies); it reaches sd-cli only when a binding's extra args carry it, which the screen allows as sanctioned spill (weights in RAM, compute on the GPU).

**Frame count.** With a reference image sd.cpp samples N+4 frames (the reference occupies one latent frame; `generate_video 480x832x37` for 33 requested is the sample size). The measured build trims that itself and decodes exactly N (`decoded 288x512x33`, ffprobe `nb_read_frames` 33), so the runner probes the decoded count with ffprobe and drops the first 4 frames only if it finds N+4; N keeps everything, and any other count keeps everything and says so. The mp4 therefore always carries N frames. The same black / frozen clip gate as the video lane runs last.

**Quality, honestly.** Measured at 288x512x33 on the reference node (preview-grade): motion, pose and layout follow the driver closely (depth-guided); the reference identity transfers partially (face, beard and clay style yes, outfit no).

### Voice and music (`render/audiocpp-generate.mjs`)

Config: `voicegen_engine`, `musicgen_engine`, `audiocpp_bin`, `audiocpp_backend` (`vulkan`, not `vulkan0`), `audiocpp_device` (an index, default 0), `audiocpp_voice_family` (default `chatterbox`), `audiocpp_voice_model`, `audiocpp_music_family` (default `ace_step`), `audiocpp_music_model`, `audiocpp_extra_args`; `audiocpp_script` overrides the runner path. A model is a GGUF file or a model package directory.

**Build audiocpp_cli on the node.** The official `audio-v0.9.0-bin-ubuntu-x64-vulkan` release binary contains thousands of AVX-512 instructions and dies with SIGILL (exit 132, reported as `ILLEGAL_INSTRUCTION`) on any CPU without AVX-512, as soon as a model runs; `--list-devices` still works, which hides it. On such a CPU (a Zen 3 box is one) build from the release tag with native CPU optimisation on (the default): `scripts/build_linux.sh --backend vulkan --model-set custom --models chatterbox,ace_step --target audiocpp_cli`. **The measured release is audio.cpp v0.9.1** (the Chatterbox S3 encoder fix, upstream #778), built natively like that with the patch below; v0.9.0, the release the figures below were first measured on, works the same. The guard needed no change for v0.9.1: its component `buffer_name Vulkan0` lines are the ones v0.9.0 prints (`render/testdata/audiocpp091-voice-clone.log` and `audiocpp091-music.log` are the real logs, and a future log-format change in audio.cpp will fail on them).

**The ACE-Step planner prefill patch (required for music).** Upstream v0.9.0 runs the ACE-Step planner's prompt prefill on the host when the backend is Vulkan (`planner_prefill_uses_host_backend()` returns true), so it loads a second copy of the planner LM on the CPU (`ace_step.planner.weights.buffer_name Vulkan0`, then ten seconds later `... CPU`; the captured log is `render/testdata/audiocpp-music-host-prefill.log`). That is a CPU placement, and the guard rightly fails a stock build. [`setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch`](../../setup/patches/audiocpp-v0.9.0-vulkan-planner-prefill.patch) (version 3, three changes in `src/models/ace_step/planner.cpp`) moves it onto the device: (1) `planner_prefill_uses_host_backend()` is false for Vulkan, so no host copy is loaded; (2) the phase prefill (`use_metal_prompt_step_prefill`) and (3) the code phase's classifier-free-guidance prefill (`use_metal_prompt_step_cfg_prefill`) run through the decode graph one token at a time on the device, the path upstream already uses on Metal. All three are needed: version 1 of the patch (change 1 alone) sent the prefill through the BATCHED prefill graph on Vulkan, and that path is corrupt on this backend (the chain-of-thought caption degenerated into garbage), so it was rejected. Measured with the bf16 package, seed 7, same prompt: the plan text is identical to the stock host-prefill build, zero `buffer_name CPU` lines, 30 s of audio in 149 to 165 s (stock 158 to 183 s); with greedy planner decoding the first 44 of 150 audio codes match the stock build and 81 of 150 overall (one near-tie flip under different float arithmetic, then autoregressive drift: the same cross-backend effect upstream reports for a quantised planner). Apply it to the v0.9.0 or v0.9.1 tree before building (`patch -p1 < audiocpp-v0.9.0-vulkan-planner-prefill.patch`): it applies to v0.9.1 unchanged, so the file keeps its v0.9.0 name, and the real v0.9.1 music log built with it has the planner on `Vulkan0` and no `buffer_name CPU` line (`render/testdata/audiocpp091-music.log`; a stock v0.9.1 build was not run, so the patch stays required); a later upstream that fixes the Vulkan prefill makes it unnecessary.

**The ACE-Step package is bf16.** Upstream grades `ace_step` q8_0 "No (planner sampling can fail)": a quantised planner samples a different token path. Bind `ace-step-1.5-turbo-bf16.gguf` (package `ace_step_turbo_bf16`, 10.09 GB), not a q8_0 file.

Argv contract: `--task tts|clon|gen --family F --model M --backend B --device N --text T [--language L] [--voice-ref R] [--lyrics L] [--duration-seconds S] [--seed N] --metrics --log --out <wav>`. Voice uses `clon` plus `--voice-ref` when a clone reference is given (the request's `clone`, else `voicegen_ref`); the language defaults to `es` like the Chatterbox worker. Output is a `.wav`. The runner finalizes the engine's file and every ffmpeg step must succeed, or the job fails with the step's stderr tail (never a silent copy of the raw engine file, and a non-wav extension is re-encoded, never renamed); ffmpeg AND ffprobe are required up front (`FFMPEG_UNAVAILABLE` before the lease), because audio nobody can measure is not a usable result. **Music**: trailing silence below -45 dB is trimmed (a measured ACE-Step piece ended 3.6 s before a 30 s request and padded the rest with silence), a short fade-out is applied (a tenth of the clip, at most 1 s), the result is loudness-normalized to -14 LUFS / -1 dBTP and delivered at 48 kHz. **Voice** is the engine's file as it wrote it (re-encoded only for a non-wav extension). Both kinds then pass the repo's dead-air gate (`render/audio-qa.mjs`, the way `comfy-music.mjs` applies it) on the file as delivered: a silent render (a Vulkan fp16 or NaN failure writes zeros and exits 0), or leading or trailing silence over 1 s, or more than 10% silence, fails `DEAD_AIR` and the file is removed. Measured on the reference node (v0.9.0): voice clone EN 7.36 s of audio in 30.2 s, ES 8.44 s in 34.8 s (RTF about 4.1); music 30 s in 108 s (RTF 3.6) before the patch. v0.9.1 with the patch: voice clone EN 7.36 s in 25.9 s (RTF 3.5), music 30 s in 143 s (RTF 4.8). `voice=finetuned` keeps the Chatterbox python worker, `voice=endpoint` keeps the speech server, and a configured audiocpp voice engine is a local voice, so the endpoint is not the default on such a box.

### Verdicts

`doctor` and `offload_status` show each engine route as CONFIGURED only when the runner, the engine binaries and every bound model file exist, and ffmpeg AND ffprobe (the black / frozen clip gate and the VACE frame count need ffprobe; the audio gates need both); an engine with nothing bound is NOT CONFIGURED, and a bound file that is missing, a half-bound engine, a backend that is not that engine's own GPU value (audio.cpp's enum for the audio routes) or a CPU backend is BOUND-BUT-MISSING. The optional TAE file is listed and never fails a route. Voice and music are independent: a box with only `voicegen_engine: audiocpp` keeps the ComfyUI verdict for music. These routes never claim ComfyUI as a prerequisite.

The ComfyUI video route's own file check reads the binding the pipeline renders with (`config.ResolveVideoFamilyBinding` of the canonical render family), not the flat `videogen_*` keys, so a box whose `videogen_family` is spelled `wan` (the runner's word for Wan 2.2) is checked against its `videogen_families["wan22"]` entry (weights, loader and pool keys) instead of files the render never loads. `local-offload fleet-measure` probes the video and music lanes through `config.VideoGenBound()` and `MusicGenBound()` (a ComfyUI script or the CT-49 engine), the same helpers `AnimateGenBound()` and `VoiceGenBound()` complete for the fleet advertisement, so an sdcpp or audio.cpp box is no longer told "skipped (no videogen_script configured)".

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
- [`render/igpu-engine.mjs`](../../render/igpu-engine.mjs) — the iGPU runners' shared plumbing: the
  backend refusal, `detectCpuPlacement`, `runEngine` (kill on the first CPU-placement line), the 4k+1
  and /32 normalization; [`render/sdcpp-video.mjs`](../../render/sdcpp-video.mjs),
  [`render/sdcpp-animate.mjs`](../../render/sdcpp-animate.mjs) and
  [`render/audiocpp-generate.mjs`](../../render/audiocpp-generate.mjs) are the three runners
- [`internal/pipeline/igpumedia.go`](../../internal/pipeline/igpumedia.go),
  [`internal/config/igpumedia.go`](../../internal/config/igpumedia.go) and
  [`internal/mediacap/igpumedia.go`](../../internal/mediacap/igpumedia.go) — the iGPU lanes' routing,
  config contract (with the CPU refusal) and route verdicts
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

## Remote routing and the media-job door (ADR 0077)

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

**Which node.** The candidates are read through `internal/rosterprobe` like every other single-shot lane (ADR 0074): each
`delegate_remotes` entry is first judged by the tailnet shape check the agent lane applies (`netguard.TailnetURL`), and one it
refuses is a named miss in the defer (`<base>: not dialled, refused by the tailnet guard (<why>)`), never a dial, while the
other nodes still serve; the rest are probed at once, in configured order, through the shared memo and negative cache. The
client reads each candidate's `/fleet/health` and keeps the nodes that list the task (and `media-job`
when input files travel), report every route the task needs as CONFIGURED when they report routes at all (a node that
predates `media_routes` is unknown, not refused), and do not hold a TEXT lease (they would answer 503). Among those, a node
with no held lease ranks first, then the shorter queue (queued plus running), then config order. Every miss is named in the
defer, with `defer_class` `capacity`.

**What travels.** A job with no input file goes through `POST /fleet/dispatch`. A job with a still (`offload_generate_video`),
a reference and driver (`offload_animate_character`) or a clone sample (`offload_generate_audio`) packs the files into a
bundle (each under its field name plus its extension, copied into a temp directory and packed with `composebundle`) and goes
through `POST /fleet/media-job` ([fleet-node.md](fleet-node.md#the-media-job-door-artifacts-and-honest-advertisement-adr-0077)).
`run-graph` carries its graph and manifest inline, so it never needs the door. The payload uses the field names the node's
builders decode; `out` and `out_dir` never travel (`out_dir` is where the fetched outputs land here). Four request fields cannot ride the fleet task and defer by name
(`defer_class` `contract`) instead of being dropped: `refine=false`, `tts_voice`, `transformer` and (`run_graph`) `devices`, whose card ids name cards on the calling machine.

**Node-side bounds.** A node holds at most one media-job body in flight (a body is about 0.58 GiB in memory at the 256 MiB
default cap, the base64 text beside the decoded bundle): a caller over it waits for the slot and, past 30 s, is answered `503`
with `Retry-After`. The client reads that as a `capacity` defer but does not read `Retry-After` and makes one pass over the node it
picked, so the call returns the defer and a later call places the job again.
The node also names a media-job's render itself (`mediajob-<16 hex>.<ext>`) and serves it only to a holder of the fleet token,
because it is rendered from the caller's private files: the client sends the bearer on every output fetch, a tokenless read of
that name is refused, and the outputs of a job with no input file (the tokenless dispatch) are still read by bare name.

**What comes back.** The client polls `/fleet/jobs/{id}` every 2 seconds inside a budget when the caller gave no deadline
(image 2 h, video and animate 6 h, audio 1 h, run-graph 2 h), then fetches every output the result names by bare name from
`/fleet/media`. Nothing lands until every file is downloaded and its sha256 equals the one the node published in
`artifacts`: a mismatch deletes what was fetched, leaves any file already at `out` untouched, and defers as
`infrastructure`. The primary output goes to the caller's `out` when given, the rest into this machine's `media_dir` (or, for
`run_graph`, the caller's `out_dir`, created if missing); no fetched file ever replaces one that already exists except the
caller's own `out`: each output is staged under a unique temp name, the primary takes the node's file name when it is free,
every other output is prefixed with the remote job id (the caller's `out` is decided first, so no secondary can take its
name), and a failure part-way removes every temp and names the files that already landed. Fetched files are mode 0644 less
the umask (an `out` that already exists keeps that file's permission bits), and an empty `media_dir` means the current directory.
Before it claims a name the client sweeps leftovers of a fetch that never finished from the destination directory:
`.media-fetch-*.part` temps and zero-byte `media-<16 hex>-*` claims older than the longest call budget plus an hour (so a call
that is still running never loses its files; each call also refreshes the modification time of its own claims and finished temps
after every download). Two limits are deliberate: a crash mid-fetch can leave an empty claim under the node's bare file name
(the primary output), which the sweep does not remove because it cannot tell it from an empty file the user made, and an empty
`media-<16 hex>-*` file older than the threshold is indistinguishable from a claim and is removed. The
result's paths are rewritten to the local copies and it gains `node`, `remote_job_id` and, when the node published no
artifacts (an older node), `unverified: true`. `meta.node` names the node and `meta.placement` reads `remote: forced` or
`remote: no <lane> lane on this machine`. A node's 503 or 429 is a `capacity` defer, a 400 or 413 a `contract` defer, a 401
or 403 (on the dispatch, the poll or the fetch) a `config` defer, a call whose own deadline passed a `budget` defer naming the
node and the remote job (while the job was sent or rendering the node may still be running it and the defer says
it cannot be recalled: a media job cannot be withdrawn, because it is claimed to running as soon as it is admitted and the
node's withdraw, `DELETE /fleet/jobs/{id}`, is for agent jobs only (ADR 0064), so the client sends none; a deadline that
passes while the outputs are fetched says the render finished and the fetch ran out of time), and a transport failure an `infrastructure` defer. An input file this
machine cannot read is `contract`; this machine's own temp directory, disk or packer failing is `infrastructure`. A defer the node itself returned (a render that
deferred) comes back as the node sent it, with `meta.node`.

**Attribution.** A call that goes to a node is the remote lane's own, like compose, vision, text and transcription (0.165.0,
D5-D11): it writes one asker ledger row (`node`, `node_id`, `route`, `placement`, `fleet_job_id`, `card_by_caller`) and, once a
node is chosen, one PAIR card on that node (queued, running, terminal), and the handle is closed on every way the call can end
(a result, a refusal, a node defer, a deadline). A call that reached no node has its row and no card. Both POSTs, the plain
dispatch and the media-job, carry `X-Offload-Asker` and, only when this machine's emitter is off, `X-Offload-Pair-Card: node`.
The local route, and an auto call that runs here, are not attributed (the pipeline writes that row).

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
