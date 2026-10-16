// node --test render/lease-read-stress.test.mjs
//
// The stress half of lease-read.test.mjs: the real Node readers against a writer in another process that keeps replacing
// the lease files by a rename over the live name, as internal/gpulease does (write beside, rename over). A reader that
// answers null for a file that is there is a lease read as gone: the fence (checkInheritedLease) calls a held lease not
// current and the render aborts. No miss is allowed.
//
// The writer renames a hard link of a seed file over the live name instead of writing a fresh file each time. The rename
// is the same primitive (MoveFileEx with replace on Windows, rename(2) elsewhere); the link only makes the loop fast enough
// to put thousands of renames into a few seconds, where a fresh file costs a file creation (tens of milliseconds on a host
// with a scanner). The writer retries a refused rename like renameReplacing does, so a rename that lost to an open reader
// is tried again and not counted.
//
// Several lease roots are rotated so a reader holding one file open does not hold the whole loop up. The test is sized by WORK,
// not by the clock, so a loaded host (the gate hosts are) makes it longer and not vacuous: the writer makes MIN_RENAMES renames
// and stops only once the reader has made MIN_LOOPS loops (a marker file says so), and the reader loops until the writer says it
// is done; a cap of CAP_MS ends a run that cannot get there. The writer is a child that is killed by pid if the test ends early.
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
const nap = (ms) => new Promise((r) => setTimeout(r, ms));

// argv: root, roots, minRenames, capMs. Stops when it has made minRenames renames AND the reader has left <root>/enough, or at the cap.
// Writes <root>/done, then prints {renames, retries, fails, capped}.
const WRITER = `
const { linkSync, renameSync, unlinkSync, existsSync, writeFileSync } = require("node:fs");
const { join } = require("node:path");
const [root, rootsArg, minArg, capArg] = process.argv.slice(1);
const files = [];
for (let r = 1; r <= Number(rootsArg); r++) {
  const d = join(root, String(r));
  files.push(join(d, "e", "7.json"), join(d, "meta.json"), join(d, "cards", "gpu-test-0.claim"));
}
const pause = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
let renames = 0, retries = 0, fails = 0, capped = false;
const cap = Date.now() + Number(capArg);
for (;;) {
  if (renames >= Number(minArg) && existsSync(join(root, "enough"))) break;
  if (Date.now() >= cap) { capped = true; break; }
  for (const path of files) {
    linkSync(path + ".seed", path + ".tmp");
    let done = false;
    for (let attempt = 0; attempt < 400 && !done; attempt++) {
      try { renameSync(path + ".tmp", path); renames++; done = true; }
      catch (e) {
        if (e.code !== "EPERM" && e.code !== "EACCES" && e.code !== "EBUSY") throw e;
        retries++;
        pause(1);
      }
    }
    if (!done) { fails++; try { unlinkSync(path + ".tmp"); } catch {} }
  }
}
writeFileSync(join(root, "done"), "");
process.stdout.write(JSON.stringify({ renames, retries, fails, capped }) + "\\n");
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
  child = spawn(process.execPath, ["-e", WRITER, root, String(ROOTS), String(MIN_RENAMES), String(CAP_MS)], { stdio: ["ignore", "pipe", "inherit"], windowsHide: true });
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
  const w = JSON.parse(out.trim());
  t.diagnostic(`${loops} loops of the five readers (about ${loops * 12} file reads) against ${w.renames} renames (${w.retries} refused and retried, ${w.fails} given up): ${JSON.stringify(miss)}`);

  assert.equal(w.capped, false, `the writer hit the ${CAP_MS} ms cap with ${w.renames} of ${MIN_RENAMES} renames: the host is too slow for the test to prove anything`);
  assert.ok(w.renames >= MIN_RENAMES, `the writer made ${w.renames} renames, want ${MIN_RENAMES}`);
  assert.ok(loops >= MIN_LOOPS, `the readers made ${loops} loops, want ${MIN_LOOPS}`);
  assert.deepEqual(miss, { readEpochRecord: 0, readLease: 0, readCardClaim: 0, checkInheritedLease: 0, leaseProgressFile: 0 },
    `a reader answered null for a lease file that exists, in ${loops} loops against ${w.renames} renames`);
});
