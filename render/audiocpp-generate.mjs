// audiocpp-generate.mjs — voice and music on the iGPU tier via audio.cpp `audiocpp_cli`
// (CT-49; v0.9.0, Apache-2.0, official Vulkan build). A spawn-per-job native binary under
// the shared GPU lease: zero-warm, nothing resident, no Python, no ComfyUI.
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
// Usage: node render/audiocpp-generate.mjs --kind voice|music <out.wav> "<text>"
//        --bin P --family F --model P --backend vulkan --device 0
//        [--clone <ref.wav>] [--lang es] [--seconds N] [--lyrics S] [--seed N]
//        [--extra-args '<json array>'] [--timeout-sec N] [--no-lock]
// Env:   FFMPEG_PATH — ffmpeg (else ffmpeg on PATH); music is loudness-normalized with it.
//
// Music is normalized like render/comfy-music.mjs does (-14 LUFS / -1 dBTP, via
// audio-qa.mjs normalizeLoudness) when ffmpeg is present; a missing ffmpeg or a failed
// normalization keeps the engine's own wav (never withhold produced audio). Voice is
// delivered as the engine wrote it.
//
// THE NO-CPU RULE: a cpu (or unset) --backend is refused before anything spawns
// (CPU_BACKEND_REFUSED), and the engine's log (--log) is scanned while it runs: the first
// line placing the model on the CPU kills the process and fails the job with CPU_PLACEMENT.
import { existsSync, copyFileSync } from "node:fs";
import { join, extname } from "node:path";
import { spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg, normalizeLoudness } from "./audio-qa.mjs";
import { parseArgs, parseExtraArgs, refuseCpuBackend, runEngine, finiteNum, makeTempDir } from "./igpu-engine.mjs";

export { parseArgs };

export const DEFAULT_LANG = "es";

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

// finalizeAudio: deliver the engine's wav at `out`. Music is loudness-normalized with
// audio-qa.mjs (-14 LUFS / -1 dBTP) when ffmpeg is present; loudnorm upsamples to 192 kHz, so
// the result is always resampled to 48 kHz on its way to `out` (a 192 kHz file is not
// encodable as flac and is no one's delivery format). A non-wav `out` extension is honoured
// by an ffmpeg re-encode, never by renaming wav bytes. A missing ffmpeg or a failed step keeps
// the engine's own wav (never withhold produced audio): the bytes are copied to `out`.
// Returns what was done: "normalized" | "converted" | "copied".
export function finalizeAudio({ ffmpeg, wav, out, normalize, workDir }) {
  const wantWav = extname(out).toLowerCase() === ".wav";
  if (ffmpeg && (normalize || !wantWav)) {
    let src = wav;
    let did = "converted";
    if (normalize) {
      const norm = join(workDir, "norm.wav");
      if (normalizeLoudness(ffmpeg, wav, norm)) {
        src = norm;
        did = "normalized";
      }
    }
    const r = spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-i", src, "-ar", "48000", out], { encoding: "utf8" });
    if (!r.error && r.status === 0 && existsSync(out)) return did;
  } else if (!ffmpeg && normalize) {
    console.error("audiocpp-generate: ffmpeg not found, music left un-normalized");
  }
  copyFileSync(wav, out);
  return "copied";
}

async function main() {
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const [out, text] = pos;
  const kind = flags.kind;
  if (!out || !text || (kind !== "voice" && kind !== "music")) {
    console.error('usage: node audiocpp-generate.mjs --kind voice|music <out.wav> "<text>" --bin P --family F --model P --backend vulkan [--device 0] [flags]');
    process.exit(2);
  }
  const need = ["bin", "family", "model", "backend"].filter((n) => !flags[n]);
  if (need.length) {
    console.error("AUDIOCPP FAILED: missing " + need.map((m) => "--" + m).join(", "));
    process.exit(2);
  }
  refuseCpuBackend(flags.backend);
  for (const [k, v] of [["--bin", flags.bin], ["--model", flags.model], ["--clone", flags.clone]]) {
    if (v && !existsSync(v)) {
      console.error(`AUDIOCPP FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const extra = parseExtraArgs(flags["extra-args"]);
  const timeoutMs = Math.round((finiteNum(flags["timeout-sec"]) ?? 0) * 1000);
  const tmp = makeTempDir("audiocpp-");
  try {
    const wav = join(tmp.dir, "out.wav");
    const args = buildAudiocppArgs({ kind, outFile: wav, text, flags, extra });
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      const { code } = await runEngine({ bin: flags.bin, args, timeoutMs, label: "audiocpp_cli" });
      if (code !== 0) throw new Error("audiocpp_cli exited " + code);
      if (!existsSync(wav)) throw new Error("audiocpp_cli exited 0 but produced no audio at " + wav);
    });
    finalizeAudio({ ffmpeg: resolveFfmpeg(), wav, out, normalize: kind === "music", workDir: tmp.dir });
    console.log("WROTE", out);
  } finally {
    tmp.cleanup();
  }
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    console.error("AUDIOCPP FAILED:", e.message);
    process.exit(1);
  });
}
