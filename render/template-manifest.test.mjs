// render/template-manifest.test.mjs - the manifest generator: a catalog row in, a manifest entry, a verdict and a readiness out.
//
// The fixtures are in template-manifest-fixture.mjs (an in-memory ComfyUI tree; every repository is `example-org/...`). Nothing here
// touches a real install, the network or a GPU.
import { test } from "node:test";
import assert from "node:assert/strict";
import * as C from "./templates-catalog.mjs";
import * as M from "./template-manifest.mjs";
import { parseManifest } from "./manifest.mjs";
import { COMFY, TEMPLATES, LICENSE_MAP, SHA_DECLARED, fixtureIo, fixtureFiles, memIo } from "./template-manifest-fixture.mjs";

function catalogOf(io = fixtureIo()) {
  const stamp = C.stampFor({ templatesDir: TEMPLATES, basis: "installed-package", io });
  return C.buildCatalog({ templatesDir: TEMPLATES, apiIds: new Set(["ApiPaidNode"]), stamp, licenseMap: C.loadLicenseMap(LICENSE_MAP), io });
}
const rowOf = (catalog, name) => catalog.templates.find((t) => t.name === name);
const verdictOf = (name) => { const v = M.verdictFor(rowOf(catalogOf(), name)); return [v.outcome, v.class]; };
const snapshotOf = (io) => C.snapshotNode({ comfyDir: COMFY, io });

// ---------------------------------------------------------------------------------------------------------------------
// The catalog row names the files an input loader reads
// ---------------------------------------------------------------------------------------------------------------------

test("catalog: a row names the input files its ACTIVE loaders read, and none for a muted or bypassed loader", () => {
  const catalog = catalogOf();
  assert.deepEqual(rowOf(catalog, "tt_input").input_files, [{ node_type: "LoadImage", name: "ref.png" }]);
  assert.deepEqual(rowOf(catalog, "tt_open").input_files, []);
});

test("INPUT_LOADER_TYPES is the loader list the graph door reads (LoadImage, LoadImageMask, LoadImageOutput, LoadVideo, LoadAudio)", () => {
  assert.deepEqual([...M.INPUT_LOADER_TYPES].sort(), ["LoadAudio", "LoadImage", "LoadImageMask", "LoadImageOutput", "LoadVideo"]);
});

// ---------------------------------------------------------------------------------------------------------------------
// The verdict: what v1 will run, defer or refuse, each with a typed class
// ---------------------------------------------------------------------------------------------------------------------

test("verdict: a paid partner-node template is a DEFER with the class api_paid (defer, never cloud)", () => {
  assert.deepEqual(verdictOf("api_tt_paid"), ["defer", "api_paid"]);
  const v = M.verdictFor(rowOf(catalogOf(), "api_tt_paid"));
  assert.equal(v.code, "TEMPLATE_API_PAID");
});

test("verdict: a template whose licence is not in the map is REFUSED as license_unknown (unknown is not permissive)", () => {
  assert.deepEqual(verdictOf("tt_unlisted"), ["refuse", "license_unknown"]);
  assert.equal(M.verdictFor(rowOf(catalogOf(), "tt_unlisted")).code, "TEMPLATE_LICENSE_UNKNOWN");
});

test("verdict: a weights file with no Hugging Face source is unknown too", () => {
  assert.deepEqual(verdictOf("tt_unresolved"), ["refuse", "license_unknown"]);
});

test("verdict: a permissive class that rests on an inferred map entry only is read as unknown", () => {
  assert.equal(rowOf(catalogOf(), "tt_inferred").license.class, "permissive", "the fixture is permissive by inference");
  assert.deepEqual(verdictOf("tt_inferred"), ["refuse", "license_unknown"]);
});

test("verdict: conditional and non-commercial licences are refused under their own class", () => {
  assert.deepEqual(verdictOf("tt_cond"), ["refuse", "license_conditional"]);
  assert.deepEqual(verdictOf("tt_nc"), ["refuse", "license_non_commercial"]);
});

test("verdict: a FLUX-family template is refused as flux_barred, a custom-node template as custom_nodes, a missing file as template_unreadable", () => {
  assert.deepEqual(verdictOf("tt_flux_pic"), ["refuse", "flux_barred"]);
  assert.deepEqual(verdictOf("tt_pack"), ["refuse", "custom_nodes"]);
  assert.deepEqual(verdictOf("tt_broken"), ["refuse", "template_unreadable"]);
});

test("verdict: a permissive template, a zero-model template and one that reads input files all run", () => {
  assert.deepEqual(verdictOf("tt_open"), ["run", null]);
  assert.deepEqual(verdictOf("tt_zero"), ["run", null]);
  assert.deepEqual(verdictOf("tt_input"), ["run", null]);
});

test("verdict: every reason is listed, the first is the class", () => {
  const row = { ...rowOf(catalogOf(), "tt_pack"), license: { ...rowOf(catalogOf(), "tt_nc").license } };
  const v = M.verdictFor(row);
  assert.equal(v.class, "custom_nodes");
  assert.deepEqual(v.reasons, ["custom_nodes", "license_non_commercial"]);
});

test("verdict: a model name that climbs out of its class directory is refused as bad_model_path", () => {
  const row = rowOf(catalogOf(), "tt_open");
  for (const name of ["../x.safetensors", "a/../../x.safetensors", "/abs/x.safetensors", "C:/x.safetensors", "a\\..\\x.safetensors"]) {
    const bad = { ...row, models: [{ ...row.models[0], name }] };
    assert.deepEqual([M.verdictFor(bad).outcome, M.verdictFor(bad).class], ["refuse", "bad_model_path"], name);
  }
  const badDir = { ...row, models: [{ ...row.models[0], directory: "../etc" }] };
  assert.equal(M.verdictFor(badDir).class, "bad_model_path");
});

// ---------------------------------------------------------------------------------------------------------------------
// The manifest entry
// ---------------------------------------------------------------------------------------------------------------------

test("manifest: models become models/<class>/<name> with the source and the declared hash; no hash means trust on first use", () => {
  const m = M.manifestFor(rowOf(catalogOf(), "tt_open"));
  assert.equal(m.template, "tt_open");
  assert.equal(m.kind, "local");
  assert.equal(m.api, false);
  assert.equal(m.reads_input, false);
  assert.deepEqual(m.node_packs, []);
  assert.deepEqual(m.models.map((x) => [x.path, x.sha256, x.trust]), [
    ["models/diffusion_models/a.safetensors", null, "tofu"],
    ["models/vae/v.safetensors", SHA_DECLARED, "declared"],
  ]);
  assert.equal(m.models[0].source_url, "https://huggingface.co/example-org/weights-a/resolve/main/a.safetensors");
  assert.deepEqual([m.license.class, m.license.class_strict], ["permissive", "permissive"]);
});

test("manifest: a legacy class directory is written as the class ComfyUI maps it to", () => {
  const row = rowOf(catalogOf(), "tt_open");
  const legacy = { ...row, models: [{ ...row.models[0], name: "w.gguf", directory: "unet" }, { ...row.models[0], name: "t.safetensors", directory: "clip" }] };
  assert.deepEqual(M.manifestFor(legacy).models.map((x) => x.path), ["models/diffusion_models/w.gguf", "models/text_encoders/t.safetensors"]);
});

test("manifest: a model with no class (an unannotated loader widget) has no path until readiness finds it", () => {
  const row = rowOf(catalogOf(), "tt_open");
  const m = M.manifestFor({ ...row, models: [{ ...row.models[0], name: "loose.safetensors", directory: null }] });
  assert.equal(m.models[0].path, null);
});

test("manifest: reads_input and the files come from the active loaders; api is the row's flag", () => {
  const catalog = catalogOf();
  const m = M.manifestFor(rowOf(catalog, "tt_input"));
  assert.equal(m.reads_input, true);
  assert.deepEqual(m.input_files, [{ node_type: "LoadImage", name: "ref.png" }]);
  assert.equal(M.manifestFor(rowOf(catalog, "api_tt_paid")).api, true);
  assert.equal(M.manifestFor(rowOf(catalog, "tt_pack")).custom_packs.length, 1);
});

test("manifest: the minimum ComfyUI version of the index entry is carried", () => {
  assert.equal(M.manifestFor(rowOf(catalogOf(), "tt_old")).min_comfyui_version, "99.0.0");
  assert.equal(M.manifestFor(rowOf(catalogOf(), "tt_open")).min_comfyui_version, null);
});

// ---------------------------------------------------------------------------------------------------------------------
// list and get: deterministic on a fixture catalog
// ---------------------------------------------------------------------------------------------------------------------

test("list: the same catalog gives the same answer, sorted by name, and a default of the runnable templates", () => {
  const a = M.listTemplates(catalogOf(), {});
  const b = M.listTemplates(catalogOf(), {});
  assert.equal(JSON.stringify(a), JSON.stringify(b));
  assert.deepEqual(a.templates.map((t) => t.name), ["tt_input", "tt_input_gone", "tt_old", "tt_open", "tt_zero"]);
  assert.match(a.basis, /installed package/);
  assert.equal(a.counts.total, 14);
  assert.equal(a.counts.runnable, 5);
});

test("list: counts say how many templates each refusal class holds, so the default's hidden ones are not invisible", () => {
  const { counts } = M.listTemplates(catalogOf(), {});
  assert.deepEqual(counts.by_class, {
    api_paid: 1, custom_nodes: 1, flux_barred: 1, license_conditional: 1, license_non_commercial: 1, license_unknown: 3, template_unreadable: 1,
  });
});

test("list: filters narrow the answer (kind, licence class, reads_input, text, tag, runnable false for the refused)", () => {
  const names = (f) => M.listTemplates(catalogOf(), f).templates.map((t) => t.name);
  assert.deepEqual(names({ reads_input: true }), ["tt_input", "tt_input_gone"]);
  assert.deepEqual(names({ license_class: "none" }), ["tt_zero"]);
  assert.deepEqual(names({ text: "text to image" }), ["tt_open"], "text matches the name, title and description");
  assert.deepEqual(names({ text: "Utility" }), [], "the tags are not searched by text");
  assert.deepEqual(names({ tag: "Utility" }), ["tt_zero"]);
  assert.deepEqual(names({ runnable: false }), ["api_tt_paid", "tt_broken", "tt_cond", "tt_flux_pic", "tt_inferred", "tt_nc", "tt_pack", "tt_unlisted", "tt_unresolved"]);
  assert.deepEqual(names({ kind: "api", runnable: false }), ["api_tt_paid"]);
  assert.deepEqual(names({ text: "Needs a newer" }), ["tt_old"]);
});

test("list: limit and offset page the answer and the totals stay whole", () => {
  const page = M.listTemplates(catalogOf(), { limit: 2, offset: 1 });
  assert.deepEqual(page.templates.map((t) => t.name), ["tt_input_gone", "tt_old"]);
  assert.deepEqual([page.matched, page.returned, page.offset, page.limit], [5, 2, 1, 2]);
});

test("list: each row carries the verdict and the licence class", () => {
  const rows = M.listTemplates(catalogOf(), { runnable: false }).templates;
  const paid = rows.find((t) => t.name === "api_tt_paid");
  assert.deepEqual([paid.runnable, paid.outcome, paid.class, paid.kind], [false, "defer", "api_paid", "api"]);
  const nc = rows.find((t) => t.name === "tt_nc");
  assert.deepEqual([nc.license_class, nc.class], ["non_commercial", "license_non_commercial"]);
});

test("get: one template's manifest and licence class; an unknown name is a typed answer", () => {
  const catalog = catalogOf();
  const got = M.getTemplate(catalog, "tt_open");
  assert.equal(got.ok, true);
  assert.equal(got.manifest.template, "tt_open");
  assert.equal(got.verdict.outcome, "run");
  assert.equal(got.manifest.license.class, "permissive");
  assert.equal(JSON.stringify(got), JSON.stringify(M.getTemplate(catalogOf(), "tt_open")));
  const unknown = M.getTemplate(catalog, "nope");
  assert.deepEqual([unknown.ok, unknown.outcome, unknown.refused, unknown.class, unknown.code, unknown.template], [false, "refuse", true, "unknown_template", "TEMPLATE_UNKNOWN_TEMPLATE", "nope"]);
  assert.match(unknown.detail, /no template named nope/);
});

// ---------------------------------------------------------------------------------------------------------------------
// readiness
// ---------------------------------------------------------------------------------------------------------------------

test("readiness: models present, missing, and the verdict beside them", () => {
  const io = fixtureIo();
  const r = M.readinessFor(rowOf(catalogOf(io), "tt_open"), snapshotOf(io), { io });
  assert.equal(r.ready, true);
  assert.deepEqual(r.models.map((m) => [m.name, m.status]), [["a.safetensors", "present"], ["v.safetensors", "present"]]);
  assert.deepEqual(r.models_missing, []);
  assert.equal(r.license.class, "permissive");
  assert.deepEqual(r.nodes, { status: "core_only", custom_packs: [] });

  const bare = fixtureIo({ models: ["vae/v.safetensors"] });
  const missing = M.readinessFor(rowOf(catalogOf(bare), "tt_open"), snapshotOf(bare), { io: bare });
  assert.equal(missing.ready, false);
  assert.deepEqual(missing.models_missing, ["models/diffusion_models/a.safetensors"]);
  assert.deepEqual(missing.blocking, ["models_missing"]);
});

test("readiness: a file under another class directory is not ready, and says where it is", () => {
  const io = fixtureIo({ models: ["vae/a.safetensors", "vae/v.safetensors"] });
  const r = M.readinessFor(rowOf(catalogOf(io), "tt_open"), snapshotOf(io), { io });
  const a = r.models.find((m) => m.name === "a.safetensors");
  assert.equal(a.status, "wrong_class");
  assert.equal(r.ready, false);
});

test("readiness: the input files a template reads are checked in the input directory", () => {
  const io = fixtureIo({ inputs: [] });
  const r = M.readinessFor(rowOf(catalogOf(io), "tt_input"), snapshotOf(io), { io });
  assert.deepEqual(r.inputs, [{ name: "ref.png", dir: "input", node_type: "LoadImage", status: "missing" }]);
  assert.deepEqual(r.inputs_missing, ["ref.png"]);
  assert.deepEqual(r.blocking, ["inputs_missing"]);
  const ok = fixtureIo();
  const staged = M.readinessFor(rowOf(catalogOf(ok), "tt_input"), snapshotOf(ok), { io: ok });
  assert.deepEqual(staged.inputs.map((i) => i.status), ["present"]);
  assert.equal(staged.ready, true);
});

test("readiness: a node whose ComfyUI is older than the template's minimum is not ready", () => {
  const io = fixtureIo({ models: ["diffusion_models/a.safetensors"] });
  const r = M.readinessFor(rowOf(catalogOf(io), "tt_old"), snapshotOf(io), { io });
  assert.deepEqual(r.comfyui, { version: "0.37.0", min_version: "99.0.0", status: "too_old" });
  assert.deepEqual(r.blocking, ["comfyui_too_old"]);
  assert.equal(r.ready, false);
});

test("readiness: a refused template is not ready whatever sits on the disk, and its verdict leads the blocking list", () => {
  const io = fixtureIo();
  const r = M.readinessFor(rowOf(catalogOf(io), "tt_nc"), snapshotOf(io), { io });
  assert.equal(r.ready, false);
  assert.equal(r.blocking[0], "license_non_commercial");
});

test("readiness: the same inputs give the same answer", () => {
  const io = fixtureIo();
  const run = () => JSON.stringify(M.readinessFor(rowOf(catalogOf(io), "tt_input"), snapshotOf(io), { io }));
  assert.equal(run(), run());
});

// ---------------------------------------------------------------------------------------------------------------------
// resolve: the gate in front of a run
// ---------------------------------------------------------------------------------------------------------------------

function resolveOf(name, opts) {
  const io = fixtureIo(opts);
  return M.resolveRun(rowOf(catalogOf(io), name), snapshotOf(io), { io });
}

test("resolve: a paid-API template is a Defer with the typed class and no manifest", () => {
  const r = resolveOf("api_tt_paid");
  assert.deepEqual([r.ok, r.deferred, r.outcome, r.class, r.code], [false, true, "defer", "api_paid", "TEMPLATE_API_PAID"]);
  assert.equal(r.runtime_manifest, undefined);
});

test("resolve: an unlisted-licence template is refused with its class and no manifest", () => {
  const r = resolveOf("tt_unlisted");
  assert.deepEqual([r.ok, r.refused, r.outcome, r.class], [false, true, "refuse", "license_unknown"]);
  assert.equal(r.runtime_manifest, undefined);
});

test("resolve: missing models are refused with the exact list, before anything is started", () => {
  const r = resolveOf("tt_open", { models: [] });
  assert.deepEqual([r.ok, r.class], [false, "models_missing"]);
  assert.deepEqual(r.missing, ["models/diffusion_models/a.safetensors", "models/vae/v.safetensors"]);
});

test("resolve: input files that are not staged are refused with the exact list", () => {
  const r = resolveOf("tt_input_gone");
  assert.deepEqual([r.ok, r.class, r.missing], [false, "inputs_missing", ["nowhere.png"]]);
});

test("resolve: a ComfyUI older than the template's minimum is refused as comfyui_too_old", () => {
  const r = resolveOf("tt_old", { models: ["diffusion_models/a.safetensors"] });
  assert.deepEqual([r.ok, r.class], [false, "comfyui_too_old"]);
});

test("resolve: a template that reads input files is a local-only ticket and says why the fleet was not asked", () => {
  const r = resolveOf("tt_input");
  assert.equal(r.ok, true);
  assert.equal(r.local_only, true);
  assert.match(r.fleet_note, /reads input files \(ref\.png\)/);
  assert.match(r.fleet_note, /same-named file/);
  assert.deepEqual(r.input_files, [{ node_type: "LoadImage", name: "ref.png" }]);
});

test("resolve: a template that reads no input file is not marked local-only for that reason, and v1 still places it on this machine", () => {
  const r = resolveOf("tt_open");
  assert.equal(r.ok, true);
  assert.equal(r.local_only, false);
  assert.match(r.fleet_note, /this machine/);
});

test("resolve: the runtime manifest names the models by their resolved path and carries NO source_url, so a run never downloads", () => {
  const r = resolveOf("tt_open");
  assert.deepEqual(r.runtime_manifest, {
    schema_version: 1,
    workflow: "tt_open",
    comfyui_min_version: "",
    node_packs: [],
    models: [
      { path: "models/diffusion_models/a.safetensors", source_url: "", sha256: null, trust: "tofu" },
      { path: "models/vae/v.safetensors", source_url: "", sha256: SHA_DECLARED, trust: "declared" },
    ],
  });
  assert.ok(r.runtime_manifest.models.every((m) => m.source_url === ""));
});

test("resolve: a model with no class is given the class directory it was found in", () => {
  const io = fixtureIo({ models: ["text_encoders/loose.safetensors"], extra: {} });
  const catalog = catalogOf(io);
  const row = rowOf(catalog, "tt_zero");
  const loose = { ...row, models: [{ name: "loose.safetensors", directory: null, url: null, hash: null, hash_type: null, node_types: ["X"], annotated: false }], license: { ...row.license, class: "permissive", class_strict: "permissive", repos: [], unresolved_models: 0, unmapped_repos: [] } };
  const r = M.resolveRun(loose, snapshotOf(io), { io });
  assert.equal(r.ok, true);
  assert.deepEqual(r.runtime_manifest.models.map((m) => m.path), ["models/text_encoders/loose.safetensors"]);
});

test("resolve: the runtime manifest survives the run-graph manifest parser, trust included", () => {
  const rt = resolveOf("tt_open").runtime_manifest;
  const parsed = parseManifest(rt);
  assert.deepEqual(parsed.models.map((m) => [m.path, m.trust ?? null]), [["models/diffusion_models/a.safetensors", "tofu"], ["models/vae/v.safetensors", "declared"]]);
  assert.deepEqual(parseManifest({ models: [{ path: "p" }] }).models, [{ path: "p", source_url: "", sha256: null }], "a manifest that names no trust parses as before");
});

test("resolve: the same inputs give the same answer", () => {
  assert.equal(JSON.stringify(resolveOf("tt_input")), JSON.stringify(resolveOf("tt_input")));
});

test("fixture tree: the in-memory filesystem and the fixture files are what the tests above assume", () => {
  const files = fixtureFiles();
  assert.ok(files[`${COMFY}/models/vae/v.safetensors`]);
  assert.equal(memIo({ "/a/b": "x" }).readText("/a/b"), "x");
});

// ---------------------------------------------------------------------------------------------------------------------
// A template whose subgraphs the catalog could not expand is not run on the models it did read
// ---------------------------------------------------------------------------------------------------------------------

// A permissive UNET loader at the top level, then `levels` subgraph instances, one inside the next, and a permissive VAE loader in the
// innermost definition. The catalog expands eight levels (MAX_SUBGRAPH_DEPTH); the converter expands more, so past that depth the
// converted graph loads a file the catalog's list never named.
const nestId = (k) => `${k.toString(16).padStart(8, "0")}-1111-4111-8111-111111111111`;
const nestLoader = (id, type, name, directory) => ({
  id, type, mode: 0, widgets_values: [name],
  properties: { cnr_id: "comfy-core", models: [{ name, directory, url: `https://huggingface.co/example-org/weights-a/resolve/main/${name}` }] },
});
const nestedWorkflow = (levels) => JSON.stringify({
  version: 0.4,
  nodes: [nestLoader(1, "UNETLoader", "a.safetensors", "diffusion_models"), { id: 2, type: nestId(1), mode: 0 }],
  definitions: {
    subgraphs: Array.from({ length: levels }, (_, k) => ({
      id: nestId(k + 1),
      nodes: [k + 1 < levels ? { id: 10, type: nestId(k + 2), mode: 0 } : nestLoader(11, "VAELoader", "deep.safetensors", "vae")],
    })),
  },
});
const loopWorkflow = () => JSON.stringify({
  version: 0.4,
  nodes: [nestLoader(1, "UNETLoader", "a.safetensors", "diffusion_models"), { id: 2, type: nestId(1), mode: 0 }],
  definitions: { subgraphs: [{ id: nestId(1), nodes: [nestLoader(11, "VAELoader", "deep.safetensors", "vae"), { id: 12, type: nestId(1), mode: 0 }] }] },
});

// A tree holding only the templates named, each with the workflow text given; the two models are on disk.
function nestIo(templates) {
  const index = JSON.stringify([{
    moduleName: "default", category: "Fixture", title: "Fixture", type: "image",
    templates: Object.keys(templates).map((name) => ({ name, mediaType: "image", mediaSubtype: "webp", openSource: true, title: name, tags: ["Image"], models: ["A"] })),
  }]);
  const extra = { [`${TEMPLATES}/index.json`]: index };
  for (const [name, wf] of Object.entries(templates)) extra[`${TEMPLATES}/${name}.json`] = wf;
  return memIo(fixtureFiles({ models: ["diffusion_models/a.safetensors", "vae/deep.safetensors"], extra }));
}
const nestCatalog = (templates) => catalogOf(nestIo(templates));

test("verdict: a template nested one level past the catalog's reach is REFUSED as subgraph_unexpanded, not run on a manifest that names only the shallow file", () => {
  const row = rowOf(nestCatalog({ tt_nest9: nestedWorkflow(9) }), "tt_nest9");
  assert.equal(row.graph.unexpanded, 1, "the catalog left the ninth instance as a leaf");
  assert.deepEqual(row.models.map((m) => m.name), ["a.safetensors"], "the hazard: the deep loader is not in the list the gate, the licence check and the host-RAM figure read");
  const v = M.verdictFor(row);
  assert.deepEqual([v.outcome, v.class, v.code], ["refuse", "subgraph_unexpanded", "TEMPLATE_SUBGRAPH_UNEXPANDED"]);
  assert.match(v.detail, /unexpanded|nest/i);
});

test("verdict: a template nested exactly as deep as the catalog expands still runs, and its manifest names the deepest file", () => {
  const row = rowOf(nestCatalog({ tt_nest8: nestedWorkflow(8) }), "tt_nest8");
  assert.equal(row.graph.unexpanded, 0);
  assert.deepEqual(M.manifestFor(row).models.map((m) => m.path), ["models/diffusion_models/a.safetensors", "models/vae/deep.safetensors"]);
  assert.deepEqual([M.verdictFor(row).outcome, M.verdictFor(row).class], ["run", null]);
});

test("verdict: a subgraph that contains itself is refused as subgraph_unexpanded (the converter refuses it too)", () => {
  const row = rowOf(nestCatalog({ tt_loop: loopWorkflow() }), "tt_loop");
  assert.equal(row.graph.unexpanded, 1);
  assert.deepEqual([M.verdictFor(row).outcome, M.verdictFor(row).class], ["refuse", "subgraph_unexpanded"]);
});

test("resolve: a template nested past the catalog's reach is refused before a manifest exists, whatever sits on the disk", () => {
  const io = nestIo({ tt_nest9: nestedWorkflow(9) });
  const r = M.resolveRun(rowOf(catalogOf(io), "tt_nest9"), snapshotOf(io), { io });
  assert.deepEqual([r.ok, r.refused, r.class, r.code], [false, true, "subgraph_unexpanded", "TEMPLATE_SUBGRAPH_UNEXPANDED"]);
  assert.equal(r.runtime_manifest, undefined);
});

test("list: a template nested past the catalog's reach is not runnable and is counted under its own class", () => {
  const l = M.listTemplates(nestCatalog({ tt_nest8: nestedWorkflow(8), tt_nest9: nestedWorkflow(9) }), {});
  assert.deepEqual(l.templates.map((t) => t.name), ["tt_nest8"]);
  assert.deepEqual(l.counts.by_class, { subgraph_unexpanded: 1 });
  assert.equal(M.listTemplates(nestCatalog({ tt_nest9: nestedWorkflow(9) }), { runnable: false }).templates[0].class, "subgraph_unexpanded");
});

// ---------------------------------------------------------------------------------------------------------------------
// A model file only the converter reads is in the list the gate reads
// ---------------------------------------------------------------------------------------------------------------------

const instanceTemplate = (inst) => JSON.stringify({
  version: 0.4,
  nodes: [{ id: 2, type: nestId(1), mode: 0, ...inst }],
  definitions: { subgraphs: [{ id: nestId(1), nodes: [nestLoader(11, "CheckpointLoaderSimple", "inner.safetensors", "checkpoints")] }] },
});

test("verdict: a file an active subgraph instance carries into an inner loader is in the list the gate reads, so the template is refused as license_unknown and not run on the inner file alone", () => {
  for (const inst of [{ widgets_values: ["outer.safetensors"], properties: { proxyWidgets: [["11", "ckpt_name"]] } }, { widgets_values: ["outer.safetensors"] }]) {
    const row = rowOf(nestCatalog({ tt_inst: instanceTemplate(inst) }), "tt_inst");
    assert.deepEqual(row.models.map((m) => m.name).sort(), ["inner.safetensors", "outer.safetensors"]);
    assert.deepEqual([M.verdictFor(row).outcome, M.verdictFor(row).class], ["refuse", "license_unknown"]);
  }
});

test("verdict: a loader whose widgets_values is an object names its file too: refused as license_unknown, not read as a template with no weights", () => {
  const wf = JSON.stringify({ version: 0.4, nodes: [{ id: 1, type: "CheckpointLoaderSimple", mode: 0, widgets_values: { ckpt_name: "keyed.safetensors" } }] });
  const row = rowOf(nestCatalog({ tt_keyed: wf }), "tt_keyed");
  assert.deepEqual(row.models.map((m) => m.name), ["keyed.safetensors"]);
  assert.deepEqual([M.verdictFor(row).outcome, M.verdictFor(row).class], ["refuse", "license_unknown"]);
});

// ---------------------------------------------------------------------------------------------------------------------
// An embedding the template's own prompt names is a model file the gate reads
// ---------------------------------------------------------------------------------------------------------------------

const embeddingTemplate = (annotation) => JSON.stringify({
  version: 0.4,
  nodes: [
    nestLoader(1, "UNETLoader", "a.safetensors", "diffusion_models"),
    { id: 2, type: "CLIPTextEncode", mode: 0, widgets_values: ["embedding:evil_ti, a cat"], properties: { cnr_id: "comfy-core", ...(annotation ? { models: [annotation] } : {}) } },
  ],
});
// A tree holding the one template, with the model files given (paths under models/).
const embeddingIo = (annotation, models = ["diffusion_models/a.safetensors", "embeddings/evil_ti.safetensors"]) => {
  const index = JSON.stringify([{ moduleName: "default", category: "Fixture", title: "Fixture", type: "image", templates: [{ name: "tt_emb", mediaType: "image", mediaSubtype: "webp", openSource: true, title: "tt_emb", tags: ["Image"], models: ["A"] }] }]);
  return memIo(fixtureFiles({ models, extra: { [`${TEMPLATES}/index.json`]: index, [`${TEMPLATES}/tt_emb.json`]: embeddingTemplate(annotation) } }));
};
const WEIGHTS_A = (name) => `https://huggingface.co/example-org/weights-a/resolve/main/${name}`;

test("verdict: a template whose own prompt names an embedding is refused as license_unknown when nothing annotates the file (the manifest must list it, with a source the map knows)", () => {
  const row = rowOf(catalogOf(embeddingIo(null)), "tt_emb");
  assert.deepEqual(row.models.map((m) => [m.directory, m.name]), [["diffusion_models", "a.safetensors"], ["embeddings", "evil_ti"]]);
  assert.deepEqual([M.verdictFor(row).outcome, M.verdictFor(row).class], ["refuse", "license_unknown"]);
});

test("verdict: the same template runs when an annotation of the embeddings class names the file and its source is in the map; the manifest lists that file once", () => {
  const annotation = { name: "evil_ti.safetensors", directory: "embeddings", url: WEIGHTS_A("evil_ti.safetensors") };
  const row = rowOf(catalogOf(embeddingIo(annotation)), "tt_emb");
  assert.deepEqual([M.verdictFor(row).outcome, M.verdictFor(row).class], ["run", null]);
  assert.deepEqual(M.manifestFor(row).models.map((m) => m.path), ["models/diffusion_models/a.safetensors", "models/embeddings/evil_ti.safetensors"]);
});

test("resolve: an embedding is named in the runtime manifest by the file ComfyUI would load, and a node that holds none is refused by name", () => {
  const annotation = { name: "evil_ti.safetensors", directory: "embeddings", url: WEIGHTS_A("evil_ti.safetensors") };
  const io = embeddingIo(annotation);
  const ok = M.resolveRun(rowOf(catalogOf(io), "tt_emb"), snapshotOf(io), { io });
  assert.equal(ok.ok, true);
  assert.deepEqual(ok.runtime_manifest.models.map((m) => m.path), ["models/diffusion_models/a.safetensors", "models/embeddings/evil_ti.safetensors"]);
  const bare = embeddingIo(annotation, ["diffusion_models/a.safetensors"]);
  const gone = M.resolveRun(rowOf(catalogOf(bare), "tt_emb"), snapshotOf(bare), { io: bare });
  assert.deepEqual([gone.ok, gone.class, gone.missing], [false, "models_missing", ["models/embeddings/evil_ti.safetensors"]]);
});

test("readiness: an embedding the prompt names without an extension is met by the file of the node that ComfyUI would load, and the manifest path says which", () => {
  for (const [file, want] of [["evil_ti.pt", "models/embeddings/evil_ti.pt"], ["evil_ti.bin", "models/embeddings/evil_ti.bin"]]) {
    const io = embeddingIo(null, ["diffusion_models/a.safetensors", `embeddings/${file}`]);
    const r = M.readinessFor(rowOf(catalogOf(io), "tt_emb"), snapshotOf(io), { io });
    const m = r.models.find((x) => x.name === "evil_ti");
    assert.deepEqual([m.status, m.resolved_path], ["present", want], file);
  }
  const none = embeddingIo(null, ["diffusion_models/a.safetensors"]);
  const r = M.readinessFor(rowOf(catalogOf(none), "tt_emb"), snapshotOf(none), { io: none });
  const m = r.models.find((x) => x.name === "evil_ti");
  assert.deepEqual([m.status, m.resolved_path], ["missing", null]);
  assert.ok(r.models_missing.includes("models/embeddings/evil_ti"));
});

test("readiness: an embedding the prompt names that the node holds only in a subfolder of the embeddings directory is met, and the manifest path names that file", () => {
  const io = embeddingIo(null, ["diffusion_models/a.safetensors", "embeddings/sd15/evil_ti.safetensors"]);
  const r = M.readinessFor(rowOf(catalogOf(io), "tt_emb"), snapshotOf(io), { io });
  const m = r.models.find((x) => x.name === "evil_ti");
  assert.deepEqual([m.status, m.resolved_path], ["present", "models/embeddings/sd15/evil_ti.safetensors"]);
});

test("resolve: an annotated embedding the node holds in a subfolder runs, and the runtime manifest names the file where it sits", () => {
  const annotation = { name: "evil_ti.safetensors", directory: "embeddings", url: WEIGHTS_A("evil_ti.safetensors") };
  const io = embeddingIo(annotation, ["diffusion_models/a.safetensors", "embeddings/sd15/evil_ti.safetensors"]);
  const ok = M.resolveRun(rowOf(catalogOf(io), "tt_emb"), snapshotOf(io), { io });
  assert.equal(ok.ok, true);
  assert.deepEqual(ok.runtime_manifest.models.map((m) => m.path), ["models/diffusion_models/a.safetensors", "models/embeddings/sd15/evil_ti.safetensors"]);
});

test("resolve: an embedding the node holds in the root and in a subfolder is refused, because ComfyUI opens one of them by set order and the manifest would name only one", () => {
  const annotation = { name: "evil_ti.safetensors", directory: "embeddings", url: WEIGHTS_A("evil_ti.safetensors") };
  const io = embeddingIo(annotation, ["diffusion_models/a.safetensors", "embeddings/evil_ti.safetensors", "embeddings/sd15/evil_ti.safetensors"]);
  const row = rowOf(catalogOf(io), "tt_emb");
  const gone = M.resolveRun(row, snapshotOf(io), { io });
  assert.deepEqual([gone.ok, gone.class, gone.missing], [false, "models_missing", ["models/embeddings/evil_ti.safetensors"]]);
  assert.match(gone.detail, /held in more than one directory or shadowed/, "the refusal says why the file is not missing");
  const m = M.readinessFor(row, snapshotOf(io), { io }).models.find((x) => x.name === "evil_ti.safetensors");
  assert.deepEqual([m.status, m.resolved_path, m.found], ["ambiguous", null, ["evil_ti.safetensors", "sd15/evil_ti.safetensors"]]);
});

test("resolve: an annotated embedding that another file of the node shadows is refused, because ComfyUI would open the other file and the licence and hash are the annotated one's", () => {
  const annotation = { name: "evil_ti.pt", directory: "embeddings", url: WEIGHTS_A("evil_ti.pt") };
  const io = embeddingIo(annotation, ["diffusion_models/a.safetensors", "embeddings/evil_ti.pt", "embeddings/evil_ti.safetensors"]);
  const row = rowOf(catalogOf(io), "tt_emb");
  const gone = M.resolveRun(row, snapshotOf(io), { io });
  assert.deepEqual([gone.ok, gone.class, gone.missing], [false, "models_missing", ["models/embeddings/evil_ti.pt"]]);
  const m = M.readinessFor(row, snapshotOf(io), { io }).models.find((x) => x.name === "evil_ti.pt");
  assert.deepEqual([m.status, m.found], ["shadowed", ["evil_ti.safetensors"]]);
  const alone = embeddingIo(annotation, ["diffusion_models/a.safetensors", "embeddings/evil_ti.pt"]);
  assert.equal(M.resolveRun(rowOf(catalogOf(alone), "tt_emb"), snapshotOf(alone), { io: alone }).ok, true, "with only the annotated file the run is allowed");
});

test("resolve: a file of no extension that ComfyUI opens first for the name refuses the run, though the inventory lists only the annotated file", () => {
  const annotation = { name: "evil_ti.safetensors", directory: "embeddings", url: WEIGHTS_A("evil_ti.safetensors") };
  const io = embeddingIo(annotation, ["diffusion_models/a.safetensors", "embeddings/evil_ti.safetensors", "embeddings/evil_ti"]);
  const row = rowOf(catalogOf(io), "tt_emb");
  const gone = M.resolveRun(row, snapshotOf(io), { io });
  assert.deepEqual([gone.ok, gone.class, gone.missing], [false, "models_missing", ["models/embeddings/evil_ti.safetensors"]]);
  const m = M.readinessFor(row, snapshotOf(io), { io }).models.find((x) => x.name === "evil_ti.safetensors");
  assert.deepEqual([m.status, m.found], ["shadowed", ["evil_ti"]]);
  const bare = embeddingIo(null, ["diffusion_models/a.safetensors", "embeddings/evil_ti.safetensors", "embeddings/evil_ti"]);
  const p = M.readinessFor(rowOf(catalogOf(bare), "tt_emb"), snapshotOf(bare), { io: bare }).models.find((x) => x.name === "evil_ti");
  assert.deepEqual([p.status, p.resolved_path], ["present", "models/embeddings/evil_ti"], "an unannotated prompt name resolves to the file ComfyUI opens, so the manifest names that file");
});

// A loader widget that names an embeddings file (ConditioningLoader) is read by ComfyUI's plain loader rule: the exact relative path under
// the first root, no subfolder search. The prompt lookup (a set of directories, a subfolder file found by the bare name) is not its rule.
const conditioningIo = (models) => {
  const index = JSON.stringify([{ moduleName: "default", category: "Fixture", title: "Fixture", type: "image", templates: [{ name: "tt_emb", mediaType: "image", mediaSubtype: "webp", openSource: true, title: "tt_emb", tags: ["Image"], models: ["A"] }] }]);
  const wf = JSON.stringify({
    version: 0.4,
    nodes: [
      nestLoader(1, "UNETLoader", "a.safetensors", "diffusion_models"),
      { id: 2, type: "ConditioningLoader", mode: 0, widgets_values: ["cond.safetensors"], properties: { cnr_id: "comfy-core", models: [{ name: "cond.safetensors", directory: "embeddings", url: WEIGHTS_A("cond.safetensors") }] } },
    ],
  });
  return memIo(fixtureFiles({ models: ["diffusion_models/a.safetensors", ...models], extra: { [`${TEMPLATES}/index.json`]: index, [`${TEMPLATES}/tt_emb.json`]: wf } }));
};

test("resolve: a conditioning file a loader widget names is met only at its exact path: held only in a subfolder it is missing, with the file named", () => {
  const io = conditioningIo(["embeddings/sd15/cond.safetensors"]);
  const row = rowOf(catalogOf(io), "tt_emb");
  const gone = M.resolveRun(row, snapshotOf(io), { io });
  assert.deepEqual([gone.ok, gone.class, gone.missing], [false, "models_missing", ["models/embeddings/cond.safetensors"]], "the widget value cond.safetensors is not in the combo of a node that holds sd15/cond.safetensors");
  const m = M.readinessFor(row, snapshotOf(io), { io }).models.find((x) => x.name === "cond.safetensors");
  assert.deepEqual([m.status, m.found, m.resolved_path], ["in_subfolder", ["sd15/cond.safetensors"], null]);
});

test("resolve: a conditioning file a loader widget names runs from the root even when a subfolder holds a copy, and the manifest names the root file", () => {
  const io = conditioningIo(["embeddings/cond.safetensors", "embeddings/sd15/cond.safetensors"]);
  const ok = M.resolveRun(rowOf(catalogOf(io), "tt_emb"), snapshotOf(io), { io });
  assert.equal(ok.ok, true, JSON.stringify(ok));
  assert.deepEqual(ok.runtime_manifest.models.map((m) => m.path), ["models/diffusion_models/a.safetensors", "models/embeddings/cond.safetensors"]);
});
