// node --test render/batch-jobs.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { parseJobs, jobArgs, resultLine } from "./batch-jobs.mjs";

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

test("resultLine: ok and error shapes", () => {
  const ok = JSON.parse(resultLine(0, { out: "a.png", seed: 7 }, true, 1234));
  assert.deepEqual(ok, { i: 0, out: "a.png", seed: 7, ok: true, ms: 1234 });
  const bad = JSON.parse(resultLine(1, { out: "b.png" }, false, 55, "comfy-render exited 1"));
  assert.equal(bad.ok, false);
  assert.equal(bad.error, "comfy-render exited 1");
});
