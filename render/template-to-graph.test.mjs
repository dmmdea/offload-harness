// node --test render/template-to-graph.test.mjs
//
// The template-to-API converter (render/template-to-graph.mjs): a ComfyUI UI-format workflow in, the API-format prompt graph out.
// One test per behaviour, on small hand-written UI-format graphs built here, the synthetic /object_info and the two hand-worked
// goldens in render/testdata/template-goldens/ (see its README: none of them is a frontend export). The value-exact comparison
// with the frontend's own export on real templates is a separate, privately run test that reads TEMPLATE_GOLDEN_DIR.
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
// wire to a node that is not in the graph, a wire to an output slot the class does not have.
const isRef = (v) => Array.isArray(v) && v.length === 2 && typeof v[0] === "string" && Number.isInteger(v[1]);
function graphProblems(graph, oi) {
  const out = [];
  for (const [id, n] of Object.entries(graph)) {
    if (!oi[n.class_type]) { out.push(`${id}: unknown class ${n.class_type}`); continue; }
    for (const [k, v] of Object.entries(n.inputs)) {
      if (!isRef(v)) continue;
      const t = graph[v[0]];
      if (!t) out.push(`${id}.${k}: wire to missing node ${v[0]}`);
      else if (oi[t.class_type] && v[1] >= oi[t.class_type].output.length) out.push(`${id}.${k}: wire to slot ${v[1]} of ${v[0]} (${t.class_type} has ${oi[t.class_type].output.length})`);
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

test("mode 4 (bypassed): an output takes the first connected input of the same type; chains resolve; a union type matches", () => {
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

test("mode 4 (bypassed): of two connected inputs of the output's type the first one, in input order, is passed through", () => {
  const spec = (mode) => [
    { id: 1, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["a.png", "image"] },
    { id: 2, type: "LoadImage", outs: [["IMAGE", "IMAGE"], ["MASK", "MASK"]], wv: ["b.png", "image"] },
    { id: 3, type: "ImageMix", mode, ins: [["image_a", "IMAGE"], ["image_b", "IMAGE"]], outs: [["IMAGE", "IMAGE"]], wv: [0.5] },
    { id: 4, type: "ImageInvert", ins: [["image", "IMAGE"]], outs: [["IMAGE", "IMAGE"]] },
  ];
  const both = convert(ui(spec(4), [[1, 0, 3, "image_a"], [2, 0, 3, "image_b"], [3, 0, 4, "image"]]));
  assert.deepEqual(graphOf(both)["4"].inputs, { image: ["1", 0] }, "image_a is first");
  const onlyB = convert(ui(spec(4), [[2, 0, 3, "image_b"], [3, 0, 4, "image"]]));
  assert.deepEqual(graphOf(onlyB)["4"].inputs, { image: ["2", 0] }, "an unconnected first input is not a candidate");
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
  for (const bad of [null, undefined, 5, "x", [], {}, { nodes: "no" }, { nodes: [null] }, { nodes: [{ type: "KSampler" }] }, { nodes: [{ id: 1 }] }]) {
    refusalOf(convert(bad), REFUSAL.BAD_TEMPLATE);
  }
  refusalOf(convert({ nodes: [{ id: 1, type: "Note" }, { id: 1, type: "Note" }] }), REFUSAL.BAD_TEMPLATE);
  refusalOf(convert({ nodes: [], links: [[1, 2]] }), REFUSAL.BAD_TEMPLATE);
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
test("frontend goldens: every <name>.workflow.json in TEMPLATE_GOLDEN_DIR converts to its frontend-exported <name>.api.json, value for value",
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
      assert.deepEqual(compareApiGraphs(r.graph, JSON.parse(readFileSync(api, "utf8"))), [], `${name}: differs from the frontend export`);
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
