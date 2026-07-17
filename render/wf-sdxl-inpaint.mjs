// wf-sdxl-inpaint.mjs — SDXL-family generative INPAINT graph (core ComfyUI nodes only).
// Masked re-denoise: white mask pixels get re-rendered from the prompt, black stays.
// VAEEncodeForInpaint zeroes the masked latents (full re-imagination inside the mask),
// so denoise stays 1.0 by default; grow_mask_by expands + feathers the seam in latent
// space. Which checkpoint/VAE a machine inpaints with is per-machine config (inpaint_*),
// never shared code. HiDream-O1 (pixel-space DiT) is NOT supported by this graph —
// the route requires an SDXL-class binding (see the plan's global constraints).
export function buildSDXLInpaint({
  image, mask, prompt, negative = "", ckpt,
  vae = "builtin",
  steps = 30, cfg = 7, sampler = "dpmpp_2m", scheduler = "karras",
  seed = Math.floor(Math.random() * 1e15),
  denoise = 1.0, growMask = 16,
} = {}) {
  if (!image) throw new Error("buildSDXLInpaint: image (staged input filename) is required");
  if (!mask) throw new Error("buildSDXLInpaint: mask (staged input filename) is required");
  if (!prompt) throw new Error("buildSDXLInpaint: prompt is required");
  if (!ckpt) throw new Error("buildSDXLInpaint: ckpt is required");
  const builtinVAE = ["builtin", "none", "checkpoint"].includes(String(vae).toLowerCase());
  const vaeRef = builtinVAE ? ["4", 2] : ["10", 0];
  const g = {
    "4":  { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: ckpt } },
    "11": { class_type: "LoadImage", inputs: { image } },
    "12": { class_type: "LoadImage", inputs: { image: mask } },
    // Mask from the mask IMAGE's red channel: a plain white-on-black PNG works with no
    // alpha-channel requirement (LoadImage's own MASK output needs alpha).
    "13": { class_type: "ImageToMask", inputs: { image: ["12", 0], channel: "red" } },
    "14": { class_type: "VAEEncodeForInpaint", inputs: { pixels: ["11", 0], vae: vaeRef, mask: ["13", 0], grow_mask_by: growMask } },
    "6":  { class_type: "CLIPTextEncode", inputs: { text: prompt, clip: ["4", 1] } },
    "7":  { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["4", 1] } },
    "3":  { class_type: "KSampler", inputs: { seed, steps, cfg, sampler_name: sampler, scheduler, denoise, model: ["4", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["14", 0] } },
    "8":  { class_type: "VAEDecode", inputs: { samples: ["3", 0], vae: vaeRef } },
    "9":  { class_type: "SaveImage", inputs: { filename_prefix: "inpaint", images: ["8", 0] } },
  };
  if (!builtinVAE) g["10"] = { class_type: "VAELoader", inputs: { vae_name: vae } };
  return g;
}
