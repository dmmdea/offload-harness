### Added - FLUX 3 Image Klein as the named image family `flux3-klein`, with BFL's prompt upsampler

- **What it does.** `generate-image --family flux3-klein` (and `offload_generate_image` with `family`) renders BFL's
  5.6B FLUX 3 Image Klein DiT on either engine. ComfyUI: the official template's graph (`render/wf-flux3-klein.mjs`:
  `CLIPLoader` type `flux3`, `EmptyFlux2LatentImage`, 16 steps / cfg 1 / euler / simple; a `base` checkpoint takes 50 /
  3.5), with card-only multicard rows `flux3-klein/bf16` and `flux3-klein/nvfp4` that free the upsampler and the text
  encoder before the DiT loads. sd.cpp: the leejet GGUF with the Qwen3-VL-4B text encoder and the FLUX.2 VAE.
  License FLUX Kommunity License v1.0, bound `commercial_use: true` by operator ruling.
- **The prompt upsampler is part of the model** (BFL: "do not use the model without a prompt upsampler").
  `imagegen_upsampler` takes BFL's upsampler file (ComfyUI runs it in-graph with BFL's t2i system prompt) or a
  llama-swap model id (either engine: the harness writes the caption before the media lease, then unloads the seat so
  the render gets its card and its host RAM). The result carries `upsampled` and `upsampled_prompt`, or
  `upsample_fallback`; `refine: false` renders the prompt verbatim. The model form is the fast one: BFL's PUP as a Q6_K
  GGUF on llama.cpp wrote a caption at 86-87 tok/s on a 5070 Ti, the same model in-graph at 4.3-7.6 tok/s, and a 16 GB
  multicard BF16 render took 63-70 s instead of 132-151 s.
- **Seeds.** `ampere-6` (sd.cpp, Q4_K + Qwen3-VL-4B Q4_K_M, `--params-backend disk`), `amd-gcn` (sd.cpp Vulkan, Q8_0 /
  Q8_0, its own master-1000 build), `blackwell-8` (ComfyUI, int8 DiT and encoder, the w4a8 upsampler in-graph, RAM
  mid/high) and `ampere-8` (ComfyUI, int8 DiT and encoder, the upsampler in the model form on the tier's
  `qwen3.5-9b-agent` seat, dynamic VRAM off, RAM mid/high) seed the family; the default image binding of every tier is
  unchanged. File set with sizes and sha256: `setup/SETUP-AGENT.md`.
- **Fixed on the way.** `generate-image --steps` help and the MCP schema said "default 30" (the default is the
  binding's); sd.cpp failures now name sd-cli's first `[E]` line and the `[W]` before it in the runner's error, where the
  cause used to fall off the 400 bytes the harness keeps.

### Changed - the Windows installer pins stable-diffusion.cpp master-1000; blackwell-8's z-image arm drops `--stream-layers`

- **Why.** FLUX 3 Klein needs sd.cpp master-968 or later and the pin was master-789. master-1000 has no
  `--stream-layers` (sd-cli exits with its usage text) and deprecates `--diffusion-fa`, so blackwell-8's z-image-turbo
  arm (flat keys and named family) is now `--vae-tiling --offload-to-cpu --flash-attn off,diffusion=on --max-vram 6.5`.
  A live node keeps its installed build and its old flags together until it reinstalls.
- **Check.** Every number through the harness route, 1024², fixed seeds (the full record is in the benchmark folder
  `2026-10-24-flux3-klein-fleet`): z-image-turbo on master-1000 on the 8 GB Blackwell reference box 54.0 s warm, sign text
  exact. FLUX 3 Klein: 6 GB Ampere (sd.cpp CUDA, Q4_K) 108.9 s; Vega 7 iGPU (sd.cpp Vulkan, Q8_0) 1102 s; 8 GB Blackwell
  (ComfyUI int8) 67-69 s warm; 8 GB Ampere laptop (ComfyUI int8) 64-78 s with the model-form upsampler against 304-488 s
  in-graph (with dynamic VRAM off the in-graph upsampler's generation buffer took the card and its weights streamed for
  every token); 16 GB Blackwell multicard BF16 132-155 s and NVFP4 110-126 s with the in-graph upsampler.
  NVFP4 is not better than BF16 on 16 GB cards (the DiT is 3 s of the wall; LPIPS 0.07-0.14 against BF16, small text
  degrades), and on 8 GB cards NVFP4's streamed DiT is 17-29 % slower than INT8's for the same image.
