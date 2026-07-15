import { test } from "node:test";
import assert from "node:assert";
import { buildWan22I2V } from "./wf-wan22-i2v.mjs";

test("two-stage high->low with leftover-noise handoff + DisTorch2 loaders", () => {
  const g = buildWan22I2V({ imagePath: "s.png", prompt: "drive-by of a sports car", length: 49, steps: 20, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  // two GGUF DisTorch2 loaders (high + low), the I2V node, two advanced samplers, tiled decode, combine
  assert.equal(types.filter((t) => t === "UnetLoaderGGUFDisTorch2MultiGPU").length, 2);
  assert.equal(types.filter((t) => t === "KSamplerAdvanced").length, 2);
  assert.ok(types.includes("WanImageToVideo"));
  // the high stage emits leftover noise; the low stage consumes it and does not add noise
  const samplers = Object.values(g).filter((n) => n.class_type === "KSamplerAdvanced");
  const high = samplers.find((s) => s.inputs.add_noise === "enable");
  const low = samplers.find((s) => s.inputs.add_noise === "disable");
  assert.ok(high && low, "need one add_noise=enable (high) and one disable (low)");
  assert.equal(high.inputs.return_with_leftover_noise, "enable");
  assert.equal(low.inputs.return_with_leftover_noise, "disable");
  assert.equal(high.inputs.end_at_step, 10); // 10/10 split of 20
  assert.equal(low.inputs.start_at_step, 10);
  assert.equal(high.inputs.noise_seed, low.inputs.noise_seed); // seeds MUST match
  // VHS_VideoCombine requires pingpong (caught at live submit on the Hunyuan path; same node here)
  const vhs = Object.values(g).find((n) => n.class_type === "VHS_VideoCombine");
  assert.ok(vhs.inputs.pingpong !== undefined, "VHS_VideoCombine requires pingpong");
});

test("4-step lightx2v defaults: LoRA per expert (HIGH on high-noise, LOW on low-noise)", () => {
  const g = buildWan22I2V({ imagePath: "s.png", prompt: "a red bike rolling", seed: 7 });
  // two LoRA loaders, one per noise expert, at strength 1.0
  const loras = Object.values(g).filter((n) => n.class_type === "LoraLoaderModelOnly");
  assert.equal(loras.length, 2, "one LoRA per expert");
  assert.ok(loras.every((l) => l.inputs.strength_model === 1.0));
  assert.ok(loras.some((l) => /HIGH_lightx2v_4step/.test(l.inputs.lora_name)), "HIGH lightx2v LoRA present");
  assert.ok(loras.some((l) => /LOW_lightx2v_4step/.test(l.inputs.lora_name)), "LOW lightx2v LoRA present");
  // topology: the HIGH LoRA takes the high-noise UNET (node 7) and feeds the high-stage
  // ModelSamplingSD3 (node 8); LOW takes the low-noise UNET (node 9) → node 10.
  assert.deepEqual(g["15"].inputs.model, ["7", 0], "HIGH LoRA reads the high-noise UNET");
  assert.deepEqual(g["8"].inputs.model, ["15", 0], "high-stage sampling reads the HIGH LoRA");
  assert.deepEqual(g["16"].inputs.model, ["9", 0], "LOW LoRA reads the low-noise UNET");
  assert.deepEqual(g["10"].inputs.model, ["16", 0], "low-stage sampling reads the LOW LoRA");
  // distilled recipe defaults: 4 steps, cfg 1.0, 2/2 split
  const samplers = Object.values(g).filter((n) => n.class_type === "KSamplerAdvanced");
  assert.ok(samplers.every((s) => s.inputs.steps === 4 && s.inputs.cfg === 1.0), "4 steps, cfg 1.0");
  assert.equal(samplers.find((s) => s.inputs.add_noise === "enable").inputs.end_at_step, 2, "2/2 split of 4");
});

test("hero mode: no distill LoRA, native step/cfg, samplers read the raw UNET", () => {
  const g = buildWan22I2V({ imagePath: "s.png", prompt: "a car on a coastal road", hero: true });
  assert.equal(Object.values(g).filter((n) => n.class_type === "LoraLoaderModelOnly").length, 0, "hero drops the LoRAs");
  assert.equal(g["8"].inputs.model[0], "7", "high-stage sampling reads the raw high-noise UNET");
  assert.equal(g["10"].inputs.model[0], "9", "low-stage sampling reads the raw low-noise UNET");
  const samplers = Object.values(g).filter((n) => n.class_type === "KSamplerAdvanced");
  assert.ok(samplers.every((s) => s.inputs.steps === 20 && s.inputs.cfg === 3.5), "hero bumps to native 20 steps / cfg 3.5");
  // an explicit steps override still wins in hero mode
  const g2 = buildWan22I2V({ imagePath: "s.png", prompt: "p", hero: true, steps: 12 });
  assert.ok(Object.values(g2).filter((n) => n.class_type === "KSamplerAdvanced").every((s) => s.inputs.steps === 12), "explicit steps wins over the hero bump");
});

test("upscale: model-based upscale + resize chained after decode; combine reads the last stage", () => {
  const g = buildWan22I2V({ imagePath: "s.png", prompt: "p", upscaleModel: "4x-UltraSharp.pth", upscaleWidth: 1920, upscaleHeight: 1080 });
  const loader = Object.values(g).find((n) => n.class_type === "UpscaleModelLoader");
  assert.equal(loader.inputs.model_name, "4x-UltraSharp.pth");
  const up = Object.values(g).find((n) => n.class_type === "ImageUpscaleWithModel");
  assert.deepEqual(up.inputs.image, ["13", 0], "upscale reads the decoded frames");
  const scale = Object.values(g).find((n) => n.class_type === "ImageScale");
  assert.equal(scale.inputs.width, 1920);
  assert.equal(scale.inputs.height, 1080);
  const combine = Object.values(g).find((n) => n.class_type === "VHS_VideoCombine");
  const scaleId = Object.entries(g).find(([, n]) => n.class_type === "ImageScale")[0];
  assert.deepEqual(combine.inputs.images, [scaleId, 0], "combine reads the upscaled+resized frames");
  // no upscale by default → combine reads the raw decode
  const plain = buildWan22I2V({ imagePath: "s.png", prompt: "p" });
  assert.deepEqual(Object.values(plain).find((n) => n.class_type === "VHS_VideoCombine").inputs.images, ["13", 0]);
  assert.equal(Object.values(plain).filter((n) => n.class_type === "ImageUpscaleWithModel").length, 0);
});
