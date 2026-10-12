// node --test render/test-isolation.test.mjs
//
// The isolation of the render suite from the machine it runs on. A render test that spawns a runner (or calls
// withGpuSlot / freeLlamaSwap) and leaves the llama-swap endpoint and the lease variables to its parent's
// environment drains, and can unload, the seat that is serving someone, and holds the test for as long as a
// starting seat holds the drain's probe (register F80: comfy-keep.test.mjs ran 15.9 s red against one such
// seat, and a run whose drain finished sent a real unload).
//
//   1. isolatedRunnerEnv (test-isolation.mjs) hands a child neither the parent's llama-swap nor its lease.
//   2. The scan: in every render test (and the test support that spawns runners) each runner spawn passes an env
//      that closes both doors, and every in-process withGpuSlot / freeLlamaSwap / quiesceLlamaSwap call names the
//      dependency that would otherwise reach the machine's llama-swap. A new test that forgets fails here, also in
//      a file whose other spawns are isolated: each spawn is judged on the env ITS call passes.
//
// The scan reads source text, so it is a net for the shapes the suite uses, not a parser: the fixtures below
// pin what it catches and what it lets through. An env reaches a spawn as the helper or both closings in the call
// itself, as a name declared in the same test or at the top of the file, as a parameter (each caller's argument is
// judged), or as a call of a builder the file declares. An env that arrives through a member (`sb.env`) or an
// import is not traced: the file is judged as a whole for that spawn, which is as far as the scan used to go.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { DEAD_LLAMA_SWAP, isolatedRunnerEnv } from "./test-isolation.mjs";
import { startFakeLlamaSwap } from "./fake-llama-swap.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const SELF = basename(fileURLToPath(import.meta.url));

// ---- 1. isolatedRunnerEnv --------------------------------------------------------------------------

test("isolatedRunnerEnv: the child gets a dead llama-swap and no lease, keeps the rest, and what the test passes wins", () => {
  const base = {
    PATH: "/bin", HOME: "/h", LLAMA_SWAP_API: "http://real:11436", LLAMA_SWAP_OTHER: "x",
    GPU_LEASE_DIR: "/lease", GPU_LEASE_EPOCH: "7", GPU_LEASE_CLASS: "media", GPU_LEASE_UNLOAD_MODELS: "a,b", GPU_LEASE_DEVICES: "gpu-1",
  };
  const env = isolatedRunnerEnv({ COMFY_DIR: "/c" }, base);
  assert.equal(env.LLAMA_SWAP_API, DEAD_LLAMA_SWAP);
  assert.equal(DEAD_LLAMA_SWAP, "http://127.0.0.1:9", "the discard port: a connection is refused, never answered");
  assert.deepEqual(Object.keys(env).filter((k) => /^(GPU_LEASE_|LLAMA_SWAP_OTHER)/.test(k)), []);
  assert.equal(env.PATH, "/bin");
  assert.equal(env.HOME, "/h");
  assert.equal(env.COMFY_DIR, "/c");
  assert.equal(isolatedRunnerEnv({ LLAMA_SWAP_API: "http://127.0.0.1:1234" }, base).LLAMA_SWAP_API, "http://127.0.0.1:1234", "a test that wants a llama-swap says so");
  assert.equal(isolatedRunnerEnv({ GPU_LEASE_DIR: "/mine" }, base).GPU_LEASE_DIR, "/mine", "a test that wants a lease says so");
  assert.equal(base.LLAMA_SWAP_API, "http://real:11436", "the base is not edited");
});

test("isolatedRunnerEnv: a real child whose parent names a llama-swap never reaches it", async () => {
  const fake = await startFakeLlamaSwap({ seats: [{ id: "trap-seat", state: "starting", slots: "hang" }] });
  try {
    const parent = { ...process.env, LLAMA_SWAP_API: fake.url, GPU_LEASE_DIR: "/lease", GPU_LEASE_EPOCH: "7" };
    const code = "const r = await fetch(process.env.LLAMA_SWAP_API + '/running', { signal: AbortSignal.timeout(3000) }).then(() => 'answered', (e) => 'refused'); " +
      "console.log(JSON.stringify({ r, lease: Object.keys(process.env).filter((k) => k.startsWith('GPU_LEASE_')) }));";
    const out = spawnSync(process.execPath, ["--input-type=module", "-e", code], { env: isolatedRunnerEnv({}, parent), encoding: "utf8", timeout: 30000, windowsHide: true });
    assert.equal(out.status, 0, out.stderr);
    assert.deepEqual(JSON.parse(out.stdout), { r: "refused", lease: [] });
    assert.deepEqual(fake.seen, [], "the child's drain never reached the parent's llama-swap");
  } finally { await fake.close(); }
});

// ---- 2. the scan -----------------------------------------------------------------------------------

// a comment goes, its line breaks stay: a line number reported here is the line of the real file
const stripComments = (src) => src
  .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ""))
  .replace(/(^|[\s;{}(])\/\/.*$/gm, "$1");

const lineOf = (src, idx) => src.slice(0, idx).split("\n").length;

// the text between the "(" at openIdx and its matching ")", skipping quoted strings
function callArgs(src, openIdx) {
  let depth = 0;
  for (let i = openIdx; i < src.length; i++) {
    const c = src[i];
    if (c === '"' || c === "'" || c === "`") {
      for (i++; i < src.length && src[i] !== c; i++) if (src[i] === "\\") i++;
      continue;
    }
    if (c === "(" || c === "[" || c === "{") depth++;
    else if (c === ")" || c === "]" || c === "}") { depth--; if (depth === 0) return src.slice(openIdx + 1, i); }
  }
  return src.slice(openIdx + 1);
}

// the arguments of a call, as text: split at the commas outside any bracket or string
function splitTop(args) {
  const parts = [];
  let depth = 0, from = 0;
  for (let i = 0; i < args.length; i++) {
    const c = args[i];
    if (c === '"' || c === "'" || c === "`") {
      for (i++; i < args.length && args[i] !== c; i++) if (args[i] === "\\") i++;
      continue;
    }
    if (c === "(" || c === "[" || c === "{") depth++;
    else if (c === ")" || c === "]" || c === "}") depth--;
    else if (c === "," && depth === 0) { parts.push(args.slice(from, i).trim()); from = i + 1; }
  }
  const last = args.slice(from).trim();
  if (last) parts.push(last);
  return parts;
}
const firstArg = (args) => splitTop(args)[0] ?? "";

// odd number of backticks before idx: the position is inside a template literal, that is, inside the source of a
// runner the test writes out and spawns (rule B judges that test)
const insideTemplate = (src, idx) => ((src.slice(0, idx).match(/(?<!\\)`/g) || []).length % 2) === 1;

const CLOSES_DEAD_PORT = /LLAMA_SWAP_API["']?\s*[:=]\s*["'`]http:\/\/127\.0\.0\.1:9["'`]/;
const CLOSES_LEASE = /delete\s+[\w.]*GPU_LEASE|startsWith\(\s*["'`]GPU_LEASE|\/\^\(?GPU_LEASE|GPU_LEASE_\w+\s*:\s*["'`]{2}/;

// text that isolates: the helper, or both closings written out
const isolatedIn = (text) => /\bisolatedRunnerEnv\s*\(/.test(text) || (CLOSES_DEAD_PORT.test(text) && CLOSES_LEASE.test(text));

// The top-level statement that holds idx: from the last line before it that starts in column 0 with something other
// than a closer, to the next line that starts with one. The suite writes its tests and helpers that way; a file
// written otherwise gets a wider block, which only makes the judgement more lenient.
function topBlock(src, idx) {
  let start = 0;
  for (const m of src.slice(0, idx).matchAll(/^(?![\s})\]])/gm)) start = m.index;
  const close = /^[})\]]/m.exec(src.slice(idx));
  return { start, text: src.slice(start, close ? idx + close.index : src.length) };
}

// the top-level block of a function or const the file declares by this name, or null
function blockNamed(src, name) {
  for (const m of src.matchAll(/^(?:export\s+)?(?:async\s+)?(?:function\s*\*?\s*|(?:const|let|var)\s+)([\w$]+)/gm)) {
    if (m[1] === name) return topBlock(src, m.index + 1).text;
  }
  return null;
}

// the name and parameters of the function a top-level block declares (null when it is not one)
function headerOf(blockText) {
  const first = blockText.split("\n", 1)[0];
  const m = /^(?:export\s+)?(?:async\s+)?function\s*\*?\s*([\w$]+)\s*\(([^)]*)\)/.exec(first)
    || /^(?:export\s+)?(?:const|let|var)\s+([\w$]+)\s*=\s*(?:async\s*)?\(([^)]*)\)\s*=>/.exec(first);
  return m ? { name: m[1], params: m[2].split(",").map((p) => p.replace(/=[\s\S]*$/, "").replace(/^\s*\.\.\./, "").trim()) } : null;
}

// Is the env expression `expr`, which the call at `at` passes, isolated? true / false, or null when the text does not
// say (a member chain, an import, a function the file does not declare): the caller then falls back to the file.
// It reads what a person reads: the helper or both closings in the expression itself; a name declared in the same
// test (the declaration and what follows it up to the call); a parameter (what each caller of the function passes);
// a call of a builder the file declares (the builder's body).
function envVerdict(src, expr, at, depth = 0) {
  const e = expr.trim();
  if (isolatedIn(e)) return true;
  if (e === "" || depth > 3) return null;
  if (e === "process.env") return false;
  const all = (vs) => (vs.includes(false) ? false : vs.includes(null) ? null : true);
  if (e.startsWith("{")) {
    const spreads = [...e.matchAll(/\.\.\.\s*([\w$.]+)/g)].map((m) => m[1]).filter((s) => s !== "process.env");
    if (spreads.length === 0) return false;
    const vs = spreads.map((s) => envVerdict(src, s, at, depth + 1));
    return vs.includes(true) ? true : all(vs);
  }
  const call = /^([\w$]+)\s*\(/.exec(e);
  if (call) {
    const body = blockNamed(src, call[1]);
    return body === null ? null : isolatedIn(body);
  }
  if (!/^[\w$]+$/.test(e)) return null;
  const lastNamed = (text, re) => { let last = null; for (const m of text.matchAll(re)) if (m[1] === e) last = m; return last; };
  const initOf = (region) => envVerdict(src, region.slice(region.indexOf("=") + 1).split(/;|\n/, 1)[0], at, depth + 1);
  // a declaration in the same top-level block, before the call
  const blk = topBlock(src, at);
  const local = lastNamed(blk.text.slice(0, at - blk.start), /\b(?:const|let|var)\s+([\w$]+)\s*=/g);
  if (local) {
    const region = blk.text.slice(local.index, at - blk.start);
    return isolatedIn(region) ? true : initOf(region);
  }
  // a parameter of the function the call is in: what each caller hands it
  const head = headerOf(blk.text);
  const slot = head ? head.params.indexOf(e) : -1;
  if (slot >= 0) {
    const vs = [];
    for (const m of src.matchAll(/(?<![\w$.])([\w$]+)\s*\(/g)) {
      if (m[1] !== head.name || /\bfunction\s*\*?\s*$/.test(src.slice(0, m.index))) continue;
      const given = splitTop(callArgs(src, m.index + m[0].length - 1))[slot];
      vs.push(given === undefined ? false : envVerdict(src, given, m.index, depth + 1)); // none given: undefined, which is the parent's env
    }
    return vs.length === 0 ? null : all(vs);
  }
  // a module-level declaration before the call
  const top = lastNamed(src.slice(0, at), /^(?:export\s+)?(?:const|let|var)\s+([\w$]+)\s*=/gm);
  if (top) {
    const region = src.slice(top.index, at);
    return isolatedIn(region) ? true : initOf(region);
  }
  return null;
}

// scanSource: the violations of one file's source. `runners` are the file names of the render modules that call
// withGpuSlot; `spawner` marks a support module whose spawn helper runs runners for the tests that import it.
export function scanSource(name, source, { runners, spawner = false }) {
  const src = stripComments(source);
  const out = [];
  const definesFree = /\bfreeLlamaSwap\s*:/.test(src);
  const definesLease = /\blease\s*:/.test(src);

  // A. in-process withGpuSlot: the default freeLlamaSwap drains and unloads the machine's llama-swap, and the
  //    default lease is the one the parent's GPU_LEASE_* names (fenced against, its once-per-lease unload
  //    claimed). Both are named, or spread from a harness the file defines.
  for (const m of src.matchAll(/\bwithGpuSlot\s*\(/g)) {
    if (/\bfunction\s+$/.test(src.slice(0, m.index)) || insideTemplate(src, m.index)) continue;
    const arg = firstArg(callArgs(src, m.index + m[0].length - 1));
    const spread = !arg.startsWith("{") || /\.\.\./.test(arg); // opts held elsewhere, or spread into the literal
    if (!/\bfreeLlamaSwap\b/.test(arg) && !(spread && definesFree)) {
      out.push(`${name}:${lineOf(src, m.index)}: withGpuSlot(...) with no freeLlamaSwap override reaches the machine's llama-swap`);
    }
    if (!/\blease\b/.test(arg) && !(spread && definesLease)) {
      out.push(`${name}:${lineOf(src, m.index)}: withGpuSlot(...) with no lease option takes the parent's GPU_LEASE_* lease (pass lease: null, or the lease the test means)`);
    }
  }

  // C. direct freeLlamaSwap / quiesceLlamaSwap calls name the endpoint or the fetch they use
  for (const m of src.matchAll(/\bfreeLlamaSwap\s*\(/g)) {
    if (/\bfunction\s+$/.test(src.slice(0, m.index)) || insideTemplate(src, m.index)) continue;
    if (callArgs(src, m.index + m[0].length - 1).trim() === "") out.push(`${name}:${lineOf(src, m.index)}: freeLlamaSwap() with no endpoint reaches the machine's llama-swap`);
  }
  for (const m of src.matchAll(/\bquiesceLlamaSwap\s*\(/g)) {
    if (/\bfunction\s+$/.test(src.slice(0, m.index)) || insideTemplate(src, m.index)) continue;
    if (!/\bfetchImpl\b|\bapi\s*:/.test(callArgs(src, m.index + m[0].length - 1))) out.push(`${name}:${lineOf(src, m.index)}: quiesceLlamaSwap(...) names neither api nor fetchImpl: it would probe the machine's llama-swap`);
  }

  // B. a node child that reaches a runner gets an environment that closes both doors. Each such spawn is judged on
  //    the env ITS call passes: isolation written for one spawn vouches for no other, so a runner spawn added to a
  //    file that already isolates another fails here.
  const fileNames = [...new Set(runners.filter((r) => src.includes(r)))];
  const fileReaches = fileNames.length > 0 || /gpu-lock\.mjs/.test(src) || spawner;
  const nodeNames = new Set([...src.matchAll(/\b(?:const|let|var)\s+([\w$]+)\s*=\s*(?:process\.)?execPath\s*(?:[;,\n]|$)/g)].map((m) => m[1]));
  const isNode = (first) => /^(?:process\.)?execPath$/.test(first) || /^["'`]node(?:\.exe)?["'`]$/.test(first) || nodeNames.has(first);
  for (const m of src.matchAll(/\b(spawn|spawnSync|execFile|execFileSync|fork)\s*\(/g)) {
    if (/\bfunction\s+$/.test(src.slice(0, m.index)) || insideTemplate(src, m.index)) continue;
    const args = callArgs(src, m.index + m[0].length - 1);
    const parts = splitTop(args);
    const fork = m[1] === "fork"; // fork(module, args, options) always runs node, on the module
    if (!fork && !isNode(parts[0] ?? "")) continue;
    // what node is told to run: the first element of the argument array (or the module of a fork)
    const list = fork ? null : (parts[1] ?? "");
    const target = fork ? (parts[0] ?? "") : list.startsWith("[") ? (splitTop(list.slice(1, -1))[0] ?? "") : list;
    const named = [...new Set(runners.filter((r) => args.includes(r)))];
    const namesRunner = named.length > 0 || /gpu-lock\.mjs/.test(args);
    // A literal flag first ("-e", "--input-type=module", "--check") or a literal script that names no runner runs
    // what the call spells out; a script held in a variable might be any runner, so the file decides.
    const reachesRunner = namesRunner || (fileReaches && !/^["'`]/.test(target));
    if (!reachesRunner) continue;
    const rest = parts.slice(fork ? 1 : 2);
    const options = rest.find((p) => p.startsWith("{")) ?? rest.find((p) => /^[\w$.]+$/.test(p) && !/^(?:cb|callback)$/.test(p)) ?? "";
    let verdict;
    if (isolatedIn(options)) verdict = true;
    else if (options.startsWith("{")) {
      const props = splitTop(options.slice(1, -1));
      const envProp = props.find((p) => /^env\s*(?::|$)/.test(p));
      if (envProp) verdict = envVerdict(src, envProp.replace(/^env\s*:?/, "") || "env", m.index);
      else verdict = props.some((p) => p.startsWith("...")) ? null : false; // no env key: the parent's
    } else verdict = options === "" ? false : null; // options held in a variable are not traced
    if (verdict === null) verdict = isolatedIn(src); // the text does not say: the file does, as before
    if (!verdict) {
      out.push(`${name}:${lineOf(src, m.index)}: spawns node for ${named.length ? named.join(", ") : "a runner"} without isolating the env it passes: use env: isolatedRunnerEnv() (or LLAMA_SWAP_API=${DEAD_LLAMA_SWAP} and a GPU_LEASE_* scrub written out, in the env this call passes)`);
    }
  }
  return out;
}

const runnersOf = () => readdirSync(HERE)
  .filter((n) => n.endsWith(".mjs") && !n.endsWith(".test.mjs") && n !== "gpu-lock.mjs")
  .filter((n) => /\bwithGpuSlot\s*\(/.test(stripComments(readFileSync(join(HERE, n), "utf8"))))
  .sort();

const FIX = { runners: ["comfy-run-graph.mjs", "sdcpp-video.mjs"] };

test("scan: an in-process withGpuSlot must name freeLlamaSwap and lease (or spread a harness that defines both)", () => {
  const v = (src) => scanSource("a.test.mjs", src, FIX);
  assert.equal(v(`await withGpuSlot({ noLock: true, comfyManaged: false }, async () => {});`).length, 2, "neither named");
  assert.match(v(`await withGpuSlot({ noLock: true, lease: null }, fn);`).join(" | "), /no freeLlamaSwap override/, "the llama-swap alone is missing");
  assert.match(v(`await withGpuSlot({ noLock: true, freeLlamaSwap: async () => {} }, fn);`).join(" | "), /no lease option/, "the lease alone is missing");
  assert.equal(v(`await withGpuSlot({ noLock: true, lease: null, freeLlamaSwap: async () => {} }, fn);`).length, 0);
  assert.equal(v(`await withGpuSlot({ lease: LEASE, checkLease: () => true, freeLlamaSwap: async () => {} }, fn);`).length, 0, "a lease the test means is a lease named");
  assert.equal(v(`const deps = { freeLlamaSwap: async () => {} }; const opts = { lease: null }; await withGpuSlot({ ...opts, ...deps }, fn);`).length, 0);
  assert.equal(v(`await withGpuSlot({ ...opts, ...deps }, fn);`).length, 2, "a spread is trusted only where the file defines the override and the lease");
  assert.equal(v(`const deps = { freeLlamaSwap: async () => {} }; await withGpuSlot({ ...opts, ...deps }, fn);`).length, 1, "the file defines the override but not the lease");
  assert.equal(v(`await withGpuSlot(opts, fn);`).length, 2);
  assert.equal(v(`// await withGpuSlot({ noLock: true }, fn);
/* withGpuSlot({}) */`).length, 0, "a comment is not a call");
});

test("scan: direct freeLlamaSwap / quiesceLlamaSwap calls name their endpoint or fetch", () => {
  assert.equal(scanSource("a.test.mjs", `await freeLlamaSwap();`, FIX).length, 1);
  assert.equal(scanSource("a.test.mjs", `await freeLlamaSwap("http://x", { log });`, FIX).length, 0);
  assert.equal(scanSource("a.test.mjs", `await quiesceLlamaSwap(["m"], { pollMs: 1 });`, FIX).length, 1);
  assert.equal(scanSource("a.test.mjs", `await quiesceLlamaSwap(["m"], { fetchImpl, pollMs: 1 });`, FIX).length, 0);
  assert.equal(scanSource("a.test.mjs", `await quiesceLlamaSwap(["m"], { api: fake.url });`, FIX).length, 0);
});

test("scan: a test that spawns a runner closes both doors, or it is named", () => {
  const spawnRunner = `const r = spawnSync(process.execPath, [join(HERE, "sdcpp-video.mjs"), ...args], { encoding: "utf8", windowsHide: true });`;
  const withEnv = (decl) => decl + `\n` + spawnRunner.replace(`{ encoding: "utf8", windowsHide: true }`, `{ encoding: "utf8", env, windowsHide: true }`);
  assert.equal(scanSource("a.test.mjs", spawnRunner, FIX).length, 1, "no env at all");
  assert.equal(scanSource("a.test.mjs", withEnv(`const env = isolatedRunnerEnv({});`), FIX).length, 0);
  assert.equal(scanSource("a.test.mjs", withEnv(`const env = { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9" };`), FIX).length, 1, "the llama-swap alone leaves the lease open");
  assert.equal(scanSource("a.test.mjs", withEnv(`const env = { ...process.env }; delete env.GPU_LEASE_DIR;`), FIX).length, 1, "the lease alone leaves the llama-swap open");
  assert.equal(scanSource("a.test.mjs", withEnv(`const env = { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9" };\nfor (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE")) delete env[k];`), FIX).length, 0, "both written out is accepted");
  assert.equal(scanSource("a.test.mjs", withEnv(`const env = { ...process.env, LLAMA_SWAP_API: "http://localhost:11436" };\ndelete env.GPU_LEASE_DIR;`), FIX).length, 1, "the machine's own llama-swap is not a closing");
  assert.equal(scanSource("a.test.mjs", `const r = spawnSync("ffmpeg", ["-i", "sdcpp-video.mjs"], { windowsHide: true });`, FIX).length, 0, "a child that is not node is not a runner");
  assert.equal(scanSource("a.test.mjs", `const r = spawnSync(process.execPath, ["--check", "captions.mjs"], { windowsHide: true });`, FIX).length, 0, "a node child that reaches no runner is out of scope");
  assert.equal(scanSource("kit.mjs", `spawn(process.execPath, [join(here, script), ...args], { env, windowsHide: true });`, { ...FIX, spawner: true }).length, 1, "a support module that runs runners for the tests is held to the same rule");
});

// The scan judges each spawn on the environment THAT call passes (register F80, review round): isolation found
// elsewhere in the file, for another spawn, vouches for nothing. A test added to a file that already isolates one
// runner is the case the scan exists for.
test("scan: every runner spawn is judged on its own env, not on the isolation found elsewhere in the file", () => {
  const ISOLATED = `spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "x"], { env: isolatedRunnerEnv(), windowsHide: true });`;
  const v = (src) => scanSource("a.test.mjs", src, { runners: [...FIX.runners, "comfy-render.mjs"] }).map((s) => s.replace(/^a\.test\.mjs:/, ""));
  const three = (second) => `${ISOLATED}\n${second}\nawait Promise.resolve();`;
  assert.deepEqual(v(ISOLATED), []);
  // a second spawn in a file that isolates the first
  const loud = v(three(`spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "y"], { env: process.env, windowsHide: true });`));
  assert.equal(loud.length, 1, loud.join("\n"));
  assert.match(loud[0], /^2: spawns node/, "the violation names the line of the spawn that is not isolated");
  assert.equal(v(three(`spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "y"], { encoding: "utf8", windowsHide: true });`)).length, 1, "no env: the child inherits the parent's");
  assert.equal(v(three(`spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "y"], { env: { ...process.env, COMFY_DIR: "/c" }, windowsHide: true });`)).length, 1, "an env literal that spreads the parent's and closes nothing");
  assert.equal(v(three(`spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "y"], { env: { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9" }, windowsHide: true });`)).length, 1, "one closing of two");
  assert.deepEqual(v(three(`spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "y"], { env: isolatedRunnerEnv({ COMFY_DIR: "/c" }), windowsHide: true });`)), [], "a second isolated spawn is fine");
  assert.deepEqual(v(three(`spawnSync(process.execPath, [join(HERE, "comfy-render.mjs"), "y"], { env: { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9", GPU_LEASE_DIR: "" }, windowsHide: true });`)), [], "both closings written out in the call itself");
  // an env held in a variable is judged by the declaration the call can see, not by another variable's
  assert.equal(v(`const a = isolatedRunnerEnv();\nspawnSync(process.execPath, [join(HERE, "comfy-render.mjs")], { env: a });\nconst b = { ...process.env };\nspawnSync(process.execPath, [join(HERE, "comfy-render.mjs")], { env: b });`).length, 1, "the second variable closes nothing");
  assert.deepEqual(v(`const a = isolatedRunnerEnv();\nspawnSync(process.execPath, [join(HERE, "comfy-render.mjs")], { env: a });\nspawnSync(process.execPath, [join(HERE, "comfy-render.mjs")], { env: a });`), [], "the same isolated variable twice");
  assert.equal(v(`test("one", () => {\n  const env = isolatedRunnerEnv();\n  spawnSync(process.execPath, [join(HERE, "comfy-render.mjs")], { env, windowsHide: true });\n});\ntest("two", () => {\n  const env = { ...process.env };\n  spawnSync(process.execPath, [join(HERE, "comfy-render.mjs")], { env, windowsHide: true });\n});`).length, 1, "the same name in another test is another variable");
  // a helper that takes the env as a parameter is judged by what each caller hands it
  const helper = (call) => `function run(script, args, env) {\n  return spawnSync(process.execPath, [script, ...args], { env, windowsHide: true });\n}\n${call}`;
  assert.deepEqual(v(helper(`run(join(HERE, "comfy-render.mjs"), [], isolatedRunnerEnv());`)), []);
  assert.equal(v(helper(`run(join(HERE, "comfy-render.mjs"), [], process.env);\nrun(join(HERE, "comfy-render.mjs"), [], isolatedRunnerEnv());`)).length, 1, "one caller hands the parent's env");
  assert.equal(v(helper(`run(join(HERE, "comfy-render.mjs"), []);`)).length, 1, "a caller that hands none: undefined means the parent's");
  const builder = (body) => `function scrubbedEnv() {\n${body}\n}\n` + helper(`run(join(HERE, "comfy-render.mjs"), [], scrubbedEnv());`);
  assert.deepEqual(v(builder(`  const env = { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9" };\n  for (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE")) delete env[k];\n  return env;`)), []);
  assert.equal(v(builder(`  return { ...process.env };`)).length, 1, "a builder that closes nothing");
  assert.equal(v(`${ISOLATED}\n` + builder(`  return { ...process.env };`)).length, 1, "a builder that closes nothing, in a file whose other spawn is isolated");
  // the other spellings of a node child
  assert.equal(v(`const NODE = process.execPath;\nspawnSync(NODE, [join(HERE, "comfy-render.mjs")], { env: process.env });`).length, 1, "node through a name bound to process.execPath");
  assert.equal(v(`spawnSync("node", [join(HERE, "comfy-render.mjs")], { env: process.env, windowsHide: true });`).length, 1, "node by name");
  assert.deepEqual(v(`spawnSync("node", [join(HERE, "comfy-render.mjs")], { env: isolatedRunnerEnv(), windowsHide: true });`), []);
  assert.equal(v(`fork(join(HERE, "comfy-render.mjs"), [], { env: process.env });`).length, 1, "fork runs node on its first argument");
  assert.equal(v(`execFileSync(process.execPath, [join(HERE, "comfy-render.mjs")], { encoding: "utf8" });`).length, 1);
  // what stays out of scope
  assert.deepEqual(v(`${ISOLATED}\nspawnSync(process.execPath, ["--input-type=module", "-e", code], { encoding: "utf8" });`), [], "an inline child that names no runner reaches none");
  assert.deepEqual(v(`${ISOLATED}\nspawnSync(process.execPath, ["--check", "captions.mjs"]);`), []);
  assert.equal(v(`${ISOLATED}\nspawnSync(process.execPath, ["-e", "import('./comfy-render.mjs')"], { encoding: "utf8" });`).length, 1, "an inline child that names a runner is judged like any other");
  assert.deepEqual(v("const name = \"comfy-render.mjs\";\nconst src = `\nspawn(process.execPath, [runner], { env: process.env });\n`;"), [], "a spawn inside a template is the text of another program, judged by the spawn that runs it");
  // a spawn the scan cannot trace is judged by the file, as before
  assert.deepEqual(v(`${ISOLATED}\nspawn(process.execPath, [sb.script], { env: sb.env });`), []);
  assert.equal(v(`spawn(process.execPath, [sb.script], { env: sb.env });\nconst x = "comfy-render.mjs";`).length, 1, "untraceable, and nothing in the file isolates anything");
});

test("scan: a runner the test writes out as a template is judged by its spawn, not by its withGpuSlot call", () => {
  const template = "writeFileSync(runner, `\nimport { withGpuSlot } from ${JSON.stringify(url(\"gpu-lock.mjs\"))};\nawait withGpuSlot({ noLock: true }, async () => 1);\n`);\nspawn(process.execPath, [runner], { env: { ...process.env } });";
  const v = scanSource("a.test.mjs", template, FIX);
  assert.equal(v.length, 1, v.join("\n"));
  assert.match(v[0], /spawns node/);
  assert.equal(scanSource("a.test.mjs", template.replace("{ ...process.env }", "isolatedRunnerEnv({})"), FIX).length, 0);
});

test("the scan finds the runners: every render module that calls withGpuSlot", () => {
  const runners = runnersOf();
  for (const must of ["comfy-run-graph.mjs", "sdcpp-generate.mjs", "comfy-render.mjs", "tts.mjs", "audiocpp-generate.mjs"]) {
    assert.ok(runners.includes(must), `${must} calls withGpuSlot but the scan does not list it as a runner: ${runners.join(", ")}`);
  }
  assert.ok(!runners.includes("gpu-lock.mjs"));
});

// the test support that spawns runners on behalf of the tests that import it
const SPAWNERS = new Set(["igpu-testkit.mjs"]);

test("no render test or test-support module reaches the machine's llama-swap or lease: runners it spawns run in an isolated environment, and in-process calls name their dependencies", () => {
  const runners = runnersOf();
  const found = [];
  for (const name of readdirSync(HERE).sort()) {
    if (!(name.endsWith(".test.mjs") || SPAWNERS.has(name)) || name === SELF) continue;
    found.push(...scanSource(name, readFileSync(join(HERE, name), "utf8"), { runners, spawner: SPAWNERS.has(name) }));
  }
  assert.deepEqual(found, [],
    "a render test that lets a runner (or withGpuSlot / freeLlamaSwap) reach the parent's llama-swap drains, and can unload, the seat that is serving someone, and a seat that is still starting holds the test for 17 s.\n" +
    "Spawn runners with `env: isolatedRunnerEnv({ ...what the test needs })` from ./test-isolation.mjs, on every spawn; give an in-process withGpuSlot `freeLlamaSwap: async () => {}` and a `lease` (null, or the one the test means).\n");
});
