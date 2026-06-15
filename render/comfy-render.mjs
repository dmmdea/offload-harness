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
  graph = {
    "4":  { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: ckpt } },
    "10": { class_type: "VAELoader", inputs: { vae_name: vae } },
    "5":  { class_type: "EmptyLatentImage", inputs: { width, height, batch_size: 1 } },
    "6":  { class_type: "CLIPTextEncode", inputs: { text: positive, clip: ["4", 1] } },
    "7":  { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["4", 1] } },
    "3":  { class_type: "KSampler", inputs: { seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1, model: ["4", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["5", 0] } },
    "8":  { class_type: "VAEDecode", inputs: { samples: ["3", 0], vae: ["10", 0] } },
    "9":  { class_type: "SaveImage", inputs: { filename_prefix: "render", images: ["8", 0] } },
  };
}

const j = async (url, opts) => { const r = await fetch(url, opts); if (!r.ok) throw new Error(url + " -> " + r.status + " " + (await r.text()).slice(0, 200)); return r.json(); };

async function waitServer() {
  for (let i = 0; i < 90; i++) {
    try { const r = await fetch(API + "/system_stats"); if (r.ok) return true; } catch {}
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
  for (let i = 0; i < 180; i++) { // up to ~6 min (low-VRAM first run is slow)
    await new Promise(r => setTimeout(r, 2000));
    let hist; try { hist = await j(`${API}/history/${prompt_id}`); } catch { continue; }
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
