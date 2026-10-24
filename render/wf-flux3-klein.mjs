// wf-flux3-klein.mjs — FLUX 3 Image Klein TEXT-TO-IMAGE graph.
//
// FLUX 3 Image Klein (Black Forest Labs, 2026-10-15) is a 5.6B DiT with a Qwen3-VL-4B text encoder (CLIPLoader type
// "flux3") and the FLUX.2 VAE (flux2-vae.safetensors, 128-channel latent at /16). `--family flux3-klein` in
// comfy-render.mjs selects this graph.
//
// Graph follows ComfyUI's shipped `image_flux3_klein_distilled_t2i` / `image_flux3_klein_t2i` templates (installed
// comfyui_workflow_templates package, ComfyUI 0.42.5) and BFL's reference code (github.com/black-forest-labs/
// flux-3-image-klein, src/flux3/pup.py and generate.py):
//   · Two checkpoints share the graph. The distilled one (the BFL API model) samples 16 steps with no guidance
//     (cfg 1.0); the Base one 50 steps at CFG 3.5. Which one a file is comes from its name (BFL ships every Base
//     file as `…-base-…`); a caller overrides the pair only as a pair.
//   · KSampler euler / simple / denoise 1 on EmptyFlux2LatentImage, the negative a CLIPTextEncode of the caller's
//     negative ("" by default, as both templates encode it; it acts only at cfg > 1, the Base checkpoint).
//   · THE PROMPT UPSAMPLER. BFL: "Do not use the model without a prompt upsampler" — the model takes full scene
//     captions and a short prompt sent as typed gives generic output. When `upsampler` names BFL's prompt upsampler
//     (the Qwen3.5-9B PUP as a ComfyUI text-encoder file, loaded with CLIPLoader type "flux3"), the graph runs it
//     first with TextGenerate: BFL's own t2i system prompt (flux3-klein-system-prompts.json, verbatim), thinking off,
//     max 3584 tokens and the reference sampling (pup.py SAMPLING: greedy — temperature 0, here top_k 1 — top_p
//     0.95, min_p 0, presence penalty 1.5 for t2i, no repetition penalty). Its caption feeds the positive encode and
//     a PreviewAny, so the runner can keep the caption beside the picture. `upsampler` "" sends the prompt verbatim:
//     the caller's caption is already the full scene (the harness's `refine:false`).
//
// CARD-ONLY PLACEMENT (placement "multicard", render/multicard-recipes.json rows flux3-klein/*): the DiT loads whole on
// the compute card, the text encoder and the VAE on the storage card, the upsampler on the compute card (it has
// finished and been freed before the DiT loads there), launched --gpu-only so no weight sits in host RAM.

import { readFileSync } from "node:fs";
import { basename } from "node:path";
import { findRecipe, multicardLoaders, rowDevices, PLACEMENTS, PLACEMENT_DEFAULT } from "./multicard-recipes.mjs";

const SYSTEM_PROMPTS = JSON.parse(readFileSync(new URL("./flux3-klein-system-prompts.json", import.meta.url), "utf8"));

/** BFL's t2i upsampler system prompt, verbatim. */
export const FLUX3_KLEIN_T2I_SYSTEM = SYSTEM_PROMPTS.t2i;

/** The sampling recipe of each checkpoint (BFL model card and the two templates). */
export const FLUX3_KLEIN_RECIPES = Object.freeze({
  distilled: Object.freeze({ steps: 16, cfg: 1.0 }),
  base: Object.freeze({ steps: 50, cfg: 3.5 }),
});

/** TextGenerate settings of the upsampler (BFL reference pup.py: SAMPLING and PRESENCE_PENALTY["t2i"]). */
export const FLUX3_KLEIN_UPSAMPLING = Object.freeze({
  max_length: 3584, top_k: 1, top_p: 0.95, min_p: 0, repetition_penalty: 1.0, presence_penalty: 1.5,
});

/** The VAE every FLUX 3 Klein file decodes with. */
export const FLUX3_KLEIN_VAE = "flux2-vae.safetensors";

/** "base" for BFL's Base checkpoint files (`…-base-…`), "distilled" for every other name. */
export function flux3KleinVariant(unet) {
  return /(^|[-_.])base([-_.]|$)/i.test(basename(String(unet || ""))) ? "base" : "distilled";
}

export function buildFlux3Klein({
  prompt, negative = "",
  unet, clip, vae = FLUX3_KLEIN_VAE,
  upsampler = "",
  steps, cfg,
  sampler = "euler", scheduler = "simple",
  width = 1024, height = 1024,
  seed = Math.floor(Math.random() * 1e15),
  filenamePrefix = "render",
  // placement: "single" (default: plain loaders, the launch profile pins the card) | "multicard" (the card-only recipe).
  placement = PLACEMENT_DEFAULT, residentGb,
} = {}) {
  if (!prompt) throw new Error("buildFlux3Klein: prompt is required");
  if (!unet) throw new Error("buildFlux3Klein: unet is required");
  if (!clip) throw new Error("buildFlux3Klein: clip is required (the Qwen3-VL-4B text encoder, bf16 or int8_convrot)");
  if (String(unet).toLowerCase().endsWith(".gguf")) {
    throw new Error("buildFlux3Klein: no GGUF loader is wired for FLUX 3 Klein on ComfyUI — bind the bf16, int8-convrot or nvfp4 .safetensors (GGUF runs on the sd.cpp engine)");
  }
  const recipe = FLUX3_KLEIN_RECIPES[flux3KleinVariant(unet)];
  steps = steps ?? recipe.steps;
  cfg = cfg ?? recipe.cfg;
  if (!Number.isFinite(steps) || steps <= 0 || !Number.isFinite(cfg) || cfg <= 0) {
    throw new Error(`buildFlux3Klein: steps/cfg must be positive numbers, got ${steps}/${cfg}`);
  }
  if (!Number.isFinite(width) || width <= 0 || !Number.isFinite(height) || height <= 0) {
    throw new Error(`buildFlux3Klein: width/height must be positive numbers, got ${width}x${height}`);
  }
  if (!PLACEMENTS.includes(placement)) {
    throw new Error(`buildFlux3Klein: placement must be ${PLACEMENTS.join("|")}, got ${JSON.stringify(placement)}`);
  }
  // EmptyFlux2LatentImage steps by 16 (VAE 8x, DiT patch 2).
  width = Math.max(64, Math.floor(width / 16) * 16);
  height = Math.max(64, Math.floor(height / 16) * 16);

  const row = placement === "multicard" ? findRecipe({ route: "image", family: "flux3-klein", unet, clip }) : null;
  const g = row ? multicardLoaders(row, { unet, clip, vae, residentGb }) : {
    "1": { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "2": { class_type: "CLIPLoader", inputs: { clip_name: clip, type: "flux3", device: "default" } },
    "3": { class_type: "VAELoader", inputs: { vae_name: vae } },
  };

  let positiveText = prompt;
  if (String(upsampler).trim()) {
    g["20"] = row
      ? { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: upsampler, type: "flux3", device: rowDevices(row).compute } }
      : { class_type: "CLIPLoader", inputs: { clip_name: upsampler, type: "flux3", device: "default" } };
    const u = FLUX3_KLEIN_UPSAMPLING;
    g["21"] = {
      class_type: "TextGenerate",
      inputs: {
        clip: ["20", 0], prompt, max_length: u.max_length,
        sampling_mode: "on",
        "sampling_mode.temperature": 1.0, "sampling_mode.top_k": u.top_k, "sampling_mode.top_p": u.top_p,
        "sampling_mode.min_p": u.min_p, "sampling_mode.repetition_penalty": u.repetition_penalty,
        "sampling_mode.seed": 0, "sampling_mode.presence_penalty": u.presence_penalty,
        thinking: false, use_default_template: true, mtp: "off",
        system_prompt: FLUX3_KLEIN_T2I_SYSTEM,
      },
    };
    g["22"] = { class_type: "PreviewAny", inputs: { source: ["21", 0] } };
    positiveText = ["21", 0];
  }
  g["6"] = { class_type: "CLIPTextEncode", inputs: { text: positiveText, clip: ["2", 0] } };
  g["7"] = { class_type: "CLIPTextEncode", inputs: { text: String(negative || ""), clip: ["2", 0] } };
  // multicard: a card-only (--gpu-only) launch never moves a loaded model off its card on its own, and the whole DiT lands on the compute card, where
  // the upsampler ran and the text encoder keeps the share its allocation did not park on the storage card. So the harness's OffloadFreeAfterEncode
  // (render/comfyui-ext) takes each of them off the cards once the prompt is encoded (23 the upsampler, 24 the encoder); the sampler reads the
  // conditioning through them, so both releases land before the DiT does.
  let cond = [["6", 0], ["7", 0]];
  if (row) {
    if (g["20"]) {
      g["23"] = { class_type: "OffloadFreeAfterEncode", inputs: { positive: cond[0], negative: cond[1], clip: ["20", 0] } };
      cond = [["23", 0], ["23", 1]];
    }
    g["24"] = { class_type: "OffloadFreeAfterEncode", inputs: { positive: cond[0], negative: cond[1], clip: ["2", 0] } };
    cond = [["24", 0], ["24", 1]];
  }
  g["8"] = { class_type: "EmptyFlux2LatentImage", inputs: { width, height, batch_size: 1 } };
  g["9"] = {
    class_type: "KSampler",
    inputs: {
      seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1.0,
      model: ["1", 0], positive: cond[0], negative: cond[1], latent_image: ["8", 0],
    },
  };
  g["10"] = { class_type: "VAEDecode", inputs: { samples: ["9", 0], vae: ["3", 0] } };
  g["11"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["10", 0] } };
  return g;
}
