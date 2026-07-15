// comfy-video.mjs — local image-to-video runner. Animates a still into a short b-roll
// clip via ComfyUI. PRIMARY model Wan 2.2 14B I2V (default; 4-step lightx2v LoRAs = fast);
// SECONDARY HunyuanVideo 1.5 480p I2V (--model hunyuan; needs 3 files not always installed).
// Single-slot GPU-locked + zero-always-warm (frees llama-swap before, frees ComfyUI after).
// Dependency-free (Node 18+). Mirrors render/comfy-render.mjs.
//
// Usage:
//   node render/comfy-video.mjs <out.mp4> <still.(png|jpg)> "<prompt>" \
//        [--model wan|hunyuan] [--frames 49] [--width 832] [--height 480] \
//        [--steps N] [--cfg X] [--hero] [--seed N] [--negative "..."] \
//        [--upscale-model name.pth] [--upscale-width 1920] [--upscale-height 1080] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy]   |   <out.mp4> --graph wf.json
//   --hero: native no-LoRA quality pass (wan; slower, better motion).  --upscale-model:
//   post-decode ESRGAN upscale (+ --upscale-width/height to resize, e.g. 720p->1080p).
import { writeFileSync, copyFileSync, readFileSync } from "node:fs";
import { basename, join } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";
import { buildWan22I2V } from "./wf-wan22-i2v.mjs";
import { buildAceStep } from "./wf-acestep.mjs";

const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (["no-lock", "keep-comfy", "hero"].includes(k)) flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const out = pos[0];
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out) { console.error('usage: node comfy-video.mjs <out.mp4> <still> "<prompt>" [--model hunyuan|wan] [flags]   |   <out.mp4> --graph wf.json'); process.exit(2); }

const j = async (url, opts) => { const r = await fetch(url, opts); if (!r.ok) throw new Error(url + " -> " + r.status + " " + (await r.text()).slice(0, 300)); return r.json(); };

// ComfyUI's LoadImage reads from C:\ComfyUI\input. Stage the still there.
function stageInput(stillPath) {
  const name = "render_in_" + Date.now() + "_" + basename(stillPath);
  copyFileSync(stillPath, join(COMFY_DIR, "input", name));
  return name;
}

async function generate() {
  // model default is wan (Hunyuan needs files absent on this box). Declared in the
  // function scope (NOT at the graph-selection line) so the width/length ternaries
  // below and the log line can read it without a temporal-dead-zone ReferenceError.
  let graph, seed = Number(flags.seed || Math.floor(Math.random() * 1e15)), model = flags.model || "wan";
  if (flags.graph) {
    graph = JSON.parse(readFileSync(flags.graph, "utf8"));
  } else if (flags.model === "ace") {
    // text-to-music (ACE-Step): no still — the prompt (style tags) is the first positional.
    const prompt = pos[1] || flags.prompt;
    if (!prompt) { console.error('error: --model ace needs a "<style tags>" prompt (e.g. "upbeat corporate, 120 bpm")'); process.exit(2); }
    const common = { prompt, seed, seconds: Number(flags.seconds || 30) };
    if (flags.steps) common.steps = Number(flags.steps);
    graph = buildAceStep(common);
  } else {
    const still = pos[1], prompt = pos[2] || flags.prompt;
    if (!still || !prompt) { console.error('error: need <still> and "<prompt>" (or --graph)'); process.exit(2); }
    const imageName = stageInput(still);
    const common = {
      imagePath: imageName, prompt, negative: flags.negative || "", seed,
      width: Number(flags.width || (model === "hunyuan" ? 848 : 832)),
      height: Number(flags.height || 480),
      length: Number(flags.frames || (model === "hunyuan" ? 33 : 49)),
    };
    if (flags.steps) common.steps = Number(flags.steps);
    if (flags.cfg) common.cfg = Number(flags.cfg);
    if (flags.hero) common.hero = true; // native no-LoRA quality pass (wan)
    if (flags["upscale-model"]) {       // optional post-decode upscale (wan)
      common.upscaleModel = flags["upscale-model"];
      if (flags["upscale-width"]) common.upscaleWidth = Number(flags["upscale-width"]);
      if (flags["upscale-height"]) common.upscaleHeight = Number(flags["upscale-height"]);
    }
    graph = model === "hunyuan" ? buildHunyuan15I2V(common) : buildWan22I2V(common);
  }
  const { prompt_id } = await j(API + "/prompt", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ prompt: graph, client_id: "video-" + seed }) });
  console.log("queued", prompt_id, flags.graph ? `(graph ${flags.graph})` : `${model} seed ${seed}`);
  let file = null;
  for (let i = 0; i < 600; i++) { // up to ~20 min (Wan native two-stage is slow)
    await new Promise((r) => setTimeout(r, 2000));
    let hist; try { hist = await j(`${API}/history/${prompt_id}`); } catch { continue; }
    const h = hist[prompt_id]; if (!h) continue;
    if (h.status && h.status.status_str === "error") throw new Error("ComfyUI exec error: " + JSON.stringify(h.status).slice(0, 500));
    file = firstOutputFile(h.outputs); if (file) break;
  }
  if (!file) throw new Error("no video produced in time");
  const q = new URLSearchParams({ filename: file.filename, subfolder: file.subfolder, type: file.type });
  const r = await fetch(`${API}/view?` + q.toString()); if (!r.ok) throw new Error("view fetch " + r.status);
  writeFileSync(out, Buffer.from(await r.arrayBuffer()));
  console.log("WROTE", out);
}

withGpuSlot(
  { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
  generate,
).catch((e) => { console.error("VIDEO GEN FAILED:", e.message); process.exit(1); });
