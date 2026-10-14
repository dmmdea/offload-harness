// node --test render/comfy-idle.test.mjs
//
// The launcher side of the ComfyUI idle bound. A kept (--keep-comfy) or warm (--batch) ComfyUI holds the models and the executor
// cache it loaded until something frees them; the harness stops it only when its lease ends. The bound is a pack that runs INSIDE
// ComfyUI (render/comfyui-ext/idle-nodes/offload_idle) and frees both after COMFY_IDLE_FREE_SEC (default 300) with an empty queue;
// this file pins what the launcher does to put it there and to know it is there:
//   - the default launch (no keep, no warm) is byte for byte what it was;
//   - a kept or warm launch adds the idle yaml to --extra-model-paths-config and the pack to the custom-node whitelist, in the
//     list argparse will read (one --whitelist-custom-nodes, the last), and passes OFFLOAD_IDLE_FREE_SEC to the child;
//   - the flags are added only when the yaml AND the pack exist, because ComfyUI exits at boot on a missing yaml;
//   - a running instance without the pack is reused, with one COMFY-IDLE-WARN, never restarted or refused;
//   - after a launch the instance is asked once (GET /offload/idle) and a silent pack is said.
// Pure: spawn, pid liveness and every HTTP probe are injected; the one real socket is a loopback server in the probe tests.
import { test } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createServer } from "node:http";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, dirname, join, relative } from "node:path";
import {
  launchFlags, ensureComfy, resolveIdleBound, fetchIdleProbe, IDLE_FREE_DEFAULT_SEC,
  EXT_IDLE_YAML, EXT_IDLE_PACK, EXT_IDLE_PACK_FILE,
} from "./comfy-lifecycle.mjs";
import * as W from "./wan-multicard.mjs";
import * as O from "./comfy-ownership.mjs";
import { scratchDir } from "./test-scratch.mjs";

const BASE = ["--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0"];
const WARM = ["--disable-smart-memory", "--reserve-vram", "1.0"];
const IDLE = { yaml: EXT_IDLE_YAML, pack: EXT_IDLE_PACK, packFile: EXT_IDLE_PACK_FILE };
const YAML_FLAG = ["--extra-model-paths-config", EXT_IDLE_YAML];
const API = "http://127.0.0.1:8188";
const GOOD_ANSWER = { limit_sec: 300, idle_sec: null, armed: false, freed: 0, last_free_unix: null, error: null };
const boom = (what) => () => { throw new Error(what); };
const count = (lines, re) => lines.filter((l) => re.test(l)).length;

// What multicardLaunch (wan-multicard.mjs) puts in launch.flags: its own yaml and the whitelist it measured with.
const MULTICARD = ["--gpu-only", "--disable-pinned-memory", "--extra-model-paths-config", W.EXT_YAML,
  "--disable-all-custom-nodes", "--whitelist-custom-nodes", "comfyui-multigpu", "comfyui-videohelpersuite", W.EXT_NODE_PACK];

// ---- the names ---------------------------------------------------------------------------------------

test("the idle yaml names a folder that holds the pack, and the launcher's constants point at both", () => {
  assert.equal(IDLE_FREE_DEFAULT_SEC, 300, "the house rule: every idle model unloads after 5 minutes");
  assert.ok(existsSync(EXT_IDLE_YAML), `the yaml is missing: ${EXT_IDLE_YAML}`);
  const text = readFileSync(EXT_IDLE_YAML, "utf8");
  const folder = /^\s+custom_nodes:\s*(\S+)\s*$/m.exec(text);
  assert.ok(folder, "the yaml names a custom_nodes folder");
  const packFile = join(dirname(EXT_IDLE_YAML), folder[1], EXT_IDLE_PACK, "__init__.py");
  assert.ok(existsSync(packFile), `the folder the yaml names holds no ${EXT_IDLE_PACK}/__init__.py: ${packFile}`);
  assert.equal(EXT_IDLE_PACK_FILE, packFile);
  assert.equal(basename(dirname(EXT_IDLE_PACK_FILE)), EXT_IDLE_PACK, "the whitelist names the pack by its folder name");
  assert.equal(W.EXT_IDLE_YAML, EXT_IDLE_YAML, "wan-multicard.mjs exports the same yaml");
  assert.equal(W.EXT_IDLE_PACK, EXT_IDLE_PACK);
  assert.notEqual(folder[1], "nodes", "apart from the multicard pack's folder: an ordinary kept launch never imports the Wan pack");
});

// ---- COMFY_IDLE_FREE_SEC -----------------------------------------------------------------------------

test("resolveIdleBound: 300 s unless COMFY_IDLE_FREE_SEC says another whole number of seconds, 5 or more", () => {
  for (const [env, want] of [[{}, 300], [{ COMFY_IDLE_FREE_SEC: "" }, 300], [{ COMFY_IDLE_FREE_SEC: "120" }, 120],
    [{ COMFY_IDLE_FREE_SEC: " 90 " }, 90], [{ COMFY_IDLE_FREE_SEC: "5" }, 5]]) {
    const lines = [];
    assert.equal(resolveIdleBound(env, (l) => lines.push(l)), want, JSON.stringify(env));
    assert.deepEqual(lines, [], "a valid value, and the default, are not said");
  }
});

test("resolveIdleBound: 0 and off switch it off, said once", () => {
  for (const v of ["0", "off", "OFF", "Off", " off "]) {
    const lines = [];
    assert.equal(resolveIdleBound({ COMFY_IDLE_FREE_SEC: v }, (l) => lines.push(l)), 0, v);
    assert.equal(count(lines, /^COMFY-IDLE-OFF/), 1, `${v}: ${JSON.stringify(lines)}`);
    assert.match(lines[0], /COMFY_IDLE_FREE_SEC/);
  }
});

test("resolveIdleBound: a value it cannot use is 300 with one COMFY-IDLE-WARN that names it", () => {
  for (const v of ["abc", "2", "4", "-1", "1.5", "300s", "1e3"]) {
    const lines = [];
    assert.equal(resolveIdleBound({ COMFY_IDLE_FREE_SEC: v }, (l) => lines.push(l)), 300, v);
    assert.equal(count(lines, /^COMFY-IDLE-WARN/), 1, `${v}: ${JSON.stringify(lines)}`);
    assert.ok(lines[0].includes(v), `the line names the value ${v}: ${lines[0]}`);
  }
});

// ---- launchFlags -------------------------------------------------------------------------------------

test("without an idle request the launch argv is byte for byte what it was", () => {
  assert.deepEqual(launchFlags({}), BASE);
  assert.deepEqual(launchFlags({ warm: true }), WARM);
  assert.deepEqual(launchFlags({ idle: null, extraArgs: "--lowvram" }), [...BASE, "--lowvram"]);
  assert.deepEqual(launchFlags({ launch: { flags: MULTICARD } }), [...BASE, ...MULTICARD], "a multicard launch without idle is unchanged");
});

test("an idle launch adds the idle yaml once, after the base flags and before the operator's extra args", () => {
  assert.deepEqual(launchFlags({ idle: IDLE }), [...BASE, ...YAML_FLAG]);
  assert.deepEqual(launchFlags({ idle: IDLE, warm: true }), [...WARM, ...YAML_FLAG]);
  assert.deepEqual(launchFlags({ idle: IDLE, extraArgs: "--lowvram" }), [...BASE, ...YAML_FLAG, "--lowvram"]);
});

test("the idle yaml is not added again when the operator's extra args already carry it, in either slash spelling", (t) => {
  // launchFlags splits COMFY_EXTRA_ARGS on whitespace, so a yaml path with a space cannot be carried in it (nor spelled by an operator): this
  // case needs a path without one. The checkout's own path is used when it has none; a checkout under a folder with a space is run from its
  // root, where the path relative to the current directory has none either; otherwise the case cannot be built here and says so.
  const yaml = [EXT_IDLE_YAML, relative(process.cwd(), EXT_IDLE_YAML)].find((p) => p && !/\s/.test(p) && existsSync(p));
  if (!yaml) return t.skip("no whitespace-free path to the idle yaml from here (run it from the repository root)");
  for (const spelled of [yaml, yaml.replaceAll("\\", "/")]) {
    const f = launchFlags({ idle: { ...IDLE, yaml }, extraArgs: `--extra-model-paths-config ${spelled}` });
    assert.deepEqual(f, [...BASE, "--extra-model-paths-config", spelled]);
  }
});

// The custom-node whitelist. cli_args.py declares --whitelist-custom-nodes with nargs='+' and NO append action, so a second
// occurrence REPLACES the first list, and `--whitelist-custom-nodes=a b` reads `b` as a stray positional (argparse, measured). The
// pack therefore goes INTO the list argparse will read: the last occurrence's value run, spelled in the space form.

test("a multicard launch gets the pack inside its own whitelist: one flag, the packs it had, the idle pack last", () => {
  const f = launchFlags({ idle: IDLE, launch: { flags: MULTICARD } });
  assert.deepEqual(f, [...BASE, "--gpu-only", "--disable-pinned-memory", "--extra-model-paths-config", W.EXT_YAML,
    "--disable-all-custom-nodes", "--whitelist-custom-nodes", "comfyui-multigpu", "comfyui-videohelpersuite", W.EXT_NODE_PACK, EXT_IDLE_PACK,
    ...YAML_FLAG]);
  assert.equal(count(f, /^--whitelist-custom-nodes$/), 1, "a second whitelist flag would replace the multicard packs");
});

test("an operator whitelist gets the pack at the end of its list", () => {
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: "--disable-all-custom-nodes --whitelist-custom-nodes x y --lowvram" }),
    [...BASE, ...YAML_FLAG, "--disable-all-custom-nodes", "--whitelist-custom-nodes", "x", "y", EXT_IDLE_PACK, "--lowvram"],
  );
});

test("with two whitelists the pack joins the LAST one, the list argparse keeps", () => {
  const f = launchFlags({ idle: IDLE, launch: { flags: MULTICARD }, extraArgs: "--whitelist-custom-nodes op1 op2" });
  assert.deepEqual(f, [...BASE, ...MULTICARD, ...YAML_FLAG, "--whitelist-custom-nodes", "op1", "op2", EXT_IDLE_PACK]);
});

test("the --flag=value spelling of the whitelist is rewritten to the space form, where the pack can follow", () => {
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: "--disable-all-custom-nodes --whitelist-custom-nodes=x" }),
    [...BASE, ...YAML_FLAG, "--disable-all-custom-nodes", "--whitelist-custom-nodes", "x", EXT_IDLE_PACK],
  );
});

test("argparse abbreviations of the two flags are read as the flags", () => {
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: "--disable-all --wh x" }),
    [...BASE, ...YAML_FLAG, "--disable-all", "--wh", "x", EXT_IDLE_PACK],
  );
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: "--disable-all --lowvram" }),
    [...BASE, ...YAML_FLAG, "--disable-all", "--lowvram", "--whitelist-custom-nodes", EXT_IDLE_PACK],
  );
});

// The shortest abbreviations, measured against ComfyUI 0.38.0's own parser (comfy.cli_args.parser.parse_args, allow_abbrev on):
// `--disable-al` is --disable-all-custom-nodes and `--wh` is --whitelist-custom-nodes; `--disable-a` (auto-launch, async-offload,
// all-custom-nodes, api-nodes) and `--w` (windows-standalone-build, whitelist-custom-nodes) are refused as ambiguous.

test("the shortest abbreviation ComfyUI accepts of --disable-all-custom-nodes is read as the flag", () => {
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: "--disable-al --lowvram" }),
    [...BASE, ...YAML_FLAG, "--disable-al", "--lowvram", "--whitelist-custom-nodes", EXT_IDLE_PACK],
  );
});

test("a prefix ComfyUI refuses as ambiguous is not read as either flag", () => {
  assert.deepEqual(launchFlags({ idle: IDLE, extraArgs: "--disable-a --lowvram" }), [...BASE, ...YAML_FLAG, "--disable-a", "--lowvram"]);
  assert.deepEqual(launchFlags({ idle: IDLE, extraArgs: "--w x --lowvram" }), [...BASE, ...YAML_FLAG, "--w", "x", "--lowvram"]);
});

test("--disable-all-custom-nodes with no whitelist gets one that names the pack, at the end", () => {
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: "--disable-all-custom-nodes --lowvram" }),
    [...BASE, ...YAML_FLAG, "--disable-all-custom-nodes", "--lowvram", "--whitelist-custom-nodes", EXT_IDLE_PACK],
  );
});

test("no --disable-all-custom-nodes, no whitelist: every custom node loads, the pack among them, and none is added", () => {
  const f = launchFlags({ idle: IDLE, extraArgs: "--lowvram" });
  assert.deepEqual(f, [...BASE, ...YAML_FLAG, "--lowvram"]);
  assert.ok(!f.includes("--whitelist-custom-nodes"), "adding a whitelist would restrict the loads to the pack alone");
});

test("a pack the operator already whitelisted is not listed twice", () => {
  assert.deepEqual(
    launchFlags({ idle: IDLE, extraArgs: `--disable-all-custom-nodes --whitelist-custom-nodes ${EXT_IDLE_PACK} x` }),
    [...BASE, ...YAML_FLAG, "--disable-all-custom-nodes", "--whitelist-custom-nodes", EXT_IDLE_PACK, "x"],
  );
});

// ComfyUI exits at boot on an --extra-model-paths-config file that does not exist (utils/extra_config.py opens it with no guard).

test("a missing yaml or pack file means no idle flags and one COMFY-IDLE-WARN, never a launch that cannot boot", (t) => {
  const dir = scratchDir(t, "comfy-idle-guard-");
  const make = (name, { yaml, pack }) => {
    const root = join(dir, name);
    const y = join(root, "extra_model_paths_idle.yaml");
    const p = join(root, "idle-nodes", EXT_IDLE_PACK, "__init__.py");
    mkdirSync(dirname(p), { recursive: true });
    if (yaml) writeFileSync(y, "offload_harness_idle:\n    custom_nodes: idle-nodes\n");
    if (pack) writeFileSync(p, "NODE_CLASS_MAPPINGS = {}\n");
    return { yaml: y, pack: EXT_IDLE_PACK, packFile: p };
  };
  for (const [name, spec, missing] of [["none", {}, "yaml"], ["yaml-only", { yaml: true }, "__init__.py"], ["pack-only", { pack: true }, "yaml"]]) {
    const idle = make(name, spec);
    const lines = [];
    assert.deepEqual(launchFlags({ idle, extraArgs: "--disable-all-custom-nodes", warn: (l) => lines.push(l) }), [...BASE, "--disable-all-custom-nodes"], name);
    assert.equal(count(lines, /^COMFY-IDLE-WARN/), 1, `${name}: ${JSON.stringify(lines)}`);
    assert.ok(lines[0].includes(missing), `${name}: the line names what is missing (${missing}): ${lines[0]}`);
  }
  const whole = make("whole", { yaml: true, pack: true });
  const lines = [];
  assert.deepEqual(launchFlags({ idle: whole, warn: (l) => lines.push(l) }), [...BASE, "--extra-model-paths-config", whole.yaml]);
  assert.deepEqual(lines, []);
});

// ---- ensureComfy: when it asks for the bound, and what the child gets ------------------------------------

function fakeChild() {
  const c = new EventEmitter();
  c.pid = 4242;
  c.unref = () => {};
  c.kill = () => {};
  c.stdout = { on() {} };
  c.stderr = { on() {} };
  return c;
}

/** Launch through ensureComfy with everything injected. `probe` answers GET /offload/idle. */
async function launch(t, { keep = false, warm = false, env = {}, envFor = () => ({}), probe, extra = {} } = {}) {
  const dir = scratchDir(t, "comfy-idle-launch-");
  const lines = [];
  const probes = [];
  let ups = 0;
  let seen = null;
  const child = fakeChild();
  const result = await ensureComfy({
    keep, warm, comfyDir: dir, env, api: API, py: "py", pollMs: 1, log: (l) => lines.push(l),
    comfyUp: async () => ups++ > 0,
    spawn: (py, args, opts) => { seen = { py, args, opts }; return child; },
    envFor, portFree: async () => true, mkdirs: () => {}, openLogFd: () => 5, closeFd: () => {}, writeLaunch: () => {},
    probeIdle: async (api) => { probes.push(api); return probe ? probe(api) : GOOD_ANSWER; },
    ...extra,
  });
  return { seen, lines, probes, child, result };
}

test("a kept launch asks for the idle bound: the yaml in argv, OFFLOAD_IDLE_FREE_SEC=300 in the child's env", async (t) => {
  const r = await launch(t, { keep: true });
  assert.deepEqual(r.seen.args, ["main.py", ...BASE, ...YAML_FLAG]);
  assert.equal(r.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "300");
  assert.equal(r.result, r.child, "the launched child is still what ensureComfy returns");
});

test("a warm launch asks for it too (and still omits --cache-none)", async (t) => {
  const r = await launch(t, { warm: true });
  assert.deepEqual(r.seen.args, ["main.py", ...WARM, ...YAML_FLAG]);
  assert.equal(r.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "300");
});

test("a launch that is neither kept nor warm is exactly what it was: no yaml, no env, no probe, no line", async (t) => {
  const r = await launch(t, {});
  assert.deepEqual(r.seen.args, ["main.py", ...BASE]);
  assert.ok(!("OFFLOAD_IDLE_FREE_SEC" in r.seen.opts.env), "its runner kills it: nothing to bound");
  assert.deepEqual(r.probes, []);
  assert.deepEqual(r.lines, []);
});

test("COMFY_IDLE_FREE_SEC shortens the bound; it reaches the child as OFFLOAD_IDLE_FREE_SEC", async (t) => {
  const r = await launch(t, { keep: true, env: { COMFY_IDLE_FREE_SEC: "120" } });
  assert.equal(r.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "120");
  assert.deepEqual(r.seen.args, ["main.py", ...BASE, ...YAML_FLAG]);
});

test("COMFY_IDLE_FREE_SEC=0 or off: no yaml, no env, no probe, one COMFY-IDLE-OFF line", async (t) => {
  for (const v of ["0", "off"]) {
    const r = await launch(t, { keep: true, env: { COMFY_IDLE_FREE_SEC: v } });
    assert.deepEqual(r.seen.args, ["main.py", ...BASE], v);
    assert.ok(!("OFFLOAD_IDLE_FREE_SEC" in r.seen.opts.env), v);
    assert.deepEqual(r.probes, [], v);
    assert.equal(count(r.lines, /^COMFY-IDLE-OFF/), 1, `${v}: ${JSON.stringify(r.lines)}`);
  }
});

test("a COMFY_IDLE_FREE_SEC it cannot use launches with 300 and says so once", async (t) => {
  const r = await launch(t, { keep: true, env: { COMFY_IDLE_FREE_SEC: "abc" } });
  assert.equal(r.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "300");
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
});

test("the child's env is a copy on top of the placement's: its other variables stay, the parent's env is not edited", async (t) => {
  const r = await launch(t, { keep: true, envFor: () => ({ FOO: "bar", CUDA_VISIBLE_DEVICES: "GPU-x" }) });
  assert.equal(r.seen.opts.env.FOO, "bar");
  assert.equal(r.seen.opts.env.CUDA_VISIBLE_DEVICES, "GPU-x");
  assert.equal(r.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "300");
  const parent = { A: "1" };
  const r2 = await launch(t, { keep: true, envFor: () => parent });
  assert.deepEqual(parent, { A: "1" }, "the env envFor returned is not written to");
  assert.equal(r2.seen.opts.env.A, "1");
});

test("adding the variable does not change the multi-GPU pinned-memory decision, whichever env the child inherits", async (t) => {
  // The decision compares the placement's env with process.env by identity: an env that IS process.env restored nothing.
  const had = process.env.CUDA_VISIBLE_DEVICES;
  process.env.CUDA_VISIBLE_DEVICES = "0,1";
  try {
    const same = await launch(t, { keep: true, envFor: () => process.env });
    assert.ok(!same.seen.args.includes("--disable-pinned-memory"), "process.env itself: no visibility was restored");
    assert.ok(!("OFFLOAD_IDLE_FREE_SEC" in process.env), "and process.env was not written to");
    assert.equal(same.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "300");
  } finally {
    if (had === undefined) delete process.env.CUDA_VISIBLE_DEVICES; else process.env.CUDA_VISIBLE_DEVICES = had;
  }
  const restored = await launch(t, { keep: true, envFor: () => ({ CUDA_VISIBLE_DEVICES: "0,1" }) });
  assert.ok(restored.seen.args.includes("--disable-pinned-memory"), "a restored multi-card visibility still gets upstream's flag");
  assert.equal(restored.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "300");
});

test("a launch whose idle files are missing launches without them: no env either, and one warning", async (t) => {
  const idleExt = { yaml: join(scratchDir(t, "comfy-idle-none-"), "nope.yaml"), pack: EXT_IDLE_PACK, packFile: join(scratchDir(t, "comfy-idle-none2-"), "nope.py") };
  const r = await launch(t, { keep: true, extra: { idleExt } });
  assert.deepEqual(r.seen.args, ["main.py", ...BASE]);
  assert.ok(!("OFFLOAD_IDLE_FREE_SEC" in r.seen.opts.env), "a variable for a pack that was not loaded");
  assert.deepEqual(r.probes, [], "nothing to ask");
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
});

test("the launch marker records the argv with the idle flags, which is what the holder's proof compares", async (t) => {
  const recorded = [];
  const r = await launch(t, { keep: true, extra: { writeLaunch: (dir, rec) => recorded.push(rec) } });
  assert.equal(recorded.length, 1);
  assert.deepEqual(recorded[0].args, r.seen.args);
});

// ---- ensureComfy: knowing the pack is there ---------------------------------------------------------------

test("after an idle launch the instance is asked once and an answer is said once", async (t) => {
  const r = await launch(t, { keep: true });
  assert.deepEqual(r.probes, [API]);
  assert.equal(count(r.lines, /^COMFY-IDLE:/), 1, JSON.stringify(r.lines));
  assert.match(r.lines.find((l) => /^COMFY-IDLE:/.test(l)), /300 s/);
  assert.equal(count(r.lines, /COMFY-IDLE-WARN/), 0);
});

test("an instance that does not answer GET /offload/idle is said, and the launch still succeeds", async (t) => {
  const r = await launch(t, { keep: true, probe: async () => null });
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
  assert.match(r.lines.find((l) => /^COMFY-IDLE-WARN/.test(l)), /\/offload\/idle/);
  assert.equal(r.result, r.child);
});

test("a probe that throws is said, and the launch still succeeds", async (t) => {
  const r = await launch(t, { warm: true, probe: boom("socket hang up") });
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
  assert.equal(r.result, r.child);
});

test("the COMFY-IDLE line states the bound the pack reports, which is the one that was asked for", async (t) => {
  const r = await launch(t, { keep: true, env: { COMFY_IDLE_FREE_SEC: "120" }, probe: async () => ({ ...GOOD_ANSWER, limit_sec: 120 }) });
  assert.equal(count(r.lines, /^COMFY-IDLE:/), 1, JSON.stringify(r.lines));
  assert.match(r.lines.find((l) => /^COMFY-IDLE:/.test(l)), /after 120 s/);
  assert.equal(count(r.lines, /COMFY-IDLE-WARN/), 0, JSON.stringify(r.lines));
});

test("a pack that reports another bound than the one asked for is a warning that names both, not a COMFY-IDLE line", async (t) => {
  // OFFLOAD_IDLE_FREE_SEC=120 went to the child; a pack that ignored it (or read another variable) answers its own 300.
  const r = await launch(t, { keep: true, env: { COMFY_IDLE_FREE_SEC: "120" } });
  assert.equal(r.seen.opts.env.OFFLOAD_IDLE_FREE_SEC, "120");
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
  const line = r.lines.find((l) => /^COMFY-IDLE-WARN/.test(l));
  assert.ok(line.includes("120") && line.includes("300"), `the warning names the bound asked for (120) and the one reported (300): ${line}`);
  assert.equal(count(r.lines, /^COMFY-IDLE:/), 0, "not claimed as the bound that was asked for");
  assert.equal(r.result, r.child, "the launch still succeeds");
});

test("an answer that does not carry limit_sec is taken to be the bound asked for", async (t) => {
  const r = await launch(t, { keep: true, env: { COMFY_IDLE_FREE_SEC: "120" }, probe: async () => ({ ...GOOD_ANSWER, limit_sec: undefined }) });
  assert.equal(count(r.lines, /COMFY-IDLE-WARN/), 0, JSON.stringify(r.lines));
  assert.match(r.lines.find((l) => /^COMFY-IDLE:/.test(l)), /after 120 s/);
});

test("a pack that answers but reports it cannot run is said with its reason", async (t) => {
  const r = await launch(t, { keep: true, probe: async () => ({ ...GOOD_ANSWER, error: "no prompt queue" }) });
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
  assert.match(r.lines.find((l) => /^COMFY-IDLE-WARN/.test(l)), /no prompt queue/);
  assert.equal(count(r.lines, /^COMFY-IDLE:/), 0, "not claimed as active");
});

// ---- ensureComfy: reusing a running instance ---------------------------------------------------------------

/** A ComfyUI that is already listening, reported to have `argv`; nothing may be launched or killed. */
async function reuse(t, { keep = false, warm = false, argv, env = {}, extra = {} } = {}) {
  const dir = scratchDir(t, "comfy-idle-reuse-");
  const lines = [];
  const asked = [];
  const result = await ensureComfy({
    keep, warm, comfyDir: dir, env, api: API, py: "py", pollMs: 1, log: (l) => lines.push(l),
    comfyUp: async () => true,
    systemArgv: async (api) => { asked.push(api); return argv; },
    spawn: boom("nothing is launched"), killPid: boom("never kill"),
    probeIdle: boom("a reused instance is not probed"),
    ...extra,
  });
  return { result, lines, asked };
}

test("a running instance with no idle pack is reused under keep, with one warning: no restart, no refusal", async (t) => {
  const r = await reuse(t, { keep: true, argv: ["main.py", ...BASE] });
  assert.equal(r.result, null, "reused, as it always was");
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
  assert.match(r.lines.find((l) => /^COMFY-IDLE-WARN/.test(l)), /no idle bound/);
  assert.deepEqual(r.asked, [API]);
});

test("the same under warm", async (t) => {
  const r = await reuse(t, { warm: true, argv: ["main.py", ...WARM] });
  assert.equal(r.result, null);
  assert.equal(count(r.lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(r.lines));
});

test("a running instance that has the idle yaml, however its path is spelled, gets no warning", async (t) => {
  for (const yaml of [EXT_IDLE_YAML, "/opt/offload/render/comfyui-ext/extra_model_paths_idle.yaml", "C:\\x\\comfyui-ext\\extra_model_paths_idle.yaml"]) {
    const r = await reuse(t, { keep: true, argv: ["main.py", ...BASE, "--extra-model-paths-config", yaml] });
    assert.equal(r.result, null);
    assert.deepEqual(r.lines, [], yaml);
  }
});

test("an argv ComfyUI does not report is unknown, not missing: no warning", async (t) => {
  const r = await reuse(t, { keep: true, argv: null });
  assert.equal(r.result, null);
  assert.deepEqual(r.lines, []);
});

test("a reuse that is neither kept nor warm is not even asked for its argv", async (t) => {
  const r = await reuse(t, { argv: ["main.py", ...BASE] });
  assert.equal(r.result, null);
  assert.deepEqual(r.asked, []);
  assert.deepEqual(r.lines, []);
});

test("with the bound switched off a kept reuse is silent about it (one COMFY-IDLE-OFF) and asks nothing", async (t) => {
  const r = await reuse(t, { keep: true, argv: ["main.py", ...BASE], env: { COMFY_IDLE_FREE_SEC: "0" } });
  assert.equal(r.result, null);
  assert.deepEqual(r.asked, []);
  assert.equal(count(r.lines, /COMFY-IDLE-WARN/), 0);
  assert.equal(count(r.lines, /^COMFY-IDLE-OFF/), 1);
});

test("a keyed kept instance the marker proves is ours, launched without the pack, is reused and warned about once", async (t) => {
  const dir = scratchDir(t, "comfy-idle-keyed-");
  const argv = ["main.py", ...BASE, "--port", "8190", "--output-directory", join(dir, "instances", "side", "output"),
    "--temp-directory", join(dir, "instances", "side")];
  O.writeLaunchOwner(dir, { pid: 77, ownerPid: 5151, args: argv, profile: { cudaDevice: "", dynamicVram: "" }, key: "side", port: 8190, leaseEpoch: 5 });
  const lines = [];
  let asked = 0;
  const result = await ensureComfy({
    keep: true, comfyDir: dir, env: { COMFY_INSTANCE: "side", GPU_LEASE_EPOCH: "5" }, api: "http://127.0.0.1:8190", py: "py", pollMs: 1,
    log: (l) => lines.push(l), comfyUp: async () => true, systemArgv: async () => { asked++; return argv; }, alive: (p) => p === 77, envFor: () => ({}),
    portFree: boom("an answering ComfyUI is never bind-probed"), mkdirs: boom("no directories for a reused instance"),
    spawn: boom("nothing is launched"), killPid: boom("never kill"), probeIdle: boom("a reused instance is not probed"),
  });
  assert.equal(result, null);
  assert.equal(count(lines, /^COMFY-IDLE-WARN/), 1, JSON.stringify(lines));
  assert.match(lines.find((l) => /^COMFY-IDLE-WARN/.test(l)), /side/);
  // reuseVerdict already read /system_stats to prove the instance is this one; the warning reads the same answer, not a second request
  // (each is an 8 s wait on an instance that answers slowly).
  assert.equal(asked, 1, "one /system_stats request for the whole reuse decision");
});

// ---- fetchIdleProbe: the real request -------------------------------------------------------------------

async function serve(handler) {
  const seen = [];
  const server = createServer((req, res) => { seen.push(`${req.method} ${req.url}`); handler(req, res); });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    api: `http://127.0.0.1:${server.address().port}`, seen,
    close: () => new Promise((resolve) => { server.closeAllConnections?.(); server.close(resolve); }),
  };
}

test("fetchIdleProbe: GET /offload/idle, the JSON object it answers, or null for anything else", async () => {
  const ok = await serve((req, res) => { res.setHeader("content-type", "application/json"); res.end(JSON.stringify(GOOD_ANSWER)); });
  try {
    assert.deepEqual(await fetchIdleProbe(ok.api), GOOD_ANSWER);
    assert.deepEqual(ok.seen, ["GET /offload/idle"]);
  } finally { await ok.close(); }

  for (const [name, handler] of [
    ["a 404", (req, res) => { res.statusCode = 404; res.end("{}"); }],
    ["text", (req, res) => { res.end("not json"); }],
    ["a JSON array", (req, res) => { res.end("[1]"); }],
    ["JSON null", (req, res) => { res.end("null"); }],
  ]) {
    const s = await serve(handler);
    try { assert.equal(await fetchIdleProbe(s.api), null, name); } finally { await s.close(); }
  }

  const gone = await serve((req, res) => res.end("{}"));
  const api = gone.api;
  await gone.close();
  assert.equal(await fetchIdleProbe(api), null, "nothing listening");
});
