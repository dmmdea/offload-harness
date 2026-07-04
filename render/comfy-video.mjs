// comfy-video.mjs — local image-to-video runner. Animates a still into a short b-roll
// clip via ComfyUI. PRIMARY model HunyuanVideo 1.5 480p I2V (cfg_distilled, 8GB-friendly);
// SECONDARY Wan 2.2 14B I2V (--model wan, photoreal hero shots, needs the GPU freed).
// Single-slot GPU-locked + zero-always-warm (frees llama-swap before, frees ComfyUI after).
// Dependency-free (Node 18+). Mirrors render/comfy-render.mjs.
//
// Usage:
//   node render/comfy-video.mjs <out.mp4> <still.(png|jpg)> "<prompt>" \
//        [--model hunyuan|wan] [--frames 33] [--width 848] [--height 480] \
//        [--steps N] [--seed N] [--negative "..."] [--api http://127.0.0.1:8188] \
//        [--no-lock] [--keep-comfy]   |   <out.mp4> --graph wf.json
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
    if (["no-lock", "keep-comfy"].includes(k)) flags[k] = true;
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
  let graph, seed = Number(flags.seed || Math.floor(Math.random() * 1e15));
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
      width: Number(flags.width || (flags.model === "wan" ? 832 : 848)),
      height: Number(flags.height || 480),
      length: Number(flags.frames || (flags.model === "wan" ? 49 : 33)),
    };
    if (flags.steps) common.steps = Number(flags.steps);
    graph = flags.model === "wan" ? buildWan22I2V(common) : buildHunyuan15I2V(common);
  }
  const { prompt_id } = await j(API + "/prompt", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ prompt: graph, client_id: "video-" + seed }) });
  console.log("queued", prompt_id, flags.graph ? `(graph ${flags.graph})` : `${flags.model || "hunyuan"} seed ${seed}`);
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
