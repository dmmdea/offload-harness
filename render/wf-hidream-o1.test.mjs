import { test } from "node:test";
import assert from "node:assert";
import { buildHiDreamO1 } from "./wf-hidream-o1.mjs";

const CKPT = "hidream_o1_image_bf16.safetensors";

test("base graph matches the official template: nodes, wiring, exact recipe", () => {
  const g = buildHiDreamO1({ prompt: "p", negative: "n", ckpt: CKPT, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const need of ["CheckpointLoaderSimple", "ModelNoiseScale", "HiDreamO1PatchSeamSmoothing",
    "BasicScheduler", "KSamplerSelect", "EmptyHiDreamO1LatentImage", "SamplerCustom", "VAEDecode", "SaveImage"]) {
    assert.ok(types.includes(need), `missing ${need}`);
  }
  assert.ok(!types.includes("KSampler"), "plain KSampler must NOT appear (official stack is SamplerCustom)");
  assert.ok(!types.includes("EmptyLatentImage"), "SD EmptyLatentImage must NOT appear (pixel-space latent)");
  // official base recipe
  const sched = Object.values(g).find((n) => n.class_type === "BasicScheduler");
  assert.equal(sched.inputs.steps, 40);
  assert.equal(sched.inputs.scheduler, "normal");
  assert.equal(sched.inputs.denoise, 1.0);
  const sel = Object.values(g).find((n) => n.class_type === "KSamplerSelect");
  assert.equal(sel.inputs.sampler_name, "dpmpp_2m_sde_gpu");
  const sc = Object.values(g).find((n) => n.class_type === "SamplerCustom");
  assert.equal(sc.inputs.cfg, 5);
  assert.equal(sc.inputs.add_noise, true);
  assert.equal(sc.inputs.noise_seed, 7);
  const ns = Object.values(g).find((n) => n.class_type === "ModelNoiseScale");
  assert.equal(ns.inputs.noise_scale, 8.0);
  // wiring: ckpt MODEL -> noise scale -> (scheduler + seam smoothing); sampler model = seam output
  assert.deepEqual(ns.inputs.model, ["4", 0]);
  assert.deepEqual(sched.inputs.model, ["20", 0]);
  const seam = Object.values(g).find((n) => n.class_type === "HiDreamO1PatchSeamSmoothing");
  assert.deepEqual(seam.inputs.model, ["20", 0]);
  assert.deepEqual(sc.inputs.model, ["21", 0]);
  // seam smoothing official values
  assert.equal(seam.inputs.start_percent, 0.8);
  assert.equal(seam.inputs.end_percent, 1.0);
  assert.equal(seam.inputs.pattern, "single_shift");
  assert.equal(seam.inputs.passes, "ramp_2_4");
  assert.equal(seam.inputs.blend, "median");
  assert.equal(seam.inputs.strength, 1.0);
  // decode reads SamplerCustom output 0 + the checkpoint's builtin VAE (pixel un-patchify)
  const dec = Object.values(g).find((n) => n.class_type === "VAEDecode");
  assert.deepEqual(dec.inputs.samples, ["3", 0]);
  assert.deepEqual(dec.inputs.vae, ["4", 2]);
  // native trained resolution default
  const lat = Object.values(g).find((n) => n.class_type === "EmptyHiDreamO1LatentImage");
  assert.equal(lat.inputs.width, 2048);
  assert.equal(lat.inputs.height, 2048);
});

test("dev graph: 28 steps cfg 1, SamplerLCM(1,1,2.5), noise_scale 7.6, NO seam smoothing", () => {
  const g = buildHiDreamO1({ prompt: "p", ckpt: "hidream_o1_image_dev_mxfp8.safetensors", variant: "dev", seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  assert.ok(!types.includes("HiDreamO1PatchSeamSmoothing"), "dev graph has no seam smoothing (official)");
  assert.ok(!types.includes("KSamplerSelect"), "dev uses SamplerLCM, not KSamplerSelect");
  const lcm = Object.values(g).find((n) => n.class_type === "SamplerLCM");
  assert.deepEqual([lcm.inputs.s_noise, lcm.inputs.s_noise_end, lcm.inputs.noise_clip_std], [1.0, 1.0, 2.5]);
  assert.equal(Object.values(g).find((n) => n.class_type === "BasicScheduler").inputs.steps, 28);
  const sc = Object.values(g).find((n) => n.class_type === "SamplerCustom");
  assert.equal(sc.inputs.cfg, 1);
  assert.deepEqual(sc.inputs.model, ["20", 0], "dev sampler reads the noise-scaled model directly");
  assert.equal(Object.values(g).find((n) => n.class_type === "ModelNoiseScale").inputs.noise_scale, 7.6);
});

test("overrides: steps/cfg/sampler/resolution win; dims snap to /32; floor guard", () => {
  const g = buildHiDreamO1({ prompt: "p", ckpt: CKPT, steps: 50, cfg: 4, sampler: "euler", width: 2500, height: 1447 });
  assert.equal(Object.values(g).find((n) => n.class_type === "BasicScheduler").inputs.steps, 50);
  assert.equal(Object.values(g).find((n) => n.class_type === "SamplerCustom").inputs.cfg, 4);
  assert.equal(Object.values(g).find((n) => n.class_type === "KSamplerSelect").inputs.sampler_name, "euler");
  const lat = Object.values(g).find((n) => n.class_type === "EmptyHiDreamO1LatentImage");
  assert.equal(lat.inputs.width, 2496, "2500 snaps down to /32");
  assert.equal(lat.inputs.height, 1440, "1447 snaps down to /32");
});

test("required args throw; unknown variant throws", () => {
  assert.throws(() => buildHiDreamO1({ ckpt: CKPT }), /prompt is required/);
  assert.throws(() => buildHiDreamO1({ prompt: "p" }), /ckpt is required/);
  assert.throws(() => buildHiDreamO1({ prompt: "p", ckpt: CKPT, variant: "fast" }), /unknown variant/);
});
