### Fixed - an edit at the seed that rendered its source no longer comes out burnt

- **What it does.** `render/noise-seed.mjs` mixes the caller's seed with the bytes of every source image. That mix is the KSampler seed for both edit families (`render/comfy-edit.mjs`, `planEdit`) and both inpaint families (`render/comfy-inpaint.mjs`). The same seed and the same file give the same render. The result and the runner's `queued` line still carry the caller's seed, and the plan's `describe` names the noise seed.
- **Why.** ComfyUI draws a KSampler's starting noise from its seed in the latent's own shape. A source rendered at seed S, then edited at seed S on the same canvas, starts from the noise that made the source, and Qwen-Image-Edit-2511 burns that edit: posterized colour and clipped channels. A pipeline that keeps one seed does exactly that, and so does a chain of edits at one seed.
- **Check.** On node-i (two A2s), 2026-11-08, a card-only 2511 edit (fp8mixed, Lightning 8-step, 1.0 MP) of a source rendered at seed 7:
  - at seed 7, 21.5 % of the pixels had a channel at 0 and 14.0 % one at 255;
  - at seeds 8 and 1234, with nothing else changed, 1.1 % and 0.3 %, and both edits were clean when looked at.
  - The burn stayed the same with the fp8 or the bf16 encoder, without the vision tower, under either reference method, as a Q3_K_M GGUF on one card, and at 40 steps without the LoRA. Without the reference latent, the graph rendered clean.
