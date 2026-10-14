// render/builder-drift.mjs — the builder drift guard (ENH-4).
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
// What is stored is names and hashes only. No template JSON, no template text and no widget value is copied
// into the repository. `explain` prints the values to the terminal for a human; nothing writes them down.
//
// Where a builder deliberately differs from its template (or differs and nobody has decided yet) the spec
// declares it (`delta`), the manifest records the status (`deliberate` / `unreviewed`) with both digests, and a
// further change on EITHER side still fails. `same` items must have equal digests; the manifest check proves it
// without a wheel.
//
// Usage:
//   node render/builder-drift.mjs check   [--templates-dir DIR | --comfy-dir DIR] [--manifest FILE]
//   node render/builder-drift.mjs explain  --templates-dir DIR | --comfy-dir DIR     (values side by side)
//   node render/builder-drift.mjs record   --templates-dir DIR | --comfy-dir DIR     (rewrite the manifest)
// DIR is the `templates` directory of the wheel (or of a `pip download` extract); --comfy-dir finds it inside a
// ComfyUI tree. `COMFY_TEMPLATES_DIR` / `COMFYUI_DIR` supply them when the flags are absent. Read-only on the
// templates; `record` writes only the manifest.
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

// What feeds an input: a named subgraph input, or "<node type>#<slot>".
function feedOf(container, node, inputName) {
  const inp = (node.inputs ?? []).find((i) => i.name === inputName);
  if (!inp || inp.link == null) return undefined;
  const link = linkOf(container, inp.link);
  if (!link) return undefined;
  if (link.origin_id === -10) return `input:${container.inputs?.[link.origin_slot]?.name ?? link.origin_slot}`;
  const origin = (container.nodes ?? []).find((n) => n.id === link.origin_id);
  return `node:${origin?.type ?? link.origin_id}#${link.origin_slot}`;
}

// templateValue: the value one spec item reads out of a parsed template. Throws a message that names the
// builder when the template no longer has the node, the type or the widget the spec points at.
export function templateValue(spec, view, tpl, item) {
  const t = item.t;
  const tag = where(spec, view, item);
  const found = findNode(tpl, t.sg ?? null, t.id);
  if (!found) throw new Error(`${tag}: template ${view.template} has no node ${t.id}${t.sg ? ` in subgraph "${t.sg}"` : ""}`);
  if (!typeMatches(tpl, found.node, t.type)) {
    throw new Error(`${tag}: template ${view.template} node ${t.id} is ${found.node.type}, the spec expects ${t.type}`);
  }
  if (t.kind === "w") {
    const wv = found.node.widgets_values;
    if (!Array.isArray(wv) || wv[t.w] === undefined) throw new Error(`${tag}: template ${view.template} node ${t.id} (${t.type}) has no widget ${t.w}`);
    return wv[t.w];
  }
  if (t.kind === "inst") {
    const s = (tpl.definitions?.subgraphs ?? []).find((x) => x.id === found.node.type);
    const widgetInputs = (s?.inputs ?? []).filter((i) => WIDGET_INPUT_TYPES.has(i.type)).map((i) => i.name);
    const idx = widgetInputs.indexOf(t.input);
    const wv = found.node.widgets_values;
    if (idx < 0 || !Array.isArray(wv) || wv[idx] === undefined) {
      throw new Error(`${tag}: template ${view.template} instance ${t.id} has no promoted widget ${t.input}`);
    }
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

export function loadManifest(path = MANIFEST_PATH) {
  return JSON.parse(readFileSync(path, "utf8"));
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
      return JSON.parse(readFileSync(p, "utf8"));
    },
  };
}

function resolveTemplatesDir(flags, env) {
  if (typeof flags["templates-dir"] === "string") return flags["templates-dir"];
  const comfy = typeof flags["comfy-dir"] === "string" ? flags["comfy-dir"] : env.COMFYUI_DIR;
  if (comfy) { const found = locateTemplates(comfy, nodeIo); if (found) return found.templatesDir; }
  return env.COMFY_TEMPLATES_DIR || null;
}

function parseArgs(argv) {
  const flags = {};
  const rest = [];
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) { const k = argv[i].slice(2); flags[k] = argv[i + 1] && !argv[i + 1].startsWith("--") ? argv[++i] : true; }
    else rest.push(argv[i]);
  }
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
  const { flags, verb } = parseArgs(argv);
  const manifestPath = typeof flags.manifest === "string" ? flags.manifest : MANIFEST_PATH;
  const templatesDir = resolveTemplatesDir(flags, env);
  const source = templatesDir ? templateSource(templatesDir) : null;
  if (verb === "check") {
    const manifest = loadManifest(manifestPath);
    const failures = [...checkCoverage(), ...checkManifestShape(manifest), ...checkBuilderSide(manifest)];
    let notes = [];
    if (source) {
      const r = checkTemplateSide(manifest, source);
      failures.push(...r.failures);
      notes = r.notes;
    }
    for (const n of notes) out(`note: ${n}\n`);
    if (!source) out("note: template side skipped (no --templates-dir / --comfy-dir / COMFY_TEMPLATES_DIR)\n");
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
