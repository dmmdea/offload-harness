// comfy-animate.mjs — local CHARACTER-ANIMATION runner (WAN-Animate-2 distilled).
// Retargets the motion of a driver video onto a reference character image via
// ComfyUI. Single-slot GPU-locked + zero-always-warm, mirroring comfy-video.mjs.
// Dependency-free (Node 18+).
//
// Usage:
//   node render/comfy-animate.mjs <out.mp4> <ref.(png|jpg)> <driver.mp4> "<prompt>" \
//        [--motion-prompt "..."] [--negative "..."] [--width 482] [--height 854] \
//        [--frames 81] [--steps 10] [--seed N] [--pose-strength 1.0] [--ref-strength 1.0] \
//        [--unet f.safetensors] [--text-encoder f] [--clip-vision f] [--vae f] \
//        [--cache-device cpu|gpu] [--cache-dtype default|int8] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy]
//   <prompt> describes the CHARACTER + BACKGROUND (what the output should show);
//   --motion-prompt describes the driver video (what motion is being transferred).
import { writeFileSync, copyFileSync } from "node:fs";
import { basename, join } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { buildWanAnimate2 } from "./wf-wan-animate2.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (["no-lock", "keep-comfy"].includes(k)) flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const [out, ref, driver, promptPos] = pos;
const prompt = promptPos || flags.prompt;
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out || !ref || !driver || !prompt) {
  console.error('usage: node comfy-animate.mjs <out.mp4> <ref.png> <driver.mp4> "<prompt>" [flags]');
  process.exit(2);
}

// ComfyUI's LoadImage/LoadVideo read from C:\ComfyUI\input. Stage both there.
function stageInput(srcPath) {
  const name = "render_in_" + Date.now() + "_" + basename(srcPath);
  copyFileSync(srcPath, join(COMFY_DIR, "input", name));
  return name;
}

async function animate() {
  const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));
  const common = {
    refImagePath: stageInput(ref),
    driverVideoPath: stageInput(driver),
    prompt, seed,
  };
  if (flags["motion-prompt"]) common.motionPrompt = flags["motion-prompt"];
  if (flags.negative) common.negative = flags.negative;
  if (flags.width) common.width = Number(flags.width);
  if (flags.height) common.height = Number(flags.height);
  if (flags.frames) common.length = Number(flags.frames);
  if (flags.steps) common.steps = Number(flags.steps);
  if (flags["pose-strength"]) common.poseStrength = Number(flags["pose-strength"]);
  if (flags["ref-strength"]) common.refStrength = Number(flags["ref-strength"]);
  // Per-machine weight binding (quality-first), same pattern as comfy-video.mjs:
  // unset = the builder's defaults (the reference box's on-disk filenames).
  if (flags.unet) common.unet = flags.unet;
  if (flags["text-encoder"]) common.textEncoder = flags["text-encoder"];
  if (flags["clip-vision"]) common.clipVision = flags["clip-vision"];
  if (flags.vae) common.vae = flags.vae;
  // Cache override is explicit-only; the builder pins cpu/default (gpu/int8
  // hard-kills ComfyUI on the reference box — see wf-wan-animate2.mjs header).
  if (flags["cache-device"]) common.cacheDevice = flags["cache-device"];
  if (flags["cache-dtype"]) common.cacheDtype = flags["cache-dtype"];
  const graph = buildWanAnimate2(common);

  const cli = resolveCli();
  const { promptId } = await submitGraph({ api: API, graph, clientId: "animate-" + seed, cli });
  console.log("queued", promptId, `wan-animate2 seed ${seed}`);
  const waitSec = Number(flags["wait-sec"] || process.env.COMFY_WAIT_SEC || 3600);
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
  animate,
).catch((e) => { console.error("ANIMATE FAILED:", e.message); process.exit(1); });
