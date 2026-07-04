import { test } from "node:test";
import assert from "node:assert";
import { buildAceStep } from "./wf-acestep.mjs";

test("builds the ACE-Step text-to-music graph wired to the live schema", () => {
  const g = buildAceStep({ prompt: "upbeat corporate, light electronic, 120 bpm", seconds: 30, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const need of ["CheckpointLoaderSimple", "ModelSamplingSD3", "TextEncodeAceStepAudio", "EmptyAceStepLatentAudio", "KSampler", "VAEDecodeAudio", "SaveAudio"]) {
    assert.ok(types.includes(need), `graph must include ${need}; got ${types.join(",")}`);
  }
  // two TextEncodeAceStepAudio: positive (tags) + negative (empty)
  const encs = Object.values(g).filter((n) => n.class_type === "TextEncodeAceStepAudio");
  assert.equal(encs.length, 2);
  assert.ok(encs.some((e) => e.inputs.tags.includes("corporate")), "positive carries the tags");
  assert.ok(encs.some((e) => e.inputs.tags === ""), "negative is empty");
  // seconds wired into the empty latent
  const lat = Object.values(g).find((n) => n.class_type === "EmptyAceStepLatentAudio");
  assert.equal(lat.inputs.seconds, 30);
  // KSampler model from ModelSamplingSD3, not the raw checkpoint
  const msId = Object.entries(g).find(([, n]) => n.class_type === "ModelSamplingSD3")[0];
  const ks = Object.values(g).find((n) => n.class_type === "KSampler");
  assert.deepEqual(ks.inputs.model, [msId, 0]);
  // VAEDecodeAudio uses the checkpoint's bundled VAE ([1,2])
  const dec = Object.values(g).find((n) => n.class_type === "VAEDecodeAudio");
  assert.deepEqual(dec.inputs.vae, ["1", 2]);
});

test("requires a prompt", () => {
  assert.throws(() => buildAceStep({ seconds: 10 }), /prompt/);
});
