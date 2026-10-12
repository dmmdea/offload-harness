// node --test render/test-isolation.test.mjs
//
// The isolation of the render suite from the machine it runs on. A render test that spawns a runner (or calls
// withGpuSlot / freeLlamaSwap) and leaves the llama-swap endpoint and the lease variables to its parent's
// environment drains, and can unload, the seat that is serving someone, and holds the test for as long as a
// starting seat holds the drain's probe (register F80: comfy-keep.test.mjs ran 15.9 s red against one such
// seat, and a run whose drain finished sent a real unload).
//
//   1. isolatedRunnerEnv (test-isolation.mjs) hands a child neither the parent's llama-swap nor its lease.
//   2. The scan: every render test (and the test support that spawns runners) either spawns no runner, or
//      closes both doors, and every in-process withGpuSlot / freeLlamaSwap / quiesceLlamaSwap call names the
//      dependency that would otherwise reach the machine's llama-swap. A new test that forgets fails here.
//
// The scan reads source text, so it is a net for the shapes the suite uses, not a parser: the fixtures below
// pin what it catches and what it lets through.
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
    const out = spawnSync(process.execPath, ["--input-type=module", "-e", code], { env: isolatedRunnerEnv({}, parent), encoding: "utf8", timeout: 30000 });
    assert.equal(out.status, 0, out.stderr);
    assert.deepEqual(JSON.parse(out.stdout), { r: "refused", lease: [] });
    assert.deepEqual(fake.seen, [], "the child's drain never reached the parent's llama-swap");
  } finally { await fake.close(); }
});

// ---- 2. the scan -----------------------------------------------------------------------------------

const stripComments = (src) => src
  .replace(/\/\*[\s\S]*?\*\//g, "")
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

// the first argument of a call, as text (up to the first comma outside any bracket or string)
function firstArg(args) {
  let depth = 0;
  for (let i = 0; i < args.length; i++) {
    const c = args[i];
    if (c === '"' || c === "'" || c === "`") {
      for (i++; i < args.length && args[i] !== c; i++) if (args[i] === "\\") i++;
      continue;
    }
    if (c === "(" || c === "[" || c === "{") depth++;
    else if (c === ")" || c === "]" || c === "}") depth--;
    else if (c === "," && depth === 0) return args.slice(0, i).trim();
  }
  return args.trim();
}

// odd number of backticks before idx: the position is inside a template literal, that is, inside the source of a
// runner the test writes out and spawns (rule B judges that test)
const insideTemplate = (src, idx) => ((src.slice(0, idx).match(/(?<!\\)`/g) || []).length % 2) === 1;

const SPAWNS_NODE = /\b(?:spawn|spawnSync|execFile|execFileSync|fork)\s*\(\s*(?:process\.)?execPath\b/;
const CLOSES_DEAD_PORT = /LLAMA_SWAP_API["']?\s*[:=]\s*["'`]http:\/\/127\.0\.0\.1:9["'`]/;
const CLOSES_LEASE = /delete\s+[\w.]*GPU_LEASE|startsWith\(\s*["'`]GPU_LEASE|\/\^\(?GPU_LEASE|GPU_LEASE_\w+\s*:\s*["'`]{2}/;

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

  // B. a test that spawns a node child which reaches a runner closes both doors in the child's environment
  const names = [...new Set(runners.filter((r) => src.includes(r)))];
  const reaches = names.length > 0 || /gpu-lock\.mjs/.test(src) || spawner;
  if (reaches && SPAWNS_NODE.test(src)) {
    const helper = /\bisolatedRunnerEnv\s*\(/.test(src);
    const inline = CLOSES_DEAD_PORT.test(src) && CLOSES_LEASE.test(src);
    if (!helper && !inline) {
      out.push(`${name}: spawns node for ${names.length ? names.join(", ") : "a runner"} without isolatedRunnerEnv() (or LLAMA_SWAP_API=${DEAD_LLAMA_SWAP} and a GPU_LEASE_* scrub written out)`);
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
  const spawnRunner = `const r = spawnSync(process.execPath, [join(HERE, "sdcpp-video.mjs"), ...args], { encoding: "utf8" });`;
  assert.equal(scanSource("a.test.mjs", spawnRunner, FIX).length, 1, "no env at all");
  assert.equal(scanSource("a.test.mjs", spawnRunner + `\nconst env = isolatedRunnerEnv({});`, FIX).length, 0);
  assert.equal(scanSource("a.test.mjs", spawnRunner + `\nconst env = { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9" };`, FIX).length, 1, "the llama-swap alone leaves the lease open");
  assert.equal(scanSource("a.test.mjs", spawnRunner + `\nconst env = { ...process.env }; delete env.GPU_LEASE_DIR;`, FIX).length, 1, "the lease alone leaves the llama-swap open");
  assert.equal(scanSource("a.test.mjs", spawnRunner + `\nconst env = { ...process.env, LLAMA_SWAP_API: "http://127.0.0.1:9" };\nfor (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE")) delete env[k];`, FIX).length, 0, "both written out is accepted");
  assert.equal(scanSource("a.test.mjs", spawnRunner + `\nconst env = { ...process.env, LLAMA_SWAP_API: "http://localhost:11436" };\ndelete env.GPU_LEASE_DIR;`, FIX).length, 1, "the machine's own llama-swap is not a closing");
  assert.equal(scanSource("a.test.mjs", `const r = spawnSync("ffmpeg", ["-i", "sdcpp-video.mjs"]);`, FIX).length, 0, "a child that is not node is not a runner");
  assert.equal(scanSource("a.test.mjs", `const r = spawnSync(process.execPath, ["--check", "captions.mjs"]);`, FIX).length, 0, "a node child that reaches no runner is out of scope");
  assert.equal(scanSource("kit.mjs", `spawn(process.execPath, [join(here, script), ...args], { env });`, { ...FIX, spawner: true }).length, 1, "a support module that runs runners for the tests is held to the same rule");
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
    "Spawn runners with `env: isolatedRunnerEnv({ ...what the test needs })` from ./test-isolation.mjs; give an in-process withGpuSlot `freeLlamaSwap: async () => {}` and a `lease` (null, or the one the test means).\n");
});
