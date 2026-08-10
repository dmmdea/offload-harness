// wf-qwen-image.mjs — Qwen-Image 2512 TEXT-TO-IMAGE graph.
//
// This is the prompt-adherence generation route: Qwen-Image 2512 is the newer DiT
// with markedly stronger instruction/text rendering than the SDXL-class default,
// and it was previously reachable only through run-graph with a hand-built
// workflow. `--family qwen-image` in comfy-render.mjs selects this graph.
//
// Graph follows ComfyUI's shipped `image_qwen_Image_2512` template (subgraph
// "Text to Image (Qwen-Image 2512)"), verified from the installed
// comfyui_workflow_templates_json package. BOTH presets come from that one
// template: `full` is its un-distilled path and `lightning4` is its own
// enable-4-steps-LoRA switch branch (primitives 4 / cfg 1 / the lightx2v LoRA)
// — NOT the separate `..._with_2steps_lora` template, which uses a different
// vendor's 2-step LoRA. Two deliberate departures, the same two as
// wf-qwen-image-edit.mjs and for the same reason — our fleet binding differs
// from the template's:
//   · loader is switchable — UnetLoaderGGUF for a .gguf unet, UNETLoader for a
//     .safetensors one. The template assumes fp8 safetensors; our verified copy
//     on this fleet is Q5_1 GGUF (and 2511/2512 K-quants are prohibited — see
//     docs/systems/media-generation.md; only `_1` quants load).
//   · the Lightning LoRA is applied as a LoRA (LoraLoaderModelOnly) rather than
//     baked into the weights, so it composes with ANY quantisation of the base.
//
// Qwen-Image is an SD3-latent-space DiT with its own text encoder and VAE split
// into separate files — there is no all-in-one checkpoint, so the generic
// CheckpointLoaderSimple graph cannot even load it. EmptySD3LatentImage is the
// correct latent source (16-channel); the SD1/SDXL EmptyLatentImage is wrong.
//
// Negative-prompt semantics — a deliberate house choice, stated honestly: the
// shipped 2512 template bakes a default Chinese quality-negative into BOTH its
// branches, and this harness bakes in NO prompt content by policy. With no
// negative of our own, an empty `negative` becomes ConditioningZeroOut of the
// positive — the family's no-negative idiom (it is how the official
// `..._2512_with_2steps_lora` template runs) — never an encoded empty string.
// A non-empty (post-trim) negative is encoded normally and is active at cfg > 1.
//
// Step/cfg are NOT defaulted here on purpose — the base model at 4 steps/cfg 1
// produces noise and a Lightning LoRA at 50 steps/cfg 4 produces mush. The
// caller picks a preset; QWEN_IMAGE_PRESETS is the source of truth for the
// pairing (template widgets: full = 50/4, lightning4 = 4/1 + LoRA).

/** Sampler settings that go with each base/LoRA combination (template widgets). */
export const QWEN_IMAGE_PRESETS = {
  // The shipped 2512 template's un-distilled recipe.
  full: { steps: 50, cfg: 4.0, lora: "" },
  // Lightning distills the sampler; cfg MUST drop to 1.0 or the guidance
  // double-counts and the render burns out.
  lightning4: { steps: 4, cfg: 1.0, lora: "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors" },
};

export function buildQwenImage({
  prompt, negative = "",
  unet,
  loader = "auto",
  clip = "qwen_2.5_vl_7b_fp8_scaled.safetensors",
  vae = "qwen_image_vae.safetensors",
  lora = "", loraStrength = 1.0,
  steps, cfg,
  sampler = "euler", scheduler = "simple",
  shift = 3.1,
  width = 1328, height = 1328,   // template-native 1:1; the official aspect table is all /16
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "render",
} = {}) {
  if (!prompt) throw new Error("buildQwenImage: prompt is required");
  if (!unet) throw new Error("buildQwenImage: unet is required");
  if (!Number.isFinite(steps) || steps <= 0) {
    throw new Error("buildQwenImage: steps is required (see QWEN_IMAGE_PRESETS — a Lightning LoRA needs few steps, the base model needs many)");
  }
  if (!Number.isFinite(cfg) || cfg <= 0) {
    throw new Error("buildQwenImage: cfg is required (Lightning needs cfg 1.0; the base model needs ~4)");
  }

  const isGGUF = loader === "gguf"
    || (loader === "auto" && String(unet).toLowerCase().endsWith(".gguf"));
  if (loader !== "auto" && loader !== "gguf" && loader !== "unet") {
    throw new Error(`buildQwenImage: loader must be auto|gguf|unet, got ${loader}`);
  }

  // Fail loud on nonsense dims: NaN would otherwise ride silently into the graph
  // and surface only as a remote ComfyUI validation error after the round-trip.
  if (!Number.isFinite(width) || width <= 0 || !Number.isFinite(height) || height <= 0) {
    throw new Error(`buildQwenImage: width/height must be positive numbers, got ${width}x${height}`);
  }
  // VAE 8x downsample + DiT patch size 2 → pixel dims must be /16 (every entry
  // in the template's aspect table is: 1328x1328, 1664x928, 1472x1104, ...).
  width = Math.max(64, Math.floor(width / 16) * 16);
  height = Math.max(64, Math.floor(height / 16) * 16);

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

  g["6"] = { class_type: "CLIPTextEncode", inputs: { text: prompt, clip: ["2", 0] } };
  // Empty OR whitespace-only negative = zeroed conditioning (the family's
  // no-negative idiom), never an encoded blank string; a real negative is
  // encoded and active at cfg > 1.
  g["7"] = String(negative).trim()
    ? { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["2", 0] } }
    : { class_type: "ConditioningZeroOut", inputs: { conditioning: ["6", 0] } };
  g["8"] = { class_type: "EmptySD3LatentImage", inputs: { width, height, batch_size: 1 } };
  g["9"] = {
    class_type: "KSampler",
    inputs: {
      seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1.0,
      model: ["5", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["8", 0],
    },
  };
  g["10"] = { class_type: "VAEDecode", inputs: { samples: ["9", 0], vae: ["3", 0] } };
  g["11"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["10", 0] } };
  return g;
}
