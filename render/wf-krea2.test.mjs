// node --test render/wf-krea2.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { buildKrea2, KREA2_RECIPE } from "./wf-krea2.mjs";

const base = {
  prompt: "a matte glass dropper bottle on wet slate",
  unet: "krea2_turbo_bf16.safetensors",
};

test("graph shape: template-faithful — regular latent, NO shift node, no LoRA branch", () => {
  const g = buildKrea2({ ...base, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const t of ["UNETLoader", "CLIPLoader", "VAELoader", "CLIPTextEncode",
                   "EmptyLatentImage", "KSampler", "VAEDecode", "SaveImage"]) {
    assert.ok(types.includes(t), t + " present");
  }
  assert.ok(!types.includes("ModelSamplingAuraFlow"),
    "krea2's template has NO shift node — adding one changes the distilled distribution");
  assert.ok(!types.includes("EmptySD3LatentImage"),
    "krea2's template uses the regular latent node, not the SD3 one");
  assert.ok(!types.includes("LoraLoaderModelOnly"),
    "turbo is baked into the weights — there is no LoRA branch");
  assert.ok(!types.includes("CheckpointLoaderSimple"),
    "there is no all-in-one checkpoint for this family");
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.seed, 7);
  assert.equal(ks.denoise, 1.0);
  // KSampler samples the LOADER's model output directly (no shift in between).
  assert.deepEqual(ks.model, ["1", 0], "model reaches the sampler straight from the loader");
});

test("turbo recipe is the default and is pinned: 8 steps / cfg 1.0 / euler / simple", () => {
  const g = buildKrea2({ ...base });
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.steps, KREA2_RECIPE.steps);
  assert.equal(ks.cfg, KREA2_RECIPE.cfg);
  assert.equal(KREA2_RECIPE.steps, 8, "template turbo recipe: 8 steps");
  assert.equal(KREA2_RECIPE.cfg, 1.0, "template turbo recipe: cfg 1.0");
  assert.equal(ks.sampler_name, "euler");
  assert.equal(ks.scheduler, "simple");
  assert.throws(() => buildKrea2({ ...base, steps: 0 }), /steps\/cfg must be positive/);
  assert.throws(() => buildKrea2({ ...base, cfg: -1 }), /steps\/cfg must be positive/);
});

test("family split files: krea2 CLIP type + qwen3vl_4b encoder + shared qwen VAE defaults", () => {
  const g = buildKrea2({ ...base });
  const clip = Object.values(g).find((n) => n.class_type === "CLIPLoader").inputs;
  assert.equal(clip.type, "krea2", "CLIPLoader must load the encoder as the krea2 type");
  assert.equal(clip.clip_name, "qwen3vl_4b_bf16.safetensors");
  const vae = Object.values(g).find((n) => n.class_type === "VAELoader").inputs;
  assert.equal(vae.vae_name, "qwen_image_vae.safetensors");
});

test("pooling: poolVvramGb > 0 loads through DisTorch2 in RATIO mode with the proven input shape", () => {
  const g = buildKrea2({ ...base, poolVvramGb: 12 });
  const pooled = Object.values(g).find((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU");
  assert.ok(pooled, "DisTorch2 loader present when pooling is configured");
  assert.ok(!Object.values(g).some((n) => n.class_type === "UNETLoader"),
    "exactly one loader — never both");
  assert.equal(pooled.inputs.virtual_vram_gb, 12);
  assert.equal(pooled.inputs.compute_device, "cuda:0");
  assert.equal(pooled.inputs.donor_device, "cuda:1");
  assert.equal(pooled.inputs.expert_mode_allocations, "",
    "RATIO mode only — the expert string is a structurally silent no-op on this node");
  assert.equal(pooled.inputs.eject_models, true);
  // The sampler still reads the loader's model output.
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.deepEqual(ks.model, ["1", 0]);

  const single = buildKrea2({ ...base, poolVvramGb: 0 });
  assert.ok(Object.values(single).some((n) => n.class_type === "UNETLoader"),
    "vvram 0 = plain single-GPU loader (the small-fleet shape)");
  assert.ok(!Object.values(single).some((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU"));
  assert.throws(() => buildKrea2({ ...base, poolVvramGb: -1 }), /poolVvramGb must be >= 0/);
});

test("empty negative becomes ConditioningZeroOut of the POSITIVE, never an encoded empty string", () => {
  const g = buildKrea2({ ...base });
  const zero = Object.values(g).find((n) => n.class_type === "ConditioningZeroOut");
  assert.ok(zero, "zero-out present when negative is empty");
  const posId = Object.entries(g).find(([, n]) => n.class_type === "CLIPTextEncode")[0];
  assert.deepEqual(zero.inputs.conditioning, [posId, 0], "zeroes the positive conditioning");
  assert.equal(Object.values(g).filter((n) => n.class_type === "CLIPTextEncode").length, 1);

  const gn = buildKrea2({ ...base, negative: "blurry, watermark" });
  assert.ok(!Object.values(gn).some((n) => n.class_type === "ConditioningZeroOut"));
  assert.equal(Object.values(gn).filter((n) => n.class_type === "CLIPTextEncode").length, 2);
});

test("dims are rounded down to /16 (shared qwen VAE 8x + patch 2), floor 64", () => {
  const g = buildKrea2({ ...base, width: 2050, height: 1030 });
  const lat = Object.values(g).find((n) => n.class_type === "EmptyLatentImage").inputs;
  assert.equal(lat.width, 2048);
  assert.equal(lat.height, 1024);
  const tiny = buildKrea2({ ...base, width: 20, height: 20 });
  const tl = Object.values(tiny).find((n) => n.class_type === "EmptyLatentImage").inputs;
  assert.equal(tl.width, 64);
  assert.equal(tl.height, 64);
  assert.throws(() => buildKrea2({ ...base, width: NaN }), /width\/height/);
});

test("required inputs fail loud", () => {
  assert.throws(() => buildKrea2({ unet: "x.safetensors" }), /prompt is required/);
  assert.throws(() => buildKrea2({ prompt: "p" }), /unet is required/);
});
