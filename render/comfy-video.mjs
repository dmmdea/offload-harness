// comfy-video.mjs — local image-to-video runner. Animates a still into a short b-roll
// clip via ComfyUI. PRIMARY model Wan 2.2 14B I2V (default; 4-step lightx2v LoRAs = fast);
// SECONDARY HunyuanVideo 1.5 480p I2V (--model hunyuan; needs 3 files absent on the 16GB box).
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
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

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
    if (flags.hero) common.hero = true; // backward compat: native IS the default now
    if (flags.fast) common.fast = true; // OPT-IN distilled speed path (wan)
    // Per-machine weight binding (quality-first): the machine's config names its Wan
    // expert weights + text encoder; unset = the builder's defaults (unchanged).
    if (flags["high-unet"]) common.highUnet = flags["high-unet"];
    if (flags["low-unet"]) common.lowUnet = flags["low-unet"];
    if (flags["text-encoder"]) common.textEncoder = flags["text-encoder"];
    if (flags["upscale-model"]) {       // optional post-decode upscale (wan)
      common.upscaleModel = flags["upscale-model"];
      if (flags["upscale-width"]) common.upscaleWidth = Number(flags["upscale-width"]);
      if (flags["upscale-height"]) common.upscaleHeight = Number(flags["upscale-height"]);
    }
    graph = model === "hunyuan" ? buildHunyuan15I2V(common) : buildWan22I2V(common);
  }
  // Shared submission/polling/retrieval (comfy-submit.mjs): CLI-preferred submit with
  // byte-identical raw fallback; hardened poll loop (dead-server watchdog).
  const cli = resolveCli();
  const { promptId } = await submitGraph({ api: API, graph, clientId: "video-" + seed, cli });
  console.log("queued", promptId, flags.graph ? `(graph ${flags.graph})` : `${model} seed ${seed}`);
  // Poll budget: the native quality recipe at 720p legitimately exceeds the old ~20-min
  // ceiling. Default 90 min; the Go harness passes COMFY_WAIT_SEC aligned to its own
  // videogen timeout and its process-tree kill remains the hard stop.
  const waitSec = Number(flags["wait-sec"] || process.env.COMFY_WAIT_SEC || 5400);
  const h = await pollOutputs({
    api: API, promptId, waitSec,
    isDone: (entry) => !!firstOutputFile(entry.outputs),
    noOutputMsg: "no video produced in time",
    onExecError: () => finalizeRun({ api: API, promptId, cli }),
  });
  const file = firstOutputFile(h.outputs);
  writeFileSync(out, await fetchView({ api: API, file }));
  console.log("WROTE", out);
  await finalizeRun({ api: API, promptId, cli });
}

withGpuSlot(
  { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
  generate,
).catch((e) => { console.error("VIDEO GEN FAILED:", e.message); process.exit(1); });
