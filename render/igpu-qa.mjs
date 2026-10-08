// igpu-qa.mjs — output checks for the iGPU media runners (CT-49), each one a gate on the
// medium the result ships in (a clip is measured as a clip, a depth frame is read as a PNG):
//
//   depth frames   sd-cli refuses 1-channel PNGs ("the number of channels for the input image
//                  must be >= 3, but got 1 channels") and depth-anything writes 1-channel
//                  PNGs at the model's own working size, so every depth frame is converted to
//                  rgb24 at exactly W x H by ffmpeg (buildDepthRgbArgs) and then READ BACK
//                  (readPngHeader / assertRgbFrames) before the directory goes to sd-cli.
//   VACE frames    with a reference image sd.cpp samples N+4 frames; the measured sd.cpp
//                  already trims the reference latent itself and decodes exactly N, so the
//                  trim is a probe-and-only-if-N+4 decision (trimDecision).
//   the clip       a black or a frozen clip is a failed render that exits 0 (a Vulkan fp16/NaN
//                  failure on an iGPU does exactly that): ffmpeg blackdetect + freezedetect
//                  over the whole clip, and a clip that is entirely black or entirely frozen
//                  fails typed (BLACK_CLIP / FROZEN_CLIP).
//
// Every ffmpeg / ffprobe call goes through an injectable `run` (default: spawnSync), so the
// decisions are tested against captured tool output without spawning anything.
//
// Dependency-free (Node 18+ built-ins only).
import { spawnSync } from "node:child_process";
import { closeSync, openSync, readSync, readdirSync } from "node:fs";
import { join } from "node:path";

export const BLACK_CLIP = "BLACK_CLIP";
export const FROZEN_CLIP = "FROZEN_CLIP";
export const DEPTH_FRAMES_INVALID = "DEPTH_FRAMES_INVALID";
export const UNMEASURABLE = "UNMEASURABLE";

// defaultRun: spawnSync with text output; `timeoutMs` (0 = none) bounds the call.
export function defaultRun(cmd, args, { timeoutMs = 0 } = {}) {
  return spawnSync(cmd, args, {
    encoding: "utf8", maxBuffer: 64 * 1024 * 1024,
    ...(timeoutMs > 0 ? { timeout: timeoutMs, killSignal: "SIGKILL" } : {}),
  });
}

// tailOf: the last n characters of a tool's stderr, for an error message.
export function tailOf(text, n = 300) {
  return String(text ?? "").trim().slice(-n);
}

// failedRun: the error for a tool run that did not succeed, with its stderr tail.
export function runFailure(what, r) {
  if (r && r.error) {
    const timedOut = r.error.code === "ETIMEDOUT";
    return new Error(`${what} ${timedOut ? "timeout (killed)" : "failed"}: ${r.error.message}`);
  }
  return new Error(`${what} failed (exit ${r ? r.status : "?"}): ${tailOf(r && r.stderr)}`);
}

// ---------------------------------------------------------------- depth frames

// buildDepthRgbArgs: ONE ffmpeg call that converts the numbered depth PNGs in rawDir to
// rgb24 at exactly width x height in outDir (same numbering). The frames come from
// ffmpeg's own "%05d.png" extraction, so they number from 1.
export function buildDepthRgbArgs({ rawDir, outDir, width, height }) {
  return ["-hide_banner", "-loglevel", "error", "-y", "-start_number", "1", "-i", join(rawDir, "%05d.png"),
    "-vf", `scale=${width}:${height}:flags=bicubic,format=rgb24`, "-pix_fmt", "rgb24", join(outDir, "%05d.png")];
}

// readPngHeader: the IHDR of a PNG: {width, height, bitDepth, colorType}. colorType 0 is
// grayscale (1 channel), 2 is RGB, 3 palette, 4 gray+alpha, 6 RGBA. Throws on a non-PNG.
export function readPngHeader(file) {
  const fd = openSync(file, "r");
  try {
    const b = Buffer.alloc(33);
    const n = readSync(fd, b, 0, 33, 0);
    if (n < 33 || b.readUInt32BE(0) !== 0x89504e47 || b.readUInt32BE(4) !== 0x0d0a1a0a || b.toString("latin1", 12, 16) !== "IHDR") {
      throw new Error(`${file} is not a PNG`);
    }
    return { width: b.readUInt32BE(16), height: b.readUInt32BE(20), bitDepth: b[24], colorType: b[25] };
  } finally {
    closeSync(fd);
  }
}

// assertRgbFrames: every frame of the control video is an 8-bit RGB PNG of exactly
// width x height, and there are exactly `count` of them. Throws DEPTH_FRAMES_INVALID naming
// the first frame that is not.
export function assertRgbFrames(dir, { count, width, height }) {
  const names = readdirSync(dir).filter((f) => f.toLowerCase().endsWith(".png")).sort();
  if (names.length !== count) {
    throw new Error(`${DEPTH_FRAMES_INVALID}: ${names.length} control frame(s) in the depth directory, expected ${count}`);
  }
  for (const f of names) {
    const h = readPngHeader(join(dir, f));
    if (h.colorType !== 2 || h.bitDepth !== 8 || h.width !== width || h.height !== height) {
      throw new Error(`${DEPTH_FRAMES_INVALID}: control frame ${f} is ${h.width}x${h.height} colour type ${h.colorType} (${h.bitDepth}-bit); sd-cli needs 8-bit RGB (colour type 2) at ${width}x${height}`);
    }
  }
  return names.length;
}

// convertDepthFrames: rgb24 W x H in outDir from the raw 1-channel depth PNGs in rawDir, then
// verified by reading the headers back.
export function convertDepthFrames({ ffmpeg, rawDir, outDir, width, height, count, timeoutMs = 0, run = defaultRun }) {
  const r = run(ffmpeg, buildDepthRgbArgs({ rawDir, outDir, width, height }), { timeoutMs });
  if (r.error || r.status !== 0) throw runFailure("depth frame RGB conversion", r);
  return assertRgbFrames(outDir, { count, width, height });
}

// ---------------------------------------------------------------- VACE frame count

// countVideoFrames: the decoded frame count of a video, by ffprobe (-count_frames reads the
// stream; nb_frames in the container header is not trusted).
export function countVideoFrames(ffprobe, file, { timeoutMs = 0, run = defaultRun } = {}) {
  const r = run(ffprobe, ["-v", "error", "-count_frames", "-select_streams", "v:0",
    "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", file], { timeoutMs });
  if (r.error || r.status !== 0) throw runFailure("ffprobe frame count", r);
  const n = Number(String(r.stdout || "").trim().split(/\s+/)[0].replace(/,$/, ""));
  if (!Number.isInteger(n) || n <= 0) throw new Error(`ffprobe frame count: no frame count in ${JSON.stringify(tailOf(r.stdout))}`);
  return n;
}

// trimDecision: how many leading frames to drop so the mp4 carries exactly `requested`
// frames. sd.cpp samples N+4 frames when a VACE reference image is given (the reference
// occupies one latent frame); the measured build trims that itself and decodes exactly N, so
// the decision is made on the decoded count: N+4 -> drop the first 4 (the reference latent),
// N -> keep all, anything else -> keep all and say so (never guess a trim).
export function trimDecision(decoded, requested) {
  if (decoded === requested + 4) return { trimFirst: 4, note: `sd-cli decoded ${decoded} frames for ${requested} requested: dropping the first 4 (the VACE reference latent)` };
  if (decoded === requested) return { trimFirst: 0, note: "" };
  return { trimFirst: 0, note: `sd-cli decoded ${decoded} frames for ${requested} requested (neither N nor N+4): keeping all of them` };
}

// ---------------------------------------------------------------- black / frozen clip gate

// ffmpeg prints the input's length in its banner: "Duration: 00:00:02.04, start: ...".
export function parseDuration(text) {
  const m = /Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)/.exec(String(text ?? ""));
  return m ? Number(m[1]) * 3600 + Number(m[2]) * 60 + Number(m[3]) : 0;
}

// parseFps: the input video stream's frame rate from ffmpeg's banner ("... 8 fps, 8 tbr ..."): the
// FIRST video stream line is the input's (the output stream is listed under "Stream mapping").
// 0 when it is not there.
export function parseFps(text) {
  const m = /Stream #\d+:\d+[^\n]*?Video:[^\n]*?,\s*(\d+(?:\.\d+)?)\s*fps/.exec(String(text ?? ""));
  return m ? Number(m[1]) : 0;
}

// parseBlackSegments: blackdetect's "black_start:0 black_end:1.9375 black_duration:1.9375".
export function parseBlackSegments(text) {
  const out = [];
  for (const m of String(text ?? "").matchAll(/black_start:\s*(-?[\d.]+)\s+black_end:\s*(-?[\d.]+)\s+black_duration:\s*(-?[\d.]+)/g)) {
    out.push({ start: Number(m[1]), end: Number(m[2]) });
  }
  return out;
}

// parseFreezeSegments: freezedetect prints "lavfi.freezedetect.freeze_start: S" and, when the
// freeze ends before the clip does, "freeze_duration: D" / "freeze_end: E". A freeze that runs to
// the end of the clip prints only its start, so an unmatched start is open-ended (end = null).
export function parseFreezeSegments(text) {
  const out = [];
  let open = null;
  for (const line of String(text ?? "").split(/\r\n|\r|\n/)) {
    let m = /freezedetect\.freeze_start:\s*(-?[\d.]+)/.exec(line);
    if (m) {
      if (open) out.push({ start: open.start, end: null });
      open = { start: Number(m[1]) };
      continue;
    }
    m = /freezedetect\.freeze_end:\s*(-?[\d.]+)/.exec(line);
    if (m && open) {
      out.push({ start: open.start, end: Number(m[1]) });
      open = null;
    }
  }
  if (open) out.push({ start: open.start, end: null });
  return out;
}

// snapSeconds: how far from the end of the clip a segment's end may be and still reach it.
// blackdetect reports the LAST FRAME's timestamp as black_end, which is one frame before the
// clip's duration, and ffmpeg's banner rounds the duration to 0.01 s; so one frame period (from
// the stream's fps) plus 0.011 s. Without a frame rate, 0.125 s (a clip of 8 fps or faster).
export function snapSeconds(fps) {
  return fps > 0 ? Math.min(0.5, 1 / fps + 0.011) : 0.125;
}

// coverage: how much of [0, duration] the segments cover. An end within `snap` seconds of the
// clip's end or an open end reaches the end.
function coverage(segments, duration, snap = 0.125) {
  let sum = 0;
  for (const s of segments) {
    let end = s.end === null || s.end === undefined ? duration : Math.min(s.end, duration);
    if (duration - end <= snap) end = duration;
    sum += Math.max(0, end - Math.max(0, s.start));
  }
  return Math.min(sum, duration);
}

// assessClip: the verdict for one clip's detector output. A clip that is entirely black or
// entirely frozen (>= `entirely` of its length) fails; partial black or frozen stretches are
// ordinary content (a fade, a held frame) and pass. Pure.
export function assessClip({ duration, black, frozen, fps = 0 }, { entirely = 0.95 } = {}) {
  if (!(duration > 0)) return { ok: false, kind: UNMEASURABLE, reason: "ffmpeg reported no duration for the clip" };
  const snap = snapSeconds(fps);
  const b = coverage(black, duration, snap) / duration;
  const f = coverage(frozen, duration, snap) / duration;
  if (b >= entirely) return { ok: false, kind: BLACK_CLIP, reason: `the clip is ${(b * 100).toFixed(0)}% black` };
  if (f >= entirely) return { ok: false, kind: FROZEN_CLIP, reason: `the clip is ${(f * 100).toFixed(0)}% frozen (no frame ever changes)` };
  return { ok: true, kind: "", reason: `black ${(b * 100).toFixed(0)}%, frozen ${(f * 100).toFixed(0)}%` };
}

// buildClipCheckArgs: one decode pass of the whole clip through both detectors. The default
// log level is needed: blackdetect and freezedetect report at info level.
export function buildClipCheckArgs(file) {
  return ["-hide_banner", "-nostats", "-i", file, "-an",
    "-vf", "blackdetect=d=0.1:pic_th=0.98,freezedetect=n=-60dB:d=0.5", "-f", "null", "-"];
}

// checkClip: run the detectors over the whole clip and throw BLACK_CLIP / FROZEN_CLIP (typed,
// not retried) for an entirely black or frozen one, and UNMEASURABLE when the clip's length could
// not be read (it was not checked, so it is not delivered either). Returns the verdict when the
// clip is alive. `label` names the clip in an error (the delivery path, when `file` is a partial).
export function checkClip(ffmpeg, file, { timeoutMs = 0, run = defaultRun, label = file } = {}) {
  const r = run(ffmpeg, buildClipCheckArgs(file), { timeoutMs });
  if (r.error || r.status !== 0) throw runFailure("clip check", r);
  const text = String(r.stderr || "");
  const verdict = assessClip({ duration: parseDuration(text), black: parseBlackSegments(text), frozen: parseFreezeSegments(text), fps: parseFps(text) });
  if (verdict.kind === UNMEASURABLE) {
    throw new Error(`${UNMEASURABLE}: ${verdict.reason} (${label}), so the clip could not be checked for a black or frozen picture; it is not delivered. This says nothing about the picture itself. Not retried automatically.`);
  }
  if (!verdict.ok) {
    throw new Error(`${verdict.kind}: ${verdict.reason} (${label}); the engine exited 0 but delivered a clip with no picture in it, which is a failed render. Not retried automatically.`);
  }
  return verdict;
}
