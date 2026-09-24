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

test("weight binding: .safetensors unets go native by default (auto); loader:gguf-distorch keeps the old wrapped path", () => {
  // auto (default, gap: bigger-models-2026-09-24.md "Interim Phase 2 round 2" item
  // 1 — the builder used to wrap EVERY unet, including safetensors, in the
  // DisTorch2/MultiGPU node; auto now goes native for safetensors instead, same as
  // the Qwen builders' own .gguf-vs-.safetensors branch).
  const g = buildWan22I2V({
    imagePath: "s.png", prompt: "p",
    highUnet: "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
    lowUnet: "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
    virtualVramGb: 9.5, // ignored by the native loader — no virtual_vram input on it
  });
  assert.equal(Object.values(g).filter((n) => n.class_type === "UnetLoaderGGUFDisTorch2MultiGPU").length, 0, "no GGUF loader for safetensors weights");
  assert.equal(Object.values(g).filter((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU").length, 0, "auto no longer wraps safetensors in DisTorch2/MultiGPU");
  const native = Object.values(g).filter((n) => n.class_type === "UNETLoader");
  assert.equal(native.length, 2, "plain native loader per safetensors expert");
  assert.ok(native.every((l) => l.inputs.weight_dtype === "default"), "no dtype down-cast — quality-first");
  // mixed case (auto) still splits correctly per expert: gguf keeps the wrapper, safetensors goes native
  const mixed = buildWan22I2V({ imagePath: "s.png", prompt: "p", highUnet: "high.safetensors", lowUnet: "low.gguf" });
  assert.equal(Object.values(mixed).filter((n) => n.class_type === "UNETLoader").length, 1);
  assert.equal(Object.values(mixed).filter((n) => n.class_type === "UnetLoaderGGUFDisTorch2MultiGPU").length, 1);
  // explicit loader:gguf-distorch is the back-compat escape hatch: same safetensors
  // files, forced onto the historical DisTorch2/MultiGPU wrapper, offload params intact.
  const wrapped = buildWan22I2V({
    imagePath: "s.png", prompt: "p", loader: "gguf-distorch",
    highUnet: "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
    lowUnet: "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
    virtualVramGb: 9.5,
  });
  const wrappedLoaders = Object.values(wrapped).filter((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU");
  assert.equal(wrappedLoaders.length, 2, "gguf-distorch forces the safetensors DisTorch2 loader per expert");
  assert.ok(wrappedLoaders.every((l) => l.inputs.virtual_vram_gb === 9.5 && l.inputs.donor_device === "cpu"), "offload params preserved");
  assert.ok(wrappedLoaders.every((l) => l.inputs.weight_dtype === "default"), "no dtype down-cast — quality-first");
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

test("loader:auto (default) — safetensors experts go native (plain UNETLoader, no DisTorch2/MultiGPU)", () => {
  const g = buildWan22I2V({
    imagePath: "s.png", prompt: "p",
    highUnet: "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
    lowUnet: "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
  });
  const loaders = Object.values(g).filter((n) => n.class_type === "UNETLoader");
  assert.equal(loaders.length, 2, "both safetensors experts load through the plain native loader");
  assert.ok(loaders.every((l) => l.inputs.weight_dtype === "default"));
  assert.equal(Object.values(g).filter((n) => /DisTorch2/.test(n.class_type)).length, 0, "no DisTorch2/MultiGPU node at all — native, no virtual_vram");
  // GGUF experts still keep the historical wrapper under auto (unchanged default behavior)
  const gguf = buildWan22I2V({ imagePath: "s.png", prompt: "p" }); // default filenames are .gguf
  assert.equal(Object.values(gguf).filter((n) => n.class_type === "UnetLoaderGGUFDisTorch2MultiGPU").length, 2);
});

test("loader:native forces the plain loader even for a mixed-extension pair, and refuses a .gguf expert", () => {
  const g = buildWan22I2V({
    imagePath: "s.png", prompt: "p", loader: "native",
    highUnet: "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
    lowUnet: "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
  });
  assert.equal(Object.values(g).filter((n) => n.class_type === "UNETLoader").length, 2);
  assert.throws(
    () => buildWan22I2V({ imagePath: "s.png", prompt: "p", loader: "native", highUnet: "wan2.2_i2v_high_noise_14B_Q4_K_S.gguf" }),
    /loader:"native" cannot load a GGUF expert/,
  );
});

test("loader:gguf-distorch forces the DisTorch2/MultiGPU wrapper even on safetensors experts (historical path, still available)", () => {
  const g = buildWan22I2V({
    imagePath: "s.png", prompt: "p", loader: "gguf-distorch",
    highUnet: "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors",
    lowUnet: "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors",
    virtualVramGb: 9.5,
  });
  const loaders = Object.values(g).filter((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU");
  assert.equal(loaders.length, 2, "forced wrapper on both safetensors experts");
  assert.ok(loaders.every((l) => l.inputs.virtual_vram_gb === 9.5));
});

test("loader: an unrecognized value is refused rather than silently defaulting", () => {
  assert.throws(
    () => buildWan22I2V({ imagePath: "s.png", prompt: "p", loader: "bogus" }),
    /loader must be auto\|native\|gguf-distorch/,
  );
});

test("loader:native works with --fast (lightx2v LoRA still applies) and with post-decode upscale", () => {
  const gFast = buildWan22I2V({
    imagePath: "s.png", prompt: "p", loader: "native", fast: true,
    highUnet: "high.safetensors", lowUnet: "low.safetensors",
  });
  const loras = Object.values(gFast).filter((n) => n.class_type === "LoraLoaderModelOnly");
  assert.equal(loras.length, 2, "LoRA loaders still attach on top of the native raw UNETs");
  assert.deepEqual(gFast["15"].inputs.model, ["7", 0]);
  assert.deepEqual(gFast["16"].inputs.model, ["9", 0]);
  const gUpscale = buildWan22I2V({
    imagePath: "s.png", prompt: "p", loader: "native",
    highUnet: "high.safetensors", lowUnet: "low.safetensors",
    upscaleModel: "4x-UltraSharp.pth", upscaleWidth: 1920, upscaleHeight: 1080,
  });
  assert.ok(Object.values(gUpscale).find((n) => n.class_type === "UpscaleModelLoader"), "upscale chain still builds under the native loader");
});
