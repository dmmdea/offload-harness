// tts.mjs — local text-to-speech narration via Chatterbox Multilingual (MIT, commercial-safe,
// zero-shot voice cloning). Dependency-free Node CLI; spawns a python worker in the .tts-venv.
// Cloning a reference clip (e.g. a reference speaker's voice) reproduces that voice+accent.
// GPU-locked (Chatterbox shares the 8GB with llama-swap). Mirrors comfy-video.mjs's python
// auto-detect + lock pattern. The Perth watermark on outputs is provenance-only (MIT allows
// commercial use).
//
// Usage: node render/tts.mjs <out.wav> "<text>" [--clone <ref.wav>] [--lang es] [--no-lock]
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { existsSync } from "node:fs";
import { spawn } from "node:child_process";
import { withGpuSlot } from "./gpu-lock.mjs";

const __dir = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (k === "no-lock") flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const out = pos[0], text = pos[1];
if (!out || !text) { console.error('usage: node tts.mjs <out.wav> "<text>" [--clone <ref.wav>] [--lang es] [--no-lock]'); process.exit(2); }

// python auto-detect, mirroring comfy-video.mjs's COMFY_PY (the .tts-venv built on ComfyUI's 3.12)
const TTS_PY = process.env.TTS_PY
  || [join(__dir, "../.tts-venv/Scripts/python.exe"), join(__dir, "../.tts-venv/bin/python")].find((p) => existsSync(p))
  || "python";
const worker = join(__dir, "tts_chatterbox.py");

async function synth() {
  // --clone wins; else fall back to the TTS_REF env (set it to your canonical
  // clean voice reference, e.g. ~/.local-offload/refs/voice-ref.wav, so narration is
  // a one-liner). No ref → Chatterbox's default built-in voice.
  const ref = flags.clone || process.env.TTS_REF;
  const args = [worker, "--out", out, "--text", text, "--lang", flags.lang || "es"];
  if (ref) args.push("--ref", ref);
  const code = await new Promise((res) => spawn(TTS_PY, args, { stdio: "inherit" }).on("close", res));
  if (code !== 0) throw new Error("tts worker exited " + code);
  console.log("WROTE", out);
}

if (!existsSync(worker)) { console.error("TTS FAILED: missing worker " + worker); process.exit(1); }
// Chatterbox is NOT ComfyUI: comfyManaged:false frees llama-swap but never starts/stops
// ComfyUI. The python worker exits on its own; the Go wrapper process-tree-kills it on
// timeout (the script-level SIGKILL backstop). GPU-locked (shares the 8GB with llama-swap).
withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, synth)
  .catch((e) => { console.error("TTS FAILED:", e.message); process.exit(1); });
