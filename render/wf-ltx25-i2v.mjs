// wf-ltx25-i2v.mjs — LTX-2.5 22B distilled IMAGE-TO-VIDEO with JOINT AUDIO.
//
// The 32GB-class video seat, selected by the measured 2026-08-12 three-way
// (matched still/prompt/seed, idle GPU): LTX-2.5 distilled int8 beat Wan 2.2
// Q8 and MiniMax-H3 on every axis at once — 1920×1088 @ 24 fps WITH a jointly
// generated soundtrack in 247 s for a 5 s clip, vs Wan's 1280×720 @ 16 fps,
// silent, 1134 s (and Wan OOMs outright at its shipped vv=7.0). Bound as the
// `ltx25` family per the Seat Frontier plan Leg 3 (execution of that verdict).
//
// Graph = the official ComfyUI `video_ltx2_5_i2v` template, converted UI→API by
// ComfyUI's own frontend (app.graphToPrompt — the purpose-built converter; a
// hand conversion is where wiring bugs hide), with the bench-proven deltas:
//   · PROMPT-ENHANCER BRANCH DELETED (the template's gemma4_e2b
//     TextGenerateLTX2Prompt path, 10.28 GB of extra weights): the harness
//     caller already does better prompt expansion on llama-swap; the branch is
//     removed, not just switched off, so its weights are never touched.
//   · `conv` VIDEO VAE — the convrot int8 transformer pairs with the conv
//     video VAE file (filename swap through the generic VAELoader).
//   · Duration/dims computed HERE (seconds × fps + 1; stage-1 at half
//     resolution, /32-aligned) instead of the template's math-expression
//     nodes — same arithmetic, no autogrow-input serialization headaches.
//   · Caller-pinned seeds: stage-1 uses `seed`, the refine pass `seed + 1`
//     (the template randomises both, which makes comparisons meaningless).
//
// Two-stage recipe (both passes distilled: fixed ManualSigmas, dual CFG 1/1,
// euler_ancestral — do NOT swap in step-count/cfg knobs, the distillation is
// the recipe): base pass at half resolution with the still injected
// (LTXVImgToVideoInplace strength 0.7) and an empty audio latent jointly
// denoised; then LTXVLatentUpsampler ×2 → refine pass (strength 1.0, the
// 4-sigma tail) re-using the stage-1 AUDIO latent; tiled video decode + audio
// VAE decode → CreateVideo (AAC + h264 in one output).
//
// POOLED LOADING (house 32GB-pool doctrine): the 20 GB int8 transformer does
// not fit a single 16 GB card — when poolVvramGb > 0 it loads through
// ComfyUI-MultiGPU's UNETLoaderDisTorch2MultiGPU in RATIO mode, the shape
// proven for the krea2 image seat (expert-mode strings are a structural no-op
// on this node — see wf-krea2.mjs; pooled serving also requires the
// --disable-dynamic-vram launch flag until MultiGPU #191 lands upstream).
// poolVvramGb <= 0 loads the plain UNETLoader (single-big-card fleet tiers).

/** Distilled two-pass schedules from the official template (fixed, part of the recipe). */
export const LTX25_BASE_SIGMAS = "1.0, 0.99375, 0.9875, 0.98125, 0.975, 0.909375, 0.725, 0.421875, 0.0";
export const LTX25_REFINE_SIGMAS = "0.85, 0.7250, 0.4219, 0.0";
/** The template's own negative (applies at the conditioning level; dual CFG stays 1/1). */
export const LTX25_TEMPLATE_NEGATIVE = "pc game, console game, video game, cartoon, childish, ugly";

export function buildLtx25I2V({
  imagePath, prompt, negative = "",
  transformer = "ltx-2.5-22b-distilled-transformer-comfy-int8-convrot.safetensors",
  textEncoder = "gemma4-12b-with-proj-ltx-2.5-comfy-int8-convrot.safetensors",
  videoVae = "ltx-2.5-video-vae-conv-bf16.safetensors",
  audioVae = "ltx-2.5-audio-vae-bf16.safetensors",
  latentUpscaler = "ltx-2.5-latent-spatial-upscaler-x2-bf16-1.0.safetensors",
  width = 1920, height = 1088,          // final resolution; bench-proven flagship setting
  length = 0, seconds = 5, frameRate = 24,
  seed = Math.floor(Math.random() * 1e15),
  i2vStrengthBase = 0.7, i2vStrengthRefine = 1.0,
  imgCompression = 18, resizeLonger = 1536,
  poolVvramGb = 0, poolCompute = "cuda:0", poolDonor = "cuda:1",
  filenamePrefix = "render_ltx25",
} = {}) {
  if (!imagePath) throw new Error("buildLtx25I2V: imagePath is required");
  if (!prompt) throw new Error("buildLtx25I2V: prompt is required");
  if (!Number.isFinite(width) || width <= 0 || !Number.isFinite(height) || height <= 0) {
    throw new Error(`buildLtx25I2V: width/height must be positive, got ${width}x${height}`);
  }
  if (!Number.isFinite(poolVvramGb) || poolVvramGb < 0) {
    throw new Error(`buildLtx25I2V: poolVvramGb must be >= 0, got ${poolVvramGb}`);
  }
  if (!negative) negative = LTX25_TEMPLATE_NEGATIVE;
  // Template arithmetic, done here: final dims /32-aligned (ResolutionSelector
  // multiple=32); stage-1 runs at exactly half; length = seconds*fps + 1 unless
  // the caller passed an explicit frame count.
  width = Math.max(64, Math.floor(width / 32) * 32);
  height = Math.max(64, Math.floor(height / 32) * 32);
  if (!length || length <= 0) length = Math.round(seconds * frameRate) + 1;

  const loader = poolVvramGb > 0
    ? {
        class_type: "UNETLoaderDisTorch2MultiGPU",
        inputs: {
          unet_name: transformer, weight_dtype: "default",
          compute_device: poolCompute, virtual_vram_gb: poolVvramGb,
          donor_device: poolDonor,
          expert_mode_allocations: "",   // ratio mode ONLY (see wf-krea2.mjs header)
          eject_models: true,
        },
      }
    : { class_type: "UNETLoader", inputs: { unet_name: transformer, weight_dtype: "default" } };

  const g = {
    // inputs + preprocess (template: resize longer dim → LTXVPreprocess)
    "1": { class_type: "LoadImage", inputs: { image: imagePath } },
    "2": { class_type: "ResizeImageMaskNode", inputs: { resize_type: "scale longer dimension", "resize_type.longer_size": resizeLonger, scale_method: "lanczos", input: ["1", 0] } },
    "3": { class_type: "LTXVPreprocess", inputs: { img_compression: imgCompression, image: ["2", 0] } },
    // models
    "4": { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "ltxv", device: "default" } },
    "5": loader,
    "6": { class_type: "VAELoader", inputs: { vae_name: videoVae } },
    "7": { class_type: "VAELoader", inputs: { vae_name: audioVae } },
    "8": { class_type: "LatentUpscaleModelLoader", inputs: { model_name: latentUpscaler } },
    // conditioning (LTXVConditioning stamps the frame rate into both branches)
    "9": { class_type: "CLIPTextEncode", inputs: { text: prompt, clip: ["4", 0] } },
    "10": { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["4", 0] } },
    "11": { class_type: "LTXVConditioning", inputs: { frame_rate: frameRate, positive: ["9", 0], negative: ["10", 0] } },
    // ---- stage 1: base pass at half resolution, joint AV ----
    "12": { class_type: "EmptyLTXVLatentVideo", inputs: { width: width / 2, height: height / 2, length, batch_size: 1 } },
    "13": { class_type: "LTXVImgToVideoInplace", inputs: { strength: i2vStrengthBase, bypass: false, vae: ["6", 0], image: ["3", 0], latent: ["12", 0] } },
    "14": { class_type: "LTXVEmptyLatentAudio", inputs: { frames_number: length, frame_rate: frameRate, batch_size: 1, audio_vae: ["7", 0] } },
    "15": { class_type: "LTXVConcatAVLatent", inputs: { video_latent: ["13", 0], audio_latent: ["14", 0] } },
    "16": { class_type: "LTXVDualCFGGuider", inputs: { video_cfg: 1, audio_cfg: 1, model: ["5", 0], positive: ["11", 0], negative: ["11", 1] } },
    "17": { class_type: "KSamplerSelect", inputs: { sampler_name: "euler_ancestral" } },
    "18": { class_type: "RandomNoise", inputs: { noise_seed: seed } },
    "19": { class_type: "ManualSigmas", inputs: { sigmas: LTX25_BASE_SIGMAS } },
    "20": { class_type: "SamplerCustomAdvanced", inputs: { noise: ["18", 0], guider: ["16", 0], sampler: ["17", 0], sigmas: ["19", 0], latent_image: ["15", 0] } },
    "21": { class_type: "LTXVSeparateAVLatent", inputs: { av_latent: ["20", 0] } },
    // ---- stage 2: ×2 latent upscale + refine, re-using the stage-1 audio latent ----
    "22": { class_type: "LTXVLatentUpsampler", inputs: { samples: ["21", 0], upscale_model: ["8", 0], vae: ["6", 0] } },
    "23": { class_type: "LTXVImgToVideoInplace", inputs: { strength: i2vStrengthRefine, bypass: false, vae: ["6", 0], image: ["3", 0], latent: ["22", 0] } },
    "24": { class_type: "LTXVConcatAVLatent", inputs: { video_latent: ["23", 0], audio_latent: ["21", 1] } },
    "25": { class_type: "LTXVDualCFGGuider", inputs: { video_cfg: 1, audio_cfg: 1, model: ["5", 0], positive: ["11", 0], negative: ["11", 1] } },
    "26": { class_type: "KSamplerSelect", inputs: { sampler_name: "euler_ancestral" } },
    "27": { class_type: "RandomNoise", inputs: { noise_seed: seed + 1 } },
    "28": { class_type: "ManualSigmas", inputs: { sigmas: LTX25_REFINE_SIGMAS } },
    "29": { class_type: "SamplerCustomAdvanced", inputs: { noise: ["27", 0], guider: ["25", 0], sampler: ["26", 0], sigmas: ["28", 0], latent_image: ["24", 0] } },
    "30": { class_type: "LTXVSeparateAVLatent", inputs: { av_latent: ["29", 0] } },
    // ---- decode + mux (template tile params) ----
    "31": { class_type: "VAEDecodeTiled", inputs: { tile_size: 512, overlap: 64, temporal_size: 64, temporal_overlap: 16, samples: ["30", 0], vae: ["6", 0] } },
    "32": { class_type: "LTXVAudioVAEDecode", inputs: { samples: ["30", 1], audio_vae: ["7", 0] } },
    "33": { class_type: "CreateVideo", inputs: { fps: frameRate, bit_depth: 8, images: ["31", 0], audio: ["32", 0] } },
    "34": { class_type: "SaveVideo", inputs: { filename_prefix: filenamePrefix, format: "auto", codec: "auto", video: ["33", 0] } },
  };
  return g;
}
