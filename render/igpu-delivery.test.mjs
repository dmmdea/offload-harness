// node --test render/igpu-delivery.test.mjs
// What the iGPU runners DELIVER and how they check it (CT-49 round 2):
//  - the dead-air gate fails CLOSED: an ffmpeg that runs but prints no loudness summary (non-zero exit,
//    signal, a build without silencedetect / ebur128) is UNMEASURABLE, never "clean" (SIL2);
//  - a result is written to a partial file beside its delivery path and renamed onto it only once it
//    passed: a failed or deadline-killed encode, a rejected clip or a failed audio gate leaves no
//    partial and never replaces a good file already there (SIL8);
//  - a clip whose length could not be read is UNMEASURABLE, and says so instead of claiming it has no
//    picture (SIL11);
//  - helper survivors from the mutation round (TST21).
import { test } from "node:test";
import assert from "node:assert";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makeStub } from "./igpu-stub.mjs";
import { gateDeadAir, finalizeAudio } from "./audiocpp-generate.mjs";
import { measure } from "./audio-qa.mjs";
import {
  encodeMp4, ensureOutDir, modelMetadataError, checkTokenCap, tokenCapFromFlags, latentTokens, partialPath, screenExtraArgs,
} from "./igpu-engine.mjs";
import { assessClip, checkClip, assertRgbFrames, snapSeconds, UNMEASURABLE } from "./igpu-qa.mjs";

const isWin = process.platform === "win32";
// assert.throws matches a regex against "Error: <message>", so anchor on the message itself
const msgStarts = (prefix) => (e) => e.message.startsWith(prefix);
const scratch = () => mkdtempSync(join(tmpdir(), "igpu-delivery-test-"));
const done = (d) => rmSync(d, { recursive: true, force: true });

// A fake ffprobe (answers a duration on stdout) and fake ffmpegs, as real executables.
function fakes(dir, ffmpegSpec) {
  return {
    ffprobe: makeStub(dir, "ffprobe", { stdout: ["12.5"] }),
    ffmpeg: makeStub(dir, "ffmpeg", ffmpegSpec),
  };
}

// ---------------------------------------------------------------- SIL2

test("gateDeadAir: a measurement without a loudness summary is UNMEASURABLE, never clean; a silent file is still DEAD_AIR", () => {
  const noSummary = { duration: 12.5, silences: [], integratedLUFS: null, truePeakDBFS: null };
  assert.throws(() => gateDeadAir({ ffmpeg: "f", ffprobe: "p", file: "x.wav", measureFn: () => noSummary }), (e) => /^UNMEASURABLE:/.test(e.message) && /NOT checked/.test(e.message) && !/^DEAD_AIR/.test(e.message));
  assert.throws(() => gateDeadAir({ ffmpeg: "f", ffprobe: "p", file: "x.wav", measureFn: () => ({ ...noSummary, integratedLUFS: undefined }) }), msgStarts("UNMEASURABLE:"));
  // ebur128 prints I: -70.0 LUFS for digital silence: a summary exists, the file is dead air
  const silent = { duration: 4, silences: [{ start: 0, end: 4, duration: 4 }], integratedLUFS: -70, truePeakDBFS: null };
  assert.throws(() => gateDeadAir({ ffmpeg: "f", ffprobe: "p", file: "x.wav", measureFn: () => silent }), msgStarts("DEAD_AIR:"));
  assert.throws(() => gateDeadAir({ ffmpeg: "f", ffprobe: "p", file: "x.wav", measureFn: () => null }), msgStarts("DEAD_AIR: the delivered audio could not be measured"));
  const ok = gateDeadAir({ ffmpeg: "f", ffprobe: "p", file: "x.wav", measureFn: () => ({ duration: 12.5, silences: [], integratedLUFS: -14, truePeakDBFS: -1 }) });
  assert.equal(ok.deadAir, false);
});

test("the REAL measure(): an ffmpeg that runs but exits non-zero (fake ffmpeg + fake ffprobe 12.5) was the 'clean' verdict - now UNMEASURABLE", () => {
  const dir = scratch();
  try {
    for (const spec of [{ exit: 1 }, { exit: 1, log: ["No such filter: 'ebur128'"] }, { exit: 0, log: ["ffmpeg version fake, nothing else"] }]) {
      const { ffmpeg, ffprobe } = fakes(dir, spec);
      const m = measure(ffmpeg, ffprobe, join(dir, "any.wav"));
      assert.equal(m.duration, 12.5, "ffprobe answered");
      assert.equal(m.integratedLUFS, null, "no loudness summary");
      assert.deepEqual(m.silences, [], "and no silence found: the old gate called this clean");
      assert.throws(() => gateDeadAir({ ffmpeg, ffprobe, file: join(dir, "any.wav") }), msgStarts("UNMEASURABLE:"), JSON.stringify(spec));
    }
  } finally { done(dir); }
});

// ---------------------------------------------------------------- SIL8, audio

test("finalizeAudio: an unmeasurable result is not delivered, and a good file already at the path survives a failed re-run", () => {
  const dir = scratch();
  try {
    const wav = join(dir, "in.wav");
    writeFileSync(wav, "RIFFengine-wav");
    const out = join(dir, "voice.wav");
    writeFileSync(out, "PREVIOUS-GOOD-AUDIO");
    const { ffmpeg, ffprobe } = fakes(dir, { exit: 1 });
    // voice .wav: copied to a partial, gated with the real measure() against a fake ffmpeg that fails
    assert.throws(() => finalizeAudio({ ffmpeg, ffprobe, wav, out, kind: "voice", workDir: dir, log: () => {} }), msgStarts("UNMEASURABLE:"));
    assert.equal(readFileSync(out, "utf8"), "PREVIOUS-GOOD-AUDIO", "the previous good file is untouched");
    assert.deepEqual(readdirSync(dir).filter((f) => f.includes(".part")), [], "and no partial is left behind");
    // a gate that rejects (dead air), a step that fails: the same
    const silent = { duration: 4, silences: [{ start: 0, end: 4, duration: 4 }], integratedLUFS: -70, truePeakDBFS: null };
    assert.throws(() => finalizeAudio({ ffmpeg, ffprobe, wav, out, kind: "voice", workDir: dir, measureFn: () => silent, log: () => {} }), /DEAD_AIR/);
    assert.equal(readFileSync(out, "utf8"), "PREVIOUS-GOOD-AUDIO");
    const failing = () => ({ status: 1, stderr: "boom" });
    assert.throws(() => finalizeAudio({ ffmpeg, ffprobe, wav, out: join(dir, "v.flac"), kind: "voice", workDir: dir, run: failing, log: () => {} }), /failed/);
    assert.deepEqual(readdirSync(dir).filter((f) => f.includes(".part")), []);
    // a passing gate replaces it
    const clean = { duration: 4, silences: [], integratedLUFS: -14, truePeakDBFS: -1 };
    finalizeAudio({ ffmpeg, ffprobe, wav, out, kind: "voice", workDir: dir, measureFn: () => clean, log: () => {} });
    assert.equal(readFileSync(out, "utf8"), "RIFFengine-wav");
    assert.deepEqual(readdirSync(dir).filter((f) => f.includes(".part")), []);
  } finally { done(dir); }
});

// ---------------------------------------------------------------- SIL8, mp4

test("encodeMp4: the encode goes to a partial beside the result; the verify hook sees it; only a pass replaces the result", () => {
  const dir = scratch();
  try {
    const out = join(dir, "nested", "clip.mp4");
    mkdirSync(join(dir, "nested"));
    writeFileSync(out, "PREVIOUS-GOOD-CLIP");
    const ffmpeg = makeStub(dir, "ffmpeg", { writes: { kind: "last_arg", content: "NEW-CLIP" } });
    let seen = null;
    encodeMp4(ffmpeg, join(dir, "in.webm"), out, 8, 0, {
      verify: (p) => {
        seen = p;
        assert.equal(readFileSync(out, "utf8"), "PREVIOUS-GOOD-CLIP", "the result is still the old one while the new one is checked");
        assert.equal(readFileSync(p, "utf8"), "NEW-CLIP");
      },
    });
    assert.ok(seen && seen !== out && seen.startsWith(join(dir, "nested")) && seen.endsWith(".mp4") && seen.includes(".part."), seen);
    assert.equal(partialPath(out), seen, "the partial path is a pure function of the result path and the pid");
    assert.equal(readFileSync(out, "utf8"), "NEW-CLIP");
    assert.ok(!existsSync(seen), "the partial was renamed away");
  } finally { done(dir); }
});

test("encodeMp4: a failed encode, an exit 0 with no output and a rejected clip leave no partial and never touch a good clip already there", () => {
  const dir = scratch();
  try {
    const out = join(dir, "clip.mp4");
    const good = () => writeFileSync(out, "PREVIOUS-GOOD-CLIP");
    const cases = [
      ["a failed encode that left bytes", { writes: { kind: "last_arg", content: "PARTIAL-MP4-BYTES" }, exit: 1 }, {}, /ffmpeg mp4 encode failed/],
      ["exit 0 and nothing written", { exit: 0 }, {}, /ffmpeg mp4 encode failed/],
      ["a clip the verify hook rejects", { writes: { kind: "last_arg", content: "BLACK" } }, { verify: () => { throw new Error("BLACK_CLIP: the clip is 100% black"); } }, /BLACK_CLIP/],
    ];
    for (const [name, spec, opts, re] of cases) {
      good();
      const ffmpeg = makeStub(dir, "ffmpeg", spec);
      assert.throws(() => encodeMp4(ffmpeg, join(dir, "in.webm"), out, 8, 0, opts), re, name);
      assert.equal(readFileSync(out, "utf8"), "PREVIOUS-GOOD-CLIP", `${name}: the previous good clip is untouched`);
      assert.deepEqual(readdirSync(dir).filter((f) => f.includes(".part")), [], `${name}: no partial left`);
    }
    // and with no previous clip, a failure delivers nothing at all
    rmSync(out);
    assert.throws(() => encodeMp4(makeStub(dir, "ffmpeg", { writes: { kind: "last_arg", content: "PARTIAL-MP4-BYTES" }, exit: 1 }), join(dir, "in.webm"), out, 8), /ffmpeg mp4 encode failed/);
    assert.ok(!existsSync(out), "a failed encode delivers no file");
    assert.throws(() => encodeMp4("", "a", "b", 8), /FFMPEG_UNAVAILABLE/);
  } finally { done(dir); }
});

test("encodeMp4: a deadline-killed encode is a timeout, removes its partial and keeps the previous clip", { skip: isWin && "POSIX kill of a script-launched stub" }, () => {
  const dir = scratch();
  try {
    const out = join(dir, "clip.mp4");
    writeFileSync(out, "PREVIOUS-GOOD-CLIP");
    const ffmpeg = makeStub(dir, "ffmpeg", { writes: { kind: "last_arg", content: "HALF" }, hang: true });
    assert.throws(() => encodeMp4(ffmpeg, join(dir, "in.webm"), out, 8, 3000), /ffmpeg mp4 encode timeout \(killed\)/);
    assert.equal(readFileSync(out, "utf8"), "PREVIOUS-GOOD-CLIP");
    assert.deepEqual(readdirSync(dir).filter((f) => f.includes(".part")), []);
  } finally { done(dir); }
}, { timeout: 60000 });

// ---------------------------------------------------------------- SIL11

test("checkClip: a clip whose length could not be read is UNMEASURABLE and says it could not be checked, not that it has no picture", () => {
  const run = () => ({ status: 0, stderr: "Input #0, matroska,webm, from 'x.webm':\n  Stream #0:0: Video: vp9, 64x64, 8 fps\n" });
  assert.throws(() => checkClip("ffmpeg", "x.partial.mp4", { run, label: "clip.mp4" }), (e) => {
    assert.match(e.message, /^UNMEASURABLE: /);
    assert.match(e.message, /could not be checked/);
    assert.match(e.message, /\(clip\.mp4\)/, "names the delivery path, not the partial");
    assert.ok(!/no picture/.test(e.message), "it must not claim the clip has no picture");
    return true;
  });
  assert.equal(UNMEASURABLE, "UNMEASURABLE");
  assert.equal(assessClip({ duration: 0, black: [], frozen: [] }).kind, UNMEASURABLE);
  // a black clip still says what it is
  const black = () => ({ status: 0, stderr: "Duration: 00:00:02.00, start: 0\n  Stream #0:0: Video: h264, 8 fps\n[blackdetect @ 0x1] black_start:0 black_end:1.875 black_duration:1.875\n" });
  assert.throws(() => checkClip("ffmpeg", "p.mp4", { run: black, label: "clip.mp4" }), (e) => e.message.startsWith("BLACK_CLIP: ") && e.message.includes("(clip.mp4)") && e.message.includes("no picture"));
});

// ---------------------------------------------------------------- TST21: helper survivors

function fakePng(file, { width, height, colorType = 2, bitDepth = 8 }) {
  const b = Buffer.alloc(33);
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]).copy(b, 0);
  b.writeUInt32BE(13, 8);
  b.write("IHDR", 12, "latin1");
  b.writeUInt32BE(width, 16);
  b.writeUInt32BE(height, 20);
  b[24] = bitDepth;
  b[25] = colorType;
  writeFileSync(file, b);
}

test("assertRgbFrames: width AND height must match, and so must the colour type, bit depth and the count", () => {
  const dir = scratch();
  try {
    fakePng(join(dir, "00001.png"), { width: 64, height: 64 });
    fakePng(join(dir, "00002.png"), { width: 64, height: 64 });
    assert.equal(assertRgbFrames(dir, { count: 2, width: 64, height: 64 }), 2);
    assert.throws(() => assertRgbFrames(dir, { count: 2, width: 64, height: 32 }), /DEPTH_FRAMES_INVALID/);
    assert.throws(() => assertRgbFrames(dir, { count: 2, width: 32, height: 64 }), /DEPTH_FRAMES_INVALID/);
    assert.throws(() => assertRgbFrames(dir, { count: 3, width: 64, height: 64 }), /DEPTH_FRAMES_INVALID/);
    fakePng(join(dir, "00002.png"), { width: 64, height: 64, colorType: 0 });
    assert.throws(() => assertRgbFrames(dir, { count: 2, width: 64, height: 64 }), /00002\.png.*colour type 0/);
    fakePng(join(dir, "00002.png"), { width: 64, height: 64, bitDepth: 16 });
    assert.throws(() => assertRgbFrames(dir, { count: 2, width: 64, height: 64 }), /16-bit/);
  } finally { done(dir); }
});

test("assessClip: the 95% line and the snap to the clip's end are exact", () => {
  const black = (end) => assessClip({ duration: 10, black: [{ start: 0, end }], frozen: [], fps: 0 });
  assert.equal(black(9.4).ok, true, "94% black is ordinary content (a fade)");
  assert.equal(black(9.6).kind, "BLACK_CLIP");
  const frozen = (end) => assessClip({ duration: 10, black: [], frozen: [{ start: 0, end }], fps: 0 });
  assert.equal(frozen(9.4).ok, true);
  assert.equal(frozen(9.6).kind, "FROZEN_CLIP");
  // snap: a slow clip (1 fps) reaches the end within 0.5 s at most, however long a frame is
  assert.equal(snapSeconds(1), 0.5);
  assert.ok(snapSeconds(24) < 0.06 && snapSeconds(24) > 0.05);
  assert.equal(snapSeconds(0), 0.125);
  assert.equal(assessClip({ duration: 10, black: [{ start: 0, end: 9.0 }], frozen: [], fps: 1 }).ok, true, "a segment 1 s short of the end of a 1 fps clip does not reach it");
  assert.equal(assessClip({ duration: 10, black: [{ start: 0, end: 9.6 }], frozen: [], fps: 1 }).kind, "BLACK_CLIP");
});

test("checkTokenCap: the advice names how to fit - frames at the size, the reference latent frame, and the 5-frame floor", () => {
  const over = (o) => { try { checkTokenCap({ flags: { "max-tokens": String(o.cap), "vae-stride": "16" }, ...o }); } catch (e) { return e.message; } return ""; };
  // 832x480: 390 tokens per latent frame. cap 780 = exactly two latent frames = 5 frames fit
  assert.match(over({ cap: 780, width: 832, height: 480, frames: 49, refLatentFrames: 0 }), /up to 5 frames fit/);
  // cap 779 = one latent frame: not even 5 frames
  assert.match(over({ cap: 779, width: 832, height: 480, frames: 49, refLatentFrames: 0 }), /even 5 frames do not fit at 832x480/);
  // the reference takes one of them: cap 1170 = 3 latent frames - 1 = 2 -> 5 frames; cap 1169 -> none
  assert.match(over({ cap: 1170, width: 832, height: 480, frames: 49, refLatentFrames: 1 }), /up to 5 frames fit/);
  assert.match(over({ cap: 1169, width: 832, height: 480, frames: 49, refLatentFrames: 1 }), /even 5 frames do not fit/);
  // a bigger cap: (latentFrames - 1) * 4 + 1
  assert.match(over({ cap: 2000, width: 832, height: 480, frames: 129, refLatentFrames: 0 }), /up to 17 frames fit/);
  assert.match(over({ cap: 2000, width: 832, height: 480, frames: 129, refLatentFrames: 0 }), /\(VAE stride 16, patch 2\)/);
});

test("tokenCapFromFlags: no cap, an empty cap and a bad cap", () => {
  assert.equal(tokenCapFromFlags({}), null);
  assert.equal(tokenCapFromFlags({ "max-tokens": "" }), null, "an empty value is no cap");
  assert.equal(tokenCapFromFlags({ "max-tokens": undefined }), null);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "4.5", "vae-stride": "16" }), /positive integer/);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "-3", "vae-stride": "16" }), /positive integer/);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "abc", "vae-stride": "16" }), /positive integer/);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "100", "vae-stride": "12" }), /8 or 16/);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "100" }), /8 or 16/);
  assert.deepEqual(tokenCapFromFlags({ "max-tokens": "100", "vae-stride": "8" }), { cap: 100, stride: 8 });
});

test("latentTokens: a frame count whose latent frames are fractional FLOORS (52 frames = 12 latent steps, not 13)", () => {
  assert.equal(latentTokens({ width: 832, height: 480, frames: 52, stride: 16 }), 26 * 15 * 13);
  assert.equal(latentTokens({ width: 832, height: 480, frames: 53, stride: 16 }), 26 * 15 * 14);
  assert.equal(latentTokens({ width: 832, height: 480, frames: 51, stride: 16 }), 26 * 15 * 13);
});

test("ensureOutDir: a directory that exists but cannot be written is OUT_DIR_UNWRITABLE", { skip: (isWin || process.getuid?.() === 0) && "needs a POSIX non-root user" }, () => {
  const dir = scratch();
  try {
    const ro = join(dir, "ro");
    mkdirSync(ro);
    chmodSync(ro, 0o555);
    assert.throws(() => ensureOutDir(join(ro, "clip.mp4")), msgStarts("OUT_DIR_UNWRITABLE:"));
    chmodSync(ro, 0o755);
    assert.equal(ensureOutDir(join(ro, "clip.mp4")), ro);
  } finally { done(dir); }
});

test("modelMetadataError: either of sd-cli's two phrases alone is enough; neither is not", () => {
  assert.match(modelMetadataError("[ERROR] tensor 'a.b' not in model metadata", "/m/x.safetensors").message, /^MODEL_INCOMPATIBLE: .*tensor 'a\.b'/);
  assert.match(modelMetadataError("[ERROR] model metadata validation failed", "/m/x.safetensors").message, /^MODEL_INCOMPATIBLE: .*model metadata validation failed/);
  assert.equal(modelMetadataError("[ERROR] something else", "/m/x.safetensors"), null);
});

test("screenExtraArgs: a cpu word is found behind ':' and whitespace; a path that merely holds the letters is not a placement", () => {
  for (const a of [["--x", "a:cpu"], ["--x", "a cpu"], ["--x", "a\tcpu"], ["--x=a b:cpu"]]) assert.ok(screenExtraArgs(a), JSON.stringify(a));
  for (const a of [["--lora-model-dir", "/data/cpu-loras"], ["--lora-model-dir=/data/cpu-loras"], ["/data/cpu-loras"], ["--threads", "4"]]) {
    assert.equal(screenExtraArgs(a), null, JSON.stringify(a));
  }
});
