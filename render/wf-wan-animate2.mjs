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
//
// CARD-ONLY PLACEMENT (multicard: { residentGb }; comfy-animate.mjs --placement multicard, planned by render/wan-multicard.mjs planMulticard): the distilled
// int8 DiT is 15.5 GiB, which a 16 GB card (about 15 GiB) cannot hold with the sampler's activations, and a node that runs ComfyUI with --gpu-only
// (the whole model must land on a card, no host-RAM streaming) cannot run the single-card graph at all. This variant splits the one DiT over the
// lease's two non-display cards through ComfyUI-MultiGPU's DisTorch2 loader and keeps the host out of it. The devices are the compute-first pair the
// LTX-2.5 graph uses (wf-ltx25-i2v.mjs): cuda:0 is the COMPUTE card and cuda:1 the STORAGE card, because this is the same int8 convrot kind of
// DiT and an int8 DiT that computes on any device but cuda:0 faults (ComfyUI-MultiGPU #220); the launch lists the compute card first.
//   · DiT: UNETLoaderDisTorch2MultiGPU, `residentGb` GiB of it on the compute card and the rest (the donor blocks) on the storage card, never system RAM;
//   · umt5 text encoder (6.27 GiB) and clip_vision_h (1.18 GiB): CLIPLoaderMultiGPU / CLIPVisionLoaderMultiGPU on the storage card, so the encode runs
//     there before the DiT lands. OffloadFreeAfterEncode (render/comfyui-ext) is wired AFTER WanAnimate2ToVideo (node 247), which is the node that
//     consumes all three prompt encodes (positive, negative and the motion prompt) and both clip-vision encodes, so every use of the encoder is done
//     when it takes the encoder off the cards; the sampler reads the conditioning through it, so the DiT is placed only afterwards. (clip_vision_h
//     stays where it is: a smaller part, counted in the storage card's stage.)
//   · wan_2.1_vae: VAELoaderMultiGPU on the storage card; OffloadFreeForDecode (check off) frees the cards of the DiT between the trim and the decode.
// The single graph is unchanged byte for byte when multicard is absent (render/testdata/builder-drift.json is read from that graph).

/** The card-only placement's devices, as ComfyUI numbers them under the compute-first launch (wan-multicard.mjs): cuda:0 computes, cuda:1 stores. */
export const ANIMATE_COMPUTE_DEVICE = "cuda:0";
export const ANIMATE_STORAGE_DEVICE = "cuda:1";
/** GiB of the DiT kept on the compute card by default (the storage card holds the rest). The arithmetic is wan-multicard.mjs's, ANIMATE_* there. */
export const ANIMATE_RESIDENT_GB_DEFAULT = 5;
/** The official template's portrait working size and the one native 81-frame chunk, the builder's defaults below and the planner's geometry. */
export const ANIMATE_DEFAULT_WIDTH = 482;
export const ANIMATE_DEFAULT_HEIGHT = 854;
export const ANIMATE_DEFAULT_FRAMES = 81;

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
  width = ANIMATE_DEFAULT_WIDTH, height = ANIMATE_DEFAULT_HEIGHT,   // the official template's portrait default (482x854)
  length = ANIMATE_DEFAULT_FRAMES,      // one native chunk (81); the distilled recipe's unit
  steps = 10,                           // distilled: 10-step lcm cfg 1 shift 5 — the recipe
  seed = Math.floor(Math.random() * 1e15),
  poseStrength = 1.0, refStrength = 1.0,
  cacheDevice = "cpu", cacheDtype = "default",   // NEVER default to gpu/int8 (crash, see header)
  filenamePrefix = "render_wan_animate2",
  multicard = null,                     // { residentGb }: the card-only placement (header)
} = {}) {
  if (!refImagePath) throw new Error("buildWanAnimate2: refImagePath is required");
  if (!driverVideoPath) throw new Error("buildWanAnimate2: driverVideoPath is required");
  if (!prompt) throw new Error("buildWanAnimate2: prompt (character/background description) is required");
  if (!Number.isFinite(length) || length <= 0) {
    throw new Error(`buildWanAnimate2: length must be positive, got ${length}`);
  }
  if (!negative) negative = WAN_ANIMATE2_TEMPLATE_NEGATIVE;
  const residentGb = multicard ? Number(multicard.residentGb ?? ANIMATE_RESIDENT_GB_DEFAULT) : 0;
  if (multicard && (!Number.isFinite(residentGb) || residentGb < 0)) {
    throw new Error(`buildWanAnimate2: multicard.residentGb must be >= 0, got ${multicard.residentGb}`);
  }

  // Node ids match the official subgraph (see header) so a diff against the
  // template stays mechanical.
  const g = {
    "239": multicard
      ? {
          class_type: "UNETLoaderDisTorch2MultiGPU",
          inputs: {
            unet_name: unet, weight_dtype: "default",
            compute_device: ANIMATE_COMPUTE_DEVICE, virtual_vram_gb: 0,
            donor_device: "cpu",           // the donor is the storage card, named in the allocation string, never system RAM
            expert_mode_allocations: `${ANIMATE_COMPUTE_DEVICE},${Number(residentGb.toFixed(2))}gb;${ANIMATE_STORAGE_DEVICE},*`,
            eject_models: false,
          },
        }
      : { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "9": multicard
      ? { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: textEncoder, type: "wan", device: ANIMATE_STORAGE_DEVICE } }
      : { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "wan", device: "default" } },
    "3": { class_type: "CLIPTextEncode", inputs: { clip: ["9", 0], text: prompt } },
    "4": { class_type: "CLIPTextEncode", inputs: { clip: ["9", 0], text: negative } },
    "222": { class_type: "CLIPTextEncode", inputs: { clip: ["9", 0], text: motionPrompt } },
    "75": multicard
      ? { class_type: "CLIPVisionLoaderMultiGPU", inputs: { clip_name: clipVision, device: ANIMATE_STORAGE_DEVICE } }
      : { class_type: "CLIPVisionLoader", inputs: { clip_name: clipVision } },
    "7": multicard
      ? { class_type: "VAELoaderMultiGPU", inputs: { vae_name: vae, device: ANIMATE_STORAGE_DEVICE } }
      : { class_type: "VAELoader", inputs: { vae_name: vae } },
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
    // multicard: takes the text encoder off the cards once node 247 has consumed every encode; the sampler reads the conditioning through it, so it runs before the DiT is placed
    ...(multicard ? { "35": { class_type: "OffloadFreeAfterEncode", inputs: { positive: ["247", 0], negative: ["247", 1], clip: ["9", 0] } } } : {}),
    "224": { class_type: "WanAnimate2Cache", inputs: { model: ["239", 0], device: cacheDevice, dtype: cacheDtype } },
    "95": { class_type: "ModelSamplingSD3", inputs: { model: ["224", 0], shift: 5.0 } },
    "18": { class_type: "BasicScheduler", inputs: { model: ["224", 0], scheduler: "simple", steps, denoise: 1.0 } },
    "27": { class_type: "KSamplerSelect", inputs: { sampler_name: "lcm" } },
    "19": { class_type: "SamplerCustom", inputs: {
      model: ["95", 0], add_noise: true, noise_seed: seed, cfg: 1.0,
      positive: [multicard ? "35" : "247", 0], negative: [multicard ? "35" : "247", 1],
      sampler: ["27", 0], sigmas: ["18", 0], latent_image: ["247", 2],
    } },
    "223": { class_type: "TrimVideoLatent", inputs: { samples: ["19", 0], trim_amount: ["247", 3] } },
    // multicard: frees the cards of the DiT before the decode (its donor blocks share the storage card with the VAE) and reports them
    ...(multicard ? { "36": { class_type: "OffloadFreeForDecode", inputs: { samples: ["223", 0], vae: ["7", 0], check: false } } } : {}),
    "6": { class_type: "VAEDecode", inputs: { samples: [multicard ? "36" : "223", 0], vae: ["7", 0] } },
    // mux keeps the driver's own audio + fps (GetVideoComponents outputs 1/2)
    "245": { class_type: "CreateVideo", inputs: { images: ["6", 0], audio: ["241", 1], fps: ["241", 2] } },
    "246": { class_type: "SaveVideo", inputs: { video: ["245", 0], filename_prefix: filenamePrefix, format: "auto", codec: "auto" } },
  };
  return g;
}
