import { test } from "node:test";
import assert from "node:assert/strict";
import { preflightGraph } from "./preflight-graph-file.mjs";

// ComfyMathExpression's real schema, verbatim from /object_info: `values` is an autogrow
// GROUP the frontend serialises as dotted children (values.a, values.b), never under its
// own name. Pinned to the real shape so a preflight that only does exact-key matching
// fails here instead of in production, where it defers a graph ComfyUI would accept.
const AUTOGROW = ["COMFY_AUTOGROW_V3", { template: { names: ["a", "b", "c"], min: 1 } }];
const AUTOGROW_MIN2 = ["COMFY_AUTOGROW_V3", { template: { names: ["a", "b", "c"], min: 2 } }];

const OBJ = {
  KSampler: { input: { required: { model: {}, positive: {}, negative: {}, latent_image: {} } } },
  SaveImage: { input: { required: { images: {} } } },
  ComfyMathExpression: { input: { required: { expression: {}, values: AUTOGROW } } },
  NeedsTwo: { input: { required: { values: AUTOGROW_MIN2 } } },
};
const fetchInfo = (cls) => (OBJ[cls] ? OBJ[cls] : null);

test("preflightGraph OK when every required input is present", async () => {
  const graph = { "3": { class_type: "SaveImage", inputs: { images: ["8", 0] } } };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, true);
  assert.deepEqual(r.missing, []);
});

test("preflightGraph flags a missing required input", async () => {
  const graph = { "3": { class_type: "SaveImage", inputs: {} } };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, false);
  assert.deepEqual(r.missing, [{ node: "3", class_type: "SaveImage", inputs: ["images"] }]);
});

test("autogrow group satisfied by dotted child keys, not its own name", async () => {
  const graph = {
    "1": {
      class_type: "ComfyMathExpression",
      inputs: { expression: "a/2", "values.a": ["9", 0] },
    },
  };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, true, `expected ok, got missing=${JSON.stringify(r.missing)}`);
});

test("autogrow group with NO children is still missing", async () => {
  const graph = { "1": { class_type: "ComfyMathExpression", inputs: { expression: "a/2" } } };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, false);
  assert.deepEqual(r.missing, [{ node: "1", class_type: "ComfyMathExpression", inputs: ["values"] }]);
});

test("autogrow min is enforced — one child does not satisfy min:2", async () => {
  const one = { "1": { class_type: "NeedsTwo", inputs: { "values.a": ["9", 0] } } };
  assert.equal((await preflightGraph(one, fetchInfo)).ok, false);
  const two = { "1": { class_type: "NeedsTwo", inputs: { "values.a": ["9", 0], "values.b": ["9", 1] } } };
  assert.equal((await preflightGraph(two, fetchInfo)).ok, true);
});

test("a dotted key does not satisfy a DIFFERENT required input", async () => {
  // `images.foo` must not be read as satisfying `images` for a non-group input… but it
  // must also not regress the group case above. Prefix matching is per required key.
  const graph = { "3": { class_type: "SaveImage", inputs: { "imagesX.a": ["8", 0] } } };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, false);
  assert.deepEqual(r.missing, [{ node: "3", class_type: "SaveImage", inputs: ["images"] }]);
});

test("preflightGraph flags an unknown node class", async () => {
  const graph = { "3": { class_type: "NoSuchNode", inputs: {} } };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, false);
  assert.deepEqual(r.unknownClasses, [{ node: "3", class_type: "NoSuchNode" }]);
});
