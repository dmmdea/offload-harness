// wf-wan22-i2v.mjs — build the Wan 2.2 14B I2V two-stage API-format graph.
// Two quality modes, both universal (any machine, param-driven — never hardware-baked):
//   • FAST (default): the 4-step lightx2v LoRAs — HIGH on the high-noise expert, LOW on
//     the low-noise expert (LoraLoaderModelOnly) + ModelSamplingSD3 shift 5; two
//     KSamplerAdvanced (2/2 split of 4 steps, leftover-noise handoff, SAME seed, cfg
//     1.0 = the distilled recipe). ~2-4 min/clip.
//   • HERO (hero:true): the native two-stage path — NO distill LoRA, more steps (20) at
//     cfg 3.5. Slower but restores camera/subject motion the 4-step LoRA trades away
//     (matters for realistic b-roll). The caller can still override steps/cfg.
// Optional post-decode UPSCALE (upscaleModel set): UpscaleModelLoader → ImageUpscaleWithModel
//   → optional ImageScale to a target size (e.g. 720p→1080p). The upscale model is a
//   caller/config input, never hardcoded — a machine that has no upscale model leaves it "".
// umt5 text encoder (type "wan"); the 16-ch Wan 2.1 VAE (the 14B A14B I2V wants 36-ch
// patch_embed input; the 48-ch wan2.2_vae is for the 5B TI2V and mismatches). Run only
// with the GPU freed of llama-swap.
export function buildWan22I2V({
  imagePath, prompt, negative = "",
  width = 832, height = 480, length = 81, steps = 4, cfg = 1.0, seed = Math.floor(Math.random() * 1e15),
  shift = 5.0, virtualVramGb = 7.0, boundaryStep,
  highUnet = "wan2.2_i2v_high_noise_14B_Q4_K_S.gguf",
  lowUnet = "wan2.2_i2v_low_noise_14B_Q4_K_M.gguf",
  highLora = "Wan_2_2_I2V_A14B_HIGH_lightx2v_4step_lora_260412_rank_64_fp16.safetensors",
  lowLora = "Wan_2_2_I2V_A14B_LOW_lightx2v_4step_lora_260412_rank_64_fp16.safetensors",
  loraStrength = 1.0,
  hero = false,
  upscaleModel = "", upscaleWidth = 0, upscaleHeight = 0, upscaleMethod = "lanczos",
  textEncoder = "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
  vae = "wan_2.1_vae.safetensors", frameRate = 16,
} = {}) {
  if (!imagePath) throw new Error("buildWan22I2V: imagePath is required");
  if (!prompt) throw new Error("buildWan22I2V: prompt is required");
  if (steps === undefined) steps = 4;
  // Hero = native two-stage: bump the fast defaults to native values unless the caller
  // set them explicitly (steps 4 / cfg 1.0 ARE the fast defaults, so treat them as unset).
  if (hero) {
    if (steps === 4) steps = 20;
    if (cfg === 1.0) cfg = 3.5;
  }
  if (boundaryStep === undefined) boundaryStep = Math.floor(steps / 2);
  const useLora = !hero;
  const distorch = (unet) => ({ class_type: "UnetLoaderGGUFDisTorch2MultiGPU", inputs: { unet_name: unet, compute_device: "cuda:0", virtual_vram_gb: virtualVramGb, donor_device: "cpu", eject_models: true } });

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
    "11": { class_type: "KSamplerAdvanced", inputs: { model: ["8", 0], add_noise: "enable", noise_seed: seed, steps, cfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["6", 2], start_at_step: 0, end_at_step: boundaryStep, return_with_leftover_noise: "enable" } },
    "12": { class_type: "KSamplerAdvanced", inputs: { model: ["10", 0], add_noise: "disable", noise_seed: seed, steps, cfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["11", 0], start_at_step: boundaryStep, end_at_step: 10000, return_with_leftover_noise: "disable" } },
    "13": { class_type: "VAEDecodeTiled", inputs: { samples: ["12", 0], vae: ["2", 0], tile_size: 256, overlap: 64, temporal_size: 32, temporal_overlap: 8 } },
  };
  if (useLora) {
    g["15"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["7", 0], lora_name: highLora, strength_model: loraStrength } };
    g["16"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["9", 0], lora_name: lowLora, strength_model: loraStrength } };
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
