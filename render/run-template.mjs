// render/run-template.mjs - the node-side half of the offload_run_template tool: list, get, readiness and resolve over the workflow
// templates a ComfyUI tree carries. It is read-only: it never runs a template, converts one, downloads a model, installs a package or
// writes a file, and it prints one JSON object (compact, key order fixed) so the same tree gives the same bytes.
//
//   node render/run-template.mjs list      --comfy-dir DIR [filters]
//   node render/run-template.mjs get       NAME --comfy-dir DIR
//   node render/run-template.mjs readiness NAME --comfy-dir DIR [--snapshot FILE]
//   node render/run-template.mjs resolve   NAME --comfy-dir DIR [--snapshot FILE]
//
// resolve is the gate in front of a run (template-manifest.mjs resolveRun): the typed refusal or Defer of a template this release will not
// run, or the runtime manifest of one it will. The Go door calls it and, on a run, hands the manifest and the template NAME to the
// run-graph runner (comfy-run-graph.mjs --template), which converts the template once ComfyUI is up.
//
// source: --comfy-dir DIR (a ComfyUI tree: its installed package and its comfy_api_nodes), or --templates-dir DIR for list and get alone.
// options: --api-nodes-dir DIR, --license-map FILE (default: templates-license-map.json beside this file), --basis KIND.
// list filters: --kind local|api|custom_nodes --tag TAG --text WORDS --group TITLE --type TYPE --license-class CLASS
//   --runnable true|false (default true) --reads-input true|false --limit N (default 50, at most 500) --offset N.
//
// Exit 0 whenever it answered, a refusal included; 2 for a call it could not read (usage, a snapshot that is not one, a tree with no
// templates package); 1 for a failure of its own.
import { pathToFileURL } from "node:url";
import { loadCatalog, nodeIo, snapshotNode } from "./templates-catalog.mjs";
import { LIST_MAX_LIMIT, getTemplate, listTemplates, readinessFor, resolveRun } from "./template-manifest.mjs";

const USAGE = `usage: run-template <list|get|readiness|resolve> [NAME] --comfy-dir DIR [options]
  list      [--kind K] [--tag T] [--text WORDS] [--group G] [--type Y] [--license-class C] [--runnable true|false] [--reads-input true|false] [--limit N] [--offset N]
  get       NAME            the manifest, verdict and licence class of one template
  readiness NAME            models present or missing, input files, nodes, ComfyUI version, licence verdict (--snapshot FILE stands in for scanning the tree)
  resolve   NAME            the gate in front of a run: a typed refusal or Defer, or the runtime manifest
source: --comfy-dir DIR, or --templates-dir DIR (list and get). Options: --api-nodes-dir DIR, --license-map FILE, --basis KIND.
`;

function parseArgs(argv) {
  const flags = {};
  const positional = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "-h" || a === "--help") { flags.help = true; continue; }
    if (!a.startsWith("--")) { positional.push(a); continue; }
    const key = a.slice(2);
    if (i + 1 >= argv.length) return { error: `--${key} needs a value` };
    flags[key] = argv[++i];
  }
  return { flags, positional };
}

const BOOLEANS = { true: true, false: false };

function boolFlag(flags, name) {
  if (flags[name] === undefined) return { value: undefined };
  if (!Object.hasOwn(BOOLEANS, flags[name])) return { error: `--${name} is true or false, not ${JSON.stringify(flags[name])}` };
  return { value: BOOLEANS[flags[name]] };
}

function intFlag(flags, name, min, max) {
  if (flags[name] === undefined) return { value: undefined };
  const n = Number(flags[name]);
  if (!/^\d+$/.test(String(flags[name])) || !Number.isSafeInteger(n) || n < min || n > max) return { error: `--${name} is a whole number from ${min} to ${max}, not ${JSON.stringify(flags[name])}` };
  return { value: n };
}

function listFilters(flags) {
  const filters = {};
  for (const [flag, key] of [["kind", "kind"], ["tag", "tag"], ["text", "text"], ["group", "group"], ["type", "type"], ["license-class", "license_class"]]) {
    if (flags[flag] !== undefined) filters[key] = flags[flag];
  }
  for (const [flag, key] of [["runnable", "runnable"], ["reads-input", "reads_input"]]) {
    const b = boolFlag(flags, flag);
    if (b.error) return b;
    if (b.value !== undefined) filters[key] = b.value;
  }
  const limit = intFlag(flags, "limit", 1, LIST_MAX_LIMIT);
  if (limit.error) return limit;
  if (limit.value !== undefined) filters.limit = limit.value;
  const offset = intFlag(flags, "offset", 0, Number.MAX_SAFE_INTEGER);
  if (offset.error) return offset;
  if (offset.value !== undefined) filters.offset = offset.value;
  return { filters };
}

// loadSnapshot: --snapshot FILE if given (a saved templates-catalog snapshot), else a fresh scan of --comfy-dir.
function loadSnapshot(flags, io) {
  if (flags.snapshot !== undefined) {
    let snap;
    try { snap = JSON.parse(io.readText(flags.snapshot)); } catch (e) { return { error: `snapshot ${flags.snapshot}: ${e?.message ?? e}` }; }
    if (!snap || typeof snap !== "object" || !snap.inventory || typeof snap.inventory !== "object") return { error: `snapshot ${flags.snapshot} is not a node snapshot (no inventory); make one with templates-catalog.mjs snapshot` };
    return { snapshot: snap };
  }
  if (typeof flags["comfy-dir"] !== "string") return { error: "readiness and resolve need --comfy-dir, or --snapshot FILE and --comfy-dir for the templates" };
  return { snapshot: snapshotNode({ comfyDir: flags["comfy-dir"], io }) };
}

export async function main(argv, { io = nodeIo, out = (s) => process.stdout.write(s), err = (s) => process.stderr.write(s) } = {}) {
  const parsed = parseArgs(argv);
  if (parsed.error) { err(`${parsed.error}\n${USAGE}`); return 2; }
  const { flags, positional } = parsed;
  const verb = positional[0];
  if (flags.help || verb === "help") { out(USAGE); return 0; }
  if (verb === undefined) { err(USAGE); return 2; }
  if (!["list", "get", "readiness", "resolve"].includes(verb)) { err(`unknown verb ${JSON.stringify(verb)}\n${USAGE}`); return 2; }
  const name = positional[1];
  if (verb !== "list" && (typeof name !== "string" || name === "")) { err(`${verb} needs a template NAME\n${USAGE}`); return 2; }
  let filters = {};
  if (verb === "list") {
    const f = listFilters(flags);
    if (f.error) { err(`${f.error}\n`); return 2; }
    filters = f.filters;
  }
  try {
    const loaded = loadCatalog(flags, io);
    if (loaded.error) { err(`${loaded.error}\n`); return 2; }
    const { catalog } = loaded;
    let answer;
    if (verb === "list") {
      answer = listTemplates(catalog, filters);
    } else if (verb === "get") {
      answer = getTemplate(catalog, name);
    } else {
      const row = catalog.templates.find((t) => t.name === name);
      if (!row) {
        answer = getTemplate(catalog, name);
      } else {
        const snap = loadSnapshot(flags, io);
        if (snap.error) { err(`${snap.error}\n`); return 2; }
        answer = verb === "readiness" ? readinessFor(row, snap.snapshot, { io }) : resolveRun(row, snap.snapshot, { io });
      }
    }
    out(`${JSON.stringify(answer)}\n`);
    return 0;
  } catch (e) {
    err(`${verb}: ${e?.message ?? e}\n`);
    return 1;
  }
}

// Run as a program. Merely importing this file runs nothing.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
}
