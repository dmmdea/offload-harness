// node --test render/template-to-graph.test.mjs
//
// The template-to-API converter (render/template-to-graph.mjs): a ComfyUI UI-format workflow in, the API-format prompt graph out.
// One test per behaviour, on small hand-written UI-format graphs built here, the synthetic /object_info and the two hand-worked
// goldens in render/testdata/template-goldens/ (see its README: none of them is a frontend export). The value-exact comparison
// with API-format files beside real templates (said to be the frontend's own export; that is unverified) is a separate, privately
// run test that reads TEMPLATE_GOLDEN_DIR.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { REFUSAL, compareApiGraphs, compareVersions, convertTemplate, parseVersion } from "./template-to-graph.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const DATA = join(HERE, "testdata", "template-goldens");
const CLI = join(HERE, "template-to-graph.mjs");
const readJson = (name) => JSON.parse(readFileSync(join(DATA, name), "utf8"));
const OI = readJson("object-info.synthetic.json");
const clone = (v) => structuredClone(v);

// ---- builders ------------------------------------------------------------------------------------------------------------
// A node spec: { id, type, ins: [[name, type, widget?]], outs: [[name, type]], wv, mode, properties }. An edge names its ends:
// [fromId, fromSlot, toId, toInputName]. The builder numbers the links and fills each side, so a test states the wiring once.
function nodeOf(s) {
  return {
    id: s.id, type: s.type, mode: s.mode ?? 0,
    inputs: (s.ins ?? []).map(([name, type, w]) => ({ name, type, link: null, ...(w ? { widget: { name } } : {}) })),
    outputs: (s.outs ?? []).map(([name, type]) => ({ name, type, links: [] })),
    properties: s.properties ?? {},
    ...(s.wv !== undefined ? { widgets_values: s.wv } : {}),
  };
}
// wire: fill the links of `nodes` from `edges`; ends -10 / -20 are a subgraph's own input / output node (`byId` has them as
// stand-ins carrying the definition's slots). `objectLinks` writes the {id, origin_id, ...} shape a definition uses.
function wire(nodes, edges, { objectLinks = false, sgIns = [], sgOuts = [] } = {}) {
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const links = [];
  edges.forEach(([from, fslot, to, toName], i) => {
    const id = i + 1;
    let type;
    if (from === -10) type = sgIns[fslot].type;
    else { const f = byId.get(from); assert.ok(f?.outputs[fslot], `edge ${i}: node ${from} has no output ${fslot}`); f.outputs[fslot].links.push(id); type = f.outputs[fslot].type; }
    let tslot;
    if (to === -20) tslot = sgOuts.findIndex((o) => o.name === toName);
    else { const t = byId.get(to); tslot = t.inputs.findIndex((x) => x.name === toName); assert.ok(tslot >= 0, `edge ${i}: node ${to} has no input ${JSON.stringify(toName)}`); t.inputs[tslot].link = id; }
    assert.ok(tslot >= 0, `edge ${i}: no target slot`);
    links.push(objectLinks ? { id, origin_id: from, origin_slot: fslot, target_id: to, target_slot: tslot, type } : [id, from, fslot, to, tslot, type]);
  });
  return links;
}
function ui(specs, edges = [], extra = {}) {
  const nodes = specs.map(nodeOf);
  return { version: 0.4, nodes, links: wire(nodes, edges), ...extra };
}
// subdef: a subgraph definition. `ins` / `outs` are its input and output slots; its inner links may start at -10 and end at -20.
function subdef(id, { ins = [], outs = [], nodes: specs, edges = [] }) {
  const nodes = specs.map(nodeOf);
  const sgIns = ins.map(([name, type]) => ({ id: `in-${name}`, name, type, linkIds: [] }));
  const sgOuts = outs.map(([name, type]) => ({ id: `out-${name}`, name, type, linkIds: [] }));
  const links = wire(nodes, edges, { objectLinks: true, sgIns, sgOuts });
  for (const l of links) {
    if (l.origin_id === -10) sgIns[l.origin_slot].linkIds.push(l.id);
    if (l.target_id === -20) sgOuts[l.target_slot].linkIds.push(l.id);
  }
  return { id, name: `def ${id}`, inputNode: { id: -10 }, outputNode: { id: -20 }, inputs: sgIns, outputs: sgOuts, widgets: [], nodes, groups: [], links, extra: {} };
}
const withDefs = (wf, ...defs) => ({ ...wf, definitions: { subgraphs: defs } });
const SG = (n) => `5c1b0000-0000-4000-8000-00000000000${n}`;

const convert = (wf, opts = {}, oi = OI) => convertTemplate(wf, oi, opts);
function graphOf(r) {
  assert.equal(r.ok, true, `expected a graph, got ${JSON.stringify(r.refusal)}`);
  return r.graph;
}
function refusalOf(r, code) {
  assert.equal(r.ok, false, "expected a refusal, got a graph");
  assert.equal(r.graph, undefined, "a refusal carries no graph");
  assert.equal(r.refusal.code, code, JSON.stringify(r.refusal));
  assert.equal(typeof r.refusal.message, "string");
  return r.refusal;
}
const warned = (r, code) => r.warnings.filter((w) => w.code === code);

// graphProblems: what is wrong with an API graph as a graph, whatever the template said: a node of a class the listing lacks, a
// wire to a node that is not in the graph, a wire to an output slot the class does not have, a wire whose output type (the
// listing's `output` list) shares no member with the type the target input declares. The type check compares only what the
// listing states as a plain type string: a combo list, a COMFY_*_V3 schema type (on either end), an input the listing does not
// name (a dotted dynamic-combo or autogrow child) and a missing target class are skipped, and "*" takes any type.
const isRef = (v) => Array.isArray(v) && v.length === 2 && typeof v[0] === "string" && Number.isInteger(v[1]);
const plainType = (t) => (typeof t === "string" && !/^COMFY_.+_V3$/.test(t) ? t : null);
const declaredType = (info, name) => {
  for (const section of ["required", "optional"]) {
    const spec = info.input?.[section]?.[name];
    if (Array.isArray(spec)) return plainType(spec[0]);
  }
  return null;
};
const sameType = (a, b) => a === "*" || b === "*" || a.split(",").some((x) => b.split(",").includes(x));
function graphProblems(graph, oi) {
  const out = [];
  for (const [id, n] of Object.entries(graph)) {
    if (!oi[n.class_type]) { out.push(`${id}: unknown class ${n.class_type}`); continue; }
    for (const [k, v] of Object.entries(n.inputs)) {
      if (!isRef(v)) continue;
      const t = graph[v[0]];
      if (!t) { out.push(`${id}.${k}: wire to missing node ${v[0]}`); continue; }
      const outputs = oi[t.class_type]?.output;
      if (!outputs) continue;
      if (v[1] >= outputs.length) { out.push(`${id}.${k}: wire to slot ${v[1]} of ${v[0]} (${t.class_type} has ${outputs.length})`); continue; }
      const want = declaredType(oi[n.class_type], k);
      const have = plainType(outputs[v[1]]);
      if (want !== null && have !== null && !sameType(have, want)) {
        out.push(`${id}.${k}: wire from slot ${v[1]} of ${v[0]} (${t.class_type}) carries ${have}, the input wants ${want}`);
      }
    }
  }
  return out;
}

// ---- goldens -------------------------------------------------------------------------------------------------------------

test("golden: a plain text-to-image template converts to the hand-worked API graph, node by node", () => {
  const r = convert(readJson("basic-t2i.workflow.json"));
  const graph = graphOf(r);
  assert.deepEqual(compareApiGraphs(graph, readJson("basic-t2i.api.json")), []);
  assert.equal(Object.keys(graph).includes("10"), false, "the Note is not a node");
  assert.equal(Object.keys(graph).includes("11"), false, "the muted LoRA loader is gone");
  assert.equal(r.stats.nodes, 7);
  assert.deepEqual(r.warnings, []);
  assert.deepEqual(graphProblems(graph, OI), []);
});

test("graphProblems: names a missing node, an out-of-range slot and an unknown class", () => {
  const g = { 1: { class_type: "LoadImage", inputs: {} }, 2: { class_type: "ImageInvert", inputs: { image: ["1", 5] } }, 3: { class_type: "ImageInvert", inputs: { image: ["9", 0] } }, 4: { class_type: "Nope", inputs: {} } };
  assert.deepEqual(graphProblems(g, OI), ["2.image: wire to slot 5 of 1 (LoadImage has 2)", "3.image: wire to missing node 9", "4: unknown class Nope"]);
});

test("graphProblems: names a wire whose output type is not the type the target input declares, and skips what it cannot compare", () => {
  const ckpt = { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "a.safetensors" } };
  const g = {
    1: ckpt,
    2: { class_type: "VAEDecode", inputs: { samples: ["5", 0], vae: ["1", 0] } }, // a MODEL into a VAE input
    3: { class_type: "VAEDecode", inputs: { samples: ["5", 0], vae: ["1", 2] } }, // fine
    4: { class_type: "SaveGLB", inputs: { mesh: ["6", 0] } }, // IMAGE,MASK output into a MESH,FILE_3D_GLB,FILE_3D input: no common member
    5: { class_type: "EmptyLatentImage", inputs: { width: ["7", 0] } }, // INT into a widget input of the same type
    6: { class_type: "ResizeSynth", inputs: { input: ["8", 1] } }, // a MATCHTYPE input: not compared
    7: { class_type: "PrimitiveInt", inputs: {} },
    8: { class_type: "LoadImage", inputs: { image: ["7", 0], nope: ["1", 0] } }, // a combo input and an input the listing does not name: not compared
    9: { class_type: "PreviewAny", inputs: { source: ["1", 1] } }, // "*" takes any type
    10: { class_type: "ImageInvert", inputs: { image: ["8", 1] } }, // a MASK into an IMAGE input
    11: { class_type: "ImageInvert", inputs: { image: ["6", 0] } }, // IMAGE,MASK into IMAGE: a common member
    12: { class_type: "ImageInvert", inputs: { image: ["13", 0] } }, // a COMFY_*_V3 output: not compared
    13: { class_type: "MatchOut", inputs: {} },
  };
  const oi = { ...OI, MatchOut: { input: { required: {} }, output: ["COMFY_MATCHTYPE_V3"] } };
  assert.deepEqual(graphProblems(g, oi), [
    "2.vae: wire from slot 0 of 1 (CheckpointLoaderSimple) carries MODEL, the input wants VAE",
    "4.mesh: wire from slot 0 of 6 (ResizeSynth) carries IMAGE,MASK, the input wants MESH,FILE_3D_GLB,FILE_3D",
    "10.image: wire from slot 1 of 8 (LoadImage) carries MASK, the input wants IMAGE",
  ]);
});

test("golden: the subgraph template expands to ids <instance>:<inner>, with its inputs and output rewired", () => {
  const r = convert(readJson("subgraph-i2v.workflow.json"));
  assert.deepEqual(compareApiGraphs(graphOf(r), readJson("subgraph-i2v.api.json")), []);
  assert.deepEqual(graphProblems(r.graph, OI), []);
  assert.equal(r.stats.expandedSubgraphs, 1);
});

test("golden: a mutated widget order turns the golden red (a swap, a type-breaking move, a combo swap)", () => {
  const golden = readJson("basic-t2i.api.json");
  const wf = readJson("basic-t2i.workflow.json");
  const mutate = (fn) => {
    const oi = clone(OI);
    fn(oi.KSampler);
    return oi;
  };
  const reorder = (info, order) => {
    info.input_order.required = order;
    const old = info.input.required;
    info.input.required = Object.fromEntries(order.map((k) => [k, old[k]]));
  };
  const base = ["model", "seed", "steps", "cfg", "sampler_name", "scheduler", "positive", "negative", "latent_image", "denoise"];
  const swap = (a, b) => base.map((k) => (k === a ? b : k === b ? a : k));
  const swapSteps = convert(wf, {}, mutate((k) => reorder(k, swap("steps", "cfg"))));
  assert.equal(swapSteps.ok, true);
  assert.notDeepEqual(compareApiGraphs(swapSteps.graph, golden), [], "steps and cfg swapped: 20 and 8 land on the wrong inputs");
  const swapCombos = convert(wf, {}, mutate((k) => reorder(k, swap("sampler_name", "scheduler"))));
  assert.equal(swapCombos.ok, true);
  assert.notDeepEqual(compareApiGraphs(swapCombos.graph, golden), [], "the two combos swapped");
  const moved = convert(wf, {}, mutate((k) => reorder(k, ["model", "steps", "seed", "cfg", "sampler_name", "scheduler", "positive", "negative", "latent_image", "denoise"])));
  assert.ok(moved.ok === false || compareApiGraphs(moved.graph, golden).length > 0, "seed after steps: refused or different, never equal");
  assert.deepEqual(compareApiGraphs(convert(wf).graph, golden), [], "the unmutated listing is still green");
});

test("compareApiGraphs: reports a missing node, an extra node, a class change, a value change, a missing and an extra input; ignores key order", () => {
  const a = { 1: { class_type: "A", inputs: { x: 1, y: ["2", 0] } }, 2: { class_type: "B", inputs: {} }, 4: { class_type: "D", inputs: {} } };
  const b = { 2: { class_type: "B", inputs: {} }, 1: { class_type: "A", inputs: { y: ["2", 0], x: 1 } } };
  assert.deepEqual(compareApiGraphs(b, { 1: a[1], 2: a[2] }), [], "key order is not a difference");
  const diffs = compareApiGraphs(a, { 1: { class_type: "A2", inputs: { x: 2, z: 3, y: ["2", 1] } }, 2: a[2], 3: { class_type: "C", inputs: {} } });
  const text = diffs.join("\n");
  assert.match(text, /3: missing/);
  assert.match(text, /4: extra/);
  assert.match(text, /1: class_type A != A2/);
  assert.match(text, /1\.x: 1 != 2/);
  assert.match(text, /1\.z: missing/);
  assert.match(text, /1\.y: \["2",0\] != \["2",1\]/);
  assert.deepEqual(compareApiGraphs({ 1: { class_type: "A", inputs: {}, _meta: { title: "x" } } }, { 1: { class_type: "A", inputs: {} } }), [], "_meta is not compared by default");
  assert.notDeepEqual(compareApiGraphs({ 1: { class_type: "A", inputs: {}, _meta: { title: "x" } } }, { 1: { class_type: "A", inputs: {}, _meta: { title: "y" } } }, { meta: true }), []);
});

test("compareApiGraphs: an input only the actual graph has is an extra, and the mirror case is a missing one, on an otherwise equal node", () => {
  const withW = { 1: { class_type: "A", inputs: { x: 1, w: 2 } } };
  const without = { 1: { class_type: "A", inputs: { x: 1 } } };
  assert.deepEqual(compareApiGraphs(withW, without), ["1.w: extra (2)"]);
  assert.deepEqual(compareApiGraphs(without, withW), ["1.w: missing (expected 2)"]);
  const multi = compareApiGraphs({ 1: { class_type: "A", inputs: { x: 1, w: ["3", 0] } }, 2: { class_type: "B", inputs: {} } }, { 1: { class_type: "A", inputs: { x: 1 } }, 2: { class_type: "B", inputs: {} } });
  assert.match(multi.join("\n"), /1\.w: extra/);
  assert.equal(multi.length, 1, "nothing else differs");
  assert.deepEqual(compareApiGraphs(withW, withW), []);
});

// ---- the object_info: where the widget order comes from ------------------------------------------------------------------

test("object_info order: input_order wins over the key order of the listing, and the key order is the fallback", () => {
  const wf = readJson("basic-t2i.workflow.json");
  const golden = readJson("basic-t2i.api.json");
  const shuffled = clone(OI);
  shuffled.KSampler.input.required = Object.fromEntries(Object.entries(shuffled.KSampler.input.required).sort(([a], [b]) => (a < b ? -1 : 1)));
  assert.deepEqual(compareApiGraphs(graphOf(convert(wf, {}, shuffled)), golden), [], "a listing re-serialised with sorted keys still converts, because input_order is read");
  const noOrder = clone(OI);
  for (const c of Object.values(noOrder)) delete c.input_order;
  assert.deepEqual(compareApiGraphs(graphOf(convert(wf, {}, noOrder)), golden), [], "no input_order: the key order is used");
  const inconsistent = clone(OI);
  inconsistent.KSampler.input_order.required = ["model", "seed", "steps"];
  refusalOf(convert(wf, {}, inconsistent), REFUSAL.BAD_OBJECT_INFO);
  for (const bad of [null, [], "x", {}, 5]) refusalOf(convert(wf, {}, bad), REFUSAL.BAD_OBJECT_INFO);
});

test("hidden inputs of a class (prompt, extra_pnginfo) are never emitted", () => {
  const graph = graphOf(convert(readJson("basic-t2i.workflow.json")));
  assert.deepEqual(Object.keys(graph["9"].inputs).sort(), ["filename_prefix", "images"]);
});

// ---- widget mapping ------------------------------------------------------------------------------------------------------

test("control_after_generate: a flagged INT (true or a string such as fixed) owns one hidden slot, read from the flag", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "RandomNoise", outs: [["NOISE", "NOISE"]], wv: [900359092894726, "randomize"] },
    { id: 2, type: "PrimitiveInt", outs: [["INT", "INT"]], wv: [5, "fixed"] },
  ])));
  assert.deepEqual(g["1"].inputs, { noise_seed: 900359092894726 });
  assert.deepEqual(g["2"].inputs, { value: 5 });
});

test("control_after_generate: a flagged INT whose next value is not a control word is refused, a flagged INT at the end of the values is fine", () => {
  const r = refusalOf(convert(ui([{ id: 1, type: "RandomNoise", outs: [["NOISE", "NOISE"]], wv: [5, 6] }])), REFUSAL.WIDGET_MISMATCH);
  assert.equal(r.details.node, "1");
  assert.equal(r.details.class_type, "RandomNoise");
  assert.deepEqual(graphOf(convert(ui([{ id: 1, type: "RandomNoise", outs: [["NOISE", "NOISE"]], wv: [5] }])))["1"].inputs, { noise_seed: 5 });
});

test("control_after_generate: a seed INT with no flag takes the slot only when the next value is a control word", () => {
  const withWord = graphOf(convert(ui([{ id: 1, type: "OldSampler", outs: [["LATENT", "LATENT"]], wv: [7, "randomize", 30] }])));
  assert.deepEqual(withWord["1"].inputs, { seed: 7, steps: 30 });
  const without = graphOf(convert(ui([{ id: 1, type: "SeedPlain", outs: [["LATENT", "LATENT"]], wv: [42, 0.5] }])));
  assert.deepEqual(without["1"].inputs, { seed: 42, strength: 0.5 }, "a name is not a flag: no control word, no slot");
});

test("upload widgets: a LoadImage owns one extra slot and a LoadAudio two", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
    { id: 2, type: "LoadAudio", outs: [["AUDIO", "AUDIO"]], wv: ["x.mp3", null, null] },
    { id: 3, type: "LoadAudio", outs: [["AUDIO", "AUDIO"]], wv: ["y.wav"] },
  ])));
  assert.deepEqual(g["1"].inputs, { image: "b.png" });
  assert.deepEqual(g["2"].inputs, { audio: "x.mp3" });
  assert.deepEqual(g["3"].inputs, { audio: "y.wav" });
  refusalOf(convert(ui([{ id: 2, type: "LoadAudio", outs: [["AUDIO", "AUDIO"]], wv: ["x.mp3", null, null, 9] }])), REFUSAL.WIDGET_MISMATCH);
});

test("upload widgets: a video loader owns one extra slot like an image loader, stored or not, and one more value is refused", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "LoadVideo", outs: [["VIDEO", "VIDEO"]], wv: ["a.mp4", "image"] },
    { id: 2, type: "LoadVideo", outs: [["VIDEO", "VIDEO"]], wv: ["b.mp4"] },
  ])));
  assert.deepEqual(g["1"].inputs, { file: "a.mp4" });
  assert.deepEqual(g["2"].inputs, { file: "b.mp4" });
  refusalOf(convert(ui([{ id: 1, type: "LoadVideo", outs: [["VIDEO", "VIDEO"]], wv: ["a.mp4", "image", "x"] }])), REFUSAL.WIDGET_MISMATCH);
});

test("a widget whose type is a union names its widgetType, and a forceInput STRING is a socket with no value slot", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "PrimitiveStringMultiline", outs: [["STRING", "STRING"]], wv: ["hello"] },
    { id: 2, type: "UnionWidget", ins: [["label", "STRING"]], outs: [["INT", "INT"]], wv: [24, 3] },
  ], [[1, 0, 2, "label"]])));
  assert.deepEqual(g["2"].inputs, { rate: 24, count: 3, label: ["1", 0] });
});

test("too many widget values is refused with the counts, too few fills the object_info default and says so", () => {
  const r = refusalOf(convert(ui([{ id: 1, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["a", "extra"] }])), REFUSAL.WIDGET_MISMATCH);
  assert.equal(r.details.node, "1");
  assert.equal(r.details.expected, 1);
  assert.equal(r.details.got, 2);
  const under = convert(ui([
    { id: 1, type: "EmptyLatentImage", outs: [["LATENT", "LATENT"]], wv: [640] },
    { id: 2, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: [] },
  ]));
  assert.deepEqual(graphOf(under)["1"].inputs, { width: 640, height: 512, batch_size: 1 });
  assert.deepEqual(warned(under, "default_filled").map((w) => `${w.node}.${w.input}`), ["1.height", "1.batch_size"]);
  assert.equal("text" in graphOf(under)["2"].inputs, false, "no default in the listing: the input is left out, not invented");
  assert.deepEqual(warned(under, "widget_value_missing").map((w) => `${w.node}.${w.input}`), ["2.text"]);
});

test("widgets_values as an object (keyed by input name) maps by name, and a key that is no input is ignored with a warning", () => {
  const r = convert(ui([{ id: 1, type: "EmptyLatentImage", outs: [["LATENT", "LATENT"]], wv: { width: 100, height: 200, batch_size: 2, videopreview: { hidden: true } } }]));
  assert.deepEqual(graphOf(r)["1"].inputs, { width: 100, height: 200, batch_size: 2 });
  assert.deepEqual(warned(r, "widget_key_ignored").map((w) => w.input), ["videopreview"]);
});

test("a custom frontend widget (socketless, not a standard type), an autogrow with widget children and an unknown schema type are refused, not mapped by guess", () => {
  const custom = refusalOf(convert(ui([{ id: 1, type: "CustomWidgetNode", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [{ x: 1 }] }])), REFUSAL.UNSUPPORTED_INPUT_TYPE);
  assert.deepEqual([custom.details.class_type, custom.details.input], ["CustomWidgetNode", "crop_region"]);
  const grow = refusalOf(convert(ui([{ id: 1, type: "AutogrowWidgets", outs: [["IMAGE", "IMAGE"]], wv: [] }])), REFUSAL.UNSUPPORTED_INPUT_TYPE);
  assert.equal(grow.details.input, "floats");
  const future = refusalOf(convert(ui([{ id: 1, type: "FutureSchemaNode", outs: [["IMAGE", "IMAGE"]], wv: [] }])), REFUSAL.UNSUPPORTED_INPUT_TYPE);
  assert.equal(future.details.type, "COMFY_FUTURESLOT_V3");
});

test("a socketless input that is also forceInput is a socket (wired or empty), not a custom widget to refuse", () => {
  const g = graphOf(convert(ui([{ id: 1, type: "ForceSocketNode", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [8] }])));
  assert.deepEqual(g["1"].inputs, { steps: 8 });
});

test("a custom widget that carries image_upload but is not a combo owns no upload slot, a COLOR widget is its stored string, hidden INT widgets keep their slots", () => {
  const g = graphOf(convert(ui([{ id: 1, type: "PainterLike", outs: [["IMAGE", "IMAGE"]], wv: ["painter.png [input]", 1024, 768, "#102030"] }])));
  assert.deepEqual(g["1"].inputs, { mask: "painter.png [input]", width: 1024, height: 768, bg_color: "#102030" });
});

test("display-only trailing values of a preview node are ignored with a warning, one more than the allowance is refused", () => {
  const r = convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "PreviewAny", ins: [["source", "*"]], wv: [null, null, false] },
    { id: 3, type: "SaveGLB", ins: [["mesh", "MESH"]], wv: ["3d/x", ""] },
  ], [[1, 0, 2, "source"]]));
  const g = graphOf(r);
  assert.deepEqual(g["2"].inputs, { source: ["1", 0] });
  assert.deepEqual(g["3"].inputs, { filename_prefix: "3d/x" });
  assert.deepEqual(warned(r, "display_values_ignored").map((w) => `${w.node}:${w.count}`), ["2:3", "3:1"]);
  refusalOf(convert(ui([{ id: 2, type: "PreviewAny", ins: [["source", "*"]], wv: [null, null, false, 1] }])), REFUSAL.WIDGET_MISMATCH);
  refusalOf(convert(ui([{ id: 3, type: "SaveGLB", ins: [["mesh", "MESH"]], wv: ["3d/x", "", ""] }])), REFUSAL.WIDGET_MISMATCH);
  refusalOf(convert(ui([{ id: 4, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], wv: ["a", null] }])), REFUSAL.WIDGET_MISMATCH);
});

test("display-only trailing values: GetImageSize and Preview3D each allow one, a second is refused", () => {
  const r = convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "GetImageSize", ins: [["image", "IMAGE"]], outs: [["width", "INT"], ["height", "INT"], ["batch_size", "INT"]], wv: ["512x512"] },
    { id: 3, type: "Preview3D", ins: [["model_file", "MESH,FILE_3D"]], wv: [""] },
  ], [[1, 0, 2, "image"]]));
  const g = graphOf(r);
  assert.deepEqual(g["2"].inputs, { image: ["1", 0] });
  assert.deepEqual(g["3"].inputs, {});
  assert.deepEqual(warned(r, "display_values_ignored").map((w) => `${w.node}:${w.count}`), ["2:1", "3:1"]);
  refusalOf(convert(ui([{ id: 2, type: "GetImageSize", ins: [["image", "IMAGE"]], wv: ["512x512", ""] }])), REFUSAL.WIDGET_MISMATCH);
  refusalOf(convert(ui([{ id: 3, type: "Preview3D", ins: [["model_file", "MESH,FILE_3D"]], wv: ["", ""] }])), REFUSAL.WIDGET_MISMATCH);
});

// ---- dotted names --------------------------------------------------------------------------------------------------------

test("dotted dynamic-combo names: the selected option's widgets follow the combo as <name>.<child>, in the option's input order", () => {
  const dyn = (wv) => graphOf(convert(ui([{ id: 1, type: "DynSampler", ins: [["clip", "CLIP"]], outs: [["generated_text", "STRING"]], wv }])))["1"].inputs;
  assert.deepEqual(dyn(["hi", 600, "on", 0.7, 64, 0, 0.1, false]), {
    prompt: "hi", max_length: 600, mode: "on", "mode.temperature": 0.7, "mode.top_k": 64, "mode.seed": 0, "mode.presence_penalty": 0.1, thinking: false,
  });
  assert.deepEqual(dyn(["hi", 600, "off", false]), { prompt: "hi", max_length: 600, mode: "off", thinking: false });
  const bad = refusalOf(convert(ui([{ id: 1, type: "DynSampler", ins: [["clip", "CLIP"]], outs: [["generated_text", "STRING"]], wv: ["hi", 600, "turbo", false] }])), REFUSAL.WIDGET_MISMATCH);
  assert.equal(bad.details.input, "mode");
  assert.deepEqual(bad.details.options, ["on", "off"]);
});

test("dotted dynamic-combo names: a MATCHTYPE link input, then the combo's child between two widgets (the shape a real resize node has)", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "ResizeSynth", ins: [["input", "IMAGE,MASK"]], outs: [["resized", "IMAGE,MASK"]], wv: ["scale longer dimension", 1536, "lanczos"] },
  ], [[1, 0, 2, "input"]])));
  assert.deepEqual(g["2"].inputs, { resize_type: "scale longer dimension", "resize_type.longer_size": 1536, scale_method: "lanczos", input: ["1", 0] });
});

test("dotted dynamic-combo names nest: a combo inside an option is named <outer>.<inner>.<child>", () => {
  const nested = (wv) => graphOf(convert(ui([{ id: 1, type: "NestedDyn", outs: [["VIDEO", "VIDEO"]], wv }])))["1"].inputs;
  assert.deepEqual(nested(["h264", "fast", 18, "main", "mp4"]), { codec: "h264", "codec.encoding": "fast", "codec.encoding.crf": 18, "codec.profile": "main", format: "mp4" });
  assert.deepEqual(nested(["h264", "slow", "baseline", "webm"]), { codec: "h264", "codec.encoding": "slow", "codec.profile": "baseline", format: "webm" });
  assert.deepEqual(nested(["raw", "webm"]), { codec: "raw", format: "webm" });
});

test("dotted autogrow names: the children are link inputs named values.a, values.b, taken from the node's own input slots", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "PrimitiveInt", outs: [["INT", "INT"]], wv: [3, "fixed"] },
    { id: 2, type: "ComfyMathExpression", ins: [["values.a", "FLOAT,INT,BOOLEAN"], ["values.b", "FLOAT,INT,BOOLEAN"]], outs: [["FLOAT", "FLOAT"], ["INT", "INT"], ["BOOL", "BOOLEAN"]], wv: ["a/2"] },
  ], [[1, 0, 2, "values.a"]])));
  assert.deepEqual(g["2"].inputs, { expression: "a/2", "values.a": ["1", 0] });
});

// ---- the frontend-only classes -------------------------------------------------------------------------------------------

test("Note and MarkdownNote are skipped, also when the listing does not know them and when they are muted", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "Note", wv: ["x"] },
    { id: 2, type: "MarkdownNote", wv: ["# y"] },
    { id: 3, type: "MarkdownNote", mode: 2, wv: ["# z"] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ])));
  assert.deepEqual(Object.keys(g), ["4"]);
});

test("Reroute nodes are resolved: a chain of them is the wire it carries, and a muted one cuts it", () => {
  const chain = convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "Reroute", ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
    { id: 3, type: "Reroute", ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, ""], [2, 0, 3, ""], [3, 0, 4, "image"]]));
  assert.deepEqual(graphOf(chain)["4"].inputs, { image: ["1", 0] });
  assert.deepEqual(Object.keys(graphOf(chain)).sort(), ["1", "4"]);
  const muted = convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "Reroute", mode: 2, ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, ""], [2, 0, 4, "image"]]));
  assert.deepEqual(graphOf(muted)["4"].inputs, {});
  assert.equal(warned(muted, "input_dropped")[0].reason, "muted");
  const loop = convert(ui([
    { id: 2, type: "Reroute", ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
    { id: 3, type: "Reroute", ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[2, 0, 3, ""], [3, 0, 2, ""], [3, 0, 4, "image"]]));
  refusalOf(loop, REFUSAL.LINK_CYCLE);
});

test("an unconnected Reroute feeds nothing: a widget keeps its stored value, a socket stays empty, and nothing is warned", () => {
  const r = convert(ui([
    { id: 1, type: "Reroute", ins: [["", "INT"]], outs: [["", "INT"]] },
    { id: 2, type: "EmptyLatentImage", ins: [["width", "INT", true]], outs: [["LATENT", "LATENT"]], wv: [512, 384, 1] },
    { id: 3, type: "Reroute", ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "width"], [3, 0, 4, "image"]]));
  const g = graphOf(r);
  assert.deepEqual(g["2"].inputs, { width: 512, height: 384, batch_size: 1 });
  assert.deepEqual(g["4"].inputs, {});
  assert.deepEqual(r.warnings, [], "nothing was lost, so nothing is said (a muted Reroute, which cut a wire, does say so)");
});

test("stats count what the converter met: notes, reroutes, primitive nodes, muted and bypassed nodes, and the nodes of the graph", () => {
  const r = convert(ui([
    { id: 1, type: "Note", wv: ["x"] },
    { id: 2, type: "MarkdownNote", wv: ["y"] },
    { id: 3, type: "PrimitiveNode", outs: [["INT", "INT"]], wv: [640, "fixed"] },
    { id: 4, type: "Reroute", ins: [["", "INT"]], outs: [["", "INT"]] },
    { id: 5, type: "Reroute", ins: [["", "INT"]], outs: [["", "INT"]] },
    { id: 6, type: "EmptyLatentImage", ins: [["width", "INT", true]], outs: [["LATENT", "LATENT"]], wv: [512, 512, 1] },
    { id: 7, type: "LoraLoader", mode: 2 },
    { id: 8, type: "LoraLoader", mode: 4 },
  ], [[3, 0, 4, ""], [4, 0, 5, ""], [5, 0, 6, "width"]]));
  assert.deepEqual(graphOf(r)["6"].inputs, { width: 640, height: 512, batch_size: 1 });
  assert.deepEqual(r.stats, { nodes: 1, expandedSubgraphs: 0, mutedNodes: 1, bypassedNodes: 1, notes: 2, reroutes: 2, primitiveNodes: 1 });
});

test("PrimitiveNode feeds its value into every widget input it is wired to, and nothing of it reaches the graph", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "PrimitiveNode", outs: [["INT", "INT"]], wv: [640, "fixed"] },
    { id: 2, type: "EmptyLatentImage", ins: [["width", "INT", true]], outs: [["LATENT", "LATENT"]], wv: [512, 512, 1] },
    { id: 3, type: "EmptyLatentImage", ins: [["height", "INT", true]], outs: [["LATENT", "LATENT"]], wv: [512, 512, 4] },
  ], [[1, 0, 2, "width"], [1, 0, 3, "height"]])));
  assert.deepEqual(Object.keys(g), ["2", "3"]);
  assert.deepEqual(g["2"].inputs, { width: 640, height: 512, batch_size: 1 });
  assert.deepEqual(g["3"].inputs, { width: 512, height: 640, batch_size: 4 });
});

test("PrimitiveNode: a combo value, a primitive with no value is refused, a muted one leaves the consumer its stored widget value and says so", () => {
  const combo = graphOf(convert(ui([
    { id: 1, type: "PrimitiveNode", outs: [["COMBO", "COMBO"]], wv: ["sd_b.safetensors", "fixed"] },
    { id: 2, type: "CheckpointLoaderSimple", ins: [["ckpt_name", "COMBO", true]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] },
  ], [[1, 0, 2, "ckpt_name"]])));
  assert.deepEqual(combo["2"].inputs, { ckpt_name: "sd_b.safetensors" });
  const none = refusalOf(convert(ui([
    { id: 1, type: "PrimitiveNode", outs: [["INT", "INT"]] },
    { id: 2, type: "EmptyLatentImage", ins: [["width", "INT", true]], outs: [["LATENT", "LATENT"]], wv: [512, 512, 1] },
  ], [[1, 0, 2, "width"]])), REFUSAL.PRIMITIVE_WITHOUT_VALUE);
  assert.equal(none.details.node, "1");
  const muted = convert(ui([
    { id: 1, type: "PrimitiveNode", mode: 2, outs: [["INT", "INT"]], wv: [640, "fixed"] },
    { id: 2, type: "EmptyLatentImage", ins: [["width", "INT", true]], outs: [["LATENT", "LATENT"]], wv: [512, 512, 1] },
  ], [[1, 0, 2, "width"]]));
  assert.deepEqual(graphOf(muted)["2"].inputs, { width: 512, height: 512, batch_size: 1 });
  assert.deepEqual(warned(muted, "widget_link_dropped").map((w) => w.input), ["width"]);
});

test("link beats widget: a widget input that is wired takes the wire, whatever its stored value was", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "PrimitiveStringMultiline", outs: [["STRING", "STRING"]], wv: ["from the wire"] },
    { id: 2, type: "CLIPTextEncode", ins: [["text", "STRING", true], ["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["stored text"] },
  ], [[1, 0, 2, "text"]])));
  assert.deepEqual(g["2"].inputs, { text: ["1", 0] });
});

// ---- mute and bypass -----------------------------------------------------------------------------------------------------

test("mode 2 (muted): the node is dropped and an input wired to it is left out with a typed warning", () => {
  const r = convert(ui([
    { id: 1, type: "CheckpointLoaderSimple", outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] },
    { id: 2, type: "LoraLoader", mode: 2, ins: [["model", "MODEL"], ["clip", "CLIP"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_a.safetensors", 1, 1] },
    { id: 3, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["x"] },
  ], [[1, 0, 2, "model"], [1, 1, 2, "clip"], [2, 1, 3, "clip"]]));
  const g = graphOf(r);
  assert.deepEqual(Object.keys(g), ["1", "3"]);
  assert.deepEqual(g["3"].inputs, { text: "x" });
  assert.deepEqual(warned(r, "input_dropped"), [{ code: "input_dropped", node: "3", input: "clip", reason: "muted" }]);
  assert.equal(r.stats.mutedNodes, 1);
});

test("mode 4 (bypassed): an output takes the connected input of its type; chains resolve; a union type matches", () => {
  const spec = [
    { id: 1, type: "CheckpointLoaderSimple", outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] },
    { id: 2, type: "LoraLoader", mode: 4, ins: [["model", "MODEL"], ["clip", "CLIP"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_a.safetensors", 1, 1] },
    { id: 5, type: "LoraLoader", mode: 4, ins: [["model", "MODEL"], ["clip", "CLIP"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_b.safetensors", 1, 1] },
    { id: 3, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["x"] },
    { id: 4, type: "LoraLoader", ins: [["model", "MODEL"], ["clip", "CLIP"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_a.safetensors", 0.5, 0.5] },
  ];
  const r = convert(ui(spec, [[1, 0, 2, "model"], [1, 1, 2, "clip"], [2, 0, 5, "model"], [2, 1, 5, "clip"], [5, 1, 3, "clip"], [5, 0, 4, "model"], [5, 1, 4, "clip"]]));
  const g = graphOf(r);
  assert.deepEqual(Object.keys(g).sort(), ["1", "3", "4"], "neither bypassed node is emitted");
  assert.deepEqual(g["3"].inputs, { text: "x", clip: ["1", 1] }, "CLIP out of two chained bypasses is the checkpoint's CLIP");
  assert.deepEqual(g["4"].inputs, { lora_name: "style_a.safetensors", strength_model: 0.5, strength_clip: 0.5, model: ["1", 0], clip: ["1", 1] });
  assert.equal(r.stats.bypassedNodes, 2);
  const union = convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "ResizeSynth", mode: 4, ins: [["input", "IMAGE,MASK"]], outs: [["resized", "IMAGE"]], wv: ["scale dimensions", 64, 64, "center", "nearest"] },
    { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "input"], [2, 0, 3, "image"]]));
  assert.deepEqual(graphOf(union)["3"].inputs, { image: ["1", 0] }, "IMAGE,MASK and IMAGE intersect");
});

test("mode 4 (bypassed): no input of the output's type drops the wire with a typed warning", () => {
  const r = convert(ui([
    { id: 1, type: "KSampler", outs: [["LATENT", "LATENT"]], wv: [1, "fixed", 20, 8, "euler", "normal", 1] },
    { id: 2, type: "VAEDecode", mode: 4, ins: [["samples", "LATENT"], ["vae", "VAE"]], outs: [["IMAGE", "IMAGE"]] },
    { id: 3, type: "SaveImage", ins: [["images", "IMAGE"]], wv: ["x"] },
  ], [[1, 0, 2, "samples"], [2, 0, 3, "images"]]));
  assert.deepEqual(graphOf(r)["3"].inputs, { filename_prefix: "x" });
  assert.deepEqual(warned(r, "input_dropped"), [{ code: "input_dropped", node: "3", input: "images", reason: "bypass_no_match" }]);
});

test("mode 4 (bypassed): an unconnected input is not a candidate, so the one connected input of the output's type is passed through", () => {
  const spec = [
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
    { id: 3, type: "ImageMix", mode: 4, ins: [["image_a", "IMAGE"], ["image_b", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [0.5] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ];
  const onlyB = convert(ui(spec, [[2, 0, 3, "image_b"], [3, 0, 4, "image"]]));
  assert.deepEqual(graphOf(onlyB)["4"].inputs, { image: ["2", 0] }, "image_a is not connected");
  const onlyA = convert(ui(spec, [[1, 0, 3, "image_a"], [3, 0, 4, "image"]]));
  assert.deepEqual(graphOf(onlyA)["4"].inputs, { image: ["1", 0] });
});

test("mode 4 (bypassed): two bypassed nodes wired to each other and read by a consumer are a link_cycle, not a hang or an internal_error", () => {
  const pass = (id) => ({ id, type: "ImageInvert", mode: 4, ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] });
  const r = convert(ui([
    pass(2), pass(3),
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[2, 0, 3, "image"], [3, 0, 2, "image"], [3, 0, 4, "image"]]));
  const refusal = refusalOf(r, REFUSAL.LINK_CYCLE);
  assert.match(refusal.details.node, /^[23]$/);
  // one bypassed node wired to itself is the same loop
  refusalOf(convert(ui([pass(2), { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] }], [[2, 0, 2, "image"], [2, 0, 4, "image"]])), REFUSAL.LINK_CYCLE);
});

test("mode 4 (bypassed): an unconnected input that has no link key at all (undefined, not null) is not a candidate either", () => {
  const spec = [
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 3, type: "ImageMix", mode: 4, ins: [["image_a", "IMAGE"], ["image_b", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [0.5] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ];
  const wf = ui(spec, [[1, 0, 3, "image_b"], [3, 0, 4, "image"]]);
  delete wf.nodes[1].inputs[0].link; // image_a: { name, type } and nothing else
  assert.equal("link" in wf.nodes[1].inputs[0], false);
  assert.deepEqual(graphOf(convert(wf))["4"].inputs, { image: ["1", 0] }, "image_b is the one connected input");
  const none = ui(spec, [[3, 0, 4, "image"]]);
  delete none.nodes[1].inputs[0].link;
  delete none.nodes[1].inputs[1].link;
  const r = convert(none);
  assert.deepEqual(graphOf(r)["4"].inputs, {}, "no connected input of the type: the wire is dropped");
  assert.deepEqual(warned(r, "input_dropped"), [{ code: "input_dropped", node: "4", input: "image", reason: "bypass_no_match" }]);
});

// the nodes a bypass test needs: two prompts, a bypassed pair node (positive and negative conditioning in and out, like the
// ControlNet apply nodes), a sampler that reads both outputs, and a checkpoint for a third type
const cond = (...names) => names.map((n) => [n, "CONDITIONING"]);
const pairSpecs = (ins, outs, mode = 4) => [
  { id: 1, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: cond("CONDITIONING"), wv: ["a cat"] },
  { id: 2, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: cond("CONDITIONING"), wv: ["blurry"] },
  { id: 3, type: "ControlNetPair", mode, ins, outs },
  { id: 4, type: "KSampler", ins: [["positive", "CONDITIONING"], ["negative", "CONDITIONING"]], outs: [["LATENT", "LATENT"]], wv: [1, "fixed", 20, 8, "euler", "normal", 1] },
  { id: 5, type: "CheckpointLoaderSimple", outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] },
];
const sampler = (r) => { const g = graphOf(r)["4"].inputs; return [g.positive, g.negative]; };

test("mode 4 (bypassed): the i-th output of a type takes the i-th connected input of that type, so positive and negative pass straight through", () => {
  const into = [[1, 0, 3, "positive"], [2, 0, 3, "negative"]];
  const straight = convert(ui(pairSpecs(cond("positive", "negative"), cond("positive", "negative")), [...into, [3, 0, 4, "positive"], [3, 1, 4, "negative"]]));
  assert.deepEqual(sampler(straight), [["1", 0], ["2", 0]], "slot 0 carries the first prompt and slot 1 the second");
  const crossed = convert(ui(pairSpecs(cond("positive", "negative"), cond("positive", "negative")), [...into, [3, 1, 4, "positive"], [3, 0, 4, "negative"]]));
  assert.deepEqual(sampler(crossed), [["2", 0], ["1", 0]], "the sampler reads slot 1 as positive: that is the second prompt, not the first");
  const mixed = convert(ui(pairSpecs([["positive", "CONDITIONING"], ["vae", "VAE"], ["negative", "CONDITIONING"]], [["positive", "CONDITIONING"], ["negative", "CONDITIONING"], ["vae", "VAE"]]),
    [...into, [5, 2, 3, "vae"], [3, 0, 4, "positive"], [3, 1, 4, "negative"]]));
  assert.deepEqual(sampler(mixed), [["1", 0], ["2", 0]], "an input of another type between them does not shift the rank within a type");
  assert.equal(graphOf(mixed)["5"].class_type, "CheckpointLoaderSimple");
  assert.deepEqual(warned(mixed, "input_dropped"), []);
});

test("mode 4 (bypassed): a type whose outputs and connected inputs differ in number is ambiguous and refused as ambiguous_bypass, naming the node", () => {
  assert.equal(REFUSAL.AMBIGUOUS_BYPASS, "ambiguous_bypass");
  // two conditioning outputs, one connected conditioning input
  const fewer = refusalOf(convert(ui(pairSpecs(cond("positive", "negative"), cond("positive", "negative")), [[1, 0, 3, "positive"], [3, 0, 4, "positive"]])), REFUSAL.AMBIGUOUS_BYPASS);
  assert.equal(fewer.details.node, "3");
  assert.equal(fewer.details.type, "CONDITIONING");
  assert.deepEqual([fewer.details.outputs, fewer.details.inputs], [2, 1]);
  assert.match(fewer.message, /\b3\b/, "the message names the node too");
  // one image output, two connected image inputs
  const spec = [
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
    { id: 3, type: "ImageMix", mode: 4, ins: [["image_a", "IMAGE"], ["image_b", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [0.5] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ];
  const more = refusalOf(convert(ui(spec, [[1, 0, 3, "image_a"], [2, 0, 3, "image_b"], [3, 0, 4, "image"]])), REFUSAL.AMBIGUOUS_BYPASS);
  assert.equal(more.details.node, "3");
  assert.deepEqual([more.details.type, more.details.outputs, more.details.inputs], ["IMAGE", 1, 2]);
  // a bypassed node nobody reads is never resolved, so it cannot be ambiguous
  const unread = convert(ui(pairSpecs(cond("positive", "negative"), cond("positive", "negative")), [[1, 0, 3, "positive"]]));
  assert.equal(unread.ok, true);
  assert.equal(unread.graph["3"], undefined);
});

test("mode 4 (bypassed): inside a subgraph the ambiguous node is named by its full id", () => {
  const def = subdef(SG(1), {
    ins: [["a", "IMAGE"], ["b", "IMAGE"]], outs: [["out", "IMAGE"]],
    nodes: [{ id: 4, type: "ImageMix", mode: 4, ins: [["image_a", "IMAGE"], ["image_b", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [0.5] }],
    edges: [[-10, 0, 4, "image_a"], [-10, 1, 4, "image_b"], [4, 0, -20, "out"]],
  });
  const wf = withDefs(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 5, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
    { id: 2, type: SG(1), ins: [["a", "IMAGE"], ["b", "IMAGE"]], outs: [["out", "IMAGE"]], wv: [] },
    { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "a"], [5, 0, 2, "b"], [2, 0, 3, "image"]]), def);
  assert.equal(refusalOf(convert(wf), REFUSAL.AMBIGUOUS_BYPASS).details.node, "2:4");
});

test("mode 4 (bypassed): a node typed * matches any type, in both directions", () => {
  const g = graphOf(convert(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "AnyIn", mode: 4, ins: [["in", "*"]], outs: [["out", "IMAGE"]] },
    { id: 3, type: "AnyOut", mode: 4, ins: [["image", "IMAGE"]], outs: [["out", "*"]] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "in"], [2, 0, 3, "image"], [3, 0, 4, "image"]])));
  assert.deepEqual(g["4"].inputs, { image: ["1", 0] });
});

test("mode 4 (bypassed): a union input matches an output on any of its members, not only the first", () => {
  const spec = (inType) => [
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "AnyIn", mode: 4, ins: [["in", inType]], outs: [["out", "IMAGE"]] },
    { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ];
  const edges = [[1, 0, 2, "in"], [2, 0, 3, "image"]];
  // IMAGE is the second member of the input's union
  const second = convert(ui(spec("MASK,IMAGE"), edges));
  assert.deepEqual(graphOf(second)["3"].inputs, { image: ["1", 0] }, "MASK,IMAGE and IMAGE share IMAGE");
  assert.deepEqual(warned(second, "input_dropped"), []);
  // the control: a union with no member of the output's type matches nothing and the wire is dropped
  const none = convert(ui(spec("MASK,LATENT"), edges));
  assert.deepEqual(graphOf(none)["3"].inputs, {});
  assert.deepEqual(warned(none, "input_dropped"), [{ code: "input_dropped", node: "3", input: "image", reason: "bypass_no_match" }]);
});

test("mode 4 (bypassed): a union output matches an input on any of its members, not only the first", () => {
  const loader = { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] };
  const reader = { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] };
  const edges = [[1, 1, 2, "in"], [2, 0, 3, "image"]];
  const via = (inType, outType) => convert(ui([loader, { id: 2, type: "AnyBoth", mode: 4, ins: [["in", inType]], outs: [["out", outType]] }, reader], edges));
  // MASK is the second member of the output's union
  const second = via("MASK", "IMAGE,MASK");
  assert.deepEqual(graphOf(second)["3"].inputs, { image: ["1", 1] }, "MASK and IMAGE,MASK share MASK");
  assert.deepEqual(warned(second, "input_dropped"), []);
  // the third member of a longer union
  assert.deepEqual(graphOf(via("MASK", "IMAGE,LATENT,MASK"))["3"].inputs, { image: ["1", 1] });
  // both sides a union, the shared member second on each
  assert.deepEqual(graphOf(via("LATENT,MASK", "IMAGE,MASK"))["3"].inputs, { image: ["1", 1] });
  // the control: a union output with no member of the input's type matches nothing and the wire is dropped
  const none = via("MASK", "IMAGE,LATENT");
  assert.deepEqual(graphOf(none)["3"].inputs, {});
  assert.deepEqual(warned(none, "input_dropped"), [{ code: "input_dropped", node: "3", input: "image", reason: "bypass_no_match" }]);
});

test("mode 4 (bypassed): outputs are grouped by their exact type, so a union or * output beside a concrete one does not join its group", () => {
  const invert = (id) => ({ id, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] });
  const loaders = [
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
  ];
  // one connected IMAGE input and three outputs: IMAGE, *, IMAGE,MASK. Each is the only output of its own type, so each is
  // unambiguous (the * and the union output match the one connected input as well, which is the same single input)
  const one = convert(ui([
    ...loaders,
    { id: 3, type: "Pass", mode: 4, ins: [["in", "IMAGE"]], outs: [["a", "IMAGE"], ["b", "*"], ["c", "IMAGE,MASK"]] },
    invert(4), invert(5), invert(6),
  ], [[1, 0, 3, "in"], [3, 0, 4, "image"], [3, 1, 5, "image"], [3, 2, 6, "image"]]));
  const g1 = graphOf(one);
  for (const id of ["4", "5", "6"]) assert.deepEqual(g1[id].inputs, { image: ["1", 0] }, `node ${id} reads its own group's output`);
  // two connected IMAGE inputs and outputs IMAGE, IMAGE, *: the two IMAGE outputs pair with the two inputs by rank, and the *
  // output beside them neither counts toward their group nor is read here
  const two = convert(ui([
    ...loaders,
    { id: 3, type: "Pass", mode: 4, ins: [["x", "IMAGE"], ["y", "IMAGE"]], outs: [["a", "IMAGE"], ["b", "IMAGE"], ["c", "*"]] },
    invert(4), invert(5),
  ], [[1, 0, 3, "x"], [2, 0, 3, "y"], [3, 0, 4, "image"], [3, 1, 5, "image"]]));
  const g2 = graphOf(two);
  assert.deepEqual(g2["4"].inputs, { image: ["1", 0] });
  assert.deepEqual(g2["5"].inputs, { image: ["2", 0] });
  // the same exact type still counts: reading the * output itself, which matches both connected inputs, is ambiguous
  const star = refusalOf(convert(ui([
    ...loaders,
    { id: 3, type: "Pass", mode: 4, ins: [["x", "IMAGE"], ["y", "IMAGE"]], outs: [["a", "IMAGE"], ["b", "IMAGE"], ["c", "*"]] },
    invert(4),
  ], [[1, 0, 3, "x"], [2, 0, 3, "y"], [3, 2, 4, "image"]])), REFUSAL.AMBIGUOUS_BYPASS);
  assert.deepEqual([star.details.type, star.details.outputs, star.details.inputs], ["*", 1, 2]);
});

// ---- subgraphs -----------------------------------------------------------------------------------------------------------

test("subgraph: a nested instance gets the id path <outer>:<inner>:<node>, with the outer wire reaching the innermost node", () => {
  const inner = subdef(SG(2), {
    ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]],
    nodes: [{ id: 7, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] }],
    edges: [[-10, 0, 7, "image"], [7, 0, -20, "out"]],
  });
  const outer = subdef(SG(1), {
    ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]],
    nodes: [{ id: 5, type: SG(2), ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]] }],
    edges: [[-10, 0, 5, "img"], [5, 0, -20, "out"]],
  });
  const wf = withDefs(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: SG(1), ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]], wv: [] },
    { id: 3, type: "SaveImage", ins: [["images", "IMAGE"]], wv: ["x"] },
  ], [[1, 0, 2, "img"], [2, 0, 3, "images"]]), outer, inner);
  const r = convert(wf);
  const g = graphOf(r);
  assert.deepEqual(g["2:5:7"], { class_type: "ImageInvert", inputs: { image: ["1", 0] } });
  assert.deepEqual(g["3"].inputs, { filename_prefix: "x", images: ["2:5:7", 0] });
  assert.equal(r.stats.expandedSubgraphs, 2);
});

test("subgraph: a consumer reading output slot N of an instance gets the inner wire that ends at slot N of the output node, whatever order the definition lists its links in", () => {
  // three outputs of three different types; the inner links are listed in the reverse of the slot order, so the first link that
  // ends at the output node is not the one for slot 0 and the one for slot 1 is not first either
  const def = subdef(SG(1), {
    ins: [], outs: [["model", "MODEL"], ["clip", "CLIP"], ["vae", "VAE"]],
    nodes: [{ id: 7, type: "CheckpointLoaderSimple", outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] }],
    edges: [[7, 2, -20, "vae"], [7, 1, -20, "clip"], [7, 0, -20, "model"]],
  });
  const slots = [["model", "MODEL"], ["clip", "CLIP"], ["vae", "VAE"]];
  const wf = withDefs(ui([
    { id: 2, type: SG(1), outs: slots, wv: [] },
    { id: 3, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["x"] },
    { id: 4, type: "VAEDecode", ins: [["vae", "VAE"]], outs: [["IMAGE", "IMAGE"]] },
    { id: 5, type: "LoraLoader", ins: [["model", "MODEL"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_a.safetensors", 1, 1] },
  ], [[2, 1, 3, "clip"], [2, 2, 4, "vae"], [2, 0, 5, "model"]]), def);
  const r = convert(wf);
  const g = graphOf(r);
  assert.deepEqual(g["3"].inputs.clip, ["2:7", 1], "slot 1 is the CLIP, not the first link listed (the VAE)");
  assert.deepEqual(g["4"].inputs.vae, ["2:7", 2], "slot 2 is the VAE");
  assert.deepEqual(g["5"].inputs.model, ["2:7", 0], "slot 0 is the MODEL, not the first link listed (the VAE)");
  assert.deepEqual(graphProblems(g, OI), [], "and every wire carries the type its input wants");
  // a slot nothing in the definition ends at is an unconnected output, not the first link that ends anywhere
  const short = subdef(SG(1), {
    ins: [], outs: [["model", "MODEL"], ["clip", "CLIP"]],
    nodes: [{ id: 7, type: "CheckpointLoaderSimple", outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] }],
    edges: [[7, 1, -20, "clip"]],
  });
  const gap = convert(withDefs(ui([
    { id: 2, type: SG(1), outs: [["model", "MODEL"], ["clip", "CLIP"]], wv: [] },
    { id: 5, type: "LoraLoader", ins: [["model", "MODEL"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_a.safetensors", 1, 1] },
  ], [[2, 0, 5, "model"]]), short));
  assert.equal("model" in graphOf(gap)["5"].inputs, false, "the unconnected slot 0 leaves the input empty");
});

test("subgraph: an input wired straight to an output is the outer wire itself", () => {
  const pass = subdef(SG(1), { ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]], nodes: [], edges: [[-10, 0, -20, "out"]] });
  const wf = withDefs(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: SG(1), ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]], wv: [] },
    { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "img"], [2, 0, 3, "image"]]), pass);
  assert.deepEqual(graphOf(convert(wf))["3"].inputs, { image: ["1", 0] });
});

test("subgraph: instance widgets_values align with the definition's widget inputs, not with the instance's own (shorter) inputs", () => {
  // the definition has 3 widget inputs (a, b, c) and the instance lists only b: values are [a, b, c]
  const def = subdef(SG(1), {
    ins: [["a", "INT"], ["b", "INT"], ["c", "INT"]], outs: [],
    nodes: [
      { id: 10, type: "PrimitiveInt", ins: [["value", "INT", true]], outs: [["INT", "INT"]], wv: [0, "fixed"] },
      { id: 11, type: "PrimitiveInt", ins: [["value", "INT", true]], outs: [["INT", "INT"]], wv: [0, "fixed"] },
      { id: 12, type: "PrimitiveInt", ins: [["value", "INT", true]], outs: [["INT", "INT"]], wv: [0, "fixed"] },
    ],
    edges: [[-10, 0, 10, "value"], [-10, 1, 11, "value"], [-10, 2, 12, "value"]],
  });
  const g = graphOf(convert(withDefs(ui([{ id: 1, type: SG(1), ins: [["b", "INT", true]], wv: [11, 22, 33] }]), def)));
  assert.deepEqual([g["1:10"].inputs.value, g["1:11"].inputs.value, g["1:12"].inputs.value], [11, 22, 33]);
});

test("subgraph: a wire on the instance wins over the widget value for that input", () => {
  const def = subdef(SG(1), {
    ins: [["a", "INT"], ["b", "INT"]], outs: [],
    nodes: [
      { id: 10, type: "PrimitiveInt", ins: [["value", "INT", true]], outs: [["INT", "INT"]], wv: [0, "fixed"] },
      { id: 11, type: "PrimitiveInt", ins: [["value", "INT", true]], outs: [["INT", "INT"]], wv: [0, "fixed"] },
    ],
    edges: [[-10, 0, 10, "value"], [-10, 1, 11, "value"]],
  });
  const wf = withDefs(ui([
    { id: 2, type: "PrimitiveInt", outs: [["INT", "INT"]], wv: [99, "fixed"] },
    { id: 1, type: SG(1), ins: [["a", "INT", true], ["b", "INT", true]], wv: [11, 22] },
  ], [[2, 0, 1, "a"]]), def);
  const g = graphOf(convert(wf));
  assert.deepEqual(g["1:10"].inputs, { value: ["2", 0] });
  assert.deepEqual(g["1:11"].inputs, { value: 22 });
});

test("subgraph: an instance input whose name differs from the definition's is wired by position when both list as many inputs, and unwired otherwise", () => {
  const def = (ins) => subdef(SG(1), {
    ins, outs: [["out", "IMAGE"]],
    nodes: [{ id: 7, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] }],
    edges: [[-10, 0, 7, "image"], [7, 0, -20, "out"]],
  });
  const wf = (d, instIns) => withDefs(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: SG(1), ins: instIns, outs: [["out", "IMAGE"]], wv: [] },
  ], [[1, 0, 2, "renamed"]]), d);
  const same = graphOf(convert(wf(def([["img", "IMAGE"]]), [["renamed", "IMAGE"]])));
  assert.deepEqual(same["2:7"].inputs, { image: ["1", 0] }, "one input each: the instance's wire belongs to the definition's input 0");
  const differ = graphOf(convert(wf(def([["img", "IMAGE"], ["extra", "IMAGE"]]), [["renamed", "IMAGE"]])));
  assert.deepEqual(differ["2:7"].inputs, {}, "one input against two and no shared name: nothing is guessed");
});

test("subgraph: properties.proxyWidgets name the inner widgets the instance's widgets_values belong to, control_after_generate entries included", () => {
  const def = subdef(SG(1), {
    ins: [], outs: [],
    nodes: [
      { id: 3, type: "KSampler", outs: [["LATENT", "LATENT"]], wv: [1, "fixed", 4, 1, "euler", "normal", 1] },
      { id: 4, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["inner text"] },
    ],
  });
  const wf = withDefs(ui([{ id: 9, type: SG(1), properties: { proxyWidgets: [["4", "text"], ["3", "seed"], ["3", "steps"], ["3", "control_after_generate"]] }, wv: ["outer text", 77, 30, "randomize"] }]), def);
  const g = graphOf(convert(wf));
  assert.equal(g["9:4"].inputs.text, "outer text");
  assert.deepEqual([g["9:3"].inputs.seed, g["9:3"].inputs.steps, g["9:3"].inputs.cfg], [77, 30, 1]);
  const short = withDefs(ui([{ id: 9, type: SG(1), properties: { proxyWidgets: [["4", "text"], ["3", "seed"]] }, wv: ["only one"] }]), def);
  refusalOf(convert(short), REFUSAL.SUBGRAPH_WIDGET_MISMATCH);
  const noValues = withDefs(ui([{ id: 9, type: SG(1), properties: { proxyWidgets: [["4", "text"]] } }]), def);
  assert.equal(graphOf(convert(noValues))["9:4"].inputs.text, "inner text", "an instance with no widgets_values leaves the inner values alone");
  const stray = convert(withDefs(ui([{ id: 9, type: SG(1), properties: { proxyWidgets: [["99", "text"]] }, wv: ["x"] }]), def));
  assert.deepEqual(warned(stray, "proxy_widget_unmatched").map((w) => w.input), ["text"]);
});

test("subgraph: widgets_values that do not match the definition's widget inputs are refused", () => {
  const def = subdef(SG(1), {
    ins: [["a", "INT"]], outs: [],
    nodes: [{ id: 10, type: "PrimitiveInt", ins: [["value", "INT", true]], outs: [["INT", "INT"]], wv: [0, "fixed"] }],
    edges: [[-10, 0, 10, "value"]],
  });
  const r = refusalOf(convert(withDefs(ui([{ id: 1, type: SG(1), ins: [["a", "INT", true]], wv: [1, 2] }]), def)), REFUSAL.SUBGRAPH_WIDGET_MISMATCH);
  assert.equal(r.details.node, "1");
});

test("subgraph: a muted instance disappears with everything in it, a bypassed one passes its inputs through by type", () => {
  const def = subdef(SG(1), {
    ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]],
    nodes: [{ id: 7, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] }],
    edges: [[-10, 0, 7, "image"], [7, 0, -20, "out"]],
  });
  const build = (mode) => withDefs(ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: SG(1), mode, ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]], wv: [] },
    { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "img"], [2, 0, 3, "image"]]), def);
  const muted = convert(build(2));
  assert.deepEqual(Object.keys(graphOf(muted)).sort(), ["1", "3"]);
  assert.deepEqual(graphOf(muted)["3"].inputs, {});
  const bypassed = convert(build(4));
  assert.deepEqual(Object.keys(graphOf(bypassed)).sort(), ["1", "3"]);
  assert.deepEqual(graphOf(bypassed)["3"].inputs, { image: ["1", 0] });
});

test("subgraph: a definition that instantiates itself is refused, an id with no definition is refused as a missing definition", () => {
  const self = subdef(SG(1), { ins: [], outs: [], nodes: [{ id: 5, type: SG(1) }] });
  refusalOf(convert(withDefs(ui([{ id: 1, type: SG(1) }]), self)), REFUSAL.SUBGRAPH_CYCLE);
  const missing = refusalOf(convert(ui([{ id: 1, type: SG(9) }])), REFUSAL.SUBGRAPH_DEFINITION_MISSING);
  assert.equal(missing.details.type, SG(9));
});

test("subgraph: a bypassed or muted node inside a definition is handled like a top-level one", () => {
  const def = subdef(SG(1), {
    ins: [["m", "MODEL"], ["c", "CLIP"]], outs: [["clip", "CLIP"]],
    nodes: [{ id: 4, type: "LoraLoader", mode: 4, ins: [["model", "MODEL"], ["clip", "CLIP"]], outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"]], wv: ["style_a.safetensors", 1, 1] }],
    edges: [[-10, 0, 4, "model"], [-10, 1, 4, "clip"], [4, 1, -20, "clip"]],
  });
  const wf = withDefs(ui([
    { id: 1, type: "CheckpointLoaderSimple", outs: [["MODEL", "MODEL"], ["CLIP", "CLIP"], ["VAE", "VAE"]], wv: ["sd_a.safetensors"] },
    { id: 2, type: SG(1), ins: [["m", "MODEL"], ["c", "CLIP"]], outs: [["clip", "CLIP"]], wv: [] },
    { id: 3, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], outs: [["CONDITIONING", "CONDITIONING"]], wv: ["x"] },
  ], [[1, 0, 2, "m"], [1, 1, 2, "c"], [2, 0, 3, "clip"]]), def);
  assert.deepEqual(graphOf(convert(wf))["3"].inputs, { text: "x", clip: ["1", 1] });
});

// ---- limits and collisions -----------------------------------------------------------------------------------------------

// a chain of `hops` Reroute nodes between a LoadImage (id 1) and an ImageInvert (id hops + 2), built as plain data: link i runs from
// node i to node i + 1
function rerouteChain(hops) {
  const nodes = [{ id: 1, type: "LoadImage", mode: 0, inputs: [], outputs: [{ name: "IMAGE", type: "IMAGE", links: [1] }, { name: "MASK", type: "MASK", links: [] }], widgets_values: ["a.png", "image"] }];
  const links = [];
  for (let i = 1; i <= hops + 1; i++) links.push([i, i, 0, i + 1, 0, "IMAGE"]);
  for (let id = 2; id <= hops + 1; id++) nodes.push({ id, type: "Reroute", mode: 0, inputs: [{ name: "", type: "IMAGE", link: id - 1 }], outputs: [{ name: "", type: "IMAGE", links: [id] }] });
  nodes.push({ id: hops + 2, type: "ImageInvert", mode: 0, inputs: [{ name: "image", type: "IMAGE", link: hops + 1 }], outputs: [{ name: "IMAGE", type: "IMAGE", links: [] }] });
  return { version: 0.4, nodes, links };
}

test("Reroute chains of any length resolve without a stack overflow and in time that does not grow with the square of the length", () => {
  const hops = 99998; // the longest chain the bound admits: every Reroute counts, so this and its two ends are the 100,000 nodes
  const t0 = Date.now();
  const r = convert(rerouteChain(hops));
  const took = Date.now() - t0;
  const g = graphOf(r);
  assert.deepEqual(g[String(hops + 2)].inputs, { image: ["1", 0] }, "100k reroutes are the wire they carry");
  assert.deepEqual(Object.keys(g).sort(), ["1", String(hops + 2)].sort());
  assert.equal(r.stats.reroutes, hops);
  assert.ok(took < 15000, `a 100k-hop chain took ${took} ms`); // loose on purpose: a copy of the visited set per hop is minutes, a walk is well under a second
  assert.equal(refusalOf(convert(rerouteChain(hops + 1)), REFUSAL.GRAPH_TOO_LARGE).details.node, "100001", "one Reroute more is past the bound, which counts it like any node");
  // closed into a loop, a long chain is still a link_cycle and not a crash
  const loop = rerouteChain(hops);
  loop.links.push([hops + 2, hops + 1, 0, 2, 0, "IMAGE"]);
  loop.nodes[1].inputs[0].link = hops + 2;
  refusalOf(convert(loop), REFUSAL.LINK_CYCLE);
});

test("a long chain of bypassed nodes resolves to the wire it carries too", () => {
  const hops = 20000;
  const wf = rerouteChain(hops);
  for (const n of wf.nodes) if (n.type === "Reroute") { n.type = "ImageInvert"; n.mode = 4; n.inputs[0].name = "image"; n.outputs[0].name = "IMAGE"; }
  assert.deepEqual(graphOf(convert(wf))[String(hops + 2)].inputs, { image: ["1", 0] }, "bypassed ImageInvert nodes pass their input through");
});

test("expansion is bounded: a nested subgraph bomb is refused as graph_too_large, whether it ends in nodes or in nothing, and a template just under the bound converts", () => {
  assert.equal(REFUSAL.GRAPH_TOO_LARGE, "graph_too_large");
  const BG = (n) => `5c1b0000-0000-4000-8000-${String(n).padStart(12, "0")}`;
  // level k holds two instances of level k - 1; level 0 holds `leaf` (one real node, or nothing)
  const bomb = (levels, leaf) => {
    const defs = [subdef(BG(0), { ins: [], outs: [], nodes: leaf ? [{ id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] }] : [] })];
    for (let k = 1; k <= levels; k++) defs.push(subdef(BG(k), { ins: [], outs: [], nodes: [{ id: 1, type: BG(k - 1) }, { id: 2, type: BG(k - 1) }] }));
    return withDefs(ui([{ id: 1, type: BG(levels) }]), ...defs);
  };
  const t0 = Date.now();
  const nodes = refusalOf(convert(bomb(18, true)), "graph_too_large"); // 2^18 = 262,144 real nodes
  assert.equal(nodes.details.limit, 100000);
  assert.equal(typeof nodes.details.node, "string", "the details name where the limit was passed");
  refusalOf(convert(bomb(18, false)), "graph_too_large"); // 2^18 instances and not one real node: instances count too
  assert.ok(Date.now() - t0 < 15000, "refused at the limit, not after expanding it");
  const ok = convert(bomb(15, true)); // 2^15 = 32,768 real nodes in 2^16 - 1 = 65,535 instances: 98,303 against the bound
  assert.equal(graphOf(ok) !== undefined, true);
  assert.equal(ok.stats.nodes, 32768);
  // the bound itself: 100,000 real nodes convert, 100,001 do not
  const flat = (n) => ({ version: 0.4, links: [], nodes: Array.from({ length: n }, (_v, i) => ({ id: i + 1, type: "ImageInvert", mode: 0, inputs: [], outputs: [] })) });
  assert.equal(convert(flat(100000)).stats.nodes, 100000);
  assert.equal(refusalOf(convert(flat(100001)), "graph_too_large").details.node, "100001");
});

// What a node costs the bound is not its kind: a definition stuffed with Notes, muted, bypassed, Reroute and primitive nodes expands
// to as many nodes as a definition of live ones, and a few kilobytes of template stand for millions of them. FILLERS are the specs
// of those nodes; stuffedBomb(levels, fill, kinds) is a chain of definitions in which level k holds two instances of level k - 1
// plus `fill` nodes whose kinds rotate through `kinds` (level 0 holds only the fill): 2^(levels + 1) - 1 instances, each with its fill.
const FILLERS = {
  note: { type: "Note", wv: ["x"] },
  markdown: { type: "MarkdownNote", wv: ["x"] },
  muted: { type: "ImageInvert", mode: 2, ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  bypassed: { type: "ImageInvert", mode: 4, ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  reroute: { type: "Reroute", ins: [["", "IMAGE"]], outs: [["", "IMAGE"]] },
  primitive: { type: "PrimitiveNode", outs: [["INT", "INT"]], wv: [1, "fixed"] },
};
function stuffedBomb(levels, fill, kinds) {
  const id = (n) => `5c1b0000-0000-4000-8000-${String(n).padStart(12, "0")}`;
  const filler = (first) => Array.from({ length: fill }, (_v, i) => ({ id: first + i, ...FILLERS[kinds[i % kinds.length]] }));
  const defs = [subdef(id(0), { ins: [], outs: [], nodes: filler(1) })];
  for (let k = 1; k <= levels; k++) defs.push(subdef(id(k), { ins: [], outs: [], nodes: [{ id: 1, type: id(k - 1) }, { id: 2, type: id(k - 1) }, ...filler(3)] }));
  return withDefs(ui([{ id: 1, type: id(levels) }]), ...defs);
}

test("expansion is bounded for every kind of node: a bomb of Notes (a few kilobytes standing for 300,000 of them) is refused as graph_too_large, not carried on until memory runs out", () => {
  // 1,023 instances of a definition with 300 Notes each: no live node anywhere and fewer instances than the bound, 306,900 Notes
  const t0 = Date.now();
  const r = refusalOf(convert(stuffedBomb(9, 300, ["note"])), REFUSAL.GRAPH_TOO_LARGE);
  const took = Date.now() - t0;
  assert.equal(r.details.limit, 100000);
  assert.equal(r.details.what, "nodes");
  assert.equal(typeof r.details.node, "string", "the details name where the limit was passed");
  assert.ok(took < 5000, `refused at the bound, not after expanding it: ${took} ms`);
  // the same shape just under the bound converts, and the Notes are what it is made of: 255 instances of 300 Notes is 76,755 nodes
  const under = convert(stuffedBomb(7, 300, ["note"]));
  assert.deepEqual(graphOf(under), {});
  assert.equal(under.stats.notes, 76500);
  assert.equal(under.stats.expandedSubgraphs, 255);
});

test("expansion is bounded for every kind of node: Notes, MarkdownNotes, muted, bypassed, Reroute and primitive nodes each count, alone and mixed", () => {
  // 511 instances with 300 of one kind each: 153,300 of them and 511 instances against the bound
  for (const kind of [...Object.keys(FILLERS), "mixed"]) {
    const kinds = kind === "mixed" ? Object.keys(FILLERS) : [kind];
    const t0 = Date.now();
    const r = refusalOf(convert(stuffedBomb(8, 300, kinds)), REFUSAL.GRAPH_TOO_LARGE);
    assert.equal(r.details.limit, 100000, kind);
    assert.ok(Date.now() - t0 < 5000, `${kind}: refused at the bound, not after expanding it`);
  }
});

test("expansion is bounded: exactly 100,000 nodes of any mix of kinds convert, and one more is refused whatever its kind", () => {
  const plain = { mode: 0, inputs: [], outputs: [] };
  const KINDS = { note: { type: "Note" }, markdown: { type: "MarkdownNote" }, muted: { type: "ImageInvert", mode: 2 }, bypassed: { type: "ImageInvert", mode: 4 },
    reroute: { type: "Reroute" }, primitive: { type: "PrimitiveNode" }, live: { type: "ImageInvert" } };
  const names = Object.keys(KINDS);
  const mix = (n, extra) => ({
    version: 0.4, links: [],
    nodes: [...Array.from({ length: n }, (_v, i) => ({ id: i + 1, ...plain, ...KINDS[names[i % names.length]] })), ...(extra ? [{ id: n + 1, ...plain, ...KINDS[extra] }] : [])],
  });
  const ok = convert(mix(100000));
  assert.equal(ok.ok, true, JSON.stringify(ok.refusal));
  const s = ok.stats;
  assert.equal(s.notes + s.mutedNodes + s.bypassedNodes + s.reroutes + s.primitiveNodes + s.nodes, 100000, "every kind was met and none was left out");
  assert.ok(s.nodes < 100000 / 2, "most of them are not live nodes");
  for (const kind of names) assert.equal(refusalOf(convert(mix(100000, kind)), REFUSAL.GRAPH_TOO_LARGE).details.node, "100001", `a ${kind} node as the 100,001st`);
});

test("expansion is bounded: subgraphs nested past 64 levels are refused as graph_too_large, tens of thousands deep included, not an internal_error from the stack", () => {
  const BG = (n) => `5c1b0000-0000-4000-8000-${String(n).padStart(12, "0")}`;
  const depth = (levels) => {
    const defs = [subdef(BG(0), { ins: [], outs: [], nodes: [{ id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] }] })];
    for (let k = 1; k <= levels; k++) defs.push(subdef(BG(k), { ins: [], outs: [], nodes: [{ id: 1, type: BG(k - 1) }] }));
    return withDefs(ui([{ id: 1, type: BG(levels) }]), ...defs);
  };
  const deep = refusalOf(convert(depth(20000)), "graph_too_large"); // the converter overflowed its stack somewhere past 5,000
  assert.equal(deep.details.limit, 64);
  assert.equal(deep.details.what, "subgraph depth");
  // depth(L) nests L + 1 instances: 64 are allowed, a 65th is not
  assert.equal(Object.hasOwn(graphOf(convert(depth(63))), Array(65).fill(1).join(":")), true, "64 levels of nesting convert; the leaf's id is the path through all of them");
  refusalOf(convert(depth(64)), "graph_too_large");
});

test("ids that collide are refused as bad_template, never resolved to whichever came last", () => {
  const def = subdef(SG(1), { ins: [], outs: [], nodes: [{ id: 5, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] }] });
  // a root node whose string id is the path of an inner node: both would be the API node "2:5"
  const live = ui([
    { id: 2, type: SG(1) },
    { id: "2:5", type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
  ]);
  const clash = refusalOf(convert(withDefs(live, def)), REFUSAL.BAD_TEMPLATE);
  assert.equal(clash.details.id, "2:5");
  // the same clash with a node that is never emitted still breaks the trail keys of the wires through it
  const note = ui([{ id: 2, type: SG(1) }, { id: "2:5", type: "Note", wv: ["x"] }]);
  assert.equal(refusalOf(convert(withDefs(note, def)), REFUSAL.BAD_TEMPLATE).details.id, "2:5");
  // a clash between two instances' inner ids, through a string id that reads like a path
  const twice = ui([{ id: "a", type: SG(1) }, { id: "a:5", type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["c.png", "image"] }]);
  refusalOf(convert(withDefs(twice, def)), REFUSAL.BAD_TEMPLATE);
  // no clash, no refusal: the ids differ
  assert.deepEqual(Object.keys(graphOf(convert(withDefs(ui([{ id: 2, type: SG(1) }, { id: "2-5", type: "Note", wv: ["x"] }]), def)))), ["2:5"]);
  // two subgraph definitions with one id
  const dup = refusalOf(convert(withDefs(ui([{ id: 2, type: SG(1) }]), def, subdef(SG(1), { ins: [], outs: [], nodes: [] }))), REFUSAL.BAD_TEMPLATE);
  assert.equal(dup.details.subgraph, SG(1));
});

// ---- refusals ------------------------------------------------------------------------------------------------------------

test("an unknown node class is refused with every class named and where it sits, never guessed; muted and bypassed ones do not count", () => {
  const def = subdef(SG(1), { ins: [], outs: [], nodes: [{ id: 5, type: "MysteryB", outs: [["x", "IMAGE"]] }] });
  const wf = withDefs(ui([
    { id: 1, type: "MysteryA" },
    { id: 2, type: SG(1) },
    { id: 3, type: "MysteryC", mode: 2 },
    { id: 4, type: "MysteryD", mode: 4 },
    { id: 6, type: "MysteryA" },
  ]), def);
  const r = refusalOf(convert(wf), REFUSAL.UNKNOWN_NODE_CLASS);
  assert.deepEqual(r.details.classes, [
    { class_type: "MysteryA", nodes: ["1", "6"] },
    { class_type: "MysteryB", nodes: ["2:5"] },
  ]);
  assert.deepEqual(graphOf(convert(ui([{ id: 3, type: "MysteryC", mode: 2 }, { id: 4, type: "MysteryD", mode: 4 }]))), {}, "only muted and bypassed unknowns: nothing to build, nothing to refuse");
});

test("a group node and a malformed template are refused with their own typed reasons", () => {
  const g = refusalOf(convert(ui([{ id: 1, type: "workflow>My Group" }])), REFUSAL.GROUP_NODE_UNSUPPORTED);
  assert.equal(g.details.type, "workflow>My Group");
  assert.equal(refusalOf(convert(ui([{ id: 1, type: "workflow/My Group" }])), REFUSAL.GROUP_NODE_UNSUPPORTED).details.type, "workflow/My Group", "the other group prefix too");
  for (const bad of [null, undefined, 5, "x", [], {}, { nodes: "no" }, { nodes: [null] }, { nodes: [{ type: "KSampler" }] }, { nodes: [{ id: 1 }] }]) {
    refusalOf(convert(bad), REFUSAL.BAD_TEMPLATE);
  }
  refusalOf(convert({ nodes: [{ id: 1, type: "Note" }, { id: 1, type: "Note" }] }), REFUSAL.BAD_TEMPLATE);
  refusalOf(convert({ nodes: [], links: [[1, 2]] }), REFUSAL.BAD_TEMPLATE);
});

test("a node whose inputs or outputs is not a list is a bad_template refusal wherever the converter would read it, never an internal_error", () => {
  const nodes = (patch) => {
    const wf = ui([
      { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
      { id: 2, type: "ImageInvert", mode: 4, ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
      { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
    ], [[1, 0, 2, "image"], [2, 0, 3, "image"]]);
    Object.assign(wf.nodes[1], patch);
    return wf;
  };
  const def = () => subdef(SG(1), {
    ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]],
    nodes: [{ id: 7, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] }],
    edges: [[-10, 0, 7, "image"], [7, 0, -20, "out"]],
  });
  const instance = (patch) => {
    const wf = withDefs(ui([
      { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
      { id: 2, type: SG(1), ins: [["img", "IMAGE"]], outs: [["out", "IMAGE"]], wv: [] },
      { id: 3, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
    ], [[1, 0, 2, "img"], [2, 0, 3, "image"]]), def());
    Object.assign(wf.nodes[1], patch);
    return wf;
  };
  for (const bad of [{}, "x", 5, true, { 0: { name: "image", link: 1 } }]) {
    for (const field of ["inputs", "outputs"]) {
      const msg = `${field} = ${JSON.stringify(bad)}`;
      assert.equal(refusalOf(convert(nodes({ [field]: bad })), REFUSAL.BAD_TEMPLATE).details.node, "2", `a bypassed node, ${msg}`);
      assert.equal(refusalOf(convert(instance({ [field]: bad })), REFUSAL.BAD_TEMPLATE).details.node, "2", `a subgraph instance, ${msg}`);
    }
  }
  // a node inside a definition is checked as it is entered
  const inner = withDefs(ui([{ id: 2, type: SG(1), wv: [] }]), def());
  inner.definitions.subgraphs[0].nodes[0].inputs = { image: 1 };
  const innerRefusal = refusalOf(convert(inner), REFUSAL.BAD_TEMPLATE);
  assert.equal(innerRefusal.details.node, "2:7");
  // the definition's own slot lists
  for (const field of ["inputs", "outputs"]) {
    const broken = withDefs(ui([{ id: 2, type: SG(1), wv: [] }]), def());
    broken.definitions.subgraphs[0][field] = { 0: { name: "x" } };
    assert.equal(refusalOf(convert(broken), REFUSAL.BAD_TEMPLATE).details.subgraph, SG(1), `definition ${field}`);
  }
  // absent and null are the same as empty: nothing wired through them, no refusal
  for (const patch of [{ inputs: null }, { outputs: null }]) assert.equal(convert(nodes(patch)).ok, true, JSON.stringify(patch));
  const absent = nodes({});
  delete absent.nodes[1].inputs;
  assert.equal(convert(absent).ok, true, "no inputs key");
});

test("link integrity: a link id the template does not list, a link that targets another node, an origin that is not there", () => {
  const base = () => ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "image"]]);
  assert.equal(graphOf(convert(base()))["2"].inputs.image[0], "1");
  const dangling = base();
  dangling.nodes[1].inputs[0].link = 99;
  assert.equal(refusalOf(convert(dangling), REFUSAL.DANGLING_LINK).details.link, 99);
  const wrongTarget = base();
  wrongTarget.links[0][3] = 7;
  refusalOf(convert(wrongTarget), REFUSAL.LINK_MISMATCH);
  const noOrigin = base();
  noOrigin.links[0][1] = 42;
  assert.equal(refusalOf(convert(noOrigin), REFUSAL.DANGLING_LINK).details.origin, 42);
});

test("ids may be strings: node ids, link ids and the link fields of an object-shaped link", () => {
  const numeric = ui([
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [[1, 0, 2, "image"]]);
  const expected = { 1: { class_type: "LoadImage", inputs: { image: "a.png" } }, 2: { class_type: "ImageInvert", inputs: { image: ["1", 0] } } };
  assert.deepEqual(compareApiGraphs(graphOf(convert(numeric)), expected), []);
  const wf = ui([
    { id: "n1", type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: "n2", type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ], [["n1", 0, "n2", "image"]]);
  // the link id as a string too, in the link list and on both ends
  wf.links = wf.links.map(([id, ...rest]) => [`L${id}`, ...rest]);
  for (const n of wf.nodes) {
    for (const i of n.inputs ?? []) if (i.link !== null) i.link = `L${i.link}`;
    for (const o of n.outputs ?? []) o.links = o.links.map((l) => `L${l}`);
  }
  const g = graphOf(convert(wf));
  assert.deepEqual(compareApiGraphs(g, { n1: expected["1"], n2: { class_type: "ImageInvert", inputs: { image: ["n1", 0] } } }), []);
  const objectLinks = clone(wf);
  objectLinks.links = objectLinks.links.map(([id, from, fslot, to, tslot]) => ({ id, origin_id: from, origin_slot: fslot, target_id: to, target_slot: tslot }));
  assert.deepEqual(graphOf(convert(objectLinks)).n2.inputs, { image: ["n1", 0] });
  const empty = clone(wf);
  empty.links[0][0] = "";
  refusalOf(convert(empty), REFUSAL.BAD_TEMPLATE);
});

// ---- seeds ---------------------------------------------------------------------------------------------------------------

test("seed pinning: only seed-named inputs that the listing's control makes a seed are pinned, a generic integer with the same control is not", () => {
  const r = convert(readJson("basic-t2i.workflow.json"), { seed: 7 });
  const g = graphOf(r);
  assert.equal(g["3"].inputs.seed, 7);
  assert.deepEqual(r.pinnedSeeds, [{ node: "3", input: "seed", from: 156680208700286, to: 7 }]);
  const sub = convert(readJson("subgraph-i2v.workflow.json"), { seed: 7 });
  const sg = graphOf(sub);
  assert.equal(sg["2:22"].inputs.noise_seed, 7, "a seed that came through a subgraph input is a literal on the inner node, and is pinned there");
  assert.equal(sg["2:23"].inputs.value, 360, "PrimitiveInt carries the same control flag and is not a seed");
  assert.equal(sg["3"].inputs.value, 1024);
  assert.deepEqual(sub.pinnedSeeds, [{ node: "2:22", input: "noise_seed", from: 4242, to: 7 }]);
});

test("seed pinning: a seed that is wired is left alone, an unflagged seed is pinned only when a control word marked it, 0 is a valid seed", () => {
  const wired = convert(ui([
    { id: 1, type: "PrimitiveInt", outs: [["INT", "INT"]], wv: [3, "fixed"] },
    { id: 2, type: "RandomNoise", ins: [["noise_seed", "INT", true]], outs: [["NOISE", "NOISE"]], wv: [5, "fixed"] },
  ], [[1, 0, 2, "noise_seed"]]), { seed: 9 });
  assert.deepEqual(graphOf(wired)["2"].inputs, { noise_seed: ["1", 0] });
  assert.deepEqual(wired.pinnedSeeds, []);
  const old = convert(ui([
    { id: 1, type: "OldSampler", outs: [["LATENT", "LATENT"]], wv: [7, "randomize", 30] },
    { id: 2, type: "SeedPlain", outs: [["LATENT", "LATENT"]], wv: [42, 0.5] },
    { id: 3, type: "DynSampler", ins: [["clip", "CLIP"]], outs: [["generated_text", "STRING"]], wv: ["hi", 600, "on", 0.7, 64, 5, 0.1, false] },
  ]), { seed: 0 });
  const g = graphOf(old);
  assert.equal(g["1"].inputs.seed, 0, "a control word marked it a seed");
  assert.equal(g["2"].inputs.seed, 42, "no flag and no control word: left alone");
  assert.equal(g["3"].inputs["mode.seed"], 5, "a sampling seed of a dynamic combo has no control: left alone");
  assert.deepEqual(old.pinnedSeeds.map((p) => p.node), ["1"]);
});

test("seed pinning: no seed option leaves every value as stored, a bad seed is refused", () => {
  const stored = graphOf(convert(readJson("basic-t2i.workflow.json")));
  assert.equal(stored["3"].inputs.seed, 156680208700286);
  for (const bad of [-1, 1.5, "7", Number.NaN, 2 ** 60, true]) {
    refusalOf(convert(readJson("basic-t2i.workflow.json"), { seed: bad }), REFUSAL.BAD_SEED);
  }
  assert.equal(convert(readJson("basic-t2i.workflow.json"), { seed: null }).ok, true, "null is no seed option");
});

test("seed pinning: a seed-named input the option left alone is listed in a seed_not_pinned warning with the reason, none is said otherwise", () => {
  const wired = convert(ui([
    { id: 1, type: "PrimitiveInt", outs: [["INT", "INT"]], wv: [3, "fixed"] },
    { id: 2, type: "RandomNoise", ins: [["noise_seed", "INT", true]], outs: [["NOISE", "NOISE"]], wv: [5, "fixed"] },
  ], [[1, 0, 2, "noise_seed"]]), { seed: 9 });
  assert.deepEqual(warned(wired, "seed_not_pinned"), [{ code: "seed_not_pinned", node: "2", input: "noise_seed", reason: "wired" }]);
  const left = convert(ui([
    { id: 1, type: "OldSampler", outs: [["LATENT", "LATENT"]], wv: [7, "randomize", 30] },
    { id: 2, type: "SeedPlain", outs: [["LATENT", "LATENT"]], wv: [42, 0.5] },
    { id: 3, type: "DynSampler", ins: [["clip", "CLIP"]], outs: [["generated_text", "STRING"]], wv: ["hi", 600, "on", 0.7, 64, 5, 0.1, false] },
  ]), { seed: 0 });
  assert.deepEqual(warned(left, "seed_not_pinned").map((w) => `${w.node}.${w.input}:${w.reason}`), ["2.seed:no_control", "3.mode.seed:no_control"], "the pinned seed of node 1 is not in it");
  assert.deepEqual(left.pinnedSeeds.map((p) => p.node), ["1"]);
  assert.deepEqual(warned(convert(readJson("basic-t2i.workflow.json"), { seed: 7 }), "seed_not_pinned"), [], "every seed was pinned");
  assert.deepEqual(warned(convert(ui([{ id: 2, type: "SeedPlain", outs: [["LATENT", "LATENT"]], wv: [42, 0.5] }])), "seed_not_pinned"), [], "no seed option, nothing to say");
});

// ---- versions ------------------------------------------------------------------------------------------------------------

test("versions: parse and compare dotted numbers of any length, ignore a v prefix and a pre-release suffix, refuse what is not a version", () => {
  assert.deepEqual(parseVersion("0.37.2"), [0, 37, 2]);
  assert.deepEqual(parseVersion("v1.2.3-rc1"), [1, 2, 3]);
  assert.equal(parseVersion("1.x"), null);
  assert.equal(parseVersion(""), null);
  assert.equal(parseVersion(5), null);
  assert.equal(compareVersions("0.3.72", "0.3.7"), 1);
  assert.equal(compareVersions("0.37.0", "0.37"), 0);
  assert.equal(compareVersions("0.8.0", "0.11.0"), -1, "numeric, not text");
  assert.equal(compareVersions("v1.2.3-rc1", "1.2.3"), 0);
  assert.equal(compareVersions("1.2.3.4", "1.2.3"), 1);
  assert.throws(() => compareVersions("nope", "1.0.0"), RangeError);
});

test("version refusal: a template whose minimum exceeds the given ComfyUI is refused with both versions; equal and newer pass", () => {
  const wf = readJson("basic-t2i.workflow.json");
  const r = refusalOf(convert(wf, { minComfyUIVersion: "0.37.0", comfyVersion: "0.36.9" }), REFUSAL.COMFY_TOO_OLD);
  assert.deepEqual([r.details.required, r.details.have], ["0.37.0", "0.36.9"]);
  assert.equal(convert(wf, { minComfyUIVersion: "0.37.0", comfyVersion: "0.37.0" }).ok, true);
  assert.equal(convert(wf, { minComfyUIVersion: "0.37.0", comfyVersion: "0.40.1" }).ok, true);
  assert.equal(convert(wf, { minComfyUIVersion: "0.8.0", comfyVersion: "0.11.0" }).ok, true, "0.11.0 is newer than 0.8.0");
});

test("version refusal: a known minimum with no ComfyUI version given is a refusal, not a pass; no minimum needs no version; a malformed version is refused", () => {
  const wf = readJson("basic-t2i.workflow.json");
  refusalOf(convert(wf, { minComfyUIVersion: "0.37.0" }), REFUSAL.COMFY_VERSION_UNKNOWN);
  assert.equal(convert(wf, {}).ok, true);
  assert.equal(convert(wf, { comfyVersion: "0.1.0" }).ok, true);
  refusalOf(convert(wf, { minComfyUIVersion: "soon", comfyVersion: "0.37.0" }), REFUSAL.BAD_VERSION);
  refusalOf(convert(wf, { minComfyUIVersion: "0.37.0", comfyVersion: "latest" }), REFUSAL.BAD_VERSION);
  assert.equal(convert(wf, { minComfyUIVersion: null, comfyVersion: null }).ok, true);
});

test("version refusal comes first: a template that is both too new and unknown-class reports the version", () => {
  refusalOf(convert(ui([{ id: 1, type: "Mystery" }]), { minComfyUIVersion: "9.0.0", comfyVersion: "0.1.0" }), REFUSAL.COMFY_TOO_OLD);
});

test("version refusal comes before everything else the call carries: a bad seed, an empty listing or a file that is no workflow never hides a too-old ComfyUI", () => {
  const wf = readJson("basic-t2i.workflow.json");
  const old = { minComfyUIVersion: "0.37.0", comfyVersion: "0.1.0" };
  refusalOf(convert(wf, { ...old, seed: -1 }), REFUSAL.COMFY_TOO_OLD);
  refusalOf(convert(wf, old, {}), REFUSAL.COMFY_TOO_OLD);
  refusalOf(convert({}, old), REFUSAL.COMFY_TOO_OLD);
  refusalOf(convert(wf, { minComfyUIVersion: "0.37.0", seed: -1 }, {}), REFUSAL.COMFY_VERSION_UNKNOWN);
  refusalOf(convert(wf, { minComfyUIVersion: "soon", comfyVersion: "0.1.0", seed: -1 }), REFUSAL.BAD_VERSION);
  // with the version fine the other refusals are reached
  const fine = { minComfyUIVersion: "0.1.0", comfyVersion: "0.37.0" };
  refusalOf(convert(wf, { ...fine, seed: -1 }), REFUSAL.BAD_SEED);
  refusalOf(convert(wf, fine, {}), REFUSAL.BAD_OBJECT_INFO);
  refusalOf(convert({}, fine), REFUSAL.BAD_TEMPLATE);
});

test("version: a ComfyUI version given with no minimum to check it against is said so in a version_check_skipped warning, never silently accepted", () => {
  const wf = readJson("basic-t2i.workflow.json");
  const skipped = convert(wf, { comfyVersion: "0.1.0" });
  assert.equal(skipped.ok, true);
  const w = warned(skipped, "version_check_skipped");
  assert.equal(w.length, 1);
  assert.equal(w[0].comfyVersion, "0.1.0");
  assert.equal(typeof w[0].reason, "string");
  assert.deepEqual(warned(convert(wf, { minComfyUIVersion: "0.3.0", comfyVersion: "0.3.0" }), "version_check_skipped"), [], "a check that ran is not a skipped one");
  assert.deepEqual(warned(convert(wf, {}), "version_check_skipped"), [], "no version given and none needed: nothing was skipped");
  assert.deepEqual(warned(convert(wf, { minComfyUIVersion: null, comfyVersion: null }), "version_check_skipped"), []);
});

// ---- result shape --------------------------------------------------------------------------------------------------------

test("the template and the listing are read, never changed, and the graph shares nothing with them", () => {
  const deepFreeze = (v) => { if (v && typeof v === "object") { Object.values(v).forEach(deepFreeze); Object.freeze(v); } return v; };
  const wf = deepFreeze(readJson("subgraph-i2v.workflow.json"));
  const oi = deepFreeze(clone(OI));
  const r = convertTemplate(wf, oi, { seed: 3 });
  assert.equal(r.ok, true);
  const multi = convertTemplate(deepFreeze(ui([{ id: 1, type: "CLIPTextEncode", ins: [["clip", "CLIP"]], wv: [["a", "b"]] }])), oi);
  assert.equal(multi.ok, true);
  assert.ok(Object.isFrozen(wf));
  assert.equal(Object.isFrozen(multi.graph["1"].inputs.text), false, "an array value is copied, not shared with the frozen template");
});

test("meta option: _meta.title is the node's own title, else the class's display name, and only when asked", () => {
  const wf = ui([
    { id: 1, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
    { id: 2, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ]);
  wf.nodes[1].title = "My Invert";
  assert.equal(graphOf(convert(wf))["1"]._meta, undefined);
  const g = graphOf(convert(wf, { meta: true }));
  assert.equal(g["1"]._meta.title, "Invert Image");
  assert.equal(g["2"]._meta.title, "My Invert");
});

// ---- the CLI -------------------------------------------------------------------------------------------------------------

function cli(...args) {
  const r = spawnSync(process.execPath, [CLI, ...args], { encoding: "utf8", timeout: 30000, windowsHide: true });
  let json = null;
  try { json = JSON.parse(r.stdout); } catch { /* the assertion on json names it */ }
  return { status: r.status, json, stdout: r.stdout, stderr: r.stderr };
}
const T = (name) => join(DATA, name);

test("CLI: prints the API graph as JSON and exits 0", () => {
  const r = cli("--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"));
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.json.ok, true);
  assert.deepEqual(compareApiGraphs(r.json.graph, readJson("basic-t2i.api.json")), []);
});

test("CLI: --seed pins, and a refusal is JSON with exit 1", () => {
  const seeded = cli("--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"), "--seed", "5");
  assert.equal(seeded.status, 0, seeded.stderr);
  assert.equal(seeded.json.graph["3"].inputs.seed, 5);
  const old = cli("--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"), "--comfy-version", "0.2.0", "--min-comfyui-version", "0.3.0");
  assert.equal(old.status, 1);
  assert.equal(old.json.ok, false);
  assert.equal(old.json.refusal.code, REFUSAL.COMFY_TOO_OLD);
  assert.deepEqual([old.json.refusal.details.required, old.json.refusal.details.have], ["0.3.0", "0.2.0"]);
});

test("CLI: --index reads the template's minimum from the packaged index by file name", () => {
  const args = ["--object-info", T("object-info.synthetic.json"), "--index", T("index.synthetic.json")];
  const tooOld = cli("--template", T("subgraph-i2v.workflow.json"), ...args, "--comfy-version", "0.50.0");
  assert.equal(tooOld.status, 1);
  assert.equal(tooOld.json.refusal.code, REFUSAL.COMFY_TOO_OLD);
  assert.equal(tooOld.json.refusal.details.required, "0.99.0");
  const unknown = cli("--template", T("basic-t2i.workflow.json"), ...args);
  assert.equal(unknown.status, 1);
  assert.equal(unknown.json.refusal.code, REFUSAL.COMFY_VERSION_UNKNOWN, "the index names a minimum and no ComfyUI version was given");
  const fine = cli("--template", T("basic-t2i.workflow.json"), ...args, "--comfy-version", "0.3.0");
  assert.equal(fine.status, 0, fine.stderr);
  const unlisted = cli("--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"), "--index", T("index.synthetic.json"), "--name", "no-minimum");
  assert.equal(unlisted.status, 0, "a template the index lists with no minimum needs no ComfyUI version");
});

test("CLI: --comfy-version with no minimum source prints a version_check_skipped warning and still converts", () => {
  const base = ["--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"), "--comfy-version", "0.1.0"];
  const skipped = (r) => (r.json?.warnings ?? []).filter((w) => w.code === "version_check_skipped").map((w) => w.comfyVersion);
  const alone = cli(...base);
  assert.equal(alone.status, 0, alone.stderr);
  assert.deepEqual(skipped(alone), ["0.1.0"], "the JSON carries the warning");
  const unlisted = cli(...base, "--index", T("index.synthetic.json"), "--name", "no-minimum");
  assert.equal(unlisted.status, 0, unlisted.stderr);
  assert.deepEqual(skipped(unlisted), ["0.1.0"], "an index that lists the template with no minimum is no minimum source either");
  const checked = cli(...base, "--min-comfyui-version", "0.0.1");
  assert.equal(checked.status, 0, checked.stderr);
  assert.deepEqual(skipped(checked), []);
});

test("CLI: a call that cannot be read is exit 2 with a typed reason, never a stack trace", () => {
  for (const args of [[], ["--template", T("basic-t2i.workflow.json")], ["--object-info", T("object-info.synthetic.json")],
    ["--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"), "--seed", "abc"],
    ["--template", T("basic-t2i.workflow.json"), "--object-info", T("object-info.synthetic.json"), "--bogus"]]) {
    const r = cli(...args);
    assert.equal(r.status, 2, `${args.join(" ")}: ${r.stderr}`);
    assert.equal(r.json.ok, false);
    assert.equal(r.json.refusal.code, "usage");
  }
  const missing = cli("--template", T("no-such-file.json"), "--object-info", T("object-info.synthetic.json"));
  assert.equal(missing.status, 2);
  assert.equal(missing.json.refusal.code, "unreadable_input");
  const notJson = cli("--template", T("README.md"), "--object-info", T("object-info.synthetic.json"));
  assert.equal(notJson.status, 2);
  assert.equal(notJson.json.refusal.code, "unreadable_input");
});

// ---- the module's own invariants -----------------------------------------------------------------------------------------

test("invariant: the converter imports only node: builtins, opens no network, starts no process and writes no file", () => {
  const src = readFileSync(CLI, "utf8").replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[\s;{}(])\/\/.*$/gm, "$1");
  for (const m of src.matchAll(/\bimport\s[^;]*?from\s+["']([^"']+)["']/g)) assert.match(m[1], /^node:/, `import of ${m[1]}`);
  assert.doesNotMatch(src, /\bfetch\s*\(|child_process|\bspawn|\bexec\w*\s*\(|createServer|XMLHttpRequest|WebSocket/);
  assert.doesNotMatch(src, /writeFile|appendFile|createWriteStream|mkdirSync|rmSync|unlinkSync|renameSync/);
  assert.doesNotMatch(src, /process\.exit\s*\(/, "an exit code is set, never forced: a pending handle must drain (the Windows libuv abort noted in comfy-nodes.mjs)");
});

test("invariant: the converter does not touch the lifecycle module", () => {
  const src = readFileSync(CLI, "utf8");
  assert.doesNotMatch(src, /comfy-lifecycle/);
});

// ---- the frontend-golden comparison (private data, skipped without it) ---------------------------------------------------

const GOLDEN_DIR = process.env.TEMPLATE_GOLDEN_DIR;
test("API-format goldens: every <name>.workflow.json in TEMPLATE_GOLDEN_DIR converts to the <name>.api.json beside it, value for value (that the frontend wrote those files is unverified)",
  { skip: !GOLDEN_DIR && "TEMPLATE_GOLDEN_DIR is not set (the folder holds object_info.json and <name>.workflow.json / <name>.api.json pairs)" }, () => {
    const oi = JSON.parse(readFileSync(join(GOLDEN_DIR, "object_info.json"), "utf8"));
    const names = readdirSync(GOLDEN_DIR).filter((f) => f.endsWith(".workflow.json")).map((f) => f.slice(0, -".workflow.json".length)).sort();
    assert.ok(names.length > 0, "no pairs in TEMPLATE_GOLDEN_DIR");
    for (const name of names) {
      const api = join(GOLDEN_DIR, `${name}.api.json`);
      assert.ok(existsSync(api), `${name}: no .api.json beside the workflow`);
      const optsFile = join(GOLDEN_DIR, `${name}.opts.json`);
      const opts = existsSync(optsFile) ? JSON.parse(readFileSync(optsFile, "utf8")) : {};
      const r = convertTemplate(JSON.parse(readFileSync(join(GOLDEN_DIR, `${name}.workflow.json`), "utf8")), oi, opts);
      assert.equal(r.ok, true, `${name}: refused ${JSON.stringify(r.refusal)}`);
      assert.deepEqual(compareApiGraphs(r.graph, JSON.parse(readFileSync(api, "utf8"))), [], `${name}: differs from the API-format export beside it`);
    }
  });

const SURVEY_DIR = process.env.TEMPLATE_SURVEY_DIR;
const SURVEY_OI = process.env.TEMPLATE_SURVEY_OBJECT_INFO;
test("survey: over a templates folder the converter never throws and every refusal carries a known code",
  { skip: !(SURVEY_DIR && SURVEY_OI) && "TEMPLATE_SURVEY_DIR and TEMPLATE_SURVEY_OBJECT_INFO are not set" }, () => {
    const oi = JSON.parse(readFileSync(SURVEY_OI, "utf8"));
    const known = new Set(Object.values(REFUSAL));
    const counts = {};
    for (const f of readdirSync(SURVEY_DIR).filter((x) => x.endsWith(".json") && x !== "index.json")) {
      const r = convertTemplate(JSON.parse(readFileSync(join(SURVEY_DIR, f), "utf8")), oi, {});
      const key = r.ok ? "ok" : r.refusal.code;
      if (r.ok) assert.deepEqual(graphProblems(r.graph, oi), [], `${f}: the converted graph is not a graph`);
      if (!r.ok) assert.ok(known.has(r.refusal.code) && r.refusal.code !== REFUSAL.INTERNAL_ERROR, `${f}: ${r.refusal.code} ${r.refusal.message}`);
      counts[key] = (counts[key] ?? 0) + 1;
    }
    console.log(`survey: ${JSON.stringify(counts)}`); // keep: the survey's one line of output
  });
