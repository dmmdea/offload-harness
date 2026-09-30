// render/templates-catalog.test.mjs — the catalog of ComfyUI workflow templates (phase A, read-only).
//
// Fixtures are in render/testdata/templates-catalog/ (README there: which are trimmed upstream
// templates and which are hand-written). Every test that needs a filesystem either reads those
// fixtures through the real io or builds a small in-memory tree, so nothing here touches a real
// ComfyUI install, the network or a GPU.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import { spawnSync } from "node:child_process";
import * as nfs from "node:fs";
import * as nos from "node:os";
import * as C from "./templates-catalog.mjs";

const FX = fileURLToPath(new URL("./testdata/templates-catalog/", import.meta.url)).replace(/\\/g, "/").replace(/\/$/, "");
const REAL_SITE = `${FX}/site-packages`;
const REAL_TEMPLATES = `${REAL_SITE}/comfyui_workflow_templates_json/templates`;
const SYN_TEMPLATES = `${FX}/synthetic/templates`;
const API_DIR = `${FX}/comfy_api_nodes`;
const loadJson = (p) => JSON.parse(readFileSync(p, "utf8"));
const realWorkflow = (name) => loadJson(`${REAL_TEMPLATES}/${name}.json`);
const synWorkflow = (name) => loadJson(`${SYN_TEMPLATES}/${name}.json`);

// A small in-memory filesystem with the same surface as C.nodeIo. `files` maps a path to its text
// (or to { size } for a file whose content nobody reads); directories are implied by their children.
function memIo(files = {}, { env = {}, home = "/srv/fixture-home" } = {}) {
  const norm = (p) => String(p).replace(/\\/g, "/").replace(/\/+$/, "");
  const map = new Map(Object.entries(files).map(([p, v]) => [norm(p), v]));
  const dirs = new Set();
  for (const p of map.keys()) {
    const parts = p.split("/");
    for (let i = 1; i < parts.length; i++) dirs.add(parts.slice(0, i).join("/") || "/");
  }
  const enoent = (p) => Object.assign(new Error(`ENOENT: ${p}`), { code: "ENOENT" });
  const io = {
    writes: [],
    env,
    home,
    exists: (p) => map.has(norm(p)) || dirs.has(norm(p)),
    readText: (p) => {
      const v = map.get(norm(p));
      if (typeof v !== "string") throw enoent(p);
      return v;
    },
    list: (p) => {
      const base = norm(p);
      if (!dirs.has(base)) return null;
      const seen = new Map();
      for (const f of map.keys()) {
        if (!f.startsWith(base + "/")) continue;
        const rest = f.slice(base.length + 1);
        const [head, ...tail] = rest.split("/");
        seen.set(head, tail.length > 0);
      }
      return [...seen].map(([name, isDir]) => ({ name, isDir, isFile: !isDir, isSymlink: false }));
    },
    stat: (p) => {
      const v = map.get(norm(p));
      if (v === undefined) return dirs.has(norm(p)) ? { size: 0, isDir: true, isFile: false } : null;
      return { size: typeof v === "string" ? Buffer.byteLength(v) : v.size, isDir: false, isFile: true };
    },
    realpath: (p) => norm(p),
    sha256: (p) => createHash("sha256").update(io.readText(p)).digest("hex"),
    writeText: (p, s) => { io.writes.push([norm(p), s]); },
  };
  return io;
}

const ids = (items) => items.map((x) => x.node.id).sort((a, b) => a - b);

// ---------------------------------------------------------------------------------------------
// The active-node walk
// ---------------------------------------------------------------------------------------------

test("walk: subgraph instances are expanded, never emitted; mode 2 and 4 make a node inactive", () => {
  const items = C.flattenActiveNodes(synWorkflow("synthetic_walk_matrix"));
  assert.equal(items.length, 12);
  assert.deepEqual(ids(items.filter((x) => x.active)), [1, 6, 7, 8, 10, 11, 20]);
  assert.deepEqual(ids(items.filter((x) => !x.active)), [2, 3, 12, 21, 30]);
  const emittedTypes = new Set(items.map((x) => x.node.type));
  for (const inst of ["a1111111-1111-4111-8111-111111111111", "b2222222-2222-4222-8222-222222222222", "c3333333-3333-4333-8333-333333333333"]) {
    assert.ok(!emittedTypes.has(inst), `instance ${inst} must be expanded, not emitted`);
  }
});

test("walk: a bypassed instance turns off everything inside it, at any depth", () => {
  const items = C.flattenActiveNodes(synWorkflow("synthetic_walk_matrix"));
  const under = items.find((x) => x.node.id === 30);
  assert.equal(under.active, false);
  assert.deepEqual(under.path, [5]);
});

test("walk: a node inside a nested subgraph reports its depth and the instance chain", () => {
  const items = C.flattenActiveNodes(synWorkflow("synthetic_walk_matrix"));
  const nested = items.find((x) => x.node.id === 20);
  assert.equal(nested.depth, 2);
  assert.deepEqual(nested.path, [4, 13]);
  assert.equal(nested.active, true);
});

test("walk: an unreferenced subgraph definition is never walked", () => {
  const items = C.flattenActiveNodes(synWorkflow("synthetic_walk_matrix"));
  assert.ok(!items.some((x) => x.node.id === 40));
});

test("walk: an instance of an undefined subgraph stays in the list as a dangling leaf", () => {
  const items = C.flattenActiveNodes(synWorkflow("synthetic_walk_matrix"));
  const dangling = items.find((x) => x.node.id === 6);
  assert.equal(dangling.dangling, true);
  assert.equal(dangling.active, true);
});

test("walk: a missing mode is active, and only 2 and 4 switch a node off", () => {
  const wf = { nodes: [{ id: 1, type: "A" }, { id: 2, type: "B", mode: 0 }, { id: 3, type: "C", mode: 1 }, { id: 4, type: "D", mode: 3 }, { id: 5, type: "E", mode: 2 }, { id: 6, type: "F", mode: 4 }] };
  assert.deepEqual(C.flattenActiveNodes(wf).map((x) => x.active), [true, true, true, true, false, false]);
});

test("walk: a subgraph that instantiates itself terminates and is flagged unexpanded", () => {
  const id = "a1111111-1111-4111-8111-111111111111";
  const wf = { nodes: [{ id: 1, type: id }], definitions: { subgraphs: [{ id, name: "loop", nodes: [{ id: 2, type: "X" }, { id: 3, type: id }] }] } };
  const items = C.flattenActiveNodes(wf);
  assert.deepEqual(ids(items), [2, 3]);
  assert.equal(items.find((x) => x.node.id === 3).unexpanded, true);
});

test("walk: the depth cap stops expansion and flags the instance unexpanded", () => {
  const items = C.flattenActiveNodes(synWorkflow("synthetic_walk_matrix"), { maxDepth: 1 });
  const cut = items.find((x) => x.node.id === 13);
  assert.equal(cut.unexpanded, true);
  assert.ok(!items.some((x) => x.node.id === 20), "the nested node is out of reach at maxDepth 1");
});

test("walk: junk in nodes and definitions is tolerated", () => {
  assert.deepEqual(C.flattenActiveNodes({}), []);
  assert.deepEqual(C.flattenActiveNodes(null), []);
  assert.deepEqual(C.flattenActiveNodes({ nodes: [null, 5, "x", { id: 1, type: "Ok" }], definitions: { subgraphs: [null, { nodes: [] }, "x"] } }).map((x) => x.node.id), [1]);
});

test("walk: a real subgraph template flattens to its inner nodes (z-image-turbo: 2 top-level + 9 inner)", () => {
  const items = C.flattenActiveNodes(realWorkflow("image_z_image_turbo"));
  assert.equal(items.length, 11);
  assert.ok(items.every((x) => x.active));
  assert.ok(items.some((x) => x.node.type === "UNETLoader" && x.depth === 1));
});

// ---------------------------------------------------------------------------------------------
// Required model files
// ---------------------------------------------------------------------------------------------

const names = (list) => list.map((m) => m.name).sort();

test("models: only active nodes need their files; bypassed, muted and bypassed-instance files are reported apart", () => {
  const r = C.requiredModels(synWorkflow("synthetic_walk_matrix"));
  assert.deepEqual(names(r.active), ["ckpt-active.safetensors", "primitive-file.safetensors", "te-nested.safetensors", "unet-in-subgraph.safetensors"]);
  assert.deepEqual(names(r.inactive), ["lora-bypassed.safetensors", "lora-inner-bypassed.safetensors", "unet-under-bypassed-instance.safetensors", "vae-muted.safetensors"]);
});

test("models: notes and unreferenced subgraph definitions contribute nothing", () => {
  const r = C.requiredModels(synWorkflow("synthetic_walk_matrix"));
  const all = [...r.active, ...r.inactive].map((m) => m.name);
  assert.ok(!all.includes("note-file.safetensors"));
  assert.ok(!all.includes("unreferenced.safetensors"));
});

test("models: an annotated requirement carries directory, url, hash and the node types that load it", () => {
  const r = C.requiredModels(synWorkflow("synthetic_walk_matrix"));
  const unet = r.active.find((m) => m.name === "unet-in-subgraph.safetensors");
  assert.deepEqual({ ...unet }, {
    name: "unet-in-subgraph.safetensors",
    directory: "diffusion_models",
    url: "https://huggingface.co/example-org/weights-a/resolve/main/split_files/diffusion_models/unet-in-subgraph.safetensors",
    hash: null,
    hash_type: null,
    annotated: true,
    node_types: ["UNETLoader"],
  });
  const hashed = C.requiredModels(synWorkflow("synthetic_unannotated")).active.find((m) => m.name === "annotated.safetensors");
  assert.equal(hashed.hash, "0".repeat(64));
  assert.equal(hashed.hash_type, "SHA256");
});

test("models: a file that only appears as a loader widget is a requirement with the class its loader implies", () => {
  const r = C.requiredModels(synWorkflow("synthetic_unannotated"));
  const by = Object.fromEntries(r.active.map((m) => [m.name, m]));
  assert.deepEqual(Object.keys(by).sort(), [
    "annotated.safetensors", "sub/dir/lora-in-subfolder.safetensors", "te-one.safetensors", "te-two.safetensors", "unannotated-unet.safetensors", "vendor-model.gguf",
  ]);
  assert.equal(by["unannotated-unet.safetensors"].directory, "diffusion_models");
  assert.equal(by["unannotated-unet.safetensors"].annotated, false);
  assert.equal(by["te-one.safetensors"].directory, "text_encoders");
  assert.equal(by["te-two.safetensors"].directory, "text_encoders");
  assert.equal(by["sub/dir/lora-in-subfolder.safetensors"].directory, "loras");
  assert.equal(by["vendor-model.gguf"].directory, null, "an unknown loader implies no class");
  assert.equal(by["annotated.safetensors"].annotated, true, "the annotation wins over the same file's widget");
});

test("models: an annotation that names the legacy class still wins over the same file's widget", () => {
  const wf = { nodes: [{ id: 1, type: "UNETLoader", mode: 0, widgets_values: ["legacy.safetensors", "default"],
    properties: { models: [{ name: "legacy.safetensors", url: "https://huggingface.co/example-org/w/resolve/main/legacy.safetensors", directory: "unet" }] } }] };
  const r = C.requiredModels(wf);
  assert.equal(r.active.length, 1, "one file, one requirement, whatever its loader implies");
  assert.equal(r.active[0].directory, "unet");
  assert.equal(r.active[0].annotated, true);
});

test("models: a loader whose widget names a different file than its annotation needs both", () => {
  const wf = { nodes: [{ id: 1, type: "VAELoader", mode: 0, widgets_values: ["old-name.safetensors"],
    properties: { models: [{ name: "new-name.safetensors", url: "https://huggingface.co/example-org/w/resolve/main/new-name.safetensors", directory: "vae" }] } }] };
  const r = C.requiredModels(wf);
  assert.deepEqual(r.active.map((m) => [m.name, m.annotated]).sort(), [["new-name.safetensors", true], ["old-name.safetensors", false]]);
});

test("models: a bypassed loader's widget file is not a requirement", () => {
  const r = C.requiredModels(synWorkflow("synthetic_unannotated"));
  assert.ok(!r.active.some((m) => m.name === "vae-bypassed-unannotated.safetensors"));
});

test("models: the same file on two active nodes is one requirement listing both node types", () => {
  const model = { name: "shared.safetensors", url: "https://huggingface.co/example-org/w/resolve/main/shared.safetensors", directory: "loras" };
  const wf = { nodes: [
    { id: 1, type: "LoraLoader", mode: 0, properties: { models: [model] } },
    { id: 2, type: "LoraLoaderModelOnly", mode: 0, properties: { models: [model] } },
  ] };
  const r = C.requiredModels(wf);
  assert.equal(r.active.length, 1);
  assert.deepEqual(r.active[0].node_types, ["LoraLoader", "LoraLoaderModelOnly"]);
});

test("models: a file needed on an active node and also present on a bypassed one is active only", () => {
  const model = { name: "both.safetensors", url: "https://huggingface.co/example-org/w/resolve/main/both.safetensors", directory: "loras" };
  const wf = { nodes: [
    { id: 1, type: "LoraLoader", mode: 4, properties: { models: [model] } },
    { id: 2, type: "LoraLoader", mode: 0, properties: { models: [model] } },
  ] };
  const r = C.requiredModels(wf);
  assert.deepEqual(names(r.active), ["both.safetensors"]);
  assert.deepEqual(r.inactive, []);
});

test("models (real): a bypassed optional LoRA is not a requirement (wan alpha t2v)", () => {
  const r = C.requiredModels(realWorkflow("video_wan2.1_alpha_t2v_14B"));
  assert.deepEqual(names(r.active), [
    "umt5_xxl_fp8_e4m3fn_scaled.safetensors", "wan2.1_t2v_14B_fp8_scaled.safetensors", "wan_alpha_2.1_rgba_lora.safetensors",
    "wan_alpha_2.1_vae_alpha_channel.safetensors", "wan_alpha_2.1_vae_rgb_channel.safetensors",
  ]);
  assert.deepEqual(names(r.inactive), ["lightx2v_T2V_14B_cfg_step_distill_v2_lora_rank64_bf16.safetensors"]);
});

test("models (real): the three files of a subgraph template sit in three classes (z-image-turbo)", () => {
  const r = C.requiredModels(realWorkflow("image_z_image_turbo"));
  assert.deepEqual(r.active.map((m) => `${m.directory}/${m.name}`).sort(), [
    "diffusion_models/z_image_turbo_bf16.safetensors", "text_encoders/qwen_3_4b.safetensors", "vae/ae.safetensors",
  ]);
});

test("note sizes: `- [file](url) (7.49 GB)` lines in a model note are the author's prose sizes, in binary units", () => {
  const wf = { nodes: [
    { id: 1, type: "MarkdownNote", mode: 0, widgets_values: ["## Model links\n\n- [a.safetensors](https://huggingface.co/x/y/resolve/main/a.safetensors) (7.49 GB)\n- [b.safetensors](https://huggingface.co/x/y/resolve/main/b.safetensors) (512 MB)\n- [c.safetensors](https://huggingface.co/x/y/resolve/main/c.safetensors)\n"] },
    { id: 2, type: "KSampler", mode: 0, widgets_values: ["- [d.safetensors](https://example.com/d.safetensors) (1 GB)"] },
  ] };
  assert.deepEqual(C.parseNoteSizes(wf), { "a.safetensors": Math.round(7.49 * 1024 ** 3), "b.safetensors": 512 * 1024 ** 2 });
  assert.deepEqual(C.parseNoteSizes({}), {});
});

test("models (real): a template with no weights has no requirements", () => {
  const r = C.requiredModels(realWorkflow("utility_interpolation_image_upscale"));
  assert.deepEqual(r, { active: [], inactive: [] });
});

// ---------------------------------------------------------------------------------------------
// API nodes, the API signals and the kind of a template
// ---------------------------------------------------------------------------------------------

test("api ids: only string-literal node_id declarations count", () => {
  const src = [
    'IO.Schema(node_id="AlphaNode", x=1)',
    "IO.Schema(node_id='BetaNode')",
    "node_id : str = Field(..., description='x')",
    'other_node_id = "NotThis"',
    'my_node_id="NorThis"',
  ].join("\n");
  assert.deepEqual([...C.parseApiNodeIds(src)].sort(), ["AlphaNode", "BetaNode"]);
});

test("api ids: the scan walks every .py under the node's own comfy_api_nodes directory", () => {
  const scan = C.scanApiNodeIds(API_DIR, C.nodeIo);
  assert.deepEqual(scan.ids, ["BriaIncreaseResolution", "FixtureSecondNode"]);
  assert.equal(scan.files, 2);
});

test("api ids: a missing directory is an empty scan, not a crash", () => {
  assert.deepEqual(C.scanApiNodeIds("D:/x/nope", memIo({})), { ids: [], files: 0 });
});

test("api signals: each signal is reported on its own and any one of them makes the template API", () => {
  const apiIds = new Set(["BriaIncreaseResolution"]);
  const byId = C.apiSignals({ name: "utility_thing", openSource: undefined, nodeTypes: ["KSampler", "BriaIncreaseResolution"], apiIds });
  assert.deepEqual({ ...byId }, { flag: true, by_node_id: ["BriaIncreaseResolution"], by_name_prefix: false, by_open_source_false: false });
  const byPrefix = C.apiSignals({ name: "api_thing", openSource: undefined, nodeTypes: ["KSampler"], apiIds });
  assert.deepEqual({ ...byPrefix }, { flag: true, by_node_id: [], by_name_prefix: true, by_open_source_false: false });
  const byFlag = C.apiSignals({ name: "thing", openSource: false, nodeTypes: [], apiIds });
  assert.deepEqual({ ...byFlag }, { flag: true, by_node_id: [], by_name_prefix: false, by_open_source_false: true });
  const none = C.apiSignals({ name: "thing", openSource: true, nodeTypes: ["KSampler"], apiIds });
  assert.equal(none.flag, false);
  assert.equal(C.apiSignals({ name: "thing", openSource: undefined, nodeTypes: ["KSampler"], apiIds }).flag, false, "no openSource field is not openSource false");
});

test("api signals: a bypassed API node still marks the template (the paid node is in the graph)", () => {
  const wf = { nodes: [{ id: 1, type: "BriaIncreaseResolution", mode: 4 }] };
  const cat = C.buildCatalog({
    templatesDir: "D:/x/t", apiIds: new Set(["BriaIncreaseResolution"]),
    io: memIo({
      "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [{ name: "byp", title: "B", mediaType: "image" }] }]),
      "D:/x/t/byp.json": JSON.stringify(wf),
    }),
  });
  assert.equal(cat.templates[0].kind, "api");
});

test("kind: api beats custom nodes beats local", () => {
  assert.equal(C.classifyKind({ api: { flag: true }, customPacks: ["p"], requiresCustomNodes: ["p"] }), "api");
  assert.equal(C.classifyKind({ api: { flag: false }, customPacks: ["p"], requiresCustomNodes: [] }), "custom_nodes");
  assert.equal(C.classifyKind({ api: { flag: false }, customPacks: [], requiresCustomNodes: ["p"] }), "custom_nodes");
  assert.equal(C.classifyKind({ api: { flag: false }, customPacks: [], requiresCustomNodes: [] }), "local");
});

// ---------------------------------------------------------------------------------------------
// Parameter surface
// ---------------------------------------------------------------------------------------------

test("param surface: proxyWidgets of every subgraph instance, nested ones included", () => {
  const surface = C.paramSurface(synWorkflow("synthetic_walk_matrix"));
  assert.deepEqual(surface.map((s) => ({ ...s })), [
    { instance: 4, node: "10", widget: "text" },
    { instance: 4, node: "11", widget: "seed" },
    { instance: 13, node: "20", widget: "clip_name" },
  ]);
});

test("param surface (real): z-image-turbo exposes nine widgets, first the prompt text", () => {
  const surface = C.paramSurface(realWorkflow("image_z_image_turbo"));
  assert.equal(surface.length, 9);
  assert.deepEqual({ ...surface[0] }, { instance: 57, node: "27", widget: "text" });
  assert.ok(surface.some((s) => s.widget === "unet_name"));
});

test("param surface: malformed proxyWidgets entries are skipped", () => {
  const id = "a1111111-1111-4111-8111-111111111111";
  const wf = { nodes: [{ id: 1, type: id, properties: { proxyWidgets: [["1", "ok"], "bad", ["only-one"], [3, "numeric-node"], null] } }], definitions: { subgraphs: [{ id, nodes: [] }] } };
  assert.deepEqual(C.paramSurface(wf).map((s) => `${s.node}:${s.widget}`), ["1:ok", "3:numeric-node"]);
  assert.deepEqual(C.paramSurface({ nodes: [{ id: 1, type: id, properties: { proxyWidgets: "nope" } }], definitions: { subgraphs: [{ id, nodes: [] }] } }), []);
  assert.deepEqual(C.paramSurface({}), []);
});

// ---------------------------------------------------------------------------------------------
// The index and the catalog
// ---------------------------------------------------------------------------------------------

test("index: groups flatten to entries that keep their group", () => {
  const flat = C.flattenIndex(loadJson(`${REAL_TEMPLATES}/index.json`));
  assert.equal(flat.length, 8);
  const z = flat.find((e) => e.name === "image_z_image_turbo");
  assert.deepEqual({ ...z.group }, { module: "default", title: "Image", type: "image", category: "Foundation" });
  assert.deepEqual(C.flattenIndex(null), []);
  assert.deepEqual(C.flattenIndex([{ templates: "nope" }, null]), []);
});

const realApiIds = () => new Set(C.scanApiNodeIds(API_DIR, C.nodeIo).ids);
const realCatalog = () => C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), io: C.nodeIo });
const rowOf = (cat, name) => cat.templates.find((r) => r.name === name);

test("catalog (real fixtures): kinds follow the API signals and the custom-node flags", () => {
  const cat = realCatalog();
  assert.equal(cat.templates.length, 8);
  const kinds = Object.fromEntries(cat.templates.map((r) => [r.name, r.kind]));
  assert.deepEqual(kinds, {
    audio_ace_step1_5_xl_turbo: "local",
    hunyuan_video_text_to_video: "local",
    image_netayume_lumina_t2i: "local",
    image_z_image_turbo: "local",
    templates_purz_pixel_sort_image: "custom_nodes",
    utility_bria_increase_resolution: "api",
    utility_interpolation_image_upscale: "local",
    "video_wan2.1_alpha_t2v_14B": "local",
  });
});

test("catalog (real): a paid template that does not say api_ is caught by its node id", () => {
  const row = rowOf(realCatalog(), "utility_bria_increase_resolution");
  assert.equal(row.api.by_node_id[0], "BriaIncreaseResolution");
  assert.equal(row.api.by_name_prefix, false);
  assert.equal(row.api.by_open_source_false, true);
});

test("catalog (real): rows carry the index fields under snake_case names", () => {
  const row = rowOf(realCatalog(), "image_z_image_turbo");
  assert.equal(row.title, "Z-Image-Turbo: Text to Image");
  assert.equal(row.media_type, "image");
  assert.deepEqual(row.tags, ["Image", "Text to Image"]);
  assert.deepEqual(row.model_labels, ["Z-Image-Turbo"]);
  assert.equal(row.size_bytes, 20830591386);
  assert.equal(row.open_source, true);
  assert.equal(row.min_comfyui_version, "0.11.0");
  assert.deepEqual({ ...row.group }, { module: "default", title: "Image", type: "image", category: "Foundation" });
  assert.equal(row.load_error, null);
});

test("catalog (real): graph facts count leaves, subgraphs and model annotations", () => {
  const cat = realCatalog();
  const z = rowOf(cat, "image_z_image_turbo").graph;
  assert.equal(z.ui_version, 0.4);
  assert.equal(z.top_level_nodes, 3);
  assert.equal(z.subgraph_definitions, 1);
  assert.equal(z.subgraph_instances, 1);
  assert.equal(z.leaf_nodes, 11);
  assert.equal(z.active_leaf_nodes, 11);
  assert.equal(z.inactive_leaf_nodes, 0);
  assert.equal(z.model_annotation_entries, 3);
  const wan = rowOf(cat, "video_wan2.1_alpha_t2v_14B").graph;
  assert.equal(wan.inactive_leaf_nodes, 1);
  assert.equal(wan.model_annotation_entries, 6, "the bypassed LoRA's annotation is still an annotation");
});

test("catalog (real): requirements and the class of each file come from the annotated models", () => {
  const cat = realCatalog();
  const ace = rowOf(cat, "audio_ace_step1_5_xl_turbo");
  assert.deepEqual(ace.models.map((m) => `${m.directory}/${m.name}`).sort(), [
    "diffusion_models/acestep_v1.5_xl_turbo_bf16.safetensors", "text_encoders/qwen_0.6b_ace15.safetensors", "text_encoders/qwen_4b_ace15.safetensors", "vae/ace_1.5_vae.safetensors",
  ]);
  assert.deepEqual(rowOf(cat, "utility_interpolation_image_upscale").models, []);
  assert.equal(rowOf(cat, "hunyuan_video_text_to_video").models.find((m) => m.name === "clip_l.safetensors").url,
    "https://huggingface.co/comfyanonymous/flux_text_encoders/resolve/main/clip_l.safetensors?download=true");
});

test("catalog (real): a custom-node template says which pack the index names", () => {
  const row = rowOf(realCatalog(), "templates_purz_pixel_sort_image");
  assert.deepEqual(row.requires_custom_nodes, ["comfyui_fill-nodes"]);
  assert.equal(row.kind, "custom_nodes");
});

test("catalog (synthetic): an active pack node makes a custom-node template, a bypassed one does not", () => {
  const cat = C.buildCatalog({ templatesDir: SYN_TEMPLATES, apiIds: new Set(), io: C.nodeIo });
  const row = rowOf(cat, "synthetic_custom_nodes");
  assert.deepEqual(row.custom_packs, ["example-pack"]);
  assert.equal(row.kind, "custom_nodes");
});

test("catalog (synthetic): each API signal alone is enough, and an entry without a file is a load error", () => {
  const cat = C.buildCatalog({ templatesDir: SYN_TEMPLATES, apiIds: new Set(), io: C.nodeIo });
  assert.equal(rowOf(cat, "api_synthetic_prefix_only").kind, "api");
  assert.equal(rowOf(cat, "api_synthetic_prefix_only").api.by_name_prefix, true);
  assert.equal(rowOf(cat, "synthetic_open_source_false").kind, "api");
  assert.equal(rowOf(cat, "synthetic_open_source_false").api.by_open_source_false, true);
  const missing = rowOf(cat, "synthetic_missing_file");
  assert.match(missing.load_error, /synthetic_missing_file\.json/);
  assert.deepEqual(missing.models, []);
  assert.equal(missing.kind, "local", "with no graph the index alone decides");
});

test("catalog: a workflow file that is not JSON is a load error on its row, not a crash", () => {
  const io = memIo({
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [{ name: "bad", title: "B", mediaType: "image" }, { name: "good", title: "G", mediaType: "image" }] }]),
    "D:/x/t/bad.json": '{ "nodes": [',
    "D:/x/t/good.json": JSON.stringify({ nodes: [{ id: 1, type: "KSampler", mode: 0 }] }),
  });
  const cat = C.buildCatalog({ templatesDir: "D:/x/t", apiIds: new Set(), io });
  assert.match(rowOf(cat, "bad").load_error, /^bad\.json: invalid JSON \(/, "a parse failure, not an unreadable file (that says `cannot read`)");
  assert.equal(rowOf(cat, "good").load_error, null);
});

test("catalog: json files that are neither indexed nor index-type assets are reported as orphans", () => {
  const good = JSON.stringify({ nodes: [] });
  const io = memIo({
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [{ name: "one", title: "One", mediaType: "image" }] }]),
    "D:/x/t/one.json": good,
    "D:/x/t/fuse_options.json": good,
    "D:/x/t/index.mcp.json": "{}",
    "D:/x/t/index.fr.json": "[]",
    "D:/x/t/index.zh-TW.json": "[]",
    "D:/x/t/index.schema.json": "{}",
    "D:/x/t/index_logo.json": "{}",
    "D:/x/t/one-1.webp": { size: 5 },
  });
  const cat = C.buildCatalog({ templatesDir: "D:/x/t", apiIds: new Set(), io });
  assert.deepEqual(cat.orphan_files, ["fuse_options"]);
  assert.equal(cat.templates.length, 1);
});

test("catalog: a missing index is an error the caller sees, not an empty catalog", () => {
  assert.throws(() => C.buildCatalog({ templatesDir: "D:/x/none", apiIds: new Set(), io: memIo({}) }), /index\.json/);
});

// ---------------------------------------------------------------------------------------------
// Counts always carry their basis (critic F-24)
// ---------------------------------------------------------------------------------------------

test("counts (real fixtures): every figure is labelled with the basis it was computed on", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const cat = C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, io: C.nodeIo });
  const s = C.summarizeCatalog(cat);
  assert.equal(s.basis, stamp.label);
  assert.match(s.basis, /installed package/);
  assert.match(s.basis, /json 0\.1\.94/);
  assert.equal(s.entries, 8);
  assert.deepEqual({ ...s.kinds }, { api: 1, custom_nodes: 1, local: 6 });
  assert.deepEqual({ ...s.local }, { total: 6, needing_weights: 5, zero_model: 1 });
  assert.equal(s.subgraph_templates, 2);
  assert.equal(s.param_surface.local, 2);
  assert.equal(s.model_annotations.entries, 18);
  assert.equal(s.model_annotations.hashed, 0);
  assert.deepEqual({ ...s.api_signals }, { by_node_id: 1, by_name_prefix: 0, by_open_source_false: 1, any: 1, disagreements: 0 });
  assert.equal(s.load_errors, 0);
});

test("counts: an unstamped catalog says so instead of borrowing a label", () => {
  const cat = C.buildCatalog({ templatesDir: SYN_TEMPLATES, apiIds: new Set(), io: C.nodeIo });
  assert.match(C.summarizeCatalog(cat).basis, /unstamped/);
});

test("counts: API signals that disagree are counted, not hidden", () => {
  const cat = C.buildCatalog({ templatesDir: SYN_TEMPLATES, apiIds: new Set(), io: C.nodeIo });
  const s = C.summarizeCatalog(cat);
  assert.equal(s.api_signals.by_name_prefix, 1);
  assert.equal(s.api_signals.by_open_source_false, 1);
  assert.equal(s.api_signals.any, 2);
  assert.equal(s.api_signals.disagreements, 2, "each of the two is flagged by one signal only");
});

// ---------------------------------------------------------------------------------------------
// The version stamp
// ---------------------------------------------------------------------------------------------

test("versions: the dist-info directory names of a site-packages directory give the wheel versions", () => {
  assert.deepEqual({ ...C.readPackageVersions(REAL_SITE, C.nodeIo) }, {
    "comfyui-workflow-templates": "0.11.68",
    "comfyui-workflow-templates-core": "0.3.359",
    "comfyui-workflow-templates-json": "0.1.94",
  });
});

test("versions: a wheel extract with only the json dist-info still yields the json version", () => {
  const io = memIo({ "D:/x/ex/comfyui_workflow_templates_json-0.1.96.dist-info/METADATA": "Name: x\n", "D:/x/ex/comfyui_workflow_templates_json/templates/index.json": "[]" });
  assert.deepEqual({ ...C.readPackageVersions("D:/x/ex", io) }, { "comfyui-workflow-templates-json": "0.1.96" });
});

test("versions: the media wheels are versioned too", () => {
  const io = memIo({ "D:/x/sp/comfyui_workflow_templates_media_api-0.3.84.dist-info/METADATA": "", "D:/x/sp/comfyui_workflow_templates_media_assets_01-0.1.47.dist-info/METADATA": "" });
  assert.deepEqual({ ...C.readPackageVersions("D:/x/sp", io) }, { "comfyui-workflow-templates-media-api": "0.3.84", "comfyui-workflow-templates-media-assets-01": "0.1.47" });
});

test("locate: finds the templates of a ComfyUI tree on a Windows or a Linux venv layout", () => {
  const win = memIo({ "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates_json/templates/index.json": "[]" });
  assert.deepEqual({ ...C.locateTemplates("D:/x/comfy", win) }, {
    sitePackages: "D:/x/comfy/.venv/Lib/site-packages",
    templatesDir: "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates_json/templates",
  });
  const lin = memIo({ "/srv/x/comfy/.venv/lib/python3.13/site-packages/comfyui_workflow_templates_json/templates/index.json": "[]" });
  assert.deepEqual({ ...C.locateTemplates("/srv/x/comfy", lin) }, {
    sitePackages: "/srv/x/comfy/.venv/lib/python3.13/site-packages",
    templatesDir: "/srv/x/comfy/.venv/lib/python3.13/site-packages/comfyui_workflow_templates_json/templates",
  });
  const plain = memIo({ "/srv/x/comfy/venv/lib/python3.12/site-packages/comfyui_workflow_templates_json/templates/index.json": "[]" });
  assert.equal(C.locateTemplates("/srv/x/comfy", plain).templatesDir, "/srv/x/comfy/venv/lib/python3.12/site-packages/comfyui_workflow_templates_json/templates");
  assert.equal(C.locateTemplates("/srv/x/comfy", memIo({})), null);
});

test("stamp (installed package): versions from the sibling dist-info, a label, an index hash and the file counts", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  assert.equal(stamp.basis, "installed-package");
  assert.equal(stamp.package_version, "0.1.94");
  assert.equal(stamp.label, "installed package (comfyui-workflow-templates 0.11.68, core 0.3.359, json 0.1.94)");
  assert.match(stamp.index_sha256, /^[0-9a-f]{64}$/);
  assert.equal(stamp.index_entries, 8);
  assert.equal(stamp.template_files, 8);
});

test("stamp: the fingerprint is deterministic and moves with any template file", () => {
  const files = {
    "D:/x/sp/comfyui_workflow_templates_json-0.1.94.dist-info/METADATA": "",
    "D:/x/sp/comfyui_workflow_templates_json/templates/index.json": JSON.stringify([{ templates: [{ name: "a" }] }]),
    "D:/x/sp/comfyui_workflow_templates_json/templates/a.json": '{"nodes":[]}',
  };
  const t = "D:/x/sp/comfyui_workflow_templates_json/templates";
  const one = C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo(files) });
  const two = C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo(files) });
  assert.equal(one.fingerprint, two.fingerprint);
  const changed = C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo({ ...files, "D:/x/sp/comfyui_workflow_templates_json/templates/a.json": '{"nodes":[{"id":1}]}' }) });
  assert.notEqual(one.fingerprint, changed.fingerprint);
});

test("stamp (repo checkout): versions from pyproject files and the checked-out commit", () => {
  const sha = "e7849852c953637088dac404faac5d678268531f";
  const io = memIo({
    "D:/x/repo/pyproject.toml": 'name = "comfyui_workflow_templates"\nversion = "0.11.71"\n',
    "D:/x/repo/packages/json/pyproject.toml": '[project]\nname = "comfyui-workflow-templates-json"\nversion = "0.1.97"\n',
    "D:/x/repo/packages/core/pyproject.toml": '[project]\nname = "comfyui-workflow-templates-core"\nversion = "0.3.362"\n',
    "D:/x/repo/.git/HEAD": "ref: refs/heads/main\n",
    "D:/x/repo/.git/refs/heads/main": sha + "\n",
    "D:/x/repo/templates/index.json": "[]",
  });
  const stamp = C.stampFor({ templatesDir: "D:/x/repo/templates", basis: "repo-checkout", io });
  assert.equal(stamp.basis, "repo-checkout");
  assert.deepEqual({ ...stamp.versions }, { "comfyui-workflow-templates": "0.11.71", "comfyui-workflow-templates-core": "0.3.362", "comfyui-workflow-templates-json": "0.1.97" });
  assert.equal(stamp.git_head, sha);
  assert.equal(stamp.label, "repo checkout at e7849852 (comfyui-workflow-templates 0.11.71, core 0.3.362, json 0.1.97)");
});

test("git head: a detached HEAD, a packed ref and a missing repository are all handled", () => {
  const sha = "0123456789abcdef0123456789abcdef01234567";
  const other = "89abcdef0123456789abcdef0123456789abcdef";
  assert.equal(C.readGitHead("D:/x/repo", memIo({ "D:/x/repo/.git/HEAD": `${sha}\n` })), sha, "detached");
  const packed = memIo({ "D:/x/repo/.git/HEAD": "ref: refs/heads/main\n", "D:/x/repo/.git/packed-refs": `# pack-refs\n${other} refs/heads/other\n${sha} refs/heads/main\n` });
  assert.equal(C.readGitHead("D:/x/repo", packed), sha, "packed");
  assert.equal(C.readGitHead("D:/x/repo", memIo({ "D:/x/repo/.git/HEAD": "ref: refs/heads/gone\n", "D:/x/repo/.git/packed-refs": `${other} refs/heads/other\n` })), null, "ref not in packed-refs");
  assert.equal(C.readGitHead("D:/x/repo", memIo({})), null, "no repository");
  assert.equal(C.readGitHead("D:/x/repo", memIo({ "D:/x/repo/.git/HEAD": "garbage\n" })), null);
});

test("stamp: a directory with no version information is labelled as such", () => {
  const stamp = C.stampFor({ templatesDir: "D:/x/loose", basis: "templates-dir", io: memIo({ "D:/x/loose/index.json": "[]" }) });
  assert.deepEqual({ ...stamp.versions }, {});
  assert.equal(stamp.package_version, null);
  assert.match(stamp.label, /no version information/);
});

test("stamp: a catalog and a node agree only when their json versions are equal", () => {
  const stamp = { package_version: "0.1.94" };
  assert.deepEqual({ ...C.checkStamp(stamp, { "comfyui-workflow-templates-json": "0.1.94" }) }, { ok: true, reason: null });
  const bad = C.checkStamp(stamp, { "comfyui-workflow-templates-json": "0.1.96" });
  assert.equal(bad.ok, false);
  assert.match(bad.reason, /0\.1\.94/);
  assert.match(bad.reason, /0\.1\.96/);
  assert.equal(C.checkStamp(stamp, {}).ok, false, "a node with no installed package cannot vouch for the catalog");
  assert.equal(C.checkStamp({ package_version: null }, { "comfyui-workflow-templates-json": "0.1.94" }).ok, false, "an unstamped catalog cannot be checked");
});

// ---------------------------------------------------------------------------------------------
// Licences, the FLUX bar and the gate
// ---------------------------------------------------------------------------------------------

const FIXTURE_MAP_PATH = `${FX}/license-map.fixture.json`;
const fixtureMap = () => C.loadLicenseMap(loadJson(FIXTURE_MAP_PATH));
const REAL_MAP_PATH = fileURLToPath(new URL("./templates-license-map.json", import.meta.url));
const hf = (repo, file = "x.safetensors") => `https://huggingface.co/${repo}/resolve/main/${file}`;
const mdl = (repo, name = "x.safetensors") => ({ name, directory: "loras", url: hf(repo, name), hash: null, hash_type: null, annotated: true, node_types: ["LoraLoader"] });

test("repo id: taken from a Hugging Face resolve or blob URL, query and host variants included", () => {
  assert.equal(C.repoOfUrl("https://huggingface.co/Comfy-Org/z_image_turbo/resolve/main/split_files/vae/ae.safetensors"), "Comfy-Org/z_image_turbo");
  assert.equal(C.repoOfUrl("https://huggingface.co/comfyanonymous/flux_text_encoders/resolve/main/clip_l.safetensors?download=true"), "comfyanonymous/flux_text_encoders");
  assert.equal(C.repoOfUrl("http://www.huggingface.co/a/b/blob/main/c.safetensors"), "a/b");
  assert.equal(C.repoOfUrl("https://example.com/a/b/resolve/main/c.safetensors"), null);
  assert.equal(C.repoOfUrl("https://huggingface.co/Comfy-Org"), null);
  assert.equal(C.repoOfUrl(null), null);
  assert.equal(C.repoOfUrl(""), null);
});

test("license map: loads by repo id, case-insensitively, and an unlisted repo is simply absent", () => {
  const map = fixtureMap();
  assert.equal(map.get("example-org/weights-a").class, "permissive");
  assert.equal(map.get("EXAMPLE-ORG/Weights-A").class, "permissive");
  assert.equal(map.get("example-org/nope"), undefined);
  assert.equal(map.has("example-org/weights-b"), true);
  assert.equal(map.size, 9);
  assert.equal(C.emptyLicenseMap().size, 0);
  assert.equal(C.emptyLicenseMap().get("a/b"), undefined);
});

test("license map: a malformed map is refused, not half-loaded", () => {
  assert.throws(() => C.loadLicenseMap({ repos: { "a/b": { class: "free-for-all", source_url: "https://x", fetched_on: "2026-01-01", basis: "x" } } }), /class/);
  assert.throws(() => C.loadLicenseMap({ repos: { "a/b": { source_url: "https://x" } } }), /class/);
  assert.throws(() => C.loadLicenseMap({ repos: [] }), /repos/);
  assert.throws(() => C.loadLicenseMap(null), /license map/);
  assert.throws(() => C.loadLicenseMap({ repos: { nonsense: { class: "permissive" } } }), /repo id/);
  assert.throws(() => C.loadLicenseMap({ repos: { "Owner/Name": { class: "permissive" }, "owner/name": { class: "conditional" } } }), /twice/);
});

test("license class: the worst known class wins, and unknown outranks permissive without hiding a known restriction", () => {
  const map = fixtureMap();
  const cls = (...repos) => C.licenseAssessment(repos.map((r) => mdl(r)), map).class;
  assert.equal(cls("example-org/weights-a"), "permissive");
  assert.equal(cls("example-org/weights-a", "example-org/weights-b"), "conditional");
  assert.equal(cls("example-org/weights-b", "example-org/weights-c"), "non_commercial");
  assert.equal(cls("example-org/weights-a", "example-org/not-in-map"), "unknown");
  assert.equal(cls("example-org/weights-b", "example-org/not-in-map"), "conditional");
  assert.equal(cls("example-org/weights-c", "example-org/not-in-map"), "non_commercial");
});

test("license class: no weights is `none`, and a file with no Hugging Face source is unknown", () => {
  const map = fixtureMap();
  assert.equal(C.licenseAssessment([], map).class, "none");
  const noUrl = { ...mdl("example-org/weights-a"), url: null };
  const a = C.licenseAssessment([mdl("example-org/weights-a"), noUrl], map);
  assert.equal(a.class, "unknown");
  assert.equal(a.unresolved_models, 1);
  const elsewhere = C.licenseAssessment([{ ...mdl("example-org/weights-a"), url: "https://example.com/a.safetensors" }], map);
  assert.equal(elsewhere.class, "unknown");
  assert.equal(elsewhere.unresolved_models, 1);
});

test("license class: a repo spelled two ways is one repo, under the spelling seen first", () => {
  const a = C.licenseAssessment([mdl("Example-Org/weights-b", "one.safetensors"), mdl("example-org/weights-b", "two.safetensors")], fixtureMap());
  assert.deepEqual(a.repos.map((r) => r.repo), ["Example-Org/weights-b"]);
});

test("license class: each repo is listed once, with its own class, and unmapped repos are named", () => {
  const a = C.licenseAssessment([mdl("example-org/weights-b", "one.safetensors"), mdl("example-org/weights-b", "two.safetensors"), mdl("example-org/not-in-map"), mdl("EXAMPLE-ORG/weights-a")], fixtureMap());
  assert.deepEqual(a.repos.map((r) => [r.repo, r.class, r.known]), [
    ["EXAMPLE-ORG/weights-a", "permissive", true],
    ["example-org/not-in-map", "unknown", false],
    ["example-org/weights-b", "conditional", true],
  ]);
  assert.deepEqual(a.unmapped_repos, ["example-org/not-in-map"]);
});

test("license class: the strict view treats an inferred entry as unknown and leaves the rest alone", () => {
  const map = fixtureMap();
  const inferred = C.licenseAssessment([mdl("example-org/weights-inferred")], map);
  assert.equal(inferred.class, "conditional");
  assert.equal(inferred.class_strict, "unknown");
  const withNc = C.licenseAssessment([mdl("example-org/weights-inferred"), mdl("example-org/weights-c")], map);
  assert.equal(withNc.class, "non_commercial");
  assert.equal(withNc.class_strict, "non_commercial");
  const plain = C.licenseAssessment([mdl("example-org/weights-b")], map);
  assert.equal(plain.class_strict, "conditional");
  assert.equal(inferred.repos[0].strict_downgraded, true);
});

const fluxOf = (name, repos, files = [], map = fixtureMap()) =>
  C.fluxAssessment({ name, models: [...repos.map((r) => mdl(r)), ...files.map((f) => mdl("example-org/weights-a", f))] }, map);

test("FLUX bar: a FLUX name, a FLUX-family repo or a map flag bars a template, and each says why", () => {
  assert.deepEqual({ ...fluxOf("flux_something", ["example-org/weights-a"]) }, { barred: true, by: ["name"], repos: [], filename_hits: [], text_encoder_repos: [] });
  assert.deepEqual({ ...fluxOf("image_ok", ["black-forest-labs/whatever"]) }, { barred: true, by: ["repo"], repos: ["black-forest-labs/whatever"], filename_hits: [], text_encoder_repos: [] });
  assert.equal(fluxOf("image_ok", ["someone/Flux.2-Turbo-ComfyUI"]).barred, true);
  assert.equal(fluxOf("image_ok", ["example-org/flagged-family"]).barred, true, "the map flag alone is enough");
  assert.deepEqual(fluxOf("flux_something", ["black-forest-labs/whatever"]).by, ["name", "repo"]);
  assert.equal(fluxOf("image_ok", ["example-org/weights-a"]).barred, false);
});

test("FLUX bar: the shared text-encoder repo is exempt by its map entry, and only by it", () => {
  assert.equal(fluxOf("image_ok", ["comfyanonymous/flux_text_encoders"]).barred, false);
  assert.equal(fluxOf("image_ok", ["comfyanonymous/flux_text_encoders"], [], C.emptyLicenseMap()).barred, true, "without the exemption the name pattern bars it");
});

test("FLUX bar: a FLUX-named weights file from another repo is reported, not barred", () => {
  const r = fluxOf("image_ok", ["example-org/weights-a"], ["flux2-vae.safetensors"]);
  assert.equal(r.barred, false);
  assert.deepEqual(r.filename_hits, ["flux2-vae.safetensors"]);
});

const gate = (over) => C.gateFor({ kind: "local", license: { class: "permissive" }, flux: { barred: false, filename_hits: [] }, ...over });

test("gate: paid API and FLUX templates are blocked outright; the reason is named and no acknowledgement helps", () => {
  assert.deepEqual({ ...gate({ kind: "api" }) }, { state: "blocked", blocked: ["api_paid"], ack: [], warnings: [] });
  assert.deepEqual({ ...gate({ flux: { barred: true, filename_hits: [] }, license: { class: "non_commercial" } }) }, { state: "blocked", blocked: ["flux"], ack: [], warnings: [] });
  assert.deepEqual(gate({ kind: "api", flux: { barred: true, filename_hits: [] } }).blocked, ["api_paid", "flux"]);
});

test("gate: non-commercial, conditional and unknown licences need an acknowledgement that names them", () => {
  for (const c of ["non_commercial", "conditional", "unknown"]) {
    assert.deepEqual({ ...gate({ license: { class: c } }) }, { state: "ack_required", blocked: [], ack: [c], warnings: [] });
  }
});

test("gate: permissive licences and templates without weights are open", () => {
  assert.equal(gate({ license: { class: "permissive" } }).state, "open");
  assert.equal(gate({ license: { class: "none" } }).state, "open");
});

test("gate: a FLUX-named file in a template that is not barred is a warning, not a verdict", () => {
  const g = gate({ flux: { barred: false, filename_hits: ["flux2-vae.safetensors"] } });
  assert.equal(g.state, "open");
  assert.deepEqual(g.warnings, ["flux_component_by_filename: flux2-vae.safetensors"]);
});

const licensedCatalog = () => C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), licenseMap: fixtureMap(), io: C.nodeIo });

test("catalog (real): templates whose repos are all mapped permissive are open", () => {
  const cat = licensedCatalog();
  for (const name of ["audio_ace_step1_5_xl_turbo", "image_z_image_turbo", "video_wan2.1_alpha_t2v_14B"]) {
    const row = rowOf(cat, name);
    assert.equal(row.license.class, "permissive", name);
    assert.equal(row.gate.state, "open", name);
    assert.equal(row.flux.barred, false, name);
  }
});

test("catalog (real): a bypassed model's repo does not colour the template (wan alpha)", () => {
  const row = rowOf(licensedCatalog(), "video_wan2.1_alpha_t2v_14B");
  assert.deepEqual(row.license.repos.map((r) => r.repo), ["Comfy-Org/Wan_2.1_ComfyUI_repackaged"]);
});

test("catalog (real): an unmapped repo makes the template unknown and needs an acknowledgement", () => {
  const cat = licensedCatalog();
  const netayume = rowOf(cat, "image_netayume_lumina_t2i");
  assert.equal(netayume.license.class, "unknown");
  assert.deepEqual(netayume.license.unmapped_repos, ["duongve/NetaYume-Lumina-Image-2.0"]);
  assert.deepEqual({ ...netayume.gate }, { state: "ack_required", blocked: [], ack: ["unknown"], warnings: [] });
});

test("catalog (real): the shared text encoder is not a FLUX bar (hunyuan t2v)", () => {
  const row = rowOf(licensedCatalog(), "hunyuan_video_text_to_video");
  assert.equal(row.flux.barred, false);
  assert.equal(row.license.class, "unknown", "its other repo is not in the map");
});

test("catalog (real): a paid template is blocked; a template with no weights has license class none", () => {
  const cat = licensedCatalog();
  assert.deepEqual({ ...rowOf(cat, "utility_bria_increase_resolution").gate }, { state: "blocked", blocked: ["api_paid"], ack: [], warnings: [] });
  const zero = rowOf(cat, "utility_interpolation_image_upscale");
  assert.equal(zero.license.class, "none");
  assert.equal(zero.gate.state, "open");
});

test("catalog (synthetic): a file with no source is unknown, and only active nodes' repos count", () => {
  const cat = C.buildCatalog({ templatesDir: SYN_TEMPLATES, apiIds: new Set(), licenseMap: fixtureMap(), io: C.nodeIo });
  const row = rowOf(cat, "synthetic_walk_matrix");
  assert.deepEqual(row.license.repos.map((r) => r.repo), ["example-org/weights-a"], "the conditional repo sits only on bypassed nodes");
  assert.equal(row.license.unresolved_models, 1, "primitive-file.safetensors has no source");
  assert.equal(row.license.class, "unknown");
});

test("catalog: without a licence map every repo is unknown", () => {
  const row = rowOf(realCatalog(), "image_z_image_turbo");
  assert.equal(row.license.class, "unknown");
  assert.deepEqual(row.license.unmapped_repos, ["Comfy-Org/z_image_turbo"]);
});

test("counts (real fixtures): licence classes, the FLUX bar and the gates are counted on the local templates that need weights", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const s = C.summarizeCatalog(C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, licenseMap: fixtureMap(), io: C.nodeIo }));
  assert.deepEqual({ ...s.license.local_needing_weights }, { permissive: 3, conditional: 0, non_commercial: 0, unknown: 2 });
  assert.deepEqual({ ...s.license.strict }, { permissive: 3, conditional: 0, non_commercial: 0, unknown: 2 });
  assert.deepEqual({ ...s.license.repos }, { distinct: 6, mapped: 4, unmapped: 2 });
  assert.equal(s.license.map.entries, 9);
  assert.deepEqual({ ...s.flux.local_needing_weights }, { by_name: 0, by_repo_only: 0, barred: 0, filename_only: 0 });
  assert.equal(s.flux.barred_all_kinds, 0);
  assert.deepEqual({ ...s.gates }, { blocked: 1, ack_required: 2, open: 5 });
});

test("counts: a template named after FLUX is barred and counted, and the default list hides it", async () => {
  const io = memIo({
    "D:/x/license-map.json": readFileSync(FIXTURE_MAP_PATH, "utf8"),
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [
      { name: "flux_fixture_a", title: "A", mediaType: "image", openSource: true },
      { name: "image_ok", title: "B", mediaType: "image", openSource: true },
    ] }]),
    "D:/x/t/flux_fixture_a.json": JSON.stringify({ nodes: [{ id: 1, type: "UNETLoader", mode: 0, properties: { models: [{ name: "a.safetensors", url: hf("example-org/weights-a", "a.safetensors"), directory: "diffusion_models" }] } }] }),
    "D:/x/t/image_ok.json": JSON.stringify({ nodes: [{ id: 1, type: "UNETLoader", mode: 0, properties: { models: [{ name: "b.safetensors", url: hf("example-org/weights-a", "b.safetensors"), directory: "diffusion_models" }] } }] }),
  });
  const cat = C.buildCatalog({ templatesDir: "D:/x/t", apiIds: new Set(), licenseMap: fixtureMap(), io });
  const s = C.summarizeCatalog(cat);
  assert.deepEqual({ ...s.flux.local_needing_weights }, { by_name: 1, by_repo_only: 0, barred: 1, filename_only: 0 });
  assert.equal(s.gates.blocked, 1);
  const list = await run(["list", "--templates-dir", "D:/x/t", "--license-map", "D:/x/license-map.json", "--json"], io);
  assert.equal(list.code, 0, list.stderr);
  const out = JSON.parse(list.stdout);
  assert.deepEqual(out.templates.map((t) => t.name), ["image_ok"]);
  assert.deepEqual({ ...out.hidden }, { api: 0, flux: 1 });
  assert.equal(out.templates[0].gate, "open");
  const all = JSON.parse((await run(["list", "--templates-dir", "D:/x/t", "--license-map", "D:/x/license-map.json", "--include-hidden", "--json"], io)).stdout);
  assert.deepEqual(all.templates.map((t) => t.name), ["flux_fixture_a", "image_ok"]);
  assert.equal(all.templates[0].gate, "blocked");
});

test("counts: licence classes are counted on local templates only; a paid template's weights are not", () => {
  const model = (repo, file) => ({ name: file, url: hf(repo, file), directory: "diffusion_models" });
  const wf = (m) => JSON.stringify({ nodes: [{ id: 1, type: "UNETLoader", mode: 0, properties: { models: [m] } }] });
  const io = memIo({
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [
      { name: "api_paid_thing", title: "P", mediaType: "image" },
      { name: "local_ok", title: "L", mediaType: "image", openSource: true },
    ] }]),
    "D:/x/t/api_paid_thing.json": wf(model("example-org/weights-c", "c.safetensors")),
    "D:/x/t/local_ok.json": wf(model("example-org/weights-a", "a.safetensors")),
  });
  const s = C.summarizeCatalog(C.buildCatalog({ templatesDir: "D:/x/t", apiIds: new Set(), licenseMap: fixtureMap(), io }));
  assert.deepEqual({ ...s.license.local_needing_weights }, { permissive: 1, conditional: 0, non_commercial: 0, unknown: 0 });
  assert.deepEqual({ ...s.gates }, { blocked: 1, ack_required: 0, open: 1 });
});

test("cli: the committed licence map is the default, and --license-map replaces it", async () => {
  const base = ["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json"];
  const dflt = JSON.parse((await run(base)).stdout);
  assert.deepEqual({ ...dflt.license.local_needing_weights }, { permissive: 3, conditional: 0, non_commercial: 0, unknown: 2 });
  assert.ok(dflt.license.map.entries >= 50);
  const custom = JSON.parse((await run([...base, "--license-map", FIXTURE_MAP_PATH])).stdout);
  assert.equal(custom.license.map.entries, 9);
  const missing = await run([...base, "--license-map", "D:/x/none.json"]);
  assert.equal(missing.code, 2);
  assert.match(missing.stderr, /license map/);
});

test("license map (committed): every entry names its class, evidence, date and, when restricted, the reason", () => {
  const map = C.loadLicenseMap(loadJson(REAL_MAP_PATH));
  assert.ok(map.size >= 50);
  for (const [repo, e] of map.entries()) {
    assert.match(repo, /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/);
    if (!/^inferred/i.test(e.basis ?? "")) {
      assert.match(e.source_url, /^https:\/\//, repo);
      assert.match(e.fetched_on, /^\d{4}-\d{2}-\d{2}$/, repo);
    }
    assert.ok(typeof e.basis === "string" && e.basis.length > 0, repo);
    if (e.class !== "permissive") assert.ok(e.conditions || e.note, `${repo}: a restricted or unknown repo says why`);
    if (e.flux_bar === true) assert.equal(e.class, "non_commercial", `${repo}: a barred FLUX-family repo is declared non-commercial`);
  }
  const lower = [...map.entries()].map(([r]) => r.toLowerCase());
  assert.equal(new Set(lower).size, lower.length, "no repo twice, ignoring case");
});

test("license map (committed): the policy facts the gate stands on", () => {
  const map = C.loadLicenseMap(loadJson(REAL_MAP_PATH));
  assert.equal(map.get("Comfy-Org/Qwen-Image-2.1").class, "non_commercial");
  assert.equal(map.get("Comfy-Org/Wan_2.2_ComfyUI_Repackaged").class, "permissive");
  assert.equal(map.get("Comfy-Org/ace_step_1.5_ComfyUI_files").class, "permissive");
  assert.equal(map.get("Kijai/WanVideo_comfy").class, "unknown");
  const h3 = map.get("Comfy-Org/MiniMax-H3");
  assert.equal(h3.class, "conditional");
  assert.match(h3.conditions, /USD 20M/);
  assert.match(h3.conditions, /territor/i);
  assert.match(map.get("Comfy-Org/ltx-2").conditions, /USD 10M/);
  assert.equal(map.get("comfyanonymous/flux_text_encoders").flux_bar, false);
  for (const inferred of ["Lightricks/LTX-2", "Comfy-Org/ltx-2.3"]) assert.match(map.get(inferred).basis, /^inferred from /, inferred);
});

// ---------------------------------------------------------------------------------------------
// Where ComfyUI looks for a model: extra_model_paths.yaml, the legacy class names, the dual dirs
// ---------------------------------------------------------------------------------------------
//
// The rules under test are those of ComfyUI 0.37.0's folder_paths.py and utils/extra_config.py:
// every model class has default directories under <comfy>/models (text_encoders also reads clip/,
// diffusion_models also reads unet/, controlnet also reads t2i_adapter/); a yaml key is mapped
// unet -> diffusion_models and clip -> text_encoders; a provider's base_path is joined to each
// listed directory, an absolute directory wins, and is_default moves a root to the front.

const YAML_A = [
  "# two providers, as a node with a second model drive would write them",
  "provider_one:",
  "    base_path: D:/x/models/",
  "    checkpoints: checkpoints",
  "    # unet is the legacy alias of diffusion_models",
  "    diffusion_models: |",
  "        diffusion_models",
  "        unet",
  "    clip: clip",
  "    vae: vae   # trailing comment",
  "provider_two:",
  "    base_path: E:/x/overflow/",
  "    is_default: true",
  "    loras: loras",
  "    text_encoders: text_encoders",
  "",
].join("\n");

const dirsOf = (roots, cls) => roots.classes[cls].dirs.map((d) => d.path);

test("yaml: providers, base_path, is_default, block scalars, comments and quotes", () => {
  const providers = C.parseExtraModelPathsYaml(YAML_A.replace(/\n/g, "\r\n"));
  assert.deepEqual(providers.map((p) => [p.name, p.base_path, p.is_default]), [["provider_one", "D:/x/models/", false], ["provider_two", "E:/x/overflow/", true]]);
  const one = Object.fromEntries(providers[0].entries.map((e) => [e.key, e.paths]));
  assert.deepEqual(one, { checkpoints: ["checkpoints"], diffusion_models: ["diffusion_models", "unet"], clip: ["clip"], vae: ["vae"] });
  const quoted = C.parseExtraModelPathsYaml('p:\n  base_path: "D:/q"\n  vae: \'vae\'\n  loras: "lo ras"\n');
  assert.equal(quoted[0].base_path, "D:/q");
  assert.deepEqual(Object.fromEntries(quoted[0].entries.map((e) => [e.key, e.paths])), { vae: ["vae"], loras: ["lo ras"] });
  assert.deepEqual(C.parseExtraModelPathsYaml(""), []);
  assert.deepEqual(C.parseExtraModelPathsYaml("not: a provider\n"), [], "a top-level scalar is not a provider block");
});

test("yaml: a comment-looking line inside a block scalar is not a directory", () => {
  const p = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  vae: |\n    vae\n    # old_vae\n\n    vae2\n  loras: loras\n");
  assert.deepEqual(Object.fromEntries(p[0].entries.map((e) => [e.key, e.paths])), { vae: ["vae", "vae2"], loras: ["loras"] });
});

test("yaml: base_path may come after the entries it applies to", () => {
  const p = C.parseExtraModelPathsYaml("late:\n  vae: vae\n  base_path: D:/late\n");
  assert.equal(p[0].base_path, "D:/late");
  assert.deepEqual(p[0].entries.map((e) => e.key), ["vae"]);
});

test("roots: with no yaml every class has its default directories, including the dual ones and the five output ones main.py adds", () => {
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [] });
  assert.deepEqual(dirsOf(roots, "checkpoints"), ["D:/x/comfy/models/checkpoints", "D:/x/comfy/output/checkpoints"]);
  assert.deepEqual(dirsOf(roots, "diffusion_models"), ["D:/x/comfy/models/unet", "D:/x/comfy/models/diffusion_models", "D:/x/comfy/output/diffusion_models"]);
  assert.deepEqual(dirsOf(roots, "text_encoders"), ["D:/x/comfy/models/text_encoders", "D:/x/comfy/models/clip", "D:/x/comfy/output/clip"]);
  assert.deepEqual(dirsOf(roots, "controlnet"), ["D:/x/comfy/models/controlnet", "D:/x/comfy/models/t2i_adapter"]);
  assert.deepEqual(dirsOf(roots, "vae"), ["D:/x/comfy/models/vae", "D:/x/comfy/output/vae"]);
  assert.deepEqual(dirsOf(roots, "loras"), ["D:/x/comfy/models/loras", "D:/x/comfy/output/loras"]);
  for (const cls of ["clip_vision", "upscale_models", "latent_upscale_models", "model_patches", "audio_encoders", "geometry_estimation", "optical_flow", "detection", "background_removal", "frame_interpolation"]) {
    assert.deepEqual(dirsOf(roots, cls), [`D:/x/comfy/models/${cls}`], cls);
  }
  assert.deepEqual(roots.classes.vae.dirs.map((d) => d.source), ["default", "output"]);
  assert.ok(roots.classes.checkpoints.exts.includes(".safetensors"));
  assert.ok(!roots.classes.diffusion_models.exts.includes(".gguf"), "core loaders do not list .gguf");
});

test("roots: a yaml key is mapped to its class (unet and clip are the legacy names) and adds its directories", () => {
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: YAML_A, dir: "D:/x/comfy" }] });
  assert.equal(roots.classes.clip, undefined, "clip is not a class of its own");
  assert.equal(roots.classes.unet, undefined);
  assert.deepEqual(dirsOf(roots, "text_encoders"), ["E:/x/overflow/text_encoders", "D:/x/comfy/models/text_encoders", "D:/x/comfy/models/clip", "D:/x/models/clip", "D:/x/comfy/output/clip"]);
  assert.deepEqual(dirsOf(roots, "diffusion_models"), ["D:/x/comfy/models/unet", "D:/x/comfy/models/diffusion_models", "D:/x/models/diffusion_models", "D:/x/models/unet", "D:/x/comfy/output/diffusion_models"]);
  assert.deepEqual(dirsOf(roots, "checkpoints"), ["D:/x/comfy/models/checkpoints", "D:/x/models/checkpoints", "D:/x/comfy/output/checkpoints"]);
  assert.equal(roots.classes.vae.dirs[1].source, "yaml:provider_one");
});

test("roots: is_default also moves a directory that is already registered to the front", () => {
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "p:\n  base_path: D:/x/comfy/models\n  is_default: true\n  clip: clip\n", dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "text_encoders"), ["D:/x/comfy/models/clip", "D:/x/comfy/models/text_encoders", "D:/x/comfy/output/clip"]);
});

test("roots: is_default puts a provider's directories first", () => {
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: YAML_A, dir: "D:/x/comfy" }] });
  assert.equal(dirsOf(roots, "loras")[0], "E:/x/overflow/loras");
  assert.equal(dirsOf(roots, "loras")[1], "D:/x/comfy/models/loras");
});

test("roots: a directory registered twice stays once, and a yaml-only class accepts any file", () => {
  const twice = "p:\n  base_path: D:/x/comfy/models\n  vae: vae\n  unet: unet\n  diffusion_models: diffusion_models\n";
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: twice, dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "vae"), ["D:/x/comfy/models/vae", "D:/x/comfy/output/vae"]);
  assert.deepEqual(dirsOf(roots, "diffusion_models"), ["D:/x/comfy/models/unet", "D:/x/comfy/models/diffusion_models", "D:/x/comfy/output/diffusion_models"]);
  const custom = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "p:\n  base_path: D:/x/m\n  brand_new_class: brand_new\n", dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(custom, "brand_new_class"), ["D:/x/m/brand_new"]);
  assert.equal(custom.classes.brand_new_class.exts, null);
});

test("roots: relative paths resolve against the yaml's directory, an absolute entry beats base_path, ~ and variables expand in base_path", () => {
  const rel = C.resolveModelRoots({ comfyDir: "/srv/comfy", yamls: [{ text: "p:\n  base_path: ../shared\n  vae: vae\n  loras: /abs/loras\nq:\n  checkpoints: local_ckpts\n", dir: "/srv/comfy" }] });
  assert.deepEqual(dirsOf(rel, "vae"), ["/srv/comfy/models/vae", "/srv/shared/vae", "/srv/comfy/output/vae"]);
  assert.deepEqual(dirsOf(rel, "loras"), ["/srv/comfy/models/loras", "/abs/loras", "/srv/comfy/output/loras"]);
  assert.deepEqual(dirsOf(rel, "checkpoints"), ["/srv/comfy/models/checkpoints", "/srv/comfy/local_ckpts", "/srv/comfy/output/checkpoints"]);
  const expanded = C.resolveModelRoots({
    comfyDir: "/srv/comfy", env: { DISK: "/mnt/big", Q: "qq" }, home: "/srv/fixture-home",
    yamls: [{ text: "p:\n  base_path: ~/models\n  vae: vae\nq:\n  base_path: $DISK/${Q}/models\n  loras: loras\nr:\n  base_path: %DISK%/w\n  clip_vision: cv\n", dir: "/srv/comfy" }],
  });
  assert.equal(dirsOf(expanded, "vae")[1], "/srv/fixture-home/models/vae");
  assert.equal(dirsOf(expanded, "loras")[1], "/mnt/big/qq/models/loras");
  assert.equal(dirsOf(expanded, "clip_vision")[1], "/mnt/big/w/cv");
});

test("roots: a models directory flag moves every default, and later yaml files add after earlier ones", () => {
  const moved = C.resolveModelRoots({ comfyDir: "D:/x/comfy", modelsDir: "D:/x/elsewhere", yamls: [] });
  assert.deepEqual(dirsOf(moved, "vae"), ["D:/x/elsewhere/vae", "D:/x/comfy/output/vae"], "the output directory does not move with the models directory");
  const two = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "a:\n  base_path: D:/a\n  vae: vae\n", dir: "D:/x/comfy" }, { text: "b:\n  base_path: D:/b\n  vae: vae\n", dir: "D:/x" }] });
  assert.deepEqual(dirsOf(two, "vae"), ["D:/x/comfy/models/vae", "D:/a/vae", "D:/b/vae", "D:/x/comfy/output/vae"]);
});

test("roots: every directory the satisfier's own yaml reader finds is found here under the class it maps to", async () => {
  const { parseExtraModelPaths } = await import("./manifest-satisfy.mjs");
  const legacy = { unet: "diffusion_models", clip: "text_encoders" };
  // is_default is a flag the satisfier's reader takes for a category; leave it out of the shared text.
  const shared = YAML_A.replace("    is_default: true\n", "");
  const theirs = parseExtraModelPaths(shared);
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: shared, dir: "D:/x/comfy" }] });
  for (const [category, dirs] of Object.entries(theirs)) {
    for (const dir of dirs) assert.ok(dirsOf(roots, legacy[category] ?? category).includes(dir), `${category}: ${dir}`);
  }
});

// A mini ComfyUI models tree, in memory.
const TREE = {
  "D:/x/comfy/models/checkpoints/a.safetensors": { size: 10 },
  "D:/x/comfy/models/checkpoints/sub/b.safetensors": { size: 11 },
  "D:/x/comfy/models/checkpoints/.git/hidden.safetensors": { size: 1 },
  "D:/x/comfy/models/unet/u1.safetensors": { size: 20 },
  "D:/x/comfy/models/unet/g.gguf": { size: 21 },
  "D:/x/comfy/models/diffusion_models/u2.safetensors": { size: 22 },
  "D:/x/comfy/models/clip/te.safetensors": { size: 30 },
  "D:/x/comfy/models/text_encoders/te2.safetensors": { size: 31 },
  "D:/x/comfy/models/vae/ae.safetensors": { size: 40 },
  "D:/x/comfy/models/vae/notes.txt": { size: 1 },
};
const invFrom = (files = TREE, yamls = []) => C.scanModelInventory(C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls }), memIo(files));
const relsOf = (inv, cls) => inv.classes[cls].files.map((f) => f.rel);

test("inventory: a class lists the union of its directories, recursively, without .git and without unlisted extensions", () => {
  const inv = invFrom();
  assert.deepEqual(relsOf(inv, "checkpoints"), ["a.safetensors", "sub/b.safetensors"]);
  assert.deepEqual(relsOf(inv, "diffusion_models"), ["u1.safetensors", "u2.safetensors"]);
  assert.deepEqual(relsOf(inv, "text_encoders"), ["te.safetensors", "te2.safetensors"]);
  assert.deepEqual(relsOf(inv, "vae"), ["ae.safetensors"]);
  assert.equal(inv.classes.diffusion_models.unlisted, 1, "g.gguf is on disk but a core loader would not offer it");
  assert.equal(inv.classes.vae.unlisted, 1, "notes.txt");
});

test("inventory: a file records its size and every directory that holds it; registered directories that do not exist are named", () => {
  const files = { ...TREE, "E:/x/overflow/vae/ae.safetensors": { size: 40 } };
  const inv = invFrom(files, [{ text: "p:\n  base_path: E:/x/overflow\n  vae: vae\n  loras: loras\n", dir: "D:/x/comfy" }]);
  const ae = inv.classes.vae.files.find((f) => f.rel === "ae.safetensors");
  assert.equal(ae.size, 40);
  assert.deepEqual(ae.dirs, ["D:/x/comfy/models/vae", "E:/x/overflow/vae"]);
  assert.equal(inv.classes.vae.files.length, 1, "one file, however many roots show it");
  assert.ok(inv.missing_dirs.some((d) => d.class === "loras" && d.path === "E:/x/overflow/loras"));
  assert.ok(inv.missing_dirs.every((d) => !d.path.endsWith("models/checkpoints")), "an existing directory is not missing");
});

test("inventory (real filesystem): links are followed, a loop back into an ancestor is not, and nothing hangs", async (t) => {
  const { mkdtempSync, mkdirSync, writeFileSync, symlinkSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const root = mkdtempSync(`${tmpdir().replace(/\\/g, "/")}/tc-links-`).replace(/\\/g, "/");
  try {
    mkdirSync(`${root}/comfy/models/vae`, { recursive: true });
    mkdirSync(`${root}/real/loras`, { recursive: true });
    writeFileSync(`${root}/comfy/models/vae/ae.safetensors`, "x");
    writeFileSync(`${root}/real/loras/l.safetensors`, "x");
    mkdirSync(`${root}/real/ckpts`, { recursive: true });
    mkdirSync(`${root}/comfy/models/checkpoints`, { recursive: true });
    writeFileSync(`${root}/real/ckpts/c.safetensors`, "x");
    try {
      symlinkSync(`${root}/real/ckpts`, `${root}/comfy/models/checkpoints/linked`, "junction");
      symlinkSync(`${root}/real/loras`, `${root}/comfy/models/loras`, "junction");
      symlinkSync(`${root}/comfy/models`, `${root}/comfy/models/vae/loop`, "junction");
    } catch (e) {
      return t.skip("no permission to create links here"); // a pass here would prove nothing, so say so
    }
    const inv = C.scanModelInventory(C.resolveModelRoots({ comfyDir: `${root}/comfy`, yamls: [] }), C.nodeIo);
    assert.deepEqual(relsOf(inv, "loras"), ["l.safetensors"], "the junction is followed");
    assert.deepEqual(relsOf(inv, "checkpoints"), ["linked/c.safetensors"], "a link nested inside a class directory is followed too");
    assert.ok(relsOf(inv, "vae").includes("ae.safetensors"));
    assert.ok(!relsOf(inv, "vae").some((r) => r.includes("loop/vae/")), "the loop is cut at the ancestor");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

const req = (directory, name, extra = {}) => ({ name, directory, url: null, hash: null, hash_type: null, annotated: true, node_types: ["X"], ...extra });
const statusOf = (r) => r.requirements.map((x) => x.status);

test("readiness: a file in a class directory is present, in the aliased directories too (no yaml at all)", () => {
  const inv = invFrom();
  const r = C.readiness([req("checkpoints", "a.safetensors"), req("diffusion_models", "u1.safetensors"), req("diffusion_models", "u2.safetensors"), req("text_encoders", "te.safetensors"), req("vae", "ae.safetensors")], inv);
  assert.deepEqual(statusOf(r), ["present", "present", "present", "present", "present"]);
  assert.equal(r.ready, true);
  assert.equal(r.not_ready_count, 0);
});

test("readiness: the legacy class names in a template resolve to the class the loader reads", () => {
  const inv = invFrom();
  assert.deepEqual(statusOf(C.readiness([req("unet", "u1.safetensors"), req("clip", "te.safetensors")], inv)), ["present", "present"]);
});

test("readiness: a file that exists but sits in the wrong class is not ready, and says where it is", () => {
  const inv = invFrom();
  const r = C.readiness([req("vae", "te.safetensors")], inv);
  assert.equal(r.ready, false);
  assert.deepEqual(r.requirements[0].status, "wrong_class");
  assert.deepEqual(r.requirements[0].found_in, [{ class: "text_encoders", rel: "te.safetensors" }]);
});

test("readiness: a file under a subfolder, or with another case, is not the name the loader would offer", () => {
  const inv = invFrom({ ...TREE, "D:/x/comfy/models/loras/Case.safetensors": { size: 5 } });
  const sub = C.readiness([req("checkpoints", "b.safetensors")], inv).requirements[0];
  assert.equal(sub.status, "in_subfolder");
  assert.deepEqual(sub.found, ["sub/b.safetensors"]);
  const cased = C.readiness([req("loras", "case.safetensors")], inv).requirements[0];
  assert.equal(cased.status, "case_mismatch");
  assert.deepEqual(cased.found, ["Case.safetensors"]);
});

test("readiness: an extension the class's core loader does not list is not ready even when the file is on disk", () => {
  const inv = invFrom();
  const r = C.readiness([req("diffusion_models", "g.gguf")], inv).requirements[0];
  assert.equal(r.status, "extension_not_listed");
});

test("readiness: a class nothing registers cannot be satisfied, and a file nobody has is missing", () => {
  const inv = invFrom();
  assert.equal(C.readiness([req("not_a_class", "x.safetensors")], inv).requirements[0].status, "class_unregistered");
  assert.equal(C.readiness([req("vae", "nope.safetensors")], inv).requirements[0].status, "missing");
});

test("readiness: a requirement with no class (an unannotated widget file) is met by the name in any class, and says so", () => {
  const inv = invFrom();
  const ok = C.readiness([req(null, "ae.safetensors", { annotated: false })], inv);
  assert.equal(ok.requirements[0].status, "present_class_unknown");
  assert.equal(ok.ready, true);
  assert.equal(C.readiness([req(null, "nope.safetensors", { annotated: false })], inv).ready, false);
});

test("readiness: ready means every requirement is met, and a template with none is ready", () => {
  const inv = invFrom();
  const mixed = C.readiness([req("vae", "ae.safetensors"), req("vae", "nope.safetensors"), req("loras", "nope2.safetensors")], inv);
  assert.equal(mixed.ready, false);
  assert.equal(mixed.not_ready_count, 2);
  assert.deepEqual(C.readiness([], inv), { mode: "directory-aware", ready: true, requirements: [], not_ready_count: 0 });
});

test("readiness (basename-only): the research's method, for reproduction: any class, any folder, any case", () => {
  const inv = invFrom({ ...TREE, "D:/x/comfy/models/loras/Case.safetensors": { size: 5 } });
  const r = C.readiness([req("vae", "te.safetensors"), req("checkpoints", "b.safetensors"), req("loras", "case.safetensors"), req("vae", "nope.safetensors")], inv, { mode: "basename-only" });
  assert.deepEqual(statusOf(r), ["present", "present", "present", "missing"]);
  assert.equal(C.readiness([req("loras", "CASE.safetensors")], inv, { mode: "basename-only" }).ready, true, "either side may carry the capitals");
  assert.equal(r.mode, "basename-only");
  assert.equal(r.ready, false);
});

test("readiness: a node whose yaml registers only the legacy key still satisfies a template that names the class", () => {
  const files = { "D:/x/only-unet/u.safetensors": { size: 1 } };
  const inv = C.scanModelInventory(C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "p:\n  base_path: D:/x\n  unet: only-unet\n", dir: "D:/x/comfy" }] }), memIo(files));
  assert.equal(C.readiness([req("diffusion_models", "u.safetensors")], inv).ready, true);
});

const ACE_TREE = {
  "D:/x/comfy/models/diffusion_models/acestep_v1.5_xl_turbo_bf16.safetensors": { size: 1 },
  "D:/x/comfy/models/vae/ace_1.5_vae.safetensors": { size: 1 },
  "D:/x/comfy/models/text_encoders/qwen_0.6b_ace15.safetensors": { size: 1 },
  "D:/x/comfy/models/text_encoders/qwen_4b_ace15.safetensors": { size: 1 },
};

test("catalog readiness: only local templates are evaluated; zero-model ones are ready, others follow their files", () => {
  const cat = licensedCatalog();
  const res = C.catalogReadiness(cat, invFrom(ACE_TREE));
  assert.deepEqual(Object.keys(res).sort(), ["audio_ace_step1_5_xl_turbo", "hunyuan_video_text_to_video", "image_netayume_lumina_t2i", "image_z_image_turbo", "utility_interpolation_image_upscale", "video_wan2.1_alpha_t2v_14B"]);
  assert.equal(res.audio_ace_step1_5_xl_turbo.ready, true);
  assert.equal(res.utility_interpolation_image_upscale.ready, true);
  assert.equal(res.image_z_image_turbo.ready, false);
  assert.equal(res.image_z_image_turbo.not_ready_count, 3);
});

test("catalog readiness: the summary counts what is ready, by gate, with a histogram of what is missing", () => {
  const cat = licensedCatalog();
  const s = C.summarizeReadiness(cat, C.catalogReadiness(cat, invFrom(ACE_TREE)));
  assert.deepEqual({ local: s.local, zero_model: s.zero_model, needing_weights: s.needing_weights }, { local: 6, zero_model: 1, needing_weights: 5 });
  assert.deepEqual({ ...s.ready }, { total: 2, zero_model: 1, with_weights: 1, names: ["audio_ace_step1_5_xl_turbo"], by_gate: { open: 1, ack_required: 0, blocked: 0 } });
  assert.deepEqual({ ...s.histogram }, { "0": 1, "1": 1, "2": 0, "3-5": 3, "6+": 0 });
});

const nodeSnapshot = (files, versions = { "comfyui-workflow-templates-json": "0.1.94" }) => ({
  package: { versions },
  inventory: invFrom(files),
});

test("node readiness: refused unless the node carries the package the catalog was built from", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const cat = C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, licenseMap: fixtureMap(), io: C.nodeIo });
  const ok = C.nodeReadiness({ catalog: cat, snapshot: nodeSnapshot(ACE_TREE) });
  assert.equal(ok.refused, undefined);
  assert.equal(ok.counts.ready.with_weights, 1);
  const bad = C.nodeReadiness({ catalog: cat, snapshot: nodeSnapshot(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }) });
  assert.match(bad.refused, /0\.1\.94/);
  assert.equal(bad.counts, undefined);
  const candidate = C.nodeReadiness({ catalog: cat, snapshot: nodeSnapshot(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }), candidate: true });
  assert.equal(candidate.refused, undefined);
  assert.equal(candidate.candidate, true);
  assert.equal(candidate.stamp_check.ok, false);
});

test("readiness table: per node counts, the union over nodes, and the two methods side by side", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const cat = C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, licenseMap: fixtureMap(), io: C.nodeIo });
  const zTree = {
    "D:/x/comfy/models/text_encoders/qwen_3_4b.safetensors": { size: 1 },
    "D:/x/comfy/models/diffusion_models/z_image_turbo_bf16.safetensors": { size: 1 },
    "D:/x/comfy/models/vae_wrong_class/ae.safetensors": { size: 1 },
    "D:/x/comfy/models/checkpoints/ae.safetensors": { size: 1 },
  };
  const table = C.readinessTable({ catalog: cat, snapshots: { alpha: nodeSnapshot(ACE_TREE), beta: nodeSnapshot(zTree) } });
  assert.equal(table.nodes.alpha.counts.ready.with_weights, 1);
  assert.equal(table.nodes.beta.counts.ready.with_weights, 0, "the VAE sits in checkpoints/, not vae/");
  assert.deepEqual(table.any_node.names, ["audio_ace_step1_5_xl_turbo"]);
  assert.deepEqual({ ...table.any_node.by_gate }, { open: 1, ack_required: 0, blocked: 0 });
  // Nearest node, over the five templates that need weights: ACE is ready on alpha (0 short), z-image
  // is 1 short on beta (its VAE is in the wrong class), netayume 1 short, hunyuan 4 short, wan 5 short.
  assert.deepEqual({ ...table.any_node.nearest_histogram }, { "0": 1, "1": 2, "2": 0, "3-5": 2, "6+": 0 });
  const both = C.readinessTable({ catalog: cat, snapshots: { beta: nodeSnapshot(zTree) }, mode: "basename-only" });
  assert.equal(both.nodes.beta.counts.ready.with_weights, 1, "by basename the same node looks ready for z-image-turbo");
  assert.deepEqual(both.nodes.beta.counts.ready.names, ["image_z_image_turbo"]);
  assert.equal(both.mode, "basename-only");
});

// A candidate package: the fixture templates copied to a temp directory, with the ACE VAE renamed,
// one template added and one removed.
async function candidateDir() {
  const { mkdtempSync, cpSync, readFileSync: rf, writeFileSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const root = mkdtempSync(`${tmpdir().replace(/\\/g, "/")}/tc-cand-`).replace(/\\/g, "/");
  const dir = `${root}/comfyui_workflow_templates_json/templates`;
  cpSync(REAL_TEMPLATES, dir, { recursive: true });
  const ace = JSON.parse(rf(`${dir}/audio_ace_step1_5_xl_turbo.json`, "utf8"));
  // A real release renames the annotation and the loader's widget together.
  for (const n of ace.nodes) {
    for (const m of n.properties?.models ?? []) if (m.name === "ace_1.5_vae.safetensors") m.name = "ace_1.5_vae_v2.safetensors";
    if (Array.isArray(n.widgets_values)) n.widgets_values = n.widgets_values.map((v) => (v === "ace_1.5_vae.safetensors" ? "ace_1.5_vae_v2.safetensors" : v));
  }
  writeFileSync(`${dir}/audio_ace_step1_5_xl_turbo.json`, JSON.stringify(ace));
  writeFileSync(`${dir}/synthetic_added.json`, JSON.stringify({ nodes: [{ id: 1, type: "KSampler", mode: 0 }] }));
  const idx = JSON.parse(rf(`${dir}/index.json`, "utf8"));
  for (const g of idx) g.templates = g.templates.filter((t) => t.name !== "utility_interpolation_image_upscale");
  idx[0].templates.push({ name: "synthetic_added", title: "Added", mediaType: "image", openSource: true });
  writeFileSync(`${dir}/index.json`, JSON.stringify(idx));
  return { root, dir, cleanup: () => rmSync(root, { recursive: true, force: true }) };
}

test("diff: what a candidate package adds, removes and changes, and which ready templates it would break on a node", async () => {
  const cand = await candidateDir();
  try {
    const installed = licensedCatalog();
    const next = C.buildCatalog({ templatesDir: cand.dir, apiIds: realApiIds(), licenseMap: fixtureMap(), io: C.nodeIo });
    const d = C.diffCatalogs(installed, next, { snapshots: { alpha: nodeSnapshot(ACE_TREE) } });
    assert.deepEqual(d.added, ["synthetic_added"]);
    assert.deepEqual(d.removed, ["utility_interpolation_image_upscale"]);
    assert.deepEqual(d.changed.map((c) => c.name), ["audio_ace_step1_5_xl_turbo"]);
    assert.deepEqual(d.changed[0].added, ["vae/ace_1.5_vae_v2.safetensors"]);
    assert.deepEqual(d.changed[0].removed, ["vae/ace_1.5_vae.safetensors"]);
    assert.deepEqual(d.readiness.alpha.regressions.map((r) => r.name), ["audio_ace_step1_5_xl_turbo"]);
    assert.deepEqual(d.readiness.alpha.regressions[0].unmet, [{ name: "ace_1.5_vae_v2.safetensors", directory: "vae", status: "missing" }]);
    assert.deepEqual(d.readiness.alpha.gains, []);
    assert.match(d.basis.from, /unstamped/);
  } finally {
    cand.cleanup();
  }
});

test("diff: a template a candidate makes ready on a node is a gain", async () => {
  const cand = await candidateDir();
  try {
    const { "D:/x/comfy/models/vae/ace_1.5_vae.safetensors": _dropped, ...rest } = ACE_TREE;
    const files = { ...rest, "D:/x/comfy/models/vae/ace_1.5_vae_v2.safetensors": { size: 1 } };
    const d = C.diffCatalogs(licensedCatalog(), C.buildCatalog({ templatesDir: cand.dir, apiIds: realApiIds(), licenseMap: fixtureMap(), io: C.nodeIo }), { snapshots: { alpha: nodeSnapshot(files) } });
    assert.deepEqual(d.readiness.alpha.gains, ["audio_ace_step1_5_xl_turbo"]);
    assert.deepEqual(d.readiness.alpha.regressions, []);
  } finally {
    cand.cleanup();
  }
});

test("diff: identical catalogs differ in nothing", () => {
  const cat = licensedCatalog();
  const d = C.diffCatalogs(cat, cat, { snapshots: { alpha: nodeSnapshot(ACE_TREE) } });
  assert.deepEqual([d.added, d.removed, d.changed, d.readiness.alpha.regressions, d.readiness.alpha.gains], [[], [], [], [], []]);
});

// A ComfyUI tree in memory, as a node would have it.
function comfyTree(extra = {}) {
  return {
    "D:/x/comfy/comfyui_version.py": '# generated\n__version__ = "0.37.0"\n',
    "D:/x/comfy/folder_paths.py": "# stand-in for folder_paths\n",
    "D:/x/comfy/utils/extra_config.py": "# stand-in for extra_config\n",
    "D:/x/comfy/extra_model_paths.yaml": "p:\n  base_path: E:/x/overflow\n  loras: loras\n",
    "D:/x/comfy/.offload-launch.json": JSON.stringify({ args: ["main.py", "--disable-smart-memory", "--reserve-vram", "1.0"] }),
    "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates-0.11.68.dist-info/METADATA": "",
    "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates_json-0.1.94.dist-info/METADATA": "",
    "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates_json/templates/index.json": "[]",
    "D:/x/comfy/comfy_api_nodes/nodes_x.py": 'IO.Schema(node_id="PaidNodeOne")\nIO.Schema(node_id="PaidNodeTwo")\n',
    "D:/x/comfy/models/vae/ae.safetensors": { size: 7 },
    "E:/x/overflow/loras/l.safetensors": { size: 8 },
    ...extra,
  };
}

test("snapshot: one node's versions, API ids, resolved model roots and model files, as plain JSON", () => {
  const io = memIo(comfyTree());
  const snap = C.snapshotNode({ comfyDir: "D:/x/comfy", io });
  assert.equal(snap.schema_version, 1);
  assert.equal(snap.comfy_dir, "D:/x/comfy");
  assert.equal(snap.comfyui_version, "0.37.0");
  assert.deepEqual({ ...snap.package.versions }, { "comfyui-workflow-templates": "0.11.68", "comfyui-workflow-templates-json": "0.1.94" });
  assert.deepEqual(snap.api_nodes.ids, ["PaidNodeOne", "PaidNodeTwo"]);
  assert.equal(snap.comfy_files.folder_paths_sha256, io.sha256("D:/x/comfy/folder_paths.py"));
  assert.equal(snap.comfy_files.extra_config_sha256, io.sha256("D:/x/comfy/utils/extra_config.py"));
  assert.notEqual(snap.comfy_files.folder_paths_sha256, snap.comfy_files.extra_config_sha256);
  assert.deepEqual(snap.launch.args, ["main.py", "--disable-smart-memory", "--reserve-vram", "1.0"]);
  assert.deepEqual(snap.yaml.map((y) => y.path), ["D:/x/comfy/extra_model_paths.yaml"]);
  assert.deepEqual(snap.model_roots.classes.loras.dirs.map((d) => d.path), ["D:/x/comfy/models/loras", "E:/x/overflow/loras", "D:/x/comfy/output/loras"]);
  assert.deepEqual(snap.inventory.classes.vae.files.map((f) => [f.rel, f.size]), [["ae.safetensors", 7]]);
  assert.deepEqual(snap.inventory.classes.loras.files.map((f) => f.rel), ["l.safetensors"]);
  assert.deepEqual(JSON.parse(JSON.stringify(snap)), snap, "a snapshot is plain data");
  assert.deepEqual(io.writes, [], "taking a snapshot writes nothing");
});

test("snapshot: the ComfyUI rule files hash the same on a CRLF and an LF checkout", () => {
  const lf = C.snapshotNode({ comfyDir: "D:/x/comfy", io: memIo(comfyTree({ "D:/x/comfy/folder_paths.py": "line one\nline two\n", "D:/x/comfy/utils/extra_config.py": "a\nb\n" })) });
  const crlf = C.snapshotNode({ comfyDir: "D:/x/comfy", io: memIo(comfyTree({ "D:/x/comfy/folder_paths.py": "line one\r\nline two\r\n", "D:/x/comfy/utils/extra_config.py": "a\r\nb\r\n" })) });
  const other = C.snapshotNode({ comfyDir: "D:/x/comfy", io: memIo(comfyTree({ "D:/x/comfy/folder_paths.py": "line one\nline three\n", "D:/x/comfy/utils/extra_config.py": "a\nb\n" })) });
  assert.deepEqual(lf.comfy_files, crlf.comfy_files);
  assert.notEqual(lf.comfy_files.folder_paths_sha256, other.comfy_files.folder_paths_sha256);
  assert.equal(lf.comfy_files.extra_config_sha256, other.comfy_files.extra_config_sha256);
});

test("snapshot: launch flags that move the models or add yaml files are honoured", () => {
  const io = memIo(comfyTree({
    "D:/x/comfy/.offload-launch.json": JSON.stringify({ args: ["main.py", "--models-directory", "D:/x/moved", "--extra-model-paths-config", "D:/x/more.yaml", "D:/x/more2.yaml", "--port", "8188"] }),
    "D:/x/more.yaml": "m:\n  base_path: D:/x/more\n  vae: vae\n",
    "D:/x/moved/vae/v.safetensors": { size: 3 },
  }));
  const snap = C.snapshotNode({ comfyDir: "D:/x/comfy", io });
  assert.deepEqual(snap.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/moved/vae", "D:/x/more/vae", "D:/x/comfy/output/vae"]);
  assert.deepEqual(snap.yaml.map((y) => y.path), ["D:/x/comfy/extra_model_paths.yaml", "D:/x/more.yaml", "D:/x/more2.yaml"]);
  assert.equal(snap.yaml[2].readable, false, "a yaml file that cannot be read is listed, not fatal");
  assert.equal(snap.launch.models_directory, "D:/x/moved");
});

test("snapshot: a tree with no templates package or no yaml still snapshots", () => {
  const io = memIo({ "D:/x/comfy/models/vae/ae.safetensors": { size: 7 } });
  const snap = C.snapshotNode({ comfyDir: "D:/x/comfy", io });
  assert.equal(snap.package, null);
  assert.equal(snap.comfyui_version, null);
  assert.equal(snap.launch, null);
  assert.deepEqual(snap.yaml, []);
  assert.deepEqual(snap.api_nodes.ids, []);
  assert.equal(snap.inventory.classes.vae.files.length, 1);
});

// Real temp files for the verbs that read snapshot files.
async function tempFiles(files) {
  const { mkdtempSync, mkdirSync, writeFileSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const root = mkdtempSync(`${tmpdir().replace(/\\/g, "/")}/tc-cli-`).replace(/\\/g, "/");
  for (const [rel, content] of Object.entries(files)) {
    const p = `${root}/${rel}`;
    mkdirSync(p.slice(0, p.lastIndexOf("/")), { recursive: true });
    writeFileSync(p, typeof content === "string" ? content : "");
  }
  return { root, cleanup: () => rmSync(root, { recursive: true, force: true }) };
}

test("cli: snapshot prints one node's snapshot as JSON from a real tree and writes nothing", async () => {
  const t = await tempFiles({
    "comfy/comfyui_version.py": '__version__ = "0.37.0"\n',
    "comfy/models/vae/ae.safetensors": "x",
    "comfy/models/text_encoders/te.safetensors": "x",
    "comfy/comfy_api_nodes/nodes_x.py": 'IO.Schema(node_id="PaidOne")\n',
  });
  try {
    const r = await run(["snapshot", "--comfy-dir", `${t.root}/comfy`]);
    assert.equal(r.code, 0, r.stderr);
    const snap = JSON.parse(r.stdout);
    assert.equal(snap.comfyui_version, "0.37.0");
    assert.deepEqual(snap.inventory.classes.vae.files.map((f) => f.rel), ["ae.safetensors"]);
    assert.deepEqual(snap.api_nodes.ids, ["PaidOne"]);
    assert.equal((await run(["snapshot"])).code, 2);
  } finally {
    t.cleanup();
  }
});

const snapshotFile = (files, versions) => JSON.stringify({ package: { versions }, inventory: invFrom(files) });
const V094 = { "comfyui-workflow-templates-json": "0.1.94" };

test("cli: readiness counts what each node could run, by the directory-aware method, and refuses a package mismatch", async () => {
  const wrongClass = { "D:/x/comfy/models/checkpoints/ae.safetensors": { size: 1 }, "D:/x/comfy/models/text_encoders/qwen_3_4b.safetensors": { size: 1 }, "D:/x/comfy/models/diffusion_models/z_image_turbo_bf16.safetensors": { size: 1 } };
  const t = await tempFiles({ "alpha.json": snapshotFile(ACE_TREE, V094), "beta.json": snapshotFile(wrongClass, V094), "old.json": snapshotFile(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }) });
  try {
    const base = ["readiness", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--basis", "installed-package", "--json"];
    const r = await run([...base, "--snapshot", `${t.root}/alpha.json`, "--snapshot", `beta=${t.root}/beta.json`]);
    assert.equal(r.code, 0, r.stderr);
    const out = JSON.parse(r.stdout);
    assert.match(out.basis, /json 0\.1\.94/);
    assert.equal(out.mode, "directory-aware");
    assert.equal(out.nodes.alpha.counts.ready.with_weights, 1);
    assert.equal(out.nodes.beta.counts.ready.with_weights, 0);
    assert.deepEqual(out.any_node.names, ["audio_ace_step1_5_xl_turbo"]);
    const refused = await run([...base, "--snapshot", `${t.root}/old.json`]);
    assert.equal(refused.code, 3);
    assert.match(JSON.parse(refused.stdout).nodes.old.refused, /0\.1\.94/);
    const cand = await run([...base, "--snapshot", `${t.root}/old.json`, "--candidate"]);
    assert.equal(cand.code, 0, cand.stderr);
    const bogus = await run([...base, "--snapshot", `${t.root}/alpha.json`, "--mode", "sideways"]);
    assert.equal(bogus.code, 2);
    assert.match(bogus.stderr, /unknown --mode/);
    const text = await run(["readiness", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--basis", "installed-package", "--snapshot", `${t.root}/alpha.json`]);
    assert.match(text.stdout.split("\n")[0], /^basis: installed package/);
    assert.match(text.stdout, /alpha .*1 ready with weights/);
  } finally {
    t.cleanup();
  }
});

test("cli: readiness --mode both lists what the basename method claims and the directory-aware one does not", async () => {
  const wrongClass = { "D:/x/comfy/models/checkpoints/ae.safetensors": { size: 1 }, "D:/x/comfy/models/text_encoders/qwen_3_4b.safetensors": { size: 1 }, "D:/x/comfy/models/diffusion_models/z_image_turbo_bf16.safetensors": { size: 1 } };
  const t = await tempFiles({ "beta.json": snapshotFile(wrongClass, V094) });
  try {
    const r = await run(["readiness", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--basis", "installed-package", "--snapshot", `${t.root}/beta.json`, "--mode", "both", "--json"]);
    assert.equal(r.code, 0, r.stderr);
    const out = JSON.parse(r.stdout);
    assert.equal(out.directory_aware.nodes.beta.counts.ready.with_weights, 0);
    assert.equal(out.basename_only.nodes.beta.counts.ready.with_weights, 1);
    assert.deepEqual(out.delta.beta, ["image_z_image_turbo"]);
  } finally {
    t.cleanup();
  }
});

test("cli: diff reports what a candidate package adds, removes, changes and breaks on a node", async () => {
  const cand = await candidateDir();
  const t = await tempFiles({ "alpha.json": snapshotFile(ACE_TREE, V094) });
  try {
    const r = await run(["diff", "--templates-dir", REAL_TEMPLATES, "--candidate-dir", cand.dir, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--snapshot", `${t.root}/alpha.json`, "--json"]);
    assert.equal(r.code, 0, r.stderr);
    const out = JSON.parse(r.stdout);
    assert.deepEqual(out.added, ["synthetic_added"]);
    assert.deepEqual(out.removed, ["utility_interpolation_image_upscale"]);
    assert.deepEqual(out.changed.map((c) => c.name), ["audio_ace_step1_5_xl_turbo"]);
    assert.deepEqual(out.readiness.alpha.regressions.map((x) => x.name), ["audio_ace_step1_5_xl_turbo"]);
    const text = await run(["diff", "--templates-dir", REAL_TEMPLATES, "--candidate-dir", cand.dir, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--snapshot", `${t.root}/alpha.json`]);
    assert.match(text.stdout, /from: /);
    assert.match(text.stdout, /added\s+1/);
    assert.match(text.stdout, /would break on alpha:\s+audio_ace_step1_5_xl_turbo/);
    assert.equal((await run(["diff", "--templates-dir", REAL_TEMPLATES])).code, 2);
  } finally {
    cand.cleanup();
    t.cleanup();
  }
});

// ---------------------------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------------------------

async function run(argv, io = C.nodeIo) {
  const out = [], err = [];
  const code = await C.main(argv, { io, out: (s) => out.push(s), err: (s) => err.push(s) });
  return { code, stdout: out.join(""), stderr: err.join("") };
}

test("cli: summary --json prints the counts with their basis and exits 0", async () => {
  const r = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--basis", "installed-package", "--json"]);
  assert.equal(r.code, 0, r.stderr);
  const s = JSON.parse(r.stdout);
  assert.equal(s.entries, 8);
  assert.match(s.basis, /installed package/);
  assert.equal(s.kinds.api, 1);
});

test("cli: summary in text mode leads with the basis line", async () => {
  const r = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--basis", "installed-package"]);
  assert.equal(r.code, 0, r.stderr);
  assert.match(r.stdout.split("\n")[0], /^basis: installed package/);
  assert.match(r.stdout, /entries\s+8/);
});

test("cli: list hides API templates by default and says how many it hid", async () => {
  const r = await run(["list", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json"]);
  assert.equal(r.code, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.equal(out.templates.some((t) => t.kind === "api"), false);
  assert.equal(out.templates.length, 7);
  assert.equal(out.hidden.api, 1);
  const all = JSON.parse((await run(["list", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--include-hidden", "--json"])).stdout);
  assert.equal(all.templates.length, 8);
});

test("cli: list filters by kind and by tag", async () => {
  const kind = JSON.parse((await run(["list", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--kind", "custom_nodes", "--json"])).stdout);
  assert.deepEqual(kind.templates.map((t) => t.name), ["templates_purz_pixel_sort_image"]);
  const tag = JSON.parse((await run(["list", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--tag", "Text to Image", "--json"])).stdout);
  assert.ok(tag.templates.length >= 1 && tag.templates.every((t) => t.tags.includes("Text to Image")));
});

test("cli: --comfy-dir finds the installed package itself", async () => {
  const io = memIo({
    "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates_json-0.1.94.dist-info/METADATA": "",
    "D:/x/comfy/.venv/Lib/site-packages/comfyui_workflow_templates_json/templates/index.json": readFileSync(`${REAL_TEMPLATES}/index.json`, "utf8"),
    "D:/x/comfy/comfy_api_nodes/nodes_x.py": 'IO.Schema(node_id="Some")',
  });
  const r = await run(["summary", "--comfy-dir", "D:/x/comfy", "--json"], io);
  assert.equal(r.code, 0, r.stderr);
  const s = JSON.parse(r.stdout);
  assert.match(s.basis, /installed package/);
  assert.match(s.basis, /json 0\.1\.94/);
  assert.equal(s.entries, 8);
  assert.equal(s.load_errors, 8, "the index is there but this tree carries none of the workflow files");
});

test("cli: an unknown verb or a missing source is a usage error (exit 2) that names the problem", async () => {
  const unknown = await run(["frobnicate"]);
  assert.equal(unknown.code, 2);
  assert.match(unknown.stderr, /unknown verb/);
  const none = await run(["summary"]);
  assert.equal(none.code, 2);
  assert.match(none.stderr, /--templates-dir|--comfy-dir/);
  const help = await run(["--help"]);
  assert.equal(help.code, 0);
  assert.match(help.stdout, /usage: templates-catalog/);
  assert.match(help.stdout, /read-only/);
});

test("cli: nothing is written unless --out is given, and then exactly that one file", async () => {
  const base = ["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json"];
  // Reads go through the real io; writes are captured.
  const spy = { ...C.nodeIo, writes: [], writeText: (p, s) => { spy.writes.push([p, s]); } };
  await run(base, spy);
  assert.deepEqual(spy.writes, []);
  const r = await run([...base, "--out", "D:/x/out/summary.json"], spy);
  assert.equal(r.code, 0, r.stderr);
  assert.equal(spy.writes.length, 1);
  assert.equal(spy.writes[0][0], "D:/x/out/summary.json");
  assert.equal(JSON.parse(spy.writes[0][1]).entries, 8);
});

// ---------------------------------------------------------------------------------------------
// Invariants of the file itself
// ---------------------------------------------------------------------------------------------

const SOURCE = readFileSync(fileURLToPath(new URL("./templates-catalog.mjs", import.meta.url)), "utf8");

test("invariant: no dependencies beyond node: builtins, no network, no child processes", () => {
  const imports = [...SOURCE.matchAll(/^\s*import\s[^;]*?from\s+["']([^"']+)["']/gm)].map((m) => m[1]);
  assert.ok(imports.length > 0);
  for (const spec of imports) assert.match(spec, /^node:/, `import ${spec} is not a node: builtin`);
  assert.doesNotMatch(SOURCE, /\bfetch\s*\(/);
  assert.doesNotMatch(SOURCE, /node:(child_process|http|https|net|dgram|dns|tls|worker_threads)/);
});

test("invariant: the only filesystem write is the explicit --out file", () => {
  const body = SOURCE.replace(/^import[^;]*;/gm, "");
  const writers = body.match(/\b(writeFileSync|writeFile|appendFileSync|appendFile|mkdirSync|mkdir|rmSync|rm|unlinkSync|unlink|renameSync|rename|copyFileSync|copyFile|symlinkSync|truncateSync|createWriteStream)\b/g) || [];
  assert.deepEqual(writers, ["writeFileSync"], "one writeFileSync (nodeIo.writeText) and nothing else that mutates the disk");
});

test("invariant: the module never mentions GPU slots or Windows-only interpreter probes", () => {
  assert.doesNotMatch(SOURCE, /withGpuSlot/);
  assert.doesNotMatch(SOURCE, /Scripts\/python\.exe/);
});

// ---------------------------------------------------------------------------------------------
// The reproduction gate: against the real installed package, when there is one
// ---------------------------------------------------------------------------------------------

const GOLDEN_DIR = process.env.TEMPLATES_CATALOG_DIR;
test("golden: the installed 0.1.94 package reproduces the research counts (skipped without TEMPLATES_CATALOG_DIR)", { skip: !GOLDEN_DIR && "set TEMPLATES_CATALOG_DIR to a comfyui_workflow_templates_json/templates directory" }, () => {
  const dir = GOLDEN_DIR.replace(/\\/g, "/");
  const stamp = C.stampFor({ templatesDir: dir, basis: "installed-package", io: C.nodeIo });
  const comfy = process.env.TEMPLATES_CATALOG_COMFY_DIR;
  const apiIds = comfy ? new Set(C.scanApiNodeIds(`${comfy.replace(/\\/g, "/")}/comfy_api_nodes`, C.nodeIo).ids) : new Set();
  const cat = C.buildCatalog({ templatesDir: dir, apiIds, stamp, licenseMap: C.loadDefaultLicenseMap(C.nodeIo), io: C.nodeIo });
  const s = C.summarizeCatalog(cat);
  assert.equal(s.load_errors, 0);
  if (stamp.package_version !== "0.1.94") return; // the figures below are for that package only
  assert.equal(s.entries, 566);
  assert.equal(s.kinds.custom_nodes, 0);
  assert.equal(s.local.total, 240);
  assert.equal(s.local.needing_weights, 235);
  assert.equal(s.local.zero_model, 5);
  assert.equal(s.subgraph_templates, 197);
  assert.equal(s.model_annotations.entries, 1129);
  assert.equal(s.model_annotations.hashed, 10);
  // Licence and FLUX figures under the committed map (evidence dated 2026-09-30), on the 235 local
  // templates that need weights. They move when the map gains evidence; update them with it.
  assert.deepEqual({ ...s.license.local_needing_weights }, { permissive: 75, conditional: 63, non_commercial: 29, unknown: 68 });
  assert.deepEqual({ ...s.license.repos }, { distinct: 133, mapped: 50, unmapped: 83 });
  assert.deepEqual({ ...s.flux.local_needing_weights }, { by_name: 23, by_repo_only: 2, barred: 25, filename_only: 6 });
  // The repo signal is narrowed by file class: the one text-generation template whose only file is a Qwen text encoder hosted in a
  // FLUX-named repo is not barred (it needs the unknown-licence acknowledgement instead), and the two that take a FLUX.2 VAE from such a repo are.
  const byName = Object.fromEntries(cat.templates.map((r) => [r.name, r]));
  assert.deepEqual([byName.llm_qwen3_text_gen.flux.barred, [...byName.llm_qwen3_text_gen.flux.text_encoder_repos], byName.llm_qwen3_text_gen.gate.state], [false, ["Comfy-Org/flux2-klein"], "ack_required"]);
  assert.deepEqual([byName.image_ideogram4_t2i.gate.state, byName.image_ideogram4_t2i_int8.gate.state], ["blocked", "blocked"]);
  if (comfy) {
    assert.equal(s.kinds.api, 326);
    assert.equal(s.api_signals.by_open_source_false, 326);
    assert.equal(s.api_signals.disagreements, 0);
    // The node-id scan alone sees every paid template, so the api_ prefix is a second opinion and not what keeps three of them paid.
    assert.equal(s.api_signals.by_node_id, s.api_signals.by_open_source_false);
    // A tree that reports the class table's ComfyUI version carries the rule files the table was checked against.
    const tree = C.snapshotNode({ comfyDir: comfy.replace(/\\/g, "/"), io: C.nodeIo });
    if (tree.comfyui_version === C.COMFYUI_RULE_FILES.version) assert.deepEqual({ ...C.checkRules(tree) }, { ok: true, table_version: C.COMFYUI_RULE_FILES.version, node_version: tree.comfyui_version, differs: [], reason: null });
  }
});

// =============================================================================================
// Coverage found by mutating the module. A mutation pass (one-line edits to the module: flipped
// conditions, dropped statements, changed constants and strings) left about a third of its 1,600
// mutants alive under the tests above; every test below passes on the module and fails on the
// mutants named in its `Kills:` comment. They are grouped by what they cover, not by mutant.
// =============================================================================================
const MODULE_PATH = fileURLToPath(new URL("./templates-catalog.mjs", import.meta.url));
const spawnModule = (args) => spawnSync(process.execPath, [MODULE_PATH, ...args], { encoding: "utf8" });
const scratch = (prefix) => {
  const root = nfs.mkdtempSync(`${nos.tmpdir().replace(/\\/g, "/")}/${prefix}-`).replace(/\\/g, "/");
  return { root, cleanup: () => nfs.rmSync(root, { recursive: true, force: true }) };
};
// A link, junction first (it needs no privilege on Windows); false when this host does not allow links.
const tryLink = (target, path) => {
  for (const type of ["junction", "file"]) {
    try { nfs.symlinkSync(target, path, type); return true; } catch { /* try the next kind */ }
  }
  return false;
};

// Every existing CLI test calls main() in-process, so the `import.meta.url === argv[1]` guard, the exit code
// hand-off and the real nodeIo are never exercised. Kills: entry guard `if (false)`, `process.exitCode = code` dropped.
test("entry: run as a script the file prints its result and exits with the verb's code", () => {
  const ok = spawnModule(["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--basis", "installed-package", "--json"]);
  assert.equal(ok.status, 0, ok.stderr);
  assert.equal(JSON.parse(ok.stdout).entries, 8, "no output at all means the entry-point guard never fired");
  const usage = spawnModule(["frobnicate"]);
  assert.equal(usage.status, 2, "a usage error must reach the shell as exit 2");
  assert.match(usage.stderr, /unknown verb/);
});

// Kills: entry guard `if (true)` (importing the module would run the CLI and exit 2).
test("entry: importing the module runs nothing", () => {
  const r = spawnSync(process.execPath, ["--input-type=module", "-e", `await import(${JSON.stringify(pathToFileURL(MODULE_PATH).href)}); console.log("imported");`], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout, "imported\n");
  assert.equal(r.stderr, "");
});

// Kills: readText keeping the BOM (PowerShell 5.1 writes one), sha256 hashing the path instead of the content, writeText a no-op.
test("nodeIo: sha256 hashes the file content, readText drops a BOM, writeText writes the file", () => {
  const t = scratch("tc-io");
  try {
    nfs.writeFileSync(`${t.root}/bom.json`, "\uFEFF{\"k\":1}");
    nfs.writeFileSync(`${t.root}/blob.bin`, "hello");
    assert.deepEqual(JSON.parse(C.nodeIo.readText(`${t.root}/bom.json`)), { k: 1 });
    assert.equal(C.nodeIo.sha256(`${t.root}/blob.bin`), createHash("sha256").update("hello").digest("hex"));
    assert.notEqual(C.nodeIo.sha256(`${t.root}/blob.bin`), C.nodeIo.sha256(`${t.root}/bom.json`), "different content, different hash");
    C.nodeIo.writeText(`${t.root}/out.txt`, "written\n");
    assert.equal(nfs.readFileSync(`${t.root}/out.txt`, "utf8"), "written\n");
  } finally {
    t.cleanup();
  }
});

// The existing --out test swaps writeText for a spy, so the real writer is never run. Kills: writeText no-op,
// `wrote FILE` confirmation dropped, stdout not printed, trailing newline dropped from the file.
test("cli: --out on the real filesystem writes exactly the text that was printed, and says so", async () => {
  const t = scratch("tc-out");
  try {
    const r = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json", "--out", `${t.root}/summary.json`]);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(nfs.readFileSync(`${t.root}/summary.json`, "utf8"), r.stdout);
    assert.match(r.stdout, /\n$/);
    assert.match(r.stderr, /wrote .*summary\.json/);
  } finally {
    t.cleanup();
  }
});

// Kills: a failed --out write exiting 0.
test("cli: a failed --out write is exit 1 and names the file; the result is still printed", async () => {
  const io = { ...C.nodeIo, writeText: () => { throw new Error("EACCES: nope"); } };
  const r = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json", "--out", "D:/x/locked.json"], io);
  assert.equal(r.code, 1);
  assert.match(r.stderr, /cannot write D:\/x\/locked\.json: EACCES/);
  assert.equal(JSON.parse(r.stdout).entries, 8);
});

// Kills: walkFiles listing an entry that is neither file nor directory (a link whose target is gone), a dangling link
// reported as a file by nodeIo.list. Either would make a template "ready" for a model that is not on the node.
test("inventory (real filesystem): a link whose target is gone is not a model file", (t) => {
  const s = scratch("tc-dangling");
  try {
    nfs.mkdirSync(`${s.root}/comfy/models/loras`, { recursive: true });
    nfs.writeFileSync(`${s.root}/comfy/models/loras/real.safetensors`, "x");
    if (!tryLink(`${s.root}/nowhere/gone`, `${s.root}/comfy/models/loras/dangling.safetensors`)) return t.skip("no permission to create links here");
    const inv = C.scanModelInventory(C.resolveModelRoots({ comfyDir: `${s.root}/comfy`, yamls: [] }), C.nodeIo);
    assert.deepEqual(relsOf(inv, "loras"), ["real.safetensors"]);
    const ready = C.readiness([req("loras", "dangling.safetensors")], inv);
    assert.equal(ready.ready, false);
  } finally {
    s.cleanup();
  }
});

// Kills: the link chain never released, so a directory reachable through two links is walked once.
test("inventory (real filesystem): two links to one directory are both listed, each under its own name", (t) => {
  const s = scratch("tc-diamond");
  try {
    nfs.mkdirSync(`${s.root}/real/x`, { recursive: true });
    nfs.writeFileSync(`${s.root}/real/x/f.safetensors`, "x");
    nfs.mkdirSync(`${s.root}/comfy/models/checkpoints`, { recursive: true });
    const linked = tryLink(`${s.root}/real/x`, `${s.root}/comfy/models/checkpoints/a`) && tryLink(`${s.root}/real/x`, `${s.root}/comfy/models/checkpoints/b`);
    if (!linked) return t.skip("no permission to create links here");
    const inv = C.scanModelInventory(C.resolveModelRoots({ comfyDir: `${s.root}/comfy`, yamls: [] }), C.nodeIo);
    assert.deepEqual(relsOf(inv, "checkpoints"), ["a/f.safetensors", "b/f.safetensors"]);
  } finally {
    s.cleanup();
  }
});

// Kills: the API-node scan following links (`follow: false` -> true).
test("api ids: the scan does not follow a link out of comfy_api_nodes", (t) => {
  const s = scratch("tc-apilink");
  try {
    nfs.mkdirSync(`${s.root}/comfy/comfy_api_nodes`, { recursive: true });
    nfs.writeFileSync(`${s.root}/comfy/comfy_api_nodes/nodes_a.py`, 'IO.Schema(node_id="Inside")\n');
    nfs.mkdirSync(`${s.root}/elsewhere`, { recursive: true });
    nfs.writeFileSync(`${s.root}/elsewhere/nodes_b.py`, 'IO.Schema(node_id="Outside")\n');
    if (!tryLink(`${s.root}/elsewhere`, `${s.root}/comfy/comfy_api_nodes/linked`)) return t.skip("no permission to create links here");
    assert.deepEqual(C.scanApiNodeIds(`${s.root}/comfy/comfy_api_nodes`, C.nodeIo), { ids: ["Inside"], files: 1 });
  } finally {
    s.cleanup();
  }
});


// ---------------------------------------------------------------------------------------------
// Tables and constants no fixture exercises
// ---------------------------------------------------------------------------------------------

// A catalog of small in-memory templates: defs = [{ name, entry?, workflow? }]. Index rows default to open source.
const catalogFrom = (defs, opts = {}, extraFiles = {}) => {
  const files = {
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation",
      templates: defs.map((d) => ({ title: d.name, mediaType: "image", openSource: true, ...d.entry, name: d.name })) }]),
    ...extraFiles,
  };
  for (const d of defs) if (d.workflow) files[`D:/x/t/${d.name}.json`] = JSON.stringify(d.workflow);
  return C.buildCatalog({ templatesDir: "D:/x/t", apiIds: new Set(), licenseMap: fixtureMap(), io: memIo(files), ...opts });
};
const rowFor = (workflow, entry = {}, opts = {}) => catalogFrom([{ name: "t", entry, workflow }], opts).templates[0];
// A loader node with one annotated model: model = { name, repo, dir }.
const loaderNode = (id, type, model, extra = {}) => ({ id, type, mode: 0, properties: { models: [{ name: model.name, url: hf(model.repo, model.name), directory: model.dir }] }, ...extra });

// Kills: MODEL_EXT losing .gguf/.pth/.pt/.ckpt/.bin/.sft/.onnx (or .safetensors), a case-sensitive extension match,
// and "any string widget is a model file".
test("models: every weights extension a template can name is a requirement, in any letter case; other strings are not", () => {
  for (const ext of [".safetensors", ".gguf", ".pth", ".pt", ".ckpt", ".bin", ".sft", ".onnx", ".SAFETENSORS"]) {
    const r = C.requiredModels({ nodes: [{ id: 1, type: "UpscaleModelLoader", mode: 0, widgets_values: [`model${ext}`] }] });
    assert.deepEqual(r.active.map((m) => m.name), [`model${ext}`], ext);
  }
  const none = C.requiredModels({ nodes: [{ id: 1, type: "KSampler", mode: 0, widgets_values: ["euler", 20, "notes.txt", "picture.png"] }] });
  assert.deepEqual(none.active, []);
});

// Kills: any single core loader mapped to the wrong class (the mapping is a table nobody re-reads).
const LOADER_CLASSES = {
  CheckpointLoaderSimple: "checkpoints", CheckpointLoader: "checkpoints", ImageOnlyCheckpointLoader: "checkpoints", unCLIPCheckpointLoader: "checkpoints",
  UNETLoader: "diffusion_models", VAELoader: "vae",
  CLIPLoader: "text_encoders", DualCLIPLoader: "text_encoders", TripleCLIPLoader: "text_encoders", QuadrupleCLIPLoader: "text_encoders",
  LoraLoader: "loras", LoraLoaderModelOnly: "loras",
  ControlNetLoader: "controlnet", DiffControlNetLoader: "controlnet", CLIPVisionLoader: "clip_vision",
  UpscaleModelLoader: "upscale_models", StyleModelLoader: "style_models", LatentUpscaleModelLoader: "latent_upscale_models",
  ModelPatchLoader: "model_patches", AudioEncoderLoader: "audio_encoders", GLIGENLoader: "gligen",
  HypernetworkLoader: "hypernetworks", PhotoMakerLoader: "photomaker",
};
test("models: the class an unannotated file is filed under, for every core loader, is a class the node registers", () => {
  const registered = Object.keys(C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [] }).classes);
  for (const [type, cls] of Object.entries(LOADER_CLASSES)) {
    const r = C.requiredModels({ nodes: [{ id: 1, type, mode: 0, widgets_values: ["m.safetensors"] }] });
    assert.equal(r.active[0].directory, cls, type);
    assert.ok(registered.includes(cls), `${type} files under ${cls}, which no directory is registered for`);
  }
});

// Kills: any class dropped from CLASS_TABLE or given the wrong default directory (9 classes were asserted, 25 exist).
const DEFAULT_DIRS = {
  checkpoints: ["checkpoints"], configs: ["configs"], loras: ["loras"], vae: ["vae"],
  text_encoders: ["text_encoders", "clip"], diffusion_models: ["unet", "diffusion_models"],
  clip_vision: ["clip_vision"], style_models: ["style_models"], embeddings: ["embeddings"], diffusers: ["diffusers"], vae_approx: ["vae_approx"],
  controlnet: ["controlnet", "t2i_adapter"], gligen: ["gligen"], upscale_models: ["upscale_models"], latent_upscale_models: ["latent_upscale_models"],
  hypernetworks: ["hypernetworks"], photomaker: ["photomaker"], classifiers: ["classifiers"], model_patches: ["model_patches"], audio_encoders: ["audio_encoders"],
  background_removal: ["background_removal"], frame_interpolation: ["frame_interpolation"], geometry_estimation: ["geometry_estimation"], optical_flow: ["optical_flow"], detection: ["detection"],
};
// main.py's apply_custom_paths adds the output directory's copy of five classes after them (`clip` is text_encoders).
const OUTPUT_DIRS = { checkpoints: "checkpoints", text_encoders: "clip", vae: "vae", diffusion_models: "diffusion_models", loras: "loras" };
test("roots: the default directories of every class ComfyUI 0.37.0 registers, and the output directories main.py adds to five of them", () => {
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [] });
  assert.deepEqual(Object.keys(roots.classes).sort(), Object.keys(DEFAULT_DIRS).sort());
  for (const [cls, dirs] of Object.entries(DEFAULT_DIRS)) {
    const expected = dirs.map((d) => `D:/x/comfy/models/${d}`);
    if (Object.hasOwn(OUTPUT_DIRS, cls)) expected.push(`D:/x/comfy/output/${OUTPUT_DIRS[cls]}`);
    assert.deepEqual(dirsOf(roots, cls), expected, cls);
  }
});

// Kills: PT_EXTS losing .ckpt/.pt/.pt2/.bin/.pth/.pkl/.sft, a case-sensitive extension match, and a wrong extension list
// on the two classes that do not take the default one (configs: .yaml, classifiers: no extension).
test("inventory: every extension the core loaders accept is listed in any letter case; the rest only count as unlisted", () => {
  const accepted = [".ckpt", ".pt", ".pt2", ".bin", ".pth", ".safetensors", ".pkl", ".sft"];
  const files = { "D:/x/comfy/models/checkpoints/UPPER.SAFETENSORS": { size: 1 }, "D:/x/comfy/models/checkpoints/m.gguf": { size: 1 }, "D:/x/comfy/models/checkpoints/m.txt": { size: 1 } };
  for (const e of accepted) files[`D:/x/comfy/models/checkpoints/m${e}`] = { size: 1 };
  const inv = invFrom(files);
  assert.deepEqual(relsOf(inv, "checkpoints"), [...accepted.map((e) => `m${e}`), "UPPER.SAFETENSORS"].sort());
  assert.equal(inv.classes.checkpoints.unlisted, 2);
  const special = invFrom({
    "D:/x/comfy/models/configs/v.yaml": { size: 1 }, "D:/x/comfy/models/configs/v.safetensors": { size: 1 },
    "D:/x/comfy/models/classifiers/noext": { size: 1 }, "D:/x/comfy/models/classifiers/x.bin": { size: 1 },
  });
  assert.deepEqual(relsOf(special, "configs"), ["v.yaml"]);
  assert.deepEqual(relsOf(special, "classifiers"), ["noext"]);
});

// Kills: a yaml-only class (extension list null) crashing the scan, and an emptied list no longer accepting everything.
test("inventory: a class only extra_model_paths.yaml names lists every file in its directory", () => {
  const yamls = [{ text: "p:\n  base_path: D:/x/m\n  brand_new_class: brand_new\n", dir: "D:/x/comfy" }];
  const inv = invFrom({ "D:/x/m/brand_new/a.weird": { size: 3 }, "D:/x/m/brand_new/sub/b.txt": { size: 4 } }, yamls);
  assert.deepEqual(relsOf(inv, "brand_new_class"), ["a.weird", "sub/b.txt"]);
  assert.equal(inv.classes.brand_new_class.unlisted, 0);
});

// Kills: requirement key ignoring the class directory; node_types not de-duplicated; a case-sensitive or full-name-only
// widget/annotation match; empty annotation fields kept as "" instead of null; empty-name annotations kept and counted.
test("models: one file name in two classes is two requirements", () => {
  const wf = { nodes: [loaderNode(1, "VAELoader", { name: "ae.safetensors", repo: "example-org/w", dir: "vae" }), loaderNode(2, "UNETLoader", { name: "ae.safetensors", repo: "example-org/w", dir: "diffusion_models" })] };
  assert.deepEqual(C.requiredModels(wf).active.map((m) => `${m.directory}/${m.name}`).sort(), ["diffusion_models/ae.safetensors", "vae/ae.safetensors"]);
});

test("models: two loaders of one type naming one file list that node type once", () => {
  const node = (id) => loaderNode(id, "LoraLoader", { name: "shared.safetensors", repo: "example-org/w", dir: "loras" });
  assert.deepEqual(C.requiredModels({ nodes: [node(1), node(2)] }).active.map((m) => m.node_types), [["LoraLoader"]]);
});

test("models: a loader widget is the annotated file when it names it in another case, under a subfolder, or with the folder in both", () => {
  // A loader type the class table does not know: a widget file that failed to match its annotation would then get a different
  // requirement key (no class) and show up as a second requirement instead of merging into the annotated one.
  const annotated = (name, widget) => C.requiredModels({ nodes: [loaderNode(1, "CustomLoader", { name, repo: "example-org/w", dir: "loras" }, { widgets_values: [widget] })] }).active.map((m) => m.name);
  assert.deepEqual(annotated("Style.safetensors", "style.safetensors"), ["Style.safetensors"], "another case");
  assert.deepEqual(annotated("style.safetensors", "sub/style.safetensors"), ["style.safetensors"], "widget under a subfolder, annotation by basename");
  assert.deepEqual(annotated("sub/style.safetensors", "sub/style.safetensors"), ["sub/style.safetensors"], "the folder in both");
});

test("models: a widget path written with backslashes (a Windows-authored workflow) is the forward-slash file", () => {
  const r = C.requiredModels({ nodes: [{ id: 1, type: "LoraLoader", mode: 0, widgets_values: ["sub\\dir\\l.safetensors"] }] });
  assert.deepEqual(r.active.map((m) => m.name), ["sub/dir/l.safetensors"]);
});

test("models: neither kind of note node names a requirement, and an annotation with no name is ignored and not counted", () => {
  const notes = ["Note", "MarkdownNote"].map((type, i) => ({ id: i + 1, type, mode: 0, widgets_values: [`noted${i}.safetensors`] }));
  assert.deepEqual(C.requiredModels({ nodes: notes }).active, []);
  const wf = { nodes: [{ id: 1, type: "VAELoader", mode: 0, properties: { models: [{ name: "", url: hf("a/b", "x.safetensors"), directory: "vae" }, { name: "ok.safetensors", directory: "vae" }] } }] };
  assert.deepEqual(C.requiredModels(wf).active.map((m) => m.name), ["ok.safetensors"]);
  assert.equal(rowFor(wf).graph.model_annotation_entries, 1);
});

test("models: an annotation's empty directory, url or hash means 'not given', not an empty string", () => {
  const r = C.requiredModels({ nodes: [{ id: 1, type: "VAELoader", mode: 0, properties: { models: [{ name: "x.safetensors", directory: "", url: "", hash: "", hash_type: "" }] } }] });
  assert.deepEqual({ ...r.active[0] }, { name: "x.safetensors", directory: null, url: null, hash: null, hash_type: null, annotated: true, node_types: ["VAELoader"] });
});

// Kills: KB/TB unit values, the thousands separator, NaN sizes stored, the key not trimmed.
test("note sizes: every unit, a thousands separator, a figure that is not a number, and a spaced file name", () => {
  const note = (text) => ({ nodes: [{ id: 1, type: "MarkdownNote", mode: 0, widgets_values: [text] }] });
  const size = (s) => C.parseNoteSizes(note(`- [f.safetensors](https://huggingface.co/x/y/resolve/main/f.safetensors) (${s})`))["f.safetensors"];
  assert.equal(size("2 KB"), 2 * 1024);
  assert.equal(size("3 MB"), 3 * 1024 ** 2);
  assert.equal(size("4 GB"), 4 * 1024 ** 3);
  assert.equal(size("1.5 TB"), 1.5 * 1024 ** 4);
  assert.equal(size("1,024 MB"), 1024 * 1024 ** 2, "thousands separator");
  assert.equal(size("1.2.3 GB"), undefined, "a malformed figure is not a size");
  assert.equal(C.parseNoteSizes(note("- [ spaced.safetensors ](https://x.co/a) (1 KB)"))["spaced.safetensors"], 1024, "the key is trimmed");
});

// Kills: row models never carrying note_size_bytes, row.note_sizes dropped.
test("catalog: a size given in the model note lands on the row's model (note_size_bytes) and in note_sizes", () => {
  const model = { name: "a.safetensors", repo: "example-org/weights-a", dir: "diffusion_models" };
  const row = rowFor({ nodes: [loaderNode(1, "UNETLoader", model), { id: 2, type: "MarkdownNote", mode: 0, widgets_values: [`- [a.safetensors](${hf(model.repo, model.name)}) (2 GB)`] }] });
  assert.equal(row.models[0].note_size_bytes, 2 * 1024 ** 3);
  assert.deepEqual({ ...row.note_sizes }, { "a.safetensors": 2 * 1024 ** 3 });
});


// ---------------------------------------------------------------------------------------------
// Walk, API scan, stamp/basis, licences, FLUX, gate wiring, rows and counts
// ---------------------------------------------------------------------------------------------

// Kills: the instance chain never popped, so a second instance of one definition is taken for recursion and never
// expanded. With one instance bypassed and its sibling active, the file both share would then be reported optional.
test("walk: two instances of one subgraph are both expanded; a file a bypassed and an active instance share is required", () => {
  const sub = "a1111111-1111-4111-8111-111111111111";
  const wf = {
    nodes: [{ id: 1, type: sub, mode: 4 }, { id: 2, type: sub, mode: 0 }],
    definitions: { subgraphs: [{ id: sub, name: "sampler", nodes: [loaderNode(10, "UNETLoader", { name: "inner.safetensors", repo: "example-org/weights-a", dir: "diffusion_models" })] }] },
  };
  assert.deepEqual(C.flattenActiveNodes(wf).map((l) => [l.path.join("/"), l.active, Boolean(l.unexpanded)]), [["1", false, false], ["2", true, false]]);
  const r = C.requiredModels(wf);
  assert.deepEqual(names(r.active), ["inner.safetensors"], "the active sibling needs it");
  assert.deepEqual(r.inactive, []);
});

// Kills: API scan reading every file instead of only .py, ids unsorted, one unreadable source aborting the scan.
test("api ids: only .py sources are read, an unreadable one is skipped, and the ids come back sorted", () => {
  const io = memIo({
    "D:/x/c/comfy_api_nodes/z_nodes.py": 'IO.Schema(node_id="Zed")',
    "D:/x/c/comfy_api_nodes/a_nodes.py": 'IO.Schema(node_id="Alpha")',
    "D:/x/c/comfy_api_nodes/notes.md": 'node_id="InMarkdown"',
    "D:/x/c/comfy_api_nodes/apis/table.json": 'node_id="InJson"',
    "D:/x/c/comfy_api_nodes/bad.py": { size: 9 },
  });
  assert.deepEqual(C.scanApiNodeIds("D:/x/c/comfy_api_nodes", io), { ids: ["Alpha", "Zed"], files: 3 });
});

// Kills: api_ matched anywhere in the name; a partner node used twice listed twice.
test("api signals: api_ is a prefix, and a partner node used twice is listed once", () => {
  assert.equal(C.apiSignals({ name: "video_api_helper", openSource: true, nodeTypes: [], apiIds: new Set() }).by_name_prefix, false);
  const s = C.apiSignals({ name: "x", openSource: true, nodeTypes: ["PaidB", "PaidA", "PaidB", "KSampler"], apiIds: new Set(["PaidA", "PaidB"]) });
  assert.deepEqual(s.by_node_id, ["PaidA", "PaidB"]);
});

// Kills: the inner node id not stringified.
test("param surface: a numeric inner node id is reported as a string", () => {
  const sub = "a1111111-1111-4111-8111-111111111111";
  const wf = { nodes: [{ id: 7, type: sub, properties: { proxyWidgets: [[3, "w"]] } }], definitions: { subgraphs: [{ id: sub, nodes: [] }] } };
  assert.deepEqual(C.paramSurface(wf).map((s) => ({ ...s })), [{ instance: 7, node: "3", widget: "w" }]);
});

// ---- the stamp: the module's thesis is that a figure without its basis is not a figure ----------

const pkgFiles = (root, version = "0.1.94") => ({
  [`${root}/comfyui_workflow_templates_json-${version}.dist-info/METADATA`]: "",
  [`${root}/comfyui_workflow_templates_json/templates/index.json`]: JSON.stringify([{ templates: [{ name: "a" }] }]),
  [`${root}/comfyui_workflow_templates_json/templates/a.json`]: '{"nodes":[]}',
});
const templatesOf = (root) => `${root}/comfyui_workflow_templates_json/templates`;

// Kills: the inferred-basis chain (labels swapped or wrong), an explicit --basis ignored, an unknown basis accepted,
// BASIS_TEXT wording for the three labels the existing tests never assert.
test("stamp: with no --basis it is inferred from the layout, an explicit one wins, and an unknown one is refused", () => {
  const stamp = (files, dir, basis) => C.stampFor({ templatesDir: dir, basis, io: memIo(files) });
  const installed = stamp(pkgFiles("D:/x/venv/Lib/site-packages"), templatesOf("D:/x/venv/Lib/site-packages"));
  assert.equal(installed.basis, "installed-package");
  assert.match(installed.label, /^installed package \(/);
  const extract = stamp(pkgFiles("D:/x/wheel-extract"), templatesOf("D:/x/wheel-extract"));
  assert.equal(extract.basis, "package-extract");
  assert.match(extract.label, /^package extract \(/);
  const repo = stamp({ "D:/x/repo/pyproject.toml": 'version = "0.11.71"\n', "D:/x/repo/templates/index.json": "[]" }, "D:/x/repo/templates");
  assert.equal(repo.basis, "repo-checkout");
  assert.match(repo.label, /^repo checkout \(/);
  const loose = stamp({ "D:/x/loose/index.json": "[]" }, "D:/x/loose");
  assert.equal(loose.basis, "templates-dir");
  assert.match(loose.label, /^templates directory \(/);
  const forced = stamp(pkgFiles("D:/x/venv/Lib/site-packages"), templatesOf("D:/x/venv/Lib/site-packages"), "package-extract");
  assert.equal(forced.basis, "package-extract", "what the operator says beats the guess");
  assert.throws(() => stamp(pkgFiles("D:/x/w"), templatesOf("D:/x/w"), "nonsense"), /unknown basis "nonsense"/);
});

// Kills: fingerprint depending on directory listing order (two OSes list differently, so two identical packages would
// look different), ignoring file names, or counting non-json files (real packages carry thousands of thumbnails);
// stamping a directory that has no index failing instead of recording index_entries 0.
test("stamp: the fingerprint ignores listing order and thumbnails, follows file names, and an index-less directory still stamps", () => {
  const t = templatesOf("D:/x/sp");
  const base = pkgFiles("D:/x/sp");
  const one = C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo(base) });
  const reversed = Object.fromEntries(Object.entries(base).reverse());
  assert.equal(C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo(reversed) }).fingerprint, one.fingerprint, "listing order must not matter");
  const renamed = { ...base, [`${t}/b.json`]: base[`${t}/a.json`] };
  delete renamed[`${t}/a.json`];
  assert.notEqual(C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo(renamed) }).fingerprint, one.fingerprint, "the same bytes under another name is another package");
  const withThumb = C.stampFor({ templatesDir: t, basis: "installed-package", io: memIo({ ...base, [`${t}/a-1.webp`]: { size: 5 } }) });
  assert.equal(withThumb.fingerprint, one.fingerprint);
  assert.equal(withThumb.template_files, 1);
  const bare = C.stampFor({ templatesDir: "D:/x/none", basis: "templates-dir", io: memIo({ "D:/x/none/a.json": "{}" }) });
  assert.equal(bare.index_entries, 0);
  assert.equal(bare.index_sha256, null);
});

// Kills: .venv/venv precedence swapped, python3.x order reversed, a templates directory with no index.json accepted.
test("locate: .venv beats venv, the higher python3.x beats the lower, and a templates directory without an index is not one", () => {
  const idx = (p) => ({ [`${p}/comfyui_workflow_templates_json/templates/index.json`]: "[]" });
  const both = memIo({ ...idx("/srv/c/venv/lib/python3.12/site-packages"), ...idx("/srv/c/.venv/lib/python3.12/site-packages") });
  assert.match(C.locateTemplates("/srv/c", both).sitePackages, /\/\.venv\//);
  const two = memIo({ ...idx("/srv/c/.venv/lib/python3.11/site-packages"), ...idx("/srv/c/.venv/lib/python3.12/site-packages") });
  assert.match(C.locateTemplates("/srv/c", two).sitePackages, /python3\.12/);
  const noIndex = memIo({ "/srv/c/.venv/lib/python3.12/site-packages/comfyui_workflow_templates_json/templates/other.json": "{}" });
  assert.equal(C.locateTemplates("/srv/c", noIndex), null);
});

// Kills: the "no installed package" and "catalog carries no version" refusals collapsing into the mismatch message.
test("stamp: the refusal says which side is missing", () => {
  assert.match(C.checkStamp({ package_version: "0.1.94" }, {}).reason, /no installed comfyui-workflow-templates-json/);
  assert.match(C.checkStamp({ package_version: null }, { "comfyui-workflow-templates-json": "0.1.94" }).reason, /no package version/);
});

// ---- licences, FLUX and the gate ------------------------------------------------------------

// Kills: evidence dates unsorted / end = start / reversed, schema_version dropped, has() case-sensitive, and the
// catalog dropping the map facts or its own schema_version.
test("license map: the evidence range is the earliest and latest fetched_on whatever the entry order; the catalog carries it", () => {
  const entry = (d) => ({ class: "permissive", source_url: "https://x", fetched_on: d, basis: "hf-card-tag" });
  const map = C.loadLicenseMap({ schema_version: 3, repos: { "a/one": entry("2026-03-01"), "a/two": entry("2026-01-05"), "a/three": entry("2026-02-10") } });
  assert.deepEqual([...map.meta.evidence_dates], ["2026-01-05", "2026-03-01"]);
  assert.equal(map.meta.schema_version, 3);
  assert.equal(map.has("A/ONE"), true, "has() ignores case like get()");
  assert.deepEqual([...C.emptyLicenseMap().meta.evidence_dates], []);
  const cat = catalogFrom([{ name: "z", workflow: { nodes: [] } }], { licenseMap: map });
  assert.deepEqual([...cat.license_map.evidence_dates], ["2026-01-05", "2026-03-01"]);
  assert.equal(cat.license_map.entries, 3);
  assert.equal(cat.schema_version, 1);
});

// Kills: license_id / basis dropped from repos[], strict_class leaking into it, class_strict of a no-weights template.
test("license class: a repos[] entry carries the licence id, the basis and the strict downgrade, and nothing internal", () => {
  const only = (repo) => C.licenseAssessment([mdl(repo)], fixtureMap()).repos[0];
  assert.deepEqual({ ...only("example-org/weights-a") }, { repo: "example-org/weights-a", class: "permissive", known: true, license_id: "apache-2.0", basis: "hf-card-tag", strict_downgraded: false });
  assert.deepEqual({ ...only("example-org/weights-inferred") }, { repo: "example-org/weights-inferred", class: "conditional", known: true, license_id: "other", basis: "inferred from example-org/weights-b (not fetched)", strict_downgraded: true });
  assert.deepEqual({ ...only("example-org/not-in-map") }, { repo: "example-org/not-in-map", class: "unknown", known: false, license_id: null, basis: null, strict_downgraded: false });
  assert.equal(C.licenseAssessment([], fixtureMap()).class_strict, "none");
});

// Kills: repoOfUrl case-sensitive on the host, and no longer anchored (a URL that merely contains an HF link).
test("repo id: the host is matched in any case and the URL must start with it", () => {
  assert.equal(C.repoOfUrl("https://HuggingFace.co/A/b/resolve/main/x.safetensors"), "A/b");
  assert.equal(C.repoOfUrl("https://proxy.example/?u=https://huggingface.co/a/b/resolve/main/x.safetensors"), null);
});

// Kills: barred repos / FLUX-named files unsorted, the FLUX name match case-sensitive.
test("FLUX bar: barred repos and FLUX-named files come back sorted and once each; the name match ignores case", () => {
  const r = C.fluxAssessment({ name: "Image_OK", models: [
    mdl("zeta/flux-two", "z-flux.safetensors"), mdl("alpha/FLUX.1-dev", "a-flux.safetensors"), mdl("zeta/flux-two", "z-flux.safetensors"), mdl("example-org/weights-a", "b-flux.safetensors"),
  ] }, fixtureMap());
  assert.deepEqual(r.repos, ["alpha/FLUX.1-dev", "zeta/flux-two"]);
  assert.deepEqual(r.filename_hits, ["a-flux.safetensors", "b-flux.safetensors", "z-flux.safetensors"]);
  assert.equal(C.fluxAssessment({ name: "Flux_Dev_Image", models: [] }).barred, true);
});

// Kills: a barred template also carrying the filename warning; the warning naming only the first file.
test("gate: only an unbarred template warns about FLUX-named files, and the warning names every one", () => {
  assert.deepEqual(gate({ flux: { barred: true, filename_hits: ["flux-x.safetensors"] } }).warnings, []);
  assert.deepEqual(gate({ flux: { barred: false, filename_hits: ["a-flux.safetensors", "b-flux.safetensors"] } }).warnings, ["flux_component_by_filename: a-flux.safetensors, b-flux.safetensors"]);
});

// The FLUX bar (ADR 0011) is unit-tested on fluxAssessment and gateFor, but at catalog level only the NAME path was
// covered, so a row that stops passing its models to the assessment kept every test green in CI.
// Kills: buildRow calling fluxAssessment with no models (golden-only kill today).
test("catalog: a template whose weights come from a FLUX-family repo is barred and blocked although its name says nothing", () => {
  const wf = (repo) => ({ nodes: [loaderNode(1, "UNETLoader", { name: "m.safetensors", repo, dir: "diffusion_models" })] });
  const cat = catalogFrom([
    { name: "image_maker", workflow: wf("black-forest-labs/whatever") },
    { name: "image_flagged", workflow: wf("example-org/flagged-family") },
    { name: "image_exempt", workflow: wf("comfyanonymous/flux_text_encoders") },
    { name: "image_plain", workflow: wf("example-org/weights-a") },
  ]);
  assert.deepEqual({ ...rowOf(cat, "image_maker").flux }, { barred: true, by: ["repo"], repos: ["black-forest-labs/whatever"], filename_hits: [], text_encoder_repos: [] });
  assert.deepEqual({ ...rowOf(cat, "image_maker").gate }, { state: "blocked", blocked: ["flux"], ack: [], warnings: [] });
  assert.equal(rowOf(cat, "image_flagged").flux.barred, true, "the map's flux_bar flag reaches the row");
  assert.equal(rowOf(cat, "image_exempt").flux.barred, false, "and so does its exemption");
  assert.equal(rowOf(cat, "image_plain").gate.state, "open");
});

// ---- rows, graph counts and the catalog ------------------------------------------------------

// Kills: any index field dropped from the row (description, media_subtype, date, usage, include_on_distributions, is_app, io).
test("catalog: a row projects every index field under its snake_case name", () => {
  const io = { inputs: [{ nodeId: 1, nodeType: "LoadImage", file: "a.png" }], outputs: [] };
  const row = rowFor({ nodes: [] }, { description: "Desc", mediaSubtype: "webp", tags: ["T1"], models: ["M1"], date: "2026-01-02", size: 123, usage: 45, openSource: true, minComfyUIVersion: "0.9.0", requiresCustomNodes: [], includeOnDistributions: ["cloud"], isApp: true, io });
  assert.equal(row.description, "Desc");
  assert.equal(row.media_subtype, "webp");
  assert.equal(row.date, "2026-01-02");
  assert.equal(row.usage, 45);
  assert.equal(row.size_bytes, 123);
  assert.deepEqual(row.include_on_distributions, ["cloud"]);
  assert.equal(row.is_app, true);
  assert.deepEqual(row.io, io);
});

// Kills: models_inactive dropped; an empty cnr_id counted as a custom pack.
test("catalog: a bypassed loader's file is listed as inactive, and an empty cnr_id is not a custom pack", () => {
  const on = loaderNode(1, "LoraLoader", { name: "on.safetensors", repo: "example-org/weights-a", dir: "loras" });
  const off = loaderNode(2, "LoraLoader", { name: "off.safetensors", repo: "example-org/weights-a", dir: "loras" }, { mode: 4 });
  const row = rowFor({ nodes: [on, off, { id: 3, type: "KSampler", mode: 0, properties: { cnr_id: "" } }] });
  assert.deepEqual(row.models.map((m) => m.name), ["on.safetensors"]);
  assert.deepEqual(row.models_inactive.map((m) => m.name), ["off.safetensors"]);
  assert.deepEqual(row.custom_packs, []);
  assert.equal(row.kind, "local");
});

// Kills: graph.unexpanded / graph.dangling / hashed-annotation counts dropped (the last is a golden-only kill today).
test("catalog: the graph facts count unexpanded instances, dangling instances and hashed annotations", () => {
  const loop = "a1111111-1111-4111-8111-111111111111";
  const gone = "b2222222-2222-4222-8222-222222222222";
  const hashed = { name: "h.safetensors", url: hf("example-org/weights-a", "h.safetensors"), directory: "vae", hash: "0".repeat(64), hash_type: "SHA256" };
  const wf = {
    nodes: [{ id: 1, type: loop, mode: 0 }, { id: 9, type: gone, mode: 0 }, { id: 5, type: "VAELoader", mode: 0, properties: { models: [hashed] } }],
    definitions: { subgraphs: [{ id: loop, name: "loop", nodes: [{ id: 2, type: loop, mode: 0 }] }] },
  };
  const row = rowFor(wf);
  assert.equal(row.graph.unexpanded, 1);
  assert.equal(row.graph.dangling, 1);
  assert.equal(row.graph.model_annotation_hashed, 1);
  assert.equal(C.summarizeCatalog(catalogFrom([{ name: "t", workflow: wf }])).model_annotations.hashed, 1);
});

// Kills: an invalid-JSON workflow reported as "cannot read".
test("catalog: a workflow that is not JSON says 'invalid JSON' and one that cannot be read says 'cannot read'", () => {
  const io = memIo({
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [{ name: "bad", title: "B", mediaType: "image" }, { name: "gone", title: "G", mediaType: "image" }] }]),
    "D:/x/t/bad.json": '{ "nodes": [',
  });
  const cat = C.buildCatalog({ templatesDir: "D:/x/t", apiIds: new Set(), io });
  assert.match(rowOf(cat, "bad").load_error, /^bad\.json: invalid JSON/);
  assert.match(rowOf(cat, "gone").load_error, /^gone\.json: cannot read \(ENOENT\)/);
});

// Kills: apiIds given as an array not counted, api_ids.count dropped.
test("catalog: apiIds may be an array, and the catalog and its summary say how many ids were scanned", () => {
  const cat = catalogFrom([{ name: "by_id", workflow: { nodes: [{ id: 1, type: "PaidNodeX", mode: 0 }] } }], { apiIds: ["PaidNodeX", "PaidNodeY"] });
  assert.equal(cat.templates[0].kind, "api");
  assert.equal(cat.api_ids.count, 2);
  assert.equal(C.summarizeCatalog(cat).api_ids, 2);
});

// Kills: orphan files unsorted; an indexed template with capitals reported as an orphan.
test("catalog: orphan files are listed in name order and matched to the index by exact name", () => {
  const cat = catalogFrom([{ name: "Mixed_Case", workflow: { nodes: [] } }], {}, { "D:/x/t/zz_orphan.json": "{}", "D:/x/t/aa_orphan.json": "{}" });
  assert.deepEqual(cat.orphan_files, ["aa_orphan", "zz_orphan"]);
});

// ---- summary figures -------------------------------------------------------------------------

// Kills: by_repo_only counting name-flagged templates too, filename_only counting barred ones, barred_all_kinds
// restricted to local templates (the first two are golden-only kills today).
test("counts: FLUX by name, by repo only and by file name, and a barred paid template, are counted apart", () => {
  const wf = (m) => ({ nodes: [loaderNode(1, "UNETLoader", m)] });
  const a = (name) => ({ name, repo: "example-org/weights-a", dir: "diffusion_models" });
  const flagged = (name) => ({ name, repo: "example-org/flagged-family", dir: "diffusion_models" });
  const s = C.summarizeCatalog(catalogFrom([
    { name: "flux_by_name", workflow: wf(a("a.safetensors")) },
    { name: "image_by_repo", workflow: wf(flagged("f.safetensors")) },
    { name: "image_file_hit", workflow: wf(a("flux2-vae.safetensors")) },
    { name: "api_flux_paid", workflow: wf(a("c.safetensors")) },
    { name: "image_plain", workflow: wf(a("d.safetensors")) },
    { name: "image_zero", workflow: { nodes: [{ id: 1, type: "KSampler", mode: 0 }] } },
    { name: "flux_name_and_repo", workflow: wf(flagged("g.safetensors")) },
    { name: "flux_name_and_file", workflow: wf(a("flux3-x.safetensors")) },
    { name: "image_by_repo_two", workflow: wf(flagged("h.safetensors")) },
    { name: "image_file_hit_two", workflow: wf(a("flux4-vae.safetensors")) },
  ]));
  // Two "repo only" and two "file only" templates against ONE of each that overlaps with the name signal: a counter that
  // forgets to exclude the overlap would come out at 1, not 2.
  assert.deepEqual({ ...s.flux.local_needing_weights }, { by_name: 3, by_repo_only: 2, barred: 5, filename_only: 2 }, "a template barred by name AND repo is not 'repo only'; one barred by name that also has a FLUX-named file is not 'file only'");
  assert.equal(s.flux.barred_all_kinds, 6, "the paid template is barred too, but the first figure is local templates with weights only");
  assert.deepEqual({ ...s.gates }, { blocked: 6, ack_required: 0, open: 4 });
});

// Kills: the strict tally using the normal class, a repo spelled two ways counted twice, every repo counted as mapped.
test("counts: the strict licence view treats an inferred entry as unknown, and a repo spelled two ways is one repo", () => {
  const m = (repo) => ({ nodes: [loaderNode(1, "UNETLoader", { name: "m.safetensors", repo, dir: "diffusion_models" })] });
  const s = C.summarizeCatalog(catalogFrom([
    { name: "one", workflow: m("example-org/weights-a") },
    { name: "two", workflow: m("Example-Org/weights-a") },
    { name: "three", workflow: m("example-org/weights-inferred") },
    { name: "four", workflow: m("example-org/not-in-map") },
    { name: "five", workflow: m("example-org/weights-c") },
  ]));
  assert.deepEqual({ ...s.license.local_needing_weights }, { permissive: 2, conditional: 1, non_commercial: 1, unknown: 1 });
  assert.deepEqual({ ...s.license.strict }, { permissive: 2, conditional: 0, non_commercial: 1, unknown: 2 });
  assert.deepEqual({ ...s.license.repos }, { distinct: 4, mapped: 3, unmapped: 1 });
});

// Kills: summary.orphan_files, summary.param_surface.all and summary.api_ids dropped.
test("counts: orphan files, the API node ids scanned, and the parameter surface over all templates and over local ones", () => {
  const sub = "a1111111-1111-4111-8111-111111111111";
  const withSurface = { nodes: [{ id: 1, type: sub, mode: 0, properties: { proxyWidgets: [["5", "text"]] } }], definitions: { subgraphs: [{ id: sub, nodes: [] }] } };
  const s = C.summarizeCatalog(catalogFrom(
    [{ name: "local_surface", workflow: withSurface }, { name: "api_surface", workflow: withSurface }, { name: "local_plain", workflow: { nodes: [] } }],
    { apiIds: new Set(["PaidNodeX", "PaidNodeY", "PaidNodeZ"]) },
    { "D:/x/t/stray.json": "{}", "D:/x/t/stray2.json": "{}" },
  ));
  assert.deepEqual({ ...s.param_surface }, { all: 2, local: 1 });
  assert.equal(s.orphan_files, 2);
  assert.equal(s.api_ids, 3);
  assert.deepEqual({ ...s.kinds }, { api: 1, custom_nodes: 0, local: 2 });
});


// ---------------------------------------------------------------------------------------------
// Paths, yaml, readiness, diff and snapshot
// ---------------------------------------------------------------------------------------------

// Kills: a backslash drive path (the usual Windows spelling in a yaml) or a lower-case drive letter no longer absolute,
// which would join it onto the yaml's own directory (D:\ComfyModels -> D:/x/comfy/D:/ComfyModels).
test("roots: a Windows base_path written with backslashes, or with a lower-case drive letter, is absolute", () => {
  const text = "p:\n  base_path: D:\\ComfyModels\n  vae: vae\nq:\n  base_path: e:/lower/case\n  loras: loras\nr:\n  base_path: F:\\Big\\Models\n  checkpoints: ckpt\\sub\n";
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text, dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "vae"), ["D:/x/comfy/models/vae", "D:/ComfyModels/vae", "D:/x/comfy/output/vae"]);
  assert.deepEqual(dirsOf(roots, "loras"), ["D:/x/comfy/models/loras", "e:/lower/case/loras", "D:/x/comfy/output/loras"]);
  assert.deepEqual(dirsOf(roots, "checkpoints"), ["D:/x/comfy/models/checkpoints", "F:/Big/Models/ckpt/sub", "D:/x/comfy/output/checkpoints"]);
});

// Kills: '.' kept, a leading '..' chain collapsed, '..' above an absolute root kept.
test("paths: normalizePath resolves '.' and '..', keeps a relative path's leading '..' and stops at the root", () => {
  assert.equal(C.normalizePath("D:/x/./y/../z"), "D:/x/z");
  assert.equal(C.normalizePath("../../a/b"), "../../a/b");
  assert.equal(C.normalizePath("a/../../b"), "../b");
  assert.equal(C.normalizePath("/../a"), "/a");
  assert.equal(C.normalizePath("D:/../a"), "D:/a");
});

// Kills: an absolute directory in a provider with no base_path joined onto the yaml dir instead of used as written.
test("roots: an absolute directory in a provider with no base_path is used as written", () => {
  const roots = C.resolveModelRoots({ comfyDir: "/srv/comfy", yamls: [{ text: "p:\n  loras: /mnt/loras\n  checkpoints: D:/ckpts\n", dir: "/srv/comfy" }] });
  assert.equal(dirsOf(roots, "loras")[1], "/mnt/loras");
  assert.equal(dirsOf(roots, "checkpoints")[1], "D:/ckpts");
});

// Kills: the case-insensitive env lookup lost (%userprofile% on Windows), an unknown variable turned into "undefined",
// a backslash home (~\) not expanded.
test("roots: variables resolve like Windows does, an unknown one stays as written, and ~\\ expands", () => {
  const text = "p:\n  base_path: %userprofile%/models\n  vae: vae\nq:\n  base_path: $NOPE/models\n  loras: loras\nr:\n  base_path: ~\\models\n  clip_vision: cv\n";
  const roots = C.resolveModelRoots({ comfyDir: "/srv/comfy", env: { USERPROFILE: "C:/fixture-home" }, home: "C:/fixture-home", yamls: [{ text, dir: "/srv/comfy" }] });
  assert.equal(dirsOf(roots, "vae")[1], "C:/fixture-home/models/vae");
  assert.equal(dirsOf(roots, "loras")[1], "/srv/comfy/$NOPE/models/loras");
  assert.equal(dirsOf(roots, "clip_vision")[1], "C:/fixture-home/models/cv");
});

// Kills: is_default accepting only lower-case `true` (Python-style `True` is what people write), a top-level scalar with
// indented lines under it crashing the parser, folded/chomped block scalars, quoted values with a trailing comment, escapes
// in quoted values, dotted provider names, and a 1-space indent read as a top-level key.
test("yaml: is_default takes YAML booleans, and unusual but valid shapes neither crash nor mis-parse", () => {
  const dflt = (v) => C.parseExtraModelPathsYaml(`p:\n  base_path: D:/x\n  is_default: ${v}\n`)[0].is_default;
  for (const v of ["true", "True", "TRUE", "yes", "on"]) assert.equal(dflt(v), true, v);
  for (const v of ["false", "no", "off", "0"]) assert.equal(dflt(v), false, v);
  assert.deepEqual(C.parseExtraModelPathsYaml("scalar: value\n  nested: line\n  vae: vae\n"), [], "a top-level scalar owns nothing");
  const folded = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  vae: >-\n    vae\n    vae2\n");
  assert.deepEqual(folded[0].entries.map((e) => e.paths), [["vae vae2"]], "a folded scalar is ONE value, its lines joined with a space (a literal `|` one is a path per line)");
  const q = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  vae: \"vae dir\"   # the note\n  loras: 'lo # ras' # note\n");
  assert.deepEqual(Object.fromEntries(q[0].entries.map((e) => [e.key, e.paths])), { vae: ["vae dir"], loras: ["lo # ras"] });
  const esc = C.parseExtraModelPathsYaml('p:\n  base_path: D:/x\n  vae: "say \\"hi\\" \\\\ ok"\n  loras: \'it\'\'s\'\n');
  assert.deepEqual(Object.fromEntries(esc[0].entries.map((e) => [e.key, e.paths])), { vae: ['say "hi" \\ ok'], loras: ["it's"] });
  assert.equal(C.parseExtraModelPathsYaml("my.models:\n  base_path: D:/x\n  vae: vae\n")[0].name, "my.models");
  const oneSpace = C.parseExtraModelPathsYaml("p:\n base_path: D:/x\n vae: vae\n");
  assert.deepEqual([oneSpace[0].base_path, oneSpace[0].entries.map((e) => e.key)], ["D:/x", ["vae"]]);
});

// ---- readiness ---------------------------------------------------------------------------------

// Kills: any boundary of the files-short histogram (2 folded into 1 or into 3-5, 3-5 running to 6, 6+ never used) and
// ready names left unsorted.
test("catalog readiness: the files-short histogram buckets 0, 1, 2, 3-5 and 6+, and the ready names come out sorted", () => {
  const row = (name, n) => ({ name, kind: "local", models: [{}], gate: { state: "open" }, short: n });
  const rows = [0, 1, 2, 3, 4, 5, 6, 7].map((n) => row(`t${n}`, n));
  const results = Object.fromEntries(rows.map((r) => [r.name, { ready: r.short === 0, not_ready_count: r.short }]));
  assert.deepEqual({ ...C.summarizeReadiness({ templates: rows }, results).histogram }, { "0": 1, "1": 1, "2": 1, "3-5": 3, "6+": 2 });
  const two = [row("zz", 0), row("aa", 0)];
  const ready = { zz: { ready: true, not_ready_count: 0 }, aa: { ready: true, not_ready_count: 0 } };
  assert.deepEqual(C.summarizeReadiness({ templates: two }, ready).ready.names, ["aa", "zz"]);
});

// Kills: wrong_class only found by exact name (a wrong-class file in another case or a subfolder reported missing);
// a class-less match naming only the first class it is found in.
test("readiness: a file under another class is found even when its case or folder differs, and a class-less match lists every class", () => {
  const inv = invFrom({ ...TREE, "D:/x/comfy/models/text_encoders/sub/Te3.safetensors": { size: 1 } });
  const cased = C.readiness([req("vae", "TE2.safetensors")], inv).requirements[0];
  assert.equal(cased.status, "wrong_class");
  assert.deepEqual(cased.found_in, [{ class: "text_encoders", rel: "te2.safetensors" }]);
  const sub = C.readiness([req("vae", "te3.safetensors")], inv).requirements[0];
  assert.equal(sub.status, "wrong_class");
  assert.deepEqual(sub.found_in, [{ class: "text_encoders", rel: "sub/Te3.safetensors" }]);
  const dup = invFrom({ "D:/x/comfy/models/vae/dup.safetensors": { size: 1 }, "D:/x/comfy/models/loras/dup.safetensors": { size: 1 } });
  const both = C.readiness([req(null, "dup.safetensors", { annotated: false })], dup).requirements[0];
  assert.equal(both.status, "present_class_unknown");
  assert.deepEqual(both.found_in.map((f) => f.class).sort(), ["loras", "vae"]);
});

// Kills: a node with no templates package crashing the readiness answer instead of being refused.
test("node readiness: a node that has no templates package at all is refused with the reason, not a crash", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const cat = C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, licenseMap: fixtureMap(), io: C.nodeIo });
  const r = C.nodeReadiness({ catalog: cat, snapshot: { package: null, inventory: invFrom(ACE_TREE) } });
  assert.match(r.refused, /reports no installed/);
  assert.equal(r.counts, undefined);
});

// ---- diff --------------------------------------------------------------------------------------

// Kills: `changed` needing BOTH an added and a removed file; kind_from/kind_to dropped; changed/added lists unsorted; a file
// moved between classes not counting as a change; basis.to reporting the FROM label.
test("diff: a template that only gains a file is changed; kinds, class moves, ordering and both basis labels are reported", () => {
  const w = (...ms) => ({ nodes: ms.map((m, i) => loaderNode(i + 1, "LoraLoader", m)) });
  const m = (name, dir = "loras") => ({ name, repo: "example-org/weights-a", dir });
  const paid = { id: 99, type: "PaidNodeX", mode: 0 };
  const a = catalogFrom([
    { name: "gains", workflow: w(m("one.safetensors")) },
    { name: "moves", workflow: w(m("x.safetensors", "loras")) },
    { name: "same", workflow: w(m("s.safetensors")) },
    { name: "zz_multi", workflow: w(m("k.safetensors")) },
    { name: "aa_multi", workflow: w(m("k.safetensors")) },
  ], { stamp: { label: "installed package (json 0.1.94)" } });
  const b = catalogFrom([
    { name: "gains", workflow: { nodes: [...w(m("one.safetensors"), m("new.safetensors")).nodes, paid] } },
    { name: "moves", workflow: w(m("x.safetensors", "text_encoders")) },
    { name: "same", workflow: w(m("s.safetensors")) },
    { name: "zz_multi", workflow: w(m("k.safetensors"), m("b-added.safetensors"), m("a-added.safetensors")) },
    { name: "aa_multi", workflow: w(m("k.safetensors"), m("y.safetensors")) },
  ], { stamp: { label: "package extract (json 0.1.96)" }, apiIds: new Set(["PaidNodeX"]) });
  const d = C.diffCatalogs(a, b);
  assert.deepEqual(d.changed.map((c) => c.name), ["aa_multi", "gains", "moves", "zz_multi"]);
  assert.deepEqual({ ...d.changed.find((c) => c.name === "gains") }, { name: "gains", kind_from: "local", kind_to: "api", added: ["loras/new.safetensors"], removed: [] });
  assert.deepEqual(d.changed.find((c) => c.name === "moves").added, ["text_encoders/x.safetensors"]);
  assert.deepEqual(d.changed.find((c) => c.name === "zz_multi").added, ["loras/a-added.safetensors", "loras/b-added.safetensors"]);
  assert.deepEqual({ ...d.basis }, { from: "installed package (json 0.1.94)", to: "package extract (json 0.1.96)" });
});

// Kills: regressions unsorted; the readiness method not reaching the "from" side, or the "to" side, of the comparison.
test("diff: regressions come out in name order, and the readiness method reaches both sides of the comparison", () => {
  const w = (...ms) => ({ nodes: ms.map((m, i) => loaderNode(i + 1, "VAELoader", m)) });
  const ae = { name: "ae.safetensors", repo: "example-org/weights-a", dir: "vae" };
  const gone = { name: "gone.safetensors", repo: "example-org/weights-a", dir: "vae" };
  const a = catalogFrom([{ name: "zz_regress", workflow: w(ae) }, { name: "aa_regress", workflow: w(ae) }, { name: "gain", workflow: w(gone) }]);
  const b = catalogFrom([{ name: "zz_regress", workflow: w(ae, gone) }, { name: "aa_regress", workflow: w(ae, gone) }, { name: "gain", workflow: w(ae) }]);
  const node = { inventory: invFrom({ "D:/x/comfy/models/checkpoints/ae.safetensors": { size: 1 } }) }; // the VAE is in the wrong class
  const loose = C.diffCatalogs(a, b, { snapshots: { n: node }, mode: "basename-only" });
  assert.deepEqual(loose.readiness.n.regressions.map((r) => r.name), ["aa_regress", "zz_regress"]);
  assert.deepEqual(loose.readiness.n.gains, ["gain"]);
  const strict = C.diffCatalogs(a, b, { snapshots: { n: node } });
  assert.deepEqual([strict.readiness.n.regressions, strict.readiness.n.gains], [[], []], "by directory nothing was ready to begin with");
});

// ---- snapshot ----------------------------------------------------------------------------------

// Kills: --base-directory ignored, or used as the models directory itself; --flag=value not understood; a flag whose next
// argument is another flag taking that flag as its value; the same yaml listed twice being read twice.
test("snapshot: --base-directory puts the models under <base>/models, flags may be --flag=value, and a value that is a flag is not a value", () => {
  const launch = (args) => memIo(comfyTree({ "D:/x/comfy/.offload-launch.json": JSON.stringify({ args }), "D:/x/more.yaml": "m:\n  base_path: D:/x/more\n  vae: vae\n" }));
  const eq = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--base-directory=D:/x/base", "--extra-model-paths-config=D:/x/more.yaml"]) });
  assert.equal(eq.launch.base_directory, "D:/x/base");
  assert.deepEqual(eq.launch.extra_model_paths_config, ["D:/x/more.yaml"]);
  assert.deepEqual(eq.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/base/models/vae", "D:/x/more/vae", "D:/x/base/output/vae"]);
  const spaced = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--base-directory", "D:/x/base"]) });
  assert.deepEqual(spaced.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/base/models/vae", "D:/x/base/output/vae"]);
  const flagAfterFlag = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--models-directory", "--port", "8188"]) });
  assert.equal(flagAfterFlag.launch.models_directory, null);
  const twice = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--extra-model-paths-config", "D:/x/comfy/extra_model_paths.yaml"]) });
  assert.equal(twice.yaml.length, 1, "the default yaml listed again is one file");
});

// Kills: relative entries of a launch-supplied yaml resolved against the ComfyUI dir instead of the yaml's own directory;
// yaml hash / provider count, package stamp and API source count dropped from the snapshot.
test("snapshot: a yaml elsewhere resolves its relative entries beside itself, and each yaml, the package and the API sources are recorded", () => {
  const io = memIo(comfyTree({
    "D:/x/comfy/.offload-launch.json": JSON.stringify({ args: ["main.py", "--extra-model-paths-config", "D:/x/cfg/more.yaml"] }),
    "D:/x/cfg/more.yaml": "m:\n  vae: relative_vae\n",
  }));
  const snap = C.snapshotNode({ comfyDir: "D:/x/comfy", io });
  assert.deepEqual(snap.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/comfy/models/vae", "D:/x/cfg/relative_vae", "D:/x/comfy/output/vae"]);
  assert.deepEqual({ ...snap.yaml[0] }, { path: "D:/x/comfy/extra_model_paths.yaml", readable: true, sha256: io.sha256("D:/x/comfy/extra_model_paths.yaml"), providers: 1 });
  assert.equal(snap.yaml[1].providers, 1);
  assert.equal(snap.package.stamp.basis, "installed-package");
  assert.equal(snap.package.stamp.package_version, "0.1.94");
  assert.equal(snap.api_nodes.files, 1);
});


// ---------------------------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------------------------

// Kills: -h and the `help` verb lost, no verb exiting 0, a verb that throws exiting 0, a value-less trailing flag accepted,
// an unknown --basis accepted, --comfy-dir with no package accepted.
test("cli: usage errors and failures have their own exit codes and name what is wrong", async () => {
  const help = await run(["-h"]);
  assert.equal(help.code, 0);
  assert.match(help.stdout, /usage: templates-catalog/);
  assert.equal((await run(["help"])).code, 0);
  const none = await run([]);
  assert.equal(none.code, 2);
  assert.match(none.stderr, /usage: templates-catalog/);
  const threw = await run(["summary", "--templates-dir", "D:/x/none"], memIo({}));
  assert.equal(threw.code, 1);
  assert.match(threw.stderr, /^summary: templates index unreadable/);
  const valueless = await run(["list", "--templates-dir", REAL_TEMPLATES, "--kind"]);
  assert.equal(valueless.code, 2);
  assert.match(valueless.stderr, /--kind needs a value/);
  const badBasis = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--basis", "nonsense"]);
  assert.equal(badBasis.code, 2);
  assert.match(badBasis.stderr, /unknown --basis "nonsense"/);
  const noPackage = await run(["summary", "--comfy-dir", "D:/x/empty"], memIo({ "D:/x/empty/readme.txt": { size: 1 } }));
  assert.equal(noPackage.code, 2);
  assert.match(noPackage.stderr, /no comfyui_workflow_templates_json package found under D:\/x\/empty/);
});

// A paid template with no api_ name and openSource true is caught only by the node id. Every existing --comfy-dir test
// asserts the basis and the entry count but not the API scan, so the scan could be switched off unnoticed.
// Kills: --comfy-dir not scanning the node's own comfy_api_nodes, the scan never run, the summary note always "none given".
test("cli: --comfy-dir scans the node's own comfy_api_nodes, so a paid template with no api_ name is caught by its node id", async () => {
  const site = "D:/x/comfy/.venv/Lib/site-packages";
  const files = {
    [`${site}/comfyui_workflow_templates_json-0.1.94.dist-info/METADATA`]: "",
    [`${site}/comfyui_workflow_templates_json/templates/index.json`]: JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [{ name: "sneaky", title: "S", mediaType: "image", openSource: true }] }]),
    [`${site}/comfyui_workflow_templates_json/templates/sneaky.json`]: JSON.stringify({ nodes: [{ id: 1, type: "PaidNodeOne", mode: 0 }] }),
    "D:/x/comfy/comfy_api_nodes/nodes_x.py": 'IO.Schema(node_id="PaidNodeOne")\nIO.Schema(node_id="PaidNodeTwo")\n',
  };
  const json = JSON.parse((await run(["summary", "--comfy-dir", "D:/x/comfy", "--json"], memIo(files))).stdout);
  assert.deepEqual([json.kinds.api, json.api_ids], [1, 2]);
  const text = (await run(["summary", "--comfy-dir", "D:/x/comfy"], memIo(files))).stdout;
  assert.match(text, /api node ids: 2 \(scanned 1 source files in the node's comfy_api_nodes\)/);
  const without = (await run(["summary", "--templates-dir", `${site}/comfyui_workflow_templates_json/templates`], memIo(files))).stdout;
  assert.match(without, /api node ids: 0 \(none given: only the api_ prefix and openSource:false mark paid templates\)/);
});

// ---- list --------------------------------------------------------------------------------------

const LIST_FILES = () => {
  const model = (name, repo) => ({ nodes: [loaderNode(1, "UNETLoader", { name, repo, dir: "diffusion_models" })] });
  return {
    "D:/x/license-map.json": readFileSync(FIXTURE_MAP_PATH, "utf8"),
    "D:/x/t/index.json": JSON.stringify([
      { moduleName: "default", title: "Image", type: "image", category: "Foundation", templates: [
        { name: "alpha_local", title: "Alpha Local", description: "Sunrise over hills", mediaType: "image", tags: ["Text to Image"], openSource: true, size: 1000 },
        { name: "beta_local", title: "Beta Local", description: "Studio portrait", mediaType: "image", tags: ["Portrait"], openSource: true },
      ] },
      { moduleName: "default", title: "Video", type: "video", category: "Foundation", templates: [
        { name: "gamma_video", title: "Gamma Video", description: "OCEAN waves", mediaType: "video", tags: ["Text to Video"], openSource: true },
        { name: "api_delta", title: "Delta Paid", description: "a partner node", mediaType: "video", openSource: false },
        { name: "api_zeta", title: "Zeta Paid", description: "another partner node", mediaType: "video", openSource: false },
        { name: "flux_epsilon", title: "Epsilon", description: "a barred thing", mediaType: "image", openSource: true },
      ] },
    ]),
    "D:/x/t/alpha_local.json": JSON.stringify(model("a.safetensors", "example-org/weights-a")),
    "D:/x/t/beta_local.json": JSON.stringify({ nodes: [] }),
    "D:/x/t/gamma_video.json": JSON.stringify(model("g.safetensors", "example-org/weights-b")),
    "D:/x/t/api_delta.json": JSON.stringify({ nodes: [] }),
    "D:/x/t/api_zeta.json": JSON.stringify({ nodes: [] }),
    "D:/x/t/flux_epsilon.json": JSON.stringify(model("e.safetensors", "example-org/weights-a")),
  };
};
const listNames = async (extra, files = LIST_FILES()) => {
  const r = await run(["list", "--templates-dir", "D:/x/t", "--license-map", "D:/x/license-map.json", ...extra, "--json"], memIo(files));
  assert.equal(r.code, 0, r.stderr);
  return JSON.parse(r.stdout);
};

// Kills: --group / --type / --text ignored or case-sensitive, a repeated filter keeping its first value, `--kind api` listing
// nothing (it is hidden by default), the hidden counts and the shown/total figures.
test("cli: list filters by group, type and text (any case), the last repeated flag wins, and --kind api still lists paid templates", async () => {
  assert.deepEqual((await listNames(["--group", "IMAGE"])).templates.map((t) => t.name), ["alpha_local", "beta_local"]);
  assert.deepEqual((await listNames(["--type", "video"])).templates.map((t) => t.name), ["gamma_video"]);
  assert.deepEqual((await listNames(["--text", "Ocean"])).templates.map((t) => t.name), ["gamma_video"]);
  assert.deepEqual((await listNames(["--text", "STUDIO"])).templates.map((t) => t.name), ["beta_local"]);
  assert.deepEqual((await listNames(["--group", "image", "--group", "video"])).templates.map((t) => t.name), ["gamma_video"]);
  assert.deepEqual((await listNames(["--kind", "api"])).templates.map((t) => t.name), ["api_delta", "api_zeta"]);
  const plain = await listNames([]);
  assert.deepEqual([plain.total, plain.shown, { ...plain.hidden }], [6, 3, { api: 2, flux: 1 }]);
});

// Kills: any field of the condensed list row wrong (license showing the gate, models counting inactive, custom_packs, group and
// type swapped, size and open_source dropped).
test("cli: list --json rows carry the template's kind, group, type, size, model count, licence class and gate", async () => {
  const alpha = (await listNames([])).templates.find((t) => t.name === "alpha_local");
  assert.deepEqual({ ...alpha }, { name: "alpha_local", title: "Alpha Local", kind: "local", group: "Image", type: "image", media_type: "image", tags: ["Text to Image"], size_bytes: 1000, open_source: true, models: 1, custom_packs: [], license: "permissive", gate: "open" });
  const gamma = (await listNames([])).templates.find((t) => t.name === "gamma_video");
  assert.equal(gamma.license, "conditional");
  assert.equal(gamma.gate, "ack_required");
  const packs = LIST_FILES();
  packs["D:/x/t/alpha_local.json"] = JSON.stringify({ nodes: [{ id: 1, type: "PackNode", mode: 0, properties: { cnr_id: "some-pack" } }, { id: 2, type: "UNETLoader", mode: 0, properties: { models: [{ name: "b.safetensors", directory: "diffusion_models", url: hf("example-org/weights-a", "b.safetensors") }, { name: "off.safetensors", directory: "loras", url: hf("example-org/weights-a", "off.safetensors") }] } }, { id: 3, type: "LoraLoader", mode: 4, properties: { models: [{ name: "off2.safetensors", directory: "loras", url: hf("example-org/weights-a", "off2.safetensors") }] } }] });
  const custom = (await listNames(["--kind", "custom_nodes"], packs)).templates[0];
  assert.deepEqual([custom.name, custom.custom_packs, custom.models], ["alpha_local", ["some-pack"], 2]);
});

// Kills: the header's paid/FLUX hidden counts swapped, the row showing the kind twice instead of the gate.
test("cli: list in text mode names how many templates are hidden and shows each row's kind, gate and file count", async () => {
  const r = await run(["list", "--templates-dir", "D:/x/t", "--license-map", "D:/x/license-map.json"], memIo(LIST_FILES()));
  assert.equal(r.code, 0, r.stderr);
  assert.match(r.stdout, /^basis: /);
  assert.match(r.stdout, /3 of 6 templates \(hidden: 2 paid API, 1 FLUX-family\)/);
  assert.match(r.stdout, /^gamma_video\s+local\s+ack_required\s+1 files\s+Gamma Video$/m);
  assert.match(r.stdout, /^alpha_local\s+local\s+open\s+1 files\s+Alpha Local$/m);
  assert.match(r.stdout, /^beta_local\s+local\s+open\s+0 files\s+Beta Local$/m);
});

// ---- the text summary --------------------------------------------------------------------------

// A catalog whose figures are all different, so a label that prints the wrong one shows.
function figureFiles() {
  const doc = loadJson(FIXTURE_MAP_PATH);
  doc.repos["example-org/weights-b"].fetched_on = "2026-03-05";
  doc.repos["example-org/weights-a"].fetched_on = "2026-01-02";
  const sub = "a1111111-1111-4111-8111-111111111111";
  const withSurface = (extra = {}) => ({ nodes: [{ id: 50, type: sub, mode: 0, properties: { proxyWidgets: [["5", "text"]] } }], definitions: { subgraphs: [{ id: sub, nodes: [] }] }, ...extra });
  const one = (repo, i, extra = {}) => ({ nodes: [loaderNode(1, "UNETLoader", { name: `m${i}.safetensors`, repo, dir: "diffusion_models" }, extra)] });
  const defs = [];
  const add = (name, workflow, entry = {}) => defs.push({ name, workflow, entry });
  for (let i = 1; i <= 4; i++) add(`local_perm_${i}`, one("example-org/weights-a", i));
  defs[0].workflow = withSurface({ nodes: [...withSurface().nodes, loaderNode(1, "UNETLoader", { name: "m1.safetensors", repo: "example-org/weights-a", dir: "diffusion_models" })] });
  add("local_hashed_a", { nodes: [{ id: 1, type: "VAELoader", mode: 0, properties: { models: [{ name: "h1.safetensors", directory: "vae", url: hf("example-org/weights-a", "h1.safetensors"), hash: "0".repeat(64), hash_type: "SHA256" }] } }] });
  add("local_hashed_b", { nodes: [{ id: 1, type: "VAELoader", mode: 0, properties: { models: [{ name: "h2.safetensors", directory: "vae", url: hf("example-org/weights-a", "h2.safetensors"), hash: "1".repeat(64), hash_type: "SHA256" }] } }] });
  for (let i = 1; i <= 3; i++) add(`local_cond_${i}`, one("example-org/weights-b", i));
  add("local_inferred", one("example-org/weights-inferred", 1));
  for (let i = 1; i <= 2; i++) add(`local_nc_${i}`, one("example-org/weights-c", i));
  for (let i = 1; i <= 6; i++) add(`local_unknown_${i}`, one(`example-org/unmapped-${i}`, i));
  add("local_zero_1", withSurface());
  add("local_zero_2", { nodes: [] });
  for (let i = 1; i <= 3; i++) add(`flux_named_${i}`, one("example-org/weights-a", i));
  add("local_repo_barred", one("example-org/flagged-family", 1));
  for (let i = 1; i <= 2; i++) add(`local_file_hit_${i}`, { nodes: [loaderNode(1, "UNETLoader", { name: `flux-part-${i}.safetensors`, repo: "example-org/weights-a", dir: "diffusion_models" })] });
  add("api_both_1", { nodes: [] }, { openSource: false });
  add("api_both_2", { nodes: [] }, { openSource: false });
  add("api_only_prefix", withSurface());
  add("id_paid_1", { nodes: [{ id: 1, type: "PaidNodeOne", mode: 0 }] });
  add("id_paid_2", { nodes: [{ id: 1, type: "PaidNodeTwo", mode: 0 }] });
  add("open_false_1", { nodes: [] }, { openSource: false });
  add("pack_1", { nodes: [{ id: 1, type: "PackNode", mode: 0, properties: { cnr_id: "some-pack" } }] });
  add("pack_2", { nodes: [] }, { requiresCustomNodes: ["other-pack"] });
  add("missing_file_1", undefined);
  const files = {
    "D:/x/license-map.json": JSON.stringify(doc),
    "D:/x/api/nodes.py": Array.from({ length: 7 }, (_, i) => `IO.Schema(node_id="${i < 2 ? ["PaidNodeOne", "PaidNodeTwo"][i] : `PaidNode${i}`}")`).join("\n"),
    "D:/x/t/stray_one.json": "{}",
    "D:/x/t/stray_two.json": "{}",
    "D:/x/t/index.json": JSON.stringify([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: defs.map((d) => ({ title: d.name, mediaType: "image", openSource: true, ...d.entry, name: d.name })) }]),
  };
  for (const d of defs) if (d.workflow) files[`D:/x/t/${d.name}.json`] = JSON.stringify(d.workflow);
  return files;
}
const figureArgs = ["--templates-dir", "D:/x/t", "--api-nodes-dir", "D:/x/api", "--license-map", "D:/x/license-map.json", "--basis", "templates-dir"];

// Kills: any row of the text summary printing the wrong figure (only the basis line and `entries` were ever asserted).
test("cli: the text summary prints, under each label, exactly the figure the JSON summary holds", async () => {
  const io = memIo(figureFiles());
  const s = JSON.parse((await run(["summary", ...figureArgs, "--json"], io)).stdout);
  const text = (await run(["summary", ...figureArgs], io)).stdout;
  const lic = (c) => `permissive ${c.permissive} | conditional ${c.conditional} | non_commercial ${c.non_commercial} | unknown ${c.unknown}`;
  const fl = s.flux.local_needing_weights;
  const rows = [
    ["entries", s.entries],
    ["api (paid partner nodes)", s.kinds.api],
    ["custom_nodes", s.kinds.custom_nodes],
    ["local (core nodes only)", s.kinds.local],
    ["needing weights", s.local.needing_weights],
    ["zero-model", s.local.zero_model],
    ["templates with subgraphs", s.subgraph_templates],
    ["with a parameter surface", `${s.param_surface.all} (local: ${s.param_surface.local})`],
    ["model annotations", `${s.model_annotations.entries} (with a hash: ${s.model_annotations.hashed})`],
    ["api signals (node id / api_ / openSource:false)", `${s.api_signals.by_node_id} / ${s.api_signals.by_name_prefix} / ${s.api_signals.by_open_source_false}`],
    ["api signal disagreements", s.api_signals.disagreements],
    ["licence class (local, needing weights)", lic(s.license.local_needing_weights)],
    ["strict: inferred map entries as unknown", lic(s.license.strict)],
    ["repos: distinct / in the map / unmapped", `${s.license.repos.distinct} / ${s.license.repos.mapped} / ${s.license.repos.unmapped}`],
    ["licence map", `${s.license.map.entries} repos, evidence ${s.license.map.evidence_dates[0]} .. ${s.license.map.evidence_dates[1]}`],
    ["FLUX bar (local, needing weights)", `by name ${fl.by_name}, by repo only ${fl.by_repo_only}, barred ${fl.barred}; FLUX-named file only ${fl.filename_only}`],
    ["gates (all templates)", `blocked ${s.gates.blocked} | ack required ${s.gates.ack_required} | open ${s.gates.open}`],
    ["workflow files not loaded", s.load_errors],
    ["files in the package not in the index", s.orphan_files],
  ];
  const esc = (v) => String(v).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  for (const [label, value] of rows) assert.match(text, new RegExp(`^\\s*${esc(label)}\\s+${esc(value)}$`, "m"), label);
  assert.match(text, new RegExp(`^api node ids: ${s.api_ids} \\(scanned 1 source files in the node's comfy_api_nodes\\)$`, "m"));
  assert.equal(text.split("\n")[0], `basis: ${s.basis}`);
  assert.notEqual(s.license.map.evidence_dates[0], s.license.map.evidence_dates[1], "the dataset must give two different evidence dates");
});

// ---- readiness ---------------------------------------------------------------------------------

const readinessBase = ["readiness", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--basis", "installed-package"];
const ZTREE_OK = {
  "D:/x/comfy/models/text_encoders/qwen_3_4b.safetensors": { size: 1 },
  "D:/x/comfy/models/diffusion_models/z_image_turbo_bf16.safetensors": { size: 1 },
  "D:/x/comfy/models/vae/ae.safetensors": { size: 1 },
};

// Kills: --detail inverted, per-template results never stripped, `readiness` with no snapshot succeeding, an unreadable
// snapshot skipped silently.
test("cli: readiness drops the per-template results unless --detail is given, and needs readable snapshots", async () => {
  const t = await tempFiles({ "alpha.json": snapshotFile(ACE_TREE, V094) });
  try {
    const plain = JSON.parse((await run([...readinessBase, "--snapshot", `${t.root}/alpha.json`, "--json"])).stdout);
    assert.equal(plain.nodes.alpha.results, undefined);
    assert.ok(plain.nodes.alpha.counts);
    const detailed = JSON.parse((await run([...readinessBase, "--snapshot", `${t.root}/alpha.json`, "--json", "--detail"])).stdout);
    assert.equal(detailed.nodes.alpha.results.audio_ace_step1_5_xl_turbo.ready, true);
    const none = await run(readinessBase);
    assert.equal(none.code, 2);
    assert.match(none.stderr, /give at least one --snapshot/);
    const unreadable = await run([...readinessBase, "--snapshot", `${t.root}/absent.json`]);
    assert.equal(unreadable.code, 2);
    assert.match(unreadable.stderr, /^snapshot .*absent\.json: /);
  } finally {
    t.cleanup();
  }
});

// Kills: any line of the text readiness table printing the wrong figure (only the basis line and one count were asserted),
// the candidate marker dropped, a refused node not printed, the `--mode both` delta heading or empty marker.
test("cli: the text readiness table prints each node's counts, gates, histogram and ready names as the JSON holds them", async () => {
  const wrongClass = { "D:/x/comfy/models/checkpoints/ae.safetensors": { size: 1 }, "D:/x/comfy/models/text_encoders/qwen_3_4b.safetensors": { size: 1 }, "D:/x/comfy/models/diffusion_models/z_image_turbo_bf16.safetensors": { size: 1 } };
  const t = await tempFiles({
    "alpha.json": snapshotFile({ ...ACE_TREE, ...ZTREE_OK }, V094),
    "beta.json": snapshotFile(wrongClass, V094),
    "old.json": snapshotFile(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }),
  });
  try {
    const snaps = ["--snapshot", `${t.root}/alpha.json`, "--snapshot", `${t.root}/beta.json`, "--snapshot", `${t.root}/old.json`];
    const json = JSON.parse((await run([...readinessBase, ...snaps, "--json"])).stdout);
    const text = (await run([...readinessBase, ...snaps])).stdout;
    assert.match(text, /^basis: installed package/);
    assert.match(text, /^mode: directory-aware$/m);
    for (const id of ["alpha", "beta"]) {
      const c = json.nodes[id].counts;
      const h = c.histogram;
      assert.match(text, new RegExp(`^${id}\\s+${c.ready.with_weights} ready with weights, ${c.ready.zero_model} zero-model \\(of ${c.needing_weights} needing weights, ${c.local} local\\)$`, "m"), id);
      assert.match(text, new RegExp(`^\\s+gates on the ready ones: open ${c.ready.by_gate.open} \\| ack required ${c.ready.by_gate.ack_required} \\| blocked ${c.ready.by_gate.blocked}$`, "m"));
      assert.match(text, new RegExp(`^\\s+files short \\(needing weights\\): 0: ${h["0"]}  1: ${h["1"]}  2: ${h["2"]}  3-5: ${h["3-5"]}  6\\+: ${h["6+"]}$`, "m"));
    }
    assert.match(text, /^\s+ready: audio_ace_step1_5_xl_turbo, image_z_image_turbo$/m);
    assert.equal(text.split("\n").filter((l) => /^\s+ready: /.test(l)).length, 1, "a node with nothing ready has no `ready:` line");
    assert.match(text, /^old\s+refused: the catalog was built from json 0\.1\.94 but the node has json 0\.1\.96$/m);
    const any = json.any_node;
    assert.match(text, new RegExp(`^any node: ${any.with_weights} ready with weights \\(open ${any.by_gate.open} \\| ack required ${any.by_gate.ack_required} \\| blocked ${any.by_gate.blocked}\\)$`, "m"));
    assert.notEqual(json.nodes.alpha.counts.histogram["0"], json.nodes.alpha.counts.histogram["1"], "the dataset must make the two buckets differ");
    // --candidate labels the refused node's numbers instead of refusing
    const cand = (await run([...readinessBase, "--snapshot", `${t.root}/old.json`, "--candidate"])).stdout;
    assert.match(cand, /^old\s+1 ready with weights, 1 zero-model .*\[candidate: package differs\]$/m);
    // --mode both
    const both = (await run([...readinessBase, "--snapshot", `${t.root}/alpha.json`, "--snapshot", `${t.root}/beta.json`, "--mode", "both"])).stdout;
    assert.match(both, /^mode: directory-aware$/m);
    assert.match(both, /^mode: basename-only$/m);
    assert.match(both, /^ready by basename only, not directory-aware:$/m);
    assert.match(both, /^  alpha: \(none\)$/m);
    assert.match(both, /^  beta: image_z_image_turbo$/m);
  } finally {
    t.cleanup();
  }
});

// ---- diff --------------------------------------------------------------------------------------

// Kills: diff needing a --snapshot, --mode not validated or its value lost from the JSON, --candidate-basis ignored, the
// candidate classified without the node's API ids, and every line of the diff text: added/removed counts, the per-template
// +/- lists, and the gains line.
test("cli: diff works without a snapshot, honours --candidate-basis and --mode, and prints added, removed, changed, breaks and gains apart", async () => {
  const cand = await candidateDir();
  const t = await tempFiles({
    "alpha.json": snapshotFile(ACE_TREE, V094),
    "beta.json": snapshotFile({ ...Object.fromEntries(Object.entries(ACE_TREE).filter(([p]) => !p.includes("ace_1.5_vae.safetensors"))), "D:/x/comfy/models/vae/ace_1.5_vae_v2.safetensors": { size: 1 } }, V094),
  });
  try {
    // a second addition, and a template the candidate makes paid by node id only (FixtureSecondNode is in the API fixture)
    const { readFileSync: rf, writeFileSync: wf } = await import("node:fs");
    const idx = JSON.parse(rf(`${cand.dir}/index.json`, "utf8"));
    idx[0].templates.push({ name: "synthetic_added_two", title: "Added two", mediaType: "image", openSource: true });
    wf(`${cand.dir}/index.json`, JSON.stringify(idx));
    wf(`${cand.dir}/synthetic_added_two.json`, JSON.stringify({ nodes: [] }));
    const z = JSON.parse(rf(`${cand.dir}/image_z_image_turbo.json`, "utf8"));
    z.nodes.push({ id: 900, type: "FixtureSecondNode", mode: 0 });
    z.nodes.push({ id: 901, type: "LoraLoader", mode: 0, properties: { models: [{ name: "extra-lora.safetensors", directory: "loras", url: hf("example-org/weights-a", "extra-lora.safetensors") }] } });
    wf(`${cand.dir}/image_z_image_turbo.json`, JSON.stringify(z));
    const base = ["diff", "--templates-dir", REAL_TEMPLATES, "--candidate-dir", cand.dir, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH];
    const bare = await run([...base, "--json", "--candidate-basis", "package-extract"]);
    assert.equal(bare.code, 0, bare.stderr);
    const out = JSON.parse(bare.stdout);
    assert.equal(out.mode, "directory-aware");
    assert.match(out.basis.to, /^package extract /);
    assert.deepEqual(out.readiness, {});
    const changedZ = out.changed.find((c) => c.name === "image_z_image_turbo");
    assert.deepEqual({ ...changedZ }, { name: "image_z_image_turbo", kind_from: "local", kind_to: "api", added: ["loras/extra-lora.safetensors"], removed: [] });
    const basename = JSON.parse((await run([...base, "--snapshot", `${t.root}/alpha.json`, "--mode", "basename-only", "--json"])).stdout);
    assert.equal(basename.mode, "basename-only");
    const bogus = await run([...base, "--mode", "sideways"]);
    assert.equal(bogus.code, 2);
    assert.match(bogus.stderr, /unknown --mode "sideways"/);
    const text = (await run([...base, "--snapshot", `${t.root}/alpha.json`, "--snapshot", `${t.root}/beta.json`])).stdout;
    assert.match(text, /^added\s+2$/m);
    assert.match(text, /^removed\s+1$/m);
    assert.match(text, /^changed \(required files differ\)\s+2$/m);
    assert.match(text, /^  audio_ace_step1_5_xl_turbo: \+\["vae\/ace_1\.5_vae_v2\.safetensors"\] -\["vae\/ace_1\.5_vae\.safetensors"\]$/m);
    assert.match(text, /^would break on alpha:\s+audio_ace_step1_5_xl_turbo$/m);
    assert.match(text, /^would gain on alpha:\s+\(none\)$/m);
    assert.match(text, /^would break on beta:\s+\(none\)$/m);
    assert.match(text, /^would gain on beta:\s+audio_ace_step1_5_xl_turbo$/m);
  } finally {
    cand.cleanup();
    t.cleanup();
  }
});

// Kills: snapshot --label ignored.
test("cli: snapshot --label stamps the snapshot with the node's name", async () => {
  const r = await run(["snapshot", "--comfy-dir", "D:/x/comfy", "--label", "node-a"], memIo(comfyTree()));
  assert.equal(r.code, 0, r.stderr);
  assert.equal(JSON.parse(r.stdout).label, "node-a");
});


// ---------------------------------------------------------------------------------------------
// The committed licence map is policy data: the gate stands on it
// ---------------------------------------------------------------------------------------------

// Only 5 of the map's 50 entries had their class asserted, so a restricted repo downgraded to `permissive` (gate: open)
// stayed green in CI (18 such flips survived). Two pins close that without pinning every permissive repo:
//  1. RESTRICTED is every entry that is not permissive, with its class. Loosening one, or adding a restriction without saying so,
//     fails here; adding a permissive entry needs no edit. Change it together with the map entry and its evidence.
//  2. A permissive entry must carry a licence id from PERMISSIVE_IDS, so a restricted licence filed as permissive (an `other` or a
//     non-commercial id) is caught without listing each repo.
const RESTRICTED = {
  "circlestone-labs/Anima": "non_commercial",
  "Comfy-Org/Anima-LLLite": "non_commercial",
  "Comfy-Org/flux1-dev": "non_commercial",
  "Comfy-Org/flux2-dev": "non_commercial",
  "Comfy-Org/flux2-klein-9B": "non_commercial",
  "Comfy-Org/hunyuan3D_2.0_repackaged": "conditional",
  "Comfy-Org/HunyuanImage_2.1_ComfyUI": "conditional",
  "Comfy-Org/HunyuanVideo_1.5_repackaged": "conditional",
  "Comfy-Org/Ideogram-4": "non_commercial",
  "Comfy-Org/Krea-2": "conditional",
  "Comfy-Org/ltx-2": "conditional",
  "Comfy-Org/ltx-2.3": "conditional",
  "Comfy-Org/MiniMax-H3": "conditional",
  "Comfy-Org/Qwen-Image-2.1": "non_commercial",
  "Comfy-Org/sam3.1": "conditional",
  "Comfy-Org/stable-audio-3": "conditional",
  "Comfy-Org/stable-diffusion-3.5-fp8": "conditional",
  "Comfy-Org/YuE2": "non_commercial",
  "FastVideo/FastVideo-FastH3-Comfy": "conditional",
  "Kijai/WanVideo_comfy": "unknown",
  "Lightricks/LTX-2": "conditional",
  "Lightricks/LTX-2.3": "conditional",
  "Lightricks/LTX-2.5": "conditional",
  "lightx2v/Minimax-h3-Turbo": "conditional",
};
const PERMISSIVE_IDS = ["apache-2.0", "mit", "openrail++"];
const COMMITTED_FLUX_BAR = {
  "Comfy-Org/flux1-dev": true,
  "Comfy-Org/flux2-dev": true,
  "Comfy-Org/flux2-klein-9B": true,
  "comfyanonymous/flux_text_encoders": false,
};

// Kills: any restricted repo's class changed (a restricted one downgraded to permissive most of all), a permissive one turned
// restricted or unknown without this table, a restriction added or dropped without it, any flux_bar flag flipped, and a permissive
// entry on a licence that is not permissive.
test("license map (committed): the restricted entries and the flux_bar flags are the reviewed ones, and a permissive entry has a permissive licence", () => {
  const map = C.loadLicenseMap(loadJson(REAL_MAP_PATH));
  const restricted = Object.fromEntries(map.entries().filter(([, e]) => e.class !== "permissive").map(([repo, e]) => [repo, e.class]));
  assert.deepEqual(restricted, RESTRICTED);
  const flags = Object.fromEntries(map.entries().filter(([, e]) => e.flux_bar !== undefined).map(([repo, e]) => [repo, e.flux_bar]));
  assert.deepEqual(flags, COMMITTED_FLUX_BAR);
  for (const [repo, e] of map.entries()) {
    if (e.class === "permissive") assert.ok(PERMISSIVE_IDS.includes(e.license_id), `${repo}: a permissive entry carries a permissive licence id, not ${JSON.stringify(e.license_id)}`);
  }
});

// ADR 0011: FLUX-family templates are barred. A flux_bar:false on the wrong repo would exempt one. Kills: flux_bar true -> false
// on any FLUX repo of the committed map (three of them survived in CI, one only died in the golden run).
test("license map (committed): every FLUX-family repo is barred through the committed map; the text-encoder repo is the one exemption", () => {
  const map = C.loadLicenseMap(loadJson(REAL_MAP_PATH));
  const barred = (repo) => C.fluxAssessment({ name: "image_ok", models: [mdl(repo)] }, map).barred;
  for (const [repo] of map.entries()) {
    if (repo.toLowerCase() === "comfyanonymous/flux_text_encoders") assert.equal(barred(repo), false, repo);
    else if (/flux/i.test(repo) || repo.toLowerCase().startsWith("black-forest-labs/")) assert.equal(barred(repo), true, `${repo} is FLUX-family and must stay barred`);
  }
  for (const repo of ["black-forest-labs/FLUX.1-dev", "Comfy-Org/flux2-dev", "Comfy-Org/flux1-dev", "Comfy-Org/flux2-klein-9B", "someone/Flux.2-Turbo-ComfyUI"]) assert.equal(barred(repo), true, repo);
});


// ---------------------------------------------------------------------------------------------
// Leftovers
// ---------------------------------------------------------------------------------------------

// Kills: the default subgraph depth cap lowered (8 -> 2 or 1). Real templates nest at most two deep, so nothing else pins it.
test("walk: the default depth cap lets an eight-deep nest through and cuts a ninth level, flagging it", () => {
  const id = (k) => `a${k}111111-1111-4111-8111-111111111111`;
  const chain = (n) => ({
    nodes: [{ id: 1, type: id(1), mode: 0 }],
    definitions: { subgraphs: Array.from({ length: n }, (_, k) => ({ id: id(k + 1), nodes: [k + 1 < n ? { id: 10 + k, type: id(k + 2), mode: 0 } : { id: 99, type: "KSampler", mode: 0 }] })) },
  });
  assert.deepEqual(C.flattenActiveNodes(chain(8)).map((l) => [l.node.type, l.depth, Boolean(l.unexpanded)]), [["KSampler", 8, false]]);
  const nine = C.flattenActiveNodes(chain(9));
  assert.equal(nine.length, 1);
  assert.equal(nine[0].unexpanded, true);
});

// Kills: a base_path written as a block scalar becoming a model class of its own.
test("yaml: a base_path given as a block scalar is not a model class", () => {
  const p = C.parseExtraModelPathsYaml("p:\n  base_path: |\n    D:/x\n  vae: vae\n");
  assert.deepEqual([p[0].base_path, p[0].entries.map((e) => e.key)], [null, ["vae"]]);
});


// ---------------------------------------------------------------------------------------------
// More leftovers
// ---------------------------------------------------------------------------------------------

// Kills: junk entries in a group's template list (null, numbers, strings) becoming phantom rows with no name.
test("index: junk inside a group's template list is skipped, not turned into rows", () => {
  const flat = C.flattenIndex([{ moduleName: "default", title: "G", type: "image", category: "Foundation", templates: [null, 5, "x", { name: "ok" }] }]);
  assert.deepEqual(flat.map((e) => e.name), ["ok"]);
});

// Kills: strict_downgraded reported for an inferred entry whose class is already unknown (nothing was downgraded).
test("license class: an inferred entry that is already unknown is not reported as downgraded", () => {
  const map = C.loadLicenseMap({ repos: { "a/inf": { class: "unknown", basis: "inferred from a/other (not fetched)", conditions: "not read" } } });
  const r = C.licenseAssessment([mdl("a/inf")], map).repos[0];
  assert.equal(r.class, "unknown");
  assert.equal(r.strict_downgraded, false);
});

// Kills: the yaml parser no longer stripping a BOM itself (a caller that hands it a BOM'd string).
test("yaml: a leading BOM does not hide the first provider", () => {
  const p = C.parseExtraModelPathsYaml("\uFEFFfirst:\n  base_path: D:/x\n  vae: vae\n");
  assert.deepEqual(p.map((x) => x.name), ["first"]);
});


// ---------------------------------------------------------------------------------------------
// Ordering, junk tolerance and the verbs nothing ran
// ---------------------------------------------------------------------------------------------

// Every `.sort()` in the module exists so output does not depend on how a filesystem lists a directory. All 15 `.sort()`
// removals survived the original suite, because in-memory trees list in insertion order and real ones alphabetically.
// Kills: added/removed templates, removed files, gains and the any-node ready names left in discovery order.
test("diff: added and removed templates, removed files and gains come out sorted", () => {
  const w = (...names) => ({ nodes: names.map((name, i) => loaderNode(i + 1, "VAELoader", { name, repo: "example-org/weights-a", dir: "vae" })) });
  const a = catalogFrom([
    { name: "zz_gone", workflow: w("g1.safetensors") }, { name: "aa_gone", workflow: w("g2.safetensors") },
    { name: "zz_gain", workflow: w("missing1.safetensors") }, { name: "aa_gain", workflow: w("missing2.safetensors") },
    { name: "multi", workflow: w("z.safetensors", "a.safetensors") },
  ]);
  const b = catalogFrom([
    { name: "zz_gain", workflow: w("ae.safetensors") }, { name: "aa_gain", workflow: w("ae.safetensors") },
    { name: "zz_new", workflow: w("n1.safetensors") }, { name: "aa_new", workflow: w("n2.safetensors") },
    { name: "multi", workflow: w("keep.safetensors") },
  ]);
  const d = C.diffCatalogs(a, b, { snapshots: { n: { inventory: invFrom({ "D:/x/comfy/models/vae/ae.safetensors": { size: 1 } }) } } });
  assert.deepEqual(d.added, ["aa_new", "zz_new"]);
  assert.deepEqual(d.removed, ["aa_gone", "zz_gone"]);
  assert.deepEqual(d.changed.find((c) => c.name === "multi").removed, ["vae/a.safetensors", "vae/z.safetensors"]);
  assert.deepEqual(d.readiness.n.gains, ["aa_gain", "zz_gain"]);
});

// Kills: any-node ready names in discovery order; readinessTable answering a mismatched node by default (candidate defaults to true).
test("readiness table: the any-node names are sorted across nodes, and a node with another package is refused by default", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const cat = C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, licenseMap: fixtureMap(), io: C.nodeIo });
  const table = C.readinessTable({ catalog: cat, snapshots: { first: nodeSnapshot(ZTREE_OK), second: nodeSnapshot(ACE_TREE), old: nodeSnapshot(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }) } });
  assert.deepEqual(table.any_node.names, ["audio_ace_step1_5_xl_turbo", "image_z_image_turbo"]);
  assert.match(table.nodes.old.refused, /0\.1\.96/);
  assert.equal(table.nodes.old.counts, undefined);
});

// Kills: a wrong-class match listing its classes in another order.
test("readiness: a file that sits in several other classes lists them in the class table's order", () => {
  const inv = invFrom({ "D:/x/comfy/models/loras/x.safetensors": { size: 1 }, "D:/x/comfy/models/checkpoints/x.safetensors": { size: 1 } });
  const r = C.readiness([req("vae", "x.safetensors")], inv).requirements[0];
  assert.equal(r.status, "wrong_class");
  assert.deepEqual(r.found_in.map((f) => f.class), ["checkpoints", "loras"]);
});

// Kills: custom pack names left unsorted.
test("catalog: the custom packs a template needs are listed by name", () => {
  const row = rowFor({ nodes: [{ id: 1, type: "A", mode: 0, properties: { cnr_id: "zeta-pack" } }, { id: 2, type: "B", mode: 0, properties: { cnr_id: "alpha-pack" } }] });
  assert.deepEqual(row.custom_packs, ["alpha-pack", "zeta-pack"]);
  assert.equal(row.kind, "custom_nodes");
});

// Kills: launch-supplied yaml files read in reverse (their order is the directory order), --flag=value entries kept in reverse,
// two keys of one provider that map to the same class registered in reverse.
test("snapshot: extra yaml files load in the order the launch record lists them; keys of one provider register in file order", () => {
  const io = memIo(comfyTree({
    "D:/x/comfy/.offload-launch.json": JSON.stringify({ args: ["main.py", "--extra-model-paths-config=D:/x/one.yaml", "--extra-model-paths-config=D:/x/two.yaml"] }),
    "D:/x/one.yaml": "a:\n  base_path: D:/x/one\n  vae: vae\n",
    "D:/x/two.yaml": "b:\n  base_path: D:/x/two\n  vae: vae\n",
  }));
  const snap = C.snapshotNode({ comfyDir: "D:/x/comfy", io });
  assert.deepEqual(snap.launch.extra_model_paths_config, ["D:/x/one.yaml", "D:/x/two.yaml"]);
  assert.deepEqual(snap.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/comfy/models/vae", "D:/x/one/vae", "D:/x/two/vae", "D:/x/comfy/output/vae"]);
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "p:\n  base_path: D:/x\n  unet: first\n  diffusion_models: |\n    second\n  other: o\n", dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "diffusion_models").slice(2, 4), ["D:/x/first", "D:/x/second"]);
});

// The `catalog` verb (the full catalog as JSON) was never invoked by any test. Kills: it returning nothing / the wrong exit code.
test("cli: the catalog verb prints the whole catalog as JSON", async () => {
  const r = await run(["catalog", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--basis", "installed-package"]);
  assert.equal(r.code, 0, r.stderr);
  const cat = JSON.parse(r.stdout);
  assert.equal(cat.schema_version, 1);
  assert.equal(cat.templates.length, 8);
  assert.match(cat.stamp.label, /^installed package/);
  assert.deepEqual(cat.orphan_files, []);
  assert.equal((await run(["catalog"])).code, 2, "no source given is a usage error");
});

// Kills: exit 3 only when EVERY node is refused (.some -> .every), an extra blank line ending text output, in stdout or in --out.
test("cli: one refused node among accepted ones still exits 3; text output ends with exactly one newline", async () => {
  const t = await tempFiles({ "alpha.json": snapshotFile(ACE_TREE, V094), "old.json": snapshotFile(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }) });
  try {
    const mixed = await run([...readinessBase, "--snapshot", `${t.root}/alpha.json`, "--snapshot", `${t.root}/old.json`]);
    assert.equal(mixed.code, 3);
    assert.equal((await run([...readinessBase, "--snapshot", `${t.root}/alpha.json`, "--snapshot", `${t.root}/old.json`, "--mode", "both"])).code, 3);
    const text = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--out", `${t.root}/summary.txt`]);
    assert.match(text.stdout, /[^\n]\n$/);
    assert.equal(nfs.readFileSync(`${t.root}/summary.txt`, "utf8"), text.stdout);
  } finally {
    t.cleanup();
  }
});

// The parser and readers are documented to ignore what they do not understand. Kills: a yaml line with no colon crashing the
// parser, and junk annotations / proxied widgets / note values crashing or being kept.
test("junk: stray yaml lines, malformed annotations, malformed proxied widgets and non-text notes are skipped, not fatal", () => {
  const p = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  just some words\n  - a list item\n  vae: vae\n");
  assert.deepEqual(p[0].entries.map((e) => e.key), ["vae"]);
  const wf = { nodes: [{ id: 1, type: "VAELoader", mode: 0, properties: { models: [null, 5, "x", { name: 3 }, {}, { name: "ok.safetensors", directory: "vae" }] } }] };
  assert.deepEqual(C.requiredModels(wf).active.map((m) => m.name), ["ok.safetensors"]);
  assert.equal(rowFor(wf).graph.model_annotation_entries, 1);
  const sub = "a1111111-1111-4111-8111-111111111111";
  const surface = C.paramSurface({ nodes: [{ id: 1, type: sub, properties: { proxyWidgets: [["1", 5], [{}, "w"], [null, "w"], ["2", "ok"]] } }], definitions: { subgraphs: [{ id: sub, nodes: [] }] } });
  assert.deepEqual(surface.map((s) => `${s.node}:${s.widget}`), ["2:ok"]);
  assert.deepEqual(C.parseNoteSizes({ nodes: [{ id: 1, type: "MarkdownNote", mode: 0, widgets_values: [5] }, { id: 2, type: "Note", mode: 0 }] }), {});
});


// ---------------------------------------------------------------------------------------------
// The documented "pipe it anywhere" mode
// ---------------------------------------------------------------------------------------------

// The header and the docs promise the whole file can be piped to `node --input-type=module -` on a node with nothing else
// deployed. Under that form process.argv[1] is "-" and import.meta.url is a synthetic [eval1] URL, so the comparison of the entry
// point with this file's own URL never matched: the piped file printed nothing and exited 0.
test("entry: the whole file piped to `node --input-type=module -` runs the verb it is given, as the file run directly does", () => {
  const t = scratch("tc-pipe");
  try {
    nfs.mkdirSync(`${t.root}/comfy/models/vae`, { recursive: true });
    nfs.writeFileSync(`${t.root}/comfy/models/vae/ae.safetensors`, "x");
    nfs.writeFileSync(`${t.root}/comfy/comfyui_version.py`, '__version__ = "0.37.0"\n');
    const piped = (args) => spawnSync(process.execPath, ["--input-type=module", "-", ...args], { input: SOURCE, encoding: "utf8", cwd: t.root });
    const snapArgs = ["snapshot", "--comfy-dir", `${t.root}/comfy`];
    const viaPipe = piped(snapArgs);
    assert.equal(viaPipe.status, 0, viaPipe.stderr);
    assert.notEqual(viaPipe.stdout, "", "no output at all means the entry-point guard never fired");
    const direct = spawnModule(snapArgs);
    assert.equal(direct.status, 0, direct.stderr);
    assert.equal(viaPipe.stdout, direct.stdout, "the piped file takes the same snapshot as the file run directly");
    assert.deepEqual(JSON.parse(viaPipe.stdout).inventory.classes.vae.files.map((f) => f.rel), ["ae.safetensors"]);
    const usage = piped(["frobnicate"]);
    assert.equal(usage.status, 2, "a usage error reaches the shell as exit 2");
    assert.match(usage.stderr, /unknown verb/);
    assert.equal(piped([]).status, 2, "no verb is a usage error too");
  } finally {
    t.cleanup();
  }
});

// Nothing sits beside a piped file, so it has no licence map of its own: the verbs that read one count every repo as unknown
// (the summary's map line says `0 repos`), and --license-map FILE names a map that is on that node. The map is looked up beside the
// file, never in whatever directory the pipe happens to run in.
test("entry: piped, the file has no licence map of its own and --license-map supplies one", () => {
  const t = scratch("tc-pipe-map");
  try {
    nfs.copyFileSync(REAL_MAP_PATH, `${t.root}/templates-license-map.json`);
    const piped = (args) => spawnSync(process.execPath, ["--input-type=module", "-", "summary", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json", ...args], { input: SOURCE, encoding: "utf8", cwd: t.root });
    const bare = piped([]);
    assert.equal(bare.status, 0, bare.stderr);
    assert.deepEqual({ ...JSON.parse(bare.stdout).license.map }, { entries: 0, evidence_dates: [] }, "a map in the working directory is not the file's own");
    const withMap = piped(["--license-map", FIXTURE_MAP_PATH]);
    assert.equal(withMap.status, 0, withMap.stderr);
    assert.equal(JSON.parse(withMap.stdout).license.map.entries, 9);
  } finally {
    t.cleanup();
  }
});

// Not a RED test: the fix for the two above must not make a script that merely imports the module run its command line. A script
// piped to node has argv[1] "-" as well.
test("entry: a script piped to node that imports the module does not run the command line", () => {
  const source = `import { main } from ${JSON.stringify(pathToFileURL(MODULE_PATH).href)};\nconsole.log(typeof main);\n`;
  const r = spawnSync(process.execPath, ["--input-type=module", "-", "frobnicate"], { input: source, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout, "function\n");
  assert.equal(r.stderr, "");
});


// ---------------------------------------------------------------------------------------------
// Residue of the mechanical sweeps
// ---------------------------------------------------------------------------------------------

// Kills: quote handling on malformed values (a value that only starts or ends with a quote, a lone quote, an empty pair);
// a `#` inside a double-quoted value cut as a comment; the block-scalar reset (a block followed by a provider indented
// differently was read as more of the block); two scalar keys of one class registered in reverse; `is_default` as a block.
test("yaml: quotes that do not pair are left as written, # inside quotes is not a comment, and a block scalar ends where it ends", () => {
  const one = (v) => C.parseExtraModelPathsYaml(`p:\n  base_path: D:/x\n  vae: ${v}\n`)[0].entries[0].paths[0];
  assert.equal(one('"abc'), '"abc');
  assert.equal(one('abc"'), 'abc"');
  assert.equal(one("'abc"), "'abc");
  assert.equal(one("abc'"), "abc'");
  assert.equal(one('"'), '"');
  assert.equal(one('""'), "");
  assert.equal(one("''"), "");
  assert.equal(one('"a # b" # note'), "a # b");
  const mixed = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  vae: |\n    one\n    two\nq:\n    base_path: D:/y\n    loras: loras\n");
  assert.deepEqual(mixed.map((x) => [x.name, x.base_path, x.entries.map((e) => `${e.key}=${e.paths.join("+")}`)]), [["p", "D:/x", ["vae=one+two"]], ["q", "D:/y", ["loras=loras"]]]);
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "p:\n  base_path: D:/x\n  unet: first\n  diffusion_models: second\n", dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "diffusion_models").slice(2, 4), ["D:/x/first", "D:/x/second"]);
  const flag = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  is_default: |\n    true\n  vae: vae\n");
  assert.deepEqual([flag[0].is_default, flag[0].entries.map((e) => e.key)], [false, ["vae"]]);
});

// Kills: the map validators weakening (each rejects with its own message), the error text itself.
test("license map: each kind of malformed map is refused with its own message", () => {
  const notObject = { message: "license map: not an object" };
  const reposShape = { message: "license map: `repos` must be an object keyed by repo id" };
  const classShape = { message: "license map: a/b: class must be one of permissive, conditional, non_commercial, unknown" };
  assert.throws(() => C.loadLicenseMap([]), notObject);
  assert.throws(() => C.loadLicenseMap("text"), notObject);
  assert.throws(() => C.loadLicenseMap({ repos: "x" }), reposShape);
  assert.throws(() => C.loadLicenseMap({}), reposShape);
  assert.throws(() => C.loadLicenseMap({ repos: { "a/b": null } }), classShape);
  assert.throws(() => C.loadLicenseMap({ repos: { "a/b": "permissive" } }), classShape);
});

// Kills: the default method string of readiness, nodeReadiness, readinessTable dropped or renamed (each reports the method it used).
test("readiness: every entry point reports the method it used, directory-aware by default", () => {
  const stamp = C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo });
  const cat = C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp, licenseMap: fixtureMap(), io: C.nodeIo });
  assert.equal(C.catalogReadiness(cat, invFrom(ACE_TREE)).audio_ace_step1_5_xl_turbo.mode, "directory-aware");
  assert.equal(C.nodeReadiness({ catalog: cat, snapshot: nodeSnapshot(ACE_TREE) }).mode, "directory-aware");
  assert.equal(C.readinessTable({ catalog: cat, snapshots: { a: nodeSnapshot(ACE_TREE) } }).mode, "directory-aware");
  assert.equal(C.readinessTable({ catalog: cat, snapshots: { a: nodeSnapshot(ACE_TREE) }, mode: "basename-only" }).nodes.a.mode, "basename-only");
});

// Kills: `readiness --mode basename-only` refused as an unknown mode; the no-source message changed.
test("cli: readiness accepts --mode basename-only on its own, and a missing source says what to give", async () => {
  const t = await tempFiles({ "alpha.json": snapshotFile(ACE_TREE, V094) });
  try {
    const r = await run([...readinessBase, "--snapshot", `${t.root}/alpha.json`, "--mode", "basename-only", "--json"]);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(JSON.parse(r.stdout).mode, "basename-only");
  } finally {
    t.cleanup();
  }
  const none = await run(["catalog"]);
  assert.equal(none.code, 2);
  assert.equal(none.stderr, "give --templates-dir or --comfy-dir\n");
});

// Kills: the candidate's stamp built from the wrong directory (the diff's "to" basis then loses the candidate's versions).
test("cli: diff labels the candidate side with the candidate package's own versions", async () => {
  const cand = await candidateDir();
  try {
    const { mkdirSync: mk, writeFileSync: wf } = await import("node:fs");
    mk(`${cand.root}/comfyui_workflow_templates_json-0.1.96.dist-info`, { recursive: true });
    wf(`${cand.root}/comfyui_workflow_templates_json-0.1.96.dist-info/METADATA`, "");
    const r = await run(["diff", "--templates-dir", REAL_TEMPLATES, "--candidate-dir", cand.dir, "--api-nodes-dir", API_DIR, "--license-map", FIXTURE_MAP_PATH, "--basis", "installed-package", "--candidate-basis", "package-extract", "--json"]);
    assert.equal(r.code, 0, r.stderr);
    const out = JSON.parse(r.stdout);
    assert.equal(out.basis.to, "package extract (json 0.1.96)");
    assert.match(out.basis.from, /^installed package \(.*json 0\.1\.94\)$/);
  } finally {
    cand.cleanup();
  }
});


// ---------------------------------------------------------------------------------------------
// Partner-node ids declared in the other ways ComfyUI spells them
// ---------------------------------------------------------------------------------------------

// nodes_bfl.py declares a node's id as a class attribute its schema reads back (`node_id=cls.NODE_ID`), and nodes_comfy_cloud.py builds
// its schemas through a helper that takes the id as its first argument. The scan took only `node_id="X"`, so 5 of the 292 ids of a
// ComfyUI 0.37.0 tree were missing, and only the redundant api_ prefix kept their templates classed as paid.
test("api ids: a class-attribute NODE_ID and a literal first argument of _cloud_schema are declarations too", () => {
  const src = [
    "class FluxKontextProImageNode(IO.ComfyNode):",
    '    NODE_ID = "FluxKontextProImageNode"',
    "    @classmethod",
    "    def define_schema(cls):",
    "        return IO.Schema(node_id=cls.NODE_ID, display_name='x')",
    "",
    "class ComfyCloudMusicNode(IO.ComfyNode):",
    "    @classmethod",
    "    def define_schema(cls):",
    "        return _cloud_schema(",
    '            "ComfyCloudMusicNode",',
    '            "Comfy Cloud Music",',
    "        )",
    "",
    "class ComfyCloudImageNode(IO.ComfyNode):",
    '    node_id = "ComfyCloudImageNode"',
    "",
    "def _cloud_schema(node_id: str, display_name: str):",
    "    return IO.Schema(node_id=node_id, display_name=display_name)",
    "def _video_schema(node_id: str, display_name: str):",
    "    return _cloud_schema(node_id, display_name)",
    "MY_NODE_ID = 'NotThis'",
    "OTHER_cloud_schema('NorThis')",
  ].join("\n");
  assert.deepEqual([...C.parseApiNodeIds(src)].sort(), ["ComfyCloudImageNode", "ComfyCloudMusicNode", "FluxKontextProImageNode"]);
});

test("api ids: a paid template that says nothing but the node id of a class-attribute node is caught by that id alone", () => {
  const io = memIo({
    "D:/x/c/comfy_api_nodes/nodes_bfl.py": 'class Flux2ProImageNode(IO.ComfyNode):\n    NODE_ID = "Flux2ProImageNode"\n    @classmethod\n    def define_schema(cls):\n        return IO.Schema(node_id=cls.NODE_ID)\n',
  });
  const scan = C.scanApiNodeIds("D:/x/c/comfy_api_nodes", io);
  assert.deepEqual(scan, { ids: ["Flux2ProImageNode"], files: 1 });
  const row = rowFor({ nodes: [{ id: 1, type: "Flux2ProImageNode", mode: 0 }] }, { openSource: true }, { apiIds: new Set(scan.ids) });
  assert.equal(row.kind, "api");
  assert.deepEqual([row.api.by_node_id, row.api.by_name_prefix, row.api.by_open_source_false], [["Flux2ProImageNode"], false, false]);
});

// ---------------------------------------------------------------------------------------------
// UNC roots and the python directory order
// ---------------------------------------------------------------------------------------------

// isAbsPath accepts a UNC prefix, but normalizePath dropped the empty segments of `//nas/models` and returned `/nas/models`: on Windows
// a path on the current drive that does not exist, so a model tree on a network share was never scanned and every model on it read
// as missing. Python's os.path.normpath keeps the double slash, and so does the share root (`..` does not climb out of it).
test("paths: a UNC root keeps its double slash and its share", () => {
  assert.equal(C.normalizePath("\\\\nas\\models\\vae"), "//nas/models/vae");
  assert.equal(C.normalizePath("//nas/models/./vae/../vae"), "//nas/models/vae");
  assert.equal(C.normalizePath("//nas/models"), "//nas/models");
  assert.equal(C.normalizePath("//nas/models/.."), "//nas/models", "'..' does not climb out of a share");
  assert.equal(C.normalizePath("//nas"), "//nas");
  assert.equal(C.normalizePath("///nas/models"), "/nas/models", "three slashes are one, as on POSIX");
  assert.equal(C.normalizePath("\\\\?\\D:\\x\\y"), "//?/D:/x/y", "an extended-length path keeps its prefix");
  assert.equal(C.normalizePath("\\\\wsl.localhost\\Distro\\home\\u"), "//wsl.localhost/Distro/home/u");
  assert.equal(C.joinPath("\\\\nas\\models", "vae"), "//nas/models/vae");
  assert.equal(C.isAbsPath("\\\\nas\\models"), true);
});

test("roots: a UNC base_path or entry is scanned where it is, and a file on the share is present", () => {
  const unc = "\\\\nas\\models";
  const text = `p:\n  base_path: ${unc}\n  vae: vae\nq:\n  vae: \\\\other\\share\\vae2\n`;
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text, dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "vae").slice(0, 3), ["D:/x/comfy/models/vae", "//nas/models/vae", "//other/share/vae2"]);
  const inv = C.scanModelInventory(roots, memIo({ "//nas/models/vae/ae.safetensors": { size: 3 } }));
  assert.deepEqual(relsOf(inv, "vae"), ["ae.safetensors"]);
  assert.equal(C.readiness([req("vae", "ae.safetensors")], inv).ready, true);
});

// locateTemplates sorted the python3.x directory names as strings, descending, so "python3.9" outranked "python3.13".
test("locate: the highest python3.x wins numerically (3.13 beats 3.9)", () => {
  const idx = (p) => ({ [`${p}/comfyui_workflow_templates_json/templates/index.json`]: "[]" });
  const io = memIo({ ...idx("/srv/c/.venv/lib/python3.9/site-packages"), ...idx("/srv/c/.venv/lib/python3.13/site-packages"), ...idx("/srv/c/.venv/lib/python3.10/site-packages") });
  assert.match(C.locateTemplates("/srv/c", io).sitePackages, /python3\.13\//);
});

// ---------------------------------------------------------------------------------------------
// The directories ComfyUI registers in main.py, and the rules the readiness check was made from
// ---------------------------------------------------------------------------------------------

// main.py's apply_custom_paths registers <output>/checkpoints, clip, vae, diffusion_models and loras after it has read the yaml files
// (add_model_folder_path maps the legacy name clip to text_encoders and appends: is_default is false), so a file saved there is listed
// by the loaders. The readiness check reproduced folder_paths.py and extra_config.py only, and reported such a file as missing.
test("roots: the five output directories main.py registers come after the yaml ones, under the class each maps to", () => {
  const yaml = "p:\n  base_path: E:/x/overflow\n  is_default: true\n  loras: loras\n  clip: clip\n";
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: yaml, dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "loras"), ["E:/x/overflow/loras", "D:/x/comfy/models/loras", "D:/x/comfy/output/loras"]);
  assert.deepEqual(dirsOf(roots, "text_encoders"), ["E:/x/overflow/clip", "D:/x/comfy/models/text_encoders", "D:/x/comfy/models/clip", "D:/x/comfy/output/clip"]);
  assert.equal(dirsOf(roots, "diffusion_models").at(-1), "D:/x/comfy/output/diffusion_models");
  assert.equal(dirsOf(roots, "checkpoints").at(-1), "D:/x/comfy/output/checkpoints");
  assert.equal(dirsOf(roots, "vae").at(-1), "D:/x/comfy/output/vae");
  assert.equal(roots.classes.loras.dirs.at(-1).source, "output");
  assert.equal(roots.classes.clip, undefined, "clip is text_encoders, not a class of its own");
  for (const cls of ["configs", "embeddings", "controlnet", "upscale_models", "vae_approx"]) assert.ok(!dirsOf(roots, cls).some((d) => d.includes("/output/")), cls);
  const moved = C.resolveModelRoots({ comfyDir: "D:/x/comfy", outputDir: "D:/x/out", yamls: [] });
  assert.deepEqual(dirsOf(moved, "vae"), ["D:/x/comfy/models/vae", "D:/x/out/vae"]);
});

test("snapshot: a file under <output>/loras is listed, and --output-directory and --base-directory move the output root", () => {
  const launch = (args, extra = {}) => memIo(comfyTree({ "D:/x/comfy/.offload-launch.json": JSON.stringify({ args }), ...extra }));
  const plain = C.snapshotNode({ comfyDir: "D:/x/comfy", io: memIo(comfyTree({ "D:/x/comfy/output/loras/saved.safetensors": { size: 5 } })) });
  assert.deepEqual(relsOf(plain.inventory, "loras"), ["l.safetensors", "saved.safetensors"]);
  const eq = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--output-directory=D:/x/out"], { "D:/x/out/vae/o.safetensors": { size: 1 }, "D:/x/comfy/output/vae/ignored.safetensors": { size: 1 } }) });
  assert.equal(eq.launch.output_directory, "D:/x/out");
  assert.deepEqual(relsOf(eq.inventory, "vae"), ["ae.safetensors", "o.safetensors"]);
  const base = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--base-directory", "D:/x/base"], { "D:/x/base/output/vae/b.safetensors": { size: 1 } }) });
  assert.deepEqual(base.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/base/models/vae", "D:/x/base/output/vae"]);
  const both = C.snapshotNode({ comfyDir: "D:/x/comfy", io: launch(["main.py", "--base-directory", "D:/x/base", "--output-directory", "D:/x/out"]) });
  assert.deepEqual(both.model_roots.classes.vae.dirs.map((d) => d.path), ["D:/x/base/models/vae", "D:/x/out/vae"], "--output-directory wins over <base>/output");
});

// The class table is ComfyUI 0.37.0's. A snapshot records the node's ComfyUI version and hashes of the two rule files (folder_paths.py
// and utils/extra_config.py, line endings normalised); readiness never looked at them, so a node on other rules was scored silently
// with this table. The hashes decide (a tree can be commits past a release and still report its version); the check is advisory.
const stampedCatalog = () => C.buildCatalog({ templatesDir: REAL_TEMPLATES, apiIds: realApiIds(), stamp: C.stampFor({ templatesDir: REAL_TEMPLATES, basis: "installed-package", io: C.nodeIo }), licenseMap: fixtureMap(), io: C.nodeIo });
const ruleFiles = { folder_paths_sha256: C.COMFYUI_RULE_FILES?.folder_paths_sha256, extra_config_sha256: C.COMFYUI_RULE_FILES?.extra_config_sha256 };

test("readiness: a node whose ComfyUI rule files are not the ones the class table was checked against says so, and is still scored", () => {
  const cat = stampedCatalog();
  const snap = (over = {}) => ({ ...nodeSnapshot(ACE_TREE), comfyui_version: "0.37.0", comfy_files: { ...ruleFiles }, ...over });
  const good = C.nodeReadiness({ catalog: cat, snapshot: snap() });
  assert.deepEqual({ ...good.rules_check }, { ok: true, table_version: "0.37.0", node_version: "0.37.0", differs: [], reason: null });
  const other = C.nodeReadiness({ catalog: cat, snapshot: snap({ comfyui_version: "0.38.0", comfy_files: { ...ruleFiles, folder_paths_sha256: "0".repeat(64) } }) });
  assert.equal(other.rules_check.ok, false);
  assert.deepEqual(other.rules_check.differs, ["folder_paths.py"]);
  assert.equal(other.rules_check.node_version, "0.38.0");
  assert.match(other.rules_check.reason, /folder_paths\.py/);
  assert.match(other.rules_check.reason, /0\.37\.0/);
  assert.equal(other.counts.ready.with_weights, 1, "advisory: the node is scored all the same");
  const both = C.nodeReadiness({ catalog: cat, snapshot: snap({ comfy_files: { folder_paths_sha256: "1".repeat(64), extra_config_sha256: null } }) });
  assert.deepEqual(both.rules_check.differs, ["folder_paths.py", "utils/extra_config.py"], "a rule file that is missing counts as different");
  assert.equal(C.nodeReadiness({ catalog: cat, snapshot: snap({ comfyui_version: "0.99.0" }) }).rules_check.ok, true, "the hashes decide: the same files under another version string are the same rules");
});

test("readiness: a snapshot with no rule-file hashes is unverified, not failed; one that names another ComfyUI version is different", () => {
  const cat = stampedCatalog();
  const bare = C.nodeReadiness({ catalog: cat, snapshot: nodeSnapshot(ACE_TREE) });
  assert.equal(bare.rules_check.ok, null);
  assert.equal(bare.rules_check.node_version, null);
  assert.match(bare.rules_check.reason, /no rule-file hashes/);
  const versioned = C.nodeReadiness({ catalog: cat, snapshot: { ...nodeSnapshot(ACE_TREE), comfyui_version: "0.40.1" } });
  assert.equal(versioned.rules_check.ok, false);
  assert.match(versioned.rules_check.reason, /0\.40\.1/);
  assert.match(versioned.rules_check.reason, /0\.37\.0/);
  const refused = C.nodeReadiness({ catalog: cat, snapshot: { ...nodeSnapshot(ACE_TREE, { "comfyui-workflow-templates-json": "0.1.96" }), comfyui_version: "0.40.1" } });
  assert.ok(refused.refused && refused.rules_check.ok === false, "a refused node still reports its rules");
});

test("cli: readiness prints one line about a node that runs other ComfyUI rules, and the exit code stays 0", async () => {
  const withRules = (files, hashes) => JSON.stringify({ package: { versions: V094 }, inventory: invFrom(files), comfyui_version: "0.37.0", comfy_files: hashes });
  const t = await tempFiles({
    "same.json": withRules(ACE_TREE, { ...ruleFiles }),
    "other.json": withRules(ACE_TREE, { ...ruleFiles, extra_config_sha256: "2".repeat(64) }),
  });
  try {
    const snaps = ["--snapshot", `${t.root}/same.json`, "--snapshot", `${t.root}/other.json`];
    const text = await run([...readinessBase, ...snaps]);
    assert.equal(text.code, 0, text.stderr);
    const lines = text.stdout.split("\n").filter((l) => /^\s+rules: /.test(l));
    assert.equal(lines.length, 1, "only the node on other rules gets a line");
    assert.match(lines[0], /utils\/extra_config\.py/);
    const json = JSON.parse((await run([...readinessBase, ...snaps, "--json"])).stdout);
    assert.deepEqual([json.nodes.same.rules_check.ok, json.nodes.other.rules_check.ok], [true, false]);
  } finally {
    t.cleanup();
  }
});

// snapshot --comfy-dir <a directory that does not exist> printed an all-null snapshot and exited 0; readiness refused it later.
test("cli: snapshot of a directory that is not a ComfyUI tree fails with exit 2 and prints no snapshot", async () => {
  const missing = await run(["snapshot", "--comfy-dir", "D:/x/nope"], memIo({}));
  assert.equal(missing.code, 2);
  assert.match(missing.stderr, /cannot read D:\/x\/nope/);
  assert.equal(missing.stdout, "");
  const bare = await run(["snapshot", "--comfy-dir", "D:/x/empty"], memIo({ "D:/x/empty/readme.txt": { size: 1 } }));
  assert.equal(bare.code, 2);
  assert.match(bare.stderr, /not a ComfyUI tree: D:\/x\/empty/);
  assert.equal(bare.stdout, "");
  const oneMarker = await run(["snapshot", "--comfy-dir", "D:/x/portable"], memIo({ "D:/x/portable/main.py": "# entry point\n" }));
  assert.equal(oneMarker.code, 0, oneMarker.stderr);
  assert.equal((await run(["snapshot", "--comfy-dir", "D:/x/comfy"], memIo(comfyTree()))).code, 0);
});

// ---------------------------------------------------------------------------------------------
// The extra_model_paths.yaml shapes ComfyUI's loader accepts
// ---------------------------------------------------------------------------------------------

// ComfyUI reads the file with yaml.safe_load: a provider may be named anything (with a space, in quotes) and a class key may be
// quoted. A header the parser did not recognise was skipped, and its base_path and entries were attached to the provider before it,
// overwriting that provider's base_path, so directories were placed under the wrong root instead of being left out.
test("yaml: a provider named with a space or in quotes is a provider of its own and lends nothing to the one before", () => {
  const text = 'p1:\n  base_path: D:/aa\n  loras: loras\nmy ui:\n  base_path: D:/bb\n  loras: loras\n"quoted name":\n  base_path: D:/cc\n  vae: vae\n\'single\':\n  vae: v2\n';
  const p = C.parseExtraModelPathsYaml(text);
  assert.deepEqual(p.map((x) => [x.name, x.base_path, x.entries.map((e) => e.key)]), [["p1", "D:/aa", ["loras"]], ["my ui", "D:/bb", ["loras"]], ["quoted name", "D:/cc", ["vae"]], ["single", null, ["vae"]]]);
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text, dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "loras").slice(0, 3), ["D:/x/comfy/models/loras", "D:/aa/loras", "D:/bb/loras"]);
});

test("yaml: a top-level line the parser cannot read ends the provider before it, so its indented lines belong to nobody", () => {
  const p = C.parseExtraModelPathsYaml("p1:\n  base_path: D:/aa\n  loras: loras\n{ flow: mapping }\n  base_path: D:/bb\n  vae: vae\n- stray\n  clip: clip\n");
  assert.deepEqual(p.map((x) => [x.name, x.base_path, x.entries.map((e) => e.key)]), [["p1", "D:/aa", ["loras"]]]);
});

test("yaml: a quoted class key is the class, and a folded scalar is one path per paragraph, joined with spaces as YAML folds it", () => {
  const p = C.parseExtraModelPathsYaml("p:\n  base_path: D:/x\n  \"loras\": loras\n  'vae': vae\n  clip: >\n    a\n    b\n\n    c\n  unet: |\n    u1\n    u2\n");
  assert.deepEqual(Object.fromEntries(p[0].entries.map((e) => [e.key, e.paths])), { loras: ["loras"], vae: ["vae"], clip: ["a b", "c"], unet: ["u1", "u2"] });
});

// ---------------------------------------------------------------------------------------------
// Basis names, help text and the list filters that are documented
// ---------------------------------------------------------------------------------------------

// BASIS_TEXT is a plain object, so a basis named after one of its inherited properties passed the check and printed a function.
test("stamp: a basis named like an inherited object property is refused like any other unknown one", () => {
  const io = memIo({ "D:/x/loose/index.json": "[]" });
  for (const basis of ["toString", "constructor", "__proto__", "hasOwnProperty"]) assert.throws(() => C.stampFor({ templatesDir: "D:/x/loose", basis, io }), /unknown basis/, basis);
});

test("cli: --basis and --candidate-basis refuse an unknown kind as a usage error (exit 2) and name the flag", async () => {
  const cand = await candidateDir();
  try {
    const diff = ["diff", "--templates-dir", REAL_TEMPLATES, "--candidate-dir", cand.dir, "--api-nodes-dir", API_DIR];
    const r = await run([...diff, "--candidate-basis", "toString"]);
    assert.equal(r.code, 2);
    assert.match(r.stderr, /unknown --candidate-basis "toString"/);
    assert.equal(r.stdout, "");
    assert.equal((await run([...diff, "--candidate-basis", "package-extract", "--json"])).code, 0);
    const summary = await run(["summary", "--templates-dir", REAL_TEMPLATES, "--basis", "toString"]);
    assert.equal(summary.code, 2);
    assert.match(summary.stderr, /unknown --basis "toString"/);
  } finally {
    cand.cleanup();
  }
});

test("cli: --help documents --candidate-basis and what `list --kind api` and --include-hidden do", async () => {
  const help = (await run(["--help"])).stdout;
  assert.match(help, /--candidate-basis KIND/);
  assert.match(help, /--kind api[^\n]*without --include-hidden/, "naming the kind is enough to list the paid templates");
  assert.match(help, /FLUX-family[^\n]*--include-hidden/);
});

// `--kind api` lists the paid templates without --include-hidden (asking for the kind is the opt-in); the FLUX-family bar is not a kind and
// stays hidden until --include-hidden, paid or not. Not a RED test: it pins the behaviour the help text and the docs now state.
test("cli: list --kind api lists the paid templates without --include-hidden, except the FLUX-family ones", async () => {
  const files = LIST_FILES();
  const index = JSON.parse(files["D:/x/t/index.json"]);
  index[1].templates.push({ name: "api_flux_kontext", title: "Flux Paid", mediaType: "image", openSource: false });
  files["D:/x/t/index.json"] = JSON.stringify(index);
  files["D:/x/t/api_flux_kontext.json"] = JSON.stringify({ nodes: [] });
  const shown = await listNames(["--kind", "api"], files);
  assert.deepEqual(shown.templates.map((t) => t.name), ["api_delta", "api_zeta"]);
  assert.deepEqual({ ...shown.hidden }, { api: 0, flux: 1 });
  const all = await listNames(["--kind", "api", "--include-hidden"], files);
  assert.deepEqual(all.templates.map((t) => t.name), ["api_delta", "api_zeta", "api_flux_kontext"]);
  const plain = await listNames([], files);
  assert.deepEqual({ ...plain.hidden }, { api: 3, flux: 1 }, "without the kind, every paid template is hidden");
});

// ---------------------------------------------------------------------------------------------
// The committed map's provenance
// ---------------------------------------------------------------------------------------------

// An entry copied from a sibling repo was never fetched: it must not carry a fetch date or a source URL (the summary's evidence range
// is computed from the dates), and it must name the repo it was copied from.
test("license map (committed): an inferred entry carries no fetch date and no source url, and names an entry that is in the map", () => {
  const map = C.loadLicenseMap(loadJson(REAL_MAP_PATH));
  let inferred = 0;
  for (const [repo, e] of map.entries()) {
    if (!/^inferred/i.test(e.basis ?? "")) continue;
    inferred++;
    assert.equal(e.fetched_on ?? null, null, `${repo}: nothing was fetched, so there is no fetch date`);
    assert.equal(e.source_url ?? null, null, `${repo}: nothing was fetched, so there is no source url`);
    const from = /^inferred from (\S+)/.exec(e.basis)?.[1];
    assert.ok(from && map.has(from) && !/^inferred/i.test(map.get(from).basis ?? ""), `${repo}: inferred from a repo that is in the map and was itself read`);
  }
  assert.ok(inferred >= 2);
});

test("license map: the evidence range counts fetched entries only, never an inferred one that carries a date", () => {
  const map = C.loadLicenseMap({ repos: {
    "a/read": { class: "permissive", fetched_on: "2026-05-01", basis: "hf-card-tag" },
    "a/copied": { class: "conditional", fetched_on: "2026-09-30", basis: "inferred from a/read (not fetched)" },
  } });
  assert.deepEqual([...map.meta.evidence_dates], ["2026-05-01", "2026-05-01"]);
});

// ---------------------------------------------------------------------------------------------
// Names every object inherits
// ---------------------------------------------------------------------------------------------

// Plain-object tables looked up with a key that comes from outside (the verb on the command line, a class in a yaml file, the type of
// a template's loader node) answered for `constructor`, `toString` and the rest: the verb `constructor` ran `Object` and handed the shell an
// object as its exit code, a yaml class of that name lost its directories, and a loader type of that name was filed under a function.
test("tables: a name every object inherits is not a verb, a legacy class, a loader type or a yaml class", async () => {
  for (const verb of ["constructor", "toString", "__proto__", "hasOwnProperty"]) {
    const r = await run([verb]);
    assert.equal(r.code, 2, verb);
    assert.match(r.stderr, /unknown verb/, verb);
  }
  assert.equal(C.mapLegacyClass("constructor"), "constructor");
  assert.equal(C.mapLegacyClass("unet"), "diffusion_models");
  const models = C.requiredModels({ nodes: [{ id: 1, type: "constructor", mode: 0, widgets_values: ["m.safetensors"] }, { id: 2, type: "toString", mode: 0, widgets_values: ["n.safetensors"] }] });
  assert.deepEqual(models.active.map((m) => m.directory), [null, null]);
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: "p:\n  base_path: D:/x/m\n  constructor: c\n  toString: t\n  vae: v\n", dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "constructor"), ["D:/x/m/c"]);
  assert.deepEqual(dirsOf(roots, "toString"), ["D:/x/m/t"]);
  assert.deepEqual(dirsOf(roots, "vae"), ["D:/x/comfy/models/vae", "D:/x/m/v", "D:/x/comfy/output/vae"]);
});

// ComfyUI skips an empty entry of extra_model_paths.yaml (`if len(y) == 0: continue`); joined onto base_path it registered base_path itself.
test("roots: an empty yaml entry registers nothing", () => {
  const roots = C.resolveModelRoots({ comfyDir: "D:/x/comfy", yamls: [{ text: 'p:\n  base_path: D:/x/m\n  vae: ""\n  loras: |\n    \n    l\n', dir: "D:/x/comfy" }] });
  assert.deepEqual(dirsOf(roots, "vae"), ["D:/x/comfy/models/vae", "D:/x/comfy/output/vae"]);
  assert.deepEqual(dirsOf(roots, "loras"), ["D:/x/comfy/models/loras", "D:/x/m/l", "D:/x/comfy/output/loras"]);
});

// ---------------------------------------------------------------------------------------------
// A text encoder hosted in a FLUX-family repo
// ---------------------------------------------------------------------------------------------

// The repo signal of the FLUX bar (ADR 0011) reads the repo's id. A repo named for the FLUX release that bundled a general text encoder (a
// Qwen one) therefore barred a plain text-generation template that holds no FLUX weights, and `blocked` has no acknowledgement path, so the
// verdict was a hard stop on a false positive. A text encoder is another maker's model wherever it is hosted: a repo whose id merely
// contains "flux" and from which a template takes only text encoders is reported (a warning), not barred. Anything else from such a repo, a
// file with no class, the maker's own organisation and a map flag still bar.
const encoder = (repo, name = "qwen_3_4b.safetensors", directory = "text_encoders") => ({ ...mdl(repo, name), directory });
const fluxOfModels = (models, name = "llm_text_gen") => C.fluxAssessment({ name, models }, fixtureMap());

test("FLUX bar: text encoders alone from a repo whose id contains flux are reported, not barred", () => {
  const r = fluxOfModels([encoder("example-org/flux-bundle"), encoder("example-org/flux-bundle", "second.safetensors", "clip"), encoder("comfyanonymous/flux_text_encoders", "t5.safetensors")]);
  assert.deepEqual({ ...r }, { barred: false, by: [], repos: [], filename_hits: [], text_encoder_repos: ["example-org/flux-bundle"] }, "the map-exempt repo is not listed: it is exempt by its entry");
  assert.deepEqual([...fluxOfModels([encoder("zeta/flux-two"), encoder("alpha/Flux-one"), encoder("zeta/flux-two", "b.safetensors")]).text_encoder_repos], ["alpha/Flux-one", "zeta/flux-two"], "sorted, once each");
});

test("FLUX bar: any other file from that repo, a file with no class, the maker's organisation or the map flag still bars", () => {
  const other = (directory) => ({ ...mdl("example-org/flux-bundle", "d.safetensors"), directory });
  assert.deepEqual({ ...fluxOfModels([encoder("example-org/flux-bundle"), other("diffusion_models")]) }, { barred: true, by: ["repo"], repos: ["example-org/flux-bundle"], filename_hits: [], text_encoder_repos: [] });
  assert.equal(fluxOfModels([encoder("example-org/flux-bundle"), other("vae")]).barred, true, "a VAE is not a text encoder");
  assert.equal(fluxOfModels([encoder("example-org/flux-bundle"), other(null)]).barred, true, "a file with no class fails closed");
  assert.equal(fluxOfModels([{ ...encoder("example-org/flux-bundle"), directory: null }]).barred, true, "so does an encoder with no class");
  assert.equal(fluxOfModels([encoder("black-forest-labs/whatever")]).barred, true, "the maker's own organisation is not narrowed");
  assert.deepEqual({ ...fluxOfModels([encoder("black-forest-labs/Flux-bundle")]) }, { barred: true, by: ["repo"], repos: ["black-forest-labs/Flux-bundle"], filename_hits: [], text_encoder_repos: [] }, "and is not listed as a narrowed repo either");
  assert.equal(fluxOfModels([encoder("example-org/flagged-family")]).barred, true, "a map flux_bar true forces the bar");
  assert.equal(fluxOfModels([encoder("example-org/flux-bundle")], "flux_named_thing").barred, true, "a FLUX name still bars");
});

test("gate: a text encoder from a FLUX-family repo is a warning that names the repos, and only on a template that is not barred", () => {
  assert.deepEqual(gate({ flux: { barred: false, filename_hits: [], text_encoder_repos: ["a/flux-x", "b/flux-y"] } }).warnings, ["flux_repo_text_encoder: a/flux-x, b/flux-y"]);
  assert.deepEqual(gate({ flux: { barred: true, filename_hits: [], text_encoder_repos: ["a/flux-x"] } }).warnings, []);
  assert.deepEqual(gate({ flux: { barred: false, filename_hits: ["f.safetensors"], text_encoder_repos: ["a/flux-x"] } }).warnings, ["flux_component_by_filename: f.safetensors", "flux_repo_text_encoder: a/flux-x"]);
});

test("catalog: a text-generation template with only a text encoder from a FLUX-family repo is not blocked, and a FLUX file beside it blocks it", () => {
  const encoderNode = loaderNode(1, "CLIPLoader", { name: "qwen_3_4b.safetensors", repo: "example-org/flux-bundle", dir: "text_encoders" });
  const row = rowFor({ nodes: [encoderNode] });
  assert.deepEqual({ ...row.flux }, { barred: false, by: [], repos: [], filename_hits: [], text_encoder_repos: ["example-org/flux-bundle"] });
  assert.deepEqual({ ...row.gate }, { state: "ack_required", blocked: [], ack: ["unknown"], warnings: ["flux_repo_text_encoder: example-org/flux-bundle"] }, "the repo is not in the map, so its licence is unknown and needs the acknowledgement");
  const both = rowFor({ nodes: [encoderNode, loaderNode(2, "UNETLoader", { name: "u.safetensors", repo: "example-org/flux-bundle", dir: "diffusion_models" })] });
  assert.deepEqual([both.flux.barred, both.flux.by, both.gate.state, both.gate.blocked], [true, ["repo"], "blocked", ["flux"]]);
});

// ---------------------------------------------------------------------------------------------
// Residue of a second mutation pass over the fixes: the paths a fix added that no test reached
// ---------------------------------------------------------------------------------------------

// Kills: the rule-file check reading a non-object comfy_files (null, a string), the wording of its reason (one file or two, with or without a
// version), and the class table drifting from the files it was checked against without a test noticing.
test("rules: the reason names the files and the release, and a comfy_files that is not an object is unverified, not a crash", () => {
  const snap = (over) => ({ ...nodeSnapshot(ACE_TREE), comfyui_version: "0.37.0", ...over });
  assert.equal(C.checkRules(snap({ comfy_files: { ...ruleFiles, folder_paths_sha256: "0".repeat(64) } })).reason,
    "folder_paths.py differs from the ComfyUI 0.37.0 file the directory rules were checked against (the node reports 0.37.0)");
  assert.equal(C.checkRules(snap({ comfy_files: { folder_paths_sha256: "0".repeat(64), extra_config_sha256: "1".repeat(64) } })).reason,
    "folder_paths.py and utils/extra_config.py differ from the ComfyUI 0.37.0 files the directory rules were checked against (the node reports 0.37.0)");
  assert.equal(C.checkRules({ comfy_files: { ...ruleFiles, extra_config_sha256: null } }).reason,
    "utils/extra_config.py differs from the ComfyUI 0.37.0 file the directory rules were checked against", "no version, no parenthesis");
  const unverified = { ok: null, table_version: "0.37.0", node_version: null, differs: [], reason: "the snapshot carries no rule-file hashes, so the ComfyUI rules it ran cannot be checked" };
  for (const comfy_files of [null, undefined, "x", 5]) assert.deepEqual({ ...C.checkRules({ comfy_files }) }, unverified, String(comfy_files));
  assert.deepEqual({ ...C.checkRules(null) }, unverified);
  assert.deepEqual({ ...C.checkRules(undefined) }, unverified);
});

test("rules: the class table is pinned to the ComfyUI 0.37.0 files by their hashes", () => {
  assert.deepEqual({ ...C.COMFYUI_RULE_FILES }, {
    version: "0.37.0",
    folder_paths_sha256: "b748be5d1e068ad673f6c4de069fb6b7b723405c40ae3c3c44465d416f7df2db",
    extra_config_sha256: "ebb923e58956587acf5196e3a76ae3c576196f2f498e5933ce9416fca1ddaf9e",
  });
  assert.equal(C.COMFYUI_CLASS_TABLE_VERSION, C.COMFYUI_RULE_FILES.version);
  assert.equal(Object.isFrozen(C.COMFYUI_RULE_FILES), true);
});

// Kills: any one of the three markers dropped from the tree check, and the missing-flag message replaced by the unreadable-directory one.
test("cli: each of the three files alone marks a ComfyUI tree, and snapshot without --comfy-dir says what to give", async () => {
  for (const marker of ["comfyui_version.py", "folder_paths.py", "main.py"]) {
    const r = await run(["snapshot", "--comfy-dir", "D:/x/only"], memIo({ [`D:/x/only/${marker}`]: "# x\n" }));
    assert.equal(r.code, 0, `${marker} alone: ${r.stderr}`);
  }
  const none = await run(["snapshot"], memIo(comfyTree()));
  assert.equal(none.code, 2);
  assert.equal(none.stderr, "give --comfy-dir\n");
});

// Kills: the blank/comment guard of the yaml reader (a blank line at column 0 would end the provider, since the line is not a `key:` line),
// and the flush of a folded scalar that is the last thing in the file. ComfyUI's own loader gives the same lists for both shapes.
test("yaml: blank lines and comments, at any indent, between the entries of a provider do not end it", () => {
  const text = "p:\n  base_path: D:/x\n\n# a comment at column 0\n  vae: vae\n    # an indented comment\n\n  loras: loras   # trailing\n#\nq:\n  base_path: D:/y\n  clip: clip\n";
  const p = C.parseExtraModelPathsYaml(text);
  assert.deepEqual(p.map((x) => [x.name, x.base_path, x.entries.map((e) => `${e.key}=${e.paths.join("+")}`)]), [["p", "D:/x", ["vae=vae", "loras=loras"]], ["q", "D:/y", ["clip=clip"]]]);
});

test("yaml: a folded scalar that ends the file is flushed, with or without a final newline", () => {
  const at = (tail) => C.parseExtraModelPathsYaml(`p:\n  base_path: D:/x\n  clip: >\n    a\n    b${tail}`)[0].entries[0].paths;
  assert.deepEqual(at(""), ["a b"]);
  assert.deepEqual(at("\n"), ["a b"]);
  assert.deepEqual(at("\n\n    c"), ["a b", "c"], "the last paragraph counts too");
});

// Kills: the candidate order across the two venv names and the two layouts (a `venv/Lib` candidate ahead of a `.venv/lib/pythonX` one), and
// the positional order of the command line (the LAST positional argument becoming the verb).
test("locate: every .venv candidate comes before any venv one, whichever layout each uses", () => {
  const idx = (p) => ({ [`${p}/comfyui_workflow_templates_json/templates/index.json`]: "[]" });
  const io = memIo({ ...idx("/srv/c/.venv/lib/python3.12/site-packages"), ...idx("/srv/c/venv/Lib/site-packages") });
  assert.match(C.locateTemplates("/srv/c", io).sitePackages, /\/\.venv\/lib\/python3\.12\/site-packages$/);
});

test("cli: the first positional argument is the verb, and later ones are ignored", async () => {
  const r = await run(["summary", "frobnicate", "--templates-dir", REAL_TEMPLATES, "--api-nodes-dir", API_DIR, "--json"]);
  assert.equal(r.code, 0, r.stderr);
  assert.equal(JSON.parse(r.stdout).entries, 8);
});

// Kills (final pass): a map flux_bar true also listing its repo as a narrowed text-encoder repo; a file named exactly like an extension being
// listed (Python's splitext gives it none, so ComfyUI does not offer it); the order of missing directories and of the files a case or
// subfolder mismatch names; and the recursion guard of the walk losing the entry it should pop (a sibling instance inside a parent read
// as a recursion), or a self-instantiating instance jumping to the front of the leaves.
test("FLUX bar: a map flux_bar true on a repo whose id contains flux bars it outright, and does not also list it as a narrowed repo", () => {
  const map = C.loadLicenseMap({ repos: { "example-org/flux-forced": { class: "non_commercial", flux_bar: true, basis: "test" } } });
  assert.deepEqual({ ...C.fluxAssessment({ name: "llm_text_gen", models: [encoder("example-org/flux-forced")] }, map) }, { barred: true, by: ["repo"], repos: ["example-org/flux-forced"], filename_hits: [], text_encoder_repos: [] });
});

test("inventory: a file named exactly like an extension has none and is not listed, and missing directories come back in registration order", () => {
  const inv = invFrom({ "D:/x/comfy/models/loras/.safetensors": { size: 1 }, "D:/x/comfy/models/loras/ok.safetensors": { size: 1 } });
  assert.deepEqual(relsOf(inv, "loras"), ["ok.safetensors"]);
  assert.equal(inv.classes.loras.unlisted, 1);
  assert.deepEqual(invFrom({}).missing_dirs.filter((d) => d.class === "text_encoders").map((d) => d.path), ["D:/x/comfy/models/text_encoders", "D:/x/comfy/models/clip", "D:/x/comfy/output/clip"]);
});

test("readiness: the files a case or a subfolder mismatch names come back in path order", () => {
  const inv = invFrom({ "D:/x/comfy/models/loras/Model.safetensors": { size: 1 }, "D:/x/comfy/models/loras/MODEL.safetensors": { size: 1 }, "D:/x/comfy/models/loras/a/x.safetensors": { size: 1 }, "D:/x/comfy/models/loras/b/x.safetensors": { size: 1 } });
  const cased = C.readiness([req("loras", "model.safetensors")], inv).requirements[0];
  assert.deepEqual([cased.status, cased.found], ["case_mismatch", ["MODEL.safetensors", "Model.safetensors"]]);
  const sub = C.readiness([req("loras", "x.safetensors")], inv).requirements[0];
  assert.deepEqual([sub.status, sub.found], ["in_subfolder", ["a/x.safetensors", "b/x.safetensors"]]);
});

test("walk: sibling instances inside a parent are each expanded, and a self-instantiating one stays a leaf in document order", () => {
  const X = "aaaaaaaa-1111-4111-8111-111111111111";
  const Y = "bbbbbbbb-1111-4111-8111-111111111111";
  const S = "cccccccc-1111-4111-8111-111111111111";
  const top = (...types) => types.map((type, i) => ({ id: i + 1, type, mode: 0 }));
  const nested = C.flattenActiveNodes({
    nodes: top("KSampler", X, "VAEDecode"),
    definitions: { subgraphs: [{ id: X, nodes: [{ id: 20, type: Y, mode: 0 }, { id: 21, type: Y, mode: 0 }] }, { id: Y, nodes: [{ id: 30, type: "CLIPTextEncode", mode: 0 }] }] },
  });
  assert.deepEqual(nested.map((l) => [l.node.id, Boolean(l.unexpanded)]), [[1, false], [30, false], [30, false], [3, false]], "the second Y inside X is a sibling, not a recursion");
  const self = C.flattenActiveNodes({
    nodes: top("KSampler", S, "VAEDecode"),
    definitions: { subgraphs: [{ id: S, nodes: [{ id: 10, type: "CLIPTextEncode", mode: 0 }, { id: 11, type: S, mode: 0 }] }] },
  });
  assert.deepEqual(self.map((l) => [l.node.id, Boolean(l.unexpanded)]), [[1, false], [10, false], [11, true], [3, false]]);
});

// The network-share root (`//server/share`) reads a leading double slash as a server, so a join onto the filesystem root must not make one:
// joining "/" and "x" through a plain "/" separator gave "//x", a share named x, where it had always been "/x".
test("paths: joining onto the filesystem root gives one leading slash, not a network share", () => {
  assert.equal(C.joinPath("/", "x"), "/x");
  assert.equal(C.joinPath("/", "a", "b"), "/a/b");
  assert.equal(C.joinPath("D:/x/", "y"), "D:/x/y");
  assert.equal(C.joinPath("D:", "x"), "D:/x");
  assert.equal(C.joinPath("a/", "/b"), "a/b");
  assert.equal(C.joinPath("//nas/", "share", "f"), "//nas/share/f", "a share root written with a trailing slash still joins");
  assert.equal(C.joinPath("", null, "x"), "x");
});

// Kills: dirName answering "" for a file at the root and "/" for a bare file name.
test("snapshot: a yaml at the root resolves its relative entries under the root, and one named without a folder keeps them relative", () => {
  const dirs = (yamlPath) => {
    const io = memIo(comfyTree({ "D:/x/comfy/.offload-launch.json": JSON.stringify({ args: ["main.py", "--extra-model-paths-config", yamlPath] }), [yamlPath]: "m:\n  vae: rel_vae\n" }));
    return C.snapshotNode({ comfyDir: "D:/x/comfy", io }).model_roots.classes.vae.dirs.map((d) => d.path);
  };
  assert.ok(dirs("/x.yaml").includes("/rel_vae"), "at the root");
  assert.ok(dirs("x.yaml").includes("rel_vae"), "with no folder");
});

// Kills: the check that a group's templates is an array reading only its type (an object or a number is not iterable).
test("index: a group whose templates is an object or a number is skipped like one whose templates is a string", () => {
  assert.deepEqual(C.flattenIndex([{ templates: { name: "x" } }, { templates: 7 }, { templates: [{ name: "ok" }] }]).map((t) => t.name), ["ok"]);
});
