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

// --- a template run: converted after ComfyUI is up, before preflight; trust on first use checked on every run ---

const TEMPLATE_ARGS = { template: "tt_open", manifest: {}, outDir: "OUT", resultPath: "R", api: "A", comfyDir: "C" };
const CONVERTED = { "1": { class_type: "SaveImage", inputs: { images: ["2", 0] } }, "2": { class_type: "EmptyImage", inputs: {} } };

test("template run: the template is converted after ComfyUI is up and before preflight, and the converted graph is the one that runs", async () => {
  const order = [];
  let preflighted = null;
  let posted = null;
  const deps = baseDeps({
    ensureComfy: async () => { order.push("start"); return { kill() {} }; },
    convert: async (spec) => { order.push("convert:" + spec.template); return { ok: true, graph: CONVERTED }; },
    preflight: async (g) => { order.push("preflight"); preflighted = g; return { ok: true, missing: [], unknownClasses: [] }; },
    postGraph: async (g) => { order.push("post"); posted = g; return { prompt_id: "p1" }; },
  });
  const r = await runGraphFlow(TEMPLATE_ARGS, deps);
  assert.equal(r.deferred, undefined);
  assert.deepEqual(order, ["start", "convert:tt_open", "preflight", "post"]);
  assert.deepEqual(preflighted, CONVERTED);
  assert.deepEqual(posted, CONVERTED);
});

test("template run: the template's overrides and seed reach the converter", async () => {
  let seen = null;
  const deps = baseDeps({ convert: async (spec) => { seen = spec; return { ok: true, graph: CONVERTED }; } });
  await runGraphFlow({ ...TEMPLATE_ARGS, overrides: { "6.text": "x" }, seed: 7 }, deps);
  assert.deepEqual([seen.template, seen.overrides, seen.seed], ["tt_open", { "6.text": "x" }, 7]);
});

test("template run: a converter refusal is a typed defer TEMPLATE_CONVERT_REFUSED naming the refusal; nothing is posted and our ComfyUI is torn down", async () => {
  const deps = baseDeps({
    convert: async () => ({ ok: false, refusal: { code: "unknown_node_class", message: "no class FooNode in this ComfyUI", details: {} } }),
    postGraph: async () => { throw new Error("POST must not run after a refused conversion"); },
  });
  const r = await runGraphFlow(TEMPLATE_ARGS, deps);
  assert.deepEqual([r.deferred, r.code, r.ref], [true, "TEMPLATE_CONVERT_REFUSED", "unknown_node_class"]);
  assert.match(r.detail, /FooNode/);
  assert.ok(deps._calls.killComfy >= 1);
  assert.equal(deps._calls.freeComfy, 1);
});

test("template run: a converter that throws is a typed RUN_ERROR defer, not an untyped exit", async () => {
  const deps = baseDeps({ convert: async () => { throw new Error("object_info fetch failed"); } });
  const r = await runGraphFlow(TEMPLATE_ARGS, deps);
  assert.deepEqual([r.deferred, r.code], [true, "RUN_ERROR"]);
  assert.match(r.detail, /object_info/);
});

test("a graph run does not convert: without a template the graph is posted as it came", async () => {
  let converts = 0;
  const deps = baseDeps({ convert: async () => { converts++; return { ok: true, graph: CONVERTED }; } });
  let posted = null;
  deps.postGraph = async (g) => { posted = g; return { prompt_id: "p1" }; };
  await runGraphFlow(ARGS, deps);
  assert.equal(converts, 0);
  assert.deepEqual(posted, graph);
});

test("trust on first use is checked on EVERY run: a cache hit does not skip it, and a refusal comes before ComfyUI starts", async () => {
  const deps = baseDeps({
    readOwner: () => ({ manifestHash: "H", unverified: [] }),
    satisfy: async () => { throw new Error("satisfy must NOT run on a cache hit"); },
    tofuCheck: async () => ({ ok: false, defer: { code: "MODEL_SHA_MISMATCH", ref: "models/vae/v.safetensors", detail: "trust on first use: hash differs" } }),
  });
  const r = await runGraphFlow(TEMPLATE_ARGS, deps);
  assert.deepEqual([r.deferred, r.code, r.ref], [true, "MODEL_SHA_MISMATCH", "models/vae/v.safetensors"]);
  assert.equal(deps._calls.ensureComfy, 0, "no ComfyUI was started for a refused model");
  assert.equal(deps._calls.lastResult.code, "MODEL_SHA_MISMATCH");
});

test("trust on first use: the files recorded for the first time are reported in the envelope, and only then", async () => {
  const first = baseDeps({ tofuCheck: async () => ({ ok: true, first_seen: ["models/vae/v.safetensors"], matched: [] }) });
  const r1 = await runGraphFlow(ARGS, first);
  assert.deepEqual(r1.tofu_first_seen, ["models/vae/v.safetensors"]);
  const again = baseDeps({ tofuCheck: async () => ({ ok: true, first_seen: [], matched: ["models/vae/v.safetensors"] }) });
  assert.equal("tofu_first_seen" in (await runGraphFlow(ARGS, again)), false);
  const plain = await runGraphFlow(ARGS, baseDeps());
  assert.equal("tofu_first_seen" in plain, false, "a graph run's envelope is what it was");
});

// --- a template run: the converted graph may load only the model files the manifest names ---
// The manifest is written from the catalog's static reading of the template and the graph from the converter's, two readers of one file.
// The runner compares what the converter produced against the list before anything is posted, so a file one reader saw and the other did
// not (a value an instance carries into an inner loader, a loader that stores no value and takes the listing's default) is a typed defer.

const LISTED = {
  schema_version: 1, workflow: "tt_open", node_packs: [],
  models: [
    { path: "models/checkpoints/ok.safetensors", source_url: "", sha256: null, trust: "tofu" },
    { path: "models/vae/sub/v.safetensors", source_url: "", sha256: null, trust: "tofu" },
    // Written the way a hand-made or another machine's manifest can write them: mixed case, and backslashes under a mixed-case folder
    // (`models\checkpoints\Sub\Mixed.safetensors`; the backslashes are doubled in the source).
    { path: "models/loras/Qwen-Image-Lightning-8steps-V1.0.safetensors", source_url: "", sha256: null, trust: "tofu" },
    { path: "models\\checkpoints\\Sub\\Mixed.safetensors", source_url: "", sha256: null, trust: "tofu" },
  ],
};
const loaderGraph = (file) => ({
  "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: file } },
  "2": { class_type: "SaveImage", inputs: { images: ["1", 0] } },
});

test("template run: a converted graph that loads a model file the manifest does not name is a typed defer TEMPLATE_MODEL_UNLISTED; nothing is preflighted or posted and our ComfyUI is torn down", async () => {
  const deps = baseDeps({
    convert: async () => ({ ok: true, graph: loaderGraph("other.safetensors") }),
    preflight: async () => { throw new Error("preflight must not run for a graph that loads an unlisted file"); },
    postGraph: async () => { throw new Error("POST must not run for a graph that loads an unlisted file"); },
  });
  const r = await runGraphFlow({ ...TEMPLATE_ARGS, manifest: LISTED }, deps);
  assert.deepEqual([r.deferred, r.code, r.ref], [true, "TEMPLATE_MODEL_UNLISTED", "other.safetensors"]);
  assert.match(r.detail, /other\.safetensors/);
  assert.deepEqual(deps._calls.lastResult, { deferred: true, code: "TEMPLATE_MODEL_UNLISTED", ref: "other.safetensors", detail: r.detail });
  assert.ok(deps._calls.killComfy >= 1);
  assert.equal(deps._calls.freeComfy, 1);
});

test("template run: a converted graph whose every model file the manifest names is posted, by the file's path, its base name or its case", async () => {
  for (const file of ["ok.safetensors", "OK.SafeTensors", "somewhere/else/ok.safetensors", "sub\\v.safetensors"]) {
    let posted = null;
    const deps = baseDeps({
      convert: async () => ({ ok: true, graph: loaderGraph(file) }),
      postGraph: async (g) => { posted = g; return { prompt_id: "p1" }; },
    });
    const r = await runGraphFlow({ ...TEMPLATE_ARGS, manifest: LISTED }, deps);
    assert.equal(r.deferred, undefined, `${file} is listed`);
    assert.equal(posted["1"].inputs.ckpt_name, file);
  }
});

test("a graph run is not checked against its manifest: only a template run compares the converted graph with the list", async () => {
  let posted = null;
  const deps = baseDeps({ postGraph: async (g) => { posted = g; return { prompt_id: "p1" }; } });
  const g = loaderGraph("other.safetensors");
  const r = await runGraphFlow({ ...ARGS, graph: g, manifest: LISTED }, deps);
  assert.equal(r.deferred, undefined);
  assert.deepEqual(posted, g);
});

// The manifest side of the match is normalised too: a manifest entry that is not lower case or not written with forward slashes still lists
// the file a graph names exactly. Each test fails if that step is dropped.

test("template run: a manifest entry in mixed case lists the model file the converted graph names exactly as the entry spells it", async () => {
  const file = "Qwen-Image-Lightning-8steps-V1.0.safetensors";
  assert.equal(LISTED.models[2].path, `models/loras/${file}`);
  let posted = null;
  const deps = baseDeps({
    convert: async () => ({ ok: true, graph: loaderGraph(file) }),
    postGraph: async (g) => { posted = g; return { prompt_id: "p1" }; },
  });
  const r = await runGraphFlow({ ...TEMPLATE_ARGS, manifest: LISTED }, deps);
  assert.equal(r.deferred, undefined, `${file} is listed by its mixed-case entry`);
  assert.equal(posted["1"].inputs.ckpt_name, file);
});

test("template run: a manifest entry written with backslashes lists the model file the converted graph names exactly as it sits under the class directory", async () => {
  assert.equal(LISTED.models[3].path.split("\\").length, 4, "the fixture holds three real backslashes, not escapes that dropped out");
  let posted = null;
  const deps = baseDeps({
    convert: async () => ({ ok: true, graph: loaderGraph("Sub/Mixed.safetensors") }),
    postGraph: async (g) => { posted = g; return { prompt_id: "p1" }; },
  });
  const r = await runGraphFlow({ ...TEMPLATE_ARGS, manifest: LISTED }, deps);
  assert.equal(r.deferred, undefined, "Sub/Mixed.safetensors is listed by the backslash entry");
  assert.equal(posted["1"].inputs.ckpt_name, "Sub/Mixed.safetensors");
});

// The same for an embedding the prompt names: the manifest's embeddings entry is matched after the same normalisation and is anchored to the
// start of the path. Each test fails if one of those steps is dropped from the embeddings branch alone.

const embeddingRunGraph = (text) => ({
  "1": { class_type: "CLIPTextEncode", inputs: { text } },
  "2": { class_type: "SaveImage", inputs: { images: ["1", 0] } },
});
const listedWith = (path) => ({ ...LISTED, models: [...LISTED.models, { path, source_url: "", sha256: null, trust: "tofu" }] });

test("template run: an embeddings manifest entry in another case, or written with backslashes, lists the embedding the converted graph's prompt names", async () => {
  const both = "Models\\Embeddings\\EVIL_TI.pt";
  assert.equal(both.split("\\").length, 3, "the fixture holds two real backslashes, not escapes that dropped out");
  for (const path of [both, "models\\embeddings\\evil_ti.safetensors", "Models/Embeddings/EVIL_TI.bin"]) {
    let posted = null;
    const deps = baseDeps({
      convert: async () => ({ ok: true, graph: embeddingRunGraph("a cat, embedding:Evil_TI") }),
      postGraph: async (g) => { posted = g; return { prompt_id: "p1" }; },
    });
    const r = await runGraphFlow({ ...TEMPLATE_ARGS, manifest: listedWith(path) }, deps);
    assert.equal(r.deferred, undefined, `${path} lists the embedding`);
    assert.equal(posted["1"].inputs.text, "a cat, embedding:Evil_TI");
  }
});

test("template run: a manifest path that only contains models/embeddings/ further along does not list an embedding, so the run is a typed defer", async () => {
  const deps = baseDeps({
    convert: async () => ({ ok: true, graph: embeddingRunGraph("a cat, embedding:evil_ti") }),
    postGraph: async () => { throw new Error("POST must not run for a graph that loads an unlisted embedding"); },
  });
  const r = await runGraphFlow({ ...TEMPLATE_ARGS, manifest: listedWith("models/loras/models/embeddings/evil_ti.safetensors") }, deps);
  assert.deepEqual([r.deferred, r.code, r.ref], [true, "TEMPLATE_MODEL_UNLISTED", "models/embeddings/evil_ti"]);
});
