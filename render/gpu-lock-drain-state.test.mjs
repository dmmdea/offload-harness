// node --test render/gpu-lock-drain-state.test.mjs
//
// The drain (quiesceLlamaSwap, gpu-lock.mjs) and the seat's state (register F81). llama-swap's /running lists
// every process that is not stopped, each with a `state`: starting, ready or stopping. The drain used to probe
// /upstream/<id>/slots of every listed seat, and for a seat that is not ready that probe is the wrong question:
//
//   starting  the upstream is not serving yet. llama-swap holds a request for the whole load (4 min on a big
//             seat), so the probe sat until its own 5 s deadline, three rounds and a grace in all: ~17 s added
//             to every render start while a seat was starting, and no request could be in flight to wait for.
//   stopping  the upstream is being killed (an in-flight request dies with it; llama-swap does not wait for it),
//             and a request to it is queued behind the stop and then STARTS the seat again: the probe would
//             load the model the drain exists to clear.
//
// A seat in either state is skipped: nothing is in flight on its upstream, so nothing is waited for and nothing is unknown.
// The drain names the seats it skipped because they were starting (`starting`), and freeLlamaSwap says, once, that it is
// unloading a load in progress, because the request that triggered that load dies with it.
// A seat that is ready, or whose state is absent or one this code does not know, is probed exactly as before.
// These tests run the real drain against a fake llama-swap on a loopback port that records every request.
import { test } from "node:test";
import assert from "node:assert/strict";
import { quiesceLlamaSwap, freeLlamaSwap } from "./gpu-lock.mjs";
import { startFakeLlamaSwap } from "./fake-llama-swap.mjs";

const BUSY = [{ id: 0, is_processing: true }];
const IDLE = [{ id: 0, is_processing: false }];

async function fakeWith(t, seats) {
  const fake = await startFakeLlamaSwap({ seats });
  t.after(() => fake.close());
  return fake;
}

// the deadline of one probe is short here, so the drain this fix removes costs ~4 s instead of ~17 s when a
// test goes red; the fix is judged by the requests that were sent, and by a bound far below even that
const FAST = { requestTimeoutMs: 400, pollMs: 20, graceMs: 50 };

for (const state of ["starting", "stopping", "Starting", " STOPPING "]) {
  test(`a seat that is ${JSON.stringify(state)} is not probed: nothing is in flight, the drain ends at once and names nothing unknown`, async (t) => {
    const fake = await fakeWith(t, [{ id: "seat-a", state, slots: "hang" }]);
    const t0 = Date.now();
    const r = await quiesceLlamaSwap(["seat-a"], { api: fake.url, ...FAST });
    const took = Date.now() - t0;
    assert.equal(fake.count("GET /upstream/"), 0, `a request reached the seat: ${fake.seen.join(", ")}`);
    assert.equal(r.drained, true);
    assert.deepEqual(r.unknown, []);
    assert.deepEqual(r.starting, state.trim().toLowerCase() === "starting" ? ["seat-a"] : [], "a seat whose load was not waited for is named, so the caller can say so; a stopping seat is not a load");
    assert.ok(took < 1000, `the drain took ${took} ms for a seat with nothing to drain`);
  });
}

test("a ready seat with a busy slot is still waited for, poll by poll, until the slot is idle", async (t) => {
  const fake = await fakeWith(t, [{ id: "seat-a", state: "ready", slots: (n) => (n < 3 ? BUSY : IDLE) }]);
  const r = await quiesceLlamaSwap(["seat-a"], { api: fake.url, ...FAST });
  assert.equal(r.drained, true);
  assert.deepEqual(r.starting, [], "a ready seat is probed, not skipped");
  assert.equal(fake.count("GET /upstream/seat-a/slots"), 3, "two busy readings, then the idle one");
  assert.ok(r.waitedMs >= 2 * FAST.pollMs, `the drain did not wait between the readings (${r.waitedMs} ms)`);
});

test("a ready seat that never answers /slots is probed every round and named unknown, never skipped", async (t) => {
  const fake = await fakeWith(t, [{ id: "seat-a", state: "ready", slots: "hang" }]);
  const r = await quiesceLlamaSwap(["seat-a"], { api: fake.url, requestTimeoutMs: 300, pollMs: 10, graceMs: 10 });
  assert.equal(r.drained, false);
  assert.deepEqual(r.unknown, ["seat-a"]);
  // the drain retries an unobservable seat a bounded number of rounds (three in all today); the loop and the
  // server share this process, so a round whose deadline fires before the server has read the request is not
  // counted: the claim is "probed, and more than once", not the exact number
  assert.ok(fake.count("GET /upstream/seat-a/slots") >= 2, "the seat was not probed: " + fake.seen.join(", "));
});

test("a starting seat beside a busy ready seat: the ready one is waited for, the starting one is not touched", async (t) => {
  const fake = await fakeWith(t, [
    { id: "loading", state: "starting", slots: "hang" },
    { id: "serving", state: "ready", slots: (n) => (n < 3 ? BUSY : IDLE) },
    { id: "leaving", state: "stopping", slots: "hang" },
  ]);
  const r = await quiesceLlamaSwap(["loading", "serving", "leaving"], { api: fake.url, ...FAST });
  assert.equal(r.drained, true);
  assert.deepEqual(r.unknown, []);
  assert.deepEqual(r.starting, ["loading"], "named by the last round, though the ready seat took several");
  assert.equal(fake.count("GET /upstream/serving/slots"), 3);
  assert.equal(fake.count("GET /upstream/loading/"), 0);
  assert.equal(fake.count("GET /upstream/leaving/"), 0);
});

test("a seat that finished loading while the drain waited on another is probed from then on and is not named", async (t) => {
  let fake;
  fake = await fakeWith(t, [
    { id: "late", state: "starting", slots: IDLE },
    { id: "serving", state: "ready", slots: (n) => { if (n === 1) fake.seat("late").state = "ready"; return n < 3 ? BUSY : IDLE; } },
  ]);
  const r = await quiesceLlamaSwap(["late", "serving"], { api: fake.url, ...FAST });
  assert.equal(r.drained, true);
  assert.deepEqual(r.starting, [], "the list is the last round's: this seat was ready by then");
  assert.ok(fake.count("GET /upstream/late/slots") >= 1, "once ready it is probed like any other seat");
});

// A llama-swap that lists a seat without a state (older builds), or with one this code has never heard of, is
// not a seat known to be idle: the probe stays. Only the two states that cannot carry a request are skipped.
for (const [what, entry] of [["no state", { model: "m" }], ["an empty state", { model: "m", state: "" }], ["an unknown state", { model: "m", state: "reloading" }], ["state ready", { model: "m", state: "ready" }]]) {
  test(`a seat listed with ${what} is probed as before`, async () => {
    let slotCalls = 0;
    const fetchImpl = async (u) => {
      if (u.endsWith("/running")) return { ok: true, status: 200, json: async () => ({ running: [entry] }) };
      if (u.endsWith("/upstream/m/slots")) {
        slotCalls++;
        return { ok: true, status: 200, json: async () => (slotCalls < 3 ? BUSY : IDLE) };
      }
      throw new Error("unexpected " + u);
    };
    const r = await quiesceLlamaSwap(["m"], { fetchImpl, pollMs: 1, graceMs: 1, timeoutMs: 5_000 });
    assert.equal(r.drained, true);
    assert.equal(slotCalls, 3, "the busy readings were waited out");
  });
}

test("freeLlamaSwap: a render start does not wait on a seat that is starting, and still unloads it", async (t) => {
  const fake = await fakeWith(t, [{ id: "seat-a", state: "starting", slots: "hang" }]);
  const logs = [];
  const t0 = Date.now();
  await freeLlamaSwap(fake.url, {
    unloadModels: null, leaseDevices: [], log: (m) => logs.push(m),
    quiesce: (ids, o) => quiesceLlamaSwap(ids, { ...o, api: fake.url, ...FAST }),
  });
  const took = Date.now() - t0;
  assert.equal(fake.count("GET /upstream/"), 0, `a request reached the seat: ${fake.seen.join(", ")}`);
  assert.equal(fake.count("POST /api/models/unload/seat-a"), 1, "the starting seat still goes: the render needs its card");
  assert.equal(logs.length, 1, `one line, said: ${JSON.stringify(logs)}`);
  assert.match(logs[0], /unloading seat-a while its load is in progress/, "names the seat and the load");
  assert.match(logs[0], /a request waiting on it is aborted/, "names what the unload costs");
  assert.ok(took < 1000, `the start took ${took} ms behind a seat that cannot have a request in flight`);
});

// The Go-side drain waits a load out (a load in progress is work in flight: the request that triggered it is waiting on
// the engine). The render helper does not wait, by decision (register F81: seconds on every render start, and no
// request on the upstream to read), but the unload that follows kills that request, so it says so, once, and only for
// a load: a seat that is already stopping is leaving and a ready seat is probed.
test("freeLlamaSwap: a starting seat among ready ones is named once; a stopping seat and a ready one are not", async (t) => {
  const fake = await fakeWith(t, [
    { id: "loading", state: "starting", slots: "hang" },
    { id: "leaving", state: "stopping", slots: "hang" },
    { id: "serving", state: "ready", slots: IDLE },
  ]);
  const logs = [];
  await freeLlamaSwap(fake.url, {
    unloadModels: null, leaseDevices: [], log: (m) => logs.push(m),
    quiesce: (ids, o) => quiesceLlamaSwap(ids, { ...o, api: fake.url, ...FAST }),
  });
  assert.equal(logs.length, 1, JSON.stringify(logs));
  assert.match(logs[0], /unloading loading while/);
  assert.doesNotMatch(logs[0], /leaving|serving/);
  for (const id of ["loading", "leaving", "serving"]) assert.equal(fake.count("POST /api/models/unload/" + id), 1, id + " is unloaded");
});

test("freeLlamaSwap: two starting seats are named together, in the plural", async (t) => {
  const fake = await fakeWith(t, [
    { id: "load-a", state: "starting", slots: "hang" },
    { id: "load-b", state: "starting", slots: "hang" },
  ]);
  const logs = [];
  await freeLlamaSwap(fake.url, {
    unloadModels: null, leaseDevices: [], log: (m) => logs.push(m),
    quiesce: (ids, o) => quiesceLlamaSwap(ids, { ...o, api: fake.url, ...FAST }),
  });
  assert.equal(logs.length, 1, JSON.stringify(logs));
  assert.match(logs[0], /unloading load-a,load-b while their loads are in progress/);
  assert.match(logs[0], /a request waiting on them is aborted/);
});

test("a drain that ends at its deadline still names the seat it did not wait for, and freeLlamaSwap says both things", async (t) => {
  const fake = await fakeWith(t, [
    { id: "loading", state: "starting", slots: "hang" },
    { id: "busy", state: "ready", slots: BUSY },
  ]);
  const r = await quiesceLlamaSwap(["loading", "busy"], { api: fake.url, timeoutMs: 150, ...FAST });
  assert.equal(r.drained, false, "the ready seat never went idle");
  assert.deepEqual(r.starting, ["loading"]);
  const logs = [];
  await freeLlamaSwap(fake.url, {
    unloadModels: null, leaseDevices: [], log: (m) => logs.push(m), drainTimeoutMs: 150,
    quiesce: (ids, o) => quiesceLlamaSwap(ids, { ...o, api: fake.url, ...FAST }),
  });
  assert.equal(logs.length, 2, JSON.stringify(logs));
  assert.match(logs[0], /proceeding without a verified drain/);
  assert.match(logs[1], /unloading loading while its load is in progress/);
});

test("freeLlamaSwap: a drain stub that reports no starting list does not break the unload or add a line", async (t) => {
  const fake = await fakeWith(t, [{ id: "seat-a", state: "ready", slots: IDLE }]);
  const logs = [];
  await freeLlamaSwap(fake.url, { unloadModels: null, leaseDevices: [], log: (m) => logs.push(m), quiesce: async () => ({ drained: true, waitedMs: 0, unknown: [] }) });
  assert.deepEqual(logs, []);
  assert.equal(fake.count("POST /api/models/unload/seat-a"), 1);
});
