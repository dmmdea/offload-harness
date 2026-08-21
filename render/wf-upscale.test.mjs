import { test } from "node:test";
import assert from "node:assert";
import { buildUpscale, nativeFactor } from "./wf-upscale.mjs";

const types = (g) => Object.values(g).map((n) => n.class_type);
const find = (g, t) => Object.values(g).find((n) => n.class_type === t);

test("native pass: LoadImage -> UpscaleModelLoader -> ImageUpscaleWithModel -> SaveImage, no resize node", () => {
  const g = buildUpscale({ image: "in.png", model: "4x-UltraSharp.pth" });
  assert.deepEqual(types(g), ["LoadImage", "UpscaleModelLoader", "ImageUpscaleWithModel", "SaveImage"]);
  assert.equal(find(g, "UpscaleModelLoader").inputs.model_name, "4x-UltraSharp.pth");
  const up = find(g, "ImageUpscaleWithModel");
  assert.deepEqual(up.inputs.upscale_model, ["2", 0]);
  assert.deepEqual(up.inputs.image, ["1", 0]);
  assert.deepEqual(find(g, "SaveImage").inputs.images, ["3", 0], "SaveImage reads the upscaler directly");
});

test("scale below native rescales the upscaled result by scale/native (2 on a 4x model = 0.5 lanczos)", () => {
  const g = buildUpscale({ image: "in.png", model: "4x-UltraSharp.pth", scale: 2 });
  const by = find(g, "ImageScaleBy");
  assert.ok(by, "ImageScaleBy present");
  assert.equal(by.inputs.scale_by, 0.5);
  assert.equal(by.inputs.upscale_method, "lanczos");
  assert.deepEqual(by.inputs.image, ["3", 0]);
  assert.deepEqual(find(g, "SaveImage").inputs.images, ["4", 0], "SaveImage reads the rescale");
});

test("scale equal to native adds no resize node", () => {
  const g = buildUpscale({ image: "in.png", model: "RealESRGAN_x4plus.pth", scale: 4 });
  assert.equal(find(g, "ImageScaleBy"), undefined);
  assert.deepEqual(find(g, "SaveImage").inputs.images, ["3", 0]);
});

test("width+height pin the output exactly via ImageScale (crop disabled) and win over scale", () => {
  const g = buildUpscale({ image: "in.png", model: "4x-UltraSharp.pth", scale: 2, width: 3000, height: 2000, method: "bicubic" });
  const sc = find(g, "ImageScale");
  assert.ok(sc, "ImageScale present");
  assert.equal(sc.inputs.width, 3000);
  assert.equal(sc.inputs.height, 2000);
  assert.equal(sc.inputs.crop, "disabled");
  assert.equal(sc.inputs.upscale_method, "bicubic");
  assert.equal(find(g, "ImageScaleBy"), undefined, "scale is ignored when width+height are pinned");
  assert.deepEqual(find(g, "SaveImage").inputs.images, ["4", 0]);
});

test("nativeFactor reads the filename; unknown names assume 4", () => {
  assert.equal(nativeFactor("4x-UltraSharp.pth"), 4);
  assert.equal(nativeFactor("RealESRGAN_x4plus.pth"), 4);
  assert.equal(nativeFactor("RealESRGAN_x2plus.pth"), 2);
  assert.equal(nativeFactor("2x_NMKD-Superscale.pth"), 2);
  assert.equal(nativeFactor("8x_NMKD-Superscale_150000_G.pth"), 8);
  assert.equal(nativeFactor("HAT_SRx4_ImageNet-pretrain.pth"), 4);
  assert.equal(nativeFactor("1x_NMKD-DeJPG.pth"), 1);
  assert.equal(nativeFactor("realesr-general-x4v3.pth"), 4);
  assert.equal(nativeFactor("OmniSR_X3_DIV2K.pth"), 3);
  // separator-less OpenModelDB style, with a later 'x' inside the same run (review round 1 miss)
  assert.equal(nativeFactor("2xLexicaRRDBNet_Sharp.pth"), 2);
  assert.equal(nativeFactor("2xLexica.pth"), 2);
  assert.equal(nativeFactor("4xLexicaDAT2_otf.pth"), 4);
  assert.equal(nativeFactor("2xHFA2kAVCSRFormer_light.pth"), 2);
  assert.equal(nativeFactor("mystery.pth"), 4);
});

test("ComfyUI core limits are enforced in the builder (so pre-flight fails before the GPU slot)", () => {
  assert.throws(() => buildUpscale({ image: "in.png", model: "m.pth", width: 20000, height: 100 }), /16384/);
  assert.throws(() => buildUpscale({ image: "in.png", model: "4x-m.pth", scale: 40 }), /outside ComfyUI/);   // scale_by 10
  assert.throws(() => buildUpscale({ image: "in.png", model: "4x-m.pth", scale: 0.03 }), /outside ComfyUI/); // scale_by 0.0075
  assert.ok(buildUpscale({ image: "in.png", model: "m.pth", width: 16384, height: 16384 }), "limit itself is allowed");
  assert.ok(buildUpscale({ image: "in.png", model: "4x-m.pth", scale: 32 }), "scale_by 8 is allowed");
});

test("rejects a half-given size, a bad method, a non-positive scale, and missing image/model", () => {
  assert.throws(() => buildUpscale({ image: "in.png", model: "m.pth", width: 100 }), /together/);
  assert.throws(() => buildUpscale({ image: "in.png", model: "m.pth", height: 100 }), /together/);
  assert.throws(() => buildUpscale({ image: "in.png", model: "m.pth", width: 10.5, height: 20 }), /positive integers/);
  assert.throws(() => buildUpscale({ image: "in.png", model: "m.pth", method: "magic" }), /method/);
  assert.throws(() => buildUpscale({ image: "in.png", model: "m.pth", scale: 0 }), /scale/);
  assert.throws(() => buildUpscale({ model: "m.pth" }), /image/);
  assert.throws(() => buildUpscale({ image: "in.png" }), /model/);
});
