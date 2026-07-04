// preflight-graph.mjs — validate an API-format graph against the live ComfyUI /object_info
// BEFORE submitting, so missing required inputs are caught without spending a GPU cycle.
// Usage: node render/preflight-graph.mjs [hunyuan|wan]   (ComfyUI must be up on COMFY_API)
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";
import { buildWan22I2V } from "./wf-wan22-i2v.mjs";
import { buildAceStep } from "./wf-acestep.mjs";

const API = process.env.COMFY_API || "http://127.0.0.1:8188";
const which = process.argv[2] || "hunyuan";
const graph = which === "wan"
  ? buildWan22I2V({ imagePath: "x.png", prompt: "test", length: 49 })
  : which === "ace"
  ? buildAceStep({ prompt: "test, ambient, 90 bpm", seconds: 10 })
  : buildHunyuan15I2V({ imagePath: "x.png", prompt: "test", width: 480, height: 848, length: 33 });

let problems = 0;
for (const [id, node] of Object.entries(graph)) {
  const r = await fetch(`${API}/object_info/${node.class_type}`);
  if (!r.ok) { console.log(`node ${id} ${node.class_type}: NOT in /object_info (${r.status})`); problems++; continue; }
  const info = (await r.json())[node.class_type];
  const required = Object.keys(info?.input?.required || {});
  const have = new Set(Object.keys(node.inputs));
  const missing = required.filter((k) => !have.has(k));
  if (missing.length) { console.log(`node ${id} ${node.class_type}: MISSING required [${missing.join(", ")}]`); problems++; }
}
console.log(problems === 0 ? `OK: ${which} graph satisfies every node's required inputs` : `${problems} node(s) with problems`);
// set exitCode (don't process.exit) so pending fetch handles drain — avoids a Windows
// libuv UV_HANDLE_CLOSING abort that would mask the real exit code.
process.exitCode = problems === 0 ? 0 : 1;
