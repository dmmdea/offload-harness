// render/comfy-run-graph.mjs — the run-graph orchestrator. Unlike the image/video/audio
// runners it does NOT let withGpuSlot manage ComfyUI: it provisions the node manifest
// (packs + models) BEFORE starting ComfyUI, so packs cloned during satisfy are actually
// loaded on ComfyUI's first (and only) start — a post-start restart never loaded them.
// withGpuSlot({comfyManaged:false}) therefore does ONLY the GPU lock + freeLlamaSwap +
// teardown; run-graph owns the ComfyUI lifecycle itself. Ownership is keyed on the
// manifest hash (NOT an ephemeral node pid). DEFER-never-cloud: every failure writes
// {deferred:true, code, ref, detail} to the result and exits 0 (a defer is data, not a crash).
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { withGpuSlot, freeComfy as _freeComfy } from "./gpu-lock.mjs";
import { comfyUp as _comfyUp, ensureComfy as _ensureComfy } from "./comfy-lifecycle.mjs";
import { parseManifest as _parse, manifestHash as _hash } from "./manifest.mjs";
import { satisfyManifest, defaultSatisfyDeps } from "./manifest-satisfy.mjs";
import { preflightGraph } from "./preflight-graph-file.mjs";
import { allOutputsByNode } from "./comfy-output.mjs";
import { readOwner as _readOwner, writeOwner as _writeOwner } from "./comfy-ownership.mjs";

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
// satisfy (disk only) → external-needs-packs guard → start ComfyUI (only if we must) →
// write ownership → preflight (now that packs are loaded) → POST → collect → envelope.
export async function runGraphFlow(args, deps) {
  const { graph, manifest, outDir, resultPath, api, comfyDir, reserveVram } = args;
  const {
    parseManifest = _parse, manifestHash = _hash,
    readOwner = _readOwner, writeOwner = _writeOwner,
    comfyUp, ensureComfy, killComfy, freeComfy, satisfy,
    preflight = preflightGraph,
    postGraph, collect = async (pid) => allOutputsByNode((await postGraph.history(pid)) || {}),
    fetchToDir, writeResult = (p, o) => writeFileSync(p, JSON.stringify(o)),
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

  // Cache short-circuit: our marker records this exact env (manifest hash only — no pid).
  const owner = readOwner(comfyDir);
  const cached = !!(owner && owner.manifestHash === wantHash);

  // Satisfy = disk provisioning only (packs+models). No /object_info gate here — the
  // node-class check now lives in preflight, AFTER ComfyUI is up with the packs.
  let sat = { ok: true, changed: false, unverified: owner?.unverified || [] };
  if (!cached) {
    sat = await satisfy(m);
    if (!sat.ok) return await writeDefer(sat.defer);
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

    // Persist the ownership marker (manifest hash + unverified models — NO pid).
    writeOwner(comfyDir, { manifestHash: wantHash, unverified: sat.unverified || [] });

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
    }
  }
}

// --- CLI wiring (production deps) ------------------------------------------------------
async function main() {
  const argv = process.argv.slice(2); const flags = {};
  for (let i = 0; i < argv.length; i++) if (argv[i].startsWith("--")) { flags[argv[i].slice(2)] = argv[i + 1]; i++; }
  const api = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
  const comfyDir = process.env.COMFY_DIR || "C:/ComfyUI";
  const graph = JSON.parse(readFileSync(flags.graph, "utf8"));
  const manifest = flags.manifest ? JSON.parse(readFileSync(flags.manifest, "utf8")) : { node_packs: [], models: [] };
  const reserveVram = flags["reserve-vram"];

  const j = async (url, opts) => { const r = await fetch(url, opts); if (!r.ok) throw new Error(url + " " + r.status); return r.json(); };
  const comfyPy = process.env.COMFY_PY || join(comfyDir, ".venv/Scripts/python.exe");
  const cmCli = process.env.COMFY_CM_CLI || join(comfyDir, "custom_nodes/ComfyUI-Manager/cm-cli.py");
  const satDeps = defaultSatisfyDeps({ comfyDir, comfyPy, api, cmCli });

  const run = () => runGraphFlow(
    { graph, manifest, outDir: flags["out-dir"] || ".", resultPath: flags.result || "run-graph-result.json", api, comfyDir, reserveVram },
    {
      comfyUp: _comfyUp,
      ensureComfy: _ensureComfy,
      killComfy: (c) => c.kill(),
      freeComfy: _freeComfy,
      satisfy: (mm) => satisfyManifest(mm, satDeps),
      preflight: preflightGraph,
      postGraph: Object.assign(async (g) => j(`${api}/prompt`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ prompt: g, client_id: "run-graph" }) }),
        { history: (pid) => pollHistory(api, pid) }),
      fetchToDir: async (f, dir) => {
        const q = new URLSearchParams({ filename: f.filename, subfolder: f.subfolder, type: f.type });
        const r = await fetch(`${api}/view?` + q); const buf = Buffer.from(await r.arrayBuffer());
        mkdirSync(dir, { recursive: true }); // standalone-mjs path: don't ENOENT on a fresh out-dir
        const p = join(dir, f.filename); writeFileSync(p, buf);
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

async function pollHistory(api, pid) {
  const waitSec = Number(process.env.COMFY_WAIT_SEC || 1800);
  for (let i = 0; i < Math.max(1, Math.ceil(waitSec / 2)); i++) {
    await new Promise((r) => setTimeout(r, 2000));
    let h; try { h = (await (await fetch(`${api}/history/${pid}`)).json())[pid]; } catch { continue; }
    if (!h) continue;
    if (h.status?.status_str === "error") throw new Error("comfy exec error " + JSON.stringify(h.status).slice(0, 300));
    if (h.outputs && Object.keys(h.outputs).length) return h.outputs;
  }
  throw new Error("no outputs in time");
}

// Only run main() when invoked directly (not when imported by the test).
if (process.argv[1] && process.argv[1].endsWith("comfy-run-graph.mjs")) {
  main().catch((e) => { console.error("RUN-GRAPH FAILED:", e.message); process.exit(1); });
}
