// wf-hidream-o1.mjs — build the HiDream-O1-Image API-format graph, matching the OFFICIAL
// Comfy-Org workflow templates exactly (verified against the raw template JSONs + the
// installed node sources, 2026-07-16 — see docs/superpowers/specs/2026-07-16-quality-first-
// generation-design.md for the source trail).
//
// Why a dedicated graph (the generic SDXL-shaped KSampler graph is WRONG for O1):
//   * O1 is an 8B PIXEL-SPACE DiT: EmptyHiDreamO1LatentImage produces its (B,3,H,W)
//     pixel latent — the SD 4-channel EmptyLatentImage is the wrong format. Its node
//     docstring: trained at ~4MP; LOWER RESOLUTIONS GO OFF-DISTRIBUTION and quality
//     regresses noticeably. Default/native: 2048x2048. Dims snap to /32.
//   * ModelNoiseScale sets the model's training-time noise scale (base 8.0 / dev 7.6
//     per the official templates) — required for the stochastic samplers both official
//     recipes use.
//   * HiDreamO1PatchSeamSmoothing (base only) blends shifted patch-grid passes in the
//     late sampling phase (PATCH_SIZE=32) — this kills the 32px patch blocking measured
//     on the generic graph (grid_excess ~2-3x at exactly 32px pitch).
//   * The official sampling stack is SamplerCustom + KSamplerSelect/SamplerLCM +
//     BasicScheduler — not a plain KSampler.
// Official recipes (template widgets, verified):
//   base: 40 steps, cfg 5, dpmpp_2m_sde_gpu, scheduler normal, noise_scale 8.0,
//         seam smoothing [start 0.8, end 1, single_shift, ramp_2_4, median, strength 1];
//         negative prompt ACTIVE at cfg 5.
//   dev:  28 steps, cfg 1 (negative inert — distilled), SamplerLCM [1, 1, 2.5],
//         scheduler normal, noise_scale 7.6, NO seam smoothing.
export function buildHiDreamO1({
  prompt, negative = "", ckpt,
  variant = "base",              // base | dev
  width = 2048, height = 2048,   // native trained resolution (see node docstring table)
  steps = 0, cfg = 0,            // 0 = the variant's official default
  sampler = "",                  // base only; "" = dpmpp_2m_sde_gpu
  scheduler = "normal",
  seed = Math.floor(Math.random() * 1e15),
  seamSmoothing = true,          // base only; dev has none in the official graph
} = {}) {
  if (!prompt) throw new Error("buildHiDreamO1: prompt is required");
  if (!ckpt) throw new Error("buildHiDreamO1: ckpt is required");
  if (variant !== "base" && variant !== "dev") throw new Error(`buildHiDreamO1: unknown variant '${variant}' (base|dev)`);
  const dev = variant === "dev";
  if (!steps) steps = dev ? 28 : 40;
  if (!cfg) cfg = dev ? 1 : 5;
  const noiseScale = dev ? 7.6 : 8.0;
  // O1's pixel-space patch grid: dims must be multiples of 32 (the official edit path
  // uses floor(a/32)*32; the latent node steps by 32).
  width = Math.max(64, Math.floor(width / 32) * 32);
  height = Math.max(64, Math.floor(height / 32) * 32);

  const g = {
    "4": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: ckpt } },
    "20": { class_type: "ModelNoiseScale", inputs: { model: ["4", 0], noise_scale: noiseScale } },
    "22": { class_type: "BasicScheduler", inputs: { model: ["20", 0], scheduler, steps, denoise: 1.0 } },
    "5": { class_type: "EmptyHiDreamO1LatentImage", inputs: { width, height, batch_size: 1 } },
    "6": { class_type: "CLIPTextEncode", inputs: { text: prompt, clip: ["4", 1] } },
    "7": { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["4", 1] } },
    "8": { class_type: "VAEDecode", inputs: { samples: ["3", 0], vae: ["4", 2] } },
    "9": { class_type: "SaveImage", inputs: { filename_prefix: "render", images: ["8", 0] } },
  };
  // Sampler object: base = KSamplerSelect(dpmpp_2m_sde_gpu); dev = SamplerLCM(1, 1, 2.5)
  // (s_noise, s_noise_end, noise_clip_std — the official dev template's widgets).
  if (dev) {
    g["23"] = { class_type: "SamplerLCM", inputs: { s_noise: 1.0, s_noise_end: 1.0, noise_clip_std: 2.5 } };
  } else {
    g["23"] = { class_type: "KSamplerSelect", inputs: { sampler_name: sampler || "dpmpp_2m_sde_gpu" } };
  }
  // MODEL path into SamplerCustom: base routes through the seam-smoothing wrapper
  // (which reads the noise-scaled model); dev feeds the noise-scaled model directly.
  let modelSrc = ["20", 0];
  if (!dev && seamSmoothing) {
    g["21"] = { class_type: "HiDreamO1PatchSeamSmoothing", inputs: {
      model: ["20", 0], start_percent: 0.8, end_percent: 1.0,
      pattern: "single_shift", passes: "ramp_2_4", blend: "median", strength: 1.0,
    } };
    modelSrc = ["21", 0];
  }
  g["3"] = { class_type: "SamplerCustom", inputs: {
    model: modelSrc, add_noise: true, noise_seed: seed, cfg,
    positive: ["6", 0], negative: ["7", 0],
    sampler: ["23", 0], sigmas: ["22", 0], latent_image: ["5", 0],
  } };
  return g;
}
