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
//   Each output is written atomically (atomic-out.mjs): a failed render leaves nothing at
//   its `out` path, and a good file already there is kept.
//   Batch exit codes: 0 every job rendered; 4 the batch ran every job and at least one
//   failed (the "ok":false rows of the results file name them); 1 the batch could not run
//   to the end (setup error, ComfyUI unusable, the disk full: the jobs not run get a
//   "not run" row); 2 usage.
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";
import { readFileSync, writeFileSync, appendFileSync, mkdirSync } from "node:fs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { comfyApi } from "./comfy-lifecycle.mjs";
import { parseJobs, jobArgs, runBatchJobs, renderExitError, batchExitCode, batchEndLine, JOB_PARAM_FLAGS, SHARED_BINDING_FLAGS } from "./batch-jobs.mjs";

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
const API = comfyApi(flags.api);

// Delegate the actual render to the proven comfy-render.mjs (ComfyUI is up now — THIS
// file's own withGpuSlot, below, already booted/confirmed it). comfy-render reads
// seed/width/height as flags OR positionals, so flags alone suffice.
// runRenderArgs: spawn comfy-render.mjs with a prebuilt argv tail (out, prompt, flags).
// --no-lifecycle is ALWAYS added: this file is always the lifecycle owner in this call
// path (comfy-render.mjs can also self-manage when run standalone — gap 4 — but here
// it must not, or its own teardown would free/unload ComfyUI's model after EVERY job
// in a --batch session, defeating the whole point of the warm session).
// The child's stderr is relayed as it arrives and its tail kept: its "RENDER FAILED:" line
// becomes the job's error, and exit RENDER_EXIT_SERVER_UNUSABLE marks the error
// serverUnusable, which stops a --batch (C-83). "close", not "exit": the tail must be
// complete before the reason is read from it.
function runRenderArgs(tail) {
  const args = [join(__dirname, "comfy-render.mjs"), ...tail, "--no-lifecycle"];
  return new Promise((resolve, reject) => {
    const c = spawn("node", args, { stdio: ["inherit", "inherit", "pipe"] });
    let errTail = "";
    c.stderr.on("data", (d) => { process.stderr.write(d); errTail = (errTail + d).slice(-8192); });
    c.on("close", (code) => (code === 0 ? resolve() : reject(renderExitError(code, errTail))));
    c.on("error", reject);
  });
}

// The collector is DERIVED from batch-jobs.mjs's emit lists — one source of
// truth, so a flag can never again be collected here and silently dropped at
// the jobArgs boundary (review-caught 2026-08-14).
const sharedFlags = {};
for (const k of ["api", ...JOB_PARAM_FLAGS, ...SHARED_BINDING_FLAGS]) {
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
  // Same ENOENT class as the render output: a missing parent must not sink the batch.
  mkdirSync(dirname(resultsPath) || ".", { recursive: true });
  writeFileSync(resultsPath, "");
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, api: API, reserveVram: flags["reserve-vram"], warm: true },
    () => runBatchJobs({
      jobs,
      runJob: (job) => runRenderArgs(jobArgs(job, sharedFlags)),
      record: (line) => appendFileSync(resultsPath, line + "\n"),
      log: (line) => console.error(line),
    }),
  ).then((summary) => {
    // The batch ran every job. It used to exit 0 whatever became of them, so a caller learned of
    // failed jobs only by grepping the log for RENDER FAILED (2026-10-09: 21 of 36 pictures). Any
    // failed job now ends it with BATCH_EXIT_JOBS_FAILED. exitCode, not exit(): the teardown has run,
    // and exit() while a socket is still closing trips libuv on Windows (comfy-video.mjs).
    const code = batchExitCode(summary);
    if (code !== 0) {
      console.error(batchEndLine(summary, resultsPath));
      process.exitCode = code;
    }
  }).catch((e) => { console.error("IMAGE BATCH FAILED:", e.message); process.exit(1); });
} else {
  if (!out || !prompt) {
    console.error('usage: node comfy-generate.mjs <out.png> "<prompt>" [--negative ...] [--width N] [--height N] [--steps N] [--seed N] [--ckpt name] | --batch jobs.jsonl [--results r.jsonl]');
    process.exit(2);
  }
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, api: API, reserveVram: flags["reserve-vram"] },
    () => runRenderArgs(jobArgs({ out, prompt }, sharedFlags)),
  ).catch((e) => { console.error("IMAGE GEN FAILED:", e.message); process.exit(1); });
}
