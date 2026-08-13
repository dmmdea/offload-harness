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
// A THIRD departure, and the one with teeth: the input scaler is
// ImageScaleToTotalPixels, not the template's FluxKontextImageScale.
// FluxKontextImageScale takes no size argument — it hard-snaps every input to the
// nearest-aspect entry of Flux-Kontext's fixed 17-entry table, whose LARGEST entry is
// 1024x1024 (1.05 MP). It is a Flux-Kontext helper, and in this graph it silently
// capped the edit seat: a 2048x1024 source came out 1456x720, every time, with no way
// to ask for more.
//
// That cap is on the OUTPUT, which is why it matters. Node 5 feeds two places, and
// only one of them cares:
//   · the two TextEncodeQwenImageEditPlus encoders — which do NOT care, because that
//     node rescales internally anyway (384x384 for the vision tokens, ~1 MP snapped to
//     8 for the reference latents). Whatever we hand them, they resize it themselves.
//   · VAEEncode -> KSampler.latent_image at denoise 1.0 — which decides the rendered
//     resolution outright. Node 5's output size IS the edit's output size.
// So the scaler was never buying the encoders anything; it was only ever setting (and
// capping) the canvas.
//
// ImageScaleToTotalPixels takes an explicit megapixel target and a `resolution_steps`
// snap. Step 16 is required, not cosmetic: the Qwen-Image VAE downscales 8x and the DiT
// patch size is 2, so pixel dimensions must be multiples of 16 for the latent to tile
// without padding. That snap is what the removed node was doing for us by accident.
//
// Step/cfg are NOT defaulted here on purpose — a Lightning LoRA at 40 steps/cfg 3
// produces mush, and the base model at 4 steps/cfg 1 produces noise. The caller
// picks a preset; QWEN_EDIT_PRESETS is the source of truth for the pairing.
// Megapixels IS defaulted, because unlike steps/cfg a resolution cannot be "half
// overridden" into a plausible-looking ruin — it is either the size you asked for or
// the size the seat caps you at.

/**
 * Ceiling on the edit canvas when the caller does not name one.
 *
 * 2.0 is not a round guess: 2048x1024 is exactly 2.0 MP under this node's
 * megapixels*1024*1024 arithmetic, so the harness's own 2048x1024 renders survive an
 * edit at their native resolution (scale factor 1.0, and both dimensions are already
 * multiples of 16). A ceiling is required rather than "just keep the source": the seat
 * is bound on 16 GB cards carrying a ~15 GB unet, and a 24 MP phone photo fed in at
 * full size is an OOM, not a feature.
 */
export const EDIT_MEGAPIXEL_CAP = 2.0;

/**
 * Floor on the edit canvas. A tiny source is scaled UP onto the model's working
 * resolution rather than rendered at its own size, which is what the node this
 * replaced did anyway (it took a 512x512 input to 1024x1024) and what both official
 * templates do — they normalise to a working canvas rather than preserve the input.
 *
 * 0.9 is chosen to sit just under the whole 1-MP-class training grid, whose lowest
 * member is 1536x640 at 0.9375 MP (1024x1024 = 1.0, 1344x768 and 1152x896 = 0.984,
 * 1216x832 = 0.965). Every one of those therefore keeps scale factor 1.0 and comes out
 * byte-for-byte its own size; a floor of 1.0 would have quietly stretched a 1344x768
 * source to 1360x768.
 *
 * It also keeps the arithmetic out of two holes found by running ComfyUI's own formula
 * over real files: a 97x53 thumbnail resolves to 0.0049 MP, under the node's declared
 * 0.01 minimum, and a pathological aspect ratio can snap a dimension to 0.
 */
export const EDIT_MEGAPIXEL_FLOOR = 0.9;

/** Qwen-Image VAE stride (8) x DiT patch size (2). Dimensions must be multiples of this. */
export const EDIT_RESOLUTION_STEPS = 16;

/**
 * Pick the megapixel target for one edit.
 *   configured > 0  -> honour it verbatim (gen_edit_megapixels / --megapixels).
 *   source known    -> the SOURCE's own megapixels, held inside [floor, cap]. In-band
 *                      sources round-trip at scale factor 1.0 — their own size, exactly.
 *   source unknown  -> the cap, which is the non-destructive choice: the failure this
 *                      whole node swap exists to kill is silently shrinking a source.
 */
export function resolveEditMegapixels({ configured = 0, width = 0, height = 0 } = {}) {
  if (Number.isFinite(configured) && configured > 0) return configured;
  if (!(width > 0 && height > 0)) return EDIT_MEGAPIXEL_CAP;
  // Exact: w*h is an integer and 1024*1024 is a power of two, so this division is
  // lossless in float64 and a same-size source round-trips at scale factor 1.0.
  const src = (width * height) / (1024 * 1024);
  return Math.min(Math.max(src, EDIT_MEGAPIXEL_FLOOR), EDIT_MEGAPIXEL_CAP);
}

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
  megapixels = EDIT_MEGAPIXEL_CAP,
  upscaleMethod = "lanczos",
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
  // ImageScaleToTotalPixels declares min 0.01 / max 16.0. Fail here with the number in
  // hand rather than let ComfyUI reject the whole prompt after the GPU lock is taken.
  if (!Number.isFinite(megapixels) || megapixels < 0.01 || megapixels > 16) {
    throw new Error(`buildQwenImageEdit: megapixels must be within [0.01, 16], got ${megapixels}`);
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
    // The working canvas — and therefore the OUTPUT resolution, since this feeds
    // VAEEncode -> KSampler.latent_image at denoise 1.0. `resolution_steps: 16` keeps
    // both dimensions on the VAE-stride x patch-size grid; skipping that snap is the
    // usual cause of soft, subtly-warped edits. See the header note on why this is NOT
    // the template's FluxKontextImageScale.
    "5": {
      class_type: "ImageScaleToTotalPixels",
      inputs: {
        image: ["4", 0],
        upscale_method: upscaleMethod,
        megapixels,
        resolution_steps: EDIT_RESOLUTION_STEPS,
      },
    },
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
