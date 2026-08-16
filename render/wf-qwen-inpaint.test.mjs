// node --test render/wf-qwen-inpaint.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { buildQwenInpaint, QWEN_INPAINT_PRESETS } from "./wf-qwen-inpaint.mjs";

const base = {
  prompt: "a small cactus in a terracotta pot",
  image: "staged_src.png",
  mask: "staged_mask.png",
  unet: "qwen-image-2512-Q5_1.gguf",
  ...QWEN_INPAINT_PRESETS.full,
};

test("graph shape: patch loader + diffsynth controlnet + VAEEncode latent, no latent-mask nodes", () => {
  const g = buildQwenInpaint({ ...base, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const t of ["CLIPLoader", "VAELoader", "LoadImage", "ImageToMask", "ModelPatchLoader",
                   "QwenImageDiffsynthControlnet", "ModelSamplingAuraFlow", "CLIPTextEncode",
                   "VAEEncode", "KSampler", "VAEDecode", "SaveImage"]) {
    assert.ok(types.includes(t), t + " present");
  }
  // Preservation is the PATCH's job — the SDXL latent-mask idiom must not leak in.
  for (const t of ["SetLatentNoiseMask", "VAEEncodeForInpaint", "EmptySD3LatentImage", "EmptyLatentImage"]) {
    assert.ok(!types.includes(t), t + " must not appear in the diffsynth route");
  }
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.seed, 7);
  assert.equal(ks.denoise, 1.0, "template recipe: full-frame regeneration at denoise 1.0");
});

test("controlnet wiring: original image (not a map) + red-channel mask + strength; sampler takes the PATCHED model", () => {
  const g = buildQwenInpaint({ ...base, strength: 0.8 });
  const entries = Object.entries(g);
  const idOf = (t) => entries.find(([, n]) => n.class_type === t)?.[0];
  const cn = g[idOf("QwenImageDiffsynthControlnet")].inputs;
  const imgLoadId = entries.find(([, n]) => n.class_type === "LoadImage" && n.inputs.image === "staged_src.png")[0];
  const maskLoadId = entries.find(([, n]) => n.class_type === "LoadImage" && n.inputs.image === "staged_mask.png")[0];
  assert.deepEqual(cn.image, [imgLoadId, 0], "controlnet consumes the ORIGINAL image");
  assert.deepEqual(cn.model_patch, [idOf("ModelPatchLoader"), 0]);
  assert.equal(cn.strength, 0.8);
  const itm = g[idOf("ImageToMask")].inputs;
  assert.deepEqual(itm.image, [maskLoadId, 0], "mask comes from the mask LoadImage");
  assert.equal(itm.channel, "red", "no-alpha fixture masks need the red channel, not LoadImage's MASK output");
  assert.deepEqual(cn.mask, [idOf("ImageToMask"), 0]);
  const ks = g[idOf("KSampler")].inputs;
  assert.deepEqual(ks.model, [idOf("QwenImageDiffsynthControlnet"), 0], "sampler runs the PATCHED model");
  const ve = g[idOf("VAEEncode")].inputs;
  assert.deepEqual(ve.pixels, [imgLoadId, 0], "latent is a plain VAEEncode of the SOURCE");
});

test("loader switches on extension; explicit override wins; nonsense rejected", () => {
  const gg = buildQwenInpaint({ ...base });
  assert.ok(Object.values(gg).some((n) => n.class_type === "UnetLoaderGGUF"));
  const gs = buildQwenInpaint({ ...base, unet: "qwen_image_2512_bf16.safetensors" });
  assert.ok(Object.values(gs).some((n) => n.class_type === "UNETLoader"));
  const forced = buildQwenInpaint({ ...base, unet: "weird.bin", loader: "gguf" });
  assert.ok(Object.values(forced).some((n) => n.class_type === "UnetLoaderGGUF"));
  assert.throws(() => buildQwenInpaint({ ...base, loader: "nonsense" }), /loader must be/);
});

test("empty negative becomes ConditioningZeroOut of the POSITIVE; real negative encodes", () => {
  const g = buildQwenInpaint({ ...base });
  const zero = Object.values(g).find((n) => n.class_type === "ConditioningZeroOut");
  assert.ok(zero, "zero-out present when negative is empty");
  assert.equal(Object.values(g).filter((n) => n.class_type === "CLIPTextEncode").length, 1);
  const gn = buildQwenInpaint({ ...base, negative: "blurry" });
  assert.ok(!Object.values(gn).some((n) => n.class_type === "ConditioningZeroOut"));
  assert.equal(Object.values(gn).filter((n) => n.class_type === "CLIPTextEncode").length, 2);
});

test("lightning preset rides as LoraLoaderModelOnly feeding the sampling shift", () => {
  const g = buildQwenInpaint({ ...base, ...QWEN_INPAINT_PRESETS.lightning4 });
  const lora = Object.entries(g).find(([, n]) => n.class_type === "LoraLoaderModelOnly");
  assert.ok(lora, "lora node present");
  const msaf = Object.values(g).find((n) => n.class_type === "ModelSamplingAuraFlow").inputs;
  assert.deepEqual(msaf.model, [lora[0], 0], "shift applies to the LoRA'd model");
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.steps, 4);
  assert.equal(ks.cfg, 1.0);
  // full preset: no lora node at all
  const gf = buildQwenInpaint({ ...base });
  assert.ok(!Object.values(gf).some((n) => n.class_type === "LoraLoaderModelOnly"));
});

test("required-param throws are actionable", () => {
  assert.throws(() => buildQwenInpaint({ ...base, prompt: "" }), /prompt is required/);
  assert.throws(() => buildQwenInpaint({ ...base, image: "" }), /image .* required/);
  assert.throws(() => buildQwenInpaint({ ...base, mask: "" }), /mask .* required/);
  assert.throws(() => buildQwenInpaint({ ...base, unet: "" }), /unet is required/);
  assert.throws(() => buildQwenInpaint({ ...base, steps: 0 }), /steps is required/);
  assert.throws(() => buildQwenInpaint({ ...base, cfg: 0 }), /cfg is required/);
  assert.throws(() => buildQwenInpaint({ ...base, strength: NaN }), /strength must be a number/);
});
