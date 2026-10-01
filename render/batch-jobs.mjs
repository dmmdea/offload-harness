// batch-jobs.mjs — pure helpers for comfy-generate.mjs --batch mode: parse the jobs
// JSONL, build a per-job comfy-render argv (job fields beat shared defaults; the
// machine's model-binding flags are shared-only), format per-job result lines and run
// the batch loop over injected callbacks.
// No I/O — the script owns files and process lifecycle. No deps.

// Per-job overridable request params vs shared-only machine binding flags. A job may
// NOT override the binding (ckpt/vae/...): which checkpoint a machine renders with is
// per-machine config threaded by the Go harness, never per-prompt data.
// BOTH lists are exported because comfy-generate.mjs derives its flag COLLECTOR
// from them: collector = api + JOB_PARAM_FLAGS + SHARED_BINDING_FLAGS. One
// source of truth, structurally — a flag collected but absent from the emit
// lists was silently dropped between the two scripts (review-caught
// 2026-08-14: the pool flags were collected and discarded, leaving the pooled
// seat rendering single-card through every harness path while 249 tests
// stayed green).
export const JOB_PARAM_FLAGS = ["negative", "width", "height", "steps", "seed"];
// schedule/transparent (qwen-image-2.1) ride this list for the same reason: the
// wrapper's collector is derived from it, so a flag missing here is a flag the
// harness sends and comfy-render never sees.
export const SHARED_BINDING_FLAGS = ["ckpt", "vae", "cfg", "sampler", "scheduler", "family", "preset", "clip", "lora", "lora-strength", "shift", "pool-vvram", "pool-compute", "pool-donor", "schedule", "transparent"];
const JOB_PARAMS = JOB_PARAM_FLAGS;
const SHARED_ONLY = SHARED_BINDING_FLAGS;

export function parseJobs(text) {
  const jobs = [];
  const lines = String(text).split(/\r?\n/);
  for (let n = 0; n < lines.length; n++) {
    const line = lines[n].trim();
    if (!line) continue;
    let j;
    try { j = JSON.parse(line); } catch (e) { throw new Error(`jobs line ${n + 1}: invalid JSON (${e.message})`); }
    if (!j || typeof j.prompt !== "string" || !j.prompt.trim()) throw new Error(`jobs line ${n + 1}: missing "prompt"`);
    if (typeof j.out !== "string" || !j.out.trim()) throw new Error(`jobs line ${n + 1}: missing "out"`);
    jobs.push(j);
  }
  return jobs;
}

export function jobArgs(job, shared = {}) {
  const args = [job.out, job.prompt];
  if (shared.api) args.push("--api", String(shared.api));
  for (const k of JOB_PARAMS) {
    const v = job[k] != null && job[k] !== "" ? job[k] : shared[k];
    if (v != null && v !== "") args.push("--" + k, String(v));
  }
  for (const k of SHARED_ONLY) {
    // "lora" forwards even as an EMPTY string: --lora "" is the documented way to
    // strip a preset's LoRA at the comfy-render layer, and dropping it here made
    // the two entry points silently diverge on identical argv (the wrapper would
    // re-apply the very LoRA the caller disabled).
    if (shared[k] != null && (shared[k] !== "" || k === "lora")) args.push("--" + k, String(shared[k]));
  }
  return args;
}

export function resultLine(i, job, ok, ms, error) {
  const r = { i, out: job.out };
  if (job.seed != null) r.seed = job.seed;
  r.ok = !!ok;
  r.ms = ms;
  if (!ok && error) r.error = String(error);
  return JSON.stringify(r);
}

// RENDER_EXIT_SERVER_UNUSABLE: comfy-render.mjs's exit code when its ComfyUI can run no
// later job: the server vanished, or it answers HTTP with a broken CUDA context (C-83).
// A cross-process contract between comfy-render.mjs and its --batch parent.
export const RENDER_EXIT_SERVER_UNUSABLE = 3;

// renderExitError: the rejection for a comfy-render child that exited non-zero.
// stderrTail is the end of the child's stderr; its last "RENDER FAILED:" line names the
// reason, so a result row says why a job failed instead of only an exit code.
export function renderExitError(code, stderrTail = "") {
  const marker = "RENDER FAILED:";
  const lines = String(stderrTail).split(/\r?\n/).filter((l) => l.startsWith(marker));
  const why = lines.length ? lines[lines.length - 1].slice(marker.length).trim() : "";
  const e = new Error(`comfy-render exited ${code}` + (why ? ": " + why : ""));
  if (code === RENDER_EXIT_SERVER_UNUSABLE) e.serverUnusable = true;
  return e;
}

// runBatchJobs: the --batch loop. runJob renders one job and throws on failure; record
// receives each result line; log receives the progress lines. The caller owns the
// files and the GPU slot, so this stays free of I/O.
// A failed job is recorded and the batch goes on (the Go side reads per-job status from
// an exit-0 batch), unless the failure is serverUnusable: then every later job would fail
// against the same server, so each gets a "not run" row and the batch throws. That is
// what lets the caller's teardown free the card and its lease (C-83, 2026-10-01: a
// poisoned ComfyUI failed jobs 3-6 over 57 min while the media lease held every card).
export async function runBatchJobs({ jobs, runJob, record, log = () => {}, now = Date.now }) {
  for (let i = 0; i < jobs.length; i++) {
    const t0 = now();
    try {
      await runJob(jobs[i], i);
    } catch (e) {
      record(resultLine(i, jobs[i], false, now() - t0, e.message));
      log(`batch ${i + 1}/${jobs.length} FAILED: ${e.message} (${Math.round((now() - t0) / 1000)}s)`);
      if (e && e.serverUnusable) {
        const why = `ComfyUI became unusable at job ${i + 1}/${jobs.length} (${e.message})`;
        for (let k = i + 1; k < jobs.length; k++) record(resultLine(k, jobs[k], false, 0, "not run: " + why));
        const abort = new Error(`${why}; ${jobs.length - i - 1} jobs not run, recorded as such`);
        abort.serverUnusable = true;
        throw abort;
      }
      continue;
    }
    record(resultLine(i, jobs[i], true, now() - t0));
    log(`batch ${i + 1}/${jobs.length} done (${Math.round((now() - t0) / 1000)}s)`);
  }
}
