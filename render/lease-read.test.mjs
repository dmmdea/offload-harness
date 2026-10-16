// node --test render/lease-read.test.mjs
//
// The Node readers of the lease (gpu-lock.mjs readLease / readEpochRecord / readCardClaim / checkInheritedLease, and the
// lease-progress.mjs reader) read files that the Go side replaces by a rename over the live name. On Windows, for the
// instant a rename has marked the old entry for deletion and the new one has not taken its name, every open of the name
// fails with ERROR_ACCESS_DENIED, however the opener shares it; libuv reports that as EPERM. A reader that took the
// failure for "no record" answered "this lease is not current" for a lease that was held, and the render aborted at its
// fence. lease-read.mjs waits that refusal out (EPERM and EACCES only, about 100 ms in all, the bound internal/gpulease
// uses) and reads a missing file once. internal/gpulease/readretry_test.go is the Go twin of sections 1 and 2.
//
//   1. the helper, with the read and the pause injected (so the counts are exact and nothing sleeps for long)
//   2. the real readers, with fs.readFileSync refusing a path the way the system does
//
// The stress against a real rename-over writer is lease-read-stress.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs, { mkdirSync, writeFileSync } from "node:fs";
import { syncBuiltinESMExports } from "node:module";
import { join } from "node:path";
import { scratchDir } from "./test-scratch.mjs";
import {
  DENIED_BUDGET_MS, DENIED_PAUSE_MS, policy, readSharedJson, readSharedSync,
} from "./lease-read.mjs";
import {
  checkInheritedLease, readCardClaim, readEpochRecord, readLease,
} from "./gpu-lock.mjs";
import { leaseProgressFile } from "./lease-progress.mjs";

const fsError = (code, path = "x") => Object.assign(new Error(`${code}: open '${path}'`), { code, syscall: "open", path });

// A scripted read: each call takes the next outcome of `plan` (the last one repeats). An outcome that is a string is the
// file's content; an `{ code }` is an error with that code. Counts the reads and the pauses asked for, on a fake clock that a
// pause advances by `tick` ms (the pause asked for by default; Windows sleeps a whole timer tick, 15.4 ms measured, whatever is asked).
function scripted(plan, { tick = null } = {}) {
  const seen = { reads: 0, pauses: [], clock: 0 };
  return {
    seen,
    read: (path) => {
      const step = plan[Math.min(seen.reads, plan.length - 1)];
      seen.reads += 1;
      // a helper that never pauses never moves the fake clock: end its loop here, as a failure and not as a hang
      if (seen.reads > 1000) throw new Error("runaway: more than 1000 reads without the clock moving");
      if (typeof step === "string") return step;
      throw fsError(step.code, path);
    },
    sleep: (ms) => { seen.pauses.push(ms); seen.clock += tick === null ? ms : tick; },
    now: () => seen.clock,
  };
}

function withWait(t, value = true) {
  const old = policy.waitsOutDenied;
  policy.waitsOutDenied = value;
  t.after(() => { policy.waitsOutDenied = old; });
}

// ---- 1. the helper ---------------------------------------------------------------------------------------

test("a file that is not there is read once and reads as absent, with no pause (a released lease stays absent at once)", (t) => {
  withWait(t);
  const s = scripted([{ code: "ENOENT" }]);
  assert.equal(readSharedSync("gone.json", { read: s.read, sleep: s.sleep, now: s.now }), null);
  assert.equal(s.seen.reads, 1, "a missing file is an answer, not a refusal to wait out");
  assert.deepEqual(s.seen.pauses, []);
});

test("an error that is not a permission refusal is not retried", (t) => {
  withWait(t);
  for (const code of ["EBUSY", "EISDIR", "EIO", "EMFILE", "ENOTDIR", "ELOOP"]) {
    const s = scripted([{ code }]);
    assert.equal(readSharedSync("x.json", { read: s.read, sleep: s.sleep, now: s.now }), null, code);
    assert.equal(s.seen.reads, 1, `${code} was read ${s.seen.reads} times, want 1`);
    assert.deepEqual(s.seen.pauses, [], code);
  }
});

test("refused opens are waited out: the first N reads fail with EPERM, then the file's content comes back", (t) => {
  withWait(t);
  const s = scripted([{ code: "EPERM" }, { code: "EPERM" }, { code: "EPERM" }, "{\"epoch\":7}"]);
  assert.equal(readSharedSync("e/7.json", { read: s.read, sleep: s.sleep, now: s.now }), "{\"epoch\":7}");
  assert.equal(s.seen.reads, 4, "three refused, one through");
  assert.deepEqual(s.seen.pauses, [DENIED_PAUSE_MS, DENIED_PAUSE_MS, DENIED_PAUSE_MS], "one pause between reads");
});

test("EACCES is waited out like EPERM", (t) => {
  withWait(t);
  const s = scripted([{ code: "EACCES" }, { code: "EPERM" }, "ok"]);
  assert.equal(readSharedSync("meta.json", { read: s.read, sleep: s.sleep, now: s.now }), "ok");
  assert.equal(s.seen.reads, 3);
});

test("a refusal that never ends is given up on after the bound, with the pauses that make up about 100 ms, and does not hang", (t) => {
  withWait(t);
  const s = scripted([{ code: "EPERM" }]);
  assert.equal(readSharedSync("meta.json", { read: s.read, sleep: s.sleep, now: s.now }), null);
  const pauses = DENIED_BUDGET_MS / DENIED_PAUSE_MS;
  assert.equal(s.seen.pauses.length, pauses, `${s.seen.pauses.length} pauses, want ${pauses}`);
  assert.equal(s.seen.reads, 1 + pauses, `${s.seen.reads} reads, want the first and ${pauses} retries`);
  assert.equal(s.seen.clock, DENIED_BUDGET_MS, "the pauses add up to the bound");
});

test("the bound is a deadline, not a count of pauses: a timer that sleeps a whole 15.4 ms tick still gives up after about 100 ms", (t) => {
  withWait(t);
  // Atomics.wait(5) comes back after one timer tick on Windows (measured 15.4 ms for 1, 2, 5 and 10 alike): 20 pauses would be 308 ms.
  const s = scripted([{ code: "EPERM" }], { tick: 15.4 });
  assert.equal(readSharedSync("meta.json", { read: s.read, sleep: s.sleep, now: s.now }), null);
  assert.equal(s.seen.pauses.length, 7, `${s.seen.pauses.length} pauses of 15.4 ms, want 7 (the first that reaches 100 ms ends it)`);
  assert.ok(s.seen.clock >= DENIED_BUDGET_MS && s.seen.clock < DENIED_BUDGET_MS + 16, `gave up at ${s.seen.clock.toFixed(1)} ms, want the first tick past ${DENIED_BUDGET_MS}`);
});

test("the bound in real time: a permanent refusal costs about 100 ms and then reads as absent", (t) => {
  withWait(t);
  const s = scripted([{ code: "EPERM" }]);
  const realSleep = (ms) => { s.seen.pauses.push(ms); Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms); };
  const t0 = performance.now();
  assert.equal(readSharedSync("meta.json", { read: s.read, sleep: realSleep }), null, "the real clock is used when none is injected");
  const took = performance.now() - t0;
  assert.ok(took >= DENIED_BUDGET_MS - 5, `gave up after ${took.toFixed(0)} ms (${s.seen.pauses.length} pauses), want the bound (about ${DENIED_BUDGET_MS} ms)`);
  // No upper bound on the wall clock: a loaded runner stalls a test for longer than the 308 ms this once guarded against. That
  // failure (20 pauses of a 15.4 ms tick) is pinned without a clock by the fake-clock test above, which wants 7 pauses.
});

test("off Windows a permission refusal is returned at once: nothing there is delete-pending, so a wait only delays the answer", (t) => {
  withWait(t, false);
  const s = scripted([{ code: "EACCES" }]);
  assert.equal(readSharedSync("meta.json", { read: s.read, sleep: s.sleep, now: s.now }), null);
  assert.equal(s.seen.reads, 1);
  assert.deepEqual(s.seen.pauses, []);
});

test("the default wait is on exactly where a rename can cause a refusal: Windows", () => {
  assert.equal(policy.waitsOutDenied, process.platform === "win32");
});

test("the wait has the Go side's bound: DENIED_BUDGET_MS is 100 ms at DENIED_PAUSE_MS 5 (internal/gpulease removeAttempts x removePause)", () => {
  assert.equal(DENIED_BUDGET_MS, 100);
  assert.equal(DENIED_PAUSE_MS, 5);
});

test("a real file reads, a real missing file is absent, and readSharedJson parses or says null", (t) => {
  const dir = scratchDir(t, "lease-read-");
  const f = join(dir, "r.json");
  writeFileSync(f, "{\"epoch\":3}");
  assert.equal(readSharedSync(f), "{\"epoch\":3}");
  assert.deepEqual(readSharedJson(f), { epoch: 3 });
  assert.equal(readSharedSync(join(dir, "none.json")), null);
  assert.equal(readSharedJson(join(dir, "none.json")), null);
  writeFileSync(f, "{not json");
  assert.equal(readSharedJson(f), null, "a record that does not parse is absent, as before");
});

// ---- 2. the real readers ---------------------------------------------------------------------------------

// Make the reads of `path` fail the way the system fails an open that meets a rename: the first `n` throw `code`, every
// other read goes through. fs.readFileSync is replaced and the named ESM exports re-synced, so the readers (which import it
// by name) see it. Returns the count of reads of that path.
const PRISTINE_READ = fs.readFileSync;
function refuseReads(t, path, n, code = "EPERM") {
  const real = fs.readFileSync; // may itself be a wrapper from an earlier call in this test: they stack
  const seen = { reads: 0 };
  fs.readFileSync = function (p, ...rest) {
    if (String(p) === path) {
      seen.reads += 1;
      if (seen.reads <= n) throw fsError(code, path);
    }
    return real.call(this, p, ...rest);
  };
  syncBuiltinESMExports();
  // restore the function the file started with, whatever order the hooks of stacked wrappers run in
  t.after(() => { fs.readFileSync = PRISTINE_READ; syncBuiltinESMExports(); });
  return seen;
}

// A card-scoped lease the way the Go side writes it: e/<epoch>.json and one claim per card.
function cardLease(dir, epoch, devices = ["gpu-test-0"], extra = {}) {
  mkdirSync(join(dir, "e"), { recursive: true });
  mkdirSync(join(dir, "cards"), { recursive: true });
  writeFileSync(join(dir, "e", `${epoch}.json`), JSON.stringify({ epoch, class: "media", holder: { pid: 1 }, devices, state: "active", ...extra }));
  for (const d of devices) writeFileSync(join(dir, "cards", `${d}.claim`), JSON.stringify({ epoch, at_ms: 1 }));
}

test("readEpochRecord waits out refused opens of the record and returns it", (t) => {
  withWait(t);
  const dir = scratchDir(t, "lease-read-");
  cardLease(dir, 7);
  const seen = refuseReads(t, join(dir, "e", "7.json"), 3);
  const rec = readEpochRecord(dir, 7);
  assert.equal(rec && rec.epoch, 7, "the record is there and good a moment later; null here is a lease read as gone");
  assert.equal(seen.reads, 4);
});

test("readLease waits out refused opens of meta.json and returns it", (t) => {
  withWait(t);
  const dir = scratchDir(t, "lease-read-");
  writeFileSync(join(dir, "meta.json"), JSON.stringify({ epoch: 5, class: "media" }));
  const seen = refuseReads(t, join(dir, "meta.json"), 3);
  const m = readLease(dir);
  assert.equal(m && m.epoch, 5);
  assert.equal(seen.reads, 4);
});

test("readCardClaim waits out refused opens of the claim and returns it (an object, and a bare epoch number)", (t) => {
  withWait(t);
  const dir = scratchDir(t, "lease-read-");
  cardLease(dir, 7);
  const claim = join(dir, "cards", "gpu-test-0.claim");
  const seen = refuseReads(t, claim, 3);
  const c = readCardClaim(dir, "gpu-test-0");
  assert.equal(c && c.epoch, 7);
  assert.equal(seen.reads, 4);
  writeFileSync(claim, "9\n");
  assert.deepEqual(readCardClaim(dir, "gpu-test-0"), { epoch: 9 }, "a bare epoch number is still read");
});

test("the fence holds a held card lease current through refused reads of its record and of its claim", (t) => {
  withWait(t);
  const dir = scratchDir(t, "lease-read-");
  cardLease(dir, 7);
  const rec = refuseReads(t, join(dir, "e", "7.json"), 2);
  const claim = refuseReads(t, join(dir, "cards", "gpu-test-0.claim"), 2);
  assert.equal(checkInheritedLease({ dir, epoch: 7 }), true,
    "a null record for a card lease falls through to meta.json, which a card lease never writes: the fence called a held lease not current and the render aborted");
  assert.equal(rec.reads, 3, "two refused, one through");
  assert.equal(claim.reads, 3);
});

test("the lease-progress reader finds the progress file of the lease's record through refused opens", (t) => {
  withWait(t);
  const dir = scratchDir(t, "lease-read-");
  const progress = join(dir, "progress.log");
  cardLease(dir, 7, ["gpu-test-0"], { progress: { file: progress } });
  const seen = refuseReads(t, join(dir, "e", "7.json"), 3);
  assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "7" }), progress);
  assert.equal(seen.reads, 4);
});

test("a released lease still reads absent through every reader, each file opened once (no wait is paid for a missing file)", (t) => {
  withWait(t);
  const dir = scratchDir(t, "lease-read-");
  mkdirSync(join(dir, "e"), { recursive: true });
  const watched = [join(dir, "e", "7.json"), join(dir, "meta.json"), join(dir, "cards", "gpu-test-0.claim")];
  const seen = watched.map((p) => refuseReads(t, p, 0));
  assert.equal(readEpochRecord(dir, 7), null);
  assert.equal(readLease(dir), null);
  assert.equal(readCardClaim(dir, "gpu-test-0"), null);
  assert.equal(checkInheritedLease({ dir, epoch: 7 }), false, "a lease that was released is not current");
  assert.equal(leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "7" }), "");
  // record: 1 (readEpochRecord) + 1 (fence) + 1 (progress) = 3; meta: 1 + 1 + 1 = 3; claim: 1
  assert.deepEqual(seen.map((s) => s.reads), [3, 3, 1], "a missing file is opened once per ask");
});
