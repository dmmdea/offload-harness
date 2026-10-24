// node --test render/builder-drift.test.mjs
//
// The builder drift guard (render/builder-drift.mjs). Three layers:
//
//   1. The committed manifest against the builders, offline: every guarded builder is rebuilt with the spec's fixed
//      arguments and each recipe widget of its output graph is compared with render/testdata/builder-drift.json. One
//      test per builder, so a red run names the builder in the test title and in the failure message.
//   2. The machinery against synthetic templates (written here from the specs, no upstream data): a widget moved in
//      the builder's graph, a widget moved in the template, a node that vanished from the template and the wiring
//      defect of upstream PR #1308 each fail naming the builder. Every recipe widget of every builder is mutated once
//      on each side.
//   3. The manifest against the real workflow-templates package, when one is at hand (COMFY_TEMPLATES_DIR is its
//      `templates` directory, or COMFYUI_DIR a ComfyUI tree that has it installed; the CLI's own resolver picks it).
//      Skipped, with the reason, otherwise: the package is not in this repository and none of it is copied here.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import {
  buildManifest, builderValues, canonical, checkBuilderSide, checkCoverage, checkManifestShape, checkTemplateSide, formatManifest,
  itemDigest, loadManifest, main, recipeSha256, resolveTemplatesDir, templateSource, templateValue, MANIFEST_PATH,
} from "./builder-drift.mjs";
import { BUILDER_SPECS, NOT_TEMPLATE_DERIVED } from "./builder-drift-specs.mjs";
import { scratchDir } from "./test-scratch.mjs";

const manifest = loadManifest();
const messages = (failures) => failures.map((f) => f.message).join("\n");

// A different value of the same type: the smallest change a widget can suffer.
const mutate = (v) => (typeof v === "number" ? v + 1 : typeof v === "boolean" ? !v : `${v}~`);

// ---------------------------------------------------------------------------------------------
// 1. The manifest against the builders
// ---------------------------------------------------------------------------------------------

test("every wf-*.mjs builder is guarded or classified as not template-derived", () => {
  assert.deepEqual(checkCoverage(), []);
});

test("a wf-*.mjs file that is neither guarded nor classified fails, naming it", (t) => {
  const dir = scratchDir(t, "bd-cov-");
  for (const name of ["wf-krea2.mjs", "wf-new-thing.mjs", "wf-new-thing.test.mjs"]) writeFileSync(join(dir, name), "export {};\n");
  const failures = checkCoverage({ renderDir: dir, specs: BUILDER_SPECS.filter((s) => s.builder === "wf-krea2"), notDerived: {} });
  const text = messages(failures);
  assert.match(text, /builder drift: wf-new-thing: is neither guarded/);
  assert.doesNotMatch(text, /wf-new-thing\.test/);
  assert.equal(failures.length, 1, text);
});

test("a guarded or classified name that has no builder file in render/ fails, naming it", (t) => {
  const dir = scratchDir(t, "bd-cov-gone-");
  writeFileSync(join(dir, "wf-other.mjs"), "export {};\n");
  const krea = BUILDER_SPECS.filter((s) => s.builder === "wf-krea2");
  const gone = checkCoverage({ renderDir: dir, specs: krea, notDerived: { "wf-other": "a reason that is long enough to be a sentence" } });
  assert.match(messages(gone), /^builder drift: wf-krea2: is named in builder-drift-specs\.mjs but render\/ has no such builder$/);
  assert.equal(gone.length, 1);
  const goneClassified = checkCoverage({ renderDir: dir, specs: [], notDerived: { "wf-other": "a reason that is long enough to be a sentence", "wf-ghost": "a reason that is long enough to be a sentence" } });
  assert.match(messages(goneClassified), /^builder drift: wf-ghost: is named in builder-drift-specs\.mjs but render\/ has no such builder$/);
  assert.equal(goneClassified.length, 1);
});

test("a builder that is both guarded and classified as not template-derived fails, naming it", (t) => {
  const dir = scratchDir(t, "bd-cov-both-");
  writeFileSync(join(dir, "wf-krea2.mjs"), "export {};\n");
  const failures = checkCoverage({ renderDir: dir, specs: BUILDER_SPECS.filter((s) => s.builder === "wf-krea2"), notDerived: { "wf-krea2": "a reason that is long enough to be a sentence" } });
  assert.match(messages(failures), /^builder drift: wf-krea2: is both guarded and listed as not template-derived; pick one$/);
  assert.equal(failures.length, 1);
});

test("a not-template-derived entry needs a reason of at least a sentence (the function, not only the data)", (t) => {
  const dir = scratchDir(t, "bd-cov-reason-");
  for (const name of ["wf-a.mjs", "wf-b.mjs", "wf-c.mjs"]) writeFileSync(join(dir, name), "export {};\n");
  const failures = checkCoverage({ renderDir: dir, specs: [], notDerived: { "wf-a": "short", "wf-b": 42, "wf-c": "     padded      " } });
  const text = messages(failures);
  for (const name of ["wf-a", "wf-b", "wf-c"]) assert.match(text, new RegExp(`builder drift: ${name}: NOT_TEMPLATE_DERIVED needs a reason of at least a sentence`));
  assert.equal(failures.length, 3, text);
});

test("every not-template-derived builder carries a stated reason", () => {
  for (const [name, reason] of Object.entries(NOT_TEMPLATE_DERIVED)) assert.ok(reason.length >= 20, `${name} needs a reason`);
});

test("the manifest and the specs describe the same builders, views and widgets", () => {
  assert.deepEqual(checkManifestShape(manifest), []);
});

test("the manifest records a template name and a package version for every view, and names and hashes only", () => {
  for (const [name, entry] of Object.entries(manifest.builders)) {
    for (const [view, rec] of Object.entries(entry.views)) {
      assert.match(rec.template, /^[a-z0-9_.-]+$/i, `${name} [${view}] template name`);
      assert.match(rec.json_version, /^\d+\.\d+\.\d+$/, `${name} [${view}] package version`);
      assert.match(rec.templateSha256, /^[0-9a-f]{64}$/);
      assert.match(rec.builderSha256, /^[0-9a-f]{64}$/);
      for (const [key, it] of Object.entries(rec.items)) {
        assert.match(it.template, /^[0-9a-f]{16}$/, `${name} [${view}] ${key}`);
        assert.ok(it.builder === null || /^[0-9a-f]{16}$/.test(it.builder), `${name} [${view}] ${key}`);
      }
    }
  }
});

test("the manifest file holds no host path, no drive letter and no prompt text", () => {
  const raw = formatManifest(manifest);
  assert.doesNotMatch(raw, /[A-Za-z]:[\\/]|\/home\/|\.venv|site-packages/);
  assert.equal(raw, formatManifest(JSON.parse(raw)));
});

for (const spec of BUILDER_SPECS) {
  test(`builder drift: ${spec.builder}`, () => {
    const failures = checkBuilderSide(manifest, { specs: [spec] });
    assert.equal(failures.length, 0, `${spec.builder} no longer builds the recipe the manifest was verified against:\n${messages(failures)}`);
  });
}

// ---------------------------------------------------------------------------------------------
// 2. The machinery, against synthetic templates
// ---------------------------------------------------------------------------------------------

test("canonical form: float noise in a template does not move a hash, a real change does", () => {
  assert.equal(canonical(3.1000000000000005), canonical(3.1));
  assert.equal(itemDigest(5.000000000000001), itemDigest(5));
  assert.notEqual(itemDigest(3.1), itemDigest(3.2));
  assert.notEqual(itemDigest("euler"), itemDigest("euler_ancestral"));
  const a = recipeSha256(new Map([["b", 1], ["a", 2]]));
  assert.equal(a, recipeSha256(new Map([["a", 2], ["b", 1]])), "key order must not flip the hash");
  assert.notEqual(a, recipeSha256(new Map([["a", 2], ["b", 2]])));
});

// synthTemplates: one synthetic template per template name, written from the specs so that every locator reads
// the builder's own value (a differing value where the spec declares a delta, `true` for a wiring item).
// `override` maps a template name + locator to the value to plant instead. Every synthetic subgraph starts with an
// IMAGE input, which is a link and not a widget, so an instance index that forgot to skip it reads the wrong value.
const WIDGET_TYPES = new Set(["STRING", "BOOLEAN", "INT", "FLOAT", "COMBO"]);
function synthTemplates({ override = new Map(), specs = BUILDER_SPECS } = {}) {
  const byName = new Map();
  const planted = new Map();
  const container = (tpl, sgName, state) => {
    if (sgName == null) return tpl;
    let s = state.sgs.get(sgName);
    if (!s) {
      s = { id: `sg-${state.sgs.size}`, name: sgName, nodes: [], links: [], inputs: [{ name: "image", type: "IMAGE" }] };
      state.sgs.set(sgName, s);
      tpl.definitions.subgraphs.push(s);
    }
    return s;
  };
  const nodeIn = (c, id, type) => {
    let n = c.nodes.find((x) => x.id === id);
    if (!n) { n = { id, type, mode: 0, widgets_values: [], inputs: [] }; c.nodes.push(n); }
    return n;
  };
  const put = (arr, idx, v, tag) => {
    if (arr[idx] !== undefined && canonical(arr[idx]) !== canonical(v)) throw new Error(`synthetic template conflict at ${tag}`);
    arr[idx] = v;
  };
  for (const spec of specs) {
    for (const view of spec.views) {
      const bVals = builderValues(spec, view);
      let entry = byName.get(view.template);
      if (!entry) {
        entry = { tpl: { nodes: [], links: [], definitions: { subgraphs: [] } }, state: { sgs: new Map() } };
        byName.set(view.template, entry);
      }
      const { tpl, state } = entry;
      for (const item of view.items) {
        const t = item.t;
        const lkey = `${view.template}|${JSON.stringify(t)}`;
        let v = item.b ? bVals.get(item.key) : true;
        if (item.delta) v = mutate(v);
        if (override.has(lkey)) { v = override.get(lkey); planted.set(lkey, true); }
        const tag = `${view.template} ${item.key}`;
        if (t.kind === "w") {
          const c = container(tpl, t.sg, state);
          const node = nodeIn(c, t.id, t.type);
          put(node.widgets_values, t.w, v, tag);
          if (t.proxied) {
            // the legacy form: the interior widget is linked to a subgraph input; the root instance lists it and has no values
            let slot = c.inputs.findIndex((i) => i.name === t.name);
            if (slot < 0) { c.inputs.push({ name: t.name, type: "COMBO" }); slot = c.inputs.length - 1; }
            if (!node.inputs.some((i) => i.name === t.name)) {
              const id = 200 + c.links.length;
              node.inputs.push({ name: t.name, type: "COMBO", widget: { name: t.name }, link: id });
              c.links.push({ id, origin_id: -10, origin_slot: slot, target_id: t.id, target_slot: node.inputs.length - 1 });
            }
            let inst = tpl.nodes.find((n) => n.type === c.id);
            if (!inst) { inst = { id: 90000 + state.sgs.size, type: c.id, mode: 0, widgets_values: [], inputs: [], properties: { proxyWidgets: [] } }; tpl.nodes.push(inst); }
            inst.properties.proxyWidgets.push([String(t.id), t.name]);
          }
        } else if (t.kind === "inst") {
          const sgName = t.type.slice("subgraph:".length);
          const s = container(tpl, sgName, state);
          const inst = nodeIn(tpl, t.id, s.id);
          if (!s.inputs.some((i) => i.name === t.input)) s.inputs.push({ name: t.input, type: "INT" });
          const idx = s.inputs.filter((i) => WIDGET_TYPES.has(i.type)).findIndex((i) => i.name === t.input);
          put(inst.widgets_values, idx, v, tag);
        } else if (t.kind === "distinct") {
          const s = container(tpl, t.sg, state);
          const n = nodeIn(s, t.id, t.type);
          t.inputs.forEach((name, k) => {
            if (n.inputs.some((i) => i.name === name)) return;
            n.inputs.push({ name, link: 100 + k });
            if (!s.inputs.some((i) => i.name === `in${k}`)) s.inputs.push({ name: `in${k}`, type: "FLOAT" });
            const slotOf = (j) => s.inputs.findIndex((i) => i.name === `in${j}`);
            s.links.push({ id: 100 + k, origin_id: -10, origin_slot: v === false ? slotOf(0) : slotOf(k), target_id: t.id, target_slot: k });
          });
        }
      }
    }
  }
  return { loadTemplate: (name) => { if (!byName.has(name)) throw new Error(`template ${name} is not in the synthetic package`); return byName.get(name).tpl; }, planted, templates: byName };
}
const SYN_VERSIONS = { "comfyui-workflow-templates-json": "9.9.1", "comfyui-workflow-templates": "9.0.0" };
const synthManifest = () => {
  const syn = synthTemplates();
  return { syn, manifest: buildManifest({ loadTemplate: syn.loadTemplate, versions: SYN_VERSIONS }) };
};

test("synthetic package: the manifest built from it is consistent and both sides pass", () => {
  const { syn, manifest: m } = synthManifest();
  assert.deepEqual(checkManifestShape(m), []);
  assert.deepEqual(checkBuilderSide(m), []);
  const r = checkTemplateSide(m, { loadTemplate: syn.loadTemplate, versions: SYN_VERSIONS });
  assert.deepEqual(r.failures, []);
  assert.deepEqual(r.notes, []);
  // statuses: same everywhere except the declared deltas and the template-only wiring item
  const statuses = new Set();
  for (const e of Object.values(m.builders)) for (const v of Object.values(e.views)) for (const it of Object.values(v.items)) statuses.add(it.status);
  assert.deepEqual([...statuses].sort(), ["deliberate", "same", "template-only", "unreviewed"]);
});

test("a template widget planted in the template turns the template side red, naming the builder (every widget of every builder)", () => {
  const { syn, manifest: m } = synthManifest();
  let planted = 0;
  for (const spec of BUILDER_SPECS) {
    for (const view of spec.views) {
      for (const item of view.items) {
        if (item.t.kind === "distinct") continue; // its own test below
        const lkey = `${view.template}|${JSON.stringify(item.t)}`;
        const was = templateValue(spec, view, syn.loadTemplate(view.template), item);
        const moved = synthTemplates({ override: new Map([[lkey, mutate(was)]]) });
        const r = checkTemplateSide(m, { loadTemplate: moved.loadTemplate, versions: SYN_VERSIONS });
        const text = messages(r.failures);
        assert.ok(moved.planted.has(lkey), `the plant for ${spec.builder} ${item.key} was not applied`);
        assert.match(text, new RegExp(`builder drift: ${spec.builder} \\[${view.name}\\]: template ${view.template} changed under the builder: widgets? [^(]*\\b${item.key.replace(/\./g, "\\.")}\\b`), `${spec.builder} ${item.key}\n${text}`);
        planted++;
      }
    }
  }
  assert.ok(planted > 100, `only ${planted} widgets were planted`);
});

test("a recipe widget moved in the builder's output graph turns the builder side red, naming the builder (every widget of every builder)", () => {
  // The baseline is built from the builders as they are now, so this test checks the machinery and stays
  // independent of whether a builder has already drifted from the committed manifest (that is the per-builder test).
  const { manifest: baseline } = synthManifest();
  let planted = 0;
  for (const spec of BUILDER_SPECS) {
    for (const view of spec.views) {
      for (const item of view.items) {
        if (!item.b) continue;
        const moved = {
          ...spec,
          views: [view],
          build: (args) => {
            const g = spec.build(args);
            const inputs = g[item.b.node].inputs;
            inputs[item.b.input] = mutate(inputs[item.b.input]);
            return g;
          },
        };
        const failures = checkBuilderSide(baseline, { specs: [moved] });
        assert.equal(failures.length, 1, `${spec.builder} [${view.name}] ${item.key}`);
        assert.equal(failures[0].builder, spec.builder);
        assert.match(failures[0].message, new RegExp(`^builder drift: ${spec.builder} \\[${view.name}\\]: recipe widget changed in the builder's output graph: ${item.key.replace(/\./g, "\\.")} `));
        planted++;
      }
    }
  }
  assert.ok(planted > 100, `only ${planted} widgets were planted`);
});

test("the PR #1308 class: one subgraph input feeding both the start and the end percent fails, naming wf-wan-animate2", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-wan-animate2");
  const view = spec.views[0];
  const item = view.items.find((i) => i.t.kind === "distinct");
  const { syn, manifest: m } = synthManifest();
  const tpl = structuredClone(syn.loadTemplate(view.template));
  const sg = tpl.definitions.subgraphs.find((s) => s.name === item.t.sg);
  const into = sg.links.filter((l) => l.target_id === item.t.id);
  for (const l of into) l.origin_slot = into[0].origin_slot; // both inputs now come from one subgraph input
  const r = checkTemplateSide(m, { loadTemplate: (n) => (n === view.template ? tpl : syn.loadTemplate(n)), versions: SYN_VERSIONS });
  assert.equal(r.failures.length, 1);
  assert.match(r.failures[0].message, /^builder drift: wf-wan-animate2 \[default\]: template video_wan_animate2_distilled changed under the builder: widget wiring\.pose_start_end_distinct /);
});

test("a template that lost the node, the type or the widget a builder copied fails, naming the builder and the package version", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-krea2");
  const view = spec.views[0];
  const { syn, manifest: m } = synthManifest();
  const cases = {
    "has no node 3": (tpl) => { const sg = tpl.definitions.subgraphs[0]; sg.nodes = sg.nodes.filter((n) => n.id !== 3); },
    "is KSamplerAdvanced, the spec expects KSampler": (tpl) => { tpl.definitions.subgraphs[0].nodes.find((n) => n.id === 3).type = "KSamplerAdvanced"; },
    "has no widget 2": (tpl) => { delete tpl.definitions.subgraphs[0].nodes.find((n) => n.id === 3).widgets_values[2]; },
  };
  for (const [what, edit] of Object.entries(cases)) {
    const tpl = structuredClone(syn.loadTemplate(view.template));
    edit(tpl);
    const r = checkTemplateSide(m, { loadTemplate: (n) => (n === view.template ? tpl : syn.loadTemplate(n)), versions: SYN_VERSIONS });
    assert.equal(r.failures.length, 1, what);
    assert.match(r.failures[0].message, new RegExp(`^builder drift: wf-krea2 \\[default\\]: .*${what}.*comfyui-workflow-templates-json 9\\.9\\.1, installed 9\\.9\\.1`), what);
  }
});

test("a package version that moved with every guarded widget unchanged is a note, not a failure", () => {
  const { syn, manifest: m } = synthManifest();
  const r = checkTemplateSide(m, { loadTemplate: syn.loadTemplate, versions: { ...SYN_VERSIONS, "comfyui-workflow-templates-json": "9.9.2" } });
  assert.deepEqual(r.failures, []);
  assert.equal(r.notes.length, BUILDER_SPECS.reduce((a, s) => a + s.views.length, 0));
  assert.match(r.notes[0], /verified against comfyui-workflow-templates-json 9\.9\.1, installed 9\.9\.2; every guarded widget of \S+ is unchanged/);
});

test("recording refuses a builder/template difference the spec does not declare, naming the builder", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-krea2");
  const view = spec.views[0];
  const step = view.items.find((i) => i.key === "ksampler.steps");
  const lkey = `${view.template}|${JSON.stringify(step.t)}`;
  const syn = synthTemplates({ override: new Map([[lkey, 99]]), specs: [spec] });
  assert.throws(() => buildManifest({ loadTemplate: syn.loadTemplate, versions: SYN_VERSIONS, specs: [spec] }), /wf-krea2 \[default\] ksampler\.steps: the builder and the template differ and the spec declares no delta/);
});

test("recording refuses a delta declaration the data no longer needs", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-wan-animate2");
  const view = spec.views[0];
  const stale = { ...spec, views: [{ ...view, items: view.items.map((i) => (i.key === "pose.end_percent" ? { ...i, delta: { kind: "deliberate", reason: "stale" } } : i)) }] };
  const syn = synthTemplates({ specs: [spec] });
  assert.throws(() => buildManifest({ loadTemplate: syn.loadTemplate, versions: SYN_VERSIONS, specs: [stale] }), /wf-wan-animate2 \[default\] pose\.end_percent: declared as a deliberate delta but builder and template now agree/);
});

// shapeFailures: checkManifestShape on a copy of the committed manifest after `edit`.
function shapeFailures(edit) {
  const copy = structuredClone(manifest);
  edit(copy.builders);
  return messages(checkManifestShape(copy));
}

test("the manifest shape check catches each way a status can contradict its digests or the spec", () => {
  // the status differs from what the spec says (here: the spec says same)
  assert.equal(
    shapeFailures((b) => { b["wf-krea2"].views.default.items["ksampler.steps"].status = "deliberate"; }),
    "builder drift: wf-krea2 [default]: widget ksampler.steps is deliberate in the manifest, the spec says same\n" +
      "builder drift: wf-krea2 [default]: widget ksampler.steps is marked deliberate but the digests agree",
  );
  // deliberate and unreviewed entries must have digests that differ
  assert.match(
    shapeFailures((b) => { const it = b["wf-qwen-image-21"].views.t2i.items["ksampler.steps"]; it.builder = it.template; }),
    /^builder drift: wf-qwen-image-21 \[t2i\]: widget ksampler\.steps is marked deliberate but the digests agree$/,
  );
  assert.match(
    shapeFailures((b) => { const it = b["wf-qwen-image-edit"].views.full.items["ksampler.cfg"]; it.builder = it.template; }),
    /^builder drift: wf-qwen-image-edit \[full\]: widget ksampler\.cfg is marked unreviewed but the digests agree$/,
  );
  // a template-only entry carries no builder digest
  assert.match(
    shapeFailures((b) => { b["wf-wan-animate2"].views.default.items["wiring.pose_start_end_distinct"].builder = "0123456789abcdef"; }),
    /^builder drift: wf-wan-animate2 \[default\]: widget wiring\.pose_start_end_distinct is template-only but carries a builder digest$/,
  );
  // a status that is not the spec's, in the other direction: the spec declares a delta the manifest calls same
  const sameButDiffer = shapeFailures((b) => { const it = b["wf-qwen-image-21"].views.t2i.items["ksampler.steps"]; it.status = "same"; });
  assert.match(sameButDiffer, /widget ksampler\.steps is same in the manifest, the spec says deliberate/);
  assert.match(sameButDiffer, /widget ksampler\.steps is marked same but the digests differ/);
});

test("an instance's widget index counts only the widget-typed subgraph inputs", () => {
  const tpl = {
    nodes: [{ id: 9, type: "sg-x", widgets_values: ["a", 7, true], inputs: [] }],
    links: [],
    definitions: { subgraphs: [{ id: "sg-x", name: "Sub", nodes: [], links: [], inputs: [
      { name: "image", type: "IMAGE" }, { name: "text", type: "STRING" }, { name: "mask", type: "MASK" }, { name: "steps", type: "INT" }, { name: "flag", type: "BOOLEAN" },
    ] }] },
  };
  const read = (input) => templateValue({ builder: "wf-x" }, { name: "v", template: "t" }, tpl, { key: "k", b: null, t: { kind: "inst", sg: null, id: 9, type: "subgraph:Sub", input } });
  assert.equal(read("text"), "a");
  assert.equal(read("steps"), 7);
  assert.equal(read("flag"), true);
  assert.throws(() => read("image"), /has no promoted widget image/);
  assert.throws(() => read("mask"), /has no promoted widget mask/);
});

test("a subgraph instance whose subgraph is not the one the spec names fails, naming the builder", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-qwen-image-21");
  const view = spec.views.find((v) => v.name === "t2i");
  const item = view.items.find((i) => i.key === "ksampler.cfg");
  const { syn } = synthManifest();
  const tpl = structuredClone(syn.loadTemplate(view.template));
  assert.equal(typeof templateValue(spec, view, tpl, item), "number"); // as written, it reads
  tpl.definitions.subgraphs.find((s) => s.name === item.t.type.slice("subgraph:".length)).name = "Some Other Subgraph";
  assert.throws(() => templateValue(spec, view, tpl, item), /wf-qwen-image-21 \[t2i\] ksampler\.cfg: template image_qwen_image_2_1_t2i node 459 is sg-\d+, the spec expects subgraph:Text to Image \(Qwen Image 2\.1\)/);
});

test("the manifest shape check catches a status that contradicts its digests, and a hand-edited hash", () => {
  const copy = structuredClone(manifest);
  const it = copy.builders["wf-krea2"].views.default.items["ksampler.steps"];
  it.template = "0000000000000000";
  assert.match(messages(checkManifestShape(copy)), /builder drift: wf-krea2 \[default\]: widget ksampler\.steps is marked same but the digests differ/);
  const copy2 = structuredClone(manifest);
  copy2.builders["wf-krea2"].views.default.builderSha256 = "0".repeat(64);
  assert.match(messages(checkBuilderSide(copy2)), /builder drift: wf-krea2 \[default\]: builderSha256 does not match/);
  const copy3 = structuredClone(manifest);
  delete copy3.builders["wf-krea2"];
  assert.match(messages(checkManifestShape(copy3)), /builder drift: wf-krea2: has a spec but no manifest entry/);
});

test("the CLI check passes on the committed manifest and fails on a planted one, naming the builder", async (t) => {
  const out = [];
  const err = [];
  const io = { out: (s) => out.push(s), err: (s) => err.push(s), env: {} };
  assert.equal(await main(["check"], io), 0, err.join(""));
  assert.match(out.join(""), /^note: template side skipped.*\nok: \d+ builders, \d+ views\n$/s);

  const dir = scratchDir(t, "bd-cli-");
  const planted = structuredClone(manifest);
  planted.builders["wf-ltx25-i2v"].views.default.items["base.sigmas"].builder = "ffffffffffffffff";
  const file = join(dir, "planted.json");
  writeFileSync(file, JSON.stringify(planted));
  out.length = 0; err.length = 0;
  assert.equal(await main(["check", "--manifest", file], io), 1);
  assert.match(err.join(""), /builder drift: wf-ltx25-i2v \[default\]: recipe widget changed in the builder's output graph: base\.sigmas /);
  assert.equal(await main(["explain"], io), 2);
  assert.equal(await main(["nonsense"], io), 2);
});

// writeSyntheticPackage: the synthetic templates as a package directory (`site-packages/<json pkg>/templates` plus the
// dist-info markers the version reader looks for). Returns the templates directory.
function writeSyntheticPackage(dir, syn = synthTemplates()) {
  const tdir = join(dir, "site-packages", "comfyui_workflow_templates_json", "templates");
  mkdirSync(tdir, { recursive: true });
  mkdirSync(join(dir, "site-packages", "comfyui_workflow_templates_json-9.9.1.dist-info"));
  writeFileSync(join(dir, "site-packages", "comfyui_workflow_templates-9.0.0.dist-info"), "");
  for (const name of syn.templates.keys()) writeFileSync(join(tdir, `${name}.json`), JSON.stringify(syn.loadTemplate(name)));
  return tdir;
}

test("the CLI reads a synthetic templates directory: explain lists the widgets, record writes only the manifest", async (t) => {
  const dir = scratchDir(t, "bd-pkg-");
  const tdir = writeSyntheticPackage(dir);
  const src = templateSource(tdir);
  assert.equal(src.versions["comfyui-workflow-templates-json"], "9.9.1");
  const out = [];
  const err = [];
  const manifestFile = join(dir, "m.json");
  const io = { out: (s) => out.push(s), err: (s) => err.push(s), env: {} };
  assert.equal(await main(["record", "--templates-dir", tdir, "--manifest", manifestFile], io), 0, err.join(""));
  assert.equal(await main(["check", "--templates-dir", tdir, "--manifest", manifestFile], io), 0, err.join(""));
  assert.match(out.join(""), /template side checked against 9\.9\.1/);
  out.length = 0;
  assert.equal(await main(["explain", "--templates-dir", tdir], io), 0);
  assert.match(out.join(""), /wf-krea2 \[default\]  template image_krea2_turbo_t2i\n\s+ksampler\.steps\s+template 8\s+builder 8\s+same/);
  assert.match(out.join(""), /DIFFERS \(unreviewed\)/);
});

// ---------------------------------------------------------------------------------------------
// 2b. Effective values: a locator may not read a widget whose input is linked
// ---------------------------------------------------------------------------------------------

const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// plantLink: link the input `name` of the node a locator points at, the way an upstream edit that wires the
// widget to a subgraph input or to another node would. The node keeps its (now stale) serialized value.
function plantLink(tpl, t, name) {
  const c = t.sg == null ? tpl : tpl.definitions.subgraphs.find((s) => s.name === t.sg);
  const node = c.nodes.find((n) => n.id === t.id);
  node.inputs = [...(node.inputs ?? []), { name, type: "INT", widget: { name }, link: 90001 }];
  c.links = [...(c.links ?? []), { id: 90001, origin_id: 5000, origin_slot: 0, target_id: t.id, target_slot: 0 }];
}

test("a link planted on the widget a locator names fails with the builder, the key and the fix (every widget locator of every builder)", () => {
  const { syn } = synthManifest();
  let planted = 0;
  for (const spec of BUILDER_SPECS) {
    for (const view of spec.views) {
      for (const item of view.items) {
        const t = item.t;
        if (t.kind === "distinct" || t.proxied) continue;
        const name = t.kind === "inst" ? t.input : t.name;
        assert.equal(typeof name, "string", `${spec.builder} [${view.name}] ${item.key}: the locator names no widget`);
        const tpl = structuredClone(syn.loadTemplate(view.template));
        plantLink(tpl, t, name);
        assert.throws(
          () => templateValue(spec, view, tpl, item),
          new RegExp(`${esc(spec.builder)} \\[${esc(view.name)}\\] ${esc(item.key)}: .* ${esc(name)} is linked \\(to node:\\S+#0\\), so its serialized value is not the one that runs`),
          `${spec.builder} [${view.name}] ${item.key}`,
        );
        planted++;
      }
    }
  }
  assert.ok(planted > 100, `only ${planted} locators were planted`);
});

test("every widget locator names its widget the way the builder's input is named (a primitive node's widget is `value`)", () => {
  for (const spec of BUILDER_SPECS) {
    for (const view of spec.views) {
      for (const item of view.items) {
        const t = item.t;
        if (t.kind !== "w" || !item.b) continue; // the ones with no builder side are pinned by the test below
        const want = /^Primitive/.test(t.type) ? "value" : item.b.input;
        assert.equal(t.name, want, `${spec.builder} [${view.name}] ${item.key}`);
      }
    }
  }
});

test("the widget locators that have no builder side, and so nothing to compare their name with, are exactly this list", () => {
  // A template-only widget locator is checked against a literal: comparing its name with itself proves nothing.
  const templateOnly = [];
  for (const spec of BUILDER_SPECS) for (const view of spec.views) for (const item of view.items) {
    if (item.t.kind === "w" && !item.b) templateOnly.push(`${spec.builder} [${view.name}] ${item.key} ${item.t.type} ${item.t.name}`);
  }
  assert.deepEqual(templateOnly, ["wf-ltx25-i2v [default] conditioning.frame_rate_expression ComfyMathExpression expression"]);
});

test("a widget locator that names no widget is refused", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-krea2");
  const view = spec.views[0];
  const { syn } = synthManifest();
  const item = { key: "ksampler.steps", t: { kind: "w", sg: "Text to Image (Krea-2 Turbo)", id: 3, type: "KSampler", w: 2 }, b: null };
  assert.throws(() => templateValue(spec, view, syn.loadTemplate(view.template), item), /wf-krea2 \[default\] ksampler\.steps: .*names no widget/);
});

// A template in the legacy form: the subgraph's widget is promoted by name in the instance's `proxyWidgets`, the
// instance has no values of its own, and the interior node's widget is linked to the subgraph input. `sgInputs` sets the
// order of the subgraph's inputs, `instanceInputs` and `rootLinks` what is wired to the instance in the root graph.
function proxiedTemplate({
  proxy = [["227", "text"], ["221", "lora_name"]], instanceValues = [], linked = true,
  sgInputs = [{ name: "text", type: "STRING" }, { name: "lora_name", type: "COMBO" }], instanceInputs = [], rootLinks = [],
} = {}) {
  return {
    nodes: [{ id: 238, type: "sg-1", mode: 0, inputs: instanceInputs, widgets_values: instanceValues, properties: { proxyWidgets: proxy } }],
    links: rootLinks,
    definitions: {
      subgraphs: [{
        id: "sg-1",
        name: "Proxied",
        inputs: sgInputs,
        nodes: [{
          id: 221, type: "LoraLoaderModelOnly", mode: 0, widgets_values: ["a.safetensors", 1],
          inputs: [{ name: "model", type: "MODEL", link: 1 }, { name: "lora_name", type: "COMBO", widget: { name: "lora_name" }, link: linked ? 2 : null }],
        }],
        links: [
          { id: 1, origin_id: 7, origin_slot: 0, target_id: 221, target_slot: 0, type: "MODEL" },
          { id: 2, origin_id: -10, origin_slot: sgInputs.findIndex((i) => i.name === "lora_name"), target_id: 221, target_slot: 1, type: "COMBO" },
        ],
      }],
    },
  };
}
const PROXY_SPEC = { builder: "wf-x" };
const PROXY_VIEW = { name: "v", template: "tpl" };
const proxiedItem = (extra = {}) => ({ key: "lora.name", b: null, t: { kind: "w", sg: "Proxied", id: 221, type: "LoraLoaderModelOnly", w: 0, name: "lora_name", ...extra } });

test("a legacy proxied widget reads the interior value, once the instance is shown to list it and to carry no value of its own", () => {
  const read = (tpl, extra = { proxied: true }) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, proxiedItem(extra));
  assert.equal(read(proxiedTemplate()), "a.safetensors");
  // the plain locator still refuses it: the widget is linked
  assert.throws(() => read(proxiedTemplate(), {}), /wf-x \[v\] lora\.name: .* lora_name is linked \(to input:lora_name\)/);
  assert.throws(() => read(proxiedTemplate({ proxy: [["227", "text"]] })), /does not list \[221, lora_name\] as a proxied widget/);
  assert.throws(() => read(proxiedTemplate({ instanceValues: ["t", "b.safetensors"] })), /instance 238 carries a promoted value for lora_name at position 1 of its proxyWidgets; read it with I \(position 1 is also/);
  assert.throws(() => read(proxiedTemplate({ linked: false })), /widget lora_name is not linked to a subgraph input; read it with W/);
});

test("a legacy proxied widget's promoted value is found where the template format puts it: the position in proxyWidgets, not the subgraph's input order", () => {
  const read = (tpl) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, proxiedItem({ proxied: true }));
  const text = { name: "text", type: "STRING" };
  const lora = { name: "lora_name", type: "COMBO" };
  // lora_name is listed first but is the second widget-typed input: the one value is at its position, so it carries one
  assert.throws(
    () => read(proxiedTemplate({ proxy: [["221", "lora_name"], ["227", "text"]], sgInputs: [text, lora], instanceValues: ["b.safetensors"] })),
    /instance 238 carries a promoted value for lora_name at position 0 of its proxyWidgets/,
  );
  // lora_name is listed second but is the first widget-typed input: the one value belongs to the widget listed first
  assert.equal(read(proxiedTemplate({ proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [lora, text], instanceValues: ["a prompt"] })), "a.safetensors");
  // a value at its own position is found whatever the order of the subgraph's inputs
  assert.throws(
    () => read(proxiedTemplate({ proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [lora, text], instanceValues: ["a prompt", "b.safetensors"] })),
    /instance 238 carries a promoted value for lora_name at position 1 of its proxyWidgets/,
  );
});

test("a proxied widget that carries a value points at I only where I would read the same position, and says so where it would not", () => {
  const read = (tpl) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, proxiedItem({ proxied: true }));
  const text = { name: "text", type: "STRING" };
  const lora = { name: "lora_name", type: "COMBO" };
  const message = (tpl) => { try { read(tpl); } catch (e) { return e.message; } return ""; };
  // proxyWidgets position 1 is also the second widget-typed input: an I locator reads the value that runs
  const agree = message(proxiedTemplate({ proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [text, lora], instanceValues: ["t", "b.safetensors"] }));
  assert.match(agree, /carries a promoted value for lora_name at position 1 of its proxyWidgets; read it with I \(position 1 is also its place among the widget-typed inputs\)/);
  // position 0 in proxyWidgets, the second among the widget-typed inputs: I would read the other widget
  const differ = message(proxiedTemplate({ proxy: [["221", "lora_name"], ["227", "text"]], sgInputs: [text, lora], instanceValues: ["b.safetensors"] }));
  assert.match(differ, /carries a promoted value for lora_name at position 0 of its proxyWidgets/);
  assert.match(differ, /I would read position 1, which holds another widget/);
  assert.match(differ, /no locator reads position 0/);
  assert.doesNotMatch(differ, /read it with I\b/);
  // the same facts the other way round
  const other = message(proxiedTemplate({ proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [lora, text], instanceValues: ["a prompt", "b.safetensors"] }));
  assert.match(other, /at position 1 of its proxyWidgets/);
  assert.match(other, /I would read position 0, which holds another widget/);
});

test("a proxied widget whose subgraph input is not widget-typed says that no locator reads it, not that I would read position -1", () => {
  const read = (tpl) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, proxiedItem({ proxied: true }));
  const message = (tpl) => { try { read(tpl); } catch (e) { return e.message; } return ""; };
  const text = { name: "text", type: "STRING" };
  // the wildcard type and a combined type are not in the set of widget-typed inputs, so I does not count the input at all
  for (const type of ["*", "COMBO,STRING"]) {
    const msg = message(proxiedTemplate({ proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [text, { name: "lora_name", type }], instanceValues: ["t", "b.safetensors"] }));
    assert.match(msg, /carries a promoted value for lora_name at position 1 of its proxyWidgets/, type);
    assert.match(msg, /no locator reads position 1: input lora_name is not a widget-typed input, so I does not count it/, type);
    assert.doesNotMatch(msg, /position -1/, type);
    assert.doesNotMatch(msg, /holds another widget/, type);
    assert.doesNotMatch(msg, /read it with I\b/, type);
  }
});

test("an I locator reads an instance by the order of the widget-typed inputs even where the instance also lists proxyWidgets", () => {
  // the claim the message above rests on, stated as a fact about the locator
  const tpl = proxiedTemplate({ proxy: [["221", "lora_name"], ["227", "text"]], sgInputs: [{ name: "text", type: "STRING" }, { name: "lora_name", type: "COMBO" }], instanceValues: ["first", "second"], linked: false });
  const sg = tpl.definitions.subgraphs[0];
  const read = (input) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, { key: "k", b: null, t: { kind: "inst", sg: null, id: 238, type: `subgraph:${sg.name}`, input } });
  assert.equal(read("text"), "first");
  assert.equal(read("lora_name"), "second");
});

test("a legacy proxied widget is refused when the root graph wires something into the instance's input for it", () => {
  const read = (tpl) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, proxiedItem({ proxied: true }));
  const entry = (name, link) => ({ name, type: name === "text" ? "STRING" : "COMBO", widget: { name }, link });
  // entries with no link are not a wiring, and an input wired for another widget is not this widget's
  assert.equal(read(proxiedTemplate({ instanceInputs: [entry("text", null), entry("lora_name", null)] })), "a.safetensors");
  assert.equal(read(proxiedTemplate({ instanceInputs: [entry("text", 7), entry("lora_name", null)], rootLinks: [[7, 41, 0, 238, 0, "STRING"]] })), "a.safetensors");
  // a link into the widget's own input: the value that runs comes through it, so the interior value is stale
  assert.throws(
    () => read(proxiedTemplate({ instanceInputs: [entry("text", null), entry("lora_name", 7)], rootLinks: [[7, 41, 0, 238, 1, "COMBO"]] })),
    /wf-x \[v\] lora\.name: .* instance 238 has its lora_name input linked at the root \(to node:\S+#0\)/,
  );
  // the object form of a link is read the same way
  assert.throws(
    () => read(proxiedTemplate({ instanceInputs: [entry("lora_name", 7)], rootLinks: [{ id: 7, origin_id: 41, origin_slot: 0, target_id: 238, target_slot: 0 }] })),
    /instance 238 has its lora_name input linked at the root/,
  );
});

test("a legacy proxied widget that is wired at the root is refused for the wiring even where the instance also carries a value", () => {
  const read = (tpl) => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, proxiedItem({ proxied: true }));
  const message = (tpl) => { try { read(tpl); } catch (e) { return e.message; } return ""; };
  const text = { name: "text", type: "STRING" };
  const lora = { name: "lora_name", type: "COMBO" };
  const entry = (name, link) => ({ name, type: name === "text" ? "STRING" : "COMBO", widget: { name }, link });
  const wired = { instanceInputs: [entry("text", null), entry("lora_name", 7)], rootLinks: [[7, 41, 0, 238, 1, "COMBO"]] };
  const wiring = /instance 238 has its lora_name input linked at the root \(to node:\S+#0\), so the interior value is not the one that runs/;
  // the wiring wins in all three cases of the carried value: the positions agree (I would be named), they differ, and the input is not widget-typed
  const cases = {
    "positions agree": { proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [text, lora], instanceValues: ["t", "b.safetensors"] },
    "positions differ": { proxy: [["221", "lora_name"], ["227", "text"]], sgInputs: [text, lora], instanceValues: ["b.safetensors"] },
    "input not widget-typed": { proxy: [["227", "text"], ["221", "lora_name"]], sgInputs: [text, { name: "lora_name", type: "*" }], instanceValues: ["t", "b.safetensors"] },
  };
  for (const [name, args] of Object.entries(cases)) {
    const msg = message(proxiedTemplate({ ...args, ...wired }));
    assert.match(msg, wiring, name);
    assert.doesNotMatch(msg, /carries a promoted value/, name);
    assert.doesNotMatch(msg, /read it with I\b/, name);
  }
  // the advice the old order gave would have failed: an I locator on the same wired instance throws "is linked"
  const tpl = proxiedTemplate({ ...cases["positions agree"], ...wired });
  const sg = tpl.definitions.subgraphs[0];
  assert.throws(
    () => templateValue(PROXY_SPEC, PROXY_VIEW, tpl, { key: "k", b: null, t: { kind: "inst", sg: null, id: 238, type: `subgraph:${sg.name}`, input: "lora_name" } }),
    /instance 238 widget lora_name is linked/,
  );
  // without the wiring the same instance still gets the carried-value message
  assert.match(message(proxiedTemplate(cases["positions agree"])), /read it with I \(position 1 is also its place among the widget-typed inputs\)/);
});

test("an instance whose own promoted widget is linked in the root graph fails", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-qwen-image-21");
  const view = spec.views.find((v) => v.name === "t2i");
  const item = view.items.find((i) => i.key === "ksampler.cfg");
  const { syn } = synthManifest();
  const tpl = structuredClone(syn.loadTemplate(view.template));
  plantLink(tpl, item.t, "cfg");
  assert.throws(() => templateValue(spec, view, tpl, item), /wf-qwen-image-21 \[t2i\] ksampler\.cfg: .* cfg is linked \(to node:\S+#0\)/);
});

// How a template serializes the input of a widget that nothing is wired to. None of these shapes is a link.
const UNLINKED_ENTRY_SHAPES = {
  "widget form, link null": (name) => ({ name, type: "INT", widget: { name }, link: null }),
  "name only, link null": (name) => ({ name, type: "INT", link: null }),
  "widget form, link key left out": (name) => ({ name, type: "INT", widget: { name } }),
};

// withUnlinkedEntries: a clone of the synthetic package in which the node each W and I locator points at carries an input
// entry for that widget with no link. Returns { loadTemplate, entries }, the number of entries added.
function withUnlinkedEntries(syn, make) {
  const tpls = new Map();
  let entries = 0;
  for (const spec of BUILDER_SPECS) {
    for (const view of spec.views) {
      if (!tpls.has(view.template)) tpls.set(view.template, structuredClone(syn.loadTemplate(view.template)));
      const tpl = tpls.get(view.template);
      for (const item of view.items) {
        const t = item.t;
        if (t.kind === "distinct" || t.proxied) continue;
        const c = t.kind === "inst" || t.sg == null ? tpl : tpl.definitions.subgraphs.find((s) => s.name === t.sg);
        const node = c.nodes.find((n) => n.id === t.id);
        const name = t.kind === "inst" ? t.input : t.name;
        if (node.inputs.some((i) => (i.widget?.name ?? i.name) === name)) continue;
        node.inputs.push(make(name));
        entries++;
      }
    }
  }
  return { loadTemplate: (name) => tpls.get(name), entries };
}

test("a widget input entry with link null is not a link: every locator of every builder still reads its value", () => {
  const { syn, manifest: m } = synthManifest();
  for (const [shape, make] of Object.entries(UNLINKED_ENTRY_SHAPES)) {
    const pkg = withUnlinkedEntries(syn, make);
    assert.ok(pkg.entries > 100, `${shape}: only ${pkg.entries} entries were added`);
    const r = checkTemplateSide(m, { loadTemplate: pkg.loadTemplate, versions: SYN_VERSIONS });
    assert.deepEqual(r.failures, [], shape);
    assert.deepEqual(r.notes, [], shape);
  }
});

test("an unlinked widget entry that gets a link is refused: the link decides, not the entry", () => {
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-krea2");
  const view = spec.views[0];
  const item = view.items.find((i) => i.key === "ksampler.steps");
  const { syn } = synthManifest();
  const tpl = structuredClone(withUnlinkedEntries(syn, UNLINKED_ENTRY_SHAPES["widget form, link null"]).loadTemplate(view.template));
  assert.equal(templateValue(spec, view, tpl, item), builderValues(spec, view).get(item.key)); // the entry with no link reads
  const c = tpl.definitions.subgraphs.find((s) => s.name === item.t.sg);
  c.nodes.find((n) => n.id === item.t.id).inputs.find((i) => i.name === item.t.name).link = 90001;
  c.links.push({ id: 90001, origin_id: 5000, origin_slot: 0, target_id: item.t.id, target_slot: 0 });
  assert.throws(() => templateValue(spec, view, tpl, item), /wf-krea2 \[default\] ksampler\.steps: .* steps is linked \(to node:\S+#0\)/);
});

test("a hand-edited templateSha256 fails the template side, naming the builder, once every widget is unchanged", () => {
  const { syn, manifest: m } = synthManifest();
  const copy = structuredClone(m);
  copy.builders["wf-krea2"].views.default.templateSha256 = "0".repeat(64);
  const r = checkTemplateSide(copy, { loadTemplate: syn.loadTemplate, versions: SYN_VERSIONS });
  assert.equal(r.failures.length, 1);
  assert.match(r.failures[0].message, /^builder drift: wf-krea2 \[default\]: templateSha256 does not match the recomputed hash \(manifest edited by hand\?\)/);
  // a moved widget is reported as that, not as a hash mismatch
  const moved = structuredClone(m);
  moved.builders["wf-krea2"].views.default.items["ksampler.steps"].template = "0000000000000000";
  moved.builders["wf-krea2"].views.default.templateSha256 = "0".repeat(64);
  const r2 = checkTemplateSide(moved, { loadTemplate: syn.loadTemplate, versions: SYN_VERSIONS });
  assert.equal(r2.failures.length, 1);
  assert.match(r2.failures[0].message, /changed under the builder: widget ksampler\.steps /);
});

test("the CLI check fails on a hand-edited templateSha256 in the manifest file", async (t) => {
  const dir = scratchDir(t, "bd-sha-");
  const tdir = writeSyntheticPackage(dir);
  const manifestFile = join(dir, "m.json");
  const out = [];
  const err = [];
  const io = { out: (s) => out.push(s), err: (s) => err.push(s), env: {} };
  assert.equal(await main(["record", "--templates-dir", tdir, "--manifest", manifestFile], io), 0, err.join(""));
  const edited = JSON.parse(readFileSync(manifestFile, "utf8"));
  edited.builders["wf-krea2"].views.default.templateSha256 = "f".repeat(64);
  writeFileSync(manifestFile, JSON.stringify(edited));
  assert.equal(await main(["check", "--templates-dir", tdir, "--manifest", manifestFile], io), 1);
  assert.match(err.join(""), /builder drift: wf-krea2 \[default\]: templateSha256 does not match/);
});

// fakeTree: a ComfyUI tree that has the package installed (an empty index.json is all the locator looks for).
function fakeTree(dir, name) {
  const tree = join(dir, name);
  const templatesDir = join(tree, ".venv", "Lib", "site-packages", "comfyui_workflow_templates_json", "templates");
  mkdirSync(templatesDir, { recursive: true });
  writeFileSync(join(templatesDir, "index.json"), "[]");
  return { tree, templatesDir: templatesDir.replace(/\\/g, "/") };
}

test("the templates directory is resolved in one order by the CLI and the tests: --templates-dir, --comfy-dir, COMFY_TEMPLATES_DIR, COMFYUI_DIR", async (t) => {
  const dir = scratchDir(t, "bd-resolve-");
  const a = fakeTree(dir, "tree-a");
  const b = fakeTree(dir, "tree-b");
  const env = { COMFY_TEMPLATES_DIR: "direct-dir", COMFYUI_DIR: a.tree };
  assert.equal(resolveTemplatesDir({ "templates-dir": "flag-dir", "comfy-dir": b.tree }, env), "flag-dir");
  assert.equal(resolveTemplatesDir({ "comfy-dir": b.tree }, env), b.templatesDir);
  assert.equal(resolveTemplatesDir({}, env), "direct-dir");
  assert.equal(resolveTemplatesDir({}, { COMFYUI_DIR: a.tree }), a.templatesDir);
  assert.equal(resolveTemplatesDir({}, {}), null);
  // a tree named by the environment that has no package is "not available"; a tree named on the command line is an error
  assert.equal(resolveTemplatesDir({}, { COMFYUI_DIR: join(dir, "no-such-tree") }), null);
  assert.throws(() => resolveTemplatesDir({ "comfy-dir": join(dir, "no-such-tree") }, env), /--comfy-dir .* has no comfyui-workflow-templates-json package/);
  const err = [];
  assert.equal(await main(["check", "--comfy-dir", join(dir, "no-such-tree")], { out: () => {}, err: (s) => err.push(s), env }), 2);
  assert.match(err.join(""), /--comfy-dir .* has no comfyui-workflow-templates-json package/);
});

// The package root (the folder that holds templates/) given where the templates directory belongs used to read as twenty
// "template X is not in the package" failures and "installed unknown", which points at the package, not at the argument.
// It is refused instead, with the path to pass, rather than followed into templates/: the manifest records which package was read.
test("a package root given as the templates directory is refused by check, explain and record; its templates folder is read", async (t) => {
  const dir = scratchDir(t, "bd-root-");
  const tdir = writeSyntheticPackage(dir);
  const root = dirname(tdir);
  writeFileSync(join(root, "__init__.py"), "");
  const want = `${root.replace(/\\/g, "/")}/templates`;
  const manifestFile = join(dir, "m.json");
  const run = async (argv, env = {}) => {
    const out = [];
    const err = [];
    const code = await main(argv, { out: (s) => out.push(s), err: (s) => err.push(s), env });
    return { code, out: out.join(""), err: err.join("") };
  };
  // the templates folder itself is read, through the flag and through the environment
  assert.equal((await run(["record", "--templates-dir", tdir, "--manifest", manifestFile])).code, 0);
  const recorded = readFileSync(manifestFile, "utf8");
  const checked = await run(["check", "--templates-dir", tdir, "--manifest", manifestFile]);
  assert.equal(checked.code, 0, checked.err);
  assert.match(checked.out, /template side checked against 9\.9\.1/);
  assert.equal((await run(["check", "--manifest", manifestFile], { COMFY_TEMPLATES_DIR: tdir })).code, 0);
  assert.equal((await run(["explain", "--templates-dir", tdir])).code, 0);
  // the package root is refused, whichever way it is given, and nothing is read or written
  for (const verb of ["check", "explain", "record"]) {
    for (const [via, argv, env] of [
      ["--templates-dir", ["--templates-dir", root], {}],
      ["COMFY_TEMPLATES_DIR", [], { COMFY_TEMPLATES_DIR: root }],
    ]) {
      const r = await run([verb, ...argv, "--manifest", manifestFile], env);
      assert.equal(r.code, 2, `${verb} via ${via}: ${r.err}`);
      assert.match(r.err, new RegExp(`^${esc(via)} .* is the package root`), `${verb} via ${via}: ${r.err}`);
      assert.ok(r.err.includes(`pass ${want}`), `${verb} via ${via} must name ${want}: ${r.err}`);
      assert.doesNotMatch(r.err, /is not in comfyui-workflow-templates-json|installed unknown/, `${verb} via ${via}`);
      assert.equal(r.out, "", `${verb} via ${via} printed nothing`);
      assert.equal(readFileSync(manifestFile, "utf8"), recorded, `${verb} via ${via} left the manifest as it was`);
    }
  }
});

test("the package-root refusal is narrow: a templates folder, a missing, empty or mixed directory and --comfy-dir are unchanged", (t) => {
  const dir = scratchDir(t, "bd-root-edge-");
  const tdir = writeSyntheticPackage(dir);
  const root = dirname(tdir);
  assert.throws(() => resolveTemplatesDir({ "templates-dir": root }, {}), /^Error: --templates-dir .* is the package root/);
  assert.throws(() => resolveTemplatesDir({}, { COMFY_TEMPLATES_DIR: `${root}/` }), /^Error: COMFY_TEMPLATES_DIR .* is the package root/);
  assert.equal(resolveTemplatesDir({ "templates-dir": tdir }, {}), tdir);
  // a directory that does not exist is read as before (the per-template failures name it)
  const missing = join(dir, "no-such-dir");
  assert.equal(resolveTemplatesDir({ "templates-dir": missing }, {}), missing);
  const empty = join(dir, "empty");
  mkdirSync(empty);
  assert.equal(resolveTemplatesDir({ "templates-dir": empty }, {}), empty);
  // top-level template files decide: a directory that holds some and also a folder called templates is a templates directory
  const mixed = join(dir, "mixed");
  mkdirSync(join(mixed, "templates"), { recursive: true });
  writeFileSync(join(mixed, "index.json"), "[]");
  assert.equal(resolveTemplatesDir({ "templates-dir": mixed }, {}), mixed);
  // a file called templates is not a folder
  const fileNamed = join(dir, "file-named-templates");
  mkdirSync(fileNamed);
  writeFileSync(join(fileNamed, "templates"), "");
  assert.equal(resolveTemplatesDir({ "templates-dir": fileNamed }, {}), fileNamed);
  // --comfy-dir and COMFYUI_DIR still return the templates directory their locator found
  const a = fakeTree(dir, "tree-a");
  assert.equal(resolveTemplatesDir({ "comfy-dir": a.tree }, {}), a.templatesDir);
  assert.equal(resolveTemplatesDir({}, { COMFYUI_DIR: a.tree }), a.templatesDir);
});

// ---------------------------------------------------------------------------------------------
// 2c. What feeds an input is a node, not a node type; the CLI refuses what it cannot read
// ---------------------------------------------------------------------------------------------

// A root-level template whose sink node (id 10) takes inputs a and b from the links given as [origin id, origin slot].
// `origins` are the nodes that feed it (id, type).
function sinkTemplate(origins, fromA, fromB) {
  return {
    nodes: [
      { id: 10, type: "Sink", mode: 0, widgets_values: [], inputs: [{ name: "a", link: 1 }, { name: "b", link: 2 }] },
      ...origins.map(([id, type]) => ({ id, type, mode: 0, widgets_values: [], inputs: [] })),
    ],
    links: [
      { id: 1, origin_id: fromA[0], origin_slot: fromA[1], target_id: 10, target_slot: 0 },
      { id: 2, origin_id: fromB[0], origin_slot: fromB[1], target_id: 10, target_slot: 1 },
    ],
  };
}
const SINK_ITEM = { key: "wiring.a_b_distinct", b: null, t: { kind: "distinct", sg: null, id: 10, type: "Sink", inputs: ["a", "b"] } };
const readDistinct = (tpl) => templateValue({ builder: "wf-x" }, { name: "v", template: "tpl" }, tpl, SINK_ITEM);

test("a wiring check tells nodes apart by id, not by type: two separate nodes of one type are distinct", () => {
  // two PrimitiveFloat nodes, one per input: two sources
  assert.equal(readDistinct(sinkTemplate([[1, "PrimitiveFloat"], [2, "PrimitiveFloat"]], [1, 0], [2, 0])), true);
  // one node feeding both inputs through one slot: one source
  assert.equal(readDistinct(sinkTemplate([[1, "PrimitiveFloat"]], [1, 0], [1, 0])), false);
  // one node, two output slots: two sources
  assert.equal(readDistinct(sinkTemplate([[1, "SplitNode"]], [1, 0], [1, 1])), true);
  // two nodes of different types: two sources
  assert.equal(readDistinct(sinkTemplate([[1, "PrimitiveFloat"], [2, "PrimitiveInt"]], [1, 0], [2, 0])), true);
  // one origin id that exists in no node list is still one source when both links name it
  assert.equal(readDistinct(sinkTemplate([], [77, 0], [77, 0])), false);
  assert.equal(readDistinct(sinkTemplate([], [77, 0], [78, 0])), true);
});

test("a wiring check over an input with no link throws, naming the node and the inputs (either input may be the unlinked one)", () => {
  const unlinkedA = sinkTemplate([[1, "PrimitiveFloat"]], [1, 0], [1, 0]);
  const msg = /wf-x \[v\] wiring\.a_b_distinct: template tpl node 10 has an unlinked input in \[a, b\]/;
  // drop the link from one input at a time: a === undefined, then b === undefined
  for (const name of ["a", "b"]) {
    const tpl = structuredClone(unlinkedA);
    const node = tpl.nodes.find((n) => n.id === 10) ?? tpl.definitions.subgraphs.flatMap((s) => s.nodes).find((n) => n.id === 10);
    node.inputs.find((i) => i.name === name).link = null;
    assert.throws(() => readDistinct(tpl), msg, `input ${name} unlinked`);
  }
  // an input the node does not list at all is unlinked too
  const missing = structuredClone(unlinkedA);
  const sink = missing.nodes.find((n) => n.id === 10) ?? missing.definitions.subgraphs.flatMap((s) => s.nodes).find((n) => n.id === 10);
  sink.inputs = sink.inputs.filter((i) => i.name !== "b");
  assert.throws(() => readDistinct(missing), msg);
  // control: as built, it reads
  assert.equal(readDistinct(unlinkedA), false);
});

test("a linked widget's message names the feeding node by type and id", () => {
  const tpl = {
    nodes: [
      { id: 4, type: "KSampler", mode: 0, widgets_values: [0, "fixed", 20], inputs: [{ name: "steps", type: "INT", widget: { name: "steps" }, link: 9 }] },
      { id: 7, type: "PrimitiveInt", mode: 0, widgets_values: [30], inputs: [] },
    ],
    links: [{ id: 9, origin_id: 7, origin_slot: 0, target_id: 4, target_slot: 0 }],
  };
  const item = { key: "ksampler.steps", b: null, t: { kind: "w", sg: null, id: 4, type: "KSampler", w: 2, name: "steps" } };
  assert.throws(() => templateValue({ builder: "wf-x" }, { name: "v", template: "tpl" }, tpl, item), /steps is linked \(to node:PrimitiveInt\/7#0\)/);
});

// cli: run the CLI main with captured output.
async function cli(argv, env = {}) {
  const out = [];
  const err = [];
  const code = await main(argv, { out: (s) => out.push(s), err: (s) => err.push(s), env });
  return { code, out: out.join(""), err: err.join("") };
}

test("a flag that needs a value and has none is refused with exit 2, never read as a flag that is on", async (t) => {
  const dir = scratchDir(t, "bd-flags-");
  const tdir = writeSyntheticPackage(dir);
  const manifestFile = join(dir, "m.json");
  assert.equal((await cli(["record", "--templates-dir", tdir, "--manifest", manifestFile])).code, 0);
  // an unset shell variable arrives as an empty argument (or as no argument at all)
  for (const flag of ["--templates-dir", "--comfy-dir", "--manifest"]) {
    const noun = flag.slice(2);
    for (const argv of [["check", flag, ""], ["check", flag, "   "], ["check", flag], ["check", flag, "--require-templates"]]) {
      const r = await cli(argv, {});
      assert.equal(r.code, 2, `${argv.map((a) => JSON.stringify(a)).join(" ")} -> ${r.out}${r.err}`);
      assert.match(r.err, new RegExp(`--${noun} needs a non-empty value`));
      assert.doesNotMatch(r.out, /^ok:|template side skipped/m);
    }
  }
  // the same flags with a value still work
  assert.equal((await cli(["check", "--templates-dir", tdir, "--manifest", manifestFile])).code, 0);
  assert.equal((await cli(["check", `--templates-dir=${tdir}`, `--manifest=${manifestFile}`])).code, 0);
  // a flag the CLI does not know is refused too: a typo must not skip a side
  const typo = await cli(["check", "--template-dir", tdir]);
  assert.equal(typo.code, 2);
  assert.match(typo.err, /unknown flag --template-dir/);
});

test("--require-templates makes a missing template side an error; --strict and --require-templates belong to check", async (t) => {
  const dir = scratchDir(t, "bd-require-");
  const tdir = writeSyntheticPackage(dir);
  const manifestFile = join(dir, "m.json");
  assert.equal((await cli(["record", "--templates-dir", tdir, "--manifest", manifestFile])).code, 0);
  // no source at all: the default run skips the side and passes, the required run is an error
  const skipped = await cli(["check", "--manifest", manifestFile]);
  assert.equal(skipped.code, 0);
  assert.match(skipped.out, /template side skipped/);
  const required = await cli(["check", "--manifest", manifestFile, "--require-templates"]);
  assert.equal(required.code, 2);
  assert.match(required.err, /--require-templates: no templates directory/);
  assert.doesNotMatch(required.out, /^ok:/m);
  // a ComfyUI tree named by the environment that has no package is "none" as well
  const emptyTree = join(dir, "no-such-tree");
  const envNone = await cli(["check", "--manifest", manifestFile, "--require-templates"], { COMFYUI_DIR: emptyTree });
  assert.equal(envNone.code, 2);
  // with a source the flag changes nothing
  const withSource = await cli(["check", "--manifest", manifestFile, "--templates-dir", tdir, "--require-templates"]);
  assert.equal(withSource.code, 0, withSource.err);
  assert.match(withSource.out, /template side checked against 9\.9\.1/);
  // the flags are for check only
  for (const flag of ["--strict", "--require-templates"]) {
    for (const verb of ["explain", "record"]) {
      const r = await cli([verb, "--templates-dir", tdir, "--manifest", join(dir, "other.json"), flag]);
      assert.equal(r.code, 2, `${verb} ${flag}`);
      assert.match(r.err, new RegExp(`${flag} belongs to check`));
    }
  }
});

test("check --strict fails on unreviewed entries and only on those", async () => {
  const plain = await cli(["check"]);
  assert.equal(plain.code, 0, plain.err);
  const strict = await cli(["check", "--strict"]);
  assert.equal(strict.code, 1);
  const lines = strict.err.trim().split("\n");
  assert.equal(lines.length, 2, strict.err);
  assert.match(strict.err, /^builder drift: wf-qwen-image-21 \[edit\]: widget encoder\.resolution is unreviewed/m);
  assert.match(strict.err, /^builder drift: wf-qwen-image-edit \[full\]: widget ksampler\.cfg is unreviewed/m);
  assert.match(strict.out, /^2 failure\(s\)$/m);
  // the unreviewed entries of the committed manifest are exactly these two (a deliberate entry is a decision, not a debt)
  const unreviewed = [];
  const deliberate = [];
  for (const [b, e] of Object.entries(manifest.builders)) for (const [v, r] of Object.entries(e.views)) for (const [k, it] of Object.entries(r.items)) {
    if (it.status === "unreviewed") unreviewed.push(`${b} [${v}] ${k}`);
    if (it.status === "deliberate") deliberate.push(`${b} [${v}] ${k}`);
  }
  assert.deepEqual(unreviewed.sort(), ["wf-qwen-image-21 [edit] encoder.resolution", "wf-qwen-image-edit [full] ksampler.cfg"]);
  assert.equal(deliberate.length, 7);
});

test("a single-dash argument and an extra positional are refused with exit 2, never ignored", async (t) => {
  const dir = scratchDir(t, "bd-dash-");
  const tdir = writeSyntheticPackage(dir);
  const manifestFile = join(dir, "m.json");
  assert.equal((await cli(["record", "--templates-dir", tdir, "--manifest", manifestFile])).code, 0);
  // `-templates-dir DIR` is a typo of the flag: it must not leave the template side skipped
  for (const argv of [
    ["check", "-templates-dir", tdir, "--manifest", manifestFile],
    ["check", "-templates-dir=" + tdir, "--manifest", manifestFile],
    ["check", "-x", "--manifest", manifestFile],
    ["-x", "check", "--manifest", manifestFile],
    ["check", "-", "--manifest", manifestFile],
    ["check", "--manifest", manifestFile, "-s"],
  ]) {
    const r = await cli(argv, {});
    assert.equal(r.code, 2, `${argv.join(" ")} -> ${r.out}${r.err}`);
    assert.match(r.err, /unknown flag -/);
    assert.doesNotMatch(r.out, /^ok:|template side skipped/m);
  }
  // a flag's value is never a dash-led word: `--manifest -x` has no value
  const dashValue = await cli(["check", "--manifest", "-x"]);
  assert.equal(dashValue.code, 2);
  assert.match(dashValue.err, /--manifest needs a non-empty value/);
  // a second positional after the verb is refused, whichever verb it follows
  for (const argv of [["check", "extra"], ["check", "extra", "--manifest", manifestFile], ["check", "check"], ["explain", "extra", "--templates-dir", tdir], ["record", "extra", "--templates-dir", tdir, "--manifest", join(dir, "o.json")]]) {
    const r = await cli(argv, {});
    assert.equal(r.code, 2, `${argv.join(" ")} -> ${r.out}${r.err}`);
    assert.match(r.err, /unexpected argument extra|unexpected argument check/);
    assert.doesNotMatch(r.out, /^ok:|^wrote /m);
  }
  // the known forms still work: no verb is check, a verb alone is itself
  assert.equal((await cli(["--manifest", manifestFile, "--templates-dir", tdir])).code, 0);
  assert.equal((await cli(["check", "--manifest", manifestFile, "--templates-dir", tdir])).code, 0);
});

test("the `=` form of a value flag is refused when empty, and a switch refuses any value", async (t) => {
  const dir = scratchDir(t, "bd-eq-");
  const tdir = writeSyntheticPackage(dir);
  const manifestFile = join(dir, "m.json");
  assert.equal((await cli(["record", "--templates-dir", tdir, "--manifest", manifestFile])).code, 0);
  // an unset variable in the `--flag="$UNSET"` shape arrives as `--flag=`
  for (const flag of ["templates-dir", "comfy-dir", "manifest"]) {
    for (const arg of [`--${flag}=`, `--${flag}=   `]) {
      const r = await cli(["check", arg], {});
      assert.equal(r.code, 2, `${JSON.stringify(arg)} -> ${r.out}${r.err}`);
      assert.match(r.err, new RegExp(`--${flag} needs a non-empty value`));
      assert.doesNotMatch(r.out, /^ok:|template side skipped/m);
    }
  }
  assert.equal((await cli(["check", "--templates-dir=", "--manifest", manifestFile], { COMFY_TEMPLATES_DIR: tdir })).code, 2);
  // a switch takes no value: `--strict=1` is not "on", and not "off" either
  for (const arg of ["--strict=1", "--strict=", "--strict=0", "--require-templates=1", "--require-templates="]) {
    const r = await cli(["check", "--manifest", manifestFile, "--templates-dir", tdir, arg], {});
    assert.equal(r.code, 2, `${arg} -> ${r.out}${r.err}`);
    assert.match(r.err, /takes no value/);
    assert.doesNotMatch(r.out, /^ok:/m);
  }
});

test("the runbook names the expected failures of check --strict: the docs and the README list exactly the unreviewed entries of the manifest", async () => {
  const strict = await cli(["check", "--strict"]);
  assert.equal(strict.code, 1);
  const count = (strict.out.match(/^(\d+) failure\(s\)$/m) ?? [])[1];
  assert.equal(count, "2");
  const entries = strict.err.trim().split("\n").map((line) => {
    const m = line.match(/^builder drift: (\S+) \[(\w+)\]: widget (\S+) is unreviewed/);
    assert.ok(m, line);
    return { builder: m[1], key: m[3] };
  });
  const readme = readFileSync(new URL("./README.md", import.meta.url), "utf8");
  const page = readFileSync(new URL("../docs/systems/media-generation.md", import.meta.url), "utf8");
  for (const [name, raw] of [["render/README.md", readme], ["the system page", page]]) {
    const text = raw.replace(/\s+/g, " ");
    assert.ok(text.includes(`exactly \`${count} failure(s)\``), `${name} states the failure count ${count}`);
    for (const e of entries) assert.ok(text.includes(e.builder) && text.includes(e.key), `${name} names ${e.builder} ${e.key}`);
  }
});

test("check --strict quotes the reason recorded for each unreviewed entry", async () => {
  const strict = await cli(["check", "--strict"]);
  assert.equal(strict.code, 1);
  assert.match(strict.err, /^builder drift: wf-qwen-image-edit \[full\]: widget ksampler\.cfg is unreviewed: .*\(on record: the template's non-LoRA CFG primitive is 4; the builder preset is 3\.0, which is the value of the KSampler's own widget \(stale, because the input is linked to the switch\)\.\)$/m);
  assert.match(strict.err, /^builder drift: wf-qwen-image-21 \[edit\]: widget encoder\.resolution is unreviewed: .*\(on record: the edit template promotes resolution 0 \(each reference keeps its own size\); the builder defaults to 1024, which its header calls the official edit default\.\)$/m);
  // the reason shown is the spec's, entry by entry
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-qwen-image-edit");
  const reason = spec.views.find((v) => v.name === "full").items.find((i) => i.key === "ksampler.cfg").delta.reason;
  assert.ok(strict.err.includes(`(on record: ${reason})`));
});

test("a wiring check keeps (node 1, slot 2) and (node 2, slot 1) apart, and its key reads node, then slot", async () => {
  const { feedSource } = await import("./builder-drift.mjs");
  // the two sources are distinct: the id and the slot are not interchangeable
  assert.equal(readDistinct(sinkTemplate([[1, "Split"], [2, "Split"]], [1, 2], [2, 1])), true);
  assert.equal(readDistinct(sinkTemplate([[1, "Split"], [2, "Split"]], [2, 1], [1, 2])), true);
  // ... while the same pair twice is one source
  assert.equal(readDistinct(sinkTemplate([[1, "Split"], [2, "Split"]], [2, 1], [2, 1])), false);
  // the key itself: node id first, output slot second; an origin that exists in no node list keeps the same order in its label
  const tpl = sinkTemplate([[1, "Split"]], [1, 2], [77, 5]);
  const root = { nodes: tpl.nodes, links: tpl.links, inputs: [] };
  const sink = tpl.nodes[0];
  assert.deepEqual(feedSource(root, sink, "a"), { key: "node:1#2", label: "node:Split/1#2" });
  assert.deepEqual(feedSource(root, sink, "b"), { key: "node:77#5", label: "node:77#5" });
});

// withTemplateFile: the synthetic package with one template file replaced by `text`.
function packageWith(dir, template, text) {
  const tdir = writeSyntheticPackage(dir);
  writeFileSync(join(tdir, `${template}.json`), text);
  return tdir;
}

test("a template file with a byte-order mark reads; a null file, a file without nodes and invalid JSON fail naming the builder", async (t) => {
  const dir = scratchDir(t, "bd-bom-");
  const good = writeSyntheticPackage(join(dir, "good"));
  const manifestFile = join(dir, "m.json");
  assert.equal((await cli(["record", "--templates-dir", good, "--manifest", manifestFile])).code, 0);
  const text = readFileSync(join(good, "image_krea2_turbo_t2i.json"), "utf8");
  const bom = await cli(["check", "--templates-dir", packageWith(join(dir, "bom"), "image_krea2_turbo_t2i", `﻿${text}`), "--manifest", manifestFile]);
  assert.equal(bom.code, 0, bom.err);
  const cases = { "null": /no nodes array/, "{}": /no nodes array/, "[]": /no nodes array/, "{ \"nodes\": [": /is not valid JSON/, "": /is not valid JSON/ };
  let n = 0;
  for (const [body, want] of Object.entries(cases)) {
    const r = await cli(["check", "--templates-dir", packageWith(join(dir, `bad${n++}`), "image_krea2_turbo_t2i", body), "--manifest", manifestFile]);
    assert.equal(r.code, 1, `${JSON.stringify(body)}: ${r.err}`);
    assert.match(r.err, /^builder drift: wf-krea2 \[default\]: /m);
    assert.match(r.err, /image_krea2_turbo_t2i/);
    assert.match(r.err, want, JSON.stringify(body));
    assert.doesNotMatch(r.err, /Cannot read propert|is not iterable/);
  }
});

test("a manifest file with a byte-order mark reads; a null or unparsable manifest is refused with exit 2", async (t) => {
  const dir = scratchDir(t, "bd-mbom-");
  const withBom = join(dir, "bom.json");
  writeFileSync(withBom, `﻿${formatManifest(manifest)}`);
  assert.equal((await cli(["check", "--manifest", withBom])).code, 0);
  for (const body of ["null", "[]", "{}", "{ nope", ""]) {
    const file = join(dir, "bad.json");
    writeFileSync(file, body);
    const r = await cli(["check", "--manifest", file]);
    assert.equal(r.code, 2, `${JSON.stringify(body)}: ${r.out}${r.err}`);
    assert.match(r.err, /manifest/);
  }
  const missing = await cli(["check", "--manifest", join(dir, "absent.json")]);
  assert.equal(missing.code, 2);
});

// ---------------------------------------------------------------------------------------------
// 2d. The branches the mutation runs found unproven
// ---------------------------------------------------------------------------------------------

test("a builder graph node of another class than the spec names fails the builder side, naming the builder", () => {
  const { manifest: baseline } = synthManifest();
  const spec = BUILDER_SPECS.find((s) => s.builder === "wf-krea2");
  const item = spec.views[0].items.find((i) => i.key === "ksampler.steps");
  const moved = { ...spec, build: (args) => { const g = spec.build(args); g[item.b.node].class_type = "KSamplerAdvanced"; return g; } };
  const failures = checkBuilderSide(baseline, { specs: [moved] });
  assert.equal(failures.length, 1, messages(failures));
  assert.match(failures[0].message, /^builder drift: wf-krea2 \[default\]: ksampler\.steps: node 9 is KSamplerAdvanced, the spec expects KSampler$/);
});

test("a node id is looked up in the subgraph the spec names, not in any subgraph that has it", () => {
  const sub = (name, steps) => ({ id: `id-${name}`, name, inputs: [], links: [], nodes: [{ id: 3, type: "KSampler", mode: 0, widgets_values: [0, "fixed", steps], inputs: [] }] });
  const item = { key: "ksampler.steps", b: null, t: { kind: "w", sg: "Wanted", id: 3, type: "KSampler", w: 2, name: "steps" } };
  const read = (tpl) => templateValue({ builder: "wf-x" }, { name: "v", template: "tpl" }, tpl, item);
  // node 3 exists only in another subgraph: the named one has none
  assert.throws(() => read({ nodes: [], links: [], definitions: { subgraphs: [sub("Other", 99)] } }), /wf-x \[v\] ksampler\.steps: template tpl has no node 3 in subgraph "Wanted"/);
  // node 3 exists in both: the named subgraph's value is read, wherever it sits in the list
  assert.equal(read({ nodes: [], links: [], definitions: { subgraphs: [sub("Other", 99), sub("Wanted", 20)] } }), 20);
  assert.equal(read({ nodes: [], links: [], definitions: { subgraphs: [sub("Wanted", 20), sub("Other", 99)] } }), 20);
});

// ---------------------------------------------------------------------------------------------
// 3. The manifest against the real workflow-templates package
// ---------------------------------------------------------------------------------------------

// The same resolver the CLI uses: COMFY_TEMPLATES_DIR, else the package inside COMFYUI_DIR.
const REAL = resolveTemplatesDir({}, process.env);

if (!REAL) {
  test("template side against the real workflow-templates package", { skip: "set COMFY_TEMPLATES_DIR (the package's templates directory) or COMFYUI_DIR (a ComfyUI tree with it installed) to compare the manifest with the real templates" }, () => {});
} else {
  const source = templateSource(REAL);
  for (const spec of BUILDER_SPECS) {
    test(`builder drift (real templates): ${spec.builder}`, (t) => {
      const r = checkTemplateSide(manifest, { ...source, specs: [spec] });
      for (const n of r.notes) t.diagnostic(n);
      assert.equal(r.failures.length, 0, `${spec.builder} was copied from a template that has changed:\n${messages(r.failures)}`);
    });
  }
}

test("the manifest path is the committed testdata file", () => {
  assert.match(MANIFEST_PATH.replace(/\\/g, "/"), /\/render\/testdata\/builder-drift\.json$/);
});
