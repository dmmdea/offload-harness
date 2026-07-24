// wf-wan22-i2v.mjs — build the Wan 2.2 14B I2V two-stage API-format graph.
// Two modes, both universal (any machine, param-driven — never hardware-baked).
// QUALITY-FIRST (operator directive, 2026-07-16): the NATIVE path is the DEFAULT; the distilled
// speed path is an explicit opt-in — never the default.
//   • NATIVE (default; was "hero"): the official two-stage recipe — NO distill LoRA,
//     20 steps at cfg 3.5, euler/simple, shift 5.0, 50/50 expert split (ComfyUI
//     official template; Wan-AI reference = 40 steps unipc, cfg 3.5, boundary 0.9 —
//     pass steps:40 to match it). The model's official training-time Chinese
//     negative is applied when the caller provides none.
//   • FAST (fast:true): the lightx2v distill LoRAs at the research-validated 8-step
//     asymmetric recipe — 4+4 split, HIGH expert LoRA 0.7 + cfg 3.0 (keeps real
//     guidance where motion is decided), LOW expert LoRA 1.0 + cfg 1.0 (distilled).
//     Recovers most of the motion the plain 4-step cfg-1 recipe trades away.
//   (hero:true is accepted for backward compatibility and equals the default.)
// Optional post-decode UPSCALE (upscaleModel set): UpscaleModelLoader → ImageUpscaleWithModel
//   → optional ImageScale to a target size (e.g. 720p→1080p). The upscale model is a
//   caller/config input, never hardcoded — a machine that has no upscale model leaves it "".
// umt5 text encoder (type "wan"); the 16-ch Wan 2.1 VAE (the 14B A14B I2V wants 36-ch
// patch_embed input; the 48-ch wan2.2_vae is for the 5B TI2V and mismatches). Run only
// with the GPU freed of llama-swap.

// Official Wan training-time negative (Wan-Video/Wan2.2 wan/configs/shared_config.py,
// sample_neg_prompt — the model is tuned against it; works with English positives).
export const WAN_OFFICIAL_NEGATIVE = "色调艳丽，过曝，静态，细节模糊不清，字幕，风格，作品，画作，画面，静止，整体发灰，最差质量，低质量，JPEG压缩残留，丑陋的，残缺的，多余的手指，画得不好的手部，画得不好的脸部，畸形的，毁容的，形态畸形的肢体，手指融合，静止不动的画面，杂乱的背景，三条腿，背景人很多，倒着走";

export function buildWan22I2V({
  imagePath, prompt, negative = "",
  width = 832, height = 480, length = 81, steps = 0, cfg = 0, seed = Math.floor(Math.random() * 1e15),
  shift = 5.0, virtualVramGb = 7.0, boundaryStep,
  highUnet = "wan2.2_i2v_high_noise_14B_Q4_K_S.gguf",
  lowUnet = "wan2.2_i2v_low_noise_14B_Q4_K_M.gguf",
  highLora = "Wan_2_2_I2V_A14B_HIGH_lightx2v_4step_lora_260412_rank_64_fp16.safetensors",
  lowLora = "Wan_2_2_I2V_A14B_LOW_lightx2v_4step_lora_260412_rank_64_fp16.safetensors",
  fast = false,
  hero = false, // backward compat; native IS the default now
  upscaleModel = "", upscaleWidth = 0, upscaleHeight = 0, upscaleMethod = "lanczos",
  textEncoder = "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
  vae = "wan_2.1_vae.safetensors", frameRate = 16,
} = {}) {
  if (!imagePath) throw new Error("buildWan22I2V: imagePath is required");
  if (!prompt) throw new Error("buildWan22I2V: prompt is required");
  void hero; // accepted, ignored: the native path is the default
  const useLora = !!fast;
  if (!negative) negative = WAN_OFFICIAL_NEGATIVE;
  // Mode defaults (0 = unset; explicit caller values always win):
  //   native: 20 steps, cfg 3.5 both experts.
  //   fast:   8 steps (4+4), cfg 3.0 high / 1.0 low, LoRA 0.7 high / 1.0 low.
  if (!steps) steps = useLora ? 8 : 20;
  const highCfg = cfg || (useLora ? 3.0 : 3.5);
  const lowCfg = cfg || (useLora ? 1.0 : 3.5);
  const highLoraStrength = 0.7, lowLoraStrength = 1.0;
  if (boundaryStep === undefined) boundaryStep = Math.floor(steps / 2);
  // Loader keyed off the weight file extension (quality-first weight binding): GGUF quants
  // load through the GGUF DisTorch2 node; full/fp8 .safetensors weights through the native
  // UNETLoader DisTorch2 variant — SAME RAM-offload params, weight_dtype "default" (never
  // down-cast: the file's own precision is the point of binding it).
  // J4 seam: the compute device is env-overridable (COMFY_COMPUTE_DEVICE) — the
  // hardcoded cuda:0 assumed an NVIDIA box; non-CUDA ComfyUI backends name their
  // devices differently. Default unchanged.
  const computeDevice = process.env.COMFY_COMPUTE_DEVICE || "cuda:0";
  const distorch = (unet) => /\.gguf$/i.test(unet)
    ? { class_type: "UnetLoaderGGUFDisTorch2MultiGPU", inputs: { unet_name: unet, compute_device: computeDevice, virtual_vram_gb: virtualVramGb, donor_device: "cpu", eject_models: true } }
    : { class_type: "UNETLoaderDisTorch2MultiGPU", inputs: { unet_name: unet, weight_dtype: "default", compute_device: computeDevice, virtual_vram_gb: virtualVramGb, donor_device: "cpu", eject_models: true } };

  const g = {
    "1": { class_type: "LoadImage", inputs: { image: imagePath } },
    "2": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "3": { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "wan" } },
    "4": { class_type: "CLIPTextEncode", inputs: { clip: ["3", 0], text: prompt } },
    "5": { class_type: "CLIPTextEncode", inputs: { clip: ["3", 0], text: negative } },
    "6": { class_type: "WanImageToVideo", inputs: { positive: ["4", 0], negative: ["5", 0], vae: ["2", 0], width, height, length, batch_size: 1, start_image: ["1", 0] } },
    "7": distorch(highUnet),
    "9": distorch(lowUnet),
    // model-sampling reads the LoRA output (fast) or the raw UNET (hero)
    "8": { class_type: "ModelSamplingSD3", inputs: { model: useLora ? ["15", 0] : ["7", 0], shift } },
    "10": { class_type: "ModelSamplingSD3", inputs: { model: useLora ? ["16", 0] : ["9", 0], shift } },
    "11": { class_type: "KSamplerAdvanced", inputs: { model: ["8", 0], add_noise: "enable", noise_seed: seed, steps, cfg: highCfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["6", 2], start_at_step: 0, end_at_step: boundaryStep, return_with_leftover_noise: "enable" } },
    "12": { class_type: "KSamplerAdvanced", inputs: { model: ["10", 0], add_noise: "disable", noise_seed: seed, steps, cfg: lowCfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["11", 0], start_at_step: boundaryStep, end_at_step: 10000, return_with_leftover_noise: "disable" } },
    "13": { class_type: "VAEDecodeTiled", inputs: { samples: ["12", 0], vae: ["2", 0], tile_size: 256, overlap: 64, temporal_size: 32, temporal_overlap: 8 } },
  };
  if (useLora) {
    g["15"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["7", 0], lora_name: highLora, strength_model: highLoraStrength } };
    g["16"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["9", 0], lora_name: lowLora, strength_model: lowLoraStrength } };
  }
  // Optional upscale chain after the decoded frames. Model-based upscale (e.g. an ESRGAN
  // 4x) then an optional exact resize to the target size (720p -> 1080p).
  let imageNode = "13";
  if (upscaleModel) {
    g["17"] = { class_type: "UpscaleModelLoader", inputs: { model_name: upscaleModel } };
    g["18"] = { class_type: "ImageUpscaleWithModel", inputs: { upscale_model: ["17", 0], image: ["13", 0] } };
    imageNode = "18";
    if (upscaleWidth > 0 && upscaleHeight > 0) {
      g["19"] = { class_type: "ImageScale", inputs: { image: [imageNode, 0], width: upscaleWidth, height: upscaleHeight, upscale_method: upscaleMethod, crop: "disabled" } };
      imageNode = "19";
    }
  }
  g["14"] = { class_type: "VHS_VideoCombine", inputs: { images: [imageNode, 0], frame_rate: frameRate, loop_count: 0, filename_prefix: "render_wan", format: "video/h264-mp4", pingpong: false, save_output: true } };
  return g;
}
