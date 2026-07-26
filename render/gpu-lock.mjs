// gpu-lock.mjs — the Node side of the machine-wide, FENCED GPU lease.
//
// One GPU is shared by llama-swap (:11436) and ComfyUI (:8188), so only ONE GPU-heavy
// job may run at a time. This file used to own that arbitration alone — and that was
// the defect: TEXT work never took the lock, so a dispatched render called
// freeLlamaSwap() and unloaded every GPU-resident model out from under an in-flight
// benchmark. Arbitration now lives in internal/gpulease (Go) and this file is a
// PARTICIPANT in the same lease, not a separate lock.
//
// THE SHARED TOKEN IS meta.json, created O_EXCL ("wx"). The lease DIRECTORY is a
// container and never a claim — when Go claimed by creating the directory while this
// side claimed by creating meta.json, the two used different atomic tokens and both
// could come away holding the card. isStale mirrors gpulease.Reclaimable exactly,
// including the conjunction; if the two drift, Go and Node will disagree about who owns
// the GPU. Regression coverage lives in internal/gpulease/contention_test.go.
//
// UNLOADING IS ONCE PER LEASE AND DRAINS FIRST. freeLlamaSwap used to run inside
// withGpuSlot, i.e. once per JOB — that is the arithmetic behind 3,356 unloads. Under
// an inherited lease the first job to claim the unload marker performs it and the rest
// skip, so N renders in one lease cost ONE teardown. And because
// POST /api/models/unload was measured to return in 1,265ms WITHOUT draining while an
// in-flight generation died at 4,107ms with 502, the caller drains before unloading.
//
// No npm dependencies.
import { mkdirSync, writeFileSync, readFileSync, rmSync, statSync, unlinkSync } from "node:fs";
import { join } from "node:path";
import { tmpdir, platform } from "node:os";
import { ensureComfy as defaultEnsureComfy } from "./comfy-lifecycle.mjs";

const DEFAULT_TTL_MS = 60 * 60 * 1000; // 1h — a real video gen can take many minutes
// Mirrors gpulease.DefaultHeartbeatTTL. Deliberately generous: a holder under heavy
// load can be descheduled well past a short TTL, and reclaiming there would hand the
// card away mid-run. Staleness alone NEVER reclaims — see isStale.
const DEFAULT_HEARTBEAT_TTL_MS = 120 * 1000;
const RENEW_EVERY_MS = 15 * 1000;

function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}

// machineStateRoot mirrors gpulease.ResolveStateRoot's default: machine-wide, never
// per-user. A per-user root is the bug this replaces.
export function machineStateRoot(env = process.env) {
  const explicit = (env.LOCAL_OFFLOAD_STATE_DIR || "").trim();
  if (explicit) return explicit;
  if (platform() === "win32") {
    return join(env.ProgramData || "C:\\ProgramData", "local-offload");
  }
  return "/var/lib/local-offload";
}

// defaultLockPath: the ONE lease directory both Go and Node contend on. GPU_LOCK
// still wins so config gpu_lock_path keeps working and tests can redirect it.
export function defaultLockPath(env = process.env) {
  return env.GPU_LOCK || join(machineStateRoot(env), "gpu", "lease");
}

function epochPath(lockPath) {
  return join(lockPath, "..", "epoch");
}

function metaPath(lockPath) {
  return join(lockPath, "meta.json");
}

// isStale implements the SAME conjunction as gpulease.Reclaimable, and the two must
// not drift: a lease is reclaimable only when the holder is provably gone, OR its
// heartbeat is stale AND its declared window has expired. A bare heartbeat timeout
// would expire a descheduled benchmark under exactly the load it exists to protect.
export function isStale(meta, { heartbeatTtlMs = DEFAULT_HEARTBEAT_TTL_MS, nowMs = Date.now() } = {}) {
  if (!meta) return true;
  const pid = meta?.holder?.pid ?? 0;
  if (pid <= 0) return nowMs > (meta.expires_at_ms ?? 0);
  if (!pidAlive(pid)) return true;
  const heartbeatStale = (meta.renewed_at_ms ?? 0) > 0 && nowMs - meta.renewed_at_ms > heartbeatTtlMs;
  const windowExpired = nowMs > (meta.expires_at_ms ?? 0);
  return heartbeatStale && windowExpired;
}

function readMeta(lockPath) {
  try {
    const m = JSON.parse(readFileSync(metaPath(lockPath), "utf8"));
    m.mtimeMs = statSync(metaPath(lockPath)).mtimeMs;
    return m;
  } catch { return null; }
}

// ensureStateRoot: create the lease container, REFUSING with an actionable message
// rather than surfacing a bare errno. The Go side refuses to start on an unwritable
// machine-wide root; this side used to rethrow a raw EACCES from deep inside a render,
// which tells an operator nothing about state_dir.
function ensureStateRoot(lockPath) {
  try {
    mkdirSync(lockPath, { recursive: true });
  } catch (e) {
    throw new Error(
      `GPU lease state root is not usable: ${lockPath} (${e.code || e.message}). ` +
      `This path must be machine-wide and writable by every GPU consumer. ` +
      `Set state_dir in the harness config (or the GPU_LOCK env) to a local, unsynced, writable path. ` +
      `Refusing to fall back to a per-user path — that silently breaks mutual exclusion across security contexts.`);
  }
}

// bumpEpoch increments the monotonic fencing counter, which lives OUTSIDE the lease
// dir so removing a lease can never reset it.
//
// A FAILURE HERE IS FATAL, deliberately. Swallowing it meant every acquisition
// returned epoch 1, which does not "degrade" the fence — it INVERTS it: a straggler
// from a previous era then matches the current epoch, passes checkInheritedLease, and
// its release() deletes the live holder's lease. Go treats this as fatal too.
function bumpEpoch(lockPath) {
  const p = epochPath(lockPath);
  let cur = 0;
  try { cur = parseInt(readFileSync(p, "utf8").trim(), 10) || 0; } catch { cur = 0; }
  const next = cur + 1;
  writeFileSync(p, String(next)); // throws on failure — see above
  return next;
}

// acquireGpuLock: returns {release(), renew(), epoch, dir} on success, or null if held
// (after waiting waitMs). The wait is a QUEUE, not a cancellation: a job whose slot is
// busy waits its turn. A crashed holder never deadlocks it — isStale reclaims a
// dead-pid lease immediately.
export async function acquireGpuLock({
  lockPath, waitMs = 30 * 60 * 1000, ttlMs = DEFAULT_TTL_MS,
  heartbeatTtlMs = DEFAULT_HEARTBEAT_TTL_MS, klass = "media", reason = "",
} = {}) {
  const deadline = Date.now() + waitMs;
  ensureStateRoot(lockPath);

  for (;;) {
    const now = Date.now();
    let epoch;
    try {
      epoch = bumpEpoch(lockPath);
    } catch (e) {
      throw new Error(`GPU lease fencing counter is not writable at ${epochPath(lockPath)} (${e.code || e.message}); ` +
                      `refusing to acquire without a usable fence`);
    }
    try {
      // THE CLAIM: exclusive creation of meta.json, byte-identical in intent to the Go
      // side's O_CREATE|O_EXCL. The full record is written in this one call, so a
      // reader never observes a half-written claim.
      writeFileSync(metaPath(lockPath), JSON.stringify({
        epoch,
        class: klass,
        // start_time_ms is 0: Node has no portable process-start lookup. gpulease
        // treats 0 as "unknown" and skips the pid-recycle check, keeping pid-liveness
        // plus the heartbeat/expiry conjunction. Documented degradation, never silent.
        holder: { pid: process.pid, start_time_ms: 0 },
        reason,
        acquired_at_ms: now,
        expires_at_ms: now + ttlMs,
        renewed_at_ms: now,
      }), { flag: "wx" });

      let released = false;
      return {
        epoch,
        dir: lockPath,
        // renew refreshes the heartbeat. WITHOUT THIS a render longer than ttlMs was
        // reclaimable mid-run — reachable today, since a video job's wait window alone
        // is 20 minutes and a warm batch holds one lease throughout.
        renew() {
          if (released) return false;
          const cur = readMeta(lockPath);
          if (!cur || cur.epoch !== epoch) return false; // fenced out
          cur.renewed_at_ms = Date.now();
          try { writeFileSync(metaPath(lockPath), JSON.stringify(cur)); return true; } catch { return false; }
        },
        release() {
          if (released) return;
          released = true;
          // Epoch-guarded: never delete a lease that is no longer ours. A MISSING
          // record must also abort — falling through on null once let this delete a
          // competing acquirer's in-progress claim.
          const cur = readMeta(lockPath);
          if (!cur || cur.epoch !== epoch) return;
          try { unlinkSync(join(lockPath, `unloaded.${epoch}`)); } catch {}
          try { unlinkSync(metaPath(lockPath)); } catch {}
        },
      };
    } catch (e) {
      if (e.code !== "EEXIST") throw e;
      // Held by someone else. Reclaim only if genuinely stale.
      if (isStale(readMeta(lockPath), { heartbeatTtlMs })) {
        try {
          unlinkSync(metaPath(lockPath));
          continue; // reclaimed — retry the claim immediately
        } catch (rmErr) {
          if (rmErr.code !== "ENOENT") {
            // A stale claim we cannot remove is NOT a free lease. Looping here was a
            // synchronous infinite loop: no await, no deadline check, so waitMs was
            // never consulted and a core spun forever. Reachable whenever another
            // security context owns the file.
            throw new Error(`GPU lease at ${metaPath(lockPath)} is stale but cannot be reclaimed ` +
                            `(${rmErr.code || rmErr.message}); check permissions on the state root`);
          }
          continue; // someone else reclaimed it first
        }
      }
      if (Date.now() >= deadline) return null;
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
}

// inheritedLease: when the Go side already holds the lease it threads the directory,
// its fencing epoch and its class down to us. We then do NOT acquire (we would deadlock
// against our own holder).
export function inheritedLease(env = process.env) {
  const dir = (env.GPU_LEASE_DIR || "").trim();
  const epoch = (env.GPU_LEASE_EPOCH || "").trim();
  if (!dir || !epoch) return null;
  return { dir, epoch: Number(epoch), class: (env.GPU_LEASE_CLASS || "").trim() || undefined };
}

// checkInheritedLease is the FENCE for an inherited lease: before an irreversible
// action, confirm the epoch we were handed is still the current one. A closing lid is
// not a crash — the process resumes, and without this it would resume and act on top of
// whoever holds the card now.
export function checkInheritedLease(lease, read = readMeta) {
  if (!lease) return true;
  const meta = read(lease.dir);
  if (!meta) return false;
  return Number(meta.epoch) === Number(lease.epoch);
}

// claimLeaseUnload: decide whether THIS process is the one that must unload for the
// current lease. Exclusive creation of a per-epoch marker makes the winner unambiguous
// even with several jobs starting under one lease at once.
//
// This is what makes the hoist REAL. Skipping the unload under an inherited lease
// without anyone performing it meant a leased render ran with every model still
// resident — the opposite of the intent. First job in, one teardown; the rest skip.
export function claimLeaseUnload(lease) {
  if (!lease || !lease.dir || !Number.isFinite(lease.epoch)) return true;
  try {
    writeFileSync(join(lease.dir, `unloaded.${lease.epoch}`), String(process.pid), { flag: "wx" });
    return true;
  } catch (e) {
    if (e.code === "EEXIST") return false; // someone already unloaded for this lease
    return true; // cannot tell => unload. Correctness over saving one teardown.
  }
}

// MEMORY_STACK: the always-loaded, CPU-only mem0 models (they hold ZERO GPU VRAM).
// freeLlamaSwap must NEVER unload these — the unload-ALL route did, needlessly tearing
// down the load-bearing memory stack on every gen job for no VRAM benefit.
//
// SOURCED FROM CONFIG/ENV, not a buried const: the Go harness threads the config's
// MemoryStack as MEMORY_STACK, so a renamed/added 3rd CPU member is honored instead
// of silently unloaded. The literal below is the fallback for a direct CLI run.
const DEFAULT_MEMORY_STACK = ["embeddinggemma", "bge-reranker-v2-m3"];
export function memoryStack(env = process.env.MEMORY_STACK) {
  if (env && env.trim()) {
    return new Set(env.split(",").map((s) => s.trim()).filter(Boolean));
  }
  return new Set(DEFAULT_MEMORY_STACK);
}

// withTimeout: every network call here needs its own deadline. Node's fetch has NO
// default request timeout, so a socket that accepts and then stalls would hang the
// drain — and with it the render — indefinitely, regardless of any timeoutMs we track.
function withTimeout(ms) {
  return AbortSignal.timeout(ms);
}

// quiesceLlamaSwap: wait for in-flight work on `ids` to finish before unloading them.
//
// MEASURED, not defensive: on llama-swap v242 an unload issued during a generation
// returned in 1,265ms without draining and the in-flight request died at 4,107ms with
// 502 Bad Gateway. The unload route does not honour in-flight work, so a caller that
// wants to avoid killing someone's request must drain first.
//
// Signal: llama-server's own /slots via llama-swap's /upstream/<id>/slots, where
// is_processing is true exactly while a slot is generating (verified against a
// 23s/1500-token generation).
//
// FAIL-SAFE, NOT FAIL-OPEN-SILENT. A 404 is only "idle" when the model is genuinely
// NOT LOADED — it is also what a loaded upstream without a /slots route returns (any
// non-llama.cpp backend on :11436; whisper is one). Accepting that as idle reported a
// verified drain while in-flight work was killed, which is the exact 502 this exists to
// prevent. So a 404 is cross-checked against /running, and anything we cannot observe
// is named in `unknown` rather than assumed quiet.
export async function quiesceLlamaSwap(ids, {
  api = process.env.LLAMA_SWAP_API || "http://localhost:11436",
  timeoutMs = 60_000, pollMs = 500, graceMs = 1_500,
  fetchImpl = fetch, nowFn = Date.now, requestTimeoutMs = 5_000,
} = {}) {
  const deadline = nowFn() + timeoutMs;
  const unknown = new Set();
  const started = nowFn();

  // Which models does the server consider loaded? Used to disambiguate a 404.
  const loaded = async () => {
    try {
      const r = await fetchImpl(`${api}/running`, { signal: withTimeout(requestTimeoutMs) });
      if (!r.ok) return null;
      const j = await r.json();
      return new Set((j.running || []).map((m) => m.model).filter(Boolean));
    } catch { return null; }
  };

  const busy = async (id, loadedSet) => {
    try {
      const r = await fetchImpl(`${api}/upstream/${id}/slots`, { signal: withTimeout(requestTimeoutMs) });
      if (r.status === 404) {
        // Not loaded => truly nothing to drain. Loaded but no /slots route => we are
        // blind to it, and must say so instead of claiming a drain.
        if (loadedSet && loadedSet.has(id)) { unknown.add(id); }
        else if (!loadedSet) { unknown.add(id); } // could not check either
        return false;
      }
      if (!r.ok) { unknown.add(id); return false; }
      const j = await r.json();
      const slots = Array.isArray(j) ? j : [j];
      return slots.some((s) => s && s.is_processing === true);
    } catch { unknown.add(id); return false; }
  };

  // An unobservable tier reads as "not busy", which would otherwise end the drain on
  // the first transient blip. Retry a BOUNDED number of extra rounds so a momentary
  // 502 (a model swapping, say) resolves, while a permanently unobservable tier — one
  // with no /slots route at all — still costs only a few polls instead of the whole
  // timeout before every render.
  const unknownRetries = 2;
  let unknownRounds = 0;
  for (;;) {
    unknown.clear(); // judge each round on its own evidence, not a stale transient
    const loadedSet = await loaded();
    const flags = await Promise.all(ids.map((id) => busy(id, loadedSet)));
    if (!flags.some(Boolean)) {
      if (unknown.size === 0) break;             // verified idle
      if (++unknownRounds > unknownRetries) break; // accept: genuinely unobservable
    } else {
      unknownRounds = 0;
    }
    if (nowFn() >= deadline) {
      return { drained: false, waitedMs: nowFn() - started, unknown: [...unknown] };
    }
    await new Promise((r) => setTimeout(r, pollMs));
  }
  if (unknown.size > 0) {
    // We could not observe some tiers. Give in-flight work a brief grace rather than
    // claiming a drain we did not verify.
    await new Promise((r) => setTimeout(r, graceMs));
  }
  return { drained: unknown.size === 0, waitedMs: nowFn() - started, unknown: [...unknown] };
}

// freeLlamaSwap: free the GPU-resident llama-swap models so their VRAM goes to a gen
// job, while leaving the CPU memory stack warm. DRAINS FIRST (see quiesceLlamaSwap).
//
// Call this ONCE PER LEASE, not per job — withGpuSlot enforces that via
// claimLeaseUnload. Errors are reported through `log` rather than swallowed: silently
// unloading nothing leaves the render to OOM against a full card.
export async function freeLlamaSwap(api = process.env.LLAMA_SWAP_API || "http://localhost:11436", opts = {}) {
  const {
    quiesce = quiesceLlamaSwap, drainTimeoutMs = 60_000, requestTimeoutMs = 10_000,
    log = (m) => console.error(m),
  } = opts;
  const keep = memoryStack();
  let ids = [];
  try {
    const r = await fetch(api + "/v1/models", { signal: withTimeout(requestTimeoutMs) });
    if (!r.ok) { log(`freeLlamaSwap: /v1/models returned ${r.status}; NOT unloading (the render keeps a shared card)`); return; }
    const j = await r.json();
    ids = (j.data || []).map((m) => m.id).filter((id) => id && !keep.has(id));
  } catch (e) {
    log(`freeLlamaSwap: could not list models (${e && e.message}); NOT unloading (the render keeps a shared card)`);
    return;
  }
  if (ids.length === 0) return;

  try {
    const res = await quiesce(ids, { api, timeoutMs: drainTimeoutMs });
    if (!res.drained) {
      // Loud, not silent: proceeding here may kill someone's in-flight request. The
      // alternative — blocking the render forever behind a stuck tier — is worse, so
      // we proceed and say so.
      log(`freeLlamaSwap: proceeding without a verified drain after ${res.waitedMs}ms` +
          (res.unknown.length ? ` (could not read /slots for: ${res.unknown.join(",")})` : ""));
    }
  } catch (e) {
    log(`freeLlamaSwap: drain failed (${e && e.message}); proceeding to unload`);
  }
  await Promise.all(ids.map((id) =>
    fetch(api + "/api/models/unload/" + id, { method: "POST", signal: withTimeout(requestTimeoutMs) })
      .catch((e) => log(`freeLlamaSwap: unload ${id} failed: ${e && e.message}`))));
}

// freeComfy: tell ComfyUI to drop loaded models + free VRAM after a job (zero-warm).
export async function freeComfy(api = process.env.COMFY_API || "http://127.0.0.1:8188") {
  try {
    await fetch(api + "/free", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ unload_models: true, free_memory: true }),
      signal: withTimeout(10_000),
    });
  } catch {}
}

// withGpuSlot centralizes the single-slot GPU lifecycle every gen runner shares. It:
//   1. takes the GPU lease — UNLESS one was inherited from the Go holder (then it is
//      already ours) or noLock is set; a busy slot THROWS GPU-busy,
//   2. unloads llama-swap ONCE PER LEASE (claimLeaseUnload picks the one job that does
//      it) so the render gets the whole GPU,
//   3. optionally ensureComfy(); warm:true is the BATCH-SESSION mode,
//   4. awaits fn(),
//   5. runs ONE guarded teardown: freeComfy() + kill a ComfyUI we spawned + release.
// A heartbeat keeps a long render's lease alive; without it a job outliving its
// declared window was reclaimable mid-run.
export async function withGpuSlot(opts, fn) {
  const {
    noLock = false,
    keepComfy = false,
    comfyManaged = true,
    warm = false,
    reserveVram,
    lockPath = defaultLockPath(),
    acquire = acquireGpuLock,
    freeLlamaSwap: freeLS = freeLlamaSwap,
    ensureComfy = defaultEnsureComfy,
    freeComfy: freeCfy = freeComfy,
    lease = inheritedLease(),
    claimUnload = claimLeaseUnload,
    checkLease = checkInheritedLease,
  } = opts || {};

  const lock = noLock || lease
    ? { release() {}, renew: null }
    : await acquire({ lockPath, ...(waitMsFromEnv() != null ? { waitMs: waitMsFromEnv() } : {}) });
  if (!lock) throw new Error("GPU is busy (another job holds the lease); try again later or --no-lock");

  // Keep our own lease alive for the whole job. .unref() so the timer can never hold
  // the process open past the work.
  let beat = null;
  if (lock.renew) {
    beat = setInterval(() => { lock.renew(); }, RENEW_EVERY_MS);
    if (typeof beat.unref === "function") beat.unref();
  }

  let comfyChild = null;
  let cleaning = false;
  const cleanup = async () => {
    if (cleaning) return; cleaning = true;
    if (beat) clearInterval(beat);
    if (comfyManaged) { try { await freeCfy(); } catch {} }
    if (comfyChild && !keepComfy) { try { comfyChild.kill(); } catch {} }
    try { lock.release(); } catch {}
  };
  const onSig = async () => { await cleanup(); process.exit(130); };
  for (const sig of ["SIGINT", "SIGTERM", "SIGBREAK"]) process.on(sig, onSig);
  try {
    // WHO MAY UNLOAD, and HOW OFTEN.
    //
    // Class first: only a `media` holder may tear the text tier down. A `text` lease is
    // a benchmark's reservation — unloading under it destroys exactly the run the lease
    // was taken to protect. (An earlier revision skipped the unload for ANY inherited
    // lease, which happened to make text safe; electing an unloader without checking
    // the class then made a text lease unload the text tier.)
    //
    // Then once per LEASE, not once per job: the first job in claims the marker and
    // performs the teardown, later jobs in the same batch skip it.
    const mayUnload = !lease || lease.class !== "text";
    if (mayUnload && (!lease || claimUnload(lease))) {
      // FENCE before an irreversible action. A process that slept through a takeover
      // (a closing lid is not a crash) must not unload on top of the current holder.
      if (lease && !checkLease(lease)) {
        throw new Error(
          `GPU lease epoch ${lease.epoch} is no longer current — this process was fenced out ` +
          `(the card was handed to another holder while we were suspended). Refusing to touch the GPU.`);
      }
      await freeLS();
    }
    if (comfyManaged) {
      comfyChild = await ensureComfy({
        ...(reserveVram != null ? { reserveVram } : {}),
        ...(warm ? { warm: true } : {}),
      });
    }
    return await fn({ comfyChild, lease });
  } finally {
    await cleanup();
    for (const sig of ["SIGINT", "SIGTERM", "SIGBREAK"]) process.removeListener(sig, onSig);
  }
}

// waitMsFromEnv: per-task GPU-lock wait window threaded by the Go harness
// (GPU_LOCK_WAIT_MS). null when unset (acquireGpuLock's own default applies).
function waitMsFromEnv() {
  const v = process.env.GPU_LOCK_WAIT_MS;
  if (v == null || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) && n >= 0 ? n : null;
}
