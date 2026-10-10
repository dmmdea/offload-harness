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
- **`gpu-lock.mjs`** — cross-process single-slot GPU mutex (mkdir-lockdir + PID/TTL stale
  reclaim) + `freeLlamaSwap` / `freeComfy`.
- **`comfy-output.mjs`** — finds the produced file in `/history`. `VHS_VideoCombine` writes
  mp4 under the **`gifs`** key for all formats (a ComfyUI quirk).
- **`atomic-out.mjs`** — delivers the produced file: staged sibling, then rename (see the
  image tool's Notes above); `atomic_out.py` is the same for the Python workers.
- **`preflight-graph.mjs`** — validates a built graph against the live `/object_info` (all
  required inputs present) **before** spending a GPU cycle.

## Usage

```bash
node render/comfy-video.mjs <out.mp4> <still.png> "<prompt>" \
     --model hunyuan --frames 17 --width 480 --height 848 \
     [--steps 50] [--seed N] [--negative "..."] [--reserve-vram 2.0] [--no-lock] [--keep-comfy]
node render/comfy-video.mjs out.mp4 still.png "<prompt>" --model wan --frames 49   # secondary
node render/comfy-video.mjs out.mp4 still.png "<prompt>" --wan-decode plain       # wan: auto (default) | plain | tiled
node render/preflight-graph.mjs hunyuan   # validate a graph vs a running ComfyUI, no gen
```

## 8 GB hard-won settings (don't regress)

- **cfg_distilled needs steps=50, NOT 12** — distillation removes the CFG/negative pass, not
  the step budget; 12 steps = garbage. CFG stays 1.
- **VAE decode is the OOM cliff** — `temporal_size: 4096` (decode-all-at-once) HARD-CRASHED the
  display driver at 33 frames. `vaeTemporalSize: 16` chunks the decode temporally and fits;
  raise toward 4096 only on bigger GPUs (fewer motion seams).
- **Wan 2.2's decode is a mode, `--wan-decode auto|plain|tiled`** (config `videogen_wan_decode`). `tiled` is
  `VAEDecodeTiled`, the node the Wan graph always used and the one a card under 12 GiB keeps; `plain` is `VAEDecode`,
  38 s at a 10.3 GB peak against 412 s at 3.2 GB on a 16 GB card (A/B 2026-10-03; that A/B's tiled arm was one chunk,
  and the clip's shape is unrecorded). `auto` (the default) reads the render card's total VRAM from
  `GET /system_stats` and runs plain from 12 GiB, else tiled, and tiled when the card cannot be read; it logs its
  choice on stderr. ComfyUI retries an out-of-memory plain decode tiled once, a second chance that can run out of
  memory too; its estimate for the plain decode follows the frame's resolution (12.0 GiB at 1280x720, the 16 GB tiers'
  own shape, on a card that reports 15.9 GiB), and no render at that shape has been run for this change. Hunyuan 1.5
  and LTX 2.5 are not touched by it. Detail: `docs/systems/media-generation.md`.
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

- **`tts.mjs`** — dep-free Node CLI: acquires the GPU lock → frees llama-swap → runs the worker.
- **`tts_chatterbox.py`** — worker in `.tts-venv` (`ChatterboxMultilingualTTS.from_pretrained →
  generate(text, language_id, audio_prompt_path=ref)`). Outputs 24 kHz WAV; inaudible Perth
  watermark (provenance only).

```bash
node render/tts.mjs out.wav "Hola, bienvenidos a mi canal." --clone ref.wav --lang es
```

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
