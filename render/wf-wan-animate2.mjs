// wf-wan-animate2.mjs — WAN-Animate-2 distilled CHARACTER ANIMATION (motion
// retargeting): a reference image of a character + a driver video of a person
// moving → the character performing the driver's motion.
//
// The T4 validation (2026-08-27, matched to the official template): identity-
// preserving retargeting of a human dance driver onto a vinyl-toy character,
// 81f @ 480×854, 10-step lcm, 298.5 s, loading NATIVE int8 on one 16 GB card
// (15.9 GB, no upcast). Genuinely new roster capability — nothing else does
// video-driven retargeting.
//
// Graph = the Motion Transfer subgraph of the official ComfyUI
// `video_wan_animate2_distilled` template (comfyui_workflow_templates_json
// 0.11.48), flattened to API format with the subgraph's own node ids, single
// chunk (81 frames). Deliberate deltas, each stated:
//   · CONTEXT-WINDOWS SWITCH FOLDED OFF — the demo's ContextWindowsManual path
//     only matters past one 81-frame chunk; single-chunk goes model-direct.
//   · CONTINUE-MOTION SWITCH FOLDED OFF — chunk-1 semantics; the demo's
//     second chunk + side-by-side Video Stitch comparison branch is dropped.
//   · POSE CACHE PINNED cpu/default — the template ships gpu/int8, which
//     hard-kills ComfyUI mid-step-1 on the reference box (silent process
//     death, no traceback; the harness watchdog then aborts at 240 s). See
//     the T4 closeout in docs/ROADMAP.md. Overridable, never the default.
//   · Model filenames are caller inputs (per-machine binding); defaults are
//     the reference box's on-disk names (`wan_2.1_vae` = the template's
//     `Wan2_1_VAE_bf16`, same model).

/** The template's own negative (standard Wan Chinese negative, applied verbatim). */
export const WAN_ANIMATE2_TEMPLATE_NEGATIVE =
  "色调艳丽，过曝，静态，细节模糊不清，字幕，风格，作品，画作，画面，静止，整体发灰，最差质量，低质量，JPEG压缩残留，丑陋的，残缺的，多余的手指，画得不好的手部，画得不好的脸部，畸形的，毁容的，形态畸形的肢体，手指融合，静止不动的画面，杂乱的背景，三条腿，背景人很多，倒着走";

export function buildWanAnimate2({
  refImagePath, driverVideoPath, prompt,
  motionPrompt = "A reference video of one person performing the motion to transfer, full body in frame.",
  negative = "",
  unet = "wan_animate_2_distill_int8_convrot.safetensors",
  textEncoder = "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
  clipVision = "clip_vision_h.safetensors",
  vae = "wan_2.1_vae.safetensors",
  width = 482, height = 854,            // the official template's portrait default
  length = 81,                          // one native chunk; the distilled recipe's unit
  steps = 10,                           // distilled: 10-step lcm cfg 1 shift 5 — the recipe
  seed = Math.floor(Math.random() * 1e15),
  poseStrength = 1.0, refStrength = 1.0,
  cacheDevice = "cpu", cacheDtype = "default",   // NEVER default to gpu/int8 (crash, see header)
  filenamePrefix = "render_wan_animate2",
} = {}) {
  if (!refImagePath) throw new Error("buildWanAnimate2: refImagePath is required");
  if (!driverVideoPath) throw new Error("buildWanAnimate2: driverVideoPath is required");
  if (!prompt) throw new Error("buildWanAnimate2: prompt (character/background description) is required");
  if (!Number.isFinite(length) || length <= 0) {
    throw new Error(`buildWanAnimate2: length must be positive, got ${length}`);
  }
  if (!negative) negative = WAN_ANIMATE2_TEMPLATE_NEGATIVE;

  // Node ids match the official subgraph (see header) so a diff against the
  // template stays mechanical.
  const g = {
    "239": { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "9": { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "wan", device: "default" } },
    "3": { class_type: "CLIPTextEncode", inputs: { clip: ["9", 0], text: prompt } },
    "4": { class_type: "CLIPTextEncode", inputs: { clip: ["9", 0], text: negative } },
    "222": { class_type: "CLIPTextEncode", inputs: { clip: ["9", 0], text: motionPrompt } },
    "75": { class_type: "CLIPVisionLoader", inputs: { clip_name: clipVision } },
    "7": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "189": { class_type: "LoadImage", inputs: { image: refImagePath } },
    "240": { class_type: "LoadVideo", inputs: { file: driverVideoPath } },
    "241": { class_type: "GetVideoComponents", inputs: { video: ["240", 0] } },
    // driver frames → working resolution; the ref image auto-matches via GetImageSize
    "243": { class_type: "ResizeImageMaskNode", inputs: { input: ["241", 0], resize_type: "scale dimensions", "resize_type.width": width, "resize_type.height": height, "resize_type.crop": "center", scale_method: "area" } },
    "256": { class_type: "GetImageSize", inputs: { image: ["243", 0] } },
    "244": { class_type: "ResizeImageMaskNode", inputs: { input: ["189", 0], resize_type: "scale dimensions", "resize_type.width": ["256", 0], "resize_type.height": ["256", 1], "resize_type.crop": "center", scale_method: "area" } },
    "76": { class_type: "CLIPVisionEncode", inputs: { clip_vision: ["75", 0], image: ["244", 0], crop: "none" } },
    "236": { class_type: "ImageFromBatch", inputs: { image: ["243", 0], batch_index: 0, length: 1 } },
    "220": { class_type: "CLIPVisionEncode", inputs: { clip_vision: ["75", 0], image: ["236", 0], crop: "none" } },
    "247": { class_type: "WanAnimate2ToVideo", inputs: {
      positive: ["3", 0], negative: ["4", 0], vae: ["7", 0],
      width: ["256", 0], height: ["256", 1], length, batch_size: 1,
      video_frame_offset: 0, pose_strength: poseStrength,
      pose_start_percent: 0.0, pose_end_percent: 1.0,
      reference_image_strength: refStrength,
      reference_image: ["244", 0], pose_video: ["243", 0],
      clip_vision_output: ["76", 0], positive_pose: ["222", 0],
      clip_vision_output_pose: ["220", 0],
    } },
    "224": { class_type: "WanAnimate2Cache", inputs: { model: ["239", 0], device: cacheDevice, dtype: cacheDtype } },
    "95": { class_type: "ModelSamplingSD3", inputs: { model: ["224", 0], shift: 5.0 } },
    "18": { class_type: "BasicScheduler", inputs: { model: ["224", 0], scheduler: "simple", steps, denoise: 1.0 } },
    "27": { class_type: "KSamplerSelect", inputs: { sampler_name: "lcm" } },
    "19": { class_type: "SamplerCustom", inputs: {
      model: ["95", 0], add_noise: true, noise_seed: seed, cfg: 1.0,
      positive: ["247", 0], negative: ["247", 1],
      sampler: ["27", 0], sigmas: ["18", 0], latent_image: ["247", 2],
    } },
    "223": { class_type: "TrimVideoLatent", inputs: { samples: ["19", 0], trim_amount: ["247", 3] } },
    "6": { class_type: "VAEDecode", inputs: { samples: ["223", 0], vae: ["7", 0] } },
    // mux keeps the driver's own audio + fps (GetVideoComponents outputs 1/2)
    "245": { class_type: "CreateVideo", inputs: { images: ["6", 0], audio: ["241", 1], fps: ["241", 2] } },
    "246": { class_type: "SaveVideo", inputs: { video: ["245", 0], filename_prefix: filenamePrefix, format: "auto", codec: "auto" } },
  };
  return g;
}
