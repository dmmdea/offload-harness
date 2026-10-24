// comfy-lifecycle.mjs — the shared, on-demand ComfyUI cold-start lifecycle. This was
// byte-identical across comfy-generate.mjs and comfy-video.mjs; centralized here so the
// cold-start + ~4-min ready-poll + zero-always-warm launch flags
// (--disable-smart-memory --cache-none --reserve-vram) live in ONE place. A `warm`
// batch session omits --cache-none so a checkpoint loads once for N renders (the
// caller still tears the session down at the batch boundary). tts.mjs does
// NOT use this (its Chatterbox worker is not ComfyUI; it passes comfyManaged:false to
// withGpuSlot). Dependency-free; deps are injectable purely for tests.
import { existsSync, createWriteStream, renameSync, rmSync, readFileSync, mkdirSync, openSync, closeSync, readSync, fstatSync, statSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn as nodeSpawn, spawnSync as nodeSpawnSync } from "node:child_process";
import { readLaunchOwner, writeLaunchOwner, restampLaunchOwner, clearLaunchOwner, harnessLaunched, pidAlive as defaultPidAlive } from "./comfy-ownership.mjs";

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

// --- per-card instances (plan P13a) ---------------------------------------------------
//
// One ComfyUI per card lets two media jobs run on two cards at once. The DEFAULT instance
// (key "") is today's single ComfyUI, left byte-identical: same argv, same launch marker,
// same console log, same spawn env, no bind check. An instance is keyed ONLY by an explicit
// COMFY_INSTANCE or by the card it is bound to (COMFY_CARD_UUID); a port on its own never
// makes one. A keyed instance owns a port (given by --api / COMFY_API, else the base
// COMFY_PORT_BASE + COMFY_INSTANCE_INDEX), its own output and temp directories and its own sqlite database (one folder
// per instance, <comfyDir>/instances/<key>, see instancePaths), its own launch marker and
// console log, and, when bound to a card, a pin by GPU uuid. The user directory stays shared:
// only the database is process-exclusive (see instancePaths).
//
// WHY A UUID AND NEVER AN INDEX. --cuda-device N counts in CUDA's default fastest-first
// order, not the driver's, so on a box with a display card index 0 can be that card, and
// any index taken from the driver or from a lease lands on the wrong one. CUDA_VISIBLE_DEVICES
// accepts the card's uuid, which names exactly one card in every ordering (measured on the
// 3-card tier: the process sees one device, of that uuid's model). So the child env carries
// the uuid and the argv carries NO --cuda-device (main.py would rewrite the env from it).

export const DEFAULT_COMFY_PORT = 8188;
export const DEFAULT_COMFY_API = "http://127.0.0.1:8188";
/** First port handed to a keyed instance that was not given an api; the next ones follow by index. */
export const COMFY_PORT_BASE = 8189;

const INSTANCE_KEY_RE = /^[A-Za-z0-9_-]{1,48}$/;
const GPU_UUID_RE = /^GPU-[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
const instanceError = (msg) => new Error("COMFY-INSTANCE-INVALID: " + msg);

/** parseCardUuid: COMFY_CARD_UUID, validated ("" = no card). An index is not an identity. */
function parseCardUuid(env) {
  const v = String(env.COMFY_CARD_UUID ?? "").trim();
  if (v && !GPU_UUID_RE.test(v)) throw instanceError("COMFY_CARD_UUID must be a full GPU uuid as the driver prints it (GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx), not an index");
  return v;
}

/** cardInstanceKey: the stable instance key of a card, from the head of its uuid. */
export function cardInstanceKey(uuid) {
  return "g" + String(uuid).slice(4, 12).toLowerCase();
}

function intEnv(env, name, dflt, lo, hi) {
  const raw = String(env[name] ?? "").trim();
  if (!raw) return dflt;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < lo || n > hi) throw instanceError(`${name} must be an integer from ${lo} to ${hi}, got '${raw}'`);
  return n;
}

/**
 * resolveInstance: which ComfyUI this run drives. `api` is the --api flag (or undefined);
 * the env supplies COMFY_API, COMFY_INSTANCE, COMFY_CARD_UUID, COMFY_PORT_BASE and
 * COMFY_INSTANCE_INDEX. Returns { key, port, api, cardUuid }. With neither an
 * explicit key nor a card the key is "" and `api` is the caller's string VERBATIM (or the
 * default), exactly as the runners resolved it before instances existed.
 */
export function resolveInstance({ api, env = process.env } = {}) {
  const given = api || env.COMFY_API || "";
  const explicit = String(env.COMFY_INSTANCE ?? "").trim();
  if (explicit && !INSTANCE_KEY_RE.test(explicit)) {
    throw instanceError(`COMFY_INSTANCE must be 1 to 48 letters, digits, '-' or '_', got '${explicit}'`);
  }
  const cardUuid = parseCardUuid(env);
  let port = null;
  try { const p = new URL(given || DEFAULT_COMFY_API).port; if (p) port = Number(p); } catch {}
  if (!explicit && !cardUuid) {
    const effective = given || DEFAULT_COMFY_API;
    return { key: "", port: port ?? DEFAULT_COMFY_PORT, api: effective, cardUuid: "" };
  }
  const key = explicit || cardInstanceKey(cardUuid);
  if (given) {
    if (port === null || port === DEFAULT_COMFY_PORT) {
      throw instanceError(`instance '${key}' cannot use the default port ${DEFAULT_COMFY_PORT}, which belongs to the unkeyed instance (give it its own --api / COMFY_API port)`);
    }
    return { key, port, api: given, cardUuid };
  }
  const base = intEnv(env, "COMFY_PORT_BASE", COMFY_PORT_BASE, 1024, 65535);
  const idx = intEnv(env, "COMFY_INSTANCE_INDEX", 0, 0, 63);
  if (base + idx > 65535) throw instanceError(`COMFY_PORT_BASE ${base} + COMFY_INSTANCE_INDEX ${idx} is past the last port`);
  const u = new URL(DEFAULT_COMFY_API);
  u.port = String(base + idx);
  return { key, port: base + idx, api: u.origin, cardUuid };
}

/** comfyApi: the api a runner talks to, from its --api flag and the instance env. */
export function comfyApi(flagApi, env = process.env) {
  return resolveInstance({ api: flagApi, env }).api;
}

/**
 * instancePaths: a keyed instance's own directories (input stays shared), all under one
 * folder, <comfyDir>/instances/<key>. `tempBase` is the value --temp-directory takes; ComfyUI
 * appends "/temp" to it itself (main.py), so `tempDir` is the directory it really uses. They
 * are deliberately NOT under <comfyDir>/temp or <comfyDir>/output: the default instance wipes
 * <comfyDir>/temp on every start (cleanup_temp_filesystem), which would delete the temp files
 * of a keyed instance running at the time.
 *
 * `dbPath` / `dbUrl` are the instance's own sqlite database, the value of ComfyUI's
 * `--database-url` (comfy/cli_args.py: type=str, default=None). Without the flag ComfyUI uses
 * `comfyui.db` in the effective user directory, one file for every instance on the box, and
 * app/database/db.py holds a process-exclusive file lock on `<db>.lock` for the life of the
 * process: a second instance waits 5 s, logs "Database is locked. Another ComfyUI process is
 * already using this database." and runs on without a database (with --enable-assets it exits
 * instead). The file sits in the instance's own folder, beside `output` and `temp` and inside
 * neither, so a temp wipe or an output scan never touches it. The URL is "sqlite:///" plus the
 * path, the form ComfyUI builds for its own default (get_database_url) and splits again with
 * get_db_path; `comfyDir` is absolute, as everywhere else here.
 */
export function instancePaths(comfyDir, key) {
  const root = join(comfyDir, "instances", key);
  const dbPath = join(root, "comfyui.db");
  return { outputDir: join(root, "output"), tempBase: root, tempDir: join(root, "temp"), dbPath, dbUrl: "sqlite:///" + dbPath };
}

/**
 * bindHosts: the addresses ComfyUI will listen on, which the bind check must probe. It
 * listens on 127.0.0.1 unless its --listen flag (read here from the extra args, last
 * occurrence wins as in argparse) says otherwise: a bare --listen is every interface, a
 * comma list is each address in it. The host the runner spells in its api is where a client
 * reaches ComfyUI, not where ComfyUI binds ("localhost" can resolve to ::1 first).
 */
export function bindHosts(extraArgs = "") {
  const toks = String(extraArgs || "").split(/\s+/).filter(Boolean);
  let value = null;
  for (let i = 0; i < toks.length; i++) {
    if (toks[i] === "--listen") value = toks[i + 1] !== undefined && !toks[i + 1].startsWith("-") ? toks[i + 1] : "";
    else if (toks[i].startsWith("--listen=")) value = toks[i].slice("--listen=".length);
  }
  if (value === null) return ["127.0.0.1"];
  const hosts = value.split(",").map((h) => h.trim()).filter(Boolean);
  return hosts.length ? hosts : ["0.0.0.0", "::"];
}

/**
 * portIsFree: can this process listen on host:port? A false answer means something else
 * holds it (or the OS refuses us), and either way the launch must not go ahead. It only
 * ever binds and releases; it never talks to, signals or kills the holder.
 */
export function portIsFree(port, host) {
  return new Promise((resolve) => {
    const srv = createServer();
    srv.once("error", () => resolve(false));
    srv.listen({ port, host }, () => srv.close(() => resolve(true)));
  });
}

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

// The listing's deadlines. A card listing is a tenth of a second on a quiet box and has run out ten
// seconds under load; one that did is asked ONCE more under the longer timeout, because "no listing"
// leaves ComfyUI on the Windows default (card 0 only) and a pooled graph then fails validation on a
// box that has the cards: one slow read must not change a launch (F24, the Go side's same rule).
export const LIST_GPUS_TIMEOUT_MS = 10000;
export const LIST_GPUS_RETRY_TIMEOUT_MS = 30000;

// listGpusWith counts the cards `nvidia-smi -L` lists (0 = no usable listing). Only a listing that
// TIMED OUT is retried: a failure that came back at once fails the same way again. A listing that
// ran out of time is never silent, because "0" then leaves ComfyUI on card 0 only and the failure
// shows up much later, as a graph that fails validation for a box that has the cards: the retry is
// said, and so is the second timeout together with what the launch does instead. spawnSync and warn
// are injectable for tests.
export function listGpusWith(spawnSync = nodeSpawnSync, warn = (line) => console.error(line)) {
  const ask = (timeout) => {
    try {
      return spawnSync("nvidia-smi", ["-L"], { encoding: "utf8", timeout, windowsHide: true });
    } catch {
      return null;
    }
  };
  const ranOut = (r) => Boolean(r && r.error && r.error.code === "ETIMEDOUT");
  let r = ask(LIST_GPUS_TIMEOUT_MS);
  if (ranOut(r)) {
    warn(`COMFY-GPU-LIST-WARN: nvidia-smi -L did not answer within ${LIST_GPUS_TIMEOUT_MS / 1000} s; asking once more under ${LIST_GPUS_RETRY_TIMEOUT_MS / 1000} s`);
    r = ask(LIST_GPUS_RETRY_TIMEOUT_MS);
    if (ranOut(r)) {
      warn(`COMFY-GPU-LIST-WARN: nvidia-smi -L did not answer within ${LIST_GPUS_RETRY_TIMEOUT_MS / 1000} s either, so the card count is unknown and ComfyUI keeps the Windows default of card 0 only: a graph that names another card (a pool's donor) will fail validation. Setting CUDA_VISIBLE_DEVICES skips the listing`);
    }
  }
  if (!r || r.status !== 0 || !r.stdout) return 0;
  return (r.stdout.match(/GPU [0-9]+/g) || []).length;
}

function defaultListGpus() {
  return listGpusWith();
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
  const cardUuid = parseCardUuid(env);
  // A keyed instance (an explicit COMFY_INSTANCE, or a card in COMFY_CARD_UUID) is pinned by
  // uuid or not at all: an index counts in CUDA's fastest-first order, not the driver's, so on
  // a box with a display card it lands on a different card than the one a lease or a uuid names.
  const explicitKey = String(env.COMFY_INSTANCE ?? "").trim();
  if (cudaDevice && (cardUuid || explicitKey)) {
    throw new Error(`COMFY-INSTANCE-CONFLICT: COMFY_CUDA_DEVICE ('${cudaDevice}') cannot be combined with an instance key (COMFY_INSTANCE, or the card in COMFY_CARD_UUID): an index counts cards in a different order than a uuid or a lease does, so a keyed instance never takes one (its pin is the card uuid): unset COMFY_CUDA_DEVICE`);
  }
  // cardUuid joins the profile only when set, so an unbound profile keeps today's exact shape.
  return { cudaDevice, dynamicVram, ...(cardUuid ? { cardUuid } : {}) };
}

const profileRequested = (p) => !!(p && (p.cudaDevice || p.dynamicVram || p.cardUuid || p.multicard));

/**
 * isSageAttentionToken: is this one launch-argument token sage attention? The exact flag, or an argparse prefix of it: ComfyUI's parser (argparse, abbreviations
 * allowed, `--use-sage-attention` one of a mutually exclusive attention group) accepts `--use-sa` as the flag, and /system_stats reports the argv as it was
 * typed. A prefix shorter than 8 characters begins other attention flags too (`--use-s` is also --use-split-cross-attention) and is not this one; a longer
 * token (`--use-sage-attention-extra`) is no prefix at all. The one predicate for the Wan sizing (wan-multicard.mjs wanAttentionMode), the reuse check of a
 * running ComfyUI (profileMismatch) and, by the same rows (render/testdata/sage-token-table.json, which both languages' tests read), internal/hostneed's sageAttention;
 * ComfyUI's parser is case-sensitive, so is this.
 */
export function isSageAttentionToken(tok) {
  const t = String(tok);
  return t.length >= 8 && "--use-sage-attention".startsWith(t);
}

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

// --- the engine-resident idle bound -----------------------------------------------------------------
//
// The house rule is that no idle model stays loaded. llama-swap keeps it with its own ttl; ComfyUI has no idle timer. The harness
// frees an instance at the end of every job (gpu-lock.mjs POSTs /free); the process itself is bounded by the pack's process bound
// (ADR 0097, the section after this one), or by the lease holder for a keyed instance kept without one. Under the harness's own launch (--disable-smart-memory, always the first flag) ComfyUI also unloads
// every model itself when a prompt ends, success or failure (execution.py, `if DISABLE_SMART_MEMORY: unload_all_models()`, ComfyUI
// 0.38.0), so a kept single-prompt instance (--cache-none) is already bare between jobs and the pack below finds nothing to free.
// What survives is the warm (--batch) session's executor cache, which keeps the loader outputs (the unloaded weights, in host RAM)
// until a free resets the executor, and a hand-started instance run without --disable-smart-memory, which keeps its models in VRAM.
// For those the bound lives INSIDE the engine, the llama-swap ttl analogue: the pack render/comfyui-ext/idle-nodes/offload_idle asks
// ComfyUI's own worker to unload the models and reset the executor cache after COMFY_IDLE_FREE_SEC (default 300) with an empty queue.
// Run live on 2026-10-14 (a keyed single-card instance on the 16 GB reference box: 230 MiB of the card at idle); docs/systems/
// media-generation.md has the record. The harness runs no watcher and never acts on another process. The same pack carries the
// process bound (COMFY_IDLE_EXIT_SEC, below), which ends the Python process and its CUDA context once nothing holds the instance.
//
// The names are defined here, not in wan-multicard.mjs, which imports this file (through gpu-lock.mjs too: a cycle); wan-multicard.mjs
// re-exports the yaml and the pack name next to EXT_YAML / EXT_NODE_PACK.
const IDLE_EXT_DIR = join(dirname(fileURLToPath(import.meta.url)), "comfyui-ext");
export const EXT_IDLE_YAML = join(IDLE_EXT_DIR, "extra_model_paths_idle.yaml");
export const EXT_IDLE_PACK = "offload_idle";
export const EXT_IDLE_PACK_FILE = join(IDLE_EXT_DIR, "idle-nodes", EXT_IDLE_PACK, "__init__.py");
export const IDLE_FREE_DEFAULT_SEC = 300;
/** The pack refuses less (a smaller bound would thrash reloads); the launcher judges COMFY_IDLE_FREE_SEC by the same rule. */
const IDLE_FREE_MIN_SEC = 5;

/**
 * resolveIdleBound: the idle bound in seconds a kept or warm launch asks for, from COMFY_IDLE_FREE_SEC: unset = 300 (the house rule),
 * `0` or `off` = none (said, once: COMFY-IDLE-OFF), a whole number of seconds of 5 or more = that, anything else = 300 and a
 * COMFY-IDLE-WARN that names the value. It is fixed when an instance is LAUNCHED: a runner that reuses a running instance cannot change it.
 */
export function resolveIdleBound(env = process.env, log = () => {}) {
  const raw = String(env.COMFY_IDLE_FREE_SEC ?? "").trim();
  if (raw === "") return IDLE_FREE_DEFAULT_SEC;
  const whole = /^\d+$/.test(raw) ? Number(raw) : null;
  if (/^off$/i.test(raw) || whole === 0) {
    log(`COMFY-IDLE-OFF: COMFY_IDLE_FREE_SEC=${raw} switches the idle bound off for the instances this run launches: they keep whatever they hold until their lease ends`);
    return 0;
  }
  if (whole !== null && Number.isSafeInteger(whole) && whole >= IDLE_FREE_MIN_SEC) return whole;
  log(`COMFY-IDLE-WARN: COMFY_IDLE_FREE_SEC=${raw} is not off, 0 or a whole number of seconds of ${IDLE_FREE_MIN_SEC} or more; using ${IDLE_FREE_DEFAULT_SEC}`);
  return IDLE_FREE_DEFAULT_SEC;
}

// --- the process bound (ADR 0097, 2026-10-24) -------------------------------------------------------
//
// The ComfyUI the harness launches is KEPT between jobs, not killed after each one: on an 8 GB laptop card the launch was 31-89 s of a
// 66-135 s render, while the bare process holds about 240 MiB of the card once its models are gone (--disable-smart-memory unloads them
// when a prompt ends, and the end-of-job /free follows). The process is bounded the way llama-swap bounds a seat, by the engine itself:
// the idle pack exits it after COMFY_IDLE_EXIT_SEC (default 300) with no prompt queued or running and no runner holding it. A runner
// holds the instance for its whole job (GET /offload/hold, openComfyHold below), so a long step that is not ComfyUI's does not look
// abandoned. A job that fails stops the instance it used (gpu-lock.mjs), so a sticky CUDA error is still cured by the next launch.
export const IDLE_EXIT_DEFAULT_SEC = 300;

/**
 * resolveIdleExit: the process bound in seconds a harness launch asks for, from COMFY_IDLE_EXIT_SEC: unset = 300, `0` or `off` = none
 * (said, once: COMFY-KEEP-OFF; the runner then kills the instance it launched after its job, the zero-warm lifecycle of ADR 0009), a
 * whole number of seconds of 5 or more = that, anything else = 300 and a COMFY-IDLE-WARN that names the value.
 */
export function resolveIdleExit(env = process.env, log = () => {}) {
  const raw = String(env.COMFY_IDLE_EXIT_SEC ?? "").trim();
  if (raw === "") return IDLE_EXIT_DEFAULT_SEC;
  const whole = /^\d+$/.test(raw) ? Number(raw) : null;
  if (/^off$/i.test(raw) || whole === 0) {
    log(`COMFY-KEEP-OFF: COMFY_IDLE_EXIT_SEC=${raw}: the ComfyUI this run launches has no process bound, so it is stopped after the job (a relaunch per job)`);
    return 0;
  }
  if (whole !== null && Number.isSafeInteger(whole) && whole >= IDLE_FREE_MIN_SEC) return whole;
  log(`COMFY-IDLE-WARN: COMFY_IDLE_EXIT_SEC=${raw} is not off, 0 or a whole number of seconds of ${IDLE_FREE_MIN_SEC} or more; using ${IDLE_EXIT_DEFAULT_SEC}`);
  return IDLE_EXIT_DEFAULT_SEC;
}

/**
 * openComfyHold: hold a running instance for this runner's job (GET /offload/hold on the idle pack, a response that stays open and
 * carries a byte every 2 s; its body is read here so the stream never backs up). Returns { state, release }:
 *   "held" — the pack holds the instance until release() (or this process's death closes the connection);
 *   "none" — the instance answered without a hold (no pack, or an older one): it has no process bound, so there is nothing to hold off;
 *   "gone" — nothing answered: the instance exited between the caller's check and this request (its bound fired), and the caller
 *            launches again.
 * release() is idempotent and never throws.
 */
export async function openComfyHold(api, { fetchImpl = fetch, timeoutMs = 5000 } = {}) {
  const ac = new AbortController();
  const release = () => { try { ac.abort(); } catch {} };
  let timer;
  let res;
  try {
    res = await Promise.race([
      fetchImpl(api + "/offload/hold", { signal: ac.signal }),
      new Promise((_, reject) => { timer = setTimeout(() => reject(Object.assign(new Error("hold timeout"), { holdTimeout: true })), timeoutMs); }),
    ]);
  } catch (e) {
    release();
    // A slow answer is not a dead instance: carry on unheld rather than launch a second one onto the same port.
    return { state: e && e.holdTimeout ? "none" : "gone", release };
  } finally {
    clearTimeout(timer);
  }
  if (!res || !res.ok || !res.body) {
    try { await res?.body?.cancel?.(); } catch {}
    release();
    return { state: "none", release };
  }
  const reader = res.body.getReader();
  (async () => { try { for (;;) { const { done } = await reader.read(); if (done) break; } } catch {} })();
  return { state: "held", release };
}

/**
 * launchShapeGap: why a self-bounded instance this harness kept was launched for another shape than this call needs ("" = the same).
 * Only what a later call can ask differently for: --cache-none (single renders) against a warm session (none), and --reserve-vram. A
 * kept instance outlives the job it was launched for, so a call that needs another shape restarts it rather than run on the old one.
 */
export function launchShapeGap(args, { warm = false, reserveVram = "1.0" } = {}) {
  if (!Array.isArray(args)) return "";
  const cacheNone = args.map(String).includes("--cache-none");
  if (cacheNone && warm) return "it was launched for single renders (--cache-none) and this call is a warm session";
  if (!cacheNone && !warm) return "it was launched as a warm session (no --cache-none) and this call renders with --cache-none";
  const had = argvFlagValue(args.map(String), "--reserve-vram");
  const want = String(reserveVram || "1.0");
  if (had !== null && Number(had) !== Number(want)) return `it was launched with --reserve-vram ${had} and this call needs ${want}`;
  return "";
}

/**
 * instanceInUse: why another client is using the instance on `api` right now ("" = nobody seen): a runner's hold beyond the caller's
 * own `ownHolds`, or, with `queue`, any prompt queued or running. A kept instance is outlived by the jobs of every runner that reuses
 * it, so a runner kills or restarts one only when this is "". An instance that does not answer is not in use. Never throws.
 */
export async function instanceInUse(api, { ownHolds = 0, queue = false, probe = fetchIdleProbe, fetchImpl = fetch } = {}) {
  const st = await probe(api);
  const holds = st && Number.isFinite(Number(st.holds)) ? Number(st.holds) : 0;
  if (holds > ownHolds) return `${holds - ownHolds} other runner(s) hold it`;
  if (!queue) return "";
  try {
    const r = await fetchImpl(api + "/queue", { signal: AbortSignal.timeout(3000) });
    if (!r.ok) return "";
    const q = await r.json();
    const n = (Array.isArray(q?.queue_running) ? q.queue_running.length : 0) + (Array.isArray(q?.queue_pending) ? q.queue_pending.length : 0);
    return n > 0 ? `${n} prompt(s) queued or running on it` : "";
  } catch {
    return "";
  }
}

/**
 * stopHarnessInstance: stop the instance on `api` after a job on it FAILED, when it is one this harness launched (its launch marker's
 * pid is alive and the server reports exactly the recorded argv). A kept instance used to be cured of a sticky CUDA error by the kill
 * every job ended with; now that it outlives its jobs, the failed job ends it instead, and the next one launches clean. Anything that
 * is not shown to be ours is left alone, and so is one another runner holds (`ownHolds` is the caller's own hold, still open): that
 * runner's job is live, and the job that ends it stops it too if it fails. Its queue is not asked: a prompt of the failed job's own
 * that hangs looks like anyone's, and a hung instance never reaches its process bound. Never throws.
 */
export async function stopHarnessInstance({ api, comfyDir = COMFY_DIR, key = "", reason = "the job on it failed", ownHolds = 0, inUse = instanceInUse, readLaunch = readLaunchOwner, systemArgv = fetchSystemArgv, alive = defaultPidAlive, killPid = (pid) => process.kill(pid), clearLaunch = clearLaunchOwner, up = comfyUp, log = (line) => console.error(line), pollMs = 1000, stopPolls = 15 } = {}) {
  try {
    const marker = key ? readLaunch(comfyDir, key) : readLaunch(comfyDir);
    // Only a self-bounded instance (one a default runner kept): an operator's explicit --keep-comfy session is theirs to end.
    if (!marker || !(Number(marker.idleExitSec) > 0)) return false;
    const argv = await systemArgv(api);
    if (!harnessLaunched(marker, argv, alive)) return false;
    const busy = await inUse(api, { ownHolds });
    if (busy) {
      log(`COMFY-KEEP-WARN: left harness-launched ComfyUI pid ${marker.pid} on ${api} running after a failed job: ${busy}`);
      return false;
    }
    log(`COMFY-KEEP: stopping harness-launched ComfyUI pid ${marker.pid} on ${api} (${reason}); the next job launches a fresh one`);
    try { killPid(marker.pid); } catch {}
    for (let i = 0; i < stopPolls; i++) {
      await new Promise((r) => setTimeout(r, pollMs));
      if (!(await up(api))) { try { if (key) clearLaunch(comfyDir, key); else clearLaunch(comfyDir); } catch {} return true; }
    }
    log(`COMFY-KEEP-WARN: ComfyUI pid ${marker.pid} on ${api} still answers after a kill; its process bound ends it once it is idle`);
  } catch (e) {
    log(`COMFY-KEEP-WARN: could not stop the ComfyUI on ${api} after a failed job (${e && e.message}); its process bound ends it once it is idle`);
  }
  return false;
}

/** fetchIdleProbe: what GET /offload/idle on a running instance answers (the pack's status object), or null. */
export async function fetchIdleProbe(api) {
  try {
    const r = await fetch(api + "/offload/idle", { signal: AbortSignal.timeout(3000) });
    if (!r.ok) return null; // a 404 is an instance without the pack
    const j = await r.json();
    return j && typeof j === "object" && !Array.isArray(j) ? j : null;
  } catch {
    return null;
  }
}

/** idleYamlToken: is this launch-argument token the idle yaml, by name, under any path spelling (`--flag=path` included)? */
const idleYamlToken = (tok) => /(^|[\\/=])extra_model_paths_idle\.yaml$/i.test(String(tok));
/** The same file, by exact path (slashes and case aside), for a yaml the operator's extra args already carry. */
const sameFile = (a, b) => String(a).replace(/\\/g, "/").toLowerCase() === String(b).replace(/\\/g, "/").toLowerCase();

const WHITELIST_FLAG = "--whitelist-custom-nodes";
const DISABLE_ALL_FLAG = "--disable-all-custom-nodes";
/**
 * isLongFlag: is this token `long`, or an argparse abbreviation of it (`--wh`, `--wh=x`)? `minLen` is the shortest prefix ComfyUI's parser
 * keeps unambiguous, measured against ComfyUI 0.38.0's own parser: `--wh` (4; `--w` also fits --windows-standalone-build) and
 * `--disable-al` (12; `--disable-a` also fits --disable-auto-launch, --disable-async-offload and --disable-api-nodes).
 */
const isLongFlag = (tok, long, minLen) => {
  const name = String(tok).split("=")[0];
  return name.length >= minLen && long.startsWith(name);
};

/**
 * withPackWhitelisted: `flags` with the idle pack in the custom-node whitelist ComfyUI will read. --whitelist-custom-nodes is
 * nargs='+' with no append action, so a second occurrence REPLACES the first list: the pack goes INTO the last occurrence's value run
 * (argparse keeps the last), never into a new flag. `--flag=a` takes one value only (`--flag=a b` leaves `b` a stray positional), so that
 * spelling is rewritten to `--flag a` for the pack to follow. With --disable-all-custom-nodes and no whitelist a whitelist naming the pack
 * is added at the end; with neither, every custom node loads (the pack among them, found through the yaml) and nothing is added.
 */
function withPackWhitelisted(flags, pack) {
  const out = [...flags];
  let at = -1;
  for (let i = 0; i < out.length; i++) if (isLongFlag(out[i], WHITELIST_FLAG, 4)) at = i;
  if (at < 0) {
    if (out.some((f) => isLongFlag(f, DISABLE_ALL_FLAG, 12))) out.push(WHITELIST_FLAG, pack);
    return out;
  }
  const tok = String(out[at]);
  const eq = tok.indexOf("=");
  if (eq >= 0) out.splice(at, 1, tok.slice(0, eq), ...(tok.slice(eq + 1) === "" ? [] : [tok.slice(eq + 1)]));
  let end = at + 1;
  while (end < out.length && !String(out[end]).startsWith("--")) end++;
  if (!out.slice(at + 1, end).includes(pack)) out.splice(end, 0, pack);
  return out;
}

/**
 * launchFlags is the exact argv tail ensureComfy spawns main.py with. Base flags
 * first, then the profile's --cuda-device, then the extra args LAST (the J4 seam's
 * contract: extra args are the tail). The profile's device wins over a --cuda-device
 * or --default-device the extra args carry — argparse takes the last occurrence, so
 * leaving them in would silently override the binding.
 * `idle` ({ yaml, pack, packFile }, null = none: the argv is then byte for byte what it was) adds the idle bound: its yaml once,
 * and the pack in the custom-node whitelist (see withPackWhitelisted).
 */
export function launchFlags({ reserveVram = "1.0", warm = false, extraArgs = "", profile = { cudaDevice: "", dynamicVram: "" }, instance = null, comfyDir = "", warn = () => {}, launch = null, idle = null } = {}) {
  const flags = ["--disable-smart-memory"];
  // warm: a BATCH session keeps ComfyUI's model cache ON so the checkpoint loads once
  // for N renders; the caller still tears the whole session down at the batch boundary
  // (zero-always-warm moves from per-render to per-batch). Default stays cache-none.
  if (!warm) flags.push("--cache-none");
  flags.push("--reserve-vram", String(reserveVram || "1.0"));
  // A multicard launch (render/wan-multicard.mjs multicardLaunch) adds the flags its placement needs right after the base ones:
  // --gpu-only, --disable-pinned-memory, the harness's extra-model-paths yaml and the custom-node whitelist. Its two cards are
  // pinned by uuid in the child env, so a --cuda-device / --default-device in the extra args (main.py would rewrite
  // CUDA_VISIBLE_DEVICES from it) is dropped, said, exactly as for a single card pinned by uuid. Nothing is added without `launch`.
  if (launch && Array.isArray(launch.flags)) flags.push(...launch.flags);
  // J4 seam: COMFY_EXTRA_ARGS appends verbatim (whitespace-split) launch flags —
  // the per-box escape hatch for non-CUDA backends (--directml, device pinning)
  // without touching shared code. Empty/unset = byte-identical launch.
  let extra = String(extraArgs || "").split(/\s+/).filter(Boolean);
  if (profile.cardUuid || profile.multicard) {
    // A card pin by uuid lives in the child env (CUDA_VISIBLE_DEVICES), never in a flag:
    // --cuda-device would rewrite that env from an index in a different order.
    const had = extra.some((f) => /^--(cuda|default)-device(=|$)/.test(f));
    extra = stripValued(stripValued(extra, "--cuda-device"), "--default-device");
    if (had) warn("COMFY-INSTANCE-WARN: the extra args carry --cuda-device/--default-device, which would override this instance's card pin by uuid; dropped");
  } else if (profile.cudaDevice) {
    extra = stripValued(stripValued(extra, "--cuda-device"), "--default-device");
    flags.push("--cuda-device", profile.cudaDevice);
  }
  if (profile.multicard) {
    // ComfyUI's main.py starts dynamic VRAM whenever --enable-dynamic-vram is given, beside --gpu-only too (cli_args.enables_dynamic_vram checks it first), and
    // the operator's extra args land after the placement's flags: it would stream the experts through host RAM under a lease that declared none.
    const had = extra.includes("--enable-dynamic-vram");
    extra = extra.filter((f) => f !== "--enable-dynamic-vram");
    if (had) warn("COMFY-INSTANCE-WARN: the extra args carry --enable-dynamic-vram, which ComfyUI honours even beside --gpu-only and which would stream the experts through host RAM under a card-only lease; dropped");
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
  if (instance && instance.key) {
    // The instance owns its port, directories and database: an extra arg naming one would
    // silently put it back on a shared one (argparse keeps the last occurrence).
    const owned = ["--port", "--output-directory", "--temp-directory", "--database-url"];
    const had = extra.some((f) => owned.some((o) => f === o || f.startsWith(o + "=")));
    for (const o of owned) extra = stripValued(extra, o);
    if (had) warn(`COMFY-INSTANCE-WARN: the extra args carry ${owned.join("/")}, which would move instance '${instance.key}' off its own port, directories and database; dropped`);
    const { outputDir, tempBase, dbUrl } = instancePaths(comfyDir, instance.key);
    flags.push("--port", String(instance.port), "--output-directory", outputDir, "--temp-directory", tempBase, "--database-url", dbUrl);
  }
  // The idle bound (resolveIdleBound): only when the yaml AND the pack file exist, because ComfyUI exits at boot on an
  // --extra-model-paths-config file that does not exist and on a custom_nodes folder that does not; a render never fails for want of it.
  // The yaml goes after the base and placement flags and before the operator's args, once; the pack goes into the whitelist last of all.
  let idleOn = false;
  if (idle && idle.yaml) {
    const missing = !existsSync(idle.yaml) ? idle.yaml : !existsSync(idle.packFile) ? idle.packFile : "";
    if (missing) {
      warn(`COMFY-IDLE-WARN: the idle bound is not launched: ${missing} is missing (ComfyUI cannot start with an extra-model-paths file or a custom_nodes folder that does not exist); this instance keeps whatever it holds until its lease ends`);
    } else {
      idleOn = true;
      if (![...flags, ...extra].some((f) => sameFile(f, idle.yaml) || sameFile(f, "--extra-model-paths-config=" + idle.yaml))) flags.push("--extra-model-paths-config", idle.yaml);
    }
  }
  flags.push(...extra);
  return idleOn ? withPackWhitelisted(flags, idle.pack) : flags;
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
  if (profile.cardUuid) {
    // The uuid itself lives in the process env, which /system_stats does not report (the
    // launch marker proves it, see reuseVerdict); the argv can still disprove the pin.
    const got = argvFlagValue(argv, "--cuda-device");
    if (got !== null) why.push(`it runs with --cuda-device ${got}, which overrides a card pin by uuid`);
  }
  if (profile.dynamicVram) {
    const on = argvDynamicVram(argv);
    if ((profile.dynamicVram === "on") !== on) {
      why.push(`it runs with dynamic VRAM ${on ? "on" : "off"}; this binding needs it ${profile.dynamicVram}`);
    }
  }
  if (profile.multicard) {
    // The two cards are provable only by the launch marker (reuseVerdict); the argv can still disprove the placement.
    if (!argv.map(String).includes("--gpu-only")) why.push("it runs without --gpu-only, which the multicard placement needs (dynamic VRAM off, everything on the cards)");
    const got = argvFlagValue(argv, "--cuda-device");
    if (got !== null) why.push(`it runs with --cuda-device ${got}, which overrides the multicard pin by uuid`);
    if (argvDynamicVram(argv)) why.push("it runs with dynamic VRAM on, which the multicard placement needs off (--gpu-only keeps everything on the cards; --enable-dynamic-vram turns the streaming back on)");
    // A call sized for the DEFAULT attention (sage === false) budgets 12.2 GiB of activations at film size; a kept instance that runs sage attention holds 15.2, so the share
    // the call was admitted with can run out of memory on it. That direction is refused (or the harness's own instance replaced); the reverse, a kept default instance and a call
    // sized for sage, only wastes a cold start and is reused. `sage` absent is the old shape (a launch that does not say) and checks nothing. The live argv is read with the same
    // predicate the sizing reads COMFY_EXTRA_ARGS with: /system_stats carries the flag as it was typed, an abbreviation included.
    if (profile.multicard.sage === false && argv.map(String).some(isSageAttentionToken)) {
      why.push("it runs with --use-sage-attention, which holds about 3 GiB more activations at film size than this launch was sized for (the default attention)");
    }
    // A GGUF expert needs the ComfyUI-GGUF pack in the custom-node whitelist (wan-multicard.mjs planMulticard sets `gguf`); a kept instance launched without it cannot load the graph.
    // The whitelist rides the argv as its own tokens, matched here by the folder name, case-insensitively.
    if (profile.multicard.gguf === true && !argv.map((t) => String(t).toLowerCase()).includes("comfyui-gguf")) {
      why.push("its custom-node whitelist has no ComfyUI-GGUF, which a GGUF expert needs");
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
 * keyedOwnershipGap: why a ComfyUI answering on a keyed instance's port is NOT shown to be
 * that instance ("" = shown). The proof is the keyed launch marker (this key, this port, a
 * live pid, the exact recorded argv) AND the live argv carrying the instance's own --port and
 * --output-directory, so a marker cannot vouch for a process that is not on the instance's
 * port and directories.
 */
function keyedOwnershipGap({ marker, argv, comfyDir, key, port, alive }) {
  const who = `instance '${key}' on port ${port}`;
  if (!harnessLaunched(marker, argv, alive)) {
    return `nothing shows that this ComfyUI is ${who}: no launch marker of this harness matches a live process running exactly this argv, so it was not started by this harness for that instance and is not reused (stop it, or give the instance another port)`;
  }
  if (marker.key !== key || marker.port !== port) {
    return `its launch marker is for instance '${marker.key ?? ""}' on port ${marker.port ?? ""}, not ${who}, so it is not reused`;
  }
  if (argvFlagValue(argv, "--port") !== String(port)) {
    return `it runs with ${argvFlagValue(argv, "--port") === null ? "no --port" : "--port " + argvFlagValue(argv, "--port")}; ${who} needs --port ${port}, so it is not reused`;
  }
  const outputDir = instancePaths(comfyDir, key).outputDir;
  if (argvFlagValue(argv, "--output-directory") !== outputDir) {
    return `it runs with ${argvFlagValue(argv, "--output-directory") === null ? "no --output-directory" : "--output-directory " + argvFlagValue(argv, "--output-directory")}; ${who} needs --output-directory ${outputDir}, so it is not reused`;
  }
  return "";
}

/**
 * reuseVerdict decides what to do with a ComfyUI that is ALREADY listening:
 *   { reuse: true }                  — the default instance with no profile requested, or
 *                                      its argv honours it; a keyed instance only on proof
 *                                      that it is that instance (keyedOwnershipGap);
 *   { restart: true, pid, reason }   — the harness launched it (fingerprint match), its
 *                                      spawner is gone, and its profile is wrong: ours
 *                                      to replace;
 *   { reuse: false, reason }         — anything else: refuse (COMFY-PROFILE-MISMATCH).
 * A foreign instance is never killed — the harness cannot know what else it serves.
 */
export async function reuseVerdict({ api, comfyDir, profile, key = "", port = null, systemArgv = fetchSystemArgv, readLaunch = readLaunchOwner, alive = defaultPidAlive }) {
  // The default instance is reused as it always was. A KEYED instance never is without proof:
  // whatever answers on its port could be another key's instance, another launcher's ComfyUI
  // or a foreign one, none of which uses this instance's directories, marker or log.
  if (!key && !profileRequested(profile)) return { reuse: true };
  const argv = await systemArgv(api);
  let reason = profileMismatch(argv, profile);
  // A card pin by uuid cannot be read off a running process (/system_stats lists device
  // names, and two cards of one model are indistinguishable), so it is provable only by
  // the keyed launch marker: the instance is ours (live pid + the exact argv) AND was
  // launched on this very card. Anything else is a mismatch, never a pass.
  const marker = comfyDir && (reason || key || profile.cardUuid || profile.multicard) ? (key ? readLaunch(comfyDir, key) : readLaunch(comfyDir)) : null;
  if (!reason && profile.cardUuid) {
    const got = harnessLaunched(marker, argv, alive) ? String((marker.profile && marker.profile.cardUuid) || "") : null;
    if (got === null) {
      reason = `its card pin cannot be read from a running process, and no launch marker of this harness proves it was started on card ${profile.cardUuid}`;
    } else if (got.toLowerCase() !== profile.cardUuid.toLowerCase()) {
      reason = `it was launched ${got ? "on card " + got : "with no card pin"}; this binding needs card ${profile.cardUuid}`;
    }
  }
  if (!reason && profile.multicard) {
    // Same proof as a single card's: the pair cannot be read off a running process, only off the launch marker of an instance
    // that is ours (live pid + the exact argv) and was launched on these two cards in this order (storage first).
    const got = harnessLaunched(marker, argv, alive) ? ((marker.profile && marker.profile.multicard && marker.profile.multicard.cards) || []) : null;
    const want = profile.multicard.cards || [];
    if (got === null) {
      reason = `its two cards cannot be read from a running process, and no launch marker of this harness proves it was started on ${want.join(" + ")}`;
    } else if (got.map((c) => String(c).toLowerCase()).join(",") !== want.map((c) => String(c).toLowerCase()).join(",")) {
      reason = `it was launched on ${got.length ? got.join(" + ") : "no card pair"}; this binding needs ${want.join(" + ")} (storage card first)`;
    }
  }
  if (!reason && key) {
    // Whose instance this is: the marker is this key's, on this port, for a live process whose
    // argv is exactly the recorded one and carries the instance's own port and output
    // directory. A gap is a refusal, never a restart: an instance that cannot be shown to be
    // this one is not ours to stop.
    const gap = keyedOwnershipGap({ marker, argv, comfyDir, key, port, alive });
    if (gap) return { reuse: false, reason: gap };
  }
  if (!reason) return { reuse: true };
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
// behind a failed render. The <node-a> disk-space defect ("[Errno 28] No space left
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
 * .offload-owned.json / .offload-launch.json convention in comfy-ownership.mjs). A keyed
 * (per-card) instance has its own file, so two instances never interleave one log. */
export function comfyLogPath(comfyDir = COMFY_DIR, key = "") {
  return join(comfyDir, key ? `offload-comfyui-${key}.log` : "offload-comfyui.log");
}

/** rotateComfyLog: age out old numbered logs and free the current filename for the
 * new run, so a long-lived box never accumulates unbounded ComfyUI console history.
 * Best-effort: a rotation failure (e.g. a concurrent reader holding the file open on
 * Windows) must never block a render. */
export function rotateComfyLog(comfyDir = COMFY_DIR, key = "") {
  const base = comfyLogPath(comfyDir, key);
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

/** trimRotatedLog: keep the archived copy of the previous run's console (`<log>.1`, the file
 * rotateComfyLog just made) to its last COMFY_LOG_CAP_BYTES. A KEPT instance writes its console
 * straight to its log file (no pipe, so no capture that could stop at the cap): the file is
 * therefore bounded by ROTATION, not by truncation mid-run. Each launch rotates, and each
 * archive is cut to the cap here, so the disk holds at most COMFY_LOG_KEEP archives of 5 MB
 * plus the live file of the run in progress; the live file is not truncated under its writer,
 * because the instance lives no longer than its lease. Best-effort, like the rotation. */
export function trimRotatedLog(comfyDir = COMFY_DIR, key = "") {
  const archive = `${comfyLogPath(comfyDir, key)}.1`;
  let fd;
  try {
    fd = openSync(archive, "r");
    const { size } = fstatSync(fd);
    if (size <= COMFY_LOG_CAP_BYTES) return;
    const buf = Buffer.alloc(COMFY_LOG_CAP_BYTES);
    readSync(fd, buf, 0, COMFY_LOG_CAP_BYTES, size - COMFY_LOG_CAP_BYTES);
    closeSync(fd);
    fd = undefined;
    writeFileSync(archive, buf);
  } catch {
    /* absent, or held open on Windows: the archive stays as it is */
  } finally {
    if (fd !== undefined) { try { closeSync(fd); } catch {} }
  }
}

/** The window tailComfyLog reads from the END of the log. A kept instance writes its console
 * to the file for as long as its lease lives, so the log can be far larger than the 20 lines
 * a failure message needs: reading all of it into one string is not an option. */
const COMFY_LOG_TAIL_WINDOW_BYTES = 256 * 1024;

/** tailComfyLog: the last `n` lines of THIS run's ComfyUI console capture, or ""
 * when there is none (no harness-managed launch this run, or nothing captured yet).
 * Synchronous — called once on a render failure, and once after a successful multicard run
 * (comfy-video.mjs reads it for ComfyUI's tiled-decode retry); never on any other happy path.
 * Reads only the last COMFY_LOG_TAIL_WINDOW_BYTES of the file; when that window starts
 * inside a line, the cut line is dropped rather than shown half. */
export function tailComfyLog(comfyDir = COMFY_DIR, n = COMFY_LOG_TAIL_LINES, key = "") {
  let fd;
  try {
    fd = openSync(comfyLogPath(comfyDir, key), "r");
    const { size } = fstatSync(fd);
    const len = Math.min(size, COMFY_LOG_TAIL_WINDOW_BYTES);
    const buf = Buffer.alloc(len);
    if (len > 0) readSync(fd, buf, 0, len, size - len);
    const lines = buf.toString("utf8").split(/\r?\n/);
    if (size > len && lines.length > 1) lines.shift(); // the window began mid-line
    if (lines.length && lines[lines.length - 1] === "") lines.pop();
    return lines.slice(-n).join("\n");
  } catch {
    return "";
  } finally {
    if (fd !== undefined) { try { closeSync(fd); } catch {} }
  }
}

/** The most one comfyLogCursor read takes, and how much of the last read it keeps to finish a line the end of a read cut. */
const COMFY_LOG_CURSOR_MAX_READ_BYTES = 8 * 1024 * 1024;
const COMFY_LOG_CURSOR_CARRY_CHARS = 256;

/**
 * comfyLogCursor: a reader of what this instance's ComfyUI console gains from now on. tailComfyLog answers "what did the console end with"
 * once, after the fact; the cursor answers "what has it said since the run began", again and again while the run waits, without reading the
 * file twice or mistaking an earlier run's lines (a kept instance's log holds them) for this one's. `read()` returns { text, state }:
 * text is what was appended since the last read, prefixed with the tail of the one before it so a line cut by the end of a read is whole in
 * the next; state is "ok", "absent" (no console file: nothing captured, or a ComfyUI this harness did not start) or "capped" (the capture
 * reached its 5 MB cap, after which ComfyUI's output is no longer written: everything later is invisible). A file that is shorter than the
 * cursor's position was replaced and is read from its start; one that appears later is read from its start. Never throws.
 */
export function comfyLogCursor(comfyDir = COMFY_DIR, key = "") {
  const path = comfyLogPath(comfyDir, key);
  let offset = null; // null: the file did not exist when the cursor was made
  try { offset = statSync(path).size; } catch { /* absent */ }
  let carry = "";
  return {
    path,
    read() {
      let size;
      try { size = statSync(path).size; } catch { return { text: "", state: "absent" }; }
      if (offset === null || size < offset) { offset = 0; carry = ""; }
      if (size === offset) return { text: "", state: "ok" };
      const len = Math.min(size - offset, COMFY_LOG_CURSOR_MAX_READ_BYTES);
      const buf = Buffer.alloc(len);
      let fd;
      try {
        fd = openSync(path, "r");
        readSync(fd, buf, 0, len, offset);
      } catch {
        return { text: "", state: "absent" };
      } finally {
        if (fd !== undefined) { try { closeSync(fd); } catch {} }
      }
      offset += len;
      const text = carry + buf.toString("utf8");
      carry = text.slice(-COMFY_LOG_CURSOR_CARRY_CHARS);
      return { text, state: /offload-comfyui\.log capped at/.test(text) ? "capped" : "ok" };
    },
  };
}

/** captureComfyOutput: pipes a just-spawned ComfyUI child's stdout+stderr into the
 * rotating log file, capped at COMFY_LOG_CAP_BYTES so a long batch session cannot
 * grow it without bound. Every step is best-effort and defensive against a fake
 * `spawn` (tests inject one that returns a bare `{ kill() {} }`, with no
 * stdout/stderr/once) — a logging failure must never take down, or even alter the
 * behaviour of, the render it exists to help diagnose. */
function captureComfyOutput(child, comfyDir, key = "") {
  if (!child) return;
  rotateComfyLog(comfyDir, key);
  let ws;
  try {
    ws = createWriteStream(comfyLogPath(comfyDir, key), { flags: "a" });
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

/**
 * stopComfy replaces a harness-launched ComfyUI that cannot serve this binding: kill it by
 * the pid its launch marker recorded (the caller has already checked the fingerprint, so
 * this never runs against an instance the harness did not start), wait for its port to go
 * quiet, then clear its marker (the keyed one for a keyed instance).
 */
export async function stopComfy({ api, comfyDir, key = "", pid, reason, up, killPid, clearLaunch, log, pollMs, stopPolls }) {
  log(`COMFY-PROFILE-RESTART: stopping harness-launched ComfyUI pid ${pid} on ${api} (${reason})`);
  try { killPid(pid); } catch {}
  let down = false;
  for (let i = 0; i < stopPolls; i++) {
    await new Promise((r) => setTimeout(r, pollMs));
    if (!(await up(api))) { down = true; break; }
  }
  if (!down) {
    const line = `COMFY-PROFILE-MISMATCH: ComfyUI on ${api} (harness-launched pid ${pid}) did not stop after a kill: ${reason}`;
    log(line);
    throw new Error(line);
  }
  try { if (key) clearLaunch(comfyDir, key); else clearLaunch(comfyDir); } catch {}
}

/** leaseEpochOf: the lease epoch the harness handed this process (GPU_LEASE_EPOCH), or null. */
function leaseEpochOf(env) {
  const raw = String(env.GPU_LEASE_EPOCH ?? "").trim();
  const n = Number(raw);
  return raw !== "" && Number.isInteger(n) && n >= 0 ? n : null;
}

/**
 * leasedPair: the two cards the lease this process runs under holds (GPU_LEASE_DEVICES, which `gpu reserve --devices` and the media admission both export), or
 * null when the lease holds any other number of cards or none. The card-only placement (`--placement multicard`) is for exactly two, so three or one is not
 * the case it is named for. Read from the env only: render/gpu-lock.mjs leaseDevices also reads the lease record, but that file imports this one.
 */
export function leasedPair(env = process.env) {
  const ids = String((env && env.GPU_LEASE_DEVICES) || "").split(",").map((s) => s.trim()).filter(Boolean);
  return ids.length === 2 ? ids : null;
}

/**
 * warnWithoutIdleBound: a running instance a kept or warm runner reuses, said once per runner start when its argv lacks the idle yaml (a hand-started
 * instance, or one an older build kept). It is reused all the same: it keeps whatever it holds until its lease ends, as it always did.
 * An argv ComfyUI does not report is unknown, not missing, and says nothing.
 */
async function warnWithoutIdleBound({ api, key, systemArgv, log }) {
  let argv = null;
  try { argv = await systemArgv(api); } catch {}
  if (!Array.isArray(argv) || argv.some(idleYamlToken)) return;
  log(`COMFY-IDLE-WARN: instance '${key || "default"}' on ${api} has no idle bound (launched without the idle pack); it keeps whatever it holds until its lease ends. To bound a hand-started instance add --extra-model-paths-config ${EXT_IDLE_YAML} to its launch`);
}

/**
 * verifyIdleBound: ask a freshly launched instance for its idle status (GET /offload/idle) and say what came back; never throws.
 * One COMFY-IDLE line when the pack is live at the bound that was asked for (a pack that reports no bound is taken at its word);
 * a COMFY-IDLE-WARN when it did not answer, reports it cannot run, or reports another bound than `secs` (it never read the variable).
 */
async function verifyIdleBound({ api, key, secs, exitSecs = 0, probeIdle, log }) {
  const who = `instance '${key || "default"}' on ${api}`;
  let answer = null;
  try { answer = await probeIdle(api); } catch {}
  if (!answer || typeof answer !== "object") {
    log(`COMFY-IDLE-WARN: ${who} did not answer GET /offload/idle, so the idle pack may not have loaded (its console log says); it keeps whatever it holds until its lease ends`);
  } else if (answer.error) {
    log(`COMFY-IDLE-WARN: ${who} reports that its idle bound is not active: ${answer.error}; it keeps whatever it holds until its lease ends`);
  } else if (typeof answer.limit_sec === "number" && answer.limit_sec !== secs) {
    // OFFLOAD_IDLE_FREE_SEC went to the child, so a pack that answers another bound did not read it: the operator asked for secs, not for this.
    log(`COMFY-IDLE-WARN: ${who} reports an idle bound of ${answer.limit_sec} s, not the ${secs} s asked for (OFFLOAD_IDLE_FREE_SEC did not reach the pack); it frees after ${answer.limit_sec} s with an empty queue`);
  } else if (exitSecs > 0 && answer.exit_limit_sec !== exitSecs) {
    log(`COMFY-IDLE-WARN: ${who} reports a process bound of ${answer.exit_limit_sec ?? "none"}, not the ${exitSecs} s asked for (OFFLOAD_IDLE_EXIT_SEC did not reach the pack, or the pack predates it); a later job's failure or an operator stops it`);
  } else {
    log(`COMFY-IDLE: ${who} frees its models and caches after ${secs} s with an empty queue${exitSecs > 0 ? `, and exits after ${exitSecs} s with no prompt and no runner holding it` : ""}`);
  }
  // Whether the process bound is shown to be live: only then may the caller leave the instance running after a good job.
  return exitSecs > 0 && !!answer && typeof answer === "object" && !answer.error && answer.exit_limit_sec === exitSecs;
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
    api: apiOpt,
    comfyDir = COMFY_DIR,
    py = COMFY_PY,
    reserveVram = "1.0",
    warm = false,
    comfyUp: up = comfyUp,
    spawn = nodeSpawn,
    envFor = cudaVisibleEnv,
    env = process.env,
    // launch: the card-only (multicard) placement's launch (render/wan-multicard.mjs multicardLaunch): extra flags, the two cards
    // to pin by uuid (storage first) and the env the placement needs. Absent = every launch behaves exactly as before.
    launch = null,
    systemArgv = fetchSystemArgv,
    // probeIdle: asks a freshly launched instance whether its idle bound is live (GET /offload/idle); null = no answer.
    probeIdle = fetchIdleProbe,
    // inUse: whether another client is using a running instance (instanceInUse), asked before a kept one is restarted.
    inUse = instanceInUse,
    // idleExt: where the idle bound's yaml and pack are (a test points it elsewhere).
    idleExt = { yaml: EXT_IDLE_YAML, pack: EXT_IDLE_PACK, packFile: EXT_IDLE_PACK_FILE },
    readLaunch = readLaunchOwner,
    writeLaunch = writeLaunchOwner,
    restampLaunch = restampLaunchOwner,
    clearLaunch = clearLaunchOwner,
    alive = defaultPidAlive,
    killPid = (pid) => process.kill(pid),
    portFree = portIsFree,
    // keep: the caller will leave this instance running after its job (runners' --keep-comfy).
    // A kept instance outlives the process that launched it, so it is spawned detached with its
    // console going to its own log FILE and unref'd (see the spawn below); openLogFd/closeFd are
    // the file seams for tests.
    keep = false,
    // idleExit: the process bound (seconds) a KEPT launch carries (resolveIdleExit; withGpuSlot passes it): the idle pack exits the
    // process after that long with no prompt and no holder. 0 = none, the instance's lifetime is then its keeper's business.
    idleExit = 0,
    // report: an object the caller passes to learn what it got; ensureComfy sets report.selfBounded = true when the instance it
    // returns (launched, or reused: null) ends itself once idle, shown by its own pack or by its marker and argv.
    report = null,
    openLogFd = (p) => openSync(p, "a"),
    closeFd = closeSync,
    mkdirs = (d) => mkdirSync(d, { recursive: true }),
    log = (line) => console.error(line),
    pollMs = 2000,
    // Startup budget: a laptop cold start (custom nodes + models on a slow disk)
    // legitimately exceeds the old hardcoded ~4 min. Default 10 min, env-tunable
    // (COMFY_START_WAIT_SEC) — same pattern as the render polls' COMFY_WAIT_SEC.
    maxPolls = Math.max(1, Math.ceil(Number(process.env.COMFY_START_WAIT_SEC || 600) * 1000 / 2000)),
    stopPolls = 15,
  } = opts;
  // Which instance this is: the default one (key "", today's behaviour) or a keyed
  // per-card one. The api defaults exactly as before (COMFY_API, else the default port).
  const instance = resolveInstance({ api: apiOpt || process.env.COMFY_API, env });
  const { api, key } = instance;
  const profile = resolveLaunchProfile(env);
  // The idle bound is for the instances that outlive their runner: a kept one, or a warm session that holds its cache. A launch that is
  // neither (COMFY_IDLE_EXIT_SEC=0: its runner kills it after the job) asks for nothing and its launch stays byte for byte what it was.
  const idleSecs = (keep || warm) ? resolveIdleBound(env, log) : 0;
  // The process bound rides on the same pack, so the pack goes in when either bound is on.
  const exitSecs = keep ? Math.max(0, Math.floor(Number(idleExit) || 0)) : 0;
  const wantPack = idleSecs > 0 || exitSecs > 0;
  // `sage` says which attention the call was sized for (wan-multicard.mjs planMulticard sets it); a launch that does not say keeps the old shape, { cards } alone
  if (launch) profile.multicard = { cards: [...launch.cards], ...(typeof launch.sage === "boolean" ? { sage: launch.sage } : {}), ...(launch.gguf === true ? { gguf: true } : {}) };
  // Can this call launch an instance on `api` at all? A keyed instance owns its --port; the
  // default endpoint is what an unlaunched ComfyUI listens on; an UNKEYED instance on any other
  // endpoint has no --port of its own, so only the operator's own extra args can put it there.
  // Without this a launch would start ComfyUI on 8188 while the run submits to `api` (plan
  // section 7, finding 7: a stray instance inside a card lease, then COMFY-PROFILE-MISMATCH).
  const canLaunchHere = Boolean(key) || instance.port === DEFAULT_COMFY_PORT
    || argvFlagValue(String(env.COMFY_EXTRA_ARGS || "").split(/\s+/).filter(Boolean), "--port") === String(instance.port);
  // A two-card lease with no multicard launch: only --placement multicard keys the pair's own instance and launches it on the pair's port, so a call aimed
  // at a port nothing answers on is missing that placement, and the generic line (key the instance, which pins ONE card) misleads (PF-4). endpointDown is
  // reached only by an unkeyed call (canLaunchHere is true for a keyed one), so there is no key to test here.
  const pair = launch ? null : leasedPair(env);
  const endpointDown = () => {
    if (pair) {
      const line = `COMFY-PLACEMENT-MISSING: ComfyUI is not answering on ${api}, and this call holds a lease on two cards (${pair.join(", ")}) without asking for the card-only placement: only --placement multicard keys the pair's own instance and launches it on its own port, and an unkeyed runner can launch only the default endpoint (port ${DEFAULT_COMFY_PORT}), so nothing was started. Run it with --placement multicard (with the DiT and text encoder of a recipe, render/multicard-recipes.json), or start ComfyUI on that port yourself`;
      log(line);
      return new Error(line);
    }
    const line = `COMFY-ENDPOINT-DOWN: ComfyUI is not answering on ${api}, and an unkeyed runner can launch only the default endpoint (port ${DEFAULT_COMFY_PORT}): a launch with no --port would start 8188 while this run submits to ${api}. Start ComfyUI on that port yourself, or key the instance (COMFY_INSTANCE=<name>, or COMFY_CARD_UUID with this --api), which makes the runner launch it on its own port`;
    log(line);
    return new Error(line);
  };
  // The reuse decision and the idle warning both read the running instance's argv (GET /system_stats, up to 8 s on a slow one): one
  // request answers both. reuseVerdict's return shape is read by other callers, so the answer is shared here, not through it.
  let argvAsked;
  const askArgv = (a) => (argvAsked ??= systemArgv(a));
  if (await up(api)) {
    let v = await reuseVerdict({ api, comfyDir, profile, key, port: instance.port, systemArgv: askArgv, readLaunch, alive });
    // A self-bounded instance this harness kept for an earlier job: its marker names the bound and it runs exactly the recorded argv.
    let keptOurs = null;
    if (v.reuse && comfyDir) {
      const kept = key ? readLaunch(comfyDir, key) : readLaunch(comfyDir);
      if (kept && Number(kept.idleExitSec) > 0 && harnessLaunched(kept, await askArgv(api), alive)) keptOurs = kept;
    }
    if (keptOurs) {
      // Launched for an earlier job whose shape was another (a warm session against single renders, another --reserve-vram):
      // restart it rather than run on the old shape, unless the process that launched it, or any other client, is still at work on it.
      const gap = launchShapeGap(keptOurs.args, { warm, reserveVram });
      const ownerBusy = typeof keptOurs.ownerPid === "number" && keptOurs.ownerPid !== process.pid && alive(keptOurs.ownerPid);
      if (gap && !ownerBusy) {
        const busy = await inUse(api, { queue: true });
        if (busy) log(`COMFY-KEEP-WARN: ComfyUI on ${api} was launched for another shape (${gap}), but ${busy}, so this call runs on it as it is`);
        else v = { restart: true, pid: keptOurs.pid, reason: gap };
      }
    }
    if (v.reuse) {
      if (report && keptOurs) report.selfBounded = true;
      // A kept instance outlives a lease whose holder died or was fenced out, and the next lease on
      // its card finds it running. Reusing it makes it that lease's: re-stamp the marker, or the
      // new holder's release (which stops by epoch) stops nothing and the instance outlives every
      // later lease with its models resident. Only an instance that was already a lease's moves
      // (restampLaunchOwner); outside a lease there is nothing to hand it to.
      const epoch = key ? leaseEpochOf(env) : null;
      if (epoch !== null) {
        try {
          const r = restampLaunch(comfyDir, key, epoch);
          if (r && r.changed) log(`COMFY-KEEP: instance '${key}' outlived lease epoch ${r.previous}; it is lease epoch ${epoch}'s now and is stopped with it`);
        } catch (e) {
          log(`COMFY-KEEP-WARN: could not hand instance '${key}' to lease epoch ${epoch} (${e && e.message}); its marker still names lease epoch ${(readLaunch(comfyDir, key) || {}).leaseEpoch ?? "?"}, so this lease's release will not stop it`);
        }
      }
      // A running instance is never restarted or refused for want of the idle pack (every kept instance in flight on an upgrade, and every
      // hand-started one, lacks it): it is said once per runner start (this call), not once per process tree, so a driver that starts one
      // runner per shot says it once per shot. Its argv is asked here because reuseVerdict does not fetch it for the default instance, and
      // never twice (askArgv); an argv ComfyUI does not report is unknown, not missing. COMFY_IDLE_FREE_SEC cannot change an instance already running.
      if (idleSecs > 0) await warnWithoutIdleBound({ api, key, systemArgv: askArgv, log });
      return null; // already running and fit for this binding — don't manage it
    }
    if (!v.restart) {
      const line = "COMFY-PROFILE-MISMATCH: ComfyUI on " + api + ": " + v.reason;
      log(line);
      throw new Error(line);
    }
    // Replacing it means launching on `api` again; if this call cannot, stopping it first would
    // leave the operator with neither instance.
    if (!canLaunchHere) throw endpointDown();
    // Ours, orphaned (a --keep-comfy session or a crashed teardown), wrong profile:
    // replace it rather than render on the wrong card.
    await stopComfy({ api, comfyDir, key, pid: v.pid, reason: v.reason, up, killPid, clearLaunch, log, pollMs, stopPolls });
  }
  // A runner given another endpoint (--api / COMFY_API) that nothing answers on must NOT launch
  // (canLaunchHere above): the one launch that is honest is the operator's own, extra args that
  // carry that very --port.
  if (!canLaunchHere) throw endpointDown();
  // An unbound COMFY_DIR must fail with its reason, not with a bad cwd from spawn(): on a
  // machine with no ComfyUI binding the caller's defer should say WHY.
  if (!comfyDir) {
    throw new Error("COMFY_DIR is not set — this machine has no ComfyUI install bound (set comfy_dir in the harness config)");
  }
  if (key) {
    // A keyed instance has its own port: bind-check it before launching. Nothing answered
    // as ComfyUI on it (see up(api) above), so whatever holds it is not ours to reuse, and
    // never ours to kill: refuse, loudly, and leave it alone. The probe goes to every address
    // ComfyUI will listen on (127.0.0.1 unless its --listen says otherwise), not to the host
    // the api happens to spell: "localhost" can resolve to ::1 first, which would miss a
    // holder on 127.0.0.1.
    for (const host of bindHosts(env.COMFY_EXTRA_ARGS)) {
      if (!(await portFree(instance.port, host))) {
        const line = `COMFY-PORT-TAKEN: port ${instance.port} on ${host} for ComfyUI instance '${key}' is held by a process that does not answer as ComfyUI on ${api}; not launching onto it, and not stopping it (free the port, or give the instance another one)`;
        log(line);
        throw new Error(line);
      }
    }
    // ComfyUI makes these itself on first use; creating them first keeps a read-only
    // surprise (a missing parent) from surfacing mid-render. Best-effort. tempDir is the
    // directory ComfyUI really uses (it appends /temp to --temp-directory).
    const { outputDir, tempDir } = instancePaths(comfyDir, key);
    for (const d of [outputDir, tempDir]) { try { mkdirs(d); } catch {} }
  }
  const flags = launchFlags({ reserveVram, warm, extraArgs: env.COMFY_EXTRA_ARGS, profile, instance: key ? instance : null, comfyDir, warn: log, launch, idle: wantPack ? idleExt : null });
  // Applied only if launchFlags found the files: the child's env and the probe below follow what it really launched.
  const idleApplied = wantPack && flags.some(idleYamlToken);
  // The process bound holds only if the pack is really in: without it, nothing would ever end a kept instance (the caller kills it).
  const exitApplied = idleApplied && exitSecs > 0 ? exitSecs : 0;
  // A --cuda-device launch has already scoped the cards (main.py rewrites
  // CUDA_VISIBLE_DEVICES to it), so the multi-GPU visibility restore below does not
  // apply — and neither does its --disable-pinned-memory: one visible card is not the
  // multi-device pinned-transfer case, and pinned memory is what keeps a streamed
  // (dynamic VRAM) bf16 DiT fast.
  // A card-bound instance is pinned by uuid in the child env, which is the whole pin:
  // one visible card, so neither the multi-GPU restore nor --cuda-device applies.
  // A multicard launch pins its two cards by uuid, storage card first: CUDA numbers the visible cards in that order, so the first
  // is cuda:0 whatever its nvidia-smi index, and the graph's cuda:0 / cuda:1 are the cards the lease holds.
  const spawnEnv = launch ? { ...env, CUDA_VISIBLE_DEVICES: launch.cudaVisibleDevices, ...(launch.env || {}) }
    : profile.cardUuid ? { ...env, CUDA_VISIBLE_DEVICES: profile.cardUuid }
    : flags.includes("--cuda-device") ? env : envFor();
  // A keyed instance with no card is not pinned to anything: it sees every card its
  // environment shows, the display card included. That is the operator's explicit choice
  // (a side-by-side instance, a test), so it is allowed, but never silent.
  if (key && !launch && !profile.cardUuid && env.CUDA_VISIBLE_DEVICES === undefined && !flags.includes("--cuda-device")) {
    log(`COMFY-INSTANCE-WARN: instance '${key}' is not bound to a card (no COMFY_CARD_UUID), so nothing pins it: it sees every card this environment shows, the display card included`);
  }
  // Upstream's own guidance for the multi-GPU Windows path it now hides by default:
  // "pass --cuda-device all --disable-pinned-memory" — pinned memory with multiple
  // visible devices risks CUDA host-transfer failures on Windows (#15737). We restore
  // visibility via env, so we also carry the second half of that guidance.
  if (spawnEnv !== process.env && String(spawnEnv.CUDA_VISIBLE_DEVICES || "").includes(",")
      && !flags.includes("--disable-pinned-memory")) {
    flags.push("--disable-pinned-memory");
  }
  // The pack reads its bound from the child's env. A COPY, made after the identity check above (which asks whether the placement's env is
  // process.env itself) so neither that decision nor the parent's env changes; a launch that asked for nothing passes spawnEnv as it was.
  const childEnv = idleApplied
    ? { ...spawnEnv, OFFLOAD_IDLE_FREE_SEC: String(idleSecs), ...(exitApplied ? { OFFLOAD_IDLE_EXIT_SEC: String(exitApplied) } : {}) }
    : spawnEnv;
  let child;
  if (keep) {
    // A KEPT instance must not be a piped child of a runner that is about to exit. As one it held
    // the runner's event loop (the runner never exited after writing its output: the P6 rollout
    // lost a finished clip to the per-shot timeout that then killed the tree), and when the runner
    // did go, the pipe closed under a process still writing to it. So: its console goes to its own
    // log file (a descriptor, not a stream, so there is no pipe to break), it is detached from the
    // runner's process group and unref'd (the runner's loop no longer waits for it), and its
    // window is hidden (a detached console process opens one on Windows). The log is bounded by
    // rotation at each launch, not by a capture that stops at 5 MB (see trimRotatedLog).
    //
    // WHO STOPS IT: the instance itself, when the launch carries a process bound (exitApplied, the
    // harness's default since ADR 0097): the idle pack exits it after COMFY_IDLE_EXIT_SEC with no prompt
    // and no runner holding it, and the pipeline's routine release of a media lease leaves it for the
    // next job (internal/comfyinst). A job that fails stops it (gpu-lock.mjs). `gpu reserve`, its
    // detached holder and `gpu release` still stop the keyed instances of their lease when they end
    // it. A kept instance WITHOUT a process bound (an explicit --keep-comfy with COMFY_IDLE_EXIT_SEC=0)
    // lives as before: a keyed one until its lease's holder stops it, the default one until its
    // operator does. Either way the idle pack frees models and caches after COMFY_IDLE_FREE_SEC.
    rotateComfyLog(comfyDir, key);
    trimRotatedLog(comfyDir, key);
    let fd = null;
    try { fd = openLogFd(comfyLogPath(comfyDir, key)); } catch (e) {
      log(`COMFY-KEEP-WARN: cannot open ${comfyLogPath(comfyDir, key)} (${e && e.message}); this instance's console goes nowhere`);
    }
    try {
      child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: ["ignore", fd ?? "ignore", fd ?? "ignore"], detached: true, windowsHide: true, env: childEnv });
    } finally {
      if (fd !== null) { try { closeFd(fd); } catch {} } // the child holds its own copy
    }
    try { child?.unref?.(); } catch {}
  } else {
    child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: ["ignore", "pipe", "pipe"], detached: false, env: childEnv, windowsHide: true });
    captureComfyOutput(child, comfyDir, key);
  }
  // Record the launch so a later job can tell this instance from a foreign one (the
  // fingerprint is pid + this exact argv). Best-effort: without it, a later profile
  // mismatch refuses instead of restarting — the safe direction. A keyed instance's record
  // also names its key, its port and the lease epoch it was launched under.
  if (child && typeof child.pid === "number") {
    const rec = { pid: child.pid, ownerPid: process.pid, args: ["main.py", ...flags], profile };
    // A self-bounded instance says so: the pipeline's release leaves it running, and a later job may reuse it across leases.
    if (exitApplied) rec.idleExitSec = exitApplied;
    if (key) {
      rec.key = key;
      rec.port = instance.port;
      const epoch = leaseEpochOf(env);
      if (epoch !== null) rec.leaseEpoch = epoch;
    }
    try { writeLaunch(comfyDir, rec); } catch (e) {
      // A kept instance is detached and is stopped by its lease's holder, which finds it by this
      // marker: without one nothing can stop it by its lease, and it would outlive it unseen.
      if (keep) log(`COMFY-KEEP-WARN: could not record the launch marker for kept instance '${key || "default"}' (${e && e.message}); the lease it was launched under cannot stop it, stop it by hand`);
    }
  }
  // Fail-fast dead-child watchdog (<node-e> stall, bigger-models-2026-09-24.md
  // "Phase 2 round 2" item 4): the poll loop below only ever asked "is the HTTP
  // port open yet?" — a child that never actually started (ENOENT from a bad cwd
  // or a python that does not exist at `py`, which node_child_process reports
  // asynchronously via 'error', not by throwing spawn() itself) or one that
  // crashed on the way up looked IDENTICAL to a slow cold boot: no process tree,
  // 0% GPU, port never open, GPU lease held the whole time — and the loop kept
  // silently polling for the FULL maxPolls budget (~10min default) before
  // failing at all. Listening for the child's own 'error'/early 'exit' turns
  // that into a failure within one poll interval, with the reason named instead
  // of a bare timeout — a boot that makes no progress now fails loudly, fast,
  // rather than reading like an unusually slow one until the budget runs out.
  let earlyExit = null;
  try {
    child?.once?.("error", (err) => { if (!earlyExit) earlyExit = { detail: `spawn error: ${err && err.message ? err.message : err}` }; });
    child?.once?.("exit", (code, signal) => {
      if (!earlyExit) earlyExit = { detail: `exited before answering on ${api} (code ${code}, signal ${signal || "none"})` };
    });
  } catch {}
  for (let i = 0; i < maxPolls; i++) {
    await new Promise((r) => setTimeout(r, pollMs));
    if (await up(api)) {
      // Ask the instance it launched whether the pack is alive: a pack that did not load (an upstream rename, a folder ComfyUI skipped) frees
      // nothing and would otherwise be silent. Said, never fatal: no render fails for want of the bound.
      const exitLive = idleApplied ? await verifyIdleBound({ api, key, secs: idleSecs, exitSecs: exitApplied, probeIdle, log }) : false;
      // The caller reads this to know whether the instance ends itself (then it may be left running after a good job): only a bound
      // the pack itself reported. An instance whose pack did not answer is killed by its runner after the job, as before.
      try { if (child) child.offloadIdleExitSec = exitLive ? exitApplied : 0; } catch {}
      if (report && exitLive) report.selfBounded = true;
      return child;
    }
    if (earlyExit) {
      const tail = tailComfyLog(comfyDir, undefined, key);
      const seeLog = tail
        ? `\nlast ComfyUI console output (${comfyLogPath(comfyDir, key)}):\n${tail}`
        : `\n(no ComfyUI console output captured — see ${comfyLogPath(comfyDir, key)})`;
      throw new Error(`COMFY-BOOT-FAILED: ${py} main.py in ${comfyDir} ${earlyExit.detail}${seeLog}`);
    }
  }
  try { child.kill(); } catch {}
  throw new Error("ComfyUI did not become ready on " + api + " after ~" + Math.round(maxPolls * pollMs / 60000) + "min (COMFY_START_WAIT_SEC to extend)");
}
