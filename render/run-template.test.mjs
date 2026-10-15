// render/run-template.test.mjs - the run-template command line: list, get, readiness and resolve over a ComfyUI tree.
// The tree is the in-memory fixture of template-manifest-fixture.mjs; nothing here touches a real install or a GPU.
import { test } from "node:test";
import assert from "node:assert/strict";
import * as C from "./templates-catalog.mjs";
import { main } from "./run-template.mjs";
import { COMFY, MAP_PATH, fixtureIo, fixtureFiles, memIo } from "./template-manifest-fixture.mjs";

async function run(argv, io = fixtureIo()) {
  let text = "";
  let errText = "";
  const code = await main(argv, { io, out: (s) => { text += s; }, err: (s) => { errText += s; } });
  let json = null;
  try { json = JSON.parse(text); } catch { /* usage errors print no JSON */ }
  return { code, text, json, errText };
}
const base = ["--comfy-dir", COMFY, "--license-map", MAP_PATH];

test("list: prints one JSON object, sorted, runnable templates by default, the same text every time", async () => {
  const a = await run(["list", ...base]);
  const b = await run(["list", ...base]);
  assert.equal(a.code, 0);
  assert.equal(a.text, b.text);
  assert.deepEqual(a.json.templates.map((t) => t.name), ["tt_input", "tt_input_gone", "tt_old", "tt_open", "tt_zero"]);
  assert.equal(a.json.ok, true);
  assert.equal(a.json.counts.total, 14);
});

test("list: the api node ids of the node's own tree decide which templates are paid", async () => {
  const r = await run(["list", ...base, "--runnable", "false", "--kind", "api"]);
  assert.deepEqual(r.json.templates.map((t) => t.name), ["api_tt_paid"]);
});

test("list: --reads-input, --license-class, --text, --limit and --offset narrow it", async () => {
  assert.deepEqual((await run(["list", ...base, "--reads-input", "true"])).json.templates.map((t) => t.name), ["tt_input", "tt_input_gone"]);
  assert.deepEqual((await run(["list", ...base, "--license-class", "none"])).json.templates.map((t) => t.name), ["tt_zero"]);
  assert.deepEqual((await run(["list", ...base, "--text", "newer"])).json.templates.map((t) => t.name), ["tt_old"]);
  const page = await run(["list", ...base, "--limit", "1", "--offset", "3"]);
  assert.deepEqual(page.json.templates.map((t) => t.name), ["tt_open"]);
  assert.equal(page.json.matched, 5);
});

test("get: the manifest and licence class of one template", async () => {
  const r = await run(["get", "tt_open", ...base]);
  assert.equal(r.code, 0);
  assert.equal(r.json.ok, true);
  assert.equal(r.json.manifest.license.class, "permissive");
  assert.equal(r.json.manifest.models.length, 2);
});

test("get: an unknown template is a typed answer, not a crash", async () => {
  const r = await run(["get", "nope", ...base]);
  assert.equal(r.code, 0);
  assert.deepEqual([r.json.ok, r.json.class], [false, "unknown_template"]);
});

test("readiness: reads the node's own model directories and input directory", async () => {
  const ready = await run(["readiness", "tt_input", ...base]);
  assert.equal(ready.json.ready, true);
  const bare = await run(["readiness", "tt_input", ...base], fixtureIo({ models: [], inputs: [] }));
  assert.equal(bare.json.ready, false);
  assert.deepEqual(bare.json.blocking, ["models_missing", "inputs_missing"]);
  assert.deepEqual(bare.json.inputs_missing, ["ref.png"]);
});

test("readiness: --snapshot FILE stands in for scanning the tree, and a file that is not a snapshot is a usage error", async () => {
  const bareSnapshot = JSON.stringify(C.snapshotNode({ comfyDir: COMFY, io: fixtureIo({ models: [] }) }));
  const files = { ...fixtureFiles(), "/srv/fixture/bare.json": bareSnapshot, "/srv/fixture/junk.json": "{}" };
  const viaSnapshot = await run(["readiness", "tt_open", ...base, "--snapshot", "/srv/fixture/bare.json"], memIo(files));
  assert.equal(viaSnapshot.code, 0);
  assert.equal(viaSnapshot.json.ready, false, "the snapshot says no model is there, whatever the tree holds");
  const junk = await run(["readiness", "tt_open", ...base, "--snapshot", "/srv/fixture/junk.json"], memIo(files));
  assert.equal(junk.code, 2);
  assert.match(junk.errText, /snapshot/);
});

test("resolve: a paid-API template is a Defer with a typed class, exit 0", async () => {
  const r = await run(["resolve", "api_tt_paid", ...base]);
  assert.equal(r.code, 0);
  assert.deepEqual([r.json.ok, r.json.deferred, r.json.class, r.json.code], [false, true, "api_paid", "TEMPLATE_API_PAID"]);
});

test("resolve: an unlisted-licence template is refused", async () => {
  const r = await run(["resolve", "tt_unlisted", ...base]);
  assert.deepEqual([r.json.ok, r.json.refused, r.json.class], [false, true, "license_unknown"]);
});

test("resolve: a runnable template answers with the runtime manifest and the local-only marker", async () => {
  const r = await run(["resolve", "tt_input", ...base]);
  assert.equal(r.json.ok, true);
  assert.equal(r.json.local_only, true);
  assert.equal(r.json.runtime_manifest.workflow, "tt_input");
  assert.ok(r.json.runtime_manifest.models.every((m) => m.source_url === ""));
});

test("usage: no verb, an unknown verb, a missing name, no source and a bad number are exit 2 with a message", async () => {
  for (const argv of [[], ["frobnicate", ...base], ["get", ...base], ["list"], ["list", ...base, "--limit", "many"], ["list", ...base, "--runnable", "maybe"]]) {
    const r = await run(argv);
    assert.equal(r.code, 2, JSON.stringify(argv));
    assert.ok(r.errText.length > 0, JSON.stringify(argv));
  }
});

test("a tree with no templates package is a usage error that says so", async () => {
  const r = await run(["list", "--comfy-dir", "/srv/empty", "--license-map", MAP_PATH], memIo({ [MAP_PATH]: "{}" }));
  assert.equal(r.code, 2);
  assert.match(r.errText, /no comfyui_workflow_templates_json package/);
});
