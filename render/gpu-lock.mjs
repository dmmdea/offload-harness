// gpu-lock.mjs — a single-slot, cross-process GPU lock + GPU-free helpers for the
// local-offload generation runners. The 8GB GPU is shared with llama-swap (:11436)
// and ComfyUI (:8188); only ONE GPU-heavy job may run at a time. The lock is a
// directory (mkdir is atomic on every OS, no deps) holding a meta.json {pid,startedAt}.
// A crashed holder leaves a stale lock; we reclaim it when its pid is dead AND it is
// older than ttl. No npm dependencies.
import { mkdirSync, writeFileSync, readFileSync, rmSync, statSync } from "node:fs";
import { join } from "node:path";

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
export async function acquireGpuLock({ lockPath, waitMs = 5 * 60 * 1000, ttlMs = DEFAULT_TTL_MS } = {}) {
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
const MEMORY_STACK = new Set(["embeddinggemma", "bge-reranker-v2-m3"]);

// freeLlamaSwap: free the GPU-resident llama-swap models so their VRAM goes to a gen job,
// while leaving the CPU memory stack warm. Per-model unload via the proven
// `/api/models/unload/<model>` route (mirrors sttclient.Unload), NOT the unload-ALL route.
// Best-effort — never throws (llama-swap may be down; unloading a not-loaded model is a no-op).
export async function freeLlamaSwap(api = process.env.LLAMA_SWAP_API || "http://localhost:11436") {
  try {
    const r = await fetch(api + "/v1/models");
    const j = await r.json();
    const ids = (j.data || []).map((m) => m.id).filter((id) => id && !MEMORY_STACK.has(id));
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
