import { test } from "node:test";
import assert from "node:assert";
import { buildWanAnimate2, WAN_ANIMATE2_TEMPLATE_NEGATIVE } from "./wf-wan-animate2.mjs";

test("builds the single-chunk Motion Transfer graph faithful to the official template", () => {
  const g = buildWanAnimate2({ refImagePath: "ref.png", driverVideoPath: "drive.mp4", prompt: "a toy astronaut", seed: 42 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const need of [
    "UNETLoader", "CLIPLoader", "CLIPTextEncode", "CLIPVisionLoader",
    "VAELoader", "LoadImage", "LoadVideo", "GetVideoComponents",
    "ResizeImageMaskNode", "GetImageSize", "CLIPVisionEncode",
    "ImageFromBatch", "WanAnimate2ToVideo", "WanAnimate2Cache",
    "ModelSamplingSD3", "BasicScheduler", "KSamplerSelect", "SamplerCustom",
    "TrimVideoLatent", "VAEDecode", "CreateVideo", "SaveVideo",
  ]) {
    assert.ok(types.includes(need), `graph must include ${need}`);
  }
  // deltas: switch/context-window/stitch branches are folded out, not carried inert
  for (const banned of ["ComfySwitchNode", "PrimitiveBoolean", "ContextWindowsManual", "ImageStitch", "ComfyMathExpression", "BatchImagesNode"]) {
    assert.ok(!types.includes(banned), `${banned} must not appear (single-chunk fold)`);
  }
  // text encoder loads with the wan CLIP type
  const clip = Object.values(g).find((n) => n.class_type === "CLIPLoader");
  assert.equal(clip.inputs.type, "wan");
  // distilled recipe pinned: 10-step simple schedule, lcm, cfg 1, shift 5
  const sched = Object.values(g).find((n) => n.class_type === "BasicScheduler");
  assert.equal(sched.inputs.steps, 10);
  assert.equal(sched.inputs.scheduler, "simple");
  assert.equal(Object.values(g).find((n) => n.class_type === "KSamplerSelect").inputs.sampler_name, "lcm");
  const sampler = Object.values(g).find((n) => n.class_type === "SamplerCustom");
  assert.equal(sampler.inputs.cfg, 1.0);
  assert.equal(sampler.inputs.noise_seed, 42);
  assert.equal(Object.values(g).find((n) => n.class_type === "ModelSamplingSD3").inputs.shift, 5.0);
  // CRASH GUARD: pose cache defaults to cpu/default, never the template's gpu/int8
  const cache = Object.values(g).find((n) => n.class_type === "WanAnimate2Cache");
  assert.equal(cache.inputs.device, "cpu");
  assert.equal(cache.inputs.dtype, "default");
  // both scheduler and sampling-shift patch hang off the CACHED model, per template
  const cacheId = Object.entries(g).find(([, n]) => n.class_type === "WanAnimate2Cache")[0];
  assert.deepEqual(sched.inputs.model, [cacheId, 0]);
  // the sampler's latent is trimmed of cache frames before decode, per template
  const trim = Object.values(g).find((n) => n.class_type === "TrimVideoLatent");
  const animateId = Object.entries(g).find(([, n]) => n.class_type === "WanAnimate2ToVideo")[0];
  assert.deepEqual(trim.inputs.trim_amount, [animateId, 3]);
  // working dims flow from the RESIZED DRIVER via GetImageSize (ref auto-matches)
  const animate = Object.values(g).find((n) => n.class_type === "WanAnimate2ToVideo");
  const sizeId = Object.entries(g).find(([, n]) => n.class_type === "GetImageSize")[0];
  assert.deepEqual(animate.inputs.width, [sizeId, 0]);
  assert.deepEqual(animate.inputs.height, [sizeId, 1]);
  assert.equal(animate.inputs.length, 81);
  // default negative = the template's own
  const neg = Object.values(g).filter((n) => n.class_type === "CLIPTextEncode").find((n) => n.inputs.text === WAN_ANIMATE2_TEMPLATE_NEGATIVE);
  assert.ok(neg, "template negative applies when caller passes none");
  // mux keeps the driver's audio + fps (GetVideoComponents outputs 1 and 2)
  const cv = Object.values(g).find((n) => n.class_type === "CreateVideo");
  const compId = Object.entries(g).find(([, n]) => n.class_type === "GetVideoComponents")[0];
  assert.deepEqual(cv.inputs.audio, [compId, 1]);
  assert.deepEqual(cv.inputs.fps, [compId, 2]);
});

test("guards: required inputs; cache override possible but never implicit", () => {
  assert.throws(() => buildWanAnimate2({ driverVideoPath: "d.mp4", prompt: "p" }), /refImagePath/);
  assert.throws(() => buildWanAnimate2({ refImagePath: "r.png", prompt: "p" }), /driverVideoPath/);
  assert.throws(() => buildWanAnimate2({ refImagePath: "r.png", driverVideoPath: "d.mp4" }), /prompt/);
  assert.throws(() => buildWanAnimate2({ refImagePath: "r.png", driverVideoPath: "d.mp4", prompt: "p", length: 0 }), /length/);
  // an explicit caller override is honored (the crash note travels in the header)
  const g = buildWanAnimate2({ refImagePath: "r.png", driverVideoPath: "d.mp4", prompt: "p", cacheDevice: "gpu", cacheDtype: "int8" });
  const cache = Object.values(g).find((n) => n.class_type === "WanAnimate2Cache");
  assert.equal(cache.inputs.device, "gpu");
});
