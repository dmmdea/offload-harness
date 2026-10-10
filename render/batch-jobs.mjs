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

// BATCH_EXIT_JOBS_FAILED: the exit code of a --batch parent (comfy-generate.mjs, comfy-inpaint.mjs)
// that ran every job to the end and at least one of them failed. Until 0.178.0 such a batch exited 0,
// so a caller had to grep the log for RENDER FAILED to learn that jobs had failed (2026-10-09: 21 of
// 36 pictures, after the disk filled). The set a --batch parent now ends with: 0 every job
// rendered, this code (the ok:false rows of the results file name the failures), 1 the batch could not
// run to the end (setup error, an unusable ComfyUI, a full disk; the jobs not run are recorded as
// such), 2 usage. It is not 3 on purpose: 3 is the child's "server unusable", and a parent that reads
// a child's 3 as that verdict (renderExitError) would misread a batch that merely had failed jobs.
// A cross-process contract: internal/imagegen.BatchExitJobsFailed is the Go caller's copy of it.
export const BATCH_EXIT_JOBS_FAILED = 4;

// The errnos that mean the volume an output goes to cannot take another byte: ENOSPC (full), EDQUOT
// (quota used up) and EROFS (remounted read-only, which is what a failing disk does).
export const DISK_FULL_CODES = Object.freeze(["ENOSPC", "EDQUOT", "EROFS"]);
const DISK_FULL_TEXT = /\b(?:ENOSPC|EDQUOT|EROFS)\b|no space left on device|disk quota exceeded|read-only file system|not enough space on the disk/i;

// isDiskFullError: does this failure say the output volume is full? By errno code when the error
// came from an fs call in this process (comfy-inpaint renders in-process), and by its words when it
// crossed a process boundary: a comfy-render child reports only its "RENDER FAILED:" line, and a
// ComfyUI that cannot save its own output reports Python's "[Errno 28] No space left on device"
// in an exec error. Every later job writes to the same place and fails the same way, which is why
// this class stops a batch (runBatchJobs, inpaint-jobs.mjs batchAbort).
export function isDiskFullError(e) {
  if (e == null) return false;
  if (e.diskFull === true || DISK_FULL_CODES.includes(e.code)) return true;
  return DISK_FULL_TEXT.test(String(e.message ?? e));
}

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

// batchExitCode: how a --batch parent that ran every job ends: 0 when none failed,
// BATCH_EXIT_JOBS_FAILED when any did. `summary` is what runBatchJobs returns.
export function batchExitCode(summary) {
  return summary && summary.failed > 0 ? BATCH_EXIT_JOBS_FAILED : 0;
}

// batchEndLine: the last stderr line of a batch that finished with failed jobs, so the
// log says it too, not only the exit code.
export function batchEndLine(summary, resultsPath) {
  return `batch finished with failed jobs: ${summary.failed} of ${summary.total} failed, ${summary.ok} ok ` +
    `(exit ${BATCH_EXIT_JOBS_FAILED}); the rows with "ok":false in ${resultsPath} name them`;
}

// runBatchJobs: the --batch loop. runJob renders one job and throws on failure; record
// receives each result line; log receives the progress lines. The caller owns the
// files and the GPU slot, so this stays free of I/O. Returns {total, ok, failed} for a
// batch that ran every job (batchExitCode turns it into the exit code).
// A failed job is recorded and the batch goes on (the Go side reads per-job status), unless
// the failure means no later job can succeed either. Then each later job gets a "not run"
// row and the batch throws, which is what lets the caller's teardown free the card and its
// lease:
//   - serverUnusable: every later job would fail against the same server (C-83, 2026-10-01:
//     a poisoned ComfyUI failed jobs 3-6 over 57 min while the media lease held every card);
//   - a full disk (isDiskFullError): every later job writes to the same volume (2026-10-09:
//     the batch went on past a full drive and left 21 zero-byte pictures).
export async function runBatchJobs({ jobs, runJob, record, log = () => {}, now = Date.now }) {
  let ok = 0, failed = 0;
  const stop = (i, why, flag) => {
    for (let k = i + 1; k < jobs.length; k++) record(resultLine(k, jobs[k], false, 0, "not run: " + why));
    const abort = new Error(`${why}; ${jobs.length - i - 1} jobs not run, recorded as such`);
    abort[flag] = true;
    return abort;
  };
  for (let i = 0; i < jobs.length; i++) {
    const t0 = now();
    try {
      await runJob(jobs[i], i);
    } catch (e) {
      failed++;
      record(resultLine(i, jobs[i], false, now() - t0, e.message));
      log(`batch ${i + 1}/${jobs.length} FAILED: ${e.message} (${Math.round((now() - t0) / 1000)}s)`);
      if (e && e.serverUnusable) {
        throw stop(i, `ComfyUI became unusable at job ${i + 1}/${jobs.length} (${e.message})`, "serverUnusable");
      }
      if (isDiskFullError(e)) {
        throw stop(i, `the disk is full at job ${i + 1}/${jobs.length}, writing ${jobs[i].out} (${e.message})`, "diskFull");
      }
      continue;
    }
    ok++;
    record(resultLine(i, jobs[i], true, now() - t0));
    log(`batch ${i + 1}/${jobs.length} done (${Math.round((now() - t0) / 1000)}s)`);
  }
  return { total: jobs.length, ok, failed };
}
