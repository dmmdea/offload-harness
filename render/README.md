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
