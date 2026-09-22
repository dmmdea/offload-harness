import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { writeOwner, readOwner, isOwnedByUs, clearOwner, writeLaunchOwner, readLaunchOwner, clearLaunchOwner, harnessLaunched, sameArgv } from "./comfy-ownership.mjs";

test("launch marker: round-trip, separate from the run-graph provisioning marker", () => {
  const dir = mkdtempSync(join(tmpdir(), "own-"));
  assert.equal(readLaunchOwner(dir), null);
  writeOwner(dir, { manifestHash: "h" });
  writeLaunchOwner(dir, { pid: 12, ownerPid: 34, args: ["main.py", "--cuda-device", "1"], profile: { cudaDevice: "1", dynamicVram: "on" } });
  assert.equal(readOwner(dir).manifestHash, "h", "run-graph's cache key survives a launch record");
  const m = readLaunchOwner(dir);
  assert.equal(m.pid, 12);
  assert.equal(m.ownerPid, 34);
  assert.deepEqual(m.args, ["main.py", "--cuda-device", "1"]);
  clearLaunchOwner(dir);
  assert.equal(readLaunchOwner(dir), null);
  assert.equal(readOwner(dir).manifestHash, "h");
});

test("harnessLaunched is a fingerprint: live pid AND the exact argv", () => {
  const m = { pid: 12, args: ["main.py", "--cuda-device", "1"] };
  const alive = (pid) => pid === 12;
  assert.equal(harnessLaunched(m, ["main.py", "--cuda-device", "1"], alive), true);
  assert.equal(harnessLaunched(m, ["main.py", "--cuda-device", "2"], alive), false, "another argv = another server");
  assert.equal(harnessLaunched(m, ["main.py", "--cuda-device", "1"], () => false), false, "dead pid = stale marker");
  assert.equal(harnessLaunched(null, ["main.py"], alive), false);
  assert.equal(harnessLaunched(m, null, alive), false);
  assert.equal(sameArgv(["a", "b"], ["a"]), false);
});

test("write/read/clear round-trip + live-pid ownership", () => {
  const dir = mkdtempSync(join(tmpdir(), "own-"));
  assert.equal(readOwner(dir), null);
  assert.equal(isOwnedByUs(dir), false);
  writeOwner(dir, { pid: process.pid, manifestHash: "abc" });
  assert.equal(readOwner(dir).manifestHash, "abc");
  assert.equal(isOwnedByUs(dir), true);              // our own live pid
  clearOwner(dir);
  assert.equal(readOwner(dir), null);
});

test("isOwnedByUs is false for a dead pid", () => {
  const dir = mkdtempSync(join(tmpdir(), "own-"));
  writeOwner(dir, { pid: 999999999, manifestHash: "x" });  // not a live pid
  assert.equal(isOwnedByUs(dir), false);
});
