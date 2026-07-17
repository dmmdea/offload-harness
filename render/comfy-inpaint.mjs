// comfy-inpaint.mjs — generative INPAINT runner: re-renders ONLY the masked region of
// an existing image on the local ComfyUI. Wraps wf-sdxl-inpaint.mjs with the standard
// lifecycle: single-slot GPU lock, free llama-swap, on-demand ComfyUI, zero-always-warm
// teardown (withGpuSlot). Stages image+mask into <COMFY_DIR>/input (LoadImage reads
// only from there — same pattern as comfy-video.mjs). Dependency-free, Node 18+.
//
// Usage:
//   node render/comfy-inpaint.mjs <out.png> <image> <mask> "<prompt>" \
//        [--negative ...] [--ckpt name] [--vae name|builtin] [--steps N] [--cfg F] \
//        [--sampler s] [--scheduler s] [--seed N] [--denoise F] [--grow-mask N] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--reserve-vram F]
import { copyFileSync, writeFileSync, unlinkSync } from "node:fs";
import { join, basename } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { buildSDXLInpaint } from "./wf-sdxl-inpaint.mjs";
import { firstOutputFile } from "./comfy-output.mjs";

const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (["no-lock"].includes(k)) flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const [out, imagePath, maskPath, prompt] = pos;
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out || !imagePath || !maskPath || !prompt) {
  console.error('usage: node comfy-inpaint.mjs <out.png> <image> <mask> "<prompt>" [flags]');
  process.exit(2);
}
if (!flags.ckpt && !process.env.COMFY_CKPT) {
  console.error("error: --ckpt is required (this machine's inpaint_ckpt binding)");
  process.exit(2);
}

const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));

// Per-call counter: Date.now() alone collides when image and mask share a basename
// (both stage within the same millisecond → the mask copy overwrites the image copy
// and BOTH LoadImage nodes read the mask — a silently-wrong "success").
let stageN = 0;
function stageInput(p) {
  const name = "inpaint_in_" + Date.now() + "_" + (stageN++) + "_" + basename(p);
  copyFileSync(p, join(COMFY_DIR, "input", name));
  return name;
}

const j = async (url, opts) => { const r = await fetch(url, opts); if (!r.ok) throw new Error(url + " -> " + r.status + " " + (await r.text()).slice(0, 200)); return r.json(); };

async function render() {
  // Staging lives INSIDE the try so a failed mask stage doesn't orphan the image copy.
  const staged = [];
  try {
  staged.push(stageInput(imagePath));
  staged.push(stageInput(maskPath));
  const graph = buildSDXLInpaint({
    image: staged[0], mask: staged[1], prompt,
    negative: flags.negative || "",
    ckpt: flags.ckpt || process.env.COMFY_CKPT,
    vae: flags.vae || "builtin",
    steps: Number(flags.steps || 0) || undefined,
    cfg: Number(flags.cfg || 0) || undefined,
    sampler: flags.sampler || undefined,
    scheduler: flags.scheduler || undefined,
    seed,
    denoise: Number(flags.denoise || 0) || undefined,
    growMask: flags["grow-mask"] != null ? Number(flags["grow-mask"]) : undefined,
  });
    const { prompt_id } = await j(API + "/prompt", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt: graph, client_id: "inpaint-" + seed }),
    });
    console.log("queued", prompt_id, "seed", seed);
    const waitSec = Number(process.env.COMFY_WAIT_SEC || 1800);
    let file = null;
    for (let i = 0; i < Math.max(1, Math.ceil(waitSec / 2)); i++) {
      await new Promise((r) => setTimeout(r, 2000));
      let hist; try { hist = await j(`${API}/history/${prompt_id}`); } catch { continue; }
      const h = hist[prompt_id];
      if (!h) continue;
      if (h.status && h.status.status_str === "error") throw new Error("ComfyUI exec error: " + JSON.stringify(h.status).slice(0, 400));
      file = firstOutputFile(h.outputs);
      if (file) break;
    }
    if (!file) throw new Error("no inpainted image produced in time");
    const q = new URLSearchParams({ filename: file.filename, subfolder: file.subfolder, type: file.type });
    const r = await fetch(`${API}/view?` + q.toString());
    if (!r.ok) throw new Error("view fetch " + r.status);
    writeFileSync(out, Buffer.from(await r.arrayBuffer()));
    console.log("WROTE", out);
  } finally {
    for (const n of staged) { try { unlinkSync(join(COMFY_DIR, "input", n)); } catch {} }
  }
}

withGpuSlot(
  { noLock: flags["no-lock"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
  render,
).catch((e) => { console.error("INPAINT FAILED:", e.message); process.exit(1); });
