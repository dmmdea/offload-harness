// node --test render/lease-read-gate.test.mjs
//
// The Node twin of internal/gpulease/gate_test.go. The Go side replaces a lease file by a rename over the live name. On Windows a
// rename that succeeded can leave the live name absent for tens of milliseconds with the staged file still beside it (measured 11 to 75
// ms, one rename that took 119 ms to return), and a reader that took that ENOENT for "no such file" answered null for a lease that was
// there a moment later: a null record of a card lease falls through to meta.json, which a card lease never writes, and the fence called
// a held lease not current and aborted the render.
//
// So an ENOENT is an answer only when it is SETTLED: no fresh staged file beside the name (STAGING_SUFFIXES, the names the Go writers
// stage under, pinned against Go by internal/gpulease/staging_drift_test.go) and a look at the name itself, taken after the look at the
// staged files, still finds nothing. While a fresh staged file stands beside it, the read waits for the rename to end, up to
// ABSENT_BUDGET_MS from the first look; when the budget runs out it returns null, which the fence reads as "not current" and aborts:
// loud and fail-safe (Node has no unreadable class, and it never grants a lease).
//
// The read, the pause, the clock, the stat and the list of staged files are injected, so the counts are exact and nothing sleeps.
import { test } from "node:test";
import assert from "node:assert/strict";
import * as lr from "./lease-read.mjs";

const { readSharedSync, policy } = lr;
const WALL = 1_700_000_000_000; // the wall clock of every test, ms
const fsError = (code, path = "x") => Object.assign(new Error(`${code}: open '${path}'`), { code, syscall: "open", path });

// A world of one lease file. `reads` is the script of read(path): a string is content, a { code } an error; the last repeats.
// `stats` maps a path to what stat(path) answers at that moment: undefined (absent), { mtimeMs } (there), or { throws: code }. A
// function value is called with the number of reads made so far, so a staged file can be there for three reads and gone after.
function world({ reads, stats = {}, tick = null, wall = WALL }) {
  const seen = { reads: 0, pauses: [], stats: [], clock: 0 };
  return {
    seen,
    opts: {
      read: (path) => {
        const step = reads[Math.min(seen.reads, reads.length - 1)];
        seen.reads += 1;
        if (seen.reads > 2000) throw new Error("runaway: more than 2000 reads");
        if (typeof step === "string") return step;
        throw fsError(step.code, path);
      },
      sleep: (ms) => { seen.pauses.push(ms); seen.clock += tick === null ? ms : tick; },
      now: () => seen.clock,
      wall: () => wall + seen.clock,
      stat: (path) => {
        seen.stats.push(path);
        let answer = stats[path];
        if (typeof answer === "function") answer = answer(seen.reads);
        if (answer && answer.throws) throw fsError(answer.throws, path);
        return answer;
      },
    },
  };
}

function withWait(t, value = true) {
  const old = policy.waitsOutDenied;
  policy.waitsOutDenied = value;
  t.after(() => { policy.waitsOutDenied = old; });
}

const FRESH = { mtimeMs: WALL };
const OLD = { mtimeMs: WALL - 10_000 };

test("the constants are the Go side's: a 250 ms budget, a 5 ms pause, a 3 s freshness, and the two staged suffixes", () => {
  assert.equal(lr.ABSENT_BUDGET_MS, 250);
  assert.equal(lr.ABSENT_PAUSE_MS, 5);
  assert.equal(lr.STAGING_FRESH_MS, 3000);
  assert.deepEqual(lr.STAGING_SUFFIXES, [".tmp", ".restamp"]);
});

test("a claim is never staged: nothing is looked for beside it, and nothing else but the one read is made", () => {
  assert.deepEqual(lr.stagingFor("/lease/cards/gpu-0.claim"), []);
  assert.deepEqual(lr.stagingFor("C:\\lease\\cards\\gpu-0.claim"), []);
  assert.deepEqual(lr.stagingFor("/lease/e/7.json"), ["/lease/e/7.json.tmp", "/lease/e/7.json.restamp"]);
  assert.deepEqual(lr.stagingFor("/lease/meta.json"), ["/lease/meta.json.tmp", "/lease/meta.json.restamp"]);
});

test("a fresh staged file beside an absent name is waited for: the rename ends and the new content is read", (t) => {
  withWait(t);
  const path = "/lease/e/7.json";
  const w = world({
    reads: [{ code: "ENOENT" }, { code: "ENOENT" }, { code: "ENOENT" }, "{\"epoch\":7}"],
    stats: { [path + ".tmp"]: (reads) => (reads < 4 ? FRESH : undefined) },
  });
  assert.equal(readSharedSync(path, w.opts), "{\"epoch\":7}", "a null here is a held lease called not current");
  assert.equal(w.seen.reads, 4, "three reads that met the rename, one after it");
  assert.deepEqual(w.seen.pauses, [5, 5, 5], "one pause between reads");
});

test("meta.json is waited for through its .restamp file", (t) => {
  withWait(t);
  const path = "/lease/meta.json";
  const w = world({
    reads: [{ code: "ENOENT" }, { code: "ENOENT" }, "{\"epoch\":5}"],
    stats: { [path + ".restamp"]: (reads) => (reads < 3 ? FRESH : undefined) },
  });
  assert.equal(readSharedSync(path, w.opts), "{\"epoch\":5}");
  assert.equal(w.seen.reads, 3);
});

test("a staged file dated in the future (a clock that stepped back) counts as fresh, and so does one that cannot be examined", (t) => {
  withWait(t);
  for (const [name, answer] of [["future", { mtimeMs: WALL + 3_600_000 }], ["EPERM", { throws: "EPERM" }]]) {
    const path = "/lease/hb.7";
    const w = world({ reads: [{ code: "ENOENT" }, "42"], stats: { [path + ".tmp"]: (reads) => (reads < 2 ? answer : undefined) } });
    assert.equal(readSharedSync(path, w.opts), "42", name);
    assert.equal(w.seen.reads, 2, name);
  }
});

test("a rename that never ends is given up on at the deadline, as null (the fence aborts: fail-safe), after about 250 ms", (t) => {
  withWait(t);
  const path = "/lease/e/7.json";
  const w = world({ reads: [{ code: "ENOENT" }], stats: { [path + ".tmp"]: FRESH } });
  assert.equal(readSharedSync(path, w.opts), null);
  assert.equal(w.seen.pauses.length, 50, "50 pauses of 5 ms");
  assert.equal(w.seen.reads, 51);
  assert.equal(w.seen.clock, 250);
});

test("the budget is a deadline, not a count of pauses: a timer that sleeps a whole 15.4 ms tick still gives up after about 250 ms", (t) => {
  withWait(t);
  const path = "/lease/e/7.json";
  const w = world({ reads: [{ code: "ENOENT" }], stats: { [path + ".tmp"]: FRESH }, tick: 15.4 });
  assert.equal(readSharedSync(path, w.opts), null);
  assert.equal(w.seen.pauses.length, 17, `${w.seen.pauses.length} pauses of 15.4 ms, want 17 (the first that reaches 250 ms ends it)`);
  assert.ok(w.seen.clock >= 250 && w.seen.clock < 250 + 16);
});

test("a name that is absent with nothing fresh beside it costs one read and three stats and no pause (a released lease)", (t) => {
  withWait(t);
  for (const [name, stats] of [["nothing beside it", {}], ["a staged file older than 3 s (a writer that died)", { "/lease/e/7.json.tmp": OLD, "/lease/e/7.json.restamp": OLD }]]) {
    const w = world({ reads: [{ code: "ENOENT" }], stats });
    assert.equal(readSharedSync("/lease/e/7.json", w.opts), null, name);
    assert.equal(w.seen.reads, 1, name);
    assert.deepEqual(w.seen.pauses, [], name);
    assert.deepEqual(w.seen.stats, ["/lease/e/7.json.tmp", "/lease/e/7.json.restamp", "/lease/e/7.json"], `${name}: both staged names, then the name itself`);
  }
});

test("a claim that is absent is one read and no stat at all", (t) => {
  withWait(t);
  const w = world({ reads: [{ code: "ENOENT" }], stats: {} });
  assert.equal(readSharedSync("/lease/cards/gpu-0.claim", w.opts), null);
  assert.equal(w.seen.reads, 1);
  assert.deepEqual(w.seen.stats, []);
  assert.deepEqual(w.seen.pauses, []);
});

test("the rename ends between the read and the look at the staged files: the name itself is looked at, found, and read again", (t) => {
  withWait(t);
  const path = "/lease/hb.7";
  const w = world({ reads: [{ code: "ENOENT" }, "42"], stats: { [path]: FRESH } });
  assert.equal(readSharedSync(path, w.opts), "42", "an answer of absent here is the miss at the tail of a rename");
  assert.equal(w.seen.reads, 2);
  assert.deepEqual(w.seen.pauses, [], "the name was already back: no pause");
});

test("a name the look finds but the read does not is read again only until the deadline", (t) => {
  withWait(t);
  const path = "/lease/hb.7";
  const w = world({ reads: [{ code: "ENOENT" }], stats: { [path]: FRESH } });
  assert.equal(readSharedSync(path, w.opts), null);
  assert.equal(w.seen.clock, 250, "bounded by the same deadline, not a spin");
});

test("off Windows an absent name is answered at once: one read, no stat, no pause (nothing there is mid-rename)", (t) => {
  withWait(t, false);
  const w = world({ reads: [{ code: "ENOENT" }], stats: { "/lease/hb.7.tmp": FRESH } });
  assert.equal(readSharedSync("/lease/hb.7", w.opts), null);
  assert.equal(w.seen.reads, 1);
  assert.deepEqual(w.seen.stats, []);
  assert.deepEqual(w.seen.pauses, []);
});
