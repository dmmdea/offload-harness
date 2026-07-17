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
//   node render/comfy-generate.mjs --batch jobs.jsonl [--results r.jsonl] \
//        [--negative ...] [--ckpt ...] [--vae ...] [--cfg ...] [--sampler ...] \
//        [--scheduler ...] [--family ...] [--api ...] [--no-lock] [--reserve-vram F]
//   Batch: one job per JSONL line ({"prompt","out",...}); N renders through ONE warm
//   ComfyUI session (checkpoint loads once), one result line per job appended to
//   --results (default <jobs>.results.jsonl). Zero-always-warm holds at the batch
//   boundary: withGpuSlot's single teardown frees VRAM + kills the spawned ComfyUI.
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";
import { readFileSync, writeFileSync, appendFileSync } from "node:fs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { parseJobs, jobArgs, resultLine } from "./batch-jobs.mjs";

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

// Delegate the actual render to the proven, unmodified comfy-render.mjs (ComfyUI is up now).
// comfy-render reads seed/width/height as flags OR positionals, so flags alone suffice.
// runRenderArgs: spawn comfy-render.mjs with a prebuilt argv tail (out, prompt, flags).
function runRenderArgs(tail) {
  const args = [join(__dirname, "comfy-render.mjs"), ...tail];
  return new Promise((resolve, reject) => {
    const c = spawn("node", args, { stdio: "inherit" });
    c.on("exit", (code) => (code === 0 ? resolve() : reject(new Error("comfy-render exited " + code))));
    c.on("error", reject);
  });
}

const sharedFlags = {};
for (const k of ["api", "negative", "width", "height", "steps", "seed", "ckpt", "vae", "cfg", "sampler", "scheduler", "family"]) {
  if (flags[k] != null) sharedFlags[k] = flags[k];
}
sharedFlags.api = API;

if (flags.batch) {
  // BATCH: N jobs through ONE warm ComfyUI session. The checkpoint loads once;
  // withGpuSlot's teardown (freeComfy + kill + release) runs ONCE, at the batch
  // boundary — zero-always-warm is preserved per-batch instead of per-render.
  const jobs = parseJobs(readFileSync(flags.batch, "utf8"));
  if (jobs.length === 0) { console.error("batch: no jobs in " + flags.batch); process.exit(2); }
  const resultsPath = flags.results || flags.batch + ".results.jsonl";
  writeFileSync(resultsPath, "");
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"], warm: true },
    async () => {
      for (let i = 0; i < jobs.length; i++) {
        const t0 = Date.now();
        try {
          await runRenderArgs(jobArgs(jobs[i], sharedFlags));
          appendFileSync(resultsPath, resultLine(i, jobs[i], true, Date.now() - t0) + "\n");
        } catch (e) {
          // A single failed render must not sink the batch: record and continue.
          appendFileSync(resultsPath, resultLine(i, jobs[i], false, Date.now() - t0, e.message) + "\n");
        }
        console.error(`batch ${i + 1}/${jobs.length} done (${Math.round((Date.now() - t0) / 1000)}s)`);
      }
    },
  ).catch((e) => { console.error("IMAGE BATCH FAILED:", e.message); process.exit(1); });
} else {
  if (!out || !prompt) {
    console.error('usage: node comfy-generate.mjs <out.png> "<prompt>" [--negative ...] [--width N] [--height N] [--steps N] [--seed N] [--ckpt name] | --batch jobs.jsonl [--results r.jsonl]');
    process.exit(2);
  }
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
    () => runRenderArgs(jobArgs({ out, prompt }, sharedFlags)),
  ).catch((e) => { console.error("IMAGE GEN FAILED:", e.message); process.exit(1); });
}
