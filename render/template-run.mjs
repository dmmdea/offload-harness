// render/template-run.mjs - what the run-graph runner does with a TEMPLATE once ComfyUI is up: find it by name in the node's own installed
// templates package, convert it with the node's own /object_info listing (template-to-graph.mjs), and apply the caller's overrides.
//
// The template is named, never a path: the runner resolves the name on whichever node runs it, against that node's own package and its
// own index (the file that carries `minComfyUIVersion`), so a name from another node's catalog that this node does not list is an
// unknown template and not a file read. Pure over an `io` (templates-catalog.mjs nodeIo shape) and the values handed in: no ComfyUI, no
// network, no GPU, no file written.
//
// Every refusal is `{ ok: false, refusal: { code, message, details } }`, the shape the converter refuses in.
import { convertTemplate } from "./template-to-graph.mjs";
import { flattenIndex, joinPath, locateTemplates, nodeIo } from "./templates-catalog.mjs";

const refusal = (code, message, details = {}) => ({ ok: false, refusal: { code, message, details } });
const hasOwn = (o, k) => Object.prototype.hasOwnProperty.call(o, k);
const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

// A template name: the file name of a template of the package, letters, digits, dot, underscore and hyphen, and a letter or digit first.
const TEMPLATE_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

// loadTemplate: the workflow and the index entry's minimum ComfyUI version, for a template this node's package lists.
export function loadTemplate({ comfyDir, name, io = nodeIo }) {
  if (typeof name !== "string" || !TEMPLATE_NAME.test(name) || name.includes("..")) {
    return refusal("bad_template_name", "a template is named by its package name (letters, digits, . _ -), never by a path", { name: String(name ?? "") });
  }
  const located = locateTemplates(comfyDir, io);
  if (!located) return refusal("templates_package_missing", `no comfyui_workflow_templates_json package found under ${comfyDir}`, { comfy_dir: comfyDir });
  let entries;
  try {
    entries = flattenIndex(JSON.parse(io.readText(joinPath(located.templatesDir, "index.json"))));
  } catch (e) {
    return refusal("bad_template", `the templates index cannot be read: ${e?.message ?? e}`, { name });
  }
  const entry = entries.find((e) => e && e.name === name);
  if (!entry) return refusal("unknown_template", `this node's templates package lists no template named ${name}`, { name });
  let workflow;
  try {
    workflow = JSON.parse(io.readText(joinPath(located.templatesDir, `${name}.json`)));
  } catch (e) {
    return refusal("bad_template", `${name}.json cannot be read as a workflow: ${e?.message ?? e}`, { name });
  }
  if (!isObj(workflow)) return refusal("bad_template", `${name}.json is not a workflow object`, { name });
  return { ok: true, workflow, min_comfyui_version: typeof entry.minComfyUIVersion === "string" ? entry.minComfyUIVersion : null, templates_dir: located.templatesDir };
}

// applyOverrides: set inputs of the converted graph. A key is `<node id>.<input name>`, split at the FIRST dot (a node id has none; an
// input name may: `values.a`). The node must be in the graph and the input must already be there as a value: an input the node does not
// have is never added, and one that is wired to another node is never cut. A value is a string, a number or a boolean. The graph handed in
// is not changed.
export function applyOverrides(graph, overrides) {
  if (overrides === undefined || overrides === null) return { ok: true, graph };
  if (!isObj(overrides)) return refusal("override_bad_key", "overrides is an object of <node id>.<input name> keys", {});
  const keys = Object.keys(overrides);
  if (keys.length === 0) return { ok: true, graph };
  const out = structuredClone(graph);
  for (const key of keys) {
    const dot = key.indexOf(".");
    if (dot <= 0 || dot === key.length - 1) return refusal("override_bad_key", `override key ${JSON.stringify(key)} is not <node id>.<input name>`, { key });
    const node = key.slice(0, dot);
    const input = key.slice(dot + 1);
    const value = overrides[key];
    if (typeof value !== "string" && typeof value !== "number" && typeof value !== "boolean") {
      return refusal("override_bad_value", `override ${key} is not a string, a number or a boolean`, { key });
    }
    if (!hasOwn(out, node) || !isObj(out[node]) || !isObj(out[node].inputs)) return refusal("override_unknown_node", `the converted graph has no node ${node}`, { key, node });
    if (!hasOwn(out[node].inputs, input)) return refusal("override_unknown_input", `node ${node} (${out[node].class_type}) has no input ${input}`, { key, node, input });
    if (Array.isArray(out[node].inputs[input])) return refusal("override_is_link", `input ${input} of node ${node} is wired to another node; an override sets values, it does not cut wires`, { key, node, input });
    out[node].inputs[input] = value;
  }
  return { ok: true, graph: out };
}

// convertForRun: the converter with the run's options, then the overrides. `comfyVersion` is the running ComfyUI's, `minComfyUIVersion`
// the index entry's. A seed pins the seed inputs; without one the template's own seed values stand.
export function convertForRun({ workflow, objectInfo, comfyVersion = null, minComfyUIVersion = null, seed, overrides }) {
  const options = { comfyVersion, minComfyUIVersion };
  if (seed !== undefined && seed !== null) options.seed = seed;
  const converted = convertTemplate(workflow, objectInfo, options);
  if (!converted.ok) return converted;
  const applied = applyOverrides(converted.graph, overrides);
  if (!applied.ok) return applied;
  return { ...converted, graph: applied.graph };
}
