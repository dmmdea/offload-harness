// render/preflight-graph-file.mjs
// Generic preflight for an ARBITRARY API-format graph: every node's class must exist
// in /object_info and every REQUIRED input must be wired, else the run would waste a
// GPU cycle. (The existing preflight-graph.mjs only checks the 3 built-in builders.)
const API = () => process.env.COMFY_API || "http://127.0.0.1:8188";

async function defaultFetchInfo(cls) {
  try {
    const r = await fetch(`${API()}/object_info/${encodeURIComponent(cls)}`);
    if (!r.ok) return null;
    const j = await r.json();
    return j[cls] || null;
  } catch { return null; }
}

export async function preflightGraph(graph, fetchInfo = defaultFetchInfo) {
  const missing = [], unknownClasses = [];
  for (const [node, spec] of Object.entries(graph || {})) {
    const cls = spec?.class_type;
    const info = await fetchInfo(cls);
    if (!info) { unknownClasses.push({ node, class_type: cls }); continue; }
    const required = Object.keys(info?.input?.required || {});
    const have = new Set(Object.keys(spec.inputs || {}));
    const miss = required.filter((k) => !have.has(k));
    if (miss.length) missing.push({ node, class_type: cls, inputs: miss });
  }
  return { ok: missing.length === 0 && unknownClasses.length === 0, missing, unknownClasses };
}
