// node --test render/wf-qwen-image-edit.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import {
  buildQwenImageEdit, QWEN_EDIT_PRESETS,
  resolveEditMegapixels, EDIT_MEGAPIXEL_CAP, EDIT_MEGAPIXEL_FLOOR, EDIT_RESOLUTION_STEPS,
} from "./wf-qwen-image-edit.mjs";

const base = {
  image: "in.png",
  prompt: "make the sofa fur",
  unet: "qwen-image-edit-2511-Q5_1.gguf",
  ...QWEN_EDIT_PRESETS.full,
};

test("graph shape: loaders, canvas scale, dual text encode, encode, sample, decode, save", () => {
  const g = buildQwenImageEdit({ ...base, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const t of ["CLIPLoader", "VAELoader", "LoadImage", "ImageScaleToTotalPixels",
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

// The regression this file exists to hold: FluxKontextImageScale takes no size
// argument and hard-clamps to Flux-Kontext's 17-entry table, whose largest entry is
// 1024x1024. Since node 5 feeds VAEEncode -> KSampler.latent_image at denoise 1.0, that
// clamp WAS the edit seat's output resolution — a 2048x1024 source rendered at 1456x720.
test("the canvas scaler is NOT the Flux-Kontext node, and takes an explicit size", () => {
  const g = buildQwenImageEdit({ ...base, megapixels: 2.0 });
  const types = Object.values(g).map((n) => n.class_type);
  assert.ok(!types.includes("FluxKontextImageScale"),
    "FluxKontextImageScale caps the edit at ~1 MP and cannot be told otherwise");

  const scale = Object.values(g).find((n) => n.class_type === "ImageScaleToTotalPixels").inputs;
  assert.equal(scale.megapixels, 2.0, "the megapixel target reaches the node");
  assert.equal(scale.upscale_method, "lanczos");
  // 8x VAE stride x patch size 2. Without the snap the latent needs padding and the
  // edit comes back soft and subtly warped.
  assert.equal(scale.resolution_steps, 16);
  assert.equal(EDIT_RESOLUTION_STEPS, 16);
});

test("megapixels flows from the caller and is range-checked against the node's schema", () => {
  assert.equal(
    Object.values(buildQwenImageEdit({ ...base, megapixels: 0.75 }))
      .find((n) => n.class_type === "ImageScaleToTotalPixels").inputs.megapixels, 0.75);
  // Unset falls back to the seat's ceiling rather than to the old ~1 MP clamp.
  assert.equal(
    Object.values(buildQwenImageEdit({ ...base }))
      .find((n) => n.class_type === "ImageScaleToTotalPixels").inputs.megapixels, EDIT_MEGAPIXEL_CAP);
  assert.ok(EDIT_MEGAPIXEL_CAP > 1.05, "the default must exceed the Flux-Kontext ceiling it replaced");
  // ImageScaleToTotalPixels declares min 0.01 / max 16.0; fail before the GPU lock.
  assert.throws(() => buildQwenImageEdit({ ...base, megapixels: 0 }), /megapixels must be within/);
  assert.throws(() => buildQwenImageEdit({ ...base, megapixels: 32 }), /megapixels must be within/);
  assert.throws(() => buildQwenImageEdit({ ...base, megapixels: "big" }), /megapixels must be within/);
});

test("resolveEditMegapixels: configured wins, else the source's own size held in band", () => {
  // An explicit target is honoured verbatim, even outside the band.
  assert.equal(resolveEditMegapixels({ configured: 4, width: 512, height: 512 }), 4);
  assert.equal(resolveEditMegapixels({ configured: 0.5, width: 4000, height: 4000 }), 0.5);
  // The harness's own 2048x1024 render is EXACTLY the cap, so it survives at 1.0 scale.
  assert.equal(resolveEditMegapixels({ width: 2048, height: 1024 }), 2.0);
  // A 24 MP phone photo is clamped: the seat runs on 16 GB cards under a ~15 GB unet.
  assert.equal(resolveEditMegapixels({ width: 6000, height: 4000 }), EDIT_MEGAPIXEL_CAP);
  // Unknown source size falls back to the cap — the non-destructive direction.
  assert.equal(resolveEditMegapixels({ width: 0, height: 0 }), EDIT_MEGAPIXEL_CAP);
  assert.equal(resolveEditMegapixels(), EDIT_MEGAPIXEL_CAP);
  // The whole 1-MP-class grid sits inside the band and keeps its own exact size.
  for (const [w, h] of [[1024, 1024], [1344, 768], [1152, 896], [1216, 832], [1536, 640]]) {
    assert.equal(resolveEditMegapixels({ width: w, height: h }), (w * h) / (1024 * 1024),
      `${w}x${h} must render at its own resolution, not a stretched one`);
  }
  // Below the band a source is scaled UP onto the working canvas — same as the node
  // this replaced, which took a 512x512 input to 1024x1024.
  assert.equal(resolveEditMegapixels({ width: 512, height: 512 }), EDIT_MEGAPIXEL_FLOOR);
  assert.equal(resolveEditMegapixels({ width: 97, height: 53 }), EDIT_MEGAPIXEL_FLOOR);
  assert.ok(EDIT_MEGAPIXEL_FLOOR >= 0.01, "the floor must clear the node's declared minimum");
});

// Guards found by replaying ComfyUI's own ImageScaleToTotalPixels arithmetic over real
// files: a 97x53 thumbnail resolves to 0.0049 MP (under the node's 0.01 minimum, which
// would throw), and a pathological aspect ratio can snap a dimension to 0 — a graph
// ComfyUI cannot execute. The floor is what keeps both out of reach.
test("degenerate sources still produce a renderable canvas", () => {
  const scaled = (w, h) => {
    const mp = resolveEditMegapixels({ width: w, height: h });
    const by = Math.sqrt((mp * 1024 * 1024) / (w * h));
    return [Math.round((w * by) / 16) * 16, Math.round((h * by) / 16) * 16];
  };
  for (const [w, h] of [[97, 53], [16384, 3], [8, 8], [1, 1], [4000, 17]]) {
    const [sw, sh] = scaled(w, h);
    assert.ok(sw >= 16 && sh >= 16, `${w}x${h} -> ${sw}x${sh} must stay on the 16px grid, not collapse`);
    assert.equal(sw % 16, 0);
    assert.equal(sh % 16, 0);
    assert.doesNotThrow(() => buildQwenImageEdit({ ...base, megapixels: resolveEditMegapixels({ width: w, height: h }) }));
  }
});

test("the reference image reaches BOTH encoders and the sampled latent, via the scaler", () => {
  const g = buildQwenImageEdit({ ...base });
  const scaleId = Object.entries(g).find(([, n]) => n.class_type === "ImageScaleToTotalPixels")[0];
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
