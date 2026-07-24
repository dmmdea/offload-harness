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

test("DEFAULT is the NATIVE quality path: no distill LoRA, 20 steps, cfg 3.5, official negative", () => {
  // Quality-first (operator directive, 2026-07-16): speed variants are opt-in, never the default.
  const g = buildWan22I2V({ imagePath: "s.png", prompt: "a red bike rolling", seed: 7 });
  assert.equal(Object.values(g).filter((n) => n.class_type === "LoraLoaderModelOnly").length, 0, "default has NO distill LoRAs");
  assert.equal(g["8"].inputs.model[0], "7", "high-stage sampling reads the raw high-noise UNET");
  assert.equal(g["10"].inputs.model[0], "9", "low-stage sampling reads the raw low-noise UNET");
  const samplers = Object.values(g).filter((n) => n.class_type === "KSamplerAdvanced");
  assert.ok(samplers.every((s) => s.inputs.steps === 20 && s.inputs.cfg === 3.5), "native 20 steps / cfg 3.5 (ComfyUI official; Wan-AI ref cfg 3.5)");
  assert.equal(samplers.find((s) => s.inputs.add_noise === "enable").inputs.end_at_step, 10, "50/50 expert split");
  // the model's official (training-time) Chinese negative is the default when none given
  const negNode = Object.values(g).filter((n) => n.class_type === "CLIPTextEncode")[1];
  assert.ok(/色调艳丽/.test(negNode.inputs.text), "official Wan negative applied by default");
  // an explicit negative wins
  const g2 = buildWan22I2V({ imagePath: "s.png", prompt: "p", negative: "blurry" });
  assert.equal(Object.values(g2).filter((n) => n.class_type === "CLIPTextEncode")[1].inputs.text, "blurry");
  // hero flag is accepted for backward compat and equals the default
  const g3 = buildWan22I2V({ imagePath: "s.png", prompt: "p", hero: true });
  assert.equal(Object.values(g3).filter((n) => n.class_type === "LoraLoaderModelOnly").length, 0);
});

test("fast: OPT-IN 8-step lightx2v path — asymmetric per-expert recipe (research-validated)", () => {
  const g = buildWan22I2V({ imagePath: "s.png", prompt: "a red bike rolling", seed: 7, fast: true });
  // two LoRA loaders, one per expert: HIGH at 0.7 (restores motion), LOW at 1.0
  const loras = Object.values(g).filter((n) => n.class_type === "LoraLoaderModelOnly");
  assert.equal(loras.length, 2, "one LoRA per expert");
  const high = Object.values(g).find((n) => n.class_type === "LoraLoaderModelOnly" && /HIGH_lightx2v_4step/.test(n.inputs.lora_name));
  const low = Object.values(g).find((n) => n.class_type === "LoraLoaderModelOnly" && /LOW_lightx2v_4step/.test(n.inputs.lora_name));
  assert.equal(high.inputs.strength_model, 0.7, "HIGH LoRA weakened to 0.7");
  assert.equal(low.inputs.strength_model, 1.0, "LOW LoRA full strength");
  // topology: HIGH LoRA reads high-noise UNET -> high-stage sampling; LOW likewise
  assert.deepEqual(g["15"].inputs.model, ["7", 0]);
  assert.deepEqual(g["8"].inputs.model, ["15", 0]);
  assert.deepEqual(g["16"].inputs.model, ["9", 0]);
  assert.deepEqual(g["10"].inputs.model, ["16", 0]);
  // 8 steps (4+4), asymmetric cfg: high 3.0 (real guidance for motion), low 1.0 (distilled)
  const samplers = Object.values(g).filter((n) => n.class_type === "KSamplerAdvanced");
  assert.ok(samplers.every((s) => s.inputs.steps === 8), "8 steps total");
  const highS = samplers.find((s) => s.inputs.add_noise === "enable");
  const lowS = samplers.find((s) => s.inputs.add_noise === "disable");
  assert.equal(highS.inputs.end_at_step, 4, "4+4 split");
  assert.equal(highS.inputs.cfg, 3.0, "high-noise expert keeps real guidance");
  assert.equal(lowS.inputs.cfg, 1.0, "low-noise expert runs distilled cfg 1");
  // explicit steps/cfg overrides still win in fast mode
  const g2 = buildWan22I2V({ imagePath: "s.png", prompt: "p", fast: true, steps: 12, cfg: 2.0 });
  assert.ok(Object.values(g2).filter((n) => n.class_type === "KSamplerAdvanced").every((s) => s.inputs.steps === 12));
  assert.ok(Object.values(g2).filter((n) => n.class_type === "KSamplerAdvanced").every((s) => s.inputs.cfg === 2.0));
});

test("weight binding: custom unet/text-encoder names plumb through (quality-first per-machine weights)", () => {
  const g = buildWan22I2V({
    imagePath: "s.png", prompt: "p",
    highUnet: "wan2.2_i2v_high_noise_14B_Q8_0.gguf",
    lowUnet: "wan2.2_i2v_low_noise_14B_Q8_0.gguf",
    textEncoder: "umt5_xxl_fp16.safetensors",
  });
  const loaders = Object.values(g).filter((n) => n.class_type === "UnetLoaderGGUFDisTorch2MultiGPU");
  assert.equal(loaders.length, 2);
  assert.ok(loaders.some((l) => l.inputs.unet_name === "wan2.2_i2v_high_noise_14B_Q8_0.gguf"));
  assert.ok(loaders.some((l) => l.inputs.unet_name === "wan2.2_i2v_low_noise_14B_Q8_0.gguf"));
  const clip = Object.values(g).find((n) => n.class_type === "CLIPLoader");
  assert.equal(clip.inputs.clip_name, "umt5_xxl_fp16.safetensors");
});

test("weight binding: .safetensors unets use the safetensors DisTorch2 loader (same offload)", () => {
  const g = buildWan22I2V({
    imagePath: "s.png", prompt: "p",
    highUnet: "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
    lowUnet: "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
    virtualVramGb: 9.5,
  });
  assert.equal(Object.values(g).filter((n) => n.class_type === "UnetLoaderGGUFDisTorch2MultiGPU").length, 0, "no GGUF loader for safetensors weights");
  const loaders = Object.values(g).filter((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU");
  assert.equal(loaders.length, 2, "safetensors DisTorch2 loader per expert");
  assert.ok(loaders.every((l) => l.inputs.virtual_vram_gb === 9.5 && l.inputs.donor_device === "cpu"), "offload params preserved");
  assert.ok(loaders.every((l) => l.inputs.weight_dtype === "default"), "no dtype down-cast — quality-first");
  // mixed case still splits correctly per expert
  const mixed = buildWan22I2V({ imagePath: "s.png", prompt: "p", highUnet: "high.safetensors", lowUnet: "low.gguf" });
  assert.equal(Object.values(mixed).filter((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU").length, 1);
  assert.equal(Object.values(mixed).filter((n) => n.class_type === "UnetLoaderGGUFDisTorch2MultiGPU").length, 1);
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

test("COMFY_COMPUTE_DEVICE overrides the DisTorch2 compute device (J4 seam); unset = cuda:0", () => {
  process.env.COMFY_COMPUTE_DEVICE = "xpu:0";
  let g;
  try {
    g = buildWan22I2V({ imagePath: "in.png", prompt: "p" });
  } finally {
    delete process.env.COMFY_COMPUTE_DEVICE;
  }
  assert.equal(g["7"].inputs.compute_device, "xpu:0");
  assert.equal(g["9"].inputs.compute_device, "xpu:0");
  const g2 = buildWan22I2V({ imagePath: "in.png", prompt: "p" });
  assert.equal(g2["7"].inputs.compute_device, "cuda:0");
});
