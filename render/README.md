# render — local image generation (ComfyUI)

A small, dependency-free Node tool that drives a local **ComfyUI** server to generate images. It is the *generation* side of the harness's vision capability — the harness **reads/assesses** images (`local-offload vqa|ocr|extract-image|assess-image`), and this **generates** them. It bakes in **no** style, prompt, model, or tuning: everything is a flag, or supply your own workflow.

## Requirements
- **Node 18+** (built-in `fetch`)
- A running **ComfyUI** (default `http://127.0.0.1:8188`) with the checkpoint/VAE you reference installed.

## Three modes

**1. Parameterized SDXL text2img (convenience)**
```bash
node render/comfy-render.mjs out.png "a misty pine forest at dawn" --steps 30 --cfg 7
```
Flags (all optional, neutral defaults): `--negative` (default *empty*), `--ckpt`, `--vae`, `--steps`, `--cfg`, `--sampler`, `--scheduler`, `--width`, `--height`, `--seed`, `--prompt` (the non-positional way to pass the prompt), `--wait-sec`, `--api`. Env: `COMFY_API`, `COMFY_CKPT`, `COMFY_VAE`, `COMFY_WAIT_SEC`, `COMFY_DEAD_SEC`. `--prompt`, `--wait-sec`, `--api` and the two poller env vars apply to **every** mode, not just this one.

**2. Any ComfyUI workflow (full control)**
```bash
node render/comfy-render.mjs out.png --graph my-workflow.json
```
`--graph` POSTs an arbitrary ComfyUI **API-format** workflow as-is — any nodes, any model, any pipeline (img2img, ControlNet, LoRA, a different base model, …). Export it from ComfyUI via **Save (API Format)**. Use this for narrow/specific pipelines; use mode 1 for quick general text2img.

**3. Model-family graphs (`--family`)**
```bash
node render/comfy-render.mjs out.png "<prompt>" --family qwen-image --ckpt qwen-image-2512-Q5_1.gguf
```
`--family hidream-o1|hidream-o1-dev|qwen-image` builds the graph the model actually shipped with instead of the generic SDXL one (the SDXL-shaped `CheckpointLoaderSimple` + `KSampler` graph is wrong for a pixel-space DiT and cannot load a split-file UNET at all). Both families require `--ckpt` (or `COMFY_CKPT`); `hidream-o1` renders at its native 2048x2048 unless you override `--width`/`--height`. `qwen-image` additionally takes `--preset full|lightning4` (default `full`; the preset fixes the steps/cfg/LoRA pairing, so `--steps` and `--cfg` are accepted only **together** — a half-override exits 2), `--clip`, `--vae` (default `qwen_image_vae.safetensors`; `--vae builtin|none|checkpoint` is rejected because the UNET file carries no VAE weights), `--lora`, `--lora-strength`, `--shift`, and `--loader auto|gguf|unet`. Which family a given machine's checkpoint belongs to is per-machine binding config, not shared code — see [`docs/systems/media-generation.md`](../docs/systems/media-generation.md).

## Hard exclusions (no people / no text)
SDXL honors real **negative** prompts at normal CFG, so exclusions are enforceable *when you want them* — just pass them (nothing is assumed by default):
```bash
node render/comfy-render.mjs out.png "an abstract product hero" \
  --negative "person, people, face, hands, text, letters, words, watermark, logo"
```
(The fast distilled 2026 models run at CFG≈1 and ignore negatives — that's why the convenience graph is SDXL.) Then QA the result: `local-offload assess-image out.png --json` → `{has_people, has_text, matches_brief, notes}`.

## Notes
- Writes a single PNG to the output path you give; first run after a model swap is slow on a low‑VRAM card. The poller waits **30 min by default**, tunable with `--wait-sec` or `COMFY_WAIT_SEC` (the Go harness sets `COMFY_WAIT_SEC` to match its own timeout, and its process-tree kill stays the hard stop). A ComfyUI that stops answering *mid-render* aborts early after `COMFY_DEAD_SEC` (default 240 s) to release the GPU slot — consecutive failed polls, not a slow one: a healthy server still answers `/history` with no output yet and resets the counter. `COMFY_WAIT_SEC` is wall-clock time, not a poll count (0.158.1). A ComfyUI that answers HTTP but can no longer render (a sticky CUDA error leaves `/history` empty and `/system_stats` answering an error) ends the wait after two HTTP-error answers from `/system_stats` in a row, probed 15 s apart, with a *server unusable* error; `comfy-render.mjs` then exits 3 (1 for any other failure, 2 for a caller mistake) and `comfy-generate.mjs --batch` stops at that job.
- **The output is written atomically (0.178.0).** The png (and every other output of the helpers in this directory) goes to a staged sibling, `<out>.partial-<pid>-<n>`, in the same directory and is renamed over `<out>` only after the write finished, so a failed render never leaves a file at `<out>` and a good file already there is kept. Before, `writeFileSync(out, …)` truncated the target first: on 2026-10-09 a full data drive left 21 zero-byte PNGs in a 36-picture batch, and a skip-existing script reads a zero-byte file as finished. A failed write names the output in its error (`ENOSPC: no space left on device, write (writing <out>)`); an empty payload is refused. A runner killed mid-write can leave a `*.partial-<pid>-<n>` file; it never carries the output's name. The helper is `atomic-out.mjs` (`atomic_out.py` for the Python workers).
- **`--batch` exit codes (`comfy-generate.mjs`, `comfy-inpaint.mjs`).** `0` every job rendered; `4` the batch ran every job and at least one failed (the rows with `"ok":false` in the `--results` file name them, and the last log line gives the counts); `1` the batch could not run to the end: a setup error, a ComfyUI that became unusable (above), or a full disk (`ENOSPC`, `EDQUOT`, `EROFS`), where the failed job and every later job get a row (`not run: the disk is full at job N/M, writing <out> …`; the inpaint batch writes its `_row: "aborted"` line with `reason: "disk_full"`); `2` usage. `local-offload generate-image --batch` ends with the same codes. Until 0.178.0 a batch with failed jobs exited 0, so a caller had to grep the log for `RENDER FAILED`. `4` is not `3` on purpose: `3` is `comfy-render.mjs`'s *server unusable* code for its parent. The results file format did not change. The table, with the Go door, is in `docs/systems/media-generation.md`.
- **Every child process starts hidden.** Every `spawn`, `spawnSync`, `execFile`, `exec` and `fork` call in this directory - production modules, test support, tests, and the scripts a test writes into a string and runs - sets `windowsHide: true` in its options object (the last argument that is not a callback; a `const` object literal declared in scope that sets it counts, a spread or a computed key after it does not, and a child_process function used as a value is refused because its calls cannot be read). On Windows a console program started by a process with no console of its own (a detached runner, a service) otherwise opens a window of its own, a Windows Terminal window on the desktop. `no-visible-windows.test.mjs` fails with `file:line` on a call without it. A child started with `stdio: "inherit"` (the sd-cli, tts and comfy-render runs) stays on the operator's console whatever `windowsHide` says, so Ctrl-C still reaches it; `console-share.test.mjs` measures that and pins that those three runs inherit stdio.
- Standalone tool (not part of the Go binary) — generation is a different stack (PyTorch/diffusion) from the GGUF text/vision tiers, and is intentionally kept separate.

---

# Video — image-to-video b-roll (Phase 2)

Animate a still into a short b-roll clip with **free, local** video models via ComfyUI.
Same standalone, dependency-free pattern as the image tool, but single-GPU-locked and
zero-always-warm (it shares the 8 GB with llama-swap, so it must coordinate). Targets the
8 GB box (RTX 3070 Mobile + 64 GB RAM).

## Models (both on disk, no downloads)

| | model | use |
|---|---|---|
| **PRIMARY** | Wan 2.2 14B I2V (two-stage GGUF + DisTorch2 RAM-offload, 4-step lightx2v LoRAs, `wan_2.1_vae`) | default b-roll — best open 16GB photoreal I2V; ~1.5min/480p, ~2.7min/720p at 4 steps |
| SECONDARY | HunyuanVideo 1.5 480p I2V (cfg_distilled Q4_K_S) | `--model hunyuan`; opt-in, needs its files installed (absent on the 16GB box) |

## Files

- **`comfy-video.mjs`** — the runner. Acquires the GPU lock → frees llama-swap → ensures
  ComfyUI (on-demand, via its `.venv` python) → builds + POSTs the graph → polls `/history`
  → fetches the mp4 → frees ComfyUI VRAM → releases the lock.
- **`wf-hunyuan15-i2v.mjs`** / **`wf-wan22-i2v.mjs`** — pure API-format graph builders, wired
  against the **live** ComfyUI node schema (see Reconciliation).
- **`gpu-lock.mjs`** — the Node side of the machine-wide GPU lease. It does not acquire (`internal/gpulease`
  does); it honours and fences the lease a runner inherits, elects one unloader per lease, drains, and runs
  `freeLlamaSwap` / `freeComfy`. `freeLlamaSwap` unloads only what the lease may take: a lease on the whole node
  unloads every model off the memory stack; a lease on SOME cards unloads the list in `GPU_LEASE_UNLOAD_MODELS`
  (every model except the memory stack and the models positively placed on other cards, by `layers` or by the
  serving config's device pin; a model nothing places is on it; `-` = none) and leaves every other model resident. With
  no list (`GPU_LEASE_DEVICES` names a card lease) every placement is unknown, so it unloads every model off the memory
  stack, as before, and says so. A job that lost the once-per-lease election
  under a card lease with a list reads that list against `/running` again before it submits, so a seat that came back
  onto the leased cards leaves again (F15; `docs/systems/gpu-lease.md`, "Between renders").
- **`comfy-output.mjs`** — finds the produced file in `/history`. `VHS_VideoCombine` writes
  mp4 under the **`gifs`** key for all formats (a ComfyUI quirk).
- **`atomic-out.mjs`** — delivers the produced file: staged sibling, then rename (see the
  image tool's Notes above); `atomic_out.py` is the same for the Python workers.
- **`lease-progress.mjs`** — the helper's heartbeat to its lease's progress file (F8). `gpu reserve -- node
  render/comfy-*.mjs ...` (or a script that runs one, or `--progress-file auto`) declares a progress contract on the lease and hands the file to the helper in
  `GPU_LEASE_PROGRESS_FILE` (or the helper reads it from the inherited lease's record); `pollOutputs` appends a line
  at most every 30 s while ComfyUI answers and still lists the prompt, `comfy-generate --batch` between pictures, and
  `withGpuSlot` when the slot is ready. A long, healthy render's lease therefore renews instead of being labelled
  "expired ... open to a takeover"; a hung helper or a prompt the server lost stops the lines and the lease reads
  `held-stalled`. It appends (the file may be a log the caller keeps), never throws, and does nothing when no file is
  declared. It cannot see a sampler wedged inside a ComfyUI that still answers HTTP (no polled step counter): that
  stays bounded by `COMFY_WAIT_SEC`. See [GPU lease](../docs/systems/gpu-lease.md).
- **`lease-read.mjs`** — the one way `render/` reads a lease file the Go side owns (`readLease`, `readEpochRecord`,
  `readCardClaim` and so the fence `checkInheritedLease`, and the record reader of `lease-progress.mjs`). Go replaces
  those files by rename-over; on Windows an open that meets the instant of the rename fails with `EPERM`, so the read
  waits that out (`EPERM` and `EACCES` only, a 100 ms deadline, Windows only). A rename that succeeded can also leave the
  live name absent for tens of milliseconds with the staged file beside it, so a missing file is an answer only once it
  is settled: while a fresh staged file stands beside it the read waits (a 250 ms deadline, Windows only) and gives
  `null` if the wait runs out, and with none it reads once, so a released lease still reads absent at once. See
  [GPU lease](../docs/systems/gpu-lease.md).
- **`preflight-graph.mjs`** — validates a built graph against the live `/object_info` (all
  required inputs present) **before** spending a GPU cycle.

## Usage

```bash
node render/comfy-video.mjs <out.mp4> <still.png> "<prompt>" \
     --model hunyuan --frames 17 --width 480 --height 848 \
     [--steps 50] [--seed N] [--negative "..."] [--reserve-vram 2.0] [--no-lock] [--keep-comfy]
     # A kept instance (--keep-comfy) is freed when its family changes: the runner records the family it
     # loads on the instance's launch marker (comfy-family.mjs) and frees a kept instance that last ran
     # another one before its first job; the end-of-run /free is awaited and loud (COMFY-FREE-WARN).
node render/comfy-video.mjs out.mp4 still.png "<prompt>" --model wan --frames 49   # secondary
node render/comfy-video.mjs out.mp4 still.png "<prompt>" --wan-decode plain       # wan: tiled (default) | plain | auto
node render/comfy-video.mjs out.mp4 still.png "<prompt>" --high-unet <fp8 high> --low-unet <fp8 low> \
     --wan-decode plain --wan-placement multicard    # wan on the TWO non-display cards of a lease, no host RAM
     # run it under `local-offload gpu reserve --devices <a>,<b> --class media --ram 0 -- ...`, flags spelled `--flag value` (not `--flag=value`);
     # [--api http://127.0.0.1:<own port>] [--wan-resident-gb 3 (3.2 at most at film size; a clip bigger than the film's was not measured, and the planner says so: WAN-GEOMETRY-NOTE; with --use-sage-attention in COMFY_EXTRA_ARGS the film-size ceiling is 0.2, so pass 0)] [--wan-cards <a>,<b>, outside a lease]
     # [under COMFY_EXTRA_ARGS=--use-sage-attention the lease sizing holds 15.2 GiB of activations for any clip: --ram 0 states the real figure for a smaller one; export the variable around
     #  `gpu reserve`: an `env COMFY_EXTRA_ARGS=...` prefix inside the wrapped command is not seen by the lease sizing, only by the runner]
     # [env WAN_COMPUTE_UUID=<index|uuid|uuid prefix of 8+>: which of the two cards computes; without it the larger card does, and of two cards within 1% of one size the higher board power (power.default_limit, at least 1.25x) does]
     # [--wait-sec N: 7200 by default under multicard, the 20-step film recipe measured 5136 s of runner wall]; the run watches ComfyUI's console
     # (WAN-OOM ends the wait when its prompt worker died of an out-of-memory error); a plain decode that ComfyUI retried tiled fails
     # the run (WAN-DECODE-FALLBACK, the clip kept as <out>.seamed.<ext>); docs/systems/media-generation.md, "Card-only Wan"
node render/preflight-graph.mjs hunyuan   # validate a graph vs a running ComfyUI, no gen
```

## 8 GB hard-won settings (don't regress)

- **cfg_distilled needs steps=50, NOT 12** — distillation removes the CFG/negative pass, not
  the step budget; 12 steps = garbage. CFG stays 1.
- **VAE decode is the OOM cliff** — `temporal_size: 4096` (decode-all-at-once) HARD-CRASHED the
  display driver at 33 frames. `vaeTemporalSize: 16` chunks the decode temporally and fits;
  raise toward 4096 only on bigger GPUs (fewer motion seams).
- **Wan 2.2's decode is a mode, `--wan-decode tiled|plain|auto`** (config `videogen_wan_decode`). `tiled` (the
  default) is `VAEDecodeTiled`, the node the Wan graph always used; `plain` is `VAEDecode`, 38 s at a 10.3 GB peak
  against 412 s at 3.2 GB on a 16 GB card (A/B 2026-10-03, with ComfyUI dynamic VRAM on; that A/B's tiled arm was one
  chunk, and the clip's shape is unrecorded). `auto` reads the render card's total VRAM from `GET /system_stats` and
  runs plain from 12 GiB, else tiled, and tiled when the card cannot be read; it logs its choice on stderr. `plain`
  and `auto` are an explicit opt-in, because no render at the 16 GB tiers' own shape (1280x720x81) was ever run with
  plain: ComfyUI's estimate for the plain decode follows the frame's resolution (12.0 GiB at 1280x720, on a card that
  reports 15.9 GiB), it retries an out-of-memory plain decode tiled only once, and a second out-of-memory fails a
  the whole render after sampling. The default flips to `auto` only after a live acceptance render at that shape
  shows a decode close to the measured 38 s and no `Ran out of memory when regular VAE decoding` line in the ComfyUI log. Hunyuan 1.5 and
  LTX 2.5 are not touched by it. Detail: `docs/systems/media-generation.md`.
- **Wan 2.2 on two cards and no host RAM, `--wan-placement multicard`** (default `single`, today's graph). The text encoder and VAE
  load on `cuda:0`, each fp8 safetensors or Q8_0 GGUF expert (the GGUF flavour of the DisTorch2 loader, no `weight_dtype`, donor = the storage card, never host RAM) with compute on `cuda:1` and `cuda:1,3gb;cuda:0,*`; the two cards are the lease's
  (`--wan-cards` only names them when no lease does), never a display card or one whose display state is unknown; the larger card computes (`cuda:1`), except that two cards within 1% of one size are ranked by board power
  (`power.default_limit`, read by a second best-effort query; the higher one computes from 1.25x, a power the driver does not report ranks nothing), and the environment variable `WAN_COMPUTE_UUID`
  (an index, a UUID or a UUID prefix of 8+ characters) names the compute card of the pair (also for the image and edit placements below; a token that names no card is refused, one outside the pair is ignored with a note);
  the plan's line contains `compute card chosen: <the rule>`, followed by the sage clause under sage attention. The runner launches its own ComfyUI on them (`--gpu-only`, a read-only safetensors
  reader and a free-then-plain-decode node from `render/comfyui-ext`, through `--extra-model-paths-config`, nothing installed into the
  shared ComfyUI). Under `--wan-decode plain` the experts are freed from both cards before the plain `VAEDecode`, and the sampled latent
  is saved first. `--wan-loader` is not read, and an unnamed expert (the builder's defaults are the Q4_K GGUF files) and `--fast` are refused before anything starts (not measured); a `.gguf` expert also loads ComfyUI-GGUF in the launch, and the typed `video` door reaches the placement from config (`videogen_placement: multicard`, `videogen_wan_resident_gb`). A driver script that wraps the helper is not a recognised render helper, so its lease declares the class default unless its `gpu reserve` passes `--ram 0` (the verb says so on a second line). Detail and numbers:
  `docs/systems/media-generation.md`.
- **LTX-2.5 on two cards and no host RAM, `--wan-placement multicard --model ltx25`** (`render/wf-ltx25-i2v.mjs`, `wan-multicard.mjs`; no `--wan-decode`, `--high-unet` or `--low-unet`).
  The same placement as Wan with the roles' device numbers reversed: the launch lists the COMPUTE card first, so **cuda:0 is the compute card and cuda:1 the storage card**,
  because an int8 DiT that computes on any other device faults (ComfyUI-MultiGPU #220); its instance key is `mcc-<compute head>-<storage head>`, distinct from the Wan key of the same pair.
  The DiT (`UNETLoaderDisTorch2MultiGPU`, `cuda:0,<R>gb;cuda:1,*`, `virtual_vram_gb` 0, `donor_device` `cpu`) keeps `--wan-resident-gb` GiB (default 9, chosen) on the compute card; the Gemma encoder
  (`OffloadCLIPLoaderDisTorch2`, `ltxv`, split `cuda:0,7gb;cuda:1,*`) is released from both cards by `OffloadFreeAfterEncode` after the prompt is encoded and before the first sampler places the DiT
  (`OFFLOAD-TE-STILL-RESIDENT` if any weight stays); both VAEs sit on cuda:1. A bf16 or GGUF `--transformer` and `--pool-vvram-gb` are refused. No LTX-2.5 run was made on the two cards: the shares are chosen,
  and the sampler's activations are unmeasured (`LTX-PLACEMENT-NOTE`). Details: `docs/systems/media-generation.md`, "LTX-2.5 on two cards".
- **Qwen-Image on two cards, `--placement multicard`** (`comfy-generate.mjs`, `comfy-render.mjs`, `comfy-edit.mjs`; default `single`, today's graph, byte for byte). The text encoder and the VAE
  load on one leased non-display card and the DiT on the other, or split between them, through the MultiGPU loaders, with the
  recipe of `render/multicard-recipes.json` (the 2512 Q5_1 GGUF, the krea2 and 2.1 bf16 DiTs and the 2511 edit fp8mixed and Q5_1; the 2.1 int8 convrot row is held back, and 2512 bf16 and sdxl
  have none: all refused by name). The two cards are the lease's (`--cards` only names them when no lease does), never a display card, and the runner starts its
  own keyed ComfyUI on them, the one the Wan placement uses. Run it under `local-offload gpu reserve --devices <a>,<b> -- node render/comfy-edit.mjs
  out.png in.png "<instruction>" --unet <fp8mixed file> --placement multicard`, flags spelled `--flag value`; `[--resident-gb N]` moves the DiT's share on the
  compute card (the DiT's total is the invariant: a smaller share puts the same bytes on the storage card, and a split that card cannot hold is refused). The 2511
  fp8mixed edit, the 2512 Q5_1 GGUF and the krea2 bf16 DiT have *measured* rows, so they declare 0 host RAM (and `gpu reserve` says so); the others declare the ordinary estimate until a run with the
  read-only loader promotes them. An unquantized DiT (krea2 bf16) is built empty and filled from the read-only map (the pack's lazy build), so it never lands whole on one card before the split. The 2.1 int8 convrot row is held back (its DiT on `cuda:1` never ran) and refused by name. The 2.1 bf16 row's 16.33 GiB text encoder is bigger than a card, and under `--gpu-only` ComfyUI would build the whole of it on the storage card before a DisTorch2 split applies, so that row loads it through the harness's own `OffloadCLIPLoaderDisTorch2` (`render/comfyui-ext`: built off the cards, then placed block by block, 8 GiB on the compute card; host RAM only transiently while it loads, and the node refuses if a weight is left there) and is held to a 2048x2048 canvas (chosen, not measured); an edit canvas pinned above the recipe's
  2.0 MP cap is refused before anything starts. The 2512 Q5_1 GGUF row is held to a canvas of at most 1,769,472 pixels (1536x1152, `maxPixels`; measured at 1328x1328 and 928x1664) and the
  krea2 bf16 row to 1,572,864 (1536x1024, measured); a larger canvas is refused by the typed door before any lease and by the runner
  (`canvasRefusal`) before ComfyUI starts: name a smaller `--width` and `--height`, or use `--placement single`. A call under `gpu reserve --devices <a>,<b>` with **no placement** at an endpoint that
  nothing answers on is refused up front too (`COMFY-PLACEMENT-MISSING`): the message names the two leased cards and `--placement multicard`, the flag that keys the pair's own instance. The runner checks the recipe's loader classes and their inputs against `/object_info` before it submits, watches the pair's
  ComfyUI console while it waits (`IMAGE-PLACEMENT-OOM` ends a wait on a worker that died of an out-of-memory error), and keeps an image whose VAE decode ComfyUI retried tiled
  beside the requested path as `<name>.tiled.<ext>` (`IMAGE-DECODE-FALLBACK`). `docs/systems/media-generation.md`, "Card-only image and edit".
- **A kept (`--keep-comfy`) or warm (`--batch`) ComfyUI frees its own models and caches after 300 s idle.** The runner launches it with
  the pack in `render/comfyui-ext/idle-nodes` (through `--extra-model-paths-config render/comfyui-ext/extra_model_paths_idle.yaml` and the
  custom-node whitelist): after `COMFY_IDLE_FREE_SEC` seconds (default 300; `0` or `off` disables it; a whole number from 5 up) with nothing
  queued or running, the instance asks its own worker to unload its models and reset its executor cache, once per busy period. A running
  prompt is never idle. ComfyUI already unloads every model when a prompt ends under the harness's `--disable-smart-memory`, so a kept
  `--cache-none` instance is bare between jobs and the pack finds nothing to do there; it bounds a warm session's executor cache (host
  RAM) and a hand-started instance that keeps its models. The harness runs no watcher; the Python process and its CUDA context are not
  freed by it and live until the lease holder stops the instance. A running instance without the pack is reused with one
  `COMFY-IDLE-WARN` per runner start; to bound a hand-started ComfyUI add the yaml to its own `--extra-model-paths-config`
  (`docs/OPERATOR-GUIDE.md`). The default single render is launched exactly as before. Not yet run in a live ComfyUI; detail and the live
  check: `docs/systems/media-generation.md`.
- **`--reserve-vram 2.0`** keeps headroom for the Windows display/WDDM (too low → a decode spike
  kills the whole process with no traceback).
- **Qwen2.5-VL fp8 text encoder CPU-offloads automatically** (~free with 64 GB RAM, saves 4–6 GB).
  Don't use `--lowvram` with the GGUF unet. Start at 17 frames, scale up once a clean gen lands.

## Reconciliation (why the builder matches reality)

The Hunyuan builder was reconciled against the **live** 1.5 schema + the official template
(workflow `wso07xgs5`, 2026-06-16): `DualCLIPLoader` type `hunyuan_video_15` (not
`hunyuan_video`); plain `CLIPTextEncode` for the prompt (not the legacy
`TextEncodeHunyuanVideo_ImageToVideo`); required `batch_size` on the I2V node; required
`pingpong` on `VHS_VideoCombine`; `ModelSamplingSD3 shift 5`. `preflight-graph.mjs` guards
against future schema drift.

---

# Music — text-to-music (ACE-Step)

Generate royalty-free instrumental beds for reels with **free, local** ACE-Step v1 3.5B —
native in ComfyUI 0.23.0 (no custom nodes), **Apache-2.0 = commercial-safe**. Reuses the same
runner + GPU lock as video.

- **`wf-acestep.mjs`** — graph builder (`CheckpointLoaderSimple → ModelSamplingSD3 →
  TextEncodeAceStepAudio ×2 → EmptyAceStepLatentAudio → KSampler → VAEDecodeAudio → SaveAudio`).
  Instrumental beds: tags-only prompt, empty lyrics.
- Driven via `comfy-video.mjs --model ace` (text-only — no still). Output FLAC under
  ComfyUI's `audio` history key (handled by `comfy-output.mjs`).

```bash
node render/comfy-video.mjs out.flac "upbeat corporate, light electronic, 120 bpm" --model ace --seconds 20
node render/preflight-graph.mjs ace   # validate the graph vs live /object_info
```

Needs `ace_step_v1_3.5b.safetensors` (7.7 GB, the all-in-one bundle: DiT + CLIP + audio VAE) in
`models/checkpoints/`. Download via curl direct-URL from `Comfy-Org/ACE-Step_ComfyUI_repackaged`
(the `huggingface-cli`/`hf` CLIs failed in this env).

**VERIFIED 2026-06-16:** tags → a 19.97 s stereo 44.1 kHz FLAC of real audio (mean −18 dB,
peak −4 dB), checkpoint loaded on 8 GB (RAM-offloaded), GPU freed cleanly after.

---

# Voice — narration / TTS (Chatterbox Multilingual, voice-cloning)

Generate Spanish (or 23-language) voiceover that **clones a reference voice** — narrate in a
reference speaker's voice from a ~10 s sample. **Chatterbox Multilingual V3
(MIT license = commercial-safe)**, run standalone in a python venv (core ComfyUI has no TTS),
GPU-locked. *(F5-Spanish was rejected despite better Spanish training — its model license is
contradictory CC-BY-NC/CC0, unsafe for a monetized channel. Same call as music: Apache/MIT only.)*

- **`tts.mjs`** — dep-free Node CLI: checks the command line → acquires the GPU lock → frees llama-swap →
  runs the worker.
- **`tts_chatterbox.py`** — worker in `.tts-venv` (`ChatterboxMultilingualTTS.from_pretrained →
  generate(text, language_id, audio_prompt_path=ref, <the sampling flags given>)`). Outputs 24 kHz WAV;
  inaudible Perth watermark (provenance only).

```bash
node render/tts.mjs out.wav "Hola, bienvenidos a mi canal." --clone ref.wav --lang es
node render/tts.mjs out.wav "Hola, bienvenidos a mi canal." --clone ref.wav --cfg-weight 0.3 --exaggeration 0.6
```

`tts.mjs <out.wav> "<text>" [flags]` — both positionals are required and the text is ONE argument (quote it):

| Flag | Meaning | Engine |
| --- | --- | --- |
| `--clone <ref.wav>` | reference voice to clone (else the `TTS_REF` environment variable) | both |
| `--lang es` | language id, default `es` | both |
| `--engine generalist\|finetuned` | which worker; default `generalist` (stock Chatterbox multilingual) | |
| `--model <merged.safetensors> --base-dir <dir>` | the fine-tuned engine's weights and base directory | `finetuned` only, and both are required with it |
| `--temperature F --cfg-weight F --exaggeration F --repetition-penalty F --min-p F --top-p F` | sampling knobs; the generalist worker passes the ones given to `generate()` as `temperature`, `cfg_weight`, `exaggeration`, `repetition_penalty`, `min_p`, `top_p` | both workers take them under the same names; only the generalist runs `generate()` today (the fine-tuned worker is a skeleton that validates and defers with exit 3 until that engine is vendored) |
| `--no-lock` | run without the GPU lease (standalone use only) | |

A sampling flag that is not given is not passed, so the model keeps its own default (a given `0` is passed). **The
command line is checked before the lock is taken or a worker starts (0.185.1), and a refusal exits 2 with the reason
and the usage line:** an unknown or misspelled flag (`--cfg_weight`; the `--flag=value` spelling is not read, write
`--flag value`), a flag with no value, a sampling value that is not a finite decimal number (`abc`, `nan`, `inf`, `1e999`,
`0x10`), an `--engine` other than the two, `--model`/`--base-dir` without `--engine finetuned`, `--engine finetuned` without
both of them (the fine-tuned worker requires both), and a third positional (an unquoted text). A token is a flag only when it
is shaped like one (`--` and a name), so a text that opens with dashes, `"--- Intro ---"`, `---` or `-x`, is the text (the case
`generate-audio out.wav -- "--- Intro ---"` keeps intact); a bare `--` ends the flags, for a text that is shaped like one
(`tts.mjs out.wav -- "--hola"`). Every value reaches the worker as one `--flag=value` token (`--text=---`,
`--temperature=-1e-2`), so a text, a path or a negative number that opens with a dash is never read as an option by the
worker's argument parser (as a separate token argparse reads `---` and `-x` as options on every Python, and `-1e-2` on 3.11,
and the worker then exits 2 after the lock was taken). Before 0.185.1 the sampling flags reached the fine-tuned worker only, so on the default engine
`--cfg-weight 0.3` changed nothing, and an unknown flag was dropped without a word. The harness's own voice lane
(`internal/pipeline/pipeline.go`) sends `--clone --lang` (generalist) or `--engine --model --base-dir --clone --lang` plus the
configured `voicegen_ft_*` recipe (finetuned); `render/tts.test.mjs` reads `pipeline.go` and fails if tts.mjs would refuse
any flag the Go side sends.

**Setup** (one-time): build `.tts-venv` at the repo root on a python torch supports (3.11–3.12;
system 3.14 is too new — `uv venv .tts-venv --python 3.11` works; note a uv venv ships no pip,
so install with `uv pip install --python .tts-venv/Scripts/python.exe …` throughout).
`chatterbox-tts` pulls CPU torch, so force the CUDA build matched to the GPU: pre-Blackwell
cards take `torch==2.6.0 torchaudio==2.6.0 --index-url .../cu124`; Blackwell (RTX 50xx, sm_120)
needs `torch==2.7.0 torchaudio==2.7.0 --index-url .../cu128` — cu124 ships no sm_120 kernels.
One trap: setuptools must be present AND <81 (`setuptools==80.9.0`) — perth, the watermarker
dep, imports `pkg_resources` (removed in setuptools 81+, absent from uv venvs), and without it
`perth.PerthImplicitWatermarker` silently resolves to `None` → `TypeError: 'NoneType' object
is not callable` at model init. The worker sets `HF_HUB_DISABLE_SYMLINKS=1` (Windows blocks
symlinks without Developer Mode).

**VERIFIED 2026-06-16:** a Spanish line cloned from a reference voice clip → a 6.4 s
24 kHz WAV; **whisper round-trip transcribes it back word-for-word** (intelligible Spanish),
GPU freed after. Clone *timbre* quality is best judged by ear — a clean ~10 s solo voice
reference (vs. a noisy stereo mix) will improve it.

**VERIFIED 2026-07-22 (<node-b>, RTX 5060 Ti / Blackwell):** the setup above (uv 3.11 venv,
torch 2.7.0+cu128, setuptools 80.9.0) → `local-offload generate-audio` end-to-end: a Spanish
line → a real 4.0 s 24 kHz mono WAV, GPU lock acquired and released.

---

## Verification status (video)

**VERIFIED 2026-06-16** end-to-end on the 8 GB box. A real source still (a sports-car hero frame
from a sample reel) → coherent **480×848 24 fps h264** clips at both **17 frames**
(0.7 s) and **49 frames** (2.0 s, usable b-roll): a smooth cinematic dolly push-in, the car
photorealistic and intact across all frames (no warping/melting/drift even over 2 s), GPU freed
cleanly after (`freeComfy`) and the llama-swap memory stack unaffected (embedder still 768).
The runner's on-demand ComfyUI auto-spawn (`.venv` python) is verified too. Builders + GPU
scheduler unit-tested (green). Settled config: cfg_distilled @ 50 steps / CFG 1 / shift 5,
`vaeTemporalSize 16`, `--reserve-vram 2.0`. 49 frames is the realistic ceiling on 8 GB.

---

## Upscale (ESRGAN-family, `comfy-upscale.mjs`)

```bash
node render/comfy-upscale.mjs out.png in.png --model 4x-UltraSharp.pth            # native factor (4x)
node render/comfy-upscale.mjs out.png in.png --model 4x-UltraSharp.pth --scale 2  # 4x, then 0.5 lanczos
node render/comfy-upscale.mjs out.png in.png --model 4x-UltraSharp.pth --width 3000 --height 2000 --method bicubic
```
Core nodes only (`UpscaleModelLoader` → `ImageUpscaleWithModel` → optional `ImageScaleBy`/`ImageScale`;
graph in `wf-upscale.mjs`, unit-tested). `--model` is a filename under ComfyUI's `upscale_models/`
(or `COMFY_UPSCALE_MODEL`); the native factor is read from it (`4x-…` → 4, unknown → 4). A half-given
size or a bad `--method` exits 2 **before** the GPU slot is taken. Same lifecycle as the other runners:
single-slot GPU lock, on-demand ComfyUI, zero-always-warm teardown. The harness route is
`local-offload upscale-image` / `offload_upscale_image` (binding `upscale_model`, falling back to
`videogen_upscale_model`).

---

# Composition — HTML/CSS motion graphics to video (HyperFrames, `compose-hyperframes.mjs`)

```bash
node render/compose-hyperframes.mjs render --hyperframes-dir <dir> --browser <chrome-headless-shell> \
  --ffmpeg <ffmpeg> --template title-card --out card.mp4
node render/compose-hyperframes.mjs render ... --template lower-third --format webm \
  --variables-file vars.json --snapshots 1,2.5 --out lt.webm
node render/captions-groups.mjs clip.segments.json --out chunks.json   # words_json for captions-bar
node render/compose-hyperframes.mjs browser --hyperframes-dir <dir>   # pinned Chrome (installer step)
node render/compose-hyperframes.mjs version --hyperframes-dir <dir>   # acceptance check
```

This is the only door to the pinned HyperFrames CLI (npm `hyperframes`, installed from the
lockfile in `setup/hyperframes/`). It is **CPU-class**: software GL and CPU encode, with no
`withGpuSlot` and no GPU lease (ADR 0059).

Every call goes through the same guards:

- **Allowlisted child env.** No cloud key, `NODE_OPTIONS` or `GPU_LEASE_*` reaches the CLI.
- **Telemetry, updates and skills off.**
- **`--json` on every invocation.** It is what skips the CLI's npm and GitHub update pings.
- **Subcommand allowlist.** Only `lint`, `check`, `render`, `snapshot`, `browser ensure|path` and
  `--version` run.
- **Fresh empty work dir as cwd**, so no `.env` is ever loaded.
- **Harness-owned HOME** at `<hyperframes_dir>/home`.

The pipeline is `lint` → `check` → `render --batch` (one row) → an ffprobe gate → optional
`snapshot`. Retry covers only `spawn EBUSY`, and only once. Each failure prints one typed line,
`COMPOSE-FAIL: <CLASS>: <detail>`, then a final JSON result that is also written to `--result`.
Templates, and the contract each one keeps, are in
[`compose-templates/`](compose-templates/README.md). The harness routes are
`offload_compose_video`, `local-offload compose-video` and the fleet task `compose-video`. See
[`docs/systems/media-generation.md`](../docs/systems/media-generation.md#composition-hyperframes).

---

# Workflow templates catalog (read-only, `templates-catalog.mjs`)

Lists and classifies the ComfyUI workflow templates a node already carries: the
`comfyui-workflow-templates` package ComfyUI pins, a checkout of the upstream repository, or a
`pip download` extract of a candidate version. Dependency-free (`node:` builtins), read-only, no
network. It never runs a template, downloads a model, installs a package or changes a config; the
only file it writes is the one named by `--out`. Every count names the package it was computed on.

```bash
node render/templates-catalog.mjs summary   --comfy-dir <comfyui-tree>          # counts, each with its basis
node render/templates-catalog.mjs list      --comfy-dir <comfyui-tree> --tag "Text to Image"
node render/templates-catalog.mjs snapshot  --comfy-dir <comfyui-tree> --label nodeA --out nodeA.json
node render/templates-catalog.mjs readiness --comfy-dir <comfyui-tree> --snapshot nodeA=nodeA.json --mode both
node render/templates-catalog.mjs diff      --comfy-dir <comfyui-tree> --snapshot nodeA=nodeA.json \
    --candidate-dir <extract>/comfyui_workflow_templates_json/templates
```

`snapshot` reads one node's package versions, API node ids, model directories (default,
`extra_model_paths.yaml` and the output directory ComfyUI registers) and model files as JSON; run it on each
node, then compare on one machine. The whole file can be piped to `node --input-type=module - snapshot
--comfy-dir <tree>` on a node with nothing deployed (a piped file has no licence map beside it, so its other
verbs need `--license-map FILE`). `readiness` is directory-aware: a file counts only if ComfyUI would offer it
to the loader that reads it. `list` hides paid API templates and FLUX-family ones unless `--include-hidden`
(naming `--kind api` lists the paid ones without it). The licence map
is `render/templates-license-map.json`. Rules, fields and gaps:
[`docs/systems/media-generation.md`](../docs/systems/media-generation.md#comfy-workflow-templates-catalog-phase-a).
Tests: `node --test render/templates-catalog.test.mjs`.

---

# Template-to-API converter (`template-to-graph.mjs`)

Converts a ComfyUI UI-format workflow template (`nodes[]`, `links[]`, `definitions.subgraphs`, `widgets_values`) into the
API-format prompt graph (`{"<id>": {"class_type", "inputs"}}`) that `POST /prompt` and the builders in this directory
use. A pure function of the template, an `/object_info` listing the caller supplies and options: no ComfyUI, no GPU, no
network, no file written. Dependency-free (`node:` builtins). Clean room: the GPL-3.0 frontend and ComfyUI's server code
were not read as source.

```bash
node render/template-to-graph.mjs --template <template.json> --object-info <object_info.json> \
    [--comfy-version X.Y.Z] [--seed N] [--min-comfyui-version X.Y.Z | --index <index.json> [--name NAME]] [--meta]
```

It prints the graph (`{"ok":true,"graph":{...},"warnings":[...],"warnings_dropped":0,"pinnedSeeds":[...],"stats":{...}}`, exit 0) or a typed
refusal (`{"ok":false,"refusal":{"code","message","details"}}`, exit 1; exit 2 for a call that cannot be read). It drops
muted nodes, passes bypassed nodes through by type (positionally, and refused as `ambiguous_bypass` when the counts differ), expands subgraphs (ids `<instance>:<inner>`) with their input and output
rewiring, resolves `Reroute` and `PrimitiveNode`, skips `Note` and `MarkdownNote`, maps `widgets_values` to named inputs in
the listing's input order (the hidden `control_after_generate` slot, upload slots, dotted dynamic-combo and autogrow names)
and pins seeds with `--seed`. A template whose minimum ComfyUI version exceeds `--comfy-version` is refused
(`comfyui_too_old`); a known minimum with no version given is a refusal too, because the version is never assumed, and a version given with no minimum to compare it with is a `version_check_skipped` warning. An
unknown node class, a widget layout that does not add up and an input type with no rule are refusals as well, never a
guess. It is meant for trusted template packages: its caps bound nodes, wires, hops, nesting depth, warnings and id length, not
the work proportional to the size of a value (see the Known gaps of the system page linked below). Rules, refusal codes, evidence and the open frontend-golden comparison:
[`docs/systems/media-generation.md`](../docs/systems/media-generation.md#template-to-api-converter-rendertemplate-to-graphmjs).
Tests: `node --test render/template-to-graph.test.mjs`; fixtures in
[`testdata/template-goldens/`](testdata/template-goldens/README.md).

---

# Template manifests and the run-template command line (`template-manifest.mjs`, `run-template.mjs`, `template-run.mjs`)

The node-side half of the `offload_run_template` tool. Read-only over a ComfyUI tree: no lease, no ComfyUI, no network, nothing
written, one compact JSON object on stdout, the same bytes for the same tree.

```bash
node render/run-template.mjs list      --comfy-dir <comfyui-tree> [--kind K] [--tag T] [--text WORDS] [--license-class C] \
    [--runnable true|false] [--reads-input true|false] [--limit N] [--offset N]
node render/run-template.mjs get       <name> --comfy-dir <comfyui-tree>
node render/run-template.mjs readiness <name> --comfy-dir <comfyui-tree> [--snapshot node.json]
node render/run-template.mjs resolve   <name> --comfy-dir <comfyui-tree> [--snapshot node.json]
```

`template-manifest.mjs` turns a catalog row into a manifest entry (models as `models/<class>/<name>` with the source the
template names and the hash it declares, node packs, licence class, API or not, input files it reads) and decides what the
first release of the tool will run: not a paid API template (a Defer, `api_paid`), not FLUX-family, no custom node pack, readable,
and a permissive licence class (or none); every other licence, `unknown` and an inferred map entry included, is refused under
its own typed class. `resolve` is the gate in front of a run: that verdict, then what the node lacks (`models_missing`,
`inputs_missing`, `comfyui_too_old`, each with the exact list; a prompt embedding it does not verify is `embedding_unverified`, with the
reason and the files), then the runtime manifest, which names every model by the path
it was found at and carries no `source_url`, so a run never downloads. `template-run.mjs` is the run side: it finds a template by
name in the node's own package, converts it with the converter above and the running ComfyUI's listing, and applies the
overrides. The run-graph runner takes it as `comfy-run-graph.mjs --template NAME [--overrides FILE] [--seed N]`: the template is
converted after ComfyUI is up and before preflight (`TEMPLATE_CONVERT_REFUSED` on a refusal, `TEMPLATE_MODEL_UNLISTED` when the converted graph
loads a model file the manifest does not name, an embedding the prompt text names included). Trust on first use for models with
no declared hash is `verifyTofuModels` in `manifest-satisfy.mjs` (a record `<file>.tofu` beside the file). Rules, classes, the
measured catalog and what is not built:
[`docs/systems/mcp-server.md`](../docs/systems/mcp-server.md#workflow-templates-offload_run_template).
Tests: `node --test render/template-manifest.test.mjs render/run-template.test.mjs render/template-run.test.mjs`; the fixture tree is
`template-manifest-fixture.mjs` (in memory, every repository `example-org/...`).

---

# Builder drift guard (`builder-drift.mjs`)

A `wf-*.mjs` builder that was copied from a ComfyUI workflow template carries the template's recipe widgets
(steps, cfg, sampler, scheduler, shift, LoRA strength, ...) as literals. `testdata/builder-drift.json` records,
per builder and view, the template name, the `comfyui-workflow-templates-json` version and sha256 hashes of those
widgets as the template and as the builder's output graph carry them (names and hashes only; no template content).

```bash
node --test render/builder-drift.test.mjs                       # builder side, offline: one test per builder
COMFYUI_DIR=<comfyui-tree> node --test render/builder-drift.test.mjs   # plus the template side against the installed package
node render/builder-drift.mjs check   --templates-dir <extract>/comfyui_workflow_templates_json/templates   # a candidate package
node render/builder-drift.mjs explain --comfy-dir <comfyui-tree>   # template and builder values side by side
node render/builder-drift.mjs record  --comfy-dir <comfyui-tree>   # re-pin, after comparing the builder with the template
node render/builder-drift.mjs check --comfy-dir <comfyui-tree> --require-templates --strict   # upgrade runbook
```

`--templates-dir` (and `COMFY_TEMPLATES_DIR`) takes the package's `templates/` folder, not the package root above it: a
directory that holds a `templates/` folder and no top-level `.json` file is refused with exit 2 and the path to pass.

The CLI fails closed. A flag that takes a value and gets none (an empty or unset shell variable, also `--flag=`), an
argument that starts with `-` and is not a known flag (single-dash forms such as `-templates-dir` included), a value given
to a switch (`--strict=1`) and a second positional after the verb all exit 2. A directory named only through
`COMFY_TEMPLATES_DIR` / `COMFYUI_DIR` that is unset still leaves the template side skipped; `--require-templates` makes
that an error (exit 2) instead of a skipped side.
`--strict` also fails (exit 1) on every `unreviewed` manifest entry; a plain `check` leaves them green. Both belong to
`check`. **The runbook command above (`--require-templates --strict`) exits 1 on the committed manifest by design:**
exactly `2 failure(s)`, for `wf-qwen-image-edit [full] ksampler.cfg` and `wf-qwen-image-21 [edit] encoder.resolution`,
the two `unreviewed` entries. Those are the expected failures; any other line is a new finding. Drop `--strict` for a
green gate.

A moved widget fails `builder drift: <builder> [<view>]: ...` naming the builder. `builder-drift-specs.mjs` lists
the widgets per builder; a new `wf-*.mjs` file must be guarded there or listed in `NOT_TEMPLATE_DERIVED` with a
reason. A locator never reads a widget whose input is linked (its serialized value is stale): it throws and the spec
points at the source that feeds it. The templates directory is resolved in one order, by the CLI and the tests alike:
`--templates-dir`, `--comfy-dir`, `COMFY_TEMPLATES_DIR`, `COMFYUI_DIR`. Rules, statuses and the first run's findings:
[`docs/systems/media-generation.md`](../docs/systems/media-generation.md#builder-drift-guard).

What a green run does not tell you:

- Node mode is never read: a bypassed or muted node or instance still reads as live.
- Wiring is mostly unguarded: one `D` item (Wan Animate 2 pose percents); the `wf-wan22-i2v` high and low pairs share
  one template locator each (steps, cfg and the step split, `high.end_at_step` / `low.start_at_step` both reading node
  127). A `feeds` locator would cover it; it is not built.
- `W` and `I` do not consult `proxyWidgets`; only `P` does.
- The two `unreviewed` entries stay green in a plain `check`; `check --strict` fails them. One is probably a real
  builder defect (`wf-qwen-image-edit` full preset: CFG 3.0 against the template's effective 4), an open item recorded in
  the system page.
