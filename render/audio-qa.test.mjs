// node --test render/audio-qa.test.mjs
// Tests the ACE-Step dead-air/loudness QA gate (F-35 regression follow-up,
// 2026-09-23). Pure-function tests run with no ffmpeg. The fixture-audio tests need
// a real ffmpeg/ffprobe (they generate lavfi tones, not npm audio deps) and skip
// cleanly when neither is on PATH, matching the Go side's lookFFmpeg() convention
// (internal/pipeline/mediagate_test.go).
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdtempSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  parseSilences, parseLoudness, assessDeadAir, rewireSeed,
  resolveFfmpeg, resolveFfprobe, measure, normalizeLoudness,
  LOUDNESS_TARGET_LUFS,
} from "./audio-qa.mjs";

// ---- pure-function tests (no ffmpeg) ---------------------------------------------

test("parseSilences: pairs silence_start/silence_end|silence_duration lines in order", () => {
  // Captured verbatim shape from a real silencedetect run (d2-music-15s.flac).
  const text = `
[Parsed_silencedetect_0 @ 0x1] silence_start: 8.770292
[Parsed_silencedetect_0 @ 0x1] silence_end: 15 | silence_duration: 6.229708
`;
  const got = parseSilences(text);
  assert.deepEqual(got, [{ start: 8.770292, end: 15, duration: 6.229708 }]);
});

test("parseSilences: multiple spans (leading + trailing) stay ordered and paired", () => {
  const text = `
silence_start: 0
silence_end: 0.3 | silence_duration: 0.3
silence_start: 11.5
silence_end: 15 | silence_duration: 3.5
`;
  const got = parseSilences(text);
  assert.equal(got.length, 2);
  assert.deepEqual(got[0], { start: 0, end: 0.3, duration: 0.3 });
  assert.deepEqual(got[1], { start: 11.5, end: 15, duration: 3.5 });
});

test("parseSilences: no matches on clean audio returns []", () => {
  assert.deepEqual(parseSilences("no silence filter output here"), []);
});

test("parseLoudness: takes the Summary block's final I:/Peak:, not a per-tick line", () => {
  // Per-tick ebur128 lines also contain "I: ... LUFS" (but never "Peak:" — that's
  // "TPK:" per-tick); the Summary block comes last and has the real answer.
  const text = `
[Parsed_ebur128_0] t: 0.1  M: -20.0 S: -20.0  I: -25.0 LUFS  LRA: 0.0 LU  TPK: -10.0 -10.0 dBFS
[Parsed_ebur128_0] t: 14.9 M: -19.0 S: -19.0  I: -19.0 LUFS  LRA: 1.2 LU  TPK: -8.2 -6.3 dBFS
[Parsed_ebur128_0] Summary:

  Integrated loudness:
    I:         -19.0 LUFS

  Loudness range:
    LRA:         7.8 LU

  True peak:
    Peak:       -6.3 dBFS
`;
  const { integratedLUFS, truePeakDBFS } = parseLoudness(text);
  assert.equal(integratedLUFS, -19.0);
  assert.equal(truePeakDBFS, -6.3);
});

test("parseLoudness: no ebur128 output returns nulls, not zeros (never fabricate a measurement)", () => {
  const { integratedLUFS, truePeakDBFS } = parseLoudness("nothing here");
  assert.equal(integratedLUFS, null);
  assert.equal(truePeakDBFS, null);
});

test("assessDeadAir: skips (never flags) when measurement is unavailable", () => {
  const v = assessDeadAir(null);
  assert.equal(v.deadAir, false);
  assert.equal(v.skipped, true);
});

test("assessDeadAir: a tail-silent fixture (measured shape) MUST trigger dead air", () => {
  // Mirrors the measured d2-music-15s.flac / d3-...flac shape: 15s clip, trailing
  // silence from 8.77s to the end.
  const measured = { duration: 15, silences: [{ start: 8.770292, end: 15, duration: 6.229708 }] };
  const v = assessDeadAir(measured);
  assert.equal(v.deadAir, true);
  assert.ok(v.reason.includes("trailing"), `expected a trailing-silence reason, got: ${v.reason}`);
});

test("assessDeadAir: a clean fixture (no silence spans) MUST pass", () => {
  const measured = { duration: 15, silences: [] };
  const v = assessDeadAir(measured);
  assert.equal(v.deadAir, false);
  assert.equal(v.reason, "clean");
});

test("assessDeadAir: a short leading blip under 1.0s does not trip the gate (only > 1.0s edges count)", () => {
  const measured = { duration: 15, silences: [{ start: 0, end: 0.4, duration: 0.4 }] };
  const v = assessDeadAir(measured);
  assert.equal(v.deadAir, false);
});

test("assessDeadAir: > 10% total silence trips the gate even with no single edge over 1.0s", () => {
  // Five 0.4s silences scattered through a 15s clip = 2.0s = 13.3% > 10%, but no
  // single span touches an edge or exceeds 1.0s alone.
  const silences = Array.from({ length: 5 }, (_, i) => ({ start: 2 + i * 2, end: 2 + i * 2 + 0.4, duration: 0.4 }));
  const v = assessDeadAir({ duration: 15, silences });
  assert.equal(v.deadAir, true);
  assert.ok(v.reason.includes("%"), `expected a fraction-based reason, got: ${v.reason}`);
});

test("rewireSeed: overwrites every node's inputs.seed, leaves other nodes and the original graph untouched", () => {
  const graph = {
    "4": { class_type: "TextEncodeAceStepAudio1.5", inputs: { seed: 111, tags: "x" } },
    "8": { class_type: "KSampler", inputs: { seed: 111, steps: 8 } },
    "6": { class_type: "EmptyAceStep1.5LatentAudio", inputs: { seconds: 15 } }, // no seed field
  };
  const out = rewireSeed(graph, 999);
  assert.equal(out["4"].inputs.seed, 999);
  assert.equal(out["8"].inputs.seed, 999);
  assert.equal(out["4"].inputs.tags, "x", "non-seed fields survive");
  assert.equal(out["6"].inputs.seconds, 15, "a node with no seed field is untouched");
  // Original graph is NOT mutated (generate()'s retry needs the original seed's
  // graph to still read 111 for logging/telemetry).
  assert.equal(graph["4"].inputs.seed, 111);
  assert.equal(graph["8"].inputs.seed, 111);
});

// ---- fixture-audio tests (need real ffmpeg/ffprobe) ------------------------------

function haveFfmpeg() {
  const ffmpeg = resolveFfmpeg();
  const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
  return ffmpeg && ffprobe ? { ffmpeg, ffprobe } : null;
}

// makeToneWav: a lavfi-generated WAV, tone for toneSec then true digital silence for
// silenceSec (anullsrc, not just a quiet tone — an unambiguous fixture). No network,
// no model, no npm audio dependency — ffmpeg's built-in synthetic sources only.
function makeToneWav(ffmpeg, dir, name, toneSec, silenceSec) {
  const out = join(dir, name);
  const filter = silenceSec > 0
    ? `sine=frequency=440:duration=${toneSec},aeval=val(0)|val(0)[a];anullsrc=r=44100:cl=stereo:d=${silenceSec}[b];[a][b]concat=n=2:v=0:a=1`
    : `sine=frequency=440:duration=${toneSec}`;
  const r = spawnSync(ffmpeg, [
    "-hide_banner", "-loglevel", "error", "-y",
    "-f", "lavfi", "-i", filter,
    "-ar", "44100", "-ac", "2", out,
  ], { encoding: "utf8" });
  assert.equal(r.status, 0, `fixture generation failed: ${r.stderr}`);
  assert.ok(existsSync(out), "fixture file was not written");
  return out;
}

test("fixture: a genuinely tail-silent clip is measured and flagged dead-air end to end", (t) => {
  const bins = haveFfmpeg();
  if (!bins) return t.skip("ffmpeg/ffprobe not on PATH");
  const dir = mkdtempSync(join(tmpdir(), "audio-qa-"));
  const file = makeToneWav(bins.ffmpeg, dir, "tail-silent.wav", 8, 7); // 8s tone + 7s true silence = 15s
  const measured = measure(bins.ffmpeg, bins.ffprobe, file);
  assert.ok(measured, "measure() must return a result for a real file");
  assert.ok(Math.abs(measured.duration - 15) < 0.5, `duration = ${measured.duration}, want ~15`);
  const verdict = assessDeadAir(measured);
  assert.equal(verdict.deadAir, true, `expected dead air, got: ${JSON.stringify(verdict)}`);
});

test("fixture: a clip with tone all the way through passes clean", (t) => {
  const bins = haveFfmpeg();
  if (!bins) return t.skip("ffmpeg/ffprobe not on PATH");
  const dir = mkdtempSync(join(tmpdir(), "audio-qa-"));
  const file = makeToneWav(bins.ffmpeg, dir, "clean.wav", 15, 0);
  const measured = measure(bins.ffmpeg, bins.ffprobe, file);
  assert.ok(measured, "measure() must return a result for a real file");
  const verdict = assessDeadAir(measured);
  assert.equal(verdict.deadAir, false, `expected clean, got: ${JSON.stringify(verdict)}`);
});

test("fixture: normalizeLoudness moves a loud clip's measured loudness toward the target", (t) => {
  const bins = haveFfmpeg();
  if (!bins) return t.skip("ffmpeg/ffprobe not on PATH");
  const dir = mkdtempSync(join(tmpdir(), "audio-qa-"));
  const file = makeToneWav(bins.ffmpeg, dir, "loud.wav", 5, 0);
  const before = measure(bins.ffmpeg, bins.ffprobe, file);
  const tmpOut = join(dir, "loud.norm.wav");
  const ok = normalizeLoudness(bins.ffmpeg, file, tmpOut);
  assert.ok(ok, "normalizeLoudness should succeed on a valid fixture");
  const after = measure(bins.ffmpeg, bins.ffprobe, tmpOut);
  assert.ok(after.integratedLUFS !== null, "normalized file must report a measurable loudness");
  // Must land materially closer to the target than the source did.
  const beforeDist = Math.abs((before.integratedLUFS ?? -100) - LOUDNESS_TARGET_LUFS);
  const afterDist = Math.abs(after.integratedLUFS - LOUDNESS_TARGET_LUFS);
  assert.ok(afterDist < beforeDist, `normalization did not move loudness toward target: before=${before.integratedLUFS} after=${after.integratedLUFS} target=${LOUDNESS_TARGET_LUFS}`);
  assert.ok(afterDist < 1.0, `normalized loudness ${after.integratedLUFS} LUFS is not within 1 LU of the ${LOUDNESS_TARGET_LUFS} LUFS target`);
});

test("resolveFfmpeg: an explicit FFMPEG_PATH to a nonexistent file resolves to empty, not a bad path", () => {
  const got = resolveFfmpeg({ FFMPEG_PATH: "Z:/nonexistent/ffmpeg.exe" });
  assert.equal(got, "");
});
