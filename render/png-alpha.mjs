// png-alpha.mjs — a minimal, dependency-free PNG codec limited to exactly what the
// sdcpp path needs: read an 8-bit, non-interlaced RGB/RGBA PNG (sd.cpp's own writer
// output, stb_image_write-shaped) and drop the alpha channel. No npm dependency — only
// node:zlib (the same inflate/deflate PNG's IDAT chunks always use) and a hand-rolled
// CRC-32 (the PNG spec's own polynomial; nowhere near worth pulling in a package for
// one function). Mirrors image-size.mjs's stance: a header/pixel reader scoped to one
// caller's real need, not a general imaging library.
//
// Used by sdcpp-generate.mjs (D5): stable-diffusion.cpp's qwen-image-2.1 VAE always
// emits RGBA (native, not request-flag-driven — binxarn wave session 5d227d30 §3c), so
// the runner flattens an ordinary render to opaque RGB itself; ComfyUI's equivalent
// step is wf-qwen-image-21.mjs's SplitImageWithAlpha node, which the same file's
// comment documents as a channel DROP, never a composite onto a background — this
// module matches that: alpha is discarded, RGB bytes pass through unchanged.
import { inflateSync, deflateSync } from "node:zlib";

const SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

// --- CRC-32 (PNG Annex D) ---------------------------------------------------------
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

// --- chunk parsing / building ------------------------------------------------------
function readChunks(buf) {
  if (buf.length < 8 || !buf.subarray(0, 8).equals(SIGNATURE)) {
    throw new Error("png-alpha: not a PNG (bad signature)");
  }
  const chunks = [];
  let off = 8;
  while (off + 8 <= buf.length) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString("latin1", off + 4, off + 8);
    const dataStart = off + 8;
    const dataEnd = dataStart + len;
    if (dataEnd + 4 > buf.length) throw new Error(`png-alpha: truncated ${type} chunk`);
    chunks.push({ type, data: buf.subarray(dataStart, dataEnd) });
    off = dataEnd + 4; // skip the trailing CRC
    if (type === "IEND") break;
  }
  return chunks;
}

function buildChunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const typeAndData = Buffer.concat([Buffer.from(type, "latin1"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(typeAndData), 0);
  return Buffer.concat([len, typeAndData, crc]);
}

// --- scanline (de)filtering (PNG spec §9) ------------------------------------------
function paeth(a, b, c) {
  const p = a + b - c;
  const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
  if (pa <= pb && pa <= pc) return a;
  if (pb <= pc) return b;
  return c;
}

// unfilter: raw scanlines (1 filter-type byte + width*bpp data bytes, per row) ->
// the flat pixel buffer (height * width * bpp, no filter bytes).
function unfilter(raw, width, height, bpp) {
  const stride = width * bpp;
  const out = Buffer.alloc(height * stride);
  let rawOff = 0, outOff = 0;
  for (let y = 0; y < height; y++) {
    const filterType = raw[rawOff];
    rawOff += 1;
    for (let x = 0; x < stride; x++) {
      const raw_x = raw[rawOff + x];
      const a = x >= bpp ? out[outOff + x - bpp] : 0;
      const b = y > 0 ? out[outOff - stride + x] : 0;
      const c = x >= bpp && y > 0 ? out[outOff - stride + x - bpp] : 0;
      let v;
      switch (filterType) {
        case 0: v = raw_x; break;
        case 1: v = raw_x + a; break;
        case 2: v = raw_x + b; break;
        case 3: v = raw_x + ((a + b) >> 1); break;
        case 4: v = raw_x + paeth(a, b, c); break;
        default: throw new Error(`png-alpha: unsupported scanline filter type ${filterType}`);
      }
      out[outOff + x] = v & 0xff;
    }
    rawOff += stride;
    outOff += stride;
  }
  return out;
}

// filter with type 0 (None) on every scanline: correctness over compression ratio —
// this module runs once per render on a single image, not in a hot loop.
function filterNone(pixels, width, height, bpp) {
  const stride = width * bpp;
  const out = Buffer.alloc(height * (stride + 1));
  for (let y = 0; y < height; y++) {
    out[y * (stride + 1)] = 0;
    pixels.copy(out, y * (stride + 1) + 1, y * stride, y * stride + stride);
  }
  return out;
}

// sd.cpp writes only RGB (colorType 2) or RGBA (colorType 6) — the two shapes this
// module needs to tell apart. Anything else (palette, grayscale, 16-bit, interlaced)
// throws by name rather than being silently mis-decoded.
const CHANNELS_BY_COLOR_TYPE = { 2: 3, 6: 4 };
const COLOR_TYPE_BY_CHANNELS = { 3: 2, 4: 6 };

/**
 * decodePng: {width, height, channels, pixels} — pixels is a flat, unfiltered
 * (height*width*channels)-byte buffer. Only 8-bit-depth, non-interlaced RGB/RGBA
 * PNGs are supported (sd.cpp's own writer output).
 */
export function decodePng(buf) {
  const chunks = readChunks(buf);
  const ihdr = chunks.find((c) => c.type === "IHDR");
  if (!ihdr) throw new Error("png-alpha: no IHDR chunk");
  const width = ihdr.data.readUInt32BE(0);
  const height = ihdr.data.readUInt32BE(4);
  const bitDepth = ihdr.data[8];
  const colorType = ihdr.data[9];
  const interlace = ihdr.data[12];
  if (bitDepth !== 8) throw new Error(`png-alpha: bit depth ${bitDepth} is not supported (only 8)`);
  if (interlace !== 0) throw new Error("png-alpha: interlaced PNGs are not supported");
  const channels = CHANNELS_BY_COLOR_TYPE[colorType];
  if (!channels) throw new Error(`png-alpha: color type ${colorType} is not supported (only RGB/RGBA)`);
  const idat = Buffer.concat(chunks.filter((c) => c.type === "IDAT").map((c) => c.data));
  const raw = inflateSync(idat);
  const pixels = unfilter(raw, width, height, channels);
  return { width, height, channels, pixels };
}

/** encodePng: the inverse of decodePng — {width, height, channels, pixels} -> PNG bytes. */
export function encodePng({ width, height, channels, pixels }) {
  const colorType = COLOR_TYPE_BY_CHANNELS[channels];
  if (colorType === undefined) throw new Error(`png-alpha: cannot encode ${channels} channels (only 3=RGB or 4=RGBA)`);
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = colorType;
  ihdr[10] = 0; // compression
  ihdr[11] = 0; // filter
  ihdr[12] = 0; // interlace
  const filtered = filterNone(pixels, width, height, channels);
  const idat = deflateSync(filtered);
  return Buffer.concat([SIGNATURE, buildChunk("IHDR", ihdr), buildChunk("IDAT", idat), buildChunk("IEND", Buffer.alloc(0))]);
}

/**
 * flattenToOpaqueRGB: an RGBA PNG -> the same pixels with the alpha channel DROPPED
 * (not composited onto any background — the ComfyUI graph's SplitImageWithAlpha this
 * mirrors is itself a channel split, never a composite; see wf-qwen-image-21.mjs's
 * decodeAndSave). An already-RGB PNG is returned byte-identical. Throws on any shape
 * decodePng does not support — callers should catch and leave the original file in
 * place rather than fail the whole render over a cosmetic post-process step.
 */
export function flattenToOpaqueRGB(buf) {
  const img = decodePng(buf);
  if (img.channels === 3) return buf; // already opaque
  const pixelCount = img.width * img.height;
  const dst = Buffer.alloc(pixelCount * 3);
  for (let i = 0; i < pixelCount; i++) {
    dst[i * 3] = img.pixels[i * 4];
    dst[i * 3 + 1] = img.pixels[i * 4 + 1];
    dst[i * 3 + 2] = img.pixels[i * 4 + 2];
  }
  return encodePng({ width: img.width, height: img.height, channels: 3, pixels: dst });
}
