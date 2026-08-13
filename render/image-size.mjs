// image-size.mjs — pixel dimensions straight from a raster image's header bytes.
// No decode, no dependency, no PIL round-trip.
//
// Only one caller needs this: the generative-edit seat sizes its working canvas from
// the SOURCE resolution (see wf-qwen-image-edit.mjs — the scaler feeding VAEEncode is
// what sets the OUTPUT resolution, so guessing there costs real pixels). Every other
// runner in render/ is told its dimensions by the caller or reads them back off
// ComfyUI, which is why this stays a header reader instead of an image library.
//
// Covers the three formats ComfyUI's LoadImage accepts: PNG, JPEG, WebP. Anything
// unrecognised or truncated reports {width: 0, height: 0} — "unknown" is data the
// caller decides about; this file never guesses a size it did not read.
//
// (render/comfy-run-graph.mjs carries its own 4-line `pngSize` for the generic
// run-graph envelope. It is deliberately left alone: that path must not grow a
// dependency on the edit seat's helpers.)

/** imageSize: {width, height} in pixels, or {width: 0, height: 0} when unreadable. */
export function imageSize(buf) {
  if (!buf || buf.length < 8) return { width: 0, height: 0 };
  return pngSize(buf) || jpegSize(buf) || webpSize(buf) || { width: 0, height: 0 };
}

// PNG: 8-byte signature, then the IHDR chunk (4-byte length + "IHDR" type), so width
// is the big-endian u32 at offset 16 and height at 20.
function pngSize(b) {
  if (b.length < 24) return null;
  if (b[0] !== 0x89 || b[1] !== 0x50 || b[2] !== 0x4e || b[3] !== 0x47) return null;
  if (b.toString("latin1", 12, 16) !== "IHDR") return null;
  return { width: b.readUInt32BE(16), height: b.readUInt32BE(20) };
}

// JPEG: walk the marker chain to the frame header. Dimensions live in SOFn, and SOFn
// can sit arbitrarily deep behind EXIF/ICC segments (a thumbnail alone pushes it past
// any fixed prefix), so the whole buffer is fair game.
function jpegSize(b) {
  if (b.length < 4 || b[0] !== 0xff || b[1] !== 0xd8) return null;
  let i = 2;
  while (i + 3 < b.length) {
    if (b[i] !== 0xff) { i++; continue; } // resync past padding
    const marker = b[i + 1];
    if (marker === 0xff) { i++; continue; } // fill byte
    // TEM and RSTn/SOI/EOI carry no length word.
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd9)) { i += 2; continue; }
    const len = b.readUInt16BE(i + 2);
    if (len < 2) return null;
    // SOF0..SOF15 carry the frame size. C4 (DHT), C8 (JPG) and CC (DAC) share the
    // 0xC0-0xCF range and are NOT frame headers.
    if (marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc) {
      if (i + 8 >= b.length) return null;
      // ...FF Cn, len(2), precision(1), height(2), width(2)
      return { width: b.readUInt16BE(i + 7), height: b.readUInt16BE(i + 5) };
    }
    i += 2 + len;
  }
  return null;
}

// WebP: RIFF container, then one of three payload headers with three different
// dimension encodings.
function webpSize(b) {
  if (b.length < 30) return null;
  if (b.toString("latin1", 0, 4) !== "RIFF" || b.toString("latin1", 8, 12) !== "WEBP") return null;
  switch (b.toString("latin1", 12, 16)) {
    case "VP8X": // extended: 24-bit little-endian canvas dimensions, stored minus one
      return { width: b.readUIntLE(24, 3) + 1, height: b.readUIntLE(27, 3) + 1 };
    case "VP8 ": // lossy: 3-byte frame tag, 3-byte sync code, then 14-bit dimensions
      if (b[23] !== 0x9d || b[24] !== 0x01 || b[25] !== 0x2a) return null;
      return { width: b.readUInt16LE(26) & 0x3fff, height: b.readUInt16LE(28) & 0x3fff };
    case "VP8L": { // lossless: 0x2f signature, then 14 bits width-1 and 14 bits height-1
      if (b[20] !== 0x2f) return null;
      const bits = b.readUInt32LE(21);
      return { width: (bits & 0x3fff) + 1, height: ((bits >>> 14) & 0x3fff) + 1 };
    }
    default:
      return null;
  }
}
