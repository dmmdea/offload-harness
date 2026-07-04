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
