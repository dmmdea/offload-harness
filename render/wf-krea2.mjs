// wf-krea2.mjs — Krea 2 Turbo TEXT-TO-IMAGE graph.
//
// Krea 2 Turbo is a turbo-distilled DiT on the Qwen-Image stack (qwen_image_vae
// + a Qwen3-VL text encoder loaded with CLIPLoader type "krea2"), selected as
// the 32GB-class image seat by the operator's blind bake-off verdict
// (2026-08-14: won both decided pairs incl. the text-rendering probe).
// `--family krea2` in comfy-render.mjs selects this graph.
//
// Graph follows ComfyUI's shipped `image_krea2_turbo_t2i` template (verified
// from the installed comfyui_workflow_templates package, cross-checked against
// the bake-off graphs recovered verbatim from /history): 8 steps / cfg 1.0 /
// euler / simple / denoise 1. Deliberate template facts this builder pins:
//   · NO ModelSamplingAuraFlow shift node — unlike Qwen-Image 2512, the krea2
//     template samples the loader's model output directly; adding a shift node
//     here would change the distribution the turbo distillation was trained on.
//   · Regular EmptyLatentImage (the template's own latent source for this
//     family), NOT the SD3 latent node wf-qwen-image.mjs uses.
//   · The turbo recipe is BAKED INTO the weights — there is no LoRA branch and
//     no "full" preset; running many steps at high cfg burns a distilled model
//     out exactly like Lightning at 50/4 (mush).
//   · Text encoder defaults to the family-standard qwen3vl_4b_bf16 split file;
//     the VAE is the shared qwen_image_vae. Neither lives in the checkpoint,
//     so a "builtin" VAE binding is a config error (caller guards, like
//     qwen-image).
//
// POOLED LOADING (house 32GB-pool doctrine — operator-reaffirmed 2026-08-14,
// quality/doctrine outrank speed): when poolVvramGb > 0 the DiT loads through
// ComfyUI-MultiGPU's `UNETLoaderDisTorch2MultiGPU` in RATIO mode
// (virtual_vram_gb) — the shape proven on the reference dual-GPU box. The
// byte-expert allocation string is deliberately NOT used: the node's
// patched_load_models_gpu reads only the post-'#' segment that expert mode
// leaves empty, so the reservation half never engages and the split silently
// collapses to one card (measured 2026-08-11; upstream ComfyUI-MultiGPU #191
// tracks the wider DynamicVRAM shadowing defect — pooled serving also requires
// launching ComfyUI with --disable-dynamic-vram until that lands, which is a
// LAUNCH concern, not a graph concern). poolVvramGb <= 0 loads through the
// plain UNETLoader — the single-GPU fleet shape.
//
// Negative-prompt semantics: same house idiom as wf-qwen-image.mjs — no baked
// prompt content; an empty/whitespace negative becomes ConditioningZeroOut of
// the positive, a real negative is encoded (active at cfg > 1, i.e. only when
// a caller also overrides cfg upward deliberately).

/** The turbo-distilled recipe the checkpoint was shipped with (template widgets). */
export const KREA2_RECIPE = { steps: 8, cfg: 1.0 };

export function buildKrea2({
  prompt, negative = "",
  unet,
  clip = "qwen3vl_4b_bf16.safetensors",
  vae = "qwen_image_vae.safetensors",
  steps = KREA2_RECIPE.steps, cfg = KREA2_RECIPE.cfg,
  sampler = "euler", scheduler = "simple",
  width = 1024, height = 1024,   // template-native 1:1; proven at 2048x1024 in the bake-off
  seed = Math.floor(Math.random() * 1e15),
  poolVvramGb = 0,
  poolCompute = "cuda:0", poolDonor = "cuda:1",
  filenamePrefix = "render",
} = {}) {
  if (!prompt) throw new Error("buildKrea2: prompt is required");
  if (!unet) throw new Error("buildKrea2: unet is required");
  if (!Number.isFinite(steps) || steps <= 0 || !Number.isFinite(cfg) || cfg <= 0) {
    throw new Error("buildKrea2: steps/cfg must be positive (the turbo recipe is 8/1.0; overriding it away from the distillation burns the render out)");
  }
  if (!Number.isFinite(width) || width <= 0 || !Number.isFinite(height) || height <= 0) {
    throw new Error(`buildKrea2: width/height must be positive numbers, got ${width}x${height}`);
  }
  if (!Number.isFinite(poolVvramGb) || poolVvramGb < 0) {
    throw new Error(`buildKrea2: poolVvramGb must be >= 0, got ${poolVvramGb}`);
  }
  // Shared qwen_image_vae (8x) + DiT patch size 2 → pixel dims must be /16,
  // same arithmetic as wf-qwen-image.mjs.
  width = Math.max(64, Math.floor(width / 16) * 16);
  height = Math.max(64, Math.floor(height / 16) * 16);

  const g = {
    "1": poolVvramGb > 0
      ? {
          class_type: "UNETLoaderDisTorch2MultiGPU",
          inputs: {
            unet_name: unet, weight_dtype: "default",
            compute_device: poolCompute, virtual_vram_gb: poolVvramGb,
            donor_device: poolDonor,
            // Ratio mode ONLY — see the header for why the expert string is
            // structurally a silent no-op on this node.
            expert_mode_allocations: "",
            eject_models: true,
          },
        }
      : { class_type: "UNETLoader", inputs: { unet_name: unet, weight_dtype: "default" } },
    "2": { class_type: "CLIPLoader", inputs: { clip_name: clip, type: "krea2", device: "default" } },
    "3": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "6": { class_type: "CLIPTextEncode", inputs: { text: prompt, clip: ["2", 0] } },
  };
  g["7"] = String(negative).trim()
    ? { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["2", 0] } }
    : { class_type: "ConditioningZeroOut", inputs: { conditioning: ["6", 0] } };
  g["8"] = { class_type: "EmptyLatentImage", inputs: { width, height, batch_size: 1 } };
  g["9"] = {
    class_type: "KSampler",
    inputs: {
      seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1.0,
      model: ["1", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["8", 0],
    },
  };
  g["10"] = { class_type: "VAEDecode", inputs: { samples: ["9", 0], vae: ["3", 0] } };
  g["11"] = { class_type: "SaveImage", inputs: { filename_prefix: filenamePrefix, images: ["10", 0] } };
  return g;
}
