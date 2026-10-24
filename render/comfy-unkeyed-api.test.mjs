// node --test render/comfy-unkeyed-api.test.mjs
//
// An UNKEYED runner given a non-default --api (plan section 7, finding 7 of the P6 rollout).
//
// The unkeyed instance is launched with no --port of its own, so a launch can start only the
// endpoint ComfyUI defaults to (8188). A blog batch ran `comfy-generate --batch --api
// http://127.0.0.1:8189` with no COMFY_INSTANCE / COMFY_CARD_UUID: resolveInstance gave key "",
// withGpuSlot called ensureComfy with no api, and ensureComfy started an unkeyed ComfyUI on 8188
// INSIDE the runner's card lease while every job was submitted to 8189. The stray held a CUDA
// context on the lease's card and made any later card-bound default binding refuse with
// COMFY-PROFILE-MISMATCH.
//
// The rule now: an unkeyed runner whose endpoint is not the default one talks to THAT endpoint. If
// it answers, it is reused; if it does not, the run fails with the reason and nothing is launched
// (8188 least of all). Nothing here touches a real port: every probe, spawn and bind check is
// injected, and 8188-8191 are never contacted.
import { test } from "node:test";
import assert from "node:assert/strict";
import { ensureComfy } from "./comfy-lifecycle.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { instanceDeps } from "./comfy-run-graph.mjs";
import { scratchDir } from "./test-scratch.mjs";

const boom = (what) => () => { throw new Error("must not be reached: " + what); };
const OTHER = "http://127.0.0.1:8189";
// What ensureComfy is told about the endpoint; keep/idleExit/report are the keep decision's (ADR 0097), not the endpoint's.
const endpointOpts = (o) => { const { keep, idleExit, report, ...rest } = o || {}; return rest; };

// ---- withGpuSlot hands the endpoint down ---------------------------------------------------

const slot = (api, instanceEnv = {}) => {
  const seen = { ensure: null, free: null };
  return withGpuSlot({
    noLock: true, lease: null, api, instanceEnv, comfyManaged: true,
    ensureComfy: async (o) => { seen.ensure = o; return null; },
    freeComfy: async (...a) => { seen.free = a; },
    freeLlamaSwap: async () => {},
  }, async () => {}).then(() => seen);
};

test("withGpuSlot: an unkeyed runner given another endpoint passes it to ensureComfy", async () => {
  const seen = await slot(OTHER);
  assert.deepEqual(endpointOpts(seen.ensure), { api: OTHER }, "ensureComfy must be told which endpoint this run drives, or it ensures 8188");
  assert.deepEqual(seen.free, [OTHER], "and the post-run free goes to the same endpoint");
});

test("withGpuSlot: the default endpoint, however it is spelled, is ensured exactly as before", async () => {
  for (const api of [undefined, "http://127.0.0.1:8188", "http://localhost:8188"]) {
    const seen = await slot(api);
    assert.deepEqual(endpointOpts(seen.ensure), {}, `api ${api}: ensureComfy gets no api for the default endpoint`);
  }
});

test("withGpuSlot: a keyed instance is unchanged", async () => {
  const seen = await slot(OTHER, { COMFY_INSTANCE: "side" });
  assert.deepEqual(endpointOpts(seen.ensure), { api: OTHER });
});

// ---- ensureComfy never launches 8188 for another endpoint ------------------------------------

function harness(t, { up = false, env = {} } = {}) {
  const calls = { up: [], spawn: [], marker: [] };
  const opts = {
    api: OTHER, env, comfyDir: scratchDir(t, "comfy-unkeyed-"), py: "py", pollMs: 1, maxPolls: 3, log: () => {},
    envFor: () => ({}),
    comfyUp: async (a) => { calls.up.push(a); return up; },
    portFree: boom("an unkeyed launch makes no bind check"),
    mkdirs: boom("an unkeyed launch makes no directories"),
    spawn: (...a) => { calls.spawn.push(a); return { pid: 5, kill() {}, once() {} }; },
    writeLaunch: (...a) => { calls.marker.push(a); },
    systemArgv: async () => null,
  };
  return { calls, opts };
}

test("ensureComfy: another endpoint that answers is reused, and only that endpoint is asked", async (t) => {
  const { calls, opts } = harness(t, { up: true });
  assert.equal(await ensureComfy(opts), null);
  assert.deepEqual(calls.up, [OTHER], "the probe goes to the endpoint the run drives, never to 8188");
  assert.equal(calls.spawn.length, 0);
});

test("ensureComfy: another endpoint that does not answer is a named failure, and nothing is launched", async (t) => {
  const { calls, opts } = harness(t, { up: false });
  await assert.rejects(ensureComfy(opts), (e) => {
    assert.match(e.message, /COMFY-ENDPOINT-DOWN/);
    assert.match(e.message, new RegExp(OTHER.replace(/\./g, "\\.")), "it names the endpoint");
    assert.match(e.message, /COMFY_INSTANCE/, "and the way to have the runner launch it");
    return true;
  });
  assert.equal(calls.spawn.length, 0, "a launch with no --port would start ComfyUI on 8188 while the run submits to the other endpoint");
  assert.equal(calls.marker.length, 0);
  assert.deepEqual(calls.up, [OTHER]);
});

test("ensureComfy: extra args that carry the endpoint's own --port still launch (the operator named the port)", async (t) => {
  const { calls, opts } = harness(t, { up: false, env: { COMFY_EXTRA_ARGS: "--port 8189 --listen 127.0.0.1" } });
  let up = false;
  opts.comfyUp = async (a) => { calls.up.push(a); return up; };
  opts.spawn = (...a) => { calls.spawn.push(a); up = true; return { pid: 5, kill() {}, once() {} }; };
  const child = await ensureComfy(opts);
  assert.ok(child, "the instance the operator pointed at its own port is launched");
  assert.equal(calls.spawn.length, 1);
  const argv = calls.spawn[0][1];
  assert.deepEqual(argv.slice(argv.indexOf("--port")), ["--port", "8189", "--listen", "127.0.0.1"]);
});

test("ensureComfy: extra args that name ANOTHER port do not excuse the launch", async (t) => {
  const { calls, opts } = harness(t, { up: false, env: { COMFY_EXTRA_ARGS: "--port 9999" } });
  await assert.rejects(ensureComfy(opts), /COMFY-ENDPOINT-DOWN/);
  assert.equal(calls.spawn.length, 0);
});

test("ensureComfy: the default endpoint launches exactly as before", async (t) => {
  const { calls, opts } = harness(t, { up: false });
  opts.api = "http://127.0.0.1:8188";
  let up = false;
  opts.comfyUp = async (a) => { calls.up.push(a); return up; };
  opts.spawn = (...a) => { calls.spawn.push(a); up = true; return { pid: 5, kill() {}, once() {} }; };
  await ensureComfy(opts);
  assert.equal(calls.spawn.length, 1);
  assert.equal(calls.spawn[0][1].includes("--port"), false, "the default instance still has no --port");
});

// ---- run-graph has the same wiring -----------------------------------------------------------

test("instanceDeps: an unkeyed run-graph on another endpoint starts and frees THAT endpoint", async () => {
  const ensureCalls = []; const freeCalls = [];
  const real = { ensureComfy: async (...a) => { ensureCalls.push(a); return "child"; }, freeComfy: async (...a) => { freeCalls.push(a); } };
  const deps = instanceDeps(OTHER, {}, real);
  await deps.ensureComfy({ comfyDir: "/c", reserveVram: "2" });
  await deps.freeComfy();
  assert.deepEqual(ensureCalls, [[{ comfyDir: "/c", reserveVram: "2", api: OTHER }]]);
  assert.deepEqual(freeCalls, [[OTHER]], "freeing the default endpoint would drop the models of a different ComfyUI");
});

// ---- a replacement that cannot be launched is not started ------------------------------------

test("ensureComfy: an orphan on another endpoint that needs replacing is refused BEFORE it is stopped when the relaunch cannot land there", async (t) => {
  // harness-launched (the marker's pid is alive and its spawner is gone), wrong profile: the
  // verdict is "restart". An unkeyed relaunch with no --port would start 8188, not this endpoint,
  // so stopping the orphan first would leave the operator with neither.
  const { writeLaunchOwner } = await import("./comfy-ownership.mjs");
  const oldArgv = ["main.py", "--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0", "--disable-dynamic-vram"];
  const dir = scratchDir(t, "comfy-unkeyed-restart-");
  writeLaunchOwner(dir, { pid: 4242, ownerPid: 5151, args: oldArgv, profile: { cudaDevice: "", dynamicVram: "" } });
  let killed = null; let spawned = 0;
  await assert.rejects(ensureComfy({
    api: OTHER, env: { COMFY_CUDA_DEVICE: "1" }, comfyDir: dir, py: "py", pollMs: 1, maxPolls: 2, log: () => {},
    alive: (pid) => pid === 4242, comfyUp: async () => true, systemArgv: async () => oldArgv,
    killPid: (pid) => { killed = pid; },
    spawn: () => { spawned++; return { pid: 7, kill() {} }; },
    envFor: () => ({}),
  }), /COMFY-ENDPOINT-DOWN/);
  assert.equal(killed, null, "nothing may be stopped when it cannot be relaunched where it was");
  assert.equal(spawned, 0);
});

// ---- a two-card lease without --placement multicard (register PF-4) ---------------------------
//
// A still run under `gpu reserve --devices <a>,<b>` (GPU_LEASE_DEVICES names the two cards) that did not pass `--placement multicard`
// is an UNKEYED runner: only --placement multicard keys the pair's own instance and launches it on the pair's port. Aimed at that
// port (COMFY_API) with nothing answering, it used to fail as COMFY-ENDPOINT-DOWN, whose advice (start ComfyUI yourself, or key the
// instance with COMFY_INSTANCE / COMFY_CARD_UUID, which pins ONE card) does not say what is missing. It now names the placement.

const PAIR = "GPU-00000000-0000-0000-0000-00000000000a,GPU-00000000-0000-0000-0000-00000000000b";

test("ensureComfy: an unkeyed runner under a two-card lease with no placement is told to pass --placement multicard", async (t) => {
  const { calls, opts } = harness(t, { up: false, env: { GPU_LEASE_DEVICES: PAIR } });
  await assert.rejects(ensureComfy(opts), (e) => {
    assert.match(e.message, /--placement multicard/, "the missing placement is named");
    assert.match(e.message, /COMFY-PLACEMENT-MISSING/, "under its own name, not the endpoint-down one");
    assert.doesNotMatch(e.message, /COMFY-ENDPOINT-DOWN/, "the generic line is not what a two-card lease gets");
    for (const id of PAIR.split(",")) assert.ok(e.message.includes(id), `the two leased cards are named (${id})`);
    assert.match(e.message, new RegExp(OTHER.replace(/\./g, "\.")), "and so is the endpoint nothing answered on");
    return true;
  });
  assert.equal(calls.spawn.length, 0, "nothing is launched");
  assert.equal(calls.marker.length, 0);
});

test("ensureComfy: a two-card lease whose endpoint answers still reuses it (the refusal is for the endpoint that is down)", async (t) => {
  const { calls, opts } = harness(t, { up: true, env: { GPU_LEASE_DEVICES: PAIR } });
  assert.equal(await ensureComfy(opts), null);
  assert.equal(calls.spawn.length, 0);
});

test("ensureComfy: one leased card, or three, keeps the endpoint-down line (the placement is for exactly two cards)", async (t) => {
  for (const devices of ["GPU-00000000-0000-0000-0000-00000000000a", `${PAIR},GPU-00000000-0000-0000-0000-00000000000c`, ""]) {
    const { opts } = harness(t, { up: false, env: devices ? { GPU_LEASE_DEVICES: devices } : {} });
    await assert.rejects(ensureComfy(opts), (e) => {
      assert.match(e.message, /COMFY-ENDPOINT-DOWN/, `devices ${JSON.stringify(devices)}`);
      assert.doesNotMatch(e.message, /--placement multicard/);
      return true;
    });
  }
});

test("ensureComfy: a keyed instance, or a multicard launch, under a two-card lease is not told about the placement", async (t) => {
  // keyed: the operator named an instance, so the launch is the runner's own and the failure is another one
  const keyed = harness(t, { up: false, env: { GPU_LEASE_DEVICES: PAIR, COMFY_INSTANCE: "side" } });
  keyed.opts.portFree = async () => false; // the instance's port is held by something that is not ComfyUI
  await assert.rejects(ensureComfy(keyed.opts), (e) => {
    assert.doesNotMatch(e.message, /COMFY-PLACEMENT-MISSING/);
    return true;
  });
  // multicard launch: the placement WAS asked for, and the extra launch it carries is not this refusal's business
  const launched = harness(t, { up: false, env: { GPU_LEASE_DEVICES: PAIR } });
  launched.opts.launch = { cards: ["GPU-00000000-0000-0000-0000-00000000000a", "GPU-00000000-0000-0000-0000-00000000000b"], cudaVisibleDevices: PAIR, key: "pair" };
  await assert.rejects(ensureComfy(launched.opts), (e) => {
    assert.doesNotMatch(e.message, /COMFY-PLACEMENT-MISSING/);
    return true;
  });
});
