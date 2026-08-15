// comfy-inpaint.mjs — generative INPAINT runner: re-renders ONLY the masked region of
// an existing image on the local ComfyUI. Two families behind one lifecycle:
//   · sdxl (default) — wf-sdxl-inpaint.mjs, masked latent inpaint (checkpoint binding).
//   · qwen           — wf-qwen-inpaint.mjs, the official DiffSynth ControlNet inpaint
//                      patch on a Qwen-Image DiT (unet + patch binding).
// Standard lifecycle: single-slot GPU lock, free llama-swap, on-demand ComfyUI,
// zero-always-warm teardown (withGpuSlot). Stages image+mask into <COMFY_DIR>/input
// (LoadImage reads only from there — same pattern as comfy-video.mjs).
//
// BATCH MODE (--batch jobs.jsonl): N inpaint jobs through ONE warm ComfyUI session —
// the checkpoint/unet loads once; withGpuSlot's teardown runs once at the batch
// boundary, so zero-always-warm is preserved per-batch instead of per-render (the
// same shape comfy-generate.mjs uses; without it a 20-case eval sweep pays the
// cold-start tax 20 times). Jobs are self-contained lines
//   {"out":..., "image":..., "mask":..., "prompt":..., "seed"?, "negative"?, "steps"?, "cfg"?, "denoise"?, "grow_mask"?, "strength"?}
// — per-job knobs win over CLI flags. Job parsing is local to this runner rather
// than batch-jobs.mjs: inpaint jobs carry image+mask fields the shared T2I emit
// lists deliberately do not know about. Results: one JSONL row per job
// ({i, out, ok, wall_ms, error?}) to --results (default <batch>.results.jsonl).
//
// Usage:
//   node render/comfy-inpaint.mjs <out.png> <image> <mask> "<prompt>" [flags]
//   node render/comfy-inpaint.mjs --batch jobs.jsonl [--results r.jsonl] [flags]
// Flags:
//   [--family sdxl|qwen] [--negative ...] [--seed N] [--denoise F] [--grow-mask N]
//   sdxl: [--ckpt name] [--vae name|builtin] [--steps N] [--cfg F] [--sampler s] [--scheduler s]
//   qwen: [--unet name] [--patch name] [--preset full|lightning4] [--steps N] [--cfg F]
//         [--strength F] [--lora name] [--lora-strength F] [--clip name] [--qvae name]
//   [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy] [--reserve-vram F]
import { copyFileSync, writeFileSync, unlinkSync, readFileSync, appendFileSync, mkdirSync } from "node:fs";
import { join, basename, dirname } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { buildSDXLInpaint } from "./wf-sdxl-inpaint.mjs";
import { buildQwenInpaint, QWEN_INPAINT_PRESETS } from "./wf-qwen-inpaint.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
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
const [out, imagePath, maskPath, prompt] = pos;
const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
const FAMILY = flags.family || "sdxl";
if (FAMILY !== "sdxl" && FAMILY !== "qwen") {
  console.error(`error: --family must be sdxl or qwen, got ${FAMILY}`);
  process.exit(2);
}

if (!flags.batch) {
  if (!out || !imagePath || !maskPath || !prompt) {
    console.error('usage: node comfy-inpaint.mjs <out.png> <image> <mask> "<prompt>" [flags] | --batch jobs.jsonl');
    process.exit(2);
  }
}
if (FAMILY === "sdxl" && !flags.ckpt && !process.env.COMFY_CKPT) {
  console.error("error: --ckpt is required for the sdxl family (this machine's inpaint_ckpt binding)");
  process.exit(2);
}
if (FAMILY === "qwen" && !flags.unet) {
  console.error("error: --unet is required for the qwen family (e.g. qwen-image-2512-Q5_1.gguf)");
  process.exit(2);
}

// Per-call counter: Date.now() alone collides when image and mask share a basename
// (both stage within the same millisecond → the mask copy overwrites the image copy
// and BOTH LoadImage nodes read the mask — a silently-wrong "success").
let stageN = 0;
function stageInput(p) {
  const name = "inpaint_in_" + Date.now() + "_" + (stageN++) + "_" + basename(p);
  copyFileSync(p, join(COMFY_DIR, "input", name));
  return name;
}

// Resolve the qwen preset ONCE: explicit --steps/--cfg win over the preset pair,
// but a half-override (steps without cfg or vice versa) is the classic mismatched
// pairing (wf-qwen-image.mjs header) — refuse it loudly.
function qwenRecipe(job) {
  const presetName = job.preset ?? flags.preset ?? "full";
  const preset = QWEN_INPAINT_PRESETS[presetName];
  if (!preset) {
    throw new Error(`unknown qwen preset "${presetName}" (have: ${Object.keys(QWEN_INPAINT_PRESETS).join(", ")})`);
  }
  const stepsOver = job.steps ?? (flags.steps != null ? Number(flags.steps) : undefined);
  const cfgOver = job.cfg ?? (flags.cfg != null ? Number(flags.cfg) : undefined);
  if ((stepsOver != null) !== (cfgOver != null)) {
    throw new Error("qwen family: override steps and cfg TOGETHER or not at all (mismatched pairings burn renders)");
  }
  return {
    steps: stepsOver ?? preset.steps,
    cfg: cfgOver ?? preset.cfg,
    lora: job.lora ?? flags.lora ?? preset.lora,
  };
}

function buildGraph(job, staged) {
  if (FAMILY === "qwen") {
    const recipe = qwenRecipe(job);
    return buildQwenInpaint({
      image: staged[0], mask: staged[1], prompt: job.prompt,
      negative: job.negative ?? flags.negative ?? "",
      unet: flags.unet,
      patch: flags.patch || undefined,
      strength: job.strength != null ? Number(job.strength)
        : flags.strength != null ? Number(flags.strength) : undefined,
      clip: flags.clip || undefined,
      vae: flags.qvae || undefined,
      steps: recipe.steps, cfg: recipe.cfg,
      lora: recipe.lora, loraStrength: flags["lora-strength"] != null ? Number(flags["lora-strength"]) : undefined,
      seed: job.seed,
    });
  }
  return buildSDXLInpaint({
    image: staged[0], mask: staged[1], prompt: job.prompt,
    negative: job.negative ?? flags.negative ?? "",
    ckpt: flags.ckpt || process.env.COMFY_CKPT,
    vae: flags.vae || "builtin",
    steps: (job.steps ?? Number(flags.steps || 0)) || undefined,
    cfg: (job.cfg ?? Number(flags.cfg || 0)) || undefined,
    sampler: flags.sampler || undefined,
    scheduler: flags.scheduler || undefined,
    seed: job.seed,
    denoise: (job.denoise ?? Number(flags.denoise || 0)) || undefined,
    growMask: job.grow_mask != null ? Number(job.grow_mask)
      : flags["grow-mask"] != null ? Number(flags["grow-mask"]) : undefined,
  });
}

// Submission deadline for BATCH mode only: a wedged ComfyUI (port accepts,
// never responds — the classic Windows CUDA-hang/OOM presentation) hangs the
// raw submit POST FOREVER (no AbortSignal, no fetch default timeout; the poll
// watchdog never runs because polling never starts). Racing the submit keeps
// the shared comfy-submit.mjs semantics untouched for other routes while
// bounding the batch: the dangling POST is abandoned, the job records failed,
// and the consecutive-failure abort ends the sweep instead of a silent
// all-night hang. Single-shot keeps the historical unbounded submit.
const SUBMIT_DEADLINE_MS = Number(process.env.COMFY_SUBMIT_DEADLINE_MS || 120_000);
function withDeadline(promise, ms, what) {
  let timer;
  const gate = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} exceeded ${ms}ms deadline (server wedged?)`)), ms);
  });
  return Promise.race([promise, gate]).finally(() => clearTimeout(timer));
}

// Best-effort interrupt of the currently executing prompt: on a poll TIMEOUT
// nothing server-side cancels the still-running job, so the next batch job
// queues BEHIND it and burns its own budget waiting — N x waitSec of serial
// timeouts. Interrupting on the timeout path makes each job's budget measure
// its OWN render.
async function interruptCurrent() {
  try {
    await fetch(API + "/interrupt", { method: "POST", signal: AbortSignal.timeout(10_000) });
    console.error("sent /interrupt after poll timeout");
  } catch (e) {
    console.error("could not /interrupt after timeout: " + e.message);
  }
}

// One inpaint render, ComfyUI already up (called inside withGpuSlot).
// batchMode gates the submit deadline + timeout-interrupt (single-shot keeps
// the historical behavior exactly).
async function renderJob(job, batchMode = false) {
  const staged = [];
  try {
    staged.push(stageInput(job.image));
    staged.push(stageInput(job.mask));
    const graph = buildGraph(job, staged);
    const cli = resolveCli();
    const submitP = submitGraph({ api: API, graph, clientId: "inpaint-" + job.seed, cli });
    const { promptId } = batchMode
      ? await withDeadline(submitP, SUBMIT_DEADLINE_MS, "submit")
      : await submitP;
    console.log("queued", promptId, "seed", job.seed, "->", job.out);
    const waitSec = Number(process.env.COMFY_WAIT_SEC || 1800);
    let h;
    try {
      h = await pollOutputs({
        api: API, promptId, waitSec,
        isDone: (entry) => !!firstOutputFile(entry.outputs),
        noOutputMsg: "no inpainted image produced in time",
        onExecError: () => finalizeRun({ api: API, promptId, cli }),
      });
    } catch (e) {
      if (batchMode && /no inpainted image produced in time/.test(e.message)) {
        await interruptCurrent();
      }
      throw e;
    }
    const file = firstOutputFile(h.outputs);
    mkdirSync(dirname(job.out) || ".", { recursive: true });
    writeFileSync(job.out, await fetchView({ api: API, file }));
    console.log("WROTE", job.out);
    await finalizeRun({ api: API, promptId, cli });
  } finally {
    for (const n of staged) {
      try { unlinkSync(join(COMFY_DIR, "input", n)); }
      catch (e) { console.error(`staged input leaked in ${COMFY_DIR}\\input: ${n} (${e.code || e.message})`); }
    }
  }
}

// Local job parsing (deliberately NOT batch-jobs.mjs — see header).
function parseInpaintJobs(text) {
  const jobs = [];
  const lines = text.split(/\r?\n/);
  for (let ln = 0; ln < lines.length; ln++) {
    if (!lines[ln].trim()) continue;
    let j;
    try { j = JSON.parse(lines[ln]); } catch (e) {
      throw new Error(`batch line ${ln + 1}: invalid JSON (${e.message})`);
    }
    for (const k of ["out", "image", "mask", "prompt"]) {
      if (!j[k] || typeof j[k] !== "string") {
        throw new Error(`batch line ${ln + 1}: "${k}" (string) is required`);
      }
    }
    if (j.seed == null) throw new Error(`batch line ${ln + 1}: "seed" is required for reproducible sweeps (no silent random seeds in batch mode)`);
    jobs.push(j);
  }
  return jobs;
}

function normalizeSeed(job) {
  return { ...job, seed: Number(job.seed) };
}

if (flags.batch) {
  const jobs = parseInpaintJobs(readFileSync(flags.batch, "utf8")).map(normalizeSeed);
  if (jobs.length === 0) { console.error("batch: no jobs in " + flags.batch); process.exit(2); }
  // Duplicate outs silently score one image under two configs downstream.
  const seenOut = new Set();
  for (const j of jobs) {
    if (seenOut.has(j.out)) { console.error("batch: duplicate out path " + j.out); process.exit(2); }
    seenOut.add(j.out);
  }
  // PRE-FLIGHT before taking the GPU slot: builder/preset/flag validation is
  // pure and local, and a flag-level config error (bad --preset, half-overridden
  // steps/cfg) fails EVERY job identically — grinding N recorded failures after
  // tearing down llama-swap and cold-starting ComfyUI for a sweep that cannot
  // succeed. Dry-run the graph build for each job and abort cheap, before the
  // expensive lifecycle. (A wrong --unet/--patch NAME is server-side only — the
  // consecutive-failure abort below owns that case.)
  for (let i = 0; i < jobs.length; i++) {
    try { buildGraph(jobs[i], ["preflight_img.png", "preflight_mask.png"]); }
    catch (e) { console.error(`batch line ${i + 1} would fail before rendering: ${e.message}`); process.exit(2); }
  }
  const resultsPath = flags.results || flags.batch + ".results.jsonl";
  mkdirSync(dirname(resultsPath) || ".", { recursive: true });
  writeFileSync(resultsPath, "");
  const MAX_CONSEC_FAIL = Number(process.env.COMFY_BATCH_MAX_CONSEC_FAIL || 3);
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"], warm: true },
    async () => {
      let okCount = 0, failCount = 0, consecFail = 0, firstErr = null;
      for (let i = 0; i < jobs.length; i++) {
        const t0 = Date.now();
        let ok = false, errMsg = null;
        try {
          await renderJob(jobs[i], true);
          ok = true; okCount++; consecFail = 0;
        } catch (e) {
          // A single failed render must not sink the batch: record and continue —
          // but LOUDLY, and a run of identical-class failures means the server or
          // the model binding is dead for every remaining job: abort instead of
          // grinding the rest of the night against a corpse.
          errMsg = e.message; failCount++; consecFail++;
          if (!firstErr) firstErr = e.message;
          console.error(`batch job ${i + 1}/${jobs.length} FAILED: ${e.message}`);
        }
        appendFileSync(resultsPath, JSON.stringify(
          ok ? { i, out: jobs[i].out, ok: true, wall_ms: Date.now() - t0 }
             : { i, out: jobs[i].out, ok: false, wall_ms: Date.now() - t0, error: errMsg }) + "\n");
        console.error(`batch ${i + 1}/${jobs.length} ${ok ? "done" : "FAILED"} (${Math.round((Date.now() - t0) / 1000)}s)`);
        if (consecFail >= MAX_CONSEC_FAIL) {
          throw new Error(`${consecFail} consecutive failures (last: ${errMsg}) — aborting the batch; ${jobs.length - i - 1} jobs not attempted`);
        }
      }
      console.error(`batch complete: ${okCount} ok / ${failCount} failed of ${jobs.length}`);
      if (okCount === 0) {
        throw new Error(`every job failed (first error: ${firstErr}) — systemic, not per-item`);
      }
    },
  ).catch((e) => { console.error("INPAINT BATCH FAILED:", e.message); process.exit(1); });
} else {
  const job = normalizeSeed({
    out, image: imagePath, mask: maskPath, prompt,
    seed: flags.seed != null ? flags.seed : Math.floor(Math.random() * 1e15),
  });
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
    () => renderJob(job),
  ).catch((e) => { console.error("INPAINT FAILED:", e.message); process.exit(1); });
}
