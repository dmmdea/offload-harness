// render/template-run.test.mjs - what the run-graph runner does with a template once ComfyUI is up: find it by name, convert it with the
// node's own /object_info, apply the caller's overrides. Pure functions over the hand-written goldens of template-goldens/ and the
// in-memory tree of template-manifest-fixture.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { applyOverrides, convertForRun, loadTemplate } from "./template-run.mjs";
import * as TR from "./template-run.mjs";
import { requiredModels } from "./templates-catalog.mjs";
import { COMFY, fixtureIo, fixtureFiles, memIo } from "./template-manifest-fixture.mjs";

const GOLD = fileURLToPath(new URL("./testdata/template-goldens/", import.meta.url));
const json = (name) => JSON.parse(readFileSync(GOLD + name, "utf8"));
const workflow = () => json("basic-t2i.workflow.json");
const objectInfo = () => json("object-info.synthetic.json");
const converted = () => { const r = convertForRun({ workflow: workflow(), objectInfo: objectInfo(), comfyVersion: "0.37.0", minComfyUIVersion: "0.3.0" }); assert.equal(r.ok, true); return r.graph; };

test("convertForRun: a template and a listing give the API graph", () => {
  const g = converted();
  assert.equal(g["6"].class_type, "CLIPTextEncode");
  assert.equal(g["6"].inputs.text, "a red fox");
  assert.deepEqual(g["3"].inputs.model, ["4", 0]);
  assert.equal(g["10"], undefined, "a Note is not a node");
});

test("convertForRun: a seed pins the seed inputs", () => {
  const r = convertForRun({ workflow: workflow(), objectInfo: objectInfo(), comfyVersion: "0.37.0", minComfyUIVersion: "0.3.0", seed: 7 });
  assert.equal(r.graph["3"].inputs.seed, 7);
});

test("convertForRun: a ComfyUI older than the template's minimum is a typed refusal, and no graph", () => {
  const r = convertForRun({ workflow: workflow(), objectInfo: objectInfo(), comfyVersion: "0.2.0", minComfyUIVersion: "0.3.0" });
  assert.equal(r.ok, false);
  assert.equal(r.refusal.code, "comfyui_too_old");
  assert.equal(r.graph, undefined);
});

test("convertForRun: a node class the listing does not know is the converter's typed refusal, passed through", () => {
  const info = objectInfo();
  delete info.KSampler;
  const r = convertForRun({ workflow: workflow(), objectInfo: info, comfyVersion: "0.37.0", minComfyUIVersion: "0.3.0" });
  assert.deepEqual([r.ok, r.refusal.code], [false, "unknown_node_class"]);
});

test("convertForRun: overrides are applied after the conversion", () => {
  const r = convertForRun({ workflow: workflow(), objectInfo: objectInfo(), comfyVersion: "0.37.0", minComfyUIVersion: "0.3.0", overrides: { "6.text": "a blue fox", "3.steps": 30 } });
  assert.equal(r.graph["6"].inputs.text, "a blue fox");
  assert.equal(r.graph["3"].inputs.steps, 30);
  assert.equal(r.graph["7"].inputs.text, "blurry", "the other prompt is untouched");
});

test("applyOverrides: no overrides leave the graph as it was, and the input graph is never changed", () => {
  const g = converted();
  const before = JSON.stringify(g);
  assert.equal(JSON.stringify(applyOverrides(g, {}).graph), before);
  assert.equal(JSON.stringify(applyOverrides(g, undefined).graph), before);
  const r = applyOverrides(g, { "6.text": "changed" });
  assert.equal(r.ok, true);
  assert.equal(JSON.stringify(g), before, "applyOverrides copies");
});

test("applyOverrides: the node is named by its id and the input by its name, split at the first dot (autogrow names keep theirs)", () => {
  const g = { "12:3": { class_type: "ComfyMathExpression", inputs: { "values.a": 1, expression: "a" } } };
  const r = applyOverrides(g, { "12:3.values.a": 5 });
  assert.equal(r.graph["12:3"].inputs["values.a"], 5);
});

test("applyOverrides: a node that is not in the graph is refused by name", () => {
  const r = applyOverrides(converted(), { "99.text": "x" });
  assert.deepEqual([r.ok, r.refusal.code, r.refusal.details.node], [false, "override_unknown_node", "99"]);
});

test("applyOverrides: an input the node does not have is refused by name, never added", () => {
  const r = applyOverrides(converted(), { "6.nope": "x" });
  assert.deepEqual([r.ok, r.refusal.code, r.refusal.details.input], [false, "override_unknown_input", "nope"]);
});

test("applyOverrides: an input that is wired to another node is refused, not cut", () => {
  const r = applyOverrides(converted(), { "3.model": "x" });
  assert.deepEqual([r.ok, r.refusal.code], [false, "override_is_link"]);
});

test("applyOverrides: only a string, a number or a boolean is a value, and a key must be <node>.<input>", () => {
  for (const bad of [{ "6.text": { a: 1 } }, { "6.text": [1] }, { "6.text": null }]) {
    assert.equal(applyOverrides(converted(), bad).refusal.code, "override_bad_value", JSON.stringify(bad));
  }
  for (const key of ["6", ".text", "6.", "text"]) {
    assert.equal(applyOverrides(converted(), { [key]: "x" }).refusal.code, "override_bad_key", key);
  }
  assert.equal(applyOverrides(converted(), [1]).refusal.code, "override_bad_key", "overrides is an object, not a list");
});

test("applyOverrides: a prototype key is not a node", () => {
  const r = applyOverrides(converted(), { "__proto__.text": "x", "constructor.name": "y" });
  assert.equal(r.ok, false);
  assert.equal(r.refusal.code, "override_unknown_node");
});

// ---------------------------------------------------------------------------------------------------------------------
// loadTemplate: a template is found by NAME in the node's own installed package
// ---------------------------------------------------------------------------------------------------------------------

test("loadTemplate: the workflow and the index entry's minimum version, by name", () => {
  const r = loadTemplate({ comfyDir: COMFY, name: "tt_old", io: fixtureIo() });
  assert.equal(r.ok, true);
  assert.equal(r.min_comfyui_version, "99.0.0");
  assert.ok(Array.isArray(r.workflow.nodes));
  assert.equal(loadTemplate({ comfyDir: COMFY, name: "tt_open", io: fixtureIo() }).min_comfyui_version, null);
});

test("loadTemplate: a name that is a path is refused before any file is read", () => {
  for (const name of ["../x", "a/b", "a\\b", "..", ".", "", "x y", "C:x"]) {
    const r = loadTemplate({ comfyDir: COMFY, name, io: fixtureIo() });
    assert.deepEqual([r.ok, r.refusal.code], [false, "bad_template_name"], JSON.stringify(name));
  }
});

test("loadTemplate: a name the index does not list is unknown_template, even when a file of that name exists", () => {
  const io = memIo(fixtureFiles({ extra: { "/srv/comfy/.venv/lib/python3.13/site-packages/comfyui_workflow_templates_json/templates/stray.json": "{}" } }));
  assert.equal(loadTemplate({ comfyDir: COMFY, name: "stray", io }).refusal.code, "unknown_template");
  assert.equal(loadTemplate({ comfyDir: COMFY, name: "nope", io }).refusal.code, "unknown_template");
});

test("loadTemplate: a listed template with no file or with invalid JSON is bad_template, a tree with no package is templates_package_missing", () => {
  assert.equal(loadTemplate({ comfyDir: COMFY, name: "tt_broken", io: fixtureIo() }).refusal.code, "bad_template");
  const broken = memIo(fixtureFiles({ extra: { "/srv/comfy/.venv/lib/python3.13/site-packages/comfyui_workflow_templates_json/templates/tt_open.json": "{not json" } }));
  assert.equal(loadTemplate({ comfyDir: COMFY, name: "tt_open", io: broken }).refusal.code, "bad_template");
  assert.equal(loadTemplate({ comfyDir: "/srv/empty", name: "tt_open", io: memIo({}) }).refusal.code, "templates_package_missing");
});

// ---------------------------------------------------------------------------------------------------------------------
// An override sets values; it never retargets a file. The licence gate, the FLUX bar and the trust-on-first-use check read the models the
// template declares, so a loader widget set to another file would run one that none of them looked at.
// ---------------------------------------------------------------------------------------------------------------------

const fileGraph = () => ({
  "1": { class_type: "UNETLoader", inputs: { unet_name: "wan2.2_ti2v_5B_fp16.safetensors", weight_dtype: "default" } },
  "2": { class_type: "LoadImage", inputs: { image: "the_lily_veil.png" } },
  "3": { class_type: "VAELoader", inputs: { vae_name: "wan2.2_vae.safetensors" } },
  "4": { class_type: "CLIPTextEncode", inputs: { text: "a fox" } },
});

test("applyOverrides: a model loader's file is never retargeted (the gate looked at the file the template names)", () => {
  for (const [key, value] of [["1.unet_name", "flux1-dev.safetensors"], ["3.vae_name", "other_vae.safetensors"]]) {
    const r = applyOverrides(fileGraph(), { [key]: value });
    assert.deepEqual([r.ok, r.refusal?.code, r.refusal?.details.node], [false, "override_is_file", key.split(".")[0]], key);
  }
});

test("applyOverrides: an input loader's file is never retargeted (a node can match an input file only by name)", () => {
  const r = applyOverrides(fileGraph(), { "2.image": "someone_else.png" });
  assert.deepEqual([r.ok, r.refusal?.code], [false, "override_is_file"]);
});

test("applyOverrides: a model file name is refused wherever it lands, as the old value or as the new one", () => {
  const g = { "9": { class_type: "SomeCustomLoader", inputs: { ckpt: "base.safetensors", note: "" } }, "4": { class_type: "CLIPTextEncode", inputs: { text: "a fox" } } };
  assert.equal(applyOverrides(g, { "9.ckpt": "x" }).refusal?.code, "override_is_file", "the input holds a model file now");
  assert.equal(applyOverrides(g, { "9.note": "flux1-dev.safetensors" }).refusal?.code, "override_is_file", "the new value is a model file");
  assert.equal(applyOverrides(g, { "4.text": "model.gguf" }).refusal?.code, "override_is_file", "a prompt that names a model file is a retarget attempt too");
});

test("applyOverrides: the values a template is made to be tuned by still go through", () => {
  const r = applyOverrides({ ...fileGraph(), "5": { class_type: "KSampler", inputs: { seed: 1, steps: 20, sampler_name: "euler" } } }, { "4.text": "a blue fox", "5.steps": 30, "5.sampler_name": "dpmpp_2m" });
  assert.equal(r.ok, true);
  assert.equal(r.graph["4"].inputs.text, "a blue fox");
});

// unlistedModels: the model files a converted graph loads that a manifest does not name (the runner's check before it posts a template run).
// The last two entries are written the way a hand-made or another machine's manifest can write them: a name in mixed case, and a path with
// backslashes under a mixed-case folder (`models\checkpoints\Sub\Mixed.safetensors`; the backslashes are doubled in the source).
const LISTED_MODELS = [
  { path: "models/checkpoints/ok.safetensors" }, { path: "models/diffusion_models/sub/u.gguf" },
  { path: "models/loras/Qwen-Image-Lightning-8steps-V1.0.safetensors" }, { path: "models\\checkpoints\\Sub\\Mixed.safetensors" },
];

test("unlistedModels: a model file the list does not name is reported once, in graph order; a link, a number and a non-model string are not files", () => {
  const graph = {
    "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "ok.safetensors" } },
    "2": { class_type: "VAELoader", inputs: { vae_name: "b.safetensors" } },
    "3": { class_type: "LoraLoader", inputs: { model: ["1", 0], lora_name: "a.safetensors", strength_model: 1, text: "notes.txt" } },
    "4": { class_type: "VAELoader", inputs: { vae_name: "b.safetensors" } },
  };
  assert.deepEqual(TR.unlistedModels(graph, LISTED_MODELS), ["b.safetensors", "a.safetensors"]);
});

test("unlistedModels: a file matches the list by its name under the class directory, by its base name, and without regard to case or slash direction", () => {
  const graph = {
    "1": { class_type: "A", inputs: { f: "OK.safetensors" } },
    "2": { class_type: "B", inputs: { f: "elsewhere\\ok.safetensors" } },
    "3": { class_type: "C", inputs: { f: "sub/u.gguf" } },
  };
  assert.deepEqual(TR.unlistedModels(graph, LISTED_MODELS), [], "the first two by base name of the listed name, the third by the whole name");
  assert.deepEqual(TR.unlistedModels({ "1": { class_type: "C", inputs: { f: "u.gguf" } } }, LISTED_MODELS), ["u.gguf"], "a listed name that is nested is not matched by its base name alone");
});

test("unlistedModels: no list names nothing, and a graph with no model file names nothing", () => {
  assert.deepEqual(TR.unlistedModels({ "1": { class_type: "A", inputs: { f: "x.pt" } } }, undefined), ["x.pt"]);
  assert.deepEqual(TR.unlistedModels({ "1": { class_type: "A", inputs: { n: 3 } }, "2": { class_type: "B" } }, LISTED_MODELS), []);
});

test("unlistedModels: on the hand-written templates the converter's graph loads only files the catalog's list names (the two readers agree, so the check defers none of them)", () => {
  for (const name of ["basic-t2i", "subgraph-i2v"]) {
    const wf = json(`${name}.workflow.json`);
    const r = convertForRun({ workflow: wf, objectInfo: objectInfo(), comfyVersion: "0.37.0", minComfyUIVersion: "0.3.0" });
    assert.equal(r.ok, true, name);
    const listed = requiredModels(wf).active.map((m) => ({ path: `models/${m.directory ?? "checkpoints"}/${m.name}` }));
    assert.ok(listed.length > 0, `${name} names a model file`);
    assert.deepEqual(TR.unlistedModels(r.graph, listed), [], name);
    assert.equal(TR.unlistedModels(r.graph, []).length, listed.length, `${name}: with the list emptied each file is unlisted`);
  }
});

// ---------------------------------------------------------------------------------------------------------------------
// An embedding is a file too. ComfyUI's CLIP tokenizer loads models/embeddings/<name>, then <name>.safetensors, <name>.pt and <name>.bin,
// from a word of the prompt text that starts with `embedding:`: no loader node and no file extension is involved, so a text override
// could name any file of that directory past the licence gate and the trust-on-first-use check.
// ---------------------------------------------------------------------------------------------------------------------

const EMBEDDING_SHAPES = [
  ["a bare name", "embedding:evil_ti, a cat", "evil_ti"],
  ["a name written with an extension the suffix test does not reach", "embedding:evil_ti.pt, a cat", "evil_ti.pt"],
  ["a weighted group", "(embedding:evil_ti:1.2) cat", "evil_ti"],
];

for (const [label, text] of EMBEDDING_SHAPES) {
  test(`applyOverrides: a text override that names an embedding is refused as override_is_file (${label})`, () => {
    const r = applyOverrides(fileGraph(), { "4.text": text });
    assert.deepEqual([r.ok, r.refusal?.code, r.refusal?.details.node], [false, "override_is_file", "4"], text);
  });
}

test("applyOverrides: the embedding identifier is matched in any letter case, as the fail-closed reading", () => {
  const r = applyOverrides(fileGraph(), { "4.text": "EMBEDDING:Evil_TI a cat" });
  assert.equal(r.refusal?.code, "override_is_file");
});

test("applyOverrides: a prompt that merely talks about embeddings, with no `embedding:` in it, still goes through", () => {
  const r = applyOverrides(fileGraph(), { "4.text": "an embedding of a cat, embeddings everywhere, embedding - a cat" });
  assert.equal(r.ok, true);
});

const embeddingGraph = (text) => ({ "4": { class_type: "CLIPTextEncode", inputs: { text, clip: ["1", 1] } } });

for (const [label, text, ref] of EMBEDDING_SHAPES) {
  test(`unlistedModels: a prompt that names an embedding is a model file under models/embeddings, unlisted unless the manifest names it (${label})`, () => {
    assert.deepEqual(TR.unlistedModels(embeddingGraph(text), LISTED_MODELS), [`models/embeddings/${ref}`], text);
  });
}

test("unlistedModels: an embedding is listed by the file ComfyUI would load, as written or with .safetensors, .pt or .bin, under models/embeddings only", () => {
  const g = embeddingGraph("embedding:evil_ti, a cat");
  for (const file of ["evil_ti.safetensors", "evil_ti.pt", "evil_ti.bin", "EVIL_TI.SafeTensors", "evil_ti"]) {
    assert.deepEqual(TR.unlistedModels(g, [{ path: `models/embeddings/${file}` }]), [], file);
  }
  assert.deepEqual(TR.unlistedModels(g, [{ path: "models/embeddings/evil_ti.ckpt" }]), ["models/embeddings/evil_ti"], "an extension ComfyUI does not try");
  assert.deepEqual(TR.unlistedModels(g, [{ path: "models/loras/evil_ti.safetensors" }]), ["models/embeddings/evil_ti"], "another class directory is not the one it loads from");
  assert.deepEqual(TR.unlistedModels(g, [{ path: "models/embeddings/sub/evil_ti.safetensors" }]), ["models/embeddings/evil_ti"], "a nested listed name is not matched by its base name alone");
});

test("unlistedModels: a name written with its extension is that file, and the extension is not appended to it", () => {
  const g = embeddingGraph("embedding:evil_ti.pt, a cat");
  assert.deepEqual(TR.unlistedModels(g, [{ path: "models/embeddings/evil_ti.pt" }]), []);
  assert.deepEqual(TR.unlistedModels(g, [{ path: "models/embeddings/evil_ti.safetensors" }]), ["models/embeddings/evil_ti.pt"]);
});

test("unlistedModels: every embedding a graph names is reported once, in graph order, whatever string input carries it", () => {
  const g = {
    "1": { class_type: "CLIPTextEncode", inputs: { text: "embedding:b, (embedding:a:0.8) embedding:b" } },
    "2": { class_type: "SomePromptNode", inputs: { positive: "x EMBEDDING:c.", seed: 3, n: ["1", 0] } },
  };
  assert.deepEqual(TR.unlistedModels(g, undefined), ["models/embeddings/b", "models/embeddings/a", "models/embeddings/c."]);
});

test("unlistedModels: text that mentions embeddings without the `embedding:` identifier names no file", () => {
  assert.deepEqual(TR.unlistedModels(embeddingGraph("an embedding of a cat, embeddings, embedding"), undefined), []);
});

// The manifest side of the match is normalised too (lower case, forward slashes, the `models/<class>/` prefix off): a manifest entry that is
// not lower case or not written with forward slashes still lists the file a graph names exactly. These two fail if that step is dropped.

test("unlistedModels: a manifest entry in mixed case lists the file a graph names exactly as the entry spells it", () => {
  assert.equal(LISTED_MODELS[2].path, "models/loras/Qwen-Image-Lightning-8steps-V1.0.safetensors");
  const graph = { "1": { class_type: "LoraLoaderModelOnly", inputs: { lora_name: "Qwen-Image-Lightning-8steps-V1.0.safetensors" } } };
  assert.deepEqual(TR.unlistedModels(graph, LISTED_MODELS), []);
  assert.deepEqual(TR.unlistedModels(graph, LISTED_MODELS.slice(0, 2)), ["Qwen-Image-Lightning-8steps-V1.0.safetensors"], "and it is the entry that lists it");
});

test("unlistedModels: a manifest entry written with backslashes lists the file a graph names exactly as it sits under the class directory", () => {
  assert.equal(LISTED_MODELS[3].path.split("\\").length, 4, "the fixture holds three real backslashes, not escapes that dropped out");
  const graph = { "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "Sub/Mixed.safetensors" } } };
  assert.deepEqual(TR.unlistedModels(graph, LISTED_MODELS), []);
  assert.deepEqual(TR.unlistedModels({ "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "Sub\\Mixed.safetensors" } } }, LISTED_MODELS), [], "and so does the same name written with a backslash");
  assert.deepEqual(TR.unlistedModels(graph, LISTED_MODELS.slice(0, 3)), ["Sub/Mixed.safetensors"], "and it is the entry that lists it");
});

// The embeddings branch of the manifest side reads the same normalised path (lower case, forward slashes) and is anchored to the start of
// it: a class directory written in another case or with backslashes lists the file, and a longer path that only contains `models/embeddings/`
// does not. Each test fails if one of those steps is dropped from the embeddings branch alone.

test("unlistedModels: an embeddings manifest entry in another case, or written with backslashes, lists the embedding the prompt names", () => {
  const g = embeddingGraph("a cat, embedding:Evil_TI");
  const both = "Models\\Embeddings\\EVIL_TI.pt";
  assert.equal(both.split("\\").length, 3, "the fixture holds two real backslashes, not escapes that dropped out");
  for (const path of [both, "models\\embeddings\\evil_ti.safetensors", "Models/Embeddings/EVIL_TI.bin"]) {
    assert.deepEqual(TR.unlistedModels(g, [{ path }]), [], path);
  }
  assert.deepEqual(TR.unlistedModels(g, [{ path: "Models\\Embeddings\\OTHER.pt" }]), ["models/embeddings/Evil_TI"], "and it is the entry that lists it");
});

test("unlistedModels: a manifest path that only contains models/embeddings/ further along, or after a longer directory name, does not list an embedding", () => {
  const g = embeddingGraph("embedding:evil_ti");
  for (const path of ["models/loras/models/embeddings/evil_ti.safetensors", "elsewhere/models/embeddings/evil_ti.pt", "models/embeddings-old/evil_ti.pt"]) {
    assert.deepEqual(TR.unlistedModels(g, [{ path }]), ["models/embeddings/evil_ti"], path);
  }
});
