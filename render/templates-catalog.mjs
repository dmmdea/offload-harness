// render/templates-catalog.mjs — a read-only catalog of the ComfyUI workflow templates a node
// carries: the `comfyui-workflow-templates` package ComfyUI itself pins, a checkout of the upstream
// repository, or a `pip download` extract of a candidate version.
//
// Phase A of the Comfy templates work. It LISTS and CLASSIFIES: which templates exist, which
// are paid partner-node templates, which need custom node packs, which model files each one
// needs, and which parameters it exposes. It never runs a template, downloads a model, installs
// a package, changes a config or touches the network; the one thing it writes is the file named by
// --out. Dependency-free (Node 18+, `node:` builtins only) and self-contained on purpose: the
// whole file can be piped to `node --input-type=module - snapshot --comfy-dir DIR` on any node to
// take a snapshot there, with nothing deployed. A piped file has no licence map beside it, so the
// other verbs count every repo as unknown unless they are given --license-map FILE.
//
// Every count it reports names the package it was computed on (`stamp.label`): the installed
// package, a candidate extract and the upstream repository's HEAD carry different numbers, and a
// figure without its basis is not a figure.
import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync, realpathSync, statSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";

// A file piped to `node --input-type=module -` has no location: node names its module `[eval1]` in the working directory and puts
// "-" in process.argv[1]. Both facts are needed to tell it from a script piped the same way that only imports this file, whose
// argv[1] is "-" too.
const NO_FILE = /\/\[eval\d+\]$/.test(import.meta.url);

// ---------------------------------------------------------------------------------------------
// Paths (logical, forward-slash; Node accepts them on Windows too)
// ---------------------------------------------------------------------------------------------

const slash = (p) => String(p).replace(/\\/g, "/");

export function isAbsPath(p) {
  return /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("/") || p.startsWith("\\\\");
}

// normalizePath: forward slashes, no duplicate separators, `.` and `..` resolved. Pure string work,
// so a Windows path can be reasoned about on a POSIX host (the tests do) and the reverse. A UNC
// path keeps its two leading slashes and its `//server/share` root (`..` does not climb out of
// it), as Python's os.path.normpath, which ComfyUI uses, does; three or more slashes are one.
export function normalizePath(p) {
  let s = slash(p);
  let prefix = "";
  const drive = /^([A-Za-z]:)(?=\/|$)/.exec(s);
  const unc = drive ? null : /^\/\/[^/]+(?:\/[^/]+)?/.exec(s);
  if (drive) { prefix = drive[1]; s = s.slice(2); }
  else if (unc) { prefix = unc[0]; s = s.slice(prefix.length); }
  const abs = s.startsWith("/") || prefix !== "";
  const out = [];
  for (const seg of s.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (out.length && out[out.length - 1] !== "..") out.pop();
      else if (!abs) out.push("..");
      continue;
    }
    out.push(seg);
  }
  const body = out.join("/");
  if (unc) return body ? `${prefix}/${body}` : prefix;
  return prefix + (abs ? "/" : "") + body;
}

// joinPath: the parts with one separator between them, normalized. A part that already ends in a
// separator (the root "/", "D:/x/") gets none added: "/" and "x" through a plain "/" made "//x",
// which normalizePath reads as a network share named x.
export function joinPath(...parts) {
  let joined = "";
  for (const part of parts) {
    if (part == null) continue;
    const p = slash(part);
    joined = joined === "" ? p : `${joined}${joined.endsWith("/") ? "" : "/"}${p}`;
  }
  return normalizePath(joined);
}

const baseName = (p) => slash(p).split("/").pop();
const dirName = (p) => { const n = normalizePath(p); const i = n.lastIndexOf("/"); return i <= 0 ? (i === 0 ? "/" : ".") : n.slice(0, i); };

// ---------------------------------------------------------------------------------------------
// The filesystem, injected. Every function below takes an `io` so a test (or a remote snapshot)
// can hand it any tree; nodeIo is the real one.
// ---------------------------------------------------------------------------------------------

export const nodeIo = {
  env: process.env,
  home: homedir(),
  exists: (p) => existsSync(p),
  readText: (p) => readFileSync(p, "utf8").replace(/^﻿/, ""),
  sha256: (p) => createHash("sha256").update(readFileSync(p)).digest("hex"),
  // list: the entries of a directory, or null when it cannot be read. A symlink or junction is
  // resolved to what it points at, so a walker follows links the way ComfyUI's own does.
  list: (p) => {
    let entries;
    try { entries = readdirSync(p, { withFileTypes: true }); } catch { return null; }
    return entries.map((d) => {
      let isDir = d.isDirectory();
      let isFile = d.isFile();
      const isSymlink = d.isSymbolicLink();
      if (isSymlink) {
        try { const st = statSync(`${slash(p)}/${d.name}`); isDir = st.isDirectory(); isFile = st.isFile(); } catch { isDir = false; isFile = false; }
      }
      return { name: d.name, isDir, isFile, isSymlink };
    });
  },
  stat: (p) => {
    try { const st = statSync(p); return { size: st.size, isDir: st.isDirectory(), isFile: st.isFile() }; } catch { return null; }
  },
  realpath: (p) => { try { return slash(realpathSync(p)); } catch { return slash(p); } },
  writeText: (p, s) => { writeFileSync(p, s); },
};

// walkFiles: every file under `root`, with its path relative to it. It follows symlinks and
// junctions (ComfyUI's listing does) and refuses only a loop back into an ancestor.
export function walkFiles(root, io, { follow = true, excludeDirs = [] } = {}) {
  const files = [];
  const chain = new Set();
  const visit = (dir, rel) => {
    const real = io.realpath(dir);
    if (chain.has(real)) return;
    const entries = io.list(dir);
    if (!entries) return;
    chain.add(real);
    for (const e of entries) {
      if (e.isDir) {
        if (excludeDirs.includes(e.name) || (e.isSymlink && !follow)) continue;
        visit(`${dir}/${e.name}`, `${rel}${e.name}/`);
      } else if (e.isFile) {
        files.push({ rel: rel + e.name, abs: `${dir}/${e.name}` });
      }
    }
    chain.delete(real);
  };
  visit(normalizePath(root), "");
  return files;
}

// ---------------------------------------------------------------------------------------------
// The active-node walk
// ---------------------------------------------------------------------------------------------

export const MODE_MUTED = 2;
export const MODE_BYPASSED = 4;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const MAX_SUBGRAPH_DEPTH = 8;

// walkGraph: a UI-format workflow is a graph whose nodes may be instances of subgraphs defined in
// `definitions.subgraphs`. A node is ACTIVE only when its mode is not muted (2) or bypassed (4)
// and every subgraph instance around it is active too. Instances are expanded and reported apart;
// leaves are the real nodes. Two things are flagged rather than hidden: an instance that cannot be
// expanded (a definition that instantiates itself, or nesting past maxDepth) stays a leaf marked
// `unexpanded`, and an instance of a definition that does not exist stays a leaf marked `dangling`.
// A definition nobody instantiates is never walked.
export function walkGraph(workflow, { maxDepth = MAX_SUBGRAPH_DEPTH } = {}) {
  const defs = new Map();
  const declared = Array.isArray(workflow?.definitions?.subgraphs) ? workflow.definitions.subgraphs : [];
  for (const sg of declared) if (sg && typeof sg === "object" && typeof sg.id === "string") defs.set(sg.id, sg);
  const leaves = [];
  const instances = [];
  const chain = [];
  const visit = (nodes, active, depth, path) => {
    if (!Array.isArray(nodes)) return;
    for (const node of nodes) {
      if (!node || typeof node !== "object") continue;
      const on = active && node.mode !== MODE_MUTED && node.mode !== MODE_BYPASSED;
      const type = typeof node.type === "string" ? node.type : "";
      const def = defs.get(type);
      if (def) {
        instances.push({ node, active: on, depth, path, subgraph: def });
        if (depth < maxDepth && !chain.includes(type)) {
          chain.push(type);
          visit(def.nodes, on, depth + 1, [...path, node.id]);
          chain.pop();
        } else {
          leaves.push({ node, active: on, depth, path, unexpanded: true });
        }
        continue;
      }
      const leaf = { node, active: on, depth, path };
      if (UUID_RE.test(type)) leaf.dangling = true;
      leaves.push(leaf);
    }
  };
  visit(workflow?.nodes, true, 0, []);
  return { leaves, instances, definitions: defs.size };
}

export function flattenActiveNodes(workflow, opts) {
  return walkGraph(workflow, opts).leaves;
}

// ---------------------------------------------------------------------------------------------
// Required model files
// ---------------------------------------------------------------------------------------------

const MODEL_EXT = [".safetensors", ".gguf", ".pth", ".pt", ".ckpt", ".bin", ".sft", ".onnx"];
const NOTE_TYPES = new Set(["MarkdownNote", "Note"]);

// The model class a core loader node reads its file from. Only used for a file that appears as a
// loader widget value and carries no `properties.models` annotation; an annotated file names its
// own class.
const LOADER_CLASS = {
  CheckpointLoaderSimple: "checkpoints", CheckpointLoader: "checkpoints", ImageOnlyCheckpointLoader: "checkpoints", unCLIPCheckpointLoader: "checkpoints",
  UNETLoader: "diffusion_models", VAELoader: "vae",
  CLIPLoader: "text_encoders", DualCLIPLoader: "text_encoders", TripleCLIPLoader: "text_encoders", QuadrupleCLIPLoader: "text_encoders",
  LoraLoader: "loras", LoraLoaderModelOnly: "loras",
  ControlNetLoader: "controlnet", DiffControlNetLoader: "controlnet", CLIPVisionLoader: "clip_vision",
  UpscaleModelLoader: "upscale_models", StyleModelLoader: "style_models", LatentUpscaleModelLoader: "latent_upscale_models",
  ModelPatchLoader: "model_patches", AudioEncoderLoader: "audio_encoders", GLIGENLoader: "gligen",
  HypernetworkLoader: "hypernetworks", PhotoMakerLoader: "photomaker",
};

const hasModelExt = (s) => { const l = s.toLowerCase(); return MODEL_EXT.some((e) => l.endsWith(e)); };
const textOrNull = (v) => (typeof v === "string" && v !== "" ? v : null);

// requiredModels: the files a template needs. `active` are the files of active nodes: the
// annotated ones (`properties.models`, which name a class, a download URL and sometimes a hash) and
// any loader widget file the author never annotated. `inactive` are annotated files that only
// bypassed or muted nodes reference: optional in practice (a Lightning LoRA on a bypassed loader),
// so not required, and reported so nobody has to wonder where they went.
export function requiredModels(workflow) {
  const { leaves } = walkGraph(workflow);
  const active = new Map();
  const inactive = new Map();
  const add = (map, req) => {
    const key = `${req.directory ?? ""}\u0000${req.name}`;
    const prev = map.get(key);
    if (!prev) { map.set(key, req); return; }
    for (const t of req.node_types) if (!prev.node_types.includes(t)) prev.node_types.push(t);
  };
  for (const { node, active: on } of leaves) {
    const type = typeof node.type === "string" ? node.type : "";
    const annotations = Array.isArray(node.properties?.models) ? node.properties.models : [];
    for (const m of annotations) {
      if (!m || typeof m !== "object" || typeof m.name !== "string" || m.name === "") continue;
      add(on ? active : inactive, {
        name: m.name, directory: textOrNull(m.directory), url: textOrNull(m.url),
        hash: textOrNull(m.hash), hash_type: textOrNull(m.hash_type), annotated: true, node_types: [type],
      });
    }
  }
  for (const key of active.keys()) inactive.delete(key);
  const annotated = new Set([...active.values()].map((m) => m.name.toLowerCase()));
  for (const { node, active: on } of leaves) {
    if (!on || !Array.isArray(node.widgets_values)) continue;
    const type = typeof node.type === "string" ? node.type : "";
    if (NOTE_TYPES.has(type)) continue;
    for (const v of node.widgets_values) {
      if (typeof v !== "string" || !hasModelExt(v)) continue;
      const name = slash(v);
      if (annotated.has(name.toLowerCase()) || annotated.has(baseName(name).toLowerCase())) continue;
      add(active, { name, directory: Object.hasOwn(LOADER_CLASS, type) ? LOADER_CLASS[type] : null, url: null, hash: null, hash_type: null, annotated: false, node_types: [type] });
    }
  }
  return { active: [...active.values()], inactive: [...inactive.values()] };
}

// parseNoteSizes: newer templates list each file's size in the model note as
// `- [file.safetensors](url) (7.49 GB)`. The units are binary (GiB printed as GB). A figure parsed
// from a note is the author's prose, not a measurement: it lands in `note_size_bytes`, never `size`.
const NOTE_SIZE_RE = /\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)\s*\(([\d.,]+)\s*(KB|MB|GB|TB)\)/g;
const NOTE_UNIT = { KB: 1024, MB: 1024 ** 2, GB: 1024 ** 3, TB: 1024 ** 4 };
export function parseNoteSizes(workflow) {
  const sizes = {};
  for (const { node } of walkGraph(workflow).leaves) {
    if (!NOTE_TYPES.has(node.type) || !Array.isArray(node.widgets_values) || typeof node.widgets_values[0] !== "string") continue;
    for (const m of node.widgets_values[0].matchAll(NOTE_SIZE_RE)) {
      const n = Number(m[3].replace(/,/g, ""));
      if (Number.isFinite(n)) sizes[m[1].trim()] = Math.round(n * NOTE_UNIT[m[4]]);
    }
  }
  return sizes;
}

// ---------------------------------------------------------------------------------------------
// API nodes: the paid partner nodes ComfyUI routes to cloud services
// ---------------------------------------------------------------------------------------------

// parseApiNodeIds: a static scan for the ways ComfyUI's comfy_api_nodes sources declare a node id.
// It reads text, never imports the code. A declaration that is commented out is still taken,
// which errs toward flagging a template as paid, the safe side of a spend gate. Three spellings
// exist (a tree reporting ComfyUI 0.37.0 has 287 ids by the first alone and 292 by all three):
//   IO.Schema(node_id="X")  and a class attribute  node_id = "X"
//   NODE_ID = "X"           a class attribute the schema reads back (node_id=cls.NODE_ID, nodes_bfl.py)
//   _cloud_schema("X", ...) the Comfy Cloud helper takes the id as its first argument
const API_NODE_ID_PATTERNS = [
  /\bnode_id\s*=\s*["']([^"'\s]+)["']/g,
  /\bNODE_ID\s*=\s*["']([^"'\s]+)["']/g,
  /\b_cloud_schema\(\s*["']([^"'\s]+)["']/g,
];
export function parseApiNodeIds(source) {
  const ids = new Set();
  const text = String(source ?? "");
  for (const re of API_NODE_ID_PATTERNS) for (const m of text.matchAll(re)) ids.add(m[1]);
  return ids;
}

// scanApiNodeIds: the ids a given ComfyUI tree declares. The catalog takes them from the node's OWN
// tree: the upstream templates repository's list of ids lags every ComfyUI release.
export function scanApiNodeIds(apiNodesDir, io = nodeIo) {
  const py = walkFiles(apiNodesDir, io, { follow: false }).filter((f) => f.rel.endsWith(".py"));
  const ids = new Set();
  for (const f of py) {
    try { for (const id of parseApiNodeIds(io.readText(f.abs))) ids.add(id); } catch { /* an unreadable source adds nothing */ }
  }
  return { ids: [...ids].sort(), files: py.length };
}

// apiSignals: three independent signals mark a paid template, and each is reported: a node whose
// type is a partner-node id, the `api_` name prefix, and the index's `openSource: false`. Any one
// is enough to call it paid. They should agree, and the summary counts where they do not.
export function apiSignals({ name, openSource, nodeTypes, apiIds }) {
  const ids = apiIds instanceof Set ? apiIds : new Set(apiIds ?? []);
  const by_node_id = [...new Set((nodeTypes ?? []).filter((t) => ids.has(t)))].sort();
  const by_name_prefix = String(name ?? "").startsWith("api_");
  const by_open_source_false = openSource === false;
  return { flag: by_node_id.length > 0 || by_name_prefix || by_open_source_false, by_node_id, by_name_prefix, by_open_source_false };
}

// classifyKind: `api` (paid partner nodes) beats `custom_nodes` (needs a node pack that core does
// not carry) beats `local` (core nodes only).
export function classifyKind({ api, customPacks, requiresCustomNodes }) {
  if (api?.flag) return "api";
  if ((customPacks?.length ?? 0) > 0 || (requiresCustomNodes?.length ?? 0) > 0) return "custom_nodes";
  return "local";
}

// ---------------------------------------------------------------------------------------------
// The parameter surface
// ---------------------------------------------------------------------------------------------

// paramSurface: the widgets a template chooses to expose on its subgraph instances
// (`properties.proxyWidgets`: [innerNodeId, widgetName] pairs), i.e. what a caller may override.
export function paramSurface(workflow) {
  const out = [];
  for (const { node } of walkGraph(workflow).instances) {
    const proxied = node.properties?.proxyWidgets;
    if (!Array.isArray(proxied)) continue;
    for (const entry of proxied) {
      if (!Array.isArray(entry) || entry.length < 2) continue;
      const [inner, widget] = entry;
      if ((typeof inner !== "string" && typeof inner !== "number") || typeof widget !== "string") continue;
      out.push({ instance: node.id, node: String(inner), widget });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// The index
// ---------------------------------------------------------------------------------------------

// index.json is a list of groups, each with a `templates` list. Files that sit beside it and are
// not workflows: index.json, the per-language indexes (index.fr.json), index.mcp.json,
// index.schema.json, index_logo.json.
const INDEX_ASSET_RE = /^index(?:[._][A-Za-z0-9-]+)?\.json$/;

export function flattenIndex(index) {
  const out = [];
  for (const g of Array.isArray(index) ? index : []) {
    if (!g || typeof g !== "object" || !Array.isArray(g.templates)) continue;
    const group = { module: g.moduleName ?? null, title: g.title ?? null, type: g.type ?? null, category: g.category ?? null };
    for (const t of g.templates) if (t && typeof t === "object") out.push({ ...t, group });
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// The version stamp
// ---------------------------------------------------------------------------------------------

const META = "comfyui-workflow-templates";
const CORE = "comfyui-workflow-templates-core";
const JSON_PKG = "comfyui-workflow-templates-json";

// readPackageVersions: wheel versions from the `<name>-<version>.dist-info` directory names in a
// site-packages directory or a wheel extract. Reads names only.
export function readPackageVersions(root, io = nodeIo) {
  const versions = {};
  for (const d of io.list(root) ?? []) {
    const m = /^(comfyui_workflow_templates(?:_[a-z0-9_]+)?)-(\d[^-]*)\.dist-info$/i.exec(d.name);
    if (m) versions[m[1].toLowerCase().replace(/_/g, "-")] = m[2];
  }
  return versions;
}

const pyprojectVersion = (path, io) => {
  try { return /^\s*version\s*=\s*"([^"]+)"/m.exec(io.readText(path))?.[1] ?? null; } catch { return null; }
};

// readRepoVersions: an upstream checkout keeps its versions in pyproject files, and its own root
// pyproject carries the meta package.
export function readRepoVersions(repoRoot, io = nodeIo) {
  const versions = {};
  const meta = pyprojectVersion(joinPath(repoRoot, "pyproject.toml"), io);
  const core = pyprojectVersion(joinPath(repoRoot, "packages/core/pyproject.toml"), io);
  const json = pyprojectVersion(joinPath(repoRoot, "packages/json/pyproject.toml"), io);
  if (meta) versions[META] = meta;
  if (core) versions[CORE] = core;
  if (json) versions[JSON_PKG] = json;
  return versions;
}

// readGitHead: the commit a checkout is on, from .git alone (HEAD, its ref, packed-refs).
export function readGitHead(repoRoot, io = nodeIo) {
  try {
    const head = io.readText(joinPath(repoRoot, ".git/HEAD")).trim();
    if (/^[0-9a-f]{40}$/.test(head)) return head;
    const ref = /^ref:\s*(\S+)/.exec(head)?.[1];
    if (!ref) return null;
    try {
      const sha = io.readText(joinPath(repoRoot, ".git", ref)).trim();
      if (/^[0-9a-f]{40}$/.test(sha)) return sha;
    } catch { /* fall through to packed-refs */ }
    const packed = io.readText(joinPath(repoRoot, ".git/packed-refs"));
    for (const line of packed.split(/\r?\n/)) {
      const m = /^([0-9a-f]{40})\s+(\S+)$/.exec(line);
      if (m && m[2] === ref) return m[1];
    }
  } catch { /* not a checkout */ }
  return null;
}

// The minor version of a `python3.13` directory name, so that 3.13 outranks 3.9 (a string sort ranks it below).
const pythonMinor = (name) => Number(/^python3\.(\d+)/.exec(name)?.[1] ?? -1);

// locateTemplates: where a ComfyUI tree keeps the installed templates, on either venv layout; the
// highest python3.x directory of a venv wins.
export function locateTemplates(comfyDir, io = nodeIo) {
  const suffix = "comfyui_workflow_templates_json/templates";
  const candidates = [];
  for (const venv of [".venv", "venv"]) {
    candidates.push(joinPath(comfyDir, venv, "Lib/site-packages"));
    const libs = io.list(joinPath(comfyDir, venv, "lib")) ?? [];
    for (const d of libs.filter((e) => e.isDir && /^python3/.test(e.name)).sort((a, b) => pythonMinor(b.name) - pythonMinor(a.name) || (a.name < b.name ? 1 : -1))) {
      candidates.push(joinPath(comfyDir, venv, "lib", d.name, "site-packages"));
    }
  }
  for (const sitePackages of candidates) {
    const templatesDir = joinPath(sitePackages, suffix);
    if (io.exists(joinPath(templatesDir, "index.json"))) return { sitePackages, templatesDir };
  }
  return null;
}

const BASIS_TEXT = {
  "installed-package": "installed package",
  "package-extract": "package extract",
  "repo-checkout": "repo checkout",
  "templates-dir": "templates directory",
};
export const BASES = Object.keys(BASIS_TEXT);

function versionsLabel(versions) {
  const parts = [];
  if (versions[META]) parts.push(`${META} ${versions[META]}`);
  if (versions[CORE]) parts.push(`core ${versions[CORE]}`);
  if (versions[JSON_PKG]) parts.push(`json ${versions[JSON_PKG]}`);
  return parts.length ? `(${parts.join(", ")})` : "(no version information)";
}

// stampFor: what a catalog was built from. `basis` says what kind of tree it is (installed
// package, a candidate extract, an upstream checkout, or a bare directory); `versions` and `label`
// say which release; `fingerprint` hashes every template file, so two nodes can be shown to carry
// the identical package without copying it.
export function stampFor({ templatesDir, basis, io = nodeIo }) {
  const dir = normalizePath(templatesDir);
  const packageRoot = dirName(dirName(dir));
  const repoRoot = dirName(dir);
  let versions = readPackageVersions(packageRoot, io);
  let source = Object.keys(versions).length > 0 ? "dist-info" : null;
  if (!source) {
    versions = readRepoVersions(repoRoot, io);
    if (Object.keys(versions).length > 0) source = "pyproject";
  }
  const inferred = source === "pyproject" ? "repo-checkout" : source === "dist-info" ? (dir.includes("site-packages") ? "installed-package" : "package-extract") : "templates-dir";
  const resolvedBasis = basis || inferred;
  const gitHead = resolvedBasis === "repo-checkout" ? readGitHead(repoRoot, io) : null;
  if (!BASES.includes(resolvedBasis)) throw new Error(`unknown basis "${resolvedBasis}" (want ${BASES.join(", ")})`);
  const names = (io.list(dir) ?? []).filter((d) => d.isFile && d.name.endsWith(".json")).map((d) => d.name).sort();
  const lines = names.map((n) => `${n}:${io.sha256(joinPath(dir, n))}`);
  let indexEntries = 0;
  try { indexEntries = flattenIndex(JSON.parse(io.readText(joinPath(dir, "index.json")))).length; } catch { /* the catalog build reports an unreadable index */ }
  const head = resolvedBasis === "repo-checkout" && gitHead ? ` at ${gitHead.slice(0, 8)}` : "";
  return {
    basis: resolvedBasis,
    label: `${BASIS_TEXT[resolvedBasis]}${head} ${versionsLabel(versions)}`,
    versions,
    package_version: versions[JSON_PKG] ?? null,
    git_head: gitHead,
    index_sha256: names.includes("index.json") ? io.sha256(joinPath(dir, "index.json")) : null,
    index_entries: indexEntries,
    template_files: names.filter((n) => !INDEX_ASSET_RE.test(n)).length,
    fingerprint: createHash("sha256").update(lines.join("\n")).digest("hex"),
  };
}

// checkStamp: a catalog only describes a node whose installed json package is the same version.
// Anything else (a mismatch, no installed package, an unstamped catalog) is refused, never
// guessed at.
export function checkStamp(stamp, nodeVersions) {
  const want = stamp?.package_version ?? null;
  const have = nodeVersions?.[JSON_PKG] ?? null;
  if (!want) return { ok: false, reason: "the catalog carries no package version, so it cannot be checked against a node" };
  if (!have) return { ok: false, reason: `the node reports no installed ${JSON_PKG}; the catalog was built from json ${want}` };
  if (want !== have) return { ok: false, reason: `the catalog was built from json ${want} but the node has json ${have}` };
  return { ok: true, reason: null };
}

// ---------------------------------------------------------------------------------------------
// Licences, the FLUX bar and the gate
// ---------------------------------------------------------------------------------------------
//
// A template names the weights it downloads, and the weights carry the licence, not the template.
// The harness keeps its own map from Hugging Face repo id to licence class (render/templates-license-map.json,
// each entry with its source URL and date). A repo the map does not list is `unknown`.
//
// Four classes, ranked by how much they restrict: permissive < unknown < conditional < non_commercial.
// A template's class is the worst of the repos its ACTIVE nodes download from. Unknown outranks
// permissive (nobody has read that licence) but never hides a known restriction: a template with one
// non-commercial repo and one unlisted repo is non_commercial. A file with no Hugging Face source
// counts as unknown. A template with no weights at all is `none`.

const LICENSE_CLASSES = ["permissive", "conditional", "non_commercial", "unknown"];
const CLASS_RANK = { none: -1, permissive: 0, unknown: 1, conditional: 2, non_commercial: 3 };
const worstClass = (classes) => classes.reduce((a, b) => (CLASS_RANK[b] > CLASS_RANK[a] ? b : a), "permissive");

// repoOfUrl: `owner/name` of a Hugging Face file URL (resolve or blob), or null for anything else.
export function repoOfUrl(url) {
  const m = /^https?:\/\/(?:www\.)?huggingface\.co\/([^/?#]+)\/([^/?#]+)\/(?:resolve|blob)\//i.exec(typeof url === "string" ? url : "");
  return m ? `${m[1]}/${m[2]}` : null;
}

export function emptyLicenseMap() {
  return loadLicenseMap({ repos: {} });
}

// An entry copied from a sibling repo instead of read is marked by its basis (`inferred from
// owner/name (not fetched)`). It carries no fetch date or source URL, and counts as no evidence.
const isInferred = (entry) => /^inferred/i.test(entry?.basis ?? "");

// loadLicenseMap: validates a map document and indexes it by repo id, ignoring case (Hugging Face
// treats `Owner/Name` and `owner/name` as one repo). A malformed map is refused whole.
export function loadLicenseMap(doc) {
  if (!doc || typeof doc !== "object" || Array.isArray(doc)) throw new Error("license map: not an object");
  if (!doc.repos || typeof doc.repos !== "object" || Array.isArray(doc.repos)) throw new Error("license map: `repos` must be an object keyed by repo id");
  const byLower = new Map();
  for (const [repo, entry] of Object.entries(doc.repos)) {
    if (!/^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/.test(repo)) throw new Error(`license map: "${repo}" is not a repo id (owner/name)`);
    if (!entry || typeof entry !== "object" || !LICENSE_CLASSES.includes(entry.class)) {
      throw new Error(`license map: ${repo}: class must be one of ${LICENSE_CLASSES.join(", ")}`);
    }
    if (byLower.has(repo.toLowerCase())) throw new Error(`license map: ${repo} appears twice (ids compare ignoring case)`);
    byLower.set(repo.toLowerCase(), { repo, entry });
  }
  const dates = [...byLower.values()].filter((v) => !isInferred(v.entry)).map((v) => v.entry.fetched_on).filter((d) => typeof d === "string").sort();
  return {
    size: byLower.size,
    get: (repo) => byLower.get(String(repo).toLowerCase())?.entry,
    has: (repo) => byLower.has(String(repo).toLowerCase()),
    entries: () => [...byLower.values()].map((v) => [v.repo, v.entry]),
    meta: { schema_version: doc.schema_version ?? null, evidence_dates: dates.length ? [dates[0], dates[dates.length - 1]] : [] },
  };
}

export function defaultLicenseMapPath() {
  if (NO_FILE) return null; // piped in: nothing sits beside the file, and the working directory is not "beside" it
  try { return fileURLToPath(new URL("./templates-license-map.json", import.meta.url)); } catch { return null; }
}

// loadDefaultLicenseMap: the map that ships beside this file. When there is none (the file was
// piped in on its own) every repo is unknown, and the caller can say so.
export function loadDefaultLicenseMap(io = nodeIo) {
  const path = defaultLicenseMapPath();
  if (!path || !io.exists(path)) return emptyLicenseMap();
  return loadLicenseMap(JSON.parse(io.readText(path)));
}

// licenseAssessment: the class of a template from the models its active nodes need. `class_strict`
// is the same with every INFERRED map entry (one copied from a sibling repo, not fetched) read as
// unknown, so the difference is visible instead of buried.
export function licenseAssessment(models, map = emptyLicenseMap()) {
  const list = Array.isArray(models) ? models : [];
  const byRepo = new Map();
  let unresolved = 0;
  for (const m of list) {
    const repo = repoOfUrl(m?.url);
    if (!repo) { unresolved++; continue; }
    const key = repo.toLowerCase();
    if (byRepo.has(key)) continue;
    const entry = map.get(repo);
    const cls = entry?.class ?? "unknown";
    const inferred = Boolean(entry) && isInferred(entry);
    byRepo.set(key, {
      repo, class: cls, known: Boolean(entry), license_id: entry?.license_id ?? null, basis: entry?.basis ?? null,
      strict_downgraded: inferred && cls !== "unknown", strict_class: inferred ? "unknown" : cls,
    });
  }
  const repos = [...byRepo.values()].sort((a, b) => (a.repo < b.repo ? -1 : a.repo > b.repo ? 1 : 0));
  const tail = unresolved > 0 ? ["unknown"] : [];
  const none = list.length === 0;
  return {
    class: none ? "none" : worstClass([...repos.map((r) => r.class), ...tail]),
    class_strict: none ? "none" : worstClass([...repos.map((r) => r.strict_class), ...tail]),
    repos: repos.map(({ strict_class, ...rest }) => rest),
    unresolved_models: unresolved,
    unmapped_repos: repos.filter((r) => !r.known).map((r) => r.repo),
  };
}

// The FLUX-family bar (ADR 0011). A template is barred when its name says FLUX or any repo its
// active nodes download from is FLUX-family: the repo id contains "flux", or it sits under the
// model maker's own organisation, unless the map says otherwise for that repo (`flux_bar` false
// exempts the shared text-encoder repo, `flux_bar` true adds one the pattern cannot see). A FLUX-
// named weights FILE from some other repo does not bar a template; it is reported, so somebody
// decides.
//
// One narrowing, by file class: a repo whose id merely contains "flux" does not bar a template
// that takes only text encoders from it. A text encoder is another maker's model wherever it is
// hosted (a Qwen one bundled with a FLUX release is not FLUX), and `blocked` has no acknowledgement
// path, so barring on the id alone was a hard stop on a false positive. Such a repo is listed in
// `text_encoder_repos` and the gate warns. Everything else still bars: any other class from the
// same repo, a file with no class (unknown fails closed), the maker's own organisation, and a map
// `flux_bar` true.
const FLUX_ID_PATTERN = /flux/i;
const FLUX_MAKER_PATTERN = /^black-forest-labs\//i;

export function fluxAssessment({ name, models }, map = emptyLicenseMap()) {
  const list = Array.isArray(models) ? models : [];
  const seen = new Map();
  for (const m of list) {
    const repo = repoOfUrl(m?.url);
    if (!repo) continue;
    let r = seen.get(repo.toLowerCase());
    if (!r) {
      const flag = map.get(repo)?.flux_bar;
      const patterned = flag !== true && flag !== false; // the map has no say: the id decides
      r = { repo, forced: flag === true, by_maker: patterned && FLUX_MAKER_PATTERN.test(repo), by_id: patterned && FLUX_ID_PATTERN.test(repo), beyond_encoders: false };
      seen.set(repo.toLowerCase(), r);
    }
    if (mapLegacyClass(m?.directory ?? "") !== "text_encoders") r.beyond_encoders = true;
  }
  const all = [...seen.values()];
  const repos = all.filter((r) => r.forced || r.by_maker || (r.by_id && r.beyond_encoders)).map((r) => r.repo).sort();
  const text_encoder_repos = all.filter((r) => r.by_id && !r.by_maker && !r.beyond_encoders).map((r) => r.repo).sort();
  const filename_hits = [...new Set(list.map((m) => m?.name).filter((n) => typeof n === "string" && /flux/i.test(n)))].sort();
  const by = [];
  if (/flux/i.test(String(name ?? ""))) by.push("name");
  if (repos.length > 0) by.push("repo");
  return { barred: by.length > 0, by, repos, filename_hits, text_encoder_repos };
}

// gateFor: what running a template would need. `blocked` reasons are hard: a paid partner-node
// template spends money (ADR 0001) and a FLUX-family template is barred (ADR 0011); no
// acknowledgement changes either. Otherwise anything but a permissive licence needs an explicit
// acknowledgement that names the class, and a template with no weights or a permissive licence is
// open. Warnings are facts the operator should see and that decide nothing.
export function gateFor({ kind, license, flux }) {
  const blocked = [];
  if (kind === "api") blocked.push("api_paid");
  if (flux?.barred) blocked.push("flux");
  const ack = blocked.length === 0 && ["conditional", "non_commercial", "unknown"].includes(license?.class) ? [license.class] : [];
  const warnings = [];
  if (flux && !flux.barred && flux.filename_hits?.length > 0) warnings.push(`flux_component_by_filename: ${flux.filename_hits.join(", ")}`);
  if (flux && !flux.barred && flux.text_encoder_repos?.length > 0) warnings.push(`flux_repo_text_encoder: ${flux.text_encoder_repos.join(", ")}`);
  return { state: blocked.length ? "blocked" : ack.length ? "ack_required" : "open", blocked, ack, warnings };
}

// ---------------------------------------------------------------------------------------------
// The catalog
// ---------------------------------------------------------------------------------------------

function readWorkflow(templatesDir, name, io) {
  const file = `${name}.json`;
  try {
    return { workflow: JSON.parse(io.readText(joinPath(templatesDir, file))), error: null };
  } catch (e) {
    if (e instanceof SyntaxError) return { workflow: null, error: `${file}: invalid JSON (${e.message})` };
    return { workflow: null, error: `${file}: cannot read (${e?.code ?? e?.message ?? "unknown error"})` };
  }
}

function emptyGraph() {
  return {
    ui_version: null, top_level_nodes: 0, subgraph_definitions: 0, subgraph_instances: 0, leaf_nodes: 0,
    active_leaf_nodes: 0, inactive_leaf_nodes: 0, unexpanded: 0, dangling: 0, model_annotation_entries: 0, model_annotation_hashed: 0,
  };
}

function buildRow(entry, { templatesDir, apiSet, licenseMap, io }) {
  const name = String(entry.name ?? "");
  const requires = Array.isArray(entry.requiresCustomNodes) ? entry.requiresCustomNodes : [];
  const { workflow, error } = readWorkflow(templatesDir, name, io);
  let graph = emptyGraph();
  let models = [];
  let inactive = [];
  let surface = [];
  let noteSizes = {};
  let customPacks = [];
  let nodeTypes = [];
  if (workflow && typeof workflow === "object") {
    const walked = walkGraph(workflow);
    const leaves = walked.leaves;
    let annotationEntries = 0;
    let annotationHashed = 0;
    const packs = new Set();
    for (const { node, active } of leaves) {
      for (const m of Array.isArray(node.properties?.models) ? node.properties.models : []) {
        if (!m || typeof m !== "object" || typeof m.name !== "string" || m.name === "") continue;
        annotationEntries++;
        if (m.hash) annotationHashed++;
      }
      const cnr = node.properties?.cnr_id;
      if (active && typeof cnr === "string" && cnr !== "" && cnr !== "comfy-core") packs.add(cnr);
    }
    customPacks = [...packs].sort();
    nodeTypes = leaves.map((l) => (typeof l.node.type === "string" ? l.node.type : ""));
    const req = requiredModels(workflow);
    noteSizes = parseNoteSizes(workflow);
    const withSize = (m) => ({ ...m, note_size_bytes: Object.hasOwn(noteSizes, m.name) ? noteSizes[m.name] : null });
    models = req.active.map(withSize);
    inactive = req.inactive.map(withSize);
    surface = paramSurface(workflow);
    graph = {
      ui_version: workflow.version ?? null,
      top_level_nodes: Array.isArray(workflow.nodes) ? workflow.nodes.length : 0,
      subgraph_definitions: walked.definitions,
      subgraph_instances: walked.instances.length,
      leaf_nodes: leaves.length,
      active_leaf_nodes: leaves.filter((l) => l.active).length,
      inactive_leaf_nodes: leaves.filter((l) => !l.active).length,
      unexpanded: leaves.filter((l) => l.unexpanded).length,
      dangling: leaves.filter((l) => l.dangling).length,
      model_annotation_entries: annotationEntries,
      model_annotation_hashed: annotationHashed,
    };
  }
  const api = apiSignals({ name, openSource: entry.openSource, nodeTypes, apiIds: apiSet });
  const kind = classifyKind({ api, customPacks, requiresCustomNodes: requires });
  const license = licenseAssessment(models, licenseMap);
  const flux = fluxAssessment({ name, models }, licenseMap);
  return {
    name,
    title: entry.title ?? null,
    description: entry.description ?? null,
    group: entry.group,
    media_type: entry.mediaType ?? null,
    media_subtype: entry.mediaSubtype ?? null,
    tags: Array.isArray(entry.tags) ? entry.tags : [],
    model_labels: Array.isArray(entry.models) ? entry.models : [],
    date: entry.date ?? null,
    size_bytes: typeof entry.size === "number" ? entry.size : null,
    usage: typeof entry.usage === "number" ? entry.usage : null,
    open_source: typeof entry.openSource === "boolean" ? entry.openSource : null,
    min_comfyui_version: entry.minComfyUIVersion ?? null,
    requires_custom_nodes: requires,
    include_on_distributions: Array.isArray(entry.includeOnDistributions) ? entry.includeOnDistributions : [],
    is_app: Boolean(entry.isApp),
    io: entry.io ?? null,
    kind,
    api,
    custom_packs: customPacks,
    graph,
    models,
    models_inactive: inactive,
    note_sizes: noteSizes,
    param_surface: surface,
    license,
    flux,
    gate: gateFor({ kind, license, flux }),
    load_error: error,
  };
}

// buildCatalog: one row per index entry. A workflow file that is missing or unreadable is that
// row's `load_error` (and the index alone classifies it); it never aborts the catalog. An index
// that cannot be read does, because nothing can be listed without it.
export function buildCatalog({ templatesDir, apiIds = new Set(), stamp = null, licenseMap = emptyLicenseMap(), io = nodeIo }) {
  const dir = normalizePath(templatesDir);
  let index;
  try {
    index = JSON.parse(io.readText(joinPath(dir, "index.json")));
  } catch (e) {
    throw new Error(`templates index unreadable (${joinPath(dir, "index.json")}): ${e?.message ?? e}`);
  }
  const apiSet = apiIds instanceof Set ? apiIds : new Set(apiIds);
  const entries = flattenIndex(index);
  const templates = entries.map((entry) => buildRow(entry, { templatesDir: dir, apiSet, licenseMap, io }));
  const indexed = new Set(entries.map((e) => String(e.name ?? "")));
  const orphan_files = (io.list(dir) ?? [])
    .filter((d) => d.isFile && d.name.endsWith(".json") && !INDEX_ASSET_RE.test(d.name))
    .map((d) => d.name.slice(0, -".json".length))
    .filter((n) => !indexed.has(n))
    .sort();
  return {
    schema_version: 1, stamp, api_ids: { count: apiSet.size },
    license_map: { entries: licenseMap.size, evidence_dates: licenseMap.meta.evidence_dates },
    orphan_files, templates,
  };
}

// summarizeCatalog: the headline numbers, every one computed on `basis`.
export function summarizeCatalog(catalog) {
  const rows = catalog.templates;
  const local = rows.filter((r) => r.kind === "local");
  const sum = (f) => rows.reduce((n, r) => n + f(r), 0);
  const flagged = (r) => r.api.by_node_id.length > 0 || r.api.by_name_prefix;
  return {
    basis: catalog.stamp?.label ?? "unstamped catalog (no version information)",
    entries: rows.length,
    load_errors: rows.filter((r) => r.load_error).length,
    orphan_files: catalog.orphan_files.length,
    kinds: {
      api: rows.filter((r) => r.kind === "api").length,
      custom_nodes: rows.filter((r) => r.kind === "custom_nodes").length,
      local: local.length,
    },
    local: {
      total: local.length,
      needing_weights: local.filter((r) => r.models.length > 0).length,
      zero_model: local.filter((r) => r.models.length === 0).length,
    },
    subgraph_templates: rows.filter((r) => r.graph.subgraph_definitions > 0).length,
    param_surface: {
      all: rows.filter((r) => r.param_surface.length > 0).length,
      local: local.filter((r) => r.param_surface.length > 0).length,
    },
    model_annotations: { entries: sum((r) => r.graph.model_annotation_entries), hashed: sum((r) => r.graph.model_annotation_hashed) },
    api_signals: {
      by_node_id: rows.filter((r) => r.api.by_node_id.length > 0).length,
      by_name_prefix: rows.filter((r) => r.api.by_name_prefix).length,
      by_open_source_false: rows.filter((r) => r.api.by_open_source_false).length,
      any: rows.filter((r) => r.api.flag).length,
      // The research's cross-check: the ids-and-prefix view of "paid" and the index's own
      // openSource:false flag describe the same set. A template one sees and the other does not.
      disagreements: rows.filter((r) => flagged(r) !== r.api.by_open_source_false).length,
    },
    api_ids: catalog.api_ids.count,
    ...summarizeLicenses(catalog, rows, local),
  };
}

// The licence, FLUX and gate counts are taken on the local templates that need weights: the
// population the operator could run. Paid templates are blocked outright and need no licence view.
function summarizeLicenses(catalog, rows, local) {
  const pop = local.filter((r) => r.models.length > 0);
  const tally = (key) => {
    const t = { permissive: 0, conditional: 0, non_commercial: 0, unknown: 0 };
    for (const r of pop) t[r.license[key]]++;
    return t;
  };
  const repos = new Map();
  for (const r of pop) for (const rep of r.license.repos) repos.set(rep.repo.toLowerCase(), rep.known);
  const mapped = [...repos.values()].filter(Boolean).length;
  return {
    license: {
      local_needing_weights: tally("class"),
      strict: tally("class_strict"),
      repos: { distinct: repos.size, mapped, unmapped: repos.size - mapped },
      map: catalog.license_map ?? { entries: 0, evidence_dates: [] },
    },
    flux: {
      local_needing_weights: {
        by_name: pop.filter((r) => r.flux.by.includes("name")).length,
        by_repo_only: pop.filter((r) => r.flux.by.includes("repo") && !r.flux.by.includes("name")).length,
        barred: pop.filter((r) => r.flux.barred).length,
        filename_only: pop.filter((r) => !r.flux.barred && r.flux.filename_hits.length > 0).length,
      },
      barred_all_kinds: rows.filter((r) => r.flux.barred).length,
    },
    gates: {
      blocked: rows.filter((r) => r.gate.state === "blocked").length,
      ack_required: rows.filter((r) => r.gate.state === "ack_required").length,
      open: rows.filter((r) => r.gate.state === "open").length,
    },
  };
}

// ---------------------------------------------------------------------------------------------
// Where ComfyUI looks for a model, and what is there
// ---------------------------------------------------------------------------------------------
//
// A model file is USABLE by a template only if ComfyUI would offer it to the loader that reads it,
// and that is a question about directories, not file names. A `VAELoader` lists the vae class and
// nothing else: the same file sitting in text_encoders/ is on disk and invisible. So readiness is
// decided the way ComfyUI 0.37.0's folder_paths.py, utils/extra_config.py and main.py decide it:
//
// - every model class has default directories under <comfy>/models, and three classes read two:
//   text_encoders also reads clip/, diffusion_models also reads unet/, controlnet also reads
//   t2i_adapter/;
// - extra_model_paths.yaml adds directories per class. Its keys `unet` and `clip` are legacy names
//   for diffusion_models and text_encoders; a provider's base_path is joined to each listed
//   directory (an absolute directory wins); is_default puts a provider's directories first;
// - once the yaml files are read, main.py's apply_custom_paths appends the output directory's
//   checkpoints, clip, vae, diffusion_models and loras (--output-directory moves the output
//   directory; --base-directory moves it and the models directory);
// - a class lists the files under its directories recursively (links followed, .git skipped) whose
//   extension its core loaders accept, by their path relative to the directory. A loader offers
//   exactly those strings, so a file in a subfolder or under another case is not the file a
//   template names.
//
// The table below is that version's. A snapshot records the node's ComfyUI version and hashes of
// folder_paths.py and utils/extra_config.py (main.py is not hashed: it holds far more than the
// few lines that matter, so a change confined to it is not seen), and readiness says when a
// node's files are not the ones the table was checked against.

const PT_EXTS = [".ckpt", ".pt", ".pt2", ".bin", ".pth", ".safetensors", ".pkl", ".sft"];
const classSpec = (dirs, exts = PT_EXTS) => ({ dirs, exts });
export const COMFYUI_CLASS_TABLE_VERSION = "0.37.0";
// The two rule files of the ComfyUI release the table below was checked against (the v0.37.0 tag), hashed the way snapshotNode hashes
// them: SHA-256 of the text with CRLF turned into LF.
export const COMFYUI_RULE_FILES = Object.freeze({
  version: COMFYUI_CLASS_TABLE_VERSION,
  folder_paths_sha256: "b748be5d1e068ad673f6c4de069fb6b7b723405c40ae3c3c44465d416f7df2db",
  extra_config_sha256: "ebb923e58956587acf5196e3a76ae3c576196f2f498e5933ce9416fca1ddaf9e",
});
const CLASS_TABLE = {
  checkpoints: classSpec(["checkpoints"]),
  configs: classSpec(["configs"], [".yaml"]),
  loras: classSpec(["loras"]),
  vae: classSpec(["vae"]),
  text_encoders: classSpec(["text_encoders", "clip"]),
  diffusion_models: classSpec(["unet", "diffusion_models"]),
  clip_vision: classSpec(["clip_vision"]),
  style_models: classSpec(["style_models"]),
  embeddings: classSpec(["embeddings"]),
  diffusers: classSpec(["diffusers"], ["folder"]),
  vae_approx: classSpec(["vae_approx"]),
  controlnet: classSpec(["controlnet", "t2i_adapter"]),
  gligen: classSpec(["gligen"]),
  upscale_models: classSpec(["upscale_models"]),
  latent_upscale_models: classSpec(["latent_upscale_models"]),
  hypernetworks: classSpec(["hypernetworks"]),
  photomaker: classSpec(["photomaker"]),
  classifiers: classSpec(["classifiers"], [""]),
  model_patches: classSpec(["model_patches"]),
  audio_encoders: classSpec(["audio_encoders"]),
  background_removal: classSpec(["background_removal"]),
  frame_interpolation: classSpec(["frame_interpolation"]),
  geometry_estimation: classSpec(["geometry_estimation"]),
  optical_flow: classSpec(["optical_flow"]),
  detection: classSpec(["detection"]),
};
const LEGACY_CLASS = { unet: "diffusion_models", clip: "text_encoders" };
// A name from a yaml file or a template is looked up as an own key only: `constructor` is not a legacy class.
export const mapLegacyClass = (name) => (Object.hasOwn(LEGACY_CLASS, name) ? LEGACY_CLASS[name] : name);

// The directories main.py's apply_custom_paths registers under the output directory, after it has read the yaml files, for the
// nodes that save checkpoints, CLIP, VAE, diffusion-model and LoRA files. `clip` is the legacy name of text_encoders.
const OUTPUT_MODEL_DIRS = ["checkpoints", "clip", "vae", "diffusion_models", "loras"];

function stripYamlComment(v) {
  const s = v.trim();
  if (s.startsWith('"') || s.startsWith("'")) {
    const end = s.indexOf(s[0], 1);
    return end < 0 ? s : s.slice(0, end + 1) + s.slice(end + 1).replace(/\s+#.*$/, "");
  }
  return s.replace(/(^|\s)#.*$/, "").trim();
}

function unquoteYaml(v) {
  if (v.length >= 2 && v.startsWith('"') && v.endsWith('"')) return v.slice(1, -1).replace(/\\(["\\])/g, "$1");
  if (v.length >= 2 && v.startsWith("'") && v.endsWith("'")) return v.slice(1, -1).replace(/''/g, "'");
  return v;
}

// splitYamlKey: `key: value`, the key plain (a provider may be named with a space) or in single or
// double quotes, as yaml.safe_load, which ComfyUI reads the file with, takes them. Null when the
// line is not a mapping key.
function splitYamlKey(t) {
  const quoted = /^(?:"([^"]*)"|'([^']*)')\s*:\s*(.*)$/.exec(t);
  if (quoted) return { key: quoted[1] ?? quoted[2], value: quoted[3] };
  const plain = /^([A-Za-z0-9_.][^:#]*?)\s*:\s*(.*)$/.exec(t);
  return plain ? { key: plain[1], value: plain[2] } : null;
}

// parseExtraModelPathsYaml: the shape ComfyUI documents, no more. Top-level keys are providers; a
// provider has `base_path`, optionally `is_default`, and `class: directory` entries whose value is
// one directory or a block scalar of several: `|` (one path per line) or `>` (YAML folds a
// paragraph into one space-joined path). Anything else in the file is ignored, so an unreadable file
// yields no providers rather than an error (the same fail-safe as the satisfier's reader): a
// top-level line that is not a `name:` header ends the provider before it, and its indented lines
// belong to nobody. Comment-looking lines inside a block scalar are skipped; ComfyUI would take them
// as paths that do not exist.
export function parseExtraModelPathsYaml(text) {
  const providers = [];
  let provider = null;
  let block = null;
  const endParagraph = () => {
    if (block?.folded && block.words.length > 0) { block.entry.paths.push(block.words.join(" ")); block.words = []; }
  };
  for (const raw of String(text ?? "").replace(/^\uFEFF/, "").split(/\r?\n/)) {
    const indent = raw.length - raw.trimStart().length;
    const t = raw.trim();
    if (block) {
      if (t === "") { endParagraph(); continue; }
      if (indent > block.indent) {
        if (!t.startsWith("#")) { if (block.folded) block.words.push(t); else block.entry.paths.push(t); }
        continue;
      }
      endParagraph();
      block = null;
    }
    if (t === "" || t.startsWith("#")) continue;
    const kv = splitYamlKey(t);
    if (indent === 0) {
      provider = kv && stripYamlComment(kv.value) === "" ? { name: kv.key, base_path: null, is_default: false, entries: [] } : null;
      if (provider) providers.push(provider);
      continue;
    }
    if (!provider || !kv) continue;
    const key = kv.key;
    const value = stripYamlComment(kv.value);
    if (value === "" || /^[|>][+-]?$/.test(value)) {
      const entry = { key, paths: [] };
      if (key !== "base_path" && key !== "is_default") provider.entries.push(entry);
      block = { entry, indent, folded: value.startsWith(">"), words: [] };
      continue;
    }
    const v = unquoteYaml(value);
    if (key === "base_path") provider.base_path = v;
    else if (key === "is_default") provider.is_default = /^(true|yes|on)$/i.test(v);
    else provider.entries.push({ key, paths: [v] });
  }
  endParagraph();
  return providers;
}

function expandPath(p, env, home) {
  let s = p;
  if (s === "~" || s.startsWith("~/") || s.startsWith("~\\")) s = (home ?? "") + s.slice(1);
  return s.replace(/\$\{(\w+)\}|\$(\w+)|%(\w+)%/g, (whole, a, b, c) => {
    const name = a ?? b ?? c;
    const v = env?.[name] ?? env?.[name.toUpperCase()];
    return v === undefined ? whole : v;
  });
}

// resolveModelRoots: the directories each class reads on a node, in ComfyUI's order, each with
// where it came from. `yamls` are the extra_model_paths files in load order: [{ text, dir }], dir
// being the directory the file lives in (relative paths are relative to it). After the yaml files
// come the five output directories main.py registers (`outputDir`, default <comfyDir>/output).
export function resolveModelRoots({ comfyDir, modelsDir = null, outputDir = null, yamls = [], env = nodeIo.env, home = nodeIo.home } = {}) {
  const models = modelsDir ? normalizePath(modelsDir) : joinPath(comfyDir, "models");
  const classes = {};
  for (const [cls, spec] of Object.entries(CLASS_TABLE)) {
    classes[cls] = { dirs: spec.dirs.map((d) => ({ path: joinPath(models, d), source: "default" })), exts: [...spec.exts] };
  }
  const add = (cls, path, isDefault, source) => {
    if (!Object.hasOwn(classes, cls)) classes[cls] = { dirs: [], exts: null };
    const c = classes[cls];
    const at = c.dirs.findIndex((d) => d.path === path);
    if (at >= 0) {
      if (isDefault && at !== 0) c.dirs.unshift(...c.dirs.splice(at, 1));
      return;
    }
    if (isDefault) c.dirs.unshift({ path, source });
    else c.dirs.push({ path, source });
  };
  for (const { text, dir } of yamls) {
    for (const provider of parseExtraModelPathsYaml(text)) {
      let base = null;
      if (provider.base_path) {
        base = expandPath(provider.base_path, env, home);
        if (!isAbsPath(base)) base = joinPath(dir, base);
      }
      for (const entry of provider.entries) {
        for (const y of entry.paths) {
          if (y === "") continue; // ComfyUI skips an empty entry; joined onto base_path it would register base_path itself
          const full = base ? (isAbsPath(y) ? y : joinPath(base, y)) : (isAbsPath(y) ? y : joinPath(dir, y));
          add(mapLegacyClass(entry.key), normalizePath(full), provider.is_default, `yaml:${provider.name}`);
        }
      }
    }
  }
  const output = outputDir ? normalizePath(outputDir) : joinPath(comfyDir, "output");
  for (const name of OUTPUT_MODEL_DIRS) add(mapLegacyClass(name), joinPath(output, name), false, "output");
  return { classes };
}

function extOf(rel) {
  const base = baseName(rel);
  const i = base.lastIndexOf(".");
  return i <= 0 ? "" : base.slice(i).toLowerCase();
}
const extListed = (rel, exts) => exts === null || exts.length === 0 || exts.includes(extOf(rel));
const byRel = (a, b) => (a.rel < b.rel ? -1 : a.rel > b.rel ? 1 : 0);

// scanModelInventory: what each class would list. A file reachable through several directories is
// one entry that names them all (a junction to a model drive makes the same file appear twice).
// Files with an extension the class's core loaders do not accept are only counted in `unlisted`.
export function scanModelInventory(roots, io = nodeIo) {
  const classes = {};
  const missing_dirs = [];
  for (const [cls, spec] of Object.entries(roots.classes)) {
    const listed = new Map();
    let unlisted = 0;
    for (const d of spec.dirs) {
      if (!io.list(d.path)) { missing_dirs.push({ class: cls, path: d.path }); continue; }
      for (const f of walkFiles(d.path, io, { follow: true, excludeDirs: [".git"] })) {
        if (!extListed(f.rel, spec.exts)) { unlisted++; continue; }
        const seen = listed.get(f.rel);
        if (seen) { if (!seen.dirs.includes(d.path)) seen.dirs.push(d.path); continue; }
        listed.set(f.rel, { rel: f.rel, dirs: [d.path], size: io.stat(f.abs)?.size ?? null });
      }
    }
    classes[cls] = { dirs: spec.dirs.map((d) => d.path), exts: spec.exts, files: [...listed.values()].sort(byRel), unlisted };
  }
  return { classes, missing_dirs };
}

// ---------------------------------------------------------------------------------------------
// Readiness
// ---------------------------------------------------------------------------------------------

const pushTo = (map, key, value) => { const list = map.get(key); if (list) list.push(value); else map.set(key, [value]); };
const indexCache = new WeakMap();

function indexInventory(inv) {
  const cached = indexCache.get(inv);
  if (cached) return cached;
  const byClass = new Map();
  const allBase = new Map();
  for (const [cls, c] of Object.entries(inv.classes ?? {})) {
    const exact = new Map();
    const lower = new Map();
    const base = new Map();
    for (const f of c.files ?? []) {
      exact.set(f.rel, f);
      pushTo(lower, f.rel.toLowerCase(), f);
      pushTo(base, baseName(f.rel).toLowerCase(), f);
      pushTo(allBase, baseName(f.rel).toLowerCase(), { class: cls, rel: f.rel });
    }
    byClass.set(cls, { exact, lower, base, exts: c.exts ?? null });
  }
  const index = { byClass, allBase };
  indexCache.set(inv, index);
  return index;
}

// checkRequirement: is this file, in this class, usable on this node? The statuses that are not
// `present` say why, and where the file is if it is somewhere else:
//   missing             not on the node at all
//   wrong_class         on the node, but under another class's directories
//   in_subfolder        under a subfolder of the right class: the loader offers `sub/name`, not `name`
//   case_mismatch       the right class, another spelling of the name
//   extension_not_listed  the class's core loaders do not accept this file type (a .gguf in a core loader)
//   class_unregistered  no directory is registered for the class
// A file with no class (an unannotated widget file) is met by its exact name in any class.
function checkRequirement(req, inv, mode) {
  const name = String(req.name);
  const directory = req.directory ?? null;
  const result = { name, directory, status: "missing" };
  const idx = indexInventory(inv);
  if (mode === "basename-only") {
    result.status = idx.allBase.has(baseName(name).toLowerCase()) ? "present" : "missing";
    return result;
  }
  if (directory === null) {
    const in_ = [...idx.byClass].filter(([, c]) => c.exact.has(name)).map(([cls]) => ({ class: cls, rel: name }));
    if (in_.length > 0) { result.status = "present_class_unknown"; result.found_in = in_; }
    return result;
  }
  const cls = mapLegacyClass(directory);
  const c = idx.byClass.get(cls);
  if (!c) { result.status = "class_unregistered"; return result; }
  if (!extListed(name, c.exts)) { result.status = "extension_not_listed"; return result; }
  if (c.exact.has(name)) { result.status = "present"; return result; }
  const cased = c.lower.get(name.toLowerCase());
  if (cased) { result.status = "case_mismatch"; result.found = cased.map((f) => f.rel); return result; }
  const sub = c.base.get(baseName(name).toLowerCase());
  if (sub) { result.status = "in_subfolder"; result.found = sub.map((f) => f.rel); return result; }
  const elsewhere = [];
  for (const [otherClass, other] of idx.byClass) {
    if (otherClass === cls) continue;
    const hit = other.exact.get(name) ?? other.lower.get(name.toLowerCase())?.[0] ?? other.base.get(baseName(name).toLowerCase())?.[0];
    if (hit) elsewhere.push({ class: otherClass, rel: hit.rel });
  }
  if (elsewhere.length > 0) { result.status = "wrong_class"; result.found_in = elsewhere; }
  return result;
}

const MET = new Set(["present", "present_class_unknown"]);

// readiness: whether every file a template needs is usable. `directory-aware` (the default) is the
// method above. `basename-only` is the older shortcut, kept so its numbers can be reproduced and
// compared: any file with that name, in any class, any folder, any case.
export function readiness(models, inv, { mode = "directory-aware" } = {}) {
  const requirements = (Array.isArray(models) ? models : []).map((m) => checkRequirement(m, inv, mode));
  const not_ready_count = requirements.filter((r) => !MET.has(r.status)).length;
  return { mode, ready: not_ready_count === 0, requirements, not_ready_count };
}

// catalogReadiness: readiness of every LOCAL template (core nodes only; paid and custom-node
// templates are not runnable by this route), keyed by template name.
export function catalogReadiness(catalog, inv, { mode = "directory-aware" } = {}) {
  const out = {};
  for (const row of catalog.templates) if (row.kind === "local") out[row.name] = readiness(row.models, inv, { mode });
  return out;
}

const bucketOf = (n) => (n === 0 ? "0" : n === 1 ? "1" : n === 2 ? "2" : n <= 5 ? "3-5" : "6+");

// summarizeReadiness: how many local templates a node could run, how many of those the gate leaves
// open, and how far the rest are (the number of files short, bucketed).
export function summarizeReadiness(catalog, results) {
  const rows = catalog.templates.filter((r) => r.kind === "local" && results[r.name]);
  const needing = rows.filter((r) => r.models.length > 0);
  const zero = rows.filter((r) => r.models.length === 0);
  const ready = needing.filter((r) => results[r.name].ready);
  const by_gate = { open: 0, ack_required: 0, blocked: 0 };
  for (const r of ready) by_gate[r.gate.state]++;
  const histogram = { "0": 0, "1": 0, "2": 0, "3-5": 0, "6+": 0 };
  for (const r of needing) histogram[bucketOf(results[r.name].not_ready_count)]++;
  return {
    local: rows.length,
    zero_model: zero.length,
    needing_weights: needing.length,
    ready: { total: ready.length + zero.length, zero_model: zero.length, with_weights: ready.length, names: ready.map((r) => r.name).sort(), by_gate },
    histogram,
  };
}

// checkRules: does the node run the ComfyUI rules the class table reproduces? The rule-file hashes
// decide (a tree can be commits past a release and still report its version); a snapshot without
// them is judged by its version string when it has one and is unverified when it has neither.
// Advisory: the node is scored either way, and this says by which rules.
export function checkRules(snapshot) {
  const node_version = snapshot?.comfyui_version ?? null;
  const base = { table_version: COMFYUI_RULE_FILES.version, node_version };
  const files = snapshot?.comfy_files;
  if (files && typeof files === "object") {
    const differs = [["folder_paths.py", "folder_paths_sha256"], ["utils/extra_config.py", "extra_config_sha256"]]
      .filter(([, key]) => files[key] !== COMFYUI_RULE_FILES[key]).map(([label]) => label);
    if (differs.length === 0) return { ok: true, ...base, differs, reason: null };
    const many = differs.length > 1;
    const seen = node_version ? ` (the node reports ${node_version})` : "";
    return { ok: false, ...base, differs, reason: `${differs.join(" and ")} ${many ? "differ" : "differs"} from the ComfyUI ${base.table_version} file${many ? "s" : ""} the directory rules were checked against${seen}` };
  }
  if (node_version !== null && node_version !== base.table_version) {
    return { ok: false, ...base, differs: [], reason: `the node reports ComfyUI ${node_version}; the directory rules were checked against ${base.table_version}` };
  }
  return { ok: null, ...base, differs: [], reason: "the snapshot carries no rule-file hashes, so the ComfyUI rules it ran cannot be checked" };
}

// nodeReadiness: a catalog can only speak for a node that carries the same templates package.
// Without that match the answer is a refusal, not a guess; `candidate` asks for the numbers anyway
// (what WOULD this candidate package do on this node) and labels them so. `rules_check` says
// whether the node runs the ComfyUI rules the directory check was made from.
export function nodeReadiness({ catalog, snapshot, mode = "directory-aware", candidate = false }) {
  const stamp_check = checkStamp(catalog.stamp, snapshot.package?.versions);
  const rules_check = checkRules(snapshot);
  if (!stamp_check.ok && !candidate) return { mode, candidate, stamp_check, rules_check, refused: stamp_check.reason };
  const results = catalogReadiness(catalog, snapshot.inventory, { mode });
  return { mode, candidate, stamp_check, rules_check, counts: summarizeReadiness(catalog, results), results };
}

// readinessTable: every node's readiness, the templates ready on at least one node, and the
// distance of the nearest node for the rest.
export function readinessTable({ catalog, snapshots, mode = "directory-aware", candidate = false }) {
  const rowByName = new Map(catalog.templates.map((t) => [t.name, t]));
  const nodes = {};
  const ready = new Set();
  const nearest = new Map();
  for (const [id, snapshot] of Object.entries(snapshots)) {
    const n = nodeReadiness({ catalog, snapshot, mode, candidate });
    nodes[id] = n;
    if (!n.counts) continue;
    for (const name of n.counts.ready.names) ready.add(name);
    for (const [name, r] of Object.entries(n.results)) {
      if (!rowByName.get(name)?.models.length) continue;
      nearest.set(name, Math.min(nearest.get(name) ?? Infinity, r.not_ready_count));
    }
  }
  const by_gate = { open: 0, ack_required: 0, blocked: 0 };
  for (const name of ready) by_gate[rowByName.get(name).gate.state]++;
  const histogram = { "0": 0, "1": 0, "2": 0, "3-5": 0, "6+": 0 };
  for (const n of nearest.values()) histogram[bucketOf(n)]++;
  return { mode, nodes, any_node: { with_weights: ready.size, names: [...ready].sort(), by_gate, nearest_histogram: histogram } };
}

// diffCatalogs: what moving from catalog `a` to catalog `b` (a candidate package) changes: templates
// added and removed, templates whose required files differ, and, for each node snapshot given,
// which templates ready under `a` would stop being ready under `b` and which would start.
export function diffCatalogs(a, b, { snapshots = {}, mode = "directory-aware" } = {}) {
  const A = new Map(a.templates.map((r) => [r.name, r]));
  const B = new Map(b.templates.map((r) => [r.name, r]));
  const key = (m) => `${m.directory ?? ""}/${m.name}`;
  const changed = [];
  for (const [name, ra] of A) {
    const rb = B.get(name);
    if (!rb) continue;
    const ka = new Set(ra.models.map(key));
    const kb = new Set(rb.models.map(key));
    const added = [...kb].filter((k) => !ka.has(k)).sort();
    const removed = [...ka].filter((k) => !kb.has(k)).sort();
    if (added.length || removed.length) changed.push({ name, kind_from: ra.kind, kind_to: rb.kind, added, removed });
  }
  changed.sort((x, y) => (x.name < y.name ? -1 : 1));
  const per = {};
  for (const [id, snapshot] of Object.entries(snapshots)) {
    const ra = catalogReadiness(a, snapshot.inventory, { mode });
    const rb = catalogReadiness(b, snapshot.inventory, { mode });
    const regressions = [];
    const gains = [];
    for (const name of Object.keys(ra).sort()) {
      if (!rb[name]) continue;
      if (ra[name].ready && !rb[name].ready) {
        regressions.push({ name, unmet: rb[name].requirements.filter((r) => !MET.has(r.status)).map((r) => ({ name: r.name, directory: r.directory, status: r.status })) });
      } else if (!ra[name].ready && rb[name].ready) {
        gains.push(name);
      }
    }
    per[id] = { regressions, gains };
  }
  const label = (c) => c.stamp?.label ?? "unstamped catalog (no version information)";
  return {
    basis: { from: label(a), to: label(b) },
    added: [...B.keys()].filter((n) => !A.has(n)).sort(),
    removed: [...A.keys()].filter((n) => !B.has(n)).sort(),
    changed,
    readiness: per,
  };
}

// ---------------------------------------------------------------------------------------------
// A node snapshot
// ---------------------------------------------------------------------------------------------

// The launch flags that move a ComfyUI directory, and the snapshot field each one fills.
const LAUNCH_DIRECTORY_FLAGS = new Map([["--models-directory", "models_directory"], ["--base-directory", "base_directory"], ["--output-directory", "output_directory"]]);

function parseLaunchArgs(rawArgs) {
  const args = rawArgs.map(String);
  const out = { args, models_directory: null, base_directory: null, output_directory: null, extra_model_paths_config: [] };
  for (let i = 0; i < args.length; i++) {
    const eq = /^(--[a-z-]+)=(.*)$/.exec(args[i]);
    const flag = eq ? eq[1] : args[i];
    if (LAUNCH_DIRECTORY_FLAGS.has(flag)) {
      const value = eq ? eq[2] : args[i + 1];
      if (typeof value === "string" && !value.startsWith("--")) out[LAUNCH_DIRECTORY_FLAGS.get(flag)] = value;
    } else if (flag === "--extra-model-paths-config") {
      if (eq) { out.extra_model_paths_config.push(eq[2]); continue; }
      for (let j = i + 1; j < args.length && !args[j].startsWith("--"); j++) out.extra_model_paths_config.push(args[j]);
    }
  }
  return out;
}

// snapshotNode: everything readiness needs to know about one node, read from disk and nothing
// else: the ComfyUI version, hashes of the two ComfyUI files whose rules the readiness check
// reproduces, how it was launched (a launch record's flags can move the models or add yaml files),
// the installed templates package and its fingerprint, the node's own API node ids, the resolved
// model roots and the model files each class would list. Plain JSON; it writes nothing.
export function snapshotNode({ comfyDir, io = nodeIo }) {
  const dir = normalizePath(comfyDir);
  const read = (p) => { try { return io.readText(p); } catch { return null; } };
  const hash = (p) => { try { return io.exists(p) ? io.sha256(p) : null; } catch { return null; } };
  // The two ComfyUI rule files are compared across operating systems, so hash their text with line
  // endings normalised: the same file is CRLF on a Windows node and LF on a Linux one.
  const textHash = (p) => { const t = read(p); return t === null ? null : createHash("sha256").update(t.replace(/\r\n/g, "\n")).digest("hex"); };
  const comfyui_version = /__version__\s*=\s*["']([^"']+)["']/.exec(read(joinPath(dir, "comfyui_version.py")) ?? "")?.[1] ?? null;
  let launch = null;
  const record = read(joinPath(dir, ".offload-launch.json"));
  if (record) {
    try {
      const parsed = JSON.parse(record);
      if (Array.isArray(parsed?.args)) launch = parseLaunchArgs(parsed.args);
    } catch { /* an unreadable launch record is no launch record */ }
  }
  const yamlPaths = [joinPath(dir, "extra_model_paths.yaml"), ...(launch?.extra_model_paths_config ?? []).map((p) => normalizePath(p))];
  const yamls = [];
  const yaml = [];
  for (const [i, path] of yamlPaths.entries()) {
    if (yamlPaths.indexOf(path) !== i) continue;
    const text = read(path);
    if (text === null) { if (i > 0) yaml.push({ path, readable: false, sha256: null, providers: 0 }); continue; }
    yamls.push({ text, dir: dirName(path) });
    yaml.push({ path, readable: true, sha256: hash(path), providers: parseExtraModelPathsYaml(text).length });
  }
  const baseDir = launch?.base_directory ?? null;
  const modelsDir = launch?.models_directory ?? (baseDir ? joinPath(baseDir, "models") : null);
  const outputDir = launch?.output_directory ?? (baseDir ? joinPath(baseDir, "output") : null);
  const roots = resolveModelRoots({ comfyDir: dir, modelsDir, outputDir, yamls, env: io.env, home: io.home });
  const located = locateTemplates(dir, io);
  const apiDir = joinPath(dir, "comfy_api_nodes");
  const api = scanApiNodeIds(apiDir, io);
  return {
    schema_version: 1,
    comfy_dir: dir,
    comfyui_version,
    comfy_files: { folder_paths_sha256: textHash(joinPath(dir, "folder_paths.py")), extra_config_sha256: textHash(joinPath(dir, "utils/extra_config.py")) },
    launch,
    package: located
      ? { site_packages: located.sitePackages, templates_dir: located.templatesDir, versions: readPackageVersions(located.sitePackages, io), stamp: stampFor({ templatesDir: located.templatesDir, basis: "installed-package", io }) }
      : null,
    api_nodes: { dir: apiDir, files: api.files, ids: api.ids },
    yaml,
    model_roots: roots,
    inventory: scanModelInventory(roots, io),
  };
}

// ---------------------------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------------------------

const BOOLEAN_FLAGS = new Set(["json", "include-hidden", "help", "candidate", "detail"]);

function parseArgs(argv) {
  const flags = {};
  const positional = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "-h") { flags.help = true; continue; }
    if (!a.startsWith("--")) { positional.push(a); continue; }
    const key = a.slice(2);
    if (BOOLEAN_FLAGS.has(key)) { flags[key] = true; continue; }
    if (i + 1 >= argv.length) return { error: `--${key} needs a value` };
    const value = argv[++i];
    flags[key] = key in flags ? [].concat(flags[key], value) : value;
  }
  return { flags, positional };
}

const USAGE = `usage: templates-catalog <verb> [options]

Lists and classifies the ComfyUI workflow templates a node carries. It is read-only: it never runs
a template, downloads a model, installs a package or changes a config. The only file it writes is
the one named by --out.

verbs:
  summary    counts, each labelled with the package it was computed on
  list       the templates; FLUX-family ones are hidden unless --include-hidden, and so are paid API ones, except that --kind api lists them without --include-hidden
  catalog    the full catalog as JSON
  snapshot   one node's package versions, API node ids, model directories and model files, as JSON
  readiness  what each node (--snapshot FILE, repeatable, or LABEL=FILE) could run: --mode directory-aware | basename-only | both
  diff       what a candidate package (--candidate-dir DIR) adds, removes and changes, and which templates it would break on each --snapshot node

source (choose one):
  --templates-dir DIR   a comfyui_workflow_templates_json/templates directory, or an upstream checkout's templates/
  --comfy-dir DIR       a ComfyUI tree; its installed package and its comfy_api_nodes are used
options:
  --api-nodes-dir DIR   the comfy_api_nodes directory whose node ids mark paid templates (default: under --comfy-dir)
  --license-map FILE    a licence map (default: templates-license-map.json beside this file)
  --basis KIND          installed-package | package-extract | repo-checkout | templates-dir (default: inferred)
  --candidate-basis KIND  what the --candidate-dir of diff is: the same kinds as --basis (default: inferred)
  --json                machine-readable output
  --out FILE            also write the output to FILE
list filters: --kind local|api|custom_nodes  --group TITLE  --type TYPE  --tag TAG  --text WORDS  --include-hidden
readiness: --candidate (answer for a node whose package differs from the catalog's, labelled) --detail (per-template results in --json)
`;

function resolveSource(flags, io) {
  const comfyDir = typeof flags["comfy-dir"] === "string" ? flags["comfy-dir"] : null;
  let templatesDir = typeof flags["templates-dir"] === "string" ? flags["templates-dir"] : null;
  if (!templatesDir && comfyDir) {
    const located = locateTemplates(comfyDir, io);
    if (!located) return { error: `no comfyui_workflow_templates_json package found under ${comfyDir}` };
    templatesDir = located.templatesDir;
  }
  if (!templatesDir) return { error: "give --templates-dir or --comfy-dir" };
  let basis = flags.basis;
  if (basis !== undefined && !BASES.includes(basis)) return { error: `unknown --basis "${basis}" (want ${BASES.join(", ")})` };
  if (!basis && comfyDir && !flags["templates-dir"]) basis = "installed-package";
  const apiDir = typeof flags["api-nodes-dir"] === "string" ? flags["api-nodes-dir"] : comfyDir ? joinPath(comfyDir, "comfy_api_nodes") : null;
  const scan = apiDir ? scanApiNodeIds(apiDir, io) : { ids: [], files: 0 };
  let licenseMap;
  try {
    licenseMap = typeof flags["license-map"] === "string" ? loadLicenseMap(JSON.parse(io.readText(flags["license-map"]))) : loadDefaultLicenseMap(io);
  } catch (e) {
    return { error: `license map: ${e?.message ?? e}` };
  }
  return { templatesDir, basis, apiIds: new Set(scan.ids), apiScan: scan, apiDir, licenseMap };
}

function loadCatalog(flags, io) {
  const src = resolveSource(flags, io);
  if (src.error) return src;
  const stamp = stampFor({ templatesDir: src.templatesDir, basis: src.basis, io });
  const catalog = buildCatalog({ templatesDir: src.templatesDir, apiIds: src.apiIds, stamp, licenseMap: src.licenseMap, io });
  return { catalog, src };
}

function formatSummary(s, apiNote) {
  const row = (label, value, indent = 0) => `${" ".repeat(indent)}${label}`.padEnd(52) + value + "\n";
  let t = `basis: ${s.basis}\n`;
  t += row("entries", s.entries);
  t += row("api (paid partner nodes)", s.kinds.api, 2);
  t += row("custom_nodes", s.kinds.custom_nodes, 2);
  t += row("local (core nodes only)", s.kinds.local, 2);
  t += row("needing weights", s.local.needing_weights, 4);
  t += row("zero-model", s.local.zero_model, 4);
  t += row("templates with subgraphs", s.subgraph_templates);
  t += row("with a parameter surface", `${s.param_surface.all} (local: ${s.param_surface.local})`);
  t += row("model annotations", `${s.model_annotations.entries} (with a hash: ${s.model_annotations.hashed})`);
  t += row("api signals (node id / api_ / openSource:false)", `${s.api_signals.by_node_id} / ${s.api_signals.by_name_prefix} / ${s.api_signals.by_open_source_false}`);
  t += row("api signal disagreements", s.api_signals.disagreements);
  const lic = (c) => `permissive ${c.permissive} | conditional ${c.conditional} | non_commercial ${c.non_commercial} | unknown ${c.unknown}`;
  t += row("licence class (local, needing weights)", lic(s.license.local_needing_weights));
  t += row("strict: inferred map entries as unknown", lic(s.license.strict), 2);
  t += row("repos: distinct / in the map / unmapped", `${s.license.repos.distinct} / ${s.license.repos.mapped} / ${s.license.repos.unmapped}`, 2);
  const dates = s.license.map.evidence_dates;
  t += row("licence map", `${s.license.map.entries} repos${dates.length ? `, evidence ${dates[0]} .. ${dates[1]}` : ""}`, 2);
  const fl = s.flux.local_needing_weights;
  t += row("FLUX bar (local, needing weights)", `by name ${fl.by_name}, by repo only ${fl.by_repo_only}, barred ${fl.barred}; FLUX-named file only ${fl.filename_only}`);
  t += row("gates (all templates)", `blocked ${s.gates.blocked} | ack required ${s.gates.ack_required} | open ${s.gates.open}`);
  t += row("workflow files not loaded", s.load_errors);
  t += row("files in the package not in the index", s.orphan_files);
  t += `api node ids: ${s.api_ids} (${apiNote})\n`;
  return t;
}

function condense(row) {
  return {
    name: row.name, title: row.title, kind: row.kind, group: row.group.title, type: row.group.type,
    media_type: row.media_type, tags: row.tags, size_bytes: row.size_bytes, open_source: row.open_source,
    models: row.models.length, custom_packs: row.custom_packs, license: row.license.class, gate: row.gate.state,
  };
}

function filterRows(rows, flags) {
  const one = (v) => (Array.isArray(v) ? v[v.length - 1] : v);
  const kind = one(flags.kind);
  const group = one(flags.group);
  const type = one(flags.type);
  const tag = one(flags.tag);
  const text = one(flags.text)?.toLowerCase();
  const hidden = { api: 0, flux: 0 };
  const shown = [];
  for (const r of rows) {
    if (kind && r.kind !== kind) continue;
    if (group && String(r.group.title ?? "").toLowerCase() !== String(group).toLowerCase()) continue;
    if (type && r.group.type !== type) continue;
    if (tag && !r.tags.includes(tag)) continue;
    if (text && !`${r.name} ${r.title ?? ""} ${r.description ?? ""}`.toLowerCase().includes(text)) continue;
    if (!flags["include-hidden"] && kind !== "api" && r.kind === "api") { hidden.api++; continue; }
    if (!flags["include-hidden"] && r.flux.barred) { hidden.flux++; continue; }
    shown.push(r);
  }
  return { shown, hidden };
}

async function emit(text, flags, io, out, err) {
  out(text.endsWith("\n") ? text : text + "\n");
  if (typeof flags.out === "string") {
    try { io.writeText(flags.out, text.endsWith("\n") ? text : text + "\n"); } catch (e) {
      err(`cannot write ${flags.out}: ${e?.message ?? e}\n`);
      return 1;
    }
    err(`wrote ${flags.out}\n`);
  }
  return 0;
}

export async function main(argv, { io = nodeIo, out = (s) => process.stdout.write(s), err = (s) => process.stderr.write(s) } = {}) {
  const parsed = parseArgs(argv);
  if (parsed.error) { err(`${parsed.error}\n`); return 2; }
  const { flags, positional } = parsed;
  const verb = positional[0];
  if (flags.help || verb === "help") { out(USAGE); return 0; }
  if (verb === undefined) { err(USAGE); return 2; }
  const verbs = { summary: verbSummary, list: verbList, catalog: verbCatalog, snapshot: verbSnapshot, readiness: verbReadiness, diff: verbDiff };
  const run = Object.hasOwn(verbs, verb) ? verbs[verb] : undefined;
  if (!run) { err(`unknown verb "${verb}"\n${USAGE}`); return 2; }
  try {
    return await run(flags, io, out, err);
  } catch (e) {
    err(`${verb}: ${e?.message ?? e}\n`);
    return 1;
  }
}

async function verbSummary(flags, io, out, err) {
  const loaded = loadCatalog(flags, io);
  if (loaded.error) { err(`${loaded.error}\n`); return 2; }
  const s = summarizeCatalog(loaded.catalog);
  const note = loaded.src.apiDir ? `scanned ${loaded.src.apiScan.files} source files in the node's comfy_api_nodes` : "none given: only the api_ prefix and openSource:false mark paid templates";
  return emit(flags.json ? JSON.stringify(s, null, 2) : formatSummary(s, note), flags, io, out, err);
}

async function verbList(flags, io, out, err) {
  const loaded = loadCatalog(flags, io);
  if (loaded.error) { err(`${loaded.error}\n`); return 2; }
  const { shown, hidden } = filterRows(loaded.catalog.templates, flags);
  const basis = loaded.catalog.stamp.label;
  if (flags.json) {
    return emit(JSON.stringify({ basis, total: loaded.catalog.templates.length, shown: shown.length, hidden, templates: shown.map(condense) }, null, 2), flags, io, out, err);
  }
  let t = `basis: ${basis}\n${shown.length} of ${loaded.catalog.templates.length} templates (hidden: ${hidden.api} paid API, ${hidden.flux} FLUX-family)\n`;
  for (const r of shown) t += `${r.name.padEnd(46)} ${r.kind.padEnd(13)} ${r.gate.state.padEnd(13)} ${String(r.models.length).padStart(2)} files  ${r.title ?? ""}\n`;
  return emit(t, flags, io, out, err);
}

async function verbCatalog(flags, io, out, err) {
  const loaded = loadCatalog(flags, io);
  if (loaded.error) { err(`${loaded.error}\n`); return 2; }
  return emit(JSON.stringify(loaded.catalog, null, 1), flags, io, out, err);
}

// What a ComfyUI tree has at its root: any one of these makes a directory worth taking a snapshot of.
const COMFY_TREE_MARKERS = ["comfyui_version.py", "folder_paths.py", "main.py"];

async function verbSnapshot(flags, io, out, err) {
  if (typeof flags["comfy-dir"] !== "string") { err("give --comfy-dir\n"); return 2; }
  const dir = normalizePath(flags["comfy-dir"]);
  if (!io.list(dir)) { err(`cannot read ${flags["comfy-dir"]}\n`); return 2; }
  if (!COMFY_TREE_MARKERS.some((f) => io.exists(joinPath(dir, f)))) {
    err(`not a ComfyUI tree: ${flags["comfy-dir"]} (none of ${COMFY_TREE_MARKERS.join(", ")} is there)\n`);
    return 2;
  }
  const snapshot = snapshotNode({ comfyDir: flags["comfy-dir"], io });
  if (typeof flags.label === "string") snapshot.label = flags.label;
  return emit(JSON.stringify(snapshot), flags, io, out, err);
}

// --snapshot FILE or --snapshot LABEL=FILE, repeatable. A file without a label is named after itself.
function loadSnapshots(flags, io) {
  const given = [].concat(flags.snapshot ?? []);
  if (given.length === 0) return { error: "give at least one --snapshot FILE (from the snapshot verb)" };
  const snapshots = {};
  for (const arg of given) {
    const m = /^([A-Za-z0-9_.-]+)=(.+)$/.exec(arg);
    const path = m ? m[2] : arg;
    const label = m ? m[1] : baseName(path).replace(/\.[^.]*$/, "");
    try { snapshots[label] = JSON.parse(io.readText(path)); } catch (e) { return { error: `snapshot ${path}: ${e?.message ?? e}` }; }
  }
  return { snapshots };
}

const READINESS_MODES = ["directory-aware", "basename-only", "both"];

function stripResults(table, detail) {
  if (detail) return table;
  const nodes = {};
  for (const [id, n] of Object.entries(table.nodes)) { const { results, ...rest } = n; nodes[id] = rest; }
  return { ...table, nodes };
}

function formatReadinessTable(table) {
  let t = `mode: ${table.mode}\n`;
  for (const [id, n] of Object.entries(table.nodes)) {
    if (n.refused) { t += `${id.padEnd(14)} refused: ${n.refused}\n`; continue; }
    const c = n.counts;
    t += `${id.padEnd(14)} ${c.ready.with_weights} ready with weights, ${c.ready.zero_model} zero-model (of ${c.needing_weights} needing weights, ${c.local} local)${n.candidate ? "  [candidate: package differs]" : ""}\n`;
    if (table.mode === "directory-aware" && n.rules_check?.ok === false) t += `${"".padEnd(14)} rules: ${n.rules_check.reason}\n`;
    t += `${"".padEnd(14)} gates on the ready ones: open ${c.ready.by_gate.open} | ack required ${c.ready.by_gate.ack_required} | blocked ${c.ready.by_gate.blocked}\n`;
    const h = c.histogram;
    t += `${"".padEnd(14)} files short (needing weights): 0: ${h["0"]}  1: ${h["1"]}  2: ${h["2"]}  3-5: ${h["3-5"]}  6+: ${h["6+"]}\n`;
    if (c.ready.names.length > 0) t += `${"".padEnd(14)} ready: ${c.ready.names.join(", ")}\n`;
  }
  t += `any node: ${table.any_node.with_weights} ready with weights (open ${table.any_node.by_gate.open} | ack required ${table.any_node.by_gate.ack_required} | blocked ${table.any_node.by_gate.blocked})\n`;
  return t;
}

async function verbReadiness(flags, io, out, err) {
  const loaded = loadCatalog(flags, io);
  if (loaded.error) { err(`${loaded.error}\n`); return 2; }
  const snaps = loadSnapshots(flags, io);
  if (snaps.error) { err(`${snaps.error}\n`); return 2; }
  const mode = typeof flags.mode === "string" ? flags.mode : "directory-aware";
  if (!READINESS_MODES.includes(mode)) { err(`unknown --mode "${mode}" (want ${READINESS_MODES.join(", ")})\n`); return 2; }
  const { catalog } = loaded;
  const candidate = Boolean(flags.candidate);
  const table = (m) => readinessTable({ catalog, snapshots: snaps.snapshots, mode: m, candidate });
  const tables = mode === "both" ? { "directory-aware": table("directory-aware"), "basename-only": table("basename-only") } : { [mode]: table(mode) };
  const refused = Object.values(tables).some((t) => Object.values(t.nodes).some((n) => n.refused));
  const basis = catalog.stamp?.label ?? "unstamped catalog (no version information)";
  let body;
  if (mode === "both") {
    const delta = {};
    for (const id of Object.keys(snaps.snapshots)) {
      const strict = new Set(tables["directory-aware"].nodes[id].counts?.ready.names ?? []);
      delta[id] = (tables["basename-only"].nodes[id].counts?.ready.names ?? []).filter((n) => !strict.has(n));
    }
    body = flags.json
      ? JSON.stringify({ basis, candidate, directory_aware: stripResults(tables["directory-aware"], flags.detail), basename_only: stripResults(tables["basename-only"], flags.detail), delta }, null, 2)
      : `basis: ${basis}\n${formatReadinessTable(tables["directory-aware"])}${formatReadinessTable(tables["basename-only"])}ready by basename only, not directory-aware:\n${Object.entries(delta).map(([id, names]) => `  ${id}: ${names.length ? names.join(", ") : "(none)"}\n`).join("")}`;
  } else {
    const t = tables[mode];
    body = flags.json ? JSON.stringify({ basis, candidate, ...stripResults(t, flags.detail) }, null, 2) : `basis: ${basis}\n${formatReadinessTable(t)}`;
  }
  const code = await emit(body, flags, io, out, err);
  return code === 0 && refused ? 3 : code;
}

async function verbDiff(flags, io, out, err) {
  if (typeof flags["candidate-dir"] !== "string") { err("give --candidate-dir (a candidate package's templates directory)\n"); return 2; }
  const candidateBasis = flags["candidate-basis"];
  if (candidateBasis !== undefined && !BASES.includes(candidateBasis)) { err(`unknown --candidate-basis "${candidateBasis}" (want ${BASES.join(", ")})\n`); return 2; }
  const loaded = loadCatalog(flags, io);
  if (loaded.error) { err(`${loaded.error}\n`); return 2; }
  const snaps = flags.snapshot === undefined ? { snapshots: {} } : loadSnapshots(flags, io);
  if (snaps.error) { err(`${snaps.error}\n`); return 2; }
  const mode = typeof flags.mode === "string" ? flags.mode : "directory-aware";
  if (mode !== "directory-aware" && mode !== "basename-only") { err(`unknown --mode "${mode}" (want directory-aware, basename-only)\n`); return 2; }
  const stamp = stampFor({ templatesDir: flags["candidate-dir"], basis: candidateBasis, io });
  const next = buildCatalog({ templatesDir: flags["candidate-dir"], apiIds: loaded.src.apiIds, stamp, licenseMap: loaded.src.licenseMap, io });
  const d = diffCatalogs(loaded.catalog, next, { snapshots: snaps.snapshots, mode });
  if (flags.json) return emit(JSON.stringify({ ...d, mode }, null, 2), flags, io, out, err);
  const row = (label, value) => `${label}`.padEnd(38) + value + "\n";
  let t = `from: ${d.basis.from}\nto:   ${d.basis.to}\n`;
  t += row("added", d.added.length) + row("removed", d.removed.length) + row("changed (required files differ)", d.changed.length);
  for (const c of d.changed) t += `  ${c.name}: +${JSON.stringify(c.added)} -${JSON.stringify(c.removed)}\n`;
  for (const [id, r] of Object.entries(d.readiness)) {
    t += row(`would break on ${id}:`, r.regressions.length ? r.regressions.map((x) => x.name).join(", ") : "(none)");
    t += row(`would gain on ${id}:`, r.gains.length ? r.gains.join(", ") : "(none)");
  }
  return emit(t, flags, io, out, err);
}

// Run as a program: `node render/templates-catalog.mjs <verb> ...`, or the whole text piped to `node --input-type=module - <verb> ...`
// (there, argv[1] is "-" and the module has no file, see NO_FILE). Merely importing this file runs nothing.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href || (NO_FILE && process.argv[1] === "-")) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
}
