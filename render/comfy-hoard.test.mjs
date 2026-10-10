// node --test render/comfy-hoard.test.mjs
//
// A kept instance that last ran another family is freed before its first job (render/comfy-family.mjs; the
// paging incident of 2026-10-09). Everything runs against a FAKE ComfyUI: an HTTP server on an ephemeral loopback port
// that records the order of the requests it receives and answers /free and /system_stats. No real
// ComfyUI is started or contacted, and the ports are never 8188-8191.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { freeComfy, withGpuSlot } from "./gpu-lock.mjs";
import { familySignature, settleInstanceFamily, freeHarnessInstance } from "./comfy-family.mjs";
import { readLaunchOwner, stampLaunchFamily } from "./comfy-ownership.mjs";
import { runGraphFlow } from "./comfy-run-graph.mjs";

const MEDIA = { dir: "X", epoch: 7, class: "media" };
const KEY = "gfake0001";

// fakeComfy: records every request, in order, into `events` (shared with the job so ordering can be asserted).
async function fakeComfy({ events, freeStatus = 200, argv = ["main.py", "--port", "1"] } = {}) {
  const frees = [];
  const srv = createServer((req, res) => {
    let body = "";
    req.on("data", (d) => (body += d));
    req.on("end", () => {
      if (req.method === "POST" && req.url === "/free") {
        frees.push(JSON.parse(body || "{}"));
        events.push("free");
        res.statusCode = freeStatus;
        return res.end("{}");
      }
      if (req.url === "/system_stats") {
        res.setHeader("content-type", "application/json");
        return res.end(JSON.stringify({ system: { argv } }));
      }
      res.statusCode = 404; res.end();
    });
  });
  await new Promise((r) => srv.listen({ port: 0, host: "127.0.0.1" }, r));
  const { port } = srv.address();
  assert.ok(port < 8188 || port > 8191, "the fake must never sit on 8188-8191");
  return { api: `http://127.0.0.1:${port}`, port, frees, close: () => new Promise((r) => srv.close(r)) };
}

// A comfy directory whose launch marker for the keyed instance says what it may still hold.
function comfyDirWith(marker) {
  const dir = mkdtempSync(join(tmpdir(), "comfy-hoard-"));
  if (marker) writeFileSync(join(dir, `.offload-launch-${KEY}.json`), JSON.stringify({ startedAt: 1, pid: 4242, ownerPid: 1, args: ["main.py", "--port", "1"], profile: {}, key: KEY, port: 1, leaseEpoch: 7, ...marker }));
  return dir;
}

const ENV = { COMFY_INSTANCE: KEY };
const quiet = { log: () => {} };

// withGpuSlot against the fake, ensureComfy standing in for "an instance that was already up" (null).
function slot(fake, comfyDir, family, fn, extra = {}) {
  return withGpuSlot({
    noLock: true, keepComfy: true, comfyManaged: true, api: fake.api, instanceEnv: ENV, comfyDir, family,
    freeLlamaSwap: async () => {}, ensureComfy: async () => null, lease: MEDIA, checkLease: () => true, claimUnload: () => false,
    ...extra,
  }, fn);
}

test("teardown ALWAYS sends /free {unload_models, free_memory} to a KEPT keyed instance, after the job", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith(null);
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const out = await slot(fake, dir, "", async () => { events.push("job"); return "done"; });
  assert.equal(out, "done");
  assert.deepEqual(events, ["job", "free"], "the free comes after the job, once");
  assert.deepEqual(fake.frees, [{ unload_models: true, free_memory: true }], "BOTH flags: weights and executor caches");
});

test("a failing /free is said loudly, never swallowed and never fatal to the finished job", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events, freeStatus: 500 });
  const dir = comfyDirWith(null);
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const lines = [];
  const orig = console.error;
  console.error = (m) => lines.push(String(m));
  try {
    assert.equal(await slot(fake, dir, "", async () => "done"), "done", "the job's result survives a failed free");
  } finally { console.error = orig; }
  assert.equal(fake.frees.length, 2, "one retry");
  assert.ok(lines.some((l) => /COMFY-FREE-WARN.*HTTP 500.*may still hold its models/.test(l)), lines.join("\n"));
});

test("an instance that is simply not there holds nothing: a refused connection is quiet and counts as freed", async () => {
  // A port that was open a moment ago and is closed now (port 1 is on fetch's blocked list: not a refusal).
  const gone = await fakeComfy({ events: [] });
  const api = gone.api;
  await gone.close();
  const lines = [];
  const ok = await freeComfy(api, { log: (m) => lines.push(m), attempts: 2, timeoutMs: 2000 });
  assert.equal(ok, true);
  assert.deepEqual(lines, []);
});

test("freeComfy resolves true on an acknowledged free", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  t.after(() => fake.close());
  assert.equal(await freeComfy(fake.api, quiet), true);
  assert.equal(fake.frees.length, 1);
});

test("a kept instance whose last family DIFFERS is freed BEFORE the first job, and records the new family", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith({ lastFamily: familySignature("qwen-image", "qwen-image-2512-Q5_1.gguf") });
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const krea2 = familySignature("krea2", "krea2_turbo_bf16.safetensors");
  await slot(fake, dir, krea2, async () => {
    events.push("job");
    // While the job runs the marker already names the family the instance now holds.
    assert.equal(readLaunchOwner(dir, KEY).lastFamily, krea2);
  }, { settleFamily: (o) => settleInstanceFamily({ ...o, settleMs: 0, log: () => {} }) });
  assert.deepEqual(events, ["free", "job", "free"], "free (family changed) -> job -> free (teardown)");
});

test("the SAME family keeps its warm weights: no free before the first job, only the teardown free", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const sig = familySignature("krea2", "krea2_turbo_bf16.safetensors");
  const dir = comfyDirWith({ lastFamily: sig });
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  await slot(fake, dir, sig, async () => { events.push("job"); }, { settleFamily: (o) => settleInstanceFamily({ ...o, settleMs: 0, log: () => {} }) });
  assert.deepEqual(events, ["job", "free"]);
});

test("a freshly launched instance (a marker with no family) is not freed first, and records its family", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith({});
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const sig = familySignature("krea2", "k.safetensors");
  await slot(fake, dir, sig, async () => { events.push("job"); assert.equal(readLaunchOwner(dir, KEY).lastFamily, sig); },
    { settleFamily: (o) => settleInstanceFamily({ ...o, settleMs: 0, log: () => {} }) });
  assert.deepEqual(events, ["job", "free"]);
});

test("after the teardown free the instance holds nothing, so the marker no longer names a family", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith({});
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  await slot(fake, dir, familySignature("krea2", "k.safetensors"), async () => {}, { settleFamily: (o) => settleInstanceFamily({ ...o, settleMs: 0, log: () => {} }) });
  assert.equal(readLaunchOwner(dir, KEY).lastFamily, undefined, "freed, so nothing is held");
  // The next runner, of ANOTHER family, therefore has nothing to free first.
  events.length = 0;
  await slot(fake, dir, familySignature("qwen-image", "q.gguf"), async () => { events.push("job"); }, { settleFamily: (o) => settleInstanceFamily({ ...o, settleMs: 0, log: () => {} }) });
  assert.deepEqual(events, ["job", "free"]);
});

test("a free that did NOT succeed before the first job leaves the old family recorded, so the next runner tries again", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events, freeStatus: 500 });
  const old = familySignature("qwen-image", "q.gguf");
  const dir = comfyDirWith({ lastFamily: old });
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const lines = [];
  const r = await settleInstanceFamily({
    comfyDir: dir, key: KEY, api: fake.api, family: familySignature("krea2", "k.safetensors"),
    free: (a) => freeComfy(a, { log: () => {}, attempts: 1 }), log: (m) => lines.push(m), settleMs: 0,
  });
  assert.equal(r.freed, false);
  assert.equal(readLaunchOwner(dir, KEY).lastFamily, old, "still recorded as holding the old family");
  assert.ok(lines.some((l) => /COMFY-FAMILY-WARN.*qwen-image/.test(l)), lines.join("\n"));
});

test("an instance with no launch marker is not one this harness launched: settle leaves it alone", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith(null);
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const r = await settleInstanceFamily({ comfyDir: dir, key: KEY, api: fake.api, family: "krea2:k", free: (a) => freeComfy(a, quiet), settleMs: 0, log: () => {} });
  assert.equal(r.freed, false);
  assert.deepEqual(events, []);
});

test("stampLaunchFamily changes only the family: pid, argv and lease epoch stay what prove the instance is ours", () => {
  const dir = comfyDirWith({});
  try {
    const before = readLaunchOwner(dir, KEY);
    assert.deepEqual(stampLaunchFamily(dir, KEY, "krea2:k"), { changed: true, previous: "" });
    const after = readLaunchOwner(dir, KEY);
    assert.equal(after.lastFamily, "krea2:k");
    for (const k of ["pid", "args", "leaseEpoch", "key", "port", "startedAt"]) assert.deepEqual(after[k], before[k], k);
    assert.deepEqual(stampLaunchFamily(dir, KEY, "krea2:k"), { changed: false, previous: "krea2:k" });
    assert.deepEqual(stampLaunchFamily(dir, KEY, ""), { changed: true, previous: "krea2:k" });
    assert.equal("lastFamily" in readLaunchOwner(dir, KEY), false);
    assert.deepEqual(stampLaunchFamily(dir, "gother", "x"), { changed: false, why: "no marker" });
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("familySignature names the family and the weights file, directories dropped", () => {
  assert.equal(familySignature("Krea2", "D:\\models\\krea2_turbo_bf16.safetensors"), "krea2:krea2_turbo_bf16.safetensors");
  assert.equal(familySignature("sdxl"), "sdxl");
  assert.equal(familySignature("", ""), "default");
  assert.notEqual(familySignature("qwen-image", "bf16.safetensors"), familySignature("qwen-image", "Q5.gguf"), "two models of one family are two signatures");
});

// ---- run-graph: the fourth way an instance came to hold two families ----------------------------------

test("freeHarnessInstance frees a kept instance the harness launched, and clears its family", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith({ lastFamily: "graph:x" });
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  const r = await freeHarnessInstance({
    comfyDir: dir, key: KEY, api: fake.api, free: (a) => freeComfy(a, quiet), alive: () => true, log: () => {},
    systemArgv: async () => ["main.py", "--port", "1"],
  });
  assert.equal(r.freed, true);
  assert.deepEqual(events, ["free"]);
  assert.equal("lastFamily" in readLaunchOwner(dir, KEY), false);
});

test("freeHarnessInstance leaves a ComfyUI the harness did not start alone", async (t) => {
  const events = [];
  const fake = await fakeComfy({ events });
  const dir = comfyDirWith({});
  t.after(async () => { await fake.close(); rmSync(dir, { recursive: true, force: true }); });
  // argv differs from the marker's: not shown to be ours
  let r = await freeHarnessInstance({ comfyDir: dir, key: KEY, api: fake.api, free: (a) => freeComfy(a, quiet), alive: () => true, log: () => {}, systemArgv: async () => ["main.py", "--port", "9"] });
  assert.equal(r.freed, false);
  // the marker's pid is gone: not shown to be ours
  r = await freeHarnessInstance({ comfyDir: dir, key: KEY, api: fake.api, free: (a) => freeComfy(a, quiet), alive: () => false, log: () => {}, systemArgv: async () => ["main.py", "--port", "1"] });
  assert.equal(r.freed, false);
  // no marker at all
  r = await freeHarnessInstance({ comfyDir: comfyDirWith(null), key: KEY, api: fake.api, free: (a) => freeComfy(a, quiet), alive: () => true, log: () => {}, systemArgv: async () => ["main.py", "--port", "1"] });
  assert.equal(r.freed, false);
  assert.deepEqual(events, [], "nothing was sent to an instance that is not shown to be ours");
});

test("run-graph frees what its graph left on an instance that was already up (the kept instance), not only the one it started", async () => {
  let freedKept = 0;
  const base = {
    graph: {}, manifest: { node_packs: [], models: [] }, outDir: ".", resultPath: "r.json", api: "http://127.0.0.1:1", comfyDir: "C", reserveVram: undefined,
  };
  const deps = (wasUp) => ({
    parseManifest: () => ({}), manifestHash: () => "h", readOwner: () => ({ manifestHash: "h" }), writeOwner: () => {},
    comfyUp: async () => wasUp, ensureComfy: async () => ({ kill() {} }), killComfy: () => {}, freeComfy: async () => {},
    freeKept: async () => { freedKept++; },
    satisfy: async () => ({ ok: true, changed: false, unverified: [] }),
    preflight: async () => ({ ok: true, unknownClasses: [], missing: [] }),
    postGraph: Object.assign(async () => ({ prompt_id: "p" }), { history: async () => ({ "1": [] }) }),
    collect: async () => ({ "9": [{ filename: "a.png", type: "output", kind: "image" }] }),
    fetchToDir: async (f, d) => ({ path: join(d, f.filename), type: f.type, kind: f.kind }),
    writeResult: () => {},
  });
  await runGraphFlow(base, deps(true));
  assert.equal(freedKept, 1, "an instance that was already up is freed by the dep that knows whose it is");
  await runGraphFlow(base, deps(false));
  assert.equal(freedKept, 1, "an instance run-graph started itself is freed and killed the old way, not twice");
});
