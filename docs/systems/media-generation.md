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
- How does a film-size Wan 2.2 clip run on two cards with no host RAM, and what does the runner refuse?

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

**A running helper tells its lease it is alive (F8).** Under `gpu reserve -- node render/comfy-*.mjs ...` (or a `.cmd`/`.sh` recipe that runs one, which the
wrapper reads) the wrapper declares a progress contract on the lease and hands the file to the helper in `GPU_LEASE_PROGRESS_FILE`; the helper
(`render/lease-progress.mjs`) appends a line to it at most every 30 s while ComfyUI answers and still lists its prompt
(`pollOutputs`, so every runner that polls ComfyUI), between the pictures of a `--batch`, and when the slot is ready.
That is what lets a long render's lease renew at the end of its term instead of being labelled "expired ... open to a
takeover" while it is healthy, and what makes a helper that hangs or a prompt the server lost read `held-stalled`. With
no file declared (a call from the harness's own doors, a `--no-lock` run) the heartbeat does nothing. The contract, the
stall window and what a beat does and does not prove are in [GPU lease](gpu-lease.md), "A wrapped render helper declares
its own contract and keeps it".

Two details of the free step are easy to get wrong:

- **It frees per model, not everything.** The always-loaded embedding and reranker models are
  small (a few hundred MiB each; the larger EmbeddingGemma-2 entry on the tiers that carry it, 1,196 to 1,536 MiB with its projector and 460 to 482 MiB text-only) and, on the reference box, pinned to the utility card, not the render card. An earlier unload-all implementation tore that memory stack down
  on every generation job for no VRAM benefit; the keep-set now protects it. Under a lease on some cards it also leaves the
  models positively placed on the other cards loaded (F15, by `layers` or by the serving config's device pin), so a render on one
  card leaves the seats placed on the others; a model nothing places is unloaded.
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

**A dead ComfyUI is a typed class on the Go side, and the two calls that had no bound have one (D-38, issue #72).**
- **The class.** On a *server unusable* failure `comfy-render.mjs` prints the usual `RENDER FAILED: <why>` line and, on a line of its
  own after it, `IGPU_CLASS=comfy_unusable` (the form `gpugen.Generate` reads from the retained output of any child, the same
  marker the iGPU engines print), then exits 3. `comfy-generate.mjs` relays its child's stderr unchanged
  (`relayRenderChild` in `batch-jobs.mjs`), so the line reaches the output the Go side reads. `Generate` returns a `RunError` of class
  `comfy_unusable`, `pipeline.classifyErr` now honours a typed `RunError` class like `gpugen.ClassifyErr` does (it used to re-guess
  from the words of the message), and the ledger row carries `err_class` `comfy_unusable` (reason code `infrastructure`) instead of
  `other`. An image `--batch` that meets a dead ComfyUI stops (C-83) and writes a row for every job: the one it stopped on
  (`comfy-render exited 3: ...`) and `not run: ComfyUI became unusable at job i/N (...)` for each later one. Those words name no class,
  so `batchErrClass` files the not-run rows (and the exit-3 row) with the batch's own typed class; a job that failed on its own
  before the stop keeps the class its words give (`oom`, `timeout`). Only the image runner (`comfy-render.mjs`, through `comfy-generate.mjs`) prints the line today; the video, animate, music,
  edit, inpaint, upscale and run-graph runners raise the same *server unusable* error but do not print it yet, so their failures
  still read from the words of their message.
- **The bounds.** `POST /prompt` and `GET /view` passed no `AbortSignal` (the `/system_stats`, `/queue` and `/history` calls always
  did), so a ComfyUI that accepts the connection and never answers held the exclusive GPU slot. `rawSubmit` is bounded by
  `SUBMIT_TIMEOUT_MS` (120 s: the call validates and queues, it does not run) and `fetchView` by `VIEW_TIMEOUT_MS` (600 s), which
  covers the **whole** request, the body read included (ComfyUI is on loopback, so 600 s is a ceiling for a multi-GB video, not an
  estimate of its transfer). Both take a `timeoutMs` a test can shorten; a timed-out call rejects with
  `ComfyUI POST /prompt timeout: no complete answer within <N> ms ...` (or `GET /view`). A timed-out `POST /prompt` is also *server
  unusable* (a server that does not answer it can run no later job): the render exits 3 with the class line, the Go side files
  `comfy_unusable`, and a `--batch` stops instead of paying the 120 s on every remaining job. A timed-out `GET /view` is not: the render
  has finished and its output is on disk, so it stays a plain failure and the Go side classes it `timeout`.

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

The Go media ops (`internal/mediaops`, behind `offload_media` and `offload_edit_image`) follow the same
rule (`deliver.go`). ffmpeg (trim, concat, convert, mux_audio) used to be handed the output path with
`-y`, which truncates it before the first byte: a clip that ran out of disk, or was killed at its
timeout, was left at the output path half written. It is now pointed at a hidden staged sibling
(`.<name>.partial-<pid>-<n>.<ext>`, the extension last because ffmpeg picks its muxer from it) and the
sibling is renamed over the output only after an exit 0 with a non-empty file; any other ending removes
it and leaves a previous good file untouched, with the same short retry of a rename that an antivirus
scanner holds up. `extract_frames` extracts into a staging directory inside the destination and moves
the frames in only when the whole run succeeded (the moves are renames within one volume, each atomic).
An output that is also an input is refused by name, because ffmpeg's own in-place refusal can no longer
fire on a staged name. GIMP never writes the destination: it exports to a private temp raster and the
PIL worker, which delivers through `atomic_out.py`, makes the output. `internal/mediaops/writes_test.go`
lists every write left in the package, and `deliver_test.go` runs the real ops against a fake engine.

**Warm batch.** `generate-image --batch` takes a jobs file and runs N renders in one session. The
only behavioral change is omitting ComfyUI's `--cache-none`, so the checkpoint loads once; teardown
still happens exactly once, at the batch boundary. A failed render is recorded and the batch
continues, one JSONL result line per job; the render script's results file format did not change. The verb
adds `--family`, `--route`, `--results`, `--resume` and `--jobs`, and sends the jobs through the fleet when this machine's
lane is busy and a node renders the recipe: see "The image batch across the fleet" below.

Exit codes of a `--batch` run (0.178.0 added the 4; until then a batch with failed jobs exited 0 and
a caller had to grep the log for `RENDER FAILED`):

| Entry point | `0` | `4` | `1` | `2` |
|---|---|---|---|---|
| `render/comfy-generate.mjs --batch` | every job rendered | the batch ran every job and at least one failed: the rows with `"ok":false` in the results file name them, and the last log line gives the counts | the batch could not run to the end: a setup error, an unusable ComfyUI, a full disk (the jobs not run get a `not run:` row), the GPU slot | usage, or no jobs |
| `render/comfy-inpaint.mjs --batch` | every job rendered | the batch ran every job and at least one failed but not all | stopped: an unusable ComfyUI, a full disk, the consecutive-failure limit, or every job failing (its `_row: "aborted"` line, or the "systemic" error) | usage, or a job that would fail before rendering |
| `local-offload generate-image --batch` | every job has its picture (rendered now, or skipped by `--resume` because the file proved itself), or the warm batch was deferred because a card was busy (`err_class: gpu_busy`; the jobs are `queued`, so a 0 does not by itself say every picture is there: read the `verdict`s) | some job failed (the batch ran every job, or was deferred after a job had failed before any lane was asked): the JSON and the `--results` file keep reporting per-job status (`failed`, `items[].ok`, `verdict`) and the stderr line gives the counts | the batch stopped (the runner's exit 1), was interrupted (the jobs that had not ended are `queued`), a one-at-a-time job found no place within the queue's ceiling (`queued`), or the command failed before any job (a bad jobs file, an empty prompt, two different pictures for one `out`, an unknown family or route, an unwritable `--results`) | an unknown verb or a flag error |

4 is not 3 on purpose: 3 is `comfy-render.mjs`'s own *server unusable* code for its parent, and
`renderExitError` reads it as a verdict. `imagegen.BatchExitJobsFailed` is the Go copy of the runner's
`BATCH_EXIT_JOBS_FAILED` (pinned to `render/batch-jobs.mjs` by a test): `GenerateBatch` treats 4 with a
non-empty results file as a finished batch and reads the rows, so the Go door reports 35 good pictures
out of 36 as a batch with one failed item, then ends with the same 4 itself. No other Go entry point
runs a media batch (`RunImageBatchAs`, and `RunImageBatch` which is it with no family, are called by the CLI verb only).

Two failures stop the batch instead of being recorded and passed over, because every later job would
fail the same way. A server that became unusable (the child exits 3): the failed job and every later
job get a row, the later ones with an `error` that starts `not run: ComfyUI became unusable at job
N/M`, and the batch exits non-zero and its teardown frees the card and the lease, instead of failing
every remaining job against the same server (C-83: 3 min each, after a 48-minute wait on the first). A
full disk (0.178.0: `ENOSPC`, `EDQUOT` or `EROFS`, read from the errno where the runner renders
in-process and from the errno token in the child's `RENDER FAILED:` line or in ComfyUI's own `[Errno
28] No space left on device` otherwise; the name or number counts, never prose, because an exec error
echoes the prompt and a prompt that mentions a full disk must not stop a batch): the same stop, with
`error` starting `not run: the disk is full at job N/M, writing <out>`, exit 1 and the error line
naming the path. It is deliberately batch-wide: the output directories of one batch are normally one
volume. The items such a stop took are ledgered with `err_class: disk_full` (they were `other`). A
failed job's `error` carries the child's own `RENDER FAILED:` reason rather than only `comfy-render
exited N`. The inpaint batch (`comfy-inpaint.mjs --batch`) stops the same way on an unusable server
and on a full disk (its `_row: "aborted"` line carries `reason`: `server_unusable`, `disk_full` or
`consecutive_failures`, with the count of jobs not attempted), and still stops after
`COMFY_BATCH_MAX_CONSEC_FAIL` (3) consecutive failures. **The default single-render path is
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
A bare `--` ends the flags on every one of these verbs (CT-52): `generate-audio out.wav -- "--- Intro ---"` keeps a text that starts with
a dash, and so do `generate-video`, `animate-character` and the verbs that take one file or text (a lone `-` is still stdin).

The Chatterbox voice worker has a CLI of its own, `node render/tts.mjs <out.wav> "<text>" [flags]` (the script `voicegen_script` names;
the Go voice lane builds this command line). The six sampling flags `--temperature --cfg-weight --exaggeration --repetition-penalty
--min-p --top-p` reach `generate()` of the default (generalist) engine under the model's own keyword names (0.185.1; before, only the
fine-tuned worker read them, so on the default engine they changed nothing), and only the ones given: an unset flag keeps the model's own
default. Both workers take the six under the same flag names, but `render/tts_chatterbox_ft.py` is still a skeleton that validates its
arguments and defers (exit 3, `FT_ENGINE_NOT_VENDORED`), so on `--engine finetuned` they take effect once that engine is vendored. The
command line is checked before the lock (`parseCli` / `checkFlags` in `render/tts.mjs`): an unknown flag, a flag with no value, a sampling
value that is not a finite decimal number, an `--engine` other than `generalist|finetuned`, `--model` / `--base-dir` without
`--engine finetuned`, `--engine finetuned` without both of them (the fine-tuned worker requires both) and a third positional (an unquoted
text) exit 2 with the reason and the usage line. A token is a flag only when it is shaped like one (`--` and a name), so a text that opens
with dashes (`"--- Intro ---"`, which the CT-52 verbs keep intact, `---` or `-x`) is the text, and a bare `--` ends the flags. Every value
reaches the worker as one `--flag=value` token, so a text, a path or a negative number that opens with a dash (`--text=---`,
`--temperature=-1e-2`) is never read as an option by the worker's argparse, which would exit 2 after the lock was taken. Every flag `internal/pipeline/pipeline.go` sends stays accepted (`render/tts.test.mjs` reads the Go source to check). Flag table: [`render/README.md`](../../render/README.md#voice--narration--tts-chatterbox-multilingual-voice-cloning).

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
| Card-only placement (opt-in, unset by default) | `imagegen_placement`, `gen_edit_placement`: `""`/`single` (the one-card lease every box has always taken) or `multicard` (the two non-display cards of the box; "Card-only image and edit"); both are family-overlay keys |
| Generative edit | `gen_edit_script`, `gen_edit_unet`, `gen_edit_preset` (`full`/`lightning8`/`lightning4`), `gen_edit_clip/vae/lora/lora_strength`, `gen_edit_steps/cfg/sampler/scheduler`, `gen_edit_megapixels` (0 = follow the source, held within 0.9-2.0), `gen_edit_timeout_sec` |
| Upscale | `upscale_script` (shipped default `render/comfy-upscale.mjs`), `upscale_model` (ComfyUI `upscale_models/` filename; empty = `videogen_upscale_model`), `upscale_timeout_sec` (600) |
| Video | `videogen_family` (`""`/`wan22` = Wan 2.2; `ltx25` = LTX-2.5 joint-AV), `videogen_unet_high`, `videogen_unet_low`, `videogen_text_encoder`, `videogen_upscale_model`, `videogen_wan_virtual_vram_gb`, `videogen_wan_decode` (`tiled` default; `plain`/`auto` opt-in), `videogen_placement` (`""`/`single` or `multicard`: the card-only Wan placement, "Card-only Wan") and `videogen_wan_resident_gb` (0 = the runner's 3; at most 64) (Wan keys; `placement` / `wan_resident_gb` in a `videogen_families` entry); `videogen_transformer`, `videogen_video_vae`, `videogen_audio_vae`, `videogen_latent_upscaler`, `videogen_fps`, `videogen_pool_vvram_gb/pool_compute/pool_donor` (LTX-2.5 keys) |
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

**The Wan decode is per card** (`videogen_wan_decode`, default `tiled`; the pipeline passes it as
`--wan-decode`, and a `videogen_families["wan22"].wan_decode` entry overrides it for a Wan render on a box seated
to another family). The Wan graph used to hard-code `VAEDecodeTiled` (tile 256/64, temporal 32/8), and the default
keeps it, so a config that never sets the key renders the graph it always did. A film session measured the
alternative on an RTX 5060 Ti 16 GB (A/B 2026-10-03): the plain `VAEDecode` took 38 s at a 10.3 GB peak, the tiled
decode 412 s at 3.2 GB (that A/B's tiled arm was one chunk), and the two outputs sit at 45 dB PSNR from each other.
That A/B ran with ComfyUI dynamic VRAM on, and the figures reached this change without the clip's width, height or
frame count, so the shape they were taken at is unrecorded. The three modes:

| Mode | Graph node | Reads the card |
|---|---|---|
| `tiled` (default; an empty value reads the same) | `VAEDecodeTiled`, unchanged from before the key existed | no |
| `plain` (opt-in) | `VAEDecode` | no |
| `auto` (opt-in) | `VAEDecode` when the render card reports at least 12 GiB of VRAM, else `VAEDecodeTiled` | yes |

**Why the default is `tiled`, not `auto`.** `auto` would run the plain `VAEDecode` by default on every 16 GB card, and
no render at the 16 GB tiers' own shape (1280x720x81) was ever run with plain. ComfyUI's own estimate there is 12.0 GiB
of a card that reports 15.9 GiB, and a second out-of-memory after ComfyUI's single tiled retry fails the whole render after sampling (the estimates behind this are worked out below). The one measurement above was taken with
ComfyUI dynamic VRAM on and a clip of unrecorded shape, so it does not say what happens at that shape. `auto` and
`plain` therefore stay available as an explicit opt-in (`videogen_wan_decode`, or `--wan-decode` on the runner), and the
default flips to `auto` only after a live acceptance render at the tier shape shows a decode close to the measured 38 s and no
`Ran out of memory when regular VAE decoding` line in the ComfyUI log.

Under `auto` the runner asks the ComfyUI it submits to, once per Wan run, after it is up and before the graph is built:
`GET /system_stats`, `devices[0].vram_total`. `devices[0]` is the primary device, the one a decode runs on (ComfyUI
lists it first on purpose and then every MultiGPU donor, read at v0.38.0), so a job pinned to one card
(`comfy_cuda_device`, a per-card instance) reads that card's size. The cut compares the reported **total**, not the
free memory; a 16 GB card reports about 15.9 GiB. When
the answer cannot be used, `auto` builds the tiled node, exactly what the graph always built: the request fails
or times out, ComfyUI answers an HTTP error or no JSON, there is no device or no numeric `vram_total`, or the
primary device is a CPU or MPS one, whose `vram_total` is the host's RAM and says nothing about a card. The runner
logs the node and the reason on stderr, one `wan-decode:` line per Wan run, for example
`wan-decode: VAEDecode (auto: the render card reports 15.9 GiB of VRAM, at least the 12.0 GiB the plain decode wants; card ...)`.
The default `tiled` and an explicit `plain` make no request, and neither does any run that is not the Wan graph; a run
on the default logs `wan-decode: VAEDecodeTiled (tiled decode requested)`.

**What `plain` risks, and what ComfyUI does when it fails.** ComfyUI's `VAE.decode` (`comfy/sd.py`, read at v0.38.0 on
2026-10-09) asks the card for the decode's estimated memory, runs the untiled decode inside a try/except that
re-raises anything that is not an out-of-memory error, and on an out-of-memory one logs `Warning: Ran out of memory
when regular VAE decoding, retrying with tiled VAE decoding.` and decodes again through `decode_tiled_3d`. That second
attempt sits outside the try/except, so it is a second chance and not a guarantee: if it also runs out of memory the
exception propagates and the render fails after sampling, and because the harness starts ComfyUI with `--cache-none`
a resubmit samples again. What the retry does for the Wan 2.1 VAE this graph loads (`wan_2.1_vae.safetensors`),
worked out from the estimator and the tile selection in that file:

- **The estimate follows the frame's resolution.** `memory_used_decode` is `(2200 if latent_frames <= 4 else 7000) x
  latent_height x latent_width x 64 x bytes_per_element`, so above 4 latent frames the clip's length drops out. At
  2 bytes per element (ComfyUI's bf16 or fp16 VAE; the harness passes no VAE precision flag) it asks 5.2 GiB at 832x480
  and 12.0 GiB at 1280x720, for 49, 81 or 121 frames alike.
- **The retry tiles space, not time.** Its budget is that same estimate, capped at 80 % of the card's total memory. It
  starts from a 256 px tile spanning the whole clip in time, shortens the time tile only while even that tile would
  exceed the budget (its estimate is 0.85 GiB, so it never does at these shapes), then doubles the spatial tile while
  the estimate still fits. The first retry tile is therefore 512 px, estimated at 3.2 GiB, at 832x480, but 1024 px,
  estimated at 9.6 GiB (80 % of the 12.0 GiB attempt that just failed), at 1280x720. Whether that tile fits where the
  whole frame did not has not been measured.
- **Where that bites.** The three 16 GB tiers (`blackwell-16`, `ampere-16`, `volta-16`) seed 1280x720x81, the shape at
  which the plain decode's estimate (12.0 GiB) sits closest to what such a card reports (15.9 GiB, three quarters of
  it), so `auto` leans hardest on the retry exactly there.

The caveat (ComfyUI issue 15453, from the research behind this change and not reproduced here): with dynamic VRAM the
other loaded models are not evicted before the decode, so a card already holding weights fails the plain attempt more
often. That is part of why `auto` sends a card under 12 GiB to the tiled node; an 8 GB card was measured the other way,
and plain `VAEDecode` never finished there (the `blackwell-8` tier record, 2026-08-23 media roster bake). A 12 GB card
sits on the cut and was not measured: pin `plain` or `tiled` there rather than trust `auto`. A box that opted in to
`plain` or `auto` and whose ComfyUI log keeps printing the warning above should go back to `tiled`, the default.

> **Unverified:** no render at the 16 GB tiers' own shape, 1280x720x81, was ever run with the plain decode, neither for
> this change (it renders nothing; its tests stand ComfyUI in with a `/system_stats` stub) nor before it, and the
> 2026-10-03 A/B is one card, with ComfyUI dynamic VRAM on, and one clip of unrecorded shape, so neither says whether
> the plain decode fits at that shape or whether the retry above rescues it when it does not. The 12 GiB cut, and how
> the plain attempt behaves beside resident models (issue 15453), were not measured either. `auto` decides on the card
> alone, which is the rule it was given, while ComfyUI's estimate follows the frame's resolution; a resolution-aware
> `auto` is a possible follow-up, not something this change does. That is why the default is `tiled` and `auto` is an
> opt-in. The acceptance condition for flipping the default to `auto`: one live render at a 16 GB tier's own shape,
> 1280x720x81, with `videogen_wan_decode` set to `auto`, whose decode stays close to the measured 38 s and whose ComfyUI log has no
> `Ran out of memory when regular VAE decoding` line.

**What this does not touch.** LTX 2.5 and Hunyuan 1.5 keep `VAEDecodeTiled`: they decode through other VAEs that
nobody measured, and their builders are called exactly as before. `--wan-decode` and `videogen_wan_decode` do
nothing for them (the pipeline passes the flag whatever the family, as it does `--wan-vvram-gb`, and their runner
branches never read it). WAN-Animate-2 already used the plain `VAEDecode`. The effect on the tiers: none. No tier seeds
the key, so every tier decodes `tiled` on a Wan render, exactly as before the key existed; a box opts in to `plain` or
`auto` by setting `videogen_wan_decode` itself.

**Card-only Wan: `--wan-placement multicard`** (`render/wan-multicard.mjs`, `render/comfyui-ext/`; the placement was measured
2026-10-11 on two RTX 5060 Ti 16 GB). A Wan 2.2 14B fp8 expert is 13.3 GiB and a film-size clip (720x1280, 81 frames, 75,600 tokens)
needs 12.2 GiB more for the sampler's activations beyond the 3 GiB of the expert kept resident (the film-size run's compute-card peak, 15.18 GiB, less
those 3), so it fits no single 16 GB card. The single-card way out is ComfyUI's dynamic
VRAM, which streams the experts through host RAM: the host pays the whole file, and a `gpu reserve` declares about 33 GiB for it.
`--wan-placement multicard` spends a second card instead of host RAM. `single`, the default, is today's graph and launch, byte for byte.

| Flag | Meaning |
|---|---|
| `--wan-placement single\|multicard` | `single` (default) changes nothing. `multicard` is the placement below; `--high-unet` / `--low-unet` must name the experts: the fp8 safetensors files, or the Q8_0 GGUF files (`Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf` / `-LowNoise-Q8_0.gguf`, 14.349 GiB each; "Q8_0 experts" below). |
| `--wan-cards <a>,<b>` | The two cards, as nvidia-smi indices or UUIDs, for a call made outside a lease. Under a lease the lease's cards (`GPU_LEASE_DEVICES`, else the lease's own record) are the authority and this can only confirm them: a card the lease does not hold is refused. With neither, the UUIDs in `CUDA_VISIBLE_DEVICES`. |
| `--wan-resident-gb N` | GiB of each expert kept on the compute card (default 3, the measured split; at most what the compute card holds beside the sampler's activations at the call's geometry, and at least what lets the storage card hold the rest of the expert beside the VAE: 3.2 GiB at most at film size on a 15.9 GiB card, from the film-size run's measured peak; under `--use-sage-attention` in `COMFY_EXTRA_ARGS` the film-size ceiling is 0.2 GiB, so pass 0: "Sage attention" below). The rest sits on the storage card. |
| `--wan-decode plain` | Under multicard: save the sampled latent, free both cards, then the plain `VAEDecode` (below). `tiled` and `auto` keep their meaning. |
| `--wan-loader` | Not read under multicard (the placement is the loader). `native`, which a film driver may still pass, is ignored with one stderr line. |

**The placement.** Two cards, numbered in the order of `CUDA_VISIBLE_DEVICES`: the runner lists the UUIDs storage first, so the first
is `cuda:0` whatever its nvidia-smi index. `cuda:0` is the STORAGE card: text encoder, VAE and the donor blocks of the expert in use
(about 10.3 GiB). `cuda:1` is the COMPUTE card: 3 GiB of the expert resident plus the sampler's activations. Each expert loads through the
DisTorch2 loader with `compute_device cuda:1` and the allocation `cuda:1,3gb;cuda:0,*`; the donor blocks stream to the compute card one at a
time over the card-to-card copy (about 6 GiB/s, no peer access: roughly 1.7 s of a 250 s step). The text encoder has to stay on `cuda:0`:
on `cuda:1` the ComfyUI process aborts.

**Which card computes.** Sizes within 1 percent of the larger card (`VRAM_EQUAL_FRACTION`, 0.01: 163 MiB at 16 GiB) count as one size, because the exact
byte count of a card's memory differs by a few MiB between boards of one generation and says nothing about their speed: a 5070 Ti reads 16303 MiB beside a
5060 Ti at 16311. Inside that band the card with the higher board power (`power.default_limit`, the board's design power, not the operator-adjustable
`power.limit`) computes when the two differ by at least 1.25 times (`POWER_RANK_RATIO`, inclusive). Both numbers are chosen, not measured. The pair this was written
for reads 300 W (5070 Ti) against 180 W (5060 Ti), 1.67 times, and the higher-power card sampled about 2.5 times as fast (measured 2026-10-12 on a 16 GB Blackwell
pair with `--use-sage-attention`: about 2.5x). Every other pair takes the roles it always had: the larger card computes; of two equal cards the one with the higher
UUID computes (the lease sorts its ids, so a pair named with `--wan-cards` in either order gets the split the benchmark measured, storage on the lower id). That
includes a pair whose board power the driver did not report: it is ranked as before, never refused, and, for a pair within the 1 percent size band, the plan says `board power not reported for idx N` so the
fall-back is visible (a pair outside the band was never going to be ranked by power, so its reason carries no such remark). The one line the plan says contains `compute card chosen: <the rule>`, followed by the sage clause under sage attention. Board power is a proxy for speed: it can mislead for a pair whose
power and speed disagree (a hard power-capped card reads low), and `WAN_COMPUTE_UUID` (below) overrides it.

`WAN_COMPUTE_UUID`, an environment variable and not a flag, names which card of the pair computes: an nvidia-smi index, a UUID, or a case-insensitive UUID prefix of 8
or more characters after the optional `GPU-` (`GPU-aaaa1111` names a card; the characters are counted after the prefix, so `GPU-aaaa` and a bare `GPU-` name nothing; a token of digits alone is read
as an index, so a UUID prefix made of digits needs the `GPU-` spelling, `GPU-11112222`, and the refusal says so). It chooses between the two cards already found and never adds
one: the lease or `--wan-cards` still decide the pair. A token that names exactly one card of the pair makes it the compute card, beating the size and the speed rule;
the resident-share and storage checks still judge it, so a named card that cannot hold the share beside the activations is refused with the numbers (the 12 GiB card of a 24 + 12 GiB pair at
`--wan-resident-gb 4`: at most -0.7 GiB; the 16 GiB card of a 24 + 16 GiB pair holds the default share of 3 at film size, 16.0 - 12.2 - 0.5 = 3.3, so naming it is admitted there and refused only
for a share over 3.3). The lease sizing assumes the larger card computes, so such a refusal comes after the lease is taken and before ComfyUI starts, never as an out-of-memory run. A token that names no card of the driver's table (a typo, a
stale id) is refused with `WAN-PLACEMENT: WAN_COMPUTE_UUID names "<token>", ...`, naming the two cards: an explicit setting must not run quietly. A token that names a real card
that is not one of the two, or begins more than one card of the table, is ignored with a one-line note (`WAN_COMPUTE_UUID names "<token>", which does not name exactly one of the two
cards (idx a, idx b); ranked instead`, appended to the plan's line), because the variable is box-level and a lease may hold a pair that does not include that card. An empty or blank value is
absent. Set it in the shell for a `gpu reserve ... --` command or a hand-run helper, and in the daemon's environment for the typed image and edit doors (below), whose runner inherits it.

`gpu_comfy_order` ([gpu-lease.md](gpu-lease.md), "every card once, fastest first") is the operator's own declaration of which card is fastest, and the harness refuses to guess
that order from what nvidia-smi reports. The board-power rule is a proxy for it on the one question this placement asks, and it is not a substitute: a box that declares the order
has said more than power does. Two follow-ons, not in this change: seeding the compute choice from `gpu_comfy_order` through the grant environment (the runner never sees the order today),
and `compute_cap` as the primary key for an fp8 pair that spans Ampere and Ada (fp8 tensor cores need Ada or newer, so a 320 W Ampere card ranks above a 200 W Ada one by power
alone; `compute_cap` is identical on the live pair, and reading it costs another column and another ladder rung on drivers that reject the field).

The lease sizing (`internal/hostneed`) still judges the compute stage against the LARGER card. Inside the 1 percent band the gap is at most 0.16 GiB at 16 GiB, so a share at the
film-size ceiling (3.2 GiB) can be declared 0 there and refused by the runner's resident check on the smaller card that computes; the default share of 3 is unaffected.

**Which cards, and what is refused before anything starts.** `planMulticard` reads the driver's card table
(`nvidia-smi --query-gpu=index,uuid,name,memory.total,memory.free,display_active,display_attached`; a failed query is asked once more
before a driver is taken to lack `display_attached`; then `--query-gpu=uuid,power.default_limit` in a second, best-effort query, asked once more when it fails, joined by UUID,
and a board whose power the driver does not report stays at 0 = unknown and ranks nothing; every ask hides its console window on Windows) and refuses, with the reason: no cards named, a dangling `--wan-cards`, anything but
exactly two, a token that names no card, a `--wan-cards` card the lease does not hold, a card that drives a display (`display_active`
Enabled or `display_attached` Yes, the harness's own rule, so a monitor on a sleeping screen counts) and a card whose display state the
driver does not report (never assumed screenless). It also refuses what was not measured or cannot run, in the same place, before the
lease slot is taken, llama-swap is unloaded or ComfyUI starts: `--fast`, an expert nobody named (`--high-unet` and
`--low-unet` are required: the builder's defaults are the Q4_K GGUF files, which fit one card and were not measured split), another family (`--model h3|hunyuan|ace`, a `--graph` file; `--model ltx25` is placed by "LTX-2.5 on two cards" below), a `--wan-resident-gb`
that one of the two cards cannot hold at the call's geometry (the compute card: its size less a chosen 0.5 GiB margin for the CUDA context, less the sampler's
activations, the benchmark's reserved allocator peaks as an envelope of 0.31 GiB per 1,000 tokens (never under 2 GiB) capped at the film-size run's
measured 12.2 GiB beyond the resident share (above the film's size nothing was measured: a bigger clip is held at 12.2 and a `WAN-GEOMETRY-NOTE` says so before the check, with what a proportional extrapolation gives, leaving the share and `--wait-sec` to the caller; the activations read the attention mode from `COMFY_EXTRA_ARGS`: 15.2 GiB at film size under `--use-sage-attention`, the line between the two measured sage points below it, 12.2 without it), so 3.2 GiB of an expert at most at film size (0.2 under sage) and 9.1 at the default 832x480x49; the storage card: the
rest of the 13.4 GiB expert (14.4 GiB for a Q8_0 GGUF expert) plus 0.8 GiB for the VAE and the context inside its size less `--reserve-vram`, 1 GiB by default, so a 24 GiB card beside
an 8 GiB one is refused with the share that would fit), a UUID prefix that names two cards, and
the placement's own flags lost to an `=` (`--wan-placement=multicard`, or `--seed=7 --wan-placement multicard`: this runner's parser does
not split `=`, so the flag is lost or swallowed, and the lease sizing, which does split it, would declare no host RAM for a call that then
ran single; write each flag as `--name value`). After ComfyUI is up and before the graph is POSTed the runner compares the
`/system_stats` device list with the pair (count, size to 5 percent and model name, in order; it cannot tell two cards of one model apart, because
`/system_stats` carries no UUID, whose order is the pin's and the roles' rule above), and asks for the harness's node pack under every decode; a mismatch
is `WAN-PLACEMENT-MISMATCH`, a missing pack `WAN-PLACEMENT-PACK`, with nothing submitted.

**Q8_0 experts, and the typed video door.** Besides the fp8 safetensors experts the placement runs the Q8_0 GGUF experts
(`Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf` and `-LowNoise-Q8_0.gguf`, 14.349 GiB each, which fit no single 16 GB card beside the sampler). A `.gguf`
expert loads through `UnetLoaderGGUFDisTorch2MultiGPU` with the same compute card and the same allocation string (`cuda:1,<resident>gb;cuda:0,*`),
`virtual_vram_gb` 0 and `eject_models` false, and no `weight_dtype` input (the quant type is in the file). With an allocation string set and
`virtual_vram_gb` 0 the loader ignores `donor_device`, so the donor is the storage card and never system RAM. The planner sizes a
`*Q8_0.gguf` expert at `WAN_EXPERT_Q8_GIB` (14.4 GiB) in its resident-share check and the fp8 expert at 13.4; the launch's custom-node whitelist
gains ComfyUI-GGUF (a run that names a `.gguf` expert and finds no such folder is refused before ComfyUI starts, and a kept instance launched without
it is replaced, or refused when it is not the harness's own). The host-RAM declaration is the same stage rule, so a Q8_0 pair on two 15.9 GiB cards
declares 0 GiB. Only the Q8_0 files are sized by name; any other `.gguf` is sized as the fp8 figure, and the runner refuses the builder's own
default Q4_K files by name (they are not named, and they fit one card). **Measured 2026-10-20:** the Q8_0 pair at 1280x720, 81 frames, 2 GiB of
each expert resident, plain decode, on a 300 W compute card and a 180 W storage card (both 15.9 GiB), with 0 GiB host RAM declared on the lease:
3,330.6 s from execution start to success, per-card peaks 10.40 GiB (compute) and 13.01 GiB (storage), the display card untouched. Host free physical
RAM dipped for a few seconds while each expert streamed onto the cards (transient staging) and held steady through sampling; the clip was inspected
frame by frame (no jumps, no flicker).

The typed `video` door reaches the placement from config: `videogen_placement: "multicard"` (or `placement` in a `videogen_families` entry; an
unknown value is a config error, as for `gen_edit_placement`) with `videogen_unet_high` / `videogen_unet_low` naming the experts, and
`videogen_wan_resident_gb` (0 = the runner's default of 3). For the Wan 2.2 graph the door then leases the box's two non-display cards as a set
(the same lease path as the edit door: gpu_lease_unavailable, never one card or the whole node when it cannot), declares the stage-rule host RAM
(0 when both cards hold their stage), passes `--wan-placement multicard` and, above 0, `--wan-resident-gb N`, and launches with no single-card pin and
no dynamic VRAM. A binding that names no expert is refused before any lease. For `videogen_family: ltx25` (or a `videogen_families["ltx25"]` entry) the same keys
place the LTX-2.5 graph, below. Another family renders as before.

**LTX-2.5 on two cards** (`--wan-placement multicard --model ltx25`; `render/wf-ltx25-i2v.mjs`, `render/wan-multicard.mjs`, `OffloadFreeAfterEncode` in `render/comfyui-ext`).
The 20.03 GiB int8 convrot DiT and the 14.32 GiB Gemma text encoder (clip type `ltxv`) are bigger than one 16 GB card together and apart from the sampler's
activations, so they run pooled across the lease's two non-display cards through DisTorch2, the donor being the other card and never system RAM. This is
the Wan placement with the roles' device numbers reversed, for one reason: an int8 DiT that computes on any device other than cuda:0 faults with an
illegal memory access (ComfyUI-MultiGPU issue #220, archived upstream, no fix coming). So the launch lists the COMPUTE card first in
`CUDA_VISIBLE_DEVICES` (CUDA numbers the visible cards in that order) and **cuda:0 is the compute card, cuda:1 the storage card**, where the Wan and image
launches list the storage card first. The compute-first launch has its own instance key, `mcc-<compute uuid head>-<storage uuid head>` (the Wan key is
`mc-<storage>-<compute>`, and a pair whose roles WAN_COMPUTE_UUID reversed would spell the same two heads the other way round), so a kept ComfyUI of one order
is never reused for the other, and the device check after the start compares cuda:0 with the compute card and cuda:1 with the storage card. The cards are chosen
as for Wan (the lease's, never a display card; the compute card by the same `chooseRoles` rule).

The graph: the DiT is `UNETLoaderDisTorch2MultiGPU` with `compute_device` `cuda:0`, `expert_mode_allocations` `cuda:0,<R>gb;cuda:1,*`, `virtual_vram_gb` 0,
`donor_device` `cpu` (with an allocation string and `virtual_vram_gb` 0 the loader ignores it: the donor is the storage card), `eject_models` false and
`weight_dtype` `default`; the text encoder is `OffloadCLIPLoaderDisTorch2` (built off the cards from the read-only map and placed block by block, host RAM
refused) with `type` `ltxv`, `device` `cuda:0` and the split `cuda:0,7gb;cuda:1,*`; both VAEs are `VAELoaderMultiGPU` on `cuda:1`. The latent upscaler has no
device input and loads on cuda:0. The DiT is a quantized checkpoint, so it is built empty and filled from the read-only map and reaches the cards only when
the first sampler loads it through DisTorch2. The encoder and the DiT do not fit the two cards at once (34.4 GiB of weights against 31.8, before the VAEs),
so the encoder must be gone from the cards before that: **`OffloadFreeAfterEncode`** sits between the conditioning (node 11) and the two guiders, takes both
conditionings and the CLIP, and returns the conditionings unchanged. Under `--gpu-only` ComfyUI's own unload frees nothing (its offload device is the card,
and unloading would first copy the weights there), so the node removes the encoder from `current_loaded_models` itself, replaces every parameter and buffer
on a card with an empty CPU tensor, returns the cached blocks to the driver, logs each card's memory before and after, and stops the run with
`OFFLOAD-TE-STILL-RESIDENT` if any of the encoder's tensors is still on a CUDA device. Because the pack is loaded with `--cache-none`, the next prompt builds and
places the encoder again.

Defaults, all **chosen, not measured**: `--wan-resident-gb` (here the GiB of the DiT kept on the compute card) is 9, so the storage card holds the other
11 GiB beside both VAEs (20.1 - 9 + 1.8 + 0.8 context = 13.7 GiB of its 15.9, less `--reserve-vram`); the encoder split is 7 GiB on the compute card and the rest on the
storage card. The planner refuses, before ComfyUI starts, a share the compute card plainly cannot hold (its size less the 1 GiB latent upscaler and the 0.5 GiB
context, 14.4 GiB on a 15.9 GiB card) and one that leaves the storage card more than it can hold; the sampler's activations are not in either check because
no LTX-2.5 run was made on the two cards, so a share that passes can still run out of memory in the first sampler step (an `LTX-PLACEMENT-NOTE` says so every
run; lower the share if it does). It also refuses a non-`.safetensors` or bf16 `--transformer` (about 39 GiB), `--pool-vvram-gb` (the other placement of the
same DiT), and there is no expert, `--fast` or GGUF check, because the graph has none. The wait is 7200 s unless `--wait-sec` / `COMFY_WAIT_SEC` says otherwise;
an LTX-2.5 time on the two cards is unmeasured. The typed door sizes the host RAM on the same stages (the DiT's share, the upscaler and the context on the compute
card; the rest of the DiT, both VAEs and the context on the storage card; the encoder is gone before the DiT) and declares 0 GiB when both fit, and refuses a
`.gguf` or bf16 transformer before any lease. The 2026-08-12 single-card-class timing of this family (247 s for a 5 s clip at 1920x1088) is a different placement.

**The launch.** The runner starts its own ComfyUI, a keyed instance `mc-<storage uuid head>-<compute uuid head>`: its own port (from
`--api` or `COMFY_API`, which must not be the unkeyed instance's 8188; with neither it falls back to the per-card port, and a ComfyUI
already holding that port stops the run, which a stderr line says up front), its output and temp folders under the install's
`instances/<key>/`, and its launch marker and console log (`offload-comfyui-<key>.log`) beside the install. That is the existing
keyed-instance mechanism, which writes into the install's tree but never into its `custom_nodes`, settings or models. The argv is the
harness's own (`--disable-smart-memory --cache-none --reserve-vram`) plus `--gpu-only --disable-pinned-memory`,
`--extra-model-paths-config render/comfyui-ext/extra_model_paths.yaml`, and `--disable-all-custom-nodes --whitelist-custom-nodes` naming the
install's MultiGPU and VideoHelperSuite packs (found by name, case-insensitively, in its `custom_nodes`) and the harness's own pack. The
child env is `CUDA_VISIBLE_DEVICES=<storage uuid>,<compute uuid>`, `CUDA_DEVICE_ORDER=PCI_BUS_ID` and `OFFLOAD_RO_LOAD=1`. For that launch
only, the caller's `COMFY_CARD_UUID`, `COMFY_CUDA_DEVICE` and `COMFY_DYNAMIC_VRAM` are dropped, because a single-card pin and dynamic VRAM
contradict a two-card `--gpu-only` placement, and `COMFY_INSTANCE` is set, so a film driver's existing environment can stay as it is. `--enable-dynamic-vram` in `COMFY_EXTRA_ARGS` is dropped too, said once (ComfyUI starts dynamic VRAM on that flag even
beside `--gpu-only`, which would stream the experts through host RAM under a lease that declared none). A
running ComfyUI is reused only when its launch marker proves it is this instance, on this pair, in this order (`reuseVerdict`); otherwise it
is replaced or refused, as for a card pin; one whose argv turns dynamic VRAM on is never reused.

**The harness-owned ComfyUI code (`render/comfyui-ext/`).** ComfyUI reads `custom_nodes` from any `--extra-model-paths-config` yaml, after
the install's own `extra_model_paths.yaml`, so the harness's node folder (`nodes/offload_multicard`) is an extra search path and nothing is
installed into the shared ComfyUI. It holds two things. First, a read-only safetensors reader, switched on by `OFFLOAD_RO_LOAD=1`: with
dynamic VRAM off, ComfyUI opens a safetensors file copy-on-write, which Windows charges to system commit in full at map time (measured:
+13.3 GiB per expert, +14.7 GiB of private bytes on the stock loader); mapped read-only the same file charges nothing. On this driver every
VRAM allocation is also charged 1:1 to the process's private bytes and to commit, so a card-only run reads "private +17 GiB, commit
+20-40 GiB" while using about 2 GiB of real host memory; host growth is private growth minus the VRAM growth on the leased cards. If the
reader cannot be installed the pack fails to import, and the runner, which asks for the pack's node before every run whatever the decode,
stops with `WAN-PLACEMENT-PACK` before any GPU work. The reader assumes nothing writes into a loaded weight in place (the map is read-only,
so such a write would crash ComfyUI with a Windows access violation, not corrupt the file); none exists on the Wan load path read at
ComfyUI 0.38.0, where `--gpu-only` copies the weights to the card. The pack check proves the pack loaded, which installs the reader whenever
the launch env asks for it; a hand-started instance with the same argv but without `OFFLOAD_RO_LOAD=1` would pass it without the reader
(the launch marker records argv, not env), and the runner never starts one that way. Second, `OffloadFreeForDecode`, below.

**Decode under multicard.** The plain decode needs about 10 GiB beyond the weights (measured +10.0 GiB at the film latent, 34 s) and the
experts' donor blocks sit on the decode card. With `--gpu-only` the offload device is the card itself, so ComfyUI's own unload frees
nothing; an expert leaves the cards when its model patcher loses its last Python reference, which is when the sampler that used it
completes (`--cache-none` drops its inputs). The pass-through node `OffloadFreeForDecode` (id 21, between the last sampler and the
`VAEDecode`) is meant to make that certain, and shows it. It saves the sampled latent first (`latents/render_wan`; a `LATENT` line names it, because an
80-minute sample is never worth losing to a failed decode), collects garbage, returns every card's cached blocks to the driver, logs each
card's free memory before and after, and stops the run with `OFFLOAD-DECODE-NOFIT` and the numbers when the VAE's card has less free than
85 % of ComfyUI's decode estimate (the measured need was 83 % of it). A stop is deliberate: ComfyUI would otherwise retry the decode tiled,
with its own temporal chunking, which is the seam the plain decode exists to avoid. A plain decode is judged by this run's console, through a cursor made before the prompt is submitted (a kept instance's log holds earlier
runs' lines, which do not count). When it shows `Ran out of memory when regular VAE decoding`, ComfyUI retried the decode tiled with
temporal tiles it sized itself, so the clip is not the single pass that was asked for and can seam where its tiles meet: the finished clip
is written beside the requested path as `<name>.seamed<ext>` (never thrown away), the saved latent is named, nothing is written to the
requested path, and the run fails with `WAN-DECODE-FALLBACK`. A console that is missing, or whose capture reached its 5 MB cap, cannot say:
`WAN-DECODE-UNVERIFIED` is printed and the clip is written as asked. `tiled` under multicard is today's `VAEDecodeTiled` (256/64, temporal
32/8): it fits beside the expert (+2.3 GiB measured), and its temporal tiles meet where ComfyUI's tiling puts them (`tiledSeamFrames`:
tiles of 8 latent frames overlapping by 2, each written at 4 times its first latent frame, so frames 24, 48 and 72 at the film's 81
frames), which the `WAN-PLACEMENT-NOTE` at the start of such a run names for the clip being made.

**While it waits.** The poll budget of a card-only run is 7,200 s unless `--wait-sec` or `COMFY_WAIT_SEC` names one (a line says which): the
20-step film recipe was projected at about 5,170 s from the benchmark (164.5 s that is not sampling plus 20 steps of 250.3 s) and took 5,136 s of runner wall, the single-card default of 5,400 s left 4.5
percent, and the sampled latent is saved only after the second sampler. A bigger clip than the film's was not measured: pass `--wait-sec`.
A lease taken with `gpu reserve` renews on the poll's heartbeat while ComfyUI still lists the prompt, so the wait may outlast the `--for` term.
The run also reads ComfyUI's console while it waits (a cursor made before the prompt is submitted, asked every 15 s): ComfyUI can
wedge after an out-of-memory error inside the DisTorch2 distribution with nothing in its history, while it goes on answering HTTP, and
the poll alone would hold both cards for the whole budget. The two lines that mean the prompt worker is gone (`Got an OOM, unloading all
loaded models` and a dead `prompt_worker` thread) end the wait with `WAN-OOM` and the console's path; the recoverable ones in the same log
(the `_scaled_mm` fallback warning, the VAE-decode retry, a node error that does reach the history) do not. A console that cannot be seen is
said once (`WAN-OOM-WATCH`) and the wait is then bounded by the budget alone. A failed prompt's exception message reaches the error whole.

**Host RAM.** `gpu reserve --devices <a>,<b> -- node render/comfy-video.mjs ... --wan-placement multicard` declares 0 when each of the
two leased cards fits its own stage, as the runner will place it (`hostneed.estimateWith`, `Facts.CardsGiB`, each card's size): the larger
card computes, so it must hold the resident share of the expert (`--wan-resident-gb`, default 3) plus 12.2 GiB of film-size activations (the
film-size run's compute-card peak, 15.18 GiB, less its 3 GiB resident share) plus a 0.5 GiB margin for the CUDA context (chosen, not measured: the 12.2 already holds a context, and the film-size run left 0.7 GiB free at its peak), which is 15.7 GiB with the default
share and 3.2 GiB of share at most on a 15.9 GiB card (under `--use-sage-attention` in `COMFY_EXTRA_ARGS`, read from the process environment, the activations are 15.2 GiB: 15.7 GiB with no share, 0.2 GiB of share at most; `WAN_FILM_ACTIVATIONS_SAGE_GIB` is mirrored by a Go test like the others); the other holds the rest of the expert plus 0.8 GiB for the VAE and the context inside its
size less `--reserve-vram` (the film-size run's storage card peaked at 10.86 GiB, the 4-step benchmark's at 11.1 GiB with 10.3 GiB of blocks). The
runner checks the same two stages before it starts, with the same figures (a Go test fails when its `WAN_FILM_ACTIVATIONS_GIB`,
`WAN_COMPUTE_CONTEXT_GIB`, `WAN_STORAGE_OVERHEAD_GIB` or `WAN_EXPERT_GIB` drift from these, and another that the two sides admit the same share
at film size). A sum would pass a 24 GiB card beside an 8 GiB one; the 8 GiB card cannot hold the
donor blocks. `--reserve-vram` is ComfyUI's model-load margin, not an activation ceiling, so it is the storage card's margin and not the compute
card's (with it on the compute card the measured call, 3.0 + 12.2 = 15.2 GiB, would have been refused on a 15.9 GiB card and declared the whole
33 GiB set): a `--reserve-vram` above 4.7 GiB no longer fits the donor blocks on a 15.9 GiB storage card, and that call takes the ordinary
estimate (the lease it needs then declares the whole set, or `--ram 0` explicitly). One card, three (a whole-node lease), no card table,
no fit, another family or no flag keep the ordinary estimate, the whole set (33 GiB). Measured host growth in the benchmark was 2.3 GiB
over ComfyUI's idle, inside the headroom the brake keeps anyway. The typed VIDEO door has one too (`videogen_placement: multicard`, "Q8_0 experts" below), as do the typed image and edit doors ("Card-only image and edit", below); the render helper can still be run by hand under an explicit `gpu reserve --devices`.
A driver script that wraps the helper is not itself a recognised render helper, so a lease around the script declares the media class
default (the largest bound family's figure) whatever the helper inside it does; the verb says so on a second line (F71), and the way out
is `--ram 0` on that `gpu reserve`, because the card-only placement keeps its weights on the two cards. The same `--ram 0` is the way out of the sage
sizing's one over-declaration (the lease reads no geometry: "Sage attention" below). And the lease reads `COMFY_EXTRA_ARGS` from the process environment
of the `gpu reserve` command only: an `env COMFY_EXTRA_ARGS=--use-sage-attention` prefix INSIDE the wrapped command is invisible to the lease sizing while the
runner sees it, so the lease would declare as for the default attention; export the variable around the `gpu reserve`, or pass `--ram 0`.

**Measured.** On two RTX 5060 Ti 16 GB, the two non-display cards of a three-card box, in one media lease with `--ram 0` (2026-10-11), on
commit 090fbd95, the tip of this branch before its second review round; that round's fixes were not re-run on the cards.
The film geometry (720x1280x81, 20 steps, boundary at 10, cfg 3.5, fp8 experts, plain decode) ran at 250.5 s per step on both experts: the runner
took 5136 s (85.6 min), of which 67 s to the first step, 24 s for the hand-over to the second expert and 31.5 s from the free node to the finished
video (the free node released both cards in 1.1 s, leaving 14.4 and 14.6 GiB free; the plain decode asks about 10.2). Per card (nvidia-smi): the compute card
13.3 to 13.65 GiB while the first expert samples, 15.18 GiB (95.3 % of the card, with 3.0 GiB of the expert resident) at the peak, which comes about 90 s
before the hand-over, and 14.9 GiB steady while the second samples; the storage card 10.86 GiB (68 %). The compute card therefore keeps a margin of
0.7 GiB at the peak, and each GiB more kept resident is a GiB less of it: the runner refuses more than 3.2 GiB at this geometry (only a 3.0 GiB share was run; 3.2 is the planner's ceiling, with its 0.5 GiB context margin chosen), and a larger clip, which was not measured, is held at the same 12.2 GiB with a `WAN-GEOMETRY-NOTE` although its activations are expected to grow with its tokens, so it probably needs
a smaller `--wan-resident-gb`. Host RAM: ComfyUI idles at 2.9 GiB private; beyond the 1:1 VRAM charge its growth is 2.33 GiB steady (2.6 at most),
with two working-set peaks of 15 GiB lasting under 15 s each (the experts' read-only maps being read at the first load and at the hand-over; file-backed,
not commit). A first benchmark of the same recipe, 4 of the 20 steps with the tiled decode, gave the same 250 s per step and 13.3 and 11.1 GiB, which
the full clip matches until about 2485 s; its 15.18 GiB peak on the compute card comes later, where the 4-step run never reached.
The clip passes the frame check (verdict CLEAN): the chroma step into the tile joins 24, 48 and 72 is z 1.92, 1.17 and 0.0 (the pre-fix tiled clips measure
z 6 to 18), the frame differences at the joins 24 and 48 are 0.69x and 0.82x the clip's median, no frozen or repeated frame, no jump, and the walk cycle is
continuous through both joins when viewed. The Qwen-Image-Edit 2511 call at the film's full
preset (fp8mixed, 40 steps, cfg 4, no LoRA, 1.47 MP) also runs on the same pair with 9 GiB of the DiT resident on the compute card: 15.98 s per step,
12 min per image, 10.5 and 10.6 GiB per card; its output carries the known 1.47 MP bottom-edge strip, as the single-card runs do.

**Sage attention** (`--use-sage-attention` in `COMFY_EXTRA_ARGS`; film session, 2026-10-12). The planner and the lease sizing read the attention mode from
`COMFY_EXTRA_ARGS`, token by token: the exact flag, or an argparse prefix of it of 8 or more characters (ComfyUI's parser accepts an unambiguous abbreviation,
so `--use-sa` is sage and `--use-s`, which also begins `--use-split-cross-attention`, is not). The compute card, nvidia-smi totals, "Planner" being what the sizing
holds on a 15.9 GiB card (the activations beyond the share, and the share it admits at most):

| Attention | Clip | Tokens | Resident per expert | Per step | Runner wall | Compute-card peak | Storage-card peak | Planner |
|---|---|---|---|---|---|---|---|---|
| default (PyTorch), 2026-10-11, two 16 GB cards | 720x1280x81 | 75,600 | 3 GiB | 250.5 s | 5,136 s | 15.18 GiB | 10.86 GiB | 12.2 GiB; 3.2 GiB at most |
| sage | 720x1280x65 | 61,200 | 2 GiB | 66 s | 1,489 s | 15,216 MiB (14.86 GiB) | not recorded | 12.9 GiB; 2.5 GiB at most |
| sage | 720x1280x81 | 75,600 | 0 | 94 s | 2,041 s | 15,482 MiB (15.12 GiB) | 14,182 MiB (13.85 GiB) | 15.2 GiB; 0.2 GiB at most, so in practice 0 |

- Sage holds about 3 GiB more activations than the default attention at film size, so `--wan-resident-gb 0` is the only setting that fits 81 frames on a 16 GB compute
  card. The activations are on the file's own basis (the card's nvidia-smi peak less the resident share, the context inside): 15,482 MiB is 15.119 GiB, rounded up to 15.2;
  14.859 GiB less the 2 GiB share is 12.859, rounded up to 12.9. The 0.5 GiB context margin is added on top, as for the default.
- **Behaviour change: sage film calls must pass `--wan-resident-gb`.** The default share of 3 is refused under sage from about 58k tokens up on a 15.9 GiB card, where the compute card's
  ceiling (15.9 - the sage activations - 0.5) falls under 3: 720x1280x61 (57,600 tokens) still admits it (at most 3.1 GiB), 720x1280x65 (61,200 tokens) refuses it (at most 2.5 GiB) and
  720x1280x81 admits 0.2 at most. Before, it was admitted by the planner and the lease sizing and ran out of memory. The refusal puts its sage clause before the ceiling and says `--wan-resident-gb 0` is the
  measured setting at film size.
- The sampler's activations under sage are the default curve up to about 57k tokens, and from there the straight line through 12.9 GiB at 61,200 tokens and 15.2 GiB at 75,600, held at 15.2
  above the film's size (nothing larger was measured; the `WAN-GEOMETRY-NOTE` extrapolates from 15.2 under sage). So sage departs from the default sizing only above about 57k tokens, and only 61,200
  and 75,600 tokens were measured: the points between are interpolated and chosen, not measured. Below 57k the default curve already covers the benchmark's 6.16 GiB reserved with sage at 20,280 tokens (it gives 6.29).
- The storage card's 13.85 GiB at resident 0 sits inside the 13.4 + 0.8 the placement already assumes, so the storage figures stand.
- Only sage was measured. Another attention flag (flash, split, quad, pytorch) is sized as the default, and is unmeasured.
- The film session's sage run and the default run were not on the same compute card, so 94 s a step against 250.5 s is not an attention-only comparison.
- The lease sizing reads no geometry: under sage it holds 15.2 GiB for any clip, so a partial-share sage call at a smaller clip (the measured 65 frames at 2 GiB) declares the ordinary set unless
  `--ram 0` states the real figure, where it declared 0 before sage was read; its detail says so. An `env COMFY_EXTRA_ARGS=...` prefix inside the wrapped command is invisible to it (see "Host RAM").
- A running ComfyUI is reused only when its argv agrees with the call's sizing in the direction that can run out of memory: a kept instance launched with `--use-sage-attention` is not reused by a
  call that was sized for the default attention (the harness restarts an instance it launched, and refuses `COMFY-PROFILE-MISMATCH` when it did not). The reverse, a kept default instance and a sage call, only wastes a cold start and is reused.

**What it says when it refuses.** `WAN-PLACEMENT: ...` (cards, display card, missing pack: nothing launched);
`WAN-PLACEMENT-MISMATCH` (ComfyUI's devices are not the pinned pair: nothing submitted); `WAN-PLACEMENT-PACK` (the harness's pack failed
to import, so the read-only reader is not there: read the console log; nothing submitted); `WAN-PLACEMENT-NOTE` (the decode is the tiled
one, which can seam where its temporal tiles meet: the frames are named for the clip); `OFFLOAD-DECODE-NOFIT` (the cards were not freed
enough for a plain decode: the latent is in the instance's output folder under `latents/`); `WAN-DECODE-FALLBACK` (the plain decode was
retried tiled after all: the run fails, the clip is kept as `<name>.seamed<ext>`, the latent is named); `WAN-DECODE-UNVERIFIED` (the
console could not be read to the end, so whether it was retried is unknown); `WAN-OOM` (the prompt worker died of an out-of-memory error
and the wait ended; ComfyUI must be restarted); `WAN-OOM-WATCH` (the console cannot be watched); `WAN-WAIT-NOTE` (the default budget
was used).

**Card-only image and edit: `imagegen_placement` / `gen_edit_placement` = `multicard`** (`render/multicard-image.mjs`,
`render/multicard-recipes.json`, `internal/pipeline/mediamulticard.go`; [ADR 0039](../architecture/decisions/0039-a-held-card-is-a-place-in-line.md),
Amendment 2026-10-11). The Wan placement above spends a second card instead of host RAM; the Qwen-Image image and edit routes can do the same.
It is opt-in per box (and per family overlay), UNSET by default, and no tier seeds it: a box that sets nothing renders exactly what it
rendered, with the same lease, the same argv and the same host-RAM declaration (each pinned by a test). The text encoder and the VAE sit on
one leased non-display card (cuda:0, the STORAGE card) and the DiT on the other (cuda:1, the COMPUTE card), whole or split between them
through the ComfyUI-MultiGPU loaders, with every weight file mapped read-only (`OFFLOAD_RO_LOAD=1`, the pack `offload_multicard` of the Wan
placement), so nothing streams from host RAM. The runners take the placement as `--placement single|multicard` (`comfy-generate.mjs`,
`comfy-render.mjs` and `comfy-edit.mjs`), `--resident-gb N` (the DiT's share kept on the compute card; only a split recipe has one) and `--cards <a>,<b>`
(only outside a lease; under one it can only confirm the lease's cards): the Wan flags without the `wan-` prefix, with the same refusal of the `=`
spelling (`--placement=multicard` is a refusal, not a silent single). The roles are the Wan placement's rule, from the same planner: two cards within 1 percent of one size are
ranked by board power, and `WAN_COMPUTE_UUID` (an index, a UUID or a UUID prefix of 8+ characters; see "Which card computes" above for what a token that names no card or a card outside the
pair does) names the compute card for the image and edit placements too. The typed image and edit doors pass no card flag, so this variable is the one override that reaches them: set it in the
daemon's environment (their runner child inherits the process environment) and, for a `gpu reserve ... --` command, in the shell. The recipe's peak and resident checks judge whichever card
the variable names, and the plan's line ends with `compute card chosen: <the rule>`.

| Route | Binding | DiT loader / text-encoder loader | Peak per card, compute / storage (GiB) | Basis |
|---|---|---|---|---|
| image | `qwen-image-2.1`, int8 convrot DiT, bf16 text encoder | plain pin / harness loader, built off the cards (5 GiB on the compute card) | 9.0 / 13.6 | predicted, **held back** (below) |
| image | `qwen-image-2.1`, bf16 DiT, bf16 text encoder | DisTorch2 (8 GiB resident) / harness loader, built off the cards (8 GiB on the compute card) | 11.0 / 10.1, at the 4.19 MP (2048x2048) canvas cap | predicted |
| image | `qwen-image` 2512, Q5_1 GGUF, fp8 text encoder | GGUF DisTorch2 (3 GiB resident) / plain | 4.9 / 12.6 | predicted |
| edit | `qwen-image-edit-2511`, fp8mixed DiT, fp8 text encoder | DisTorch2 (9 GiB resident), bypass LoRA / plain | 13.0 / 11.0, at the 2.0 MP canvas cap | **measured** |
| edit | `qwen-image-edit-2511`, Q5_1 GGUF, fp8 text encoder | GGUF DisTorch2 (3 GiB resident) / plain | 5.6 / 12.6, at the 2.0 MP canvas cap | predicted |

**A row can be held back, and the `qwen-image-2.1` int8 convrot row is.** It puts a comfy_kitchen int8 DiT on `cuda:1`. ComfyUI-MultiGPU's int8 dispatch path has a recorded illegal
memory access when the compute device is not `cuda:0` (upstream issue 220, unfixed; it is why the pooled LTX-2.5 seat keeps compute `cuda:0`), and the
benchmark arms the row cites ran the int8 DiT on `cuda:0` with the bf16 text encoder on `cuda:1`, the reverse of the recipe, so the placement itself never ran.
A held-back row carries a `blocked` sentence (`render/multicard-recipes.json`, `Blocked` in `internal/mediacap/multicard.go`, pinned together): it stays in the
table for the proof arm, but it is no recipe. `findRecipe` refuses it by name for every caller (the planner, the builders, the runners), `FindMulticardRow` does not
return it, so the host-RAM estimate, the doctor and the typed doors see a binding without a recipe, and the door's refusal says which row is held back and why. One int8
arm with the DiT on `cuda:1` lifts it (clear `blocked` and `Blocked`).

**The 2.1 bf16 text encoder is built off the cards.** The bf16 Qwen3-VL-8B encoder is 16.33 GiB, more than the 15.9 GiB storage card. Under `--gpu-only` ComfyUI takes the
text encoder's initial device from the text-encoder device (`cuda:0`) and builds the whole encoder there before the DisTorch2 allocation string that splits it is attached, so the stock
`CLIPLoaderDisTorch2MultiGPU` ran out of memory 1.4 s in (14.10 GiB allocated) on the one live card-only run of this row; the 13.3 GiB the row's old evidence line called the
encoder's share is the bf16 DiT file's size, not the encoder's. The row therefore names the `distorch2-host` text-encoder loader: the harness's own
`OffloadCLIPLoaderDisTorch2` (`render/comfyui-ext/nodes/offload_multicard`), which runs the stock loader with ComfyUI's one answer to "where is the encoder built" changed to the CPU,
then places it at once through DisTorch2's own load path, block by block, by the allocation `cuda:1,8gb;cuda:0,*`: 8 GiB of the encoder lands on the compute card and 8.33 GiB on the storage
card, and the node refuses to continue if more than 64 MiB of weights are left in host RAM. The encoder computes on the storage card (`cuda:0`, which also keeps its embedding blocks);
the share parked on the compute card is streamed across block by block at encode time, so the compute card runs no encoder activations. Host RAM is used transiently, at load time only:
the encoder is staged whole on the CPU while it is built and filled (16.33 GiB, read through the read-only map, released when the placement ends) and no weight stays there during
sampling. The recipe relies on the encoder leaving both cards when its last text-encode node completes (`--cache-none` drops it), before the sampler lands the DiT's share on the compute card; the live proof render has to confirm that.
The peaks are arithmetic, not measured. At encode the storage card holds 8.33 GiB of encoder + 0.24 VAE + 0.5 context + about 1.0 activations = 10.1 GiB and the compute card 8.0 + 0.5 = 8.5 GiB;
at sample the compute card holds 8.0 DiT share + 2.5 activations at 2048x2048 + 0.5 context = 11.0 GiB and the storage card the 5.25 GiB donor blocks + 0.24 + 0.5 = 6.0 GiB plus the VAE
decode working set (not measured). The row's canvas cap is 2048x2048, the family default, chosen and not measured. The int8 convrot row names the same loader.
The launchable card-only IMAGE rows are therefore the 2.1 bf16 DiT (predicted), the 2512 Q5_1 GGUF and the krea2 bf16 DiT (both measured, below; the edit rows are unchanged). The load-time warning of
`imagegen_placement` agrees: a box that binds `qwen-image-2.1` to `multicard` is no longer told every render will be refused.

**Measured and predicted.** A row is *measured* only when its per-card peaks were read in a run that used the read-only loader. Three rows have one:
- the 2511 fp8mixed edit (2026-10-11 benchmark, section 4.6: 1.47 MP on two RTX 5060 Ti 16 GB, 145.5 s, 9.4 s per step, peaks 11.9 GiB on the compute card and
  10.9 GiB on the storage card, 1.87 GiB of host growth; one card cannot hold the model and the merging LoRA loader ran out of memory, which is
  why the recipe uses the bypass loader). A 2026-10-20 run of the lightning 8-step recipe at 1024x1024 on a 300 W + 180 W pair took 110.6 s, peaks 11.40 and 10.60 GiB;
- the 2512 Q5_1 GGUF (2026-10-20, resident 10 GiB, 0 GiB host RAM declared on the lease): 1328x1328 in 415.2 s (peaks 11.84 GiB compute, 8.26 GiB storage) and
  928x1664 in 401.3 s (11.74 and 8.26 GiB);
- the krea2 bf16 DiT (2026-10-20, 0 GiB host RAM declared): 1024x1024 at resident 12 in 82.1 s (14.30 and 12.10 GiB) and 1536x1024 at resident 11 in 68.5 s
  (13.15 and 13.10 GiB). The row's resident share is 11.

Every image arm of the 10-11 benchmark ran the stock loader, which charges each weight file to host
commit, so the two other rows keep what the arms showed (the 2.1 int8 and bf16 arms ran pixel-identical to the single-card render; the int8 row is held back) and
the split shares and peaks are arithmetic from them: they are *predicted*, and the file says so per row (`basis`, `evidence`). A live run
with the read-only loader promotes a row: flip `basis` in `render/multicard-recipes.json` and `Measured` in `internal/mediacap/multicard.go`
(`TestTheMulticardRowsAreTheRunnersRecipes` fails until both agree).

**What the typed door does.** A binding that asks for the placement takes its own need kind in the media admission (`needMulticard`):

- *The pair is named, not allocated.* The plan is the box's two non-display cards, as a set. The display card is never in it, whatever the
  display cap (the allocator may give the screen's card to a ONE-card call up to `gpu_display_cap_pct`; this is not that), and a card whose display state
  the driver did not report is not counted. A box without exactly two such cards (one, or three on a larger box) answers `gpu_lease_unavailable` with the
  count and the way out (`local-offload gpu reserve --devices <card>,<card> -- <command>`), and runs nothing.
- *It queues like any lease.* A held card of the pair is a place in line whose token names both cards (`gpu_queued`, a capacity defer), never a run on
  the one that is free. Two multicard calls serialize, and a multicard call and a single-card call never share a card (the in-process slots are
  set-based). The batch takes one pair for the whole batch.
- *It is never degraded.* A host that does not lease cards (`gpu_card_scoped_leases` off, or no green reader audit), an unreadable card table and a
  lease directory that will not open each answer `gpu_lease_unavailable` with the reason; the call does not fall back to the whole node or to the
  default instance, where the runner would refuse a call that names no cards, every time, after the node was held for it. A router may place such
  a call on another node.
- *No recipe, no lease.* A binding that has no row in `render/multicard-recipes.json` (krea2 in any file but the bf16 DiT with the bf16 text encoder; 2512 bf16, 38 GiB, which
  fits no card; sdxl; hidream; the 2.1 edit; an int8, int4 or nf4 text encoder, whose render moved off the bf16 one by 28.65 dB PSNR against the 30 dB
  gate; the 2.1 int8 convrot DiT, whose row is held back, so the reason names the row and says why) is refused before any lease is spent, naming
  the key. The same goes for an edit binding whose
  `gen_edit_megapixels` pins a canvas above the recipe's 2.0 MP cap, which the runner honours verbatim: the peaks are scaled to the cap, and a larger canvas
  has no measured or scaled figure. The runner refuses the same bindings by name, as a second line of defence.
- *A canvas above the image row's cap is refused before any lease.* The 2512 Q5_1 GGUF row carries `maxPixels: 1769472` (1536x1152, 1.77 MP, `MaxPixels` in
  `internal/mediacap/multicard.go`, pinned to the JSON by `TestTheMulticardRowsAreTheRunnersRecipes`), 0.3 % above the larger of its two measured canvases
  (1328x1328 and 928x1664, both at resident 10; an earlier 928x1664 run at a larger resident share ran out of memory in the sampler). The krea2 bf16 row's cap is
  1572864 (1536x1024, measured). The door judges the request's own `width` and `height` (`mediaNeed.forCanvas`, `internal/pipeline/mediamulticard.go`), a side it
  leaves out being the family default (1328 for qwen-image), so the default-size 1328x1328 call fits the 2512 row. The refusal names the cap, the asked size and the way out (a smaller width and height, or unset `imagegen_placement` for the single-card
  placement), and nothing was leased. The runner holds the same cap (`canvasRefusal`, `render/multicard-recipes.mjs`) after the pair was held, as the second line. The edit
  rows have no pixel cap, only the 2.0 MP edit canvas above; the batch door does not judge its jobs' sizes, the runner refuses an over-cap job on its own.
- *A card-only call with no placement is refused up front.* A runner started under `gpu reserve --devices <a>,<b>` without `--placement multicard`, aimed at a
  non-default ComfyUI endpoint that nothing answers on, ends with `COMFY-PLACEMENT-MISSING` (`ensureComfy`, `render/comfy-lifecycle.mjs`) instead of a
  start-it-yourself line: it names the two leased cards and `--placement multicard`, the one flag that keys the pair's own instance and launches it. An endpoint that answers
  is still reused, and nothing is launched in the refused case.
- *The runner gets the lease's cards and nothing else.* `--placement multicard` and `GPU_LEASE_DEVICES`; no `COMFY_CARD_UUID` or `COMFY_API` (the pair has
  an instance of its own, keyed by the two cards and shared with the Wan placement), no `comfy_cuda_device` pin and no dynamic VRAM (both contradict two
  cards under `--gpu-only`; the runner would drop them for the launch anyway), and no `--pool-*` (a pool is another placement). The operator's
  `comfy_extra_args` are theirs and stay.
- *The finished answer names the pair.* A generate-image, edit-image or batch that ran on the pair adds `devices` to its success answer: the two lease ids,
  sorted, the key and spelling the queued answer already uses (`TestAMulticardImageBindingLeasesTwoNonDisplayCardsAndPassesThePlacement`). Under a parent
  lease they are the parent's two cards. A batch carries them on every item it rendered here, so the item `generate-image --batch` prints has them too. A call
  that did not hold the pair (the one-card lease, a pool that holds two cards, a named family without the key, `single`) adds nothing: its answer is byte for
  byte what it was.
- *Under a parent lease* (`local-offload gpu reserve --devices <a>,<b> -- <command>`) the call runs inside it on both cards, if the lease holds exactly two
  non-display cards of this box; one card, the whole node and a lease that includes the display card are refused with the reason.
- *The lane probe and the place-in-line keeper read the same named plan*, so a router sees a held card of the pair as a busy lane, and the differential
  table of lane states (`TestMediaLaneFreeNeverRefusesAGrant`) covers the card-only states.

**The resident share moves bytes both ways.** `--resident-gb N` keeps N GiB of the DiT on the compute card and the allocation string sends the REMAINDER to
the storage card, so the DiT's total is the invariant (`recipePeaks`): a larger share raises the compute card's peak, a smaller one puts the same bytes on the
storage card, whose room is the thinner. For the fp8mixed edit (19.1 GiB DiT, 9 GiB resident, 11.0 GiB on the storage card) `--resident-gb 5` is 15.0 GiB there
against the 14.9 GiB a 16 GB card has left after the reserve, and the whole DiT on one card ran out of memory at load in the benchmark; the runner refuses such a
split naming the storage card and the figure, and the Go declaration keeps the ordinary estimate (the Wan estimator already computes it this way). A larger share
does not lower the storage figure, since no run measured that (the 2.1 bf16 row, whose storage peak is its text-encoder stage, would be refused somewhat
early on a smaller share, on the conservative side).

**What the runner checks, and watches.** Before it submits, `verifyImagePlacement` compares ComfyUI's device list with the pinned pair and asks `/object_info`
for the harness's pack and for the recipe's own loader classes: a class the pack does not register is `MISSING_NODE` with its pack hint, an answer it cannot get
is a refusal, and a loader that no longer declares an input the recipe sets (ComfyUI drops an undeclared input without a word, so a renamed
`expert_mode_allocations` would run with no split at all) is `IMAGE-PLACEMENT-NODE-INPUT`; the graph's classes, the LoRA loader included, are preflighted just before
the POST. A pinned edit canvas above the recipe's cap is a usage error before anything starts. While the prompt runs the poll is handed a watch on the pair's
own ComfyUI console, made before the submit (the Wan runner's: `comfyLogCursor`, `createOomWatch`): an out-of-memory error inside DisTorch2 that kills the prompt
worker leaves `/history` empty and `/system_stats` answering, so without it the wait would run to the end of its budget with both cards leased; the watch ends it
at once with `IMAGE-PLACEMENT-OOM` (the console line, the recipe and its per-card peaks, the restart). When the prompt finishes the same console says whether
ComfyUI retried the VAE decode tiled after an out-of-memory error (`Ran out of memory when regular VAE decoding`): such an image is not the single-pass decode
the recipe was measured on and can show seams, so it is kept beside the requested path as `<name>.tiled.<ext>` and the run fails with `IMAGE-DECODE-FALLBACK`;
a console that cannot be read to the end (missing, or its 5 MB capture capped) says `IMAGE-DECODE-UNVERIFIED` and delivers, since unknown is not a fallback. A
run with no placement is exactly what it was: no preflight, no cursor, no watch.

**Host RAM.** The declaration is sized on the PAIR, not on the largest card (`hostneed.estimatePlaced`, `Facts.CardsGiB`). A *measured* recipe whose per-card
peaks fit the two leased cards, each less the runner's `--reserve-vram`, declares 0, and says so: the declaration carries a `CardOnly` mark, and both the
pipeline's log (once per key) and `gpu reserve` print it with its reason ("measured with the read-only safetensors loader ... so no weights stream from RAM"),
because nothing can correct a 0 afterwards (`calibratedRAM` leaves 0 alone) and a false one would stream silently. A card-only image run records its VRAM peak and
host growth under its own footprint key (the quant carries `+multicard`), not the single-card one. A call that pins an edit canvas above the recipe's cap, a lease of any number of cards but two, a recipe that does not fit,
a *predicted* row and a binding with no recipe keep the ordinary streaming figure, with the reason in the declaring line. Declaring the figure of a row
nobody has measured would admit on a guess; this errs toward waiting for the host's room. `gpu reserve` around `comfy-generate.mjs`, `comfy-render.mjs` or
`comfy-edit.mjs` with `--placement multicard` reads the same recipe (`Facts.Placement`, `--resident-gb`, `--megapixels`).

**Not measured, said once.** The int8 convrot DiT on `cuda:1` has not been run at all (the row is held back), and the bf16 row has been run once, card-only, with the stock text-encoder loader, and
did not reach the sampler (the encoder was built whole on the storage card); the harness's own loader that replaces it (`OffloadCLIPLoaderDisTorch2`) has not run yet, so where the encoder's
bytes land, the host RAM its staging takes and the two peaks are the first things a live proof render of the row has to read; the out-of-memory watch, the tiled-decode verdict
and the loader-input check are tested against fakes and the benchmark's log lines, not against a live wedge; the typed edit door records no footprint or host peak
(the single-card edit route never did), so nothing yet cross-checks a measured zero against the host's real growth. The read-only reader mapped the bf16 text encoder in that one live run (750 tensors, 16.33 GiB), but nothing has run it past the
build (the other tests use fakes of the loaders);
the installed ComfyUI-MultiGPU is a newer commit than the one the benchmark pinned (the loader classes the recipes name are present, checked by name);
the 2512 Q5_1 peaks are arithmetic (the 2.1 bf16 row's split shares are too); the port fallback for a pair instance with neither `--api` nor `COMFY_API` is untested; the lane
probe answers for the image route, not for the edit route; and a box with three non-display cards is refused until a pair is named with `gpu reserve --devices`,
because which two of three is a measurement nobody made.


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
patched code, not just the (soon read-only) upstream URL. The same pin (upstream and mirror) is a row of
`render/node-packs.json`, the table a run-graph manifest's `node_packs` is checked against (a pack is taken only at a
pinned repository and commit; see the [manifest flow](../flows/run-graph-manifest-satisfaction.md)). All four fleet Windows/Linux ComfyUI
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
(`imagegen_vae` must not be `"builtin"`; steps/cfg together or neither). **Card-only on two
16 GB cards (the placement to use there):** `imagegen_placement: multicard` with the bf16 DiT and the bf16
text encoder takes the `krea2/bf16` recipe row (11 GiB of the DiT resident on the compute card, the rest on the
storage card, nothing in host RAM; measured 2026-10-20, see "Measured and predicted" above). The bf16 DiT's file
carries plain bf16 tensors and no quantization marker, so stock ComfyUI would build it whole on the offload card
(the storage card under `--gpu-only`) and run out of memory there before the split applies; the harness's lazy
build (`offload_multicard` part 1) builds it empty and fills it from the read-only map instead. The pool keys
below are the older ratio-mode path. **Pooling (the
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
(`generate-image --batch [--family F]`) renders ONE binding, the default or the family named, so every
batch item — and the batch payload's top level — carries that binding's `family` and, when declared, its
`license` and `commercial_use`. `width`/`height` in a
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
`generate-image --batch --family F` renders every job under that family (`RunImageBatchAs`: the family's checkpoint, card pin,
timeout and license, and the lease asked for on the family's card); a family on stable-diffusion.cpp has no warm session and is refused
by name, and `--transparent` with `--batch` is refused (an alpha channel is one render's).

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
  better at 2K; the binding picks. `turbo` = the Qwen-Image-2.1-Turbo checkpoint's saved 8-step list
  (`model_index.json` `sample_sigmas`: dynamic shifting off, shift 1.0, no terminal stretch — fed through
  `ManualSigmas` unchanged, terminal 0 appended) and refuses any step count but 8; bind it as its own named
  family on the Turbo weights (`qwen_image_2.1_turbo_bf16` / `_int8_convrot`, 8 steps, cfg 1). The 2.1 edit
  graph takes `gen_edit_schedule` `comfy` (default: `KSampler`) or `turbo` (the same custom-sampler wiring on
  the cached model and the encoder's latent); it has no size-dependent `official` mode because its latent is
  the encoder's.
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
| `comfy_extra_args` | `COMFY_EXTRA_ARGS` | verbatim extra flags; `""` = inherit the process env, as before. A `--cuda-device`/`--default-device` here loses to `comfy_cuda_device`. `--use-sage-attention` (or an argparse prefix of 8+ characters) here changes the card-only Wan sizing: its activations are 15.2 GiB at film size, so the default `--wan-resident-gb` is refused and a sage film call passes 0 ("Sage attention", above) |

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
| launch flags | `--port`, `--output-directory <comfy dir>/instances/<key>/output`, `--temp-directory <comfy dir>/instances/<key>`, `--database-url sqlite:///<comfy dir>/instances/<key>/comfyui.db`, before the extra args | none of these |
| database | its own sqlite file, `<comfy dir>/instances/<key>/comfyui.db`, so no two instances lock each other | ComfyUI's default: `comfyui.db` in the user directory |
| launch marker | `.offload-launch-<key>.json`: pid, the spawning process, argv, profile, key, port and the lease epoch (`GPU_LEASE_EPOCH`) when launched under a lease | `.offload-launch.json`, unchanged |
| console log | `offload-comfyui-<key>.log`, rotated on its own | `offload-comfyui.log` |
| input directory | shared; runners stage under names unique across processes (clock, process id, random suffix, sequence number: `render/comfy-input.mjs`) | shared |

A `--port`, `--output-directory`, `--temp-directory` or `--database-url` in `COMFY_EXTRA_ARGS` is dropped (with a
`COMFY-INSTANCE-WARN:` line) for a keyed instance, because the last occurrence wins and would put it
back on a shared port, folder or database. The default instance's extra args are passed through
untouched, `--database-url` included.

**Why one folder per instance, outside `temp` and `output`.** ComfyUI appends `/temp` to the value of
`--temp-directory` itself, so the temp directory a keyed instance really uses is
`<comfy dir>/instances/<key>/temp`. The default instance deletes `<comfy dir>/temp` on every start
(assets disabled, the harness default), and the harness launches the default instance per job, so a
keyed temp directory nested under `<comfy dir>/temp` would be wiped whenever the default instance
started. A test pins that no keyed directory is inside the default instance's `temp` or `output`
tree, and that the directories the launcher creates are the ones ComfyUI really uses.

**One database per instance (rollout finding 8).** Read from ComfyUI's own parser and startup code
(`comfy/cli_args.py`, `app/database/db.py`, `main.py`, v0.38.0), not guessed:

- *The flag.* `--database-url`, `type=str`, `default=None`: "Specify the database URL, e.g. for an
  in-memory database you can use 'sqlite:///:memory:'. Defaults to 'comfyui.db' in the effective user
  directory." With no flag the file is `<user directory>/comfyui.db`, so every instance on a box shares
  it. A keyed instance gets `sqlite:///<comfy dir>/instances/<key>/comfyui.db`, in its own folder
  beside `output` and `temp` and inside neither (a temp wipe or an output scan cannot reach it). The
  URL has the form ComfyUI builds for its own default and splits again in `get_db_path`.
- *The lock.* `_init_file_db` takes a file lock on `<db>.lock` before it touches the file and keeps it
  until the process exits (the OS drops it when the process dies, so a killed instance leaves no stale
  lock). A second process logs "Database lock is held; waiting up to 5s for it to be released", waits
  5 s, then raises "Could not acquire lock on database", and `setup_database` logs "Database is
  locked. Another ComfyUI process is already using this database.". With assets off (the harness
  default: the launcher never passes `--enable-assets` and no seeded config sets it) it then returns
  and the server starts without a database; with `--enable-assets` it exits with status 1. So the
  shared file cost every instance after the first 5 s at start and its database, and nothing else:
  the renders completed, with three instances up side by side. The console logs of keyed instances on
  the 3-card box show it (2026-10-03): the two lines 5 s apart, then start-up carrying on, which also
  proves assets were off there, since `--enable-assets` would have exited. It would have been a failed
  launch the day assets are enabled in `COMFY_EXTRA_ARGS`.
- *What uses the database.* Only the asset system (`app/assets`), through five tables: `assets`,
  `asset_contents`, `tags`, `asset_tags` and `asset_system_state` (the stored hash mode). With assets
  off (the harness default) it is touched only at start-up, which checks the stored hash mode and
  writes it the first time or when it changes, and at shutdown, which wipes the temp rows. With
  `--enable-assets` it is also the catalogue behind the asset routes (`/api/assets...` and
  `/api/tags`; each answers 503 while assets are off), the background seeder and scanner over models,
  input and output, the registration of uploaded, executed and cached outputs, and
  `/view?filename=blake3:<hash>` (404 while assets are off). Rendering, the queue, workflows,
  settings, the API nodes and ComfyUI-Manager do not use it: a search of the core, the API nodes and
  the installed custom nodes found it only under `app/assets`, in the asset-manager calls of
  `main.py`, `server.py`, `execution.py` and `comfy_execution/asset_enrichment.py`, and in one
  ComfyUI-Manager end-to-end test, which launches ComfyUI with a database URL of its own. With assets
  off the lock therefore disabled no feature the harness uses; the change removes the 5 s start-up
  wait and the error, and makes `--enable-assets` usable on a keyed instance.
- *A user directory of its own: not needed, and left shared.* (1) `--user-directory` is
  `type=is_valid_directory`: the path must already exist or ComfyUI exits at argument parsing, while
  `--database-url` is a plain string and `prepare_file_db_path` creates its folder. (2) The user
  directory holds more than the database: `users.json`, the per-user settings and workflows, and
  ComfyUI-Manager's configuration (`__manager/config.ini`: security level, network mode and the
  like). A new directory per instance would start each one with none of that, Manager at its defaults
  instead of the operator's, for no gain: the database is the only process-exclusive file there
  (Manager's console log is already named by port, and the harness drives ComfyUI through the API,
  not the front end that writes the settings). (3) With `--database-url` given, `copy_legacy_default_db`
  returns early, so a keyed instance never touches the shared `comfyui.db` or its `.bkp`, `.lock`,
  `-wal` and `-shm` files. Revisit only if a custom node is found that keeps process-exclusive
  state in the user directory.
- *Upgrade.* A running instance keeps the argv it started with, so one launched before this change
  keeps the shared database until its next launch, and it is still reused: the database flag is not
  part of the ownership check. The first launch of a keyed instance builds its database by running
  the migrations (eight; about 0.5 s measured); later launches reuse the file. A ComfyUI whose parser
  predates `--database-url` would reject the flag (argparse exits with status 2) and not start a
  keyed instance. The flag is in the v0.38.0 parser this was read from; that checkout is shallow, so
  older parsers were not checked.
- *Verified.* It was first run against ComfyUI's own code in scratch folders (CPU only: no server,
  port, card or model). Its
  argument parser accepts the keyed argv `launchFlags` builds, and exits with status 2 on a
  `--user-directory` folder that does not exist while accepting a `--database-url` in folders that do
  not. Its `init_db` (v0.38.0, SQLAlchemy 2.0.54), run on Windows for two keys, the second started
  while the first held its lock, came up in about 0.5 s each on two different files (`comfyui.db`,
  `-wal`, `-shm` and `.lock`); run twice on one file, the second waited 5.05 s and failed with
  "Could not acquire lock on database". Then live (2026-10-20, one Windows node, `ensureComfy` with
  `--cpu`, since the lock is taken at start-up before any device work): two keyed instances, the
  second started while the first was up. Under the previous `launchFlags` the second logged
  "Database lock is held; waiting up to 5s" and then "Database is locked. Another ComfyUI process is
  already using this database."; under this one each logged "Database upgraded from None to
  0008_drop_asset_meta", neither logged the lock, and each had its own
  `<comfy dir>/instances/<key>/comfyui.db`.

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
  instance lives no longer than its lease, which bounds it, and the idle pack below writes one line into it per free. `tailComfyLog` (the lines a failure message
  carries) reads only the last 256 KB of the file, so a long-lived instance's log is never read whole.
- **It is freed when its family changes (2026-10-09, `render/comfy-family.mjs`).** ComfyUI keeps the models it loaded in
  host memory between prompts ("Using RAM pressure cache"; "Model Krea2 prepared for dynamic VRAM loading. 24449MB Staged" in
  the instance's own log) and drops them only when told to; an instance that served one family and is handed a job of
  another holds both. The incident's instance held 57 GiB private, probably that way (a Qwen-Image model cached next to a
  Krea 2 one: reconstructed, not observed; the session that handled it reported that POST `/free` released 52 GiB between
  prompts without killing a job, a reading no record of which survives here). A ComfyUI process launched fresh on 2026-10-10
  held the same 57.7 GiB private about seven minutes after launch (a reading on the reference box, from a session, not recorded in
  this repository), so this fix may account for only part of the figure and the
  footprint of one lane is unmeasured. The ways two families could come to share an instance, read from the code and the
  instances' logs: a runner killed before its `finally` (the pipeline's timeout kills the whole tree) never sent its
  end-of-run `/free`, and the post-run `/free` of `gpugen` waited one second; the holder's proof before stopping a kept
  instance at release (`GET /system_stats`, three seconds) failed on an instance busy with a 550-second prompt, so it
  outlived its lease; the next lease reused it and loaded its family beside the first; and `run-graph` left whatever its
  graph loaded on an instance that was already up. What changes: every ComfyUI runner passes a **family signature**
  (the family plus the weights file, so a Q5 GGUF and a bf16 safetensors of one family count as two) to `withGpuSlot`;
  the instance's launch marker records it (`lastFamily`); a runner that finds a kept instance whose marker names ANOTHER
  family frees it, awaited, before its first job (`COMFY-FAMILY-FREE`), and the same family keeps its warm weights; the
  end-of-run `/free` (both flags, always, kept or not) is awaited, retried once and loud when an instance that is
  listening did not acknowledge it (`COMFY-FREE-WARN`), and clears the recorded family; a free that did not succeed
  before the first job leaves the old family recorded so the next runner tries again; `run-graph` frees an instance it
  ran on only when the harness launched it (live pid, the marker's exact argv); the holder's proof is asked three times of
  eight seconds; `gpugen`'s post-run `/free` waits five seconds and says a failure that reached an instance. Tests run
  against a fake ComfyUI that records the order of `/free` and the job (`render/comfy-hoard.test.mjs`).
- **Who stops it.** A kept instance lives no longer than the GPU lease it was launched under; its marker
  records that lease epoch (`leaseEpoch`). The **holder of the lease stops it on release**: `gpu reserve`
  when its wrapped command ends, and the pipeline when its media lease is released
  (`internal/comfyinst`: the marker is matched to the epoch, the instance is shown to be the harness's by pid
  and exact argv on its own endpoint, freed with `POST /free`, then stopped). A kept **default** instance
  (no key) records no lease epoch and is stopped by whoever kept it, as it always was. That bounds the
  process. What it keeps in models and caches is bounded sooner, by the instance itself (next bullet).
- **A kept or warm instance frees its own models and caches after 300 s idle (0.186, `render/comfyui-ext/idle-nodes/offload_idle`).**
  The rule is that no idle model stays loaded. llama-swap keeps it with its own `ttl`; ComfyUI has no idle timer (its parser takes no
  idle or timeout argument). The runner's end-of-job `POST /free` and the holder's stop at release cover the ordinary path. The harness
  also launches every ComfyUI with `--disable-smart-memory`, and with that flag ComfyUI itself calls `unload_all_models()` when a prompt
  ends, on success and on a handled error (`execution.py`, after the node loop; read in ComfyUI 0.38.0, not re-read on other nodes'
  versions). So a kept single-prompt instance (`--cache-none`) is already bare between jobs, with or without the runner's `/free`, and the
  pack finds nothing to release there: it does not fire and logs nothing. What survives that unload, and what the pack is for, is (a) a
  warm (`--batch`) session's executor cache, which keeps the loader outputs (the weights the unload moved to host RAM) until a free
  resets the executor, so a runner killed before its cleanup or a warm session that stalls holds host RAM (the 2026-10-09 incident above,
  `render/comfy-family.mjs`, was an instance with the cache on: "Using RAM pressure cache" in its log, 57 GiB private); and (b) a hand-started
  instance launched without `--disable-smart-memory`, which keeps its models loaded in VRAM after a prompt. That is the gap, and the
  bound for it lives inside the engine, the way llama-swap's `ttl` does: the harness runs no timer or watcher and acts on no other
  process (plan I5), and the pack only ever acts on its own ComfyUI. This is read from ComfyUI's code and checked against a fake queue;
  **it has not been run inside a live ComfyUI** (the live check below is the way to do it).
  - *Where it is loaded.* A launch with `keep` or `warm` (`--keep-comfy`, `--batch`) adds
    `--extra-model-paths-config render/comfyui-ext/extra_model_paths_idle.yaml` once, before the operator's own args, and puts the pack
    `offload_idle` INTO the custom-node whitelist ComfyUI will read (`--whitelist-custom-nodes` is `nargs='+'` with no append, so a
    second flag would replace the multicard packs, and `--whitelist-custom-nodes=a b` reads `b` as a stray argument: that spelling is
    rewritten to the space form). With `--disable-all-custom-nodes` and no whitelist one is added; with neither, every custom node
    loads, the pack among them. It is added only when the yaml and the pack file exist (ComfyUI exits at boot on an
    `--extra-model-paths-config` file that does not exist): otherwise one `COMFY-IDLE-WARN` and a launch without it. The default
    single render, which its runner kills, is byte for byte what it was. The pack sits in its own folder, apart from
    `nodes/`, so an ordinary kept launch never imports the fail-closed Wan pack.
  - *What it does.* One daemon thread polls every 5 s. A poll is busy when a prompt is queued or running, or `task_counter` moved (a
    prompt that began and ended between two polls); both are read under the queue's mutex. After 300 s of polls that were not busy,
    and if the engine holds something (a loaded model, or the executor cache is on, which is the warm case), it sets the two flags
    `POST /free` sets, `unload_models` and `free_memory`, in one hold of that mutex after re-checking that nothing is queued, once per
    busy period. ComfyUI's own worker thread drains them (unload all models, reset the executor, collect garbage); the pack never
    touches a model. A prompt that lands in the microsecond after the check runs first and the flags drain after it: one avoidable
    reload, never a corrupted run. A running prompt, even a 550 s one, is never idle, and the gaps inside a batch are seconds, so
    back-to-back work is not affected; a driver that pauses more than 5 minutes between items pays one reload, which is the rule and
    not a regression. Under `--cache-none` an instance that holds no model (ComfyUI's own end-of-prompt unload ran, as did the runner's
    `/free`) does not fire at all; with the cache on (a warm session) it fires once after the last prompt, whatever the models are
    doing, and logs `idle bound reached` even when the runner's `/free` already reset the cache and the free has little left to release.
  - *Settings.* `COMFY_IDLE_FREE_SEC` on the runner: unset is 300, `0` or `off` is none (one `COMFY-IDLE-OFF` line), a whole number of
    seconds from 5 up is that; anything else is 300 with a `COMFY-IDLE-WARN`. It reaches the pack as `OFFLOAD_IDLE_FREE_SEC` and is
    fixed when an instance is launched: a runner that reuses a running instance cannot change it. `OFFLOAD_IDLE_POLL_SEC` (1 to 30,
    default 5) is the test seam.
  - *Knowing it is live.* The pack serves `GET /offload/idle` (`limit_sec`, `idle_sec`, `armed`, `freed`, `last_free_unix`, `error`),
    read-only. After a launch with the pack the runner asks once: an answer that reports the bound that was asked for is one
    `COMFY-IDLE` line; no answer, an `error` (the pack could not find ComfyUI's queue after an upstream change), or a `limit_sec`
    other than the one asked for (the pack never read `OFFLOAD_IDLE_FREE_SEC`) is one `COMFY-IDLE-WARN` and the render goes on. The bound itself
    does not depend on the route, and a failure inside the pack is logged once as `[offload_idle] NOT ACTIVE` and never fails a job.
  - *Reuse and hand-started instances.* A running instance reused under `keep` or `warm` whose argv lacks the idle yaml (hand-started,
    or kept by an older build) is reused, never restarted or refused, with one `COMFY-IDLE-WARN` per runner start (a driver that starts
    one runner per shot says it once per shot); it costs one `GET /system_stats` (up to 8 s on a slow instance, shared with the ownership
    proof of a keyed instance, never asked twice); an argv ComfyUI does not report is unknown and says nothing. The harness does not inject into an instance it does not own (ADR 0009): to bound one, add
    `--extra-model-paths-config <repo>/render/comfyui-ext/extra_model_paths_idle.yaml` to its launch, and the pack name to its
    `--whitelist-custom-nodes` if it disables custom nodes ([operator guide](../OPERATOR-GUIDE.md)).
  - *What it does not bound.* The models and the executor cache only. The Python process and its CUDA context (it idles at 2.9 GiB
    private, above) live until the holder stops the instance at release, and an instance whose holder died lives until an operator
    stops it. A self-exit from inside ComfyUI is deliberately not built: a runner in a long step that is not ComfyUI's (ffmpeg, a QA
    pass, an LLM call) looks the same as an abandoned instance, and a stopped instance breaks that runner's next prompt with a refused
    connection where a `/free` only costs a reload. A pack-side check of the lease epoch is unsafe as well: reusing an instance
    re-stamps its marker to the new epoch (`restampLaunchOwner`) while the child's environment keeps the epoch it was launched under.
    A sweep of markers whose epoch is dead, if one is wanted, is a separate slice (an explicit `gpu doctor` action, which plan I5
    allows where it forbids a watcher). So the register rows that say a kept instance lives to the end of its lease stay `partly`:
    models and caches are bounded, the process is lease-bounded.
  - *Live check, not yet run on a card (the tests use a fake queue).* On a node with ComfyUI installed, as the operator, one lease per
    case so the wrapped command does not end between steps (`local-offload gpu reserve --class media -- sh -c '<the sequence>'`),
    with `COMFY_INSTANCE=idlecheck` in the environment so the instance is keyed and its holder stops it at release (a kept default
    instance is stopped by hand). Two instruments, because the cases show in different places: per-card memory
    (`nvidia-smi --query-gpu=memory.used`, `torch_vram_free` in `/system_stats`) for VRAM, and the private bytes of the process that
    listens on the instance's port (`(Get-Process -Id (Get-NetTCPConnection -LocalPort <port> -State Listen).OwningProcess).PrivateMemorySize64`)
    for host RAM. Per-card memory alone cannot show the warm case: the cache lives in host RAM.
    - *A. The premise, and the no-op.* `node render/comfy-generate.mjs o1.png "a red cube" --keep-comfy`. Read per-card memory at once:
      it is already the bare context, because ComfyUI unloaded the model itself when the prompt ended. A model still resident means
      the premise does not hold on this ComfyUI version, and the pack's first case is back. `GET /system_stats` carries the idle yaml in
      `system.argv`, the instance's log has `[offload_idle] idle bound 300 s`, `GET /offload/idle` answers. Wait 330 s: no `idle bound
      reached` line and `freed` stays 0, because the gate found nothing to free (the expected no-op).
    - *B. The warm case, the one the pack is for.* `node render/comfy-generate.mjs --batch jobs.jsonl --keep-comfy` with two small jobs:
      a warm launch (no `--cache-none`) that carries the pack and stays up; the batch's own `/free` empties the cache at its end. Then
      `node render/comfy-render.mjs o2.png "a red cube" --no-lifecycle` against that instance (it skips the runner's `/free`, so the
      executor cache holds the loader outputs again). Read the process's private bytes at t+0 (the checkpoint's weights are in host
      RAM), at t+240 s (unchanged, no fire line) and at t+330 s (down by about the checkpoint's size, `idle bound reached` in the log,
      `freed` 1 in `GET /offload/idle`). If the line and `freed` are there and the private bytes did not fall, the free was asked for
      and the allocator kept the pages: record that as a finding about the reading, not about the pack. Render again in the same
      lease: it reloads and renders.
    - *C. The smart-memory case (a hand-started instance).* Start ComfyUI by hand without `--disable-smart-memory`, with `--cache-none
      --extra-model-paths-config <repo>/render/comfyui-ext/extra_model_paths_idle.yaml`, in a lease of its own. Render once with
      `comfy-render.mjs ... --no-lifecycle` and read per-card memory at t+0 (the model resident), t+240 s (unchanged) and t+330 s
      (down to the bare context, `idle bound reached`). The control is the same instance started without the yaml: it stays resident,
      and a kept or warm runner that reuses it prints exactly one `COMFY-IDLE-WARN` and does not restart it.
    - *D. The rest.* A 3-job `generate-image --batch` is unchanged and does not fire inside a gap under 300 s (re-run the S3 acceptance,
      because a warm launch now carries the pack). `COMFY_IDLE_FREE_SEC=60` shortens the bound; `0` gives no flag and no line. On a Wan
      pair, a kept multicard instance idle for 330 s with both cards read before and after settles whether `/free` returns VRAM under
      `--gpu-only`, where ComfyUI's own unload frees nothing and a weight leaves the cards when its patcher loses its last reference
      (the executor reset in `free_memory` should drop the cached ones; unverified). Release each lease: the holder still stops the
      instance (marker cleared, no ComfyUI process left). Record the node, the ComfyUI version, the times and the readings on the
      register row.

**Verifying a pin on Windows.** The driver reports no per-process rows for graphics-mode (WDDM)
cards, so confirm that an instance is on its card by per-card memory deltas, not process ids.

| line | meaning |
|---|---|
| `COMFY-INSTANCE-INVALID: …` | a key, uuid, port base or index that cannot name an instance, or a keyed instance on the default port |
| `COMFY-INSTANCE-CONFLICT: …` | an instance key (explicit, or a card uuid) and a device index both given |
| `COMFY-INSTANCE-WARN: …` | extra args that would override the instance's pin, port or directories were dropped, or an instance with no card pin was launched |
| `COMFY-PORT-TAKEN: …` | the instance's port is held, on an address ComfyUI will listen on, by something that is not ComfyUI; nothing was launched or killed |
| `COMFY-PROFILE-MISMATCH: …` | a ComfyUI answers on the instance's port but is not shown to be that instance (or is on the wrong card); refused, and stopped only when the harness's own marker proves it is the harness's and its spawner is gone |
| `COMFY-GPU-LIST-WARN: …` | `nvidia-smi -L`, which counts the cards for a Windows launch (`cudaVisibleEnv`), did not answer in time: the first line says it is being asked once more under a longer timeout, a second line that it ran out again and the launch keeps ComfyUI's Windows default of card 0 only (a pooled graph then fails validation) |

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
| any call on a host that does not lease cards, or whose card table cannot be read at admission (after one retry) | the whole node | the whole node | exactly as before this change, and the answer of a call that must wait says why it waits for the node ([below](#a-card-table-that-runs-out-of-time-is-not-a-refusal)) |

**Host RAM.** A generation call also declares the host RAM its weights will stream from (the UNet or checkpoint and the
text encoder of its binding, for a video call the family's binding with the request's own `transformer` put over it, counted
in full when together they do not fit the card it runs on; 0 when they fit; the media
class default for the audio.cpp lanes and for a `run-graph` (or a helper's `--graph`) whose graph cannot be read or sized: the largest
ComfyUI family the box binds, an upper bound that is not their own size, and 0 on a box that binds none; a graph that can be sized
declares the weight files its loaders name, in full when they do not fit the card and 0 when they fit (the same fit rule, see
`gpu-lease.md`); **the sd.cpp lanes
declare the larger of that and the weights they park themselves** (the bound files in full when the lane's extra args carry
`--offload-to-cpu`, an estimate from their sizes until three measured runs raise it, F60), so an sd.cpp-only node's spill is inside
the guard; nothing for upscale and voice) and the grant admits it
as one figure taken for both quantities, the working set against the available memory and the commit (stated by nobody) against
physical RAM, each with what the leases already granted have yet to load and `gpu_host_ram_headroom_gib` (default 8) kept, as a brake on declared
loads (the declaration is an estimate until three measured runs raise it, and an admitted lane can still leave the box close to a line): see "Host RAM" in [gpu-lease.md](gpu-lease.md). Two Krea 2 bf16 lanes, each
24.48 + 8.27 GiB, on a host that fits one: the first is admitted, the second finds idle cards and waits in the same line with
`waiting for host RAM: needs 32.8 GiB, only X GiB of Y GiB physical is available, Z GiB headroom` (or `needs 32.8 GiB, committed X of Y GiB physical, Z GiB headroom` when the commit charge is what refused; a queued place in line for a door that
can resume, a busy defer for the rest) until the first is done
(`TestTheSecondLaneWaitsWhileTheFirstIsStillLoading`). A need no state of the host admits is `gpu_lease_unavailable`, naming
`--ram` and the headroom key.

**The allocator and the display card.** An unpinned single-card call takes the allocator's card
(`gpu_lease`, "The allocator"): not claimed by a live lease, not promised to a caller waiting in line
(below), not held by another job in this process, with the free VRAM and host RAM. The display card is the one whose
`display_active` reads Enabled **or** whose `display_attached` reads Yes (`gpuprobe.ScreenCardUUIDs`, the card table's rule);
it is auto-assigned at any presence, but only while the VRAM in use plus the call's footprint stays within
`gpu_display_cap_pct` of it (default 60; 0 closes it; ADR 0039, amendment 2026-10-12). A media call declares no
footprint, so an unpinned call never takes it under a cap below 100: it picks among the other cards. An explicit pin may always name it (the operator's word). Ties go to a card with no resident seat, then
the cheapest eviction, then the lowest id.

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
take, which is every model of the roster except the memory stack and the models positively placed on
another card (`gpualloc.UnloadModels`, the rule `gpu reserve --devices` uses). A model is placed by `layers`,
else by the device pin of its entry in the serving config (`serving_config_path`); a seat placed on another
card stays, and a model nothing places goes (a `proxy:` seat with no `layers` entry is unloaded under any
card lease). A card call gets the variable whenever the roster was read: with no llama-swap endpoint it is `-`, and with
the roster unreadable it gets none, because nothing then says what is resident and a list built from the config alone is
never complete: the render lane keeps its own rule (every model off the memory stack) and the log says why. An unset variable
makes the runner unload every model off the memory stack, so a render on one card empties the seats placed on the others
(register C-86, F15): the price of an unreadable roster, taken because it is the safe direction. A runner that finds a lease
on cards with no list unloads every model off the memory stack and says that it had none (`render/gpu-lock.mjs`). The post-run
`/free` goes to the endpoint the runner was given.

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
was not resumed is not invented: a first-time call leaves one only when it gives up. The call's PAIR card closes with
the answer, before the door replies, and quietly: `completed` with no start and the reason in `error`, never a red
`failed` (and never "harness process exited before the job finished" over a door a client killed right after the
reply), because a place in line is the lease queue working ([pair-workloads.md](pair-workloads.md), *A held card is
not a failure*). Tokens are in `<state>/gpu/tokens`, not among the waiters (an older binary prunes any waiter
whose process stopped polling); a binary that predates them does not honour them, so on a host that mixes
versions it can take a card ahead of a token holder, which costs the holder its place and never
exclusivity. A call that holds the whole node on such a host leaves the same kind of token. A host that
does not lease cards keeps the plain `gpu busy` answer. Both answers name the lease in the way as a **lease of a class**,
the phrase `gpu status` leads with, never as the bare class word (`held by media` read as a media *seat* holding the
cards): `gpu queued: card(s) <ids> held by a media-class lease ("<reason>"); your place in line is #1 ...`, and
`gpu busy: held by a text-class lease (30s, reason "<reason>")`.

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
   the fix, and it matters on a Linux host only. When it moves it must stay its own time-boxed, best-effort
   read and never become part of the card-table read: that read is the per-device query alone, and a process
   listing joined to it would put the slowest thing `nvidia-smi` does under load on every media call's path.
5. **"Enqueue on the node daemon's job queue" (the plan's spike): not adopted, and why.** The daemon runs media
   jobs inline in the request handler: `concurrencyCapped` is false for `image-gen`, `video-gen`, `animate`,
   `audio-gen` and `run-graph` (a parked media job would hold an execution slot and starve the agent lane), so a
   dispatch to the local daemon would wait in the same per-card slots and leases this admission already waits in
   and add no queue of its own. The place in line is the token.

### A card table that runs out of time is not a refusal

A card-scoped call reads the card table at admission (to plan) and again inside its allocation (the allocator's
input, once per pick and once more at the grant-time check of a call that waited). Each read is one `nvidia-smi` exec
of the per-device query, with no process listing, under 5 s and, when it ran out of time, once more under 15 s
([gpu-lease.md](gpu-lease.md#reserving-cards-the-card-table-and-the-reader-audit-plan-p3)). On 2026-10-09 a session
benchmarking image generation, with two cards running other sessions' renders, got
`the card table: nvidia-smi: nvidia-smi: context deadline exceeded` as a `gpu_lease_unavailable` defer after 6 s,
while the calls beside it got the ordinary queued answer: the admission read had answered, the allocation's re-read
ran out its 5 s, and the raw error left `acquireCards`. A card-table read that fails, even after its retry, is never the
reason a media call is refused. What the call does instead depends on where the read failed:

| where the read failed | what the call does | what the answer and the log say |
|---|---|---|
| at admission | asks for the whole node, as it did before cards were leased | the log, on every such call; and a call that then has to wait is queued with a token whose reason reads "the card table could not be read (...), so this call asks for the whole node" |
| inside the allocation of an unpinned call (the admission read had answered) | is placed from the newest table it did read (the admission one, at worst) with the operator's screen kept closed to it: a table it could not refresh is the one thing that could misstate the display card's free VRAM. Who holds what is the lease directory's, read fresh every time, so it still takes a free card, or queues on the cards that qualify. It does not ask nvidia-smi again for the rest of the call, so a wedged driver costs one retry per call and not one per stage | the log; and a call that waits gets the ordinary queued answer (`gpu_queued`, a token, the cards it waits for) whose reason says the table could not be re-read, how it was tried and how old the table it used was |
| a call that named its cards (a `comfy_cuda_device` pin, a pool, a declared device) | does not read the table inside the allocation at all: nothing is left to choose, and what is taken comes from the lease directory, this process's slots and the places held in line (`gpualloc.Claims`) | nothing to say: it queues on the cards it named |
| a call under its parent's lease (`gpu reserve --devices ... -- <cmd>`) | reads the table to pick which of the parent's cards to run on; a table that cannot be read (after the retry) sends it to the legacy path, inside the parent's lease: the default instance, with no card of its own (a healthy table binds it to one of the parent's cards) | the log, on every such call; a call that is then refused (another job in this process holds the node) carries the same words in its busy answer; a call that runs has no answer to carry them |

The unload list a lease may take (`GPU_LEASE_UNLOAD_MODELS`) is scoped from the table the call already holds, not from a
third read after the grant: which seat sits on which card needs the card list, not a fresh reading, and a read that ran
out used to widen the list to every seat, so a render on one card emptied the seats on the others (register C-86). A model
placed on another card is no longer on the list (F15; by `layers` or by its serving config pin), and one nothing places still is.
`TestAnAllocationReadTimeoutQueuesTheCallInsteadOfDeferringIt` and its neighbours in
`internal/pipeline/mediaadmit_cardtable_test.go` pin each row; the same file pins that a degraded call places from the
newest table it read (not the admission one), and that a caller who has gone while the read hangs is not placed at all and
leaves no place in line. A call that names its cards still queues behind the place held for them
(`TestACardHeldForAQueuedCallerIsNotFreeForANewcomerThatNamesIt`): that claim is the one thing keeping it from taking a card
ahead of the caller in line, since a named plan never asks the allocator. The render helper's own card count
(`nvidia-smi -L`, `cudaVisibleEnv`) is asked once more under a longer timeout when it ran out, for the same reason: "no
listing" would leave ComfyUI on the Windows default of card 0 only, and a pooled graph then fails validation with no hint
of the cause; the retry, and a second timeout together with what the launch does instead, are written to stderr
(`COMFY-GPU-LIST-WARN`).

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
- **A bound route whose file is gone is a typed defer before the lease (CT-54).** The five engine lanes (sdcpp video, sdcpp animate,
  audio.cpp voice and music, the sdcpp image lane) read the same `mediacap` derivation the fleet node's door, `doctor` and
  `offload_status` read, after their own checks that every required key is set and before they resolve the runner script and the
  engine binary, create the output directory, mint the seed, refine the prompt or take the lease. A route that derives BOUND-BUT-MISSING
  (the config binds it and a weight, the engine binary, the runner script, ffmpeg or ffprobe is not there) defers with `err_class` `route_not_ready`; the reason names the route, the config key and the missing path, and says another
  node may serve it. Before this a local call took the media lease, spawned the runner and deferred `other` ("--model not found")
  milliseconds later, while a node answered the same condition typed (503). A required key left unset (nothing bound, or one key of
  several) is the lane's own `<key> is not configured` sentence with no class: it names the key, and it comes first. (For the video,
  animate and audio routes mediacap reads a partly bound route BOUND-BUT-MISSING, so `doctor` and the node door call it that; the sdcpp
  image route reads one of `sdcpp_bin` / `sdcpp_model` unset as NOT CONFIGURED.) The class is a config fault of the box in the closed reason set, and no
  circuit breaker counts it (the breaker judges the llama tiers).
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
| (the pipeline, before the lease) | `route_not_ready` | the route is BOUND-BUT-MISSING: the config binds it and a file it names is gone (CT-54, below) |
| `IGPU_CLASS=comfy_unusable` (the ComfyUI image runner) | `comfy_unusable` | the ComfyUI server stopped answering or answers with a broken CUDA context (D-38); the runner exits 3 |
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
| a full volume (0.178.0) | `disk_full` | `ENOSPC`, `EDQUOT` or `EROFS`: the typed errno of a Go file call, or the errno name/number in a runner's output (`ENOSPC:` from Node, `[Errno 28]` from Python; `[WinError 112]` on Windows), never the prose around it, because an exec error echoes the prompt. Both classifiers (`gpugen.ClassifyErr` and the image lanes' `pipeline.classifyErr`) share `gpugen.IsDiskFull`; a batch that stopped on a full drive was recorded as `other` before |
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

A run of the suite leaves nothing in the OS temp dir. A test that needs a folder takes it from
`scratchDir(t, prefix)` (`render/test-scratch.mjs`), which removes it when the test ends, passed or failed;
`render/test-scratch.test.mjs` fails on a new bare `mkdtemp` in a render test or in its support modules (the few
that remain are listed there with the reason each is removed on every path) and when the Windows stub launcher of
`igpu-stub.mjs` leaves anything behind. The runners' own scratch is held to the same rule: `makeTempDir`
(`igpu-engine.mjs`) removes its folder on every exit path, signals included, and sweeps what a killed runner
left; the compose work dir and the provisioning scratch of `manifest-satisfy.mjs` are removed when their step
ends, passed or failed (`igpu-lifecycle.test.mjs`, `compose-hyperframes.test.mjs` and
`manifest-satisfy.test.mjs` pin them).

A run of the suite touches neither the llama-swap nor the GPU lease of the machine it runs on (register F80). A runner a test
spawns reaches `withGpuSlot`, whose default `freeLlamaSwap` lists the models of `LLAMA_SWAP_API` (default: the machine's own), drains
every seat off the memory stack and sends a real unload; a seat that is still starting held that drain for 3 x 5 s, and a
drain that finished unloaded a seat that was serving someone. A suite that runs under `gpu reserve` also hands every runner
that lease (`GPU_LEASE_*`): the runner would fence against it and claim its once-per-lease unload marker. So a test spawns a
runner with `env: isolatedRunnerEnv({ ...what it needs })` (`render/test-isolation.mjs`: `LLAMA_SWAP_API` is a loopback port
nothing listens on, `LLAMA_SWAP_*` and `GPU_LEASE_*` are dropped; what the test passes explicitly wins), and an in-process
`withGpuSlot` names `freeLlamaSwap` and `lease` (`null`, or the lease the test means). `render/test-isolation.test.mjs` scans
every render test and fails on a runner spawn whose own call does not pass an env that closes both (each spawn is judged on its
call, so a spawn added beside an isolated one fails) and on an in-process call that names neither; its
fixtures pin what the scan catches and lets through. A test that needs a llama-swap with behaviour starts
`render/fake-llama-swap.mjs` on a loopback port (per-seat state and `/slots` answers, a request log, `close()` destroys the
sockets a hung request still holds) and hands the runner its `url`.

No child process the suite starts may open a console window. Every `spawn` / `spawnSync` call in `render/` (tests, test support
and the scripts the tests write into strings included) sets `windowsHide: true` in its options object, and
`render/no-visible-windows.test.mjs` fails with `file:line` on one that does not (and on a child_process function used as a
value, which hides its calls). A child with `stdio: "inherit"` stays on the operator's console whatever `windowsHide` says, so
Ctrl-C still reaches the attached sd-cli / tts / comfy-render runs (`render/console-share.test.mjs` measures it). The Go side
builds its children with `winexec.Command` / `winexec.CommandContext` (`internal/winexec`): the child's own window is hidden
always, and it gets `CREATE_NO_WINDOW` only when the parent has no console to hand on, so under a terminal it shares the console
and gets the operator's Ctrl-C as before; a child that must outlive its parent is `CommandDetached`, the command an operator
names after `--` in `gpu reserve` is `CommandIfNoConsole`, and the Windows cage starts its child with `CREATE_NO_WINDOW` and
`SW_HIDE`. `TestEveryChildProcessStartIsHidden` there reads every Go file of the repository that builds for Windows (tests and
the `tools/` modules included) and lists the few process starts that stay unhidden, each with its reason.
The flags are asserted by tests; the desktop itself is watched by `node scripts/window-watch-run.mjs -- <compiled test binary>`, which runs a
binary with no console of its own (the case that opens windows; use `go test -c`, because `go.exe` is itself a console program) while
`scripts/window-watch.ps1` logs every new top-level window, and exits 3 when a console window appeared (`--selftest` is its positive control; `--hidden-console` starts the command with a window-less console
of its own, which the render suite needs because `node --test` starts every test file with a plain `spawn`).

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
- [`render/wf-wan22-i2v.mjs`](../../render/wf-wan22-i2v.mjs) /
  [`render/comfy-video.mjs`](../../render/comfy-video.mjs) — the Wan 2.2 graph and its runner: the
  decode mode (`chooseWanDecode` is the one rule), the `/system_stats` card read (`readRenderCard`)
  and `buildGraphForRun`, the single path from flags to a graph
- [`render/wan-multicard.mjs`](../../render/wan-multicard.mjs) — the card-only placement
  (`--wan-placement multicard`): the card table with the board power (`queryCardsRanked`), the choice and refusal of the two cards and their roles (`resolveWanCards`, `chooseRoles`, `WAN_COMPUTE_UUID`), the ComfyUI launch
  it needs (`multicardLaunch`, `multicardEnv`), the device check (`checkComfyDevices`) and `planMulticard`, the one call the
  runner makes before anything starts; [`render/wan-multicard.test.mjs`](../../render/wan-multicard.test.mjs) pins it
- [`render/comfyui-ext/extra_model_paths.yaml`](../../render/comfyui-ext/extra_model_paths.yaml) and
  [`render/comfyui-ext/nodes/offload_multicard/__init__.py`](../../render/comfyui-ext/nodes/offload_multicard/__init__.py) — the
  harness-owned ComfyUI code the placement loads through `--extra-model-paths-config`: the read-only safetensors reader and
  `OffloadFreeForDecode` ([tests](../../render/comfyui-ext/test_offload_multicard.py), run by
  [`render/comfyui-ext.test.mjs`](../../render/comfyui-ext.test.mjs))
- [`render/comfyui-ext/extra_model_paths_idle.yaml`](../../render/comfyui-ext/extra_model_paths_idle.yaml) and
  [`render/comfyui-ext/idle-nodes/offload_idle/__init__.py`](../../render/comfyui-ext/idle-nodes/offload_idle/__init__.py) — the idle bound a
  kept or warm ComfyUI is launched with: it frees what the instance keeps (a loaded model, the executor cache) after 300 s with an empty
  queue, not yet run in a live ComfyUI ([tests](../../render/comfyui-ext/test_offload_idle.py), run by [`render/comfyui-ext.test.mjs`](../../render/comfyui-ext.test.mjs));
  the launcher side (`resolveIdleBound`, the yaml and whitelist splice in `launchFlags`, the child env, the probe) is in
  [`render/comfy-lifecycle.mjs`](../../render/comfy-lifecycle.mjs), pinned by [`render/comfy-idle.test.mjs`](../../render/comfy-idle.test.mjs)
- [`internal/hostneed/hostneed.go`](../../internal/hostneed/hostneed.go) — `estimateWith`: a card-only Wan call declares 0 host RAM
  when each leased card fits its own stage (`CardOnly.SageAttention`: the activations at 15.2 GiB under `--use-sage-attention`)
- [`render/multicard-recipes.json`](../../render/multicard-recipes.json) /
  [`render/multicard-recipes.mjs`](../../render/multicard-recipes.mjs) /
  [`render/multicard-image.mjs`](../../render/multicard-image.mjs) — the card-only image and edit placement
  (`--placement multicard`): the recipe table (loaders, resident shares, per-card peaks, `basis`), the loader nodes a graph names and the
  refusals by name, and the planner and launch the three image runners make before anything starts; the tests are beside them
- [`internal/mediacap/multicard.go`](../../internal/mediacap/multicard.go),
  [`internal/hostneed/multicard.go`](../../internal/hostneed/multicard.go) and
  [`internal/pipeline/mediamulticard.go`](../../internal/pipeline/mediamulticard.go) — the Go side: the recipe table's copy (pinned to the
  JSON by a test), the host-RAM verdict on the pair, and the typed doors' two-card need
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
- [`render/tts.mjs`](../../render/tts.mjs), [`render/tts_chatterbox.py`](../../render/tts_chatterbox.py) and
  [`render/tts_chatterbox_ft.py`](../../render/tts_chatterbox_ft.py) — the Chatterbox voice lane: `parseCli` / `checkFlags` (the
  strict command line), `buildWorkerArgs` (the six sampling flags to either worker), and the workers' argparse;
  [`render/tts.test.mjs`](../../render/tts.test.mjs) pins it, runs the real workers against stub `torch` / `chatterbox` modules
  under `python -I` (no model, no GPU) and reads `internal/pipeline/pipeline.go` for the flags the voice lane sends
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
- [`internal/mediacap/recipe.go`](../../internal/mediacap/recipe.go) — what an image family is: the resolved recipe,
  its digest and its diff (ADR 0082); [`internal/mediacap/fingerprint.go`](../../internal/mediacap/fingerprint.go) — the
  sampled content fingerprint of a weight file and its cache (Amendment 2);
  [`internal/fleetnode/image_recipe.go`](../../internal/fleetnode/image_recipe.go) — the node's `image_recipes` rows and the 412 check
- [`internal/pipeline/medialane.go`](../../internal/pipeline/medialane.go) — the read-only lane question;
  [`internal/mediaremote/place.go`](../../internal/mediaremote/place.go) and
  [`identity.go`](../../internal/mediaremote/identity.go) — overflow placement and the strict match;
  [`ticket.go`](../../internal/mediaremote/ticket.go), [`scan.go`](../../internal/mediaremote/scan.go),
  [`door.go`](../../internal/mediaremote/door.go) and [`queuefs.go`](../../internal/mediaremote/queuefs.go) (with its
  `queuefs_lock_*.go` per-platform file lock) — the ticket queue (ADR 0083); [`internal/pipeline/medialane_place.go`](../../internal/pipeline/medialane_place.go) and
  [`internal/core/place.go`](../../internal/core/place.go) — the place a ticket keeps in the machine's own line
- [`internal/mediacap/graphfiles.go`](../../internal/mediacap/graphfiles.go) — the files a graph names and which of them a tree
  lacks; [`internal/fleetnode/graph_files.go`](../../internal/fleetnode/graph_files.go) — the node's 503 at the door;
  [`internal/pipeline/medialane_graph.go`](../../internal/pipeline/medialane_graph.go) — the lane question for a whole-node graph and the
  place such a graph keeps in the line while it is a ticket;
  [`internal/mediaremote/graph.go`](../../internal/mediaremote/graph.go) — the one-card rule, `min_vram_gb` and its default table, and
  what keeps a graph that reads input files on this machine
- [`internal/mediaremote/named.go`](../../internal/mediaremote/named.go) — a call that names a node: the one candidate, the typed
  refusals and what a row is read for (ADR 0082 Amendment 3); [`internal/mediacap/lane_rows.go`](../../internal/mediacap/lane_rows.go) and
  [`internal/fleetnode/media_recipes.go`](../../internal/fleetnode/media_recipes.go) — the video, animation and audio family rows a node publishes
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
- [`render/builder-drift.mjs`](../../render/builder-drift.mjs), [`render/builder-drift-specs.mjs`](../../render/builder-drift-specs.mjs),
  [`render/testdata/builder-drift.json`](../../render/testdata/builder-drift.json) — the builder drift guard:
  which template widget each `wf-*.mjs` builder copied, and the hashes it was verified against
- [`render/testdata/templates-catalog/`](../../render/testdata/templates-catalog/README.md) — its
  fixtures: trimmed upstream templates (MIT) and hand-written edge cases
- [`render/template-to-graph.mjs`](../../render/template-to-graph.mjs) — the template-to-API converter:
  a UI-format workflow in, the API-format prompt graph out, typed refusals, a JSON CLI
- [`render/testdata/template-goldens/`](../../render/testdata/template-goldens/README.md) — its
  fixtures: a synthetic `/object_info` listing, hand-worked goldens and a stand-in index

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
(not `mediaSlot`). A second call waits `gpu_wait_ms` and then defers `compose_busy`, and the call's PAIR card closes quiet
for it like a held GPU card ([pair-workloads.md](pair-workloads.md), *A held card is not a failure*). On the fleet,
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
  isolation disabled. `html` and `project_dir` are accepted from the local MCP and CLI doors, the
  same trusted-caller posture as `run-graph`, and over the fleet only by a node that opted in to the project door
  (`fleet_compose_projects`, [ADR 0071](../architecture/decisions/0071-a-fleet-token-holder-may-send-a-whole-composition-project-to-render.md)),
  which takes a whole project from a holder of the fleet bearer at `POST /fleet/compose-project`. The dispatch door
  needs the fleet bearer like every lane
  ([ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)) and renders only the node's
  vetted templates, so it refuses both at ack time. It also ignores a caller's `out`:
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
- `auto` (the default) runs here when this machine has the lane and the lane is free, and the call is then byte-identical to
  one made before the route existed. When the lane exists and is NOT free, an image call may go to an idle node that renders
  the same recipe instead of waiting ([Overflow](#overflow-a-busy-lane-places-an-image-call-on-the-fleet-adr-0082) below), and a
  whole-node graph to an idle single-card node that holds its files ([Graphs](#graphs-on-the-fleet-adr-0082-amendment-1) below);
  nothing else about `auto` changed. "Has the lane" is read from the files, not the binding: `mediacap` derives the route (the script,
  the weights its graph loads, the custom nodes it names), so a default config, which binds every script, does not make a
  thin client look like a render box. With no lane here, `auto` goes to a node from `delegate_remotes`; with no lane and
  no fleet configured it runs here and returns the pipeline's own deferral, as before.
- `remote` always goes to a node. `remotes` narrows the nodes for one call; each must already be in `delegate_remotes`,
  and the call is refused before any probe otherwise.
- A video, animation or audio call that carries `node` goes to that one node, whatever its route is set to except `local`
  ([Naming a node](#naming-a-node-a-video-animation-or-audio-call-runs-on-the-one-node-you-pick-adr-0082-amendment-3)).

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
builders decode; `out` and `out_dir` never travel (`out_dir` is where the fetched outputs land here). Three request fields cannot ride the fleet task and defer by name
(`defer_class` `contract`) instead of being dropped: `tts_voice`, `transformer` and (`run_graph`) `devices`, whose card ids name cards on the calling machine.
An image job's `refine=false` travels (payload field `refine`, ADR 0082): the client sends it only to a node whose health
says `refine_honoured`, and a node that predates it is a named miss in the defer (`capacity`), never one that refines the
prompt anyway.

**Node-side bounds.** A node holds at most one media-job body in flight (a body is about 0.58 GiB in memory at the 256 MiB
default cap, the base64 text beside the decoded bundle): a caller over it waits for the slot and, past 30 s, is answered `503`
with `Retry-After`. The client reads that as a `capacity` defer but does not read `Retry-After` and makes one pass over the node it
picked, so the call returns the defer and a later call places the job again.
The node also names a media-job's render itself (`mediajob-<16 hex>.<ext>`) and serves it only to a holder of the fleet token,
because it is rendered from the caller's private files: the client sends the bearer on every output fetch, a tokenless read of
that name is refused, as it is for every name the node serves (the outputs of a plain dispatch need the bearer too).

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
deferred) comes back as the node sent it, with `meta.node` and the `meta.err_class` its lane filed it under (the node's
poll carries the class beside the reason as `err_class`; a node older than that publishes none, and the caller reads an empty class).

**Attribution.** A call that goes to a node is the remote lane's own, like compose, vision, text and transcription (0.165.0,
D5-D11): it writes one asker ledger row (`node`, `node_id`, `route`, `placement`, `fleet_job_id`, `card_by_caller`) and, once a
node is chosen, one PAIR card on that node (queued, running, terminal), and the handle is closed on every way the call can end
(a result, a refusal, a node defer, a deadline), with the terminal frame posted before the door answers; a node that answers
that another job holds its card (`gpu_busy`, `gpu_queued`) closes the card quiet, not red, by the `err_class` its poll carries
([pair-workloads.md](pair-workloads.md), *A held card is not a failure*); a node that does not publish the class leaves the
card closed red. A call that reached no node has its row and no card, and a panic in the lane closes the card failed with the
panic before it goes on. Both POSTs, the plain
dispatch and the media-job, carry `X-Offload-Asker` and, only when this machine's emitter is off, `X-Offload-Pair-Card: node`, and,
only when non-empty, the asker's session and run class (`X-Offload-Origin`, `X-Offload-Run-Class`; the node believes the class only from a request
it can authenticate, [ADR 0087](../architecture/decisions/0087-the-askers-origin-and-run-class-ride-the-fleet-wire.md)).
The local route, and an auto call that runs here, are not attributed (the pipeline writes that row).

## Naming a node: a video, animation or audio call runs on the one node you pick (ADR 0082, Amendment 3)

`offload_generate_video`, `offload_animate_character` and `offload_generate_audio` (CLI `generate-video`, `animate-character`,
`generate-audio`) take **`node`** (`--node`): a node id, as the `node_id` of its `/fleet/health` spells it (any case), or one of the
`delegate_remotes` URLs. The named node is the **only** candidate: a box that has the lane itself still sends the call there, no other
node is tried and this machine's lane is not asked. `route local` with a node contradicts itself and is refused; a node on an image
or a graph call is refused by name by the library and the CLI (this release carries it for the three lanes only: the image and graph
verbs have no `--node` and the image and graph MCP tools list no `node`, so one sent to them is dropped like any unlisted argument). `meta.placement` reads
`remote: named node <name>`. Nothing about a call with no `node` changed: it is placed as before and no row is read for it.

**Refused before anything is sent.** A node that cannot take the call is refused by a word before the POST, so no job exists on it.
The deferred answer's data is `{refusal, node, lane, family, missing[], roster[]}`:

| `refusal` | Defer | Meaning |
| --- | --- | --- |
| `node_unknown` | `contract` | The name matches no node of the roster this call may use (`remotes` narrows it). `roster[]` lists the ids that answered. |
| `node_is_self` | `contract` | The name is this machine's own node: the call it would run itself is the local route. |
| `node_lacks_route` | `capacity`, err_class `no_node_configured` | The node does not serve the lane, or its route is not CONFIGURED (the answer names the state, and the files of its row when it published one). |
| `node_lacks_media_job` | `capacity`, `no_node_configured` | The call carries input files (a still, a reference and driver, a clone sample) and the node has not opened the media-job door. |
| `node_lacks_model` | `capacity`, `no_node_configured` | The node's `media_recipes` row for the family the call renders is absent or lists a file with `bytes` -1; `missing[]` names the files. |
| `node_below_min_vram` | `capacity`, `no_node_configured` | `min_vram_gb` was given (video and animation) and the node's largest card is smaller. |

A node that does not answer, or holds a text lease, is an ordinary `capacity` defer with no err class, because it may take the call
later; a node id that no answering node has, while some roster entry that was dialled did not answer, is that same defer, not
`node_unknown`, because the id may be one of them (a URL outside the roster is always `node_unknown`). A roster entry the tailnet
guard refuses is never dialled and has no id, so it does not make an unknown id a defer: the id is `node_unknown` and the reason names
the refused entries. A URL you name that is on the roster but that the guard refuses is filed `capacity` with err_class
`no_node_configured` and no refusal word, as `pickNodeFor` files such an entry. A named call on a machine with no `delegate_remotes`
never reaches a roster: it is the plain `config` defer (`no delegate_remotes configured`) with no refusal word, and this machine's own
lane is not tried. The four `capacity` refusals are a fact about the node that waiting does not change: the remedy is to name
another node (the pairing is the one `no_node_configured` already has). Nothing falls back: the named node refusing, another node
or this machine could have run the call, and neither is tried.

**What a node publishes.** `/fleet/health` gains `media_recipes[]`, additive: one row
`{lane, name, default, engine, digest, files:[{role, name, bytes}], license, commercial_use}` per family a binding of the node names
(`lane` is `video-gen`, `animate` or `audio-gen`). A video row is named by the `model` value that selects it: the ComfyUI families
`wan22`, `ltx25`, `hunyuan`, `h3` (the default family always; another only when `videogen_families` binds it or every file its graph
loads is on the node) and an sd.cpp family by its own name; the animation row is `wan-animate2`, the music row `acestep`.
`bytes` is the size where the node finds the file and `-1` where it does not, so a row whose weight is gone is still listed and the
refusal can name it. The ComfyUI rows exist only on a node that has a ComfyUI install (`comfy_dir` is a directory on it): the shipped
default config binds the three scripts on every machine, so the binding alone makes no row, and a node without the install (or with
only sd.cpp families) publishes none of them and omits the key when it has no sd.cpp video family either; a node with the install
lists every lane its config binds, an image-only node with ComfyUI included, with -1 for each weight it lacks. A ComfyUI file keeps
its loader-relative name; an absolute, drive-lettered or `..` name is published as its file name, and an sd.cpp file by its bare file
name whatever its path looks like. **A row is a statement of sizes, not an identity:** no fingerprint, `digest` always empty, built from
a stat per file and memoised for a minute. There is no row for voice, the audio.cpp engines or the sd.cpp animation lane.

**When a row judges.** Only a node that published at least one row for the call's lane is judged, by the row of the family the call
renders: with no `model`, the node's default row; a `model` is matched by row name exactly and otherwise folded the way the node folds it
(`ltx25`, `h3`, `hunyuan` render themselves, every other spelling, `wan` included, renders Wan 2.2; `ace` has no row and is not judged).
A lane the node publishes no row for is **unknown, not refused**: the call goes to the node's own door, which refuses a missing weight
itself with a 503, as a forced call does. Music is judged by the `acestep` row; voice, `voice=endpoint` included, is never refused for it.
A named call that ran on a node whose row judged it adds `rendered_family` and `rendered_files` to the result, beside the node's own
answer: what the node's row says it loads for that family, not a check of the render.

**`min_vram_gb` on video and animation** (`--min-vram-gb`) filters the remote candidates of any placement that leaves the machine, by
the largest card of the node as a graph's does (a 16 GB card reads 15.92 GiB, which counts as 16); on a named node it is
`node_below_min_vram`. The caller's number only: the Wan 720p default floor of a graph is not applied. A malformed value is refused by
name on every route. It filters nodes only; this machine's lane is its own grant.

**Not in this release.** No video, animation or audio call overflows: identity (a digest of the files' content and the settings
that change pixels), the busy-lane probe and tickets with input files are the releases that follow (ADR 0082 Amendment 3 lists them).
A node with the media-job door closed takes a named call that carries no input file and refuses one that does.

## Overflow: a busy lane places an image call on the fleet (ADR 0082)

A heavy image call on a box that has the lane used to wait for it however many other nodes stood idle with the same model on
disk. `offload_generate_image` (CLI `generate-image`) with `route` `auto` now asks its own pipeline first, and when the lane is
not free sends the call to an idle node that renders **the same recipe**. Everything else is as it was: a box with no
`delegate_remotes` asks nothing and reads no node, `route local` never leaves, `route remote` is the ADR 0077 path, and a
call that finds the lane free runs here with no node read at all.

**Is the lane free?** `core.LaneProber`, implemented by the pipeline (`Pipeline.MediaLaneFree`, `internal/pipeline/medialane.go`),
is a read-only question: would a wait-0 admission of this call be granted now? It reads the state the admission reads (the lease
directory, the places in line, the in-process slots, the host's memory through the guard's own function, and the allocator for a
call that names no card) and says busy only when something that makes the grant refuse is present. It creates nothing (no lease,
place in line, epoch, waiter, ledger row, PAIR card or ComfyUI instance) and removes nothing: it reads the lease directory through
`gpulease.Manager.ReadOnly`, a view that skips the records the line's readers prune as housekeeping (an expired place in line, a
waiter that stopped polling) and leaves them on disk (`TestMediaLaneFreeWritesNothing`, with a state for each). It says free when it cannot say: a lease the process
inherited (`gpu reserve -- local-offload ...`), a card table that does not read in 4 s, nor in the one 10 s retry that follows a read that ran out of time (both chosen, not measured; the
retry is the admission's rule at a shorter bound, and a table still unread leaves a `media lane probe` line in the log, once a minute per probe and cause while the fault lasts, F55), a task or a binding
it does not model (sd.cpp, an unknown family). A call that resumes a place
in line (a ticket's local run) counts only the callers that arrived before that place, on every plan, because the lease queue serves
a resumed place by arrival time (`TestMediaLaneFreeOrdersByArrivalForACallThatResumesItsPlace`); a new arrival counts every other
caller in line. A differential test (`TestMediaLaneFreeNeverRefusesAGrant`) drives the real
admission over a table of lane states and fails if a lane the grant would serve is called busy.

**What a family is.** A name says nothing about which weights sit under it. A family's identity is its **recipe**
(`mediacap.ImageRecipeWith`): the checkpoint, text encoder, VAE and LoRA it loads, each with its byte size on the node's disk and a content fingerprint, and
the sampling it renders with (steps, cfg, sampler, scheduler, schedule, shift, LoRA strength, preset), plus the license the result
is tagged with, digested with sha256 over canonical JSON. The identity is **name, size and content** (F59, ADR 0082 Amendment 2).
The first cut (0.179.0) had name and size only, so two same-named files of one size with different bytes (a same-named re-release
with the same tensor layout, a file updated in place on one node only) digested alike and the node's 412 check could not catch
them. Each present weight file now carries a **fingerprint**: a sha256 of a scheme tag, the file's size and a SAMPLE of its bytes,
64 blocks of 1 MiB at evenly spaced offsets (the first at the start of the file, the last ending at its end; a file of 64 MiB or
less is hashed whole). The block size and count are chosen. It is a sample and not a full hash for a measured reason (time) and a judged one (the window): on one
workstation, page cache not controlled (read these as warm-cache figures), it read 0.17 to 0.87 s per file for files of 0.36 to 42 GB, while a whole-file sha256 ran at 389 MB/s on a core (about
50 s for a 20 GB checkpoint, about 110 s for 42 GB, extrapolated), minutes per node after every release or model change with no
match possible meanwhile. That the window is too costly is a judgement; RAM was not the reason (a streaming hash holds one 1 MiB buffer). It separates two real builds of one 42 GB model that share a size (58 of 64 sample blocks differed). It
does **not** see a change confined to the stretch between two sampled blocks (the blocks are evenly spaced, so that stretch is
about a sixty-third of the file: roughly 0.3 GB of a 20 GB checkpoint and 0.65 GB of a 42 GB one; a layer-wise merge or a baked-in
VAE can sit inside it, and a pinned test shows a rewritten stretch leaving the fingerprint unchanged), nor a file replaced by one of
the same size and the same modification time while the process that cached the old reading runs; compare whole-file hashes after a
merge you swap in or a copy you doubt; the cache (path, size, modification time) is in memory and a
restart reads again (a one-shot CLI call is a fresh process, so on a busy lane it reads its family's samples on every call, before
it makes its ticket: an estimate of one to three seconds from the measured rates; on a free lane it reads nothing, because the
lane is asked first). `RecipeVersion` is 2, so a digest of 0.179.0 never equals one of this release. The digest is over **resolved** values: a key the binding leaves unset
takes the builder's default (Qwen-Image-2.1: 40 steps, cfg 1, euler, scheduler `simple`, schedule `official`), so a node that
writes the default out and a node that leaves it unset are one recipe. That table is pinned by a test to the constants in
`render/wf-qwen-image-21.mjs` and `render/comfy-render.mjs`; any other graph family leaves unset keys unset, which is stricter.
Node-local keys (`comfy_*`, timeouts, scripts, `imagegen_reserve_vram`, the pool keys) are not part of it, and
`TestRecipeClassifiesEveryClearListKey` fails when a key joins the image overlay's clear list without being classified. An sd.cpp
binding has no recipe; a ComfyUI recipe never matches one.

**What a node publishes.** `/fleet/health` gains `image_recipes[]` (one row per ComfyUI binding that names a checkpoint: `name`,
`default`, `digest`, `files[{role,name,bytes,fingerprint}]`, the resolved sampling, `license`, `commercial_use`, and `explicit`, the
sampling keys the binding actually set) and `refine_honoured`. The rows come from a memo of 60 s (the route cache's), so health stays
cheap; `bytes` is `-1` for a file the node lacks, which never matches. **Health never waits for a hash.** It reads the fingerprint
cache; a file not hashed yet starts one background read (one file at a time in this process) and is published `"fingerprint":"pending"` (`"unreadable"`
when it cannot be read, retried after 30 s), and **a row with any such file has an empty `digest`**, because two nodes that were both
pending would otherwise share a digest of unhashed files. The memo is rebuilt as soon as a read settles, so a pending row becomes its
digest on the next health read. An image-gen dispatch may carry `recipe_digest`: the node recomputes the digest of the family the
payload names from the files on disk **now** (waiting for each fingerprint it does not have, because a dispatch is not health: for 20 s at most per file or until the request is gone, and a file still unread is named `pending`) and refuses a mismatch, or a file it cannot
fingerprint, with **412**, creating no job. The refusal's JSON body names the `cause` beside `error`: `pending` (every file it lacks
a fingerprint for is still being read: it cannot say yet whether the family changed), `unreadable` (a file is there and cannot be read) or `changed`
(the family no longer digests to the digest sent, or the node no longer serves it); the delegator reads health again, and reads the
cause to decide how long to leave the node alone (below). A read that does not finish in 60 s is given up on:
that file reads `"unreadable"`, the single reader serves the others, and the harness logs it once; until then it holds the reader and
the other files stay `pending`. The "send family=X to use its Y" hint reads this machine's other families from the cache without
waiting, and a call with no fleet overflow because of a file this machine could not fingerprint logs which file and why.

**Which node.** A node is a candidate when it serves `image-gen`; holds a family whose digest equals this machine's (strict,
no substitution); runs this same release (`harness_version`: the graph builders ship with it); carries refine when the call sent
`refine=false`; has the family's route (`generate_image` or `generate_image:<name>`) CONFIGURED when it reports its routes;
accepts a per-request `steps` when the graph takes steps and cfg together (the node's binding must have **set** cfg, `explicit`);
and holds **no lease of any class** (the delegator cannot know which card a node's lease sits on or which card the job would
take, so it is conservative; the node's own grant, including the host-RAM guard, stays the authority). A roster entry that is this
machine's own node (its `node_id` equals `fleet_node_id`, else the OS hostname, the rule `fleet-serve` names itself by) is never a
candidate: it would match perfectly and send the call back into the lane that is not free. It is named in `cluster[]` as `skipped`. Candidates rank by the
shorter queue (`queue_depth`), then config order. The job goes under the **node's own** family name for the recipe and with the
digest; it never goes under the caller's name (a name binds different files on different nodes) and never with none (a call that
names no family is this machine's default binding and must not land on a node's different default).

**When a node says no.** A job goes to one node at a time, under a fresh job id for each (the ticket tries one node per scan and has no bound on how
many it tries over its life). A refusal at the door (`503`, `429`, `412`, any
other status, or a connection that was never made) means nothing ran, and the call moves on; so does a node that accepted the
job and answers `gpu_busy` or `gpu_queued` (another job holds its card) or `gpu_lease_unavailable` (it could not take its lease:
nothing ran). Any other answer after acceptance is final: a render that failed is the call's result and is never re-placed, a
POST that was sent and got no answer is final too (the node may hold the job; `TestAnAmbiguousPostIsFinal`), a job a node holds
is never also run here (a media job cannot be withdrawn, ADR 0064), and a node that goes away mid-job fails the call naming the
node and the remote job. A node that does not answer a health read is left alone for 5, 15, 60 and then 300 s by consecutive
failure (the last holds; each miss is one line on the process's stderr, once per step and none per scan,
`TestADeadNodeIsLoggedOncePerBackoffStepNotPerScan`), a `Retry-After` pauses a node for that long (jittered once), and a node that took a call and passed it
back is left alone for 60 s and then 300 s per consecutive bounce (only a call it serves clears the count; a health read does
not): a node that reads idle but whose own grant refuses every job would otherwise park each call for its whole `gpu_wait_ms`.
The paused node's row carries the node's own reason (`it took the last call and passed it back: gpu busy: waiting for host RAM: ...`;
a lease it could not take reads `refused`, `it took the last call and could not take its lease: ...`), and a job adopted from a process
that ended is paused the same way and under the same name (address and node id). The reason is quoted on one line, its address
redacted, capped at 600 bytes. Before this, a card-held bounce read "another job held its card" whatever the node said, so a host
short of RAM was told as a held card, and an adopted job's pause read so for every class, a lease it could not take included
(`TestABouncedRowSaysWhyTheNodePassedTheCallBack`, `TestAnAdoptedJobTheNodePassedBackGoesBackInLine`,
`TestAPassedBackReasonIsQuotedOnOneLineAndCapped`).
Chosen constants, not measurements (`TestABounceIsRememberedAndTheNodeComesBackAtTheNextStep`,
`TestARetryAfterHoldsTheNodeAndReadsAsRefused`, `TestARefusedDialMovesOnAndBacksTheNodeOff`, `TestABouncedAttemptGoesBackToWaitingAndTheNextScanTriesTheNextNode`).
A node that answers 503 or 429 with no `Retry-After` is left alone for the first step (5 s; a graph's 503 that carries no refusal word is
the node's own state and its row's cause reads `it answered 503 to the last dispatch at its door`, not "full"; a graph's 503 for a file the
node lacks carries the word `not_capable`, is that ticket's own `not-capable` row and pauses nothing: Graphs on the fleet, below); one that
refuses with another status is left alone like a bounce. A 412 is read by its `cause`: `pending` (the node has not finished reading its
weight files, so it cannot say yet whether they changed) leaves the node alone for a minute, the first bounce step (chosen), and is **not**
counted as a bounce, so a run of them never reaches the five-minute step and a later `changed` still starts at a minute; `unreadable`,
`changed` and a 412 that names no cause this build knows are bounced like any other refusal, and the cluster row says which it was (for the
last, that the node did not say why). The memory is shared by every ticket, an image's and a graph's: a node paused for one is paused for
the others. The memory is per process (the placer is a package variable): the MCP server keeps it across the calls
of its session, and a one-shot CLI call (`generate-image` and the other verbs) starts with none, though its ticket keeps it across
its own scans, so it pays a bounced node's `gpu_wait_ms` once.

**Attribution.** The call's PAIR card opens on the first node that ACCEPTS the job, not before the POST, so a node that refused
it leaves none. A node that accepted and then bounced it has its card closed quiet (`completed`, never started, the reason kept:
`RemoteAttribution.Bounce`) and writes no ledger row; the call's one asker row names the node that served it, with the route
`auto` and the placement text.

**What the answer says.** A call that ran on a node answers with `meta.node` and `meta.placement`: `remote: this machine's image
lane is not free (<what holds it, with its reason and the time its lease declared>); ran on <node>`. A call that nothing admitted
is a ticket, and its answer is the ticket's (below): `cluster[]` is one row per roster node in config order,
`{node, state, why, differs?, holder?, since}`, and the reason quotes them. `state` is `free` (idle, but another ticket is first in
line for it), `busy` (the node holds a lease: `holder` is its class, reason and time left), `draining`, `in-use` (a job of this
machine is running there), `not-capable` (the key that differs, in the config key's own name, and the family of this machine that
WOULD match: `send family=<ours> to use its <theirs>`; or the release, or the refine, the route or the steps rule), `unreachable`,
`refused`, `bounced` or `skipped`; a node being left alone says why (`bounced`: it had the call and passed it back; `refused`: it
answered the dispatch with a refusal, or took the job and could not take its lease; `unreachable`: it did not answer) and for how long, and `since` is when the ticket first found
the node in that state. A node absent from `delegate_remotes` is not named: nothing in the config says it exists.

**What does not overflow.** A call under a lease its process inherited, a call on a box with no `delegate_remotes`, and anything but an
image job or a whole-node graph ([Graphs on the fleet](#graphs-on-the-fleet-adr-0082-amendment-1) below): video, animation and
audio carry no per-family identity yet. A call that carries a `waiter_token` is not exempt any more: an image call or a graph joins
the ticket queue and brings its place (ADR 0083, and Amendment 1 for the graph).

**Named limits of overflow.** Every remote job pays ComfyUI's cold start (zero-warm). The same recipe and seed on a different GPU
architecture is the same composition, not bit-identical pixels. A mis-predicted placement costs one bounce per node per pause: up
to the node's `gpu_wait_ms` parked on the node and one deferred row there before the placer sees it. The ticket queue below closes
the two limits the first cut named (a call resumed with its token was not moved, and a call that went to the fleet first queued
locally from when it came back, not from when it arrived).

**Reading a node's recipes** (the first live check, before relying on a match): `curl -s http://<node>:18811/fleet/health` and
read `image_recipes` on the target and, on a machine that also runs `fleet-serve`, on the delegator's own node: the digest of the
family you will name there is the one the target's row must equal. A row whose files say `pending` and whose `digest` is empty is
a node that has not finished reading them: read it again in a few seconds (a row that stays `pending` for minutes, or says
`unreadable`, has a disk problem and the harness log has the line). **Every node must run the same release for digests to
match** (the digest is version 2 and a recipe is compared only between the same release), so read the three live matches of
0.179.0 again only after every node of them runs this one. A pair that does not match once both publish digests is a finding about
the files (other bytes under one name), not a defect to fix in the matcher: the cluster answer names the file and prints the first
twelve hex digits of each fingerprint. Neither `offload_status` nor `cluster[]` shows a local digest
(`cluster[]` names the keys that differ, not the digest); a delegator that does not serve has no surface that prints its own.

## The ticket queue: a busy image call waits for the first matching lane that frees anywhere (ADR 0083)

(A busy whole-node **graph** is a ticket too, with its own identity and its own rules for who may take it: see [Graphs on the
fleet](#graphs-on-the-fleet-adr-0082-amendment-1). Everything below that does not say "image" holds for both.)

Overflow sends a busy call to an idle node. When **every** lane is busy the call used to wait in one place and, if it was an MCP
call, come back as a queued answer the caller was told to re-send. A call whose lane is not free is now a **ticket**: the harness
holds the job and starts it on the first matching lane that frees, here or on a node, with one answer that says where it stands and
why, and no caller loop. A free lane runs the call here as it always did; a box with no `delegate_remotes` asks nothing; `route
local` and `route remote` are unchanged.

**The ticket.** One file per ticket, `<state root>/media-queue/mq-<id>.json`, written atomically and owner-only (0600 on POSIX, in a
directory made 0700 where the queue creates it; on Windows the ACL of the state root decides), so readable by every process of the
same user (`internal/mediaremote/ticket.go`, `queuefs.go`). That root is the one the GPU lease uses (`state_dir`, else `LOCAL_OFFLOAD_STATE_DIR`, else `%ProgramData%\local-offload` on Windows or `/var/lib/local-offload` elsewhere; no config key is needed), so a config that sets no `state_dir` has
the queue, and the tickets, the node claims and the lease's place tokens of a machine share one root
(`TestTicketQueueNeedsNoStateDirKey`, `TestTicketStoreAndLeaseShareOneStateRoot`, `TestABusyGraphIsATicketOnAMachineWithNoStateDirKey`). `gpu_lock_path` moves only the lease directory. It holds the owner (pid and start time) and its beat, the request's key, the
arrival time, the seed, the place in the local line, the state, the node and the node's job id once there is one, the nodes whose recipe
matched at the last scan, and the last answer; the request body stays in the owning process, so the file keeps no prompt (a graph's
body is the one the store keeps, beside the file, below; and a finished ticket keeps the call's result, which carries an image's
refined prompt, so every file of the queue is owner-only). States:
`waiting`, `local` (the local run is in flight), `placing` (a node is claimed and the job is being sent), `placed` (the node
accepted it), `fetching`, `done`, `failed`.

**One request, one ticket.** The key is the sha256 of the task, the prompt and the parameters as sent, without the `waiter_token`
(numbers are canonicalised, so `2048`, `2048.0` and a `json.Number` agree). Any identical re-send attaches to the live
ticket, with or without a token: that is what ends a caller's loop, because the tenth identical request is the first one's ticket
(`TestIdenticalResendAttachesToTheLiveTicket`). `waiter_token=mq-...` names a ticket and collects its result; a token that belongs
to a different request is refused by name, and one that names nothing is a new arrival. A `tk-...` place of the local line is
brought into the ticket. An `mq-` token never reaches the lease line (`TestMQTokenNeverReachesThePipeline`). A result is handed over
once per request: the call that takes it marks the file collected and the file stays 2 minutes (chosen), so another call waiting for
the same ticket, in this process or another, gets the result too and nothing renders twice
(`TestASecondCallNamingTheTicketStillGetsTheResultTheFirstTook`); a later request that only repeats the content is a new job (a
caller that wants a second picture of one prompt differs in a parameter, a seed). **One seed.** When the caller gave none, the ticket mints one (1 to 1e9, the pipeline's
own range) at creation, so a bounce, a re-placement, an adoption and a local run render one composition
(`TestTicketFixesOneSeed`); the key is taken before, so a re-send without a seed attaches.

**Seniority in the machine's own line.** `core.PlaceKeeper`, implemented by the pipeline (`medialane_place.go`) and forwarded by the
MCP runner, wraps the lease manager's place-keeping tokens and adds no admission logic: a place is left at creation with the
ticket's arrival time on the cards the real admission would queue the call on (the pin, or the card the allocator would pick),
touched on its own rhythm (a place holds its cards for 30 s after its last touch; a keeper touches every waiting ticket's place every
scan interval, ~10 s, without waiting for the scan, because a scan visits its tickets one after another and each visit asks one lane
question of up to 14 s (the card table's 4 s read and, when that ran out, its one 10 s retry), so a place touched only at its ticket's turn would lapse once enough tickets wait:
`TestAPlaceIsTouchedWhileASlowScanIsRunning`), left again with the SAME arrival time if it lapsed, let go when the ticket ends, and spent by the final local run through the `waiter_token` path every
resumed call takes. A caller that arrives after the ticket queues behind it. A host that does not lease cards keeps no places: the
ticket waits without local seniority and its answer says so. The lane question for a call that resumes a place is ordered by arrival
on every plan (`gpualloc.QueuedClaimsBefore`); without that, two places on one idle card each read the other as in its way and
neither would start (`TestKeptTokenHoldsSeniorityAgainstLaterWaiters` in the pipeline, `TestTicketKeepsItsPlaceInTheLocalLine` here).

**The scanner.** One per process, started with the process's first ticket and gone with its last, every 10 s +/- 20 % (a chosen
transport constant, not a resource number), oldest ticket first. A scan DECIDES; a job runs in a goroutine of its own, one per
ticket in flight, so a local run that waits in the lease line stalls neither another ticket's scan nor the touch that keeps each
place alive. For a waiting ticket a scan: (1) keeps its place; (2) **local first**: when the lane question says the ticket would be
served now, starts the local run with the place as its `waiter_token` (one ticket per scan, the oldest). A run the lane refuses at
once, or that loses the race to another job, comes back with nothing run and the ticket goes back to waiting
(`TestALostRaceOnTheLocalLaneGoesBackToWaiting`); the lane is then **paused for that ticket** for 5, 15, 60 and then 300 s by
consecutive refusal (the node steps; a reading of busy resets them) and the fleet is looked at meanwhile, so a lane that reads free
and refuses at once (`gpu_wait_ms` 0, or a younger place on the card that a call without a wait cannot pass) is not asked in a loop,
each run writing the pipeline's own deferred row (`TestALostRaceOnTheLocalLaneIsNotARetryLoopAndTheFleetIsTriedMeanwhile`). A lane the
pipeline TRIED to read and could not (the card table past its 4 s bound and its 10 s retry, the lease directory not opening: `core.LaneVerdict.Unjudged`)
is not a free lane: the fleet is looked at in the same scan, and the run here is the answer only when no node can take the call
(`TestAnUnreadLaneStillLooksAtTheFleet`, `TestAnUnreadLaneWithNoNodeRunsHere`); a call the prober does not model at all (another task, a
binding without a ComfyUI lane, an inherited lease) runs here at once;
(3) otherwise asks the fleet with ADR 0082's own filter and ranking (`candidatesFor`), minus the nodes a job of the machine has a
claim on (an image ticket's or a graph ticket's) and the nodes an **older** waiting ticket of the machine matches (an older ticket not yet scanned is first in line for every
node: `TestTicketOldestFirstPerNode`), and sends the first node left the job. **Late binding**: nothing is sent to a node until a
lane is free there, so no job sits parked on a node that frees later than another (`TestLateBindingStartsOnFirstFreedLane`). One node
is tried per scan; a node that refuses or bounces the job is left alone (5 s for a full node that names no `Retry-After`, 60 s
and then 300 s for a bounce) and the ticket tries the next at its next scan. A ticket never gives up on a lane; it ends after 8 h
without one. A freed lane is noticed within one scan interval (at most 12 s) plus the time the previous scan took, since a scan visits
its tickets one after another and each costs one lane question: that is N reads per interval for N waiting tickets, each a few hundred
milliseconds when the table reads and bounded by 14 s when it does not (the 4 s read and its one 10 s retry).

**A node a job of this machine has a claim on is in use for every scanner on the machine.** Reading the tickets and then sending would
leave the window in which two processes both read a node idle and both send it a job. A claim file per node (`use-<hash>.json`) is written
together with the ticket's `placing` state, which already names the job about to be sent (its id is minted before the claim), under
a directory lock (`.lock`, an operating-system file lock held for a few file operations: `flock`, `LockFileEx`), before the job is
sent (`TestTwoProcessesNeverDispatchTwoTicketsToOneSingleJobNode`, `TestClaimNodeIsExclusiveAndLetsGoOfTheDead`, `TestTheTicketNamesItsJobBeforeThePost`).
The lock is never a file whose age decides it is stale: every way of breaking a stale file lock has a gap in which a slow contender
removes the fresh lock of the one that broke it (two holders inside at once, 24 of 300 contended rounds on the first design), while the
system frees an OS lock when its holder's process ends, however it ends (`TestTheDirectoryLockNeverHasTwoHolders`,
`TestACrashedHoldersLockIsFreedWhenItsProcessEnds`); the lock file stays in the directory for good, since removing it would let a late
contender lock a new file while an earlier holder still has the old. A cancel that lands while the claim waits for the lock ends the
ticket before anything is sent: the claim is let go and nothing reaches the node (`TestACancelDuringTheClaimSendsNothingAndFinishesOnce`). A claim is
live while its ticket has a job in flight on that node and its owner is alive, or died less than 2 minutes ago (chosen: the job it
sent keeps running on the node, which shows its lease only once the job is admitted; after the grace the node's own health speaks
for it, `TestClaimNodeIsExclusiveAndLetsGoOfTheDead`). **A whole-node graph is a ticket and takes the same claim, under the same lock, before it
sends**, so an image ticket is never sent a node a graph ticket is running on, a graph ticket is never sent a node an image ticket is
running on, and two graph tickets of two processes never stack (`TestAnImageTicketNeverSendsANodeAGraphTicketHasAJobOn`,
`TestAGraphTicketLeavesANodeAnotherTicketHasAJobOn`, `TestTwoProcessesNeverStackTwoGraphsOnOneNode`,
`TestAPlacedGraphMarksItsNodeInUseForTheOtherProcess`); a row for such a node reads `a job of this machine is running there (ticket
mq-..., since ...)`. **A claim of an older release.** A 0.180.0 process took this claim for its one-shot graph call, which had no
ticket: the record is marked `kind: graph`, names the call (`gr-...`), has no ticket file behind it and no scanner to beat for it, and
is live while its process is (and younger than the longest call budget plus an hour) and, when the process is gone, only for the first
2 minutes after the claim was taken, counted from the claim and not from the death, since the call wrote no beat: that is the window in
which the job may be on the node before its lease shows, and after it the node's own health speaks for it. This release writes none
and still reads and honours it, because the processes of one machine are not all restarted at a deploy (an MCP server keeps its
process for days) and a scanner that did not honour it would send the node a second job
(`TestAnOlderReleasesGraphClaimHoldsWhileItsOwnerLivesAndForTheGraceAfter`, `TestATicketNeverSendsANodeAnOlderReleasesGraphCallHasAJobOn`);
a row for such a node reads `a job of this machine is running there (graph call, since ...)`. A call forced to a node (`route` remote,
or `auto` on a machine with no lane for it, image or graph) goes through `callWith`, takes no claim and reads none, as before this
release: the node's own grant is what stands between it and a ticket's job. Every machine has the claim directory: only one whose lease
root is refused (a cloud-synced folder) keeps no queue, says so once in its log, and there nothing is a ticket: `auto` places neither an
image nor a graph on the fleet and the call waits in the machine's own line (`TestARefusedStateRootKeepsNoQueueAndSaysSo`).

**Owners die.** An owner is dead when its process is gone or it has not written its beat for 10 minutes (chosen). A `waiting` ticket
of a dead owner, and a `local` one (its render died with its process), are swept by the next scan or call of any process and their
places let go (`TestWaitingTicketOfDeadOwnerIsSwept`); the identical call that finds one makes a new ticket that keeps its arrival
time (`TestDeadOwnerLocalTicketIsSweptAndRerendered`). A `placed` or `fetching` ticket stays for 6 hours (chosen) and is **adopted by
the next identical call**: the new owner polls the recorded node job and fetches its outputs and never sends the request again, so
the job renders once; the adopted call writes its one ledger row and opens no card (`TestDeadOwnerPlacedTicketIsAdopted`), and that row
names the node and the job like any remote call's (`node`, `node_id`, `fleet_job_id`; the adopter never dispatches, so its handle is told the
ticket's recorded node and job, `core.NameNode`) because the placement note's `; ran on <node>` tail is cut by the ledger's 120-byte cap
whenever the lane sentence before it is long (`TestAnAdoptedTicketsRowNamesTheNodeItRanOn`). A `placing`
ticket (the owner ended while the job was being sent) is asked about at the node once: a node that denies the job was never given it, and
the ticket goes back in line with its arrival (`TestADeadOwnersPlacingTicketThatWasNeverSentGoesBackInLine`); one with no job id (a file
of an older build) is not polled, and the answer says the job may be on the node (`TestAPlacingTicketWithNoJobIdIsNeverPolled`). A job
the node accepted and passed back goes back in line too, and the node is left alone for a while (`TestAnAdoptedJobTheNodePassedBackGoesBackInLine`).
A finished ticket nobody collected stays 6 hours when its job succeeded and 10 minutes when it failed (chosen: the caller was told to call
once and come back for it, and a repeat of a failed request should try again; `TestAFinishedResultNobodyCollectedIsKeptForHoursAFailureIsNot`).
A temp file of a write that never finished, and a claim with no live
owner, are removed by the same sweep.

**A scan writes no card, no row and no waiter.** It reads the lane, the roster and the files, writes its own ticket file (beat,
matches, answer) and touches its place (`TestScanNeverWritesCardsOrRows`, the register C01 regression: the first design of this loop
opened a PAIR card per attempt). The call's one ledger row and one PAIR card open at dispatch: at the node's acceptance for a remote
job (as in ADR 0082), by the pipeline's own run for a local one (a refused local run writes the pipeline's own deferred row, one per
refusal, spaced by the pause above). Re-attaching by token or by resend creates no row, no card and no place (`TestReattachByTokenCreatesNoJobRowOrCard`).

**The answer.** A call whose window ends before its job is done answers with a capacity deferral of class `gpu_queued`:

```
{queued: true, scope: "cluster", ticket: "mq-...", waiter_token: "mq-...", position: N,
 local: {state, why, holder?, ahead?, since}, cluster: [{node, state, why, differs?, holder?, since}, ...],
 next: "the harness holds this job and starts it on the first matching lane that frees; do not call again or resend; a repeat of the
        request attaches to this ticket; call with waiter_token=mq-... only to collect the result (a finished result is kept for 6 hours)"}
```

`position` is the ticket's rank among the machine's waiting tickets (1 is next); `local` is this machine's own lane (`free`, `busy`
with its holder and the callers ahead, or `ram` when the host's memory, not the card, is what is short); `cluster[]` is described
above and names EVERY roster node (`TestAnswerNamesEveryRosterNode`). The window is `wait_s` of the MCP door (0 answers after the
ticket's first look; at most 3600), else the machine's `gpu_wait_ms`; the ticket goes on after the call returns, and the answer
text tells the caller not to call again. The CLI (`generate-image`) blocks until the job is done, prints on stderr where the ticket
stands (queued with the reasons, then where it started), and the first interrupt cancels a ticket that sent nothing; a job already on a
node cannot be recalled (ADR 0064), so the ticket stays for the next identical command to adopt (`TestCLIBlocksAndCancelsUndispatched`).
`offload_status` gains `media_queue[]` (ticket, state, family, position, node, age, whether its owner is alive and it holds a place,
and why while it waits); absent when there are none.

**The roster is re-read.** An MCP session never reloads its config, so the ticket re-reads `delegate_remotes` from the config file the
session runs on whenever the file's mtime or size changes (`TestRosterReReadWithoutRestart`); only that key moves, and a file that
does not parse leaves the roster as it was. The server passes its resolved config path, as does the CLI.

**Named limits.** One first-in-first-out line per delegating machine (its state dir): two machines each hold a queue and meet only at
a node's own grant; a holder node's queue (ADR 0030) is the named upgrade. A CLI ticket dies with the CLI (a waiting one is swept, a
placed one adopted). A process suspended for longer than 10 minutes reads as dead to the others until it writes its beat again: its
waiting tickets can be swept meanwhile, an identical call can then make a second ticket, and a placed one is adopted rather than
re-sent (the job is then polled by two processes); a guard that also requires the pid to be gone is the named follow-up. Each waiting ticket costs one lane question (a bounded `nvidia-smi` read) and one roster read per scan; the first
scan reuses the creation reading for 2 s. A node that frees between two scans can be taken by a caller that is neither a ticket nor a whole-node graph (a video,
animation or audio call placed on the fleet, or any call forced to a node; a graph yields to the older waiting tickets, below); the
job then costs one bounce. The scan interval and jitter, the pause steps, the owner and keep timings and the 8 h ceiling are chosen
constants, not measurements. Nothing here was run against a live lease, a live node or a render.

## Graphs on the fleet (ADR 0082, Amendment 1)

`offload_run_graph` (CLI `run-graph`) with `route` `auto` is a **ticket** when its own lane is not free (ADR 0083 Amendment 1), and
every route that sends a graph to a node holds it to the rules below. The graph is the caller's, so it has no recipe and no family:
what makes a node a fit is that it **holds the files the graph names**, and the node says so itself.

**A graph is a ticket.** The ticket queue above is the busy-lane path of an image call and of a whole-node graph (a graph that names
`devices` is card-scoped: it is never offered to a node, and it is no ticket). A lane the pipeline could not READ (the card table past
its bound, the lease directory not opening) is not a free lane for a graph either, as for an image: the graph is a ticket and its first
scan looks at the fleet (`TestMediaLaneFreeGraphIsFreeWhenTheCardTableCannotBeRead`,
`TestMediaLaneFreeGraphIsUnjudgedWhenTheLeaseDirectoryCannotBeOpened`, `TestAnUnreadLaneStillLooksAtTheFleetForAGraph`,
`TestAnUnreadLaneWithNoNodeRunsAGraphHere`). Five things differ from an image's, each held by a test:

- **Identity is the graph's content.** The key is the sha256 of the task and the parameters without the `waiter_token`, with the
  graph and the manifest taken as what they say (canonical JSON: keys sorted, whitespace dropped, numbers as written) in place of the
  paths they were read from. The MCP door writes an inline `graph_json` to a new temp file on every call, so a key made of the path
  would make every resend a new job; and a file edited under the same path is another graph. The door removes those temp files when
  its call returns (`run-graph` the ones of `--graph-json` and `--manifest-json`), waiting out a delete that Windows refuses for a
  moment (about half a second) and naming on stderr a file that still cannot go; a file the caller named is never removed, and the
  ticket needs none of them because it keeps its own copy of the bytes (next item). `out_dir`, `reserve_vram`,
  `min_vram_gb` and `model_family` are part of the key (the same graph sent to another folder is another job). One ticket per identical
  request, from either of two processes, and `waiter_token=mq-...` collects it (`TestABusyGraphIsATicketAndTheSameGraphAttachesToIt`,
  `TestAGraphsKeyIsItsContent`, `TestTwoProcessesAskingForOneGraphAtOnceMakeOneTicket`). The ticket mints no seed and sends no
  recipe digest: the graph owns its own sampling (`TestAGraphTicketKeepsItsPlaceInTheLocalLine`).
- **The ticket is its bytes.** A ticket may wait for hours and the caller's graph file is the caller's: reused for the next shot,
  edited, removed. The ticket writes the graph and the manifest as the plan holds them into its queue directory
  (`body-<id>-graph.json`, `body-<id>-manifest.json`; the one request body the store keeps, owner-only like every file of the queue
because a graph carries its prompt text), takes its key from those bytes, and
  the node's payload, the local run, the need and an adopter all read them, so whichever lane serves it renders the same graph
  (`TestAGraphTicketRunsTheBytesItWasAdmittedWith`, `TestAGraphTicketSendsTheBytesItWasAdmittedWithToANode`,
  `TestACreatedGraphTicketIsKeyedByWhatItHolds`, `TestAGraphTicketsKeyComesFromTheBytesItHolds`). The copies go with the ticket and
  stay while a dead owner's placed ticket can be adopted (`TestAGraphTicketsCopiesLiveAndDieWithTheirTicket`,
  `TestALosingCreateLeavesNoCopies`). The ticket also stores a path key (the same paths and parameters, no file read): a
  `waiter_token` is accepted for a request with the same content or the same path key, so a finished result is collected by its token
  and a dead owner's placed job is adopted from the copies after the file is gone or overwritten
  (`TestAGraphTicketIsCollectedByItsTokenOnceItsFilesAreGoneOrEdited`,
  `TestADeadOwnersPlacedGraphIsAdoptedAfterTheCallersFilesAreGone`); a repeat WITHOUT a token attaches by content alone, and an
  abandoned ticket's arrival is inherited only by the same content
  (`TestATokenOfAnAbandonedGraphTicketInheritsItsArrivalOnlyForTheSameContent`).
- **Seniority.** The place a graph keeps in this machine's line is the whole-node token the real admission leaves for a waiting graph
  (no card named, reason `run-graph`; `Pipeline.leaveGraphPlace`), at the ticket's arrival time; a place the call already held is
  brought along; the final local run spends it through `waiter_token`
  (`TestAGraphPlaceHoldsSeniorityAndIsSpentByTheLocalRun`, `TestAGraphThatArrivesWithAPlaceBringsItIntoTheTicket`). A host that
  does not lease cards keeps none, and the answer says so (`TestAGraphTicketOnABoxWithoutPlacesSaysSo`).
- **The node decides identity at its door, and a refusal is about THIS ticket.** The 503 a node answers when the graph names a file
  it does not hold carries `refusal: "not_capable"` in its body (a full node's and a route-not-ready 503 do not). The delegator reads
  the word, not the sentence: the ticket's row for that node is `not-capable` with the node's own words, **no pause is put on the
  node** (the placer's 5 s for a 503 that names no `Retry-After` is for a node that is full, and it would hide the node from an image
  ticket and from every other graph it can serve), the ticket asks that node again only after 5 minutes (chosen, not measured: the
  longest pause the placer puts on any node, and the interval at which an operator who places the missing file finds the ticket
  waiting for it), and the next candidate is tried in the same breath. The memory is the owning process's; the cluster row it writes
  is what the other processes read (`TestAFileRefusalIsNotCapableForThatTicketAndNeverFull`,
  `TestATicketAsksANodeThatRefusedItAgainAfterAWhile`, `TestAFileRefusalMovesTheGraphOnAtOnce`, and on the node
  `TestAGraphFileRefusalCarriesTheNotCapableWord`, `TestARouteThatIsNotReadyIsNotSaidToBeNotCapable`).
- **A graph that reads input files is a local-only ticket.** A node can match such a file only by name, so the ticket is never offered
  to a node: it reads no roster, matches no node (so, once its first scan has run, it holds none against a younger ticket; before that it
  is first in line for every node, like any ticket not yet scanned) and runs when this machine's lane serves
  it. It is still a ticket: one per request, a place and a position in this machine's line, and every answer carries `fleet_note`
  (and the same sentence in its reason) saying why the fleet was not asked
  (`TestAGraphThatReadsAnInputFileIsALocalOnlyTicketThatKeepsItsPlace`, `TestALocalOnlyGraphTicketIsNeverMovedAndHoldsNoNodeAgainOthers`).

What the ticket closes of the one-shot overflow it replaces, and what it keeps. Closed: the late local join (a graph no node took
joined this machine's own line from when its fleet attempt was over), the lane that frees after the one look at the fleet, the repeat
that was a second call, the bound of three nodes (`TestABouncingGraphIsNotBoundedToThreeNodes`), the `tk-` token a caller had to
re-send, and the abandoned job: a graph a dead process sent is adopted by the next identical call, polled and fetched and never sent
again (`TestADeadOwnersPlacedGraphIsAdopted`). Kept, as the ticket's own rules and no longer a second copy of them: the claim file is
taken under the directory lock, so two processes, or two calls of one process, never put two graphs on one node and a graph and an
image never share one (`TestTwoProcessesNeverStackTwoGraphsOnOneNode`, `TestTwoGraphCallsOfOneProcessDoNotStackOnOneNode`,
`TestAnImageTicketNeverSendsANodeAGraphTicketHasAJobOn`, `TestAGraphTicketLeavesANodeAnotherTicketHasAJobOn`), and a graph is an
ordinary member of the line, so a node an older waiting ticket matches is that ticket's, whichever kind it is
(`TestAGraphDoesNotTakeANodeAnOlderImageTicketIsFirstInLineFor`, `TestAGraphTicketLeavesANodeAnOlderTicketIsFirstInLineFor`,
`TestAnImageTicketLeavesANodeAnOlderGraphTicketIsFirstInLineFor`). The one-shot path itself (`tryGraphOverflow`, `placeGraph`, the
in-process hold) is removed. **The doors**: the MCP door answers inside `wait_s` (else `gpu_wait_ms`) with the ticket and the ticket
goes on; `run-graph` on the CLI blocks until the job is done, prints on stderr where the ticket stands, and the first interrupt cancels
a ticket that sent nothing.

**What an operator sees differently from 0.180.0** (the whole list is in ADR 0083 Amendment 1 and the CHANGELOG): a busy graph comes
back with a ticket and `cluster[]`, not the local lane's queued answer with a `tk-` token to re-send; a graph that carries a `tk-`
`waiter_token` is no longer kept on this machine, its place comes into the ticket and it may go to a node (`TestAGraphThatArrivesWithAPlaceBringsItIntoTheTicket`);
the deferral a graph ends in after a failed local run no longer carries `cluster[]`; and on a machine whose lease root is refused, so
that it keeps no queue, a busy graph is no longer offered to the fleet (it waits in the machine's own line, as an image always did).

**The node's door (the ACK check).** Before it writes a temp file, takes a lease or starts ComfyUI, `buildRunGraph`
(`internal/fleetnode/graph_files.go`, with `mediacap.ScanGraph` and `mediacap.MissingModelFiles`) reads the graph:

- a **weight file** is any string input, under **any key but one**, whose value ends in `.safetensors`, `.gguf`, `.pth`, `.pt`,
  `.ckpt` or `.sft`: `ckpt_name`, `unet_name`, `vae_name`, `lora_name`, but also a `ModelPatchLoader`'s `name`, a wrapper loader's
  `model`, an `ipadapter_file` or a primitive's `value` (every custom node picks its own key; the extension is the stable part).
  The two exclusions the code keeps: the key `filename_prefix` (an output prefix, whatever its value) is never read, and neither is
  a value that contains `://` (a URL). A link and a number are not strings and are not files either. The price is a text that
  itself ends in a weight extension: it reads as a file name and costs a re-placeable 503 (the graph still runs on its own
  machine). It must
  resolve **by name** under **any class directory of any models root** of the node (`<comfy_dir>/models/*` and every entry of
  `extra_model_paths.yaml`), as a ComfyUI loader opens it: the name relative to the class directory, subfolders included, never a
  search for the basename. A class directory that is a symlink or a Windows junction counts. The class is not matched to the
  loader (the harness does not know every loader; ComfyUI's own preflight stays the authority on a file under a class its loader
  will not open).
- an **input file** is the file input of an input loader: `image` of `LoadImage`, `LoadImageMask` and `LoadImageOutput`, `file` of
  `LoadVideo`, `audio` of `LoadAudio`; it must be in `<comfy_dir>/input` (or `output` or `temp` when its `[output]` / `[temp]`
  annotation says so). **Input files do not travel with a graph**: `/fleet/dispatch` carries JSON and the media-job door carries no
  input for `run-graph`, so a node that lacks the file cannot be given it, and the refusal says so (carrying them is a named seam,
  not built).
- a name that is absolute or climbs out with `..` is never looked up and reads as missing (the name is data from a peer).
- a model the **manifest** tells the node to fetch (a `models[]` entry with a `source_url`) is not missing: the satisfier
  downloads it before it starts ComfyUI, as it always did.

A node that lacks any of them answers **503** with the existing route-not-ready refusal, naming what is missing (up to six names
of each kind), and **no job, temp file or lease exists**. Past six missing weight files it says `7 or more files ... and more` and
gives no count: the node stops looking at the seventh (any holder of the fleet bearer can send a graph and a graph can name thousands of files, and a
missing name costs a stat in every class directory of every models root, so the work is bounded by the six shown and not by the
graph); a list of missing input files is short and is counted exactly. The 503 body is `{status, error, refusal}`, and `refusal` is
`"not_capable"` (the node is not full and not out of route: it cannot run THIS graph). The delegator's `cluster[]` row quotes the
`error` sentence, cut at 600 bytes, so a graph missing many long names shows its first few there, and the node's own 503 body (a
`curl` of the dispatch) lists up to six. The ticket goes on to the next node instead of this one cold-starting ComfyUI to defer. The check is by **name, not content**: a same-named file of other bytes is held. For a
weight file the name is the practical identity (a model is published under one). For an **input file** it is not enough, because
those are the caller's own and carry generic names (`image.png`, `ComfyUI_00001_.png [output]`) that two machines share with
different bytes; so the delegator never offers a graph that reads one to a node (above). A call that must leave its machine anyway
(`route remote`, or `auto` on a machine with no graph lane of its own) still reaches this check with one, and for it the check
stays by name: that is a named limit. A custom node's own file key is not read.

**Which node.** A candidate serves `run-graph` with `run_graph` CONFIGURED when it reports its routes, runs this release (the
file check ships with it: an equal `harness_version` proves that only when the release that adds the check carries a new version
number, so the version bump ships with the merge, before any delegator is deployed), holds no lease of any class, is not this machine's own node, and passes the two rules below. The
release proof and the two rules hold for **every** remote placement of a graph, not only the overflow: `route remote` and a thin
client's `auto` pick a node by the same health fields and refuse a node that runs another release, or publishes none, by name
(they do not read the media lease; the node's own grant refuses a job it cannot take). A fleet part-way through an upgrade
therefore cannot place a graph on a node that is not yet on this release. The
lane question is the whole-node reading of `Pipeline.MediaLaneFree` (`internal/pipeline/medialane_graph.go`: any lease,
in-process slot or place in line, and the host's memory against what the graph declares: the weight files its own loaders name
(`pipeline.graphRAM`), or the media class default, the largest family the box binds, when the graph cannot be read or sized), under the same relaxation contract as the image's (`TestMediaLaneFreeGraphNeverRefusesAGrant`,
`TestMediaLaneFreeGraphWritesNothing`). The graph travels as it is: no family, no digest, no seed, `out_dir` and `min_vram_gb` never. A node's
503 for a file it lacks moves the ticket to the next lane and marks that node not-capable for that ticket (above); any other 503 (a
full node, a route that is not CONFIGURED) is the placer's, as for an image: a node that names no `Retry-After` is left alone for about
5 s in the calling process. When nothing admits, the ticket waits and its answer carries `cluster[]` naming, per node, the file it
lacks. A graph that names `devices`, or runs under an inherited lease, is never a ticket and is not offered to a node; one that **reads an
input file** is a ticket that is not offered to a node (above). A call with no lane of its own to wait for (`route remote`, or `auto`
on a thin client) is placed on a node as before, checked by name only, and held to the same release proof and the two rules below.
Sending a size and a hash of each input file to the node would lift the restriction for such graphs and close that limit; it is not built.

**One card.** A graph that names no devices holds the whole node and runs in the default ComfyUI instance, which sees every
card. On a node with several, the delegator cannot tell which card that instance picks, and it may be the display card. So a
whole-node graph is placed **only on a node that publishes exactly one card** in `gpu_devices`: a node with more is a named miss
(`has 3 cards, ...`), and so is a node that publishes none (unknown is never a pass). This is a rule of this client's placement, and
it covers every remote placement of a graph (the overflow, `route remote`, a thin client): this client cannot send a graph to a
multi-GPU node. It does not change what such a node does with a graph sent to it by hand, nor a graph run on its own machine, whose
whole-node instance can still pick the display card; that local case is parked. A single card that **drives a display** passes the
rule: a box whose only card is its display card runs there by necessity, the allocator's own carve-out (`gpuprobe/display.go`), so
the rule protects the display card of a multi-GPU box and nothing else.

**What the two rules place today.** With the fleet as the film owner described it on 2026-10-09 (the 16 GB cards are the three of
the multi-card workstation; the single-card nodes carry 8 GB cards), a graph that gets the default Wan floor below has **no remote
target**: the one-card rule excludes the node with the 16 GB cards and the floor excludes the nodes the rule admits. It waits for
its own lane, and a thin client cannot reach the workstation for a graph at all. That is the film owner's statement of the fleet,
not a measurement. The durable fix is on the node (keep a whole-node graph off its display card there), which is the parked local
item.

**`min_vram_gb`.** `offload_run_graph` and `run-graph --min-vram-gb N` ask for a node whose **largest** card is at least N GB;
a node that publishes no cards cannot be shown to have it. A card counts as the size it rounds to: nvidia-smi reports MiB/1024,
so a 16 GB card reads 15.92 or 15.93 GiB and a literal comparison with 16 would shut out every 16 GB card
(`TestMinVRAMReadsTheLargestCardAndTheNominalSize`). With no argument, a graph that names `model_family` `wan22` (`wan2.2` and `wan`
are the same) via `model_family` / `--model-family` and carries a `height` input of 720 or more (the tallest of any node; none = no
floor) gets a floor of 16 GB. **That row is chosen from the film owner's stated constraint that Wan 2.2 14B at 720p needs a 16 GB
card; the harness did not measure it**, and every row that cites it says "chosen". The caller's number wins over it and 0 lifts it.
The floor filters **remote** candidates only: the local lane is this machine's own grant, and `route local` never reads it. A
malformed value (negative, not a number) is refused by name on every route (`mediaremote.Run`), so a call that stays on this
machine never drops it silently.

**Named limits of the graph ticket.** The graph is read once, when its ticket is made, and the ticket keeps that copy, so a file
overwritten or removed meanwhile changes nothing (a repeat of the overwritten file is another ticket). What it does not keep is the
input files a graph reads: their bytes are no part of the key, so a finished result nobody collected (kept up to six hours) answers a
repeat of the same graph even when the caller has put other bytes under the same input name; set another `out_dir` (part of the key)
or collect first. The ticket remembers a node's refusal in the owning process only: an adopter of a ticket that went back to waiting
asks that node once more. A graph ticket's place in this machine's line is the whole-node token, so every call that arrives after it
queues behind it on this machine's own lane, an image call whose own card is idle included (it can still go to an idle node), until
the ticket runs, is canceled or ends (8 h at most); that is the seniority the operator asked for, and it is also what it costs. Nothing
here was run against a live lease, a live node or a render: the live acceptance in the CHANGELOG entry of this change (A9) is the
first live evidence.

**Not built.** Input files for graphs, and with them a size and hash of each to let a graph that reads one go to a node; a
content check of the files; `devices` on a remote graph; a custom node's own file key; a default floor for other families or for
video (`offload_generate_video` takes no `min_vram_gb`).

## The image batch across the fleet: one ticket per job when the lane is busy and a node can render the recipe (ADR 0083, decision 13)

`local-offload generate-image --batch jobs.jsonl [--family F] [--route auto|local|remote] [--results r.jsonl] [--resume] [--jobs N]` is the
P0's batch (register F28): jobs in, a picture at the exact `out` of each, a verdict per job, and the lanes of the whole roster when this
machine's own is held. The orchestration is `mediaremote.RunBatch` (`internal/mediaremote/batch.go`); the verb (`generate_image_batch.go`)
reads the file, wires the warm session (`pipeline.RunImageBatchAs`, which needs a ComfyUI and so cannot live in `mediaremote`) and keeps
the results file.

| flag | meaning |
|---|---|
| `--family F` | the family every job renders under (the single render's `family`; default: this machine's default binding) |
| `--route auto` (default) | where the jobs run is decided once, below |
| `--route local` | the warm batch on this machine whatever the lane says, as before the fleet existed |
| `--route remote` | each job sent to a roster node through the single-render call, one at a time; refused up front without `delegate_remotes`; there is no ticket, so a job whose every node is busy, text-leased or not answering is asked again after a pause (5 s, 15 s, 1 min, then every 5 min; chosen) for up to the queue's 8 h ceiling, and ends `queued`, never `failed`, if it finds no place by then (the jobs not sent yet are then queued at once); a fleet that cannot take the job as configured (no node serves it, a route is not configured, no node carries `refine=false`, a roster entry the tailnet guard refuses) is not waited for, and the job fails at once naming each node's miss; a node chosen and then lost when the job is sent, or after, fails it too; the node's recipe is not compared (below) |
| `--results r.jsonl` | one row per job, kept in `r.jsonl.partial` as each job is final and written whole, in job order, over `r.jsonl` when the batch ends; an earlier `r.jsonl` is untouched until then, so a batch the command refuses leaves it whole and one that dies leaves `r.jsonl.partial` with the jobs that ended; the run that reopens the name (the `--resume` that recovers a killed batch) first sets that record aside as `r.jsonl.partial.prev` (one is kept, the newest; an empty partial never replaces it), says so, and puts it back if the batch is then refused; opened before anything runs, so an unwritable path, a folder, and on Windows a read-only file stop the command with no lane time spent; it may not name the `--batch` file |
| `--resume` | skip a job whose `out` already holds a file that proves itself (below) |
| `--jobs N` | the most tickets in flight (0 = the default below); applies to the ticket path only |

**Where the batch runs.** Decided once, at the start, and never mixed:

1. **One warm session** (`RunImageBatchAs`: the checkpoint loads once, the batch boundary tears ComfyUI down once) when `--route local`;
   when the machine has no `delegate_remotes`; when the process runs under an inherited lease (`gpu reserve -- local-offload ...`: its card was
   chosen for it); when it has no queue (the lease refuses the state root, so there is nowhere to keep a ticket; a config with no `state_dir` has one); when this machine's lane is **free** (`MediaLaneFree` for the batch's
   family: free, and readable); and when the lane is not free but **no roster node renders the recipe**. The last is the honest limit: krea2 and
   the 2512 build exist on this machine's files only (the nodes hold no such weights, so the recipe match of ADR 0082 finds none), so a batch of
   either runs on the workstation as before. The batch **keeps its place in the lease line** for the lane for up to the queue's own 8 h ceiling
   (`BatchHoldWindow`, chosen, not measured), not the 90 s `gpu_wait_ms` window: the command blocks until it has the lane instead of coming back
   `queued` with exit 0 to be re-run from the back of the line (`TestBatchWithNoRemoteMatchRunsWarmOnThisMachineAndSaysWhy`,
   `TestRunImageBatchWaitingKeepsItsPlaceUntilTheCardFrees`), and Ctrl-C ends the wait at once with the rows `queued`
   (`TestRunImageBatchWaitingEndsOnInterruptAndLeavesNoLease`). The line it prints names each node and why it does not match (`not-capable`, the
   key that differs), which is also in the JSON as `cluster`. Per-job tickets would be one cold start per picture on the same lane, which is
   slower than waiting for it once. Every other warm case (a free lane, `--route local`, no fleet, an inherited lease, no queue) waits
   the pipeline's own `gpu_wait_ms` window and defers as before; a node that gains the weights later matches by recipe and the batch then splits.
2. **One ticket per job** when the lane is not free (busy, or the pipeline could not read it) and at least one roster node renders the recipe.
   Each job is the ticket queue's own call (`RunWith`, blocking), so it starts on the first matching lane that frees, here or on a node, in file
   order, with every guarantee of the queue. A node is claimed by the ticket that has a job on it, so **a batch never sends two jobs to one
   single-job node at once** (`TestBatchNeverDispatchesTwoJobsToOneSingleJobNodeAtOnce`). The job reaches the node as it was written:
   `refine=false` only when the job says so, the seed, the negative, `width`, `height` and `steps` exactly, the family under the node's own name
   (`TestBatchJobsPlacedThroughTickets`). The output is fetched, checked against the sha256 the node published, to the exact `out`. A node renders
   under its own zero-warm lifecycle (ADR 0009): one ComfyUI cold start per job. There is no mixed local/remote work stealing and no batch task on
   the fleet in v1: a node renders one job per ticket and the warm session exists only here. **A ticket that ends without a place is not a failed job
   and is not sent again**: the queue's own ceiling (a ticket that waited 8 h without a lane anywhere) and a ticket it could not take over carry the held
   class like a lost race does, but they come from a ticket that is over, so the job is `queued` with the queue's reason and `--resume` renders it; a new
   ticket would only wait another ceiling (`TestBatchTicketThatExpiresIsQueuedAndNotSentAgain`). Once one job has waited the ceiling, the jobs not yet
   sent are queued at once as `not started`, not sent to wait it again in turn (`TestBatchTicketCeilingQueuesTheJobsNotYetSent`).
3. **One at a time** through the single-render call when `--route remote`, or when this machine has no image lane at all (a thin client). The claim
   belongs to the ticket queue and that call picks a node without one, so two jobs in flight could pick one idle node; `--jobs` is told it did not
   apply (`TestBatchOneAtATimeWhereNoClaimProtectsANode`). Three things are different here, and the first line printed says them. **A busy place
   is waited for**: there is no ticket to hold the job, so a held card (a node's `gpu_busy`) or a fleet with no node free to take it (every node
   unreachable or text-leased, or a node refusing the send for want of room: the delegator's own capacity defer) is asked again after a pause until
   the job is placed, Ctrl-C ends it, or the queue's 8 h ceiling passes, and then the job is `queued` and `--resume` renders it, and so are the jobs
   not sent yet (`TestBatchSerialCeilingQueuesTheJobsNotYetSent`). **A place that can never come is not waited for**: `pickNodeFor` files
   `core.ErrClassNoNodeConfigured` (the defer class stays capacity) when it has no node and every node was left out for a configuration fact (it does
   not serve image generation, a route the call needs is not configured on it, it cannot carry `refine=false`, the tailnet guard refused its roster
   entry) and none for a reason that passes (unreachable, text-leased), and the job fails at once with the reason naming each node's miss; one
   node that may take it later keeps the wait (`TestBatchSerialFailsAtOnceWhereNoNodeCanEverTakeTheJob`,
   `TestPickNodeFilesNoNodeConfiguredOnlyWhenEveryNodeIsLeftOutForConfiguration`, `TestBatchSerialStillWaitsWhereAnyNodeMightTakeTheJobLater`). Any
   other answer (a failed render, a refused contract, a node chosen and then lost when the job was sent or after) is final at once
   (`TestBatchSerialWaitsForAPlaceAndNeverFailsTheJob`, `TestBatchSerialEndsQueuedAtTheCeilingNeverFailed`, `TestBatchSerialWaitsOnlyForAPlace`,
   `TestBatchSerialInterruptEndsTheWait`). **The recipe is not compared** with this machine's: a row carries
   the `family`, `license` and `commercial_use` the node that rendered it reported (nothing when it reported nothing), and the printed header
   names none, so a node whose own default is another family is never recorded under this machine's default binding
   (`TestBatchSerialRowsCarryTheNodesOwnFamilyAndLicense`, `TestBatchPayloadSerialCarriesNoLocalFamilyOrLicense`). **A family only the nodes
   define is carried by name** under `--route remote`, and on a machine with no image lane, as a single render carries it; a machine that renders
   locally still refuses a name it does not know (`TestBatchCarriesAFamilyThisMachineCannotResolveWhereItCannotRenderLocally`).

**How many in flight.** `--jobs N`, else 1 for this machine's lane plus one per roster node whose recipe matches (busy or idle: a busy node
frees), at most 8, and never more than there are jobs (`TestBatchNeverHasMoreThanNInFlight`). The cap of 8 is a chosen constant, not a
measurement; more tickets than lanes only wait. A job whose single render comes back "a card is held", because a sibling job took the lane a
moment after it read free, is sent again, at most twice (chosen): asked again, a call that finds the lane busy becomes a ticket and waits, so the retry
is what turns a lost race into a wait. A lane that refuses the same job three times in a row is a place not had, so the row is `queued`, never `failed`; any
other failure is a `failed` row at once, and the queue's own end of a ticket is not a lost race and is not asked again (`TestBatchRetriesALostLaneRaceAndOnlyThat`).

**The rows.** One per job, in the printed JSON (`items`) and in `--results`: `{i, out, seed, ok, ms, error?, node?, family?, sha256?, verdict}`, the first
six keys being the ones `pipeline.parseBatchResults` reads from the render script's own results file.

| `verdict` | meaning |
|---|---|
| `rendered` | a lane ran it, and the file at `out` is there, not empty, and hashed (`sha256`); `node` is the roster node's id, or this machine's own id for a render here |
| `skipped-existing` | `--resume` found the file and it proved itself; `ms` is 0, there is no `node`, `sha256` is the file's |
| `failed` | it did not render; `error` says why |
| `queued` | the job has no result yet: the command ended before it did (an interrupt, a warm batch a busy lane kept waiting past its window), or no lane or node had a place for it (the queue's ceiling, a card that stayed held, a job not sent because an earlier one had waited the ceiling); `error` says which, and `--resume` renders it |

`ms` is the render's wall time as its lane reports it (the wait for a lane is not in it). `family` is the name THIS machine gives the recipe: a node
renders the same recipe under its own name for it, and the row says where (`node`), not under which name. `sha256` is read from the file on disk; for
a node's job the fetch had already verified the file against the hash the node published. A lane that said it rendered a file that is missing, or
empty, or that came back from a node that published no sha256, did not render it (`TestBatchAJobIsRenderedOnlyWhenItsFileIsThere`). A failed job
leaves no file of its own at its path: an empty file a lane left is removed, and the fetch has already put a node's unverified file at `out`, so the batch moves it beside it as `<out>.unverified` (the
render took minutes; the row names where it went) and a later `--resume` renders the job again instead of taking the file for finished. A picture that was already at `out` before the batch is
not a failed job's to remove: a render that fails keeps it (the render helpers write atomically), and only the node's file the fetch put at `out` replaced it. A job whose `out`
folder cannot be made fails alone, before any lane is asked for it (`TestBatchAnOutThatCannotBeMadeFailsOnlyThatJob`). The printed JSON keeps its shape
(`count`, `succeeded`, `failed`, `items`, `family`, `license`, `commercial_use`, `refine_fallbacks`) and gains `mode` (`warm`, `tickets`, `serial` or
`none`), `skipped` and, when no node matched, `cluster`; every item gains the row's keys. `refine_fallbacks` counts the warm items whose refiner fell
back, so it is present only for a warm batch with a refiner configured whose warm call ran and answered (a ticket or one-at-a-time render does not measure it, nor does a
warm call a busy lane kept waiting or that stopped, and a 0 would claim it did: `TestBatchPayloadReportsRefineFallbacksOnlyWhereTheyWereMeasured`). A one-at-a-time batch carries no `family`, `license` or `commercial_use` in its header, and its items carry the node's. A deferred warm batch now lists its jobs as `queued` items
(its `count` was 0).

**Resume is fail-safe by construction.** On 2026-10-09 a full disk left 21 zero-byte PNGs at the `out` paths, and a skip-existing script read them as
finished. `--resume` skips a job only when the file at its `out` exists, is a regular file, is not empty, **decodes whole as a PNG** (a file cut
short fails), and has the width and height the job names (a size the job does not name is not checked). The header is read first: a canvas that
is not the named size, or is over 64 Mpx (`resumeMaxPixels`, chosen, not measured), is rendered again without being decoded, because a decode
allocates the whole canvas and a stray huge file at an `out` must not take the RAM of a resume pre-pass (`TestResumeDoesNotDecodeACanvasItCannotUse`). Anything doubtful is rendered again
(`TestBatchResumeSkipsOnlyExistingAndVerifying`: a zero-byte file, a file cut short, another size, not a picture, missing;
`TestGenerateImageBatchResultsFileAndResume` through the verb). Two limits, said. A job with no `out` is never skipped, because its default name hashes
a seed minted afresh each run. A family that snaps the size (qwen-image-2.1 floors to 32) gives back a smaller picture than a size off its grid asks
for, which never matches and is rendered again, so give a resumable batch sizes on the family's grid. Resume does not read the previous `--results`
file: the files are the truth, and each run writes its own full results.

**Before anything runs** the command refuses, by name: an empty batch, an empty prompt (`job N: empty prompt`, as before), two *different* jobs for one
`out` (the second would replace the first; two identical jobs are one picture; one file spelled two ways, relative and absolute or in another case
where the file system ignores case, is one `out`: `TestBatchOutsThatNameOneFileTwoWaysClash`), an unknown family on a machine that renders locally,
an unknown route, a negative `--jobs`, `--transparent`, `--results` naming the `--batch` file, and `--route remote` with no `delegate_remotes`
(`TestBatchRefusalsBeforeAnythingRuns`, `TestGenerateImageBatchFlagRefusals`, `TestGenerateImageBatchResultsFileIsNeverTheJobsFileNorLostToARefusal`). A `--remote` that is not in
`delegate_remotes` keeps the batch on this machine and says why.

**Seeds, names and adoption.** The verb gives every job its seed and its `out` once, before it decides where the batch runs
(`pipeline.NormalizeImageBatch`), so both paths name a job the same way; the family is part of a default `out`'s hash, as it is of the single
render's, so one prompt and seed under two families are two files (`TestNormalizeImageBatchFamilyIsPartOfTheDefaultOut`). A job with no seed gets a
random one on each run. An interrupted batch leaves a job already on a node running there (it cannot be recalled, ADR 0064) and its ticket for the
next *identical* command to adopt: run the same file again, and give each job an explicit `seed`, or the re-run's minted seeds are new requests and
the node's job is rendered twice.

**Interrupt.** The verb runs both paths under one interrupt context. The first Ctrl-C cancels a ticket that has sent nothing and, on the warm
session, the render itself: the child is built with `exec.CommandContext` and a tree kill (`gpugen.Generate`), so it and its descendants are killed,
and the process stays up to release the lease and tear ComfyUI down. Before the fleet batch the warm session ran under `context.Background()` and a
Ctrl-C killed the whole process. The second Ctrl-C is the default one.

**Exit status.** 0: every job has its picture, or a busy lane deferred the whole warm batch (nothing ran, the jobs are `queued`, as it always
was: so a 0 is not "every picture is there", count the `queued` rows). 4: some job failed (`imagegen.BatchExitJobsFailed`), whether the batch ran
every job or was deferred after a job had failed before any lane was asked. 1: it stopped (the warm call's error), was interrupted (`queued` rows),
a job found no place (the queue's ceiling, or a card that stayed held: `queued` rows, and the jobs not sent yet with them), or it was refused before any job, so a wrapper tells "some failed"
from "stopped" (`TestBatchExit4OnAnyFailure`, `TestBatchInterruptLeavesQueuedRowsAndExit1`). The line the command prints says which: an interrupt (a job already on a node cannot be
recalled), a job that waited the ceiling (and the jobs behind it were not sent), or jobs no lane or node had a place for (`TestBatchExitSaysWhyTheJobsAreUnfinished`).

**Named limits.** Nothing here was run against a live lease, a live node or a render: the tests use the real fleet-node servers, ticket files and
results file with fakes for the pipeline and the renderer, and a node stub for the warm session. The 8-ticket cap, the two retries, the 8 h hold
and ceiling, the pauses between asks and the 64 Mpx resume ceiling are chosen, not measured, as are the queue's own constants. **The warm path writes
its rows when its session ends**: the render script runs to the end before the batch sees any job's result, so the rows of the jobs it renders reach
the results file (`<results>.partial`) together at that point, and a kill, crash or power loss mid-render leaves, for those jobs, only the render
script's own `batch-<stamp>.results.jsonl` in the media dir. A pre-flight failure or a skipped job is written as soon as the mode is chosen (a lane
and roster read, before any job runs), and the ticket path writes each row as its job ends. The batch moves a rejected file onto `<out>.unverified` over
any earlier file of that name. A node job costs a ComfyUI cold start, so for the 2.1 families the ticket path pays off when the lane is held
by a long job and a node is idle, not when the lane is free (that is the warm session's case). The ticket path inherits the queue's named limits:
one line per delegating machine, a CLI ticket dies with the CLI, a late-binding gap of up to a scan interval.

## Comfy workflow templates catalog (phase A)

`render/templates-catalog.mjs` is a read-only catalog of the ComfyUI workflow templates a node
carries: the `comfyui-workflow-templates` package that ComfyUI itself pins, a checkout of the upstream
repository, or a `pip download` extract of a candidate version. It lists and classifies. It never runs a
template, downloads a model, installs a package, changes a config or touches the network, and the only
file it writes is the one named by `--out`. The `offload_run_template` tool reads the same catalog (see
[the tool's section](mcp-server.md#workflow-templates-offload_run_template) for what it lists, gates and
runs); there is still no config key, fleet task or route for the catalog itself. A template is a UI-format
graph and `run-graph` takes API-format graphs only (the tool description says so, and
`preflight-graph-file.mjs` reads each node's `class_type`), so running one needs the converter
([below](#template-to-api-converter-rendertemplate-to-graphmjs)), which this phase does not contain.

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
| model files | `properties.models` of active nodes (class, URL, hash), plus widget files nobody annotated (a node's `widgets_values` as a list or as an object keyed by input name, and an active subgraph instance's own `widgets_values`, listed with no class); files that only bypassed nodes reference are reported apart and not required |
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
(Accepted 2026-10-14, together with the third state) cannot say `conditional`, which is why the map has a third class. The map's 50 entries
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
the FLUX-family ones stay hidden. The catalog reports this gate and runs nothing; the run-template tool applies a
stricter one in its first release ([its section](mcp-server.md#workflow-templates-offload_run_template)): a template runs only
when its class is permissive (or it has no weights), an `ack_required` template is refused under its own class, and nothing
acknowledges it. The catalog row also names the input files the template's active loaders read (`input_files`).

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
where the file is), `in_subfolder`, `case_mismatch`, `extension_not_listed`, `class_unregistered`, and, for an embedding a prompt names, `embedding_unverified`. That lookup fails closed and is narrower than
ComfyUI's (ComfyUI warns and skips a missing embedding, and finds the others in the roots and every subfolder through a set, so which
directory answers varies from run to run): the harness accepts a name only when exactly one physical file answers it, case-exactly, anywhere in the roots and
their subfolders (a `.git` directory too: the class listing skips it, ComfyUI's embedding search does not), a
file reached by several paths (a link, a hard link, a root inside a root) counting once, and the status carries the reason: a case variant
answers it, more than one file could answer it, or a file answers another name ComfyUI may try for the template's prompts, listed as
`embedding_probes`. A loader widget's embeddings file (an annotation that records no prompt name) is met by its exact path like any
other class.
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

### Builder drift guard

`render/builder-drift.mjs` guards the `render/wf-*.mjs` builders that were copied from a template of the
`comfyui-workflow-templates-json` package. A template widget that changes under a builder that copied it
(upstream workflow_templates PR #1308: a pose-end percent wired to the pose-start input) goes unseen
otherwise, because the builder keeps building the old recipe.

- **What is recorded.** `render/testdata/builder-drift.json`, per builder and view (a preset or a template
  variant): the template name, the package versions, `templateSha256` and `builderSha256` (sha256 of the recipe
  widgets as the template and as the builder's output graph carry them) and one digest pair per widget with a
  status: `same`; `deliberate` (the builder knowingly differs, the reason is in the spec); `unreviewed` (a
  difference the first run found that nobody has judged yet); `template-only` (a wiring check, or a pinned
  pass-through expression). Names and hashes only: no template JSON, text or widget value is stored in the
  repository.
- **Where each widget lives, and why a locator never reads a linked widget.** `render/builder-drift-specs.mjs`
  pairs a template locator with a builder-graph locator per widget. A widget whose input is linked (to a subgraph
  input, or to another node) keeps an old value in its own `widgets_values`; the value that runs comes through
  the link. So `W` (a node's widget, by index and input name) and `I` (the widget a root subgraph instance
  promotes) throw when the widget they point at is linked, naming the builder, the key and the fix, and the spec
  points at the source instead: `I` for a subgraph input the root instance promotes, `W` on the primitive that
  feeds it (a primitive behind a switch is read on the branch being checked), `P` for the legacy form in which the
  interior widget is linked to a subgraph input, the root instance lists it in `proxyWidgets`, carries no value at
  its position in that list (the instance's `widgets_values` follow the list, not the order of the subgraph's
  inputs) and has nothing wired to its input for the widget, so the interior widget is the only place the value
  lives (the extractor checks all four facts). A wiring is checked first and sends the spec to the source that
  feeds it, whatever else the instance carries, because `I` throws on a wired input. A value the instance carries
  is read by `I` only where its position in `proxyWidgets` is also its place among the subgraph's widget-typed
  inputs, because `I` indexes by input order, not by `proxyWidgets`; the message says which case it is, and where
  the two positions differ, or the input is not widget-typed (so `I` does not count it), it says that no locator
  reads that position.
  A math node that only passes a value through (the LTX frame rate) is pinned as a template-only item,
  so a changed expression cannot hide behind the locator that skips it.
- **Builder side, offline.** `node --test render/builder-drift.test.mjs` has one test per builder
  (`builder drift: wf-krea2`): it rebuilds the graph with fixed arguments and compares every widget; a moved
  widget fails `builder drift: <builder> [<view>]: recipe widget changed in the builder's output graph: <key>`.
  A new `wf-*.mjs` file must be guarded or listed in `NOT_TEMPLATE_DERIVED` with a reason, or the coverage test
  fails.
- **Template side, with the package.** With the package's `templates` directory at hand (see the order below) the
  same widgets are read from the template JSON; a change fails naming the builder, the template and the package
  version the builder was last verified against, and with every widget unchanged the recorded `templateSha256`
  must equal the recomputed hash (a hand-edited manifest fails). A newer package with every guarded widget
  unchanged is a note. The package is not in this repository, so without a directory the template-side test is
  skipped with that reason.
- **CLI.** `node render/builder-drift.mjs check|explain|record [--templates-dir DIR | --comfy-dir DIR]`. `check` is
  the same as the tests (the template side runs when a directory is given, so it can be pointed at a candidate
  package extract before an upgrade); `explain` prints the template and builder values side by side;
  `record` rewrites the manifest after a person has compared the builder with the template, and refuses a
  builder/template difference the spec does not declare. The CLI fails closed: a flag that takes a value and gets
  none (an empty or unset shell variable, `--templates-dir "$UNSET"` or `--templates-dir=`) exits 2, as does any
  argument that starts with `-` and is not a known flag (`--template-dir`, and the single-dash `-templates-dir`
  or `-x`), a value given to a switch (`--strict=1`) and a second positional after the verb. So neither a
  mistyped flag nor an empty flag value can skip the template side. A directory named only through the
  environment (`COMFY_TEMPLATES_DIR`, `COMFYUI_DIR`) that is unset, or names a tree without the package, still
  leaves the template side skipped with a note: that is the case `check --require-templates` is for. It makes a
  missing template side an error (exit 2) instead of a note, for an upgrade runbook. `check --strict` also fails (exit 1)
  on every `unreviewed` manifest entry; a plain `check` leaves those green. Both flags belong to `check`.
  **The runbook command `check --require-templates --strict` exits 1 on the committed manifest, by design,
  until the two `unreviewed` entries are decided:** it prints exactly `2 failure(s)`, one for
  `wf-qwen-image-edit` `[full]` `ksampler.cfg` and one for `wf-qwen-image-21` `[edit]` `encoder.resolution`.
  Those two are the expected failures, with the template side unchanged. Any further line, or a template-side failure, is new and is the finding.
  Use the command without `--strict` where a green gate is needed; it reports the same template side. A template
  or manifest file may start with a byte-order mark; one that is not JSON, or a template with no `nodes` array,
  fails with a message that names the file (the template side names the builder too).
- **Which directory is read.** The CLI and the tests use one function, and the first source that is set decides:
  `--templates-dir`, `--comfy-dir` (a ComfyUI tree), the `COMFY_TEMPLATES_DIR` environment variable (a `templates`
  directory), the `COMFYUI_DIR` environment variable (a ComfyUI tree). A tree named on the command line that has no
  package is an error; one named by the environment that has none means "no package here" and the template side
  is skipped. `--templates-dir` and `COMFY_TEMPLATES_DIR` take the package's `templates/` folder: the package root
  above it (a directory that holds a `templates/` folder and no top-level `.json` file) is refused with exit 2 and the
  path to pass, because it matches no template and the manifest records which directory was read.
- **Not guarded.** Seeds, prompts, model file names, sizes the caller sets, and the builders with no copied
  widget (`wf-hunyuan15-i2v`, `wf-sdxl-inpaint`, `wf-upscale`, each with its reason in the spec).
- **Known limits.** What a green run does not tell you:
  - Node mode is never read. A bypassed or muted node, or a bypassed or muted subgraph instance, still reads as
    live, so a template that bypasses a node the builder keeps (or the reverse) is not a difference to the guard.
  - Wiring is mostly unguarded. One `D` item (the Wan Animate 2 pose start and end percent) keeps two inputs on
    separate sources. The `wf-wan22-i2v` high and low pairs share one template locator each: the steps (node 128),
    the cfg (node 126) and the step split (`high.end_at_step` and `low.start_at_step` both read node 127), so a
    template that split any of the three would not be seen. A `feeds` locator (the source of an input, by node id and slot)
    would cover both; it is not built, and the code for it comes later.
  - `W` and `I` do not consult `proxyWidgets`; only `P` does. A widget a template starts promoting through
    `proxyWidgets` is seen by a `W` or `I` locator only through the linked-widget refusal.
  - The two `unreviewed` entries stay green in a plain `check` for good: a difference nobody judged does not
    fail. `check --strict` fails them.

First run, on package `comfyui-workflow-templates-json` 0.1.96: eleven builders, eighteen views, and every
guarded widget equal except six entries, which the manifest records. Two are `unreviewed` (nobody has judged
them): `wf-qwen-image-edit` full preset (template CFG 4, builder 3.0) and the `wf-qwen-image-21` edit encoder
resolution (template 0, builder 1024). Four are `deliberate`, stated in the builder's header: the
`wf-qwen-image-21` step count in both views (the model README's 40, template 25) and the `wf-wan-animate2`
pose-cache device and dtype (pinned to cpu and default). The `wf-h3-av` turbo path equals its template: the
steps, LoRA file and LoRA strength are read from the root instance that promotes them (the steps primitive's own
widget is linked and is not the value that runs). `wf-ltx25-i2v` reads its frame rate and video fps the same way,
from the root instance that promotes the frame-rate value (the primitive that holds it is linked to that input, so
its own widget is stale); only the two i2v bypass switches are read from a primitive, the boolean one that feeds
them, and the math expression that passes the frame rate through is pinned as a template-only item.
`wf-wan-animate2` copies the `_distilled` template: its
pose strength, start and end percent and reference strength equal the builder's, and the template feeds the
start and the end percent from two separate subgraph inputs (a wiring check keeps it so).

Open item: the `wf-qwen-image-edit` full preset runs CFG 3.0 where the template's effective non-LoRA CFG (the
primitive that feeds the sampler) is 4. The 3.0 is the value of the sampler's own widget, which is stale because its
input is linked to the switch, so this is probably a real builder defect and not a choice. It is recorded, not
changed: the builder keeps its recipe in this change. Resolve it by setting the builder to the template's value
(and re-recording) or by declaring the difference `deliberate` with a reason; the same goes for the
`wf-qwen-image-21` edit encoder resolution. Until then `check --strict` is red on both.

## Template-to-API converter (`render/template-to-graph.mjs`)

A workflow template is a UI-format graph (`nodes[]`, `links[]`, `definitions.subgraphs`, `widgets_values`).
`POST /prompt`, the `run-graph` door and the render builders take API-format graphs only:
`{"<id>": {"class_type", "inputs"}}`. `render/template-to-graph.mjs` converts the first into the second. It is the
converter the templates catalog (phase A, above) did not contain. The run-graph runner calls it for a template run
(`comfy-run-graph.mjs --template NAME`, behind `offload_run_template`): once ComfyUI is up, with the running ComfyUI's own
`/object_info` and version, before preflight, so a refusal is the defer `TEMPLATE_CONVERT_REFUSED` (and a model file the converted graph loads that the run's manifest does not name is the defer `TEMPLATE_MODEL_UNLISTED`, `unlistedModels` in `template-run.mjs`; an embedding the prompt text names counts as one) and the graph that is
checked and posted is the converted one. `render/template-run.mjs` finds the template by name in the node's own package
(a name the node's index does not list is an unknown template, never a file read), passes the index's `minComfyUIVersion`
and applies the caller's overrides to the converted graph. The converter itself is still a pure function, there is no
config key or fleet task for it, and the ComfyUI lifecycle module is untouched.

**Class: none.** No GPU slot, no lease, no ComfyUI process. It is a pure function of three inputs: the template, an
`/object_info` listing and options. The listing is never fetched here: the caller passes the one a running ComfyUI served
(or a saved copy of it), and a listing from an older or newer ComfyUI than the one that will run the graph is a mismatch
for the caller to avoid. It imports only `node:` builtins, opens no network, starts no process and writes no file (a test
fails if it does).

**Clean room.** It was written from the public descriptions of the two formats, the MIT-licensed template files, the
harness's own builders under `render/` and the shape of an `/object_info` listing. The GPL-3.0 frontend (which has its own
graph-to-prompt code) and ComfyUI's server code were not opened or read as source. The frontend is a black box: its own
export of a template is the golden the converter is compared with.

```bash
node render/template-to-graph.mjs --template <template.json> --object-info <object_info.json> \
    [--comfy-version X.Y.Z] [--seed N] [--min-comfyui-version X.Y.Z | --index <index.json> [--name NAME]] [--meta]
```

It prints one JSON document. Exit 0: `{"ok":true,"graph":{...},"warnings":[...],"warnings_dropped":0,"pinnedSeeds":[...],"stats":{...}}`. Exit 1:
`{"ok":false,"refusal":{"code","message","details"}}`, a typed refusal of a call that was well formed. Exit 2: the same
shape with code `usage` or `unreadable_input`, a call that could not be read. `--index` reads the template's minimum from
the templates package's `index.json` (the file that carries `minComfyUIVersion`; a template file does not) by the template's
file name, or by `--name`; a template the index does not list has no known minimum, and a ComfyUI version given when no minimum is known is neither refused nor silent: the graph is printed with a `version_check_skipped` warning. From code:
`convertTemplate(template, objectInfo, { comfyVersion, minComfyUIVersion, seed, meta })`, and
`compareApiGraphs(actual, expected)`, which lists the differences between two API graphs node by node (class and inputs;
key order and `_meta` are not differences unless asked).

**What it does.**

| rule | behaviour |
|---|---|
| muted (mode 2) | the node is dropped; an input wired to it is left out with an `input_dropped` warning, and a widget input keeps its stored value (`widget_link_dropped`) |
| bypassed (mode 4) | the node is not emitted; the i-th output of a type takes the i-th connected input of that type, so the positive and negative conditioning of a bypassed ControlNet apply node pass straight through (a union such as `IMAGE,MASK` matches when the two share a member, `*` matches any); chains resolve; no connected input of the type drops the wire (`bypass_no_match`); a type with a different number of outputs and connected inputs is refused (`ambiguous_bypass`), because which input an output takes would be a guess. Only an output something reads is resolved, so a bypassed node nobody reads is never refused |
| subgraph instances | expanded; inner nodes get the id `<instance>:<inner>` (nested: `<a>:<b>:<inner>`); a wire into the instance reaches the inner nodes through the definition's input node (id -10); an inner wire into its output node (id -20) is what leaves the instance; a bypassed or muted instance is handled as a node |
| instance widget values | `properties.proxyWidgets` (`[innerNodeId, widgetName]` pairs) when the instance has them: its `widgets_values` belong to those inner widgets, in order. Otherwise they align with the definition's widget inputs, not with the instance's own `inputs` array, which can list fewer; an instance wire wins over its widget value |
| `Reroute`, `PrimitiveNode` | resolved into the inputs they feed (a primitive's first value is a literal on every consumer); neither reaches the graph |
| `Note`, `MarkdownNote` | skipped, also when muted |
| link beats widget | a widget input that is wired takes the wire, whatever value it stored |
| widget values | mapped to named inputs in the listing's `input_order` (the key order when a class has none), `required` then `optional`; `hidden` inputs are not inputs. An input is a widget when its type is INT, FLOAT, STRING, BOOLEAN, COMBO or COLOR, a list of options, or a union type with a `widgetType`, unless `forceInput` makes it a socket |
| `control_after_generate` | a flagged INT owns one hidden slot after it (flag `true` or a string such as `fixed`), which must hold one of the four control words. A listing that has no flag at all is read by the data: a seed-named INT followed by a control word owns the slot, one followed by anything else does not |
| upload slots | after a COMBO with `image_upload` or `video_upload` one extra value, after one with `audio_upload` two (counted from the template files) |
| dotted dynamic-combo names | the selected option's widgets follow the combo as `<combo>.<child>`, nested to any depth, in the option's input order |
| dotted autogrow names | the children (`values.a`, `values.b`) are wires named by the node's own input slots; an autogrow group of widgets is refused |
| display-only values | `PreviewAny`, `GetImageSize`, `SaveGLB` and `Preview3D` store trailing values of frontend-only display widgets; up to 3, 1, 1 and 1 of them are ignored (`display_values_ignored`), one more is a refusal |
| seeds | with `seed`, an input is pinned when it is named `seed` or `noise_seed`, the listing's control makes it a seed, and it holds a stored or primitive value. A wired seed, a generic INT that shares the control (a `PrimitiveInt`) and a sampling seed with no control are left alone, each as a `seed_not_pinned` warning naming the node and input with its reason (`wired`, or `no_control`); the change is listed in `pinnedSeeds` |
| fewer values than widgets | the listing's `default` fills the input and says so (`default_filled`); with no default the input is left out (`widget_value_missing`) |
| long chains | `Reroute`, bypassed and nested-instance chains are followed in a loop with one visited set, so a chain as long as the limit below allows (99,998 `Reroute` nodes between two live nodes) resolves without a stack overflow and without a copy of the trail per hop; a loop in the wires through them is `link_cycle`, found by the visited set where it closes (the details name that node). Behind the set, one resolution may take no more hops than the wires of every scope entered, because a wire met twice is a loop: a longer walk is refused as `link_cycle` too, with `what: "hops"` in the details, so a defect in the set cannot become a hang |
| limits | every node met in every scope that is entered is counted as it is met, whatever its kind (live, muted, bypassed, `Note`, `MarkdownNote`, `Reroute`, primitive, and each subgraph instance), before any work is done for it: past 100,000 the call is refused (`graph_too_large`, `what: "nodes"`); the links of every scope entered (the root's and each instance's) are counted as the scope is entered, and past 1,000,000 the call is refused the same way (`what: "wires"`); subgraphs nested more than 64 levels deep are refused the same way (`what: "subgraph depth"`); a node id longer than 256 characters is a `bad_template`; at most 1,000 warnings are kept, and the ones past that are counted in the result's `warnings_dropped`. Real templates use a handful of nodes per level and three levels |
| id collisions | a node whose final id (`<instance>:<inner>` path included) is also another node's, in any scope and of any kind, and a subgraph definition id that appears twice, are `bad_template`, never resolved to whichever came last |

**What it refuses, with a code and the details that locate it, and no graph.** `bad_template` (a file that is no workflow, a node or a subgraph definition whose `inputs` or `outputs` is not a list, a node id longer than 256 characters, a node id two scopes both give and a subgraph definition id that appears twice), `bad_object_info`,
`bad_seed`, `bad_version` (a malformed version); `comfyui_too_old` (the template's minimum exceeds the given ComfyUI;
both versions are in the details) and `comfy_version_unknown` (a minimum is known and no ComfyUI version was given: the
version is passed in, never assumed); `unknown_node_class` (every class the listing lacks, with the node ids; muted and
bypassed nodes do not count); `graph_too_large` (more than 100,000 nodes of any kind, subgraph instances included, counted over every expanded scope, more than 1,000,000 links over every scope entered, or subgraphs nested more than 64 levels; the details give the limit, what it counts and where it was passed); `ambiguous_bypass` (a bypassed node with a different number of outputs and connected inputs of one type; the details give the node, the type and both counts); `group_node_unsupported`; `subgraph_definition_missing`, `subgraph_cycle`,
`subgraph_widget_mismatch`; `dangling_link`, `link_mismatch`, `link_cycle` (a loop in the wires, or a resolution that used one wire twice); `widget_mismatch` (more values than widgets, a
flagged INT not followed by a control word, a dynamic combo's value that is no option of it); `unsupported_input_type` (a
custom frontend widget such as an image comparer or a file-upload viewer, an autogrow of widgets, an input type with no
rule); `primitive_without_value`; `internal_error` (a defect in the converter, never a template's fault). The version check
runs first, before the seed, the listing and the template are looked at, so a too-old ComfyUI is never hidden behind another refusal. A refusal is a typed answer, and the converter does not try the next best reading.

**What it does not do.** It does not check that a required input is wired (`preflight-graph-file.mjs` does, against a live
listing), that a model file exists (the manifest satisfier does), or that a node pack is installed. It does not submit
anything.

**Evidence and status.**

- `node --test render/template-to-graph.test.mjs`: one test per behaviour on small hand-built UI-format graphs, a synthetic
  listing and two hand-worked goldens in `render/testdata/template-goldens/` (see its README: none of it is a frontend
  export). A mutated widget order turns the golden red (a swap of two inputs, a swap of two combos, a type-breaking move).
  A mutation run of 24 one-line changes to the converter, each applied to a copy and proved applied, turned the suite red
  every time. A review then found twelve one-line changes that survived it; the fixes for its findings were written red first, and a
  later run of 34 one-line changes (those twelve and the rules added since) turned the suite red every time too. A second independent check found the expansion cap charged only live nodes and subgraph instances: a template of a few hundred kilobytes, nested definitions each stuffed with `Note` nodes, stayed under the cap (65,535 instances) and ended in an `internal_error` after the walk had used gigabytes of memory. Every node met is now charged first, and tests pin the `Note`-, muted-, bypassed-, `Reroute`- and primitive-heavy bombs and an exact boundary of mixed kinds; the three undefined-link guards (a `Reroute`'s input, an instance's input, a live node's input) have a test each; the cycle tests assert which guard found a loop, so the hop bound behind the visited set cannot hide a set that stopped working; and the 99,998-hop chain test runs on a worker thread under a test-runner timeout, which a test body on the runner's own thread is never held to. A last independent check found that a hostile template could still exhaust memory or time in ways the node cap does not see; one test each, written red first, pins the wire cap (a definition of one `Note` and 2,000 links instanced 501 times is refused, 500 convert), the 1,000 kept warnings with the rest counted in `warnings_dropped`, and the 256-character node id.
- The value-for-value comparison with the frontend's own export is `TEMPLATE_GOLDEN_DIR` (a folder with `object_info.json`
  and `<name>.workflow.json` / `<name>.api.json` pairs); the test skips without it. It was run privately on two saved
  workflows with an API-format file beside each (the shape of the frontend's Save (API Format) export, `_meta` titles
  included; that the frontend wrote them is unverified): 50 of 50 and 20 of 20 nodes matched, inputs included, one of them a
  subgraph template with dynamic combos. Neither file is in the repository.
- A survey over a templates package (`TEMPLATE_SURVEY_DIR`) shows what refuses and why. On `comfyui-workflow-templates-json`
  0.1.96 (593 files) with an `/object_info` listing saved on 2026-08-14, which is older than the package: 405 convert, and
  every converted graph passes the graph check (each wire reaches a node that is in the graph and an output slot its class
  has, and the output's type, from the listing's `output` list, shares a member with the type the target input declares;
  5,055 nodes). The type check exists because the slot-matching rule for a subgraph's outputs was once pinned by no test:
  dropping it left the suite green and put 37 wrong wires into 9 converted templates (a VAE into a MODEL input, a mesh into
  an image input), which the type check flags and the new synthetic test, with three outputs of three types read in crossed order, turns red. The refusals: 86 `unknown_node_class` (classes newer than the listing), 45 `widget_mismatch`
  (mostly schema skew between package and listing, and display widgets of nodes such as `CustomCombo`), 40
  `unsupported_input_type` (mostly the image comparer), 2 `ambiguous_bypass` (a bypassed node with one output and two connected inputs of its type, which the earlier first-of-type reading converted), 15 `bad_template` (files that are not workflows). With `seed` set, 18 of the 405 leave at least one seed-named input alone and say so (`seed_not_pinned`: 6 wired seeds, 15 seeds with no control). The counts
  describe that pair of package and listing and nothing else.
- A malformed-field fuzz over the 405 convertible templates (29,928 cases: `inputs`, `outputs`, `widgets_values` and `properties` of the first nodes and of every bypassed node and subgraph instance replaced by an object, a string or a number, and the slot lists, nodes and links of each definition) ends every case in a graph or a typed refusal, never in `internal_error`; the same fuzz on the converter as first written ended 921 of its cases in one.

**Known gaps.** The acceptance run, at least 10 non-API templates compared node by node with frontend-exported goldens, is
a follow-up (the two private pairs are API-format files placed beside the workflows; that the frontend wrote them is unverified, so
"frontend-exported" is a description of intent, not a checked fact). It needs a running ComfyUI to serve a current `/object_info` and a scripted frontend export of each template,
which is GPU-lease work (ComfyUI is started only as a lease-wrapped command and stopped when it ends), so it was not run
here; two matching pairs are not ten, and no template class is claimed beyond those. Every node in both private pairs is mode 0, so
no comparison with a frontend export covers a bypassed or a muted node. Untested against a frontend export:
the bypass pairing rule (positional by type; two templates of the survey above, which the earlier first-of-type reading converted, are now refused for it), the display-only counts, COLOR widgets and the upload slot counts (read
from the template files). A class with a custom frontend widget is refused, not guessed; each such class is a candidate for
the follow-up to decide. The caps bound the nodes of every kind, the wires, the hops of one wire resolution, the nesting depth, the warnings kept and the length of a node id. They do not bound the `widgets_values` copied per instance, nor the scans of a definition's inputs and links that each instance, and each read of one of its outputs, makes, so a hostile template can still use time and memory in proportion to the size of its values and definitions times the number of instances. The converter is meant for trusted template packages.
