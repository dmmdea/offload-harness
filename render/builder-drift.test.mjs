// node --test render/builder-drift.test.mjs
//
// The builder drift guard (ENH-4, render/builder-drift.mjs). Three layers:
//
//   1. The committed manifest against the builders, offline: every guarded builder is rebuilt with the spec's fixed
//      arguments and each recipe widget of its output graph is compared with render/testdata/builder-drift.json. One
//      test per builder, so a red run names the builder in the test title and in the failure message.
//   2. The machinery against synthetic templates (written here from the specs, no upstream data): a widget moved in
//      the builder's graph, a widget moved in the template, a node that vanished from the template and the wiring
//      defect of upstream PR #1308 each fail naming the builder. Every recipe widget of every builder is mutated once
//      on each side.
//   3. The manifest against the real workflow-templates package, when one is at hand (COMFY_TEMPLATES_DIR is its
//      `templates` directory, or COMFYUI_DIR a ComfyUI tree that has it installed). Skipped, with the reason,
//      otherwise: the package is not in this repository and none of it is copied here.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  buildManifest, builderValues, canonical, checkBuilderSide, checkCoverage, checkManifestShape, checkTemplateSide, formatManifest,
  itemDigest, loadManifest, main, recipeSha256, templateSource, templateValue, MANIFEST_PATH,
} from "./builder-drift.mjs";
import { BUILDER_SPECS, NOT_TEMPLATE_DERIVED } from "./builder-drift-specs.mjs";
import { locateTemplates, nodeIo } from "./templates-catalog.mjs";
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
  // names listed but absent from render/ fail too
  assert.match(text, /builder drift: wf-krea2: is named in builder-drift-specs\.mjs but render\/ has no such builder|^$/m.test(text) ? /wf-krea2/ : /wf-new-thing/);
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
// `override` maps a template name + locator to the value to plant instead.
function synthTemplates({ override = new Map(), specs = BUILDER_SPECS } = {}) {
  const byName = new Map();
  const planted = new Map();
  const container = (tpl, sgName, state) => {
    if (sgName == null) return tpl;
    let s = state.sgs.get(sgName);
    if (!s) {
      s = { id: `sg-${state.sgs.size}`, name: sgName, nodes: [], links: [], inputs: [] };
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
          put(nodeIn(container(tpl, t.sg, state), t.id, t.type).widgets_values, t.w, v, tag);
        } else if (t.kind === "inst") {
          const sgName = t.type.slice("subgraph:".length);
          const s = container(tpl, sgName, state);
          const inst = nodeIn(tpl, t.id, s.id);
          let idx = s.inputs.findIndex((i) => i.name === t.input);
          if (idx < 0) { s.inputs.push({ name: t.input, type: "INT" }); idx = s.inputs.length - 1; }
          put(inst.widgets_values, idx, v, tag);
        } else if (t.kind === "distinct") {
          const s = container(tpl, t.sg, state);
          const n = nodeIn(s, t.id, t.type);
          t.inputs.forEach((name, k) => {
            if (n.inputs.some((i) => i.name === name)) return;
            n.inputs.push({ name, link: 100 + k });
            s.inputs.push({ name: `in${k}`, type: "FLOAT" });
            s.links.push({ id: 100 + k, origin_id: -10, origin_slot: v === false ? 0 : k, target_id: t.id, target_slot: k });
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
  for (const l of sg.links) if (l.target_id === item.t.id) l.origin_slot = 0; // both inputs now come from one subgraph input
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

test("the CLI reads a synthetic templates directory: explain lists the widgets, record writes only the manifest", async (t) => {
  const dir = scratchDir(t, "bd-pkg-");
  const syn = synthTemplates();
  const tdir = join(dir, "site-packages", "comfyui_workflow_templates_json", "templates");
  mkdirSync(tdir, { recursive: true });
  mkdirSync(join(dir, "site-packages", "comfyui_workflow_templates_json-9.9.1.dist-info"));
  writeFileSync(join(dir, "site-packages", "comfyui_workflow_templates-9.0.0.dist-info"), "");
  for (const name of syn.templates.keys()) writeFileSync(join(tdir, `${name}.json`), JSON.stringify(syn.loadTemplate(name)));
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
// 3. The manifest against the real workflow-templates package
// ---------------------------------------------------------------------------------------------

function realTemplatesDir() {
  const direct = process.env.COMFY_TEMPLATES_DIR;
  if (direct) return direct;
  const tree = process.env.COMFYUI_DIR;
  return tree ? locateTemplates(tree, nodeIo)?.templatesDir ?? null : null;
}
const REAL = realTemplatesDir();

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
