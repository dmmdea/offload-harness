import { test } from "node:test";
import assert from "node:assert/strict";
import { preflightGraph } from "./preflight-graph-file.mjs";

const OBJ = {
  KSampler: { input: { required: { model: {}, positive: {}, negative: {}, latent_image: {} } } },
  SaveImage: { input: { required: { images: {} } } },
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

test("preflightGraph flags an unknown node class", async () => {
  const graph = { "3": { class_type: "NoSuchNode", inputs: {} } };
  const r = await preflightGraph(graph, fetchInfo);
  assert.equal(r.ok, false);
  assert.deepEqual(r.unknownClasses, [{ node: "3", class_type: "NoSuchNode" }]);
});
