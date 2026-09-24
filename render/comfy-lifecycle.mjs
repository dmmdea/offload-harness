// comfy-lifecycle.mjs — the shared, on-demand ComfyUI cold-start lifecycle. This was
// byte-identical across comfy-generate.mjs and comfy-video.mjs; centralized here so the
// cold-start + ~4-min ready-poll + zero-always-warm launch flags
// (--disable-smart-memory --cache-none --reserve-vram) live in ONE place. A `warm`
// batch session omits --cache-none so a checkpoint loads once for N renders (the
// caller still tears the session down at the batch boundary). tts.mjs does
// NOT use this (its Chatterbox worker is not ComfyUI; it passes comfyManaged:false to
// withGpuSlot). Dependency-free; deps are injectable purely for tests.
import { existsSync, createWriteStream, renameSync, rmSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { spawn as nodeSpawn, spawnSync as nodeSpawnSync } from "node:child_process";
import { readLaunchOwner, writeLaunchOwner, clearLaunchOwner, harnessLaunched, pidAlive as defaultPidAlive } from "./comfy-ownership.mjs";

// resolveComfyDir: the ComfyUI install this machine drives. The old default was
// "C:/ComfyUI" on EVERY platform, so a Linux node reported an install it cannot have and
// the fleet advertised the routes that drive it. Off Windows an unset COMFY_DIR is
// UNBOUND ("") — the honest answer, which the harness reports as NOT CONFIGURED instead
// of a path that will never exist.
export function resolveComfyDir(env = process.env) {
  return env.COMFY_DIR || (process.platform === "win32" ? "C:/ComfyUI" : "");
}

// resolveComfyPy: ComfyUI's deps live in its venv, not the system python. Probes BOTH
// platform families — the Windows candidates first, so Windows resolution is unchanged.
// Only Windows paths were probed before, so a Linux node fell through to a bare "python"
// that is either absent on Ubuntu or the system interpreter without torch: ComfyUI could
// not be launched by the harness at all. tts.mjs already probed both families; this is
// that pattern applied where it was missed.
export function resolveComfyPy(comfyDir = COMFY_DIR, env = process.env) {
  if (env.COMFY_PY) return env.COMFY_PY;
  const bare = process.platform === "win32" ? "python" : "python3";
  if (!comfyDir) return bare;
  return [".venv/Scripts/python.exe", "venv/Scripts/python.exe", "python_embeded/python.exe",
          ".venv/bin/python", "venv/bin/python"]
           .map((p) => join(comfyDir, p)).find((p) => existsSync(p))
    || bare;
}

export const COMFY_DIR = resolveComfyDir();
export const COMFY_PY = resolveComfyPy(COMFY_DIR);

// cudaVisibleEnv: ComfyUI >= 0.34 defaults WINDOWS to CUDA_VISIBLE_DEVICES=0 when the
// operator passed no device selection (upstream #15737 "Limit Windows multi-GPU
// visibility" + #15813) — silently hiding every card but the first. Our pooled
// multi-GPU tiers (DisTorch2 image/video seats) need them all: a graph naming
// cuda:1 as donor then fails prompt validation with "donor_device: 'cuda:1' not in
// ['cpu','cuda:0']" (measured on the blackwell-2x16 box, 2026-08-27). Restore full
// visibility for the CHILD process only, and only when the operator has not already
// scoped devices via CUDA_VISIBLE_DEVICES or a --cuda-device in COMFY_EXTRA_ARGS.
// Env-based (not --cuda-device all) so older ComfyUI versions, which take only an
// integer there, keep working. listGpus is injectable for tests.
export function cudaVisibleEnv(env = process.env, listGpus = defaultListGpus) {
  if (process.platform !== "win32") return env;
  if (env.CUDA_VISIBLE_DEVICES !== undefined) return env;
  if ((env.COMFY_EXTRA_ARGS || "").includes("--cuda-device")) return env;
  const n = listGpus();
  if (!(n > 1)) return env; // 0/1 GPU or no nvidia-smi: upstream default is fine
  const all = Array.from({ length: n }, (_, i) => i).join(",");
  return { ...env, CUDA_VISIBLE_DEVICES: all };
}

function defaultListGpus() {
  try {
    const r = nodeSpawnSync("nvidia-smi", ["-L"], { encoding: "utf8", timeout: 10000 });
    if (r.status !== 0 || !r.stdout) return 0;
    return (r.stdout.match(/GPU [0-9]+/g) || []).length;
  } catch {
    return 0;
  }
}

// comfyUp: is a ComfyUI HTTP server already answering on api?
export async function comfyUp(api = process.env.COMFY_API || "http://127.0.0.1:8188") {
  try { const r = await fetch(api + "/system_stats", { signal: AbortSignal.timeout(8000) }); return r.ok; } catch { return false; }
}

// --- launch profile (per-binding ComfyUI launch, plan D6) ---------------------------
//
// The Go harness threads a binding's launch profile through the child env:
//   COMFY_CUDA_DEVICE  — `--cuda-device <n>` (ComfyUI device order, comma list allowed).
//                        --cuda-device HIDES every other card (main.py rewrites
//                        CUDA_VISIBLE_DEVICES to exactly this list), so a single-card
//                        route cannot land on a card it was not given — notably the
//                        display card, which ComfyUI's fastest-first order makes cuda:0
//                        on a mixed box. Never --default-device: that only REORDERS the
//                        visible list and silently re-maps what every "cuda:N" pool key
//                        means.
//   COMFY_DYNAMIC_VRAM — "on" strips --disable-dynamic-vram from the extra args (a box
//                        whose user env disables it for DisTorch2 pooling can still
//                        stream a bf16 DiT larger than one card); "off" adds it; unset
//                        leaves COMFY_EXTRA_ARGS alone.
//   COMFY_EXTRA_ARGS   — verbatim extra flags (config comfy_extra_args, else the
//                        inherited env), as before.
// Unset profile = byte-identical launch to before.

/** Flags that turn ComfyUI's dynamic VRAM off (comfy/cli_args.py enables_dynamic_vram). */
export const DYNAMIC_VRAM_DISABLERS = Object.freeze(["--disable-dynamic-vram", "--highvram", "--gpu-only", "--novram", "--cpu"]);

/** resolveLaunchProfile reads the profile from env, refusing values ComfyUI would not take. */
export function resolveLaunchProfile(env = process.env) {
  const cudaDevice = String(env.COMFY_CUDA_DEVICE ?? "").replace(/\s+/g, "");
  if (cudaDevice && !/^\d+(,\d+)*$/.test(cudaDevice)) {
    throw new Error(`COMFY_CUDA_DEVICE must be a ComfyUI device index or a comma list of them (e.g. "1" or "1,2"), got '${env.COMFY_CUDA_DEVICE}'`);
  }
  const dynamicVram = String(env.COMFY_DYNAMIC_VRAM ?? "").trim().toLowerCase();
  if (dynamicVram && dynamicVram !== "on" && dynamicVram !== "off") {
    throw new Error(`COMFY_DYNAMIC_VRAM must be on, off or unset, got '${env.COMFY_DYNAMIC_VRAM}'`);
  }
  return { cudaDevice, dynamicVram };
}

const profileRequested = (p) => !!(p && (p.cudaDevice || p.dynamicVram));

/** Remove every `flag value` pair (and a `flag=value` form) from an argv list. */
function stripValued(args, flag) {
  const out = [];
  for (let i = 0; i < args.length; i++) {
    if (args[i] === flag) { i++; continue; }
    if (args[i].startsWith(flag + "=")) continue;
    out.push(args[i]);
  }
  return out;
}

/**
 * launchFlags is the exact argv tail ensureComfy spawns main.py with. Base flags
 * first, then the profile's --cuda-device, then the extra args LAST (the J4 seam's
 * contract: extra args are the tail). The profile's device wins over a --cuda-device
 * or --default-device the extra args carry — argparse takes the last occurrence, so
 * leaving them in would silently override the binding.
 */
export function launchFlags({ reserveVram = "1.0", warm = false, extraArgs = "", profile = { cudaDevice: "", dynamicVram: "" }, warn = () => {} } = {}) {
  const flags = ["--disable-smart-memory"];
  // warm: a BATCH session keeps ComfyUI's model cache ON so the checkpoint loads once
  // for N renders; the caller still tears the whole session down at the batch boundary
  // (zero-always-warm moves from per-render to per-batch). Default stays cache-none.
  if (!warm) flags.push("--cache-none");
  flags.push("--reserve-vram", String(reserveVram || "1.0"));
  // J4 seam: COMFY_EXTRA_ARGS appends verbatim (whitespace-split) launch flags —
  // the per-box escape hatch for non-CUDA backends (--directml, device pinning)
  // without touching shared code. Empty/unset = byte-identical launch.
  let extra = String(extraArgs || "").split(/\s+/).filter(Boolean);
  if (profile.cudaDevice) {
    extra = stripValued(stripValued(extra, "--cuda-device"), "--default-device");
    flags.push("--cuda-device", profile.cudaDevice);
  }
  if (profile.dynamicVram === "on") {
    extra = extra.filter((f) => f !== "--disable-dynamic-vram");
    const still = extra.filter((f) => DYNAMIC_VRAM_DISABLERS.includes(f));
    if (still.length) {
      warn(`COMFY-PROFILE-WARN: comfy_dynamic_vram=on but the extra args still carry ${still.join(" ")}, which also turns dynamic VRAM off`);
    }
  } else if (profile.dynamicVram === "off" && !extra.includes("--disable-dynamic-vram")) {
    extra.push("--disable-dynamic-vram");
  }
  flags.push(...extra);
  return flags;
}

/** argvFlagValue: the value of the LAST `flag v` / `flag=v` in an argv (argparse semantics). */
function argvFlagValue(argv, flag) {
  let v = null;
  for (let i = 0; i < argv.length; i++) {
    const a = String(argv[i]);
    if (a === flag && i + 1 < argv.length) v = String(argv[i + 1]);
    else if (a.startsWith(flag + "=")) v = a.slice(flag.length + 1);
  }
  return v;
}

/** argvDynamicVram: would a ComfyUI launched with this argv run dynamic VRAM (NVIDIA, torch >= 2.8)? */
export function argvDynamicVram(argv) {
  const a = argv.map(String);
  if (a.includes("--enable-dynamic-vram")) return true;
  return !a.some((f) => DYNAMIC_VRAM_DISABLERS.includes(f));
}

/**
 * profileMismatch explains why a running ComfyUI launched with `argv` cannot serve a
 * binding that needs `profile` ("" = it can). An unreadable argv cannot prove anything,
 * so with a profile requested it is a mismatch, never a pass.
 */
export function profileMismatch(argv, profile) {
  if (!profileRequested(profile)) return "";
  if (!Array.isArray(argv)) {
    return "the running ComfyUI does not report its launch argv (/system_stats system.argv), so it cannot be shown to honour this binding's launch profile";
  }
  const why = [];
  if (profile.cudaDevice) {
    const got = argvFlagValue(argv, "--cuda-device");
    if ((got || "").replace(/\s+/g, "") !== profile.cudaDevice) {
      why.push(`it runs with ${got ? "--cuda-device " + got : "no --cuda-device (every visible card, ComfyUI's default device first)"}; this binding needs --cuda-device ${profile.cudaDevice}`);
    }
  }
  if (profile.dynamicVram) {
    const on = argvDynamicVram(argv);
    if ((profile.dynamicVram === "on") !== on) {
      why.push(`it runs with dynamic VRAM ${on ? "on" : "off"}; this binding needs it ${profile.dynamicVram}`);
    }
  }
  return why.join("; ");
}

/** fetchSystemArgv: the running server's sys.argv from GET /system_stats, or null. */
export async function fetchSystemArgv(api) {
  try {
    const r = await fetch(api + "/system_stats", { signal: AbortSignal.timeout(8000) });
    if (!r.ok) return null;
    const j = await r.json();
    const argv = j?.system?.argv;
    return Array.isArray(argv) ? argv : null;
  } catch {
    return null;
  }
}

/**
 * reuseVerdict decides what to do with a ComfyUI that is ALREADY listening:
 *   { reuse: true }                  — no profile requested, or its argv honours it;
 *   { restart: true, pid, reason }   — the harness launched it (fingerprint match), its
 *                                      spawner is gone, and its profile is wrong: ours
 *                                      to replace;
 *   { reuse: false, reason }         — anything else: refuse (COMFY-PROFILE-MISMATCH).
 * A foreign instance is never killed — the harness cannot know what else it serves.
 */
export async function reuseVerdict({ api, comfyDir, profile, systemArgv = fetchSystemArgv, readLaunch = readLaunchOwner, alive = defaultPidAlive }) {
  if (!profileRequested(profile)) return { reuse: true };
  const argv = await systemArgv(api);
  const reason = profileMismatch(argv, profile);
  if (!reason) return { reuse: true };
  const marker = comfyDir ? readLaunch(comfyDir) : null;
  if (harnessLaunched(marker, argv, alive)) {
    if (typeof marker.ownerPid === "number" && marker.ownerPid !== process.pid && alive(marker.ownerPid)) {
      return { reuse: false, reason: `${reason} — it is harness-launched but still in use by process ${marker.ownerPid}` };
    }
    return { restart: true, pid: marker.pid, reason };
  }
  return { reuse: false, reason: `${reason} — it was not started by this harness, so it is not reused (stop it, or start it with the binding's flags)` };
}

// --- ComfyUI console capture (F-38 audit, 2026-09-24) ---------------------------
//
// `spawn(..., { stdio: "ignore" })` used to discard every line ComfyUI itself ever
// printed — memory-management decisions, node-level errors, the actual OS error
// behind a failed render. The Aorus disk-space defect ("[Errno 28] No space left
// on device") cost real diagnostic time because the ONLY signal available for a
// failed production render was the terse /history execution_error JSON; the real
// error was found only by building a stdout-capturing bypass copy of render/ by
// hand and re-running the exact same graph outside the harness.
//
// This captures the harness's OWN ComfyUI launches (never a reused foreign
// instance — see ensureComfy's reuse branch, which never calls this) to a single
// rotating, size-bounded log file beside the ComfyUI install, and withGpuSlot
// (gpu-lock.mjs) appends its last ~20 lines to a render failure's error message.

const COMFY_LOG_CAP_BYTES = 5 * 1024 * 1024; // per run — generous for console text, never unbounded
const COMFY_LOG_KEEP = 3; // previous runs kept as .1..3 (logrotate-style) beside the current file
export const COMFY_LOG_TAIL_LINES = 20;

/** comfyLogPath: this run's ComfyUI console capture, beside the install (matches the
 * .offload-owned.json / .offload-launch.json convention in comfy-ownership.mjs). */
export function comfyLogPath(comfyDir = COMFY_DIR) {
  return join(comfyDir, "offload-comfyui.log");
}

/** rotateComfyLog: age out old numbered logs and free the current filename for the
 * new run, so a long-lived box never accumulates unbounded ComfyUI console history.
 * Best-effort: a rotation failure (e.g. a concurrent reader holding the file open on
 * Windows) must never block a render. */
export function rotateComfyLog(comfyDir = COMFY_DIR) {
  const base = comfyLogPath(comfyDir);
  try {
    const oldest = `${base}.${COMFY_LOG_KEEP}`;
    if (existsSync(oldest)) rmSync(oldest, { force: true });
    for (let i = COMFY_LOG_KEEP - 1; i >= 1; i--) {
      const from = `${base}.${i}`;
      if (existsSync(from)) renameSync(from, `${base}.${i + 1}`);
    }
    if (existsSync(base)) renameSync(base, `${base}.1`);
  } catch {}
}

/** tailComfyLog: the last `n` lines of THIS run's ComfyUI console capture, or ""
 * when there is none (no harness-managed launch this run, or nothing captured yet).
 * Synchronous — only ever called once, on a render failure, never on the happy path. */
export function tailComfyLog(comfyDir = COMFY_DIR, n = COMFY_LOG_TAIL_LINES) {
  try {
    const text = readFileSync(comfyLogPath(comfyDir), "utf8");
    const lines = text.split(/\r?\n/);
    if (lines.length && lines[lines.length - 1] === "") lines.pop();
    return lines.slice(-n).join("\n");
  } catch {
    return "";
  }
}

/** captureComfyOutput: pipes a just-spawned ComfyUI child's stdout+stderr into the
 * rotating log file, capped at COMFY_LOG_CAP_BYTES so a long batch session cannot
 * grow it without bound. Every step is best-effort and defensive against a fake
 * `spawn` (tests inject one that returns a bare `{ kill() {} }`, with no
 * stdout/stderr/once) — a logging failure must never take down, or even alter the
 * behaviour of, the render it exists to help diagnose. */
function captureComfyOutput(child, comfyDir) {
  if (!child) return;
  rotateComfyLog(comfyDir);
  let ws;
  try {
    ws = createWriteStream(comfyLogPath(comfyDir), { flags: "a" });
    // createWriteStream's own open() failure (e.g. a synthetic test comfyDir that
    // does not exist on disk) surfaces asynchronously as an "error" event, not a
    // thrown exception — an unhandled one would crash the whole process. Swallow
    // it: logging is diagnostic best-effort, never load-bearing for the render.
    ws.on("error", () => {});
  } catch { return; }
  let bytes = 0, capped = false;
  const onData = (chunk) => {
    if (capped) return;
    bytes += chunk.length;
    if (bytes > COMFY_LOG_CAP_BYTES) {
      capped = true;
      try { ws.write("\n... [offload-comfyui.log capped at 5MB for this run] ...\n"); } catch {}
      return;
    }
    try { ws.write(chunk); } catch {}
  };
  // child.stdout/stderr are Readables and can themselves emit "error" (e.g. EPIPE
  // when withGpuSlot's teardown kills the child mid-write) — same class of
  // unhandled-event crash as the write stream's own "error" above, on the OTHER
  // end of the pipe. An unhandled one here would crash the whole render process,
  // which is exactly the invariant this function exists to never violate.
  const onStreamError = () => {};
  try { child.stdout?.on?.("data", onData); child.stdout?.on?.("error", onStreamError); } catch {}
  try { child.stderr?.on?.("data", onData); child.stderr?.on?.("error", onStreamError); } catch {}
  const close = () => { try { ws.end(); } catch {} };
  try { child.once?.("exit", close); } catch {}
  try { child.once?.("error", close); } catch {}
}

// ensureComfy: if ComfyUI is already up, reuse it — unless this binding carries a launch
// profile the running instance contradicts (then: restart it when the harness launched
// it and nobody holds it, otherwise refuse with a COMFY-PROFILE-MISMATCH line; never
// render a single-card binding on whatever card a foreign instance happens to default
// to). Otherwise launch it on-demand with the zero-always-warm flags and poll until
// ready, returning the spawned child so the caller can kill it.
// --reserve-vram holds VRAM back for the Windows display/WDDM; 1.0 leaves the most for
// the GGUF model on 8GB — it is PER-WORKFLOW-OVERRIDABLE (invariant 5: raise to 1.5-2.0
// for Wan; ACE-Step differs). Deps (comfyUp/spawn/timing/profile readers) are injectable
// for tests only; production calls use the real defaults.
export async function ensureComfy(opts = {}) {
  const {
    api = process.env.COMFY_API || "http://127.0.0.1:8188",
    comfyDir = COMFY_DIR,
    py = COMFY_PY,
    reserveVram = "1.0",
    warm = false,
    comfyUp: up = comfyUp,
    spawn = nodeSpawn,
    envFor = cudaVisibleEnv,
    env = process.env,
    systemArgv = fetchSystemArgv,
    readLaunch = readLaunchOwner,
    writeLaunch = writeLaunchOwner,
    clearLaunch = clearLaunchOwner,
    alive = defaultPidAlive,
    killPid = (pid) => process.kill(pid),
    log = (line) => console.error(line),
    pollMs = 2000,
    // Startup budget: a laptop cold start (custom nodes + models on a slow disk)
    // legitimately exceeds the old hardcoded ~4 min. Default 10 min, env-tunable
    // (COMFY_START_WAIT_SEC) — same pattern as the render polls' COMFY_WAIT_SEC.
    maxPolls = Math.max(1, Math.ceil(Number(process.env.COMFY_START_WAIT_SEC || 600) * 1000 / 2000)),
    stopPolls = 15,
  } = opts;
  const profile = resolveLaunchProfile(env);
  if (await up(api)) {
    const v = await reuseVerdict({ api, comfyDir, profile, systemArgv, readLaunch, alive });
    if (v.reuse) return null; // already running and fit for this binding — don't manage it
    if (!v.restart) {
      const line = "COMFY-PROFILE-MISMATCH: ComfyUI on " + api + ": " + v.reason;
      log(line);
      throw new Error(line);
    }
    // Ours, orphaned (a --keep-comfy session or a crashed teardown), wrong profile:
    // replace it rather than render on the wrong card.
    log(`COMFY-PROFILE-RESTART: stopping harness-launched ComfyUI pid ${v.pid} on ${api} (${v.reason})`);
    try { killPid(v.pid); } catch {}
    let down = false;
    for (let i = 0; i < stopPolls; i++) {
      await new Promise((r) => setTimeout(r, pollMs));
      if (!(await up(api))) { down = true; break; }
    }
    if (!down) {
      const line = `COMFY-PROFILE-MISMATCH: ComfyUI on ${api} (harness-launched pid ${v.pid}) did not stop after a kill: ${v.reason}`;
      log(line);
      throw new Error(line);
    }
    try { clearLaunch(comfyDir); } catch {}
  }
  // An unbound COMFY_DIR must fail with its reason, not with a bad cwd from spawn(): on a
  // machine with no ComfyUI binding the caller's defer should say WHY.
  if (!comfyDir) {
    throw new Error("COMFY_DIR is not set — this machine has no ComfyUI install bound (set comfy_dir in the harness config)");
  }
  const flags = launchFlags({ reserveVram, warm, extraArgs: env.COMFY_EXTRA_ARGS, profile, warn: log });
  // A --cuda-device launch has already scoped the cards (main.py rewrites
  // CUDA_VISIBLE_DEVICES to it), so the multi-GPU visibility restore below does not
  // apply — and neither does its --disable-pinned-memory: one visible card is not the
  // multi-device pinned-transfer case, and pinned memory is what keeps a streamed
  // (dynamic VRAM) bf16 DiT fast.
  const spawnEnv = flags.includes("--cuda-device") ? env : envFor();
  // Upstream's own guidance for the multi-GPU Windows path it now hides by default:
  // "pass --cuda-device all --disable-pinned-memory" — pinned memory with multiple
  // visible devices risks CUDA host-transfer failures on Windows (#15737). We restore
  // visibility via env, so we also carry the second half of that guidance.
  if (spawnEnv !== process.env && String(spawnEnv.CUDA_VISIBLE_DEVICES || "").includes(",")
      && !flags.includes("--disable-pinned-memory")) {
    flags.push("--disable-pinned-memory");
  }
  const child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: ["ignore", "pipe", "pipe"], detached: false, env: spawnEnv });
  captureComfyOutput(child, comfyDir);
  // Record the launch so a later job can tell this instance from a foreign one (the
  // fingerprint is pid + this exact argv). Best-effort: without it, a later profile
  // mismatch refuses instead of restarting — the safe direction.
  if (child && typeof child.pid === "number") {
    try { writeLaunch(comfyDir, { pid: child.pid, ownerPid: process.pid, args: ["main.py", ...flags], profile }); } catch {}
  }
  for (let i = 0; i < maxPolls; i++) {
    await new Promise((r) => setTimeout(r, pollMs));
    if (await up(api)) return child;
  }
  try { child.kill(); } catch {}
  throw new Error("ComfyUI did not become ready on " + api + " after ~" + Math.round(maxPolls * pollMs / 60000) + "min (COMFY_START_WAIT_SEC to extend)");
}
