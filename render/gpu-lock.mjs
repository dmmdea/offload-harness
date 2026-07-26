// gpu-lock.mjs — the Node side of the machine-wide, FENCED GPU lease.
//
// One GPU is shared by llama-swap (:11436) and ComfyUI (:8188), so only ONE GPU-heavy
// job may run at a time. This file used to own that arbitration alone — and that was
// the defect: TEXT work never took the lock, so a dispatched render called
// freeLlamaSwap() and unloaded every GPU-resident model out from under an in-flight
// benchmark. Arbitration now lives in internal/gpulease (Go) and this file is a
// PARTICIPANT in the same lease, not a separate lock.
//
// THREE THINGS CHANGED, each fixing a measured defect:
//
//  1. THE LOCK IS MACHINE-WIDE. The old default was join(tmpdir(), ...), which on
//     Windows is PER-USER — a process in another security context silently took a
//     DIFFERENT lock and mutual exclusion evaporated. The default is now
//     <state-root>/gpu/lease, the exact path internal/gpulease uses, so Go and Node
//     contend on ONE object. The meta.json SCHEMA IS SHARED for the same reason: if
//     this file wrote its old {pid,startedAt} shape, the Go reader would see no
//     holder pid and reclaim a lease we are actively holding.
//
//  2. freeLlamaSwap IS HOISTED OUT OF THE PER-JOB PATH. It used to run inside
//     withGpuSlot, i.e. once per job — that arithmetic is why the server log holds
//     3,356 unloads. When a lease is INHERITED from the Go side (GPU_LEASE_DIR +
//     GPU_LEASE_EPOCH in env) this file skips both acquisition and the unload: the
//     lease holder already did it once for the whole batch.
//
//  3. UNLOADING NOW DRAINS FIRST. Measured on llama-swap v242: POST
//     /api/models/unload/<id> returned in 1,265ms WITHOUT draining and killed an
//     in-flight generation, which died at 4,107ms with 502 Bad Gateway. The unload
//     route does not honour in-flight work, so the CALLER must. quiesceLlamaSwap
//     polls each model's upstream /slots until no slot reports is_processing.
//
// No npm dependencies.
import { mkdirSync, writeFileSync, readFileSync, rmSync, statSync } from "node:fs";
import { join } from "node:path";
import { tmpdir, platform } from "node:os";
import { ensureComfy as defaultEnsureComfy } from "./comfy-lifecycle.mjs";

const DEFAULT_TTL_MS = 60 * 60 * 1000; // 1h — a real video gen can take many minutes
// Mirrors gpulease.DefaultHeartbeatTTL. Deliberately generous: a holder under heavy
// load can be descheduled well past a short TTL, and reclaiming there would hand the
// card away mid-run. Staleness alone NEVER reclaims — see isStale.
const DEFAULT_HEARTBEAT_TTL_MS = 120 * 1000;

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

// legacyLockPath: the pre-machine-wide location. We ALSO create it for one release
// as mixed-version insurance — a not-yet-upgraded binary on this box still checks
// the old path, and without this dual-write it would see a free lock and start a
// second GPU job. Deployments here drift (two machines, hand-deployed), so this is
// not hypothetical.
export function legacyLockPath() {
  return join(tmpdir(), "local-offload-gpu.lock");
}

function epochPath(lockPath) {
  return join(lockPath, "..", "epoch");
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
    const m = JSON.parse(readFileSync(join(lockPath, "meta.json"), "utf8"));
    m.mtimeMs = statSync(join(lockPath, "meta.json")).mtimeMs;
    return m;
  } catch { return null; }
}

// bumpEpoch increments the monotonic fencing counter that lives OUTSIDE the lease
// dir, so removing a lease can never reset it. Best-effort: a counter we cannot read
// starts from the current time in ms, which is still monotonic in practice and can
// never collide downward with a previously issued epoch.
function bumpEpoch(lockPath) {
  const p = epochPath(lockPath);
  let cur = 0;
  try { cur = parseInt(readFileSync(p, "utf8").trim(), 10) || 0; } catch { cur = 0; }
  const next = cur + 1;
  try { writeFileSync(p, String(next)); } catch { /* non-fatal: fence degrades, lock still holds */ }
  return next;
}

// acquireGpuLock: returns {release(), epoch} on success, or null if held (after waiting waitMs).
// The wait is a QUEUE, not a cancellation: a job whose slot is busy waits its turn.
// A crashed holder never deadlocks it — isStale reclaims a dead-pid lease immediately.
export async function acquireGpuLock({
  lockPath, waitMs = 30 * 60 * 1000, ttlMs = DEFAULT_TTL_MS,
  heartbeatTtlMs = DEFAULT_HEARTBEAT_TTL_MS, klass = "media", reason = "",
} = {}) {
  const deadline = Date.now() + waitMs;
  for (;;) {
    try {
      mkdirSync(lockPath, { recursive: true }); // recursive:true creates <root>/gpu on a fresh box
    } catch (e) {
      if (e.code !== "EEXIST") throw e;
    }
    // mkdir with recursive:true does NOT throw on an existing dir, so existence alone
    // cannot signal "held". The meta file is the real token: claim it exclusively.
    let claimed = false;
    try {
      const now = Date.now();
      const epoch = bumpEpoch(lockPath);
      // wx: fail if it already exists — this is the atomic claim.
      writeFileSync(join(lockPath, "meta.json"), JSON.stringify({
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
      claimed = true;
      let released = false;
      return {
        epoch,
        release() {
          if (released) return;
          released = true;
          // Epoch-guarded: never delete a lease that is no longer ours. Leaking one
          // is recoverable; silently handing the GPU to a third party is not.
          const cur = readMeta(lockPath);
          if (cur && cur.epoch !== epoch) return;
          try { rmSync(lockPath, { recursive: true, force: true }); } catch {}
          try { rmSync(legacyLockPath(), { recursive: true, force: true }); } catch {}
        },
      };
    } catch (e) {
      if (claimed || e.code !== "EEXIST") throw e;
      // Held — reclaim if stale, else wait.
      if (isStale(readMeta(lockPath), { heartbeatTtlMs })) {
        try { rmSync(join(lockPath, "meta.json"), { force: true }); } catch {}
        continue; // retry the claim immediately
      }
      if (Date.now() >= deadline) return null;
      await new Promise((r) => setTimeout(r, 1000));
    } finally {
      if (claimed) {
        // Mixed-version insurance: an older binary on this box still checks the
        // legacy per-user path. Best-effort; never fatal.
        try { mkdirSync(legacyLockPath(), { recursive: true }); } catch {}
      }
    }
  }
}

// inheritedLease: when the Go side already holds the lease it threads the directory
// and its fencing epoch down to us. We then do NOT acquire (we would deadlock against
// our own holder) and do NOT unload (the holder did it once for the whole batch).
export function inheritedLease(env = process.env) {
  const dir = (env.GPU_LEASE_DIR || "").trim();
  const epoch = (env.GPU_LEASE_EPOCH || "").trim();
  if (!dir || !epoch) return null;
  return { dir, epoch: Number(epoch) };
}

// checkInheritedLease is the FENCE for an inherited lease: before an irreversible
// action, confirm the epoch we were handed is still the current one. A closing lid
// is not a crash — the process resumes, and without this it would resume and act on
// top of whoever holds the card now.
export function checkInheritedLease(lease, read = readMeta) {
  if (!lease) return true;
  const meta = read(lease.dir);
  if (!meta) return false;
  return Number(meta.epoch) === Number(lease.epoch);
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

// quiesceLlamaSwap: wait for in-flight work on `ids` to finish before unloading them.
//
// MEASURED, not defensive: on llama-swap v242 an unload issued during a generation
// returned in 1,265ms without draining and the in-flight request died at 4,107ms with
// 502 Bad Gateway. The unload route does not honour in-flight work, so a caller that
// wants to avoid killing someone's request must drain first.
//
// Signal: llama-server's own /slots via llama-swap's /upstream/<id>/slots, where
// is_processing is true exactly while a slot is generating (verified against a
// 23s/1500-token generation). A model that is not loaded 404s — nothing to drain.
//
// FAIL-SAFE, NOT FAIL-OPEN-SILENT: if /slots cannot be read (older llama-server,
// --no-slots), we cannot prove the tier is idle, so we wait out a short grace period
// and report it rather than pretending the drain succeeded. Returns {drained, waitedMs,
// unknown:[ids]} so the caller can log honestly.
export async function quiesceLlamaSwap(ids, {
  api = process.env.LLAMA_SWAP_API || "http://localhost:11436",
  timeoutMs = 60_000, pollMs = 500, graceMs = 1_500, fetchImpl = fetch, nowFn = Date.now,
} = {}) {
  const deadline = nowFn() + timeoutMs;
  const unknown = new Set();
  const started = nowFn();

  const busy = async (id) => {
    try {
      const r = await fetchImpl(`${api}/upstream/${id}/slots`);
      if (r.status === 404) return false;      // not loaded => nothing in flight
      if (!r.ok) { unknown.add(id); return false; }
      const j = await r.json();
      const slots = Array.isArray(j) ? j : [j];
      return slots.some((s) => s && s.is_processing === true);
    } catch { unknown.add(id); return false; }
  };

  for (;;) {
    const flags = await Promise.all(ids.map(busy));
    if (!flags.some(Boolean)) break;
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
// job, while leaving the CPU memory stack warm. DRAINS FIRST (see quiesceLlamaSwap) —
// unloading without draining kills in-flight requests, measured.
//
// Call this ONCE PER LEASE, not per job. Per-job is what produced 3,356 unloads.
// Best-effort — never throws (llama-swap may be down; unloading a not-loaded model is
// a no-op).
export async function freeLlamaSwap(api = process.env.LLAMA_SWAP_API || "http://localhost:11436", opts = {}) {
  const { quiesce = quiesceLlamaSwap, drainTimeoutMs = 60_000, log = () => {} } = opts;
  const keep = memoryStack();
  try {
    const r = await fetch(api + "/v1/models");
    const j = await r.json();
    const ids = (j.data || []).map((m) => m.id).filter((id) => id && !keep.has(id));
    if (ids.length === 0) return;
    const res = await quiesce(ids, { api, timeoutMs: drainTimeoutMs });
    if (!res.drained) {
      // Loud, not silent: proceeding here may kill someone's in-flight request. The
      // alternative — blocking the render forever behind a stuck tier — is worse, so
      // we proceed and say so.
      log(`freeLlamaSwap: proceeding without a verified drain after ${res.waitedMs}ms` +
          (res.unknown.length ? ` (could not read /slots for: ${res.unknown.join(",")})` : ""));
    }
    await Promise.all(ids.map((id) =>
      fetch(api + "/api/models/unload/" + id, { method: "POST" }).catch(() => {})));
  } catch {}
}

// freeComfy: tell ComfyUI to drop loaded models + free VRAM after a job (zero-warm).
export async function freeComfy(api = process.env.COMFY_API || "http://127.0.0.1:8188") {
  try {
    await fetch(api + "/free", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ unload_models: true, free_memory: true }),
    });
  } catch {}
}

// withGpuSlot centralizes the single-slot GPU lifecycle every gen runner shares. It:
//   1. takes the GPU lease — UNLESS one was inherited from the Go holder (then it is
//      already ours) or noLock is set; a busy slot THROWS GPU-busy (the runner exits
//      non-zero → the Go wrapper maps it to a clean defer),
//   2. freeLlamaSwap() so the render gets the whole GPU — SKIPPED when the lease was
//      inherited, because the holder already unloaded once for the whole batch,
//   3. optionally ensureComfy(); warm:true is the BATCH-SESSION mode (the checkpoint
//      loads once for N renders),
//   4. awaits fn(),
//   5. runs ONE guarded teardown: freeComfy() + kill a ComfyUI we spawned + release.
//      Guarded so neither the finally nor a SIGINT/SIGTERM/SIGBREAK double-runs it.
// Deps (acquire/freeLlamaSwap/ensureComfy/freeComfy) are injectable for tests only.
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
  } = opts || {};

  // An inherited lease is already held by our parent: acquiring again would contend
  // with ourselves, and unloading again would repeat the per-job teardown this change
  // exists to eliminate.
  const lock = noLock || lease
    ? { release() {} }
    : await acquire({ lockPath, ...(waitMsFromEnv() != null ? { waitMs: waitMsFromEnv() } : {}) });
  if (!lock) throw new Error("GPU is busy (another job holds the lease); try again later or --no-lock");

  let comfyChild = null;
  let cleaning = false;
  const cleanup = async () => {
    if (cleaning) return; cleaning = true;
    if (comfyManaged) { try { await freeCfy(); } catch {} }
    if (comfyChild && !keepComfy) { try { comfyChild.kill(); } catch {} }
    try { lock.release(); } catch {}
  };
  const onSig = async () => { await cleanup(); process.exit(130); };
  for (const sig of ["SIGINT", "SIGTERM", "SIGBREAK"]) process.on(sig, onSig);
  try {
    if (!lease) {
      await freeLS();                     // give the render the whole GPU (mem-stack stays warm)
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
