// igpu-engine.mjs — shared plumbing for the iGPU media runners (sdcpp-video.mjs,
// sdcpp-animate.mjs, audiocpp-generate.mjs): the spawn-per-job native CLIs that serve
// video, animate, voice and music on a box whose only GPU is a Vulkan iGPU.
//
// THE RULE THIS FILE ENFORCES: no model runs on CPU on these engines. The guards:
//   1. refuseCpuBackend / refuseExtraArgs — a backend that is not a Vulkan device, and any
//      *_extra_args element that changes the backend or placement, is refused before
//      anything spawns (mirrors config.CPUBackendRefusal / config.ExtraArgsRefusal on the Go
//      side; the Go side refuses first, this is the runner's own door for a hand-run or a
//      stale config).
//   2. createLogGuard + runEngine — a POSITIVE guard. The engine's own log is read line by
//      line while it runs and a run PASSES only on affirmative evidence that the work ran on
//      the GPU (per engine, see createLogGuard); a line that places compute on the CPU kills
//      the process tree at once, and a run that ends without the positive evidence is
//      CPU_PLACEMENT as well ("no GPU evidence was seen"). Silence is never a pass.
//   3. token cap (latentTokens / checkTokenCap) — keeps one GPU dispatch inside the amdgpu
//      2 s lockup timeout; a device reset in the log is GPU_RESET, never retried.
//   4. lifecycle (installLifecycle) — SIGTERM/SIGINT/SIGHUP and the parent disappearing kill
//      the engine tree and remove the temp dirs, so a cancelled job never leaves the iGPU
//      held or frames behind.
//
// Dependency-free (Node 18+ built-ins only).
import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, renameSync, writeFileSync, accessSync, constants as fsConstants } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, extname, isAbsolute, join, resolve } from "node:path";

export const CPU_PLACEMENT = "CPU_PLACEMENT";
export const CPU_BACKEND_REFUSED = "CPU_BACKEND_REFUSED";
export const GPU_RESET = "GPU_RESET";
export const TOKEN_CAP_EXCEEDED = "TOKEN_CAP_EXCEEDED";
export const EXTRA_ARGS_REFUSED = "EXTRA_ARGS_REFUSED";
export const ILLEGAL_INSTRUCTION = "ILLEGAL_INSTRUCTION";
export const BINARY_NOT_ABSOLUTE = "BINARY_NOT_ABSOLUTE";
export const OUT_DIR_UNWRITABLE = "OUT_DIR_UNWRITABLE";
export const MODEL_INCOMPATIBLE = "MODEL_INCOMPATIBLE";
export const ENGINE_CRASHED = "ENGINE_CRASHED";
export const OUT_OF_MEMORY = "OUT_OF_MEMORY";
export const DEVICE_INVALID = "DEVICE_INVALID";

// ---------------------------------------------------------------- the error class line

// Every typed failure a runner ends with is also reported as ONE short final machine line,
// "IGPU_CLASS=<class>", after the (long) human line. The Go side (internal/gpugen) reads the class
// from that line anywhere in the output it captured; the human line is not the carrier, because
// gpugen keeps only its tail in the error text and a real GPU_RESET line is longer than that tail.
// The classes are gpugen.ClassifyErr's names (internal/gpugen pins both lists together).
export const CLASS_MARKER = "IGPU_CLASS=";
const TOKEN_CLASSES = {
  CPU_PLACEMENT: "cpu_placement", CPU_BACKEND_REFUSED: "cpu_backend_refused", GPU_RESET: "gpu_reset",
  TOKEN_CAP_EXCEEDED: "token_cap_exceeded", EXTRA_ARGS_REFUSED: "extra_args_refused",
  ILLEGAL_INSTRUCTION: "illegal_instruction", BLACK_CLIP: "black_clip", FROZEN_CLIP: "frozen_clip",
  DEPTH_FRAMES_INVALID: "depth_frames_invalid", MODEL_INCOMPATIBLE: "model_incompatible",
  BINARY_NOT_ABSOLUTE: "binary_not_absolute", OUT_DIR_UNWRITABLE: "out_dir_unwritable",
  DEAD_AIR: "dead_air", FFMPEG_UNAVAILABLE: "ffmpeg_unavailable", UNMEASURABLE: "unmeasurable",
  ENGINE_CRASHED: "engine_crashed", OUT_OF_MEMORY: "oom", DEVICE_INVALID: "device_invalid",
};

// errorClass: the class a failure message belongs to ("" = untyped): its leading TOKEN_NAME:, else
// "timeout" when it reports one. Pure.
export function errorClass(message) {
  const text = String(message ?? "");
  const tok = /^\s*([A-Z][A-Z0-9_]+):/.exec(text);
  if (tok && TOKEN_CLASSES[tok[1]]) return TOKEN_CLASSES[tok[1]];
  if (/\btimeout\b/i.test(text)) return "timeout";
  return "";
}

// reportFatal: print a runner's failure the way every iGPU runner ends: the human line, then the class
// line when the failure is typed. `then`, when given, runs once the LAST line has been written out.
export function reportFatal(label, e, then) {
  const msg = e && e.message ? e.message : String(e);
  const cls = errorClass(msg);
  const human = `${label} FAILED: ${msg}\n`;
  if (cls) {
    process.stderr.write(human);
    process.stderr.write(CLASS_MARKER + cls + "\n", then);
  } else {
    process.stderr.write(human, then);
  }
}

// failAndExit: report a runner's failure and exit with `code` once it has been written out.
// process.exit() right after a write drops output that a full pipe could not take at once (it is
// queued), and the class line at the end of a failure is the one part gpugen reads its class from.
// Writes complete in order, so the callback of the last write means all of it is out; the timer is the
// bound for a pipe nobody reads.
export function failAndExit(label, e, code = 1) {
  setTimeout(() => process.exit(code), 3000).unref();
  reportFatal(label, e, () => process.exit(code));
}

// parseArgs: positionals + --flags. `booleans` names the flags that take no value.
// Every other --flag consumes the next token (a missing value is undefined, which every
// consumer treats as unset). A bare `--` ends flag parsing: everything after it is
// positional, so a prompt, TTS text or lyrics that starts with "--" (a lyrics section
// marker such as "--- Intro ---") stays a positional. The harness emits its flags first
// and `-- <out> [<still>] <text>` last for exactly that reason.
export function parseArgs(argv, booleans = ["no-lock"]) {
  const pos = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--") {
      for (let j = i + 1; j < argv.length; j++) pos.push(argv[j]);
      break;
    }
    if (typeof a === "string" && a.startsWith("--") && a.length > 2) {
      const k = a.slice(2);
      if (booleans.includes(k)) flags[k] = true;
      else {
        flags[k] = argv[i + 1];
        i++;
      }
    } else pos.push(a);
  }
  return { pos, flags };
}

// parseExtraArgs: a JSON array of strings (the Go side marshals config's *_extra_args
// that way, so a token with spaces survives). Empty/undefined = [].
export function parseExtraArgs(raw) {
  if (raw === undefined || raw === null || String(raw).trim() === "") return [];
  let v;
  try {
    v = JSON.parse(String(raw));
  } catch (e) {
    throw new Error("--extra-args must be a JSON array of strings: " + e.message);
  }
  if (!Array.isArray(v) || v.some((x) => typeof x !== "string")) {
    throw new Error("--extra-args must be a JSON array of strings");
  }
  return v;
}

// ---------------------------------------------------------------- extra-args screening

// Flags that change the backend or the placement of a model. The runner supplies the
// backend itself, so an extra-args element naming any of these is never legitimate: whatever
// it is followed by overrides the GPU backend the script put in the argv.
const PLACEMENT_FLAGS = new Set([
  "--backend", "-b", "--params-backend", "--clip-on-cpu", "--vae-on-cpu",
  "--control-net-cpu", "--rpc",
]);

// cpu-named flags that are NOT placements. sd.cpp's --offload-to-cpu parks the weights in RAM
// and stages them to the device step by step; every compute buffer stays on the GPU, and the
// log guard still kills a run that shows one on the CPU. The house rule (2026-10-07) allows
// RAM as overflow while it adds capability, so the screen lets the flag through; whether a
// box should use it is a per-seat measurement, not a screen (on a UMA iGPU box "VRAM" is the
// same memory, so it only adds copies and the bindings leave it off). Mirrors
// config.sanctionedSpillFlags.
const SANCTIONED_SPILL_FLAGS = new Set(["--offload-to-cpu"]);

// screenExtraArgs: the first element of an *_extra_args list that changes the backend or
// the placement, as {index, arg, why}; null when the list is clean. `engine` is "sdcpp",
// "da3" or "audiocpp" (audio.cpp's --device is the runner's too). Pure. Mirrors
// config.ExtraArgsRefusal.
export function screenExtraArgs(args, { engine = "sdcpp" } = {}) {
  const list = Array.isArray(args) ? args : [];
  for (let i = 0; i < list.length; i++) {
    const arg = String(list[i]);
    const low = arg.toLowerCase().trim();
    const name = low.split("=")[0].trim();
    if (!SANCTIONED_SPILL_FLAGS.has(name) && PLACEMENT_FLAGS.has(name)) return { index: i, arg, why: `${name} sets the backend or where a model lives` };
    if (engine === "audiocpp" && name === "--device") return { index: i, arg, why: "--device is chosen by the audiocpp_device key" };
    if (!SANCTIONED_SPILL_FLAGS.has(name) && name.startsWith("-") && /cpu/.test(name)) return { index: i, arg, why: "a cpu-named flag places a model on the CPU" };
    if (low.split(/[=,&:\s]+/).some((p) => /^cpu\d*$/.test(p))) return { index: i, arg, why: "cpu as a backend value" };
  }
  return null;
}

// refuseExtraArgs: throws EXTRA_ARGS_REFUSED naming the offending element; returns the list.
export function refuseExtraArgs(args, { engine = "sdcpp", key = "extra args" } = {}) {
  const hit = screenExtraArgs(args, { engine });
  if (hit) {
    throw new Error(`${EXTRA_ARGS_REFUSED}: ${key}[${hit.index}] ${JSON.stringify(hit.arg)}: ${hit.why} (no model runs on CPU on this engine; backends and devices are set by the *_backend / *_device keys only)`);
  }
  return Array.isArray(args) ? args : [];
}

// ---------------------------------------------------------------- backend refusal

// A backend assignment value is accepted only when it names a Vulkan device ("vulkan",
// "vulkan0", ...): an allowlist, so "cpu", "best", "auto", "blas", "opencl", "rpc" and a typo
// such as "vulcan" are all refused rather than left to the binary to resolve.
const VULKAN_BACKEND = /^vulkan\d*$/;

// refuseCpuBackend: throws CPU_BACKEND_REFUSED for a backend that is unset or anything but a
// Vulkan device. sd-cli's --backend takes one value ("vulkan0") or per-module assignments
// ("diffusion=vulkan0,vae=cpu"; "&" joins the devices of one module), so every assignment is
// checked. Returns the trimmed backend when it is acceptable.
export function refuseCpuBackend(backend) {
  const b = String(backend ?? "").trim();
  if (b === "") {
    throw new Error(CPU_BACKEND_REFUSED + ": --backend is unset (name a GPU backend such as vulkan0; no model runs on CPU on this engine)");
  }
  for (const part of b.toLowerCase().split(/[,&]/)) {
    let v = part.trim();
    const eq = v.lastIndexOf("=");
    if (eq >= 0) v = v.slice(eq + 1).trim();
    if (!VULKAN_BACKEND.test(v)) {
      throw new Error(CPU_BACKEND_REFUSED + `: --backend ${JSON.stringify(b)} is not a Vulkan device (${JSON.stringify(v)}); no model runs on CPU on this engine, so only vulkan / vulkanN is accepted`);
    }
  }
  return b;
}

// ---------------------------------------------------------------- binaries and the out dir

// refuseRelativeBinary: ONE resolution rule for every engine binary. The Go side resolves the
// bound value with mediaops.ResolveBinary (an explicit path is stat'd, a bare name is looked up
// on PATH) and passes the ABSOLUTE path; a runner refuses anything else instead of re-deriving
// its own answer (a bare "sd-cli" used to read CONFIGURED in doctor and then fail every call in
// existsSync). Returns the path when it is absolute and exists.
export function refuseRelativeBinary(flag, path) {
  const p = String(path ?? "");
  if (!isAbsolute(p)) {
    throw new Error(`${BINARY_NOT_ABSOLUTE}: ${flag} ${JSON.stringify(p)} is not an absolute path (the harness resolves a bare name on PATH and passes the absolute path; a hand run must do the same)`);
  }
  if (!existsSync(p)) throw new Error(`${flag} not found: ${p}`);
  return p;
}

// ensureOutDir: create (mkdir -p) the directory the result will land in, before anything
// spawns, so a minutes-long render never ends in "cannot write the output". Throws
// OUT_DIR_UNWRITABLE naming the directory when it cannot be created or written.
export function ensureOutDir(out) {
  const dir = dirname(resolve(String(out)));
  try {
    mkdirSync(dir, { recursive: true });
    accessSync(dir, fsConstants.W_OK);
  } catch (e) {
    throw new Error(`${OUT_DIR_UNWRITABLE}: cannot use ${dir} for the output: ${e.message}`);
  }
  return dir;
}

// modelMetadataError: sd-cli refuses a model whose tensors do not match what its architecture
// needs ("Diffusion model tensor '...' not in model metadata" / "model metadata validation
// failed": the public Wan2.1 VACE GGUFs lack vace_patch_embedding.weight). The guard cannot
// know the file, so the runner calls this on a non-zero exit with the engine log. Returns a
// MODEL_INCOMPATIBLE error naming the model file, or null.
export function modelMetadataError(log, modelFile) {
  const m = /not in model metadata|model metadata validation failed/i.exec(String(log ?? ""));
  if (!m) return null;
  const tensor = /tensor '([^']+)'/.exec(String(log))?.[1];
  return new Error(`${MODEL_INCOMPATIBLE}: sd-cli refused the model ${modelFile}: ${tensor ? `tensor '${tensor}' is not in its metadata` : "model metadata validation failed"}. This file is not a complete checkpoint for this architecture (the public Wan2.1 VACE GGUFs lack vace_patch_embedding.weight; use the .safetensors VACE model). Not retried.`);
}

// ---------------------------------------------------------------- the positive GPU-evidence guard

// What counts as evidence, per engine (formats read from the engines' own source at the
// pinned commits and confirmed against real logs in render/testdata, see its README):
//
//   sdcpp    (stable-diffusion.cpp 3f8527a "master-929", a1ded76 "master-945")  PASS needs ALL of
//              - "ggml_vulkan: <n> = <device> (...)" for a NON-software device,
//              - the diffusion stage on a Vulkan device: a "<module> compute buffer size: ... on
//                Vulkan<N>" line for a module that is not an auxiliary one (text encoder / VAE /
//                TAE), or the auto-fit plan's line for the diffusion component ("DiT   params ...
//                -> compute Vulkan<N>, params ..."),
//              - and no "compute buffer size ... on CPU" line and no plan line "-> compute CPU".
//            sd.cpp prints the backend via ggml_backend_name() (compute: "CPU" for the CPU
//            backend, "Vulkan0" for the first Vulkan device) and the params buffer type via
//            ggml_backend_buft_name() ("CPU", "Vulkan0", or "Vulkan_Host" for pinned host
//            memory). Params resting in RAM with compute on the GPU is the sanctioned
//            overflow, so a params line "on CPU" / "on Vulkan_Host" and a plan line
//            "params RAM" / "params CPU" are neither evidence nor a placement.
//            sd.cpp's log record has two shapes and BOTH are read for good, because nodes upgrade at
//            different times (a fleet update moves one node to the next release while the rest stay):
//              master-929 and before   "[VERBOSE] ggml_runner.cpp:1019 - <message>"   (the tag is padded to 7:
//                                      "[INFO   ]", "[WARN   ]", "[ERROR  ]"; the line number is padded too)
//              master-945 and after    "[V] <message> --- ggml_runner.cpp:1019"       (#2104: one-letter tags [D] [V]
//                                      [I] [W] [E], and the source location moved to the END, unpadded; #2106: the
//                                      separator is " --- " (a build between the two commits has " - "), and the
//                                      newlines of a prompt echo are escaped, "\n" as the two characters)
//            A record of several lines (a parameter dump, "System Info") carries its tag on the FIRST line and its
//            tail on the LAST ("} --- main.cpp:699"). So every sd.cpp line is NORMALISED before an anchored shape reads
//            it (normalizeSdLine): one leading level tag in either spelling (with the "file.cpp:N - " source prefix
//            after the old one) and one trailing source tail are cut, and the raw line is kept for the error
//            messages. The shapes that are anchored at the start of a record read the normalised line; the plain
//            words (a CPU backend, a lost device) read the whole raw line.
//   da3      (depth-anything.cpp da3-cli)    PASS needs "[da3] da::Backend using device: Vulkan<N>".
//            "offload_weights: ... (N host-only tensors kept on CPU ...)" is a storage line.
//   audiocpp (audio.cpp audiocpp_cli)        PASS needs a "<component>.weights.buffer_name
//            Vulkan<N>" line, and ANY "<component>.weights.buffer_name CPU" line is a placement
//            (upstream loads a second host copy of the ACE-Step planner for the prompt prefill).
//
// Never scanned for anything: ggml's "loaded CPU backend" registration, "Initializing
// backend: CPU", and (sd.cpp only, on its own record shapes) the SDCliParams / SDContextParams /
// SDGenerationParams dump blocks and the tokenizer echo lines ("split prompt ..." / "parse '...'").
// A dump block opens on the record whose whole message is "SDCliParams {" (or the other two) and
// closes on a normalised "}" at column 0: the bare "}" of the old shape, "} --- main.cpp:699" of the
// new one. A block that is never closed ends at the next LONG-tag record (the old shape's valve)
// and, in the new shape, ONLY at its close: the one-letter tags are deliberately not a valve. A new-shape
// record ends with its tail, so the "}" always carries one; a block that does not close is a record shape
// this guard does not understand, and the loud outcome is the right one (the rest of the log is skipped,
// the run ends without its evidence and fails CPU_PLACEMENT "no GPU evidence", the way this very shape
// first showed), where a valve would pass the run on a guess and hide that the closing line changed.
//
// The request's own text (prompt, negative prompt, TTS text, lyrics) never switches the CPU detector
// off: every other line is scanned whole, the placement and evidence shapes are anchored at the start
// of the record (after its head), and a line that echoes the request can only fail to count as
// POSITIVE evidence. A plain-words shape (a device reset) is read with the request's text taken out.
// A record that IS a line of the request ("[V] SDCliParams {" as a prompt line) opens no dump block.
export const GUARD_ENGINES = ["sdcpp", "da3", "audiocpp"];

const SOFTWARE_DEVICE = /\b(?:llvmpipe|lavapipe|swiftshader)\b/i;
// audio.cpp's own record heads: "[TIMING ts=20261003-172137] ", "[TRACE ts=...] "
const AUDIO_HEAD = String.raw`(?:\[[A-Z]+(?:\s+ts=[^\]\s]*)?\s*\]\s*)*`;
// sd.cpp's record head and tail, in the two shapes (see the comment above). Every placement and evidence
// shape below starts at the beginning of the record (after the head), so text the request echoes in the
// MIDDLE of some other line can neither forge a shape nor, by being a substring of a real one, hide it.
//   old head   "[VERBOSE] ggml_runner.cpp:1019 - ", "[INFO   ] main.cpp:699  - ": the long tag and the source it prints in front
//   new head   "[V] ": the one-letter tag alone (the bare lines of the tests and of ggml itself have none)
//   tail       " --- ggml_runner.cpp:1019": the new source, behind the message (" - ..." in a build between #2104 and #2106)
const SD_OLD_HEAD = /^\[(?:DEBUG|VERBOSE|INFO|WARN|WARNING|ERROR)\s*\]\s+[\w./+-]+:\d+\s+-\s+/;
const SD_NEW_TAG = /^\[[DVIWE?]\](?:\s|$)/;
// (one whitespace char in front of the dashes, the run before it is cut by hand: a `\s+` here would make a
// line of thousands of spaces, such as a padded prompt in a dump, cost the square of its length)
const SD_TAIL = /\s(?:---|-)\s+[\w./+-]+:\d+\s*$/;

// normalizeSdLine: an sd.cpp log line without its record head and its source tail. Pure; cuts at most
// one head and one tail, and only at the two ends of the line.
//   text    the rest. Leading whitespace is kept: a dump's indented "  }" is not its closing "}".
//   tagged  the line carried a head, i.e. it STARTS a record (a bare "SDCliParams {" is only text).
// A record of several lines has its head on the first line and its tail on the last, so a middle line
// has neither and the last one is its tail alone ("} --- main.cpp:699" is "}").
export function normalizeSdLine(line) {
  let text = String(line ?? "");
  let tagged = false;
  const head = SD_OLD_HEAD.exec(text) || SD_NEW_TAG.exec(text);
  if (head) {
    text = text.slice(head[0].length);
    tagged = true;
  }
  const tail = SD_TAIL.exec(text);
  if (tail) {
    let end = tail.index;
    while (end > 0 && /\s/.test(text[end - 1])) end--; // the space(s) the tail was appended after go with it
    text = text.slice(0, end);
  }
  return { text, tagged };
}

// sd.cpp's parameter dumps: only the engine that prints them (sdcpp) opens a block, and only on a
// record whose whole message is the header, so text echoed elsewhere that happens to end in
// "SDCliParams {" opens nothing. Matched against the NORMALISED text of a tagged line.
const BLOCK_START = /^(?:SDCliParams|SDContextParams|SDGenerationParams)\s*\{\s*$/;
// The long-tag record heads that end an unterminated dump block (the old shape's valve; the new
// shape's one-letter tags are not in it on purpose, see above), plus audio.cpp's own heads.
const LOG_PREFIX = /^\[(?:VERBOSE|INFO|WARN|WARNING|ERROR|DEBUG|TRACE|TIMING)(?:\s+ts=[^\]\s]*)?\s*\]/;
const TOKENIZER_ECHO = [/^split prompt\s+"/, /^parse\s+'/];
const GPU_RESET_RE = /ErrorDeviceLost|device lost|context is lost/i;
const GGML_DEVICE_LINE = /^\s*ggml_vulkan:\s*(\d+)\s*=\s*(.+?)\s*(?:\||$)/;
const NO_VULKAN_DEVICE = /ggml_vulkan:\s*Found\s+0\s+Vulkan\s+devices|ggml_vulkan:\s*No\s+devices\s+found/i;
// stable-diffusion.cpp: a compute buffer on a backend; the module name is one word. Read on the normalised text.
const SD_COMPUTE_BUFFER = /^([\w.+-]+)\s+compute buffer size:.*?\bon\s+(\S+)/;
// The auto-fit plan's line of the diffusion component, "DiT   params 5162 MiB, compute reserve 2048 MiB ->
// compute Vulkan0, params Vulkan0" (the old shape's head eats its indentation, the new one keeps it). The
// compute backend is group 1. Read on the normalised text; "-> compute CPU" is a placement (SD_CPU_SHAPES).
const SD_PLAN_DIT = /^\s*DiT\s+params\b.*?->\s*compute\s+([A-Za-z_]+\d*)\b/i;
// The modules that are NOT the diffusion stage, by sd.cpp's own runner names (get_desc()), matched
// against the WHOLE module name: the text encoders (t5, umt5, clip, llm), the VAEs (vae, wan_vae,
// flux_vae, ...), the tiny autoencoders (tae*, taesd, taehv), the control-net runner (control_net),
// ESRGAN and the vision tower. A substring match would count a diffusion model whose NAME holds one
// of the words (Wan2.1-Fun-14B-Control) as auxiliary and fail a healthy run for lack of evidence.
export const SD_AUX_MODULE = /^(?:(?:um)?t5(?:[_-]?xxl)?|clip(?:[_-](?:l|g|h|vision|text))?|llm|text[_-]?enc(?:oder)?|conditioner|(?:[\w.]*[_-])?vae(?:[_-][\w.]*)?|tae\w*|control[_-]?net|esrgan|pmid|(?:clip[_-])?vision(?:[_-]\w*)?)$/i;
const SD_CPU_SHAPES = [
  /\bloading CPU backend\b/i, // [WARN] ggml_extend_backend.cpp: the actual no-GPU fallback
  /\bUsing CPU backend\b/i, // LOG_VERBOSE when the default compute backend is the CPU
  /\bNo devices found!/i,
  /->\s*compute\s+cpu\d*\b/i, // auto-fit plan: "... -> compute CPU, params RAM"
  /auto-fit:\s*no GPU memory budget available;\s*using CPU/i,
];
const DA3_BACKEND = /^\[da3\]\s+da::Backend using device:\s*(\S+)/i;
const DA3_CPU_SHAPES = [/^\[da3\]\s+offload_weights:.*->\s*CPU\b/i, /^\[da3\]\s+.*node\(s\) run on CPU\b/i];
const AUDIO_BUFFER = new RegExp("^" + AUDIO_HEAD + String.raw`([\w.-]+)\.weights\.buffer_name\s+(\S+)`);

function stripAnsi(s) {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");
}

// buildEchoMatcher: what the guard knows of the request's own text (prompt, negative prompt, TTS text,
// lyrics; each line of a multi-line text counts too). It is used for two things only, and never to
// make a line invisible to the CPU detector:
//   isEcho(line)  the line IS an echo of that text (the text is most of the line, or all of it): such
//                 a line can carry no POSITIVE evidence, so text that looks like a device or buffer
//                 line is not evidence. A real evidence line that merely contains a fragment of the
//                 text ("Vulkan0" as a prompt) is not an echo.
//   strip(line)   the line with the text taken out, for the one shape that is plain words (a device
//                 reset), so a prompt about "device lost" cannot end a healthy run.
// A piece shorter than 6 characters is ignored: no evidence line is that short, so it can forge
// nothing, and stripping it would only eat real words.
// The pieces of a text are the text, each of its lines, and the text as sd.cpp (master-945+) prints
// a prompt: on ONE line, with every newline escaped to the two characters "\n" (and a CR to "\r").
// Without that spelling a two-line prompt echoed on one line would be two half-covered pieces
// (under 60% each) and no echo at all.
const escapeNewlines = (s) => s.replace(/\r/g, "\\r").replace(/\n/g, "\\n");
function buildEchoMatcher(echoes) {
  const pieces = new Set();
  for (const raw of echoes || []) {
    if (typeof raw !== "string") continue;
    for (const part of [raw, escapeNewlines(raw), ...raw.split(/\r\n|\r|\n/)]) if (part.trim().length >= 6) pieces.add(part.trim());
  }
  const full = [...pieces].sort((x, y) => y.length - x.length);
  return {
    isEcho(line) {
      const t = line.trim();
      return t !== "" && full.some((p) => line.includes(p) && p.length >= 0.6 * t.length);
    },
    strip(line) {
      let out = line;
      for (const p of full) out = out.split(p).join(" ");
      return out;
    },
  };
}

// createLogGuard: a stateful line scanner. scan(line) returns null, or
// {kind: "CPU_PLACEMENT" | "GPU_RESET", line, lineNo} for a line that must abort the run now;
// verdict() is called when the engine exits and says whether the positive evidence was seen.
export function createLogGuard({ engine, echoes = [] }) {
  if (!GUARD_ENGINES.includes(engine)) throw new Error(`createLogGuard: unknown engine ${JSON.stringify(engine)} (want ${GUARD_ENGINES.join(", ")})`);
  const echo = buildEchoMatcher(echoes);
  const st = { lineNo: 0, inBlock: false, device: false, diffusion: false, da3: false, audio: false };
  const evidence = [];
  const note = (s) => { if (evidence.length < 20) evidence.push(s); };

  const cpu = (line) => ({ kind: CPU_PLACEMENT, line: line.trim(), lineNo: st.lineNo });

  function scan(raw) {
    st.lineNo++;
    const line = stripAnsi(String(raw ?? ""));
    // `text` is what the shapes anchored at the start of a record read. For sd.cpp it is the line
    // without its record head and its source tail (normalizeSdLine); the other engines' lines as they are.
    let text = line;
    if (engine === "sdcpp") {
      const rec = normalizeSdLine(line);
      text = rec.text;
      // the parameter dumps and the tokenizer echoes repeat the request's text and every path:
      // the only lines that are skipped outright, and only on sd.cpp's own record shapes
      if (st.inBlock) {
        if (/^\}\s*$/.test(text)) { st.inBlock = false; return null; } // the old "}" and the new "} --- main.cpp:699"
        if (!LOG_PREFIX.test(line)) return null; // a dump line, or the lines of a new-shape record: never scanned
        st.inBlock = false; // an unterminated old-shape dump: the next long-tag record ends it
      }
      if (rec.tagged && BLOCK_START.test(text) && !echo.isEcho(line)) { st.inBlock = true; return null; }
      if (TOKENIZER_ECHO.some((re) => re.test(text))) return null;
    }
    // the head and the tail of an sd.cpp record dilute the share the request's text has of the line, so
    // the message alone is asked too
    const isEcho = echo.isEcho(line) || (text !== line && echo.isEcho(text));

    // a lost device is plain words: read it with the request's own text taken out
    if (GPU_RESET_RE.test(echo.strip(line))) return { kind: GPU_RESET, line: line.trim(), lineNo: st.lineNo };

    // every placement shape reads the WHOLE line, whatever the request said: its text cannot switch
    // the CPU detector off
    if (NO_VULKAN_DEVICE.test(line)) return cpu(line);
    if (/\bggml_vulkan\b/i.test(line) && SOFTWARE_DEVICE.test(line)) return cpu(line);
    const dev = GGML_DEVICE_LINE.exec(line);
    if (dev) {
      if (!isEcho) {
        st.device = true;
        note(`device ${dev[1]} = ${dev[2]}`);
      }
      return null;
    }

    if (engine === "sdcpp") {
      if (SD_CPU_SHAPES.some((re) => re.test(line))) return cpu(line);
      const cb = SD_COMPUTE_BUFFER.exec(text);
      if (cb) {
        const [, desc, on] = cb;
        if (/^CPU\d*$/i.test(on)) return cpu(line);
        if (/^Vulkan\d+$/i.test(on) && !isEcho && !SD_AUX_MODULE.test(desc)) {
          st.diffusion = true;
          note(`${desc} compute on ${on}`);
        }
      }
      // the auto-fit plan puts the diffusion component on a device before anything is loaded: a second
      // way to show the diffusion stage is on the GPU ("-> compute CPU" was a placement above)
      const plan = SD_PLAN_DIT.exec(text);
      if (plan && /^Vulkan\d+$/i.test(plan[1]) && !isEcho) {
        st.diffusion = true;
        note(`DiT plan: compute on ${plan[1]}`);
      }
    } else if (engine === "da3") {
      const b = DA3_BACKEND.exec(line);
      if (b) {
        if (/^Vulkan\d+$/i.test(b[1])) {
          if (!isEcho) {
            st.da3 = true;
            note(`da3 backend ${b[1]}`);
          }
          return null;
        }
        return cpu(line);
      }
      if (DA3_CPU_SHAPES.some((re) => re.test(line))) return cpu(line);
    } else {
      const m = AUDIO_BUFFER.exec(line);
      if (m) {
        if (/^CPU\d*$/i.test(m[2])) return cpu(line);
        if (/^Vulkan\d+$/i.test(m[2]) && !isEcho) {
          st.audio = true;
          note(`${line.trim().split(/\s+/).slice(-2).join(" ")}`);
        }
      }
    }
    return null;
  }

  function verdict() {
    let ok;
    let expected;
    if (engine === "sdcpp") {
      ok = st.device && st.diffusion;
      expected = `a non-software "ggml_vulkan: <n> = <device>" line${st.device ? " (seen)" : " (missing)"} and a diffusion-stage "compute buffer size ... on Vulkan<N>" line (or the auto-fit plan's DiT "-> compute Vulkan<N>" line)${st.diffusion ? " (seen)" : " (missing)"}`;
    } else if (engine === "da3") {
      ok = st.da3;
      expected = `a "da::Backend using device: Vulkan<N>" line`;
    } else {
      ok = st.audio;
      expected = `a "<component>.weights.buffer_name Vulkan<N>" line`;
    }
    return { ok, expected, evidence: evidence.slice() };
  }

  return { scan, verdict, engine };
}

// scanLog: run a whole log text through a fresh guard (tests and offline checks). Returns
// {fatal, verdict}: the first aborting line as {kind, line, lineNo} (null when none) and the
// end-of-run verdict. Pure.
export function scanLog(text, opts) {
  const g = createLogGuard(opts);
  let fatal = null;
  for (const l of String(text ?? "").split(/\r\n|\r|\n/)) {
    fatal = g.scan(l);
    if (fatal) break;
  }
  return { fatal, verdict: g.verdict() };
}

export function cpuPlacementError(hit) {
  return new Error(`${CPU_PLACEMENT}: the engine placed a model on the CPU (log line ${hit.lineNo}: ${hit.line}) — no model runs on CPU on this engine; aborted`);
}

export function noGpuEvidenceError(label, verdict) {
  return new Error(`${CPU_PLACEMENT}: no GPU evidence was seen in ${label}'s log (expected ${verdict.expected}) — a run that cannot show it ran on the GPU is treated as a CPU run; no model runs on CPU on this engine`);
}

export function gpuResetError(hit, engine = "sdcpp") {
  const advice = engine === "audiocpp"
    ? "shorten the request (seconds or text length)"
    : "keep the request inside the configured token cap (sdcpp_max_tokens, or animategen_sdcpp_max_tokens for animate) by lowering width/height/frames";
  return new Error(`${GPU_RESET}: the GPU reset during the run (log line ${hit.lineNo}: ${hit.line}). The amdgpu driver resets the compute ring when one GPU dispatch runs past its default 2 s lockup timeout; ${advice}. This failure is never retried automatically.`);
}

// ---------------------------------------------------------------- token cap

// latentTokens: the number of latent tokens one attention pass of a Wan-family DiT sees.
// The VAE downsamples `stride` times per side, the DiT patch is 2x2, time is /4 with the
// first frame kept, and a VACE reference image adds `refLatentFrames` latent frames:
//   ceil(W/(stride*2)) * ceil(H/(stride*2)) * (floor((frames-1)/4) + 1 + refLatentFrames)
// internal/config LatentTokens is the Go twin; render/testdata/token-cap-table.json pins both.
export function latentTokens({ width, height, frames, stride, refLatentFrames = 0 }) {
  const cell = stride * 2;
  return Math.ceil(width / cell) * Math.ceil(height / cell) * (Math.floor((frames - 1) / 4) + 1 + refLatentFrames);
}

// fitAdvice: how a request over the cap can be made to fit (frames at this size first).
function fitAdvice({ width, height, stride, refLatentFrames, cap }) {
  const cell = stride * 2;
  const perLatentFrame = Math.ceil(width / cell) * Math.ceil(height / cell);
  const latentFrames = Math.floor(cap / perLatentFrame) - refLatentFrames;
  if (latentFrames >= 2) return `at ${width}x${height} up to ${(latentFrames - 1) * 4 + 1} frames fit; or lower width/height`;
  return `even 5 frames do not fit at ${width}x${height}: lower width and height`;
}

// tokenCapFromFlags: {cap, stride} from --max-tokens / --vae-stride, or null (no cap
// configured = no check). A cap needs a stride of 8 or 16.
export function tokenCapFromFlags(flags) {
  const rawCap = flags["max-tokens"];
  if (rawCap === undefined || rawCap === null || rawCap === "") return null;
  const cap = Number(rawCap);
  if (!Number.isInteger(cap) || cap <= 0) throw new Error(`--max-tokens must be a positive integer (got ${JSON.stringify(rawCap)})`);
  const stride = Number(flags["vae-stride"]);
  if (stride !== 8 && stride !== 16) throw new Error(`--vae-stride must be 8 or 16 when --max-tokens is set (got ${JSON.stringify(flags["vae-stride"])})`);
  return { cap, stride };
}

// checkTokenCap: throws TOKEN_CAP_EXCEEDED when the request is over the configured cap, and
// returns the token count (or 0 when no cap is configured). Runs before anything spawns.
export function checkTokenCap({ flags, width, height, frames, refLatentFrames = 0 }) {
  const c = tokenCapFromFlags(flags);
  if (!c) return 0;
  const tokens = latentTokens({ width, height, frames, stride: c.stride, refLatentFrames });
  if (tokens > c.cap) {
    throw new Error(`${TOKEN_CAP_EXCEEDED}: ${width}x${height}x${frames}${refLatentFrames ? " + reference" : ""} needs ${tokens} latent tokens (VAE stride ${c.stride}, patch 2) but the cap is ${c.cap}; one GPU dispatch that long would hit the amdgpu 2 s lockup timeout and reset the GPU. To fit: ${fitAdvice({ width, height, stride: c.stride, refLatentFrames, cap: c.cap })}. Not retried.`);
  }
  return tokens;
}

// ---------------------------------------------------------------- deadline

// processStartMs: when this process started, so a deadline covers node start-up and every
// pre-spawn step (the llama-swap drain, frame extraction) like gpugen's own does.
export function processStartMs() {
  return Date.now() - Math.round(process.uptime() * 1000);
}

// makeDeadline: a deadline `timeoutSec` after the runner process started (0/unset = none).
export function makeDeadline(timeoutSec, startMs = processStartMs()) {
  const sec = Number(timeoutSec);
  const active = Number.isFinite(sec) && sec > 0;
  const atMs = active ? startMs + Math.round(sec * 1000) : 0;
  return {
    active,
    atMs,
    // remainingMs: what is left, at least 1 ms while active (0 = no deadline, the form
    // runEngine reads as "unbounded").
    remainingMs: () => (active ? Math.max(1, atMs - Date.now()) : 0),
    // enforce: throws a timeout when the budget is already spent before a step starts.
    enforce: (what) => {
      if (active && atMs - Date.now() <= 0) throw new Error(`${what} timeout: the ${Math.round(sec)}s budget (counted from the runner's start) was spent before it could start`);
    },
  };
}

// ---------------------------------------------------------------- lifecycle and kill

const liveEngines = new Set();
const cleanups = new Set();
let exitHookInstalled = false;
let lifecycleInstalled = false;

// How long a runner that is told to stop waits for its engine to be gone before it exits, and so
// before the media lease is released: long enough for the kernel to tear down an engine's Vulkan
// context (the amdgpu lockup timeout is 2 s), shorter than gpugen's SIGTERM grace (5 s) so the
// runner is not SIGKILLed in the middle of the wait.
export const ENGINE_EXIT_WAIT_MS = 3000;

// processTable: pid -> ppid for every process (POSIX only; empty on Windows or on failure):
// /proc on Linux, `ps` elsewhere.
export function processTable() {
  const table = new Map();
  if (process.platform === "win32") return table;
  try {
    if (existsSync("/proc/self/stat")) {
      for (const name of readdirSync("/proc")) {
        if (!/^\d+$/.test(name)) continue;
        try {
          const stat = readFileSync(`/proc/${name}/stat`, "utf8");
          // "pid (comm) S ppid ...": comm may hold spaces and parentheses, so cut at the LAST ")"
          table.set(Number(name), Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]));
        } catch { /* gone while we looked */ }
      }
    } else {
      const r = spawnSync("ps", ["-A", "-o", "pid=,ppid="], { encoding: "utf8" });
      for (const line of String(r.stdout || "").split("\n")) {
        const m = /^\s*(\d+)\s+(\d+)\s*$/.exec(line);
        if (m) table.set(Number(m[1]), Number(m[2]));
      }
    }
  } catch { /* best effort */ }
  return table;
}

// descendantsOf: every descendant of `pid`, deepest first (so a parent is never killed before its
// children can be found). Pure over `table`.
export function descendantsOf(pid, table = processTable()) {
  const children = new Map();
  for (const [p, pp] of table) {
    if (!children.has(pp)) children.set(pp, []);
    children.get(pp).push(p);
  }
  const order = [];
  const walk = (p) => { for (const c of children.get(p) || []) { order.push(c); walk(c); } };
  walk(pid);
  return order.reverse();
}

// killTree: the engine and everything it started, not just the child. On Windows taskkill /T.
// Elsewhere the engine is NOT detached: it stays in the runner's process group, so that gpugen's
// SIGKILL of that group (when the runner itself cannot answer a SIGTERM) takes the engine with it
// (a detached engine would survive it and keep the iGPU). The runner therefore walks the engine's
// descendants itself. A child that does lead its own group (a caller-supplied spawn) has that group
// killed too. Best-effort, never throws.
export function killTree(child) {
  if (!child || child.pid === undefined || child.exitCode !== null || child.signalCode) return;
  try {
    if (process.platform === "win32") {
      spawnSync("taskkill", ["/PID", String(child.pid), "/T", "/F"], { stdio: "ignore" });
      return;
    }
    try { process.kill(-child.pid, "SIGKILL"); } catch { /* not a group leader: the normal case */ }
    for (const p of descendantsOf(child.pid)) {
      try { process.kill(p, "SIGKILL"); } catch { /* already gone */ }
    }
    try { process.kill(child.pid, "SIGKILL"); } catch { /* already gone */ }
  } catch { /* nothing more to do */ }
}

function killLiveEngines() {
  for (const c of [...liveEngines]) killTree(c);
}

// enginePids: the pids of every live engine and its descendants (what a shutdown must see gone).
export function enginePids() {
  if (liveEngines.size === 0) return []; // nothing to find: skip the process table scan on a normal exit
  const table = processTable();
  const out = [];
  for (const c of liveEngines) {
    if (c.pid === undefined || c.exitCode !== null || c.signalCode) continue;
    out.push(c.pid, ...descendantsOf(c.pid, table));
  }
  return out;
}

// pidGone: the process does not exist, or is only a zombie waiting to be reaped (a killed child of
// this very process stays one until the event loop runs, and the shutdown paths below are
// synchronous).
export function pidGone(pid) {
  try {
    process.kill(pid, 0);
  } catch (e) {
    return !(e && e.code === "EPERM");
  }
  if (process.platform === "win32") return false;
  try {
    if (existsSync("/proc/self/stat")) {
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
      return /^[ZX]/.test(stat.slice(stat.lastIndexOf(")") + 2));
    }
    const r = spawnSync("ps", ["-o", "stat=", "-p", String(pid)], { encoding: "utf8" });
    return r.status !== 0 || /^\s*Z/.test(String(r.stdout || ""));
  } catch {
    return true; // /proc/<pid> vanished between the two calls
  }
}

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// waitUntilGone: block (at most maxMs) until every pid is gone. Returns {gone, waitedMs}.
export function waitUntilGone(pids, { maxMs = ENGINE_EXIT_WAIT_MS, pollMs = 25, isGone = pidGone, sleep = sleepSync, now = Date.now } = {}) {
  const t0 = now();
  let alive = pids.filter((p) => !isGone(p));
  while (alive.length > 0 && now() - t0 < maxMs) {
    sleep(pollMs);
    alive = alive.filter((p) => !isGone(p));
  }
  return { gone: alive.length === 0, waitedMs: now() - t0, alive };
}

function runCleanups() {
  for (const fn of [...cleanups]) {
    try { fn(); } catch { /* best effort */ }
  }
}

// stopEngines: kill every live engine tree and WAIT for it to be gone. A runner that exits (and so
// lets the lease go) while its engine is still tearing down its Vulkan context lets the next job
// start a second engine on the same iGPU.
function stopEngines(deps) {
  const pids = deps.pids();
  deps.kill();
  return pids.length ? deps.wait(pids) : { gone: true, waitedMs: 0, alive: [] };
}

const defaultDeps = {
  pids: enginePids,
  kill: killLiveEngines,
  wait: (pids) => waitUntilGone(pids),
  cleanup: runCleanups,
  exit: (code) => process.exit(code),
};

function ensureExitHook() {
  if (exitHookInstalled) return;
  exitHookInstalled = true;
  process.on("exit", () => {
    // an engine still live when the process exits (a crash, an uncaught error) is killed AND waited for
    stopEngines(defaultDeps);
    runCleanups();
  });
}

function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return e && e.code === "EPERM";
  }
}

// installLifecycle: make the runner kill its engine tree and remove its temp dirs when it is
// told to stop (SIGTERM / SIGINT / SIGHUP) and when its parent disappears (process.ppid
// changes, or the original parent no longer exists), so cleanup never depends on a graceful
// exit by the parent. It exits only AFTER the engine tree is gone (bounded by ENGINE_EXIT_WAIT_MS).
// Idempotent. `pollMs` is the parent-watch interval; `deps` replaces the pieces a test observes
// (pids, kill, wait, cleanup, exit).
export function installLifecycle({ pollMs = Number(process.env.IGPU_PARENT_POLL_MS) || 1000, deps = {} } = {}) {
  ensureExitHook();
  if (lifecycleInstalled) return;
  lifecycleInstalled = true;
  const d = { ...defaultDeps, ...deps };
  let dying = false;
  const die = (code, why) => {
    if (dying) return;
    dying = true;
    try { process.stderr.write(`igpu-engine: ${why}; killing the engine tree and cleaning up\n`); } catch { /* stderr may be gone */ }
    const w = stopEngines(d);
    if (!w.gone) {
      try { process.stderr.write(`igpu-engine: engine process(es) ${w.alive.join(",")} still present after ${w.waitedMs} ms\n`); } catch { /* ignore */ }
    }
    d.cleanup();
    d.exit(code);
  };
  for (const [sig, code] of [["SIGTERM", 143], ["SIGINT", 130], ["SIGHUP", 129]]) {
    process.on(sig, () => die(code, `received ${sig}`));
  }
  const parent = process.ppid;
  if (parent > 1) {
    const t = setInterval(() => {
      if (process.ppid !== parent || !pidAlive(parent)) die(1, "the parent process is gone");
    }, pollMs);
    t.unref();
  }
}

// Signals that mean the engine itself crashed (an assert, a segfault, a bad access), as opposed to
// being told to stop (SIGTERM, SIGINT, SIGHUP) or killed (SIGKILL: the OOM killer or a timeout).
const CRASH_SIGNALS = new Set(["SIGSEGV", "SIGABRT", "SIGBUS", "SIGFPE", "SIGTRAP", "SIGSYS"]);

// memoryFailure: the first log line in which ggml / sd.cpp / audio.cpp / the Vulkan driver reports it
// ran out of memory ("insufficient memory (attempted to allocate 5162.00 MB)", "alloc compute buffer
// failed", "Device memory allocation of size N failed", ErrorOutOfDeviceMemory), or "".
const MEMORY_FAILURE = /insufficient memory|alloc(?:ate|ation)? compute buffer failed|failed to alloc(?:ate)? (?:compute )?buffer|device memory allocation of size|ErrorOutOf(?:Device|Host)Memory|\bout of memory\b/i;
export function memoryFailure(lines) {
  for (const l of lines || []) if (MEMORY_FAILURE.test(l)) return String(l).trim().slice(0, 200);
  return "";
}

// memoryError: the typed OUT_OF_MEMORY failure (err_class oom), naming the log line that says so.
export function memoryError(label, line, how) {
  return new Error(`${OUT_OF_MEMORY}: ${label} ran out of memory (${how}): ${line}. Lower the width, height or frames, or free memory on this box. Not retried automatically.`);
}

// engineExitError: the error for an engine that exited non-zero: a refused model file
// (MODEL_INCOMPATIBLE), an out-of-memory report in its log (OUT_OF_MEMORY), else "<label> exited N".
export function engineExitError(label, code, log, modelFile) {
  const lines = String(log ?? "").split(/\r\n|\r|\n/);
  return (modelFile ? modelMetadataError(log, modelFile) : null)
    || (memoryFailure(lines) ? memoryError(label, memoryFailure(lines), `exit ${code}`) : null)
    || new Error(`${label} exited ${code}`);
}

// runEngine: spawn `bin args`, tee every output line to stderr (that is the progress the Go
// side tails), feed each line to `guard` (createLogGuard) and settle:
//   resolves {code, log}            the engine exited normally (any code; the caller judges it)
//                                   and, for a clean exit, the guard saw its positive evidence
//   rejects  CPU_PLACEMENT          a line placed compute on the CPU, or a clean exit showed no
//                                   GPU evidence at all
//   rejects  GPU_RESET              the log reports a lost GPU device
//   rejects  ILLEGAL_INSTRUCTION    the engine died of SIGILL / exit 132
//   rejects  signal                 the engine was killed by a signal (SIGKILL suggests the OOM
//                                   killer on a UMA iGPU)
//   rejects  timeout                timeoutMs elapsed
// Every rejection that kills the engine waits for the process to be gone first (bounded), so
// the GPU is free when the promise settles. stdout and stderr are both read.
export function runEngine({ bin, args, env, timeoutMs = 0, guard, label = "engine", spawnImpl = spawn }) {
  if (!guard) throw new Error("runEngine needs a log guard (createLogGuard): no engine runs unobserved");
  return new Promise((resolve, reject) => {
    let settled = false;
    let aborted = null;
    ensureExitHook();
    const child = spawnImpl(bin, args, {
      env: { ...process.env, ...(env || {}) },
      stdio: ["ignore", "pipe", "pipe"],
      // NOT detached: the engine stays in the runner's process group, so a SIGKILL of that group (gpugen's
      // escalation when the runner cannot answer a SIGTERM) takes the engine with it. killTree walks its
      // descendants itself.
      detached: false,
      windowsHide: true,
    });
    liveEngines.add(child);
    const chunks = [];
    let timer = null;
    let deathTimer = null;
    const finish = (fn, v) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      if (deathTimer) clearTimeout(deathTimer);
      liveEngines.delete(child);
      fn(v);
    };
    // abort: kill the tree now and reject once it is dead (or after 5 s, whichever is first).
    const abort = (err) => {
      if (settled || aborted) return;
      aborted = err;
      killTree(child);
      deathTimer = setTimeout(() => finish(reject, err), 5000);
    };
    const onLine = (line) => {
      if (settled || aborted) return;
      chunks.push(line);
      process.stderr.write(line + "\n");
      const hit = guard.scan(line);
      if (hit) abort(hit.kind === GPU_RESET ? gpuResetError(hit, guard.engine) : cpuPlacementError(hit));
    };
    const feed = (stream) => {
      let buf = "";
      stream.setEncoding("utf8");
      stream.on("data", (d) => {
        buf += d;
        const parts = buf.split(/\r\n|\r|\n/);
        buf = parts.pop();
        for (const p of parts) if (p !== "") onLine(p);
      });
      stream.on("end", () => { if (buf !== "") onLine(buf); });
    };
    feed(child.stdout);
    feed(child.stderr);
    child.on("error", (e) => finish(reject, new Error(`${label} failed to start: ${e.message}`)));
    child.on("close", (code, signal) => {
      if (aborted) return finish(reject, aborted);
      if (signal === "SIGILL" || code === 132) {
        return finish(reject, new Error(`${ILLEGAL_INSTRUCTION}: ${label} died with SIGILL (exit ${code ?? 132}): the binary uses CPU instructions this machine's CPU lacks (an instruction-set mismatch, e.g. the audio.cpp release build is AVX-512 and a Zen 3 CPU has none). Build the engine on the node instead of using a prebuilt release.`));
      }
      if (signal) {
        const mem = memoryFailure(chunks);
        if (mem) return finish(reject, memoryError(label, mem, `died of signal ${signal}`));
        if (signal === "SIGKILL") {
          return finish(reject, new Error(`${label} was killed by signal SIGKILL (SIGKILL on a UMA iGPU box usually means the kernel out-of-memory (OOM) killer)`));
        }
        if (CRASH_SIGNALS.has(signal)) {
          return finish(reject, new Error(`${ENGINE_CRASHED}: ${label} died of signal ${signal}: an engine crash, not a timeout; the same request would crash it again. Not retried automatically.`));
        }
        return finish(reject, new Error(`${label} was killed by signal ${signal}`));
      }
      if (code === 0) {
        const v = guard.verdict();
        if (!v.ok) return finish(reject, noGpuEvidenceError(label, v));
      }
      finish(resolve, { code, log: chunks.join("\n"), evidence: guard.verdict().evidence });
    });
    if (timeoutMs > 0) {
      timer = setTimeout(() => {
        abort(new Error(`${label} timeout after ${Math.round(timeoutMs / 1000)}s (killed)`));
      }, timeoutMs);
    }
  });
}

// ---------------------------------------------------------------- normalization helpers

// normalizeFrames: a Wan-family video model takes 4k+1 frames. Round to the NEAREST 4k+1
// (a tie goes up), minimum 5. A non-numeric or non-positive request returns `def`
// (itself normalized).
export function normalizeFrames(n, def = 49) {
  let v = Math.round(Number(n));
  if (!Number.isFinite(v) || v <= 0) v = Math.round(Number(def));
  if (!Number.isFinite(v) || v <= 5) return 5;
  const down = Math.floor((v - 1) / 4) * 4 + 1;
  const up = down + 4;
  return v - down < up - v ? down : up;
}

// floorFrames: the largest 4k+1 not above `available`, or 0 when fewer than 5 exist. Used
// when a driver clip is shorter than the frames the caller asked for.
export function floorFrames(available) {
  const a = Math.floor(Number(available));
  if (!Number.isFinite(a) || a < 5) return 0;
  return Math.floor((a - 1) / 4) * 4 + 1;
}

// normalizeSize: width/height to a multiple of 32, FLOORED (never grow a render past the
// memory the caller sized for), minimum 32. Non-numeric or non-positive returns `def`.
export function normalizeSize(v, def) {
  const n = Math.floor(Number(v));
  if (!Number.isFinite(n) || n <= 0) return def;
  return Math.max(32, Math.floor(n / 32) * 32);
}

// vulkanDeviceFromBackend: "vulkan1" -> "1", "vulkan" -> "", per-module forms -> the first
// vulkanN found. Used to pin GGML_VK_VISIBLE_DEVICES for a helper whose CLI has no backend
// flag (depth-anything.cpp), so it sees the same device the sd-cli backend names.
export function vulkanDeviceFromBackend(backend) {
  const m = /vulkan(\d+)/i.exec(String(backend ?? ""));
  return m ? m[1] : "";
}

// finiteNum: a flag value as a number, or undefined (unset or not a number).
export function finiteNum(v) {
  if (v === undefined || v === null || v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}

// mp4Args: re-encode a sd-cli .webm/.avi to an H.264 mp4 (yuv420p, CRF 16, `fps`).
// `trimFirst` drops that many leading frames (the VACE reference latent when sd-cli keeps it).
export function mp4Args(src, dst, fps, { trimFirst = 0 } = {}) {
  const vf = trimFirst > 0 ? ["-vf", `trim=start_frame=${trimFirst},setpts=PTS-STARTPTS`] : [];
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", src, ...vf,
    "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "16", "-r", String(fps), "-movflags", "+faststart", dst];
}

// partialPath: where a result is written before it is delivered: a hidden sibling of `dst` in the
// SAME directory (so the final rename is atomic and never crosses a drive), with dst's extension
// last (ffmpeg picks the container from it). The pid keeps two runners apart.
export function partialPath(dst) {
  const ext = extname(dst);
  return join(dirname(dst), `.${basename(dst, ext)}.${process.pid}.part${ext}`);
}

// registerCleanup: run `fn` on every exit path of the runner (the process 'exit' hook and the signal /
// parent-gone handlers, like the temp dirs). Returns the function that unregisters it.
export function registerCleanup(fn) {
  cleanups.add(fn);
  ensureExitHook();
  return () => cleanups.delete(fn);
}

// deliverFile: make `partial` the file at `dst`, atomically (rename). A result is only ever PUBLISHED
// by this, so a failed or killed run never leaves a half-written file at the delivery path and a
// re-run of the same request never overwrites a good clip with a partial one.
export function deliverFile(partial, dst) {
  renameSync(partial, dst);
}

// encodeMp4: `timeoutMs` (0 = none) bounds the encode, so the runner's deadline covers it too. The
// encode writes to a partial file beside `dst`; `opts.verify(partialPath)` (optional) may throw to
// reject it (the black / frozen clip gate); only then is it renamed onto `dst`. Any failure, a
// rejection or a deadline kill removes the partial and leaves whatever was at `dst` untouched.
export function encodeMp4(ffmpeg, src, dst, fps, timeoutMs = 0, opts = {}) {
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const { verify, ...mp4Opts } = opts;
  const partial = partialPath(dst);
  const rm = () => { try { rmSync(partial, { force: true }); } catch { /* best effort */ } };
  const unregister = registerCleanup(rm);
  try {
    const r = spawnSync(ffmpeg, mp4Args(src, partial, fps, mp4Opts), {
      encoding: "utf8", ...(timeoutMs > 0 ? { timeout: timeoutMs, killSignal: "SIGKILL" } : {}),
    });
    if (r.error || r.status !== 0 || !existsSync(partial)) {
      const timedOut = r.error && r.error.code === "ETIMEDOUT";
      throw new Error((timedOut ? "ffmpeg mp4 encode timeout (killed): " : "ffmpeg mp4 encode failed: ") + (r.error ? r.error.message : String(r.stderr || "").trim().slice(-300)));
    }
    if (verify) verify(partial);
    deliverFile(partial, dst);
  } catch (e) {
    rm();
    throw e;
  } finally {
    unregister();
  }
}

const OWNER_FILE = ".igpu-owner";

// sweepStaleTempDirs: remove the leftovers of runners that died without cleaning up (SIGKILL,
// power loss): directories in `base` named `prefix*` whose owner marker names a process that no
// longer exists. A directory with no marker, or whose owner is alive (another job, this one), is
// left alone. Returns the directories removed. Best effort, never throws.
export function sweepStaleTempDirs(prefix, base = tmpdir()) {
  const removed = [];
  try {
    for (const name of readdirSync(base)) {
      if (!name.startsWith(prefix)) continue;
      const dir = join(base, name);
      let owner;
      try { owner = Number(readFileSync(join(dir, OWNER_FILE), "utf8").trim()); } catch { continue; }
      if (!Number.isInteger(owner) || owner <= 0 || owner === process.pid || pidAlive(owner)) continue;
      try { rmSync(dir, { recursive: true, force: true }); removed.push(dir); } catch { /* best effort */ }
    }
  } catch { /* the base dir may not be listable */ }
  return removed;
}

// makeTempDir: a private work dir under the OS temp dir, removed by cleanup() on every
// exit path: the caller's finally, the process 'exit' hook, and installLifecycle's signal /
// parent-gone handlers (which run every registered cleanup), so a killed job leaves no
// frames behind.
export function makeTempDir(prefix) {
  sweepStaleTempDirs(prefix);
  const dir = mkdtempSync(join(tmpdir(), prefix));
  // the owner marker lets a LATER runner tell this dir from a dead one's (a runner that was SIGKILLed
  // cannot clean up after itself)
  try { writeFileSync(join(dir, OWNER_FILE), String(process.pid)); } catch { /* best effort */ }
  let done = false;
  const cleanup = () => {
    if (done) return;
    done = true;
    cleanups.delete(cleanup);
    try { rmSync(dir, { recursive: true, force: true }); } catch { /* best effort */ }
  };
  cleanups.add(cleanup);
  ensureExitHook();
  return { dir, cleanup };
}
