import { test } from "node:test";
import assert from "node:assert/strict";
import { runGraphFlow } from "./comfy-run-graph.mjs";

const graph = { "1": { class_type: "LoadImage", inputs: { image: "x.png" } },
                "2": { class_type: "SaveImage", inputs: { images: ["1", 0] } } };

// Fully-injected deps: NO real ComfyUI, NO real FS. Every collaborator is a spy so the tests
// exercise the REAL control-flow branches (provision-before-start, ownership, teardown, defer).
function baseDeps(over = {}) {
  const calls = { ensureComfy: 0, killComfy: 0, freeComfy: 0, satisfy: 0 };
  const deps = {
    parseManifest: (m) => m,
    manifestHash: () => "H",
    readOwner: () => null,                         // no marker → not cached (satisfy runs)
    writeOwner: (dir, rec) => { calls.owner = rec; },
    comfyUp: async () => false,                    // nothing already running → we start it
    ensureComfy: async () => { calls.ensureComfy++; return { kill() { calls.killComfy++; } }; },
    killComfy: (c) => { calls.killComfy++; c && c.kill && c.kill(); },
    freeComfy: async () => { calls.freeComfy++; },
    satisfy: async () => { calls.satisfy++; return { ok: true, changed: false, unverified: [] }; },
    preflight: async () => ({ ok: true, missing: [], unknownClasses: [] }),
    postGraph: async () => ({ prompt_id: "p1" }),
    collect: async () => ({ "2": [{ filename: "out.png", subfolder: "", type: "output", kind: "image" }] }),
    fetchToDir: async (f) => ({ path: `OUT/${f.filename}`, type: f.type, kind: f.kind, width: 64, height: 48 }),
    writeResult: (path, obj) => { calls.lastResult = obj; },
    ...over,
  };
  deps._calls = calls;
  return deps;
}

const ARGS = { graph, manifest: {}, outDir: "OUT", resultPath: "R", api: "A", comfyDir: "C" };

test("happy path: not cached, ComfyUI down → satisfy runs, we START + TEAR DOWN ComfyUI", async () => {
  // NOTE killComfy default double-counts (dep + child.kill), so assert >=1, and that the
  // child was killed exactly once via freeComfy pairing.
  const deps = baseDeps();
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.deferred, undefined);
  assert.equal(r.image_path, "OUT/out.png");
  assert.deepEqual(Object.keys(r.outputs), ["2"]);
  assert.equal(r.outputs["2"][0].width, 64);
  assert.equal(r.outputs["2"][0].height, 48);
  assert.equal(deps._calls.satisfy, 1);           // went THROUGH satisfy (no cache)
  assert.equal(deps._calls.ensureComfy, 1);       // we started ComfyUI (it was down)
  assert.ok(deps._calls.killComfy >= 1);          // OUR instance torn down
  assert.equal(deps._calls.freeComfy, 1);         // zero-warm /free on OUR instance
  assert.equal(deps._calls.owner.manifestHash, "H");
  assert.equal(deps._calls.owner.pid, undefined); // ownership keyed on hash, NOT pid
});

test("external ComfyUI + packs changed → DEFER EXTERNAL_COMFY_NEEDS_PACKS, never touch it", async () => {
  const deps = baseDeps({
    comfyUp: async () => true,                     // an external instance is already up
    satisfy: async () => ({ ok: true, changed: true, unverified: [] }), // packs moved
  });
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.deferred, true);
  assert.equal(r.code, "EXTERNAL_COMFY_NEEDS_PACKS");
  assert.equal(deps._calls.ensureComfy, 0);        // did NOT start anything
  assert.equal(deps._calls.killComfy, 0);          // did NOT kill the external instance
  assert.equal(deps._calls.freeComfy, 0);          // left it entirely alone
  assert.equal(deps._calls.lastResult.deferred, true); // defer written to the result file
});

test("cache hit (down + unchanged): satisfy SKIPPED, ComfyUI started, preflight runs", async () => {
  let preflighted = false;
  const deps = baseDeps({
    readOwner: () => ({ manifestHash: "H", unverified: [] }), // marker matches wantHash → cached
    satisfy: async () => { throw new Error("satisfy must NOT run on a cache hit"); },
    preflight: async () => { preflighted = true; return { ok: true, missing: [], unknownClasses: [] }; },
  });
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.deferred, undefined);
  assert.equal(deps._calls.satisfy, 0);            // provisioning skipped
  assert.equal(deps._calls.ensureComfy, 1);        // still started (it was down)
  assert.equal(preflighted, true);                 // preflight still gates the run
});

test("preflight unknown class → DEFER NODE_CLASS_MISSING (ref = the class)", async () => {
  const deps = baseDeps({
    preflight: async () => ({ ok: false, missing: [], unknownClasses: [{ node: "2", class_type: "FooNode" }] }),
  });
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.code, "NODE_CLASS_MISSING");
  assert.equal(r.ref, "FooNode");
  assert.ok(deps._calls.killComfy >= 1);           // our started instance is still torn down
});

test("preflight missing required input → DEFER PREFLIGHT_MISSING_INPUTS", async () => {
  const deps = baseDeps({
    preflight: async () => ({ ok: false, unknownClasses: [], missing: [{ node: "2", class_type: "SaveImage", inputs: ["images"] }] }),
  });
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.code, "PREFLIGHT_MISSING_INPUTS");
  assert.equal(r.ref, "SaveImage");
});

test("a satisfy fail-open warning is surfaced on stderr (SATISFY WARN)", async () => {
  // The warning is the ONLY operator-visible signal that the coherence check was skipped.
  const seen = [];
  const orig = console.error;
  console.error = (...a) => { seen.push(a.join(" ")); };
  try {
    const deps = baseDeps({
      satisfy: async () => ({ ok: true, changed: false, unverified: [], warning: "coherence check skipped (test)" }),
    });
    const r = await runGraphFlow(ARGS, deps);
    assert.equal(r.deferred, undefined, "a warning is not a defer");
  } finally { console.error = orig; }
  assert.ok(seen.some((l) => l.includes("SATISFY WARN") && l.includes("coherence check skipped")),
    "the warning must reach stderr");
});

test("satisfy DEFER short-circuits BEFORE start + POST", async () => {
  const deps = baseDeps({
    satisfy: async () => ({ ok: false, defer: { code: "VENV_INCOHERENT", ref: "x", detail: "d" } }),
    postGraph: async () => { throw new Error("POST must not run after a satisfy DEFER"); },
  });
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.deferred, true);
  assert.equal(r.code, "VENV_INCOHERENT");
  assert.equal(deps._calls.ensureComfy, 0);        // never started ComfyUI
});

test("defer path writes the {deferred,code,ref,detail} envelope via writeResult", async () => {
  const deps = baseDeps({
    preflight: async () => ({ ok: false, missing: [], unknownClasses: [{ node: "2", class_type: "Zzz" }] }),
  });
  await runGraphFlow(ARGS, deps);
  const w = deps._calls.lastResult;
  assert.deepEqual(Object.keys(w).sort(), ["code", "deferred", "detail", "ref"]);
  assert.equal(w.deferred, true);
  assert.equal(w.code, "NODE_CLASS_MISSING");
  assert.equal(w.ref, "Zzz");
});

test("a throw in POST becomes a typed RUN_ERROR defer (never an untyped exit-1)", async () => {
  const deps = baseDeps({ postGraph: async () => { throw new Error("comfy exec error boom"); } });
  const r = await runGraphFlow(ARGS, deps);
  assert.equal(r.deferred, true);
  assert.equal(r.code, "RUN_ERROR");
  assert.match(r.detail, /boom/);
  assert.ok(deps._calls.killComfy >= 1);           // teardown still ran in finally
});
