// node --test render/inpaint-jobs.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { parseInpaintJobs, qwenRecipe } from "./inpaint-jobs.mjs";
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
