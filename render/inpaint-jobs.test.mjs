// node --test render/inpaint-jobs.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { parseInpaintJobs, qwenRecipe } from "./inpaint-jobs.mjs";
import * as inpaintMod from "./inpaint-jobs.mjs";
import { QWEN_INPAINT_PRESETS } from "./wf-qwen-inpaint.mjs";

const J = (o) => JSON.stringify({ out: "a.png", image: "i.png", mask: "m.png", prompt: "p", seed: 1, ...o });

test("parse: happy path, blank lines skipped, seed coerced to number", () => {
  const jobs = parseInpaintJobs(J({}) + "\n\n" + J({ out: "b.png", seed: "7" }) + "\n");
  assert.equal(jobs.length, 2);
  assert.strictEqual(jobs[1].seed, 7);
});

test("parse: required fields, seed presence, seed finiteness, duplicate outs all refuse loudly", () => {
  assert.throws(() => parseInpaintJobs(J({ image: undefined })), /"image" \(string\) is required/);
  assert.throws(() => parseInpaintJobs(JSON.stringify({ out: "a.png", image: "i.png", mask: "m.png", prompt: "p" })), /"seed" is required/);
  assert.throws(() => parseInpaintJobs(J({ seed: "abc" })), /not a finite number/);
  assert.throws(() => parseInpaintJobs(J({}) + "\n" + J({})), /duplicate out path/);
  assert.throws(() => parseInpaintJobs("{not json"), /invalid JSON/);
});

test("qwenRecipe: preset default, job>flags>preset precedence, matched-pair rule", () => {
  const P = QWEN_INPAINT_PRESETS;
  assert.deepEqual(qwenRecipe({}, {}, P), { steps: 20, cfg: 2.5, lora: "" });
  assert.equal(qwenRecipe({}, { preset: "lightning4" }, P).steps, 4);
  // job wins over flags
  const r = qwenRecipe({ steps: 8, cfg: 1.5 }, { steps: "20", cfg: "2.5" }, P);
  assert.deepEqual([r.steps, r.cfg], [8, 1.5]);
  // half-override refused, from either source alone
  assert.throws(() => qwenRecipe({}, { steps: "8" }, P), /TOGETHER/);
  assert.throws(() => qwenRecipe({ cfg: 1.0 }, {}, P), /TOGETHER/);
  // unknown preset refused with the roster
  assert.throws(() => qwenRecipe({ preset: "lightening4" }, {}, P), /unknown qwen preset/);
});

// C-83: the inpaint batch's consecutive-failure abort needed three failures in a row; on a
// poisoned server each one cost a full wait budget. An unusable server stops it at once.
test("batchAbort: an unusable server stops the inpaint batch at once; ordinary failures stop it only after the consecutive limit (C-83)", () => {
  assert.equal(typeof inpaintMod.batchAbort, "function", "inpaint-jobs.mjs must export batchAbort");
  const dead = new Error("ComfyUI's CUDA context is broken"); dead.serverUnusable = true;
  const a = inpaintMod.batchAbort({ err: dead, consecFail: 1, maxConsecFail: 3 });
  assert.equal(a.reason, "server_unusable");
  assert.match(a.message, /CUDA context is broken/);
  const plain = new Error("node error");
  assert.equal(inpaintMod.batchAbort({ err: plain, consecFail: 2, maxConsecFail: 3 }), null);
  const c = inpaintMod.batchAbort({ err: plain, consecFail: 3, maxConsecFail: 3 });
  assert.equal(c.reason, "consecutive_failures");
  assert.equal(c.message, "3 consecutive failures (last: node error)");
  assert.equal(inpaintMod.batchAbort({ err: null, consecFail: 0, maxConsecFail: 3 }), null);
});
