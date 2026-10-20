// node --test render/wan-multicard-run.test.mjs
// The glue of the card-only placement in comfy-video.mjs: planRun (what main() decides before it takes the GPU) and generate() (what it does
// once it holds it), with every call to the outside world replaced by a recording fake. wan-multicard.test.mjs covers the placement's own
// functions; this file covers the places they are wired in, which a one-line change can break without failing any of them.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generate, planRun, seamedPath, GENERATE_IO, buildGraphForRun, submitChecked, verifyMulticardDevices, verifyMulticardPack } from "./comfy-video.mjs";
import { fetchView, finalizeRun, resolveCli } from "./comfy-submit.mjs";
import { comfyApi, comfyLogCursor } from "./comfy-lifecycle.mjs";
import { writeFileAtomic } from "./atomic-out.mjs";
import * as M from "./wan-multicard.mjs";

// Synthetic ids (the same placeholders wan-multicard.test.mjs uses): none names a real card.
const U0 = "GPU-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const U2 = "GPU-aaaa1111-dddd-eeee-ffff-000000000000";
const CARDS = M.parseCardTable([
  `0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`,
  `1, ${U1}, Test GPU B, 16303, 13800, Disabled, Yes`,
  `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`,
].join("\n"));
const LEASE = { GPU_LEASE_DEVICES: "gpu-aaaa1111-dddd-eeee-ffff-000000000000,gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee" };
const ARGV = ["o.mp4", "s.png", "p", "--wan-placement", "multicard", "--high-unet", "high_fp8.safetensors", "--low-unet", "low_fp8.safetensors"];
// planMulticard on the synthetic table, the way main() calls it but with the card table and the pack scan replaced
const planFn = (a) => M.planMulticard({
  ...a, cards: () => CARDS,
  launchFn: (p) => M.multicardLaunch(p, { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] }),
});

// ---- planRun: what main() decides before it takes the GPU ------------------------------------------------------

test("planRun: the single path asks for nothing of the card-only placement, and the lease slot carries no launch", () => {
  const boom = () => { throw new Error("the card table must not be read"); };
  const run = planRun(["o.mp4", "s.png", "p", "--model", "wan", "--seed", "7"], { env: {}, planFn: (a) => M.planMulticard({ ...a, cards: boom }) });
  assert.equal(run.multicard, null);
  assert.equal(run.out, "o.mp4");
  assert.equal(run.note, "");
  assert.equal(run.api, "http://127.0.0.1:8188", "the default instance, untouched");
  assert.ok(!("launch" in run.slot) && !("instanceEnv" in run.slot), "no launch, no instance environment");
  assert.deepEqual(Object.keys(run.slot).sort(), ["api", "comfyManaged", "family", "keepComfy", "noLock", "reserveVram"]);
  assert.equal(run.slot.family, "video-wan");
});

test("planRun: the card-only plan's launch and environment ride in the lease slot, and the api comes from THAT environment", () => {
  const seen = [];
  const run = planRun(ARGV, { env: { ...LEASE }, planFn: (a) => { const p = planFn(a); seen.push(p); return p; }, say: () => {} });
  assert.ok(run.multicard && run.multicard === seen[0], "main() plans the placement once and uses that plan");
  assert.equal(run.slot.launch, run.multicard.launch, "withGpuSlot launches ComfyUI with the placement's flags");
  assert.equal(run.slot.instanceEnv, run.multicard.env, "and with the placement's environment (the instance key, the pins dropped)");
  assert.equal(run.slot.api, run.api);
  assert.equal(run.api, comfyApi(undefined, run.multicard.env), "the api is the instance's own port, read from the plan's environment");
  assert.notEqual(new URL(run.api).port, "8188", "never the unkeyed instance's port");
  assert.equal(run.slot.family, "video-wan:high_fp8.safetensors");
  assert.match(run.note, /no --api or COMFY_API.*falls back to http:\/\/127\.0\.0\.1:\d+/, "the port it got is said");
  assert.equal(planRun([...ARGV, "--api", "http://127.0.0.1:18852"], { env: { ...LEASE }, planFn, say: () => {} }).note, "", "a port the caller gave needs no note");
});

test("planRun: the placement is for the Wan 2.2 and LTX-2.5 graphs only, so another model or a --graph file is refused before any card is read", () => {
  const env = { ...LEASE };
  assert.throws(() => planRun(["o.mp4", "s.png", "p", "--model", "h3", "--wan-placement", "multicard"], { env }), /places the Wan 2\.2 graph and the LTX-2\.5 one \(--model ltx25\); this invocation builds --model h3/);
  assert.throws(() => planRun(["o.mp4", "s.png", "p", "--model", "hunyuan", "--wan-placement", "multicard"], { env }), /--model hunyuan/);
  assert.throws(() => planRun(["o.mp4", "--graph", "wf.json", "--wan-placement", "multicard"], { env }), /builds a --graph file/);
});

// ---- generate(): what the run does once it holds the GPU --------------------------------------------------------

const PLAN = { launch: { key: "mc-aaaa1111-cccc3333" }, pair: {}, env: {} };
const GRAPH = { "20": { class_type: "VHS_VideoCombine", inputs: {} } };
const DONE = { outputs: { "20": { gifs: [{ filename: "wan_00001.mp4", subfolder: "v", type: "output" }] } } };

function fakeIo(over = {}) {
  const rec = { calls: [], out: [], err: [], polls: [], written: [], cursors: [], finalized: 0 };
  const io = {
    verifyDevices: async () => { rec.calls.push("verifyDevices"); },
    verifyPack: async () => { rec.calls.push("verifyPack"); },
    buildGraph: async () => { rec.calls.push("buildGraph"); return { graph: GRAPH, seed: 7, model: "wan" }; },
    resolveCli: () => { rec.calls.push("resolveCli"); return null; },
    submit: async () => { rec.calls.push("submit"); return { promptId: "p1" }; },
    poll: async (opt) => { rec.calls.push("poll"); rec.polls.push(opt); return DONE; },
    view: async () => { rec.calls.push("view"); return Buffer.from("mp4"); },
    write: (p, b) => { rec.calls.push("write"); rec.written.push([p, b]); },
    // the instance's console as the run's cursor reads it: quiet unless a test says otherwise (rec.reads is a queue of { text, state })
    logCursor: (dir, key) => { rec.cursors.push([dir, key]); return { path: `${dir}/offload-comfyui-${key}.log`, read: () => (rec.reads && rec.reads.length ? rec.reads.shift() : { text: "", state: "ok" }) }; },
    finalize: async () => { rec.calls.push("finalize"); rec.finalized++; },
    stdout: (...a) => rec.out.push(a.join(" ")),
    stderr: (...a) => rec.err.push(a.join(" ")),
    env: {}, comfyDir: "/c",
    ...over,
  };
  return { io, rec };
}

test("generate()'s defaults are the real calls: the graph comes from buildGraphForRun (the one that lets --wan-decode auto read the card), the rest from their modules", async () => {
  assert.equal(GENERATE_IO.buildGraph, buildGraphForRun);
  assert.equal(GENERATE_IO.submit, submitChecked);
  assert.equal(GENERATE_IO.verifyDevices, verifyMulticardDevices);
  assert.equal(GENERATE_IO.verifyPack, verifyMulticardPack);
  // the poll is reached through a literal `pollOutputs(` call (gpu_progress_test.go finds the heartbeating helpers by it), so it is checked by what it does
  const seen = [];
  const entry = { outputs: { 20: { gifs: [{ filename: "a.mp4" }] } } };
  const h = await GENERATE_IO.poll({
    api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs, env: {}, sleep: async () => {}, now: () => 0,
    fetchImpl: async (url) => { seen.push(String(url)); return { ok: true, json: async () => ({ p1: entry }) }; },
  });
  assert.equal(h, entry, "it is pollOutputs: the history entry of the finished prompt");
  assert.ok(seen.some((u) => u.endsWith("/history/p1")), seen.join());
  assert.equal(GENERATE_IO.view, fetchView);
  assert.equal(GENERATE_IO.write, writeFileAtomic);
  assert.equal(GENERATE_IO.logCursor, comfyLogCursor);
  assert.equal(GENERATE_IO.finalize, finalizeRun);
  assert.equal(GENERATE_IO.resolveCli, resolveCli);
});

test("generate() under multicard checks the devices and then the pack, before it builds or submits anything, and finalizes last", async () => {
  const { io, rec } = fakeIo();
  await generate("o.mp4", "http://x", ["o.mp4", "s.png", "p"], {}, PLAN, io);
  assert.deepEqual(rec.calls, ["verifyDevices", "verifyPack", "buildGraph", "resolveCli", "submit", "poll", "view", "write", "finalize"]);
  assert.deepEqual(rec.written.map((w) => [w[0], String(w[1])]), [["o.mp4", "mp4"]]);
  assert.deepEqual(rec.out, ["queued p1 wan seed 7", "WROTE o.mp4"]);
});

test("generate() without a plan touches nothing of the placement: no device check, no pack check, no console read", async () => {
  const { io, rec } = fakeIo();
  await generate("o.mp4", "http://x", ["o.mp4", "s.png", "p"], {}, null, io);
  assert.deepEqual(rec.calls, ["buildGraph", "resolveCli", "submit", "poll", "view", "write", "finalize"]);
  assert.deepEqual(rec.err, []);
});

test("generate() names the sampled latent the free node saved, once per file, after WROTE", async () => {
  const outputs = { ...DONE.outputs, "21": { latents: [{ filename: "wan_00001_.latent", subfolder: "latents", type: "output" }, { filename: "b.latent" }] } };
  const { io, rec } = fakeIo({ poll: async () => ({ outputs }) });
  await generate("o.mp4", "http://x", [], {}, PLAN, io);
  assert.deepEqual(rec.out.slice(1), ["WROTE o.mp4", "LATENT latents/wan_00001_.latent (in ComfyUI's output directory)", "LATENT b.latent (in ComfyUI's output directory)"]);
  const none = fakeIo();
  await generate("o.mp4", "http://x", [], {}, PLAN, none.io);
  assert.ok(!none.rec.out.some((l) => l.startsWith("LATENT")), "a run without the free node prints no LATENT line");
});

// ---- the plain decode that ComfyUI retried tiled ----------------------------------------------------------------
// --wan-decode plain asks for ONE decode pass, the clip without seams. When that decode runs out of memory ComfyUI retries it tiled with temporal
// tiles it sizes itself (sd.py, "Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding"), and the clip can seam where the
// tiles meet. Writing that clip to the path the caller named and exiting 0 hands the seams to whoever keys on the exit code.
const PLAIN_GRAPH = { "13": { class_type: "VAEDecode", inputs: {} }, "20": { class_type: "VHS_VideoCombine", inputs: {} } };
const TILED_GRAPH = { "13": { class_type: "VAEDecodeTiled", inputs: {} }, "20": { class_type: "VHS_VideoCombine", inputs: {} } };
const RETRY = "Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n";
const LATENT_OUT = { outputs: { ...DONE.outputs, "21": { latents: [{ filename: "wan_00001_.latent", subfolder: "latents", type: "output" }] } } };
const decodeRun = (graph, reads, over = {}) => {
  const f = fakeIo({ buildGraph: async () => ({ graph, seed: 7, model: "wan" }), poll: async (opt) => { f.rec.polls.push(opt); return LATENT_OUT; }, ...over });
  f.rec.reads = reads;
  return f;
};

test("a plain decode that ComfyUI retried tiled is not written to the requested path: the clip is kept beside it and the run fails, naming the latent", async () => {
  const { io, rec } = decodeRun(PLAIN_GRAPH, [{ text: "got prompt\n", state: "ok" }, { text: RETRY + "Prompt executed in 5000 seconds\n", state: "ok" }]);
  await assert.rejects(generate("out/o.mp4", "http://x", [], { "wait-sec": "60" }, PLAN, io), /^Error: WAN-DECODE-FALLBACK: ComfyUI ran out of memory in the plain decode and retried it tiled.*out\/o\.seamed\.mp4.*instead of out\/o\.mp4.*latents\/wan_00001_\.latent/s);
  assert.deepEqual(rec.written.map((w) => w[0]), ["out/o.seamed.mp4"], "nothing at the requested path, and the finished clip is not thrown away");
  assert.ok(rec.out.includes("LATENT latents/wan_00001_.latent (in ComfyUI's output directory)"), "the latent that lets the decode be redone is named");
  assert.ok(!rec.out.includes("WROTE out/o.mp4"));
  assert.equal(rec.finalized, 1, "the run is finalized before it fails");
  assert.deepEqual(rec.cursors, [["/c", "mc-aaaa1111-cccc3333"]], "the console read is this placement's own instance, and only this run's lines");
});

test("a plain decode whose console shows no retry is written to the requested path, and nothing is said", async () => {
  const { io, rec } = decodeRun(PLAIN_GRAPH, [{ text: "step\n", state: "ok" }]);
  await generate("o.mp4", "http://x", [], { "wait-sec": "60" }, PLAN, io);
  assert.deepEqual(rec.written.map((w) => w[0]), ["o.mp4"]);
  assert.deepEqual(rec.err, []);
});

test("a retry seen by the watch DURING the wait still counts at the end, and a tiled decode (the default) is never judged by it", async () => {
  const mid = decodeRun(PLAIN_GRAPH, []);
  const io = { ...mid.io, poll: async (opt) => { mid.rec.polls.push(opt); mid.rec.reads.push({ text: RETRY, state: "ok" }); opt.watch(); mid.rec.reads.push({ text: "later lines\n", state: "ok" }); return LATENT_OUT; } };
  await assert.rejects(generate("o.mp4", "http://x", [], { "wait-sec": "60" }, PLAN, io), /WAN-DECODE-FALLBACK/);
  const tiled = decodeRun(TILED_GRAPH, [{ text: RETRY, state: "ok" }]);
  await generate("o.mp4", "http://x", [], { "wait-sec": "60" }, PLAN, tiled.io);
  assert.deepEqual(tiled.rec.written.map((w) => w[0]), ["o.mp4"], "the tiled decode is the one that was asked for");
  assert.deepEqual(tiled.rec.err, []);
});

test("a console that cannot be read says so loudly, and the clip is still written: unknown is not a fallback", async () => {
  for (const reads of [[{ text: "", state: "absent" }], [{ text: "boot\n... [offload-comfyui.log capped at 5MB for this run] ...\n", state: "capped" }, { text: "", state: "ok" }]]) {
    const { io, rec } = decodeRun(PLAIN_GRAPH, reads);
    await generate("o.mp4", "http://x", [], { "wait-sec": "60" }, PLAN, io);
    assert.deepEqual(rec.written.map((w) => w[0]), ["o.mp4"]);
    assert.ok(rec.err.some((l) => /^WAN-DECODE-UNVERIFIED: .*plain decode.*check the clip for seams/.test(l)), rec.err.join("\n"));
  }
});

test("generate() hands the poll the run's wait budget: --wait-sec, else COMFY_WAIT_SEC, else the default", async () => {
  const wait = async (flags, env) => { const { io, rec } = fakeIo({ env }); await generate("o.mp4", "http://x", [], flags, null, io); return rec.polls[0].waitSec; };
  assert.equal(await wait({ "wait-sec": "123" }, { COMFY_WAIT_SEC: "999" }), 123);
  assert.equal(await wait({}, { COMFY_WAIT_SEC: "999" }), 999);
  assert.equal(await wait({}, {}), 5400);
});

// main() itself takes the GPU lease and frees the inference server, so no test runs it; what it does with the plan is pinned the way
// comfy-video.test.mjs pins the glue of generate(): the source, code only, so a comment cannot satisfy the pin.
test("main() hands the lease slot planRun's slot and generate() planRun's own pieces, in that order", async () => {
  const { readFileSync } = await import("node:fs");
  const src = readFileSync(new URL("./comfy-video.mjs", import.meta.url), "utf8");
  const from = src.indexOf("async function main(");
  assert.ok(from >= 0, "main() is declared in comfy-video.mjs");
  const code = src.slice(from, src.indexOf("\n}\n", from)).replace(/\/\/[^\n]*/g, "");
  assert.match(code, /const run = planRun\(process\.argv\.slice\(2\)\);/);
  assert.match(code, /if \(run\.note\) console\.error\(run\.note\);/);
  assert.match(code, /if \(!run\.out\) \{ console\.error\(USAGE\); process\.exit\(2\); \}/);
  assert.match(code, /const API = run\.api;/);
  assert.match(code, /await withGpuSlot\(\{ \.\.\.run\.slot, comfyManaged: true, api: API \}, \(\) => generate\(run\.out, API, run\.pos, run\.flags, run\.multicard\)\);/);
});

// ---- the console watch in the run ------------------------------------------------------------------------------

test("generate() under multicard hands the poll a watch on THIS instance's console, made before the prompt is submitted; without the placement the poll gets none", async () => {
  const made = [];
  const cursor = { path: "/c/log", read: () => ({ text: "Got an OOM, unloading all loaded models.\n", state: "ok" }) };
  const { io, rec } = fakeIo({ logCursor: (dir, key) => { rec.calls.push("logCursor"); made.push([dir, key]); return cursor; } });
  await generate("o.mp4", "http://x", [], {}, PLAN, io);
  assert.deepEqual(made, [["/c", "mc-aaaa1111-cccc3333"]]);
  assert.ok(rec.calls.indexOf("logCursor") > rec.calls.indexOf("verifyPack") && rec.calls.indexOf("logCursor") < rec.calls.indexOf("submit"), rec.calls.join(" "));
  assert.equal(typeof rec.polls[0].watch, "function");
  assert.match(rec.polls[0].watch(), /^WAN-OOM: /, "it is the OOM watch of that console");
  const plain = fakeIo({ logCursor: () => { throw new Error("no console is read without the placement"); } });
  await generate("o.mp4", "http://x", [], {}, null, plain.io);
  assert.ok(!("watch" in plain.rec.polls[0]), "the poll's options are what they always were");
});

// ---- the wait budget of a film-size run ------------------------------------------------------------------------

// The benchmark measured 250.3 s per step at the film's geometry and a 4-step prompt wall of 1,165.7 s: the 20-step recipe is about 5,170 s
// (the 164.5 s that is not sampling, plus 20 steps). The single-card default of 5,400 s leaves 4.5 percent; a run that is 5 percent slower is
// killed at the end of an hour and a half of sampling and nothing of it is kept (the latent is saved after the second sampler).
test("generate() under multicard gives the wait 2 h unless the caller chose a budget, and says which it used", async () => {
  assert.equal(M.MULTICARD_WAIT_SEC, 7200);
  assert.ok(M.MULTICARD_WAIT_SEC >= 1.35 * (164.5 + 20 * 250.3), "at least 35 percent over the measured projection of the 20-step film run");
  const run = async (flags, env, plan) => { const { io, rec } = fakeIo({ env }); await generate("o.mp4", "http://x", [], flags, plan, io); return { wait: rec.polls[0].waitSec, err: rec.err }; };
  const dflt = await run({}, {}, PLAN);
  assert.equal(dflt.wait, 7200);
  assert.ok(dflt.err.some((l) => /^WAN-WAIT-NOTE: .*7200 s.*--wait-sec/.test(l)), dflt.err.join("\n"));
  // 5170 s is the projection from the 4-step benchmark; the film-size run's runner wall was 5136 s. The note must not call the projection a measurement.
  assert.ok(dflt.err.some((l) => /^WAN-WAIT-NOTE: .*projected at about 5170 s.*measured 5136 s of runner wall/.test(l)), dflt.err.join("\n"));
  assert.ok(!dflt.err.some((l) => /measured about 5170/.test(l)), "5170 s was never measured");
  const flag = await run({ "wait-sec": "9000" }, { COMFY_WAIT_SEC: "999" }, PLAN);
  assert.equal(flag.wait, 9000);
  assert.ok(!flag.err.some((l) => /WAN-WAIT-NOTE/.test(l)), "a budget the caller chose needs no note");
  const env = await run({}, { COMFY_WAIT_SEC: "6000" }, PLAN);
  assert.equal(env.wait, 6000);
  assert.ok(!env.err.some((l) => /WAN-WAIT-NOTE/.test(l)));
  const single = await run({}, {}, null);
  assert.equal(single.wait, 5400, "the single placement keeps its budget");
  assert.deepEqual(single.err, []);
});

test("seamedPath keeps the clip beside the requested path: before the extension, whatever the separators and dots of the folder", () => {
  assert.equal(seamedPath("o.mp4"), "o.seamed.mp4");
  assert.equal(seamedPath("out/o.mp4"), "out/o.seamed.mp4");
  assert.equal(seamedPath("C:\\out.v2\\o.mp4"), "C:\\out.v2\\o.seamed.mp4");
  assert.equal(seamedPath("out.v2/o"), "out.v2/o.seamed", "a dot in the folder is not an extension");
  assert.equal(seamedPath("C:\\out.v2\\o"), "C:\\out.v2\\o.seamed", "nor is one before a backslash");
  assert.equal(seamedPath("out/.hidden"), "out/.hidden.seamed", "a leading dot is not an extension either");
  assert.equal(seamedPath("a.b.mp4"), "a.b.seamed.mp4");
});
