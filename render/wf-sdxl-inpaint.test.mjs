// node --test render/wf-sdxl-inpaint.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { buildSDXLInpaint } from "./wf-sdxl-inpaint.mjs";

const base = { image: "in.png", mask: "mask.png", prompt: "clean surface", ckpt: "sdxl.safetensors" };

test("graph shape: load, mask, encode-for-inpaint, ksampler, decode, save", () => {
  const g = buildSDXLInpaint({ ...base, seed: 7 });
  const types = Object.values(g).map((n) => n.class_type);
  for (const t of ["LoadImage", "ImageToMask", "VAEEncodeForInpaint", "KSampler", "VAEDecode", "SaveImage"]) {
    assert.ok(types.includes(t), t + " present");
  }
  const ks = Object.values(g).find((n) => n.class_type === "KSampler").inputs;
  assert.equal(ks.seed, 7);
  assert.equal(ks.denoise, 1.0, "default denoise 1.0 (VAEEncodeForInpaint expects full denoise)");
  const enc = Object.values(g).find((n) => n.class_type === "VAEEncodeForInpaint").inputs;
  assert.equal(enc.grow_mask_by, 16, "default mask growth blends seams");
});

test("two LoadImage nodes: image and mask files", () => {
  const g = buildSDXLInpaint({ ...base });
  const loads = Object.values(g).filter((n) => n.class_type === "LoadImage").map((n) => n.inputs.image);
  assert.deepEqual(loads.sort(), ["in.png", "mask.png"]);
});

test("vae builtin decodes+encodes from the checkpoint loader; standalone uses VAELoader", () => {
  const gb = buildSDXLInpaint({ ...base, vae: "builtin" });
  assert.ok(!Object.values(gb).some((n) => n.class_type === "VAELoader"), "builtin: no VAELoader");
  const gs = buildSDXLInpaint({ ...base, vae: "sdxl_vae.safetensors" });
  const vl = Object.values(gs).find((n) => n.class_type === "VAELoader");
  assert.equal(vl.inputs.vae_name, "sdxl_vae.safetensors");
});

test("required args throw", () => {
  assert.throws(() => buildSDXLInpaint({ ...base, image: "" }), /image/);
  assert.throws(() => buildSDXLInpaint({ ...base, mask: "" }), /mask/);
  assert.throws(() => buildSDXLInpaint({ ...base, prompt: "" }), /prompt/);
  assert.throws(() => buildSDXLInpaint({ ...base, ckpt: "" }), /ckpt/);
});
