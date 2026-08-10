// node --test render/wf-qwen-image.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { buildQwenImage, QWEN_IMAGE_PRESETS } from "./wf-qwen-image.mjs";

const base = {
  prompt: "a matte glass dropper bottle on wet slate",
  unet: "qwen-image-2512-Q5_1.gguf",
  ...QWEN_IMAGE_PRESETS.full,
};

test("graph shape: loaders, SD3 latent, sampling shift, sample, decode, save", () => {
  const g = buildQwenImage({ ...base, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const t of ["CLIPLoader", "VAELoader", "CLIPTextEncode", "EmptySD3LatentImage",
                   "ModelSamplingAuraFlow", "KSampler", "VAEDecode", "SaveImage"]) {
    assert.ok(types.includes(t), t + " present");
  }
  assert.ok(!types.includes("EmptyLatentImage"),
    "SD1/SDXL 4-channel latent must never appear — qwen-image is SD3-latent");
  assert.ok(!types.includes("CheckpointLoaderSimple"),
    "there is no all-in-one checkpoint for this family");
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.seed, 7);
  assert.equal(ks.denoise, 1.0);
});

test("loader switches on extension: .gguf -> UnetLoaderGGUF, .safetensors -> UNETLoader", () => {
  const gg = buildQwenImage({ ...base, unet: "qwen-image-2512-Q5_1.gguf" });
  assert.ok(Object.values(gg).some((n) => n.class_type === "UnetLoaderGGUF"));
  assert.ok(!Object.values(gg).some((n) => n.class_type === "UNETLoader"));

  const gs = buildQwenImage({ ...base, unet: "qwen_image_2512_fp8_e4m3fn.safetensors" });
  assert.ok(Object.values(gs).some((n) => n.class_type === "UNETLoader"));
  assert.ok(!Object.values(gs).some((n) => n.class_type === "UnetLoaderGGUF"));

  // explicit override beats the extension
  const forced = buildQwenImage({ ...base, unet: "weird.bin", loader: "gguf" });
  assert.ok(Object.values(forced).some((n) => n.class_type === "UnetLoaderGGUF"));
  assert.throws(() => buildQwenImage({ ...base, loader: "nonsense" }), /loader must be/);
});

test("empty negative becomes ConditioningZeroOut of the POSITIVE, never an encoded empty string", () => {
  const g = buildQwenImage({ ...base });
  const zero = Object.values(g).find((n) => n.class_type === "ConditioningZeroOut");
  assert.ok(zero, "zero-out present when negative is empty");
  const posId = Object.entries(g).find(([, n]) => n.class_type === "CLIPTextEncode")[0];
  assert.deepEqual(zero.inputs.conditioning, [posId, 0], "zeroes the positive conditioning");
  assert.equal(Object.values(g).filter((n) => n.class_type === "CLIPTextEncode").length, 1,
    "exactly one text encode — nothing encodes an empty string");

  const gn = buildQwenImage({ ...base, negative: "blurry, watermark" });
  assert.ok(!Object.values(gn).some((n) => n.class_type === "ConditioningZeroOut"));
  assert.equal(Object.values(gn).filter((n) => n.class_type === "CLIPTextEncode").length, 2,
    "a real negative is encoded and active");
});

test("KSampler wiring: positive/negative/latent/model all reach the sampler", () => {
  const g = buildQwenImage({ ...base, negative: "blurry" });
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  const at = (ref) => g[ref[0]].class_type;
  assert.equal(at(ks.model), "ModelSamplingAuraFlow", "model comes through the sampling shift");
  assert.equal(at(ks.latent_image), "EmptySD3LatentImage");
  assert.equal(at(ks.positive), "CLIPTextEncode");
  assert.equal(at(ks.negative), "CLIPTextEncode");
  const dec = Object.values(g).find((n) => n.class_type === "VAEDecode").inputs;
  assert.equal(at(dec.samples), "KSampler");
  assert.equal(at(dec.vae), "VAELoader");
});

test("LoRA is optional, model-only, and inserted between loader and sampling", () => {
  const none = buildQwenImage({ ...base });
  assert.ok(!Object.values(none).some((n) => n.class_type === "LoraLoaderModelOnly"));

  const g = buildQwenImage({ ...base, ...QWEN_IMAGE_PRESETS.lightning4 });
  const [loraId, lora] = Object.entries(g).find(([, n]) => n.class_type === "LoraLoaderModelOnly");
  assert.match(lora.inputs.lora_name, /2512-Lightning-4steps/);
  const loaderId = Object.entries(g).find(([, n]) => /^(UnetLoaderGGUF|UNETLoader)$/.test(n.class_type))[0];
  assert.deepEqual(lora.inputs.model, [loaderId, 0], "LoRA takes the raw loader output");
  const ms = Object.values(g).find((n) => n.class_type === "ModelSamplingAuraFlow").inputs;
  assert.deepEqual(ms.model, [loraId, 0], "sampling shift applies AFTER the LoRA");
});

test("presets pair steps with cfg — Lightning must drop cfg to 1.0", () => {
  assert.equal(QWEN_IMAGE_PRESETS.full.steps, 50);
  assert.equal(QWEN_IMAGE_PRESETS.full.cfg, 4.0);
  assert.equal(QWEN_IMAGE_PRESETS.full.lora, "", "the full preset must not pull a distillation LoRA");
  assert.equal(QWEN_IMAGE_PRESETS.lightning4.steps, 4);
  assert.equal(QWEN_IMAGE_PRESETS.lightning4.cfg, 1.0);
  assert.ok(QWEN_IMAGE_PRESETS.lightning4.lora, "lightning4 names its LoRA");
});

test("dims default to template-native 1328 and snap to /16", () => {
  const g = buildQwenImage({ ...base });
  const lat = Object.values(g).find((n) => n.class_type === "EmptySD3LatentImage").inputs;
  assert.equal(lat.width, 1328);
  assert.equal(lat.height, 1328);
  assert.equal(lat.batch_size, 1);

  const odd = buildQwenImage({ ...base, width: 1670, height: 930 });
  const lo = Object.values(odd).find((n) => n.class_type === "EmptySD3LatentImage").inputs;
  assert.equal(lo.width, 1664, "floors to /16");
  assert.equal(lo.height, 928, "floors to /16");
});

test("steps/cfg are required — an unpaired sampler setting is the classic silent ruin", () => {
  const { steps, cfg, ...noSampler } = base;
  assert.throws(() => buildQwenImage({ ...noSampler, cfg: 4 }), /steps is required/);
  assert.throws(() => buildQwenImage({ ...noSampler, steps: 50 }), /cfg is required/);
});

test("required inputs are enforced", () => {
  assert.throws(() => buildQwenImage({ ...base, prompt: undefined }), /prompt/);
  assert.throws(() => buildQwenImage({ ...base, unet: undefined }), /unet/);
});
