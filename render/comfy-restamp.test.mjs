// node --test render/comfy-restamp.test.mjs
//
// A kept ComfyUI instance lives no longer than the lease it was launched under: its launch
// marker records the lease epoch, and the HOLDER of that lease stops the instance on release by
// matching that epoch (internal/comfyinst.StopForLease). A kept instance is detached, so it
// SURVIVES a holder that crashes or is fenced out, and the next lease on the card finds it
// running. It reuses it (a live keyed instance with a marker that proves it is ours), and the
// marker still names the DEAD lease. At its own release the new holder stops epoch N, which
// matches nothing: the instance outlives every later lease and holds its models resident (the
// 5-minute idle-unload rule). Worse, a fenced-out straggler of the old lease would still match it
// and stop the instance the new lease is using.
//
// Reusing an instance that was a lease's takes it over: the marker is re-stamped with the lease
// that reuses it. An instance that was never a lease's (a marker with no epoch: whoever kept it
// owns it) is not claimed by a lease that happens to reuse it.
//
// Pure: pid liveness, the HTTP probes and the spawn are injected, and no port is contacted; the
// markers are real files under a scratch directory.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, readdirSync, statSync, utimesSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import * as L from "./comfy-lifecycle.mjs";
import * as O from "./comfy-ownership.mjs";

const BASE_FLAGS = ["--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0"];
const profile = { cudaDevice: "", dynamicVram: "" };
const boom = (what) => () => { throw new Error(what); };
const scratch = () => mkdtempSync(join(tmpdir(), "comfy-restamp-"));
const markerFile = (dir, key) => join(dir, `.offload-launch-${key}.json`);
const readMarker = (dir, key) => JSON.parse(readFileSync(markerFile(dir, key), "utf8"));

/** A kept instance of key `side` on a port nothing here contacts, launched under `leaseEpoch`. */
function keptInstance({ leaseEpoch } = {}) {
  const dir = scratch();
  const argv = ["main.py", ...BASE_FLAGS, "--port", "8190", "--output-directory", join(dir, "instances", "side", "output"), "--temp-directory", join(dir, "instances", "side")];
  const rec = { pid: 77, ownerPid: 5151, args: argv, profile, key: "side", port: 8190 };
  if (leaseEpoch !== undefined) rec.leaseEpoch = leaseEpoch;
  O.writeLaunchOwner(dir, rec);
  return { dir, argv };
}

function reuse({ dir, argv }, env, extra = {}) {
  return L.ensureComfy({
    api: "http://127.0.0.1:8190", env: { COMFY_INSTANCE: "side", ...env }, comfyDir: dir, py: "py", pollMs: 1, log: () => {},
    comfyUp: async () => true, systemArgv: async () => argv, alive: (p) => p === 77,
    envFor: () => ({}), portFree: boom("an answering ComfyUI is never bind-probed"), mkdirs: boom("no directories for a reused instance"),
    spawn: boom("nothing is launched"), killPid: boom("never kill"),
    ...extra,
  });
}

test("reusing a kept instance under a new lease takes its marker over", async () => {
  const k = keptInstance({ leaseEpoch: 5 });
  const before = readMarker(k.dir, "side");
  assert.equal(await reuse(k, { GPU_LEASE_EPOCH: "6" }), null, "the instance is reused, not launched");
  const after = readMarker(k.dir, "side");
  assert.equal(after.leaseEpoch, 6, "the instance now belongs to the lease that reuses it, so that lease's release stops it");
  // Everything that proves the instance is the harness's own is untouched.
  assert.deepEqual({ ...after, leaseEpoch: undefined }, { ...before, leaseEpoch: undefined });
});

test("reusing it under the same lease, or outside a lease, leaves the marker byte for byte alone", async () => {
  for (const env of [{ GPU_LEASE_EPOCH: "5" }, {}]) {
    const k = keptInstance({ leaseEpoch: 5 });
    const raw = readFileSync(markerFile(k.dir, "side"), "utf8");
    const old = new Date(2020, 0, 1);
    utimesSync(markerFile(k.dir, "side"), old, old);
    assert.equal(await reuse(k, env), null);
    assert.equal(readFileSync(markerFile(k.dir, "side"), "utf8"), raw);
    assert.equal(statSync(markerFile(k.dir, "side")).mtimeMs, old.getTime(), "and it is not even rewritten");
  }
});

test("an instance that was never a lease's is not claimed by a lease that reuses it", async () => {
  const k = keptInstance(); // a marker with no epoch: the operator's own kept instance
  const raw = readFileSync(markerFile(k.dir, "side"), "utf8");
  assert.equal(await reuse(k, { GPU_LEASE_EPOCH: "6" }), null);
  assert.equal(readFileSync(markerFile(k.dir, "side"), "utf8"), raw, "taking it over would have the lease stop an instance it never started");
});

test("a re-stamp that cannot be written is said, and the instance is still reused", async () => {
  const k = keptInstance({ leaseEpoch: 5 });
  const lines = [];
  const child = await reuse(k, { GPU_LEASE_EPOCH: "6" }, {
    log: (l) => lines.push(l),
    restampLaunch: () => { throw new Error("EPERM: operation not permitted"); },
  });
  assert.equal(child, null);
  assert.equal(lines.filter((l) => /COMFY-KEEP-WARN/.test(l)).length, 1);
  assert.match(lines.join("\n"), /still names lease epoch 5/);
});

test("the default instance has no lease epoch and is never re-stamped", async () => {
  const dir = scratch();
  const argv = ["main.py", ...BASE_FLAGS];
  O.writeLaunchOwner(dir, { pid: 77, ownerPid: 5151, args: argv, profile });
  const raw = readFileSync(join(dir, ".offload-launch.json"), "utf8");
  const calls = [];
  await L.ensureComfy({
    api: "http://127.0.0.1:8188", env: { GPU_LEASE_EPOCH: "6" }, comfyDir: dir, py: "py", pollMs: 1, log: () => {},
    comfyUp: async () => true, systemArgv: async () => argv, alive: (p) => p === 77, envFor: () => ({}),
    spawn: boom("nothing is launched"), restampLaunch: (...a) => calls.push(a),
  });
  assert.deepEqual(calls, []);
  assert.equal(readFileSync(join(dir, ".offload-launch.json"), "utf8"), raw);
});

// ---- restampLaunchOwner itself ---------------------------------------------------------------

test("restampLaunchOwner changes the epoch and nothing else", () => {
  const k = keptInstance({ leaseEpoch: 5 });
  const before = readMarker(k.dir, "side");
  assert.deepEqual(O.restampLaunchOwner(k.dir, "side", 9), { changed: true, previous: 5 });
  const after = readMarker(k.dir, "side");
  assert.equal(after.leaseEpoch, 9);
  assert.equal(after.startedAt, before.startedAt, "the launch time is the instance's, not the re-stamp's: the pid-recycle proof reads it");
  assert.equal(after.pid, before.pid);
  assert.deepEqual(after.args, before.args);
  assert.equal(readdirSync(k.dir).filter((f) => f.endsWith(".tmp")).length, 0, "no temporary file is left behind");
});

test("restampLaunchOwner leaves an epochless, absent or foreign marker alone", () => {
  const none = keptInstance();
  assert.deepEqual(O.restampLaunchOwner(none.dir, "side", 9), { changed: false, why: "the marker names no lease" });
  assert.equal(readMarker(none.dir, "side").leaseEpoch, undefined);
  const empty = scratch();
  assert.deepEqual(O.restampLaunchOwner(empty, "side", 9), { changed: false, why: "no marker" });
  const k = keptInstance({ leaseEpoch: 5 });
  assert.deepEqual(O.restampLaunchOwner(k.dir, "other", 9), { changed: false, why: "no marker" });
  assert.deepEqual(O.restampLaunchOwner(k.dir, "side", 5), { changed: false, why: "already this lease's" });
});

// ---- a kept launch whose marker cannot be written is said ----------------------------------

test("a kept instance whose launch marker cannot be written says so: nothing can stop it by its lease", async () => {
  const dir = scratch();
  const lines = [];
  let up = false;
  await L.ensureComfy({
    keep: true, api: "http://127.0.0.1:8190", env: { COMFY_INSTANCE: "side", GPU_LEASE_EPOCH: "6" }, comfyDir: dir, py: "py", pollMs: 1, log: (l) => lines.push(l),
    comfyUp: async () => up, portFree: async () => true, mkdirs: () => {}, envFor: () => ({}),
    openLogFd: () => 5, closeFd: () => {},
    spawn: () => { up = true; return { pid: 9, unref() {}, kill() {}, once() {} }; },
    writeLaunch: () => { throw new Error("ENOSPC: no space left on device"); },
  });
  const warn = lines.filter((l) => /COMFY-KEEP-WARN/.test(l) && /launch marker/.test(l));
  assert.equal(warn.length, 1, `expected one warning about the marker, got ${JSON.stringify(lines)}`);
  assert.match(warn[0], /ENOSPC/);
  assert.match(warn[0], /lease/);
});

test("a launch that is not kept stays silent about its marker, as before", async () => {
  const dir = scratch();
  const lines = [];
  let up = false;
  await L.ensureComfy({
    api: "http://127.0.0.1:8190", env: { COMFY_INSTANCE: "side" }, comfyDir: dir, py: "py", pollMs: 1, log: (l) => lines.push(l),
    comfyUp: async () => up, portFree: async () => true, mkdirs: () => {}, envFor: () => ({}),
    spawn: () => { up = true; const c = { pid: 9, kill() {}, once() {}, stdout: { on() {} }, stderr: { on() {} } }; return c; },
    writeLaunch: () => { throw new Error("ENOSPC"); },
  });
  assert.equal(lines.filter((l) => /COMFY-KEEP-WARN/.test(l)).length, 0);
});
