// node --test render/gpu-lock.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { rmSync, mkdtempSync, writeFileSync, readFileSync } from "node:fs";
import {
  acquireGpuLock, isStale, memoryStack,
  quiesceLlamaSwap, freeLlamaSwap, checkInheritedLease,
} from "./gpu-lock.mjs";

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

// SCHEMA NOTE: the lease record is now SHARED with internal/gpulease (Go), because
// both sides contend on the same directory. If this side kept the old flat
// {pid, mtimeMs} shape, the Go reader would find no holder pid and reclaim a lease we
// are actively holding. Holder identity therefore lives at holder.pid, and staleness
// is expressed with renewed_at_ms / expires_at_ms rather than a bare mtime + ttl.
const heldMeta = (over = {}) => ({
  epoch: 1,
  class: "media",
  holder: { pid: process.pid, start_time_ms: 0 },
  acquired_at_ms: 1_000_000,
  renewed_at_ms: 1_000_000,
  expires_at_ms: 2_000_000,
  ...over,
});

test("a lease whose holder is dead is reclaimable", async () => {
  assert.equal(isStale(heldMeta({ holder: { pid: 999999999, start_time_ms: 0 }, expires_at_ms: 0 }),
    { nowMs: 1_000_000 }), true);
  // A fresh lease held by a live pid is NOT stale.
  assert.equal(isStale(heldMeta(), { nowMs: 1_000_500 }), false);
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

test("a dead-pid lease is reclaimable IMMEDIATELY, even when young (no 1h deadlock)", async () => {
  // Regression: a holder killed without releasing (crash / Go timeout-kill) leaves a fresh
  // record but a dead pid. It must be reclaimable at once — an `aged && !alive` AND-gate
  // once left the GPU deadlocked for up to the full 1h TTL.
  assert.equal(isStale(heldMeta({ holder: { pid: 999999999, start_time_ms: 0 } }),
    { nowMs: 1_000_500 }), true);
  // A live holder with a young lease is still NOT stale (don't steal a working job's slot).
  assert.equal(isStale(heldMeta(), { nowMs: 1_000_500 }), false);
});

// The reclaim rule is a CONJUNCTION and both halves are load-bearing. It mirrors
// gpulease.Reclaimable exactly; if these two implementations drift, Go and Node will
// disagree about who owns the GPU.
test("a stale heartbeat ALONE does not reclaim (a descheduled bench keeps its slot)", () => {
  const nowMs = 5_000_000;
  assert.equal(isStale(heldMeta({
    renewed_at_ms: nowMs - 30 * 60_000,   // very stale heartbeat
    expires_at_ms: nowMs + 15 * 60_000,   // but the declared window is still open
  }), { nowMs }), false);
});

test("stale heartbeat AND expired window reclaims (the dead --detach proxy case)", () => {
  const nowMs = 5_000_000;
  assert.equal(isStale(heldMeta({
    renewed_at_ms: nowMs - 30 * 60_000,
    expires_at_ms: nowMs - 10 * 60_000,
  }), { nowMs }), true);
});

// --- the drain -------------------------------------------------------------
// MEASURED on llama-swap v242: an unload issued mid-generation returned in 1,265ms
// without draining and the in-flight request died at 4,107ms with 502 Bad Gateway.
// The unload route does not honour in-flight work, so the caller must drain first.
// The signal is llama-server's /slots (is_processing), verified true throughout a
// 23s/1500-token generation and false on completion.

const slotsResponse = (processing) => ({
  ok: true, status: 200, json: async () => [{ id: 0, is_processing: processing }],
});

test("quiesce waits while a slot is processing, then reports a verified drain", async () => {
  let polls = 0;
  const fetchImpl = async () => { polls++; return slotsResponse(polls < 3); };
  const r = await quiesceLlamaSwap(["m"], { fetchImpl, pollMs: 1, timeoutMs: 5_000 });
  assert.equal(r.drained, true, "drain verified once no slot reports is_processing");
  assert.ok(polls >= 3, `expected to keep polling while busy, polled ${polls}`);
});

test("quiesce treats a 404 (model not loaded) as nothing to drain", async () => {
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async () => ({ ok: false, status: 404, json: async () => ({}) }),
    pollMs: 1, timeoutMs: 1_000,
  });
  assert.equal(r.drained, true);
});

test("quiesce does NOT claim a drain it could not verify", async () => {
  // An older llama-server, or --no-slots, means we cannot observe the tier. Reporting
  // drained:true here would be a lie that silently reintroduces the 502.
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async () => { throw new Error("connection refused"); },
    pollMs: 1, timeoutMs: 1_000, graceMs: 1,
  });
  assert.equal(r.drained, false, "unverifiable must not report as drained");
  assert.deepEqual(r.unknown, ["m"], "the unobservable tier is named so the caller can log it");
});

test("quiesce gives up after timeoutMs rather than blocking a render forever", async () => {
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async () => slotsResponse(true), // never idle
    pollMs: 1, timeoutMs: 30,
  });
  assert.equal(r.drained, false, "a stuck tier must not deadlock the queue");
});

test("freeLlamaSwap DRAINS BEFORE it unloads, and never unloads the memory stack", async () => {
  const order = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) {
      return { ok: true, status: 200, json: async () => ({ data: [
        { id: "gemma-4-e4b" }, { id: "embeddinggemma" }, { id: "bge-reranker-v2-m3" },
      ] }) };
    }
    if (u.includes("/api/models/unload/")) {
      order.push("unload:" + u.split("/api/models/unload/")[1]);
      return { ok: true, status: 200, json: async () => ({}) };
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  try {
    await freeLlamaSwap("http://x", {
      quiesce: async (ids) => { order.push("drain:" + ids.join("+")); return { drained: true, waitedMs: 0, unknown: [] }; },
    });
  } finally { globalThis.fetch = realFetch; }

  assert.equal(order[0], "drain:gemma-4-e4b", "the drain must precede every unload");
  assert.ok(order.includes("unload:gemma-4-e4b"), "the GPU tier is unloaded");
  assert.ok(!order.some((o) => o.includes("embeddinggemma")),
    "invariant 1: the CPU memory stack is never drained or unloaded");
  assert.ok(!order.some((o) => o.includes("bge-reranker")),
    "invariant 1: the reranker is never drained or unloaded");
});

// --- fencing ---------------------------------------------------------------

test("release is epoch-guarded: a fenced-out holder must not delete the current lease", async () => {
  const dir = mkdtempSync(join(tmpdir(), "gpulease-"));
  const lock = join(dir, "gpu", "lease");
  const a = await acquireGpuLock({ lockPath: lock, waitMs: 0 });
  assert.ok(a, "acquire");
  // Someone else reclaims and takes over: same dir, higher epoch.
  writeFileSync(join(lock, "meta.json"), JSON.stringify({
    epoch: a.epoch + 1, class: "text",
    holder: { pid: process.pid, start_time_ms: 0 },
    acquired_at_ms: Date.now(), renewed_at_ms: Date.now(), expires_at_ms: Date.now() + 60_000,
  }));
  a.release(); // the fenced-out holder tries to clean up
  const after = JSON.parse(readFileSync(join(lock, "meta.json"), "utf8"));
  assert.equal(after.epoch, a.epoch + 1,
    "the CURRENT holder's lease survived a stale holder's release");
  rmSync(dir, { recursive: true, force: true });
});

test("checkInheritedLease fences a resumed process out (the closing-lid case)", () => {
  assert.equal(checkInheritedLease({ dir: "X", epoch: 5 }, () => ({ epoch: 5 })), true);
  assert.equal(checkInheritedLease({ dir: "X", epoch: 5 }, () => ({ epoch: 6 })), false,
    "a process that slept through a takeover must NOT act on the GPU");
  assert.equal(checkInheritedLease({ dir: "X", epoch: 5 }, () => null), false,
    "a vanished lease is not ours either");
});
