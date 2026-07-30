// node --test render/gpu-lock.test.mjs
//
// This file no longer tests acquisition, staleness or the epoch counter: Node does not
// implement them any more. internal/gpulease (Go) is the single implementation, and the
// duplicate was deleted because two languages independently implementing one concurrency
// rule produced a fresh divergence in every review round.
//
// What is tested here is what Node still owns: reading the shared record, the fence, the
// once-per-lease unload election, and the drain.
import { test } from "node:test";
import assert from "node:assert";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { rmSync, mkdtempSync, writeFileSync } from "node:fs";
import {
  memoryStack, quiesceLlamaSwap, freeLlamaSwap,
  checkInheritedLease, claimLeaseUnload, readLease, inheritedLease,
} from "./gpu-lock.mjs";

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

// --- reading the shared record ---------------------------------------------
// The schema is owned by Go. Node only reads it, but it must read it CORRECTLY: the
// fence compares epochs, and a record Node cannot parse reads as "fenced out", which
// would block every render.

test("readLease parses a Go-shaped record, and returns null for a missing one", () => {
  const dir = mkdtempSync(join(tmpdir(), "gpulease-read-"));
  assert.equal(readLease(dir), null, "absent record => null");
  writeFileSync(join(dir, "meta.json"), JSON.stringify({
    epoch: 12, class: "media", holder: { pid: 4242, start_time_ms: 99 },
    acquired_at_ms: 1, renewed_at_ms: 2, expires_at_ms: 3,
  }));
  const m = readLease(dir);
  assert.equal(m.epoch, 12);
  assert.equal(m.class, "media");
  assert.equal(m.holder.pid, 4242, "the nested holder.pid must survive the round trip");
  rmSync(dir, { recursive: true, force: true });
});

test("inheritedLease requires BOTH dir and epoch, and carries the class", () => {
  assert.equal(inheritedLease({}), null);
  assert.equal(inheritedLease({ GPU_LEASE_DIR: "X" }), null, "a dir with no epoch is not a lease");
  assert.equal(inheritedLease({ GPU_LEASE_EPOCH: "3" }), null, "an epoch with no dir is not a lease");
  assert.deepEqual(inheritedLease({ GPU_LEASE_DIR: "X", GPU_LEASE_EPOCH: "3", GPU_LEASE_CLASS: "text" }),
    { dir: "X", epoch: 3, class: "text" });
});

// --- the fence -------------------------------------------------------------

test("checkInheritedLease fences a resumed process out (the closing-lid case)", () => {
  assert.equal(checkInheritedLease({ dir: "X", epoch: 5 }, () => ({ epoch: 5 })), true);
  assert.equal(checkInheritedLease({ dir: "X", epoch: 5 }, () => ({ epoch: 6 })), false,
    "a process that slept through a takeover must NOT act on the GPU");
  assert.equal(checkInheritedLease({ dir: "X", epoch: 5 }, () => null), false,
    "a vanished lease is not ours either");
});

// --- once-per-lease unload election ----------------------------------------

test("claimLeaseUnload elects exactly one unloader per lease epoch", () => {
  const dir = mkdtempSync(join(tmpdir(), "gpulease-unload-"));
  const lease = { dir, epoch: 11 };
  const winners = [claimLeaseUnload(lease), claimLeaseUnload(lease), claimLeaseUnload(lease)]
    .filter(Boolean).length;
  assert.equal(winners, 1, "exactly one job per lease may unload");
  // A NEW lease (higher epoch) gets its own unload — the card was handed over.
  assert.equal(claimLeaseUnload({ dir, epoch: 12 }), true, "a new lease unloads again");
  rmSync(dir, { recursive: true, force: true });
});

test("claimLeaseUnload unloads when it cannot tell (correctness over saving a teardown)", () => {
  assert.equal(claimLeaseUnload(null), true);
  assert.equal(claimLeaseUnload({ dir: join(tmpdir(), "definitely-not-here-xyz"), epoch: 1 }), true);
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
  const fetchImpl = async (url) => {
    if (String(url).endsWith("/running")) {
      return { ok: true, status: 200, json: async () => ({ running: [{ model: "m" }] }) };
    }
    polls++;
    return slotsResponse(polls < 3);
  };
  const r = await quiesceLlamaSwap(["m"], { fetchImpl, pollMs: 1, timeoutMs: 5_000 });
  assert.equal(r.drained, true, "drain verified once no slot reports is_processing");
  assert.ok(polls >= 3, `expected to keep polling while busy, polled ${polls}`);
});

test("quiesce treats a 404 for an UNLOADED model as nothing to drain", async () => {
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async (url) => {
      if (String(url).endsWith("/running")) {
        return { ok: true, status: 200, json: async () => ({ running: [] }) }; // m is not loaded
      }
      return { ok: false, status: 404, json: async () => ({}) };
    },
    pollMs: 1, timeoutMs: 1_000, graceMs: 1,
  });
  assert.equal(r.drained, true, "an unloaded model has nothing in flight");
  assert.deepEqual(r.unknown, []);
});

// THE FAIL-OPEN THIS CLOSES: a LOADED upstream with no /slots route also 404s — any
// non-llama.cpp backend on :11436, and whisper is one. Reporting that as a verified
// drain let the unload fire into in-flight work, which is the measured 502.
test("quiesce does NOT call a 404 idle when /running says the model IS loaded", async () => {
  const r = await quiesceLlamaSwap(["whisper-stt"], {
    fetchImpl: async (url) => {
      if (String(url).endsWith("/running")) {
        return { ok: true, status: 200, json: async () => ({ running: [{ model: "whisper-stt" }] }) };
      }
      return { ok: false, status: 404, json: async () => ({}) }; // loaded, but no /slots route
    },
    pollMs: 1, timeoutMs: 1_000, graceMs: 1,
  });
  assert.equal(r.drained, false, "a loaded-but-unobservable tier must not report as drained");
  assert.deepEqual(r.unknown, ["whisper-stt"], "the unobservable tier is named for the caller's log");
});

test("quiesce clears a transient unknown once the tier becomes observable", async () => {
  let n = 0;
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async (url) => {
      if (String(url).endsWith("/running")) {
        return { ok: true, status: 200, json: async () => ({ running: [{ model: "m" }] }) };
      }
      n++;
      if (n === 1) return { ok: false, status: 502, json: async () => ({}) }; // one blip
      return { ok: true, status: 200, json: async () => [{ id: 0, is_processing: n < 3 }] };
    },
    pollMs: 1, timeoutMs: 5_000, graceMs: 1,
  });
  assert.equal(r.drained, true, "a resolved transient must not leave the drain marked unverified");
  assert.deepEqual(r.unknown, []);
});

test("quiesce gives up after timeoutMs rather than blocking a render forever", async () => {
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async (url) => {
      if (String(url).endsWith("/running")) {
        return { ok: true, status: 200, json: async () => ({ running: [{ model: "m" }] }) };
      }
      return slotsResponse(true); // never idle
    },
    pollMs: 1, timeoutMs: 30,
  });
  assert.equal(r.drained, false, "a stuck tier must not deadlock the queue");
});

test("freeLlamaSwap DRAINS BEFORE it unloads, and never unloads the memory stack", async () => {
  const order = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) {
      return { ok: true, status: 200, json: async () => ({ data: [
        { id: "gemma-4-e4b" }, { id: "embeddinggemma" }, { id: "bge-reranker-v2-m3" },
      ] }) };
    }
    if (u.endsWith("/running")) {
      // The GPU tier is loaded; the CPU memory stack is loaded too (it always is).
      // Neither fact may leak an unload of the latter.
      return { ok: true, status: 200, json: async () => ({ running: [
        { model: "gemma-4-e4b" }, { model: "embeddinggemma" }, { model: "bge-reranker-v2-m3" },
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

test("freeLlamaSwap does NOT unload when it cannot list models (better a slow render than a dead tier)", async () => {
  const calls = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) return { ok: false, status: 500, json: async () => ({}) };
    calls.push(u);
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const logged = [];
  try {
    await freeLlamaSwap("http://x", { log: (m) => logged.push(m) });
  } finally { globalThis.fetch = realFetch; }
  assert.equal(calls.length, 0, "nothing was unloaded");
  assert.ok(logged.some((m) => /NOT unloading/.test(m)), "and it said so, loudly");
});

// --- llama-swap v208 compatibility (live-found on lenovo-m720q) ---------------
// MEASURED on llama-swap v208 (commit e8d4384): /upstream/<id>/slots is a PROXY
// route (`Any /upstream/*` -> proxyToUpstream) — requesting it for a model that
// is not loaded SWAPS THE MODEL IN. The drain protocol therefore loaded ~3GB of
// model into VRAM immediately before each render, and per-model
// POST /api/models/unload/<id> does not exist there (v208's only unload is
// GET /unload, all models), so nothing could ever be unloaded again. Renders
// then failed allocation (unet 1882MB / clip 1285MB) whenever the stolen VRAM
// had not idle-expired: fleet job success was TTL roulette.

test("quiesce NEVER probes /upstream for a model /running does not list (v208: that probe swap-loads it)", async () => {
  const urls = [];
  const r = await quiesceLlamaSwap(["gemma4-e2b", "whisper-stt"], {
    fetchImpl: async (url) => {
      urls.push(String(url));
      if (String(url).endsWith("/running")) {
        return { ok: true, status: 200, json: async () => ({ running: [] }) };
      }
      return { ok: false, status: 404, json: async () => ({}) };
    },
    pollMs: 1, timeoutMs: 1_000, graceMs: 1,
  });
  assert.equal(r.drained, true, "nothing loaded => nothing to drain");
  assert.ok(!urls.some((u) => u.includes("/upstream/")),
    `an unloaded model must never be probed via /upstream (got: ${urls.join(", ")})`);
});

test("quiesce does not touch /upstream at all when /running is unreadable", async () => {
  const urls = [];
  const r = await quiesceLlamaSwap(["m"], {
    fetchImpl: async (url) => {
      urls.push(String(url));
      if (String(url).endsWith("/running")) {
        return { ok: false, status: 500, json: async () => ({}) };
      }
      return { ok: true, status: 200, json: async () => [{ id: 0, is_processing: false }] };
    },
    pollMs: 1, timeoutMs: 200, graceMs: 1,
  });
  assert.ok(!urls.some((u) => u.includes("/upstream/")),
    "blind => hands off: probing /upstream may LOAD the model on v208");
  assert.equal(r.drained, false, "an unobservable server is not a verified drain");
  assert.deepEqual(r.unknown, ["m"], "the unobservable id is named");
});

test("freeLlamaSwap unloads ONLY models /running lists as loaded", async () => {
  const unloads = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) {
      return { ok: true, status: 200, json: async () => ({ data: [
        { id: "gemma4-e2b" }, { id: "whisper-stt" }, { id: "sdxl-turbo" },
      ] }) };
    }
    if (u.endsWith("/running")) {
      return { ok: true, status: 200, json: async () => ({ running: [{ model: "whisper-stt" }] }) };
    }
    if (u.includes("/api/models/unload/")) {
      unloads.push(u.split("/api/models/unload/")[1]);
      return { ok: true, status: 200, json: async () => ({}) };
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  try {
    await freeLlamaSwap("http://x", {
      quiesce: async () => ({ drained: true, waitedMs: 0, unknown: [] }),
    });
  } finally { globalThis.fetch = realFetch; }
  assert.deepEqual(unloads, ["whisper-stt"],
    "unloading a model that is not loaded is 10s of timeout per id on v208, for nothing");
});

test("freeLlamaSwap falls back to GET /unload (v208 unload-all) when per-model unload is unavailable", async () => {
  const calls = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) {
      return { ok: true, status: 200, json: async () => ({ data: [{ id: "whisper-stt" }] }) };
    }
    if (u.endsWith("/running")) {
      return { ok: true, status: 200, json: async () => ({ running: [{ model: "whisper-stt" }] }) };
    }
    if (u.includes("/api/models/unload/")) {
      calls.push("per-model");
      return { ok: false, status: 404, json: async () => ({}) };
    }
    if (u.endsWith("/unload") && (!init || !init.method || init.method === "GET")) {
      calls.push("unload-all");
      return { ok: true, status: 200, json: async () => ({}) };
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  try {
    await freeLlamaSwap("http://x", {
      quiesce: async () => ({ drained: true, waitedMs: 0, unknown: [] }),
    });
  } finally { globalThis.fetch = realFetch; }
  assert.ok(calls.includes("per-model"), "the modern route is tried first");
  assert.ok(calls.includes("unload-all"), "v208's GET /unload is the fallback");
});

test("freeLlamaSwap refuses the unload-all fallback while a memory-stack model is resident", async () => {
  const calls = [];
  const logged = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) {
      return { ok: true, status: 200, json: async () => ({ data: [
        { id: "gemma4-e2b" }, { id: "embeddinggemma" },
      ] }) };
    }
    if (u.endsWith("/running")) {
      return { ok: true, status: 200, json: async () => ({ running: [
        { model: "gemma4-e2b" }, { model: "embeddinggemma" },
      ] }) };
    }
    if (u.includes("/api/models/unload/")) {
      calls.push("per-model:" + u.split("/api/models/unload/")[1]);
      return { ok: false, status: 404, json: async () => ({}) };
    }
    if (u.endsWith("/unload")) {
      calls.push("unload-all");
      return { ok: true, status: 200, json: async () => ({}) };
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  try {
    await freeLlamaSwap("http://x", {
      quiesce: async () => ({ drained: true, waitedMs: 0, unknown: [] }),
      log: (m) => logged.push(m),
    });
  } finally { globalThis.fetch = realFetch; }
  assert.ok(!calls.includes("unload-all"),
    "unload-all would tear down the memory stack (invariant 1) — must refuse");
  assert.ok(logged.some((m) => /memory stack|cannot unload-all|per-model unload unavailable/i.test(m)),
    "and the refusal is loud, naming why");
});

test("freeLlamaSwap refuses to unload anything when /running is unreadable", async () => {
  const calls = [];
  const logged = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    const u = String(url);
    if (u.endsWith("/v1/models")) {
      return { ok: true, status: 200, json: async () => ({ data: [{ id: "gemma4-e2b" }] }) };
    }
    if (u.endsWith("/running")) {
      return { ok: false, status: 500, json: async () => ({}) };
    }
    calls.push(u);
    return { ok: true, status: 200, json: async () => ({}) };
  };
  try {
    await freeLlamaSwap("http://x", {
      quiesce: async () => ({ drained: true, waitedMs: 0, unknown: [] }),
      log: (m) => logged.push(m),
    });
  } finally { globalThis.fetch = realFetch; }
  assert.equal(calls.length, 0, "blind => no unload, no drain, no fallback");
  assert.ok(logged.some((m) => /NOT unloading/.test(m)), "and the refusal is loud");
});
