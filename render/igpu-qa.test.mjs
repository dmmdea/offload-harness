// node --test render/igpu-qa.test.mjs
// The iGPU runners' output gates (igpu-qa.mjs): depth frames are converted to rgb24 at
// exactly W x H and READ BACK, the VACE frame count is a probe-and-only-if-N+4 decision, and
// an entirely black or entirely frozen clip fails typed. The decisions are tested against the
// REAL ffmpeg detector output (captured below from ffmpeg 9 on 64x64 lavfi clips) and a stub
// `run`; the ffmpeg-gated tests then run the real tools on real files.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync, mkdirSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  buildDepthRgbArgs, readPngHeader, assertRgbFrames, convertDepthFrames, countVideoFrames, trimDecision,
  parseDuration, parseFps, snapSeconds, parseBlackSegments, parseFreezeSegments, assessClip, buildClipCheckArgs, checkClip,
  runFailure, BLACK_CLIP, FROZEN_CLIP, DEPTH_FRAMES_INVALID,
} from "./igpu-qa.mjs";
import { resolveFfmpeg, resolveFfprobe } from "./audio-qa.mjs";

const scratch = () => mkdtempSync(join(tmpdir(), "igpu-qa-test-"));

// Captured with: ffmpeg -hide_banner -nostats -i X.mp4 -an -vf "blackdetect=d=0.1:pic_th=0.98,freezedetect=n=-60dB:d=0.5" -f null -
// (X = 2 s of black / 2 s of flat grey / 1 s of test pattern then 1 s of black / 2 s of test pattern).
const REAL = {
  black: `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'black.mp4':
  Duration: 00:00:02.00, start: 0.000000, bitrate: 9 kb/s
[Parsed_freezedetect_1 @ 000001ba5871a000] lavfi.freezedetect.freeze_start: 0
[Parsed_blackdetect_0 @ 000001ba58719f00] black_start:0 black_end:1.9375 black_duration:1.9375`,
  frozen: `  Duration: 00:00:02.00, start: 0.000000, bitrate: 9 kb/s
[Parsed_freezedetect_1 @ 000002857bd2c5c0] lavfi.freezedetect.freeze_start: 0`,
  alive: `  Duration: 00:00:02.00, start: 0.000000, bitrate: 52 kb/s`,
  halfBlack: `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'halfblack.mp4':
  Duration: 00:00:02.00, start: 0.000000, bitrate: 32 kb/s
[Parsed_freezedetect_1 @ 00000235845bcb00] lavfi.freezedetect.freeze_start: 1
[Parsed_blackdetect_0 @ 00000235845bca00] black_start:1 black_end:1.9375 black_duration:0.9375`,
};
const verdictOf = (text) => assessClip({ duration: parseDuration(text), black: parseBlackSegments(text), frozen: parseFreezeSegments(text), fps: parseFps(text) });
// A 5-frame clip at 8 fps (0.625 s) as ffmpeg reports it: the banner rounds the duration to 0.63 and
// blackdetect's black_end is the LAST FRAME's timestamp (0.5), 0.13 s short of the duration.
REAL.shortBlack = `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'c5.mp4':
  Duration: 00:00:00.63, start: 0.000000, bitrate: 21 kb/s
  Stream #0:0[0x1](und): Video: h264 (High) (avc1 / 0x31637661), yuv420p(progressive), 64x64 [SAR 1:1 DAR 1:1], 9 kb/s, 8 fps, 8 tbr, 16384 tbn (default)
Stream mapping:
  Stream #0:0[0x1](und): Video: h264 (High) (avc1 / 0x31637661), yuv420p(progressive), 64x64 [SAR 1:1 DAR 1:1], 47 kb/s, 16 fps, 16 tbr, 16384 tbn (default)
[Parsed_freezedetect_1 @ 000001fc9ed0c600] lavfi.freezedetect.freeze_start: 0
[Parsed_blackdetect_0 @ 000001fc9ed0cc00] black_start:0 black_end:0.5 black_duration:0.5`;

// ---------------------------------------------------------------- the clip gate

test("assessClip on real detector output: an all-black clip is BLACK_CLIP, a flat frozen clip is FROZEN_CLIP", () => {
  assert.equal(verdictOf(REAL.black).kind, BLACK_CLIP);
  assert.equal(verdictOf(REAL.frozen).kind, FROZEN_CLIP);
  const alive = verdictOf(REAL.alive);
  assert.equal(alive.ok, true);
});

test("assessClip: a SHORT all-black clip is BLACK_CLIP (black_end is the last frame, one frame period short of the rounded duration), not FROZEN_CLIP", () => {
  assert.equal(parseFps(REAL.shortBlack), 8, "the input stream's rate, not the output mapping's");
  assert.equal(verdictOf(REAL.shortBlack).kind, BLACK_CLIP);
  assert.ok(Math.abs(snapSeconds(8) - 0.136) < 1e-9);
  assert.equal(snapSeconds(0), 0.125);
  assert.equal(parseFps("no stream line"), 0);
});

test("assessClip: half a clip of black (a fade, a cut to black) is content, not a failed render", () => {
  const v = verdictOf(REAL.halfBlack);
  assert.equal(v.ok, true, v.reason);
});

test("parseFreezeSegments: an unmatched freeze_start is open-ended; start+end pairs close; a freeze to the end reaches it", () => {
  assert.deepEqual(parseFreezeSegments(REAL.frozen), [{ start: 0, end: null }]);
  const closed = "lavfi.freezedetect.freeze_start: 1\nlavfi.freezedetect.freeze_duration: 2\nlavfi.freezedetect.freeze_end: 3";
  assert.deepEqual(parseFreezeSegments(closed), [{ start: 1, end: 3 }]);
  const two = "lavfi.freezedetect.freeze_start: 0\nlavfi.freezedetect.freeze_start: 5";
  assert.deepEqual(parseFreezeSegments(two), [{ start: 0, end: null }, { start: 5, end: null }]);
  assert.deepEqual(parseFreezeSegments(""), []);
});

test("assessClip: a clip frozen from 0.5 s on is 75% frozen and passes; frozen from 0.05 s on (>=95%) fails", () => {
  assert.equal(assessClip({ duration: 2, black: [], frozen: [{ start: 0.5, end: null }] }).ok, true);
  const v = assessClip({ duration: 2, black: [], frozen: [{ start: 0.05, end: null }] });
  assert.equal(v.kind, FROZEN_CLIP);
});

test("assessClip: several black stretches that together cover the clip fail; an unmeasurable clip fails", () => {
  const v = assessClip({ duration: 2, black: [{ start: 0, end: 1 }, { start: 1, end: 2 }], frozen: [] });
  assert.equal(v.kind, BLACK_CLIP);
  const u = assessClip({ duration: 0, black: [], frozen: [] });
  assert.equal(u.ok, false);
  assert.equal(u.kind, "UNMEASURABLE");
});

test("parseDuration reads ffmpeg's banner; parseBlackSegments reads blackdetect lines", () => {
  assert.equal(parseDuration("  Duration: 00:01:02.50, start"), 62.5);
  assert.equal(parseDuration("no banner"), 0);
  assert.deepEqual(parseBlackSegments("black_start:1.5 black_end:3 black_duration:1.5"), [{ start: 1.5, end: 3 }]);
});

test("buildClipCheckArgs: one decode pass of the whole clip through blackdetect AND freezedetect, at the default log level", () => {
  const a = buildClipCheckArgs("/c/clip.mp4");
  assert.equal(a[a.indexOf("-i") + 1], "/c/clip.mp4");
  const vf = a[a.indexOf("-vf") + 1];
  assert.match(vf, /blackdetect/);
  assert.match(vf, /freezedetect/);
  assert.ok(!a.includes("-loglevel"), "the detectors report at info level");
});

test("checkClip with a stub run: a black clip throws BLACK_CLIP naming the file, a frozen one FROZEN_CLIP, a live one passes", () => {
  const stub = (text, status = 0) => () => ({ status, stderr: text, stdout: "" });
  assert.throws(() => checkClip("ffmpeg", "/c/b.mp4", { run: stub(REAL.black) }), (e) => e.message.startsWith(BLACK_CLIP) && e.message.includes("/c/b.mp4"));
  assert.throws(() => checkClip("ffmpeg", "/c/f.mp4", { run: stub(REAL.frozen) }), new RegExp(FROZEN_CLIP));
  assert.equal(checkClip("ffmpeg", "/c/a.mp4", { run: stub(REAL.alive) }).ok, true);
});

test("checkClip: an ffmpeg failure is an error with its stderr tail, never a pass", () => {
  assert.throws(() => checkClip("ffmpeg", "/c/x.mp4", { run: () => ({ status: 1, stderr: "Invalid data found when processing input" }) }), /clip check failed \(exit 1\): Invalid data/);
  assert.throws(() => checkClip("ffmpeg", "/c/x.mp4", { run: () => ({ error: Object.assign(new Error("spawn ENOENT"), { code: "ENOENT" }) }) }), /clip check failed: spawn ENOENT/);
  assert.match(runFailure("x", { error: Object.assign(new Error("t"), { code: "ETIMEDOUT" }) }).message, /timeout \(killed\)/);
});

// ---------------------------------------------------------------- VACE frame count

test("trimDecision: N keeps everything (the measured sd.cpp decodes exactly N), N+4 drops the 4 reference frames, anything else keeps all and says so", () => {
  assert.deepEqual(trimDecision(33, 33), { trimFirst: 0, note: "" });
  const t = trimDecision(37, 33);
  assert.equal(t.trimFirst, 4);
  assert.match(t.note, /37 frames for 33 requested: dropping the first 4/);
  const odd = trimDecision(35, 33);
  assert.equal(odd.trimFirst, 0);
  assert.match(odd.note, /neither N nor N\+4/);
});

test("countVideoFrames with a stub run: reads nb_read_frames, rejects junk and failures", () => {
  assert.equal(countVideoFrames("ffprobe", "x.webm", { run: () => ({ status: 0, stdout: "33\n" }) }), 33);
  assert.equal(countVideoFrames("ffprobe", "x.webm", { run: () => ({ status: 0, stdout: "37," }) }), 37);
  assert.throws(() => countVideoFrames("ffprobe", "x.webm", { run: () => ({ status: 0, stdout: "N/A\n" }) }), /no frame count/);
  assert.throws(() => countVideoFrames("ffprobe", "x.webm", { run: () => ({ status: 1, stderr: "moov atom not found" }) }), /ffprobe frame count failed \(exit 1\): moov/);
});

// ---------------------------------------------------------------- depth frames

// A PNG signature + IHDR chunk is all readPngHeader reads; the CRC is not checked there.
function pngHeader({ width, height, bitDepth = 8, colorType }) {
  const b = Buffer.alloc(33);
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]).copy(b, 0);
  b.writeUInt32BE(13, 8);
  b.write("IHDR", 12, "latin1");
  b.writeUInt32BE(width, 16);
  b.writeUInt32BE(height, 20);
  b[24] = bitDepth;
  b[25] = colorType;
  return b;
}

test("readPngHeader: width, height, bit depth and colour type; a non-PNG throws", () => {
  const d = scratch();
  try {
    const f = join(d, "a.png");
    writeFileSync(f, pngHeader({ width: 518, height: 896, bitDepth: 16, colorType: 0 }));
    assert.deepEqual(readPngHeader(f), { width: 518, height: 896, bitDepth: 16, colorType: 0 });
    const bad = join(d, "b.png");
    writeFileSync(bad, Buffer.from("not a png at all, just text of some length........"));
    assert.throws(() => readPngHeader(bad), /is not a PNG/);
  } finally { rmSync(d, { recursive: true, force: true }); }
});

test("assertRgbFrames: a 1-channel (grayscale) PNG - what depth-anything writes - is refused, naming the frame; so is a wrong size or count", () => {
  const d = scratch();
  try {
    writeFileSync(join(d, "00001.png"), pngHeader({ width: 512, height: 288, colorType: 2 }));
    writeFileSync(join(d, "00002.png"), pngHeader({ width: 512, height: 288, colorType: 0 }));
    assert.throws(() => assertRgbFrames(d, { count: 2, width: 512, height: 288 }), (e) => e.message.startsWith(DEPTH_FRAMES_INVALID) && e.message.includes("00002.png") && /colour type 0/.test(e.message));
    writeFileSync(join(d, "00002.png"), pngHeader({ width: 518, height: 288, colorType: 2 }));
    assert.throws(() => assertRgbFrames(d, { count: 2, width: 512, height: 288 }), /518x288/);
    writeFileSync(join(d, "00002.png"), pngHeader({ width: 512, height: 288, colorType: 2 }));
    assert.equal(assertRgbFrames(d, { count: 2, width: 512, height: 288 }), 2);
    assert.throws(() => assertRgbFrames(d, { count: 3, width: 512, height: 288 }), /2 control frame\(s\).*expected 3/);
    writeFileSync(join(d, "00003.png"), pngHeader({ width: 512, height: 288, bitDepth: 16, colorType: 2 }));
    assert.throws(() => assertRgbFrames(d, { count: 3, width: 512, height: 288 }), /16-bit/);
  } finally { rmSync(d, { recursive: true, force: true }); }
});

test("buildDepthRgbArgs: numbered PNGs in, rgb24 at exactly W x H out, numbering from 1 (ffmpeg's own extraction)", () => {
  const a = buildDepthRgbArgs({ rawDir: "/raw", outDir: "/out", width: 288, height: 512 });
  assert.equal(a[a.indexOf("-start_number") + 1], "1");
  assert.match(a[a.indexOf("-i") + 1].replace(/\\/g, "/"), /\/raw\/%05d\.png$/);
  assert.match(a[a.indexOf("-vf") + 1], /scale=288:512.*format=rgb24/);
  assert.equal(a[a.indexOf("-pix_fmt") + 1], "rgb24");
  assert.match(a[a.length - 1].replace(/\\/g, "/"), /\/out\/%05d\.png$/);
});

test("convertDepthFrames: an ffmpeg failure is an error with its stderr; a conversion that leaves a grayscale frame is caught by the read-back", () => {
  const d = scratch();
  try {
    assert.throws(() => convertDepthFrames({ ffmpeg: "ffmpeg", rawDir: d, outDir: d, width: 8, height: 8, count: 1, run: () => ({ status: 1, stderr: "Impossible to convert between the formats" }) }),
      /depth frame RGB conversion failed \(exit 1\): Impossible/);
    // a stub "ffmpeg" that exits 0 but leaves a 1-channel frame behind
    writeFileSync(join(d, "00001.png"), pngHeader({ width: 8, height: 8, colorType: 0 }));
    assert.throws(() => convertDepthFrames({ ffmpeg: "ffmpeg", rawDir: d, outDir: d, width: 8, height: 8, count: 1, run: () => ({ status: 0, stderr: "" }) }), new RegExp(DEPTH_FRAMES_INVALID));
  } finally { rmSync(d, { recursive: true, force: true }); }
});

// ---------------------------------------------------------------- the real tools

const ffmpeg = resolveFfmpeg();
const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
const real = { skip: !ffmpeg || !ffprobe ? "no ffmpeg/ffprobe" : false };
const ff = (...args) => spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", ...args], { encoding: "utf8" });

test("real ffmpeg: 1-channel 16-bit depth PNGs of the model's own size become rgb24 PNGs at exactly W x H", real, () => {
  const d = scratch();
  try {
    const raw = join(d, "raw");
    const out = join(d, "out");
    mkdirSync(raw);
    mkdirSync(out);
    // what depth-anything writes: grayscale, not the requested size
    for (let i = 1; i <= 3; i++) {
      const r = ff("-f", "lavfi", "-i", `testsrc2=s=90x160:d=0.2:r=10:alpha=0`, "-frames:v", "1", "-vf", "format=gray16be", join(raw, `${String(i).padStart(5, "0")}.png`));
      assert.equal(r.status, 0, r.stderr);
    }
    assert.equal(readPngHeader(join(raw, "00001.png")).colorType, 0, "the fixture really is 1-channel");
    assert.equal(convertDepthFrames({ ffmpeg, rawDir: raw, outDir: out, width: 64, height: 96, count: 3 }), 3);
    for (const f of readdirSync(out)) assert.deepEqual(readPngHeader(join(out, f)), { width: 64, height: 96, bitDepth: 8, colorType: 2 });
  } finally { rmSync(d, { recursive: true, force: true }); }
});

test("real ffmpeg: checkClip fails an all-black clip and a flat frozen clip, passes a moving one and a clip that is half black", real, () => {
  const d = scratch();
  try {
    const mk = (name, ...input) => {
      const f = join(d, name);
      const r = ff(...input, "-c:v", "libx264", "-pix_fmt", "yuv420p", f);
      assert.equal(r.status, 0, r.stderr);
      return f;
    };
    const black = mk("black.mp4", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=2:r=16");
    const frozen = mk("frozen.mp4", "-f", "lavfi", "-i", "color=c=gray:s=64x64:d=2:r=16");
    const alive = mk("alive.mp4", "-f", "lavfi", "-i", "testsrc2=s=64x64:d=2:r=16");
    const half = mk("half.mp4", "-f", "lavfi", "-i", "testsrc2=s=64x64:d=1:r=16", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1:r=16", "-filter_complex", "[0][1]concat=n=2:v=1:a=0");
    assert.throws(() => checkClip(ffmpeg, black), new RegExp(BLACK_CLIP));
    assert.throws(() => checkClip(ffmpeg, frozen), new RegExp(FROZEN_CLIP));
    assert.equal(checkClip(ffmpeg, alive).ok, true);
    assert.equal(checkClip(ffmpeg, half).ok, true);
    assert.equal(countVideoFrames(ffprobe, alive), 32);
  } finally { rmSync(d, { recursive: true, force: true }); }
});
