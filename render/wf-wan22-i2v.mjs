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
// DECODE (decode; config videogen_wan_decode, helper flag --wan-decode): how the latent becomes frames.
//   • tiled (default): VAEDecodeTiled (tile 256/64, temporal 32/8), the node this graph always used, so a caller
//     that names no mode gets the graph exactly as it was before the key existed.
//   • plain: VAEDecode. One pass over the whole frame, so the VRAM peak follows the frame's resolution (ComfyUI
//     sizes this VAE's decode from height x width, with the frame count a mere step at 4 latent frames).
//   • auto (opt-in): plain when vramTotalBytes (the render card's total VRAM; the runner reads it from the
//     ComfyUI it submits to) is at least WAN_PLAIN_DECODE_MIN_VRAM_BYTES, else tiled. No reading = tiled.
//   Why the default is tiled and not auto: auto would run the plain decode by default on every 16 GB card, and no
//   render at the 16 GB tiers' own shape (1280x720x81) was ever run with plain. ComfyUI's own estimate there is
//   12.0 GiB of a 15.9 GiB card, and a second out-of-memory after ComfyUI's single tiled retry (below) fails a
//   ~70-minute render after sampling. The one measurement, on an RTX 5060 Ti 16 GB (A/B 2026-10-03): plain 38 s at a
//   10.3 GB peak against tiled 412 s at 3.2 GB (that A/B's tiled arm was one chunk), 45 dB PSNR between the two,
//   was taken with ComfyUI dynamic VRAM on and a clip of unrecorded shape. So auto and plain stay available as an
//   explicit opt-in, and the default flips to auto only after a live acceptance render at the tier shape shows a
//   ~40 s decode with no "Ran out of memory when regular VAE decoding" line in the ComfyUI log.
//   ComfyUI's VAE.decode catches an out-of-memory plain decode and retries it tiled (comfy/sd.py, read at
//   v0.38.0), which makes plain worth trying on a big card, but the retry is a second chance and not a
//   guarantee: it sits outside the try/except and can run out of memory too, and the harness runs ComfyUI with
//   --cache-none, so a failed decode is a failed render. Only THIS graph reads it: LTX 2.5 and Hunyuan 1.5
//   decode through other VAEs nobody measured and keep VAEDecodeTiled.

// Official Wan training-time negative (Wan-Video/Wan2.2 wan/configs/shared_config.py,
// sample_neg_prompt — the model is tuned against it; works with English positives).
export const WAN_OFFICIAL_NEGATIVE = "色调艳丽，过曝，静态，细节模糊不清，字幕，风格，作品，画作，画面，静止，整体发灰，最差质量，低质量，JPEG压缩残留，丑陋的，残缺的，多余的手指，画得不好的手部，画得不好的脸部，畸形的，毁容的，形态畸形的肢体，手指融合，静止不动的画面，杂乱的背景，三条腿，背景人很多，倒着走";

export const WAN_DECODE_MODES = Object.freeze(["auto", "plain", "tiled"]);
// tiled is today's graph; auto and plain are an explicit opt-in (see the DECODE note in the header).
export const WAN_DECODE_DEFAULT = "tiled";
// The smallest total VRAM at which "auto" runs the plain decode. 16 GB cards are the measured class (they
// report ~15.9 GiB); a card under 12 GiB was not measured and keeps the tiled decode. A nominal 12 GB card
// sits on the cut and was never read, so which side it lands on is not claimed: pin plain or tiled there.
export const WAN_PLAIN_DECODE_MIN_VRAM_BYTES = 12 * 1024 ** 3;

const gib = (bytes) => (bytes / 1024 ** 3).toFixed(1);

// chooseWanDecode is the one rule: which decode node a (mode, render-card VRAM) pair builds, and why in a
// sentence the runner can log. The builder calls it for the node and the runner calls it for the log, so
// the two cannot disagree. vramTotalBytes counts only as a finite positive number; anything else (not
// read, unreadable, a CPU device's RAM) is "no reading" and auto stays on the tiled decode.
export function chooseWanDecode(decode, vramTotalBytes) {
  if (!WAN_DECODE_MODES.includes(decode)) {
    throw new Error(`buildWan22I2V: decode must be ${WAN_DECODE_MODES.join("|")}, got ${JSON.stringify(decode)}`);
  }
  if (decode === "plain") return { node: "VAEDecode", why: "plain decode requested" };
  if (decode === "tiled") return { node: "VAEDecodeTiled", why: "tiled decode requested" };
  const min = `${gib(WAN_PLAIN_DECODE_MIN_VRAM_BYTES)} GiB`;
  if (typeof vramTotalBytes !== "number" || !Number.isFinite(vramTotalBytes) || vramTotalBytes <= 0) {
    return { node: "VAEDecodeTiled", why: "auto: the render card's VRAM was not read, so the tiled decode this graph always used stays" };
  }
  if (vramTotalBytes >= WAN_PLAIN_DECODE_MIN_VRAM_BYTES) {
    return { node: "VAEDecode", why: `auto: the render card reports ${gib(vramTotalBytes)} GiB of VRAM, at least the ${min} the plain decode wants` };
  }
  return { node: "VAEDecodeTiled", why: `auto: the render card reports ${gib(vramTotalBytes)} GiB of VRAM, under the ${min} the plain decode wants` };
}

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
  // loader (config: videogen_wan_loader, "" == "auto"): "auto" decides PER EXPERT
  // by file extension — a .gguf expert keeps the historical DisTorch2/MultiGPU
  // wrapper (ComfyUI's DynamicVRAM streaming does not support GGUF — bigger-models-
  // 2026-09-24.md), a .safetensors expert loads through the plain, un-wrapped
  // UNETLoader (no virtual_vram; ComfyUI's own dynamic-VRAM streaming does the
  // offload instead, measured ~27.6% faster wall-clock than the GGUF/DisTorch2
  // path on <node-b>, "Interim Phase 2 round 2" item 1). "native" FORCES the plain
  // loader on both experts (refused on a .gguf file — it cannot stream). "gguf-
  // distorch" FORCES the DisTorch2/MultiGPU wrapper on both experts regardless of
  // extension — the historical, always-worked behavior, still available for a
  // card too small for native streaming or a mixed-precision box.
  loader = "auto",
  // decode (config: videogen_wan_decode): "tiled" (the default) | "plain" | "auto", see the DECODE note in the header.
  // vramTotalBytes is the render card's total VRAM, which only "auto" reads; left undefined (a caller
  // that never looked at the card, preflight-graph.mjs) auto builds the tiled decode, as before.
  decode = WAN_DECODE_DEFAULT, vramTotalBytes,
} = {}) {
  if (!imagePath) throw new Error("buildWan22I2V: imagePath is required");
  if (!prompt) throw new Error("buildWan22I2V: prompt is required");
  if (!["auto", "native", "gguf-distorch"].includes(loader)) {
    throw new Error(`buildWan22I2V: loader must be auto|native|gguf-distorch, got ${JSON.stringify(loader)}`);
  }
  const decodeNode = chooseWanDecode(decode, vramTotalBytes).node; // also refuses an unknown decode
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
  // Loader keyed off the weight file extension (quality-first weight binding), per
  // expert, gated by `loader`. J4 seam: the compute device is env-overridable
  // (COMFY_COMPUTE_DEVICE) — the hardcoded cuda:0 assumed an NVIDIA box; non-CUDA
  // ComfyUI backends name their devices differently. Default unchanged.
  const computeDevice = process.env.COMFY_COMPUTE_DEVICE || "cuda:0";
  const distorch = (unet) => /\.gguf$/i.test(unet)
    ? { class_type: "UnetLoaderGGUFDisTorch2MultiGPU", inputs: { unet_name: unet, compute_device: computeDevice, virtual_vram_gb: virtualVramGb, donor_device: "cpu", eject_models: true } }
    : { class_type: "UNETLoaderDisTorch2MultiGPU", inputs: { unet_name: unet, weight_dtype: "default", compute_device: computeDevice, virtual_vram_gb: virtualVramGb, donor_device: "cpu", eject_models: true } };
  // native: the core, un-wrapped loader — no DisTorch2, no virtual_vram, no
  // MultiGPU dependency. ComfyUI's own dynamic-VRAM streaming (comfy_dynamic_vram
  // on the launch profile, not a graph input) does the RAM/VRAM offload instead.
  const native = (unet) => ({ class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } });
  const loaderFor = (unet) => {
    const isGguf = /\.gguf$/i.test(unet);
    if (loader === "native") {
      if (isGguf) {
        throw new Error(`buildWan22I2V: loader:"native" cannot load a GGUF expert (${unet}) — ComfyUI's DynamicVRAM streaming does not support GGUF; use loader:"gguf-distorch" (or "auto") for a GGUF-bound box`);
      }
      return native(unet);
    }
    if (loader === "gguf-distorch") return distorch(unet);
    // auto
    return isGguf ? distorch(unet) : native(unet);
  };

  const g = {
    "1": { class_type: "LoadImage", inputs: { image: imagePath } },
    "2": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "3": { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "wan" } },
    "4": { class_type: "CLIPTextEncode", inputs: { clip: ["3", 0], text: prompt } },
    "5": { class_type: "CLIPTextEncode", inputs: { clip: ["3", 0], text: negative } },
    "6": { class_type: "WanImageToVideo", inputs: { positive: ["4", 0], negative: ["5", 0], vae: ["2", 0], width, height, length, batch_size: 1, start_image: ["1", 0] } },
    "7": loaderFor(highUnet),
    "9": loaderFor(lowUnet),
    // model-sampling reads the LoRA output (fast) or the raw UNET (hero)
    "8": { class_type: "ModelSamplingSD3", inputs: { model: useLora ? ["15", 0] : ["7", 0], shift } },
    "10": { class_type: "ModelSamplingSD3", inputs: { model: useLora ? ["16", 0] : ["9", 0], shift } },
    "11": { class_type: "KSamplerAdvanced", inputs: { model: ["8", 0], add_noise: "enable", noise_seed: seed, steps, cfg: highCfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["6", 2], start_at_step: 0, end_at_step: boundaryStep, return_with_leftover_noise: "enable" } },
    "12": { class_type: "KSamplerAdvanced", inputs: { model: ["10", 0], add_noise: "disable", noise_seed: seed, steps, cfg: lowCfg, sampler_name: "euler", scheduler: "simple", positive: ["6", 0], negative: ["6", 1], latent_image: ["11", 0], start_at_step: boundaryStep, end_at_step: 10000, return_with_leftover_noise: "disable" } },
    // Either decode keeps id 13: the upscale chain and the combine below read ["13", 0] whichever ran.
    "13": decodeNode === "VAEDecode"
      ? { class_type: "VAEDecode", inputs: { samples: ["12", 0], vae: ["2", 0] } }
      : { class_type: "VAEDecodeTiled", inputs: { samples: ["12", 0], vae: ["2", 0], tile_size: 256, overlap: 64, temporal_size: 32, temporal_overlap: 8 } },
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
