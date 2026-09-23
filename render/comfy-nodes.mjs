// comfy-nodes.mjs — node-class preflight for the render runners: ask the running ComfyUI
// whether it can build every node class a graph names BEFORE the graph is POSTed.
//
// Why (OptiPlex parity audit, 2026-09-23): the Wan 2.2 graph ends in VHS_VideoCombine, a
// custom-node class. On a box without ComfyUI-VideoHelperSuite the POST came back 400
// `missing_node_type`, the runner died on it through process.exit, and Windows libuv
// aborted the exit (0xc0000409) — so the caller's defer read as a crash, not as "install
// this pack". The preflight turns that into one line naming the class and the pack, and
// nothing is submitted.
//
// It is a preflight, not a gate: when /object_info cannot be read (older server, transport
// error) it steps aside and the submission reports whatever the server reports. A class
// counts as missing only on a positive answer — /object_info/<class> returns 200 with an
// object that does not carry that class.
//
// Dependency-free, Node 18+.

// NODE_PACKS names the custom-node pack behind each non-core class the shipped builders
// emit (render/wf-*.mjs). Every other class they use is core ComfyUI. The Go side keeps
// the same table for `doctor` (internal/mediacap/routeneeds.go); a class missing here is
// still reported, just without the pack hint.
export const NODE_PACKS = Object.freeze({
  VHS_VideoCombine: "ComfyUI-VideoHelperSuite (https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite)",
  UNETLoaderDisTorch2MultiGPU: "ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU)",
  UnetLoaderGGUFDisTorch2MultiGPU:
    "ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU) together with ComfyUI-GGUF (https://github.com/city96/ComfyUI-GGUF)",
  UnetLoaderGGUF: "ComfyUI-GGUF (https://github.com/city96/ComfyUI-GGUF)",
});

// graphClasses: the distinct class_type values of an API-format graph, in first-seen order.
export function graphClasses(graph) {
  const seen = new Set();
  for (const node of Object.values(graph || {})) {
    if (node && typeof node.class_type === "string" && node.class_type) seen.add(node.class_type);
  }
  return [...seen];
}

// missingNodeClasses asks /object_info/<class> for every class the graph names.
// Returns { checked: true, missing: [...] }, or { checked: false, reason } when the server
// could not answer the question (the caller then submits and lets the server speak).
export async function missingNodeClasses(api, graph, { fetchImpl = fetch, timeoutMs = 10_000 } = {}) {
  const missing = [];
  for (const cls of graphClasses(graph)) {
    let r;
    try {
      r = await fetchImpl(`${api}/object_info/${encodeURIComponent(cls)}`, { signal: AbortSignal.timeout(timeoutMs) });
    } catch (e) {
      return { checked: false, missing: [], reason: `GET /object_info/${cls} failed: ${e?.message || e}` };
    }
    if (!r || !r.ok) {
      return { checked: false, missing: [], reason: `GET /object_info/${cls} answered HTTP ${r?.status}` };
    }
    let body;
    try { body = await r.json(); } catch {
      return { checked: false, missing: [], reason: `GET /object_info/${cls} returned no JSON` };
    }
    if (!body || typeof body !== "object" || !Object.prototype.hasOwnProperty.call(body, cls)) missing.push(cls);
  }
  return { checked: true, missing };
}

// missingNodeMessage is the one line a defer carries: greppable code, every missing class
// with its pack, where to install it, and that nothing was submitted.
export function missingNodeMessage(api, missing, comfyDir = "") {
  const parts = missing.map((cls) => (NODE_PACKS[cls] ? `${cls} (custom node pack ${NODE_PACKS[cls]})` : cls));
  const where = comfyDir ? `${comfyDir.replace(/[\\/]+$/, "")}/custom_nodes` : "ComfyUI's custom_nodes";
  return `MISSING_NODE: ComfyUI at ${api} has no node class ${parts.join(", ")} — install the pack into ${where} and restart ComfyUI (\`local-offload doctor\` lists every route's node classes). Nothing was submitted.`;
}

// assertNodeClasses throws MISSING_NODE when the server positively lacks a class, and logs
// (never throws) when the preflight could not run.
export async function assertNodeClasses(api, graph, { fetchImpl = fetch, comfyDir = "", log = (m) => console.error(m) } = {}) {
  const res = await missingNodeClasses(api, graph, { fetchImpl });
  if (!res.checked) {
    log(`comfy-nodes: node-class preflight skipped (${res.reason}); the submission reports any missing class`);
    return;
  }
  if (res.missing.length) throw new Error(missingNodeMessage(api, res.missing, comfyDir));
}
