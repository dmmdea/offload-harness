// node --test render/comfy-nodes.test.mjs
// The node-class preflight's answers, against a stubbed /object_info.
import { test } from "node:test";
import assert from "node:assert";
import { graphClasses, missingNodeClasses, missingNodeMessage, NODE_PACKS } from "./comfy-nodes.mjs";
import { buildWan22I2V } from "./wf-wan22-i2v.mjs";
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";

const reply = (status, body) => ({ ok: status >= 200 && status < 300, status, json: async () => body });

test("graphClasses: distinct classes in first-seen order, junk nodes ignored", () => {
  const g = { "1": { class_type: "A" }, "2": { class_type: "B" }, "3": { class_type: "A" }, "4": null, "5": {} };
  assert.deepEqual(graphClasses(g), ["A", "B"]);
});

test("missingNodeClasses: 200 {} is a missing class; 200 {cls:...} is present", async () => {
  const g = { "1": { class_type: "Have" }, "2": { class_type: "Lack" } };
  const res = await missingNodeClasses("http://c", g, { fetchImpl: async (u) => reply(200, u.endsWith("/Have") ? { Have: {} } : {}) });
  assert.deepEqual(res, { checked: true, missing: ["Lack"] });
});

test("missingNodeClasses: a transport error or a non-2xx is 'not checked', never 'missing'", async () => {
  const g = { "1": { class_type: "X" } };
  const down = await missingNodeClasses("http://c", g, { fetchImpl: async () => { throw new Error("ECONNREFUSED"); } });
  assert.equal(down.checked, false);
  assert.match(down.reason, /ECONNREFUSED/);
  const old = await missingNodeClasses("http://c", g, { fetchImpl: async () => reply(404, {}) });
  assert.equal(old.checked, false);
  assert.match(old.reason, /HTTP 404/);
});

test("missingNodeMessage names every class, its pack when known, and says nothing was submitted", () => {
  const m = missingNodeMessage("http://127.0.0.1:8188", ["VHS_VideoCombine", "SomeCoreNode"], "D:\\ComfyUI\\");
  assert.match(m, /^MISSING_NODE: ComfyUI at http:\/\/127\.0\.0\.1:8188 has no node class /);
  assert.match(m, /VHS_VideoCombine \(custom node pack ComfyUI-VideoHelperSuite \(https:\/\/github\.com\/Kosinkadink\/ComfyUI-VideoHelperSuite\)\)/);
  assert.match(m, /, SomeCoreNode — /);
  assert.match(m, /D:\\ComfyUI\/custom_nodes/);
  assert.match(m, /Nothing was submitted\.$/);
});

test("NODE_PACKS covers every custom-node class the video builders emit", () => {
  const custom = (g) => graphClasses(g).filter((c) => /GGUF|MultiGPU|^VHS_/.test(c));
  const graphs = [
    buildWan22I2V({ imagePath: "x.png", prompt: "p" }),
    buildWan22I2V({ imagePath: "x.png", prompt: "p", highUnet: "h.safetensors", lowUnet: "l.safetensors" }),
    buildHunyuan15I2V({ imagePath: "x.png", prompt: "p" }),
  ];
  for (const g of graphs) for (const c of custom(g)) assert.ok(NODE_PACKS[c], `${c} has no pack hint`);
});
