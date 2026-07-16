// tts.mjs — local text-to-speech narration. Owns the GPU lock + lifecycle and dispatches
// to a python worker by --engine: generalist (stock Chatterbox multilingual,
// tts_chatterbox.py) or finetuned (vendored fine-tuned engine, tts_chatterbox_ft.py).
// GPU-locked (Chatterbox shares the GPU with llama-swap). Chatterbox is NOT ComfyUI:
// comfyManaged:false frees llama-swap but never starts/stops ComfyUI. The python worker
// exits on its own; the Go wrapper process-tree-kills it on timeout (the script-level
// SIGKILL backstop). The Perth watermark on outputs is provenance-only (MIT allows
// commercial use).
//
// Usage: node render/tts.mjs <out.wav> "<text>" [--engine generalist|finetuned]
//        [--clone <ref.wav>] [--lang es] [--model <merged.safetensors>] [--base-dir <dir>]
//        [--temperature F --cfg-weight F --exaggeration F --repetition-penalty F] [--no-lock]
import { join, dirname } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { existsSync } from "node:fs";
import { spawn } from "node:child_process";
import { withGpuSlot } from "./gpu-lock.mjs";

const __dir = dirname(fileURLToPath(import.meta.url));

// The two python workers this launcher dispatches to (siblings in render/).
export const GENERALIST_WORKER = "tts_chatterbox.py";
export const FT_WORKER = "tts_chatterbox_ft.py";

// parseArgs splits argv into positionals + flags. --no-lock is boolean; all other --flags
// take the next token as their value.
export function parseArgs(argv) {
  const pos = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) {
      const k = argv[i].slice(2);
      if (k === "no-lock") flags[k] = true;
      else {
        flags[k] = argv[i + 1];
        i++;
      }
    } else pos.push(argv[i]);
  }
  return { pos, flags };
}

// selectWorker picks the python worker for the requested engine (default generalist).
export function selectWorker(flags) {
  return flags.engine === "finetuned" ? FT_WORKER : GENERALIST_WORKER;
}

// buildWorkerArgs builds the python worker argv. The reference clip comes from --clone
// (or the TTS_REF env — set it to your canonical clean voice reference so narration is
// a one-liner); lang defaults to es. For the fine-tuned engine it also threads
// --model/--base-dir and any recipe knobs (passed through verbatim; the worker binds
// them as kwargs — never positionally).
export function buildWorkerArgs(out, text, flags, env = process.env) {
  const args = ["--out", out, "--text", text, "--lang", flags.lang || "es"];
  const ref = flags.clone || env.TTS_REF;
  if (ref) args.push("--ref", ref);
  if (flags.engine === "finetuned") {
    if (flags.model) args.push("--model", flags.model);
    if (flags["base-dir"]) args.push("--base-dir", flags["base-dir"]);
    for (const k of ["temperature", "cfg-weight", "exaggeration", "repetition-penalty", "min-p", "top-p"]) {
      if (flags[k] != null) args.push("--" + k, flags[k]);
    }
  }
  return args;
}

async function main() {
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const out = pos[0];
  const text = pos[1];
  if (!out || !text) {
    console.error('usage: node tts.mjs <out.wav> "<text>" [--engine generalist|finetuned] [--clone <ref.wav>] [--lang es] [--model <f>] [--base-dir <d>] [recipe flags] [--no-lock]');
    process.exit(2);
  }
  // python auto-detect, mirroring comfy-video.mjs's COMFY_PY (the .tts-venv built on ComfyUI's 3.12)
  const TTS_PY = process.env.TTS_PY
    || [join(__dir, "../.tts-venv/Scripts/python.exe"), join(__dir, "../.tts-venv/bin/python")].find((p) => existsSync(p))
    || "python";
  const worker = join(__dir, selectWorker(flags));
  if (!existsSync(worker)) {
    console.error("TTS FAILED: missing worker " + worker);
    process.exit(1);
  }
  const args = [worker, ...buildWorkerArgs(out, text, flags)];
  await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
    const code = await new Promise((res) => spawn(TTS_PY, args, { stdio: "inherit" }).on("close", res));
    if (code !== 0) throw new Error("tts worker exited " + code);
    console.log("WROTE", out);
  });
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    console.error("TTS FAILED:", e.message);
    process.exit(1);
  });
}
