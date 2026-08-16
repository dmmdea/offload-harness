// wf-qwen-inpaint.mjs — Qwen-Image DiffSynth ControlNet INPAINT graph.
//
// The family-unified masked-inpaint route: the official Qwen-Image DiffSynth
// ControlNet inpaint patch (model_patches/*.safetensors, Apache-2.0,
// docs.comfy.org-blessed) driving a Qwen-Image DiT, so inpaint can share the
// VAE/encoder stack the generation and edit seats already use. `--family qwen`
// in comfy-inpaint.mjs selects this graph; the SDXL route stays the default.
//
// Graph follows ComfyUI's shipped `image_qwen_image_controlnet_patch` template
// (verified from the installed comfyui_workflow_templates_json package, ComfyUI
// 0.32.0) with the inpaint wiring the docs describe: ModelPatchLoader loads the
// patch, QwenImageDiffsynthControlnet takes the ORIGINAL image (not a
// preprocessed map) plus a MASK, and the latent is a plain VAEEncode of the
// source — there is NO SetLatentNoiseMask/VAEEncodeForInpaint; preservation is
// the patch's job (full-frame regeneration, denoise 1.0). Node mechanics
// verified in comfy_extras/nodes_model_patch.py:
//   · the node INVERTS the mask internally (mask = 1.0 - mask), so callers pass
//     the standard ComfyUI convention: WHITE/1.0 = region to repaint (same as
//     wf-sdxl-inpaint.mjs). Fixture masks are white-on-black RGB PNGs with no
//     alpha, so the mask must arrive via ImageToMask(red) — LoadImage's MASK
//     output would be empty for them.
//   · the (inverted) mask is bilinear-downscaled to latent size and concatenated
//     as the patch's extra input channel; residuals are added per double-block
//     scaled by `strength`.
// Same two deliberate departures as wf-qwen-image.mjs, for the same fleet
// reasons: switchable GGUF/safetensors loader, and Lightning as a composable
// LoraLoaderModelOnly rather than baked weights.
//
// OFF-LABEL NOTE (recorded, deliberate): the patch is published for base
// Qwen-Image; this fleet's on-disk bases are the 2512 line. Architecturally
// compatible (60 controlnet blocks / 3072 dim / additional_in_dim 4, verified
// against the patch's own safetensors header) — quality on 2512 is exactly what
// the inpainteval Q2 arm measures. A Q2 loss is scoped to THIS pairing, not the
// route.
//
// Step/cfg are NOT defaulted here, same policy as wf-qwen-image.mjs: the base
// recipe and the Lightning recipe are incompatible pairings and the caller must
// pick one. QWEN_INPAINT_PRESETS is the source of truth (template's KSampler
// note: base = 20 steps / cfg 2.5; 4-step Lightning = 4 / 1.0).

/** Sampler settings per base/LoRA combination (template's KSampler note). */
export const QWEN_INPAINT_PRESETS = {
  // The template's un-distilled recipe for the patched model.
  full: { steps: 20, cfg: 2.5, lora: "" },
  // Lightning distills the sampler; cfg MUST drop to 1.0. Doubly off-label on
  // this fleet (2512 LoRA on the patched 2512 base) — A/B material, not the
  // default sweep recipe.
  lightning4: { steps: 4, cfg: 1.0, lora: "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors" },
};

export function buildQwenInpaint({
  prompt, negative = "",
  image, mask,                 // staged input names (COMFY input dir), mask white=repaint
  unet,
  loader = "auto",
  patch = "qwen_image_inpaint_diffsynth_controlnet.safetensors",
  strength = 1.0,
  clip = "qwen_2.5_vl_7b_fp8_scaled.safetensors",
  vae = "qwen_image_vae.safetensors",
  lora = "", loraStrength = 1.0,
  steps, cfg,
  sampler = "euler", scheduler = "simple",
  shift = 3.1,
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "inpaint",
} = {}) {
  if (!prompt) throw new Error("buildQwenInpaint: prompt is required");
  if (!image) throw new Error("buildQwenInpaint: image (staged input name) is required");
  if (!mask) throw new Error("buildQwenInpaint: mask (staged input name, white=repaint) is required");
  if (!unet) throw new Error("buildQwenInpaint: unet is required");
  if (!patch) throw new Error("buildQwenInpaint: patch is required (the DiffSynth inpaint controlnet in model_patches/)");
  if (!Number.isFinite(steps) || steps <= 0) {
    throw new Error("buildQwenInpaint: steps is required (see QWEN_INPAINT_PRESETS — base needs 20, Lightning needs 4)");
  }
  if (!Number.isFinite(cfg) || cfg <= 0) {
    throw new Error("buildQwenInpaint: cfg is required (base needs 2.5; Lightning needs 1.0)");
  }
  if (!Number.isFinite(strength)) {
    throw new Error(`buildQwenInpaint: strength must be a number, got ${strength}`);
  }

  const isGGUF = loader === "gguf"
    || (loader === "auto" && String(unet).toLowerCase().endsWith(".gguf"));
  if (loader !== "auto" && loader !== "gguf" && loader !== "unet") {
    throw new Error(`buildQwenInpaint: loader must be auto|gguf|unet, got ${loader}`);
  }

  // No scale node on purpose: the DiffSynth patch self-heals latent-size
  // mismatches by re-encoding the control image at the sampler's latent size,
  // and this route's eval (inpainteval) hard-taints any output whose dims
  // differ from the source — so the graph renders at the SOURCE's native size.
  // Callers own /16-friendly inputs (the fixture is 1024x1024).

  const g = {
    "1": isGGUF
      ? { class_type: "UnetLoaderGGUF", inputs: { unet_name: unet } }
      : { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "2": { class_type: "CLIPLoader", inputs: { clip_name: clip, type: "qwen_image", device: "default" } },
    "3": { class_type: "VAELoader", inputs: { vae_name: vae } },
  };

  // LoRA rides the MODEL only — the text encoder is a separate CLIPLoader here.
  const modelSrc = lora ? "4" : "1";
  if (lora) {
    g["4"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["1", 0], lora_name: lora, strength_model: loraStrength } };
  }
  g["5"] = { class_type: "ModelSamplingAuraFlow", inputs: { model: [modelSrc, 0], shift } };

  g["6"] = { class_type: "LoadImage", inputs: { image } };
  g["7"] = { class_type: "LoadImage", inputs: { image: mask } };
  // White-on-black RGB mask, no alpha: the red channel IS the mask.
  g["8"] = { class_type: "ImageToMask", inputs: { image: ["7", 0], channel: "red" } };

  g["9"] = { class_type: "ModelPatchLoader", inputs: { name: patch } };
  g["10"] = {
    class_type: "QwenImageDiffsynthControlnet",
    inputs: {
      model: ["5", 0], model_patch: ["9", 0], vae: ["3", 0],
      image: ["6", 0],       // the ORIGINAL image — not a canny/preprocessed map
      strength,
      mask: ["8", 0],        // white=repaint; node inverts internally
    },
  };

  g["11"] = { class_type: "CLIPTextEncode", inputs: { text: prompt, clip: ["2", 0] } };
  // Empty OR whitespace-only negative = zeroed conditioning (the family's
  // no-negative idiom), never an encoded blank string.
  g["12"] = String(negative).trim()
    ? { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["2", 0] } }
    : { class_type: "ConditioningZeroOut", inputs: { conditioning: ["11", 0] } };

  // Latent = plain VAEEncode of the SOURCE (template shape): the patch, not a
  // latent mask, owns in/out-of-mask behavior.
  g["13"] = { class_type: "VAEEncode", inputs: { pixels: ["6", 0], vae: ["3", 0] } };
  g["14"] = {
    class_type: "KSampler",
    inputs: {
      seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1.0,
      model: ["10", 0], positive: ["11", 0], negative: ["12", 0], latent_image: ["13", 0],
    },
  };
  g["15"] = { class_type: "VAEDecode", inputs: { samples: ["14", 0], vae: ["3", 0] } };
  g["16"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["15", 0] } };
  return g;
}
