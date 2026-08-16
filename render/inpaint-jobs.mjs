// inpaint-jobs.mjs — the PURE parts of comfy-inpaint.mjs's batch mode, extracted
// so they are importable and testable (the batch-jobs.mjs convention: the runner
// script stays a thin lifecycle shell, the invariants live in a tested module).
//
// Inpaint jobs are deliberately NOT batch-jobs.mjs jobs: they carry image+mask
// fields the shared T2I emit lists know nothing about, and their per-job knob
// set (grow_mask/denoise/strength) is inpaint-specific.

/** Parse an inpaint batch JSONL. Every job needs out/image/mask/prompt (strings)
 * and a FINITE seed — reproducibility is the whole point of a sweep, so silent
 * random or NaN seeds are refused at parse time, not discovered server-side.
 * Duplicate out paths are refused too: two jobs writing one file means the
 * results claim N outputs while disk holds N-1, and the eval scores one image
 * under two configs. */
export function parseInpaintJobs(text) {
  const jobs = [];
  const seenOut = new Set();
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
    if (j.seed == null) {
      throw new Error(`batch line ${ln + 1}: "seed" is required for reproducible sweeps (no silent random seeds in batch mode)`);
    }
    j.seed = Number(j.seed);
    if (!Number.isFinite(j.seed)) {
      throw new Error(`batch line ${ln + 1}: "seed" is not a finite number`);
    }
    if (seenOut.has(j.out)) {
      throw new Error(`batch line ${ln + 1}: duplicate out path ${j.out}`);
    }
    seenOut.add(j.out);
    jobs.push(j);
  }
  return jobs;
}

/** Resolve the qwen steps/cfg/lora recipe for one job: job knobs win over CLI
 * flags win over the named preset — but steps and cfg must move TOGETHER
 * (half-overriding a matched pairing is the classic way to burn a render:
 * Lightning at 20 steps produces mush, the base at 4 steps produces noise). */
export function qwenRecipe(job, flags, presets) {
  const presetName = job.preset ?? flags.preset ?? "full";
  const preset = presets[presetName];
  if (!preset) {
    throw new Error(`unknown qwen preset "${presetName}" (have: ${Object.keys(presets).join(", ")})`);
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
