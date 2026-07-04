// wf-wan22-i2v.mjs — build the native Wan 2.2 14B I2V two-stage API-format graph.
// SECONDARY photoreal path (research 2026-06-16): high-noise Q4_K_S then low-noise
// Q4_K_M GGUF, each via UnetLoaderGGUFDisTorch2MultiGPU (RAM-offload, ~6-7GB virtual)
// + ModelSamplingSD3 shift 5; two KSamplerAdvanced (10/10 split, leftover-noise
// handoff, SAME seed); umt5 text encoder (type "wan"); clip_vision NOT used in 2.2.
// Run only with the GPU fully freed of llama-swap. ~12-25 min/clip native.
export function buildWan22I2V({
  imagePath, prompt, negative = "",
  width = 832, height = 480, length = 81, steps = 20, cfg = 3.5, seed = Math.floor(Math.random() * 1e15),
  shift = 5.0, virtualVramGb = 7.0, boundaryStep,
  highUnet = "wan2.2_i2v_high_noise_14B_Q4_K_S.gguf",
  lowUnet = "wan2.2_i2v_low_noise_14B_Q4_K_M.gguf",
  textEncoder = "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
  vae = "wan2.2_vae.safetensors", frameRate = 16,
} = {}) {
  if (!imagePath) throw new Error("buildWan22I2V: imagePath is required");
  if (!prompt) throw new Error("buildWan22I2V: prompt is required");
  if (steps === undefined) steps = 20;
  if (boundaryStep === undefined) boundaryStep = Math.floor(steps / 2);
  const distorch = (unet) => ({ class_type: "UnetLoaderGGUFDisTorch2MultiGPU", inputs: { unet_name: unet, compute_device: "cuda:0", virtual_vram_gb: virtualVramGb, donor_device: "cpu", eject_models: true } });
  return {
    "1": { class_type: "LoadImage", inputs: { image: imagePath } },
    "2": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "3": { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "wan" } },
    "4": { class_type: "CLIPTextEncode", inputs: { clip: ["3", 0], text: prompt } },
    "5": { class_type: "CLIPTextEncode", inputs: { clip: ["3", 0], text: negative } },
    "6": { class_type: "WanImageToVideo", inputs: { positive: ["4", 0], negative: ["5", 0], vae: ["2", 0], width, height, length, batch_size: 1, start_image: ["1", 0] } },
    "7": distorch(highUnet),
    "8": { class_type: "ModelSamplingSD3", inputs: { model: ["7", 0], shift } },
    "9": distorch(lowUnet),
    "10": { class_type: "ModelSamplingSD3", inputs: { model: ["9", 0], shift } },
    "11": { class_type: "KSamplerAdvanced", inputs: { model: ["8", 0], add_noise: "enable", noise_seed: seed, steps, cfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["6", 2], start_at_step: 0, end_at_step: boundaryStep, return_with_leftover_noise: "enable" } },
    "12": { class_type: "KSamplerAdvanced", inputs: { model: ["10", 0], add_noise: "disable", noise_seed: seed, steps, cfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["11", 0], start_at_step: boundaryStep, end_at_step: 10000, return_with_leftover_noise: "disable" } },
    "13": { class_type: "VAEDecodeTiled", inputs: { samples: ["12", 0], vae: ["2", 0], tile_size: 256, overlap: 64, temporal_size: 32, temporal_overlap: 8 } },
    "14": { class_type: "VHS_VideoCombine", inputs: { images: ["13", 0], frame_rate: frameRate, loop_count: 0, filename_prefix: "render_wan", format: "video/h264-mp4", pingpong: false, save_output: true } },
  };
}
