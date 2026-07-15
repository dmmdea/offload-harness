import { test } from "node:test";
import assert from "node:assert";
import { buildAceStep } from "./wf-acestep.mjs";

test("builds the ACE-Step v1.5 split text-to-music graph wired to the live schema", () => {
  const g = buildAceStep({ prompt: "upbeat corporate, light electronic, 120 bpm", seconds: 30, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  // v1.5 is a SPLIT stack: separate UNET / DualCLIP (type ace) / VAE loaders, the 1.5
  // encoder + latent nodes, AuraFlow model-sampling, and a zeroed-out negative.
  for (const need of ["UNETLoader", "VAELoader", "DualCLIPLoader", "TextEncodeAceStepAudio1.5", "ConditioningZeroOut", "EmptyAceStep1.5LatentAudio", "ModelSamplingAuraFlow", "KSampler", "VAEDecodeAudio", "SaveAudio"]) {
    assert.ok(types.includes(need), `graph must include ${need}; got ${types.join(",")}`);
  }
  // must NOT carry the retired v1 all-in-one nodes
  for (const gone of ["CheckpointLoaderSimple", "TextEncodeAceStepAudio", "EmptyAceStepLatentAudio", "ModelSamplingSD3"]) {
    assert.ok(!types.includes(gone), `graph must NOT include the retired v1 node ${gone}`);
  }
  // exactly one encoder (positive); the negative is a ConditioningZeroOut of it
  const encs = Object.values(g).filter((n) => n.class_type === "TextEncodeAceStepAudio1.5");
  assert.equal(encs.length, 1, "one v1.5 encoder");
  assert.ok(encs[0].inputs.tags.includes("corporate"), "positive carries the tags");
  assert.ok(encs[0].inputs.keyscale, "keyscale must be supplied (no schema default)");
  const encId = Object.entries(g).find(([, n]) => n.class_type === "TextEncodeAceStepAudio1.5")[0];
  const zero = Object.values(g).find((n) => n.class_type === "ConditioningZeroOut");
  assert.deepEqual(zero.inputs.conditioning, [encId, 0], "negative zeroes out the positive conditioning");
  // seconds wired into the empty latent
  const lat = Object.values(g).find((n) => n.class_type === "EmptyAceStep1.5LatentAudio");
  assert.equal(lat.inputs.seconds, 30);
  // KSampler model comes from ModelSamplingAuraFlow, not the raw UNET
  const msId = Object.entries(g).find(([, n]) => n.class_type === "ModelSamplingAuraFlow")[0];
  const ks = Object.values(g).find((n) => n.class_type === "KSampler");
  assert.deepEqual(ks.inputs.model, [msId, 0]);
  // VAEDecodeAudio uses the standalone VAELoader (node 2), not a checkpoint-bundled VAE
  const vaeId = Object.entries(g).find(([, n]) => n.class_type === "VAELoader")[0];
  const dec = Object.values(g).find((n) => n.class_type === "VAEDecodeAudio");
  assert.deepEqual(dec.inputs.vae, [vaeId, 0]);
});

test("requires a prompt", () => {
  assert.throws(() => buildAceStep({ seconds: 10 }), /prompt/);
});
