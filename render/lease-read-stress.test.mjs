// node --test render/lease-read-stress.test.mjs
//
// The stress half of lease-read.test.mjs: the real Node readers against a writer in another process that keeps replacing
// the lease files by a rename over the live name, as internal/gpulease does for a record and the heartbeat (write beside,
// rename over). A reader that answers null for a file that is there is a lease read as gone: the fence (checkInheritedLease)
// calls a held lease not current and the render aborts. No miss is allowed. Go never renames over a card claim (it creates one
// once and deletes it); the claim is renamed over here as well, which is harder on its reader than production and keeps the
// reader in the loop.
//
// EVERY RENAME REPLACES. The writer links a seed file to <live>.tmp and renames that over the live name, a hard link in place of
// a fresh file so that the loop makes thousands of renames in seconds (a fresh file costs tens of milliseconds on a host with a
// scanner). A link is only a stand-in for a fresh file while it is a DIFFERENT inode from the one in place: renaming a link over
// another name of the same inode replaces nothing. On POSIX rename(2) then does nothing at all and leaves <live>.tmp where it was
// (the next link of it fails with EEXIST); on Windows it succeeds even over a name a reader holds open, so no reader ever meets a
// refusal. A first form linked ONE seed: after the first pass the live name and the seed were the same inode, only that pass
// replaced anything (9 of 4,761 renames on Windows), the test proved nothing there, and on Linux it died on the second pass.
// So each file has two seeds (<live>.seed and <live>.seed2, two inodes, one body) and the writer alternates them, which leaves the
// file in place never the inode being installed. The writer checks that against the inodes themselves and reports `replaced`.
//
// WHAT RENAMING OVER A READER DOES. Windows: a rename over a name a reader has open is refused (EPERM, EACCES or EBUSY), and the
// writer retries it as renameReplacing does; `retries` counts those, and the test requires some, so the readers and the rename
// really did meet (the writer keeps going until the first one, within the cap). Elsewhere rename(2) is atomic and is never
// refused, so `retries` must be 0. A run that would not have met a reader fails instead of passing.
//
// Several lease roots are rotated so a reader holding one file open does not hold the whole loop up. The test is sized by WORK,
// not by the clock, so a loaded host (the gate hosts are) makes it longer and not vacuous: the writer makes MIN_RENAMES renames
// and stops only once the reader has made MIN_LOOPS loops (a marker file says so), and the reader loops until the writer says it
// is done; a cap of CAP_MS ends a run that cannot get there. The writer is a child that is killed by pid if the test ends early.
// A writer that throws still writes `done` and reports the error, so the test fails at once with the cause and not at the cap.
//
// MEASURED (a 36-thread Windows host; the same loop, 40 s per run, with 4 reader processes and a Go or a Node writer, the
// readers as they were before lease-read.mjs and as they are now) is in the change fragment. On a quiet host the plain
// readFileSync missed nothing, so this test is a guard on the fixed readers and not a red on the old ones.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { awaitTreeGone, killTree, scratchDir } from "./test-scratch.mjs";
import { checkInheritedLease, readCardClaim, readEpochRecord, readLease } from "./gpu-lock.mjs";
import { leaseProgressFile } from "./lease-progress.mjs";

const ROOTS = 6;
const MIN_RENAMES = 600;
const MIN_LOOPS = 150;
const CAP_MS = 60_000;
const WINDOWS = process.platform === "win32";
const nap = (ms) => new Promise((r) => setTimeout(r, ms));

// argv: root, roots, minRenames, capMs, minRetries. Stops when it has made minRenames renames AND met minRetries refusals AND the reader
// has left <root>/enough, or at the cap. Writes <root>/done, then prints {renames, replaced, retries, linkRetries, fails, capped, crashed}.
const WRITER = `
const { linkSync, renameSync, unlinkSync, existsSync, statSync, writeFileSync } = require("node:fs");
const { join } = require("node:path");
const [root, rootsArg, minArg, capArg, needRetriesArg] = process.argv.slice(1);
const files = [];
for (let r = 1; r <= Number(rootsArg); r++) {
  const d = join(root, String(r));
  files.push(join(d, "e", "7.json"), join(d, "meta.json"), join(d, "cards", "gpu-test-0.claim"));
}
const SEEDS = [".seed", ".seed2"];
const ino = (p) => statSync(p, { bigint: true }).ino;
const seedIno = new Map(files.map((p) => [p, SEEDS.map((s) => ino(p + s))]));
const inPlace = new Map(files.map((p) => [p, ino(p)]));
const side = new Map(files.map((p) => [p, 0]));
const pause = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const stats = { renames: 0, replaced: 0, retries: 0, linkRetries: 0, fails: 0, capped: false, crashed: null };
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
  const cap = Date.now() + Number(capArg);
  for (;;) {
    if (stats.renames >= Number(minArg) && stats.retries >= Number(needRetriesArg) && existsSync(join(root, "enough"))) break;
    if (Date.now() >= cap) { stats.capped = true; break; }
    for (const path of files) {
      const tmp = path + ".tmp";
      const i = side.get(path);
      if (!unrefused(() => linkSync(path + SEEDS[i], tmp), "linkRetries")) { stats.fails++; continue; }
      if (!unrefused(() => renameSync(tmp, path), "retries")) {
        stats.fails++;
        try { unlinkSync(tmp); } catch {}
        continue;
      }
      stats.renames++;
      if (seedIno.get(path)[i] !== inPlace.get(path)) stats.replaced++;
      inPlace.set(path, seedIno.get(path)[i]);
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
  child = spawn(process.execPath, ["-e", WRITER, root, String(ROOTS), String(MIN_RENAMES), String(CAP_MS), String(minRetries)], { stdio: ["ignore", "pipe", "inherit"], windowsHide: true });
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
  t.diagnostic(`${loops} loops of the five readers (about ${loops * 12} file reads) against ${w.renames} renames (${w.replaced} replaced a different inode than the one in place, ${w.retries} refused and retried, ${w.linkRetries} link refusals, ${w.fails} given up): ${JSON.stringify(miss)}`);

  assert.equal(w.crashed, null, `the writer stopped on an error after ${w.renames} renames: ${w.crashed}`);
  assert.equal(w.replaced, w.renames, `only ${w.replaced} of ${w.renames} renames installed a different inode than the one in place: a rename of a link over its own inode replaces nothing and no reader can be refused by it`);
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
