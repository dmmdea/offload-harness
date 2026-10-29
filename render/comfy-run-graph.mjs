// render/comfy-run-graph.mjs — the run-graph orchestrator. Unlike the image/video/audio
// runners it does NOT let withGpuSlot manage ComfyUI: it provisions the node manifest
// (packs + models) BEFORE starting ComfyUI, so packs cloned during satisfy are actually
// loaded on ComfyUI's first (and only) start — a post-start restart never loaded them.
// withGpuSlot({comfyManaged:false}) therefore does ONLY the GPU lock + freeLlamaSwap +
// teardown; run-graph owns the ComfyUI lifecycle itself. Ownership is keyed on the
// manifest hash (NOT an ephemeral node pid). DEFER-never-cloud: every failure writes
// {deferred:true, code, ref, detail} to the result and exits 0 (a defer is data, not a crash).
import { readFileSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { writeFileAtomic } from "./atomic-out.mjs";
import { withGpuSlot, freeComfy as _freeComfy } from "./gpu-lock.mjs";
import { comfyUp as _comfyUp, ensureComfy as _ensureComfy, resolveComfyDir, resolveComfyPy, comfyApi, resolveInstance, DEFAULT_COMFY_PORT, fetchSystemArgv } from "./comfy-lifecycle.mjs";
import { freeHarnessInstance } from "./comfy-family.mjs";
import { parseManifest as _parse, manifestHash as _hash } from "./manifest.mjs";
import { satisfyManifest, defaultSatisfyDeps, verifyTofuModels, defaultTofuDeps } from "./manifest-satisfy.mjs";
import { convertForRun, loadTemplate, unlistedModels } from "./template-run.mjs";
import { leaseBeat } from "./lease-progress.mjs";
import { preflightGraph } from "./preflight-graph-file.mjs";
import { allOutputsByNode } from "./comfy-output.mjs";
import { readOwner as _readOwner, writeOwner as _writeOwner } from "./comfy-ownership.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

const __dirname = dirname(fileURLToPath(import.meta.url));

// pngSize: read a PNG's pixel dimensions straight from the buffer (generic — the harness
// never interprets the graph). Layout: 8-byte signature, then the IHDR chunk
// (4-byte length + "IHDR" type), so width is the big-endian u32 at offset 16 and height
// at offset 20. Non-PNG (or too-short) buffers report 0/0.
export function pngSize(buf) {
  if (buf && buf.length >= 24 &&
      buf[0] === 0x89 && buf[1] === 0x50 && buf[2] === 0x4e && buf[3] === 0x47) {
    return { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20) };
  }
  return { width: 0, height: 0 };
}

// runGraphFlow: provision BEFORE start. Deps are injectable for tests; production wiring
// is in main(). Order (spec §3/§4, reshaped): parse → is-comfy-already-up? → cache check →
// satisfy (disk only) → trust on first use → external-needs-packs guard → start ComfyUI (only if we
// must) → write ownership → convert a template (only when one is named) → preflight (now that
// packs are loaded) → POST → collect → envelope.
//
// A TEMPLATE run names a template (args.template) instead of carrying a graph: a template is a UI-format
// workflow, and turning it into the API graph needs the running ComfyUI's /object_info, which exists only
// once ComfyUI is up. So the convert step sits after the start and before preflight, and the graph that
// preflight checks and POST sends is the converted one. Everything around it is this same flow.
export async function runGraphFlow(args, deps) {
  const { manifest, outDir, resultPath, api, comfyDir, reserveVram, template, overrides, seed } = args;
  let { graph } = args;
  const {
    parseManifest = _parse, manifestHash = _hash,
    readOwner = _readOwner, writeOwner = _writeOwner,
    comfyUp, ensureComfy, killComfy, freeComfy, satisfy,
    // packsKeyNow: the sorted `name@commit` key of the packs the manifest names, as they are on disk now. The cache holds the
    // key it stored, so a pack an operator moved, replaced or deleted since is provisioned again instead of trusted.
    packsKeyNow,
    // convert: a template run's step from a template name to the API graph, once ComfyUI is up ({ok, graph} or {ok:false, refusal}).
    // tofuCheck: trust on first use for the models that ask for it, on EVERY run (a cached one too): the cache says the environment
    // was provisioned, not that a file is still the one that was trusted. ({ok, first_seen[]} or {ok:false, defer}.)
    convert, tofuCheck,
    // freeKept: free what the graph left on an instance that was ALREADY UP when this run began, if it
    // is one this harness launched (a kept per-card instance). Without it an arbitrary graph's models
    // stayed in that instance for the next lease's family to be loaded beside (comfy-family.mjs).
    freeKept = async () => {},
    preflight = preflightGraph,
    postGraph, collect = async (pid) => allOutputsByNode((await postGraph.history(pid)) || {}),
    fetchToDir, writeResult = (p, o) => writeFileAtomic(p, JSON.stringify(o)),
  } = deps;

  // A typed DEFER is a valid outcome: write it to the result (data the Go side reads) and return it.
  const writeDefer = async (d) => {
    const out = { deferred: true, code: d.code, ref: d.ref || "", detail: d.detail || "" };
    await writeResult(resultPath, out);
    return out;
  };

  const m = parseManifest(manifest);
  const wantHash = manifestHash(m);

  // Is a ComfyUI already answering that we did NOT start? (external instance)
  const wasUp = await comfyUp(api);

  // Cache short-circuit: our marker records this exact env (manifest hash + the commits the packs were at — no pid). The
  // manifest hash holds no commit, so the pack key is what notices a pack that moved since.
  const owner = readOwner(comfyDir);
  const keyNow = packsKeyNow ? await packsKeyNow(m.node_packs || []) : null;
  const cached = !!(owner && owner.manifestHash === wantHash && (keyNow === null || (owner.packs ?? "") === keyNow));

  // Satisfy = disk provisioning only (packs+models). No /object_info gate here — the
  // node-class check now lives in preflight, AFTER ComfyUI is up with the packs.
  let sat = { ok: true, changed: false, unverified: owner?.unverified || [] };
  if (!cached) {
    sat = await satisfy(m);
    if (!sat.ok) return await writeDefer(sat.defer);
    // Fail-open note from the satisfier (e.g. coherence check skipped on an unchanged env
    // because its subprocess could not spawn) — surfaced on stderr, never fatal.
    if (sat.warning) console.error("SATISFY WARN", sat.warning);
  }

  // Trust on first use: a file that declares no hash is checked against the hash the first run recorded. Before ComfyUI starts, so a
  // refused file costs no start, and whether or not satisfy ran.
  let tofuFirstSeen = [];
  if (tofuCheck) {
    const t = await tofuCheck(m);
    if (!t.ok) return await writeDefer(t.defer);
    tofuFirstSeen = t.first_seen || [];
  }

  // Packs changed under an EXTERNAL ComfyUI we don't own → we can't reload it: DEFER.
  if (sat.changed && wasUp) {
    return await writeDefer({
      code: "EXTERNAL_COMFY_NEEDS_PACKS", ref: "",
      detail: "packs changed but a ComfyUI we do not own is running on " + api,
    });
  }

  let comfyChild = null;
  let ownComfy = false;
  try {
    // Start ComfyUI ONLY if nothing is up. Started AFTER provisioning → packs load on the
    // first (and only) start; no restart is ever needed. If wasUp && !changed we reuse the
    // existing external instance (packs already present) and never touch it (ownComfy stays false).
    if (!wasUp) {
      try { comfyChild = await ensureComfy({ reserveVram }); ownComfy = true; }
      catch (e) { return await writeDefer({ code: "COMFY_START_FAILED", ref: "", detail: String(e.message || e) }); }
    }

    // Persist the ownership marker (manifest hash + pack commits + unverified models — NO pid).
    writeOwner(comfyDir, { manifestHash: wantHash, packs: cached ? (keyNow ?? "") : (sat.packsKey ?? ""), unverified: sat.unverified || [] });

    // A template becomes its API graph here, against the live /object_info of this ComfyUI. A refusal (an unknown node class, a ComfyUI
    // older than the template, an override that names nothing) is a typed defer, and the ComfyUI we started goes down with it.
    if (template) {
      const converted = await convert({ template, overrides, seed });
      if (!converted.ok) {
        return await writeDefer({ code: "TEMPLATE_CONVERT_REFUSED", ref: converted.refusal.code, detail: converted.refusal.message });
      }
      graph = converted.graph;
      // The manifest and the graph are two readers' answers (the catalog's and the converter's). A model file the graph loads that the manifest
      // does not name is one the licence gate, the FLUX bar, trust on first use and the host-RAM figure never saw: defer before preflight, and
      // before anything is posted. An embedding the prompt text names (`embedding:<name>`) is such a file too, as models/embeddings/<name>.
      const unlisted = unlistedModels(graph, m.models);
      if (unlisted.length > 0) {
        return await writeDefer({
          code: "TEMPLATE_MODEL_UNLISTED", ref: unlisted[0],
          detail: `the converted template loads ${unlisted.length} model file(s) its manifest does not name (${unlisted.join(", ")}); the licence gate, the FLUX bar and the trust-on-first-use check read only the files the manifest names, so it is not run`,
        });
      }
    }

    // Preflight now runs against the live /object_info WITH the packs loaded, so it catches
    // missing node classes (NODE_CLASS_MISSING) and unwired required inputs.
    const pf = await preflight(graph);
    if (!pf.ok) {
      return await writeDefer({
        code: pf.unknownClasses.length ? "NODE_CLASS_MISSING" : "PREFLIGHT_MISSING_INPUTS",
        ref: (pf.unknownClasses[0]?.class_type) || (pf.missing[0]?.class_type) || "",
        detail: JSON.stringify(pf),
      });
    }

    // POST + collect node-addressed outputs.
    const { prompt_id } = await postGraph(graph);
    const outputs = await collect(prompt_id);

    // Fetch every output file to outDir; build the envelope (with width/height per file).
    const envelope = { outputs: {}, image_path: null, unverified_models: sat.unverified || [] };
    if (tofuFirstSeen.length > 0) envelope.tofu_first_seen = tofuFirstSeen;
    // A satisfy warning (a fail-open coherence check, or venv conflicts that predate this run) travels in the result, not only on stderr.
    if (sat.warning) envelope.warnings = [sat.warning];
    for (const [nodeId, files] of Object.entries(outputs)) {
      envelope.outputs[nodeId] = [];
      for (const f of files) {
        const rec = await fetchToDir(f, outDir);
        envelope.outputs[nodeId].push({ path: rec.path, type: rec.type, kind: rec.kind, width: rec.width || 0, height: rec.height || 0 });
        if (!envelope.image_path && f.kind === "image") envelope.image_path = rec.path;
      }
    }
    if (!Object.keys(envelope.outputs).length) return await writeDefer({ code: "RUN_ERROR", ref: "", detail: "graph produced no outputs" });
    await writeResult(resultPath, envelope);
    return envelope;
  } catch (e) {
    // Any throw (POST, collect, fetch, writeOwner, writeResult) becomes a typed DEFER so it
    // NEVER escapes as an untyped exit-1 (fix #8).
    return await writeDefer({ code: "RUN_ERROR", ref: "", detail: String(e.message || e) });
  } finally {
    // Zero-warm teardown of OUR instance only. An external ComfyUI we never started is left alone.
    if (ownComfy && comfyChild) {
      try { await freeComfy(); } catch {}
      try { killComfy(comfyChild); } catch {}
    } else if (wasUp) {
      // An instance that was already running and that the harness launched (a kept per-card instance)
      // holds whatever this graph loaded; freeKept decides whose it is, and frees only the harness's.
      try { await freeKept(); } catch {}
    }
  }
}

// instanceDeps: run-graph's own ComfyUI start and free, bound to the instance its api and
// env name. A keyed (per-card) instance is started and freed on ITS endpoint, and so is an unkeyed
// one on ANOTHER endpoint (ensureComfy would otherwise ensure, and the free would drop the models
// of, the default 8188: plan section 7, finding 7); the default instance is called exactly as
// before (no api to ensureComfy, none to freeComfy). Throws COMFY-INSTANCE-INVALID on an env that
// cannot name an instance. `real` is injectable so the wiring is tested by behaviour.
export function instanceDeps(api, env = process.env, real = { ensureComfy: _ensureComfy, freeComfy: _freeComfy }) {
  const instance = resolveInstance({ api, env });
  const own = Boolean(instance.key) || instance.port !== DEFAULT_COMFY_PORT;
  const startOpts = own ? { api } : {};
  const freeArgs = own ? [api] : [];
  return {
    ensureComfy: (o) => real.ensureComfy({ ...o, ...startOpts }),
    freeComfy: () => real.freeComfy(...freeArgs),
  };
}

// --- CLI wiring (production deps) ------------------------------------------------------
async function main() {
  const argv = process.argv.slice(2); const flags = {};
  for (let i = 0; i < argv.length; i++) if (argv[i].startsWith("--")) { flags[argv[i].slice(2)] = argv[i + 1]; i++; }
  // resolveCli throws LOUDLY on a set-but-wrong COMFYUI_PP_CLI (deliberate — explicit
  // config never degrades silently), and an env that cannot name an instance
  // (COMFY-INSTANCE-INVALID) throws too, but run-graph's contract is "every failure is a
  // typed DEFER in the result file + exit 0", so either throw must become a defer here
  // rather than escape as an untyped exit-1 the Go side cannot classify.
  let cli = null; let api; let deps;
  try { api = comfyApi(flags.api); deps = instanceDeps(api); cli = resolveCli(); } catch (e) {
    const out = { deferred: true, code: "RUN_ERROR", ref: "", detail: String(e.message || e) };
    writeFileAtomic(flags.result || "run-graph-result.json", JSON.stringify(out));
    console.error("RUN-GRAPH DEFER", JSON.stringify(out));
    return;
  }
  const comfyDir = resolveComfyDir();
  // A run names a graph (--graph, an API-format file) or a template (--template, a name in this node's installed templates package,
  // converted once ComfyUI is up, with optional --overrides FILE and --seed N). Both is a contradiction.
  const template = flags.template || null;
  if (template && flags.graph) {
    const out = { deferred: true, code: "RUN_ERROR", ref: "", detail: "--graph and --template are exclusive" };
    writeFileAtomic(flags.result || "run-graph-result.json", JSON.stringify(out));
    console.error("RUN-GRAPH DEFER", JSON.stringify(out));
    return;
  }
  let overrides; let seed;
  try {
    if (flags.overrides) overrides = JSON.parse(readFileSync(flags.overrides, "utf8"));
    if (flags.seed !== undefined) {
      seed = Number(flags.seed);
      if (!/^\d+$/.test(String(flags.seed)) || !Number.isSafeInteger(seed)) throw new Error(`--seed is a whole number from 0, not ${JSON.stringify(flags.seed)}`);
    }
  } catch (e) {
    const out = { deferred: true, code: "RUN_ERROR", ref: "", detail: String(e.message || e) };
    writeFileAtomic(flags.result || "run-graph-result.json", JSON.stringify(out));
    console.error("RUN-GRAPH DEFER", JSON.stringify(out));
    return;
  }
  const graph = template ? null : JSON.parse(readFileSync(flags.graph, "utf8"));
  const manifest = flags.manifest ? JSON.parse(readFileSync(flags.manifest, "utf8")) : { node_packs: [], models: [] };
  const reserveVram = flags["reserve-vram"];

  // Shared with the lifecycle module: this line used to hardcode the Windows venv path
  // with no candidate list and no existence check, so run_graph spawned a binary that
  // cannot exist on Linux — advertised to the fleet, broken on arrival.
  const comfyPy = resolveComfyPy(comfyDir);
  const instance = resolveInstance({ api, env: process.env });
  const satDeps = { ...defaultSatisfyDeps({ comfyDir, comfyPy, api }), beat: (line) => leaseBeat(line, { force: true }) };

  const run = () => runGraphFlow(
    { graph, manifest, outDir: flags["out-dir"] || ".", resultPath: flags.result || "run-graph-result.json", api, comfyDir, reserveVram, template, overrides, seed },
    {
      comfyUp: _comfyUp,
      ensureComfy: deps.ensureComfy,
      killComfy: (c) => c.kill(),
      freeComfy: deps.freeComfy,
      freeKept: () => freeHarnessInstance({
        comfyDir, key: instance.key, api: instance.api,
        free: () => deps.freeComfy(), systemArgv: fetchSystemArgv,
      }),
      satisfy: (mm) => satisfyManifest(mm, satDeps),
      packsKeyNow: satDeps.packsKey,
      // Trust on first use for the models that ask for it (a template run's manifest does); a graph run's manifest asks for none.
      tofuCheck: (mm) => verifyTofuModels(mm.models, { ...satDeps, tofu: defaultTofuDeps() }),
      // A template becomes its API graph against THIS ComfyUI's own /object_info and version, which exist only once it is up.
      convert: async ({ template: name, overrides: ov, seed: sd }) => {
        const t = loadTemplate({ comfyDir, name });
        if (!t.ok) return t;
        const [objectInfo, stats] = await Promise.all([getJson(`${api}/object_info`), getJson(`${api}/system_stats`)]);
        const comfyVersion = stats?.system?.comfyui_version ?? stats?.comfyui_version ?? null;
        return convertForRun({ workflow: t.workflow, objectInfo, comfyVersion, minComfyUIVersion: t.min_comfyui_version, seed: sd, overrides: ov });
      },
      preflight: preflightGraph,
      // Submission/polling via the shared comfy-submit.mjs layer: CLI-preferred submit
      // (idempotent lease, typed outcomes, provenance) with byte-identical raw fallback;
      // hardened poll loop (dead-server watchdog — run-graph gains the 2026-07-30 fix
      // the image runner already had). The runGraphFlow dep-injection shape is unchanged,
      // so the flow tests keep driving synthetic postGraph/collect implementations.
      postGraph: Object.assign(
        async (g) => {
          const { promptId } = await submitGraph({ api, graph: g, clientId: "run-graph", cli });
          return { prompt_id: promptId };
        },
        { history: (pid) => pollHistoryShared(api, pid, cli) }),
      fetchToDir: async (f, dir) => {
        const buf = await fetchView({ api, file: f });
        mkdirSync(dir, { recursive: true }); // standalone-mjs path: don't ENOENT on a fresh out-dir
        const p = join(dir, f.filename); writeFileAtomic(p, buf);
        const { width, height } = pngSize(buf);
        return { path: p, type: f.type, kind: f.kind, width, height };
      },
    });

  // run-graph manages ComfyUI itself (provision-before-start), so withGpuSlot does ONLY the
  // GPU lock + freeLlamaSwap + teardown (comfyManaged:false skips its ensureComfy/freeComfy).
  const res = await withGpuSlot({ comfyManaged: false, reserveVram }, run);
  // BOTH success and defer are valid outcomes already written to the result file → exit 0.
  // Only a truly unexpected throw (caught by main().catch below) exits non-zero.
  if (res.deferred) console.error("RUN-GRAPH DEFER", JSON.stringify(res));
  else console.log("WROTE", flags.result);
}

// getJson: one GET of the running ComfyUI as JSON; a non-2xx answer throws (the flow turns a throw into a typed RUN_ERROR defer).
async function getJson(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(`GET ${url} answered ${r.status}`);
  return r.json();
}

// pollHistoryShared: the shared hardened poll loop, returning the outputs map (the shape
// runGraphFlow's collect expects). Terminal bookkeeping (authoritative timing + lease
// release) runs through the CLI when one is resolved — warn-only, after outputs exist.
async function pollHistoryShared(api, pid, cli) {
  const waitSec = Number(process.env.COMFY_WAIT_SEC || 1800);
  const h = await pollOutputs({
    api, promptId: pid, waitSec,
    isDone: (entry) => !!(entry.outputs && Object.keys(entry.outputs).length),
    noOutputMsg: "no outputs in time",
    onExecError: () => finalizeRun({ api, promptId: pid, cli }),
  });
  await finalizeRun({ api, promptId: pid, cli });
  return h.outputs;
}

// Only run main() when invoked directly (not when imported by the test).
if (process.argv[1] && process.argv[1].endsWith("comfy-run-graph.mjs")) {
  main().catch((e) => { console.error("RUN-GRAPH FAILED:", e.message); process.exit(1); });
}
