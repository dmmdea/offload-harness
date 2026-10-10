// node --test render/comfy-video.test.mjs
// comfy-video.mjs arg parsing, graph building and the pre-submit node-class preflight,
// with ComfyUI STUBBED — no render, no network, no GPU. Importing the runner has no side
// effects (it runs main() only when invoked as the script).
import { test } from "node:test";
import assert from "node:assert";
import { readFileSync, mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  BOOL_FLAGS, parseArgs, wanVvramGb, wanDecodeMode, readRenderCard, runsWanGraph,
  buildGraphFromArgs, buildGraphForRun, submitChecked,
} from "./comfy-video.mjs";

const stage = (p) => "staged_" + p; // stands in for the copy into <COMFY_DIR>/input

// A fake /object_info: answers 200 with the class present, or 200 {} for a class the
// server does not have (what ComfyUI returns for an unknown class).
function objectInfo(present) {
  const calls = [];
  const fetchImpl = async (url) => {
    calls.push(url);
    const cls = decodeURIComponent(url.split("/object_info/")[1]);
    return { ok: true, status: 200, json: async () => (present.has(cls) ? { [cls]: { input: {} } } : {}) };
  };
  return { fetchImpl, calls };
}

test("parseArgs: --fast is a boolean, trailing or mid-argv (it used to swallow the next token)", () => {
  // Trailing: the harness appends --fast after every value flag.
  let { pos, flags } = parseArgs(["o.mp4", "s.png", "push in", "--seed", "42", "--fast"]);
  assert.equal(flags.fast, true);
  assert.equal(flags.seed, "42");
  assert.deepEqual(pos, ["o.mp4", "s.png", "push in"]);
  // Mid-argv, followed by a value flag: that flag must keep its value.
  ({ pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--fast", "--upscale-model", "4x.pth"]));
  assert.equal(flags.fast, true);
  assert.equal(flags["upscale-model"], "4x.pth");
  assert.deepEqual(pos, ["o.mp4", "s.png", "p"], "the upscale model name must not become a positional");
});

test("buildGraphFromArgs: --fast selects the lightx2v LoRA recipe; absent = the native recipe", () => {
  const fast = buildGraphFromArgs(...Object.values(parseArgs(["o.mp4", "s.png", "p", "--seed", "1", "--fast"])), { stage }).graph;
  const loras = Object.values(fast).filter((n) => n.class_type === "LoraLoaderModelOnly");
  assert.equal(loras.length, 2, "fast mode loads both distill LoRAs");
  const native = buildGraphFromArgs(...Object.values(parseArgs(["o.mp4", "s.png", "p", "--seed", "1"])), { stage }).graph;
  assert.equal(Object.values(native).filter((n) => n.class_type === "LoraLoaderModelOnly").length, 0);
});

test("--wan-vvram-gb threads into both Wan DisTorch2 loaders; absent keeps the builder default", () => {
  const split = (argv) => {
    const { pos, flags } = parseArgs(argv);
    const g = buildGraphFromArgs(pos, flags, { stage }).graph;
    return [g["7"].inputs.virtual_vram_gb, g["9"].inputs.virtual_vram_gb];
  };
  assert.deepEqual(split(["o.mp4", "s.png", "p", "--wan-vvram-gb", "4.5"]), [4.5, 4.5]);
  assert.deepEqual(split(["o.mp4", "s.png", "p"]), [7, 7], "the builder default is unchanged");
});

test("--wan-loader threads through to the builder's loader choice", () => {
  const classesFor = (argv) => {
    const { pos, flags } = parseArgs(argv);
    const g = buildGraphFromArgs(pos, flags, { stage }).graph;
    return new Set([g["7"].class_type, g["9"].class_type]);
  };
  // default filenames are GGUF: --wan-loader native must refuse them
  assert.throws(
    () => buildGraphFromArgs(...Object.values(parseArgs(["o.mp4", "s.png", "p", "--wan-loader", "native"])), { stage }),
    /loader:"native" cannot load a GGUF expert/,
  );
  // with safetensors experts bound, native produces the plain loader, no flag = unchanged (GGUF/DisTorch2)
  const nativeArgv = ["o.mp4", "s.png", "p", "--high-unet", "high.safetensors", "--low-unet", "low.safetensors", "--wan-loader", "native"];
  assert.deepEqual(classesFor(nativeArgv), new Set(["UNETLoader"]));
  assert.deepEqual(classesFor(["o.mp4", "s.png", "p"]), new Set(["UnetLoaderGGUFDisTorch2MultiGPU"]), "unset --wan-loader keeps the historical default");
});

test("wanVvramGb refuses a value that is not a positive number", () => {
  assert.equal(wanVvramGb({}), undefined);
  assert.equal(wanVvramGb({ "wan-vvram-gb": "11" }), 11);
  for (const bad of ["0", "-3", "seven", ""]) {
    assert.throws(() => wanVvramGb({ "wan-vvram-gb": bad }), /--wan-vvram-gb must be a positive number/, `value ${JSON.stringify(bad)}`);
  }
});

test("submitChecked: a class the server lacks is a MISSING_NODE defer naming the pack, and nothing is POSTed", async () => {
  const { pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--seed", "1"]);
  const { graph } = buildGraphFromArgs(pos, flags, { stage });
  const all = new Set(Object.values(graph).map((n) => n.class_type));
  all.delete("VHS_VideoCombine");
  const { fetchImpl } = objectInfo(all);
  let submitted = 0;
  await assert.rejects(
    submitChecked({ api: "http://comfy.test", graph, clientId: "c", cli: null, fetchImpl, submit: async () => { submitted++; return { promptId: "x" }; }, comfyDir: "D:/ComfyUI" }),
    (e) => {
      assert.match(e.message, /^MISSING_NODE: /);
      assert.match(e.message, /VHS_VideoCombine \(custom node pack ComfyUI-VideoHelperSuite/);
      assert.match(e.message, /D:\/ComfyUI\/custom_nodes/);
      return true;
    },
  );
  assert.equal(submitted, 0, "the graph must never reach the POST");
});

test("submitChecked: every class present -> submitted once, each class asked about once", async () => {
  const { pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--seed", "1"]);
  const { graph } = buildGraphFromArgs(pos, flags, { stage });
  const classes = new Set(Object.values(graph).map((n) => n.class_type));
  const { fetchImpl, calls } = objectInfo(classes);
  let submitted = 0;
  const res = await submitChecked({ api: "http://comfy.test", graph, clientId: "c", cli: null, fetchImpl, submit: async () => { submitted++; return { promptId: "p1" }; } });
  assert.equal(res.promptId, "p1");
  assert.equal(submitted, 1);
  assert.equal(calls.length, classes.size);
});

test("submitChecked: an unreadable /object_info steps aside and lets the submission speak", async () => {
  const graph = { "1": { class_type: "VHS_VideoCombine", inputs: {} } };
  const fetchImpl = async () => { throw new Error("ECONNREFUSED"); };
  let submitted = 0;
  await submitChecked({ api: "http://comfy.test", graph, clientId: "c", cli: null, fetchImpl, submit: async () => { submitted++; return { promptId: "p" }; } });
  assert.equal(submitted, 1);
});

// --- --wan-decode (config videogen_wan_decode): auto | plain | tiled -----------------------------------
// Measured on a 16 GB card (A/B 2026-10-03): plain VAEDecode 38 s at a 10.3 GB peak, tiled 412 s at 3.2 GB.
// ComfyUI is stubbed throughout: no render, no GPU, no ComfyUI process.

const GIB = 1024 ** 3;
const API = "http://comfy.test";

// A fake GET /system_stats: answers with `body` (or the given status) and records every URL asked.
function systemStats(body, { status = 200 } = {}) {
  const calls = [];
  const fetchImpl = async (url) => {
    calls.push(String(url));
    return { ok: status >= 200 && status < 300, status, json: async () => body };
  };
  return { fetchImpl, calls };
}
// What a ComfyUI with one render card answers (the shape of /system_stats devices[]).
const oneCard = (bytes, extra = {}) => ({ devices: [{ name: "cuda:0 Test Card", type: "cuda", index: 0, vram_total: bytes, vram_free: bytes, ...extra }] });
const decodeNode = (graph) => Object.values(graph).map((n) => n.class_type).find((c) => /^VAEDecode/.test(c));
// buildGraphForRun with the card reading stubbed and the log captured.
async function runFor(argv, fetchImpl) {
  const logs = [];
  const { pos, flags } = parseArgs(argv);
  const built = await buildGraphForRun(pos, flags, API, { stage, fetchImpl, log: (m) => logs.push(m) });
  return { ...built, logs };
}
const WAN_ARGV = ["o.mp4", "s.png", "p", "--seed", "1"];

test("parseArgs: --wan-decode is a value flag (not a BOOL_FLAG) and leaves the flags around it alone", () => {
  assert.ok(!BOOL_FLAGS.includes("wan-decode"), "a bool flag would swallow nothing and read the mode as a positional");
  const { pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--wan-decode", "plain", "--fast", "--seed", "3"]);
  assert.equal(flags["wan-decode"], "plain");
  assert.equal(flags.fast, true);
  assert.equal(flags.seed, "3");
  assert.deepEqual(pos, ["o.mp4", "s.png", "p"]);
});

test("wanDecodeMode: absent is auto, the three modes pass, and anything else (empty, dangling, wrong case) is refused", () => {
  assert.equal(wanDecodeMode({}), "auto");
  for (const m of ["auto", "plain", "tiled"]) assert.equal(wanDecodeMode({ "wan-decode": m }), m);
  for (const bad of ["fast", "", "Plain", "VAEDecode", undefined]) {
    assert.throws(() => wanDecodeMode({ "wan-decode": bad }), /--wan-decode must be auto\|plain\|tiled/, `value ${JSON.stringify(bad)}`);
  }
  // A trailing --wan-decode with no value parses to undefined; it is a mistake, not "auto".
  assert.throws(() => wanDecodeMode(parseArgs(["o.mp4", "s.png", "p", "--wan-decode"]).flags), /--wan-decode must be/);
});

test("--wan-decode threads through buildGraphFromArgs to the Wan builder; without a card reading, auto builds the tiled decode", () => {
  const graphFor = (argv, opts = {}) => buildGraphFromArgs(...Object.values(parseArgs(argv)), { stage, ...opts }).graph;
  assert.equal(decodeNode(graphFor([...WAN_ARGV, "--wan-decode", "plain"])), "VAEDecode");
  assert.equal(decodeNode(graphFor([...WAN_ARGV, "--wan-decode", "tiled"])), "VAEDecodeTiled");
  assert.equal(decodeNode(graphFor(WAN_ARGV)), "VAEDecodeTiled", "no flag and no reading: auto stays on today's node");
  assert.equal(decodeNode(graphFor([...WAN_ARGV, "--wan-decode", "auto"], { vramTotalBytes: 16 * GIB })), "VAEDecode");
  assert.equal(decodeNode(graphFor(WAN_ARGV, { vramTotalBytes: 16 * GIB })), "VAEDecode", "the absent flag is auto");
  assert.equal(decodeNode(graphFor([...WAN_ARGV, "--wan-decode", "tiled"], { vramTotalBytes: 16 * GIB })), "VAEDecodeTiled", "an explicit mode beats the card");
  assert.throws(() => graphFor([...WAN_ARGV, "--wan-decode", "bogus"]), /--wan-decode must be auto\|plain\|tiled/);
});

test("--wan-decode is Wan-only: hunyuan and ltx25 keep their VAEDecodeTiled whatever it says", () => {
  for (const model of ["hunyuan", "ltx25"]) {
    const { graph } = buildGraphFromArgs(...Object.values(parseArgs([...WAN_ARGV, "--model", model, "--wan-decode", "plain"])), { stage, vramTotalBytes: 16 * GIB });
    const classes = Object.values(graph).map((n) => n.class_type);
    assert.ok(classes.includes("VAEDecodeTiled"), `${model} still decodes tiled`);
    assert.ok(!classes.includes("VAEDecode"), `${model} must not gain a plain VAEDecode`);
    assert.ok(!classes.includes("WanImageToVideo"), `${model} is not the Wan graph`);
  }
});

test("readRenderCard reads devices[0].vram_total from GET <api>/system_stats: the primary device, which ComfyUI lists first", async () => {
  const { fetchImpl, calls } = systemStats({ devices: [
    { name: "cuda:1 Primary", type: "cuda", index: 1, vram_total: 16 * GIB },
    { name: "cuda:0 Donor", type: "cuda", index: 0, vram_total: 48 * GIB },
  ] });
  assert.deepStrictEqual(await readRenderCard(API, { fetchImpl }), { vramTotal: 16 * GIB, name: "cuda:1 Primary" });
  assert.deepStrictEqual(calls, [`${API}/system_stats`]);
});

test("readRenderCard: every answer that does not name a card is an error, never a guess", async () => {
  const cases = {
    "HTTP 500": systemStats({}, { status: 500 }),
    "no devices key": systemStats({ system: {} }),
    "empty devices": systemStats({ devices: [] }),
    "devices is not an array": systemStats({ devices: { 0: oneCard(16 * GIB).devices[0] } }),
    "null device": systemStats({ devices: [null] }),
    "vram_total missing": systemStats({ devices: [{ name: "x", type: "cuda" }] }),
    "vram_total a string": systemStats(oneCard("17179869184")),
    "vram_total zero": systemStats(oneCard(0)),
    "vram_total negative": systemStats(oneCard(-1)),
    "vram_total NaN": systemStats(oneCard(NaN)),
    "a CPU device (its vram_total is the host's RAM)": systemStats(oneCard(64 * GIB, { type: "cpu" })),
    "an MPS device (its vram_total is the host's RAM)": systemStats(oneCard(64 * GIB, { type: "mps" })),
  };
  for (const [name, { fetchImpl }] of Object.entries(cases)) {
    const r = await readRenderCard(API, { fetchImpl });
    assert.ok(typeof r.error === "string" && r.error, `${name}: names why`);
    assert.equal(r.vramTotal, undefined, `${name}: carries no size`);
  }
  const refused = await readRenderCard(API, { fetchImpl: async () => { throw new Error("ECONNREFUSED"); } });
  assert.match(refused.error, /GET \/system_stats failed: ECONNREFUSED/);
  const noJson = await readRenderCard(API, { fetchImpl: async () => ({ ok: true, status: 200, json: async () => { throw new SyntaxError("Unexpected token"); } }) });
  assert.match(noJson.error, /returned no JSON/);
  assert.match((await readRenderCard(API, { fetchImpl: systemStats({}, { status: 503 }).fetchImpl })).error, /answered HTTP 503/);
});

test("auto: a 16 GiB card builds VAEDecode, an 8 GiB card VAEDecodeTiled, an unreadable one VAEDecodeTiled — and the log says which and why", async () => {
  const big = await runFor(WAN_ARGV, systemStats(oneCard(16 * GIB)).fetchImpl);
  assert.equal(decodeNode(big.graph), "VAEDecode");
  assert.equal(big.logs.length, 1, "one line per run");
  assert.match(big.logs[0], /^wan-decode: VAEDecode \(auto: the render card reports 16\.0 GiB of VRAM, at least the 12\.0 GiB the plain decode wants; card cuda:0 Test Card\)$/);

  const small = await runFor(WAN_ARGV, systemStats(oneCard(8 * GIB)).fetchImpl);
  assert.equal(decodeNode(small.graph), "VAEDecodeTiled");
  assert.match(small.logs[0], /^wan-decode: VAEDecodeTiled \(auto: the render card reports 8\.0 GiB of VRAM, under the 12\.0 GiB the plain decode wants; card cuda:0 Test Card\)$/);

  for (const [label, fetchImpl, reason] of [
    ["the request fails", async () => { throw new Error("ECONNREFUSED"); }, /GET \/system_stats failed: ECONNREFUSED/],
    ["HTTP 500", systemStats({}, { status: 500 }).fetchImpl, /answered HTTP 500/],
    ["no devices", systemStats({ devices: [] }).fetchImpl, /lists no device/],
  ]) {
    const none = await runFor(WAN_ARGV, fetchImpl);
    assert.equal(decodeNode(none.graph), "VAEDecodeTiled", `${label}: today's node`);
    assert.match(none.logs[0], /^wan-decode: VAEDecodeTiled \(auto: the render card's VRAM was not read, so the tiled decode this graph always used stays; /, label);
    assert.match(none.logs[0], reason, label);
  }
});

test("auto cuts at 12 GiB inclusive through the whole path, and a missing flag is auto", async () => {
  assert.equal(decodeNode((await runFor(WAN_ARGV, systemStats(oneCard(12 * GIB)).fetchImpl)).graph), "VAEDecode");
  assert.equal(decodeNode((await runFor(WAN_ARGV, systemStats(oneCard(12 * GIB - 1)).fetchImpl)).graph), "VAEDecodeTiled");
  assert.equal(decodeNode((await runFor([...WAN_ARGV, "--wan-decode", "auto"], systemStats(oneCard(16 * GIB)).fetchImpl)).graph), "VAEDecode");
});

test("an explicit --wan-decode reads no card: zero requests, and the log names the request", async () => {
  for (const [mode, node] of [["plain", "VAEDecode"], ["tiled", "VAEDecodeTiled"]]) {
    const s = systemStats(oneCard(mode === "plain" ? 8 * GIB : 24 * GIB)); // a card that would choose the opposite
    const r = await runFor([...WAN_ARGV, "--wan-decode", mode], s.fetchImpl);
    assert.equal(decodeNode(r.graph), node, mode);
    assert.equal(s.calls.length, 0, `${mode} must make no request`);
    assert.deepStrictEqual(r.logs, [`wan-decode: ${node} (${mode} decode requested)`]);
  }
});

test("a run that does not build the Wan graph reads no card and logs nothing", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wan-decode-"));
  const graphFile = join(dir, "wf.json");
  writeFileSync(graphFile, JSON.stringify({ "1": { class_type: "LoadImage", inputs: { image: "x.png" } } }));
  const argvs = [
    [...WAN_ARGV, "--model", "hunyuan"],
    [...WAN_ARGV, "--model", "ltx25"],
    [...WAN_ARGV, "--model", "h3"],
    ["o.flac", "upbeat corporate", "--model", "ace"],
    ["o.mp4", "--graph", graphFile],
  ];
  const realLog = console.log;
  console.log = () => {}; // the ace branch announces its prompt on stdout
  try {
    for (const argv of argvs) {
      const s = systemStats(oneCard(16 * GIB));
      const r = await runFor(argv, s.fetchImpl);
      assert.equal(s.calls.length, 0, `${argv.join(" ")}: no /system_stats request`);
      assert.deepStrictEqual(r.logs, [], `${argv.join(" ")}: no wan-decode line`);
    }
  } finally { console.log = realLog; }
});

test("a bad --wan-decode on a Wan run throws before the still is staged; on a non-Wan run it is not read", async () => {
  const staged = [];
  const { pos, flags } = parseArgs([...WAN_ARGV, "--wan-decode", "bogus"]);
  await assert.rejects(
    buildGraphForRun(pos, flags, API, { stage: (p) => { staged.push(p); return "x"; }, fetchImpl: systemStats({}).fetchImpl, log: () => {} }),
    /--wan-decode must be auto\|plain\|tiled/,
  );
  assert.deepEqual(staged, [], "nothing was copied into ComfyUI's input dir");
  const ok = await runFor([...WAN_ARGV, "--model", "ltx25", "--wan-decode", "bogus"], systemStats({}).fetchImpl);
  assert.equal(decodeNode(ok.graph), "VAEDecodeTiled", "ltx25 never reads the flag");
});

test("a run buildGraphFromArgs would refuse for a missing still or prompt reads no card: it exits right after", async () => {
  // process.exit(2) is how the runner refuses; stand it in with a throw so the test can see it happen.
  const realExit = process.exit;
  const realErr = console.error;
  process.exit = (code) => { throw new Error(`exit ${code}`); };
  console.error = () => {};
  try {
    for (const argv of [["o.mp4", "only-one-positional"], ["o.mp4"]]) {
      const s = systemStats(oneCard(16 * GIB));
      const { pos, flags } = parseArgs(argv);
      await assert.rejects(buildGraphForRun(pos, flags, API, { stage, fetchImpl: s.fetchImpl, log: () => {} }), /exit 2/, argv.join(" "));
      assert.equal(s.calls.length, 0, `${argv.join(" ")}: no request before the refusal`);
    }
  } finally { process.exit = realExit; console.error = realErr; }
});

test("runsWanGraph agrees with the dispatch in buildGraphFromArgs: every family it dispatches on is non-Wan, anything else renders Wan", () => {
  const src = readFileSync(new URL("./comfy-video.mjs", import.meta.url), "utf8");
  const dispatched = new Set([...src.matchAll(/\bmodel === "([a-z0-9._-]+)"/g)].map((m) => m[1]));
  assert.ok(dispatched.size >= 4, `found the dispatch literals in comfy-video.mjs (got ${[...dispatched]})`);
  for (const fam of dispatched) assert.equal(runsWanGraph({ model: fam }), false, `${fam} is built by another builder`);
  const listed = src.match(/NON_WAN_MODELS = Object\.freeze\(\[([^\]]*)\]\)/);
  assert.ok(listed, "NON_WAN_MODELS is declared");
  const nonWan = [...listed[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
  assert.deepStrictEqual([...nonWan].sort(), [...dispatched].sort(), "the list and the dispatch name the same families");
  // The Wan builder is the fall-through: no model, the spelled-out names and an unknown value all reach it.
  for (const model of [undefined, "wan", "wan22", "something-new"]) assert.equal(runsWanGraph({ model }), true, String(model));
  assert.equal(runsWanGraph({ graph: "wf.json" }), false, "a --graph file is the caller's own graph");
  assert.equal(runsWanGraph({ graph: "wf.json", model: "wan" }), false);
});

test("buildGraphForRun over real HTTP: a stand-in ComfyUI answering /system_stats with a 16 GiB card gets the plain decode", async () => {
  const hits = [];
  const srv = createServer((req, res) => {
    hits.push(req.url);
    if (req.url === "/system_stats") {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ system: { os: "nt" }, devices: oneCard(17_094_934_528).devices }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  try {
    const { port } = srv.address();
    assert.ok(port < 8188 || port > 8191, "the stand-in must never sit on 8188-8191");
    const logs = [];
    const { pos, flags } = parseArgs(WAN_ARGV);
    // No fetchImpl: this is the runner's own transport, global fetch with its timeout signal.
    const r = await buildGraphForRun(pos, flags, `http://127.0.0.1:${port}`, { stage, log: (m) => logs.push(m) });
    assert.equal(decodeNode(r.graph), "VAEDecode");
    assert.deepStrictEqual(hits, ["/system_stats"]);
    assert.match(logs[0], /^wan-decode: VAEDecode \(auto: the render card reports 15\.9 GiB of VRAM/);
  } finally {
    srv.closeAllConnections?.();
    await new Promise((r) => srv.close(r));
  }
});

// --- the glue and the two defaults the tests above could not see ------------------------------------------
// generate() is the only caller of buildGraphForRun and only main() calls generate(); main() takes the GPU
// lease and frees the inference server, so no test runs it. A refactor that put back the plain
// buildGraphFromArgs call, or dropped the await, would leave every test above green while auto quietly became
// "always tiled" (today's behaviour on the 16 GB tiers, so nothing would look broken). Pin the glue the way
// the runsWanGraph test pins the dispatch: read the source, code only, so a comment cannot satisfy the pin.
test("generate() takes its graph from buildGraphForRun, the one call that lets --wan-decode auto read the card", () => {
  const src = readFileSync(new URL("./comfy-video.mjs", import.meta.url), "utf8");
  const from = src.indexOf("async function generate(");
  const to = src.indexOf("async function main(");
  assert.ok(from >= 0 && to > from, "generate() is declared before main() in comfy-video.mjs");
  const code = src.slice(from, to).replace(/\/\/[^\n]*/g, "");
  assert.match(code, /const \{ graph, seed, model \} = await buildGraphForRun\(pos, flags, API\);/, "generate() awaits buildGraphForRun(pos, flags, API) for its graph");
  assert.doesNotMatch(code, /buildGraphFromArgs\(/, "generate() must not build the graph around the card read");
  assert.match(code, /submitChecked\(\{ api: API, graph,/, "and it submits that graph");
});

test("buildGraphForRun's default log is stderr: one wan-decode line there, nothing on stdout", async () => {
  // Every test above injects log. The choice goes to stderr (stdout is where the runner's own queued/WROTE
  // lines go), and the default is the one thing a refactor could move without a test noticing.
  const errs = [], outs = [];
  const realErr = console.error, realLog = console.log;
  console.error = (...a) => errs.push(a.join(" "));
  console.log = (...a) => outs.push(a.join(" "));
  try {
    const { pos, flags } = parseArgs(WAN_ARGV);
    await buildGraphForRun(pos, flags, API, { stage, fetchImpl: systemStats(oneCard(16 * GIB)).fetchImpl });
  } finally { console.error = realErr; console.log = realLog; }
  assert.equal(errs.length, 1, "one stderr line per Wan run");
  assert.match(errs[0], /^wan-decode: VAEDecode \(auto: the render card reports 16\.0 GiB/);
  assert.deepStrictEqual(outs, [], "nothing on stdout");
});

test("readRenderCard stops waiting on a ComfyUI that takes the request and never answers, so the run goes on tiled instead of holding the GPU lease", async () => {
  // main() runs the read inside withGpuSlot: a stalled /system_stats would hold the lease for the whole
  // videogen timeout. The stub fetchImpl of the tests above ignores the signal, so only a real socket shows it.
  const srv = createServer(() => {}); // takes the request, never answers
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  let guard;
  try {
    const { port } = srv.address();
    assert.ok(port < 8188 || port > 8191, "the stand-in must never sit on 8188-8191");
    // The race is the test's own bound: with no abort signal the read waits forever, and a hang freezes the run
    // instead of failing it.
    const r = await Promise.race([
      readRenderCard(`http://127.0.0.1:${port}`, { timeoutMs: 50 }),
      new Promise((_, rej) => { guard = setTimeout(() => rej(new Error("readRenderCard ignored its timeout: still waiting after 1500 ms")), 1500); }),
    ]);
    assert.match(r.error, /^GET \/system_stats failed: /);
    assert.equal(r.vramTotal, undefined);
  } finally {
    clearTimeout(guard);
    srv.closeAllConnections?.();
    await new Promise((r) => srv.close(r));
  }
});

test("readRenderCard's default bound is seconds: it hands fetch an abort signal built from a short, finite timeout", async () => {
  // buildGraphForRun does not forward timeoutMs, so production always runs on this default; pinning only an
  // explicit value would let the default drift to minutes, or go, unnoticed.
  const realTimeout = AbortSignal.timeout;
  const asked = [], signals = [];
  AbortSignal.timeout = (ms) => { asked.push(ms); return realTimeout.call(AbortSignal, ms); };
  try {
    const fetchImpl = async (_url, init) => { signals.push(init?.signal); return { ok: true, status: 200, json: async () => oneCard(16 * GIB) }; };
    assert.equal((await readRenderCard(API, { fetchImpl })).vramTotal, 16 * GIB);
  } finally { AbortSignal.timeout = realTimeout; }
  assert.equal(signals.length, 1);
  assert.ok(signals[0] instanceof AbortSignal, "the request carries an abort signal");
  assert.equal(asked.length, 1, "built from exactly one timeout");
  assert.ok(Number.isFinite(asked[0]) && asked[0] > 0 && asked[0] <= 15_000, `a few seconds, never the run's whole timeout (got ${asked[0]} ms)`);
});
