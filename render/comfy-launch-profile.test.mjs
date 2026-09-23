// node --test render/comfy-launch-profile.test.mjs
// The per-binding ComfyUI launch profile (COMFY_CUDA_DEVICE / COMFY_DYNAMIC_VRAM) and
// the guard that refuses to REUSE a running ComfyUI whose launch argv contradicts it.
// ComfyUI is stood in for by a real HTTP server answering /system_stats.
import { test } from "node:test";
import assert from "node:assert";
import http from "node:http";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  ensureComfy, launchFlags, resolveLaunchProfile, profileMismatch, argvDynamicVram,
  reuseVerdict, fetchSystemArgv, comfyUp,
} from "./comfy-lifecycle.mjs";
import { writeLaunchOwner, readLaunchOwner } from "./comfy-ownership.mjs";

const NO_PROFILE = { cudaDevice: "", dynamicVram: "" };
const BOUND_DIR = "/fake/comfyui";

test("launchFlags: no profile = the byte-identical launch, extra args last", () => {
  assert.deepEqual(launchFlags({ reserveVram: "1.0", extraArgs: "--disable-dynamic-vram --x 1", profile: NO_PROFILE }),
    ["--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0", "--disable-dynamic-vram", "--x", "1"]);
});

test("launchFlags: the profile's --cuda-device wins over any device flag in the extra args", () => {
  const f = launchFlags({ extraArgs: "--cuda-device 0 --default-device=2 --verbose", profile: { cudaDevice: "1", dynamicVram: "" } });
  assert.deepEqual(f, ["--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0", "--cuda-device", "1", "--verbose"]);
  assert.ok(!f.some((x) => x.startsWith("--default-device")), "never --default-device: it re-maps what the pool keys mean");
});

test("launchFlags: dynamic VRAM on strips the disabler, off adds it exactly once", () => {
  const on = launchFlags({ extraArgs: "--disable-dynamic-vram", profile: { cudaDevice: "", dynamicVram: "on" } });
  assert.ok(!on.includes("--disable-dynamic-vram"));
  const off = launchFlags({ extraArgs: "", profile: { cudaDevice: "", dynamicVram: "off" } });
  assert.equal(off.filter((x) => x === "--disable-dynamic-vram").length, 1);
  const off2 = launchFlags({ extraArgs: "--disable-dynamic-vram", profile: { cudaDevice: "", dynamicVram: "off" } });
  assert.equal(off2.filter((x) => x === "--disable-dynamic-vram").length, 1, "not duplicated");
  const warned = [];
  launchFlags({ extraArgs: "--highvram", profile: { cudaDevice: "", dynamicVram: "on" }, warn: (l) => warned.push(l) });
  assert.match(warned.join("\n"), /COMFY-PROFILE-WARN: .*--highvram/);
});

test("resolveLaunchProfile: device indices and on/off only", () => {
  assert.deepEqual(resolveLaunchProfile({}), NO_PROFILE);
  assert.deepEqual(resolveLaunchProfile({ COMFY_CUDA_DEVICE: " 1, 2 ", COMFY_DYNAMIC_VRAM: "ON" }), { cudaDevice: "1,2", dynamicVram: "on" });
  assert.throws(() => resolveLaunchProfile({ COMFY_CUDA_DEVICE: "cuda:1" }), /COMFY_CUDA_DEVICE must be/);
  assert.throws(() => resolveLaunchProfile({ COMFY_CUDA_DEVICE: "all" }), /COMFY_CUDA_DEVICE must be/);
  assert.throws(() => resolveLaunchProfile({ COMFY_DYNAMIC_VRAM: "auto" }), /COMFY_DYNAMIC_VRAM must be/);
});

test("profileMismatch: device and dynamic VRAM read the way argparse and cli_args do", () => {
  const p1 = { cudaDevice: "1", dynamicVram: "" };
  assert.equal(profileMismatch(["main.py", "--cuda-device", "1"], p1), "");
  assert.equal(profileMismatch(["main.py", "--cuda-device=1"], p1), "");
  assert.match(profileMismatch(["main.py", "--cuda-device", "1", "--cuda-device", "0"], p1), /--cuda-device 0/, "argparse keeps the LAST value");
  assert.match(profileMismatch(["main.py"], p1), /no --cuda-device/);
  assert.match(profileMismatch(null, p1), /does not report its launch argv/, "unprovable is a mismatch");
  assert.equal(profileMismatch(null, NO_PROFILE), "", "no profile = nothing to prove");
  const on = { cudaDevice: "", dynamicVram: "on" };
  const off = { cudaDevice: "", dynamicVram: "off" };
  assert.equal(profileMismatch(["main.py"], on), "", "dynamic VRAM is ComfyUI's default");
  for (const dis of ["--disable-dynamic-vram", "--highvram", "--gpu-only", "--novram", "--cpu"]) {
    assert.match(profileMismatch(["main.py", dis], on), /dynamic VRAM off/, dis);
    assert.equal(profileMismatch(["main.py", dis], off), "", dis);
  }
  assert.equal(argvDynamicVram(["main.py", "--highvram", "--enable-dynamic-vram"]), true, "--enable-dynamic-vram overrides");
  assert.match(profileMismatch(["main.py"], off), /dynamic VRAM on/);
});

// A real HTTP server standing in for ComfyUI's /system_stats.
async function stubComfy(argv) {
  const state = { argv, statsHits: 0 };
  const srv = http.createServer((req, res) => {
    if (req.url === "/system_stats") {
      state.statsHits++;
      res.setHeader("content-type", "application/json");
      const system = { os: "nt", comfyui_version: "0.37.0" };
      if (state.argv !== undefined) system.argv = state.argv;
      res.end(JSON.stringify({ system, devices: [] }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  state.api = `http://127.0.0.1:${srv.address().port}`;
  state.close = () => new Promise((r) => srv.close(r));
  return state;
}

const noSpawn = () => { throw new Error("must not spawn"); };
const envSame = () => process.env;

test("reuse: no profile requested => a running ComfyUI is reused without reading its argv", async () => {
  const s = await stubComfy(["main.py"]);
  try {
    const child = await ensureComfy({ api: s.api, env: {}, spawn: noSpawn, envFor: envSame, comfyDir: BOUND_DIR });
    assert.equal(child, null);
    assert.equal(s.statsHits, 1, "comfyUp's own probe only: no profile, no argv read");
  } finally { await s.close(); }
});

test("COMFY-PROFILE-MISMATCH: a foreign ComfyUI without the binding's --cuda-device is refused, never reused", async () => {
  const s = await stubComfy(["main.py", "--disable-dynamic-vram"]);
  const logs = [];
  try {
    await assert.rejects(
      ensureComfy({ api: s.api, env: { COMFY_CUDA_DEVICE: "1" }, spawn: noSpawn, envFor: envSame,
        comfyDir: mkdtempSync(join(tmpdir(), "comfy-own-")), log: (l) => logs.push(l) }),
      /^Error: COMFY-PROFILE-MISMATCH: ComfyUI on http:\/\/127\.0\.0\.1:\d+: it runs with no --cuda-device/);
    assert.ok(logs.some((l) => l.startsWith("COMFY-PROFILE-MISMATCH: ")), "greppable line on stderr: " + logs.join(" | "));
    assert.match(logs.join("\n"), /not started by this harness/);
  } finally { await s.close(); }
});

test("COMFY-PROFILE-MISMATCH: dynamic VRAM on requested, the running instance disables it", async () => {
  const s = await stubComfy(["main.py", "--cuda-device", "1", "--disable-dynamic-vram"]);
  try {
    await assert.rejects(
      ensureComfy({ api: s.api, env: { COMFY_CUDA_DEVICE: "1", COMFY_DYNAMIC_VRAM: "on" }, spawn: noSpawn,
        envFor: envSame, comfyDir: BOUND_DIR, log: () => {} }),
      /COMFY-PROFILE-MISMATCH: .*dynamic VRAM off; this binding needs it on/);
  } finally { await s.close(); }
});

test("reuse: a running ComfyUI whose argv honours the profile is reused", async () => {
  const s = await stubComfy(["main.py", "--disable-smart-memory", "--cuda-device", "1"]);
  try {
    const child = await ensureComfy({ api: s.api, env: { COMFY_CUDA_DEVICE: "1", COMFY_DYNAMIC_VRAM: "on" },
      spawn: noSpawn, envFor: envSame, comfyDir: BOUND_DIR });
    assert.equal(child, null);
  } finally { await s.close(); }
});

test("an unreadable argv with a profile requested is refused (it cannot be proven)", async () => {
  const s = await stubComfy(undefined); // a server that omits system.argv
  try {
    assert.equal(await fetchSystemArgv(s.api), null);
    await assert.rejects(
      ensureComfy({ api: s.api, env: { COMFY_CUDA_DEVICE: "1" }, spawn: noSpawn, envFor: envSame, comfyDir: BOUND_DIR, log: () => {} }),
      /COMFY-PROFILE-MISMATCH: .*does not report its launch argv/);
  } finally { await s.close(); }
});

test("harness-launched, orphaned, wrong profile => stopped and relaunched with the binding's flags", async () => {
  const oldArgv = ["main.py", "--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0", "--disable-dynamic-vram"];
  const s = await stubComfy(oldArgv);
  const dir = mkdtempSync(join(tmpdir(), "comfy-own-"));
  // pid 4242 is "alive" (faked), its spawner 5151 is gone.
  writeLaunchOwner(dir, { pid: 4242, ownerPid: 5151, args: oldArgv, profile: NO_PROFILE });
  let killed = null; let spawned = null; let closed = false;
  try {
    const child = await ensureComfy({
      api: s.api, env: { COMFY_CUDA_DEVICE: "1", COMFY_DYNAMIC_VRAM: "on", COMFY_EXTRA_ARGS: "--disable-dynamic-vram" },
      comfyDir: dir, alive: (pid) => pid === 4242, pollMs: 1, log: () => {},
      comfyUp: async (api) => (closed ? spawned !== null : comfyUp(api)),
      killPid: (pid) => { killed = pid; s.close(); closed = true; },
      spawn: (py, args, o) => { spawned = { args, env: o.env }; return { pid: 7777, kill() {} }; },
      envFor: () => { throw new Error("a --cuda-device launch must not restore multi-GPU visibility"); },
    });
    assert.equal(killed, 4242, "the orphan is stopped by its recorded pid");
    assert.ok(child && child.pid === 7777);
    const i = spawned.args.indexOf("--cuda-device");
    assert.equal(spawned.args[i + 1], "1");
    assert.ok(!spawned.args.includes("--disable-dynamic-vram"), "profile on stripped the user-env disabler");
    assert.ok(!spawned.args.includes("--disable-pinned-memory"), "one visible card keeps pinned memory");
    const rec = readLaunchOwner(dir);
    assert.equal(rec.pid, 7777);
    assert.equal(spawned.args[0], "main.py");
    assert.deepEqual(rec.args, spawned.args, "the new launch is fingerprinted with the exact argv ComfyUI will report");
  } finally { if (!closed) await s.close(); }
});

test("harness-launched but still held by a live spawner => refused, not killed", async () => {
  const argv = ["main.py", "--disable-smart-memory"];
  const s = await stubComfy(argv);
  const dir = mkdtempSync(join(tmpdir(), "comfy-own-"));
  writeLaunchOwner(dir, { pid: 4242, ownerPid: 5151, args: argv, profile: NO_PROFILE });
  try {
    await assert.rejects(
      ensureComfy({ api: s.api, env: { COMFY_CUDA_DEVICE: "1" }, comfyDir: dir, alive: () => true, log: () => {},
        killPid: () => { throw new Error("must not kill an instance another harness process holds"); },
        spawn: noSpawn, envFor: envSame }),
      /COMFY-PROFILE-MISMATCH: .*still in use by process 5151/);
  } finally { await s.close(); }
});

test("a stale marker (argv differs from what the server reports) never makes a foreign ComfyUI look ours", async () => {
  const s = await stubComfy(["main.py", "--listen"]);
  const dir = mkdtempSync(join(tmpdir(), "comfy-own-"));
  writeLaunchOwner(dir, { pid: 4242, ownerPid: 5151, args: ["main.py", "--disable-smart-memory"], profile: NO_PROFILE });
  try {
    const v = await reuseVerdict({ api: s.api, comfyDir: dir, profile: { cudaDevice: "1", dynamicVram: "" }, alive: () => true });
    assert.equal(v.reuse, false);
    assert.ok(!v.restart);
    assert.match(v.reason, /not started by this harness/);
  } finally { await s.close(); }
});

test("a fresh launch with a profile carries --cuda-device and records its fingerprint", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-own-"));
  let spawned = null;
  const child = await ensureComfy({
    comfyUp: async () => spawned !== null, env: { COMFY_CUDA_DEVICE: "2" }, comfyDir: dir, pollMs: 1,
    spawn: (py, args) => { spawned = args; return { pid: 9001, kill() {} }; },
    envFor: () => { throw new Error("must not restore multi-GPU visibility on a scoped launch"); },
  });
  assert.equal(child.pid, 9001);
  const i = spawned.indexOf("--cuda-device");
  assert.deepEqual(spawned.slice(i, i + 2), ["--cuda-device", "2"]);
  assert.equal(readLaunchOwner(dir).pid, 9001);
});
