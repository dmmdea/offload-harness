// node --test render/comfy-keep.test.mjs
//
// Instance lifecycle for --keep-comfy (plan P13b, finding 3 of the P6 rollout). A runner that
// launches ComfyUI itself and is told to keep it used to never exit: ensureComfy spawned the
// instance as a non-detached child with PIPED stdout/stderr, so the runner's event loop stayed
// attached to a process that was meant to outlive it, and the first thing the runner's parent
// saw was a hang until its per-shot timeout killed the tree and deleted the finished clip.
//
// A kept instance is now spawned detached with its console going to its own log FILE, and
// unref'd: it neither holds the runner's event loop nor owns a pipe that a runner's exit
// would close under it. The instance lives no longer than its LEASE; the holder of that
// lease stops it on release (internal/comfyinst, called by the lease holders). The non-kept
// path is untouched (comfy-instance.test.mjs pins its spawn line).
//
// The first group uses REAL processes: a stub ComfyUI (a node script standing in for
// main.py, on an ephemeral loopback port: never 8188-8191) and a real runner. A fake
// {kill(){}} child cannot prove anything about an event loop.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync, readFileSync, existsSync, statSync, rmSync, mkdirSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { EventEmitter } from "node:events";
import { ensureComfy, comfyLogPath, rotateComfyLog, tailComfyLog, trimRotatedLog } from "./comfy-lifecycle.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";

const HERE = fileURLToPath(new URL(".", import.meta.url));
const url = (f) => pathToFileURL(join(HERE, f)).href;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A stub ComfyUI: answers /system_stats with the argv it was given (what harnessLaunched
// compares) and /free, and keeps WRITING to stdout so that a pipe closed under it would kill it.
const STUB_MAIN = `
const http = require("node:http");
const args = process.argv.slice(2);
const port = Number(args[args.indexOf("--port") + 1]);
const srv = http.createServer((req, res) => {
  if (req.url === "/system_stats") { res.setHeader("content-type", "application/json"); res.end(JSON.stringify({ system: { argv: ["main.py", ...args] } })); return; }
  res.end("{}");
});
srv.listen(port, "127.0.0.1", () => console.log("stub-comfy listening " + port));
process.stdout.on("error", () => process.exit(7)); // a broken pipe is a crash for a real ComfyUI too
setInterval(() => console.log("stub-comfy tick " + Date.now()), 100);
setTimeout(() => process.exit(0), 90000); // a leaked stub removes itself
`;

function freePort() {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once("error", reject);
    s.listen({ port: 0, host: "127.0.0.1" }, () => { const { port } = s.address(); s.close(() => resolve(port)); });
  });
}

const alive = (pid) => { try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; } };
function killTree(pid) {
  if (process.platform === "win32") spawnSync("taskkill", ["/T", "/F", "/PID", String(pid)]);
  else { try { process.kill(pid, "SIGKILL"); } catch {} }
}

async function setup() {
  const comfyDir = mkdtempSync(join(tmpdir(), "comfy-keep-"));
  writeFileSync(join(comfyDir, "main.py"), STUB_MAIN);
  const port = await freePort();
  assert.ok(port < 8188 || port > 8191, "the stub must never sit on 8188-8191");
  const runner = join(comfyDir, "runner.mjs");
  writeFileSync(runner, `
import { withGpuSlot } from ${JSON.stringify(url("gpu-lock.mjs"))};
import { ensureComfy } from ${JSON.stringify(url("comfy-lifecycle.mjs"))};
const out = await withGpuSlot({
  noLock: true, keepComfy: true, comfyManaged: true, api: process.env.TEST_API,
  ensureComfy: (o) => ensureComfy({ ...o, pollMs: 100 }),
}, async () => "job-done");
console.log("RUNNER-RESULT " + out);
`);
  return { comfyDir, port, api: `http://127.0.0.1:${port}`, runner };
}

test("a runner given keep-comfy exits after its job while the instance it launched stays up", async (t) => {
  const { comfyDir, port, api, runner } = await setup();
  const markerPath = join(comfyDir, ".offload-launch-keepcase.json");
  let stubPid = 0;
  let child;
  t.after(() => { if (stubPid) killTree(stubPid); if (child && child.exitCode === null) killTree(child.pid); try { rmSync(comfyDir, { recursive: true, force: true }); } catch {} });

  child = spawn(process.execPath, [runner], {
    env: { ...process.env, COMFY_DIR: comfyDir, COMFY_PY: process.execPath, COMFY_INSTANCE: "keepcase", TEST_API: api, COMFY_START_WAIT_SEC: "30" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "", stderr = "";
  child.stdout.on("data", (d) => (stdout += d));
  child.stderr.on("data", (d) => (stderr += d));
  const exited = new Promise((resolve) => child.once("exit", (code) => resolve(code)));
  const t0 = Date.now();
  let giveUp;
  const code = await Promise.race([exited, new Promise((r) => { giveUp = setTimeout(() => r("timeout"), 15000); })]);
  clearTimeout(giveUp);
  if (existsSync(markerPath)) stubPid = JSON.parse(readFileSync(markerPath, "utf8")).pid;
  assert.notEqual(code, "timeout", `the runner never exited: a kept instance is holding its event loop (stdout: ${stdout} stderr: ${stderr})`);
  assert.equal(code, 0, `runner failed: ${stderr}`);
  assert.match(stdout, /RUNNER-RESULT job-done/);
  assert.ok(Date.now() - t0 < 14000, "the runner took as long as its own timeout");

  // The instance outlives the runner: alive, answering, and its console went to a FILE.
  assert.ok(stubPid > 0, "the launch marker must name the instance");
  assert.ok(alive(stubPid), "the kept instance died with its runner");
  const r = await fetch(api + "/system_stats", { signal: AbortSignal.timeout(5000) });
  assert.ok(r.ok, "the kept instance stopped answering");
  const log = comfyLogPath(comfyDir, "keepcase");
  assert.ok(existsSync(log), "the instance's console log is missing");
  assert.match(readFileSync(log, "utf8"), /stub-comfy listening/);

  // And it keeps writing after the runner is gone: with a pipe this is the write that breaks.
  const before = statSync(log).size;
  await sleep(600);
  assert.ok(alive(stubPid), "the kept instance died after its runner exited (a closed pipe?)");
  assert.ok(statSync(log).size > before, "the kept instance stopped writing its console");
});

// ---- the spawn contract, with an injected spawn --------------------------------------

function fakeKeptChild() {
  const c = new EventEmitter();
  c.pid = 4242;
  c.unrefCalls = 0;
  c.unref = () => { c.unrefCalls++; };
  c.kill = () => {};
  return c;
}

test("a kept launch spawns detached, console to a log file descriptor, unref'd, window hidden", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-spawn-"));
  let ups = 0;
  let seen;
  const fds = { opened: [], closed: [] };
  const child = fakeKeptChild();
  await ensureComfy({
    keep: true, comfyDir: dir, env: { COMFY_INSTANCE: "kc", COMFY_API: "http://127.0.0.1:45999" }, api: "http://127.0.0.1:45999",
    comfyUp: async () => ups++ > 0,
    spawn: (py, argv, opts) => { seen = { py, argv, opts }; return child; },
    envFor: () => process.env,
    openLogFd: (path) => { fds.opened.push(path); return 77; },
    closeFd: (fd) => fds.closed.push(fd),
    portFree: async () => true,
    mkdirs: () => {},
    writeLaunch: () => {},
    pollMs: 1,
  });
  assert.equal(seen.opts.detached, true);
  assert.equal(seen.opts.windowsHide, true, "a detached console process opens its own window on Windows unless hidden");
  assert.deepEqual(seen.opts.stdio, ["ignore", 77, 77], "stdout and stderr go to the log file, never to a pipe");
  assert.equal(child.unrefCalls, 1, "the child must be unref'd or it holds the runner's event loop");
  assert.deepEqual(fds.opened, [comfyLogPath(dir, "kc")]);
  assert.deepEqual(fds.closed, [77], "the runner's own copy of the descriptor must be closed once the child has its dup");
});

test("a non-kept launch is unchanged: piped, attached, not unref'd", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-plain-"));
  let ups = 0;
  let seen;
  const child = fakeKeptChild();
  await ensureComfy({
    comfyDir: dir, env: {}, comfyUp: async () => ups++ > 0,
    spawn: (py, argv, opts) => { seen = opts; return child; },
    envFor: () => process.env, writeLaunch: () => {}, pollMs: 1,
  });
  assert.deepEqual(seen.stdio, ["ignore", "pipe", "pipe"]);
  assert.equal(seen.detached, false);
  assert.equal(seen.windowsHide, undefined);
  assert.equal(child.unrefCalls, 0);
});

test("a kept instance that never answers is still killed: a half-started instance is nobody's", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-dead-"));
  let killed = 0;
  const child = fakeKeptChild();
  child.kill = () => { killed++; };
  await assert.rejects(
    ensureComfy({
      keep: true, comfyDir: dir, env: {}, comfyUp: async () => false, spawn: () => child, envFor: () => process.env,
      openLogFd: () => 5, closeFd: () => {}, writeLaunch: () => {}, pollMs: 1, maxPolls: 3,
    }),
    /did not become ready/,
  );
  assert.equal(killed, 1);
});

// ---- the log: bounded by rotation, not by a pipe ------------------------------------

test("trimRotatedLog keeps the last 5 MB of an oversized archived console log", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-trim-"));
  const cap = 5 * 1024 * 1024;
  const head = Buffer.alloc(cap, "a"), tail = Buffer.from("THE-END-OF-THE-RUN\n");
  writeFileSync(comfyLogPath(dir), Buffer.concat([head, Buffer.alloc(1024 * 1024, "b"), tail]));
  rotateComfyLog(dir);
  trimRotatedLog(dir);
  const archived = comfyLogPath(dir) + ".1";
  assert.ok(existsSync(archived));
  assert.ok(statSync(archived).size <= cap, `archive is ${statSync(archived).size} bytes, over the 5 MB cap`);
  assert.match(readFileSync(archived, "utf8").slice(-20), /THE-END-OF-THE-RUN/);
});

test("trimRotatedLog leaves a small archive alone and never throws on a missing one", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-trim2-"));
  trimRotatedLog(dir); // nothing to trim
  writeFileSync(comfyLogPath(dir), "small\n");
  rotateComfyLog(dir);
  trimRotatedLog(dir);
  assert.equal(readFileSync(comfyLogPath(dir) + ".1", "utf8"), "small\n");
});

test("tailComfyLog reads a bounded window of a huge live log, not the whole file", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-tail-"));
  const line = "x".repeat(99) + "\n";
  writeFileSync(comfyLogPath(dir, "k"), line.repeat(60000) + "last-line\n"); // ~6 MB of ordinary lines
  const tail = tailComfyLog(dir, 3, "k");
  assert.equal(tail.split("\n").length, 3);
  assert.ok(tail.endsWith("last-line"));
  // One 300 KB line then the last line: a whole-file read would return both; the bounded window
  // starts inside the long line, so the cut line is dropped and only the whole last line remains.
  writeFileSync(comfyLogPath(dir, "j"), "y".repeat(300 * 1024) + "\nlast-line\n");
  assert.equal(tailComfyLog(dir, 2, "j"), "last-line");
});

test("tailComfyLog on a small log is what it always was", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-keep-tail2-"));
  writeFileSync(comfyLogPath(dir), "a\nb\nc\nd\n");
  assert.equal(tailComfyLog(dir, 2), "c\nd");
  assert.equal(tailComfyLog(dir, 20), "a\nb\nc\nd");
  assert.equal(tailComfyLog(join(dir, "missing"), 2), "");
});

test("withGpuSlot hands keepComfy to ensureComfy as keep, and only then", async () => {
  const seen = [];
  const run = (keepComfy) => withGpuSlot({
    noLock: true, keepComfy, comfyManaged: true,
    ensureComfy: async (o) => { seen.push(o); return null; },
    freeComfy: async () => {}, freeLlamaSwap: async () => {},
  }, async () => "x");
  await run(true);
  await run(false);
  assert.equal(seen[0].keep, true);
  assert.equal("keep" in seen[1], false, "a runner that tears its own ComfyUI down must call ensureComfy exactly as before");
});
