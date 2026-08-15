import { test } from "node:test";
import assert from "node:assert";
import { buildLtx25I2V, LTX25_BASE_SIGMAS, LTX25_REFINE_SIGMAS, LTX25_TEMPLATE_NEGATIVE } from "./wf-ltx25-i2v.mjs";

test("builds the two-stage joint-AV graph faithful to the converted official template", () => {
  const g = buildLtx25I2V({ imagePath: "still.png", prompt: "a slow pan", seed: 42 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const need of [
    "LoadImage", "ResizeImageMaskNode", "LTXVPreprocess", "CLIPLoader",
    "LTXVConditioning", "EmptyLTXVLatentVideo", "LTXVImgToVideoInplace",
    "LTXVEmptyLatentAudio", "LTXVConcatAVLatent", "LTXVDualCFGGuider",
    "SamplerCustomAdvanced", "LTXVSeparateAVLatent", "LTXVLatentUpsampler",
    "VAEDecodeTiled", "LTXVAudioVAEDecode", "CreateVideo", "SaveVideo",
  ]) {
    assert.ok(types.includes(need), `graph must include ${need}`);
  }
  // BENCH DELTA: the prompt-enhancer branch is deleted, not switched off
  for (const banned of ["TextGenerateLTX2Prompt", "ComfySwitchNode", "PrimitiveBoolean", "ComfyMathExpression", "ResolutionSelector"]) {
    assert.ok(!types.includes(banned), `${banned} must not appear (enhancer/math branches are removed)`);
  }
  // text encoder loads with the ltxv CLIP type
  const clip = Object.values(g).find((n) => n.class_type === "CLIPLoader");
  assert.equal(clip.inputs.type, "ltxv");
  // BENCH DELTA: conv video VAE by default; audio VAE distinct
  const vaes = Object.values(g).filter((n) => n.class_type === "VAELoader").map((n) => n.inputs.vae_name);
  assert.ok(vaes.some((v) => /video-vae-conv/.test(v)), "conv video VAE is the default");
  assert.ok(vaes.some((v) => /audio-vae/.test(v)), "audio VAE present");
  // distilled recipe is pinned: fixed sigmas, dual CFG 1/1, euler_ancestral
  const sigmas = Object.values(g).filter((n) => n.class_type === "ManualSigmas").map((n) => n.inputs.sigmas);
  assert.ok(sigmas.includes(LTX25_BASE_SIGMAS) && sigmas.includes(LTX25_REFINE_SIGMAS));
  for (const guider of Object.values(g).filter((n) => n.class_type === "LTXVDualCFGGuider")) {
    assert.equal(guider.inputs.video_cfg, 1);
    assert.equal(guider.inputs.audio_cfg, 1);
  }
  for (const ks of Object.values(g).filter((n) => n.class_type === "KSamplerSelect")) {
    assert.equal(ks.inputs.sampler_name, "euler_ancestral");
  }
  // seeds: stage-1 = caller seed, refine = seed + 1 (template randomises both)
  const seeds = Object.values(g).filter((n) => n.class_type === "RandomNoise").map((n) => n.inputs.noise_seed).sort((a, b) => a - b);
  assert.deepEqual(seeds, [42, 43]);
  // arithmetic: defaults 1920x1088 → stage-1 latent at half; length 5*24+1
  const lat = Object.values(g).find((n) => n.class_type === "EmptyLTXVLatentVideo");
  assert.equal(lat.inputs.width, 960);
  assert.equal(lat.inputs.height, 544);
  assert.equal(lat.inputs.length, 121);
  const audio = Object.values(g).find((n) => n.class_type === "LTXVEmptyLatentAudio");
  assert.equal(audio.inputs.frames_number, 121);
  // stage-2 refine re-uses the STAGE-1 audio latent (separate output index 1)
  const sep1Id = Object.entries(g).find(([, n]) => n.class_type === "LTXVSeparateAVLatent")[0];
  const concats = Object.values(g).filter((n) => n.class_type === "LTXVConcatAVLatent");
  assert.ok(concats.some((c) => JSON.stringify(c.inputs.audio_latent) === JSON.stringify([sep1Id, 1])), "refine pass must re-use the stage-1 audio latent");
  // i2v inject strengths per template: 0.7 base, 1.0 refine, bypass pinned false
  const inj = Object.values(g).filter((n) => n.class_type === "LTXVImgToVideoInplace");
  assert.deepEqual(inj.map((n) => n.inputs.strength).sort(), [0.7, 1.0]);
  for (const n of inj) assert.equal(n.inputs.bypass, false);
  // default negative = the template's own
  const negNode = Object.values(g).filter((n) => n.class_type === "CLIPTextEncode").find((n) => n.inputs.text === LTX25_TEMPLATE_NEGATIVE);
  assert.ok(negNode, "template negative applies when caller passes none");
  // joint AV mux: CreateVideo carries both frames and audio at the graph fps
  const cv = Object.values(g).find((n) => n.class_type === "CreateVideo");
  assert.equal(cv.inputs.fps, 24);
  assert.ok(cv.inputs.images && cv.inputs.audio);
});

test("pooled loading: DisTorch2 ratio mode when poolVvramGb > 0, plain UNETLoader otherwise", () => {
  const pooled = buildLtx25I2V({ imagePath: "s.png", prompt: "p", poolVvramGb: 12, poolCompute: "cuda:0", poolDonor: "cuda:1" });
  const pl = Object.values(pooled).find((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU");
  assert.ok(pl, "pooled build loads through UNETLoaderDisTorch2MultiGPU");
  assert.equal(pl.inputs.virtual_vram_gb, 12);
  assert.equal(pl.inputs.expert_mode_allocations, "", "ratio mode only — expert strings are a silent no-op");
  assert.equal(pl.inputs.donor_device, "cuda:1");
  const plain = buildLtx25I2V({ imagePath: "s.png", prompt: "p" });
  assert.ok(Object.values(plain).find((n) => n.class_type === "UNETLoader"), "unpooled build uses the plain loader");
  assert.ok(!Object.values(plain).find((n) => n.class_type === "UNETLoaderDisTorch2MultiGPU"));
});

test("guards: required inputs and dimension alignment", () => {
  assert.throws(() => buildLtx25I2V({ prompt: "p" }), /imagePath/);
  assert.throws(() => buildLtx25I2V({ imagePath: "s.png" }), /prompt/);
  assert.throws(() => buildLtx25I2V({ imagePath: "s.png", prompt: "p", poolVvramGb: -1 }), /poolVvramGb/);
  // /32 alignment on final dims (ResolutionSelector multiple=32 semantics)
  const g = buildLtx25I2V({ imagePath: "s.png", prompt: "p", width: 1930, height: 1090 });
  const lat = Object.values(g).find((n) => n.class_type === "EmptyLTXVLatentVideo");
  assert.equal(lat.inputs.width, 1920 / 2);
  assert.equal(lat.inputs.height, 1088 / 2);
  // explicit frame count wins over seconds*fps+1
  const g2 = buildLtx25I2V({ imagePath: "s.png", prompt: "p", length: 49 });
  assert.equal(Object.values(g2).find((n) => n.class_type === "EmptyLTXVLatentVideo").inputs.length, 49);
});
