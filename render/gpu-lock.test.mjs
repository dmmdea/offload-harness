// node --test render/gpu-lock.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { rmSync, mkdtempSync } from "node:fs";
import { acquireGpuLock, isStale, memoryStack } from "./gpu-lock.mjs";

test("acquire is exclusive; second acquire fails fast", async () => {
  const dir = mkdtempSync(join(tmpdir(), "gpulock-"));
  const lock = join(dir, "gpu.lock");
  const a = await acquireGpuLock({ lockPath: lock, waitMs: 0 });
  assert.ok(a, "first acquire should succeed");
  const b = await acquireGpuLock({ lockPath: lock, waitMs: 0 });
  assert.equal(b, null, "second acquire should fail while held");
  a.release();
  const c = await acquireGpuLock({ lockPath: lock, waitMs: 0 });
  assert.ok(c, "acquire should succeed after release");
  c.release();
  rmSync(dir, { recursive: true, force: true });
});

test("a stale lock (old mtime, dead pid) is reclaimable", async () => {
  // A lock whose recorded pid is not alive AND is older than ttl is stale.
  assert.equal(isStale({ pid: 999999999, mtimeMs: 0 }, { ttlMs: 1000, nowMs: 1_000_000 }), true);
  // A fresh lock by a live pid is NOT stale.
  assert.equal(isStale({ pid: process.pid, mtimeMs: 1_000_000 }, { ttlMs: 1000, nowMs: 1_000_500 }), false);
});

test("MEMORY_STACK is sourced from env, not a buried const (invariant 1)", () => {
  // Default (env unset) carries the two canonical CPU-only mem0 models — never unloaded.
  const def = memoryStack("");
  assert.ok(def.has("embeddinggemma"), "default keeps embeddinggemma");
  assert.ok(def.has("bge-reranker-v2-m3"), "default keeps bge-reranker-v2-m3");
  // The Go harness threads config.MemoryStack as a comma-separated env; a renamed/added
  // 3rd CPU member is honored (not silently unloaded). Trimming + empties handled.
  const env = memoryStack("embeddinggemma, bge-reranker-v2-m3 , new-cpu-embedder ,");
  assert.ok(env.has("new-cpu-embedder"), "an added CPU member from env is honored");
  assert.equal(env.size, 3);
  assert.ok(!env.has(""), "empty entries dropped");
});

test("a dead-pid lock is reclaimable IMMEDIATELY, even when young (no 1h deadlock)", async () => {
  // Regression: a holder killed without releasing (crash / Go timeout-kill) leaves a fresh
  // mtime but a dead pid. It must be reclaimable at once — the old `aged && !alive` AND-gate
  // left the GPU deadlocked for up to the full 1h TTL.
  assert.equal(isStale({ pid: 999999999, mtimeMs: 1_000_000 }, { ttlMs: 3_600_000, nowMs: 1_000_500 }), true);
  // A live holder with a young lock is still NOT stale (don't steal a working job's lock).
  assert.equal(isStale({ pid: process.pid, mtimeMs: 1_000_000 }, { ttlMs: 3_600_000, nowMs: 1_000_500 }), false);
});
