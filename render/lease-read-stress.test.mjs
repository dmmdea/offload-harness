// node --test render/lease-read-stress.test.mjs
//
// The stress half of lease-read.test.mjs: the real Node readers against a writer in another process that keeps replacing
// the lease files by a rename over the live name, as internal/gpulease does for a record and the heartbeat (write beside,
// rename over). A reader that answers null for a file that is there is a lease read as gone: the fence (checkInheritedLease)
// calls a held lease not current and the render aborts. The test wants no miss. Go never renames over a card claim (it creates one
// once and deletes it); the claim is renamed over here as well, which is harder on its reader than production and keeps the
// reader in the loop.
//
// WHAT IT PINS. One thing: at its size (600 renames over 18 files, 150 loops of the five readers; LONG MODE below multiplies it),
// no reader answers null for a file that is there while a writer replaces it by rename-over. Plain readFileSync missed nothing
// either, so this test is a guard against the readers starting to miss and is not a red on the old ones.
//
// WHAT IT DOES NOT PIN. None of the waits in lease-read.mjs. Node opens with FILE_SHARE_DELETE, so a rename over a name a reader has
// open does not refuse that reader, and the refusals counted below are the writer's. With the wait for a refused open taken out (REFUSALS
// empty, so a refusal reads as absent at once) this test still passed 5 of 5 on Windows. The proofs are the tests that put a reader in
// the state and count what it does:
//   - the wait for a refused open (EPERM, EACCES): the injected-refusal tests in lease-read.test.mjs, gpu-lock.test.mjs and
//     lease-progress.test.mjs, which went red, 10 of 81, with the wait out;
//   - the wait for an absent name with a fresh staged file beside it (the ENOENT of a rename that was not refused, 11 to 75 ms as a
//     reader sees it, about once in 74,000 renames on the host that showed it): lease-read-gate.test.mjs here, and
//     internal/gpulease/gate_test.go for the Go readers;
//   - the reclaim rule, under which a lease whose heartbeat cannot be read is never made stale by it: heartbeat_unknown_test.go,
//     keeper_unknown_test.go and tree_marker_unknown_test.go in internal/gpulease.
// A run of this test cannot be counted on to meet either state naturally. The Go reader met the delete-pending refusal 5 times in
// about 124,000 reads of a heartbeat renamed in a loop on a loaded host, Node not once in about a million reads, and removing a
// record or a claim leaves that state too, which this loop never does. The Go control for it is readretry_windows_test.go (a real
// delete-pending file refuses even an open that shares delete); for Node it is the injected-refusal tests, and the Node probe of a
// held delete-pending file is a measurement in the change fragment, not a test.
//
// WHERE IT RUNS. CI runs this file on Linux only (the build job's `node --test render/*.test.mjs` is on ubuntu-latest), so the
// Windows assertions below (some of the writer's renames refused) run on the gated hosts and the workstations only.
//
// EVERY RENAME REPLACES. The writer links a seed file to <live>.tmp and renames that over the live name, a hard link in place of
// a fresh file so that the loop makes thousands of renames in seconds (a fresh file costs tens of milliseconds on a host with a
// scanner). A link is only a stand-in for a fresh file while it is a DIFFERENT inode from the one in place: renaming a link over
// another name of the same inode replaces nothing. On POSIX rename(2) then does nothing at all and leaves <live>.tmp where it was
// (the next link of it fails with EEXIST); on Windows it succeeds even over a name a reader holds open, so no reader ever meets a
// refusal. A first form linked ONE seed: after the first pass the live name and the seed were the same inode and only that pass
// replaced anything (18 of 11,052 renames at the six roots below; a reviewer's run counted 9 of 4,761, nine being one replacement
// for each of nine files), so the test proved nothing there, and on Linux the writer died at the link of the third pass
// (EEXIST, 36 renames in). So each file has two seeds (<live>.seed and <live>.seed2, two inodes, one body) and the writer
// alternates them, which leaves the file in place never the inode being installed. The writer OBSERVES it: after each rename it
// takes the inode of the live name, and counts a replacement only when that is the seed it just installed and not the inode it saw in
// place before. It reports the count as `replaced`.
//
// WHAT RENAMING OVER A READER DOES. Windows: a rename over a name a reader has open is refused (EPERM, EACCES or EBUSY), and the
// writer retries it as renameReplacing does; `retries` counts those, and the test requires some, so the writer and the readers
// really met (the writer keeps going until the first one, within the cap). Elsewhere rename(2) is atomic and is never refused, so
// `retries` must be 0. A run that would not have met a reader fails instead of passing.
//
// Several lease roots are rotated so a reader holding one file open does not hold the whole loop up. The test is sized by WORK,
// not by the clock, so a loaded host (the gate hosts are) makes it longer and not vacuous: the writer makes MIN_RENAMES renames
// and stops only once the reader has made MIN_LOOPS loops (a marker file says so), and the reader loops until the writer says it
// is done; a cap of CAP_MS ends a run that cannot get there. The writer is a child that is killed by pid if the test ends early.
// A writer that throws, in its setup as well as its loop, still writes `done` and reports the error, so the test fails at once with
// the cause and not at the cap.
//
// LONG MODE. LEASE_READ_STRESS_SCALE=<n> (a whole number, default 1) multiplies the renames, the loops and the cap by n, for a gated
// host that wants a longer run. Unset, the committed size runs, which is small and fast.
//
// MEASURED (a 36-thread Windows host; the same loop, 40 s per run, with 4 reader processes and a Go or a Node writer, the
// readers as they were before lease-read.mjs and as they are now) is in the change fragment.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdirSync, utimesSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { awaitTreeGone, killTree, scratchDir } from "./test-scratch.mjs";
import { checkInheritedLease, readCardClaim, readEpochRecord, readLease } from "./gpu-lock.mjs";
import { leaseProgressFile } from "./lease-progress.mjs";

const ROOTS = 6;
const SCALE = Math.max(1, Math.floor(Number(process.env.LEASE_READ_STRESS_SCALE)) || 1); // long mode (see the header)
const MIN_RENAMES = 600 * SCALE;
const MIN_LOOPS = 150 * SCALE;
const CAP_MS = 60_000 * SCALE;
// File reads one loop of the five readers makes: readEpochRecord 1 (e/7.json), readLease 1 (meta.json), readCardClaim 1 (the claim),
// checkInheritedLease 2 (the record and the one claim it names), leaseProgressFile 1 (the record, which names the progress file).
const READS_PER_LOOP = 6;
// A staged file older than this, beside an absent name, is a dead writer's leftover to the reader (lease-read.mjs counts one fresh for
// 3 s), and not a rename in flight. A writer whose staged file is old when it renames is not the writer of a lease file.
const STAGED_FRESH_MS = 2000;
const WINDOWS = process.platform === "win32";
const nap = (ms) => new Promise((r) => setTimeout(r, ms));

// argv: root, roots, minRenames, capMs, minRetries. Stops when it has made minRenames renames AND met minRetries refusals AND the reader
// has left <root>/enough, or at the cap. Writes <root>/done, then prints {renames, replaced, absentAfter, retries, linkRetries, fails,
// capped, crashed}. `replaced` is what it OBSERVED: renames after which the live name has the inode just installed and not the one seen
// in place before. `absentAfter` is how often the live name was absent when it looked right after its own successful rename (the
// episode of the read gate, seen from the writer; it looks again until the name is back).
const WRITER = `
const { linkSync, renameSync, unlinkSync, existsSync, statSync, writeFileSync } = require("node:fs");
const { join } = require("node:path");
const [root, rootsArg, minArg, capArg, needRetriesArg, freshArg] = process.argv.slice(1);
const files = [];
for (let r = 1; r <= Number(rootsArg); r++) {
  const d = join(root, String(r));
  files.push(join(d, "e", "7.json"), join(d, "meta.json"), join(d, "cards", "gpu-test-0.claim"));
}
const SEEDS = [".seed", ".seed2"];
const ino = (p) => statSync(p, { bigint: true }).ino;
const pause = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const stats = { renames: 0, replaced: 0, absentAfter: 0, staleStaged: 0, retries: 0, linkRetries: 0, fails: 0, capped: false, crashed: null };
const STAGED_FRESH_MS = Number(freshArg);
// The inode of the live name, looked at right after the writer's own rename. The name can be absent for tens of milliseconds after
// a rename that was not refused (the episode the read gate waits for), so an ENOENT is looked at again, counted once, and only a name
// that stays absent for 5 s is an error. The 5 s is a DEADLINE and not a count of pauses: Atomics.wait(1) sleeps a whole timer tick on
// Windows (15.4 ms), so a count of 5000 pauses would be over a minute.
function observe(path) {
  const until = Date.now() + 5000;
  let first = true;
  for (;;) {
    try { return ino(path); }
    catch (e) {
      if (e.code !== "ENOENT") throw e;
      if (first) { stats.absentAfter++; first = false; }
      if (Date.now() >= until) throw new Error("the live name " + path + " stayed absent for 5 s after a rename");
      pause(1);
    }
  }
}
// Run op until it is not refused (EPERM, EACCES, EBUSY: a reader holds the name); each refusal is counted under counter. Any other
// error is real and ends the writer. True when op ran.
function unrefused(op, counter) {
  for (let attempt = 0; attempt < 400; attempt++) {
    try { op(); return true; }
    catch (e) {
      if (e.code !== "EPERM" && e.code !== "EACCES" && e.code !== "EBUSY") throw e;
      stats[counter]++;
      pause(1);
    }
  }
  return false;
}
try {
  const seedIno = new Map(files.map((p) => [p, SEEDS.map((s) => ino(p + s))]));
  const inPlace = new Map(files.map((p) => [p, ino(p)])); // the inode last SEEN in place, not the one expected there
  const side = new Map(files.map((p) => [p, 0]));
  const cap = Date.now() + Number(capArg);
  for (;;) {
    if (stats.renames >= Number(minArg) && stats.retries >= Number(needRetriesArg) && existsSync(join(root, "enough"))) break;
    if (Date.now() >= cap) { stats.capped = true; break; }
    for (const path of files) {
      const tmp = path + ".tmp";
      const i = side.get(path);
      if (!unrefused(() => linkSync(path + SEEDS[i], tmp), "linkRetries")) { stats.fails++; continue; }
      if (Date.now() - statSync(tmp).mtimeMs > STAGED_FRESH_MS) stats.staleStaged++;
      if (!unrefused(() => renameSync(tmp, path), "retries")) {
        stats.fails++;
        try { unlinkSync(tmp); } catch {}
        continue;
      }
      stats.renames++;
      const seen = observe(path);
      if (seen === seedIno.get(path)[i] && seen !== inPlace.get(path)) stats.replaced++;
      inPlace.set(path, seen);
      side.set(path, 1 - i);
    }
  }
} catch (e) {
  stats.crashed = String(e && e.code ? e.code + " " : "") + String(e && e.message);
}
writeFileSync(join(root, "done"), "");
process.stdout.write(JSON.stringify(stats) + "\\n");
`;

function seedRoot(dir) {
  mkdirSync(join(dir, "e"), { recursive: true });
  mkdirSync(join(dir, "cards"), { recursive: true });
  const rec = JSON.stringify({
    epoch: 7, class: "media", holder: { pid: 1 }, devices: ["gpu-test-0"], state: "active",
    progress: { file: join(dir, "progress.log") }, note: "x".repeat(300),
  });
  const claim = JSON.stringify({ epoch: 7, at_ms: 1 });
  for (const [path, body] of [[join(dir, "e", "7.json"), rec], [join(dir, "meta.json"), rec], [join(dir, "cards", "gpu-test-0.claim"), claim]]) {
    writeFileSync(path, body);
    writeFileSync(path + ".seed", body);
    writeFileSync(path + ".seed2", body);
    // The seeds are an hour old when the writer starts, so a staged file that is a link of one is old unless the writer made it fresh.
    const old = new Date(Date.now() - 3600_000);
    for (const seed of [".seed", ".seed2"]) utimesSync(path + seed, old, old);
  }
}

test("the readers never answer null for a lease file that is there while a writer replaces it by rename-over in a loop", async (t) => {
  let child = null;
  const root = scratchDir(t, "lease-read-stress-", {
    teardown: async () => { if (child && child.exitCode === null) await awaitTreeGone(killTree(child.pid)); },
  });
  const dirs = [];
  for (let r = 1; r <= ROOTS; r++) {
    const d = join(root, String(r));
    seedRoot(d);
    dirs.push(d);
  }
  const minRetries = WINDOWS ? 1 : 0;
  child = spawn(process.execPath, ["-e", WRITER, root, String(ROOTS), String(MIN_RENAMES), String(CAP_MS), String(minRetries), String(STAGED_FRESH_MS)], { stdio: ["ignore", "pipe", "inherit"], windowsHide: true });
  let out = "";
  child.stdout.on("data", (d) => { out += d; });
  const closed = new Promise((resolve) => child.on("close", resolve));
  await nap(250); // the writer is up

  const miss = { readEpochRecord: 0, readLease: 0, readCardClaim: 0, checkInheritedLease: 0, leaseProgressFile: 0 };
  let loops = 0;
  let enough = false;
  const cap = Date.now() + CAP_MS;
  while (Date.now() < cap) {
    loops += 1;
    const dir = dirs[loops % dirs.length];
    if (!readEpochRecord(dir, 7)) miss.readEpochRecord += 1;
    if (!readLease(dir)) miss.readLease += 1;
    if (!readCardClaim(dir, "gpu-test-0")) miss.readCardClaim += 1;
    if (!checkInheritedLease({ dir, epoch: 7 })) miss.checkInheritedLease += 1;
    if (!leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "7" })) miss.leaseProgressFile += 1;
    if (!enough && loops >= MIN_LOOPS) {
      writeFileSync(join(root, "enough"), "");
      enough = true;
    }
    if (enough && existsSync(join(root, "done"))) break;
  }
  await closed;
  assert.ok(out.trim(), "the writer printed nothing: it never ran (its own stderr is above)");
  const w = JSON.parse(out.trim());
  t.diagnostic(`${loops} loops of the five readers (about ${loops * READS_PER_LOOP} file reads) against ${w.renames} renames (${w.replaced} observed to replace a different inode than the one in place, ${w.retries} refused and retried, ${w.linkRetries} link refusals, ${w.absentAfter} met the live name absent right after the writer's own rename, ${w.staleStaged} staged files older than the reader's freshness when renamed, ${w.fails} given up): ${JSON.stringify(miss)}`);

  assert.equal(w.crashed, null, `the writer stopped on an error after ${w.renames} renames: ${w.crashed}`);
  assert.equal(w.staleStaged, 0, `${w.staleStaged} of ${w.renames} renames installed a staged file that was already older than ${STAGED_FRESH_MS} ms: a reader counts a staged file that old beside an absent name as a dead writer's leftover and answers "not there", so a run past 3 s would read its own writer's rename in flight as a lease that is gone`);
  assert.equal(w.replaced, w.renames, `only ${w.replaced} of ${w.renames} renames were observed to put a different inode than the one in place under the live name: a rename of a link over its own inode replaces nothing and no reader can be refused by it`);
  if (WINDOWS) {
    assert.ok(w.retries > 0, `no rename was refused in ${w.renames} renames against ${loops} reader loops (capped: ${w.capped}): the writer and the readers never met, so this run proves nothing`);
  } else {
    assert.equal(w.retries + w.linkRetries, 0, `${w.retries} renames and ${w.linkRetries} links were refused off Windows, where rename(2) is atomic and is never refused by a reader`);
  }
  assert.equal(w.capped, false, `the writer hit the ${CAP_MS} ms cap with ${w.renames} of ${MIN_RENAMES} renames: the host is too slow for the test to prove anything`);
  assert.ok(w.renames >= MIN_RENAMES, `the writer made ${w.renames} renames, want ${MIN_RENAMES}`);
  assert.ok(loops >= MIN_LOOPS, `the readers made ${loops} loops, want ${MIN_LOOPS}`);
  assert.deepEqual(miss, { readEpochRecord: 0, readLease: 0, readCardClaim: 0, checkInheritedLease: 0, leaseProgressFile: 0 },
    `a reader answered null for a lease file that exists, in ${loops} loops against ${w.renames} renames`);
});
