// node --test render/no-visible-windows.test.mjs
//
// House rule: nothing the render code or its tests start may open a console window on the operator's
// desktop. On Windows a console program started by a process that has no console of its own (a runner
// started detached, a test child started detached, a service) gets a NEW console window unless it is
// started hidden, and with Windows Terminal as the default terminal that window is a Windows Terminal
// window. One run of this suite put several of them on a workstation's screen, one titled taskkill.exe:
// about 90 spawn / spawnSync calls passed no `windowsHide`. comfy-submit.mjs and compose-hyperframes.mjs
// already did, and comfy-submit.test.mjs asserts it for the spawn it builds.
//
// So every child-process call in render/ (production modules, test-support modules, tests, and the scripts
// the tests write into a string and run in a child) sets `windowsHide: true` in its options. This test reads
// every source under render/ and fails on the calls that do not, naming file:line. spawn-scan.mjs is the
// scanner; section 1 pins what it understands, section 2 is the rule.
//
// There is no allow-list of calls: a call that genuinely could not take the flag would be listed here with its
// reason (NO_OWN_CALLS below only names the one file that loads child_process and starts nothing itself).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { basename, dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { lex, scanSource, usesChildProcess } from "./spawn-scan.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const SELF = basename(fileURLToPath(import.meta.url));

const bad = (src) => scanSource(src).filter((s) => !s.ok);
const all = (src) => scanSource(src);

// ---- 1. the scanner ------------------------------------------------------------------------------------

test("scanner: a spawn / spawnSync without windowsHide is a violation, with it is not", () => {
  const head = 'import { spawn, spawnSync } from "node:child_process";\n';
  assert.equal(bad(head + 'spawnSync("taskkill", ["/PID", "1"], { stdio: "ignore" });').length, 1);
  assert.equal(bad(head + 'spawnSync("taskkill", ["/PID", "1"], { stdio: "ignore", windowsHide: true });').length, 0);
  assert.equal(bad(head + 'spawn("node", ["a.js"]);').length, 1, "no options object at all");
  assert.equal(bad(head + 'spawn("node", ["a.js"], { windowsHide: true });').length, 0);
  assert.equal(bad(head + 'const c = spawn(\n  process.execPath,\n  [a, b],\n  {\n    stdio: "ignore",\n    detached: true,\n  },\n);').length, 1, "a multi-line call");
  assert.equal(bad(head + 'const c = spawn(\n  process.execPath,\n  [a, b],\n  {\n    stdio: "ignore",\n    windowsHide: true,\n  },\n);').length, 0);
  assert.equal(bad(head + 'spawn("x", [], { windowsHide: false });').length, 1, "windowsHide: false is not hiding");
  assert.equal(bad(head + 'spawn("x", [], { windowsHide });').length, 1, "a shorthand that could be anything is not a literal true");
});

test("scanner: a windowsHide in a comment or a message string does not count", () => {
  const head = 'import { spawn } from "node:child_process";\n';
  assert.equal(bad(head + 'spawn("x", [], { stdio: "ignore" /* windowsHide: true */ });').length, 1);
  assert.equal(bad(head + 'spawn("x", [], { stdio: "ignore", // windowsHide: true\n});').length, 1);
  assert.equal(bad(head + 'spawn("x", ["windowsHide: true"], { stdio: "ignore" });').length, 1);
});

test("scanner: aliases, namespaces, the require chain and the injected seam are all calls", () => {
  assert.equal(bad('import { spawn as nodeSpawn, spawnSync as nodeSpawnSync } from "node:child_process";\nnodeSpawn("a", []);\nnodeSpawnSync("b", []);').length, 2);
  assert.equal(bad('import * as cp from "node:child_process";\ncp.spawn("a", []);\ncp.execFileSync("b", []);').length, 2);
  assert.equal(bad('const cp = require("child_process");\ncp.exec("a");').length, 1);
  assert.equal(bad('const { spawn: run, execFile } = require("node:child_process");\nrun("a", []);\nexecFile("b", [], () => {});').length, 2);
  assert.equal(bad('const { spawn } = await import("node:child_process");\nspawn("a", []);').length, 1);
  assert.equal(bad('require("child_process").spawn("a", []);').length, 1);
  assert.equal(bad('(await import("node:child_process")).spawnSync("a", []);').length, 1);
  assert.equal(bad('export async function f({ spawnImpl = real } = {}) {\n  return spawnImpl("a", [], { stdio: "pipe" });\n}').length, 1, "the injected seam is judged like the real thing");
  assert.equal(bad('export async function f({ spawnImpl = real } = {}) {\n  return spawnImpl("a", [], { stdio: "pipe", windowsHide: true });\n}').length, 0);
});

test("scanner: things that are not child-process calls are not judged", () => {
  const head = 'import { spawn } from "node:child_process";\n';
  assert.equal(all(head + "const m = /a/.exec(s); const n = re.exec(t); fakeSpawn([]); isSpawnFailure(e);").length, 0, ".exec on a regex and helpers that merely contain the word");
  assert.equal(all(head + "// spawn(\"a\", [])\n/* spawn(\"b\") */").length, 0, "comments");
  assert.equal(all(head + 'const s = "a spawn() that errors"; test("spawn(x) is retried", () => {});').length, 0, "a message that names spawn()");
  assert.equal(all(head + "const r = /spawn\\(/;").length, 0, "a regex literal");
  assert.equal(all("function spawn(a) { return a; }\nconst o = { spawn(b) { return b; } };\nspawn(1);").length, 0, "no import of child_process: the name is the file's own");
  assert.equal(all(head + "function wrap() { return { spawn(cmd) { return cmd; } }; }").length, 0, "a method definition");
});

test("scanner: a call that only forwards its caller's arguments is not judged", () => {
  const s = all('import { spawn } from "node:child_process";\nconst f = { spawnImpl: (...a) => (child = spawn(...a)) };');
  assert.equal(s.length, 1);
  assert.equal(s[0].ok, true);
  assert.match(s[0].why, /forwards/);
});

test("scanner: an options object a file declares is followed to its windowsHide", () => {
  const head = 'import { spawn } from "node:child_process";\n';
  assert.equal(bad(head + 'const OPTS = { stdio: "ignore", windowsHide: true };\nspawn("a", [], OPTS);').length, 0);
  assert.equal(bad(head + 'const OPTS = { stdio: "ignore" };\nspawn("a", [], OPTS);').length, 1);
  assert.equal(bad(head + "spawn(a, b, opts);").length, 1, "an options name nothing in the file defines");
});

test("scanner: a script held in a string (node -e, a fixture written to disk) is held to the rule", () => {
  const head = 'import { spawn } from "node:child_process";\n';
  const tpl ='writeFileSync(p, `import {spawn} from "node:child_process";\nconst c = spawn(cmd[0], cmd.slice(1), {stdio: "ignore", detached: true});\nc.unref();`);';
  assert.equal(bad(tpl).length, 1);
  assert.equal(bad(tpl.replace("detached: true", "detached: true, windowsHide: true")).length, 0);
  const sq = "const p = spawn(process.execPath, ['-e', 'x'], {stdio:'ignore'}); const q = \"const c=require('child_process').spawn(process.execPath,[a,b],{stdio:'ignore',detached:true});c.unref();\";";
  assert.equal(bad("import { spawn } from \"node:child_process\";\n" + sq).length, 2, "the outer call and the one in the string");
  assert.equal(bad("const t = `const fs=require('fs'),cp=require('child_process');const c=cp.spawn(process.execPath,['-e','0'],{stdio:'ignore'});`;").length, 1, "a namespace bound inside the script");
  assert.equal(bad("const t = `const x = ${JSON.stringify(a)}; const { spawn } = require(\"node:child_process\"); spawn(process.execPath, [${JSON.stringify(b)}], { windowsHide: true });`;").length, 0, "a ${} in the script");
  assert.equal(all(head + 'const t = "spawn(x) in a message";').length, 0, "a string that does not name child_process holds no script");
});

test("scanner: the tokenizer handles nested templates, regexes with quotes, and fails loudly when it loses its place", () => {
  const nested = 'import { spawn } from "node:child_process";\nconst a = `x ${`y ${1 + 1}`} z`; const re = /["\'`]/g; const d = 4 / 2;\nspawn("a", [], { windowsHide: true });';
  assert.equal(bad(nested).length, 0);
  assert.equal(all(nested).length, 1);
  assert.throws(() => lex("const a = `never closed"), /cannot tokenize/);
  assert.throws(() => lex("const a = 'never closed\nconst b = 1;"), /cannot tokenize/);
  assert.equal(usesChildProcess('import { spawn } from "node:child_process";'), true);
  assert.equal(usesChildProcess('// import { spawn } from "node:child_process";\nconst a = 1;'), false);
});

// ---- 1b. call shapes the scanner must not be fooled by ---------------------------------------------------
// Each case below started a child without windowsHide: true and passed the first version of the scanner.

const CP_HEAD = 'import { spawn, spawnSync, execFile, exec } from "node:child_process";\n';

test("scanner: every way of binding a child_process function is followed", () => {
  assert.equal(bad('import cp, { spawn } from "node:child_process";\nspawn("x", []);').length, 1, "default plus named import");
  assert.equal(bad('import cp, { spawn } from "node:child_process";\ncp.spawn("x", []);').length, 1, "the default import is the namespace");
  assert.equal(bad('import cp from "node:child_process";\nconst { spawn } = cp;\nspawn("x", []);').length, 1, "destructured from a bound namespace");
  assert.equal(bad('const cp = require("node:child_process");\nconst { spawn: run } = cp;\nrun("x", []);').length, 1, "destructured with a rename");
  assert.equal(bad("const { spawn } = require(`child_process`);\nspawn(\"x\", []);").length, 1, "a template literal specifier");
  assert.equal(bad('const spawn = require("child_process").spawn;\nspawn("x", [], {});').length, 1, "a function picked off the require chain is a function, not a namespace");
  assert.equal(bad('const spawn = require("child_process").spawn;\nspawn("x", [], { windowsHide: true });').length, 0);
  assert.equal(bad('const run = require("node:child_process").execFileSync;\nrun("x", []);').length, 1, "picked and renamed");
  assert.equal(bad('const run = (await import("node:child_process")).spawnSync;\nrun("x", []);').length, 1, "picked off a dynamic import");
});

test("scanner: the options are the last non-callback argument, and only an object literal's own windowsHide counts", () => {
  assert.equal(bad(CP_HEAD + 'execFile("x", [], (e) => { spawn("y", [], { windowsHide: true }); });').length, 1, "a hidden spawn inside the callback must not vouch for the execFile");
  assert.equal(bad(CP_HEAD + 'execFile("x", [], { windowsHide: true }, (e) => {});').length, 0);
  assert.equal(bad(CP_HEAD + 'exec("x", { windowsHide: true }, (e) => {});').length, 0);
  assert.equal(bad(CP_HEAD + 'exec("x", (e) => {});').length, 1, "a callback is not options");
  assert.equal(bad(CP_HEAD + 'execFile("x", [], opts, done);').length, 1, "options nothing in the file defines, a callback by name");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { env: { windowsHide: true } });').length, 1, "a nested object is not the option");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { stdio: "ignore", ...{ windowsHide: true } });').length, 1, "a spread is not a literal true");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { windowsHide: true, ...{ windowsHide: false } });').length, 1, "a later spread can override");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { windowsHide: true, ...extra });').length, 1, "a later spread can override");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { windowsHide: true, windowsHide: false });').length, 1, "a later property overrides");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { windowsHide: false, windowsHide: true });').length, 0, "the last property wins");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], { ...extra, windowsHide: true });').length, 0, "a spread before the flag cannot override it");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], cond ? { windowsHide: true } : {});').length, 1, "an expression is not an object literal");
  assert.equal(bad(CP_HEAD + 'spawn("x", ["a"]);').length, 1, "an argument list is not options");
});

test("scanner: an options name is resolved in its scope, not across the file", () => {
  const two = CP_HEAD + 'function a() { const opts = { windowsHide: true }; spawn("a", [], opts); }\nfunction b(opts) { spawn("b", [], opts); }\n';
  const sites = bad(two);
  assert.equal(sites.length, 1, "only the call in b");
  assert.equal(sites[0].line, 3);
  assert.equal(bad(CP_HEAD + 'const opts = { windowsHide: true };\nfunction b(opts) { spawn("b", [], opts); }').length, 1, "a parameter shadows the outer object");
  assert.equal(bad(CP_HEAD + 'const opts = { windowsHide: true };\nconst f = (opts) => spawn("b", [], opts);').length, 1, "an arrow parameter shadows it too");
  assert.equal(bad(CP_HEAD + 'const opts = { windowsHide: true };\nfunction b(x) { return spawn("b", [], opts); }').length, 0, "an outer object nothing shadows");
  assert.equal(bad(CP_HEAD + 'function a() { const o = { windowsHide: true }; return () => spawn("x", [], o); }').length, 0, "an inner closure sees it");
  assert.equal(bad(CP_HEAD + 'function a() { const o = { windowsHide: true }; }\nfunction c() { return spawn("x", [], o); }').length, 1, "a block's object is out of sight after it");
  assert.equal(bad(CP_HEAD + 'spawn("x", [], o);\nconst o = { windowsHide: true };').length, 1, "declared after the call");
  assert.equal(bad(CP_HEAD + 'let o = { windowsHide: true };\no = {};\nspawn("x", [], o);').length, 1, "a let can be reassigned");
  assert.equal(bad(CP_HEAD + 'const o = { windowsHide: true };\no.windowsHide = false;\nspawn("x", [], o);').length, 1, "the flag is overwritten later");
  assert.equal(bad(CP_HEAD + 'const o = { stdio: "ignore" };\nspawn("x", [], o);').length, 1, "a declared object without the flag");
  assert.equal(bad(CP_HEAD + 'const base = { windowsHide: true };\nconst o = { ...base, stdio: "ignore" };\nspawn("x", [], o);').length, 1, "an object built by spread is not followed");
});

test("scanner: optional calls, call / apply / bind and aliases through a conditional are calls", () => {
  assert.equal(bad(CP_HEAD + 'spawn?.("x", []);').length, 1, "optional call");
  assert.equal(bad(CP_HEAD + 'spawn.call(null, "x", []);').length, 1, "call without options");
  assert.equal(bad(CP_HEAD + 'spawn.call(null, "x", [], { windowsHide: true });').length, 0, "call with options (the first argument is `this`)");
  assert.equal(bad(CP_HEAD + 'spawn.apply(null, ["x", []]);').length, 1, "apply cannot be read");
  assert.equal(bad(CP_HEAD + 'const run = spawn.bind(null);\nrun("x", []);').length, 1, "bind hides the call");
  assert.equal(bad(CP_HEAD + 'const run = c ? spawn : spawnSync;\nrun("x", []);').length, 1, "an alias chosen by a conditional");
  assert.equal(bad(CP_HEAD + 'const run = c ? spawn : spawnSync;\nrun("x", [], { windowsHide: true });').length, 0);
  assert.equal(bad(CP_HEAD + 'const run = other ?? spawn;\nrun("x", []);').length, 1, "an alias chosen by ??");
  assert.equal(bad(CP_HEAD + 'const run = other || spawn;\nrun("x", []);').length, 1, "an alias chosen by ||");
});

test("scanner: a child_process function used as a value, or a promisified one, is not lost", () => {
  assert.equal(bad(CP_HEAD + 'use(spawn);').length, 1, "passed as an argument");
  assert.equal(bad(CP_HEAD + 'const t = { spawn };').length, 1, "stored in an object");
  assert.equal(bad(CP_HEAD + 'const t = [spawn][0];').length, 1, "stored in an array");
  assert.equal(bad(CP_HEAD + 'Reflect.apply(spawn, null, ["x", []]);').length, 1, "applied by Reflect");
  assert.equal(bad('import * as cp from "node:child_process";\nuse(cp);').length, 1, "the namespace passed as an argument");
  assert.equal(bad(CP_HEAD + 'const run = promisify(execFile);\nawait run("x", []);').length, 1, "promisify(execFile)");
  assert.equal(bad(CP_HEAD + 'const run = util.promisify(execFile);\nawait run("x", [], { windowsHide: true });').length, 0);
  assert.equal(bad(CP_HEAD + 'async function f({ spawnImpl = spawn } = {}) { return spawnImpl("x", []); }').length, 1, "a default of the real spawn is an alias");
  assert.equal(bad(CP_HEAD + 'async function f({ spawnImpl = spawn } = {}) { return spawnImpl("x", [], { windowsHide: true }); }').length, 0);
});

test("scanner: only a rest parameter's arguments are forwarded unjudged", () => {
  assert.equal(bad(CP_HEAD + 'function f(...a) { return spawn(...a); }').length, 0, "a rest parameter forwards its caller's arguments");
  assert.equal(bad(CP_HEAD + 'const f = (...a) => spawn(...a);').length, 0);
  assert.equal(bad(CP_HEAD + 'const args = ["x", []];\nspawn(...args);').length, 1, "an array this file builds is not the caller's");
  assert.equal(bad(CP_HEAD + 'function f(...a) { return 1; }\nspawn(...a);').length, 1, "a rest parameter out of scope");
  assert.equal(bad(CP_HEAD + 'function f(...a) { return spawn("x", ...a); }').length, 1, "extra leading arguments are not a plain forward");
});

// ---- 2. the rule ---------------------------------------------------------------------------------------

// every source under render/ that can start a process. _third_party is vendored code; the node_modules a
// local install might have made is not ours either.
function sources(dir) {
  const out = [];
  for (const e of readdirSync(dir, { withFileTypes: true }).sort((a, b) => (a.name < b.name ? -1 : 1))) {
    if (e.isDirectory()) {
      if (e.name === "node_modules" || e.name === "_third_party") continue;
      out.push(...sources(join(dir, e.name)));
    } else if (/\.(mjs|cjs|js)$/.test(e.name) && !(dir === HERE && e.name === SELF)) {
      out.push(join(dir, e.name));
    }
  }
  return out;
}

// files that load child_process and start nothing themselves, with the reason. Such a file may also hold the
// module as a value (`cp[name]`, which wraps a function by name); a call it makes is still judged.
const NO_OWN_CALLS = {
  "testdata/clock-preload.cjs": "wraps cp.spawn / cp.spawnSync for a test and hands every call's arguments on unchanged: the options are the caller's",
};

test("every child-process call in render/ sets windowsHide: true (no console window on the operator's desktop)", () => {
  const violations = [];
  const blind = [];
  let calls = 0;
  const files = sources(HERE);
  assert.ok(files.length > 50, `the scan found only ${files.length} sources: it is looking in the wrong place`);
  for (const file of files) {
    const rel = relative(HERE, file).replace(/\\/g, "/");
    const src = readFileSync(file, "utf8");
    let sites;
    try {
      sites = scanSource(src);
    } catch (e) {
      violations.push(`${rel}: ${e.message}`);
      continue;
    }
    calls += sites.length;
    // a file that imports child_process and shows no call is a scanner that went blind, not a clean file
    if (usesChildProcess(src) && sites.length === 0 && !(rel in NO_OWN_CALLS)) blind.push(rel);
    for (const s of sites) {
      if (s.ok) continue;
      if (rel in NO_OWN_CALLS && s.why.startsWith("the child_process module ")) continue; // the wrapper holds the module as a value
      violations.push(`${rel}:${s.line}  ${s.call}  (${s.why})`);
    }
  }
  assert.ok(calls > 50, `the scan found only ${calls} child-process calls: it is blind`);
  assert.deepEqual(blind, [], "these files import child_process but the scan found no call in them");
  assert.deepEqual(violations, [],
    `${violations.length} child-process call(s) start a process without windowsHide: true.\n` +
    "On Windows a console program started by a process with no console gets a NEW visible window (a Windows\n" +
    "Terminal window on the desktop). Add `windowsHide: true` to the call's options, keeping the other options as they are.\n");
});
