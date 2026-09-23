// node --test render/png-alpha.test.mjs
// Round-trips the encoder/decoder against known pixel data (no external fixture files —
// the encoder IS the fixture generator, exercised the same way sdcpp-generate.mjs's own
// tests build their RGBA PNG) and pins flattenToOpaqueRGB's channel-drop behavior.
import { test } from "node:test";
import assert from "node:assert";
import { encodePng, decodePng, flattenToOpaqueRGB } from "./png-alpha.mjs";

// A 3x2 RGBA image: distinct per-pixel alpha, including 255 (opaque), 0 (fully
// transparent) and 128 (partial) — mirrors the wave log's measured spread (P8: min
// alpha 0, max 255, 62.94% of pixels < 255).
function rgbaFixture() {
  const width = 3, height = 2;
  const pixels = Buffer.from([
    255, 0, 0, 255, 0, 255, 0, 128, 0, 0, 255, 0,
    255, 255, 0, 243, 0, 255, 255, 200, 255, 0, 255, 10,
  ]);
  return { width, height, channels: 4, pixels };
}

test("encodePng -> decodePng round-trips pixels exactly (RGBA)", () => {
  const img = rgbaFixture();
  const buf = encodePng(img);
  // A real PNG signature + IHDR/IDAT/IEND, so any other PNG reader also accepts it.
  assert.equal(buf[0], 0x89);
  assert.equal(buf.toString("latin1", 1, 4), "PNG");
  const back = decodePng(buf);
  assert.equal(back.width, img.width);
  assert.equal(back.height, img.height);
  assert.equal(back.channels, 4);
  assert.deepEqual(Buffer.from(back.pixels), img.pixels);
});

test("flattenToOpaqueRGB: an RGBA PNG with partial alpha becomes RGB with no alpha channel, pixel values preserved", () => {
  const img = rgbaFixture();
  const rgba = encodePng(img);
  const flattened = flattenToOpaqueRGB(rgba);
  const out = decodePng(flattened);
  assert.equal(out.channels, 3, "alpha channel must be gone, not just set to 255");
  assert.equal(out.width, img.width);
  assert.equal(out.height, img.height);
  // Every RGB triplet survives untouched — SplitImageWithAlpha DROPS alpha, it does
  // not composite onto a background, so the RGB bytes must be byte-identical to the
  // source, not re-blended.
  for (let i = 0; i < img.width * img.height; i++) {
    for (let c = 0; c < 3; c++) {
      assert.equal(out.pixels[i * 3 + c], img.pixels[i * 4 + c], `pixel ${i} channel ${c}`);
    }
  }
});

test("flattenToOpaqueRGB: an already-opaque RGB PNG passes through byte-identical", () => {
  const rgb = encodePng({ width: 2, height: 1, channels: 3, pixels: Buffer.from([10, 20, 30, 40, 50, 60]) });
  const flattened = flattenToOpaqueRGB(rgb);
  assert.ok(flattened === rgb || flattened.equals(rgb));
  assert.equal(decodePng(flattened).channels, 3);
});

test("flattenToOpaqueRGB: fully-transparent and fully-opaque pixels both flatten (no special-casing alpha=0 or 255)", () => {
  const img = rgbaFixture();
  const flattened = flattenToOpaqueRGB(encodePng(img));
  const out = decodePng(flattened);
  // Pixel index 1 had alpha 128 (partial, the harness's own "not byte-opaque" finding
  // — binxarn wave §3c: 5.53% of an ORDINARY prompt's pixels sat at alpha 243-255) and
  // pixel index 2 had alpha 0 (fully transparent, P8's sticker case) — both must
  // still carry their RGB bytes in the flattened output.
  assert.equal(out.pixels[1 * 3 + 1], 255); // pixel 1 green channel
  assert.equal(out.pixels[2 * 3 + 2], 255); // pixel 2 blue channel
});
