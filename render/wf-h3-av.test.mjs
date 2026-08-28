import { test } from "node:test";
import assert from "node:assert";
import { buildH3AV, h3FramesFor } from "./wf-h3-av.mjs";

test("frame arithmetic matches the template formula (frames % 17 == 5, min 5)", () => {
  assert.equal(h3FramesFor(5), 124);   // template default: 5s -> 124
  assert.equal(h3FramesFor(0), 5);     // floor
  for (const s of [1, 2, 3.4, 5, 8]) {
    const f = h3FramesFor(s);
    assert.equal(f % 17, 5, `frames(${s})=${f} must satisfy %17==5`);
  }
});

test("t2v graph: turbo-8 verdict recipe by default, faithful node set", () => {
  const g = buildH3AV({ prompt: "a storyboard", seed: 42 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const need of [
    "UNETLoader", "CLIPLoader", "VAELoader", "KSamplerSelect", "RandomNoise",
    "LoraLoaderModelOnly", "MiniMaxH3ImageToVideo", "BasicScheduler",
    "BasicGuider", "SamplerCustomAdvanced", "VAEDecode", "VAEDecodeAudio",
    "CreateVideo", "SaveVideo",
  ]) {
    assert.ok(types.includes(need), `graph must include ${need}`);
  }
  // V3 dynamic nodes are computed here, never serialized
  for (const banned of ["ResolutionSelector", "ComfyMathExpression", "ComfySwitchNode", "PrimitiveBoolean"]) {
    assert.ok(!types.includes(banned), `${banned} must not appear`);
  }
  // native loader ONLY — DisTorch upcasts int8 to bf16 (measured; see header)
  assert.ok(!types.includes("UNETLoaderDisTorch2MultiGPU"), "no pooled loader for h3");
  // turbo default: LoRA in the model path, 8 steps; scheduler+guider hang off the LoRA'd model
  const sched = Object.values(g).find((n) => n.class_type === "BasicScheduler");
  assert.equal(sched.inputs.steps, 8);
  const loraId = Object.entries(g).find(([, n]) => n.class_type === "LoraLoaderModelOnly")[0];
  assert.deepEqual(sched.inputs.model, [loraId, 0]);
  assert.deepEqual(Object.values(g).find((n) => n.class_type === "BasicGuider").inputs.model, [loraId, 0]);
  // recipe pins: res_multistep + simple + denoise 1
  assert.equal(Object.values(g).find((n) => n.class_type === "KSamplerSelect").inputs.sampler_name, "res_multistep");
  assert.equal(sched.inputs.scheduler, "simple");
  // clip type minimax; joint AV mux at 24fps
  assert.equal(Object.values(g).find((n) => n.class_type === "CLIPLoader").inputs.type, "minimax");
  const cv = Object.values(g).find((n) => n.class_type === "CreateVideo");
  assert.equal(cv.inputs.fps, 24);
  assert.ok(cv.inputs.images && cv.inputs.audio);
  // t2v: no LoadImage, no first_frame
  assert.ok(!types.includes("LoadImage"));
  assert.equal(Object.values(g).find((n) => n.class_type === "MiniMaxH3ImageToVideo").inputs.first_frame, undefined);
  // dims ÷32-aligned from the 1280x736 default; frames 124
  const c = Object.values(g).find((n) => n.class_type === "MiniMaxH3ImageToVideo").inputs;
  assert.equal(c.width, 1280);
  assert.equal(c.height, 736);
  assert.equal(c.length, 124);
});

test("i2v wires the still; hero drops the LoRA and runs 20 steps", () => {
  const g = buildH3AV({ prompt: "p", imagePath: "still.png", hero: true, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  assert.ok(types.includes("LoadImage"));
  const loadId = Object.entries(g).find(([, n]) => n.class_type === "LoadImage")[0];
  assert.deepEqual(Object.values(g).find((n) => n.class_type === "MiniMaxH3ImageToVideo").inputs.first_frame, [loadId, 0]);
  assert.ok(!types.includes("LoraLoaderModelOnly"), "hero = the non-LoRA path");
  const sched = Object.values(g).find((n) => n.class_type === "BasicScheduler");
  assert.equal(sched.inputs.steps, 20);
  assert.deepEqual(sched.inputs.model, ["127", 0]);
});

test("guards: prompt required, dims aligned, negative rejected loudly", () => {
  assert.throws(() => buildH3AV({}), /prompt/);
  assert.throws(() => buildH3AV({ prompt: "p", negative: "blurry" }), /negative/);
  const g = buildH3AV({ prompt: "p", width: 1290, height: 710 });
  const c = Object.values(g).find((n) => n.class_type === "MiniMaxH3ImageToVideo").inputs;
  assert.equal(c.width, 1280);
  assert.equal(c.height, 704);
  // explicit frames win over seconds
  const g2 = buildH3AV({ prompt: "p", length: 49 });
  assert.equal(Object.values(g2).find((n) => n.class_type === "MiniMaxH3ImageToVideo").inputs.length, 49);
});
