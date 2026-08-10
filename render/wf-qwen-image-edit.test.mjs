// node --test render/wf-qwen-image-edit.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { buildQwenImageEdit, QWEN_EDIT_PRESETS } from "./wf-qwen-image-edit.mjs";

const base = {
  image: "in.png",
  prompt: "make the sofa fur",
  unet: "qwen-image-edit-2511-Q5_1.gguf",
  ...QWEN_EDIT_PRESETS.full,
};

test("graph shape: loaders, kontext scale, dual text encode, encode, sample, decode, save", () => {
  const g = buildQwenImageEdit({ ...base, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const t of ["CLIPLoader", "VAELoader", "LoadImage", "FluxKontextImageScale",
                   "TextEncodeQwenImageEditPlus", "VAEEncode", "ModelSamplingAuraFlow",
                   "KSampler", "VAEDecode", "SaveImage"]) {
    assert.ok(types.includes(t), t + " present");
  }
  assert.equal(types.filter((t) => t === "TextEncodeQwenImageEditPlus").length, 2,
    "positive and negative both go through the edit encoder");
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.seed, 7);
  assert.equal(ks.denoise, 1.0);
});

test("loader switches on extension: .gguf -> UnetLoaderGGUF, .safetensors -> UNETLoader", () => {
  const gg = buildQwenImageEdit({ ...base, unet: "qwen-image-edit-2511-Q5_1.gguf" });
  assert.ok(Object.values(gg).some((n) => n.class_type === "UnetLoaderGGUF"));
  assert.ok(!Object.values(gg).some((n) => n.class_type === "UNETLoader"));

  const gs = buildQwenImageEdit({ ...base, unet: "qwen_image_edit_2511_fp8.safetensors" });
  assert.ok(Object.values(gs).some((n) => n.class_type === "UNETLoader"));
  assert.ok(!Object.values(gs).some((n) => n.class_type === "UnetLoaderGGUF"));

  // explicit override beats the extension
  const forced = buildQwenImageEdit({ ...base, unet: "weird.bin", loader: "gguf" });
  assert.ok(Object.values(forced).some((n) => n.class_type === "UnetLoaderGGUF"));
  assert.throws(() => buildQwenImageEdit({ ...base, loader: "nonsense" }), /loader must be/);
});

test("the reference image reaches BOTH encoders and the sampled latent, via the scaler", () => {
  const g = buildQwenImageEdit({ ...base });
  const scaleId = Object.entries(g).find(([, n]) => n.class_type === "FluxKontextImageScale")[0];
  const encoders = Object.values(g).filter((n) => n.class_type === "TextEncodeQwenImageEditPlus");
  for (const e of encoders) {
    assert.deepEqual(e.inputs.image1, [scaleId, 0], "encoder reads the SCALED image");
    assert.ok(Array.isArray(e.inputs.vae), "vae wired in, else no reference latents are emitted");
  }
  const enc = Object.values(g).find((n) => n.class_type === "VAEEncode").inputs;
  assert.deepEqual(enc.pixels, [scaleId, 0], "latent comes from the SCALED image, not the raw one");
});

test("LoRA is optional, model-only, and inserted between loader and sampling", () => {
  const none = buildQwenImageEdit({ ...base });
  assert.ok(!Object.values(none).some((n) => n.class_type === "LoraLoaderModelOnly"));

  const g = buildQwenImageEdit({ ...base, ...QWEN_EDIT_PRESETS.lightning8 });
  const [loraId, lora] = Object.entries(g).find(([, n]) => n.class_type === "LoraLoaderModelOnly");
  assert.match(lora.inputs.lora_name, /Lightning-8steps/);
  const loaderId = Object.entries(g).find(([, n]) => /^(UnetLoaderGGUF|UNETLoader)$/.test(n.class_type))[0];
  assert.deepEqual(lora.inputs.model, [loaderId, 0], "LoRA takes the raw loader output");
  const ms = Object.values(g).find((n) => n.class_type === "ModelSamplingAuraFlow").inputs;
  assert.deepEqual(ms.model, [loraId, 0], "sampling shift applies AFTER the LoRA");
});

test("presets pair steps with cfg — Lightning must drop cfg to 1.0", () => {
  assert.equal(QWEN_EDIT_PRESETS.lightning8.steps, 8);
  assert.equal(QWEN_EDIT_PRESETS.lightning8.cfg, 1.0);
  assert.equal(QWEN_EDIT_PRESETS.lightning4.cfg, 1.0);
  assert.equal(QWEN_EDIT_PRESETS.full.lora, "", "the full preset must not pull a distillation LoRA");
  assert.ok(QWEN_EDIT_PRESETS.full.cfg > 1.0);
});

test("steps/cfg are required — an unpaired sampler setting is the classic silent ruin", () => {
  const { steps, cfg, ...noSampler } = base;
  assert.throws(() => buildQwenImageEdit({ ...noSampler, cfg: 3 }), /steps is required/);
  assert.throws(() => buildQwenImageEdit({ ...noSampler, steps: 40 }), /cfg is required/);
});

test("required inputs are enforced", () => {
  assert.throws(() => buildQwenImageEdit({ ...base, image: undefined }), /image/);
  assert.throws(() => buildQwenImageEdit({ ...base, prompt: undefined }), /prompt/);
  assert.throws(() => buildQwenImageEdit({ ...base, unet: undefined }), /unet/);
});
