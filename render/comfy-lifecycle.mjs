// comfy-lifecycle.mjs — the shared, on-demand ComfyUI cold-start lifecycle. This was
// byte-identical across comfy-generate.mjs and comfy-video.mjs; centralized here so the
// cold-start + ~4-min ready-poll + zero-always-warm launch flags
// (--disable-smart-memory --cache-none --reserve-vram) live in ONE place. A `warm`
// batch session omits --cache-none so a checkpoint loads once for N renders (the
// caller still tears the session down at the batch boundary). tts.mjs does
// NOT use this (its Chatterbox worker is not ComfyUI; it passes comfyManaged:false to
// withGpuSlot). Dependency-free; deps are injectable purely for tests.
import { existsSync, createWriteStream, renameSync, rmSync, readFileSync, mkdirSync, openSync, closeSync, readSync, fstatSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
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

// --- per-card instances (plan P13a) ---------------------------------------------------
//
// One ComfyUI per card lets two media jobs run on two cards at once. The DEFAULT instance
// (key "") is today's single ComfyUI, left byte-identical: same argv, same launch marker,
// same console log, same spawn env, no bind check. An instance is keyed ONLY by an explicit
// COMFY_INSTANCE or by the card it is bound to (COMFY_CARD_UUID); a port on its own never
// makes one. A keyed instance owns a port (given by --api / COMFY_API, else the base
// COMFY_PORT_BASE + COMFY_INSTANCE_INDEX), its own output and temp directories (one folder
// per instance, <comfyDir>/instances/<key>, see instancePaths), its own launch marker and
// console log, and, when bound to a card, a pin by GPU uuid.
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
 */
export function instancePaths(comfyDir, key) {
  const root = join(comfyDir, "instances", key);
  return { outputDir: join(root, "output"), tempBase: root, tempDir: join(root, "temp") };
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

const profileRequested = (p) => !!(p && (p.cudaDevice || p.dynamicVram || p.cardUuid));

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
export function launchFlags({ reserveVram = "1.0", warm = false, extraArgs = "", profile = { cudaDevice: "", dynamicVram: "" }, instance = null, comfyDir = "", warn = () => {} } = {}) {
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
  if (profile.cardUuid) {
    // A card pin by uuid lives in the child env (CUDA_VISIBLE_DEVICES), never in a flag:
    // --cuda-device would rewrite that env from an index in a different order.
    const had = extra.some((f) => /^--(cuda|default)-device(=|$)/.test(f));
    extra = stripValued(stripValued(extra, "--cuda-device"), "--default-device");
    if (had) warn("COMFY-INSTANCE-WARN: the extra args carry --cuda-device/--default-device, which would override this instance's card pin by uuid; dropped");
  } else if (profile.cudaDevice) {
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
  if (instance && instance.key) {
    // The instance owns its port and directories: an extra arg naming one would silently
    // put it back on a shared one (argparse keeps the last occurrence).
    const owned = ["--port", "--output-directory", "--temp-directory"];
    const had = extra.some((f) => owned.some((o) => f === o || f.startsWith(o + "=")));
    for (const o of owned) extra = stripValued(extra, o);
    if (had) warn(`COMFY-INSTANCE-WARN: the extra args carry ${owned.join("/")}, which would move instance '${instance.key}' off its own port and directories; dropped`);
    const { outputDir, tempBase } = instancePaths(comfyDir, instance.key);
    flags.push("--port", String(instance.port), "--output-directory", outputDir, "--temp-directory", tempBase);
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
  const marker = comfyDir && (reason || key || profile.cardUuid) ? (key ? readLaunch(comfyDir, key) : readLaunch(comfyDir)) : null;
  if (!reason && profile.cardUuid) {
    const got = harnessLaunched(marker, argv, alive) ? String((marker.profile && marker.profile.cardUuid) || "") : null;
    if (got === null) {
      reason = `its card pin cannot be read from a running process, and no launch marker of this harness proves it was started on card ${profile.cardUuid}`;
    } else if (got.toLowerCase() !== profile.cardUuid.toLowerCase()) {
      reason = `it was launched ${got ? "on card " + got : "with no card pin"}; this binding needs card ${profile.cardUuid}`;
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
 * Synchronous — only ever called once, on a render failure, never on the happy path.
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
    systemArgv = fetchSystemArgv,
    readLaunch = readLaunchOwner,
    writeLaunch = writeLaunchOwner,
    clearLaunch = clearLaunchOwner,
    alive = defaultPidAlive,
    killPid = (pid) => process.kill(pid),
    portFree = portIsFree,
    // keep: the caller will leave this instance running after its job (runners' --keep-comfy).
    // A kept instance outlives the process that launched it, so it is spawned detached with its
    // console going to its own log FILE and unref'd (see the spawn below); openLogFd/closeFd are
    // the file seams for tests.
    keep = false,
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
  if (await up(api)) {
    const v = await reuseVerdict({ api, comfyDir, profile, key, port: instance.port, systemArgv, readLaunch, alive });
    if (v.reuse) return null; // already running and fit for this binding — don't manage it
    if (!v.restart) {
      const line = "COMFY-PROFILE-MISMATCH: ComfyUI on " + api + ": " + v.reason;
      log(line);
      throw new Error(line);
    }
    // Ours, orphaned (a --keep-comfy session or a crashed teardown), wrong profile:
    // replace it rather than render on the wrong card.
    await stopComfy({ api, comfyDir, key, pid: v.pid, reason: v.reason, up, killPid, clearLaunch, log, pollMs, stopPolls });
  }
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
  const flags = launchFlags({ reserveVram, warm, extraArgs: env.COMFY_EXTRA_ARGS, profile, instance: key ? instance : null, comfyDir, warn: log });
  // A --cuda-device launch has already scoped the cards (main.py rewrites
  // CUDA_VISIBLE_DEVICES to it), so the multi-GPU visibility restore below does not
  // apply — and neither does its --disable-pinned-memory: one visible card is not the
  // multi-device pinned-transfer case, and pinned memory is what keeps a streamed
  // (dynamic VRAM) bf16 DiT fast.
  // A card-bound instance is pinned by uuid in the child env, which is the whole pin:
  // one visible card, so neither the multi-GPU restore nor --cuda-device applies.
  const spawnEnv = profile.cardUuid ? { ...env, CUDA_VISIBLE_DEVICES: profile.cardUuid }
    : flags.includes("--cuda-device") ? env : envFor();
  // A keyed instance with no card is not pinned to anything: it sees every card its
  // environment shows, the display card included. That is the operator's explicit choice
  // (a side-by-side instance, a test), so it is allowed, but never silent.
  if (key && !profile.cardUuid && env.CUDA_VISIBLE_DEVICES === undefined && !flags.includes("--cuda-device")) {
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
    // WHO STOPS IT: the instance lives no longer than the GPU lease it was launched under (the
    // marker records that epoch). The holder of the lease stops it on release: `gpu reserve`
    // when its wrapped command ends, and the pipeline when its media lease is released
    // (internal/comfyinst). A kept default instance (no key) has no lease epoch in its marker and
    // is stopped by its operator, as before.
    rotateComfyLog(comfyDir, key);
    trimRotatedLog(comfyDir, key);
    let fd = null;
    try { fd = openLogFd(comfyLogPath(comfyDir, key)); } catch (e) {
      log(`COMFY-KEEP-WARN: cannot open ${comfyLogPath(comfyDir, key)} (${e && e.message}); this instance's console goes nowhere`);
    }
    try {
      child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: ["ignore", fd ?? "ignore", fd ?? "ignore"], detached: true, windowsHide: true, env: spawnEnv });
    } finally {
      if (fd !== null) { try { closeFd(fd); } catch {} } // the child holds its own copy
    }
    try { child?.unref?.(); } catch {}
  } else {
    child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: ["ignore", "pipe", "pipe"], detached: false, env: spawnEnv });
    captureComfyOutput(child, comfyDir, key);
  }
  // Record the launch so a later job can tell this instance from a foreign one (the
  // fingerprint is pid + this exact argv). Best-effort: without it, a later profile
  // mismatch refuses instead of restarting — the safe direction. A keyed instance's record
  // also names its key, its port and the lease epoch it was launched under.
  if (child && typeof child.pid === "number") {
    const rec = { pid: child.pid, ownerPid: process.pid, args: ["main.py", ...flags], profile };
    if (key) {
      rec.key = key;
      rec.port = instance.port;
      const epoch = leaseEpochOf(env);
      if (epoch !== null) rec.leaseEpoch = epoch;
    }
    try { writeLaunch(comfyDir, rec); } catch {}
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
    if (await up(api)) return child;
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
