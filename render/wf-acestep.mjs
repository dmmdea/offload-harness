// wf-acestep.mjs — build the native ComfyUI ACE-Step v1 text-to-music graph.
// Reconciled against the LIVE /object_info node schema (2026-06-16). Instrumental music
// beds: tags-only prompt (genre/mood/instruments/BPM), empty lyrics. Needs the all-in-one
// checkpoint ace_step_v1_3.5b.safetensors (bundles DiT + CLIP text encoder + music VAE) in
// C:\ComfyUI\models\checkpoints. Apache-2.0 = commercial-safe. Output FLAC via SaveAudio
// (the produced file lands under node.audio in /history — see comfy-output.mjs).
export function buildAceStep({
  prompt, lyrics = "", seconds = 30, steps = 50, cfg = 4.0, shift = 5.0,
  lyricsStrength = 1.0, seed = Math.floor(Math.random() * 1e15),
  ckpt = "ace_step_v1_3.5b.safetensors", filenamePrefix = "render_music",
} = {}) {
  if (!prompt) throw new Error("buildAceStep: prompt (style tags) is required");
  if (steps === undefined) steps = 50;
  return {
    // all-in-one checkpoint → MODEL[0], CLIP[1], VAE[2]
    "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: ckpt } },
    "2": { class_type: "ModelSamplingSD3", inputs: { model: ["1", 0], shift } },
    // positive: the style tags (instrumental → empty lyrics); negative: empty conditioning
    "3": { class_type: "TextEncodeAceStepAudio", inputs: { clip: ["1", 1], tags: prompt, lyrics, lyrics_strength: lyricsStrength } },
    "4": { class_type: "TextEncodeAceStepAudio", inputs: { clip: ["1", 1], tags: "", lyrics: "", lyrics_strength: 1.0 } },
    "5": { class_type: "EmptyAceStepLatentAudio", inputs: { seconds, batch_size: 1 } },
    "6": { class_type: "KSampler", inputs: { model: ["2", 0], seed, steps, cfg, sampler_name: "euler", scheduler: "simple", positive: ["3", 0], negative: ["4", 0], latent_image: ["5", 0], denoise: 1 } },
    "7": { class_type: "VAEDecodeAudio", inputs: { samples: ["6", 0], vae: ["1", 2] } },
    "8": { class_type: "SaveAudio", inputs: { audio: ["7", 0], filename_prefix: filenamePrefix } },
  };
}
