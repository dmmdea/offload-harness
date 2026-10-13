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
//        [--temperature F --cfg-weight F --exaggeration F --repetition-penalty F --min-p F --top-p F] [--no-lock]
//
// The command line is checked before anything starts (parseCli): an unknown flag, a flag with no value,
// a sampling value that is not a finite number, an unknown --engine, --model/--base-dir without
// --engine finetuned, or a third positional (an unquoted text) exits 2 with a usage line. The six sampling
// flags go to BOTH workers under the same names; a flag that is not given is not forwarded, so the model
// keeps its own default. A text that opens with dashes but is not shaped like a flag name ("--- Intro ---")
// is the text; a bare "--" ends the flags (tts.mjs out.wav -- "--hola" gives the text --hola).
import { join, dirname } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { existsSync } from "node:fs";
import { spawn } from "node:child_process";
import { withGpuSlot } from "./gpu-lock.mjs";

const __dir = dirname(fileURLToPath(import.meta.url));

// The two python workers this launcher dispatches to (siblings in render/).
export const GENERALIST_WORKER = "tts_chatterbox.py";
export const FT_WORKER = "tts_chatterbox_ft.py";

// The sampling flags both workers take under these same names (the worker binds them as generate()
// kwargs, never positionally).
export const SAMPLING_FLAGS = ["temperature", "cfg-weight", "exaggeration", "repetition-penalty", "min-p", "top-p"];

// Every flag this CLI reads. VALUE_FLAGS take the next token as their value, BOOL_FLAGS take none; any other
// --name is refused (checkFlags) instead of being dropped. internal/pipeline/pipeline.go builds this command
// line for the voice lane: every flag it sends must be listed here (tts.test.mjs reads pipeline.go to check).
export const VALUE_FLAGS = ["engine", "clone", "lang", "model", "base-dir", ...SAMPLING_FLAGS];
export const BOOL_FLAGS = ["no-lock"];
export const KNOWN_FLAGS = [...VALUE_FLAGS, ...BOOL_FLAGS];
export const ENGINES = ["generalist", "finetuned"];

// A token is a flag when it is shaped like one: "--", a name of letters, digits and underscores (not opening
// with a digit) joined by single dashes, and an optional "=value" (which checkFlags refuses with the spelling to use). A text that opens
// with dashes ("--- Intro ---", the CT-52 case the Go verbs keep intact) is therefore a positional, not a flag.
const FLAG_SHAPE = /^--[A-Za-z_][A-Za-z0-9_]*(?:-[A-Za-z0-9_]+)*(?:=[\s\S]*)?$/;

// parseArgs splits argv into positionals + flags. --no-lock is boolean; all other --flags
// take the next token as their value. A bare "--" ends the flags: every later token is a positional. The
// flags object has no prototype, so a flag named like an Object.prototype member (--__proto__) is a flag
// like any other and checkFlags sees it.
export function parseArgs(argv) {
  const pos = [];
  const flags = Object.create(null);
  let ended = false;
  for (let i = 0; i < argv.length; i++) {
    if (!ended && argv[i] === "--") ended = true;
    else if (!ended && FLAG_SHAPE.test(argv[i])) {
      const k = argv[i].slice(2);
      if (BOOL_FLAGS.includes(k)) flags[k] = true;
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
// --model/--base-dir. The sampling flags (SAMPLING_FLAGS) go to EITHER engine under the same
// names, verbatim, and only the ones given: the worker binds them as generate() kwargs — never
// positionally — and an unset one keeps the model's own default.
export function buildWorkerArgs(out, text, flags, env = process.env) {
  const args = ["--out", out, "--text", text, "--lang", flags.lang || "es"];
  const ref = flags.clone || env.TTS_REF;
  if (ref) args.push("--ref", ref);
  if (flags.engine === "finetuned") {
    if (flags.model) args.push("--model", flags.model);
    if (flags["base-dir"]) args.push("--base-dir", flags["base-dir"]);
  }
  for (const k of SAMPLING_FLAGS) {
    if (flags[k] != null) args.push("--" + k, flags[k]);
  }
  return args;
}

export const USAGE = 'usage: node tts.mjs <out.wav> "<text>" [--engine generalist|finetuned] [--clone <ref.wav>] [--lang es] '
  + "[--model <f> --base-dir <d> (finetuned only)] [--temperature F] [--cfg-weight F] [--exaggeration F] "
  + "[--repetition-penalty F] [--min-p F] [--top-p F] [--no-lock]";

// A decimal number the python workers read too: float() takes it (no hex, no whitespace, no nan/inf spelling) and, for a
// negative one, argparse takes it as a VALUE. Measured on Python 3.11 (the README's venv): a dash-led token is a value
// only when it is -N or -N.N / -.N; "-1e-2" and "-5." are read as flags and the worker exits 2 after the lock was taken
// (3.14 reads them as values; the rule here is the narrower one, so it holds on both).
const DECIMAL = /^(?:\+?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?|-(?:\d+|\d*\.\d+))$/;

// checkFlags judges the parsed flags and returns the reason to refuse them, or null to accept. Nothing
// the operator typed is dropped silently: an unknown flag, a value flag with no value (last on the line,
// empty, or followed by another flag), an unknown engine, a sampling value that is not a finite number and
// a worker-specific flag the chosen engine would ignore are all refused, naming the flag.
export function checkFlags(flags) {
  for (const k of Object.keys(flags)) {
    if (KNOWN_FLAGS.includes(k)) continue;
    // --name=value: say how to write it, for a flag that exists
    const eq = k.indexOf("=");
    const name = eq > 0 ? k.slice(0, eq) : "";
    let hint = "";
    if (BOOL_FLAGS.includes(name)) hint = ` (--${name} takes no value)`;
    else if (KNOWN_FLAGS.includes(name)) hint = ` (a flag takes its value as the next argument: --${name} ${k.slice(eq + 1)})`;
    return `unknown flag --${k}${hint}`;
  }
  for (const k of VALUE_FLAGS) {
    if (!(k in flags)) continue;
    const v = flags[k];
    if (typeof v !== "string" || v === "" || v.startsWith("--")) return `--${k} needs a value`;
  }
  if ("engine" in flags && !ENGINES.includes(flags.engine)) {
    return `--engine must be ${ENGINES.join(" or ")}, not ${JSON.stringify(flags.engine)}`;
  }
  for (const k of SAMPLING_FLAGS) {
    if (!(k in flags)) continue;
    if (!DECIMAL.test(flags[k]) || !Number.isFinite(Number(flags[k]))) {
      return `--${k} must be a finite number, not ${JSON.stringify(flags[k])}`;
    }
  }
  if (flags.engine !== "finetuned") {
    for (const k of ["model", "base-dir"]) {
      if (k in flags) return `--${k} applies to --engine finetuned only (the ${flags.engine || "default"} engine would ignore it)`;
    }
  }
  return null;
}

// parseCli turns argv into a run plan, or says why the command line is refused. It never starts a
// worker, takes a lock or reads the environment beyond TTS_REF, so it is the part tests exercise.
export function parseCli(argv, env = process.env) {
  const { pos, flags } = parseArgs(argv);
  const bad = checkFlags(flags);
  if (bad) return { ok: false, error: bad };
  const out = pos[0];
  const text = pos[1];
  if (!out || !text) return { ok: false, error: 'missing <out.wav> or "<text>"' };
  if (pos.length > 2) {
    return { ok: false, error: `unexpected extra argument ${JSON.stringify(pos[2])} (quote the whole text: tts.mjs <out.wav> "<text>")` };
  }
  return { ok: true, out, text, flags, worker: selectWorker(flags), args: buildWorkerArgs(out, text, flags, env) };
}

async function main() {
  const plan = parseCli(process.argv.slice(2));
  if (!plan.ok) {
    console.error("tts.mjs: " + plan.error);
    console.error(USAGE);
    process.exit(2);
  }
  const { out, flags } = plan;
  // python auto-detect, mirroring comfy-video.mjs's COMFY_PY (the repo-root .tts-venv; see render/README.md Setup)
  const TTS_PY = process.env.TTS_PY
    || [join(__dir, "../.tts-venv/Scripts/python.exe"), join(__dir, "../.tts-venv/bin/python")].find((p) => existsSync(p))
    || "python";
  const worker = join(__dir, plan.worker);
  if (!existsSync(worker)) {
    console.error("TTS FAILED: missing worker " + worker);
    process.exit(1);
  }
  const args = [worker, ...plan.args];
  await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
    const code = await new Promise((res) => spawn(TTS_PY, args, { stdio: "inherit", windowsHide: true }).on("close", res));
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
