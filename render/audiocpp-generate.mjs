// audiocpp-generate.mjs — voice and music on the iGPU tier via audio.cpp `audiocpp_cli`
// (CT-49; v0.9.0, Apache-2.0). A spawn-per-job native binary under the shared GPU lease:
// zero-warm, nothing resident, no Python, no ComfyUI.
//
//   voice  --task tts           --family chatterbox --model <gguf> --text T --language L
//          --task clon (when --clone is given) adds --voice-ref <wav>
//   music  --task gen           --family ace_step   --model <gguf> --text <style prompt>
//          [--lyrics L] --duration-seconds S
//   common --backend <vulkan> --device <n> --seed <n> --metrics --log --out <wav>
// (verified against audiocpp_cli --help and model_specs/chatterbox.json + ace_step.json;
// --task takes `clon`, the CLI enum, not the model-spec word `clone`). The mapping lives
// HERE: audio.cpp flag drift on a pin bump is fixed in this file, never in Go.
//
// Usage: node render/audiocpp-generate.mjs [flags] -- <out.wav> "<text>"
//        --kind voice|music --bin ABS --family F --model P --backend vulkan --device 0
//        [--clone <ref.wav>] [--lang es] [--seconds N] [--lyrics S] [--seed N]
//        [--extra-args '<json array>'] [--timeout-sec N] [--no-lock]
// (the positionals may also come first when neither starts with "--"; the harness sends `--`
// before them so lyrics or text such as "--- Intro ---" stay positional.)
// Env:   FFMPEG_PATH — ffmpeg (else ffmpeg on PATH); ffprobe beside it or on PATH.
//
// BACKEND VALUES ARE audio.cpp's OWN: --backend vulkan|cuda|hip|rocm|metal and the device index
// as a separate --device N. "vulkan0" is sd.cpp's spelling and is refused here (the CLI would
// reject it at run time); cpu and best are refused (no model runs on CPU).
//
// THE NO-CPU RULE: a non-GPU --backend, and any --extra-args element that changes the backend or
// placement (--backend, --device, ...), is refused before anything spawns
// (CPU_BACKEND_REFUSED / EXTRA_ARGS_REFUSED). The engine's log (--log) is a POSITIVE guard
// (igpu-engine.mjs createLogGuard): the run passes only with a "<component>.weights.buffer_name
// Vulkan<N>" line, ANY "*.weights.buffer_name CPU" line kills it with CPU_PLACEMENT, and a run
// that ends with no GPU evidence is CPU_PLACEMENT too.
// DEADLINE: --timeout-sec counts from this process's start (the llama-swap drain spends it);
// SIGTERM/SIGINT/SIGHUP and a vanished parent kill the engine and remove the temp dir.
//
// FINALIZE (finalizeAudio): every ffmpeg step must succeed or the job fails with its stderr tail
// (never a silent copy of the raw engine file). Music: trim trailing silence below -45 dB (the
// measured ACE-Step piece ends 3.6 s before a 30 s request and pads the rest with silence), a
// short fade-out, loudnorm to -14 LUFS / -1 dBTP, 48 kHz. Voice is delivered as the engine
// wrote it (re-encoded only for a non-wav extension). Both kinds then pass the repo's dead-air gate
// (audio-qa.mjs, the way comfy-music.mjs applies it) on the file as delivered: a render that
// is silent (a Vulkan fp16 / NaN failure writes zeros and exits 0) fails typed as DEAD_AIR and
// the file is removed. ffmpeg AND ffprobe are required up front (FFMPEG_UNAVAILABLE, before the
// lease): audio nobody can verify is not a usable result.
import { existsSync, copyFileSync, rmSync } from "node:fs";
import { join, extname } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import {
  resolveFfmpeg, resolveFfprobe, measure, assessDeadAir, durationSec, LOUDNESS_TARGET_LUFS, TRUE_PEAK_TARGET_DBTP,
} from "./audio-qa.mjs";
import {
  parseArgs, parseExtraArgs, refuseExtraArgs, runEngine, createLogGuard, installLifecycle,
  makeDeadline, finiteNum, makeTempDir, refuseRelativeBinary, ensureOutDir, CPU_BACKEND_REFUSED,
  engineExitError, reportFatal,
} from "./igpu-engine.mjs";
import { defaultRun, runFailure } from "./igpu-qa.mjs";

export { parseArgs };

export const DEFAULT_LANG = "es";
export const AUDIO_BACKENDS = ["vulkan", "cuda", "hip", "rocm", "metal"];
// the trailing-silence threshold, the kept tail and the fade-out length of the music chain
export const TAIL_SILENCE_DB = -45;
export const TAIL_KEEP_SEC = 0.15;
export const FADE_OUT_SEC = 1.0;
export const LOUDNORM_LRA = 11;

// refuseAudioBackend: audio.cpp's GPU backends only (mirrors config.AudiocppBackendRefusal).
// Returns the trimmed backend.
export function refuseAudioBackend(backend) {
  const b = String(backend ?? "").trim();
  if (b === "") throw new Error(`${CPU_BACKEND_REFUSED}: --backend is unset (name an audio.cpp GPU backend such as vulkan; no model runs on CPU on this engine)`);
  if (!AUDIO_BACKENDS.includes(b.toLowerCase())) {
    const hint = /^vulkan\d+$/i.test(b) ? ` audio.cpp takes the device index separately: --backend vulkan --device ${b.replace(/^vulkan/i, "")}.` : "";
    throw new Error(`${CPU_BACKEND_REFUSED}: --backend ${JSON.stringify(b)} is not an audio.cpp GPU backend (want ${AUDIO_BACKENDS.join(", ")}; cpu and best are refused: no model runs on CPU on this engine).${hint}`);
  }
  return b.toLowerCase();
}

// refuseAudioDevice: the --device index, a non-negative integer ("" / unset = 0).
export function refuseAudioDevice(device) {
  const d = String(device ?? "").trim();
  if (d === "") return "0";
  if (!/^\d+$/.test(d)) throw new Error(`--device ${JSON.stringify(d)} is not a device index (a non-negative integer such as 0; audio.cpp takes the backend and the device separately)`);
  return d;
}

// buildAudiocppArgs: the audiocpp_cli argv. Pure; unit-tested without spawning. --log is
// required: the CPU-placement guard reads the backend line it prints.
export function buildAudiocppArgs({ kind, outFile, text, flags, extra = [] }) {
  if (kind !== "voice" && kind !== "music") throw new Error(`--kind must be voice or music (got ${kind})`);
  const clone = kind === "voice" && flags.clone;
  const task = kind === "music" ? "gen" : clone ? "clon" : "tts";
  const a = ["--task", task, "--family", flags.family, "--model", flags.model,
    "--backend", flags.backend, "--device", String(flags.device ?? "0"), "--text", text];
  if (kind === "voice") {
    a.push("--language", flags.lang || DEFAULT_LANG);
    if (clone) a.push("--voice-ref", flags.clone);
  } else {
    if (flags.lyrics) a.push("--lyrics", flags.lyrics);
    const s = finiteNum(flags.seconds);
    if (s !== undefined && s > 0) a.push("--duration-seconds", String(s));
  }
  if (finiteNum(flags.seed) !== undefined) a.push("--seed", String(Math.round(finiteNum(flags.seed))));
  a.push("--metrics", "--log");
  for (const e of extra) a.push(e);
  a.push("--out", outFile);
  return a;
}

// ---------------------------------------------------------------- finalize

const FF = ["-hide_banner", "-loglevel", "error", "-y"];

// buildTrimTailArgs: cut the trailing silence below TAIL_SILENCE_DB, keeping TAIL_KEEP_SEC of it.
// silenceremove only trims the START of a stream, so the clip is reversed, trimmed, reversed
// back (the whole clip is buffered: seconds of audio, never a problem). Float PCM so nothing
// is quantized between steps.
export function buildTrimTailArgs({ src, dst }) {
  return [...FF, "-i", src,
    "-af", `areverse,silenceremove=start_periods=1:start_threshold=${TAIL_SILENCE_DB}dB:start_silence=${TAIL_KEEP_SEC},areverse`,
    "-c:a", "pcm_f32le", dst];
}

// fadeSeconds: the fade-out length for a clip of `duration` seconds (never more than a tenth of it).
export function fadeSeconds(duration) {
  return Math.max(0.05, Math.min(FADE_OUT_SEC, duration / 10));
}

// buildMasterArgs: fade-out, loudnorm to -14 LUFS / -1 dBTP and 48 kHz in ONE pass to `dst`
// (the container follows dst's extension). loudnorm upsamples to 192 kHz internally, so the
// explicit -ar 48000 is what keeps the delivered file encodable (flac, mp3) and sensible.
export function buildMasterArgs({ src, dst, duration }) {
  const fade = fadeSeconds(duration);
  return [...FF, "-i", src,
    "-af", `afade=t=out:st=${Math.max(0, duration - fade).toFixed(3)}:d=${fade.toFixed(3)},loudnorm=I=${LOUDNESS_TARGET_LUFS}:TP=${TRUE_PEAK_TARGET_DBTP}:LRA=${LOUDNORM_LRA}`,
    "-ar", "48000", dst];
}

// buildConvertArgs: a plain re-encode to dst's extension at 48 kHz (voice with a non-wav out).
export function buildConvertArgs({ src, dst }) {
  return [...FF, "-i", src, "-ar", "48000", dst];
}

// runStep: one ffmpeg step; any failure is an error carrying the stderr tail, and the output it
// was meant to produce must exist.
function runStep(run, ffmpeg, args, dst, what) {
  const r = run(ffmpeg, args);
  if (r.error || r.status !== 0) throw runFailure(what, r);
  if (!existsSync(dst)) throw new Error(`${what} failed: ffmpeg exited 0 but wrote no ${dst}`);
}

// gateDeadAir: the repo's dead-air gate (audio-qa.mjs) on the file as delivered. A file the
// gate cannot measure (ffprobe sees no duration) is a failure too: ffprobe was verified up
// front, so "cannot measure" means the audio is empty or unreadable.
export function gateDeadAir({ ffmpeg, ffprobe, file, measureFn = measure }) {
  const verdict = assessDeadAir(measureFn(ffmpeg, ffprobe, file));
  if (verdict.skipped) throw new Error(`DEAD_AIR: the delivered audio could not be measured (${verdict.reason}): it is empty or unreadable`);
  if (verdict.deadAir) throw new Error(`DEAD_AIR: ${verdict.reason}; the engine exited 0 but the audio is not usable. Not retried automatically.`);
  return verdict;
}

// finalizeAudio: deliver the engine's wav at `out`, or throw. Returns what was done:
// {did: "mastered" | "converted" | "copied", trimmedSec, verdict}.
//   music  trim trailing silence -> fade-out + loudnorm -14 LUFS / -1 dBTP + 48 kHz -> dead-air gate
//   voice  the engine's wav as it is (re-encoded for a non-wav extension) -> dead-air gate
// On a failed gate the delivered file is removed (a QA gate on our own broken output).
export function finalizeAudio({ ffmpeg, ffprobe, wav, out, kind, workDir, run = defaultRun, measureFn = measure, durationFn = durationSec, log = (m) => console.error(m) }) {
  if (!ffmpeg || !ffprobe) {
    throw new Error("FFMPEG_UNAVAILABLE: ffmpeg/ffprobe could not be resolved (set ffmpeg_path, or put both on PATH): the audio's trim / loudness / dead-air checks cannot run");
  }
  const wantWav = extname(out).toLowerCase() === ".wav";
  let did;
  let trimmedSec = 0;
  let verdict;
  let touched = false; // `out` is only ours to remove once a step has started writing it
  try {
    if (kind === "music") {
      const trimmed = join(workDir, "trimmed.wav");
      runStep(run, ffmpeg, buildTrimTailArgs({ src: wav, dst: trimmed }), trimmed, "ffmpeg trailing-silence trim");
      const before = durationFn(ffprobe, wav);
      const after = durationFn(ffprobe, trimmed);
      if (!(after > 0)) throw new Error("DEAD_AIR: the trailing-silence trim left no audio: the whole render is silence");
      trimmedSec = Math.max(0, before - after);
      if (trimmedSec > 0.05) log(`audiocpp-generate: trimmed ${trimmedSec.toFixed(2)}s of trailing silence`);
      touched = true;
      runStep(run, ffmpeg, buildMasterArgs({ src: trimmed, dst: out, duration: after }), out, "ffmpeg fade/loudness master");
      did = "mastered";
    } else if (wantWav) {
      touched = true;
      copyFileSync(wav, out);
      did = "copied";
    } else {
      touched = true;
      runStep(run, ffmpeg, buildConvertArgs({ src: wav, dst: out }), out, "ffmpeg re-encode");
      did = "converted";
    }
    verdict = gateDeadAir({ ffmpeg, ffprobe, file: out, measureFn });
  } catch (e) {
    // a QA gate on our own broken output, or a failed step that left a partial file: nothing is delivered
    if (touched) {
      try { rmSync(out, { force: true }); } catch { /* best effort */ }
    }
    throw e;
  }
  return { did, trimmedSec, verdict };
}

async function main() {
  installLifecycle();
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const deadline = makeDeadline(flags["timeout-sec"]);
  const [out, text] = pos;
  const kind = flags.kind;
  if (!out || !text || (kind !== "voice" && kind !== "music")) {
    console.error('usage: node audiocpp-generate.mjs [flags] -- <out.wav> "<text>" --kind voice|music --bin ABS --family F --model P --backend vulkan [--device 0]');
    process.exit(2);
  }
  const need = ["bin", "family", "model", "backend"].filter((n) => !flags[n]);
  if (need.length) {
    console.error("AUDIOCPP FAILED: missing " + need.map((m) => "--" + m).join(", "));
    process.exit(2);
  }
  flags.backend = refuseAudioBackend(flags.backend);
  flags.device = refuseAudioDevice(flags.device);
  const extra = refuseExtraArgs(parseExtraArgs(flags["extra-args"]), { engine: "audiocpp", key: "--extra-args" });
  const bin = refuseRelativeBinary("--bin", flags.bin);
  for (const [k, v] of [["--model", flags.model], ["--clone", flags.clone]]) {
    if (v && !existsSync(v)) {
      console.error(`AUDIOCPP FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  ensureOutDir(out);
  // audio nobody can measure is not a usable result: refuse before the lease and the GPU
  const ffmpeg = resolveFfmpeg();
  const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
  if (!ffmpeg || !ffprobe) {
    throw new Error("FFMPEG_UNAVAILABLE: ffmpeg/ffprobe could not be resolved (set ffmpeg_path, or put both on PATH): the audio's trim / loudness / dead-air checks cannot run");
  }
  const tmp = makeTempDir("audiocpp-");
  try {
    const wav = join(tmp.dir, "out.wav");
    const args = buildAudiocppArgs({ kind, outFile: wav, text, flags, extra });
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      deadline.enforce("audiocpp_cli");
      const guard = createLogGuard({ engine: "audiocpp", echoes: [text, flags.lyrics] });
      const { code, log } = await runEngine({ bin, args, timeoutMs: deadline.remainingMs(), label: "audiocpp_cli", guard });
      if (code !== 0) throw engineExitError("audiocpp_cli", code, log, "");
      if (!existsSync(wav)) throw new Error("audiocpp_cli exited 0 but produced no audio at " + wav);
    });
    deadline.enforce("audio finalize");
    finalizeAudio({ ffmpeg, ffprobe, wav, out, kind, workDir: tmp.dir });
    console.log("WROTE", out);
  } finally {
    tmp.cleanup();
  }
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    reportFatal("AUDIOCPP", e);
    process.exit(1);
  });
}
