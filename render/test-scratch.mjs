// test-scratch.mjs - the one way a render test makes a throwaway directory. Test support only; never
// imported by a runner.
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
// A helper that builds a fixture takes `t` and passes it on (`fixture(t, ...)`). Which raw
// `mkdtemp` calls may remain in a test file, and why, is listed in test-scratch.test.mjs.
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// removeTree: remove a folder and everything in it. On Windows a scanner or a child that is still
// exiting can hold a handle for a moment, so it retries (linear back-off, about 2.5 s in all). It
// throws when the folder still cannot go: a folder a test cannot delete is a defect to see, not to hide.
export function removeTree(dir) {
  rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
}

// scratchDir: a new folder under the OS temp dir named `<prefix><random>`, removed with its contents
// when test `t` ends. `t` is the TestContext node:test hands the test (or a subtest) callback; a call
// without it is refused BEFORE the folder is made, so forgetting it cannot leave a folder behind.
export function scratchDir(t, prefix) {
  if (!t || typeof t.after !== "function") {
    throw new TypeError("scratchDir(t, prefix): t must be the TestContext node:test passes to the test callback (a helper that makes a fixture takes it as its first argument)");
  }
  const dir = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => removeTree(dir));
  return dir;
}
