// node --test render/igpu-lifecycle.test.mjs
// The REAL runners' lifecycle (CT-49 round 2), each pinned so that deleting its wiring fails a test:
//  - SIL4: each runner's main() installs the lifecycle - a vanished parent, SIGTERM / SIGINT / SIGHUP
//    kill the engine tree and remove the temp dirs (the old lifecycle tests drove a private harness);
//  - SIL3: the engine lives in the runner's process group, so gpugen's SIGKILL of that group takes it
//    along, and a runner that was SIGKILLed leaves a temp dir the next job sweeps;
//  - SIL12: a runner told to stop exits only AFTER its engine is gone, so the lease is not released
//    while the iGPU is still held;
//  - the process 'exit' hook removes the temp dirs.
// The engines are stub executables (render/igpu-stub.mjs). Needs ffmpeg for the animate runner only.
import { test } from "node:test";
import assert from "node:assert";
import { spawn, spawnSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { makeStub, stubAvailable } from "./igpu-stub.mjs";
import { resolveFfmpeg, resolveFfprobe } from "./audio-qa.mjs";
import { descendantsOf, pidGone, waitUntilGone, sweepStaleTempDirs, ENGINE_EXIT_WAIT_MS } from "./igpu-engine.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const isWin = process.platform === "win32";
const skipAll = stubAvailable() ? false : "cannot build a stub engine binary here (needs go on Windows)";
const realFfmpeg = resolveFfmpeg();
const hasFfmpeg = Boolean(realFfmpeg && resolveFfprobe(realFfmpeg));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const alive = (pid) => { try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; } };
async function waitFor(fn, ms, what) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    const v = fn();
    if (v) return v;
    await sleep(40);
  }
  throw new Error(`timed out waiting for ${what}`);
}
async function waitGone(pid, ms = 30000) {
  await waitFor(() => !alive(pid), ms, `pid ${pid} to exit`).catch(() => {});
  return !alive(pid);
}
const readPid = (file) => (existsSync(file) && readFileSync(file, "utf8").trim() !== "" ? Number(readFileSync(file, "utf8")) : 0);

const SD_HEADER = [
  "ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1",
  "[VERBOSE] ggml_runner.cpp:1019 - wan compute buffer size: 512.00 MB(VRAM) on Vulkan0 (peak across 1 segment)",
];

// One runner under test: where it runs, its argv, the engine's pid file and the private TEMP.
function setup(kind) {
  const root = mkdtempSync(join(tmpdir(), "igpu-life-"));
  const work = join(root, "work");
  const priv = join(root, "tmp");
  mkdirSync(work);
  mkdirSync(priv);
  const f = (n) => { const p = join(work, n); writeFileSync(p, "x"); return p; };
  // ffmpeg / ffprobe stand-ins that exist (the runners resolve them before any engine starts); the
  // animate runner really runs ffmpeg, so it gets the real one
  const fake = join(root, "fake");
  mkdirSync(fake);
  const ext = isWin ? ".exe" : "";
  copyFileSync(process.execPath, join(fake, `ffmpeg${ext}`));
  copyFileSync(process.execPath, join(fake, `ffprobe${ext}`));
  const ffmpeg = kind === "animate" ? realFfmpeg : join(fake, `ffmpeg${ext}`);
  const pidFile = join(work, "engine.pid");
  let script;
  let args;
  if (kind === "video") {
    script = "sdcpp-video.mjs";
    const bin = makeStub(work, "sd-cli", { pidFile, log: SD_HEADER, hang: true });
    args = ["--sd-bin", bin, "--model", f("m.gguf"), "--vae", f("v.safetensors"), "--t5xxl", f("t5.gguf"), "--backend", "vulkan0",
      "--frames", "5", "--width", "64", "--height", "64", "--no-lock", "--", join(work, "out", "clip.mp4"), f("still.png"), "a calm sea"];
  } else if (kind === "audio") {
    script = "audiocpp-generate.mjs";
    const bin = makeStub(work, "audiocpp_cli", { pidFile, log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], hang: true });
    args = ["--kind", "music", "--bin", bin, "--family", "ace_step", "--model", f("m.gguf"), "--backend", "vulkan", "--device", "0", "--no-lock",
      "--", join(work, "out", "m.wav"), "warm lo-fi bed"];
  } else {
    script = "sdcpp-animate.mjs";
    const drv = join(work, "driver.mp4");
    const r = spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=96x64:d=1:r=16", "-c:v", "libx264", "-pix_fmt", "yuv420p", drv]);
    assert.equal(r.status, 0, String(r.stderr));
    // the depth engine is the first (and here the hanging) engine
    const sd = makeStub(work, "sd-cli", { log: SD_HEADER });
    const depth = makeStub(work, "da3-cli", { pidFile, log: ["[da3] da::Backend using device: Vulkan0"], hang: true });
    args = ["--sd-bin", sd, "--model", f("vace.safetensors"), "--vae", f("v.safetensors"), "--t5xxl", f("t5.gguf"), "--backend", "vulkan0",
      "--depth-bin", depth, "--depth-model", f("d.gguf"), "--frames", "5", "--width", "64", "--height", "64", "--no-lock",
      "--", join(work, "out", "a.mp4"), f("ref.png"), drv, "a knight"];
  }
  const env = { ...process.env, TEMP: priv, TMP: priv, TMPDIR: priv, FFMPEG_PATH: ffmpeg, LLAMA_SWAP_API: "http://127.0.0.1:9", IGPU_PARENT_POLL_MS: "250" };
  for (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE") || k === "GGML_VK_VISIBLE_DEVICES") delete env[k];
  return { root, work, priv, pidFile, script: join(here, script), args, env, done: () => rmSync(root, { recursive: true, force: true }) };
}

const tempDirs = (sb) => readdirSync(sb.priv).filter((n) => !n.startsWith("."));
const KINDS = ["video", "audio", "animate"];
const kindSkip = (kind) => skipAll || (kind === "animate" && !hasFfmpeg && "the animate runner extracts real frames and needs ffmpeg + ffprobe");

// ---------------------------------------------------------------- SIL4: the parent goes away

for (const kind of KINDS) {
  test(`${kind} runner: when its parent is SIGKILLed the runner and its engine die and the temp dirs go (installLifecycle is wired into main)`, { skip: kindSkip(kind), timeout: 120000 }, async () => {
    const sb = setup(kind);
    let runnerPid = 0;
    let parent;
    try {
      const parentJs = join(sb.work, "parent.mjs");
      writeFileSync(parentJs, `import {spawn} from "node:child_process"; import {writeFileSync} from "node:fs";
const [info, ...cmd] = process.argv.slice(2);
const c = spawn(cmd[0], cmd.slice(1), {stdio: "ignore", env: process.env, detached: true}); // detached: on Windows a child of a killed parent shares its fate otherwise
writeFileSync(info, String(c.pid));
setInterval(() => {}, 1000);`);
      const info = join(sb.work, "runner.pid");
      parent = spawn(process.execPath, [parentJs, info, process.execPath, sb.script, ...sb.args], { env: sb.env, stdio: "ignore" });
      runnerPid = await waitFor(() => readPid(info), 30000, "the runner to start");
      const enginePid = await waitFor(() => readPid(sb.pidFile), 60000, "the engine to start (the host may be too loaded)");
      assert.ok(tempDirs(sb).length > 0, "the runner made its temp dir");
      parent.kill("SIGKILL");
      assert.ok(await waitGone(runnerPid), "the orphaned runner must notice and exit");
      assert.ok(await waitGone(enginePid, 20000), "its engine must die with it");
      assert.deepEqual(tempDirs(sb), [], "and the temp dirs must be removed");
    } finally {
      try { parent?.kill("SIGKILL"); } catch { /* gone */ }
      for (const p of [runnerPid, readPid(sb.pidFile)]) if (p && alive(p)) { try { process.kill(p, "SIGKILL"); } catch { /* gone */ } }
      sb.done();
    }
  });
}

// ---------------------------------------------------------------- SIL4 + SIL12: told to stop

for (const kind of KINDS) {
  for (const [sig, code] of [["SIGTERM", 143], ["SIGINT", 130], ["SIGHUP", 129]]) {
    test(`${kind} runner: ${sig} exits ${code} only after the engine is gone, and removes the temp dirs`, { skip: kindSkip(kind) || (isWin && "POSIX signals"), timeout: 120000 }, async () => {
      const sb = setup(kind);
      let child;
      try {
        child = spawn(process.execPath, [sb.script, ...sb.args], { env: sb.env, stdio: ["ignore", "ignore", "pipe"] });
        let stderr = "";
        child.stderr.on("data", (d) => { stderr += d; });
        const closed = new Promise((resolve) => child.on("close", (c, s) => resolve({ c, s })));
        const enginePid = await waitFor(() => readPid(sb.pidFile), 60000, "the engine to start (the host may be too loaded)");
        child.kill(sig);
        const { c, s } = await closed;
        // SIL12: the moment the runner has exited, its engine is already gone (no polling)
        assert.equal(alive(enginePid) && !pidGone(enginePid), false, `the engine must be gone when the runner has exited (${stderr.slice(-200)})`);
        assert.equal(c, code, `${sig}: exit code (signal ${s})`);
        assert.deepEqual(tempDirs(sb), [], "the temp dirs are removed");
        assert.ok(!existsSync(join(sb.work, "out", kind === "audio" ? "m.wav" : kind === "video" ? "clip.mp4" : "a.mp4")), "no result is delivered");
      } finally {
        try { child?.kill("SIGKILL"); } catch { /* gone */ }
        const p = readPid(sb.pidFile);
        if (p && alive(p)) { try { process.kill(p, "SIGKILL"); } catch { /* gone */ } }
        sb.done();
      }
    });
  }
}

// ---------------------------------------------------------------- SIL3: the process group

test("video runner: the engine is in the runner's process group; SIGKILL of that group (gpugen's escalation) takes it along, and the next job sweeps the leaked temp dir", { skip: skipAll || (process.platform !== "linux" && "reads /proc and needs POSIX groups"), timeout: 120000 }, async () => {
  const sb = setup("video");
  let runner;
  try {
    runner = spawn(process.execPath, [sb.script, ...sb.args], { env: sb.env, stdio: "ignore", detached: true }); // detached = its own group, as gpugen's Setpgid makes it
    const enginePid = await waitFor(() => readPid(sb.pidFile), 60000, "the engine to start (the host may be too loaded)");
    const pgrp = (pid) => Number(readFileSync(`/proc/${pid}/stat`, "utf8").split(")")[1].trim().split(" ")[2]);
    assert.equal(pgrp(enginePid), runner.pid, "the engine must be in the RUNNER's group (a detached engine would survive the group kill)");
    const leaked = tempDirs(sb);
    assert.ok(leaked.length === 1, "the runner made one temp dir");
    process.kill(-runner.pid, "SIGKILL");
    assert.ok(await waitGone(enginePid, 20000), "the engine must die with the runner's group");
    assert.deepEqual(tempDirs(sb), leaked, "a SIGKILLed runner cannot clean up: its temp dir is still there ...");
    // ... and the next job's makeTempDir sweeps it
    const mod = pathToFileURL(join(here, "igpu-engine.mjs")).href;
    const r = spawnSync(process.execPath, ["--input-type=module", "-e", `import {makeTempDir} from ${JSON.stringify(mod)}; const t = makeTempDir("sdcpp-video-"); t.cleanup();`], { env: sb.env, encoding: "utf8" });
    assert.equal(r.status, 0, r.stderr);
    assert.deepEqual(tempDirs(sb), [], "swept by the next job");
  } finally {
    try { process.kill(-runner.pid, "SIGKILL"); } catch { /* gone */ }
    sb.done();
  }
});

test("sweepStaleTempDirs: removes the dir of a dead owner, keeps a live owner's, an unmarked dir and other prefixes", () => {
  const base = mkdtempSync(join(tmpdir(), "igpu-sweep-"));
  try {
    const mk = (name, owner) => {
      const d = join(base, name);
      mkdirSync(d);
      if (owner !== undefined) writeFileSync(join(d, ".igpu-owner"), String(owner));
      writeFileSync(join(d, "frame.png"), "x");
      return d;
    };
    const gone = spawnSync(process.execPath, ["-e", "0"]); // a pid that is certainly dead afterwards
    assert.equal(gone.status, 0);
    const dead = mk("sdcpp-video-dead", gone.pid ?? 2147483646);
    const live = mk("sdcpp-video-live", process.ppid > 1 ? process.ppid : process.pid);
    const self = mk("sdcpp-video-self", process.pid);
    const unmarked = mk("sdcpp-video-unmarked");
    const other = mk("other-prefix-dead", gone.pid ?? 2147483646);
    const junk = mk("sdcpp-video-junk", "not-a-pid");
    const removed = sweepStaleTempDirs("sdcpp-video-", base);
    assert.deepEqual(removed, [dead]);
    for (const d of [live, self, unmarked, other, junk]) assert.ok(existsSync(d), d);
    assert.ok(!existsSync(dead));
  } finally { rmSync(base, { recursive: true, force: true }); }
});

// ---------------------------------------------------------------- SIL12 internals

test("installLifecycle: told to stop it asks for the pids, kills, WAITS for them, cleans up, and only then exits", { skip: skipAll || (isWin && "POSIX signals are not delivered by process.emit on Windows the same way") }, () => {
  const mod = pathToFileURL(join(here, "igpu-engine.mjs")).href;
  const code = `
    import { installLifecycle } from ${JSON.stringify(mod)};
    const log = [];
    installLifecycle({ pollMs: 100000, deps: {
      pids: () => { log.push("pids"); return [111, 222]; },
      kill: () => log.push("kill"),
      wait: (pids) => { log.push("wait:" + pids.join(",")); return { gone: true, waitedMs: 7, alive: [] }; },
      cleanup: () => log.push("cleanup"),
      exit: (c) => { log.push("exit:" + c); console.log(JSON.stringify(log)); process.exitCode = 0; },
    } });
    process.emit("SIGTERM");
    process.emit("SIGINT"); // a second signal while dying changes nothing
    setTimeout(() => {}, 50);
  `;
  const r = spawnSync(process.execPath, ["--input-type=module", "-e", code], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(JSON.parse(r.stdout.trim()), ["pids", "kill", "wait:111,222", "cleanup", "exit:143"]);
});

test("installLifecycle: an engine that is still there after the wait is reported, not hidden, and the runner still exits", { skip: skipAll || (isWin && "see above") }, () => {
  const mod = pathToFileURL(join(here, "igpu-engine.mjs")).href;
  const code = `
    import { installLifecycle } from ${JSON.stringify(mod)};
    installLifecycle({ pollMs: 100000, deps: {
      pids: () => [4242], kill: () => {}, wait: () => ({ gone: false, waitedMs: 3000, alive: [4242] }),
      cleanup: () => {}, exit: (c) => { console.log("exit:" + c); process.exitCode = 0; },
    } });
    process.emit("SIGHUP");
  `;
  const r = spawnSync(process.execPath, ["--input-type=module", "-e", code], { encoding: "utf8" });
  assert.equal(r.stdout.trim(), "exit:129");
  assert.match(r.stderr, /engine process\(es\) 4242 still present after 3000 ms/);
});

test("waitUntilGone: waits for a process that goes away late, gives up at the bound, and never sleeps for one that is already gone", () => {
  let t = 0;
  const base = { now: () => t, sleep: (ms) => { t += ms; }, pollMs: 25 };
  // gone after 700 ms
  const late = waitUntilGone([1, 2], { ...base, maxMs: 3000, isGone: (p) => (p === 1 ? true : t >= 700) });
  assert.equal(late.gone, true);
  assert.ok(late.waitedMs >= 700 && late.waitedMs < 800, `waited ${late.waitedMs}`);
  // never gone: stops at the bound and says who is left
  t = 0;
  const stuck = waitUntilGone([7], { ...base, maxMs: 1000, isGone: () => false });
  assert.deepEqual([stuck.gone, stuck.alive], [false, [7]]);
  assert.ok(stuck.waitedMs >= 1000 && stuck.waitedMs < 1100);
  // already gone: no sleep at all
  t = 0;
  assert.equal(waitUntilGone([5], { ...base, isGone: () => true }).waitedMs, 0);
  assert.equal(waitUntilGone([], { ...base }).gone, true);
  assert.equal(ENGINE_EXIT_WAIT_MS, 3000);
  assert.ok(ENGINE_EXIT_WAIT_MS < 5000, "shorter than gpugen's SIGTERM grace");
});

test("descendantsOf: every descendant, deepest first; unrelated processes are not included", () => {
  const table = new Map([[10, 1], [11, 10], [12, 10], [13, 11], [14, 13], [20, 1], [21, 20]]);
  const d = descendantsOf(10, table);
  assert.deepEqual([...d].sort((a, b) => a - b), [11, 12, 13, 14]);
  assert.ok(d.indexOf(14) < d.indexOf(13) && d.indexOf(13) < d.indexOf(11), "a child is listed before its parent");
  assert.deepEqual(descendantsOf(99, table), []);
});

test("pidGone: a zombie (a killed child not yet reaped) counts as gone, a live process does not", { skip: process.platform !== "linux" && "reads /proc" }, () => {
  const c = spawn(process.execPath, ["-e", "setInterval(()=>{},1000)"], { stdio: "ignore" });
  const pid = c.pid;
  assert.equal(pidGone(pid), false);
  process.kill(pid, "SIGKILL");
  // the event loop is blocked here, so the child cannot be reaped: it stays a zombie
  const end = Date.now() + 5000;
  let z = false;
  while (Date.now() < end && !z) {
    z = /^[ZX]/.test(readFileSync(`/proc/${pid}/stat`, "utf8").split(")")[1].trim());
  }
  assert.ok(z, "the killed child became a zombie");
  assert.equal(alive(pid), true, "kill -0 still answers for a zombie");
  assert.equal(pidGone(pid), true);
});

// ---------------------------------------------------------------- the exit hook

test("the process 'exit' hook removes the temp dirs of a runner that exits (or throws) without cleaning up", () => {
  const mod = pathToFileURL(join(here, "igpu-engine.mjs")).href;
  for (const how of ["process.exit(0)", "throw new Error('boom')"]) {
    const base = mkdtempSync(join(tmpdir(), "igpu-exit-"));
    try {
      const code = `import {makeTempDir} from ${JSON.stringify(mod)}; const t = makeTempDir("exit-hook-"); console.log(t.dir); ${how};`;
      const r = spawnSync(process.execPath, ["--input-type=module", "-e", code], { env: { ...process.env, TEMP: base, TMP: base, TMPDIR: base }, encoding: "utf8" });
      const dir = r.stdout.trim().split("\n")[0];
      assert.ok(dir.startsWith(base), `the temp dir is under the private TEMP: ${dir}`);
      assert.ok(!existsSync(dir), `${how}: the temp dir must be removed by the exit hook`);
      assert.deepEqual(readdirSync(base), []);
    } finally { rmSync(base, { recursive: true, force: true }); }
  }
});
