// render/template-manifest.mjs - the manifest generator of the run-template tool: a row of the templates catalog (templates-catalog.mjs)
// in, a manifest entry, a verdict, a readiness and the gate in front of a run out.
//
// A template is a UI-format workflow plus a list of the files and nodes it needs. This module turns the catalog's reading of it into
// what a run needs to know: which model files (as `models/<class>/<name>`, with the source the template names and the hash it declares,
// if any), which node packs, which licence class, whether it calls a paid partner node, and whether it reads input files. It is pure: a
// function of a catalog row, a node snapshot (templates-catalog.mjs snapshotNode) and an `io` with `exists`. No ComfyUI, no GPU, no
// network, no file written, no model downloaded.
//
// The verdict is the gate of this release. A run is allowed for one kind of template: not a paid API template, not FLUX-family, no custom
// node pack, readable, and a licence class that is permissive (or none: a template with no weights). Everything else is answered with
// a typed class and never run:
//
//   defer   api_paid                       a paid partner-node template spends money; the harness defers it and never falls back to cloud
//   refuse  flux_barred                    FLUX-family weights are barred
//           custom_nodes                   this release provisions no custom node pack
//           license_unknown                the licence is not in the harness's map, or rests on an entry that was inferred, not read:
//                                          unknown is not permissive
//           license_conditional            the weights carry conditions on use (the third licence state)
//           license_non_commercial         the weights are non-commercial
//           template_unreadable            the index lists it and the file cannot be read
//           subgraph_unexpanded            the catalog's walk left a subgraph instance unexpanded (nested past its depth, or a subgraph that
//                                          contains itself), so the models, licence and API signals were read from an incomplete graph
//           bad_model_path                 a model name or class that would leave its class directory
//           models_missing / inputs_missing / comfyui_too_old     (resolve only) what the node lacks, as an exact list
//
// A model downloaded by the satisfier would be a model the harness chose to fetch; this module never produces a source for one: the
// runtime manifest (runtimeManifest) names every model by path and leaves `source_url` empty.
import { INPUT_LOADER_TYPES, MAX_SUBGRAPH_DEPTH, mapLegacyClass, readiness as modelReadiness } from "./templates-catalog.mjs";
import { compareVersions } from "./template-to-graph.mjs";

export { INPUT_LOADER_TYPES };

export const CLASS = Object.freeze({
  API_PAID: "api_paid",
  FLUX_BARRED: "flux_barred",
  CUSTOM_NODES: "custom_nodes",
  LICENSE_UNKNOWN: "license_unknown",
  LICENSE_CONDITIONAL: "license_conditional",
  LICENSE_NON_COMMERCIAL: "license_non_commercial",
  TEMPLATE_UNREADABLE: "template_unreadable",
  SUBGRAPH_UNEXPANDED: "subgraph_unexpanded",
  BAD_MODEL_PATH: "bad_model_path",
  UNKNOWN_TEMPLATE: "unknown_template",
  MODELS_MISSING: "models_missing",
  INPUTS_MISSING: "inputs_missing",
  COMFYUI_TOO_OLD: "comfyui_too_old",
  FLEET_NOT_OFFERED: "fleet_not_offered",
});

const MET = new Set(["present", "present_class_unknown"]);
const SHA256 = /^[0-9a-f]{64}$/i;
const SAFE_CLASS = /^[A-Za-z0-9_]+$/;
const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

const codeOf = (cls) => `TEMPLATE_${cls.toUpperCase()}`;

// ---------------------------------------------------------------------------------------------------------------------
// The manifest entry
// ---------------------------------------------------------------------------------------------------------------------

// A name that stays inside its class directory: no absolute path, no drive, no backslash trick, no ".." segment.
function safeModelName(name) {
  if (typeof name !== "string" || name === "" || name.includes("\0")) return false;
  const n = name.replace(/\\/g, "/");
  if (n.startsWith("/") || /^[A-Za-z]:/.test(n)) return false;
  return !n.split("/").some((seg) => seg === ".." || seg === "" || seg === ".");
}

function modelEntry(m) {
  const directory = typeof m.directory === "string" && m.directory !== "" ? mapLegacyClass(m.directory) : null;
  const declared = typeof m.hash === "string" && /^sha-?256$/i.test(m.hash_type ?? "") && SHA256.test(m.hash) ? m.hash.toLowerCase() : null;
  return {
    name: m.name,
    directory,
    path: directory ? `models/${directory}/${m.name}` : null,
    source_url: m.url ?? "",
    sha256: declared,
    trust: declared ? "declared" : "tofu",
    node_types: Array.isArray(m.node_types) ? m.node_types : [],
    note_size_bytes: m.note_size_bytes ?? null,
  };
}

// manifestFor: everything a run or a reader needs to know about one template, from its catalog row.
export function manifestFor(row) {
  const inputs = Array.isArray(row.input_files) ? row.input_files : [];
  return {
    schema_version: 1,
    template: row.name,
    title: row.title ?? null,
    kind: row.kind,
    api: Boolean(row.api?.flag),
    min_comfyui_version: row.min_comfyui_version ?? null,
    license: {
      class: row.license?.class ?? "unknown",
      class_strict: row.license?.class_strict ?? row.license?.class ?? "unknown",
      repos: row.license?.repos ?? [],
      unmapped_repos: row.license?.unmapped_repos ?? [],
      unresolved_models: row.license?.unresolved_models ?? 0,
    },
    reads_input: inputs.length > 0,
    input_files: inputs,
    custom_packs: row.custom_packs ?? [],
    node_packs: [],
    flux_barred: Boolean(row.flux?.barred),
    models: (row.models ?? []).map(modelEntry),
    models_bypassed: (row.models_inactive ?? []).length,
    param_surface: row.param_surface ?? [],
    load_error: row.load_error ?? null,
  };
}

// ---------------------------------------------------------------------------------------------------------------------
// The verdict
// ---------------------------------------------------------------------------------------------------------------------

// licenseVerdict: "permitted" for a permissive class or none (no weights), else the licence refusal class. A permissive class that rests
// on an inferred map entry (class_strict says unknown) is read as unknown, and so is a class that is unknown.
function licenseVerdict(row) {
  const cls = row.license?.class ?? "unknown";
  if (cls === "conditional") return CLASS.LICENSE_CONDITIONAL;
  if (cls === "non_commercial") return CLASS.LICENSE_NON_COMMERCIAL;
  if (cls === "unknown" || (row.license?.class_strict ?? cls) === "unknown") return CLASS.LICENSE_UNKNOWN;
  return "permitted";
}

const DETAIL = {
  [CLASS.API_PAID]: "a paid partner-node template spends money; the harness defers it and never falls back to cloud (ADR 0001)",
  [CLASS.FLUX_BARRED]: "FLUX-family weights are barred (ADR 0011)",
  [CLASS.CUSTOM_NODES]: "this release provisions no custom node pack",
  [CLASS.LICENSE_UNKNOWN]: "the licence of the weights is not in the harness's licence map, or rests on an entry that was inferred and not read; unknown is not permissive",
  [CLASS.LICENSE_CONDITIONAL]: "the weights carry conditions on use (the conditional licence state); this release runs permissive weights only",
  [CLASS.LICENSE_NON_COMMERCIAL]: "the weights are non-commercial (ADR 0058); this release runs permissive weights only",
  [CLASS.TEMPLATE_UNREADABLE]: "the template is listed in the index and its file cannot be read",
  [CLASS.SUBGRAPH_UNEXPANDED]: `the template nests a subgraph more than ${MAX_SUBGRAPH_DEPTH} levels deep, or has a subgraph that contains itself: the catalog did not expand it, so the template's models, licence and API signals were read from an incomplete graph, and this release refuses it rather than run on a list that may be short`,
  [CLASS.BAD_MODEL_PATH]: "a model name or class directory of the template would leave its class directory",
};

// verdictFor: what this release does with the template. `reasons` lists every class that applies, in this order; `class` is the first.
// A graph the walk could not finish comes right after an unreadable file and before every check that reads the graph: the API, FLUX,
// custom-pack, model-path and licence answers below were all computed from a leaf set that may lack a loader.
export function verdictFor(row) {
  const reasons = [];
  const unexpanded = row.graph?.unexpanded ?? 0;
  if (row.load_error) reasons.push(CLASS.TEMPLATE_UNREADABLE);
  if (unexpanded > 0) reasons.push(CLASS.SUBGRAPH_UNEXPANDED);
  if (row.kind === "api" || row.api?.flag) reasons.push(CLASS.API_PAID);
  if (row.flux?.barred) reasons.push(CLASS.FLUX_BARRED);
  if (row.kind === "custom_nodes" || (row.custom_packs ?? []).length > 0 || (row.requires_custom_nodes ?? []).length > 0) reasons.push(CLASS.CUSTOM_NODES);
  if ((row.models ?? []).some((m) => !safeModelName(m.name) || (m.directory != null && !SAFE_CLASS.test(m.directory)))) reasons.push(CLASS.BAD_MODEL_PATH);
  const lic = licenseVerdict(row);
  if (lic !== "permitted") reasons.push(lic);
  if (reasons.length === 0) return { outcome: "run", class: null, code: null, reasons: [], detail: "" };
  const cls = reasons[0];
  const detail = cls === CLASS.TEMPLATE_UNREADABLE ? `${DETAIL[cls]}: ${row.load_error}` : cls === CLASS.SUBGRAPH_UNEXPANDED ? `${DETAIL[cls]} (${unexpanded} subgraph instance(s) left unexpanded)` : cls === CLASS.CUSTOM_NODES && (row.custom_packs ?? []).length > 0 ? `${DETAIL[cls]} (the template needs ${row.custom_packs.join(", ")})` : DETAIL[cls];
  return { outcome: cls === CLASS.API_PAID ? "defer" : "refuse", class: cls, code: codeOf(cls), reasons, detail };
}

// typed: the answer for a template that does not run. A Defer says deferred:true (do it another way, never in the cloud); a refusal says
// refused:true. Both carry the class and the code, and neither carries a manifest.
function typed(outcome, cls, template, detail, extra = {}) {
  return { ok: false, outcome, ...(outcome === "defer" ? { deferred: true } : { refused: true }), class: cls, code: codeOf(cls), template, detail, ...extra };
}

const verdictAnswer = (v, template) => typed(v.outcome, v.class, template, v.detail, { reasons: v.reasons });

// ---------------------------------------------------------------------------------------------------------------------
// list and get
// ---------------------------------------------------------------------------------------------------------------------

const basisOf = (catalog) => catalog.stamp?.label ?? "unstamped catalog (no version information)";

function summaryRow(row, v) {
  return {
    name: row.name,
    title: row.title ?? null,
    kind: row.kind,
    group: row.group?.title ?? null,
    type: row.group?.type ?? null,
    media_type: row.media_type ?? null,
    tags: row.tags ?? [],
    license_class: row.license?.class ?? "unknown",
    runnable: v.outcome === "run",
    outcome: v.outcome,
    class: v.class,
    reads_input: (row.input_files ?? []).length > 0,
    models: (row.models ?? []).length,
    min_comfyui_version: row.min_comfyui_version ?? null,
    size_bytes: row.size_bytes ?? null,
  };
}

export const LIST_DEFAULT_LIMIT = 50;
export const LIST_MAX_LIMIT = 500;

// listTemplates: the catalog, filtered and paged, sorted by name. `runnable` defaults to true (what this release can run); false lists the
// refused ones. `counts` is over the whole catalog, so a filter never hides how many templates each refusal class holds.
export function listTemplates(catalog, filters = {}) {
  const rows = catalog.templates.map((row) => ({ row, v: verdictFor(row) })).sort((a, b) => cmp(a.row.name, b.row.name));
  const by_class = {};
  for (const { v } of rows) if (v.class) by_class[v.class] = (by_class[v.class] ?? 0) + 1;
  const counts = { total: rows.length, runnable: rows.filter(({ v }) => v.outcome === "run").length, by_class: Object.fromEntries(Object.entries(by_class).sort(([a], [b]) => cmp(a, b))) };
  const wantRunnable = filters.runnable === undefined || filters.runnable === null ? true : Boolean(filters.runnable);
  const text = typeof filters.text === "string" && filters.text !== "" ? filters.text.toLowerCase() : null;
  const group = typeof filters.group === "string" && filters.group !== "" ? filters.group.toLowerCase() : null;
  const matched = rows.filter(({ row, v }) => {
    if ((v.outcome === "run") !== wantRunnable) return false;
    if (filters.kind && row.kind !== filters.kind) return false;
    if (group && String(row.group?.title ?? "").toLowerCase() !== group) return false;
    if (filters.type && row.group?.type !== filters.type) return false;
    if (filters.tag && !(row.tags ?? []).includes(filters.tag)) return false;
    if (text && !`${row.name} ${row.title ?? ""} ${row.description ?? ""}`.toLowerCase().includes(text)) return false;
    if (filters.license_class && row.license?.class !== filters.license_class) return false;
    if (filters.reads_input !== undefined && filters.reads_input !== null && ((row.input_files ?? []).length > 0) !== Boolean(filters.reads_input)) return false;
    return true;
  });
  const limit = Math.min(Math.max(Number.isInteger(filters.limit) ? filters.limit : LIST_DEFAULT_LIMIT, 1), LIST_MAX_LIMIT);
  const offset = Math.max(Number.isInteger(filters.offset) ? filters.offset : 0, 0);
  const page = matched.slice(offset, offset + limit);
  return { ok: true, basis: basisOf(catalog), counts, matched: matched.length, returned: page.length, offset, limit, templates: page.map(({ row, v }) => summaryRow(row, v)) };
}

function unknownTemplate(catalog, name) {
  return typed("refuse", CLASS.UNKNOWN_TEMPLATE, name, `no template named ${name} in this catalog (${basisOf(catalog)})`);
}

// getTemplate: one template's manifest, verdict and licence class; an unknown name is a typed refusal.
export function getTemplate(catalog, name) {
  const row = catalog.templates.find((t) => t.name === name);
  if (!row) return unknownTemplate(catalog, name);
  const v = verdictFor(row);
  return { ok: true, template: name, basis: basisOf(catalog), runnable: v.outcome === "run", verdict: v, manifest: manifestFor(row) };
}

// ---------------------------------------------------------------------------------------------------------------------
// readiness
// ---------------------------------------------------------------------------------------------------------------------

// splitInputName: "ref.png" is a file of the input directory, "ref.png [output]" one of the output directory, "[temp]" one of the
// temp directory (not checked: nothing stages a file there).
function splitInputName(name) {
  const m = /^(.*?)\s*\[(input|output|temp)\]$/.exec(name);
  return m ? { file: m[1], dir: m[2] } : { file: name, dir: "input" };
}

const joinPath = (...parts) => parts.join("/").replace(/\\/g, "/").replace(/\/+/g, "/");

function inputStatus(entry, snapshot, io) {
  if (typeof entry.name !== "string" || entry.name === "") return { name: null, dir: "input", node_type: entry.node_type, status: "unchecked" };
  const { file, dir } = splitInputName(entry.name);
  const base = snapshot.launch?.base_directory ?? snapshot.comfy_dir;
  const root = dir === "input" ? joinPath(base, "input") : dir === "output" ? (snapshot.launch?.output_directory ?? joinPath(base, "output")) : null;
  const status = root === null ? "unchecked" : io.exists(joinPath(root, file)) ? "present" : "missing";
  return { name: file, dir, node_type: entry.node_type, status };
}

function comfyStatus(row, snapshot) {
  const version = snapshot.comfyui_version ?? null;
  const min = row.min_comfyui_version ?? null;
  if (!min) return { version, min_version: null, status: "no_minimum" };
  if (!version) return { version, min_version: min, status: "unknown_version" };
  try {
    return { version, min_version: min, status: compareVersions(version, min) < 0 ? "too_old" : "ok" };
  } catch {
    return { version, min_version: min, status: "unknown_version" };
  }
}

// resolvedPath: where the satisfier is told to look for a model: the class directory the template names, else the class directory the
// file was found in (an unannotated loader widget names none). An embedding the prompt text names is the file ComfyUI would load for
// that name on this node (`resolved`: the name as written, else with .safetensors, .pt or .bin appended), so the runtime manifest names a
// real file the satisfier, the trust-on-first-use check and the host-RAM figure can find, and the runner's comparison can match.
function resolvedPath(m, req) {
  if (req.resolved) return `models/${m.directory}/${req.resolved}`;
  if (m.path) return m.path;
  const hit = req.status === "present_class_unknown" ? req.found_in?.[0] : null;
  return hit ? `models/${hit.class}/${hit.rel}` : null;
}

// readinessFor: whether the node holds what the template needs, beside the verdict. Reads the snapshot's model inventory (directory-aware:
// a file is usable only in a directory ComfyUI would offer it from), the input directory through `io`, and ComfyUI's version.
export function readinessFor(row, snapshot, { io }) {
  const manifest = manifestFor(row);
  const verdict = verdictFor(row);
  // embedding_refs are the prompt names an annotation of the embeddings class covers (the catalog records them): readiness checks that the
  // annotated file is the one ComfyUI opens for each. The manifest and the row list the same models in the same order.
  const checked = modelReadiness(manifest.models.map((m, i) => ({ name: m.name, directory: m.directory, embedding_refs: row.models?.[i]?.embedding_refs })), snapshot.inventory, { mode: "directory-aware" });
  const models = manifest.models.map((m, i) => {
    const req = checked.requirements[i];
    return {
      name: m.name, directory: m.directory, path: m.path, status: req.status, trust: m.trust,
      ...(req.found ? { found: req.found } : {}), ...(req.found_in ? { found_in: req.found_in } : {}),
      resolved_path: MET.has(req.status) ? resolvedPath(m, req) : null,
    };
  });
  const models_missing = models.filter((m) => !MET.has(m.status)).map((m) => m.path ?? m.name);
  const inputs = manifest.input_files.map((e) => inputStatus(e, snapshot, io));
  const inputs_missing = inputs.filter((i) => i.status === "missing").map((i) => i.name);
  const comfyui = comfyStatus(row, snapshot);
  const lic = licenseVerdict(row);
  const blocking = [
    ...(verdict.class ? [verdict.class] : []),
    ...(models_missing.length > 0 ? [CLASS.MODELS_MISSING] : []),
    ...(inputs_missing.length > 0 ? [CLASS.INPUTS_MISSING] : []),
    ...(comfyui.status === "too_old" ? [CLASS.COMFYUI_TOO_OLD] : []),
  ];
  return {
    ok: true,
    template: row.name,
    ready: blocking.length === 0,
    blocking,
    verdict: { outcome: verdict.outcome, class: verdict.class },
    license: { class: manifest.license.class, class_strict: manifest.license.class_strict, verdict: lic },
    models,
    models_missing,
    inputs,
    inputs_missing,
    nodes: { status: manifest.custom_packs.length > 0 ? "packs_required" : "core_only", custom_packs: manifest.custom_packs },
    comfyui,
  };
}

// ---------------------------------------------------------------------------------------------------------------------
// resolve: the gate in front of a run
// ---------------------------------------------------------------------------------------------------------------------

// The sentence that says why the fleet was not asked. Every run of this release is on this machine's lane: the fleet door takes an
// API-format graph, and a template is converted on the node that runs it, with that node's own listing of its nodes. A template that
// reads input files is local for a reason of its own: input files carry generic names that two machines share with different bytes.
export function fleetNote(inputFiles) {
  const named = inputFiles.map((i) => i.name).filter(Boolean);
  if (inputFiles.length === 0) {
    return "this machine runs a template on its own lane in this release; the fleet door takes an API-format graph, and a template is converted on the node that runs it";
  }
  const shown = named.length > 3 ? `${named.slice(0, 3).join(", ")} and ${named.length - 3} more` : named.join(", ");
  return `this machine did not offer the template to the fleet: it reads input files (${shown || "unnamed"}), and a node can match an input file only by name, so a node's same-named file of other bytes would run in its place; the template runs on this machine's own lane`;
}

// runtimeManifest: the manifest the run-graph satisfier is handed. Every model is named by the path it was found at and carries no
// source: a run never fetches a model, so a file that goes missing between the check and the run is a typed defer, not a download. A
// declared hash is the satisfier's to check; a model with none is trusted on first use.
export function runtimeManifest(row, models) {
  return {
    schema_version: 1,
    workflow: row.name,
    comfyui_min_version: row.min_comfyui_version ?? "",
    node_packs: [],
    models: models.map((m) => ({ path: m.resolved_path, source_url: "", sha256: m.sha256, trust: m.trust })),
  };
}

// resolveRun: the verdict, then what the node lacks, then the manifest. The first thing that stops a run is the answer, typed.
export function resolveRun(row, snapshot, { io }) {
  const verdict = verdictFor(row);
  if (verdict.outcome !== "run") return verdictAnswer(verdict, row.name);
  const ready = readinessFor(row, snapshot, { io });
  if (ready.models_missing.length > 0) {
    // An embedding the node holds more than once, or one a file ComfyUI tries first shadows, is not missing; it is refused because the file
    // ComfyUI opens for it is not the one file the manifest would name (it picks the directory by set order), and the entry says which.
    const unclear = ready.models.filter((m) => m.status === "ambiguous" || m.status === "shadowed");
    const why = unclear.length === 0 ? "" : `; ${unclear.length} of them ${unclear.length === 1 ? "is" : "are"} held in more than one directory or shadowed by a file ComfyUI tries first, so the file ComfyUI opens is not the one file the manifest would name (${unclear.map((m) => m.path ?? m.name).join(", ")})`;
    return typed("refuse", CLASS.MODELS_MISSING, row.name, `${ready.models_missing.length} model file(s) are not where ComfyUI would load them from; this release never downloads a model${why}`, { missing: ready.models_missing });
  }
  if (ready.inputs_missing.length > 0) {
    return typed("refuse", CLASS.INPUTS_MISSING, row.name, `${ready.inputs_missing.length} input file(s) the template reads are not staged in the input directory`, { missing: ready.inputs_missing });
  }
  if (ready.comfyui.status === "too_old") {
    return typed("refuse", CLASS.COMFYUI_TOO_OLD, row.name, `the template needs ComfyUI ${ready.comfyui.min_version} and this node runs ${ready.comfyui.version}`, { version: ready.comfyui.version, min_version: ready.comfyui.min_version });
  }
  const manifest = manifestFor(row);
  const models = ready.models.map((m, i) => ({ ...m, sha256: manifest.models[i].sha256 }));
  return {
    ok: true,
    outcome: "run",
    template: row.name,
    license: { class: manifest.license.class, verdict: "permitted" },
    reads_input: manifest.reads_input,
    input_files: manifest.input_files,
    local_only: manifest.reads_input,
    fleet_note: fleetNote(manifest.input_files),
    min_comfyui_version: manifest.min_comfyui_version,
    tofu_models: models.filter((m) => m.trust === "tofu").map((m) => m.resolved_path),
    runtime_manifest: runtimeManifest(row, models),
  };
}
