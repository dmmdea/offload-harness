// comfy-generate.mjs — local TEXT-TO-IMAGE runner: the single entrypoint the local-offload
// `generate_image` MCP tool shells out to. It wraps the proven, UNMODIFIED comfy-render.mjs
// (SDXL/RealVisXL via the ComfyUI HTTP API) with the lifecycle that the bare image renderer
// deliberately omits: single-slot GPU lock, free llama-swap first, start ComfyUI on-demand if
// it's down, free ComfyUI after (zero-always-warm). The lifecycle now lives in the shared
// withGpuSlot (gpu-lock.mjs) + ensureComfy (comfy-lifecycle.mjs) — behavior is unchanged.
// Dependency-free.
//
// Usage:
//   node render/comfy-generate.mjs <out.png> "<prompt>" \
//        [--negative "..."] [--width 1024] [--height 1024] [--steps 30] [--seed N] \
//        [--ckpt name.safetensors] [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy]
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";
import { withGpuSlot } from "./gpu-lock.mjs";

const __dirname = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (["no-lock", "keep-comfy"].includes(k)) flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const out = pos[0], prompt = pos[1];
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
if (!out || !prompt) {
  console.error('usage: node comfy-generate.mjs <out.png> "<prompt>" [--negative ...] [--width N] [--height N] [--steps N] [--seed N] [--ckpt name]');
  process.exit(2);
}

// Delegate the actual render to the proven, unmodified comfy-render.mjs (ComfyUI is up now).
// comfy-render reads seed/width/height as flags OR positionals, so flags alone suffice.
function runRender() {
  const args = [join(__dirname, "comfy-render.mjs"), out, prompt, "--api", API];
  for (const k of ["negative", "width", "height", "steps", "seed", "ckpt", "vae", "cfg", "sampler", "scheduler", "family"]) {
    if (flags[k] != null) args.push("--" + k, String(flags[k]));
  }
  return new Promise((resolve, reject) => {
    const c = spawn("node", args, { stdio: "inherit" });
    c.on("exit", (code) => (code === 0 ? resolve() : reject(new Error("comfy-render exited " + code))));
    c.on("error", reject);
  });
}

withGpuSlot(
  { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
  runRender,
).catch((e) => { console.error("IMAGE GEN FAILED:", e.message); process.exit(1); });
