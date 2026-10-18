// render/template-manifest-fixture.mjs - an in-memory ComfyUI tree for the run-template tests. Test support only; never imported by a
// runner. Nothing here touches the disk, the network or a GPU: `fixtureIo()` is a filesystem with the same surface as
// templates-catalog.mjs's nodeIo, holding a small installed templates package, a comfy_api_nodes stand-in, a few model files and
// one input file, and `LICENSE_MAP` is the licence map the templates are classified with.
//
// Every repository id is `example-org/...`: the entries are not evidence about any repo.
import { createHash } from "node:crypto";

export const COMFY = "/srv/comfy";
export const SITE = `${COMFY}/.venv/lib/python3.13/site-packages`;
export const TEMPLATES = `${SITE}/comfyui_workflow_templates_json/templates`;
export const MAP_PATH = "/srv/fixture/license-map.json";
export const SHA_DECLARED = "ab".repeat(32);

export const LICENSE_MAP = {
  schema_version: 1,
  repos: {
    "example-org/weights-a": { class: "permissive", license_id: "apache-2.0", source_url: "https://example.com/a", fetched_on: "2026-01-01", basis: "hf-card-tag" },
    "example-org/weights-b": { class: "conditional", license_id: "other", conditions: "Revenue above a threshold needs a paid agreement.", source_url: "https://example.com/b", fetched_on: "2026-01-01", basis: "hf-card-tag" },
    "example-org/weights-c": { class: "non_commercial", license_id: "cc-by-nc-4.0", conditions: "Non-commercial use only.", source_url: "https://example.com/c", fetched_on: "2026-01-01", basis: "hf-card-tag" },
    "example-org/weights-inferred-ok": { class: "permissive", license_id: "apache-2.0", source_url: "https://example.com/i", fetched_on: "2026-01-01", basis: "inferred from example-org/weights-a (not fetched)" },
  },
};

const hf = (repo, file) => `https://huggingface.co/${repo}/resolve/main/${file}`;
const annotated = (name, directory, repo, extra = {}) => ({ name, directory, url: repo ? hf(repo, name) : null, ...extra });
const loader = (id, type, file, models, extra = {}) => ({
  id, type, mode: 0, widgets_values: [file, "default"], properties: { cnr_id: "comfy-core", ...(models ? { models } : {}) }, ...extra,
});
const workflow = (...nodes) => JSON.stringify({ version: 0.4, nodes });
const unet = (repo, name = "a.safetensors") => loader(1, "UNETLoader", name, [annotated(name, "diffusion_models", repo)]);
const save = () => ({ id: 90, type: "SaveImage", mode: 0, widgets_values: ["out"], properties: { cnr_id: "comfy-core" } });

// name -> { entry: the index entry, wf: the workflow file text, or null for a listed template whose file is missing }
export const TEMPLATE_SET = {
  tt_open: {
    entry: { title: "Open: text to image", tags: ["Image", "Text to Image"], models: ["A"] },
    wf: workflow(unet("example-org/weights-a"), loader(2, "VAELoader", "v.safetensors", [annotated("v.safetensors", "vae", "example-org/weights-a", { hash: SHA_DECLARED, hash_type: "SHA256" })]), save()),
  },
  tt_input: {
    entry: { title: "Input: image to image", tags: ["Image", "Image to Image"], models: ["A"] },
    wf: workflow(
      unet("example-org/weights-a"),
      { id: 3, type: "LoadImage", mode: 0, widgets_values: ["ref.png", "image"], properties: { cnr_id: "comfy-core" } },
      { id: 4, type: "LoadImage", mode: 2, widgets_values: ["muted.png", "image"], properties: { cnr_id: "comfy-core" } },
      { id: 5, type: "LoadImageMask", mode: 4, widgets_values: ["bypassed.png", "alpha", "image"], properties: { cnr_id: "comfy-core" } },
      save(),
    ),
  },
  tt_input_gone: {
    entry: { title: "Input: the file is not staged", tags: ["Image"], models: ["A"] },
    wf: workflow(unet("example-org/weights-a"), { id: 3, type: "LoadImage", mode: 0, widgets_values: ["nowhere.png", "image"], properties: { cnr_id: "comfy-core" } }, save()),
  },
  tt_zero: { entry: { title: "Zero model: stitch", tags: ["Utility"], models: ["None"] }, wf: workflow(save()) },
  api_tt_paid: {
    entry: { title: "Paid: partner node", tags: ["API"], models: ["None"], openSource: false },
    wf: workflow({ id: 1, type: "ApiPaidNode", mode: 0, widgets_values: [], properties: { cnr_id: "comfy-core" } }, save()),
  },
  tt_unlisted: { entry: { title: "Unlisted licence", tags: ["Image"], models: ["X"] }, wf: workflow(unet("example-org/not-in-the-map"), save()) },
  tt_unresolved: {
    entry: { title: "A weights file with no Hugging Face source", tags: ["Image"], models: ["X"] },
    wf: workflow(loader(1, "VAELoader", "from-nowhere.safetensors", [annotated("from-nowhere.safetensors", "vae", null)]), save()),
  },
  tt_inferred: { entry: { title: "Permissive by inference only", tags: ["Image"], models: ["X"] }, wf: workflow(unet("example-org/weights-inferred-ok"), save()) },
  tt_cond: { entry: { title: "Conditional licence", tags: ["Image"], models: ["B"] }, wf: workflow(unet("example-org/weights-b", "b.safetensors"), save()) },
  tt_nc: { entry: { title: "Non-commercial licence", tags: ["Image"], models: ["C"] }, wf: workflow(unet("example-org/weights-c", "c.safetensors"), save()) },
  tt_pack: {
    entry: { title: "Needs a custom node pack", tags: ["Image"], models: ["None"] },
    wf: workflow({ id: 1, type: "PackNode", mode: 0, widgets_values: [], properties: { cnr_id: "example-pack" } }, save()),
  },
  tt_flux_pic: { entry: { title: "FLUX by name", tags: ["Image"], models: ["A"] }, wf: workflow(unet("example-org/weights-a"), save()) },
  tt_old: { entry: { title: "Needs a newer ComfyUI", tags: ["Image"], models: ["A"], minComfyUIVersion: "99.0.0" }, wf: workflow(unet("example-org/weights-a"), save()) },
  tt_broken: { entry: { title: "Listed, no file", tags: ["Image"], models: ["None"] }, wf: null },
};

export function indexJson() {
  const templates = Object.entries(TEMPLATE_SET).map(([name, t]) => ({ name, mediaType: "image", mediaSubtype: "webp", openSource: true, ...t.entry }));
  return JSON.stringify([{ moduleName: "default", category: "Fixture", title: "Fixture", type: "image", templates }]);
}

// A filesystem with the surface of templates-catalog.mjs's nodeIo. `files` maps a path to its text, or to { size } for a file whose
// content nobody reads; directories are implied by their children. `writes` records every writeText.
export function memIo(files = {}, { env = {}, home = "/srv/fixture-home" } = {}) {
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
        const [head, ...tail] = f.slice(base.length + 1).split("/");
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

// A ComfyUI tree: the installed templates package, the node's own API node ids, ComfyUI's version, the model files `models` names
// (path under models/ -> true) and the input files `inputs` names. `extra` adds or replaces files.
export function fixtureFiles({ models = ["diffusion_models/a.safetensors", "vae/v.safetensors", "diffusion_models/b.safetensors", "diffusion_models/c.safetensors"], inputs = ["ref.png"], version = "0.37.0", extra = {} } = {}) {
  const files = {
    [`${COMFY}/comfyui_version.py`]: `__version__ = "${version}"\n`,
    [`${SITE}/comfyui_workflow_templates_json-0.1.0.dist-info/METADATA`]: "Metadata-Version: 2.1\nName: comfyui-workflow-templates-json\nVersion: 0.1.0\n",
    [`${TEMPLATES}/index.json`]: indexJson(),
    [`${COMFY}/comfy_api_nodes/nodes_fixture.py`]: 'class N:\n    schema = IO.Schema(node_id="ApiPaidNode")\n',
    [MAP_PATH]: JSON.stringify(LICENSE_MAP),
  };
  for (const [name, t] of Object.entries(TEMPLATE_SET)) if (t.wf !== null) files[`${TEMPLATES}/${name}.json`] = t.wf;
  for (const m of models) files[`${COMFY}/models/${m}`] = { size: 1024 };
  for (const f of inputs) files[`${COMFY}/input/${f}`] = { size: 64 };
  return { ...files, ...extra };
}

export function fixtureIo(opts) {
  return memIo(fixtureFiles(opts));
}
