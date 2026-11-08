// wf-acestep.mjs — build the ComfyUI ACE-Step v1.5 (turbo) text-to-music graph.
// Verified against the LIVE /object_info schema + the official audio_ace_step1_5_xl_turbo
// template (2026-07-15). The v1.5 stack is SPLIT — NOT the old v1 all-in-one checkpoint:
// UNETLoader (DiT) + DualCLIPLoader type "ace" (qwen 0.6b + 4b text encoders) + VAELoader.
// Instrumental beds: tags-only prompt (genre/mood/instruments), empty lyrics. Apache-2.0 =
// commercial-safe. Output FLAC via SaveAudio (lands under node.audio in /history — see
// comfy-output.mjs). keyscale has no schema default and MUST be supplied. generateAudioCodes
// true runs the qwen_4b LLM (higher quality, slower); set false for fast instrumental beds.
//
// CARD-ONLY PLACEMENT (placement "multicard", render/multicard-recipes.json row acestep/xl-turbo-bf16): the DiT, the two text encoders and the VAE do not fit one
// 16 GB card together, so the DiT loads whole on the compute card (UNETLoaderMultiGPU), both text encoders whole on the storage card (DualCLIPLoaderMultiGPU,
// type "ace") and the VAE on the storage card (VAELoaderMultiGPU): every weight in VRAM, none in host RAM. Only nodes 1 to 3 differ; the sampling graph is the same.
import { findRecipe, multicardLoaders, PLACEMENTS, PLACEMENT_DEFAULT } from "./multicard-recipes.mjs";

export function buildAceStep({
  prompt, lyrics = "", seconds = 30, steps = 8, cfg = 1.0, shift = 3.0,
  bpm = 120, timeSignature = "4", language = "en", keyScale = "C major",
  generateAudioCodes = true, cfgScale = 2.0, temperature = 0.85, topP = 0.9, topK = 0, minP = 0,
  seed = Math.floor(Math.random() * 1e15),
  unet = "acestep_v1.5_xl_turbo_bf16.safetensors",
  clip1 = "qwen_0.6b_ace15.safetensors", clip2 = "qwen_4b_ace15.safetensors",
  vae = "ace_1.5_vae.safetensors", filenamePrefix = "render_music",
  // placement: "single" (default: the graph below, byte for byte) | "multicard" (the card-only recipe). residentGb is refused by the plain-DiT recipe, so it is not taken.
  placement = PLACEMENT_DEFAULT,
} = {}) {
  if (!prompt) throw new Error("buildAceStep: prompt (style tags) is required");
  if (!PLACEMENTS.includes(placement)) throw new Error(`buildAceStep: placement must be ${PLACEMENTS.join("|")}, got ${JSON.stringify(placement)}`);
  if (steps === undefined) steps = 8;
  // clip_name1 is the 0.6b encoder and clip_name2 the 4b one, the order the single graph has; the recipe is picked by the 4b file (its defaultClip)
  const loaders = placement === "multicard"
    ? multicardLoaders(findRecipe({ route: "music", family: "acestep", unet, clip: clip2 }), { unet, clip: clip1, clip2, vae })
    : null;
  return {
    "1": loaders ? loaders["1"] : { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "2": loaders ? loaders["3"] : { class_type: "VAELoader", inputs: { vae_name: vae } },
    "3": loaders ? loaders["2"] : { class_type: "DualCLIPLoader", inputs: { clip_name1: clip1, clip_name2: clip2, type: "ace" } },
    "4": { class_type: "TextEncodeAceStepAudio1.5", inputs: { clip: ["3", 0], tags: prompt, lyrics, seed, bpm, duration: seconds, timesignature: timeSignature, language, keyscale: keyScale, generate_audio_codes: generateAudioCodes, cfg_scale: cfgScale, temperature, top_p: topP, top_k: topK, min_p: minP } },
    "5": { class_type: "ConditioningZeroOut", inputs: { conditioning: ["4", 0] } },
    "6": { class_type: "EmptyAceStep1.5LatentAudio", inputs: { seconds, batch_size: 1 } },
    "7": { class_type: "ModelSamplingAuraFlow", inputs: { model: ["1", 0], shift } },
    "8": { class_type: "KSampler", inputs: { model: ["7", 0], seed, steps, cfg, sampler_name: "euler", scheduler: "simple", positive: ["4", 0], negative: ["5", 0], latent_image: ["6", 0], denoise: 1 } },
    "9": { class_type: "VAEDecodeAudio", inputs: { samples: ["8", 0], vae: ["2", 0] } },
    "10": { class_type: "SaveAudio", inputs: { audio: ["9", 0], filename_prefix: filenamePrefix } },
  };
}
