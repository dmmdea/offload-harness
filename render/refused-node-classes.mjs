// refused-node-classes.mjs — the node classes a run-graph door refuses by name.
//
// A graph is data: node classes wired together. But a third-party node pack can ship a class that EXECUTES A STRING INPUT AS
// CODE, and once that pack is installed in the ComfyUI the door uses, a graph holding the class runs the caller's code on the
// node's host. Nothing legitimate submits such a class through this door, so a graph that holds one is refused by name,
// before ComfyUI is started or contacted and before anything is provisioned: a typed NODE_CLASS_REFUSED defer whose ref is the class.
//
// The table is render/refused-node-classes.json, read once from beside this module; a row is a class whose pack source was read
// and found to execute a caller-supplied input. The match is exact and case-sensitive: `class_type` of a node of an API-format
// graph, `type` of a node of a UI-format workflow (its top-level nodes and those of its subgraphs). The scan reads the whole
// document, so a graph wrapped in another object is read too. A file that cannot be read, or that holds a malformed row, is
// NO table (null), and with no table every graph is refused: the failure is closed. The Go node keeps a copy
// (internal/mediacap/refusednodes.go) that a test holds to this file.
//
// Dependency-free, Node 18+.
import { readFileSync } from "node:fs";

// loadRefusedNodeClasses: the rows of a table file, each {class, pack, why}; null when the file cannot be read or any row is malformed.
export function loadRefusedNodeClasses(url = new URL("./refused-node-classes.json", import.meta.url)) {
  try {
    const file = JSON.parse(readFileSync(url, "utf8"));
    if (!file || !Array.isArray(file.classes)) return null;
    const rows = [];
    for (const r of file.classes) {
      if (!r || typeof r !== "object" || typeof r.class !== "string" || r.class === "" || typeof r.pack !== "string" || typeof r.why !== "string") return null;
      rows.push(Object.freeze({ class: r.class, pack: r.pack, why: r.why }));
    }
    return Object.freeze(rows);
  } catch {
    return null;
  }
}

export const REFUSED_NODE_CLASSES = loadRefusedNodeClasses();

// graphNodeClasses: every node class a graph or workflow document names, as a Set.
export function graphNodeClasses(doc) {
  const found = new Set();
  const stack = [doc];
  while (stack.length) {
    const v = stack.pop();
    if (!v || typeof v !== "object") continue;
    if (Array.isArray(v)) { for (const x of v) stack.push(x); continue; }
    if (typeof v.class_type === "string") found.add(v.class_type);
    for (const [k, x] of Object.entries(v)) {
      if (k === "nodes" && Array.isArray(x)) {
        for (const n of x) if (n && typeof n === "object" && typeof n.type === "string") found.add(n.type);
      }
      stack.push(x);
    }
  }
  return found;
}

// checkGraphClasses: null when the graph holds no refused class, else the typed defer {code, ref, detail}: ref is the first row of
// the table (in file order) the graph holds, the detail names the class, its pack and why, and says to remove the node.
export function checkGraphClasses(graph, rows = REFUSED_NODE_CLASSES) {
  if (!rows) {
    return {
      code: "NODE_CLASS_REFUSED", ref: "",
      detail: "render/refused-node-classes.json could not be read, so no graph can be checked against it and none is run; restore the file",
    };
  }
  const found = graphNodeClasses(graph);
  const row = rows.find((r) => found.has(r.class));
  if (!row) return null;
  return {
    code: "NODE_CLASS_REFUSED", ref: row.class,
    detail: `the graph holds a node of class ${row.class} (pack ${row.pack}), which ${row.why}, so it would run the caller's code on this node; remove the ${row.class} node from the graph (render/refused-node-classes.json)`,
  };
}

// assertGraphClasses: checkGraphClasses for a runner that builds nothing of its own to defer with: throws an Error whose message
// leads with the code, and whose .code and .ref carry it.
export function assertGraphClasses(graph, rows = REFUSED_NODE_CLASSES) {
  const r = checkGraphClasses(graph, rows);
  if (r) throw Object.assign(new Error(`${r.code}: ${r.detail}`), { code: r.code, ref: r.ref });
}
