// wf-qwen-image-21.mjs — Qwen-Image-2.1 TEXT-TO-IMAGE and multi-reference EDIT graphs.
//
// Qwen-Image-2.1 is a 7B single-stream DiT with a Qwen3-VL-8B text encoder and a
// NEW 4-channel (RGBA) VAE. It is NOT a variant of Qwen-Image 2512: wf-qwen-image.mjs
// (SD3 latent, AuraFlow shift, CLIPTextEncode) cannot drive it. `--family
// qwen-image-2.1` in comfy-render.mjs (T2I) and comfy-edit.mjs (edit) selects these
// graphs. It needs ComfyUI >= v0.37.0 (the nodes below arrived in PR #16400).
//
// LICENSE: the weights are Qwen Research License (non-commercial; §1.i/§2.a/§2.b).
// The harness binds this family only as a NAMED opt-in overlay that tags every
// result (ADR 0058) — nothing in this file decides that, it only builds graphs.
//
// Graph facts, each verified at ComfyUI source (master 95539f56) and against the
// official templates (image_qwen_image_2_1_t2i / _image_edit):
//   · UNETLoader(weight_dtype "default") + CLIPLoader(type "qwen_image" — a
//     Qwen3-VL-8B file under CLIPType.QWEN_IMAGE routes to the 2.1 encoder) +
//     VAELoader(the 2.1 VAE).
//   · TextEncodeQwenImage21(clip, prompt, negative_prompt, resolution, [vae],
//     images.image_1..image_16) -> positive, negative, latent. The reference
//     inputs are an Autogrow group: in API format they are named
//     "images.image_N" (comfy_api finalize_prefix joins group + name with ".";
//     the saved edit template wires exactly those names).
//   · EmptyLatentImage for T2I: it emits a 4-channel /8 latent tagged
//     downscale_ratio_spacial 8, and the sampler re-shapes it to the model's
//     64-channel /16 latent (comfy.sample.fix_empty_latent_channels). NOT
//     EmptySD3LatentImage and NO ModelSamplingAuraFlow (that is a linear-shift
//     family; 2.1 samples as ModelType.FLUX with an exponential shift).
//   · VAEDecode yields a 4-channel IMAGE (the VAE's output_channels is 4). SaveImage
//     writes RGBA verbatim. Unless the caller asked for transparency, the alpha is
//     split off (SplitImageWithAlpha -> RGB) so an ordinary prompt can never hand a
//     partially-transparent PNG to a downstream compositor.
//
// Dimensions: every official 2K size is a multiple of 32 (2048x2048, 2400x1792,
// 2528x1696, 2752x1536, and their transposes) and diffusers floors to 32, so W/H
// are snapped DOWN to /32 (floor 256). Default 2048x2048 — the model's native 2K.
//
// Sampling defaults are the official recipe: 40 steps, euler, cfg 1.0 (one DiT
// pass per step; the negative is unused at cfg 1). Two schedule modes:
//   "official" — diffusers' FlowMatchEulerDiscreteScheduler as configured in the
//     model repo (use_dynamic_shifting, exponential time shift, mu from the target
//     token count on a 256 -> 8192 line, shift_terminal 0.02), computed HERE and
//     fed through ManualSigmas + SamplerCustomAdvanced. Golden-tested against the
//     real scheduler (render/testdata/qwen-image-21-sigmas.golden.json).
//   "comfy" — KSampler(euler, simple) with the model's own fixed shift (ComfyUI
//     supported_models.QwenImage21: shift 0.69 at every size, no terminal stretch).
// ComfyUI issue #16447 contests which looks better at 2K; the builder carries both
// and the binding (imagegen_schedule) picks.
//
// Edit: image_1 is the edit TARGET (the encoder emits its empty latent on
// image_1's grid, and "any other size shifts the edit"), images 2..N are
// references (<image2>..<imageN> in the prompt). Up to 10 (the official limit;
// the node itself grows to 16). The model path runs through QwenImage21Cache so
// the prefix KV-cache placement is a binding choice (device auto|gpu|cpu|off —
// "off" and "gpu" stay out of the host-RAM prefetch path of ComfyUI #16443).

/** The official sampling recipe (QwenLM README: 40 steps, guidance 1; diffusers true_cfg_scale=1.0). */
export const QWEN_IMAGE_21_RECIPE = Object.freeze({ steps: 40, cfg: 1.0, sampler: "euler", scheduler: "simple" });

/** Native 2K default size (README aspect table, 1:1). */
export const QWEN_IMAGE_21_DEFAULT_SIZE = 2048;

/** Snap grid and floor for T2I dimensions. */
export const QWEN_IMAGE_21_DIM_STEP = 32;
export const QWEN_IMAGE_21_DIM_MIN = 256;
/** ComfyUI's MAX_RESOLUTION (EmptyLatentImage max). */
export const QWEN_IMAGE_21_DIM_MAX = 16384;

/** The official reference-image ceiling (the node's Autogrow allows 16). */
export const QWEN_IMAGE_21_MAX_REFS = 10;

/** Encoder `resolution` default (the node's own default and the official edit default). */
export const QWEN_IMAGE_21_RESOLUTION = 1024;

/** The two schedule modes. */
export const QWEN_IMAGE_21_SCHEDULES = Object.freeze(["official", "comfy"]);

/** QwenImage21Cache options (comfy_extras/nodes_qwen.py). */
export const QWEN_IMAGE_21_CACHE_DEVICES = Object.freeze(["auto", "gpu", "cpu", "off"]);
export const QWEN_IMAGE_21_CACHE_DTYPES = Object.freeze(["default", "int8", "int4"]);

/**
 * Qwen/Qwen-Image-2.1 scheduler/scheduler_config.json (the fields the official
 * pipeline reads). max/base shift ride a line from base_image_seq_len to
 * max_image_seq_len tokens and are EXTRAPOLATED past it (a 2048x2048 target is
 * 16384 tokens).
 */
export const QWEN_IMAGE_21_SCHEDULER = Object.freeze({
  baseShift: 0.5,
  maxShift: 0.9,
  baseImageSeqLen: 256,
  maxImageSeqLen: 8192,
  shiftTerminal: 0.02,
});

/**
 * The official transparent-image prompt format (QwenLM/Qwen-Image-2.1 README,
 * "Transparent Image Generation (RGBA)"):
 *   This is an RGBA image with transparency. <your description>. The image has
 *   alpha channel and the background is transparent.
 */
export const RGBA_PROMPT_PREFIX = "This is an RGBA image with transparency. ";
export const RGBA_PROMPT_SUFFIX = ". The image has alpha channel and the background is transparent.";

/** Wrap a description in the official RGBA template (idempotent). */
export function rgbaPrompt(desc) {
  const s = String(desc ?? "").trim();
  if (s.startsWith(RGBA_PROMPT_PREFIX.trim())) return s; // already in the template: never double-wrap
  // The template supplies the sentence's closing period; a caller's own trailing
  // period(s) would otherwise render as ".." inside the conditioning text.
  const body = s.replace(/[.\s]+$/, "");
  return RGBA_PROMPT_PREFIX + body + RGBA_PROMPT_SUFFIX;
}

/** Snap one pixel dimension DOWN to the /32 grid, floored at 256. */
export function snapDim(v) {
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) throw new Error(`qwen-image-2.1: dimension must be a positive number, got ${v}`);
  if (n > QWEN_IMAGE_21_DIM_MAX) throw new Error(`qwen-image-2.1: dimension ${n} exceeds ComfyUI's maximum ${QWEN_IMAGE_21_DIM_MAX}`);
  return Math.max(QWEN_IMAGE_21_DIM_MIN, Math.floor(n / QWEN_IMAGE_21_DIM_STEP) * QWEN_IMAGE_21_DIM_STEP);
}

/** Target token count the official pipeline computes mu from: one DiT token per 16x16 px tile. */
export function qwenImage21Tokens(width, height) {
  return Math.floor(height / 16) * Math.floor(width / 16);
}

/** diffusers calculate_shift: mu on the base->max line, extrapolated past max_image_seq_len. */
export function qwenImage21Mu(width, height, s = QWEN_IMAGE_21_SCHEDULER) {
  const n = qwenImage21Tokens(width, height);
  const m = (s.maxShift - s.baseShift) / (s.maxImageSeqLen - s.baseImageSeqLen);
  const b = s.baseShift - m * s.baseImageSeqLen;
  return n * m + b;
}

/**
 * The official sigma schedule, step for step what diffusers'
 * FlowMatchEulerDiscreteScheduler.set_timesteps(sigmas=linspace(1, 1/steps, steps), mu)
 * produces with the model's scheduler config:
 *   1. sigmas = linspace(1.0, 1/steps, steps)
 *   2. exponential time shift: e^mu / (e^mu + (1/t - 1))
 *   3. stretch so the last value lands on shift_terminal:
 *        scale = (1 - s[-1]) / (1 - terminal);  s = 1 - (1 - s) / scale
 *   4. append the terminal 0
 * Returns steps + 1 values, strictly decreasing, first 1.0, last 0.
 */
export function qwenImage21Sigmas({ width, height, steps, scheduler = QWEN_IMAGE_21_SCHEDULER } = {}) {
  if (!Number.isInteger(steps) || steps < 1) throw new Error(`qwen-image-2.1: steps must be a positive integer, got ${steps}`);
  const mu = qwenImage21Mu(width, height, scheduler);
  const emu = Math.exp(mu);
  // numpy.linspace's own arithmetic: i * step + start, with the endpoint pinned.
  const stop = 1 / steps;
  const step = steps === 1 ? 0 : (stop - 1.0) / (steps - 1);
  const lin = Array.from({ length: steps }, (_, i) => (i === steps - 1 ? stop : i * step + 1.0));
  let sig = lin.map((t) => emu / (emu + (1 / t - 1)));
  if (scheduler.shiftTerminal) {
    const scale = (1 - sig[sig.length - 1]) / (1 - scheduler.shiftTerminal);
    sig = sig.map((t) => 1 - (1 - t) / scale);
  }
  sig.push(0);
  return sig;
}

/**
 * ManualSigmas' parser is re.findall(r"[-+]?(?:\d*\.*\d+)") — it has NO exponent
 * support, so "1e-05" would parse as the two numbers 1 and -05. Every value is
 * printed fixed-point (toFixed never switches to exponent form below 1e21).
 * 8 decimals is below float32 resolution near 1.0, which is what ComfyUI stores.
 */
export function formatSigmas(sigmas, decimals = 8) {
  return sigmas.map((v) => {
    if (!Number.isFinite(v)) throw new Error(`qwen-image-2.1: non-finite sigma ${v}`);
    return v.toFixed(decimals);
  }).join(", ");
}

function requireName(v, what) {
  if (typeof v !== "string" || !v.trim()) throw new Error(`buildQwenImage21: ${what} is required`);
  if (["builtin", "none", "checkpoint"].includes(v.trim().toLowerCase())) {
    throw new Error(`buildQwenImage21: ${what} must name a file — the 2.1 weights ship as separate UNET/text-encoder/VAE files, there is no built-in ${what}`);
  }
}

function requireSampling(steps, cfg) {
  // Steps and cfg travel together or not at all (the harness pair guard): a
  // half-override is the classic silently-wrong render.
  if ((steps === undefined) !== (cfg === undefined)) {
    throw new Error("buildQwenImage21: steps and cfg are a pair — set both or neither (official recipe 40 / 1.0)");
  }
  const st = steps === undefined ? QWEN_IMAGE_21_RECIPE.steps : steps;
  const c = cfg === undefined ? QWEN_IMAGE_21_RECIPE.cfg : cfg;
  if (!Number.isInteger(st) || st < 1 || st > 10000) throw new Error(`buildQwenImage21: steps must be an integer in [1, 10000], got ${st}`);
  if (!Number.isFinite(c) || c <= 0 || c > 100) throw new Error(`buildQwenImage21: cfg must be in (0, 100], got ${c}`);
  return { steps: st, cfg: c };
}

function requireResolution(r) {
  if (!Number.isInteger(r) || r < 0 || r > 4096 || r % 32 !== 0) {
    throw new Error(`buildQwenImage21: resolution must be 0 (keep each reference's own size) or a multiple of 32 up to 4096, got ${r}`);
  }
}

function loaders({ unet, clip, vae }) {
  requireName(unet, "unet (the 2.1 diffusion model, e.g. qwen_image_2.1_bf16.safetensors)");
  requireName(clip, "clip (the Qwen3-VL-8B text encoder, e.g. qwen3vl_8b_bf16.safetensors)");
  requireName(vae, "vae (the 2.1 RGBA VAE, e.g. qwen_image_2.1_vae_bf16.safetensors)");
  if (String(unet).toLowerCase().endsWith(".gguf")) {
    // No GGUF loader is wired: 2.1 GGUFs need the leejet ComfyUI-GGUF fork, and the
    // city96 pack most boxes carry reports "Unknown model architecture".
    throw new Error("buildQwenImage21: no GGUF loader is wired for qwen-image-2.1 — bind a .safetensors UNET (bf16 or int8_convrot)");
  }
  return {
    "1": { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "2": { class_type: "CLIPLoader", inputs: { clip_name: clip, type: "qwen_image", device: "default" } },
    "3": { class_type: "VAELoader", inputs: { vae_name: vae } },
  };
}

function decodeAndSave(g, samplesRef, transparent, filenamePrefix) {
  g["11"] = { class_type: "VAEDecode", inputs: { samples: samplesRef, vae: ["3", 0] } };
  if (transparent) {
    g["13"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["11", 0] } };
  } else {
    // Output 0 is the RGB image; output 1 is the (inverted) alpha as a MASK, unused.
    g["12"] = { class_type: "SplitImageWithAlpha", inputs: { image: ["11", 0] } };
    g["13"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["12", 0] } };
  }
}

/**
 * Text-to-image. Required: prompt, unet, clip, vae. W/H snap down to /32
 * (floor 256), default 2048x2048. schedule "official" (default) | "comfy".
 * transparent wraps the prompt in the official RGBA template and keeps the
 * alpha channel; otherwise the output is an opaque RGB PNG.
 */
export function buildQwenImage21({
  prompt, negative = "",
  unet, clip, vae,
  steps, cfg,
  sampler = QWEN_IMAGE_21_RECIPE.sampler,
  scheduler = QWEN_IMAGE_21_RECIPE.scheduler,
  schedule = "official",
  transparent = false,
  width = QWEN_IMAGE_21_DEFAULT_SIZE, height = QWEN_IMAGE_21_DEFAULT_SIZE,
  resolution = QWEN_IMAGE_21_RESOLUTION,
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "render",
} = {}) {
  if (typeof prompt !== "string" || !prompt.trim()) throw new Error("buildQwenImage21: prompt is required");
  if (!QWEN_IMAGE_21_SCHEDULES.includes(schedule)) {
    throw new Error(`buildQwenImage21: schedule must be one of ${QWEN_IMAGE_21_SCHEDULES.join("|")}, got ${schedule}`);
  }
  const s = requireSampling(steps, cfg);
  requireResolution(resolution);
  if (!Number.isInteger(seed) || seed < 0) throw new Error(`buildQwenImage21: seed must be a non-negative integer, got ${seed}`);
  const W = snapDim(width);
  const H = snapDim(height);

  const g = loaders({ unet, clip, vae });
  g["4"] = {
    class_type: "TextEncodeQwenImage21",
    inputs: {
      clip: ["2", 0],
      prompt: transparent ? rgbaPrompt(prompt) : prompt,
      negative_prompt: negative || "",
      resolution,
    },
  };
  g["5"] = { class_type: "EmptyLatentImage", inputs: { width: W, height: H, batch_size: 1 } };

  if (schedule === "official") {
    g["6"] = { class_type: "RandomNoise", inputs: { noise_seed: seed } };
    // cfg 1.0 (the official recipe) is ONE model pass per step: BasicGuider on the
    // positive. Any other cfg is real classifier-free guidance, which BasicGuider
    // cannot express — a CFGGuider then carries the negative, as diffusers does
    // when true_cfg_scale > 1.
    g["7"] = s.cfg === 1
      ? { class_type: "BasicGuider", inputs: { model: ["1", 0], conditioning: ["4", 0] } }
      : { class_type: "CFGGuider", inputs: { model: ["1", 0], positive: ["4", 0], negative: ["4", 1], cfg: s.cfg } };
    g["8"] = { class_type: "KSamplerSelect", inputs: { sampler_name: sampler } };
    g["9"] = { class_type: "ManualSigmas", inputs: { sigmas: formatSigmas(qwenImage21Sigmas({ width: W, height: H, steps: s.steps })) } };
    g["10"] = {
      class_type: "SamplerCustomAdvanced",
      inputs: { noise: ["6", 0], guider: ["7", 0], sampler: ["8", 0], sigmas: ["9", 0], latent_image: ["5", 0] },
    };
  } else {
    g["10"] = {
      class_type: "KSampler",
      inputs: {
        seed, steps: s.steps, cfg: s.cfg, sampler_name: sampler, scheduler, denoise: 1.0,
        model: ["1", 0], positive: ["4", 0], negative: ["4", 1], latent_image: ["5", 0],
      },
    };
  }
  decodeAndSave(g, ["10", 0], transparent, filenamePrefix);
  return g;
}

/**
 * Multi-reference edit. images[0] is the edit TARGET (image_1), the rest are
 * references (<image2>..). Each entry is a filename already staged into
 * ComfyUI's input dir. Up to QWEN_IMAGE_21_MAX_REFS. resolution: the encoder's
 * target side length (default 1024; 0 keeps each reference's own size, rounded
 * to 32). The sampler denoises the encoder's own latent (output 2), which sits
 * on image_1's grid — so the output takes the target's aspect. transparent keeps
 * both the sources' alpha (JoinImageWithAlpha: LoadImage flattens to RGB and
 * returns alpha as an inverted MASK) and the output's; otherwise RGB in, RGB out.
 */
export function buildQwenImage21Edit({
  images, prompt, negative = "",
  unet, clip, vae,
  steps, cfg,
  sampler = QWEN_IMAGE_21_RECIPE.sampler,
  scheduler = QWEN_IMAGE_21_RECIPE.scheduler,
  resolution = QWEN_IMAGE_21_RESOLUTION,
  cacheDevice = "auto", cacheDtype = "default",
  transparent = false,
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "edit",
} = {}) {
  if (!Array.isArray(images) || images.length === 0) {
    throw new Error("buildQwenImage21Edit: images is required (images[0] is the edit target)");
  }
  if (images.length > QWEN_IMAGE_21_MAX_REFS) {
    throw new Error(`buildQwenImage21Edit: at most ${QWEN_IMAGE_21_MAX_REFS} images (the official reference limit), got ${images.length}`);
  }
  images.forEach((im, i) => {
    if (typeof im !== "string" || !im.trim()) throw new Error(`buildQwenImage21Edit: images[${i}] must be a staged input filename`);
  });
  if (typeof prompt !== "string" || !prompt.trim()) throw new Error("buildQwenImage21Edit: prompt is required");
  if (!QWEN_IMAGE_21_CACHE_DEVICES.includes(cacheDevice)) {
    throw new Error(`buildQwenImage21Edit: cacheDevice must be one of ${QWEN_IMAGE_21_CACHE_DEVICES.join("|")}, got ${cacheDevice}`);
  }
  if (!QWEN_IMAGE_21_CACHE_DTYPES.includes(cacheDtype)) {
    throw new Error(`buildQwenImage21Edit: cacheDtype must be one of ${QWEN_IMAGE_21_CACHE_DTYPES.join("|")}, got ${cacheDtype}`);
  }
  const s = requireSampling(steps, cfg);
  requireResolution(resolution);
  if (!Number.isInteger(seed) || seed < 0) throw new Error(`buildQwenImage21Edit: seed must be a non-negative integer, got ${seed}`);

  const g = loaders({ unet, clip, vae });
  g["4"] = { class_type: "QwenImage21Cache", inputs: { model: ["1", 0], device: cacheDevice, dtype: cacheDtype } };

  const enc = {
    clip: ["2", 0],
    vae: ["3", 0],
    prompt,
    negative_prompt: negative || "",
    resolution,
  };
  images.forEach((im, i) => {
    const load = String(20 + i);
    g[load] = { class_type: "LoadImage", inputs: { image: im } };
    let src = [load, 0];
    if (transparent) {
      const join = String(40 + i);
      g[join] = { class_type: "JoinImageWithAlpha", inputs: { image: [load, 0], alpha: [load, 1] } };
      src = [join, 0];
    }
    enc[`images.image_${i + 1}`] = src;
  });
  g["5"] = { class_type: "TextEncodeQwenImage21", inputs: enc };
  g["10"] = {
    class_type: "KSampler",
    inputs: {
      seed, steps: s.steps, cfg: s.cfg, sampler_name: sampler, scheduler, denoise: 1.0,
      model: ["4", 0], positive: ["5", 0], negative: ["5", 1], latent_image: ["5", 2],
    },
  };
  decodeAndSave(g, ["10", 0], transparent, filenamePrefix);
  return g;
}
