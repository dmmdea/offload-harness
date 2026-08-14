// comfy-edit.mjs — GENERATIVE, mask-free image edit runner (Qwen-Image-Edit 2511).
// Wraps wf-qwen-image-edit.mjs with the standard lifecycle: single-slot GPU lock,
// free llama-swap, on-demand ComfyUI, zero-always-warm teardown (withGpuSlot).
// Stages the input into <COMFY_DIR>/input (LoadImage reads only from there).
// Dependency-free, Node 18+.
//
// Sibling routes, kept distinct on purpose:
//   edit_image    — deterministic PIL ops (crop/resize/composite). No model.
//   inpaint_image — SDXL re-denoise INSIDE a user-supplied mask.
//   this          — instruction-following rewrite of the whole frame, no mask.
//
// Usage:
//   node render/comfy-edit.mjs <out.png> <image> "<instruction>" \
//        [--negative ...] [--unet name] [--preset full|lightning8|lightning4] \
//        [--lora name] [--lora-strength F] [--steps N] [--cfg F] \
//        [--megapixels F] \
//        [--clip name] [--vae name] [--sampler s] [--scheduler s] [--shift F] \
//        [--seed N] [--api http://127.0.0.1:8188] [--no-lock] [--reserve-vram F]
import { copyFileSync, readFileSync, writeFileSync, unlinkSync } from "node:fs";
import { join, basename } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { buildQwenImageEdit, QWEN_EDIT_PRESETS, resolveEditMegapixels } from "./wf-qwen-image-edit.mjs";
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
const [out, imagePath, prompt] = pos;
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out || !imagePath || !prompt) {
  console.error('usage: node comfy-edit.mjs <out.png> <image> "<instruction>" [flags]');
  process.exit(2);
}
const unet = flags.unet || process.env.COMFY_EDIT_UNET;
if (!unet) {
  console.error("error: --unet is required (this machine's edit_unet binding)");
  process.exit(2);
}

// A preset pairs steps+cfg+lora. Mixing a Lightning LoRA with base-model steps
// (or vice versa) still renders — it just renders badly — so the pairing is
// resolved here rather than left to whatever the caller half-specifies.
const presetName = flags.preset || process.env.COMFY_EDIT_PRESET || "lightning8";
const preset = QWEN_EDIT_PRESETS[presetName];
if (!preset) {
  console.error(`error: --preset must be one of ${Object.keys(QWEN_EDIT_PRESETS).join("|")}, got ${presetName}`);
  process.exit(2);
}
const steps = Number(flags.steps || 0) || preset.steps;
const cfg = Number(flags.cfg || 0) || preset.cfg;
const lora = flags.lora != null ? flags.lora : preset.lora;
const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));

// Working canvas. Unset means "keep the source's own resolution, capped" — so the
// source is measured here, where the real file is still on disk, rather than guessed
// in the graph builder (which only ever sees the staged filename). An unreadable
// header is not fatal: resolveEditMegapixels falls back to the cap.
const configuredMP = Number(flags.megapixels || process.env.COMFY_EDIT_MEGAPIXELS || 0);
let src = { width: 0, height: 0 };
try { src = imageSize(readFileSync(imagePath)); } catch { /* size stays unknown */ }
const megapixels = resolveEditMegapixels({ configured: configuredMP, width: src.width, height: src.height });

let stageN = 0;
function stageInput(p) {
  const name = "edit_in_" + Date.now() + "_" + (stageN++) + "_" + basename(p);
  copyFileSync(p, join(COMFY_DIR, "input", name));
  return name;
}

async function render() {
  const staged = [];
  try {
    staged.push(stageInput(imagePath));
    const graph = buildQwenImageEdit({
      image: staged[0], prompt,
      negative: flags.negative || "",
      unet,
      loader: flags.loader || "auto",
      clip: flags.clip || undefined,
      vae: flags.vae || undefined,
      lora, loraStrength: Number(flags["lora-strength"] || 0) || undefined,
      steps, cfg, megapixels,
      sampler: flags.sampler || undefined,
      scheduler: flags.scheduler || undefined,
      shift: Number(flags.shift || 0) || undefined,
      seed,
    });
    // Shared submission/polling/retrieval (comfy-submit.mjs): CLI-preferred submit with
    // byte-identical raw fallback; hardened poll loop (dead-server watchdog).
    const cli = resolveCli();
    const { promptId } = await submitGraph({ api: API, graph, clientId: "edit-" + seed, cli });
    const srcNote = src.width > 0 ? `${src.width}x${src.height}` : "size-unknown";
    console.log("queued", promptId, "seed", seed, "preset", presetName, "steps", steps, "cfg", cfg,
      lora ? "lora " + lora : "no-lora", `src ${srcNote}`, `mp ${megapixels}`);
    const waitSec = Number(process.env.COMFY_WAIT_SEC || 1800);
    const h = await pollOutputs({
      api: API, promptId, waitSec,
      isDone: (entry) => !!firstOutputFile(entry.outputs),
      noOutputMsg: "no edited image produced in time",
      onExecError: () => finalizeRun({ api: API, promptId, cli }),
    });
    const file = firstOutputFile(h.outputs);
    writeFileSync(out, await fetchView({ api: API, file }));
    console.log("WROTE", out);
    await finalizeRun({ api: API, promptId, cli });
  } finally {
    for (const n of staged) { try { unlinkSync(join(COMFY_DIR, "input", n)); } catch {} }
  }
}

withGpuSlot(
  { noLock: flags["no-lock"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
  render,
).catch((e) => { console.error("EDIT FAILED:", e.message); process.exit(1); });
