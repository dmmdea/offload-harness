// node --test render/batch-exit-codes.test.mjs
//
// The REAL comfy-generate.mjs --batch and comfy-inpaint.mjs --batch (and the real comfy-render.mjs child
// the first one spawns) against a FAKE ComfyUI: an http server on an ephemeral port that answers just
// enough of the API. What the unit tests cannot show is what a caller sees: the process exit code, the
// results file and the files left on disk.
//
// Nothing here touches a GPU, a lease or a real ComfyUI. The runners are given a non-default --api that
// the fake answers, so ensureComfy REUSES it and can never launch one (a launch needs the default port or
// an explicit --port in COMFY_EXTRA_ARGS, which is blanked); --no-lock takes no lease; llama-swap is
// pointed at a dead port so nothing on the machine is unloaded; the environment is scrubbed of every
// lease, instance and ComfyUI variable and PATH holds only node, so no comfyui-pp-cli is found and
// submission is the raw HTTP path.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const GENERATE = join(HERE, "comfy-generate.mjs");
const INPAINT = join(HERE, "comfy-inpaint.mjs");
const REPO_CLI = join(HERE, "..", "tools", "comfyui", "bin", "comfyui-pp-cli" + (process.platform === "win32" ? ".exe" : ""));
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", "base64");

// The fake answers /system_stats, /prompt, /history/<id>, /view and /free. A prompt whose text carries
// BOOM-DISK is refused the way a ComfyUI whose own drive is full refuses it (Python's errno text);
// BOOM-NODE is a validation refusal, an ordinary per-job failure; anything else renders.
async function fakeComfy() {
  const seen = { prompts: [] };
  const server = createServer((req, res) => {
    const url = new URL(req.url, "http://fake");
    if (req.method === "GET" && url.pathname === "/system_stats") { res.setHeader("content-type", "application/json"); res.end('{"system":{"argv":["main.py"]}}'); return; }
    if (req.method === "POST" && url.pathname === "/prompt") {
      let body = "";
      req.on("data", (d) => { body += d; });
      req.on("end", () => {
        const text = JSON.stringify(JSON.parse(body).prompt);
        seen.prompts.push(text);
        if (text.includes("BOOM-DISK")) { res.statusCode = 500; res.end("OSError: [Errno 28] No space left on device"); return; }
        if (text.includes("BOOM-NODE")) { res.statusCode = 400; res.end('{"error":{"type":"prompt_outputs_failed_validation"}}'); return; }
        res.setHeader("content-type", "application/json");
        res.end(JSON.stringify({ prompt_id: "p" + seen.prompts.length, number: seen.prompts.length, node_errors: {} }));
      });
      return;
    }
    if (url.pathname.startsWith("/history/")) {
      const id = url.pathname.slice("/history/".length);
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ [id]: { status: { status_str: "success", completed: true, messages: [] }, outputs: { 9: { images: [{ filename: "render_00001_.png", subfolder: "", type: "output" }] } } } }));
      return;
    }
    if (url.pathname === "/view") { res.setHeader("content-type", "image/png"); res.end(PNG); return; }
    if (req.method === "POST" && url.pathname === "/free") { res.setHeader("content-type", "application/json"); res.end("{}"); return; }
    res.statusCode = 404; res.end("{}");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return { url: `http://127.0.0.1:${server.address().port}`, seen, close: () => new Promise((r) => server.close(r)) };
}

function scrubbedEnv(comfyDir) {
  const env = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (/^(GPU_LEASE_|COMFY|LLAMA_SWAP|SDCPP|OFFLOAD_)/i.test(k) || /^path$/i.test(k)) continue;
    env[k] = v;
  }
  env.PATH = dirname(process.execPath);
  env.LLAMA_SWAP_API = "http://127.0.0.1:9";
  env.COMFY_DIR = comfyDir;
  env.COMFY_EXTRA_ARGS = "";
  env.COMFY_WAIT_SEC = "60";
  return env;
}

function run(script, args, env) {
  return new Promise((resolve, reject) => {
    const c = spawn(process.execPath, [script, ...args], { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "";
    c.stdout.on("data", (d) => { stdout += d; });
    c.stderr.on("data", (d) => { stderr += d; });
    const timer = setTimeout(() => { c.kill(); reject(new Error("runner did not finish in 120 s\n" + stderr)); }, 120_000);
    c.on("close", (code) => { clearTimeout(timer); resolve({ code, stdout, stderr }); });
    c.on("error", (e) => { clearTimeout(timer); reject(e); });
  });
}

const jsonl = (path) => readFileSync(path, "utf8").split(/\r?\n/).filter(Boolean).map((l) => JSON.parse(l));
const litter = (dir) => readdirSync(dir).filter((n) => n.includes(".partial-"));

async function scenario(t, body) {
  if (existsSync(REPO_CLI)) {
    t.skip("a locally built comfyui-pp-cli would take the CLI submission path instead of the raw one this test drives");
    return;
  }
  const fake = await fakeComfy();
  const dir = mkdtempSync(join(tmpdir(), "batch-exit-"));
  const comfyDir = join(dir, "comfy");
  mkdirSync(join(comfyDir, "input"), { recursive: true });
  const outDir = join(dir, "renders");
  mkdirSync(outDir);
  try {
    await body({ fake, dir, outDir, env: scrubbedEnv(comfyDir), jobsPath: join(dir, "jobs.jsonl"), resultsPath: join(dir, "results.jsonl") });
  } finally {
    await fake.close();
    rmSync(dir, { recursive: true, force: true });
  }
}

const genJobs = (outDir, specs) => specs.map(([name, prompt], i) => JSON.stringify({ prompt, out: join(outDir, name), seed: 100 + i })).join("\n") + "\n";
const genArgs = (s, fake) => ["--batch", s.jobsPath, "--results", s.resultsPath, "--api", fake.url, "--no-lock"];

test("comfy-generate --batch: a full disk at job 2 stops the batch with exit 1, jobs 3..4 are 'not run' and never submitted, and nothing is left at their paths", async (t) => {
  await scenario(t, async (s) => {
    writeFileSync(s.jobsPath, genJobs(s.outDir, [["a.png", "a red bike"], ["b.png", "BOOM-DISK a blue car"], ["c.png", "a green boat"], ["d.png", "a pink kite"]]));
    const r = await run(GENERATE, genArgs(s, s.fake), s.env);
    assert.equal(r.code, 1, r.stderr);
    const rows = jsonl(s.resultsPath);
    assert.deepEqual(rows.map((x) => [x.i, x.ok]), [[0, true], [1, false], [2, false], [3, false]]);
    assert.match(rows[1].error, /No space left on device/);
    assert.match(rows[2].error, /^not run: the disk is full at job 2\/4, writing .*b\.png/);
    assert.match(r.stderr, /IMAGE BATCH FAILED: the disk is full at job 2\/4, writing .*b\.png/);
    assert.match(r.stderr, /2 jobs not run, recorded as such/);
    assert.equal(s.fake.seen.prompts.length, 2, "jobs 3 and 4 must never reach ComfyUI");
    assert.ok(statSync(join(s.outDir, "a.png")).size > 0, "job 1 rendered before the disk filled");
    for (const n of ["b.png", "c.png", "d.png"]) assert.equal(existsSync(join(s.outDir, n)), false, n + " must not exist");
    assert.deepEqual(litter(s.outDir), []);
  });
});

test("comfy-generate --batch: ordinary failures (a refused prompt, an output path that cannot be replaced) do not stop it; it ends with exit 4 and leaves no half-written file", async (t) => {
  await scenario(t, async (s) => {
    mkdirSync(join(s.outDir, "occupied.png")); // a directory where the picture should go: the delivery rename fails
    writeFileSync(s.jobsPath, genJobs(s.outDir, [["a.png", "BOOM-NODE a red bike"], ["occupied.png", "a blue car"], ["c.png", "a green boat"]]));
    const r = await run(GENERATE, genArgs(s, s.fake), s.env);
    assert.equal(r.code, 4, r.stderr);
    const rows = jsonl(s.resultsPath);
    assert.deepEqual(rows.map((x) => [x.i, x.ok]), [[0, false], [1, false], [2, true]]);
    assert.match(r.stderr, /batch finished with failed jobs: 2 of 3 failed, 1 ok \(exit 4\)/);
    assert.match(r.stderr, /RENDER FAILED/, "the child's own reason is still logged");
    assert.equal(existsSync(join(s.outDir, "a.png")), false);
    assert.ok(statSync(join(s.outDir, "occupied.png")).isDirectory(), "what was at the path is untouched");
    assert.ok(statSync(join(s.outDir, "c.png")).size > 0, "the batch went on to the end");
    assert.deepEqual(litter(s.outDir), [], "a failed delivery leaves no staged file");
  });
});

test("comfy-generate --batch: a clean batch exits 0 and says nothing about failed jobs", async (t) => {
  await scenario(t, async (s) => {
    writeFileSync(s.jobsPath, genJobs(s.outDir, [["a.png", "a red bike"]]));
    const r = await run(GENERATE, genArgs(s, s.fake), s.env);
    assert.equal(r.code, 0, r.stderr);
    assert.deepEqual(jsonl(s.resultsPath).map((x) => x.ok), [true]);
    assert.doesNotMatch(r.stderr, /finished with failed jobs/);
    assert.ok(statSync(join(s.outDir, "a.png")).size > 0);
    assert.deepEqual(litter(s.outDir), []);
  });
});

const inpaintJobs = (s, specs) => {
  writeFileSync(join(s.dir, "image.png"), PNG);
  writeFileSync(join(s.dir, "mask.png"), PNG);
  return specs.map(([name, prompt], i) => JSON.stringify({ out: join(s.outDir, name), image: join(s.dir, "image.png"), mask: join(s.dir, "mask.png"), prompt, seed: 7 + i })).join("\n") + "\n";
};
const inpaintArgs = (s, fake) => ["--batch", s.jobsPath, "--results", s.resultsPath, "--api", fake.url, "--no-lock", "--ckpt", "fake.safetensors"];

test("comfy-inpaint --batch: one failed job among good ones finishes the batch with exit 4", async (t) => {
  await scenario(t, async (s) => {
    writeFileSync(s.jobsPath, inpaintJobs(s, [["a.png", "fix the sky"], ["b.png", "BOOM-NODE fix the roof"]]));
    const r = await run(INPAINT, inpaintArgs(s, s.fake), s.env);
    assert.equal(r.code, 4, r.stderr);
    assert.deepEqual(jsonl(s.resultsPath).map((x) => [x.i, x.ok]), [[0, true], [1, false]]);
    assert.match(r.stderr, /batch finished with failed jobs: 1 of 2 failed, 1 ok \(exit 4\)/);
    assert.ok(statSync(join(s.outDir, "a.png")).size > 0);
    assert.equal(existsSync(join(s.outDir, "b.png")), false);
    assert.deepEqual(litter(s.outDir), []);
  });
});

test("comfy-inpaint --batch: a full disk stops it at once (exit 1, an aborted row with reason disk_full) instead of waiting for three failures", async (t) => {
  await scenario(t, async (s) => {
    writeFileSync(s.jobsPath, inpaintJobs(s, [["a.png", "BOOM-DISK fix the sky"], ["b.png", "fix the roof"], ["c.png", "fix the door"]]));
    const r = await run(INPAINT, inpaintArgs(s, s.fake), s.env);
    assert.equal(r.code, 1, r.stderr);
    const rows = jsonl(s.resultsPath);
    assert.equal(rows.length, 2, "the failed job's row and the aborted row");
    assert.equal(rows[0].ok, false);
    assert.equal(rows[1]._row, "aborted");
    assert.equal(rows[1].reason, "disk_full");
    assert.equal(rows[1].not_attempted, 2);
    assert.match(r.stderr, /INPAINT BATCH FAILED: the disk is full/);
    assert.equal(s.fake.seen.prompts.length, 1, "jobs 2 and 3 must never reach ComfyUI");
    assert.deepEqual(readdirSync(s.outDir), []);
  });
});
