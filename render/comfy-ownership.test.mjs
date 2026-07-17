import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { writeOwner, readOwner, isOwnedByUs, clearOwner } from "./comfy-ownership.mjs";

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
