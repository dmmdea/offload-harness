// batch-jobs.mjs — pure helpers for comfy-generate.mjs --batch mode: parse the jobs
// JSONL, build a per-job comfy-render argv (job fields beat shared defaults; the
// machine's model-binding flags are shared-only), and format per-job result lines.
// Pure functions, no I/O — the script owns files and process lifecycle. No deps.

// Per-job overridable request params vs shared-only machine binding flags. A job may
// NOT override the binding (ckpt/vae/...): which checkpoint a machine renders with is
// per-machine config threaded by the Go harness, never per-prompt data.
const JOB_PARAMS = ["negative", "width", "height", "steps", "seed"];
const SHARED_ONLY = ["ckpt", "vae", "cfg", "sampler", "scheduler", "family"];

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
    if (shared[k] != null && shared[k] !== "") args.push("--" + k, String(shared[k]));
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
