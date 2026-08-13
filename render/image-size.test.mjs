// node --test render/image-size.test.mjs
//
// Buffers are synthesised here rather than committed as binary fixtures. The parser
// was separately checked against 24 real PIL-written files (PNG/JPEG/WebP, including a
// progressive JPEG and a 50 KB-EXIF JPEG) and agreed with PIL on every one; these
// tests pin the header layouts and the "unknown stays unknown" contract.
import { test } from "node:test";
import assert from "node:assert";
import { imageSize } from "./image-size.mjs";

function png(w, h) {
  const b = Buffer.alloc(33);
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]).copy(b, 0);
  b.writeUInt32BE(13, 8);
  b.write("IHDR", 12, "latin1");
  b.writeUInt32BE(w, 16);
  b.writeUInt32BE(h, 20);
  return b;
}

// FFD8, then `pad` bytes of APP1 payload, then SOF0. `sof` lets a caller pick SOF2
// (progressive) or a decoy marker from the same 0xC0-0xCF range.
function jpeg(w, h, { pad = 0, sof = 0xc0 } = {}) {
  const app1 = Buffer.concat([Buffer.from([0xff, 0xe1]), Buffer.alloc(2), Buffer.alloc(pad)]);
  app1.writeUInt16BE(pad + 2, 2);
  const frame = Buffer.alloc(11);
  frame.writeUInt8(0xff, 0);
  frame.writeUInt8(sof, 1);
  frame.writeUInt16BE(9, 2); // segment length
  frame.writeUInt8(8, 4); // sample precision
  frame.writeUInt16BE(h, 5);
  frame.writeUInt16BE(w, 7);
  return Buffer.concat([Buffer.from([0xff, 0xd8]), app1, frame]);
}

function webp(fourcc, fill) {
  const b = Buffer.alloc(32);
  b.write("RIFF", 0, "latin1");
  b.write("WEBP", 8, "latin1");
  b.write(fourcc, 12, "latin1");
  fill(b);
  return b;
}

test("PNG dimensions come off IHDR", () => {
  assert.deepEqual(imageSize(png(2048, 1024)), { width: 2048, height: 1024 });
  assert.deepEqual(imageSize(png(97, 53)), { width: 97, height: 53 });
});

test("JPEG: SOF is found however deep it sits, and SOF2 counts", () => {
  assert.deepEqual(imageSize(jpeg(1344, 768)), { width: 1344, height: 768 });
  // A fat EXIF/ICC segment pushes the frame header past any fixed-size prefix read.
  assert.deepEqual(imageSize(jpeg(1600, 900, { pad: 50000 })), { width: 1600, height: 900 });
  // Progressive JPEGs use SOF2.
  assert.deepEqual(imageSize(jpeg(1234, 567, { sof: 0xc2 })), { width: 1234, height: 567 });
});

test("JPEG: DHT/DAC/JPG share the 0xC0-0xCF range and are NOT frame headers", () => {
  for (const decoy of [0xc4, 0xc8, 0xcc]) {
    assert.deepEqual(imageSize(jpeg(800, 600, { sof: decoy })), { width: 0, height: 0 },
      `0x${decoy.toString(16)} must not be read as a frame header`);
  }
});

test("WebP: all three payload headers encode dimensions differently", () => {
  // VP8X: 24-bit little-endian canvas size, stored minus one.
  const x = webp("VP8X", (b) => { b.writeUIntLE(2048 - 1, 24, 3); b.writeUIntLE(1024 - 1, 27, 3); });
  assert.deepEqual(imageSize(x), { width: 2048, height: 1024 });

  // VP8 (lossy): sync code, then 14-bit dimensions stored as-is.
  const lossy = webp("VP8 ", (b) => {
    b[23] = 0x9d; b[24] = 0x01; b[25] = 0x2a;
    b.writeUInt16LE(1344, 26); b.writeUInt16LE(768, 28);
  });
  assert.deepEqual(imageSize(lossy), { width: 1344, height: 768 });

  // VP8L (lossless): 0x2f signature, then 14 bits width-1 and 14 bits height-1 packed.
  const lossless = webp("VP8L", (b) => {
    b[20] = 0x2f;
    b.writeUInt32LE((512 - 1) | ((512 - 1) << 14), 21);
  });
  assert.deepEqual(imageSize(lossless), { width: 512, height: 512 });
});

test("unknown stays unknown — the reader never guesses a size it did not read", () => {
  for (const b of [null, undefined, Buffer.alloc(0), Buffer.from("not an image at all"),
                   png(64, 64).subarray(0, 20), Buffer.from([0xff, 0xd8, 0xff])]) {
    assert.deepEqual(imageSize(b), { width: 0, height: 0 });
  }
  // A RIFF/WEBP container with an unrecognised payload must not be guessed either.
  assert.deepEqual(imageSize(webp("XXXX", () => {})), { width: 0, height: 0 });
});
