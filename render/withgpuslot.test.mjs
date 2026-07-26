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

// --- inherited lease -------------------------------------------------------
// When the Go holder already owns the lease it threads GPU_LEASE_DIR/GPU_LEASE_EPOCH
// down. Acquiring again would contend with our own holder, and unloading again would
// repeat the per-job teardown this change exists to remove: freeLlamaSwap ran INSIDE
// withGpuSlot, i.e. once per job, which is the arithmetic behind 3,356 unloads.

test("inherited lease: acquire is SKIPPED (we would contend with our own holder)", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps, lease: { dir: "X", epoch: 7 }, checkLease: () => true },
    async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("acquire"), "must not re-acquire a lease we already hold");
  assert.ok(h.calls.includes("fn"), "the job still runs");
});

// THE HOIST, both halves. Skipping the unload under an inherited lease WITHOUT anyone
// performing it was the defect: a leased render then ran with every model still
// resident. Exactly one job per lease unloads.
test("inherited lease: the FIRST job performs the unload (the hoist actually happens)", async () => {
  const h = harness();
  await withGpuSlot({
    ...h.opts, ...h.deps,
    lease: { dir: "X", epoch: 7 }, checkLease: () => true,
    claimUnload: () => true, // we are the first job under this lease
  }, async () => { h.calls.push("fn"); });
  assert.ok(h.calls.includes("freeLlamaSwap"),
    "somebody must unload for the lease, or the render runs against a full card");
  assert.ok(h.calls.indexOf("freeLlamaSwap") < h.calls.indexOf("fn"), "unload precedes the job");
});

// CLASS GATE. A `text` lease is a benchmark's reservation; unloading under it destroys
// exactly the run the lease exists to protect. This regressed once already: skipping the
// unload for ANY inherited lease made text safe by accident, and electing an unloader
// without checking the class then made a text lease tear the text tier down.
test("inherited TEXT lease NEVER unloads the text tier, even as the first job", async () => {
  const h = harness();
  await withGpuSlot({
    ...h.opts, ...h.deps,
    lease: { dir: "X", epoch: 7, class: "text" }, checkLease: () => true,
    claimUnload: () => true, // even if we would win the election
  }, async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("freeLlamaSwap"),
    "a text reservation must never unload the tier it was taken to protect");
  assert.ok(h.calls.includes("fn"), "the job still runs");
});

test("inherited MEDIA lease does unload (the class gate is not a blanket skip)", async () => {
  const h = harness();
  await withGpuSlot({
    ...h.opts, ...h.deps,
    lease: { dir: "X", epoch: 7, class: "media" }, checkLease: () => true,
    claimUnload: () => true,
  }, async () => { h.calls.push("fn"); });
  assert.ok(h.calls.includes("freeLlamaSwap"), "a media lease still frees the card");
});

// THE FENCE, exercised. A process that slept through a takeover must abort rather than
// unload on top of whoever holds the card now — the incident replayed by a closing lid.
test("a FENCED-OUT inherited lease refuses to touch the GPU", async () => {
  const h = harness();
  await assert.rejects(
    withGpuSlot({
      ...h.opts, ...h.deps,
      lease: { dir: "X", epoch: 7, class: "media" },
      checkLease: () => false, // our epoch is no longer current
      claimUnload: () => true,
    }, async () => { h.calls.push("fn"); }),
    /fenced out/i
  );
  assert.ok(!h.calls.includes("freeLlamaSwap"),
    "a fenced-out process must NOT unload — that is the original incident, replayed by a lid");
  assert.ok(!h.calls.includes("fn"), "and it must not run the job either");
});

test("inherited lease: a LATER job skips the unload (once per lease, not per job)", async () => {
  const h = harness();
  await withGpuSlot({
    ...h.opts, ...h.deps,
    lease: { dir: "X", epoch: 7 }, checkLease: () => true,
    claimUnload: () => false, // another job already unloaded for this lease
  }, async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("freeLlamaSwap"),
    "unloading per job is the arithmetic behind 3,356 teardowns");
});

test("inherited lease: teardown still runs (freeComfy + kill), and release is a no-op", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps, lease: { dir: "X", epoch: 7 }, checkLease: () => true },
    async () => { h.calls.push("fn"); });
  assert.ok(h.calls.includes("freeComfy"), "ComfyUI is still torn down at the batch boundary");
  assert.equal(h.killed.n, 1, "a ComfyUI we spawned is still killed");
  assert.equal(h.released, 0, "we never took the lease, so we must never release it");
});

test("WITHOUT an inherited lease the old behaviour is unchanged (acquire + unload)", async () => {
  const h = harness();
  // lease:null is explicit so the ambient GPU_LEASE_DIR of a real run cannot leak in.
  await withGpuSlot({ ...h.opts, ...h.deps, lease: null }, async () => { h.calls.push("fn"); });
  assert.deepEqual(h.calls, ["acquire", "freeLlamaSwap", "ensureComfy", "fn", "freeComfy", "release"]);
});

test("the job callback receives the lease so it can fence before irreversible work", async () => {
  const h = harness();
  let seen;
  await withGpuSlot({ ...h.opts, ...h.deps, lease: { dir: "X", epoch: 7 }, checkLease: () => true },
    async (ctx) => { seen = ctx.lease; });
  assert.deepEqual(seen, { dir: "X", epoch: 7 });
});
