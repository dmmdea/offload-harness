// node --test render/withgpuslot.test.mjs
// Tests the centralized withGpuSlot lifecycle via injected deps (no real GPU/ComfyUI/
// llama-swap, no network). Verifies the guarded cleanup + lock.release run on BOTH the
// success and the throw paths, the order (freeLlamaSwap before fn), the comfyManaged
// flag, and the keepComfy guard.
import { test } from "node:test";
import assert from "node:assert";
import { withGpuSlot } from "./gpu-lock.mjs";

function harness({ lockNull = false, comfyChild = {}, keepComfy = false, comfyManaged = true } = {}) {
  const calls = [];
  let released = 0;
  const killed = { n: 0 };
  const child = comfyChild ? { kill() { killed.n++; } } : null;
  const deps = {
    acquire: async () => {
      calls.push("acquire");
      return lockNull ? null : { release() { released++; calls.push("release"); } };
    },
    freeLlamaSwap: async () => { calls.push("freeLlamaSwap"); },
    ensureComfy: async () => { calls.push("ensureComfy"); return child; },
    freeComfy: async () => { calls.push("freeComfy"); },
  };
  return { calls, deps, get released() { return released; }, killed, opts: { keepComfy, comfyManaged } };
}

test("success path: freeLlamaSwap -> ensureComfy -> fn -> freeComfy -> kill -> release", async () => {
  const h = harness();
  const r = await withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); return "ok"; });
  assert.equal(r, "ok");
  assert.deepEqual(h.calls, ["acquire", "freeLlamaSwap", "ensureComfy", "fn", "freeComfy", "release"]);
  assert.equal(h.released, 1, "lock released exactly once");
  assert.equal(h.killed.n, 1, "spawned ComfyUI killed");
});

test("throw path: cleanup + release STILL run, error propagates", async () => {
  const h = harness();
  await assert.rejects(
    withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); throw new Error("boom"); }),
    /boom/
  );
  assert.ok(h.calls.includes("freeComfy"), "freeComfy ran on throw");
  assert.ok(h.calls.includes("release"), "lock released on throw");
  assert.equal(h.released, 1, "released exactly once even on throw");
  assert.equal(h.killed.n, 1, "spawned ComfyUI killed on throw");
});

test("busy lock (acquire -> null) throws GPU-busy, never runs fn", async () => {
  const h = harness({ lockNull: true });
  let ran = false;
  await assert.rejects(
    withGpuSlot({ ...h.opts, ...h.deps }, async () => { ran = true; }),
    /busy/i
  );
  assert.equal(ran, false, "fn must not run when the slot is busy");
  assert.ok(!h.calls.includes("freeLlamaSwap"), "no teardown of llama-swap when we never got the slot");
});

test("comfyManaged:false skips ensureComfy AND freeComfy (TTS path)", async () => {
  const h = harness({ comfyManaged: false });
  await withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("ensureComfy"), "ensureComfy skipped when comfyManaged:false");
  assert.ok(!h.calls.includes("freeComfy"), "freeComfy skipped when comfyManaged:false");
  assert.equal(h.released, 1, "lock still released");
});

test("keepComfy:true does NOT kill the spawned ComfyUI", async () => {
  const h = harness({ keepComfy: true });
  await withGpuSlot({ ...h.opts, ...h.deps }, async () => {});
  assert.equal(h.killed.n, 0, "ComfyUI left running with keepComfy");
  assert.ok(h.calls.includes("freeComfy"), "VRAM still freed");
  assert.equal(h.released, 1);
});

test("freeLlamaSwap runs BEFORE fn (the render gets the whole GPU)", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); });
  assert.ok(h.calls.indexOf("freeLlamaSwap") < h.calls.indexOf("fn"), "freeLlamaSwap precedes fn");
});

test("warm is threaded to ensureComfy and teardown still frees at the end", async () => {
  const calls = [];
  let ensureOpts = null;
  await withGpuSlot({
    warm: true,
    acquire: async () => ({ release() { calls.push("release"); } }),
    freeLlamaSwap: async () => { calls.push("freeLS"); },
    ensureComfy: async (o) => { ensureOpts = o; calls.push("ensure"); return { kill() { calls.push("kill"); } }; },
    freeComfy: async () => { calls.push("freeComfy"); },
  }, async () => { calls.push("fn"); });
  assert.equal(ensureOpts.warm, true, "warm reaches ensureComfy");
  assert.deepEqual(calls, ["freeLS", "ensure", "fn", "freeComfy", "kill", "release"],
    "guarded teardown unchanged: free + kill + release AFTER fn (the whole batch)");
});

test("no-lock mode: acquire skipped, fn runs, no release error", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps, noLock: true }, async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("acquire"), "acquire skipped in no-lock mode");
  assert.ok(h.calls.includes("fn"));
});
