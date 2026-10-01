// node --test render/batch-jobs.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { parseJobs, jobArgs, resultLine, runBatchJobs, JOB_PARAM_FLAGS, SHARED_BINDING_FLAGS } from "./batch-jobs.mjs";
import * as batchMod from "./batch-jobs.mjs";
import { buildRenderGraph, parseRenderArgs } from "./comfy-render.mjs";

test("parseJobs: valid JSONL, skips blank lines", () => {
  const jobs = parseJobs('{"prompt":"a red bike","out":"a.png","seed":7}\n\n{"prompt":"a green apple","out":"b.png"}\n');
  assert.equal(jobs.length, 2);
  assert.equal(jobs[0].seed, 7);
  assert.equal(jobs[1].out, "b.png");
});

test("parseJobs: invalid JSON names the line", () => {
  assert.throws(() => parseJobs('{"prompt":"ok","out":"a.png"}\n{nope}\n'), /line 2/);
});

test("parseJobs: missing prompt or out names the line", () => {
  assert.throws(() => parseJobs('{"out":"a.png"}\n'), /line 1.*prompt/);
  assert.throws(() => parseJobs('{"prompt":"x"}\n'), /line 1.*out/);
});

test("jobArgs: job fields override shared, binding flags come from shared only", () => {
  const args = jobArgs(
    { prompt: "p", out: "o.png", seed: 42, negative: "cats" },
    { api: "http://x:1", negative: "text, watermark", ckpt: "m.safetensors", cfg: "5", family: "hidream-o1" },
  );
  assert.deepEqual(args.slice(0, 2), ["o.png", "p"]);
  const flag = (k) => args[args.indexOf("--" + k) + 1];
  assert.equal(flag("api"), "http://x:1");
  assert.equal(flag("negative"), "cats", "job negative beats shared");
  assert.equal(flag("seed"), "42");
  assert.equal(flag("ckpt"), "m.safetensors");
  assert.equal(flag("cfg"), "5");
  assert.equal(flag("family"), "hidream-o1");
  assert.ok(!args.includes("--width"), "unset numerics emit no flag");
});

test("jobArgs: EVERY exported binding + job flag is emitted — the composed chain, not per-hop lists", () => {
  // Review-caught 2026-08-14: the pool flags were collected by comfy-generate
  // and silently dropped here, leaving the pooled seat rendering single-card
  // through every harness path while all per-hop tests stayed green. The
  // collector now derives from these exports; this test pins the OTHER half —
  // jobArgs must emit every key it declares, so a flag can never again vanish
  // between the two scripts.
  const shared = { api: "http://x:1" };
  for (const k of [...JOB_PARAM_FLAGS, ...SHARED_BINDING_FLAGS]) shared[k] = "V-" + k;
  const args = jobArgs({ prompt: "p", out: "o.png" }, shared);
  for (const k of [...JOB_PARAM_FLAGS, ...SHARED_BINDING_FLAGS]) {
    const i = args.indexOf("--" + k);
    assert.ok(i >= 0, "--" + k + " must be emitted");
    assert.equal(args[i + 1], "V-" + k, "--" + k + " carries the shared value");
  }
  for (const k of ["pool-vvram", "pool-compute", "pool-donor"]) {
    assert.ok(SHARED_BINDING_FLAGS.includes(k), k + " must stay in the binding list (the pooled-seat regression)");
  }
});

test("jobArgs -> comfy-render: the qwen-image-2.1 flags survive the wrapper hop and reach the graph", () => {
  for (const k of ["schedule", "transparent"]) {
    assert.ok(SHARED_BINDING_FLAGS.includes(k), k + " must be in the binding list or comfy-generate drops it");
  }
  // The composed chain, not per-hop lists: what comfy-generate emits is exactly what
  // comfy-render parses into a graph.
  const shared = {
    family: "qwen-image-2.1", ckpt: "qwen_image_2.1_bf16.safetensors", clip: "qwen3vl_8b_bf16.safetensors",
    vae: "qwen_image_2.1_vae_bf16.safetensors", schedule: "comfy", transparent: "1",
  };
  const argv = jobArgs({ prompt: "a sticker of a fox", out: "o.png", seed: 3 }, shared);
  const { graph } = buildRenderGraph({ ...parseRenderArgs(argv), env: {} });
  const types = Object.values(graph).map((n) => n.class_type);
  assert.ok(types.includes("KSampler") && !types.includes("ManualSigmas"), "--schedule comfy reached the builder");
  assert.ok(!types.includes("SplitImageWithAlpha"), "--transparent 1 reached the builder");
});

test("jobArgs: an EMPTY shared lora still forwards — it strips a preset's LoRA downstream", () => {
  const args = jobArgs({ prompt: "p", out: "o.png" }, { family: "qwen-image", preset: "lightning4", lora: "" });
  const i = args.indexOf("--lora");
  assert.ok(i >= 0, "--lora present despite empty value");
  assert.equal(args[i + 1], "", "empty string survives to comfy-render");
  // ...while other empty shared values still emit no flag at all.
  const none = jobArgs({ prompt: "p", out: "o.png" }, { family: "qwen-image", clip: "", shift: "" });
  assert.ok(!none.includes("--clip") && !none.includes("--shift"));
});

test("resultLine: ok and error shapes", () => {
  const ok = JSON.parse(resultLine(0, { out: "a.png", seed: 7 }, true, 1234));
  assert.deepEqual(ok, { i: 0, out: "a.png", seed: 7, ok: true, ms: 1234 });
  const bad = JSON.parse(resultLine(1, { out: "b.png" }, false, 55, "comfy-render exited 1"));
  assert.equal(bad.ok, false);
  assert.equal(bad.error, "comfy-render exited 1");
});

// C-83 (2026-10-01): a poisoned ComfyUI failed jobs 3-6 of a qwen-image batch one by one
// (48 min + 3 x 3 min) while the media lease held every card and nothing ran.
test("runBatchJobs: a serverUnusable failure stops the batch — the failed job and every later job get a row, later jobs never run (C-83)", async () => {
  const jobs = [0, 1, 2, 3, 4].map((i) => ({ prompt: "p" + i, out: `o${i}.png`, seed: 10 + i }));
  const ran = [], rows = [], logs = [];
  const runJob = async (job, i) => {
    ran.push(i);
    if (i === 2) {
      const e = new Error("comfy-render exited 3: ComfyUI's CUDA context is broken");
      e.serverUnusable = true;
      throw e;
    }
  };
  const err = await runBatchJobs({ jobs, runJob, record: (l) => rows.push(JSON.parse(l)), log: (l) => logs.push(l), now: () => 0 })
    .then(() => null, (e) => e);
  assert.deepEqual(ran, [0, 1, 2], "no job may run on an unusable server");
  assert.ok(err, "the batch must fail loud");
  assert.equal(err.serverUnusable, true);
  assert.match(err.message, /job 3\/5/);
  assert.match(err.message, /2 jobs not run/);
  assert.deepEqual(rows.map((r) => [r.i, r.ok]), [[0, true], [1, true], [2, false], [3, false], [4, false]]);
  assert.match(rows[2].error, /CUDA context is broken/);
  assert.match(rows[3].error, /^not run: ComfyUI became unusable at job 3\/5/);
  assert.equal(rows[4].out, "o4.png");
  assert.equal(rows[4].seed, 14);
  assert.ok(logs.some((l) => /FAILED/.test(l)), "the failure is logged as a failure, not as done");
});

test("runBatchJobs: an ordinary failure is recorded and the batch goes on (Go reads per-job status from an exit-0 batch)", async () => {
  const jobs = [0, 1, 2].map((i) => ({ prompt: "p" + i, out: `o${i}.png` }));
  const ran = [], rows = [];
  await runBatchJobs({
    jobs,
    runJob: async (j, i) => { ran.push(i); if (i === 1) throw new Error("comfy-render exited 1: ComfyUI exec error"); },
    record: (l) => rows.push(JSON.parse(l)),
    now: () => 0,
  });
  assert.deepEqual(ran, [0, 1, 2]);
  assert.deepEqual(rows.map((r) => r.ok), [true, false, true]);
});

test("renderExitError: exit 3 is serverUnusable and carries the child's RENDER FAILED reason (C-83)", () => {
  assert.equal(typeof batchMod.renderExitError, "function", "batch-jobs.mjs must export renderExitError");
  const e = batchMod.renderExitError(3, "queued x seed 1\nRENDER FAILED: ComfyUI's CUDA context is broken: GET /system_stats answered HTTP 500\n");
  assert.equal(e.serverUnusable, true);
  assert.equal(e.message, "comfy-render exited 3: ComfyUI's CUDA context is broken: GET /system_stats answered HTTP 500");
  const o = batchMod.renderExitError(1, "");
  assert.ok(!o.serverUnusable);
  assert.equal(o.message, "comfy-render exited 1");
  assert.equal(batchMod.RENDER_EXIT_SERVER_UNUSABLE, 3, "the exit code is a cross-process contract");
});
