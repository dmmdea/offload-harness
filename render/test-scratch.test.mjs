// node --test render/test-scratch.test.mjs
//
// The hygiene of the render suite itself: a run of `node --test render/*.test.mjs` leaves nothing in the OS
// temp dir. One run used to leave ~150 folders (a workstation that ran it all day collected thousands, on the
// system drive), because each test made its folder with a bare mkdtempSync and either never removed it or
// removed it on the success path only.
//
//   1. scratchDir (test-scratch.mjs) removes the folder when its test ends, passed or failed.
//   2. The test support that builds things on disk (the Windows stub launcher of igpu-stub.mjs) leaves nothing.
//   3. A render test (or the test-support modules it calls) may not make a temp folder of its own: the raw
//      mkdtemp calls that remain are listed below with the reason each is removed on every path. A new one
//      fails here until it uses scratchDir.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { removeTree, scratchDir } from "./test-scratch.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const SELF = basename(fileURLToPath(import.meta.url));

// a node child that inherits nothing of the test runner's context, with a private OS temp dir
function childEnv(priv) {
  const env = { ...process.env, TEMP: priv, TMP: priv, TMPDIR: priv };
  delete env.NODE_TEST_CONTEXT; // left in, a nested `node --test` would behave as a child of this run
  return env;
}

// ---- 1. scratchDir -------------------------------------------------------------------------------

test("scratchDir: a folder under the OS temp dir with the prefix, with whatever the test puts in it, gone when the test ends", async (t) => {
  let made = "";
  await t.test("a test that makes a scratch folder", (st) => {
    made = scratchDir(st, "scratch-self-");
    assert.equal(resolve(dirname(made)), resolve(tmpdir()));
    assert.ok(basename(made).startsWith("scratch-self-"));
    mkdirSync(join(made, "a", "b"), { recursive: true });
    writeFileSync(join(made, "a", "b", "f.txt"), "x");
    assert.ok(existsSync(join(made, "a", "b", "f.txt")), "usable while the test runs");
  });
  assert.notEqual(made, "");
  assert.equal(existsSync(made), false, "removed with its contents once its test ended");
});

test("scratchDir: two folders asked for in one test are two folders, and both go", async (t) => {
  let a = "";
  let b = "";
  await t.test("a test that makes two", (st) => {
    a = scratchDir(st, "scratch-two-");
    b = scratchDir(st, "scratch-two-");
    assert.notEqual(a, b);
  });
  assert.equal(existsSync(a) || existsSync(b), false);
});

test("scratchDir: a test that FAILS still has its folder removed (the success-path rmSync that this replaces did not run)", (t) => {
  const priv = scratchDir(t, "scratch-fail-");
  const tmp = join(priv, "tmp");
  mkdirSync(tmp);
  const probe = join(priv, "probe.test.mjs");
  const told = join(priv, "told.txt");
  writeFileSync(probe, [
    'import { test } from "node:test";',
    'import { writeFileSync } from "node:fs";',
    `import { scratchDir } from ${JSON.stringify(pathToFileURL(join(HERE, "test-scratch.mjs")).href)};`,
    'test("makes a folder, fills it, then fails", (t) => {',
    '  const d = scratchDir(t, "probe-");',
    '  writeFileSync(d + "/x", "x");',
    '  writeFileSync(process.env.PROBE_TOLD, d);',
    '  throw new Error("fails on purpose");',
    "});",
    "",
  ].join("\n"));
  const r = spawnSync(process.execPath, ["--test", probe], { env: { ...childEnv(tmp), PROBE_TOLD: told }, encoding: "utf8" });
  assert.notEqual(r.status, 0, "the probe test fails on purpose:\n" + r.stdout + r.stderr);
  assert.match(r.stdout + r.stderr, /fails on purpose/, "and it failed for that reason");
  const made = readFileSync(told, "utf8");
  assert.equal(resolve(dirname(made)), resolve(tmp), "the probe's folder was made in the private temp dir (so the check below is not vacuous)");
  assert.equal(existsSync(made), false, "its folder is gone although the test failed");
  assert.deepEqual(readdirSync(tmp), [], "and nothing else was left there");
});

test("scratchDir: a call without the test's context is refused before any folder is made (forgetting t cannot leave one behind)", () => {
  const marker = `scratch-no-t-${process.pid}-`;
  assert.throws(() => scratchDir(undefined, marker), /TestContext/);
  assert.throws(() => scratchDir("a prefix passed in the place of t", marker), /TestContext/);
  assert.deepEqual(readdirSync(tmpdir()).filter((n) => n.startsWith(marker)), [], "no folder was made");
});

test("removeTree: removes a tree with its contents, and a folder that is already gone is not an error", (t) => {
  const dir = scratchDir(t, "scratch-rm-");
  mkdirSync(join(dir, "x", "y"), { recursive: true });
  writeFileSync(join(dir, "x", "y", "f"), "1");
  removeTree(join(dir, "x"));
  assert.deepEqual(readdirSync(dir), []);
  removeTree(join(dir, "never-existed"));
});

// ---- 2. the test support leaves nothing ------------------------------------------------------------

// On POSIX a stub is a shell script and nothing is built, so there this passes by construction; the check
// has teeth on Windows with go, where makeStub compiles a launcher.
test("igpu-stub: a process that builds a stub executable leaves nothing in its OS temp dir (the Windows launcher was a cache nobody removed)", (t) => {
  const priv = scratchDir(t, "scratch-stub-");
  const code = `import { stubAvailable } from ${JSON.stringify(pathToFileURL(join(HERE, "igpu-stub.mjs")).href)}; process.stdout.write(String(stubAvailable()));`;
  const r = spawnSync(process.execPath, ["--input-type=module", "-e", code], { env: childEnv(priv), encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /^(true|false)$/);
  assert.deepEqual(readdirSync(priv), [], "the launcher it built (or tried to) is gone with the process");
});

// ---- 3. no render test makes a temp folder of its own ------------------------------------------------

// file -> [raw mkdtemp calls that may stay, why each is removed on EVERY path]. Add to this only with a
// reason: the default is `scratchDir(t, prefix)`, which cannot be forgotten.
const RAW_MKDTEMP = {
  "audiocpp-generate.test.mjs": [1, "work(): every test that calls it removes the folder in a finally"],
  "comfy-keep.test.mjs": [1, "the one test that launches a detached stub registers, right after making the folder, the after-hook that kills the stub and then removes the folder (hooks run in registration order, and a running stub holds its files open on Windows)"],
  "comfy-submit.test.mjs": [3, "each test removes its folder in a finally"],
  "igpu-delivery.test.mjs": [1, "scratch(): every caller removes the folder in a finally"],
  "igpu-engine.test.mjs": [1, "scratch(): every caller removes the folder in a finally"],
  "igpu-guard.test.mjs": [1, "the test removes its folder in a finally"],
  "igpu-lifecycle.test.mjs": [2, "the sweep and exit-hook tests each remove their folder in a finally"],
  "igpu-qa.test.mjs": [1, "scratch(): every caller removes the folder in a finally"],
  "igpu-runners-main.test.mjs": [1, "sandbox(): every caller runs sb.done() in a finally"],
  "igpu-stub.mjs": [1, "the stub launcher's folder: removed by a process exit hook, and at once if the build fails"],
  "igpu-testkit.mjs": [1, "sandbox(): every caller runs sb.done() in a finally"],
  "lease-progress.test.mjs": [1, "tmp(): every caller removes the folder in a finally"],
  "manifest-satisfy.test.mjs": [1, "realDeps(): both callers remove the folder in a finally"],
  "sdcpp-generate-main.test.mjs": [1, "sandbox(): every caller runs sb.done() in a finally"],
  "sdcpp-generate.test.mjs": [1, "tmpPng(): both callers remove the folder in a finally"],
  "templates-catalog.test.mjs": [4, "the helpers hand back a cleanup that every caller runs in a finally, and the inline one removes its folder in a finally"],
};
// the modules the tests call to set things up are held to the same rule as the tests
const SUPPORT = new Set(["igpu-stub.mjs", "igpu-testkit.mjs"]);

const stripComments = (src) => src
  .replace(/\/\*[\s\S]*?\*\//g, "")
  .replace(/(^|[\s;{}(])\/\/.*$/gm, "$1");

test("no render test or test-support module makes a temp folder of its own: it uses scratchDir, or the raw mkdtemp is listed with the reason its removal is on every path", () => {
  const found = {};
  for (const name of readdirSync(HERE).sort()) {
    if (!(name.endsWith(".test.mjs") || SUPPORT.has(name)) || name === SELF) continue;
    const n = (stripComments(readFileSync(join(HERE, name), "utf8")).match(/\bmkdtemp(Sync)?\s*\(/g) || []).length;
    if (n) found[name] = n;
  }
  const listed = Object.fromEntries(Object.entries(RAW_MKDTEMP).map(([name, [n]]) => [name, n]));
  const drift = [];
  for (const name of new Set([...Object.keys(found), ...Object.keys(listed)])) {
    if ((found[name] || 0) !== (listed[name] || 0)) drift.push(`${name}: ${found[name] || 0} raw mkdtemp call(s), ${listed[name] || 0} listed`);
  }
  assert.deepEqual(drift, [],
    "a raw mkdtemp in a render test is a folder nobody removes on a failed run (and on every run, if the rmSync is forgotten).\n" +
    "Use `const dir = scratchDir(t, \"prefix-\")` from ./test-scratch.mjs: it is removed when the test ends.\n" +
    "If the test must own its removal (a stub process has to be killed first, say), list the call in RAW_MKDTEMP with the reason.\n");
});
