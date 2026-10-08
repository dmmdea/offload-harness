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
import { existsSync, mkdirSync, mkdtempSync, rmSync, accessSync, constants as fsConstants } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join, resolve } from "node:path";

export const CPU_PLACEMENT = "CPU_PLACEMENT";
export const CPU_BACKEND_REFUSED = "CPU_BACKEND_REFUSED";
export const GPU_RESET = "GPU_RESET";
export const TOKEN_CAP_EXCEEDED = "TOKEN_CAP_EXCEEDED";
export const EXTRA_ARGS_REFUSED = "EXTRA_ARGS_REFUSED";
export const ILLEGAL_INSTRUCTION = "ILLEGAL_INSTRUCTION";
export const BINARY_NOT_ABSOLUTE = "BINARY_NOT_ABSOLUTE";
export const OUT_DIR_UNWRITABLE = "OUT_DIR_UNWRITABLE";
export const MODEL_INCOMPATIBLE = "MODEL_INCOMPATIBLE";

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
//   sdcpp    (stable-diffusion.cpp 3f8527a)  PASS needs ALL of
//              - "ggml_vulkan: <n> = <device> (...)" for a NON-software device,
//              - a "<module> compute buffer size: ... on Vulkan<N>" line for a module that is
//                not an auxiliary one (text encoder / VAE / TAE): the diffusion stage,
//              - and no "compute buffer size ... on CPU" line.
//            sd.cpp prints the backend via ggml_backend_name() (compute: "CPU" for the CPU
//            backend, "Vulkan0" for the first Vulkan device) and the params buffer type via
//            ggml_backend_buft_name() ("CPU", "Vulkan0", or "Vulkan_Host" for pinned host
//            memory). Params resting in RAM with compute on the GPU is the sanctioned
//            overflow, so a params line "on CPU" / "on Vulkan_Host" is neither evidence nor a
//            placement.
//   da3      (depth-anything.cpp da3-cli)    PASS needs "[da3] da::Backend using device: Vulkan<N>".
//            "offload_weights: ... (N host-only tensors kept on CPU ...)" is a storage line.
//   audiocpp (audio.cpp audiocpp_cli)        PASS needs a "<component>.weights.buffer_name
//            Vulkan<N>" line, and ANY "<component>.weights.buffer_name CPU" line is a placement
//            (upstream loads a second host copy of the ACE-Step planner for the prompt prefill).
//
// Never scanned for anything: ggml's "loaded CPU backend" registration, "Initializing
// backend: CPU", the SDCliParams / SDContextParams / SDGenerationParams dump blocks, the
// tokenizer echo lines ("split prompt ..." / "parse '...'"), and any line that contains the
// request's prompt, negative prompt, TTS text or lyrics verbatim.
export const GUARD_ENGINES = ["sdcpp", "da3", "audiocpp"];

const SOFTWARE_DEVICE = /\b(?:llvmpipe|lavapipe|swiftshader)\b/i;
const BLOCK_START = /\b(?:SDCliParams|SDContextParams|SDGenerationParams)\s*\{\s*$/;
const LOG_PREFIX = /^\[(?:VERBOSE|INFO|WARN|WARNING|ERROR|DEBUG|TRACE)\s*\]/;
const TOKENIZER_ECHO = [/\bsplit prompt\s+"/, /(?:^|\s)parse\s+'/];
const GPU_RESET_RE = /ErrorDeviceLost|device lost|context is lost/i;
const GGML_DEVICE_LINE = /^\s*ggml_vulkan:\s*(\d+)\s*=\s*(.+?)\s*(?:\||$)/;
const NO_VULKAN_DEVICE = /ggml_vulkan:\s*Found\s+0\s+Vulkan\s+devices|ggml_vulkan:\s*No\s+devices\s+found/i;
// stable-diffusion.cpp: a compute buffer on a backend, and the modules that are not the
// diffusion stage.
const SD_COMPUTE_BUFFER = /^(?:.*?\s-\s)?(.+?)\s+compute buffer size:.*?\bon\s+(\S+)/;
const SD_AUX_MODULE = /(?:^|[\s_.-])(?:t5\w*|umt5\w*|clip\w*|llm\w*|\w*vae\w*|tae\w*|taehv|taesd|esrgan|text_?enc\w*|conditioner|control\w*|vision\w*)(?:[\s_.-]|$)/i;
const SD_CPU_SHAPES = [
  /\bloading CPU backend\b/i, // [WARN] ggml_extend_backend.cpp: the actual no-GPU fallback
  /\bUsing CPU backend\b/i, // LOG_VERBOSE when the default compute backend is the CPU
  /\bNo devices found!/i,
  /->\s*compute\s+cpu\d*\b/i, // auto-fit plan: "... -> compute CPU, params RAM"
  /auto-fit:\s*no GPU memory budget available;\s*using CPU/i,
];
const DA3_BACKEND = /da::Backend using device:\s*(\S+)/i;
const DA3_CPU_SHAPES = [/offload_weights:.*->\s*CPU\b/i, /node\(s\) run on CPU\b/i];
const AUDIO_BUFFER = /\.weights\.buffer_name\s+(\S+)/;

function stripAnsi(s) {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");
}

// buildEchoMatcher: a predicate for log lines that merely repeat user text. A line drops when
// it contains an echoed string verbatim (each line of a multi-line string counts too). A
// string shorter than 6 characters is too likely to occur inside a real log line, so it only
// drops a line that IS that string, or that holds it in quotes.
function buildEchoMatcher(echoes) {
  const full = [];
  const short = [];
  for (const raw of echoes || []) {
    if (typeof raw !== "string" || raw.trim() === "") continue;
    const pieces = new Set([raw.trim()]);
    for (const part of raw.split(/\r\n|\r|\n/)) if (part.trim() !== "") pieces.add(part.trim());
    for (const p of pieces) (p.length >= 6 ? full : short).push(p);
  }
  return (line) => {
    const t = line.trim();
    if (full.some((p) => line.includes(p))) return true;
    return short.some((p) => t === p || line.includes(`"${p}"`) || line.includes(`'${p}'`));
  };
}

// createLogGuard: a stateful line scanner. scan(line) returns null, or
// {kind: "CPU_PLACEMENT" | "GPU_RESET", line, lineNo} for a line that must abort the run now;
// verdict() is called when the engine exits and says whether the positive evidence was seen.
export function createLogGuard({ engine, echoes = [] }) {
  if (!GUARD_ENGINES.includes(engine)) throw new Error(`createLogGuard: unknown engine ${JSON.stringify(engine)} (want ${GUARD_ENGINES.join(", ")})`);
  const echoed = buildEchoMatcher(echoes);
  const st = { lineNo: 0, inBlock: false, device: false, diffusion: false, params: false, da3: false, audio: false };
  const evidence = [];
  const note = (s) => { if (evidence.length < 20) evidence.push(s); };

  const cpu = (line) => ({ kind: CPU_PLACEMENT, line: line.trim(), lineNo: st.lineNo });

  function scan(raw) {
    st.lineNo++;
    const line = stripAnsi(String(raw ?? ""));
    if (st.inBlock) {
      if (/^\}\s*$/.test(line)) { st.inBlock = false; return null; }
      if (!LOG_PREFIX.test(line)) return null; // an indented dump line: never scanned
      st.inBlock = false; // an unterminated dump: the next log record ends it
    }
    if (BLOCK_START.test(line)) { st.inBlock = true; return null; }
    if (TOKENIZER_ECHO.some((re) => re.test(line))) return null;
    if (echoed(line)) return null;

    if (GPU_RESET_RE.test(line)) return { kind: GPU_RESET, line: line.trim(), lineNo: st.lineNo };

    if (NO_VULKAN_DEVICE.test(line)) return cpu(line);
    if (/\bggml_vulkan\b/i.test(line) && SOFTWARE_DEVICE.test(line)) return cpu(line);
    const dev = GGML_DEVICE_LINE.exec(line);
    if (dev) {
      st.device = true;
      note(`device ${dev[1]} = ${dev[2]}`);
      return null;
    }

    if (engine === "sdcpp") {
      if (SD_CPU_SHAPES.some((re) => re.test(line))) return cpu(line);
      const cb = SD_COMPUTE_BUFFER.exec(line);
      if (cb) {
        const [, desc, on] = cb;
        if (/^CPU\d*$/i.test(on)) return cpu(line);
        if (/^Vulkan\d+$/i.test(on)) {
          st.params = true;
          if (!SD_AUX_MODULE.test(desc)) {
            st.diffusion = true;
            note(`${desc} compute on ${on}`);
          }
        }
        return null;
      }
      if (/prepared params backend buffers\b.*\bon\s+Vulkan\d+\b/.test(line)) st.params = true;
    } else if (engine === "da3") {
      const b = DA3_BACKEND.exec(line);
      if (b) {
        if (/^Vulkan\d+$/i.test(b[1])) {
          st.da3 = true;
          note(`da3 backend ${b[1]}`);
          return null;
        }
        return cpu(line);
      }
      if (DA3_CPU_SHAPES.some((re) => re.test(line))) return cpu(line);
    } else {
      const m = AUDIO_BUFFER.exec(line);
      if (m) {
        if (/^CPU\d*$/i.test(m[1])) return cpu(line);
        if (/^Vulkan\d+$/i.test(m[1])) {
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
      expected = `a non-software "ggml_vulkan: <n> = <device>" line${st.device ? " (seen)" : " (missing)"} and a diffusion-stage "compute buffer size ... on Vulkan<N>" line${st.diffusion ? " (seen)" : " (missing)"}`;
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

export function gpuResetError(hit) {
  return new Error(`${GPU_RESET}: the GPU reset during the run (log line ${hit.lineNo}: ${hit.line}). The amdgpu driver resets the compute ring when one GPU dispatch runs past its default 2 s lockup timeout; keep the request inside the configured token cap (sdcpp_max_tokens, or animategen_sdcpp_max_tokens for animate) by lowering width/height/frames. This failure is never retried automatically.`);
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

// killTree: the whole process tree, not just the child. On Windows taskkill /T; elsewhere the
// engine is spawned detached (its own process group), so the group is killed. Best-effort,
// never throws.
export function killTree(child) {
  if (!child || child.pid === undefined || child.exitCode !== null || child.signalCode) return;
  try {
    if (process.platform === "win32") {
      spawnSync("taskkill", ["/PID", String(child.pid), "/T", "/F"], { stdio: "ignore" });
    } else {
      try { process.kill(-child.pid, "SIGKILL"); } catch { child.kill("SIGKILL"); }
    }
  } catch { /* nothing more to do */ }
}

function killLiveEngines() {
  for (const c of [...liveEngines]) killTree(c);
}

function runCleanups() {
  for (const fn of [...cleanups]) {
    try { fn(); } catch { /* best effort */ }
  }
}

function ensureExitHook() {
  if (exitHookInstalled) return;
  exitHookInstalled = true;
  process.on("exit", () => { killLiveEngines(); runCleanups(); });
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
// exit by the parent. Idempotent. `pollMs` is the parent-watch interval.
export function installLifecycle({ pollMs = Number(process.env.IGPU_PARENT_POLL_MS) || 1000 } = {}) {
  ensureExitHook();
  if (lifecycleInstalled) return;
  lifecycleInstalled = true;
  const die = (code, why) => {
    try { process.stderr.write(`igpu-engine: ${why}; killing the engine tree and cleaning up\n`); } catch { /* stderr may be gone */ }
    killLiveEngines();
    runCleanups();
    process.exit(code);
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
      detached: process.platform !== "win32",
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
      if (hit) abort(hit.kind === GPU_RESET ? gpuResetError(hit) : cpuPlacementError(hit));
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
        const hint = signal === "SIGKILL" ? " (SIGKILL on a UMA iGPU box usually means the kernel out-of-memory (OOM) killer)" : "";
        return finish(reject, new Error(`${label} was killed by signal ${signal}${hint}`));
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

// encodeMp4: `timeoutMs` (0 = none) bounds the encode, so the runner's deadline covers it too.
export function encodeMp4(ffmpeg, src, dst, fps, timeoutMs = 0, opts = {}) {
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const r = spawnSync(ffmpeg, mp4Args(src, dst, fps, opts), {
    encoding: "utf8", ...(timeoutMs > 0 ? { timeout: timeoutMs, killSignal: "SIGKILL" } : {}),
  });
  if (r.error || r.status !== 0 || !existsSync(dst)) {
    const timedOut = r.error && r.error.code === "ETIMEDOUT";
    throw new Error((timedOut ? "ffmpeg mp4 encode timeout (killed): " : "ffmpeg mp4 encode failed: ") + (r.error ? r.error.message : String(r.stderr || "").trim().slice(-300)));
  }
}

// makeTempDir: a private work dir under the OS temp dir, removed by cleanup() on every
// exit path: the caller's finally, the process 'exit' hook, and installLifecycle's signal /
// parent-gone handlers (which run every registered cleanup), so a killed job leaves no
// frames behind.
export function makeTempDir(prefix) {
  const dir = mkdtempSync(join(tmpdir(), prefix));
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
