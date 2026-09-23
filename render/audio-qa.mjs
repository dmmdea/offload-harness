// audio-qa.mjs — post-render QA gate for generated audio (F-35 regression follow-up,
// 2026-09-23; root cause refined 2026-09-23 on an 8GB box, ComfyUI 95539f56, ACE-Step
// 1.5 XL turbo, the harness's own render/wf-acestep.mjs graph). Root cause: with
// generate_audio_codes true, comfy/text_encoders/ace15.py's ACE-Step LM always emits
// exactly duration*5 audio codes (min=max) and reliably PLANS the song to end 2-6s
// BEFORE the requested duration, filling the remainder with silence codes — a hard
// cut to about -65 dBFS, not a fade. Measured per-second RMS at 30s requested (same
// prompt, two seeds): seed 7 -> real music to 27.99s, then 2.01s silence; seed
// 759155896809805 -> music to 24.89s, then 5.11s silence (17% of the clip). Asking for
// MORE than the target duration avoids it: at 36s requested, three independent seeds
// put music across the WHOLE requested span (seed 7 to 31.11s; seed 759155896809805 to
// 30.24s; seed 11 to about 34s then a natural fade) — the LM's early-ending behavior
// scales with the requested length, not with wall-clock content. Turning
// generate_audio_codes off removes the planned ending but drops the LM the model card
// names as the quality path and let one seed's level pump 15dB second-to-second — not
// an option. render/comfy-music.mjs's generate() therefore renders OVER-LENGTH
// (renderSeconds = seconds + max(6, ceil(0.2*seconds)), see computeRenderSeconds
// there) whenever the graph is built from args and ffmpeg/ffprobe resolve, then this
// module's trimToSeconds() cuts the file back to the requested seconds (1.0s
// fade-out on the cut) BEFORE the gate below ever measures it. What follows is the
// defense-in-depth the constitution's media-QA-gate carve-out calls for, for whatever
// the trim doesn't catch (a seed whose cut lands inside the trimmed window, or a
// caller-supplied --graph that skips the over-render since its duration is opaque to
// this module): measure, retry once with a new seed, then typed-defer if it persists;
// always normalize loudness to a safe target on the accepted result.
//
// Dependency-free (Node 18+, matches the render/*.mjs convention): shells out to
// ffmpeg/ffprobe exactly the way audioio.go does on the Go side (same configured
// binary — comfy-music.mjs receives FFMPEG_PATH from Pipeline.genEnv()). ffmpeg
// missing is NEVER a render failure — the gate degrades to a skip (best-effort,
// never withholds an already-produced render per the house content-preservation
// rule).
import { spawnSync } from "node:child_process";
import { dirname, join, extname } from "node:path";
import { existsSync } from "node:fs";

// resolveFfmpeg: $FFMPEG_PATH (threaded from Pipeline.genEnv on a configured
// machine) > "ffmpeg" on PATH (probed by actually spawning it — a bare existsSync
// can't see PATH resolution). Returns "" when neither works, never throws — the
// caller treats "" as "skip the gate".
export function resolveFfmpeg(env = process.env) {
  const explicit = env.FFMPEG_PATH;
  if (explicit) {
    return existsSync(explicit) ? explicit : "";
  }
  const probe = spawnSync("ffmpeg", ["-version"], { stdio: "ignore" });
  return probe.error ? "" : "ffmpeg";
}

// resolveFfprobe: sibling of the resolved ffmpeg (same dir, same extension) if that
// exists, else "ffprobe" on PATH, else "".
export function resolveFfprobe(ffmpegPath) {
  if (ffmpegPath && ffmpegPath !== "ffmpeg") {
    const dir = dirname(ffmpegPath);
    const ext = extname(ffmpegPath);
    const sibling = join(dir, "ffprobe" + ext);
    if (existsSync(sibling)) return sibling;
  }
  const probe = spawnSync("ffprobe", ["-version"], { stdio: "ignore" });
  return probe.error ? "" : "ffprobe";
}

// durationSec: the file's duration via ffprobe. Returns 0 on any failure (caller
// treats 0 as "cannot assess — skip").
export function durationSec(ffprobePath, file) {
  const r = spawnSync(ffprobePath, [
    "-v", "error", "-show_entries", "format=duration",
    "-of", "default=noprint_wrappers=1:nokey=1", file,
  ], { encoding: "utf8" });
  const n = Number(String(r.stdout || "").trim());
  return Number.isFinite(n) && n > 0 ? n : 0;
}

// parseSilences: pairs ffmpeg silencedetect's "silence_start: X" / "silence_end: Y |
// silence_duration: Z" lines (stderr) into [{start,end,duration}, ...], in order.
// Exported for unit testing against captured ffmpeg output without spawning it.
export function parseSilences(stderrText) {
  const starts = [...stderrText.matchAll(/silence_start:\s*(-?[\d.]+)/g)].map((m) => Number(m[1]));
  const ends = [...stderrText.matchAll(/silence_end:\s*(-?[\d.]+)\s*\|\s*silence_duration:\s*(-?[\d.]+)/g)]
    .map((m) => ({ end: Number(m[1]), duration: Number(m[2]) }));
  const n = Math.min(starts.length, ends.length);
  const out = [];
  for (let i = 0; i < n; i++) out.push({ start: starts[i], end: ends[i].end, duration: ends[i].duration });
  return out;
}

// parseLoudness: the LAST "I: <x> LUFS" and the (Summary-only) "Peak: <y> dBFS"
// lines from a combined silencedetect+ebur128 run. Per-tick ebur128 lines repeat
// "I:" throughout the file; taking the last match lands on the final Summary value.
// "Peak:" (as opposed to the per-tick "TPK:") only appears in the Summary's "True
// peak:" section, so the first/only match is already the right one.
export function parseLoudness(stderrText) {
  const iMatches = [...stderrText.matchAll(/\bI:\s*(-?[\d.]+)\s*LUFS/g)];
  const peakMatches = [...stderrText.matchAll(/\bPeak:\s*(-?[\d.]+)\s*dBFS/g)];
  const integratedLUFS = iMatches.length ? Number(iMatches[iMatches.length - 1][1]) : null;
  const truePeakDBFS = peakMatches.length ? Number(peakMatches[peakMatches.length - 1][1]) : null;
  return { integratedLUFS, truePeakDBFS };
}

// measure: run ffmpeg once (silencedetect + ebur128 chained, one decode pass) plus
// ffprobe for duration. Returns null when ffmpeg/ffprobe are unavailable or the
// spawn fails — the caller must treat null as "skip the gate", never as "clean".
export function measure(ffmpegPath, ffprobePath, file, { noiseDB = -45, minSilenceSec = 0.5 } = {}) {
  if (!ffmpegPath || !ffprobePath) return null;
  const duration = durationSec(ffprobePath, file);
  if (!duration) return null;
  const r = spawnSync(ffmpegPath, [
    "-hide_banner", "-nostats", "-i", file,
    "-af", `silencedetect=noise=${noiseDB}dB:d=${minSilenceSec},ebur128=peak=true`,
    "-f", "null", "-",
  ], { encoding: "utf8" });
  if (r.error) return null;
  const text = String(r.stderr || "");
  const silences = parseSilences(text);
  const { integratedLUFS, truePeakDBFS } = parseLoudness(text);
  return { duration, silences, integratedLUFS, truePeakDBFS };
}

// assessDeadAir: the QA gate's verdict. "Dead air" per the JOB spec: trailing OR
// leading silence > 1.0s, OR total silence > 10% of the clip. A silence block is
// "leading"/"trailing" when it touches the very start/end (100ms tolerance for
// ffmpeg's float rounding). Pure function — unit-testable against `measure()`'s
// output shape without spawning ffmpeg.
export function assessDeadAir(measured, { edgeToleranceSec = 0.1, maxEdgeSilenceSec = 1.0, maxSilentFraction = 0.10 } = {}) {
  if (!measured) return { deadAir: false, skipped: true, reason: "no measurement (ffmpeg/ffprobe unavailable)" };
  const { duration, silences } = measured;
  const leading = silences.find((s) => s.start <= edgeToleranceSec);
  const trailing = silences.find((s) => duration - s.end <= edgeToleranceSec);
  const leadingSec = leading ? leading.duration : 0;
  const trailingSec = trailing ? trailing.duration : 0;
  const totalSilentSec = silences.reduce((a, s) => a + s.duration, 0);
  const silentFraction = totalSilentSec / duration;
  const badLeading = leadingSec > maxEdgeSilenceSec;
  const badTrailing = trailingSec > maxEdgeSilenceSec;
  const badFraction = silentFraction > maxSilentFraction;
  const deadAir = badLeading || badTrailing || badFraction;
  const reasons = [];
  if (badTrailing) reasons.push(`trailing silence ${trailingSec.toFixed(2)}s > ${maxEdgeSilenceSec}s`);
  if (badLeading) reasons.push(`leading silence ${leadingSec.toFixed(2)}s > ${maxEdgeSilenceSec}s`);
  if (badFraction) reasons.push(`${(silentFraction * 100).toFixed(1)}% of the clip is silent (> ${maxSilentFraction * 100}%)`);
  return {
    deadAir, skipped: false,
    leadingSec, trailingSec, silentFraction, duration,
    reason: reasons.join("; ") || "clean",
  };
}

// TRUE_PEAK_TARGET_DBTP / LOUDNESS_TARGET_LUFS: the normalization target applied to
// every accepted render (independent of the dead-air verdict — the 0.0 dBFS true
// peak measured on two of three A/B renders is a SEPARATE defect from dead air and
// needs fixing regardless). -14 LUFS integrated / -1 dBTP true peak matches the
// common streaming-loudness convention (Spotify/YouTube target -14 LUFS; -1 dBTP
// keeps headroom below 0 dBFS so a downstream lossy re-encode's peak overshoot
// can't clip). Single-pass loudnorm (not the two-pass measure-then-apply form): a
// 15-30s clip is short enough that single-pass's slightly less precise convergence
// is an acceptable trade against a second ffmpeg pass doubling this gate's latency
// on every render.
export const LOUDNESS_TARGET_LUFS = -14;
export const TRUE_PEAK_TARGET_DBTP = -1;
const LOUDNESS_RANGE_TARGET_LU = 11;

// normalizeLoudness: single-pass ffmpeg loudnorm to LOUDNESS_TARGET_LUFS /
// TRUE_PEAK_TARGET_DBTP, re-encoding `file` to `tmpOut` (caller renames over the
// original — never overwrite in place, ffmpeg cannot read and write the same file).
// Best-effort: returns false (leaves `file` untouched) on any ffmpeg failure rather
// than throwing, so a normalization hiccup never costs the render itself (house
// rule: never withhold already-produced content).
export function normalizeLoudness(ffmpegPath, file, tmpOut) {
  if (!ffmpegPath) return false;
  const r = spawnSync(ffmpegPath, [
    "-hide_banner", "-loglevel", "error", "-y", "-i", file,
    "-af", `loudnorm=I=${LOUDNESS_TARGET_LUFS}:TP=${TRUE_PEAK_TARGET_DBTP}:LRA=${LOUDNESS_RANGE_TARGET_LU}`,
    tmpOut,
  ], { encoding: "utf8" });
  return !r.error && r.status === 0 && existsSync(tmpOut);
}

// trimToSeconds: re-encodes `file` down to exactly `seconds` long, with a
// fadeSec-long fade-out (ffmpeg afade=t=out) ending exactly at the cut, writing the
// result to `tmpOut` (caller renames over the original — same convention as
// normalizeLoudness; ffmpeg cannot read and write the same file). This is the
// over-render mitigation's other half (see the module header and
// comfy-music.mjs's computeRenderSeconds): comfy-music.mjs renders LONGER than the
// caller asked for, and this cuts it back to exactly what was requested before the
// dead-air gate below ever sees the file, so the ACE-Step LM's own silence-padded
// tail never reaches the measurement. Best-effort: returns false (leaves `file`
// untouched) on any ffmpeg failure or a non-positive `seconds`, rather than
// throwing — the house rule that a QA/post-processing hiccup never costs an
// already-produced render applies here exactly as it does to normalizeLoudness.
export function trimToSeconds(ffmpegPath, file, seconds, tmpOut, { fadeSec = 1.0 } = {}) {
  if (!ffmpegPath || !(seconds > 0)) return false;
  const fadeStart = Math.max(0, seconds - fadeSec);
  const r = spawnSync(ffmpegPath, [
    "-hide_banner", "-loglevel", "error", "-y", "-i", file,
    "-t", String(seconds),
    "-af", `afade=t=out:st=${fadeStart}:d=${fadeSec}`,
    tmpOut,
  ], { encoding: "utf8" });
  return !r.error && r.status === 0 && existsSync(tmpOut);
}

// rewireSeed: returns a NEW graph (shallow clone; original untouched) with every
// node's inputs.seed overwritten to newSeed. Node-id-agnostic (matches by an
// inputs.seed field rather than hardcoding wf-acestep.mjs's node ids "4"/"8") so it
// also works on a caller-supplied --graph whose ids may differ.
export function rewireSeed(graph, newSeed) {
  const out = {};
  for (const [id, node] of Object.entries(graph)) {
    if (node && node.inputs && typeof node.inputs.seed === "number") {
      out[id] = { ...node, inputs: { ...node.inputs, seed: newSeed } };
    } else {
      out[id] = node;
    }
  }
  return out;
}
