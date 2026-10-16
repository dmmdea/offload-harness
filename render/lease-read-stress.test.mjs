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
// Several lease roots are rotated so a reader holding one file open does not hold the whole loop up. The test is bounded
// (a few seconds) and its writer is a child that is killed by pid if the test ends early.
//
// MEASURED (a 36-thread Windows host; the same loop, 40 s per run, with 4 reader processes and a Go or a Node writer, the
// readers as they were before lease-read.mjs and as they are now) is in the change fragment. On a quiet host the plain
// readFileSync missed nothing, so this test is a guard on the fixed readers and not a red on the old ones.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { awaitTreeGone, killTree, scratchDir } from "./test-scratch.mjs";
import { checkInheritedLease, readCardClaim, readEpochRecord, readLease } from "./gpu-lock.mjs";
import { leaseProgressFile } from "./lease-progress.mjs";

const ROOTS = 6;
const RUN_MS = 3000;
const nap = (ms) => new Promise((r) => setTimeout(r, ms));

// argv: root, roots, ms. Prints {renames, retries, fails} when the time is up.
const WRITER = `
const { linkSync, renameSync, unlinkSync } = require("node:fs");
const { join } = require("node:path");
const [root, rootsArg, msArg] = process.argv.slice(1);
const files = [];
for (let r = 1; r <= Number(rootsArg); r++) {
  const d = join(root, String(r));
  files.push(join(d, "e", "7.json"), join(d, "meta.json"), join(d, "cards", "gpu-test-0.claim"));
}
const pause = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
let renames = 0, retries = 0, fails = 0;
const end = Date.now() + Number(msArg);
while (Date.now() < end) {
  for (const path of files) {
    if (Date.now() >= end) break;
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
process.stdout.write(JSON.stringify({ renames, retries, fails }) + "\\n");
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
  child = spawn(process.execPath, ["-e", WRITER, root, String(ROOTS), String(RUN_MS)], { stdio: ["ignore", "pipe", "inherit"], windowsHide: true });
  let out = "";
  child.stdout.on("data", (d) => { out += d; });
  const closed = new Promise((resolve) => child.on("close", resolve));
  await nap(250); // the writer is up

  const miss = { readEpochRecord: 0, readLease: 0, readCardClaim: 0, checkInheritedLease: 0, leaseProgressFile: 0 };
  let loops = 0;
  const stop = Date.now() + RUN_MS - 500;
  while (Date.now() < stop) {
    loops += 1;
    const dir = dirs[loops % dirs.length];
    if (!readEpochRecord(dir, 7)) miss.readEpochRecord += 1;
    if (!readLease(dir)) miss.readLease += 1;
    if (!readCardClaim(dir, "gpu-test-0")) miss.readCardClaim += 1;
    if (!checkInheritedLease({ dir, epoch: 7 })) miss.checkInheritedLease += 1;
    if (!leaseProgressFile({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: "7" })) miss.leaseProgressFile += 1;
  }
  await closed;
  const w = JSON.parse(out.trim());
  t.diagnostic(`${loops} loops of the five readers (about ${loops * 12} file reads) against ${w.renames} renames (${w.retries} refused and retried, ${w.fails} given up): ${JSON.stringify(miss)}`);

  assert.ok(w.renames >= 20, `the writer made ${w.renames} renames in ${RUN_MS} ms: the test proved nothing`);
  assert.ok(loops >= 100, `the readers made ${loops} loops: the test proved nothing`);
  assert.deepEqual(miss, { readEpochRecord: 0, readLease: 0, readCardClaim: 0, checkInheritedLease: 0, leaseProgressFile: 0 },
    `a reader answered null for a lease file that exists, in ${loops} loops against ${w.renames} renames`);
});
