// wf-qwen-image-edit.mjs — Qwen-Image-Edit 2511 GENERATIVE edit graph.
//
// This is the instruction-following edit route ("make the sofa fur", "remove the
// car"), and it is a different thing from the two edit-ish routes that already
// exist: `edit_image` is deterministic PIL ops, and `inpaint_image` re-denoises
// inside a user-supplied mask with an SDXL-class checkpoint. Here the model reads
// the reference image through its own vision encoder and rewrites the whole frame
// from a text instruction — no mask.
//
// Graph follows ComfyUI's shipped `image_qwen_image_edit_2511` template. Two
// deliberate departures, both because our binding differs from the template's:
//   · loader is switchable — UnetLoaderGGUF for a .gguf unet, UNETLoader for a
//     .safetensors one. The template assumes safetensors; our verified copy on
//     this fleet is Q5_1 GGUF.
//   · the Lightning LoRA is applied as a LoRA (LoraLoaderModelOnly) rather than
//     baked into the weights. Same few-step result, but it composes with ANY
//     quantisation of the base model instead of requiring a separate merged file.
//
// The template's FluxKontextMultiReferenceLatentMethod nodes are omitted: its own
// note says they are unnecessary for Comfy-format files, and they are only needed
// for multi-reference edits, which this route does not expose yet.
//
// Step/cfg are NOT defaulted here on purpose — a Lightning LoRA at 40 steps/cfg 3
// produces mush, and the base model at 4 steps/cfg 1 produces noise. The caller
// picks a preset; QWEN_EDIT_PRESETS is the source of truth for the pairing.

/** Sampler settings that go with each base/LoRA combination. */
export const QWEN_EDIT_PRESETS = {
  // Template defaults for the un-distilled base model.
  full: { steps: 40, cfg: 3.0, lora: "" },
  // Lightning distills the sampler; cfg MUST drop to 1.0 or the guidance
  // double-counts and the edit burns out.
  lightning8: { steps: 8, cfg: 1.0, lora: "Qwen-Image-Edit-2511-Lightning-8steps-V1.0-bf16.safetensors" },
  lightning4: { steps: 4, cfg: 1.0, lora: "Qwen-Image-Edit-2511-Lightning-4steps-V1.0-bf16.safetensors" },
};

export function buildQwenImageEdit({
  image, prompt, negative = "",
  unet,
  loader = "auto",
  clip = "qwen_2.5_vl_7b_fp8_scaled.safetensors",
  vae = "qwen_image_vae.safetensors",
  lora = "", loraStrength = 1.0,
  steps, cfg,
  sampler = "euler", scheduler = "simple",
  shift = 3.1,
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "edit",
} = {}) {
  if (!image) throw new Error("buildQwenImageEdit: image (staged input filename) is required");
  if (!prompt) throw new Error("buildQwenImageEdit: prompt is required");
  if (!unet) throw new Error("buildQwenImageEdit: unet is required");
  if (!Number.isFinite(steps) || steps <= 0) {
    throw new Error("buildQwenImageEdit: steps is required (see QWEN_EDIT_PRESETS — a Lightning LoRA needs few steps, the base model needs many)");
  }
  if (!Number.isFinite(cfg) || cfg <= 0) {
    throw new Error("buildQwenImageEdit: cfg is required (Lightning needs cfg 1.0; the base model needs ~3)");
  }

  const isGGUF = loader === "gguf"
    || (loader === "auto" && String(unet).toLowerCase().endsWith(".gguf"));
  if (loader !== "auto" && loader !== "gguf" && loader !== "unet") {
    throw new Error(`buildQwenImageEdit: loader must be auto|gguf|unet, got ${loader}`);
  }

  const g = {
    "1": isGGUF
      ? { class_type: "UnetLoaderGGUF", inputs: { unet_name: unet } }
      : { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "2": { class_type: "CLIPLoader", inputs: { clip_name: clip, type: "qwen_image", device: "default" } },
    "3": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "4": { class_type: "LoadImage", inputs: { image } },
    // Snap to a resolution the model was trained on; skipping this is the usual
    // cause of soft, subtly-warped edits.
    "5": { class_type: "FluxKontextImageScale", inputs: { image: ["4", 0] } },
  };

  // LoRA rides the MODEL only — the text encoder is a separate CLIPLoader here.
  const modelSrc = lora ? "6" : "1";
  if (lora) {
    g["6"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["1", 0], lora_name: lora, strength_model: loraStrength } };
  }
  g["7"] = { class_type: "ModelSamplingAuraFlow", inputs: { model: [modelSrc, 0], shift } };

  // vae is wired into the encoder so it emits reference latents alongside the
  // vision tokens; without it the model sees the instruction but not the image.
  g["8"] = { class_type: "TextEncodeQwenImageEditPlus", inputs: { clip: ["2", 0], prompt, vae: ["3", 0], image1: ["5", 0] } };
  g["9"] = { class_type: "TextEncodeQwenImageEditPlus", inputs: { clip: ["2", 0], prompt: negative, vae: ["3", 0], image1: ["5", 0] } };
  g["10"] = { class_type: "VAEEncode", inputs: { pixels: ["5", 0], vae: ["3", 0] } };
  g["11"] = {
    class_type: "KSampler",
    inputs: {
      seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1.0,
      model: ["7", 0], positive: ["8", 0], negative: ["9", 0], latent_image: ["10", 0],
    },
  };
  g["12"] = { class_type: "VAEDecode", inputs: { samples: ["11", 0], vae: ["3", 0] } };
  g["13"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["12", 0] } };
  return g;
}
