// wf-h3-av.mjs — MiniMax-H3 fl2va TEXT/IMAGE-TO-VIDEO with JOINT AUDIO.
//
// Seated as the OPT-IN `h3` family (operator-approved 2026-08-27) on the T4
// bake-off verdict: vs the seated LTX-2.5 at matched still/prompt/seed/~0.9MP,
// H3 turbo-8 won prompt adherence (executes full scripted action arcs and
// multi-shot storyboards with hard cuts), i2v source fidelity (lighting/
// wardrobe/atmosphere preserved), and audio design (scored soundtrack with
// impact transients vs ambience wash) at ~2x the wall (457.8s vs 233.7s).
// LTX-2.5 stays the default family; pass model:"h3" deliberately.
//
// Graph = the official ComfyUI `video_minimax_h3_t2v/i2v` template
// (comfyui_workflow_templates_json 0.11.48), Motion subgraph flattened to API
// format with the subgraph's node ids. Deliberate deltas, each stated:
//   · TURBO IS THE DEFAULT (template default is off): the 8-step turbo-LoRA
//     recipe IS the bake-off verdict recipe. hero:true opts into the
//     non-LoRA 20-step path (the template's own alternative).
//   · ResolutionSelector + the frame-count ComfyMathExpression are computed
//     HERE (÷32 alignment; frames = max(5, round(sec*24)) padded so
//     frames % 17 == 5) — same arithmetic, no V3 dynamic-input serialization.
//   · NATIVE SINGLE-CARD LOADER ONLY — deliberately NO DisTorch pool option:
//     the pooled path upcasts the int8 DiT to bf16 (20.97 GB disk → 37.46 GB
//     pooled, measured 2026-08-27) while the plain loader keeps native
//     convrot-W4A4 ops with partial offload. The house loaded-size rule.

/** Template frame arithmetic: 24fps, min 5, padded to frames % 17 == 5. */
export function h3FramesFor(seconds) {
  const base = Math.max(5, Math.round(seconds * 24));
  return base + (((5 - (base % 17)) % 17) + 17) % 17;
}

export function buildH3AV({
  imagePath = "", prompt, negative = "",
  transformer = "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
  textEncoder = "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
  videoVae = "minimax_h3_video_vae_fp16.safetensors",
  audioVae = "minimax_h3_audio_vae_fp32.safetensors",
  turboLora = "minimax_h3_fl2v_turbo_8step_v1.0_comfyui_bf16.safetensors",
  width = 1280, height = 736,
  length = 0, seconds = 5, frameRate = 24,
  steps = 0,                       // 0 = recipe default: 8 turbo / 20 hero
  hero = false,                    // non-LoRA 20-step path (template alt recipe)
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "render_h3",
} = {}) {
  if (!prompt) throw new Error("buildH3AV: prompt is required");
  if (!Number.isFinite(width) || width <= 0 || !Number.isFinite(height) || height <= 0) {
    throw new Error(`buildH3AV: width/height must be positive, got ${width}x${height}`);
  }
  width = Math.max(64, Math.floor(width / 32) * 32);
  height = Math.max(64, Math.floor(height / 32) * 32);
  if (!length || length <= 0) length = h3FramesFor(seconds);
  const turbo = !hero;
  if (!steps || steps <= 0) steps = turbo ? 8 : 20;

  const g = {
    "127": { class_type: "UNETLoader", inputs: { unet_name: transformer, weight_dtype: "default" } },
    "128": { class_type: "CLIPLoader", inputs: { clip_name: textEncoder, type: "minimax", device: "default" } },
    "119": { class_type: "VAELoader", inputs: { vae_name: videoVae } },
    "120": { class_type: "VAELoader", inputs: { vae_name: audioVae } },
    "123": { class_type: "KSamplerSelect", inputs: { sampler_name: "res_multistep" } },
    "129": { class_type: "RandomNoise", inputs: { noise_seed: seed } },
  };
  let modelSrc = ["127", 0];
  if (turbo) {
    g["134"] = { class_type: "LoraLoaderModelOnly", inputs: { model: ["127", 0], lora_name: turboLora, strength_model: 1.0 } };
    modelSrc = ["134", 0];
  }
  const cond = { clip: ["128", 0], vae: ["119", 0], prompt, width, height, length };
  if (imagePath) {
    g["150"] = { class_type: "LoadImage", inputs: { image: imagePath } };
    cond.first_frame = ["150", 0];
  }
  g["131"] = { class_type: "MiniMaxH3ImageToVideo", inputs: cond };
  g["124"] = { class_type: "BasicScheduler", inputs: { model: modelSrc, scheduler: "simple", steps, denoise: 1.0 } };
  g["126"] = { class_type: "BasicGuider", inputs: { model: modelSrc, conditioning: ["131", 0] } };
  g["125"] = { class_type: "SamplerCustomAdvanced", inputs: { noise: ["129", 0], guider: ["126", 0], sampler: ["123", 0], sigmas: ["124", 0], latent_image: ["131", 1] } };
  g["122"] = { class_type: "VAEDecode", inputs: { samples: ["125", 0], vae: ["119", 0] } };
  g["121"] = { class_type: "VAEDecodeAudio", inputs: { samples: ["125", 0], vae: ["120", 0] } };
  g["130"] = { class_type: "CreateVideo", inputs: { images: ["122", 0], audio: ["121", 0], fps: frameRate } };
  g["92"] = { class_type: "SaveVideo", inputs: { video: ["130", 0], filename_prefix: filenamePrefix, format: "auto", codec: "auto" } };
  // NOTE the negative is deliberately unused: the template carries NO negative
  // conditioning node for H3 (BasicGuider is single-cond); kept in the signature
  // so a caller passing one gets a loud error rather than silence.
  if (negative) throw new Error("buildH3AV: the official H3 graph has no negative-conditioning path; remove the negative");
  return g;
}
