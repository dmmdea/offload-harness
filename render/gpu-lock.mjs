// gpu-lock.mjs — a single-slot, cross-process GPU lock + GPU-free helpers for the
// local-offload generation runners. The 8GB GPU is shared with llama-swap (:11436)
// and ComfyUI (:8188); only ONE GPU-heavy job may run at a time. The lock is a
// directory (mkdir is atomic on every OS, no deps) holding a meta.json {pid,startedAt}.
// A crashed holder leaves a stale lock; we reclaim it when its pid is dead AND it is
// older than ttl. No npm dependencies.
import { mkdirSync, writeFileSync, readFileSync, rmSync, statSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { ensureComfy as defaultEnsureComfy } from "./comfy-lifecycle.mjs";

const DEFAULT_TTL_MS = 60 * 60 * 1000; // 1h — a real video gen can take many minutes

function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}

// isStale: reclaim a lock as soon as its recorded holder pid is confirmed DEAD — the
// pid-liveness check is the whole point, so don't AND it behind the long TTL (that left the
// single-slot GPU deadlocked for up to a full hour after a crash / timeout-kill). The TTL
// stays as the fallback when the holder can't be identified (no pid) and as a pid-recycle
// backstop for a still-"alive" but ancient lock.
export function isStale(meta, { ttlMs = DEFAULT_TTL_MS, nowMs = Date.now() } = {}) {
  if (!meta) return true;
  if (typeof meta.pid === "number" && !pidAlive(meta.pid)) return true;
  return nowMs - (meta.mtimeMs ?? 0) > ttlMs;
}

function readMeta(lockPath) {
  try {
    const m = JSON.parse(readFileSync(join(lockPath, "meta.json"), "utf8"));
    m.mtimeMs = statSync(join(lockPath, "meta.json")).mtimeMs;
    return m;
  } catch { return null; }
}

// acquireGpuLock: returns {release()} on success, or null if held (after waiting waitMs).
// DEFAULT waitMs is a GENEROUS QUEUE WAIT (30min), set 2026-06-30 for the "serialize GPU workloads" goal.
// The aim is to ORGANIZE concurrent GPU jobs into a serial queue, NOT to cancel them: a job whose slot is
// busy WAITS its turn and then runs — it never drops its work. 30min covers queuing behind even the longest
// job (video ~20min) with margin. A CRASHED holder never deadlocks the queue: isStale reclaims a dead-pid
// lock immediately (and any lock past the 1h TTL). Callers with a different window (video 20min, audio 2min)
// set GPU_LOCK_WAIT_MS, which withGpuSlot passes through and overrides this default. Sequential gen (the
// serial hero stage runs one image at a time) never contends, so it acquires instantly.
export async function acquireGpuLock({ lockPath, waitMs = 30 * 60 * 1000, ttlMs = DEFAULT_TTL_MS } = {}) {
  const deadline = Date.now() + waitMs;
  for (;;) {
    try {
      mkdirSync(lockPath); // atomic: throws EEXIST if held
      writeFileSync(join(lockPath, "meta.json"), JSON.stringify({ pid: process.pid, startedAt: Date.now() }));
      let released = false;
      return {
        release() {
          if (released) return;
          released = true;
          try { rmSync(lockPath, { recursive: true, force: true }); } catch {}
        },
      };
    } catch (e) {
      if (e.code !== "EEXIST") throw e;
      // Held — reclaim if stale, else wait.
      if (isStale(readMeta(lockPath), { ttlMs })) {
        try { rmSync(lockPath, { recursive: true, force: true }); } catch {}
        continue; // retry the mkdir immediately
      }
      if (Date.now() >= deadline) return null;
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
}

// MEMORY_STACK: the always-loaded, CPU-only mem0 models (they hold ZERO GPU VRAM).
// freeLlamaSwap must NEVER unload these — the unload-ALL route did, needlessly tearing down
// the load-bearing memory stack on every gen job for no VRAM benefit. Everything else on
// llama-swap is GPU-resident + swappable, so freeing it per-model gives the render the GPU.
//
// SOURCED FROM CONFIG/ENV, not a buried const (invariant 1): the Go harness threads the
// config's MemoryStack as the MEMORY_STACK env (comma-separated), so a renamed/added 3rd
// CPU member is honored instead of silently unloaded. The literal default below is the
// fallback when the env is unset (e.g. a direct CLI run).
const DEFAULT_MEMORY_STACK = ["embeddinggemma", "bge-reranker-v2-m3"];
export function memoryStack(env = process.env.MEMORY_STACK) {
  if (env && env.trim()) {
    return new Set(env.split(",").map((s) => s.trim()).filter(Boolean));
  }
  return new Set(DEFAULT_MEMORY_STACK);
}

// freeLlamaSwap: free the GPU-resident llama-swap models so their VRAM goes to a gen job,
// while leaving the CPU memory stack warm. Per-model unload via the proven
// `/api/models/unload/<model>` route (mirrors sttclient.Unload), NOT the unload-ALL route.
// Best-effort — never throws (llama-swap may be down; unloading a not-loaded model is a no-op).
export async function freeLlamaSwap(api = process.env.LLAMA_SWAP_API || "http://localhost:11436") {
  const keep = memoryStack();
  try {
    const r = await fetch(api + "/v1/models");
    const j = await r.json();
    const ids = (j.data || []).map((m) => m.id).filter((id) => id && !keep.has(id));
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

// defaultLockPath: the single shared GPU lock all gen runners contend on.
export function defaultLockPath() {
  return process.env.GPU_LOCK || join(tmpdir(), "local-offload-gpu.lock");
}

// withGpuSlot centralizes the single-slot GPU lifecycle every gen runner shared (lifted
// verbatim from comfy-generate.mjs, the only one with the full guarded teardown + signal
// handlers + double-run guard). It:
//   1. acquires the cross-process GPU lock (honoring noLock); a busy slot THROWS GPU-busy
//      (the runner exits non-zero → the Go wrapper maps it to a clean defer — invariant 4),
//   2. freeLlamaSwap() so the render gets the whole 8GB (CPU mem-stack stays warm — inv. 1),
//   3. optionally ensureComfy() (skipped when comfyManaged:false — the TTS path),
//   4. awaits fn(),
//   5. runs ONE guarded teardown (zero-always-warm — invariant 3): freeComfy() + kill a
//      ComfyUI we spawned (unless keepComfy) + lock.release(). The teardown is guarded so
//      neither the finally nor a SIGINT/SIGTERM/SIGBREAK can double-run it; a graceful
//      signal releases the lock+VRAM instead of leaking them (a forced SIGKILL is still
//      backstopped by the Go wrapper's process-tree kill + defer /free).
// waitMs comes from GPU_LOCK_WAIT_MS (the Go harness threads the per-task value so a queued
// TTS isn't starved by a long video job); unset → acquireGpuLock's default. Deps
// (acquire/freeLlamaSwap/ensureComfy/freeComfy) are injectable for tests only.
export async function withGpuSlot(opts, fn) {
  const {
    noLock = false,
    keepComfy = false,
    comfyManaged = true,
    reserveVram,
    lockPath = defaultLockPath(),
    acquire = acquireGpuLock,
    freeLlamaSwap: freeLS = freeLlamaSwap,
    ensureComfy = defaultEnsureComfy,
    freeComfy: freeCfy = freeComfy,
  } = opts || {};

  const lock = noLock
    ? { release() {} }
    : await acquire({ lockPath, ...(waitMsFromEnv() != null ? { waitMs: waitMsFromEnv() } : {}) });
  if (!lock) throw new Error("GPU is busy (another gen job holds the lock); try again later or --no-lock");

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
    await freeLS();                       // give the render the whole 8GB (CPU mem-stack stays warm)
    if (comfyManaged) comfyChild = await ensureComfy(reserveVram != null ? { reserveVram } : {});
    return await fn({ comfyChild });
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
