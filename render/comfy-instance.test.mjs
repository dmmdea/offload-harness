// node --test render/comfy-instance.test.mjs
// Per-card ComfyUI instances (plan P13a, the render-layer half). Two halves:
//   1. the EMPTY instance key (the default, one ComfyUI on its default port) is pinned
//      against literals taken from the code before this change: same argv, same marker
//      file and record, same log file, same spawn env, no bind check;
//   2. a non-empty key (one per card) gets its own port, output and temp directories,
//      launch marker, log, and a card pin by GPU uuid (CUDA_VISIBLE_DEVICES, never a
//      --cuda-device index: that flag counts in CUDA's fastest-first order, not the
//      driver's, so the same number names a different card).
// Pure: spawn, port probe, directory creation, pid liveness and the HTTP probes are
// injected; the only real I/O is scratch files under the OS temp dir and one ephemeral
// loopback socket in the port-probe test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:net";
import { PassThrough } from "node:stream";
import { EventEmitter } from "node:events";
import { mkdtempSync, readFileSync, writeFileSync, existsSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { withGpuSlot } from "./gpu-lock.mjs";
import * as RG from "./comfy-run-graph.mjs";
import * as L from "./comfy-lifecycle.mjs";
import * as O from "./comfy-ownership.mjs";

// Synthetic ids: the keyless leak-gate shape rule treats a repeated-nibble head as a placeholder.
const U0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const K0 = "gaaaa1111";
const BASE_FLAGS = ["--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0"];
const scratch = () => mkdtempSync(join(tmpdir(), "comfy-inst-"));
const boom = (what) => () => { throw new Error(what); };

// ---- 1. the empty key is today's launch, byte for byte --------------------------------

test("empty key: launchFlags is today's argv, with or without an instance argument", () => {
  const want = [...BASE_FLAGS, "--x", "1"];
  assert.deepEqual(L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1" }), want);
  assert.deepEqual(L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1", instance: null, comfyDir: "/c" }), want);
  assert.deepEqual(L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1", instance: L.resolveInstance({ env: {} }), comfyDir: "/c" }), want,
    "the resolved default instance adds nothing");
  assert.deepEqual(L.launchFlags({ warm: true, profile: { cudaDevice: "2", dynamicVram: "" } }),
    ["--disable-smart-memory", "--reserve-vram", "1.0", "--cuda-device", "2"], "the legacy pin path is unchanged");
});

test("empty key: marker, log and rotation names are today's", () => {
  const dir = scratch();
  assert.equal(L.comfyLogPath(dir), join(dir, "offload-comfyui.log"));
  assert.equal(L.comfyLogPath(dir, ""), join(dir, "offload-comfyui.log"));
  O.writeLaunchOwner(dir, { pid: 5, ownerPid: 6, args: ["main.py"], profile: { cudaDevice: "", dynamicVram: "" } });
  assert.deepEqual(readdirSync(dir), [".offload-launch.json"]);
  const rec = JSON.parse(readFileSync(join(dir, ".offload-launch.json"), "utf8"));
  assert.deepEqual(Object.keys(rec), ["startedAt", "pid", "ownerPid", "args", "profile"], "no key, port or epoch field on the default instance");
  assert.deepEqual(O.readLaunchOwner(dir), rec);
  O.clearLaunchOwner(dir);
  assert.deepEqual(readdirSync(dir), []);
});

test("empty key: ensureComfy spawns today's argv in today's env, writes today's marker, never probes the port", async () => {
  const dir = scratch();
  const sentinelEnv = { SENTINEL: "1" };
  let spawned = null; let envForCalls = 0; let up = false;
  const writes = [];
  const child = await L.ensureComfy({
    comfyUp: async () => up, comfyDir: dir, py: "py", pollMs: 1, env: { GPU_LEASE_EPOCH: "7" },
    writeLaunch: (...a) => { writes.push(a); O.writeLaunchOwner(...a); },
    envFor: () => { envForCalls++; return sentinelEnv; },
    portFree: boom("the default instance never bind-checks"),
    mkdirs: boom("the default instance never creates directories"),
    spawn: (py, args, o) => { spawned = { py, args, o }; up = true; return { pid: 4321, kill() {} }; },
  });
  assert.equal(child.pid, 4321);
  assert.deepEqual(spawned.args, ["main.py", ...BASE_FLAGS]);
  assert.equal(spawned.o.env, sentinelEnv, "the child env is whatever envFor returned: no pin added");
  assert.equal(envForCalls, 1);
  assert.equal(spawned.o.cwd, dir);
  assert.ok(existsSync(join(dir, ".offload-launch.json")));
  assert.ok(!readdirSync(dir).some((f) => /offload-launch-/.test(f)), "no keyed marker");
  const rec = O.readLaunchOwner(dir);
  assert.deepEqual(Object.keys(rec), ["startedAt", "pid", "ownerPid", "args", "profile"], "the lease epoch is recorded for keyed instances only");
  assert.deepEqual(rec.profile, { cudaDevice: "", dynamicVram: "" });
  assert.equal(writes.length, 1);
  assert.deepEqual(Object.keys(writes[0][1]), ["pid", "ownerPid", "args", "profile"], "the record handed to the marker writer has today's four fields");
  assert.equal(writes[0].length, 2, "and is written to the default marker (no key argument)");
});

test("empty key: resolveInstance keeps the api string verbatim and the unset default", () => {
  const d = L.resolveInstance({ env: {} });
  assert.equal(d.key, "");
  assert.equal(d.port, 8188);
  assert.equal(d.api, L.DEFAULT_COMFY_API);
  assert.equal(d.cardUuid, "");
  for (const api of ["http://localhost:8188", "http://some-host:8188/", "http://localhost:8190", "not a url", " http://localhost:8188 "]) {
    assert.equal(L.resolveInstance({ api, env: {} }).key, "", api);
    assert.equal(L.resolveInstance({ api, env: {} }).api, api, "verbatim: " + api);
  }
  assert.equal(L.resolveInstance({ env: { COMFY_API: "http://localhost:8188" } }).api, "http://localhost:8188");
  assert.equal(L.comfyApi(undefined, {}), L.DEFAULT_COMFY_API);
  assert.equal(L.comfyApi("http://localhost:8188", { COMFY_API: "http://localhost:9999" }), "http://localhost:8188", "a flag beats the env, as before");
});

test("withGpuSlot with no instance asks for today's calls: ensureComfy without api, freeComfy without arguments", async () => {
  let ensureOpts = null; let freeArgs = null; let tailArgs = null;
  const lease = { dir: "X", epoch: 7, class: "media" };
  await assert.rejects(withGpuSlot({
    lease, checkLease: () => true, claimUnload: () => true, freeLlamaSwap: async () => {},
    instanceEnv: {},
    ensureComfy: async (o) => { ensureOpts = o; return { kill() {} }; },
    freeComfy: async (...a) => { freeArgs = a; },
    tailLog: (...a) => { tailArgs = a; return "tail"; },
  }, async () => { throw new Error("boom"); }), /boom/);
  assert.deepEqual(ensureOpts, {});
  assert.deepEqual(freeArgs, []);
  assert.deepEqual(tailArgs, []);
});

// ---- 2. instance resolution ---------------------------------------------------------

test("resolveInstance: a port alone is not an instance; the key comes only from an explicit key or a card uuid", () => {
  const r = L.resolveInstance({ api: "http://localhost:8190", env: {} });
  assert.equal(r.key, "", "an --api on another port keeps its unkeyed behaviour (tests and scripts rely on it)");
  assert.equal(r.port, 8190);
  assert.equal(r.api, "http://localhost:8190");
  assert.equal(r.cardUuid, "");
  assert.equal(L.resolveInstance({ env: { COMFY_API: "http://localhost:8191" } }).key, "");
});

test("resolveInstance: a card uuid derives the key and, with no api, a port from the configured base", () => {
  const r = L.resolveInstance({ env: { COMFY_CARD_UUID: U0 } });
  assert.equal(r.key, K0);
  assert.equal(r.cardUuid, U0);
  assert.equal(r.port, 8189, "the first port after the default instance");
  assert.equal(new URL(r.api).port, "8189");
  const idx = L.resolveInstance({ env: { COMFY_CARD_UUID: U0, COMFY_PORT_BASE: "9000", COMFY_INSTANCE_INDEX: "2" } });
  assert.equal(idx.port, 9002);
  assert.equal(new URL(idx.api).port, "9002", "the api the runner talks to carries the same port the instance listens on");
  // an explicit api wins over the base; the key still comes from the card
  const e = L.resolveInstance({ api: "http://localhost:8192", env: { COMFY_CARD_UUID: U1 } });
  assert.deepEqual([e.key, e.port, e.api], ["gbbbb2222", 8192, "http://localhost:8192"]);
});

test("resolveInstance: an explicit key wins over the card-derived one", () => {
  const r = L.resolveInstance({ env: { COMFY_INSTANCE: "gpu-a", COMFY_CARD_UUID: U0 } });
  assert.equal(r.key, "gpu-a");
  assert.equal(r.cardUuid, U0);
  assert.equal(L.resolveInstance({ api: "http://localhost:8193", env: { COMFY_INSTANCE: "side" } }).key, "side");
});

test("resolveInstance: refuses what cannot be an instance", () => {
  assert.throws(() => L.resolveInstance({ env: { COMFY_INSTANCE: "../x" } }), /COMFY-INSTANCE-INVALID.*COMFY_INSTANCE/);
  assert.throws(() => L.resolveInstance({ env: { COMFY_INSTANCE: "a".repeat(49) } }), /COMFY-INSTANCE-INVALID/);
  assert.throws(() => L.resolveInstance({ env: { COMFY_CARD_UUID: "GPU-0" } }), /COMFY-INSTANCE-INVALID.*COMFY_CARD_UUID/);
  assert.throws(() => L.resolveInstance({ env: { COMFY_CARD_UUID: "1" } }), /COMFY-INSTANCE-INVALID/, "an index is not an identity");
  assert.throws(() => L.resolveInstance({ api: "http://localhost:8188", env: { COMFY_CARD_UUID: U0 } }), /COMFY-INSTANCE-INVALID.*8188/,
    "the default port belongs to the unkeyed instance");
  assert.throws(() => L.resolveInstance({ env: { COMFY_CARD_UUID: U0, COMFY_PORT_BASE: "80" } }), /COMFY-INSTANCE-INVALID.*COMFY_PORT_BASE/);
  assert.throws(() => L.resolveInstance({ env: { COMFY_CARD_UUID: U0, COMFY_INSTANCE_INDEX: "-1" } }), /COMFY-INSTANCE-INVALID.*COMFY_INSTANCE_INDEX/);
});

test("resolveLaunchProfile: the card uuid is part of the profile only when set, and never with an index", () => {
  assert.deepEqual(L.resolveLaunchProfile({}), { cudaDevice: "", dynamicVram: "" }, "the unbound shape is unchanged");
  assert.deepEqual(L.resolveLaunchProfile({ COMFY_CARD_UUID: U0 }), { cudaDevice: "", dynamicVram: "", cardUuid: U0 });
  assert.throws(() => L.resolveLaunchProfile({ COMFY_CARD_UUID: U0, COMFY_CUDA_DEVICE: "2" }), /COMFY-INSTANCE-CONFLICT/);
  assert.deepEqual(L.resolveLaunchProfile({ COMFY_CARD_UUID: U0, COMFY_CUDA_DEVICE: "" }), { cudaDevice: "", dynamicVram: "", cardUuid: U0 },
    "a blank index (the harness always exports one) is not a conflict");
});

test("resolveLaunchProfile: a keyed instance never takes a device index, card-bound or not", () => {
  // --cuda-device counts in CUDA's fastest-first order (index 0 is the display card on the
  // 3-card tier), so an index on a keyed instance lands on the wrong card.
  for (const env of [{ COMFY_INSTANCE: "side", COMFY_CUDA_DEVICE: "2" }, { COMFY_INSTANCE: " side ", COMFY_CUDA_DEVICE: "0,1" },
                     { COMFY_INSTANCE: "side", COMFY_CARD_UUID: U0, COMFY_CUDA_DEVICE: "1" }]) {
    assert.throws(() => L.resolveLaunchProfile(env), /COMFY-INSTANCE-CONFLICT.*COMFY_INSTANCE/, JSON.stringify(env));
  }
  assert.deepEqual(L.resolveLaunchProfile({ COMFY_INSTANCE: "side", COMFY_CUDA_DEVICE: "" }), { cudaDevice: "", dynamicVram: "" },
    "a blank index (the harness always exports one) is not a conflict");
  for (const blank of ["", "   "]) {
    assert.deepEqual(L.resolveLaunchProfile({ COMFY_INSTANCE: blank, COMFY_CUDA_DEVICE: "2" }), { cudaDevice: "2", dynamicVram: "" },
      `no key (${JSON.stringify(blank)}): the legacy index path for the default instance is untouched`);
    assert.equal(L.resolveInstance({ env: { COMFY_INSTANCE: blank } }).key, "", "and a blank key is no instance either");
  }
});

test("keyed ensureComfy: an explicit key plus a device index is refused before anything is launched", async () => {
  const h = keyedHarness();
  h.opts.env = { COMFY_INSTANCE: "side", COMFY_CUDA_DEVICE: "2" };
  h.opts.api = "http://localhost:8190";
  await assert.rejects(L.ensureComfy(h.opts), /COMFY-INSTANCE-CONFLICT/);
  assert.equal(h.spawns.length, 0);
  assert.equal(h.probes.length, 0);
});

// ---- 3. launch flags ----------------------------------------------------------------

test("launchFlags: a keyed instance gets its own port and directories, before the extra args", () => {
  const inst = { key: "p8189", port: 8189 };
  const f = L.launchFlags({ extraArgs: "--verbose", instance: inst, comfyDir: "/c" });
  assert.deepEqual(f, [...BASE_FLAGS, "--port", "8189", "--output-directory", join("/c", "instances", "p8189", "output"),
    "--temp-directory", join("/c", "instances", "p8189"), "--verbose"]);
});

test("instancePaths: a keyed instance's REAL temp dir is outside the default instance's temp tree", () => {
  // ComfyUI appends "/temp" to --temp-directory (main.py), and the default instance wipes
  // <dir>/temp on every start (cleanup_temp_filesystem), so nesting a keyed temp under it
  // deletes a running keyed instance's temp files.
  for (const key of [K0, "side", "a-b_c"]) {
    const p = L.instancePaths("/c", key);
    assert.equal(p.tempDir, join(p.tempBase, "temp"), "tempDir is what ComfyUI really uses: the flag value plus /temp");
    for (const [name, dir, defaults] of [["temp", p.tempDir, join("/c", "temp")], ["output", p.outputDir, join("/c", "output")]]) {
      assert.ok(relative(defaults, dir).startsWith(".."), `${name} dir ${dir} is inside the default instance's ${defaults}`);
      assert.ok(relative(dir, defaults).startsWith(".."), `the default ${name} dir is inside ${dir}`);
    }
    assert.ok(relative(L.instancePaths("/c", "other").tempDir, p.tempDir).startsWith(".."), "and not inside another instance's");
  }
});

test("launchFlags: extra args cannot move a keyed instance off its own port or directories", () => {
  const warned = [];
  const f = L.launchFlags({ extraArgs: "--port 1 --output-directory=/x --temp-directory /y --verbose",
    instance: { key: "p8189", port: 8189 }, comfyDir: "/c", warn: (l) => warned.push(l) });
  assert.equal(f.filter((x) => x === "--port").length, 1);
  assert.equal(f[f.indexOf("--port") + 1], "8189");
  assert.ok(!f.some((x) => x.startsWith("--output-directory=")), "the = form is stripped too");
  assert.ok(!f.includes("/x") && !f.includes("/y"));
  assert.equal(f.at(-1), "--verbose");
  assert.match(warned.join("\n"), /COMFY-INSTANCE-WARN: .*--port/);
});

test("launchFlags: a card-bound instance passes NO --cuda-device and drops one from the extra args", () => {
  const warned = [];
  const f = L.launchFlags({ extraArgs: "--cuda-device 0 --default-device=1 --verbose",
    profile: { cudaDevice: "", dynamicVram: "", cardUuid: U0 }, instance: { key: K0, port: 8189 }, comfyDir: "/c", warn: (l) => warned.push(l) });
  assert.ok(!f.some((x) => x.startsWith("--cuda-device") || x.startsWith("--default-device")), f.join(" "));
  assert.ok(f.includes("--verbose"));
  assert.match(warned.join("\n"), /COMFY-INSTANCE-WARN: .*--cuda-device/);
  const none = L.launchFlags({ profile: { cudaDevice: "", dynamicVram: "", cardUuid: U0 } });
  assert.ok(!none.includes("--cuda-device"));
  const both = L.launchFlags({ profile: { cudaDevice: "2", dynamicVram: "", cardUuid: U0 } });
  assert.ok(!both.includes("--cuda-device"), "the uuid pin outranks an index, whichever way the profile was built");
});

// ---- 4. ownership marker and log, keyed ----------------------------------------------

test("keyed launch marker: its own file, the lease epoch, and it never touches the default marker", () => {
  const dir = scratch();
  O.writeLaunchOwner(dir, { pid: 1, ownerPid: 2, args: ["main.py"], profile: {} });
  O.writeLaunchOwner(dir, { pid: 11, ownerPid: 12, args: ["main.py", "--port", "8189"], profile: { cardUuid: U0 }, key: K0, port: 8189, leaseEpoch: 42 });
  assert.deepEqual(readdirSync(dir).sort(), [".offload-launch-" + K0 + ".json", ".offload-launch.json"].sort());
  const k = O.readLaunchOwner(dir, K0);
  assert.equal(k.pid, 11);
  assert.equal(k.ownerPid, 12);
  assert.equal(k.leaseEpoch, 42);
  assert.equal(k.port, 8189);
  assert.equal(k.key, K0);
  assert.equal(O.readLaunchOwner(dir).pid, 1, "the default marker is a different file");
  assert.equal(O.readLaunchOwner(dir, "other"), null);
  O.clearLaunchOwner(dir, K0);
  assert.equal(O.readLaunchOwner(dir, K0), null);
  assert.equal(O.readLaunchOwner(dir).pid, 1, "clearing a keyed marker leaves the default one");
  O.writeLaunchOwner(dir, { pid: 13, ownerPid: 14, args: [], profile: {}, key: "p8190", port: 8190 });
  assert.ok(!("leaseEpoch" in O.readLaunchOwner(dir, "p8190")), "no epoch field without a lease");
});

test("keyed log: its own file and its own rotation; the default log is left alone", () => {
  const dir = scratch();
  assert.equal(L.comfyLogPath(dir, K0), join(dir, "offload-comfyui-" + K0 + ".log"));
  writeFileSync(L.comfyLogPath(dir), "default run");
  writeFileSync(L.comfyLogPath(dir, K0), "card run 1\nline2\nline3");
  L.rotateComfyLog(dir, K0);
  assert.ok(!existsSync(L.comfyLogPath(dir, K0)), "the keyed filename is free for the new run");
  assert.equal(readFileSync(L.comfyLogPath(dir, K0) + ".1", "utf8"), "card run 1\nline2\nline3");
  assert.equal(readFileSync(L.comfyLogPath(dir), "utf8"), "default run", "the default log is not rotated by a keyed instance");
  writeFileSync(L.comfyLogPath(dir, K0), "a\nb\nc\nd");
  assert.equal(L.tailComfyLog(dir, 2, K0), "c\nd");
  assert.equal(L.tailComfyLog(dir, 2), "default run", "the unkeyed tail is the unkeyed log");
});

// ---- 5. ensureComfy, keyed ----------------------------------------------------------

function keyedHarness({ up = async () => false, portFree = async () => true } = {}) {
  const dir = scratch();
  const made = []; const spawns = []; const probes = [];
  let started = false;
  return {
    dir, made, spawns, probes,
    opts: {
      api: "http://localhost:8189", comfyDir: dir, py: "py", pollMs: 1, log: () => {},
      env: { COMFY_CARD_UUID: U0, GPU_LEASE_EPOCH: "42" },
      envFor: boom("a card-pinned launch must not ask envFor for multi-GPU visibility"),
      comfyUp: async (a) => (started ? true : up(a)),
      portFree: async (p, host) => { probes.push([p, host]); return portFree(p, host); },
      mkdirs: (d) => made.push(d),
      killPid: boom("never kill on a bind check"),
      spawn: (py, args, o) => { started = true; spawns.push({ args, env: o.env }); return { pid: 9001, kill() {} }; },
    },
  };
}

test("keyed ensureComfy: own port and directories, uuid pin in the child env, no --cuda-device", async () => {
  const h = keyedHarness();
  const child = await L.ensureComfy(h.opts);
  assert.equal(child.pid, 9001);
  const { args, env } = h.spawns[0];
  assert.deepEqual(args, ["main.py", ...BASE_FLAGS, "--port", "8189", "--output-directory", join(h.dir, "instances", K0, "output"), "--temp-directory", join(h.dir, "instances", K0)]);
  assert.ok(!args.includes("--cuda-device"));
  assert.equal(env.CUDA_VISIBLE_DEVICES, U0, "the pin is the uuid, in the child env");
  assert.deepEqual(h.made, [join(h.dir, "instances", K0, "output"), join(h.dir, "instances", K0, "temp")],
    "the directories ComfyUI really uses (it appends /temp to --temp-directory)");
  assert.equal(h.probes.length, 1);
  assert.deepEqual(h.probes[0], [8189, "127.0.0.1"], "bound where ComfyUI listens (no --listen: loopback IPv4), not at the api's host spelling");
});

test("keyed ensureComfy: the marker is its own file and records the owner pid and the lease epoch", async () => {
  const h = keyedHarness();
  await L.ensureComfy(h.opts);
  assert.ok(existsSync(join(h.dir, ".offload-launch-" + K0 + ".json")));
  assert.ok(!existsSync(join(h.dir, ".offload-launch.json")), "the default marker is not written");
  const rec = O.readLaunchOwner(h.dir, K0);
  assert.equal(rec.pid, 9001);
  assert.equal(rec.ownerPid, process.pid);
  assert.equal(rec.leaseEpoch, 42);
  assert.equal(rec.port, 8189);
  assert.equal(rec.profile.cardUuid, U0);
  assert.deepEqual(rec.args, h.spawns[0].args, "the fingerprint is the exact argv");
});

test("keyed ensureComfy: no lease epoch in the env => none recorded; a non-number is not recorded either", async () => {
  for (const epoch of [undefined, "", "abc"]) {
    const h = keyedHarness();
    h.opts.env = { COMFY_CARD_UUID: U0, ...(epoch === undefined ? {} : { GPU_LEASE_EPOCH: epoch }) };
    await L.ensureComfy(h.opts);
    assert.ok(!("leaseEpoch" in O.readLaunchOwner(h.dir, K0)), `epoch=${JSON.stringify(epoch)}`);
  }
});

test("keyed ensureComfy: its console goes to its own log", async () => {
  const h = keyedHarness();
  h.opts.spawn = () => {
    const c = new EventEmitter(); c.pid = 9002; c.stdout = new PassThrough(); c.stderr = new PassThrough(); c.kill = () => {};
    setImmediate(() => { c.stdout.write("card console line\n"); c.emit("exit", 0); });
    return c;
  };
  let up = 0;
  h.opts.comfyUp = async () => up++ > 0;
  await L.ensureComfy(h.opts);
  await new Promise((r) => setTimeout(r, 300));
  assert.match(readFileSync(L.comfyLogPath(h.dir, K0), "utf8"), /card console line/);
  assert.ok(!existsSync(L.comfyLogPath(h.dir)), "the default log is not created by a keyed instance");
});

test("keyed ensureComfy: a port held by something that is not ComfyUI is refused, and never killed", async () => {
  const h = keyedHarness({ portFree: async () => false });
  await assert.rejects(L.ensureComfy(h.opts), /COMFY-PORT-TAKEN.*8189/);
  assert.equal(h.spawns.length, 0, "no launch onto a taken port");
  assert.equal(readdirSync(h.dir).length, 0, "and no marker or log left behind");
});

test("keyed ensureComfy: a default-port ComfyUI answering does not block the card's own instance", async () => {
  const h = keyedHarness({ up: async (a) => a.endsWith(":8188") });
  const child = await L.ensureComfy(h.opts);
  assert.equal(child.pid, 9001, "the card's instance launched on its own port");
});

test("keyed ensureComfy: an explicit key without a card gets its own port and files but does not pin", async () => {
  const h = keyedHarness();
  h.opts.env = { COMFY_INSTANCE: "side" };
  h.opts.api = "http://localhost:8190";
  h.opts.envFor = () => ({ FROM: "envFor" });
  await L.ensureComfy(h.opts);
  const { args, env } = h.spawns[0];
  assert.deepEqual(args.slice(args.indexOf("--port"), args.indexOf("--port") + 2), ["--port", "8190"]);
  assert.ok(args.includes(join(h.dir, "instances", "side", "output")));
  assert.deepEqual(env, { FROM: "envFor" });
  assert.ok(existsSync(join(h.dir, ".offload-launch-side.json")));
});

test("keyed ensureComfy: an instance that nothing pins to a card says so, and one that is pinned stays quiet", async () => {
  const warnOf = async (env) => {
    const h = keyedHarness(); const lines = [];
    h.opts.log = (l) => lines.push(l);
    h.opts.api = "http://localhost:8190";
    h.opts.env = env;
    h.opts.envFor = () => ({});
    await L.ensureComfy(h.opts);
    return lines.filter((l) => /COMFY-INSTANCE-WARN/.test(l));
  };
  const unpinned = await warnOf({ COMFY_INSTANCE: "side" });
  assert.equal(unpinned.length, 1);
  assert.match(unpinned[0], /instance 'side'.*not bound to a card.*every card/);
  assert.deepEqual(await warnOf({ COMFY_INSTANCE: "side", CUDA_VISIBLE_DEVICES: U0 }), [], "pinned by the operator's own CUDA_VISIBLE_DEVICES");
  assert.deepEqual(await warnOf({ COMFY_INSTANCE: "side", COMFY_EXTRA_ARGS: "--cuda-device 1" }), [], "pinned by the operator's own extra args");
  assert.deepEqual(await warnOf({ COMFY_INSTANCE: "side", COMFY_CARD_UUID: U0 }), [], "bound to a card by uuid");
});

test("an --api on another port with no key launches only when the operator's own extra args carry that --port: unkeyed argv, the default marker", async () => {
  const dir = scratch(); let spawned = null; let up = false;
  await L.ensureComfy({
    api: "http://localhost:8190", env: { COMFY_EXTRA_ARGS: "--port 8190" }, comfyDir: dir, py: "py", pollMs: 1, envFor: () => ({}), comfyUp: async () => up,
    portFree: boom("no bind check without a key"), mkdirs: boom("no directories without a key"),
    spawn: (py, args) => { spawned = args; up = true; return { pid: 5, kill() {} }; },
  });
  assert.deepEqual(spawned, ["main.py", ...BASE_FLAGS, "--port", "8190"]);
  assert.deepEqual(readdirSync(dir).filter((f) => f.startsWith(".offload-launch")), [".offload-launch.json"]);
});

test("an --api on another port with no key and no --port of the operator's own is refused, never launched on 8188 (plan section 7, finding 7)", async () => {
  const dir = scratch(); let spawned = 0; const asked = [];
  await assert.rejects(L.ensureComfy({
    api: "http://localhost:8190", env: {}, comfyDir: dir, py: "py", pollMs: 1, log: () => {}, envFor: () => ({}),
    comfyUp: async (a) => { asked.push(a); return false; },
    portFree: boom("no bind check without a key"), mkdirs: boom("no directories without a key"),
    spawn: () => { spawned++; return { pid: 5, kill() {} }; },
  }), /COMFY-ENDPOINT-DOWN/);
  assert.equal(spawned, 0);
  assert.deepEqual(asked, ["http://localhost:8190"]);
  assert.deepEqual(readdirSync(dir).filter((f) => f.startsWith(".offload-launch")), []);
});

test("portIsFree: false while something listens, true once it has closed", async () => {
  const srv = createServer();
  await new Promise((r) => srv.listen({ port: 0, host: "localhost" }, r));
  const port = srv.address().port;
  try {
    assert.equal(await L.portIsFree(port, "localhost"), false);
  } finally {
    await new Promise((r) => srv.close(r));
  }
  assert.equal(await L.portIsFree(port, "localhost"), true);
});

// ---- 6. reuse by uuid ---------------------------------------------------------------

// The argv a harness-launched card instance really has: its own port and output directory.
const ARGV = ["main.py", "--port", "8189", "--output-directory", join("/c", "instances", K0, "output")];
const rv = (over = {}) => L.reuseVerdict({
  api: "http://localhost:8189", comfyDir: "/c", key: K0, port: 8189, profile: { cudaDevice: "", dynamicVram: "", cardUuid: U0 },
  systemArgv: async () => ARGV, alive: (p) => p === 77, ...over,
});
const marker = (o = {}) => ({ pid: 77, ownerPid: 5151, args: ARGV, profile: { cardUuid: U0 }, key: K0, port: 8189, ...o });

test("reuse by uuid: a harness-launched instance recorded on this card is reused", async () => {
  const seen = [];
  const v = await rv({ readLaunch: (...a) => { seen.push(a); return marker(); } });
  assert.deepEqual(v, { reuse: true });
  assert.deepEqual(seen, [["/c", K0]], "the keyed marker is the one read");
});

test("reuse by uuid: the same card spelled in another case is the same card", async () => {
  let reads = 0;
  assert.deepEqual(await rv({ readLaunch: () => { reads++; return marker({ profile: { cardUuid: U0.toLowerCase() } }); } }), { reuse: true });
  assert.equal(reads, 1, "reused on the strength of the marker, not by default");
});

test("reuse by uuid: an instance recorded on another card is replaced when its spawner is gone", async () => {
  const v = await rv({ readLaunch: () => marker({ profile: { cardUuid: U1 } }), alive: (p) => p === 77 });
  assert.equal(v.restart, true);
  assert.equal(v.pid, 77);
  assert.match(v.reason, /launched on card GPU-bbbb2222.*needs card GPU-aaaa1111/);
});

test("reuse by uuid: ... and refused while another process still holds it", async () => {
  const v = await rv({ readLaunch: () => marker({ profile: { cardUuid: U1 } }), alive: () => true });
  assert.equal(v.reuse, false);
  assert.match(v.reason, /still in use by process 5151/);
});

test("reuse by uuid: an instance with no marker cannot be shown to be on the card, so it is refused (never killed)", async () => {
  const v = await rv({ readLaunch: () => null });
  assert.equal(v.reuse, false);
  assert.ok(!v.restart);
  assert.match(v.reason, /card pin.*not started by this harness/);
});

test("reuse by uuid: a marker without a pin is a different pin", async () => {
  const v = await rv({ readLaunch: () => marker({ profile: {} }), alive: (p) => p === 77 });
  assert.equal(v.restart, true);
  assert.match(v.reason, /no card pin/);
});

test("reuse by uuid: a --cuda-device in the running argv overrides the pin", async () => {
  const bad = [...ARGV, "--cuda-device", "0"];
  const own = marker({ args: bad });
  // the marker proves it is the harness's own (live pid, exact argv) and its spawner is gone: ours to replace
  const v = await rv({ systemArgv: async () => bad, readLaunch: () => own, alive: (p) => p === 77 });
  assert.equal(v.reuse, undefined);
  assert.equal(v.restart, true);
  assert.equal(v.pid, 77);
  assert.match(v.reason, /--cuda-device 0/);
  // while another process still holds it: refused, not stopped
  const held = await rv({ systemArgv: async () => bad, readLaunch: () => own, alive: () => true });
  assert.equal(held.reuse, false);
  assert.ok(!held.restart);
  assert.match(held.reason, /still in use by process 5151/);
  // and with no marker it is not provably ours: refused, never stopped
  const foreign = await rv({ systemArgv: async () => bad, readLaunch: () => null, alive: () => true });
  assert.equal(foreign.reuse, false);
  assert.ok(!foreign.restart);
  assert.match(foreign.reason, /--cuda-device 0.*not started by this harness/);
});

test("ensureComfy keyed: a wrong-card orphan is stopped and its KEYED marker cleared, then relaunched on the right card", async () => {
  const h = keyedHarness();
  let alive = true; const cleared = []; let killed = null;
  const oldArgv = ["main.py", "--port", "8189", "old"];
  Object.assign(h.opts, {
    comfyUp: async () => (alive || h.spawns.length > 0),
    systemArgv: async () => oldArgv,
    readLaunch: () => ({ pid: 77, ownerPid: 5151, args: oldArgv, profile: { cardUuid: U1 }, key: K0, port: 8189 }),
    alive: (p) => p === 77,
    killPid: (p) => { killed = p; alive = false; },
    clearLaunch: (...a) => cleared.push(a),
    writeLaunch: () => {},
  });
  const child = await L.ensureComfy(h.opts);
  assert.equal(killed, 77);
  assert.deepEqual(cleared, [[h.dir, K0]]);
  assert.equal(child.pid, 9001);
  assert.equal(h.spawns[0].env.CUDA_VISIBLE_DEVICES, U0);
});

test("ensureComfy keyed: a foreign instance on the card's port with no marker is refused and not reused", async () => {
  const h = keyedHarness();
  Object.assign(h.opts, { comfyUp: async () => true, systemArgv: async () => ARGV, readLaunch: () => null });
  await assert.rejects(L.ensureComfy(h.opts), /COMFY-PROFILE-MISMATCH.*card pin/);
  assert.equal(h.spawns.length, 0);
});

// ---- 7. withGpuSlot and the runners ---------------------------------------------------

test("withGpuSlot with a keyed instance: ensureComfy, the post-run free and the log tail all address that instance", async () => {
  let ensureOpts = null; let freeArgs = null; let tailArgs = null;
  const lease = { dir: "X", epoch: 7, class: "media" };
  await assert.rejects(withGpuSlot({
    api: "http://localhost:8189", lease, checkLease: () => true, claimUnload: () => true, freeLlamaSwap: async () => {},
    instanceEnv: { COMFY_CARD_UUID: U0 },
    ensureComfy: async (o) => { ensureOpts = o; return { kill() {} }; },
    freeComfy: async (...a) => { freeArgs = a; },
    tailLog: (...a) => { tailArgs = a; return "console tail"; },
  }, async () => { throw new Error("boom"); }), (err) => {
    assert.match(err.message, new RegExp("offload-comfyui-" + K0 + "\\.log"), "the message names the keyed log");
    assert.match(err.message, /console tail/);
    return true;
  });
  assert.equal(ensureOpts.api, "http://localhost:8189");
  assert.deepEqual(freeArgs, ["http://localhost:8189"]);
  assert.equal(tailArgs[2], K0);
});

test("withGpuSlot: an explicit api on the default port frees that api and leaves ensureComfy as before", async () => {
  let ensureOpts = null; let freeArgs = null;
  await withGpuSlot({
    api: "http://localhost:8188", lease: { dir: "X", epoch: 7, class: "media" }, checkLease: () => true, claimUnload: () => true,
    freeLlamaSwap: async () => {}, instanceEnv: {},
    ensureComfy: async (o) => { ensureOpts = o; return null; },
    freeComfy: async (...a) => { freeArgs = a; },
  }, async () => {});
  assert.deepEqual(ensureOpts, {});
  assert.deepEqual(freeArgs, ["http://localhost:8188"]);
});

test("withGpuSlot: a lane with no ComfyUI never resolves an instance, so a bad instance env cannot break the voice lane", async () => {
  await withGpuSlot({ comfyManaged: false, lease: null, noLock: true, instanceEnv: { COMFY_CARD_UUID: "garbage" } }, async () => {});
});

test("every render runner resolves its api through the instance and hands it to withGpuSlot", () => {
  const runners = ["animate", "edit", "generate", "inpaint", "music", "render", "upscale", "video", "run-graph"];
  for (const r of runners) {
    const src = readFileSync(new URL(`./comfy-${r}.mjs`, import.meta.url), "utf8");
    assert.match(src, /comfyApi\(/, `comfy-${r}.mjs reads its api through comfyApi()`);
    assert.ok(!/flags\.api \|\| process\.env\.COMFY_API/.test(src), `comfy-${r}.mjs still hard-wires the default api`);
    if (r === "run-graph") continue;
    const calls = [...src.matchAll(/withGpuSlot\(\s*\{([^}]*)\}/g)].filter((m) => /comfyManaged: true/.test(m[1]));
    assert.ok(calls.length >= 1, `comfy-${r}.mjs: no managed withGpuSlot call found`);
    for (const m of calls) assert.match(m[1], /api: API\b/, `comfy-${r}.mjs: a managed withGpuSlot call does not pass the api`);
  }
});

// ---- 8. run-graph: the keyed wiring, tested by behaviour --------------------------------

test("instanceDeps: a keyed run-graph starts and frees ITS instance; the default instance is called as before", async () => {
  const ensureCalls = []; const freeCalls = [];
  const real = { ensureComfy: async (...a) => { ensureCalls.push(a); return "child"; }, freeComfy: async (...a) => { freeCalls.push(a); } };
  const keyed = RG.instanceDeps("http://localhost:8190", { COMFY_INSTANCE: "side" }, real);
  assert.equal(await keyed.ensureComfy({ comfyDir: "/c", reserveVram: "2" }), "child");
  await keyed.freeComfy();
  assert.deepEqual(ensureCalls, [[{ comfyDir: "/c", reserveVram: "2", api: "http://localhost:8190" }]], "ensureComfy is told the instance endpoint");
  assert.deepEqual(freeCalls, [["http://localhost:8190"]], "and so is the free");
  ensureCalls.length = 0; freeCalls.length = 0;
  const plain = RG.instanceDeps("http://127.0.0.1:8188", {}, real);
  await plain.ensureComfy({ comfyDir: "/c" });
  await plain.freeComfy();
  assert.deepEqual(ensureCalls, [[{ comfyDir: "/c" }]], "no api for the default instance");
  assert.deepEqual(freeCalls, [[]], "and a free with no arguments");
  const card = RG.instanceDeps("http://localhost:8189", { COMFY_CARD_UUID: U0 }, real);
  await card.freeComfy();
  assert.deepEqual(freeCalls.at(-1), ["http://localhost:8189"], "a card-bound instance is keyed too");
  assert.throws(() => RG.instanceDeps("http://localhost:8190", { COMFY_CARD_UUID: "1" }, real), /COMFY-INSTANCE-INVALID/);
});

test("run-graph takes its ComfyUI start and free from instanceDeps, never straight from the lifecycle", () => {
  const rg = readFileSync(new URL("./comfy-run-graph.mjs", import.meta.url), "utf8");
  const body = rg.split(/\r?\n/).filter((l) => !/^import /.test(l) && !/real = \{/.test(l)).join("\n");
  assert.ok(!/\b_ensureComfy\b|\b_freeComfy\b/.test(body), "run-graph wires the unwrapped lifecycle calls again");
  assert.match(rg, /instanceDeps\(api\)/, "main resolves its deps through instanceDeps");
});

test("run-graph: a bad instance env is a typed defer in the result file and exit 0, never an untyped exit 1", () => {
  const dir = scratch(); const result = join(dir, "r.json");
  const r = spawnSync(process.execPath, [fileURLToPath(new URL("./comfy-run-graph.mjs", import.meta.url)), "--graph", join(dir, "nope.json"), "--result", result],
    { encoding: "utf8", env: { ...process.env, COMFY_CARD_UUID: "1", COMFY_INSTANCE: "" }, timeout: 60000 });
  assert.equal(r.status, 0, "exit " + r.status + ": " + r.stderr);
  const out = JSON.parse(readFileSync(result, "utf8"));
  assert.equal(out.deferred, true);
  assert.equal(out.code, "RUN_ERROR");
  assert.match(out.detail, /COMFY-INSTANCE-INVALID/);
  assert.match(r.stderr, /RUN-GRAPH DEFER/);
});

// ---- 9. ownership of a keyed instance that is not card-bound ------------------------------

const SIDE_ARGV = ["main.py", "--port", "8190", "--output-directory", join("/c", "instances", "side", "output")];
const sideProfile = { cudaDevice: "", dynamicVram: "" };
const sideMarker = (o = {}) => ({ pid: 77, ownerPid: 5151, args: SIDE_ARGV, profile: sideProfile, key: "side", port: 8190, ...o });
const sv = (over = {}) => L.reuseVerdict({
  api: "http://localhost:8190", comfyDir: "/c", key: "side", port: 8190, profile: sideProfile,
  systemArgv: async () => SIDE_ARGV, alive: (p) => p === 77, ...over,
});

test("reuse by key: an instance with a key and no card pin is reused only on the strength of its own marker", async () => {
  let argvCalls = 0;
  const v = await sv({ systemArgv: async () => { argvCalls++; return SIDE_ARGV; }, readLaunch: () => null });
  assert.equal(v.reuse, false, "whatever answers on the port is not this instance until a marker says so");
  assert.ok(!v.restart);
  assert.equal(argvCalls, 1, "the running argv is read even though no launch profile was asked for");
  assert.match(v.reason, /instance 'side'.*port 8190/);
  const seen = [];
  assert.deepEqual(await sv({ readLaunch: (...a) => { seen.push(a); return sideMarker(); } }), { reuse: true });
  assert.deepEqual(seen, [["/c", "side"]], "the keyed marker is the one read");
});

test("reuse by key: another key's or port's marker, a dead pid, a different or unreadable argv, a foreign port or directory: refused, never stopped", async () => {
  const cases = {
    "no marker": { readLaunch: () => null },
    "a marker of another key": { readLaunch: () => sideMarker({ key: "other" }) },
    "a marker with no key": { readLaunch: () => { const m = sideMarker(); delete m.key; return m; } },
    "a marker of another port": { readLaunch: () => sideMarker({ port: 8191 }) },
    "a marker with no port": { readLaunch: () => { const m = sideMarker(); delete m.port; return m; } },
    "a dead pid": { readLaunch: () => sideMarker(), alive: () => false },
    "an argv that is not the marker's": { readLaunch: () => sideMarker({ args: [...SIDE_ARGV, "--x"] }) },
    "an argv the server does not report": { systemArgv: async () => null, readLaunch: () => sideMarker() },
    "a running argv with no port of its own": { systemArgv: async () => ["main.py"], readLaunch: () => sideMarker({ args: ["main.py"] }) },
    "a running argv on another port": { systemArgv: async () => ["main.py", "--port", "8191", "--output-directory", SIDE_ARGV[4]], readLaunch: () => sideMarker({ args: ["main.py", "--port", "8191", "--output-directory", SIDE_ARGV[4]] }) },
    "a running argv on another output directory": { systemArgv: async () => [...SIDE_ARGV.slice(0, 4), "/elsewhere"], readLaunch: () => sideMarker({ args: [...SIDE_ARGV.slice(0, 4), "/elsewhere"] }) },
  };
  for (const [name, over] of Object.entries(cases)) {
    const v = await sv(over);
    assert.equal(v.reuse, false, name);
    assert.ok(!v.restart, name + ": a refused instance is never stopped");
    assert.match(v.reason, /instance 'side'/, name);
  }
});

test("ensureComfy: two explicit keys that resolve to the same port do not share one instance", async () => {
  const dir = scratch();
  const argvA = ["main.py", ...BASE_FLAGS, "--port", "8189", "--output-directory", join(dir, "instances", "a", "output"), "--temp-directory", join(dir, "instances", "a")];
  O.writeLaunchOwner(dir, { pid: 77, ownerPid: 5151, args: argvA, profile: sideProfile, key: "a", port: 8189 });
  const common = {
    comfyDir: dir, py: "py", pollMs: 1, log: () => {}, comfyUp: async () => true, systemArgv: async () => argvA, alive: (p) => p === 77,
    envFor: () => ({}), portFree: boom("an answering ComfyUI is never bind-probed"), mkdirs: boom("no directories for a reused instance"),
    spawn: boom("nothing may be launched onto a taken port"), killPid: boom("never kill"),
  };
  assert.equal(await L.ensureComfy({ ...common, env: { COMFY_INSTANCE: "a" } }), null, "key a finds its own instance and reuses it");
  await assert.rejects(L.ensureComfy({ ...common, env: { COMFY_INSTANCE: "b" } }), /COMFY-PROFILE-MISMATCH.*instance 'b'/);
});

test("ensureComfy: a foreign ComfyUI answering on an explicit key's port, with no marker at all, is refused and not reused", async () => {
  const h = keyedHarness();
  Object.assign(h.opts, { env: { COMFY_INSTANCE: "side" }, api: "http://127.0.0.1:8190", comfyUp: async () => true, systemArgv: async () => ["main.py", "--port", "8190"], readLaunch: () => null });
  await assert.rejects(L.ensureComfy(h.opts), /COMFY-PROFILE-MISMATCH.*instance 'side'/);
  assert.equal(h.spawns.length, 0);
  assert.equal(h.probes.length, 0);
});

// ---- 10. the bind check probes where ComfyUI listens ---------------------------------------

test("bindHosts: the addresses ComfyUI will listen on, from its --listen flag", () => {
  assert.deepEqual(L.bindHosts(""), ["127.0.0.1"]);
  assert.deepEqual(L.bindHosts(undefined), ["127.0.0.1"]);
  assert.deepEqual(L.bindHosts("--verbose --x 1"), ["127.0.0.1"], "ComfyUI's own default is IPv4 loopback");
  assert.deepEqual(L.bindHosts("--listen"), ["0.0.0.0", "::"], "a bare --listen is all interfaces");
  assert.deepEqual(L.bindHosts("--listen --verbose"), ["0.0.0.0", "::"]);
  assert.deepEqual(L.bindHosts("--listen 127.0.0.2"), ["127.0.0.2"]);
  assert.deepEqual(L.bindHosts("--listen=127.0.0.2,127.0.0.3"), ["127.0.0.2", "127.0.0.3"]);
  assert.deepEqual(L.bindHosts("--listen 127.0.0.2 --listen 127.0.0.9"), ["127.0.0.9"], "the last occurrence wins, as in argparse");
});

test("keyed ensureComfy: the bind check covers every address ComfyUI will listen on", async () => {
  const env = { COMFY_CARD_UUID: U0, COMFY_EXTRA_ARGS: "--listen 127.0.0.2,127.0.0.3" };
  const h = keyedHarness();
  h.opts.env = env;
  await L.ensureComfy(h.opts);
  assert.deepEqual(h.probes, [[8189, "127.0.0.2"], [8189, "127.0.0.3"]]);
  const t = keyedHarness({ portFree: async (p, host) => host !== "127.0.0.3" });
  t.opts.env = env;
  await assert.rejects(L.ensureComfy(t.opts), /COMFY-PORT-TAKEN.*127\.0\.0\.3/);
  assert.equal(t.spawns.length, 0);
});

test("keyed ensureComfy: a foreign holder of 127.0.0.1:port is detected even when the api spells the host localhost", async () => {
  const srv = createServer();
  await new Promise((r) => srv.listen({ port: 0, host: "127.0.0.1" }, r));
  const port = srv.address().port;
  try {
    let spawned = 0;
    await assert.rejects(L.ensureComfy({
      api: `http://localhost:${port}`, comfyDir: scratch(), py: "py", pollMs: 1, log: () => {}, env: { COMFY_CARD_UUID: U0 },
      envFor: boom("never reached"), comfyUp: async () => false, portFree: L.portIsFree, mkdirs: () => {},
      spawn: () => { spawned++; return { pid: 1, kill() {} }; },
    }), /COMFY-PORT-TAKEN/);
    assert.equal(spawned, 0, "ComfyUI binds 127.0.0.1, so a holder there must stop the launch");
  } finally {
    await new Promise((r) => srv.close(r));
  }
});

// ---- 11. withGpuSlot with an api on an unkeyed instance -------------------------------------

test("withGpuSlot: an unkeyed instance on another port is ensured on THAT endpoint, and the free follows it too", async () => {
  let ensureOpts = null; let freeArgs = null;
  await withGpuSlot({
    api: "http://localhost:8190", lease: { dir: "X", epoch: 7, class: "media" }, checkLease: () => true, claimUnload: () => true,
    freeLlamaSwap: async () => {}, instanceEnv: {},
    ensureComfy: async (o) => { ensureOpts = o; return null; },
    freeComfy: async (...a) => { freeArgs = a; },
  }, async () => {});
  assert.deepEqual(ensureOpts, { api: "http://localhost:8190" }, "ensureComfy is told the endpoint: an unkeyed launch has no --port, so left to itself it ensures 8188");
  assert.deepEqual(freeArgs, ["http://localhost:8190"], "the post-run free goes to the endpoint the runner talked to");
});
