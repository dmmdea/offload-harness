// node --test render/lease-progress.test.mjs
//
// F8: a running render helper heartbeats the progress file of the lease it runs under, so a healthy long
// render is never labelled "expired ... open to a takeover" while a hung or lost one still is. Nothing here
// touches a GPU, a lease or a real ComfyUI: the unit tests inject the clock, the fetch and the append; the
// end-to-end test runs the real comfy-generate.mjs --batch (and the real comfy-render.mjs it spawns)
// against a fake ComfyUI on an ephemeral port, the way batch-exit-codes.test.mjs does.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  PROGRESS_FILE_ENV, BEAT_EVERY_MS, leaseProgressFile, createLeaseHeartbeat, leaseBeat, resetLeaseBeatForTest,
} from "./lease-progress.mjs";
import { readEpochRecord, readLease, withGpuSlot } from "./gpu-lock.mjs";
import { pollOutputs } from "./comfy-submit.mjs";
import { runBatchJobs } from "./batch-jobs.mjs";
import { satisfyModels } from "./manifest-satisfy.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const GENERATE = join(HERE, "comfy-generate.mjs");
const REPO_CLI = join(HERE, "..", "tools", "comfyui", "bin", "comfyui-pp-cli" + (process.platform === "win32" ? ".exe" : ""));
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", "base64");

const tmp = () => mkdtempSync(join(tmpdir(), "lease-progress-"));
const lines = (path) => readFileSync(path, "utf8").split(/\r?\n/).filter(Boolean).map((l) => JSON.parse(l));
const clock = () => { let t = 1_000_000; return { now: () => t, sleep: async (ms) => { t += ms; }, jump: (ms) => { t += ms; } }; };
const json = (obj) => ({ ok: true, json: async () => obj, text: async () => "" });

// ---------------------------------------------------------------------------------------
// where the file comes from
// ---------------------------------------------------------------------------------------

test("leaseProgressFile: the wrapper's env var wins, and only an absolute path counts", () => {
  const dir = tmp();
  try {
    const abs = join(dir, "p.jsonl");
    assert.equal(leaseProgressFile({ [PROGRESS_FILE_ENV]: abs }), abs);
    assert.equal(leaseProgressFile({ [PROGRESS_FILE_ENV]: "  " + abs + "  " }), abs);
    // A relative path would be relative to the helper's directory, not the reader's.
    assert.equal(leaseProgressFile({ [PROGRESS_FILE_ENV]: "relative/p.jsonl" }), "");
    assert.equal(leaseProgressFile({}), "");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("leaseProgressFile: with no env var it reads the inherited lease's own record (a detached hold declared --progress-file)", () => {
  const dir = tmp();
  try {
    const file = join(dir, "work", "p.jsonl");
    mkdirSync(join(dir, "e"), { recursive: true });
    // A card-scoped lease keeps e/<epoch>.json ...
    writeFileSync(join(dir, "e", "7.json"), JSON.stringify({ epoch: 7, state: "active", progress: { file, stall_ms: 600000 } }));
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "7" }), file);
    // ... and the record of another epoch is not this lease's.
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "8" }), "");
    // A whole-node lease keeps meta.json.
    const whole = tmp();
    try {
      writeFileSync(join(whole, "meta.json"), JSON.stringify({ epoch: 3, progress: { file, stall_ms: 1 } }));
      assert.equal(leaseProgressFile({ GPU_LEASE_DIR: whole, GPU_LEASE_EPOCH: "3" }), file);
      assert.equal(leaseProgressFile({ GPU_LEASE_DIR: whole, GPU_LEASE_EPOCH: "4" }), "", "an epoch that is not the record's");
    } finally { rmSync(whole, { recursive: true, force: true }); }
    // A lease that declares none, an unreadable record and a missing directory all read as "no file".
    writeFileSync(join(dir, "e", "9.json"), JSON.stringify({ epoch: 9, state: "active" }));
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "9" }), "");
    writeFileSync(join(dir, "e", "10.json"), "{not json");
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "10" }), "");
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: join(dir, "nowhere"), GPU_LEASE_EPOCH: "1" }), "");
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "not-a-number" }), "");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("the record reader here and gpu-lock.mjs's read the same two files (this module does not import gpu-lock: comfy-submit imports this one)", () => {
  const dir = tmp();
  try {
    mkdirSync(join(dir, "e"), { recursive: true });
    writeFileSync(join(dir, "e", "5.json"), JSON.stringify({ epoch: 5, progress: { file: join(dir, "a") } }));
    writeFileSync(join(dir, "meta.json"), JSON.stringify({ epoch: 6, progress: { file: join(dir, "b") } }));
    assert.equal(readEpochRecord(dir, 5).progress.file, join(dir, "a"));
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "5" }), join(dir, "a"));
    assert.equal(readLease(dir).progress.file, join(dir, "b"));
    assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "6" }), join(dir, "b"));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

// ---------------------------------------------------------------------------------------
// the beat
// ---------------------------------------------------------------------------------------

test("a beat APPENDS one JSON line with a one-line detail, and never writes over what the caller keeps in the file", () => {
  const dir = tmp();
  try {
    const file = join(dir, "log.jsonl");
    writeFileSync(file, '{"detail":"the caller wrote this"}\n');
    const c = clock();
    const hb = createLeaseHeartbeat({ env: { [PROGRESS_FILE_ENV]: file }, now: c.now });
    assert.equal(hb.beat("ComfyUI is running\r\nprompt p1"), true);
    const rows = lines(file);
    assert.equal(rows.length, 2);
    assert.equal(rows[0].detail, "the caller wrote this");
    assert.equal(rows[1].detail, "ComfyUI is running prompt p1", "a detail is one line: the status call prints the last line");
    assert.equal(rows[1].at, c.now());
    assert.equal(rows[1].pid, process.pid);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("beats are throttled to BEAT_EVERY_MS; force overrides it; the very first is due at once", () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    const hb = createLeaseHeartbeat({ env: { [PROGRESS_FILE_ENV]: file }, now: c.now });
    assert.equal(hb.due(), true);
    assert.equal(hb.beat("one"), true);
    assert.equal(hb.due(), false);
    c.jump(BEAT_EVERY_MS - 1);
    assert.equal(hb.beat("too soon"), false);
    assert.equal(hb.beat("forced", { force: true }), true);
    c.jump(BEAT_EVERY_MS);
    assert.equal(hb.due(), true);
    assert.equal(hb.beat("later"), true);
    assert.deepEqual(lines(file).map((r) => r.detail), ["one", "forced", "later"]);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("with no progress file every call is a no-op, and a failing append never throws", () => {
  const none = createLeaseHeartbeat({ env: {} });
  assert.equal(none.file, "");
  assert.equal(none.due(), false);
  assert.equal(none.beat("x", { force: true }), false);
  const dir = tmp();
  try {
    const hb = createLeaseHeartbeat({
      env: { [PROGRESS_FILE_ENV]: join(dir, "p.jsonl") },
      append: () => { throw Object.assign(new Error("EACCES"), { code: "EACCES" }); },
    });
    assert.equal(hb.beat("x"), false, "a heartbeat that cannot be written costs the lease its evidence, never the render");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("leaseBeat is the process-wide heartbeat, read from the environment on first use", () => {
  const dir = tmp();
  const saved = process.env[PROGRESS_FILE_ENV];
  try {
    const file = join(dir, "p.jsonl");
    process.env[PROGRESS_FILE_ENV] = file;
    resetLeaseBeatForTest();
    assert.equal(leaseBeat("first", { force: true }), true);
    assert.equal(leaseBeat("second", { force: true }), true);
    assert.deepEqual(lines(file).map((r) => r.detail), ["first", "second"]);
    delete process.env[PROGRESS_FILE_ENV];
    resetLeaseBeatForTest();
    assert.equal(leaseBeat("none", { force: true }), false);
  } finally {
    if (saved === undefined) delete process.env[PROGRESS_FILE_ENV]; else process.env[PROGRESS_FILE_ENV] = saved;
    resetLeaseBeatForTest();
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------------------
// the poll loop beats on evidence the render is alive, and only then
// ---------------------------------------------------------------------------------------

// A scripted ComfyUI: /history/<id> answers `history()`, /queue answers `queue()`, anything else {}.
function comfyDouble({ history, queue }) {
  const urls = [];
  const fetchImpl = async (url) => {
    urls.push(url);
    if (url.includes("/history/")) return history(urls.filter((u) => u.includes("/history/")).length);
    if (url.endsWith("/queue")) return queue(urls.filter((u) => u.endsWith("/queue")).length);
    return json({});
  };
  return { fetchImpl, urls };
}

const doneAfter = (n, id = "p1") => (polls) => json(polls < n ? {} : { [id]: { outputs: { 9: { images: [{ filename: "a.png" }] } }, status: { status_str: "success" } } });
const running = (id = "p1") => () => json({ queue_running: [[1, id, {}, {}, []]], queue_pending: [] });

test("pollOutputs: a listed running prompt beats at poll 1 and then once per BEAT_EVERY_MS of wait, with a detail naming the prompt", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    const { fetchImpl } = comfyDouble({ history: doneAfter(40), queue: running("p1") });
    await pollOutputs({
      api: "http://x", promptId: "p1", waitSec: 600, isDone: (e) => !!e.outputs?.[9]?.images?.[0],
      fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file },
    });
    const rows = lines(file);
    // 39 unfinished polls cover 78 virtual seconds: beats at 2 s, 32 s and 62 s (each >= 30 s after the last).
    assert.equal(rows.length, 3, JSON.stringify(rows));
    assert.match(rows[0].detail, /^ComfyUI is running prompt p1 \(2s into the wait\)$/);
    assert.deepEqual(rows.map((r) => r.at - rows[0].at), [0, 30_000, 60_000]);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: a pending prompt is listed too (it is waiting its turn on the server, not lost)", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    const { fetchImpl } = comfyDouble({ history: doneAfter(3), queue: () => json({ queue_running: [[1, "other", {}, {}, []]], queue_pending: [[2, "p1", {}, {}, []]] }) });
    await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs?.[9]?.images?.[0], fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file } });
    assert.equal(lines(file).length, 1);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: a prompt the server no longer lists anywhere (a restarted ComfyUI lost it) never beats, so the lease reads stalled", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    // The queue is not empty: it runs somebody else's prompt. Only THIS prompt's presence vouches for this render.
    const { fetchImpl, urls } = comfyDouble({ history: () => json({}), queue: () => json({ queue_running: [[1, "someone-else", {}, {}, []]], queue_pending: [[2, "p10", {}, {}, []]] }) });
    await assert.rejects(pollOutputs({ api: "http://x", promptId: "p1", waitSec: 120, isDone: () => false, fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file }, noOutputMsg: "gone" }), /gone/);
    assert.equal(existsSync(file), false);
    assert.ok(urls.some((u) => u.endsWith("/queue")), "the queue WAS asked");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

// The shape the poll loop's own comment names (C-83): a poisoned server can answer /history with an HTTP
// error for the rest of the budget. That answer says the server is UP, not that the prompt is alive, so it
// is no evidence on its own: only the prompt's presence in /queue vouches for it.
const historyHttp500 = () => ({ ok: false, status: 500, json: async () => ({}), text: async () => "internal error" });

test("pollOutputs: a /history that answers HTTP 500 and a /queue that does not list the prompt never beats (the server is up, the render is not vouched for)", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    const { fetchImpl, urls } = comfyDouble({ history: historyHttp500, queue: () => json({ queue_running: [[1, "someone-else", {}, {}, []]], queue_pending: [] }) });
    await assert.rejects(pollOutputs({ api: "http://x", promptId: "p1", waitSec: 120, isDone: () => false, fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file }, noOutputMsg: "gone" }), /gone/);
    assert.equal(existsSync(file), false, "an HTTP error from /history is not a sign that this prompt is alive");
    assert.ok(urls.some((u) => u.endsWith("/queue")), "the queue WAS asked");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: a /history that answers HTTP 500 while /queue lists the prompt running does beat (the render is alive, the history route is what is ill)", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    const done = doneAfter(1);
    let n = 0;
    const { fetchImpl } = comfyDouble({ history: () => (++n <= 4 ? historyHttp500() : done(1)), queue: running("p1") });
    await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 120, isDone: (e) => !!e.outputs?.[9]?.images?.[0], fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file } });
    const rows = lines(file);
    assert.equal(rows.length, 1);
    assert.match(rows[0].detail, /^ComfyUI is running prompt p1/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: a /queue that errors, hangs up or answers nonsense is no evidence; a later good answer beats", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    // The throttle is claimed only by a WRITTEN beat, so every poll asks again until one is vouched for.
    const answers = [
      () => ({ ok: false, status: 500, json: async () => ({}), text: async () => "" }),
      () => { throw new Error("socket hang up"); },
      () => json("not an object"),
      () => json({ queue_running: "nope" }),
      running("p1"),
    ];
    const { fetchImpl } = comfyDouble({ history: doneAfter(8), queue: (n) => answers[Math.min(n - 1, answers.length - 1)]() });
    await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 120, isDone: (e) => !!e.outputs?.[9]?.images?.[0], fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file } });
    assert.equal(lines(file).length, 1, "the first four answers vouched for nothing, the fifth did");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: a prompt ComfyUI lists in /history (finishing) beats without asking /queue", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    let n = 0;
    const { fetchImpl, urls } = comfyDouble({
      history: () => json(++n < 2 ? { p1: { status: { status_str: "running" } } } : { p1: { outputs: { 9: { images: [{ filename: "a.png" }] } }, status: { status_str: "success" } } }),
      queue: () => { throw new Error("must not be asked"); },
    });
    await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs?.[9]?.images?.[0], fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file } });
    assert.match(lines(file)[0].detail, /^ComfyUI is finishing prompt p1/);
    assert.equal(urls.filter((u) => u.endsWith("/queue")).length, 0);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: time the server does not answer (dead-server watchdog territory) never beats, even though the loop is alive", async () => {
  const dir = tmp();
  try {
    const file = join(dir, "p.jsonl");
    const c = clock();
    const fetchImpl = async () => { throw new Error("ECONNREFUSED"); };
    await assert.rejects(pollOutputs({ api: "http://x", promptId: "p1", waitSec: 600, isDone: () => false, fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: file, COMFY_DEAD_SEC: "30" } }), /stopped answering/);
    assert.equal(existsSync(file), false);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("pollOutputs: with no progress file declared it asks /queue for nothing and writes nothing", async () => {
  const c = clock();
  const { fetchImpl, urls } = comfyDouble({ history: doneAfter(5), queue: () => { throw new Error("must not be asked"); } });
  await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs?.[9]?.images?.[0], fetchImpl, sleep: c.sleep, now: c.now, env: {} });
  assert.equal(urls.filter((u) => u.endsWith("/queue")).length, 0);
});

test("pollOutputs: an unwritable progress file does not fail the render", async () => {
  const dir = tmp();
  try {
    const c = clock();
    const { fetchImpl } = comfyDouble({ history: doneAfter(4), queue: running("p1") });
    // A directory where the file should be: appendFileSync throws EISDIR on every platform.
    const h = await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs?.[9]?.images?.[0], fetchImpl, sleep: c.sleep, now: c.now, env: { [PROGRESS_FILE_ENV]: dir } });
    assert.ok(h.outputs);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

// ---------------------------------------------------------------------------------------
// the other places a helper tells the lease where it is
// ---------------------------------------------------------------------------------------

test("withGpuSlot tells the lease's progress file once the slot is ready, before the work runs", async () => {
  const events = [];
  const result = await withGpuSlot({
    noLock: true, comfyManaged: false, lease: null, freeLlamaSwap: async () => {},
    beat: (detail, opts) => events.push(["beat", detail, opts]),
  }, async () => { events.push(["fn"]); return "ok"; });
  assert.equal(result, "ok");
  assert.equal(events[0][0], "beat");
  assert.match(events[0][1], /GPU work starting/);
  assert.deepEqual(events[0][2], { force: true });
  assert.deepEqual(events[1], ["fn"]);
});

test("runBatchJobs tells its progress callback before and after every job, and a callback that throws cannot fail the batch", async () => {
  const told = [];
  const jobs = [{ out: "a.png", prompt: "a" }, { out: "b.png", prompt: "b" }];
  const summary = await runBatchJobs({
    jobs,
    runJob: async (job) => { if (job.out === "b.png") throw new Error("refused"); },
    record: () => {},
    progress: (line) => { told.push(line); if (line.includes("starting")) throw new Error("a broken heartbeat"); },
  });
  assert.deepEqual(summary, { total: 2, ok: 1, failed: 1 });
  assert.deepEqual(told, ["batch job 1/2 starting", "batch job 1/2 done", "batch job 2/2 starting", "batch job 2/2 failed"]);
});

test("satisfyModels tells its beat which model it is on, before ComfyUI exists to poll (run-graph with a manifest)", async () => {
  const told = [];
  const deps = {
    comfyDir: "C:/ComfyUI", beat: (line) => told.push(line),
    exists: (p) => p.endsWith("have.safetensors"), readSentinel: () => ({ oid: "h", size: 0, mtime: 0 }), writeSentinel: () => {},
    download: async () => {}, sha256: async () => "h",
  };
  const r = await satisfyModels([
    { path: "models/checkpoints/have.safetensors", sha256: "h" },
    { path: "models/checkpoints/need.safetensors", source_url: "https://huggingface.co/x/y", sha256: "h" },
  ], deps);
  assert.equal(r.ok, true);
  assert.deepEqual(told, [
    "manifest: checking models/checkpoints/have.safetensors",
    "manifest: checking models/checkpoints/need.safetensors",
    "manifest: reading the publisher's digest for models/checkpoints/need.safetensors",
    "manifest: downloading models/checkpoints/need.safetensors",
    "manifest: downloaded models/checkpoints/need.safetensors",
  ]);
  // No beat given: unchanged behaviour.
  const bare = await satisfyModels([{ path: "models/checkpoints/need.safetensors", source_url: "https://huggingface.co/x/y", sha256: "h" }], { ...deps, beat: undefined, exists: () => false });
  assert.equal(bare.ok, true);
});

test("comfy-run-graph hands the satisfier a beat that writes to the lease's progress file", () => {
  const src = readFileSync(join(HERE, "comfy-run-graph.mjs"), "utf8");
  assert.match(src, /beat: \(line\) => leaseBeat\(line, \{ force: true \}\)/);
});

// ---------------------------------------------------------------------------------------
// end to end: the real runners against a fake ComfyUI
// ---------------------------------------------------------------------------------------

// The fake lists each prompt as RUNNING for its first history poll (what a real ComfyUI does while the
// sampler works) and finishes it on the next one.
async function fakeComfy() {
  const seen = { prompts: 0, queueAsked: 0 };
  const polls = new Map();
  const server = createServer((req, res) => {
    const url = new URL(req.url, "http://fake");
    const send = (obj) => { res.setHeader("content-type", "application/json"); res.end(JSON.stringify(obj)); };
    if (req.method === "GET" && url.pathname === "/system_stats") return send({ system: { argv: ["main.py"] } });
    if (req.method === "POST" && url.pathname === "/prompt") {
      req.on("data", () => {});
      req.on("end", () => { seen.prompts++; send({ prompt_id: "p" + seen.prompts, number: seen.prompts, node_errors: {} }); });
      return;
    }
    if (url.pathname === "/queue") {
      seen.queueAsked++;
      return send({ queue_running: [...polls.keys()].filter((id) => polls.get(id) === 1).map((id) => [1, id, {}, {}, []]), queue_pending: [] });
    }
    if (url.pathname.startsWith("/history/")) {
      const id = url.pathname.slice("/history/".length);
      const n = (polls.get(id) || 0) + 1;
      polls.set(id, n);
      return send(n < 2 ? {} : { [id]: { status: { status_str: "success", completed: true, messages: [] }, outputs: { 9: { images: [{ filename: "r.png", subfolder: "", type: "output" }] } } } });
    }
    if (url.pathname === "/view") { res.setHeader("content-type", "image/png"); res.end(PNG); return; }
    if (req.method === "POST" && url.pathname === "/free") return send({});
    res.statusCode = 404; res.end("{}");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return { url: `http://127.0.0.1:${server.address().port}`, seen, close: () => new Promise((r) => server.close(r)) };
}

function scrubbedEnv(comfyDir, extra) {
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
  return { ...env, ...extra };
}

function run(script, args, env) {
  return new Promise((resolve, reject) => {
    const c = spawn(process.execPath, [script, ...args], { env, stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
    let stdout = "", stderr = "";
    c.stdout.on("data", (d) => { stdout += d; });
    c.stderr.on("data", (d) => { stderr += d; });
    const timer = setTimeout(() => { c.kill(); reject(new Error("runner did not finish in 120 s\n" + stderr)); }, 120_000);
    c.on("close", (code) => { clearTimeout(timer); resolve({ code, stdout, stderr }); });
    c.on("error", (e) => { clearTimeout(timer); reject(e); });
  });
}

test("end to end: the real comfy-generate --batch and the comfy-render child it spawns move the lease's progress file", async (t) => {
  if (existsSync(REPO_CLI)) { t.skip("a locally built comfyui-pp-cli would take the CLI submission path instead of the raw one this test drives"); return; }
  const fake = await fakeComfy();
  const dir = tmp();
  try {
    const comfyDir = join(dir, "comfy");
    mkdirSync(join(comfyDir, "input"), { recursive: true });
    const outDir = join(dir, "renders");
    mkdirSync(outDir);
    const progress = join(dir, "progress", "wrap.jsonl");
    mkdirSync(dirname(progress));
    const jobsPath = join(dir, "jobs.jsonl");
    writeFileSync(jobsPath, ["a", "b"].map((n, i) => JSON.stringify({ prompt: "a " + n, out: join(outDir, n + ".png"), seed: 100 + i })).join("\n") + "\n");
    const r = await run(GENERATE, ["--batch", jobsPath, "--results", join(dir, "results.jsonl"), "--api", fake.url, "--no-lock"], scrubbedEnv(comfyDir, { [PROGRESS_FILE_ENV]: progress }));
    assert.equal(r.code, 0, r.stderr);
    const rows = lines(progress);
    const details = rows.map((x) => x.detail);
    assert.ok(details.some((d) => /^batch job 1\/2 starting$/.test(d)), details.join(" | "));
    assert.ok(details.some((d) => /^batch job 2\/2 done$/.test(d)), details.join(" | "));
    assert.ok(details.some((d) => /^ComfyUI is running prompt p1 /.test(d)), "the comfy-render child beat from its poll loop: " + details.join(" | "));
    assert.ok(details.some((d) => /^ComfyUI is running prompt p2 /.test(d)), "and so did the second job's child");
    assert.ok(statSync(join(outDir, "b.png")).size > 0);
    // The same run with no progress file declared asks the fake's /queue for nothing.
    const before = fake.seen.queueAsked;
    const r2 = await run(GENERATE, [join(outDir, "c.png"), "a c", "--api", fake.url, "--no-lock"], scrubbedEnv(comfyDir, {}));
    assert.equal(r2.code, 0, r2.stderr);
    assert.equal(fake.seen.queueAsked, before, "no declared progress file, no /queue request");
  } finally {
    await fake.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
