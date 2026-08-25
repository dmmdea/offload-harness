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

// A dynamic input GROUP is not wired under its own name. ComfyUI's autogrow inputs
// (COMFY_AUTOGROW_V3 — e.g. ComfyMathExpression's `values`, which grows a, b, c…) are
// serialised by the frontend as DOTTED CHILD KEYS: `values.a`, `values.b`. The group
// name itself never appears in the graph, so an exact-key check reports every such node
// as missing a required input and defers a graph ComfyUI would happily accept.
//
// Satisfaction rule: the exact key, or at least `min` dotted children (autogrow declares
// its own minimum; default 1). Counting children rather than just detecting one keeps the
// check honest for groups that require more than one wire. `min` is read BEFORE the
// zero-children early-out: an autogrow group declaring min=0 is satisfied by an EMPTY
// group (e.g. an optional `images` group serialised with no wires), and the old
// `children === 0 → false` deferred exactly that valid shape (found live 2026-08-24 on
// the Mage-Flow T2I template; the workaround was a dummy `"images": {}` key).
function satisfies(spec, key, def) {
  const have = Object.keys(spec.inputs || {});
  if (have.includes(key)) return true;
  const children = have.filter((h) => h.startsWith(`${key}.`)).length;
  const min = Number(def?.[1]?.template?.min ?? 1);
  return children >= (Number.isFinite(min) ? min : 1);
}

export async function preflightGraph(graph, fetchInfo = defaultFetchInfo) {
  const missing = [], unknownClasses = [];
  for (const [node, spec] of Object.entries(graph || {})) {
    const cls = spec?.class_type;
    const info = await fetchInfo(cls);
    if (!info) { unknownClasses.push({ node, class_type: cls }); continue; }
    const required = info?.input?.required || {};
    const miss = Object.keys(required).filter((k) => !satisfies(spec, k, required[k]));
    if (miss.length) missing.push({ node, class_type: cls, inputs: miss });
  }
  return { ok: missing.length === 0 && unknownClasses.length === 0, missing, unknownClasses };
}
