// comfy-render.mjs — a general image-generation runner for the local ComfyUI HTTP API.
//
// Part of the local-offload harness's vision side: the harness READS/assesses images
// with the Qwen3-VL tier (`local-offload vqa|ocr|extract-image|assess-image`); this
// standalone tool GENERATES them. It bakes in NO style, prompt, negative, model, or
// tuning — everything is a flag (or your own workflow), so it can be used as broadly
// or as narrowly as you like.
//
// Two modes:
//   1. --graph <workflow.json>   POST an arbitrary ComfyUI API-format workflow as-is
//                                (full control: any nodes, any model, any pipeline).
//   2. (default) a minimal SDXL text2img graph, fully parameterized via flags.
// Either way it POSTs the graph, polls /history, fetches the first output image, and
// writes it to <out>.
//
// Why SDXL for the built-in convenience graph: SDXL honors real NEGATIVE prompts at
// normal CFG, so hard exclusions (e.g. "no people / no text") are enforceable when you
// want them — pass them via --negative. Pair with `local-offload assess-image` to QA a
// render against such exclusions. (Nothing here assumes you want them; the default
// negative is empty.)
//
// Requires: Node 18+ (built-in fetch) and a running ComfyUI (default :8188). No npm deps.
//
// Usage:
//   node comfy-render.mjs <out.png> "<prompt>" [seed] [width] [height] \
//        [--negative "..."] [--ckpt name.safetensors] [--vae name.safetensors] \
//        [--steps 30] [--cfg 7] [--sampler dpmpp_2m] [--scheduler karras] \
//        [--api http://127.0.0.1:8188]
//   node comfy-render.mjs <out.png> --graph my-workflow.json [--api ...]

import { readFileSync, writeFileSync } from "node:fs";
import { buildHiDreamO1 } from "./wf-hidream-o1.mjs";

const argv = process.argv.slice(2);
const pos = [];
const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) { flags[argv[i].slice(2)] = argv[i + 1]; i++; }
  else pos.push(argv[i]);
}

const out = pos[0];
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out) { console.error('usage: node comfy-render.mjs <out.png> "<prompt>" [seed] [w] [h] [flags]   |   <out.png> --graph wf.json'); process.exit(2); }

// Build the graph: either the caller's full workflow, or a parameterized SDXL text2img.
let graph;
let seed = Number(pos[2] || flags.seed || Math.floor(Math.random() * 1e15));
let width = Number(pos[3] || flags.width || 1024);
let height = Number(pos[4] || flags.height || 1024);

if (flags.graph) {
  graph = JSON.parse(readFileSync(flags.graph, "utf8"));
} else if (flags.family === "hidream-o1" || flags.family === "hidream-o1-dev") {
  // Model-family-correct graph (quality-first): the generic SDXL-shaped KSampler graph
  // is WRONG for HiDream-O1 (pixel-space DiT — wrong latent node, missing ModelNoiseScale
  // + patch-seam smoothing, off-distribution below ~4MP). --family selects the graph the
  // model was actually shipped with; which family a checkpoint is belongs to per-machine
  // config (imagegen_family), never shared code.
  const positive = pos[1] || flags.prompt || "";
  if (!positive) { console.error("error: a prompt is required (positional or --prompt), unless you pass --graph"); process.exit(2); }
  if (!flags.ckpt && !process.env.COMFY_CKPT) { console.error("error: --family hidream-o1 requires --ckpt (the o1 checkpoint filename)"); process.exit(2); }
  // O1 native resolution is 2048x2048 (trained ~4MP): positional/flag dims override,
  // otherwise the builder's native default applies — NOT the SDXL 1024 default.
  width = Number(pos[3] || flags.width || 2048);
  height = Number(pos[4] || flags.height || 2048);
  graph = buildHiDreamO1({
    prompt: positive, negative: flags.negative || "",
    ckpt: flags.ckpt || process.env.COMFY_CKPT,
    variant: flags.family === "hidream-o1-dev" ? "dev" : "base",
    width, height, seed,
    steps: Number(flags.steps || 0), cfg: Number(flags.cfg || 0),
    sampler: flags.sampler || "",
  });
} else {
  const positive = pos[1] || flags.prompt || "";
  if (!positive) { console.error("error: a prompt is required (positional or --prompt), unless you pass --graph"); process.exit(2); }
  const negative = flags.negative || "";                 // neutral default: no baked exclusions
  const ckpt = flags.ckpt || process.env.COMFY_CKPT || "RealVisXL_V5.0_fp16.safetensors";
  const vae = flags.vae || process.env.COMFY_VAE || "sdxl_vae.safetensors";
  const steps = Number(flags.steps || 30);
  const cfg = Number(flags.cfg || 7);
  const sampler = flags.sampler || "dpmpp_2m";
  const scheduler = flags.scheduler || "karras";
  // The graph shape is the same for SDXL and for a DiT (HiDream); what differs is
  // WHERE the VAE comes from. `--vae builtin` decodes with the VAE that the CHECKPOINT
  // LOADER supplies (its 3rd output, ["4",2] — verified against /object_info:
  // CheckpointLoaderSimple.output = [MODEL, CLIP, VAE]) instead of loading a standalone
  // one. That is REQUIRED for HiDream: its .safetensors carries NO VAE weights at all
  // (1474 tensors, all DiT/text/vision — it is pixel-space), so a standalone 4-channel
  // sdxl_vae cannot decode its output. Any other --vae value keeps the standalone
  // VAELoader (SDXL's default path), so callers that don't pass --vae are unaffected.
  const builtinVAE = ["builtin", "none", "checkpoint"].includes(String(vae).toLowerCase());
  graph = {
    "4":  { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: ckpt } },
    "5":  { class_type: "EmptyLatentImage", inputs: { width, height, batch_size: 1 } },
    "6":  { class_type: "CLIPTextEncode", inputs: { text: positive, clip: ["4", 1] } },
    "7":  { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["4", 1] } },
    "3":  { class_type: "KSampler", inputs: { seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1, model: ["4", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["5", 0] } },
    "8":  { class_type: "VAEDecode", inputs: { samples: ["3", 0], vae: builtinVAE ? ["4", 2] : ["10", 0] } },
    "9":  { class_type: "SaveImage", inputs: { filename_prefix: "render", images: ["8", 0] } },
  };
  if (!builtinVAE) graph["10"] = { class_type: "VAELoader", inputs: { vae_name: vae } };
}

const j = async (url, opts) => { const r = await fetch(url, opts); if (!r.ok) { const e = new Error(url + " -> " + r.status + " " + (await r.text()).slice(0, 200)); e.httpStatus = r.status; throw e; } return r.json(); };

async function waitServer() {
  for (let i = 0; i < 90; i++) {
    // Per-probe abort: a wedged-but-listening server hangs sockets; without a signal the
    // fetch would stall this loop far past its intended ~3min cap.
    try { const r = await fetch(API + "/system_stats", { signal: AbortSignal.timeout(8000) }); if (r.ok) return true; } catch {}
    await new Promise(r => setTimeout(r, 2000));
  }
  throw new Error("ComfyUI not reachable on " + API + " after ~3min");
}

async function main() {
  await waitServer();
  const { prompt_id } = await j(API + "/prompt", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ prompt: graph, client_id: "render-" + seed }),
  });
  console.log("queued", prompt_id, flags.graph ? `(graph: ${flags.graph})` : `seed ${seed} ${width}x${height}`);
  let img = null;
  // Poll budget: quality-first renders (e.g. HiDream-O1 bf16 at native 2048, 40-step
  // SDE, RAM-offloaded) legitimately run far beyond the old ~6-min ceiling. Default
  // 30 min; the Go harness passes COMFY_WAIT_SEC aligned to its own timeout and its
  // process-tree kill remains the hard stop.
  const waitSec = Number(flags["wait-sec"] || process.env.COMFY_WAIT_SEC || 1800);
  // Dead-server watchdog (2026-07-30): a ComfyUI that wedges MID-render (queue accepted,
  // then the server stops answering — model loaded, 0% util, HTTP dead) used to burn the
  // ENTIRE quality-first budget above while the Go side held the exclusive GPU slot, so
  // every later job on the node bounced with "gpu busy" until a manual process restart.
  // "Not reachable" is not "not finished": consecutive FAILED polls (fetch threw) abort
  // early and release the slot in seconds. A slow render on a HEALTHY server still
  // answers /history (with no output yet), which resets the counter — the long budget
  // continues to govern that case, and the Go process-tree kill remains the hard stop.
  const deadRaw = Number(process.env.COMFY_DEAD_SEC);
  const deadSec = Number.isFinite(deadRaw) ? Math.max(10, deadRaw) : 240;
  let lastAnswerAt = Date.now();
  let prevTickAt = Date.now();
  for (let i = 0; i < Math.max(1, Math.ceil(waitSec / 2)); i++) {
    await new Promise(r => setTimeout(r, 2000));
    // Suspend/resume fence (this fleet closes lids mid-render by design): a timer jump
    // means the MACHINE slept, not the server — do not count that time as dead.
    if (Date.now() - prevTickAt > 120_000) lastAnswerAt = Date.now();
    prevTickAt = Date.now();
    let hist;
    // Per-poll abort (30s): sockets that HANG (wedged-but-listening server) must count
    // as unreachable time too, or the watchdog goes blind exactly when it is needed —
    // generous because a swap-thrashed quality render answers slowly but honestly. The
    // counter is WALL time since the last ANSWER of any kind: an HTTP error status IS
    // an answer (server alive), only network/abort failures accrue dead time.
    try { hist = await j(`${API}/history/${prompt_id}`, { signal: AbortSignal.timeout(30_000) }); } catch (e) {
      if (e && e.httpStatus) { lastAnswerAt = Date.now(); continue; }
      const deadFor = Math.floor((Date.now() - lastAnswerAt) / 1000);
      if (deadFor >= deadSec) {
        throw new Error(`ComfyUI stopped answering mid-render (unreachable ${deadFor}s, COMFY_DEAD_SEC=${deadSec}); aborting early to release the GPU slot`);
      }
      continue;
    }
    lastAnswerAt = Date.now();
    const h = hist[prompt_id];
    if (!h) continue;
    if (h.status && h.status.status_str === "error") throw new Error("ComfyUI exec error: " + JSON.stringify(h.status).slice(0, 400));
    for (const node of Object.values(h.outputs || {})) { if (node.images && node.images[0]) { img = node.images[0]; break; } }
    if (img) break;
  }
  if (!img) throw new Error("no image produced in time");
  const q = new URLSearchParams({ filename: img.filename, subfolder: img.subfolder || "", type: img.type || "output" });
  const r = await fetch(`${API}/view?` + q.toString());
  if (!r.ok) throw new Error("view fetch " + r.status);
  const buf = Buffer.from(await r.arrayBuffer());
  writeFileSync(out, buf);
  console.log("WROTE", out, buf.length, "bytes");
}
main().catch(e => { console.error("RENDER FAILED:", e.message); process.exit(1); });
