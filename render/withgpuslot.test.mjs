// node --test render/withgpuslot.test.mjs
//
// Tests the centralized withGpuSlot lifecycle via injected deps (no real GPU/ComfyUI/
// llama-swap, no network).
//
// withGpuSlot no longer acquires anything: internal/gpulease (Go) owns acquisition and
// the runner INHERITS a lease. So the old "acquire -> ... -> release" ordering tests are
// gone; what is verified now is that the runner REFUSES to run without a lease, honours
// the class gate and the fence, elects one unloader per lease, and still runs its guarded
// ComfyUI teardown.
import { test } from "node:test";
import assert from "node:assert";
import { withGpuSlot } from "./gpu-lock.mjs";

const MEDIA = { dir: "X", epoch: 7, class: "media" };

function harness({ comfyChild = {}, keepComfy = false, comfyManaged = true } = {}) {
  const calls = [];
  const killed = { n: 0 };
  const child = comfyChild ? { kill() { killed.n++; } } : null;
  const deps = {
    freeLlamaSwap: async () => { calls.push("freeLlamaSwap"); },
    ensureComfy: async () => { calls.push("ensureComfy"); return child; },
    freeComfy: async () => { calls.push("freeComfy"); },
    checkLease: () => true,
    claimUnload: () => true,
  };
  return { calls, deps, killed, opts: { keepComfy, comfyManaged, lease: MEDIA } };
}

test("success path: freeLlamaSwap -> ensureComfy -> fn -> freeComfy -> kill", async () => {
  const h = harness();
  const r = await withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); return "ok"; });
  assert.equal(r, "ok");
  assert.deepEqual(h.calls, ["freeLlamaSwap", "ensureComfy", "fn", "freeComfy"]);
  assert.equal(h.killed.n, 1, "spawned ComfyUI killed");
});

test("throw path: cleanup STILL runs, error propagates", async () => {
  const h = harness();
  await assert.rejects(
    withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); throw new Error("boom"); }),
    /boom/
  );
  assert.ok(h.calls.includes("freeComfy"), "freeComfy ran on throw");
  assert.equal(h.killed.n, 1, "spawned ComfyUI killed on throw");
});

// THE COLLAPSE, asserted. A runner with no lease must refuse rather than grab the card:
// re-adding acquisition here is exactly the duplicate implementation that was deleted,
// and rendering unarbitrated is what tore the text tier down in the first place.
test("NO LEASE => refuse, with a message that says how to get one", async () => {
  let ran = false;
  await assert.rejects(
    withGpuSlot({ lease: null, freeLlamaSwap: async () => { ran = true; } },
      async () => { ran = true; }),
    /GPU lease missing[\s\S]*gpu reserve/
  );
  assert.equal(ran, false, "nothing touched the GPU without a lease");
});

test("--no-lock still works as the deliberate escape hatch", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps, lease: null, noLock: true },
    async () => { h.calls.push("fn"); });
  assert.ok(h.calls.includes("fn"), "the job runs");
});

test("comfyManaged:false skips ensureComfy AND freeComfy (TTS path)", async () => {
  const h = harness({ comfyManaged: false });
  await withGpuSlot({ ...h.opts, ...h.deps }, async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("ensureComfy"), "ensureComfy skipped when comfyManaged:false");
  assert.ok(!h.calls.includes("freeComfy"), "freeComfy skipped when comfyManaged:false");
});

test("keepComfy:true does NOT kill the spawned ComfyUI", async () => {
  const h = harness({ keepComfy: true });
  await withGpuSlot({ ...h.opts, ...h.deps }, async () => {});
  assert.equal(h.killed.n, 0, "ComfyUI left running with keepComfy");
  assert.ok(h.calls.includes("freeComfy"), "VRAM still freed");
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
    warm: true, lease: MEDIA, checkLease: () => true, claimUnload: () => true,
    freeLlamaSwap: async () => { calls.push("freeLS"); },
    ensureComfy: async (o) => { ensureOpts = o; calls.push("ensure"); return { kill() { calls.push("kill"); } }; },
    freeComfy: async () => { calls.push("freeComfy"); },
  }, async () => { calls.push("fn"); });
  assert.equal(ensureOpts.warm, true, "warm reaches ensureComfy");
  assert.deepEqual(calls, ["freeLS", "ensure", "fn", "freeComfy", "kill"],
    "guarded teardown unchanged: free + kill AFTER fn (the whole batch)");
});

// --- once per lease, and only for media ------------------------------------

test("the FIRST job under a lease performs the unload (the hoist actually happens)", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps, claimUnload: () => true },
    async () => { h.calls.push("fn"); });
  assert.ok(h.calls.includes("freeLlamaSwap"),
    "somebody must unload for the lease, or the render runs against a full card");
});

test("a LATER job under the same lease skips the unload (once per lease, not per job)", async () => {
  const h = harness();
  await withGpuSlot({ ...h.opts, ...h.deps, claimUnload: () => false },
    async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("freeLlamaSwap"),
    "unloading per job is the arithmetic behind 3,356 teardowns");
});

// CLASS GATE. A `text` lease is a benchmark's reservation; unloading under it destroys
// exactly the run the lease exists to protect. This regressed once: skipping the unload
// for ANY inherited lease made text safe by accident, and electing an unloader without
// checking the class then made a text lease tear the text tier down.
test("an inherited TEXT lease NEVER unloads the text tier, even as the first job", async () => {
  const h = harness();
  await withGpuSlot({
    ...h.opts, ...h.deps,
    lease: { dir: "X", epoch: 7, class: "text" },
    claimUnload: () => true, // even if we would win the election
  }, async () => { h.calls.push("fn"); });
  assert.ok(!h.calls.includes("freeLlamaSwap"),
    "a text reservation must never unload the tier it was taken to protect");
  assert.ok(h.calls.includes("fn"), "the job still runs");
});

// THE FENCE. A process that slept through a takeover must abort rather than unload on
// top of whoever holds the card now — the incident replayed by a closing lid.
test("a FENCED-OUT inherited lease refuses to touch the GPU", async () => {
  const h = harness();
  await assert.rejects(
    withGpuSlot({ ...h.opts, ...h.deps, checkLease: () => false },
      async () => { h.calls.push("fn"); }),
    /fenced out/i
  );
  assert.ok(!h.calls.includes("freeLlamaSwap"),
    "a fenced-out process must NOT unload — that is the original incident, replayed by a lid");
  assert.ok(!h.calls.includes("fn"), "and it must not run the job either");
});

test("the job callback receives the lease so it can fence before its own irreversible work", async () => {
  const h = harness();
  let seen;
  await withGpuSlot({ ...h.opts, ...h.deps }, async (ctx) => { seen = ctx.lease; });
  assert.deepEqual(seen, MEDIA);
});
