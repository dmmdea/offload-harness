# render — local image generation (ComfyUI)

A small, dependency-free Node tool that drives a local **ComfyUI** server to generate images. It is the *generation* side of the harness's vision capability — the harness **reads/assesses** images (`local-offload vqa|ocr|extract-image|assess-image`), and this **generates** them. It bakes in **no** style, prompt, model, or tuning: everything is a flag, or supply your own workflow.

## Requirements
- **Node 18+** (built-in `fetch`)
- A running **ComfyUI** (default `http://127.0.0.1:8188`) with the checkpoint/VAE you reference installed.

## Two modes

**1. Parameterized SDXL text2img (convenience)**
```bash
node render/comfy-render.mjs out.png "a misty pine forest at dawn" --steps 30 --cfg 7
```
Flags (all optional, neutral defaults): `--negative` (default *empty*), `--ckpt`, `--vae`, `--steps`, `--cfg`, `--sampler`, `--scheduler`, `--width`, `--height`, `--seed`, `--api`. Env: `COMFY_API`, `COMFY_CKPT`, `COMFY_VAE`.

**2. Any ComfyUI workflow (full control)**
```bash
node render/comfy-render.mjs out.png --graph my-workflow.json
```
`--graph` POSTs an arbitrary ComfyUI **API-format** workflow as-is — any nodes, any model, any pipeline (img2img, ControlNet, LoRA, a different base model, …). Export it from ComfyUI via **Save (API Format)**. Use this for narrow/specific pipelines; use mode 1 for quick general text2img.

## Hard exclusions (no people / no text)
SDXL honors real **negative** prompts at normal CFG, so exclusions are enforceable *when you want them* — just pass them (nothing is assumed by default):
```bash
node render/comfy-render.mjs out.png "an abstract product hero" \
  --negative "person, people, face, hands, text, letters, words, watermark, logo"
```
(The fast distilled 2026 models run at CFG≈1 and ignore negatives — that's why the convenience graph is SDXL.) Then QA the result: `local-offload assess-image out.png --json` → `{has_people, has_text, matches_brief, notes}`.

## Notes
- Writes a single PNG to the output path you give; first run after a model swap is slow on a low‑VRAM card (the poller waits up to ~6 min).
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
- **`preflight-graph.mjs`** — validates a built graph against the live `/object_info` (all
  required inputs present) **before** spending a GPU cycle.

## Usage

```bash
node render/comfy-video.mjs <out.mp4> <still.png> "<prompt>" \
     --model hunyuan --frames 17 --width 480 --height 848 \
     [--steps 50] [--seed N] [--negative "..."] [--reserve-vram 2.0] [--no-lock] [--keep-comfy]
node render/comfy-video.mjs out.mp4 still.png "<prompt>" --model wan --frames 49   # secondary
node render/preflight-graph.mjs hunyuan   # validate a graph vs a running ComfyUI, no gen
```

## 8 GB hard-won settings (don't regress)

- **cfg_distilled needs steps=50, NOT 12** — distillation removes the CFG/negative pass, not
  the step budget; 12 steps = garbage. CFG stays 1.
- **VAE decode is the OOM cliff** — `temporal_size: 4096` (decode-all-at-once) HARD-CRASHED the
  display driver at 33 frames. `vaeTemporalSize: 16` chunks the decode temporally and fits;
  raise toward 4096 only on bigger GPUs (fewer motion seams).
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

**Setup** (one-time): the `.tts-venv` is built on ComfyUI's 3.12 python (system 3.14 is too new
for torch); `pip install chatterbox-tts` then force CUDA torch (`torch==2.6.0 --index-url
.../cu124` — the package pulls CPU torch). The worker sets `HF_HUB_DISABLE_SYMLINKS=1` (Windows
blocks symlinks without Developer Mode).

**VERIFIED 2026-06-16:** a Spanish line cloned from a reference voice clip → a 6.4 s
24 kHz WAV; **whisper round-trip transcribes it back word-for-word** (intelligible Spanish),
GPU freed after. Clone *timbre* quality is best judged by ear — a clean ~10 s solo voice
reference (vs. a noisy stereo mix) will improve it.

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
