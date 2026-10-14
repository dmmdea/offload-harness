// render/builder-drift.mjs — the builder drift guard.
//
// A `render/wf-*.mjs` builder that was copied from a ComfyUI workflow template (the
// `comfyui-workflow-templates-json` wheel) carries the template's recipe widgets (steps, cfg, sampler,
// scheduler, shift, LoRA strength, ...) as literals. When upstream changes one of those widgets, the
// builder keeps rendering the old recipe and nothing tells anyone (the class of upstream
// Comfy-Org/workflow_templates PR #1308: a template widget that was wrong, then fixed, under a builder that
// had copied it). This module closes that gap with one committed manifest, `testdata/builder-drift.json`:
//
//   per builder, per view (a preset or a template variant):
//     { template, json_version, meta_version,
//       templateSha256   sha256 of the recipe widgets as the TEMPLATE carries them,
//       builderSha256    sha256 of the same widgets as the BUILDER's output graph carries them,
//       items: { <widget key>: { template, builder, status } } }   (per-widget digests, never values)
//
// Two checks read it:
//   * builder side (offline, always): rebuild the graph with the spec's fixed arguments, re-extract the recipe
//     widgets, and compare with the manifest. A moved widget fails NAMING THE BUILDER and the widget.
//   * template side (needs the installed wheel or a candidate extract): re-extract the same widgets from the
//     template JSON and compare. A template that changed under a builder fails NAMING THE BUILDER, the
//     template and the wheel version the builder was last verified against.
//
// Every template value is read from the place it runs from. A widget whose input is linked (to a subgraph input, or to
// another node) carries a stale serialized value, so a locator that points at one fails (see `templateValue`) and the
// spec points at the effective source instead.
//
// What is stored is names and hashes only. No template JSON, no template text and no widget value is copied
// into the repository. `explain` prints the values to the terminal for a human; nothing writes them down.
//
// Where a builder deliberately differs from its template (or differs and nobody has decided yet) the spec
// declares it (`delta`), the manifest records the status (`deliberate` / `unreviewed`) with both digests, and a
// further change on EITHER side still fails. `same` items must have equal digests; the manifest check proves it
// without a wheel.
//
// Usage:
//   node render/builder-drift.mjs check   [--templates-dir DIR | --comfy-dir DIR] [--manifest FILE] [--require-templates] [--strict]
//   node render/builder-drift.mjs explain  --templates-dir DIR | --comfy-dir DIR     (values side by side)
//   node render/builder-drift.mjs record   --templates-dir DIR | --comfy-dir DIR     (rewrite the manifest)
// A flag that takes a value and has none (an empty or unset shell variable) is an error (exit 2), as is an argument that
// starts with `-` and is not a known flag (`-templates-dir`, `-x`) and a second positional after the verb: a typo must not
// silently skip a side. `--require-templates` turns "no templates directory" into an
// error (exit 2) instead of a skipped template side; use it in an upgrade runbook. `--strict` also fails (exit 1) on every
// `unreviewed` manifest entry; without it those entries stay green until a person judges them.
// DIR is the `templates` directory of the wheel (or of a `pip download` extract); --comfy-dir finds it inside a
// ComfyUI tree. The first source that is set decides, in this order: --templates-dir, --comfy-dir, the
// `COMFY_TEMPLATES_DIR` environment variable (a templates directory), the `COMFYUI_DIR` environment variable (a ComfyUI
// tree). The tests use the same function (`resolveTemplatesDir`). A tree named on the command line that has no package
// is an error; one named by the environment that has none counts as "no package here" and the template side is
// skipped. Read-only on the templates; `record` writes only the manifest.
import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import { writeFileAtomic } from "./atomic-out.mjs";
import { joinPath, locateTemplates, nodeIo, readPackageVersions } from "./templates-catalog.mjs";
import { BUILDER_SPECS, NOT_TEMPLATE_DERIVED } from "./builder-drift-specs.mjs";

export const MANIFEST_SCHEMA = 1;
export const MANIFEST_PATH = fileURLToPath(new URL("./testdata/builder-drift.json", import.meta.url));
const RENDER_DIR = fileURLToPath(new URL("./", import.meta.url));

const JSON_PKG = "comfyui-workflow-templates-json";
const META_PKG = "comfyui-workflow-templates";
// A subgraph input of one of these types is a widget on the instance node; every other type is a link.
const WIDGET_INPUT_TYPES = new Set(["STRING", "BOOLEAN", "INT", "FLOAT", "COMBO"]);

// ---------------------------------------------------------------------------------------------
// Canonical values and hashes
// ---------------------------------------------------------------------------------------------

// Numbers are compared at 12 significant digits: the templates serialize 3.1 as 3.1000000000000005.
export function normalize(v) {
  if (typeof v === "number") return Number.isFinite(v) ? Number(v.toPrecision(12)) : String(v);
  if (Array.isArray(v)) return v.map(normalize);
  return v;
}
export const canonical = (v) => JSON.stringify(normalize(v));
const sha256 = (s) => createHash("sha256").update(s).digest("hex");
export const itemDigest = (v) => sha256(canonical(v)).slice(0, 16);

// recipeSha256: the hash over a set of (key, value) pairs, ordered by key so extraction order cannot flip it.
export function recipeSha256(values) {
  const rows = [...values.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0)).map(([k, v]) => [k, normalize(v)]);
  return sha256(JSON.stringify(rows));
}

// ---------------------------------------------------------------------------------------------
// Extraction: the builder's output graph
// ---------------------------------------------------------------------------------------------

export function buildView(spec, view) {
  const graph = spec.build({ ...view.args });
  if (!graph || typeof graph !== "object") throw new Error(`${spec.builder} [${view.name}]: the builder returned no graph`);
  return graph;
}

function builderValue(spec, view, graph, item) {
  const { node, cls, input } = item.b;
  const n = graph[node];
  if (!n) throw new Error(`${spec.builder} [${view.name}] ${item.key}: the builder's graph has no node ${node}`);
  if (n.class_type !== cls) {
    throw new Error(`${spec.builder} [${view.name}] ${item.key}: node ${node} is ${n.class_type}, the spec expects ${cls}`);
  }
  const v = n.inputs?.[input];
  if (v === undefined) throw new Error(`${spec.builder} [${view.name}] ${item.key}: node ${node} (${cls}) has no input ${input}`);
  return v;
}

// builderValues: key -> value for every spec item that has a builder side.
export function builderValues(spec, view) {
  const graph = buildView(spec, view);
  const out = new Map();
  for (const item of view.items) if (item.b) out.set(item.key, builderValue(spec, view, graph, item));
  return out;
}

// ---------------------------------------------------------------------------------------------
// Extraction: the template JSON
// ---------------------------------------------------------------------------------------------

const where = (spec, view, item) => `${spec.builder} [${view.name}] ${item.key}`;

function findNode(tpl, sg, id) {
  if (sg == null) {
    const node = (tpl.nodes ?? []).find((n) => n.id === id);
    return node ? { node, container: { nodes: tpl.nodes, links: tpl.links, inputs: [] }, root: true } : null;
  }
  for (const s of (tpl.definitions?.subgraphs ?? []).filter((x) => x.name === sg)) {
    const node = (s.nodes ?? []).find((n) => n.id === id);
    if (node) return { node, container: s, root: false };
  }
  return null;
}

// A node's type is its class name, or (for an instance of a subgraph) the subgraph's id; "subgraph:NAME"
// asks for the second and checks the name.
function typeMatches(tpl, node, type) {
  if (type == null) return true;
  if (type.startsWith("subgraph:")) {
    const s = (tpl.definitions?.subgraphs ?? []).find((x) => x.id === node.type);
    return s?.name === type.slice("subgraph:".length);
  }
  return node.type === type;
}

function linkOf(container, linkId) {
  for (const l of container.links ?? []) {
    if (Array.isArray(l)) { if (l[0] === linkId) return { origin_id: l[1], origin_slot: l[2] }; }
    else if (l.id === linkId) return l;
  }
  return null;
}

// What feeds an input: a named subgraph input, or one output slot of one node. `key` is what two sources are compared by,
// the node's ID and the slot (two separate nodes of one type are two sources, and one node feeding both inputs is one);
// `label` is for a message and adds the node's type.
export function feedSource(container, node, inputName) {
  const inp = (node.inputs ?? []).find((i) => i.name === inputName);
  if (!inp || inp.link == null) return undefined;
  const link = linkOf(container, inp.link);
  if (!link) return undefined;
  if (link.origin_id === -10) {
    const key = `input:${container.inputs?.[link.origin_slot]?.name ?? link.origin_slot}`;
    return { key, label: key };
  }
  const origin = (container.nodes ?? []).find((n) => n.id === link.origin_id);
  return {
    key: `node:${link.origin_id}#${link.origin_slot}`,
    label: origin ? `node:${origin.type}/${link.origin_id}#${link.origin_slot}` : `node:${link.origin_id}#${link.origin_slot}`,
  };
}
const feedOf = (container, node, inputName) => feedSource(container, node, inputName)?.key;
const feedLabel = (container, node, inputName) => feedSource(container, node, inputName)?.label;

// The input of `node` that carries widget `name`, when that input is linked: the widget's serialized value is then
// stale (the value that runs comes through the link). An unlinked widget may have no input entry at all.
function linkedInput(node, name) {
  return (node.inputs ?? []).find((i) => (i.widget?.name ?? i.name) === name && i.link != null) ?? null;
}

const widgetInputNames = (sg) => (sg?.inputs ?? []).filter((i) => WIDGET_INPUT_TYPES.has(i.type)).map((i) => i.name);
const subgraphOfInstance = (tpl, instance) => (tpl.definitions?.subgraphs ?? []).find((x) => x.id === instance.type);

// The position of [node id, widget name] in an instance's `properties.proxyWidgets`, or -1. In the legacy form the
// instance's `widgets_values` follow this list, not the order of the subgraph's inputs (the frontend's own migration
// of a legacy proxy reads the host value at the entry's position in the list).
const proxyPosition = (inst, t) => (inst.properties?.proxyWidgets ?? []).findIndex((p) => String(p[0]) === String(t.id) && p[1] === t.name);

// A legacy-form widget: the interior widget is linked to a subgraph input, the root instance lists it in
// `properties.proxyWidgets`, carries no value at that position and has nothing wired to its input for it, so the
// interior widget is the only place the value lives. Anything else is refused: an instance that carries the value is
// read with I, an input wired in the root graph by the source that feeds it, an unlinked widget with W.
function proxiedWidget(tpl, found, t, tag, view) {
  const wv = found.node.widgets_values;
  if (!Array.isArray(wv) || wv[t.w] === undefined) throw new Error(`${tag}: template ${view.template} node ${t.id} (${t.type}) has no widget ${t.w}`);
  const inp = linkedInput(found.node, t.name);
  const link = inp ? linkOf(found.container, inp.link) : null;
  if (!link || link.origin_id !== -10) throw new Error(`${tag}: template ${view.template} node ${t.id} (${t.type}) widget ${t.name} is not linked to a subgraph input; read it with W`);
  const sgInput = found.container.inputs?.[link.origin_slot]?.name;
  const instances = (tpl.nodes ?? []).filter((n) => n.type === found.container.id);
  const listing = instances.filter((n) => proxyPosition(n, t) >= 0);
  if (!listing.length) throw new Error(`${tag}: template ${view.template} the root instance of subgraph "${found.container.name}" does not list [${t.id}, ${t.name}] as a proxied widget; the widget is promoted some other way, find where it runs from`);
  for (const inst of listing) {
    const pos = proxyPosition(inst, t);
    if (Array.isArray(inst.widgets_values) && inst.widgets_values[pos] !== undefined) {
      throw new Error(`${tag}: template ${view.template} instance ${inst.id} carries a promoted value for ${sgInput}; read it with I (its widgets_values follow proxyWidgets, position ${pos})`);
    }
    const wired = linkedInput(inst, sgInput);
    if (wired) {
      const root = { nodes: tpl.nodes, links: tpl.links, inputs: [] };
      throw new Error(`${tag}: template ${view.template} instance ${inst.id} has its ${sgInput} input linked at the root (to ${feedLabel(root, inst, wired.name) ?? "another node"}), so the interior value is not the one that runs; point the spec at the source that feeds it`);
    }
  }
  return wv[t.w];
}

// templateValue: the value one spec item reads out of a parsed template. Throws a message that names the
// builder when the template no longer has the node, the type or the widget the spec points at, and when the
// widget the spec points at is linked (its serialized value is stale; the effective one lives at the link's source).
export function templateValue(spec, view, tpl, item) {
  const t = item.t;
  const tag = where(spec, view, item);
  const found = findNode(tpl, t.sg ?? null, t.id);
  if (!found) throw new Error(`${tag}: template ${view.template} has no node ${t.id}${t.sg ? ` in subgraph "${t.sg}"` : ""}`);
  if (!typeMatches(tpl, found.node, t.type)) {
    throw new Error(`${tag}: template ${view.template} node ${t.id} is ${found.node.type}, the spec expects ${t.type}`);
  }
  const refuseLinked = (name, what) => {
    const inp = linkedInput(found.node, name);
    if (!inp) return;
    throw new Error(`${tag}: template ${view.template} ${what} ${name} is linked (to ${feedLabel(found.container, found.node, inp.name) ?? "another node"}), so its serialized value is not the one that runs; point the spec at the effective source (I for a subgraph input the root instance promotes, W on the node that feeds it, P for a legacy proxied widget)`);
  };
  if (t.kind === "w") {
    if (typeof t.name !== "string" || !t.name) throw new Error(`${tag}: the spec's widget locator names no widget (W and P take the widget's input name)`);
    if (t.proxied) return proxiedWidget(tpl, found, t, tag, view);
    const wv = found.node.widgets_values;
    if (!Array.isArray(wv) || wv[t.w] === undefined) throw new Error(`${tag}: template ${view.template} node ${t.id} (${t.type}) has no widget ${t.w}`);
    refuseLinked(t.name, `node ${t.id} (${t.type}) widget`);
    return wv[t.w];
  }
  if (t.kind === "inst") {
    const s = subgraphOfInstance(tpl, found.node);
    const idx = widgetInputNames(s).indexOf(t.input);
    const wv = found.node.widgets_values;
    if (idx < 0 || !Array.isArray(wv) || wv[idx] === undefined) {
      throw new Error(`${tag}: template ${view.template} instance ${t.id} has no promoted widget ${t.input}`);
    }
    refuseLinked(t.input, `instance ${t.id} widget`);
    return wv[idx];
  }
  if (t.kind === "distinct") {
    const [a, b] = t.inputs.map((name) => feedOf(found.container, found.node, name));
    if (a === undefined || b === undefined) throw new Error(`${tag}: template ${view.template} node ${t.id} has an unlinked input in [${t.inputs.join(", ")}]`);
    return a !== b;
  }
  throw new Error(`${tag}: unknown locator kind ${t.kind}`);
}

export function templateValues(spec, view, tpl) {
  const out = new Map();
  for (const item of view.items) out.set(item.key, templateValue(spec, view, tpl, item));
  return out;
}

// ---------------------------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------------------------

// formatManifest: indented JSON with each widget's digests on one line, so the file reads as a table.
export function formatManifest(manifest) {
  const row = /\{\n\s+"template": ("[0-9a-f]+"),\n\s+"builder": ("[0-9a-f]+"|null),\n\s+"status": ("[a-z-]+")\n\s+\}/g;
  return JSON.stringify(manifest, null, 2).replace(row, '{ "template": $1, "builder": $2, "status": $3 }') + "\n";
}

// parseJsonText: JSON.parse that strips a byte-order mark (an editor on Windows writes one) and names what it was reading.
function parseJsonText(text, what) {
  try { return JSON.parse(text.charCodeAt(0) === 0xfeff ? text.slice(1) : text); }
  catch (e) { throw new Error(`${what} is not valid JSON (${e.message})`); }
}

export function loadManifest(path = MANIFEST_PATH) {
  let text;
  try { text = readFileSync(path, "utf8"); } catch (e) { throw new Error(`manifest ${path} cannot be read: ${e.message}`); }
  const m = parseJsonText(text, `manifest ${path}`);
  if (!m || typeof m !== "object" || Array.isArray(m) || !m.builders || typeof m.builders !== "object") {
    throw new Error(`manifest ${path} is not a drift manifest (no builders object)`);
  }
  return m;
}

function statusOf(spec, view, item, tVal, bVal) {
  if (!item.b) return "template-only";
  const same = itemDigest(tVal) === itemDigest(bVal);
  if (same) {
    if (item.delta) throw new Error(`${where(spec, view, item)}: declared as a ${item.delta.kind} delta but builder and template now agree; remove the delta`);
    return "same";
  }
  if (!item.delta) {
    throw new Error(`${where(spec, view, item)}: the builder and the template differ and the spec declares no delta; either fix the builder or add { kind: "deliberate"|"unreviewed", reason } to the item`);
  }
  return item.delta.kind;
}

// buildManifest: evaluates every spec against the templates. `loadTemplate(name)` returns the parsed JSON.
export function buildManifest({ loadTemplate, versions, specs = BUILDER_SPECS }) {
  const builders = {};
  for (const spec of specs) {
    const views = {};
    for (const view of spec.views) {
      const tpl = loadTemplate(view.template);
      const tVals = templateValues(spec, view, tpl);
      const bVals = builderValues(spec, view);
      const items = {};
      for (const item of view.items) {
        const tv = tVals.get(item.key);
        const bv = item.b ? bVals.get(item.key) : undefined;
        items[item.key] = { template: itemDigest(tv), builder: item.b ? itemDigest(bv) : null, status: statusOf(spec, view, item, tv, bv) };
      }
      views[view.name] = {
        template: view.template,
        json_version: versions[JSON_PKG] ?? null,
        meta_version: versions[META_PKG] ?? null,
        templateSha256: recipeSha256(tVals),
        builderSha256: recipeSha256(bVals),
        items,
      };
    }
    builders[spec.builder] = { module: `render/${spec.builder}.mjs`, views };
  }
  return { schema: MANIFEST_SCHEMA, builders };
}

// ---------------------------------------------------------------------------------------------
// Checks. Each returns an array of failure objects { builder, view, message }; the message always
// starts with "builder drift: <builder>" so a red test names the builder.
// ---------------------------------------------------------------------------------------------

// The extractors tag their errors "<builder> [<view>] <key>: ..." so a throw out of buildManifest names the builder;
// inside a check the "builder drift: <builder> [<view>]:" prefix already says it.
const untag = (message, spec, view) => message.replace(`${spec.builder} [${view.name}] `, "");
const fail = (builder, view, message) => ({ builder, view, message: `builder drift: ${builder}${view ? ` [${view}]` : ""}: ${message}` });

// checkCoverage: every `render/wf-*.mjs` is either guarded or classified as not template-derived, and every
// guarded or classified name still exists. A new builder cannot skip the question.
export function checkCoverage({ renderDir = RENDER_DIR, specs = BUILDER_SPECS, notDerived = NOT_TEMPLATE_DERIVED } = {}) {
  const failures = [];
  const files = readdirSync(renderDir).filter((f) => /^wf-.+\.mjs$/.test(f) && !f.endsWith(".test.mjs")).map((f) => f.replace(/\.mjs$/, ""));
  const guarded = new Set(specs.map((s) => s.builder));
  for (const f of files) {
    if (guarded.has(f) && Object.hasOwn(notDerived, f)) failures.push(fail(f, null, "is both guarded and listed as not template-derived; pick one"));
    else if (!guarded.has(f) && !Object.hasOwn(notDerived, f)) {
      failures.push(fail(f, null, "is neither guarded (BUILDER_SPECS) nor classified (NOT_TEMPLATE_DERIVED) in builder-drift-specs.mjs; say which workflow template it was copied from, or why it was not"));
    }
  }
  for (const name of [...guarded, ...Object.keys(notDerived)]) {
    if (!files.includes(name)) failures.push(fail(name, null, "is named in builder-drift-specs.mjs but render/ has no such builder"));
  }
  for (const [name, reason] of Object.entries(notDerived)) {
    if (typeof reason !== "string" || reason.trim().length < 20) failures.push(fail(name, null, "NOT_TEMPLATE_DERIVED needs a reason of at least a sentence"));
  }
  return failures;
}

// checkManifestShape: the manifest and the specs describe the same builders, views and widgets, and each
// status is consistent with its digests (no wheel needed).
export function checkManifestShape(manifest, { specs = BUILDER_SPECS } = {}) {
  const failures = [];
  if (manifest.schema !== MANIFEST_SCHEMA) failures.push(fail("manifest", null, `schema ${manifest.schema}, this module reads ${MANIFEST_SCHEMA}`));
  const specNames = new Set(specs.map((s) => s.builder));
  for (const name of Object.keys(manifest.builders ?? {})) {
    if (!specNames.has(name)) failures.push(fail(name, null, "is in the manifest but has no spec in builder-drift-specs.mjs"));
  }
  for (const spec of specs) {
    const entry = manifest.builders?.[spec.builder];
    if (!entry) { failures.push(fail(spec.builder, null, "has a spec but no manifest entry; run `node render/builder-drift.mjs record`")); continue; }
    const viewNames = new Set(spec.views.map((v) => v.name));
    for (const name of Object.keys(entry.views ?? {})) if (!viewNames.has(name)) failures.push(fail(spec.builder, name, "is in the manifest but not in the spec"));
    for (const view of spec.views) {
      const rec = entry.views?.[view.name];
      if (!rec) { failures.push(fail(spec.builder, view.name, "has a spec view but no manifest entry")); continue; }
      if (rec.template !== view.template) failures.push(fail(spec.builder, view.name, `manifest names template ${rec.template}, the spec names ${view.template}`));
      if (!rec.json_version) failures.push(fail(spec.builder, view.name, "records no workflow-templates package version"));
      const keys = new Set(view.items.map((i) => i.key));
      for (const k of Object.keys(rec.items ?? {})) if (!keys.has(k)) failures.push(fail(spec.builder, view.name, `widget ${k} is in the manifest but not in the spec`));
      for (const item of view.items) {
        const it = rec.items?.[item.key];
        if (!it) { failures.push(fail(spec.builder, view.name, `widget ${item.key} has no manifest entry`)); continue; }
        const equal = it.builder !== null && it.builder === it.template;
        const want = !item.b ? "template-only" : item.delta ? item.delta.kind : "same";
        if (it.status !== want) failures.push(fail(spec.builder, view.name, `widget ${item.key} is ${it.status} in the manifest, the spec says ${want}`));
        if (it.status === "same" && !equal) failures.push(fail(spec.builder, view.name, `widget ${item.key} is marked same but the digests differ`));
        if ((it.status === "deliberate" || it.status === "unreviewed") && equal) failures.push(fail(spec.builder, view.name, `widget ${item.key} is marked ${it.status} but the digests agree`));
        if (it.status === "template-only" && it.builder !== null) failures.push(fail(spec.builder, view.name, `widget ${item.key} is template-only but carries a builder digest`));
      }
    }
  }
  return failures;
}

// checkBuilderSide: rebuild every view's graph and compare each recipe widget with the manifest.
export function checkBuilderSide(manifest, { specs = BUILDER_SPECS } = {}) {
  const failures = [];
  for (const spec of specs) {
    for (const view of spec.views) {
      const rec = manifest.builders?.[spec.builder]?.views?.[view.name];
      if (!rec) continue; // checkManifestShape reports it
      let vals;
      try { vals = builderValues(spec, view); } catch (e) { failures.push(fail(spec.builder, view.name, untag(e.message, spec, view))); continue; }
      const moved = [];
      for (const item of view.items) {
        if (!item.b) continue;
        const now = itemDigest(vals.get(item.key));
        const was = rec.items?.[item.key]?.builder;
        if (was !== now) moved.push(`${item.key} (manifest ${was}, builder now ${now})`);
      }
      if (moved.length) failures.push(fail(spec.builder, view.name, `recipe widget changed in the builder's output graph: ${moved.join("; ")}. If the change is deliberate, re-verify it against template ${view.template} and run \`node render/builder-drift.mjs record\`.`));
      else if (recipeSha256(vals) !== rec.builderSha256) failures.push(fail(spec.builder, view.name, "builderSha256 does not match the recomputed hash (manifest edited by hand?)"));
    }
  }
  return failures;
}

// checkUnreviewed (`check --strict`): every manifest entry whose status is `unreviewed`, a builder/template difference the
// first run found that nobody has judged. A plain check leaves them green; the strict one fails until a person turns each
// into a fix or a `deliberate` delta.
export function checkUnreviewed(manifest, { specs = BUILDER_SPECS } = {}) {
  const failures = [];
  for (const [builder, entry] of Object.entries(manifest.builders ?? {})) {
    for (const [view, rec] of Object.entries(entry.views ?? {})) {
      for (const [key, it] of Object.entries(rec.items ?? {})) {
        if (it.status !== "unreviewed") continue;
        const reason = specs.find((s) => s.builder === builder)?.views.find((v) => v.name === view)?.items.find((i) => i.key === key)?.delta?.reason;
        failures.push(fail(builder, view, `widget ${key} is unreviewed: the builder and the template differ and nobody has judged it; fix the builder or declare the difference deliberate with a reason${reason ? ` (on record: ${reason})` : ""}`));
      }
    }
  }
  return failures;
}

// checkTemplateSide: re-read every template and compare. Returns { failures, notes }; a note says a wheel
// version moved while every guarded widget stayed put.
export function checkTemplateSide(manifest, { loadTemplate, versions, specs = BUILDER_SPECS }) {
  const failures = [];
  const notes = [];
  for (const spec of specs) {
    for (const view of spec.views) {
      const rec = manifest.builders?.[spec.builder]?.views?.[view.name];
      if (!rec) continue;
      let vals;
      try { vals = templateValues(spec, view, loadTemplate(view.template)); } catch (e) {
        failures.push(fail(spec.builder, view.name, `${untag(e.message, spec, view)} (verified against ${JSON_PKG} ${rec.json_version}, installed ${versions[JSON_PKG] ?? "unknown"})`));
        continue;
      }
      const moved = [];
      for (const item of view.items) {
        const now = itemDigest(vals.get(item.key));
        const was = rec.items?.[item.key]?.template;
        if (was !== now) moved.push(item.key);
      }
      if (moved.length) {
        failures.push(fail(spec.builder, view.name, `template ${view.template} changed under the builder: widget${moved.length > 1 ? "s" : ""} ${moved.join(", ")} (builder last verified against ${JSON_PKG} ${rec.json_version}, installed ${versions[JSON_PKG] ?? "unknown"}). Compare the builder with the template, then \`node render/builder-drift.mjs record\`.`));
        continue;
      }
      if (recipeSha256(vals) !== rec.templateSha256) {
        failures.push(fail(spec.builder, view.name, "templateSha256 does not match the recomputed hash (manifest edited by hand?)"));
      } else if (versions[JSON_PKG] && versions[JSON_PKG] !== rec.json_version) {
        notes.push(`${spec.builder} [${view.name}]: verified against ${JSON_PKG} ${rec.json_version}, installed ${versions[JSON_PKG]}; every guarded widget of ${view.template} is unchanged`);
      }
    }
  }
  return { failures, notes };
}

// ---------------------------------------------------------------------------------------------
// Template source (a directory) and the CLI
// ---------------------------------------------------------------------------------------------

export function templateSource(templatesDir) {
  const dir = String(templatesDir).replace(/\\/g, "/").replace(/\/$/, "");
  const sitePackagesDir = joinPath(dir, "../..");
  return {
    dir,
    versions: readPackageVersions(sitePackagesDir),
    loadTemplate: (name) => {
      const p = `${dir}/${name}.json`;
      if (!existsSync(p)) throw new Error(`template ${name} is not in ${JSON_PKG} (no ${name}.json in the templates directory)`);
      const tpl = parseJsonText(readFileSync(p, "utf8"), `template ${name}`);
      if (!tpl || !Array.isArray(tpl.nodes)) throw new Error(`template ${name} has no nodes array (it is not a workflow file)`);
      return tpl;
    },
  };
}

// resolveTemplatesDir: the templates directory the checks read, or null when none is named. The first source that is
// set decides: --templates-dir, --comfy-dir, COMFY_TEMPLATES_DIR, COMFYUI_DIR. A ComfyUI tree named by a flag that has
// no package throws (it is a request); one named by the environment returns null (the package is not installed here).
export function resolveTemplatesDir(flags, env) {
  if (typeof flags["templates-dir"] === "string") return flags["templates-dir"];
  if (typeof flags["comfy-dir"] === "string") {
    const found = locateTemplates(flags["comfy-dir"], nodeIo);
    if (!found) throw new Error(`--comfy-dir ${flags["comfy-dir"]} has no ${JSON_PKG} package (looked in .venv and venv under it)`);
    return found.templatesDir;
  }
  if (env.COMFY_TEMPLATES_DIR) return env.COMFY_TEMPLATES_DIR;
  if (env.COMFYUI_DIR) return locateTemplates(env.COMFYUI_DIR, nodeIo)?.templatesDir ?? null;
  return null;
}

// A flag that takes a value must get a non-empty one, and an argument that starts with `-` and is not a known flag is
// refused, in the single-dash form too (`-templates-dir`, `-x`), as is a second positional after the verb: an unset shell
// variable (`--templates-dir "$UNSET"`) or a typo must fail, never skip the side it was meant to name. A value never starts
// with `-`, so `--manifest -x` has no value.
const VALUE_FLAGS = new Set(["templates-dir", "comfy-dir", "manifest"]);
const SWITCH_FLAGS = new Set(["require-templates", "strict"]);
export function parseArgs(argv) {
  const flags = {};
  const rest = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith("-")) { rest.push(a); continue; }
    const eq = a.indexOf("=");
    const dashes = a.startsWith("--") ? 2 : 1;
    const name = eq < 0 ? a.slice(dashes) : a.slice(dashes, eq);
    if (dashes === 1) throw new Error(`unknown flag ${eq < 0 ? a : a.slice(0, eq)} (flags start with two dashes; known: ${[...VALUE_FLAGS, ...SWITCH_FLAGS].map((f) => `--${f}`).join(", ")})`);
    if (SWITCH_FLAGS.has(name)) {
      if (eq >= 0) throw new Error(`--${name} takes no value`);
      flags[name] = true;
      continue;
    }
    if (!VALUE_FLAGS.has(name)) throw new Error(`unknown flag --${name} (known: ${[...VALUE_FLAGS, ...SWITCH_FLAGS].map((f) => `--${f}`).join(", ")})`);
    const value = eq >= 0 ? a.slice(eq + 1) : i + 1 < argv.length && !argv[i + 1].startsWith("-") ? argv[++i] : undefined;
    if (typeof value !== "string" || !value.trim()) throw new Error(`--${name} needs a non-empty value`);
    flags[name] = value;
  }
  if (rest.length > 1) throw new Error(`unexpected argument ${rest[1]} after ${rest[0]} (one verb: check | explain | record)`);
  return { flags, verb: rest[0] ?? "check" };
}

const show = (v) => { const s = JSON.stringify(v); return s.length > 60 ? `${s.slice(0, 57)}...` : s; };

export function explain({ source, specs = BUILDER_SPECS }) {
  const lines = [];
  for (const spec of specs) {
    for (const view of spec.views) {
      lines.push(`${spec.builder} [${view.name}]  template ${view.template}`);
      const tpl = source.loadTemplate(view.template);
      const bVals = builderValues(spec, view);
      for (const item of view.items) {
        const tv = templateValue(spec, view, tpl, item);
        const same = item.b ? itemDigest(tv) === itemDigest(bVals.get(item.key)) : null;
        const tag = !item.b ? "template-only" : same ? "same" : `DIFFERS${item.delta ? ` (${item.delta.kind})` : " (undeclared)"}`;
        lines.push(`  ${item.key.padEnd(34)} template ${show(tv).padEnd(40)} builder ${item.b ? show(bVals.get(item.key)).padEnd(40) : "-".padEnd(40)} ${tag}`);
      }
    }
  }
  return lines.join("\n") + "\n";
}

export async function main(argv, { out = (s) => process.stdout.write(s), err = (s) => process.stderr.write(s), env = process.env } = {}) {
  let parsed;
  try { parsed = parseArgs(argv); } catch (e) { err(`${e.message}\n`); return 2; }
  const { flags, verb } = parsed;
  for (const flag of ["strict", "require-templates"]) {
    if (flags[flag] && verb !== "check") { err(`--${flag} belongs to check, not ${verb}
`); return 2; }
  }
  const manifestPath = typeof flags.manifest === "string" ? flags.manifest : MANIFEST_PATH;
  let templatesDir;
  try { templatesDir = resolveTemplatesDir(flags, env); } catch (e) { err(`${e.message}\n`); return 2; }
  const source = templatesDir ? templateSource(templatesDir) : null;
  if (verb === "check") {
    if (flags["require-templates"] && !source) {
      err("--require-templates: no templates directory (give --templates-dir DIR or --comfy-dir DIR, or set COMFY_TEMPLATES_DIR / COMFYUI_DIR to a directory that has the package)\n");
      return 2;
    }
    let manifest;
    try { manifest = loadManifest(manifestPath); } catch (e) { err(`${e.message}\n`); return 2; }
    const failures = [...checkCoverage(), ...checkManifestShape(manifest), ...checkBuilderSide(manifest)];
    if (flags.strict) failures.push(...checkUnreviewed(manifest));
    let notes = [];
    if (source) {
      const r = checkTemplateSide(manifest, source);
      failures.push(...r.failures);
      notes = r.notes;
    }
    for (const n of notes) out(`note: ${n}\n`);
    if (!source) out("note: template side skipped (no --templates-dir / --comfy-dir / COMFY_TEMPLATES_DIR / COMFYUI_DIR)\n");
    for (const f of failures) err(`${f.message}\n`);
    const n = BUILDER_SPECS.reduce((a, s) => a + s.views.length, 0);
    out(failures.length ? `${failures.length} failure(s)\n` : `ok: ${BUILDER_SPECS.length} builders, ${n} views${source ? ", template side checked against " + (source.versions[JSON_PKG] ?? "unknown version") : ""}\n`);
    return failures.length ? 1 : 0;
  }
  if (verb === "explain" || verb === "record") {
    if (!source) { err("give --templates-dir DIR or --comfy-dir DIR (or set COMFY_TEMPLATES_DIR / COMFYUI_DIR)\n"); return 2; }
    if (verb === "explain") { out(explain({ source })); return 0; }
    const manifest = buildManifest({ loadTemplate: source.loadTemplate, versions: source.versions });
    writeFileAtomic(manifestPath, formatManifest(manifest));
    out(`wrote ${manifestPath} (${JSON_PKG} ${source.versions[JSON_PKG] ?? "unknown"})\n`);
    return 0;
  }
  err(`unknown verb ${verb} (check | explain | record)\n`);
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; }, (e) => { process.stderr.write(`${e.message}\n`); process.exitCode = 2; });
}
