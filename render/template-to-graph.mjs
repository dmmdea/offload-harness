// render/template-to-graph.mjs - the template-to-API converter: a ComfyUI UI-format workflow (the JSON the workflow-templates
// package ships and the frontend saves: nodes[], links[], definitions.subgraphs, widgets_values) in, the API-format prompt graph
// ({"<id>": {"class_type", "inputs"}}) that POST /prompt and the render builders take, out.
//
//   node render/template-to-graph.mjs --template FILE --object-info FILE [--comfy-version X.Y.Z] [--seed N]
//        [--min-comfyui-version X.Y.Z | --index FILE [--name NAME]] [--meta]
//
// prints {"ok":true,"graph":{...},"warnings":[...],"pinnedSeeds":[...],"stats":{...}} and exits 0, or
// {"ok":false,"refusal":{"code","message","details"}} and exits 1 (a typed refusal of a call that was well formed), or the
// same shape with code "usage" or "unreadable_input" and exit 2 (a call that could not be read).
//
// Clean room. This file was written from the public descriptions of the two formats, the MIT-licensed template files, the
// harness's own builders under render/ and the shape of an /object_info listing. The frontend's own graph-to-prompt code (which
// is GPL-3.0) and ComfyUI's server code were never opened or read as source; they are black boxes whose output a golden is
// compared with (see compareApiGraphs and the TEMPLATE_GOLDEN_DIR test).
//
// It is a pure function of three inputs: the template, an /object_info listing (never fetched here: the caller passes the one
// a running ComfyUI served, or a saved copy) and options. It reads nothing else, opens no network, starts no process and writes
// no file. Dependency-free (Node 18+, `node:` builtins only).
//
// What it does, in the order a node meets it:
//   - a template that expands past LIMITS (100,000 nodes of any kind, subgraph instances included, counted over every scope that
//     is entered; 64 levels of nesting) is refused (graph_too_large), and a node id that two scopes both give, or a subgraph
//     definition id that appears twice, is a bad_template: neither is resolved to whichever came last;
//   - mode 2 (muted) nodes are dropped; a wire that came from one is left out (a typed warning), or, for a widget input, the
//     widget's stored value stays;
//   - mode 4 (bypassed) nodes are not emitted; the i-th output of a type takes the i-th connected input of that type (a union
//     type matches when the two share a member, so positive and negative conditioning pass straight through), chains resolve,
//     no connected input of the type drops the wire (a typed warning), and a type with a different number of outputs and
//     connected inputs is refused (ambiguous_bypass), because which input an output takes would be a guess;
//   - subgraph instances are expanded: their nodes get the id `<instance>:<inner>` (nested: `<a>:<b>:<inner>`), a wire into the
//     instance reaches the inner nodes through the definition's input node (id -10), an inner wire into its output node
//     (id -20) is what leaves the instance, and the instance's widgets_values reach the inner widgets through the
//     definition's widget inputs, or through properties.proxyWidgets when the instance has them;
//   - Reroute nodes and PrimitiveNode widgets are resolved into the inputs they feed; Note and MarkdownNote are skipped;
//   - widgets_values are mapped to named inputs in the listing's input order (`input_order`, else the key order), counting the
//     hidden control_after_generate slot that follows a flagged INT, the upload slots of an image, video or audio loader, and
//     the dotted names a dynamic combo (`<combo>.<child>`, nested to any depth) and an autogrow (`<group>.<child>`, link-fed)
//     give their children;
//   - a link beats a widget value everywhere;
//   - a seed option pins the seed inputs (see pinSeeds below), and a seed-named input it left alone is listed in a warning.
//
// What it never does is guess. A class the listing does not know, a template that needs a newer ComfyUI than the one it was
// told about, a widget layout that does not add up, an input type it has no rule for, a bypassed node whose outputs cannot be
// paired with its inputs: each is a refusal with a code, the details that locate it, and no graph. What it cannot check it says
// so: a ComfyUI version given with no minimum to compare it with is a warning (version_check_skipped), never a silent pass.
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

// ---------------------------------------------------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------------------------------------------------

export const REFUSAL = Object.freeze({
  BAD_TEMPLATE: "bad_template",
  BAD_OBJECT_INFO: "bad_object_info",
  BAD_VERSION: "bad_version",
  COMFY_VERSION_UNKNOWN: "comfy_version_unknown",
  COMFY_TOO_OLD: "comfyui_too_old",
  UNKNOWN_NODE_CLASS: "unknown_node_class",
  AMBIGUOUS_BYPASS: "ambiguous_bypass",
  GROUP_NODE_UNSUPPORTED: "group_node_unsupported",
  SUBGRAPH_DEFINITION_MISSING: "subgraph_definition_missing",
  SUBGRAPH_CYCLE: "subgraph_cycle",
  SUBGRAPH_WIDGET_MISMATCH: "subgraph_widget_mismatch",
  DANGLING_LINK: "dangling_link",
  LINK_MISMATCH: "link_mismatch",
  LINK_CYCLE: "link_cycle",
  GRAPH_TOO_LARGE: "graph_too_large",
  WIDGET_MISMATCH: "widget_mismatch",
  UNSUPPORTED_INPUT_TYPE: "unsupported_input_type",
  PRIMITIVE_WITHOUT_VALUE: "primitive_without_value",
  BAD_SEED: "bad_seed",
  INTERNAL_ERROR: "internal_error",
});

class Refusal extends Error {
  constructor(code, message, details = {}) {
    super(message);
    this.code = code;
    this.details = details;
  }
}
const refuse = (code, message, details) => { throw new Refusal(code, message, details); };

// ---------------------------------------------------------------------------------------------------------------------
// Versions
// ---------------------------------------------------------------------------------------------------------------------

// parseVersion: "0.37.2" -> [0, 37, 2]. Any number of dotted numeric parts; a leading v and a -pre-release or +build suffix are
// ignored (a pre-release is compared as its release). Anything else is null.
export function parseVersion(v) {
  if (typeof v !== "string") return null;
  const m = v.trim().match(/^v?(\d+(?:\.\d+)*)(?:[-+].*)?$/);
  return m ? m[1].split(".").map(Number) : null;
}

// compareVersions: -1, 0 or 1, numeric part by part with missing parts read as 0 ("0.37" equals "0.37.0"). Throws a RangeError
// on a string that is not a version.
export function compareVersions(a, b) {
  const pa = parseVersion(a);
  const pb = parseVersion(b);
  if (pa === null) throw new RangeError(`not a version: ${JSON.stringify(a)}`);
  if (pb === null) throw new RangeError(`not a version: ${JSON.stringify(b)}`);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d !== 0) return d < 0 ? -1 : 1;
  }
  return 0;
}

// ---------------------------------------------------------------------------------------------------------------------
// Comparing two API graphs
// ---------------------------------------------------------------------------------------------------------------------

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const hasOwn = (o, k) => Object.prototype.hasOwnProperty.call(o, k);
// a JSON text with the keys of every object sorted, so two values compare by content and not by key order
const canon = (v) => JSON.stringify(v, (_k, x) => (isObj(x) ? Object.fromEntries(Object.keys(x).sort().map((k) => [k, x[k]])) : x));
const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

// compareApiGraphs: the differences between an API graph and an expected one, node by node, as readable lines; [] when they
// agree. Key order is never a difference. The unit is class_type plus inputs (a link is [nodeId, slot]); `_meta` is compared
// only with { meta: true }, because the frontend's export carries it and the harness's builders do not.
export function compareApiGraphs(actual, expected, { meta = false } = {}) {
  const out = [];
  const a = isObj(actual) ? actual : {};
  const e = isObj(expected) ? expected : {};
  const ids = [...new Set([...Object.keys(a), ...Object.keys(e)])].sort((x, y) => cmp(x, y));
  for (const id of ids) {
    if (!hasOwn(a, id)) { out.push(`${id}: missing`); continue; }
    if (!hasOwn(e, id)) { out.push(`${id}: extra`); continue; }
    const na = a[id] ?? {};
    const ne = e[id] ?? {};
    if (na.class_type !== ne.class_type) out.push(`${id}: class_type ${na.class_type} != ${ne.class_type}`);
    const ia = isObj(na.inputs) ? na.inputs : {};
    const ie = isObj(ne.inputs) ? ne.inputs : {};
    for (const k of [...new Set([...Object.keys(ia), ...Object.keys(ie)])].sort((x, y) => cmp(x, y))) {
      if (!hasOwn(ia, k)) out.push(`${id}.${k}: missing (expected ${canon(ie[k])})`);
      else if (!hasOwn(ie, k)) out.push(`${id}.${k}: extra (${canon(ia[k])})`);
      else if (canon(ia[k]) !== canon(ie[k])) out.push(`${id}.${k}: ${canon(ia[k])} != ${canon(ie[k])}`);
    }
    if (meta && canon(na._meta) !== canon(ne._meta)) out.push(`${id}._meta: ${canon(na._meta)} != ${canon(ne._meta)}`);
  }
  return out;
}

// ---------------------------------------------------------------------------------------------------------------------
// Constants of the two formats
// ---------------------------------------------------------------------------------------------------------------------

// The types the frontend draws as a widget (a COMBO is also any input whose type is a list of options). COLOR is the colour
// picker: its value is the hex string, stored as it is (UNVERIFIED against a frontend export: the template files only show the
// stored string).
const WIDGET_TYPES = new Set(["INT", "FLOAT", "STRING", "BOOLEAN", "COMBO", "COLOR"]);
// The values of the hidden combo the frontend adds after a flagged INT widget.
const CONTROL_WORDS = new Set(["fixed", "increment", "decrement", "randomize"]);
// The extra widgets_values slots a loader's upload button owns, by the option that marks it. Counted from the MIT template files:
// an image loader stores ["a.png","image"], a video loader ["a.mp4","image"], an audio loader ["a.mp3",null,null].
const UPLOAD_SLOTS = [["image_upload", 1], ["video_upload", 1], ["audio_upload", 2]];
// Output and preview nodes whose widgets_values carry trailing values of frontend-only display widgets (a text preview, a 3D
// viewer's state): the listing names no input for them, so they cannot be API inputs. Up to this many trailing values are
// ignored (and said so); one more is a refusal. Counted from the MIT template files (PreviewAny stores [null, null, null] or
// [null, null, false], the others one display value), UNVERIFIED against a frontend export.
const DISPLAY_VALUES = new Map([["PreviewAny", 3], ["GetImageSize", 1], ["SaveGLB", 1], ["Preview3D", 1]]);
const NOTE_TYPES = new Set(["Note", "MarkdownNote"]);
const SEED_NAME = /^(?:noise_)?seed$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const DYNAMIC_COMBO = "COMFY_DYNAMICCOMBO_V3";
const AUTOGROW = "COMFY_AUTOGROW_V3";
const MATCHTYPE = "COMFY_MATCHTYPE_V3";
// What one call may expand to. A template of a few kilobytes can nest subgraph definitions so that one instance stands for millions
// of nodes, or so deep that a recursive walk runs out of stack: past either bound the call is refused (graph_too_large), never
// carried on into a crash or a memory exhaustion. Real templates sit far below both (a handful of nodes per level, three levels).
export const LIMITS = Object.freeze({
  maxNodes: 100000, // every node met in every scope that is entered, of any kind (live, muted, bypassed, Note, Reroute, primitive, instance)
  maxSubgraphDepth: 64, // instances inside instances
});
const SG_INPUT_NODE = "-10";
const SG_OUTPUT_NODE = "-20";

const clone = (v) => (v === undefined ? undefined : structuredClone(v));
const lastSegment = (name) => name.slice(name.lastIndexOf(".") + 1);

// A type string may be a union ("IMAGE,MASK", "FLOAT,INT,BOOLEAN") and "*" matches anything.
function typesMatch(a, b) {
  if (typeof a !== "string" || typeof b !== "string") return false;
  if (a === "*" || b === "*") return true;
  const sb = new Set(b.split(","));
  return a.split(",").some((t) => sb.has(t));
}

// ---------------------------------------------------------------------------------------------------------------------
// The object_info
// ---------------------------------------------------------------------------------------------------------------------

// widgetKind: the widget type an input spec makes, or null when it is a socket only. A list is a combo; INT, FLOAT, STRING,
// BOOLEAN and COMBO are widgets unless forceInput says socket; a union type such as "FLOAT,INT" is a widget of its `widgetType`.
function widgetKind(type, opts) {
  if (opts.forceInput === true) return null;
  if (Array.isArray(type)) return "COMBO";
  if (typeof type !== "string") return null;
  if (WIDGET_TYPES.has(type)) return type;
  if (typeof opts.widgetType === "string" && WIDGET_TYPES.has(opts.widgetType)) return opts.widgetType;
  return null;
}

// specEntries: [[name, spec]] of an input group ({required?, optional?}) in order: `required` then `optional`; inside a group the
// listing's input_order when it has one (it must name exactly the group's keys), else the key order. `hidden` is not an input.
function specEntries(cls, info) {
  if (!isObj(info) || !isObj(info.input)) refuse(REFUSAL.BAD_OBJECT_INFO, `object_info: ${cls} has no input description`, { class_type: cls });
  const entries = [];
  for (const section of ["required", "optional"]) {
    const group = info.input[section] ?? {};
    if (!isObj(group)) refuse(REFUSAL.BAD_OBJECT_INFO, `object_info: ${cls}.input.${section} is not an object`, { class_type: cls, section });
    const keys = Object.keys(group);
    let names = keys;
    const order = isObj(info.input_order) ? info.input_order[section] : undefined;
    if (order !== undefined) {
      const ok = Array.isArray(order) && order.length === keys.length && new Set(order).size === order.length && order.every((n) => hasOwn(group, n));
      if (!ok) refuse(REFUSAL.BAD_OBJECT_INFO, `object_info: ${cls}.input_order.${section} does not list the keys of input.${section}`, { class_type: cls, section });
      names = order;
    }
    for (const n of names) entries.push([n, group[n]]);
  }
  return entries;
}

// a nested group (a dynamic combo option's `inputs`, an autogrow template's `input`): the key order, required then optional
function nestedEntries(group) {
  const entries = [];
  if (!isObj(group)) return entries;
  for (const section of ["required", "optional"]) {
    if (isObj(group[section])) for (const [n, spec] of Object.entries(group[section])) entries.push([n, spec]);
  }
  return entries;
}

function checkSpec(cls, name, spec) {
  if (!Array.isArray(spec) || spec.length === 0) refuse(REFUSAL.BAD_OBJECT_INFO, `object_info: ${cls}.${name} is not an input spec`, { class_type: cls, input: name });
  return [spec[0], isObj(spec[1]) ? spec[1] : {}];
}

// ---------------------------------------------------------------------------------------------------------------------
// The template: scopes, nodes, links
// ---------------------------------------------------------------------------------------------------------------------

function normLink(l) {
  let id, origin, originSlot, target, targetSlot;
  if (Array.isArray(l) && l.length >= 5) [id, origin, originSlot, target, targetSlot] = l;
  else if (isObj(l)) ({ id, origin_id: origin, origin_slot: originSlot, target_id: target, target_slot: targetSlot } = l);
  else return null;
  const idOk = (x) => typeof x === "number" || (typeof x === "string" && x !== "");
  if (!idOk(id) || !idOk(origin) || !idOk(target) || !Number.isInteger(originSlot) || !Number.isInteger(targetSlot)) return null;
  return { id, origin: String(origin), originRaw: origin, originSlot, target: String(target), targetSlot };
}

function makeScope(nodesArr, linksArr, prefix, parent, def) {
  if (!Array.isArray(nodesArr)) refuse(REFUSAL.BAD_TEMPLATE, "the template has no nodes array", { where: prefix || "root" });
  const nodes = new Map();
  for (const n of nodesArr) {
    if (!isObj(n) || !(typeof n.id === "number" || (typeof n.id === "string" && n.id !== "")) || typeof n.type !== "string" || n.type === "") {
      refuse(REFUSAL.BAD_TEMPLATE, "a node has no id or no type", { where: prefix || "root" });
    }
    const k = String(n.id);
    if (nodes.has(k)) refuse(REFUSAL.BAD_TEMPLATE, `node id ${k} appears twice`, { where: prefix || "root", id: n.id });
    // the slot lists are read as arrays further on (a bypass pairs them, an instance is wired through them): a list that is
    // absent or null is an empty one, anything else that is not an array is the template's fault, caught here and nowhere later
    for (const field of ["inputs", "outputs"]) {
      if (n[field] !== undefined && n[field] !== null && !Array.isArray(n[field])) {
        refuse(REFUSAL.BAD_TEMPLATE, `node ${prefix}${k}: ${field} is not an array`, { where: prefix || "root", node: `${prefix}${k}`, field });
      }
    }
    nodes.set(k, n);
  }
  if (linksArr !== undefined && linksArr !== null && !Array.isArray(linksArr)) refuse(REFUSAL.BAD_TEMPLATE, "links is not an array", { where: prefix || "root" });
  const links = new Map();
  for (const l of linksArr ?? []) {
    const rec = normLink(l);
    if (rec === null) refuse(REFUSAL.BAD_TEMPLATE, "a link record is malformed", { where: prefix || "root" });
    const lk = String(rec.id);
    if (links.has(lk)) refuse(REFUSAL.BAD_TEMPLATE, `link id ${lk} appears twice`, { where: prefix || "root", id: rec.id });
    links.set(lk, rec);
  }
  return { prefix, nodes, links, parent, def, literals: new Map() };
}

// ---------------------------------------------------------------------------------------------------------------------
// The conversion
// ---------------------------------------------------------------------------------------------------------------------

// checkVersions: the first thing a call is held to, before the seed, the listing and the template are looked at, so a too-old
// ComfyUI is never hidden behind another refusal. With no minimum there is nothing to compare: a version that was given is then
// said to be unchecked in a warning (the call is not refused, and it is never silent).
function checkVersions(options, warnings) {
  const min = options.minComfyUIVersion ?? null;
  const have = options.comfyVersion ?? null;
  if (min === null) {
    if (have !== null) {
      warnings.push({
        code: "version_check_skipped", comfyVersion: have,
        reason: "no minimum ComfyUI version is known for this template, so the version was not checked against anything",
      });
    }
    return;
  }
  if (parseVersion(min) === null) refuse(REFUSAL.BAD_VERSION, `the template's minimum ComfyUI version is not a version: ${JSON.stringify(min)}`, { minComfyUIVersion: min });
  if (have === null) {
    refuse(REFUSAL.COMFY_VERSION_UNKNOWN, `the template needs ComfyUI ${min} or newer and no ComfyUI version was given (it is never assumed)`, { required: min });
  }
  if (parseVersion(have) === null) refuse(REFUSAL.BAD_VERSION, `the ComfyUI version is not a version: ${JSON.stringify(have)}`, { comfyVersion: have });
  if (compareVersions(have, min) < 0) {
    refuse(REFUSAL.COMFY_TOO_OLD, `the template needs ComfyUI ${min} or newer, this one is ${have}`, { required: min, have });
  }
}

function build(template, objectInfo, options) {
  const warnings = [];
  checkVersions(options, warnings);
  const seed = options.seed ?? null;
  if (seed !== null && !(typeof seed === "number" && Number.isSafeInteger(seed) && seed >= 0)) {
    refuse(REFUSAL.BAD_SEED, "the seed must be a non-negative safe integer", { seed: typeof seed === "number" ? seed : String(seed) });
  }
  if (!isObj(objectInfo) || Object.keys(objectInfo).length === 0) {
    refuse(REFUSAL.BAD_OBJECT_INFO, "the object_info must be a non-empty object keyed by node class", {});
  }
  if (!isObj(template) || !Array.isArray(template.nodes)) refuse(REFUSAL.BAD_TEMPLATE, "not a UI-format workflow: no nodes array", {});

  const defs = new Map();
  const subgraphs = template.definitions?.subgraphs;
  if (subgraphs !== undefined && !Array.isArray(subgraphs)) refuse(REFUSAL.BAD_TEMPLATE, "definitions.subgraphs is not an array", {});
  for (const d of subgraphs ?? []) {
    if (!isObj(d) || typeof d.id !== "string" || d.id === "") refuse(REFUSAL.BAD_TEMPLATE, "a subgraph definition has no id", {});
    for (const field of ["inputs", "outputs"]) {
      if (d[field] !== undefined && d[field] !== null && !Array.isArray(d[field])) {
        refuse(REFUSAL.BAD_TEMPLATE, `subgraph ${d.id}: ${field} is not an array`, { subgraph: d.id, field });
      }
    }
    if (defs.has(d.id)) refuse(REFUSAL.BAD_TEMPLATE, `subgraph definition ${d.id} appears twice`, { subgraph: d.id });
    defs.set(d.id, d);
  }

  const ctx = {
    oi: objectInfo, defs, seed,
    warnings, pinnedSeeds: [], live: [], instances: new Map(), proxies: new Map(), entryCache: new Map(), keys: new Set(), budget: 0,
    stats: { nodes: 0, expandedSubgraphs: 0, mutedNodes: 0, bypassedNodes: 0, notes: 0, reroutes: 0, primitiveNodes: 0 },
  };
  const root = makeScope(template.nodes, template.links, "", null, null);
  collect(ctx, root, []);

  // every class the live nodes use must be in the listing: all unknown ones are named at once, never guessed
  const unknown = new Map();
  for (const item of ctx.live) {
    if (!hasOwn(objectInfo, item.node.type)) {
      if (!unknown.has(item.node.type)) unknown.set(item.node.type, []);
      unknown.get(item.node.type).push(item.key);
    }
  }
  if (unknown.size > 0) {
    const classes = [...unknown.entries()].sort(([a], [b]) => cmp(a, b)).map(([class_type, nodes]) => ({ class_type, nodes }));
    refuse(REFUSAL.UNKNOWN_NODE_CLASS, `the object_info does not know ${classes.map((c) => c.class_type).join(", ")}`, { classes });
  }

  const graph = {};
  for (const item of ctx.live) graph[item.key] = buildNode(ctx, item, options);
  ctx.stats.nodes = ctx.live.length;

  // a proxy widget nobody used names a widget the converter could not find: say so
  for (const [innerKey, byName] of ctx.proxies) {
    for (const [name, p] of byName) {
      if (!p.used) ctx.warnings.push({ code: "proxy_widget_unmatched", node: p.instance, input: name, target: innerKey.slice(p.instance.length + 1) });
    }
  }
  return { ok: true, graph, warnings: ctx.warnings, pinnedSeeds: ctx.pinnedSeeds, stats: ctx.stats };
}

export function convertTemplate(template, objectInfo, options = {}) {
  try {
    return build(template, objectInfo, isObj(options) ? options : {});
  } catch (e) {
    if (e instanceof Refusal) return { ok: false, refusal: { code: e.code, message: e.message, details: e.details } };
    // a defect in this file, not in the template: still a typed answer, and a survey test fails on it
    return { ok: false, refusal: { code: REFUSAL.INTERNAL_ERROR, message: String(e?.message ?? e), details: {} } };
  }
}

// ---- node kinds ------------------------------------------------------------------------------------------------------

// classify: what a node is to the converter. Mode first (a muted Note is muted), then the frontend-only classes, then a subgraph
// instance, then a real class.
function classify(ctx, node) {
  if (node.mode === 2) return "muted";
  if (NOTE_TYPES.has(node.type)) return "note";
  if (node.mode === 4) return "bypassed";
  if (node.type === "Reroute") return "reroute";
  if (node.type === "PrimitiveNode") return "primitive";
  if (ctx.defs.has(node.type)) return "instance";
  if (node.type.startsWith("workflow>") || node.type.startsWith("workflow/")) return "group";
  if (UUID.test(node.type) && !hasOwn(ctx.oi, node.type)) return "missing_definition";
  return "real";
}

// charge: one more node counts against LIMITS.maxNodes at the moment collect meets it, whatever its kind. A Note costs as much as a
// live node: a definition stuffed with Notes, muted or bypassed nodes expands as far as one of live ones, and each instance of it
// is walked, recorded in ctx.keys and given a scope map. So an expansion bomb is refused after the first 100,000 nodes it meets,
// before any per-node work, and not after all of it.
function charge(ctx, key) {
  if (++ctx.budget > LIMITS.maxNodes) {
    refuse(REFUSAL.GRAPH_TOO_LARGE, `the template expands to more than ${LIMITS.maxNodes} nodes (every node of every expanded subgraph counts, of any kind, and so does each subgraph instance; stopped at ${key})`, { limit: LIMITS.maxNodes, what: "nodes", node: key });
  }
}

// collect: walk a scope in file order; real live nodes go to ctx.live under their final id, live subgraph instances are entered.
// Every node met is charged first, of any kind: nothing below (the key record, the classification, an instance's scope) is paid for
// a node the bound has not counted.
function collect(ctx, scope, stack) {
  for (const node of scope.nodes.values()) {
    const key = scope.prefix + node.id;
    charge(ctx, key);
    const kind = classify(ctx, node);
    // every node of every scope has one final id: two that read the same (a root node whose id is the path of an inner node) would
    // be one API node, or one trail key, with the last silently winning
    if (ctx.keys.has(key)) refuse(REFUSAL.BAD_TEMPLATE, `node id ${key} is given twice (a node of one scope has the id path of a node of another)`, { where: scope.prefix || "root", id: key });
    ctx.keys.add(key);
    switch (kind) {
      case "muted": ctx.stats.mutedNodes++; break;
      case "bypassed": ctx.stats.bypassedNodes++; break;
      case "note": ctx.stats.notes++; break;
      case "reroute": ctx.stats.reroutes++; break;
      case "primitive": ctx.stats.primitiveNodes++; break;
      case "group":
        refuse(REFUSAL.GROUP_NODE_UNSUPPORTED, `node ${key} is a group node (${node.type}); group nodes are not converted`, { node: key, type: node.type });
        break;
      case "missing_definition":
        refuse(REFUSAL.SUBGRAPH_DEFINITION_MISSING, `node ${key} is shaped like a subgraph instance but the template defines no subgraph ${node.type}`, { node: key, type: node.type });
        break;
      case "instance": {
        if (stack.includes(node.type)) refuse(REFUSAL.SUBGRAPH_CYCLE, `subgraph ${node.type} contains itself (reached again at ${key})`, { node: key, type: node.type });
        if (stack.length >= LIMITS.maxSubgraphDepth) {
          refuse(REFUSAL.GRAPH_TOO_LARGE, `subgraphs are nested more than ${LIMITS.maxSubgraphDepth} levels deep (stopped at ${key})`, { limit: LIMITS.maxSubgraphDepth, what: "subgraph depth", node: key });
        }
        const def = ctx.defs.get(node.type);
        const inner = makeScope(def.nodes, def.links, `${key}:`, { scope, node }, def);
        enterInstance(ctx, scope, node, def, inner, key);
        ctx.instances.set(key, inner);
        ctx.stats.expandedSubgraphs++;
        collect(ctx, inner, [...stack, node.type]);
        break;
      }
      default:
        ctx.live.push({ key, node, scope });
    }
  }
}

// Does the definition's input j show up as a widget on the instance? By its type, or because an inner consumer takes it into a
// widget input.
function isWidgetInput(def, inner, j) {
  const t = String(def.inputs?.[j]?.type ?? "").split(",")[0];
  if (WIDGET_TYPES.has(t)) return true;
  for (const l of inner.links.values()) {
    if (l.origin === SG_INPUT_NODE && l.originSlot === j) {
      const target = inner.nodes.get(l.target);
      if (target?.inputs?.[l.targetSlot]?.widget) return true;
    }
  }
  return false;
}

// enterInstance: where the instance's own widgets_values go. With properties.proxyWidgets they belong, in order, to
// [innerNodeId, widgetName] pairs and become overrides of those inner widgets. Without them they align with the definition's
// widget inputs (not with the instance's own inputs array, which can list fewer) and become the literal each such input carries.
function enterInstance(ctx, parent, inst, def, inner, key) {
  const wv = inst.widgets_values;
  const has = Array.isArray(wv) && wv.length > 0;
  const proxies = inst.properties?.proxyWidgets;
  if (Array.isArray(proxies) && proxies.length > 0) {
    const ok = proxies.every((p) => Array.isArray(p) && p.length >= 2 && (typeof p[0] === "string" || typeof p[0] === "number") && typeof p[1] === "string");
    if (!ok) refuse(REFUSAL.SUBGRAPH_WIDGET_MISMATCH, `node ${key}: properties.proxyWidgets has a malformed entry`, { node: key });
    if (!has) return;
    if (wv.length !== proxies.length) {
      refuse(REFUSAL.SUBGRAPH_WIDGET_MISMATCH, `node ${key}: ${wv.length} widgets_values for ${proxies.length} proxy widgets`, { node: key, expected: proxies.length, got: wv.length });
    }
    proxies.forEach((p, i) => {
      const innerKey = `${key}:${p[0]}`;
      if (!ctx.proxies.has(innerKey)) ctx.proxies.set(innerKey, new Map());
      ctx.proxies.get(innerKey).set(p[1], { value: clone(wv[i]), used: false, instance: key });
    });
    return;
  }
  if (!has) return;
  const widgetInputs = [];
  (def.inputs ?? []).forEach((_i, j) => { if (isWidgetInput(def, inner, j)) widgetInputs.push(j); });
  if (wv.length !== widgetInputs.length) {
    refuse(REFUSAL.SUBGRAPH_WIDGET_MISMATCH, `node ${key}: ${wv.length} widgets_values for ${widgetInputs.length} widget inputs of its subgraph`, { node: key, expected: widgetInputs.length, got: wv.length });
  }
  widgetInputs.forEach((j, k) => inner.literals.set(j, clone(wv[k])));
}

// ---- resolving a wire to what produces it ----------------------------------------------------------------------------
// A source is one of: { kind: "ref", key, slot } (a real node's output), { kind: "literal", value } (a PrimitiveNode or an
// instance widget), { kind: "muted" } (it came from a muted node), { kind: "none", reason } (it came from nothing).
//
// Finding the source is a walk along ONE path: a Reroute hands on its one input, a bypassed node the one input its output takes, a
// subgraph instance the one inner wire that leaves it. So each step either settles on a source or names the next wire to follow
// ({ kind: "follow", scope, link }), and settle() loops over the steps with a single visited set. That is what keeps a chain of a
// hundred thousand Reroute nodes off the call stack and costs one set insertion per hop, not a copy of the whole trail.

const MUTED = Object.freeze({ kind: "muted" });
const nothing = (reason) => ({ kind: "none", reason });
const follow = (scope, link) => ({ kind: "follow", scope, link });

function settle(ctx, step, trail) {
  while (step.kind === "follow") step = originStep(ctx, step.scope, step.link, trail);
  return step;
}

// resolveInput: the source of input `idx` of `node`, with `trail` the (node, slot) pairs this walk has passed through.
function resolveInput(ctx, scope, node, idx, trail) {
  return settle(ctx, inputStep(ctx, scope, node, idx), trail);
}

// inputStep: the wire into input `idx` of `node`. The link must exist and point back at this node and slot.
function inputStep(ctx, scope, node, idx) {
  const slot = node.inputs[idx];
  const key = scope.prefix + node.id;
  const link = scope.links.get(String(slot.link));
  if (!link) refuse(REFUSAL.DANGLING_LINK, `node ${key} input ${slot.name ?? idx} names link ${slot.link}, which the template does not list`, { node: key, input: slot.name, link: slot.link });
  if (link.target !== String(node.id) || link.targetSlot !== idx) {
    refuse(REFUSAL.LINK_MISMATCH, `link ${link.id} does not end at node ${key} input ${idx}`, { node: key, input: slot.name, link: link.id });
  }
  return follow(scope, link);
}

function originStep(ctx, scope, link, trail) {
  if (link.origin === SG_INPUT_NODE) {
    if (!scope.parent) refuse(REFUSAL.DANGLING_LINK, `link ${link.id} starts at a subgraph input node outside any subgraph`, { link: link.id, origin: link.originRaw });
    return subgraphInputStep(ctx, scope, link.originSlot);
  }
  const origin = scope.nodes.get(link.origin);
  if (!origin) refuse(REFUSAL.DANGLING_LINK, `link ${link.id} starts at node ${link.originRaw}, which is not in the template`, { link: link.id, origin: link.originRaw });
  return outputStep(ctx, scope, origin, link.originSlot, trail);
}

function outputStep(ctx, scope, node, slot, trail) {
  const id = scope.prefix + node.id;
  const tk = `${id}#${slot}`;
  if (trail.has(tk)) refuse(REFUSAL.LINK_CYCLE, `the wires through ${id} loop back on themselves`, { node: id });
  trail.add(tk);
  switch (classify(ctx, node)) {
    case "muted": return MUTED;
    case "note": return nothing("unconnected");
    case "primitive": {
      const wv = node.widgets_values;
      if (!Array.isArray(wv) || wv.length === 0) refuse(REFUSAL.PRIMITIVE_WITHOUT_VALUE, `PrimitiveNode ${id} feeds an input and holds no value`, { node: id });
      return { kind: "literal", value: clone(wv[0]) };
    }
    case "reroute": {
      const first = node.inputs?.[0];
      if (!first || first.link === null || first.link === undefined) return nothing("unconnected");
      return inputStep(ctx, scope, node, 0);
    }
    case "bypassed": return passThroughStep(ctx, scope, node, slot);
    case "instance": {
      const inner = ctx.instances.get(id);
      const exit = [...inner.links.values()].find((l) => l.target === SG_OUTPUT_NODE && l.targetSlot === slot);
      if (!exit) return nothing("unconnected");
      return follow(inner, exit);
    }
    default: return { kind: "ref", key: id, slot };
  }
}

// A bypassed node hands its outputs to its inputs positionally by type: the i-th output of a type T takes the i-th connected input
// of type T (a union type matches when the two share a member, "*" matches any type). A node with two outputs of one type, such as
// the positive and negative conditioning of a ControlNet apply node, keeps them apart that way. With no connected input of the type
// there is nothing to pass: the wire is dropped (a typed warning where it was read). When the type has a different number of outputs
// and connected inputs, which input an output takes would be a guess, so the call is refused (ambiguous_bypass). Only an output that
// something reads is resolved, so a bypassed node nobody reads is never refused.
function passThroughStep(ctx, scope, node, slot) {
  const outputs = node.outputs ?? [];
  const outType = outputs[slot]?.type;
  const wired = [];
  (node.inputs ?? []).forEach((i, j) => { if (i && i.link !== null && i.link !== undefined && typesMatch(i.type, outType)) wired.push(j); });
  if (wired.length === 0) return nothing("bypass_no_match");
  const same = [];
  outputs.forEach((o, j) => { if (o?.type === outType) same.push(j); });
  if (same.length !== wired.length) {
    const id = scope.prefix + node.id;
    refuse(REFUSAL.AMBIGUOUS_BYPASS,
      `bypassed node ${id} has ${same.length} output(s) of type ${outType} and ${wired.length} connected input(s) of it: which input an output takes is ambiguous, and is never guessed`,
      { node: id, type: outType, output: slot, outputs: same.length, inputs: wired.length });
  }
  return inputStep(ctx, scope, node, wired[same.indexOf(slot)]);
}

// An inner wire from the definition's input node j is the instance's input of that name (by position when the instance lists as
// many inputs as the definition and none carries the name): its wire if it has one, else the literal its widget carries.
function subgraphInputStep(ctx, scope, j) {
  const def = scope.def;
  const inst = scope.parent.node;
  const instScope = scope.parent.scope;
  const sgIn = def.inputs?.[j];
  if (!sgIn) refuse(REFUSAL.BAD_TEMPLATE, `subgraph ${def.id} has no input ${j}`, { subgraph: def.id, slot: j });
  const instInputs = inst.inputs ?? [];
  let idx = instInputs.findIndex((i) => i && i.name === sgIn.name);
  if (idx < 0 && instInputs.length === (def.inputs ?? []).length) idx = j;
  if (idx >= 0 && instInputs[idx]?.link !== null && instInputs[idx]?.link !== undefined) return inputStep(ctx, instScope, inst, idx);
  if (scope.literals.has(j)) return { kind: "literal", value: clone(scope.literals.get(j)) };
  return nothing("unconnected");
}

// ---- widgets ---------------------------------------------------------------------------------------------------------

function entriesOf(ctx, cls) {
  if (!ctx.entryCache.has(cls)) ctx.entryCache.set(cls, specEntries(cls, ctx.oi[cls]));
  return ctx.entryCache.get(cls);
}

// mapWidgets: widgets_values -> { values: {inputName: value}, seedNames }. The values are consumed in the order the listing gives
// the inputs; a widget that is also wired still owns its slot. A control_after_generate slot follows a flagged INT, an upload
// slot follows an upload loader's widget, a dynamic combo's selected option contributes its own widgets right after it.
function mapWidgets(ctx, item, overrides) {
  const { key, node } = item;
  const cls = node.type;
  const raw = node.widgets_values;
  if (raw !== undefined && raw !== null && typeof raw !== "object") refuse(REFUSAL.BAD_TEMPLATE, `node ${key}: widgets_values is neither a list nor an object`, { node: key });
  const keyed = isObj(raw);
  const vals = Array.isArray(raw) ? raw : [];
  const values = {};
  const seedNames = new Set();
  const seenKeys = new Set();
  let idx = 0;
  const here = { node: key, class_type: cls };
  const warn = (code, input) => ctx.warnings.push({ code, node: key, input });

  const take = (full) => {
    if (keyed) { seenKeys.add(full); return hasOwn(raw, full) ? { has: true, value: raw[full] } : { has: false }; }
    return idx < vals.length ? { has: true, value: vals[idx++] } : { has: false };
  };
  const peek = () => (!keyed && idx < vals.length ? { has: true, value: vals[idx] } : { has: false });
  const override = (full, got) => {
    const p = overrides?.get(full);
    if (!p) return got;
    p.used = true;
    return { has: true, value: p.value };
  };
  // a widget value, or the listing's default when the template stored fewer values than there are widgets
  const put = (full, got, opts) => {
    if (got.has) { values[full] = clone(got.value); return true; }
    if (hasOwn(opts, "default")) { values[full] = clone(opts.default); warn("default_filled", full); return true; }
    warn("widget_value_missing", full);
    return false;
  };

  const walk = (entries, prefix) => {
    for (const [name, spec] of entries) {
      const full = prefix + name;
      const [type, opts] = checkSpec(cls, full, spec);
      if (type === DYNAMIC_COMBO) {
        const got = override(full, take(full));
        if (!put(full, got, {})) continue;
        const options = opts.options;
        if (!Array.isArray(options)) refuse(REFUSAL.BAD_OBJECT_INFO, `object_info: ${cls}.${full} has no options list`, { class_type: cls, input: full });
        const chosen = options.find((o) => isObj(o) && String(o.key) === String(got.value));
        if (!chosen) {
          refuse(REFUSAL.WIDGET_MISMATCH, `node ${key} (${cls}): ${JSON.stringify(got.value)} is not an option of ${full}`,
            { ...here, input: full, got: got.value, options: options.map((o) => o?.key) });
        }
        walk(nestedEntries(chosen.inputs), `${full}.`);
        continue;
      }
      if (type === AUTOGROW) {
        for (const [childName, childSpec] of nestedEntries(opts.template?.input)) {
          const [childType, childOpts] = checkSpec(cls, `${full}.${childName}`, childSpec);
          if (widgetKind(childType, childOpts) !== null) {
            refuse(REFUSAL.UNSUPPORTED_INPUT_TYPE, `${cls}.${full} is an autogrow group of widgets; its value layout is not mapped`, { ...here, input: full, type: AUTOGROW });
          }
        }
        continue; // children are wires, named by the node's own input slots
      }
      if (type === MATCHTYPE) continue;
      if (typeof type === "string" && /^COMFY_.+_V3$/.test(type)) {
        refuse(REFUSAL.UNSUPPORTED_INPUT_TYPE, `${cls}.${full}: the input type ${type} has no mapping rule`, { ...here, input: full, type });
      }
      const kind = widgetKind(type, opts);
      if (kind === null) {
        if (opts.socketless === true && opts.forceInput !== true) {
          refuse(REFUSAL.UNSUPPORTED_INPUT_TYPE, `${cls}.${full} is a custom frontend widget of type ${String(type)}; its value layout is not mapped`, { ...here, input: full, type: String(type) });
        }
        continue; // a socket: its wire comes from the node's input slots
      }
      if (opts.file_upload) {
        refuse(REFUSAL.UNSUPPORTED_INPUT_TYPE, `${cls}.${full} is a file-upload widget with extra frontend widgets; its value layout is not mapped`, { ...here, input: full, type: "file_upload" });
      }
      const got = override(full, take(full));
      put(full, got, opts);
      if (kind === "INT") {
        // the hidden control combo: the flag decides; a listing with no flag at all is read by the data (a seed-named INT followed
        // by one of the four control words owns it)
        const flag = opts.control_after_generate;
        let controlled = false;
        if (flag === undefined || flag === null) {
          const next = peek();
          if (SEED_NAME.test(lastSegment(name)) && next.has && CONTROL_WORDS.has(next.value)) { idx++; controlled = true; }
        } else if (flag) {
          controlled = true;
          const next = peek();
          if (next.has) {
            if (!CONTROL_WORDS.has(next.value)) {
              refuse(REFUSAL.WIDGET_MISMATCH, `node ${key} (${cls}): ${full} is flagged control_after_generate and the next value is ${JSON.stringify(next.value)}, not a control word`,
                { ...here, input: full, got: next.value });
            }
            idx++;
          }
        }
        if (controlled) {
          const p = overrides?.get("control_after_generate");
          if (p) p.used = true;
          if (SEED_NAME.test(lastSegment(name))) seedNames.add(full);
        }
      }
      // an upload loader's button owns slots after its COMBO widget; a custom widget that merely carries the flag (a painter's mask,
      // a STRING) does not
      if (!keyed && kind === "COMBO") for (const [flagName, slots] of UPLOAD_SLOTS) if (opts[flagName]) idx = Math.min(idx + slots, vals.length);
    }
  };
  walk(entriesOf(ctx, cls), "");

  if (keyed) {
    for (const k of Object.keys(raw)) if (!seenKeys.has(k)) warn("widget_key_ignored", k);
  } else if (idx < vals.length) {
    const extra = vals.length - idx;
    if (extra > (DISPLAY_VALUES.get(cls) ?? 0)) {
      refuse(REFUSAL.WIDGET_MISMATCH, `node ${key} (${cls}): ${vals.length} widgets_values for ${idx} widgets`, { ...here, expected: idx, got: vals.length });
    }
    ctx.warnings.push({ code: "display_values_ignored", node: key, count: extra });
  }
  return { values, seedNames };
}

// ---- one API node ----------------------------------------------------------------------------------------------------

function buildNode(ctx, item, options) {
  const { key, node, scope } = item;
  const { values, seedNames } = mapWidgets(ctx, item, ctx.proxies.get(key));
  const inputs = { ...values };
  const wired = new Set();
  const slots = node.inputs ?? []; // an array: makeScope refused any other shape
  slots.forEach((slot, i) => {
    if (!isObj(slot) || slot.link === null || slot.link === undefined) return;
    const name = slot.name;
    if (typeof name !== "string" || name === "") return;
    const src = resolveInput(ctx, scope, node, i, new Set());
    if (src.kind === "ref") { inputs[name] = [src.key, src.slot]; wired.add(name); return; }
    if (src.kind === "literal") { inputs[name] = src.value; return; }
    const reason = src.kind === "muted" ? "muted" : src.reason;
    // "unconnected": the wire ends at nothing (an instance input nobody wired), so nothing was lost and nothing is said; a widget
    // keeps its stored value and a socket stays empty (the preflight reports a missing required input)
    const lost = reason !== "unconnected";
    if (hasOwn(values, name)) { if (lost) ctx.warnings.push({ code: "widget_link_dropped", node: key, input: name, reason }); return; }
    delete inputs[name];
    if (lost) ctx.warnings.push({ code: "input_dropped", node: key, input: name, reason });
  });
  // pinSeeds: with a seed option, every input that is a seed (named seed or noise_seed, and made one by the listing's
  // control_after_generate) and holds a stored or primitive value is set to it; a wired seed, an INT that merely shares the
  // control (a PrimitiveInt) and a sampling seed with no control are left alone. A seed-named input left alone is not silent:
  // it is a seed_not_pinned warning with the reason ("wired", or "no_control" for a seed the listing gives no control)
  if (ctx.seed !== null) {
    const pinned = new Set();
    for (const name of seedNames) {
      if (hasOwn(inputs, name) && !wired.has(name)) {
        ctx.pinnedSeeds.push({ node: key, input: name, from: inputs[name], to: ctx.seed });
        inputs[name] = ctx.seed;
        pinned.add(name);
      }
    }
    for (const name of Object.keys(inputs)) {
      if (SEED_NAME.test(lastSegment(name)) && !pinned.has(name)) {
        ctx.warnings.push({ code: "seed_not_pinned", node: key, input: name, reason: wired.has(name) ? "wired" : "no_control" });
      }
    }
  }
  const out = { class_type: node.type, inputs };
  if (options.meta === true) {
    const title = typeof node.title === "string" && node.title !== "" ? node.title : ctx.oi[node.type]?.display_name ?? node.type;
    out._meta = { title };
  }
  return out;
}

// ---------------------------------------------------------------------------------------------------------------------
// The CLI
// ---------------------------------------------------------------------------------------------------------------------

const USAGE = "usage: node render/template-to-graph.mjs --template FILE --object-info FILE [--comfy-version X.Y.Z] [--seed N] " +
  "[--min-comfyui-version X.Y.Z | --index FILE [--name NAME]] [--meta]";

const cliRefusal = (code, message, details = {}) => ({ ok: false, refusal: { code, message, details } });

function parseArgs(argv) {
  const flags = {};
  const valued = new Set(["template", "object-info", "comfy-version", "min-comfyui-version", "index", "name", "seed"]);
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith("--")) return { error: `unexpected argument ${JSON.stringify(a)}` };
    const k = a.slice(2);
    if (k === "meta") { flags.meta = true; continue; }
    if (!valued.has(k)) return { error: `unknown flag ${a}` };
    if (i + 1 >= argv.length || argv[i + 1].startsWith("--")) return { error: `${a} needs a value` };
    flags[k] = argv[++i];
  }
  if (typeof flags.template !== "string") return { error: "give --template" };
  if (typeof flags["object-info"] !== "string") return { error: "give --object-info" };
  if (flags.seed !== undefined && !/^\d+$/.test(flags.seed)) return { error: "--seed must be a non-negative integer" };
  if (flags["min-comfyui-version"] !== undefined && flags.index !== undefined) return { error: "give --min-comfyui-version or --index, not both" };
  if (flags.name !== undefined && flags.index === undefined) return { error: "--name goes with --index" };
  return { flags };
}

function readJson(path, what) {
  try {
    return { value: JSON.parse(readFileSync(path, "utf8")) };
  } catch (e) {
    return { error: cliRefusal("unreadable_input", `${what} ${path}: ${e?.message ?? e}`, { file: path, what }) };
  }
}

// The templates package's index.json is a list of categories, each with a templates list; an entry has a name and, for most,
// minComfyUIVersion. A template the index does not list has no known minimum.
function indexMinimum(index, name) {
  const stack = [index];
  while (stack.length > 0) {
    const v = stack.pop();
    if (Array.isArray(v)) { for (let i = v.length - 1; i >= 0; i--) stack.push(v[i]); continue; }
    if (!isObj(v)) continue;
    if (v.name === name && typeof v.minComfyUIVersion === "string") return v.minComfyUIVersion;
    if (v.name === name) return null;
    for (const x of Object.values(v)) if (x !== null && typeof x === "object") stack.push(x);
  }
  return null;
}

export function main(argv) {
  const parsed = parseArgs(argv);
  if (parsed.error) return { code: 2, out: cliRefusal("usage", `${parsed.error}. ${USAGE}`) };
  const { flags } = parsed;
  const template = readJson(flags.template, "template");
  if (template.error) return { code: 2, out: template.error };
  const info = readJson(flags["object-info"], "object-info");
  if (info.error) return { code: 2, out: info.error };
  let min = flags["min-comfyui-version"] ?? null;
  if (flags.index !== undefined) {
    const index = readJson(flags.index, "index");
    if (index.error) return { code: 2, out: index.error };
    const base = flags.name ?? flags.template.split(/[\\/]/).pop().replace(/\.json$/, "").replace(/\.workflow$/, "");
    min = indexMinimum(index.value, base);
  }
  const options = { comfyVersion: flags["comfy-version"] ?? null, minComfyUIVersion: min, meta: flags.meta === true };
  if (flags.seed !== undefined) options.seed = Number(flags.seed);
  const result = convertTemplate(template.value, info.value, options);
  return { code: result.ok ? 0 : 1, out: result };
}

// Run as a program: `node render/template-to-graph.mjs ...`. Merely importing this file runs nothing.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  const { code, out } = main(process.argv.slice(2));
  process.stdout.write(`${JSON.stringify(out, null, 2)}\n`);
  process.exitCode = code;
}
