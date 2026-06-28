// wf-hunyuan15-i2v.mjs — build the native HunyuanVideo 1.5 480p I2V API-format graph.
// PRIMARY 8GB path. Wiring RECONCILED against the LIVE ComfyUI 1.5 node schema + the
// official 1.5 I2V template (workflow wso07xgs5 + adversarial verify, 2026-06-16), which
// corrected several research-era guesses — each with source evidence:
//   - DualCLIPLoader type MUST be "hunyuan_video_15" (sd.py:1662). "hunyuan_video" loads
//     the legacy llama encoder; the qwen2.5-vl + byt5 files on disk are the 1.5 recipe.
//   - The positive prompt uses a PLAIN CLIPTextEncode, NOT TextEncodeHunyuanVideo_ImageToVideo
//     (nodes_hunyuan.py:300 — that's the legacy March-2025 llama encoder, incompatible with
//     the 1.5 Qwen tokenizer; qwen_image.py:20 has no image_embeds path). sigclip embeds feed
//     the I2V node directly via clip_vision_output (node 9).
//   - HunyuanVideo15ImageToVideo REQUIRES batch_size (else prompt validation fails at submit).
//   - cfg_distilled-Q4_K_S distills away the CFG (negative) pass, NOT the step budget:
//     cfg=1, steps=50, shift=5 (no step_distilled GGUF on disk; 12 steps = garbage).
//   - ModelSamplingSD3 shift 5 before sampling (the distilled flow is tuned for it).
// All five models verified present on disk. Output mp4 via VHS_VideoCombine (under node.gifs).
export function buildHunyuan15I2V({
  imagePath, prompt, negative = "",
  width = 848, height = 480, length = 33, steps = 50, cfg = 1, shift = 5,
  seed = Math.floor(Math.random() * 1e15), batchSize = 1,
  // VAE decode tiling. temporal_size < frame count chunks the decode temporally — the key
  // 8GB lever: 4096 (decode-all-at-once, best quality) HARD-CRASHED the display driver on
  // an 8GB 3070 at 33 frames; 16 decodes in temporal chunks and fits. Raise on bigger GPUs.
  vaeTileSize = 256, vaeTemporalSize = 16,
  unet = "hunyuanvideo1.5_480p_i2v_cfg_distilled-Q4_K_S.gguf",
  vae = "hunyuanvideo15_vae_fp16.safetensors",
  clipVision = "sigclip_vision_patch14_384.safetensors",
  textEncoder = "qwen_2.5_vl_7b_fp8_scaled.safetensors",
  glyphEncoder = "byt5_small_glyphxl_fp16.safetensors",
  frameRate = 24,
} = {}) {
  if (!imagePath) throw new Error("buildHunyuan15I2V: imagePath is required");
  if (!prompt) throw new Error("buildHunyuan15I2V: prompt is required");
  if (steps === undefined) steps = 50;
  return {
    "1": { class_type: "LoadImage", inputs: { image: imagePath } },
    "2": { class_type: "UnetLoaderGGUF", inputs: { unet_name: unet } },
    "3": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "4": { class_type: "CLIPVisionLoader", inputs: { clip_name: clipVision } },
    "5": { class_type: "CLIPVisionEncode", inputs: { clip_vision: ["4", 0], image: ["1", 0], crop: "center" } },
    // DualCLIPLoader loads the Qwen2.5-VL LLM encoder + ByT5 glyph encoder for the 1.5 recipe.
    "6": { class_type: "DualCLIPLoader", inputs: { clip_name1: textEncoder, clip_name2: glyphEncoder, type: "hunyuan_video_15" } },
    // 1.5 uses a plain CLIPTextEncode for BOTH prompts; sigclip feeds the I2V node (9), not here.
    "7": { class_type: "CLIPTextEncode", inputs: { clip: ["6", 0], text: prompt } },
    "8": { class_type: "CLIPTextEncode", inputs: { clip: ["6", 0], text: negative } },
    "9": { class_type: "HunyuanVideo15ImageToVideo", inputs: { positive: ["7", 0], negative: ["8", 0], vae: ["3", 0], clip_vision_output: ["5", 0], start_image: ["1", 0], width, height, length, batch_size: batchSize } },
    // distilled flow is tuned for an SD3 shift; apply to the model before sampling.
    "13": { class_type: "ModelSamplingSD3", inputs: { model: ["2", 0], shift } },
    "10": { class_type: "KSampler", inputs: { seed, steps, cfg, sampler_name: "euler", scheduler: "simple", denoise: 1, model: ["13", 0], positive: ["9", 0], negative: ["9", 1], latent_image: ["9", 2] } },
    "11": { class_type: "VAEDecodeTiled", inputs: { samples: ["10", 0], vae: ["3", 0], tile_size: vaeTileSize, overlap: 64, temporal_size: vaeTemporalSize, temporal_overlap: 8 } },
    "12": { class_type: "VHS_VideoCombine", inputs: { images: ["11", 0], frame_rate: frameRate, loop_count: 0, filename_prefix: "render_i2v", format: "video/h264-mp4", pingpong: false, save_output: true } },
  };
}
