// render/template-run.test.mjs - what the run-graph runner does with a template once ComfyUI is up: find it by name, convert it with the
// node's own /object_info, apply the caller's overrides. Pure functions over the hand-written goldens of template-goldens/ and the
// in-memory tree of template-manifest-fixture.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { applyOverrides, convertForRun, loadTemplate } from "./template-run.mjs";
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
