// test-scratch.mjs - the one way a render test makes a throwaway directory, and the one way it stops a
// process tree that runs in one. Test support only; never imported by a runner.
//
// A test that makes its own `mkdtempSync(join(tmpdir(), ...))` and leaves the removal to a trailing
// `rmSync` loses the folder whenever an assertion throws first, and a test that has no `rmSync` loses
// it every time: one run of `node --test render/*.test.mjs` left ~150 folders in the OS temp dir, and
// a workstation that runs the suite all day collected thousands of them on the system drive.
// scratchDir ties the removal to the test that asked for the folder, so it goes when that test ends,
// passed or failed (node:test runs a test's `after` hooks either way).
//
//   test("...", (t) => {
//     const dir = scratchDir(t, "my-feature-");
//     ...
//   });
//
// A test that starts a process which keeps files of that folder open (a detached stub server, say)
// hands scratchDir the teardown that stops it. The teardown runs first, and the folder is removed
// after it whether it threw or not (a running process holds its files open on Windows, and
// node:test runs hooks in the order they were registered, so a second hook could not be put ahead):
//
//   const dir = scratchDir(t, "my-feature-", { teardown: async () => { await awaitTreeGone(killTree(pid)); } });
//
// A helper that builds a fixture takes `t` and passes it on (`fixture(t, ...)`). Which raw
// `mkdtemp` calls may remain in a test file, and why, is listed in test-scratch.test.mjs.
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { descendantsOf, pidGone, processTable } from "./igpu-engine.mjs";

const sleepSync = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// The codes a Windows folder removal reports while a scanner, or a process that is still exiting,
// holds a handle on something in it. They clear by themselves; any other code is a real fault.
const TRANSIENT = new Set(["EPERM", "EBUSY", "ENOTEMPTY"]);

// removeTree: remove a folder and everything in it. A transient failure (EPERM, EBUSY, ENOTEMPTY)
// is retried with a doubling back-off, 50 ms first and 1 s at most per step, until about 15 s have
// passed since the first attempt: a tree teardown on a starved Windows box can hold handles that
// long. It throws when the folder still cannot go, and at once on any other error: a folder a test
// cannot delete is a defect to see, not to hide. A folder that is already gone is not an error.
// `rm`, `sleep` and `now` are seams for the test of the back-off itself.
export function removeTree(dir, { rm = rmSync, sleep: nap = sleepSync, now = Date.now, totalMs = 15000, firstMs = 50, maxStepMs = 1000 } = {}) {
  const t0 = now();
  let step = firstMs;
  for (;;) {
    try {
      rm(dir, { recursive: true, force: true });
      return;
    } catch (e) {
      const left = totalMs - (now() - t0);
      if (!e || !TRANSIENT.has(e.code) || left <= 0) throw e;
      nap(Math.min(step, left));
      step = Math.min(step * 2, maxStepMs);
    }
  }
}

// scratchDir: a new folder under the OS temp dir named `<prefix><random>`, removed with its contents
// when test `t` ends. `t` is the TestContext node:test hands the test (or a subtest) callback; a call
// without it is refused BEFORE the folder is made, so forgetting it cannot leave a folder behind.
// `teardown` (optional, sync or async) runs before the removal, and the removal runs even if it throws.
export function scratchDir(t, prefix, { teardown } = {}) {
  if (!t || typeof t.after !== "function") {
    throw new TypeError("scratchDir(t, prefix): t must be the TestContext node:test passes to the test callback (a helper that makes a fixture takes it as its first argument)");
  }
  const dir = mkdtempSync(join(tmpdir(), prefix));
  t.after(async () => {
    try {
      if (teardown) await teardown();
    } finally {
      removeTree(dir);
    }
  });
  return dir;
}

// killTree: kill `pid` and everything it started, and return every pid it took down (the tree as it
// was before the kill; hand it to awaitTreeGone). Windows: taskkill /T names each process it ends
// ("... PID 123 (child process of PID 456) ..."), which is read from its output in any language.
// Elsewhere the descendants are listed first (a child that loses its parent is re-parented and could
// not be found afterwards) with the same descendantsOf the runners use, then killed deepest first.
// Best-effort, never throws.
export function killTree(pid, { platform = process.platform } = {}) {
  const pids = new Set([pid]);
  try {
    if (platform === "win32") {
      const r = spawnSync("taskkill", ["/T", "/F", "/PID", String(pid)], { windowsHide: true, encoding: "utf8" });
      // the FIRST pid on a line is the process ended; a second one names its parent, which is still alive
      for (const line of `${r.stdout || ""}\n${r.stderr || ""}`.split(/\r?\n/)) {
        const m = /PID\s+(\d+)/i.exec(line);
        if (m) pids.add(Number(m[1]));
      }
    } else {
      const kids = descendantsOf(pid, processTable());
      for (const p of kids) pids.add(p);
      for (const p of [...kids, pid]) {
        try { process.kill(p, "SIGKILL"); } catch { /* already gone */ }
      }
    }
  } catch { /* nothing more to do */ }
  return [...pids];
}

// awaitTreeGone: wait until every pid in `pids` (a pid, or the list killTree returned) is gone, a
// zombie counting as gone, polling every `pollMs` for at most `timeoutMs`. taskkill returns before
// Windows has released everything the tree held, and on a starved box that gap is seconds: removing
// the tree's folder in it fails with EPERM. Prints one line when the bound passes; returns
// {gone, waitedMs, alive}, never throws (the removal that follows is what fails loudly).
export async function awaitTreeGone(pids, { timeoutMs = 10000, pollMs = 50, isGone = pidGone, now = Date.now, nap = sleep, log = (s) => process.stderr.write(s + "\n") } = {}) {
  const t0 = now();
  let alive = [].concat(pids).filter((p) => !isGone(p));
  while (alive.length > 0 && now() - t0 < timeoutMs) {
    await nap(pollMs);
    alive = alive.filter((p) => !isGone(p));
  }
  const waitedMs = now() - t0;
  if (alive.length > 0) log(`awaitTreeGone: pid(s) ${alive.join(", ")} still alive after ${waitedMs} ms (bound ${timeoutMs} ms)`);
  return { gone: alive.length === 0, waitedMs, alive };
}
