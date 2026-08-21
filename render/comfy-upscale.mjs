// comfy-upscale.mjs — ESRGAN-family image UPSCALE runner. Wraps wf-upscale.mjs with the
// standard lifecycle: single-slot GPU lock, free llama-swap, on-demand ComfyUI,
// zero-always-warm teardown (withGpuSlot). Stages the input into <COMFY_DIR>/input
// (LoadImage reads only from there). Dependency-free, Node 18+.
//
// Sibling routes, kept distinct on purpose:
//   edit_image  resize — plain resampling (PIL), no model: exact, free, CPU.
//   this        — model-synthesized detail at the ESRGAN model's native factor
//                 (4x for a 4x model), optionally rescaled (--scale) or pinned
//                 (--width + --height). Enlargement, not a faithful photo restore.
//
// Usage:
//   node render/comfy-upscale.mjs <out.png> <image> --model 4x-UltraSharp.pth \
//        [--scale F] [--width N --height N] [--method lanczos|bicubic|bilinear|area|nearest-exact] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--reserve-vram F]
import { copyFileSync, readFileSync, writeFileSync, unlinkSync, mkdirSync } from "node:fs";
import { join, basename, dirname } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { buildUpscale } from "./wf-upscale.mjs";
import { imageSize } from "./image-size.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (["no-lock"].includes(k)) flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const [out, imagePath] = pos;
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out || !imagePath) {
  console.error("usage: node comfy-upscale.mjs <out.png> <image> --model <upscale_models filename> [flags]");
  process.exit(2);
}
const model = flags.model || process.env.COMFY_UPSCALE_MODEL;
if (!model) {
  console.error("error: --model is required (this machine's upscale_model binding, e.g. 4x-UltraSharp.pth)");
  process.exit(2);
}
const opts = {
  model,
  scale: flags.scale != null ? Number(flags.scale) : undefined,
  width: flags.width != null ? Number(flags.width) : undefined,
  height: flags.height != null ? Number(flags.height) : undefined,
  method: flags.method || "lanczos",
};
// A requested --scale is made EXACT by measuring the source here (PNG/JPEG/WebP header,
// no decode) and pinning the output size, so the result does not depend on what the
// model's filename claims its factor is. Only when the header cannot be read does the
// builder's scale/nativeFactor fallback apply — and it says so.
if (opts.scale != null && opts.width == null) {
  let src = { width: 0, height: 0 };
  try { src = imageSize(readFileSync(imagePath)); } catch { /* size stays unknown */ }
  if (src.width > 0 && src.height > 0) {
    opts.width = Math.round(src.width * opts.scale);
    opts.height = Math.round(src.height * opts.scale);
    opts.scale = undefined;
    console.error(`scale ${flags.scale} on a ${src.width}x${src.height} source -> exact ${opts.width}x${opts.height}`);
  } else {
    console.error(`warning: could not read the source size; --scale ${flags.scale} falls back to the model's filename factor`);
  }
}
// PRE-FLIGHT before taking the GPU slot: the builder's validation is pure and local,
// and a flag error (half-given size, bad method) cannot be fixed by a cold ComfyUI
// start — fail cheap here instead of after tearing down llama-swap.
try { buildUpscale({ image: "preflight.png", ...opts }); }
catch (e) { console.error("error: " + e.message); process.exit(2); }

let stageN = 0;
function stageInput(p) {
  const name = "upscale_in_" + Date.now() + "_" + (stageN++) + "_" + basename(p);
  copyFileSync(p, join(COMFY_DIR, "input", name));
  return name;
}

async function render() {
  const staged = [];
  try {
    staged.push(stageInput(imagePath));
    const graph = buildUpscale({ image: staged[0], ...opts });
    const cli = resolveCli();
    const { promptId } = await submitGraph({ api: API, graph, clientId: "upscale-" + Date.now(), cli });
    console.log("queued", promptId, "model", model,
      opts.width ? `target ${opts.width}x${opts.height}` : opts.scale != null ? `scale ${opts.scale}` : "native factor",
      "->", out);
    const waitSec = Number(process.env.COMFY_WAIT_SEC || 600);
    const h = await pollOutputs({
      api: API, promptId, waitSec,
      isDone: (entry) => !!firstOutputFile(entry.outputs),
      noOutputMsg: "no upscaled image produced in time",
      onExecError: () => finalizeRun({ api: API, promptId, cli }),
    });
    const file = firstOutputFile(h.outputs);
    mkdirSync(dirname(out) || ".", { recursive: true });
    writeFileSync(out, await fetchView({ api: API, file }));
    console.log("WROTE", out);
    await finalizeRun({ api: API, promptId, cli });
  } finally {
    for (const n of staged) {
      try { unlinkSync(join(COMFY_DIR, "input", n)); }
      catch (e) { console.error(`staged input leaked in ${join(COMFY_DIR, "input")}: ${n} (${e.code || e.message})`); }
    }
  }
}

withGpuSlot(
  { noLock: flags["no-lock"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
  render,
).catch((e) => { console.error("UPSCALE FAILED:", e.message); process.exit(1); });
