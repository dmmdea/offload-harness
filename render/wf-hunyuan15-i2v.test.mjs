import { test } from "node:test";
import assert from "node:assert";
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";

test("builds the 1.5 I2V graph wired to the LIVE schema (reconciled wso07xgs5)", () => {
  // build with DEFAULTS for steps/wiring so the test guards the corrected defaults
  const g = buildHunyuan15I2V({
    imagePath: "still.png", prompt: "slow cinematic push-in on a sports car",
    width: 480, height: 848, length: 33, seed: 42,
  });
  const types = Object.values(g).map((n) => n.class_type);
  for (const need of ["LoadImage", "UnetLoaderGGUF", "HunyuanVideo15ImageToVideo", "CLIPVisionLoader", "ModelSamplingSD3", "VAEDecodeTiled", "VHS_VideoCombine"]) {
    assert.ok(types.includes(need), `graph must include ${need}; got ${types.join(",")}`);
  }
  // HARD fix: the 1.5 path must NOT use the legacy llama text-encode node
  assert.ok(!types.includes("TextEncodeHunyuanVideo_ImageToVideo"), "1.5 uses plain CLIPTextEncode, not the legacy 1.0 encoder");
  // HARD fix: DualCLIPLoader must use the 1.5-specific type
  const dual = Object.values(g).find((n) => n.class_type === "DualCLIPLoader");
  assert.equal(dual.inputs.type, "hunyuan_video_15");
  // HARD fix: cfg_distilled unet → CFG=1 + steps 50 (distilled removes the CFG pass, NOT the steps)
  const unet = Object.values(g).find((n) => n.class_type === "UnetLoaderGGUF");
  assert.match(unet.inputs.unet_name, /cfg_distilled/);
  const ks = Object.values(g).find((n) => n.class_type === "KSampler");
  assert.equal(ks.inputs.cfg, 1);
  assert.equal(ks.inputs.steps, 50);
  // SOFT fix: KSampler model must come from ModelSamplingSD3, not the raw unet
  const msId = Object.entries(g).find(([, n]) => n.class_type === "ModelSamplingSD3")[0];
  assert.deepEqual(ks.inputs.model, [msId, 0]);
  // HARD fix (verifier-caught): HunyuanVideo15ImageToVideo requires batch_size
  const i2v = Object.values(g).find((n) => n.class_type === "HunyuanVideo15ImageToVideo");
  assert.ok(i2v.inputs.batch_size !== undefined, "HunyuanVideo15ImageToVideo requires batch_size");
  // SOFT fix: sigclip center-crop; and the still is wired in
  const cve = Object.values(g).find((n) => n.class_type === "CLIPVisionEncode");
  assert.equal(cve.inputs.crop, "center");
  const load = Object.values(g).find((n) => n.class_type === "LoadImage");
  assert.equal(load.inputs.image, "still.png");
  // HARD fix (caught at live submit): VHS_VideoCombine requires pingpong, else 400 validation
  const vhs = Object.values(g).find((n) => n.class_type === "VHS_VideoCombine");
  assert.ok(vhs.inputs.pingpong !== undefined, "VHS_VideoCombine requires pingpong");
  assert.equal(vhs.inputs.format, "video/h264-mp4");
});

test("an explicit steps override is still honored", () => {
  const g = buildHunyuan15I2V({ imagePath: "s.png", prompt: "x", steps: 30 });
  const ks = Object.values(g).find((n) => n.class_type === "KSampler");
  assert.equal(ks.inputs.steps, 30);
});

test("requires imagePath and prompt", () => {
  assert.throws(() => buildHunyuan15I2V({ prompt: "x" }), /imagePath/);
  assert.throws(() => buildHunyuan15I2V({ imagePath: "x.png" }), /prompt/);
});
