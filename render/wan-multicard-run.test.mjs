// node --test render/wan-multicard-run.test.mjs
// The glue of the card-only placement in comfy-video.mjs: planRun (what main() decides before it takes the GPU) and generate() (what it does
// once it holds it), with every call to the outside world replaced by a recording fake. wan-multicard.test.mjs covers the placement's own
// functions; this file covers the places they are wired in, which a one-line change can break without failing any of them.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generate, planRun, GENERATE_IO, buildGraphForRun, submitChecked, verifyMulticardDevices, verifyMulticardPack } from "./comfy-video.mjs";
import { pollOutputs, fetchView, finalizeRun, resolveCli } from "./comfy-submit.mjs";
import { tailComfyLog, comfyApi } from "./comfy-lifecycle.mjs";
import { writeFileAtomic } from "./atomic-out.mjs";
import * as M from "./wan-multicard.mjs";

// Synthetic ids (the same placeholders wan-multicard.test.mjs uses): none names a real card.
const U0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const U2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000";
const CARDS = M.parseCardTable([
  `0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`,
  `1, ${U1}, Test GPU B, 16303, 13800, Disabled, Yes`,
  `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`,
].join("\n"));
const LEASE = { GPU_LEASE_DEVICES: "gpu-cccc3333-dddd-eeee-ffff-000000000000,gpu-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" };
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

test("planRun: the placement is for the Wan 2.2 graph only, so another model or a --graph file is refused before any card is read", () => {
  const env = { ...LEASE };
  assert.throws(() => planRun(["o.mp4", "s.png", "p", "--model", "ltx25", "--wan-placement", "multicard"], { env }), /places the Wan 2\.2 graph; this invocation builds --model ltx25/);
  assert.throws(() => planRun(["o.mp4", "s.png", "p", "--model", "hunyuan", "--wan-placement", "multicard"], { env }), /--model hunyuan/);
  assert.throws(() => planRun(["o.mp4", "--graph", "wf.json", "--wan-placement", "multicard"], { env }), /builds a --graph file/);
});

// ---- generate(): what the run does once it holds the GPU --------------------------------------------------------

const PLAN = { launch: { key: "mc-cccc3333-aaaa1111" }, pair: {}, env: {} };
const GRAPH = { "20": { class_type: "VHS_VideoCombine", inputs: {} } };
const DONE = { outputs: { "20": { gifs: [{ filename: "wan_00001.mp4", subfolder: "v", type: "output" }] } } };

function fakeIo(over = {}) {
  const rec = { calls: [], out: [], err: [], polls: [], tails: [], written: [] };
  const io = {
    verifyDevices: async () => { rec.calls.push("verifyDevices"); },
    verifyPack: async () => { rec.calls.push("verifyPack"); },
    buildGraph: async () => { rec.calls.push("buildGraph"); return { graph: GRAPH, seed: 7, model: "wan" }; },
    resolveCli: () => { rec.calls.push("resolveCli"); return null; },
    submit: async () => { rec.calls.push("submit"); return { promptId: "p1" }; },
    poll: async (opt) => { rec.calls.push("poll"); rec.polls.push(opt); return DONE; },
    view: async () => { rec.calls.push("view"); return Buffer.from("mp4"); },
    write: (p, b) => { rec.calls.push("write"); rec.written.push([p, b]); },
    tailLog: (dir, n, key) => { rec.calls.push("tailLog"); rec.tails.push([dir, n, key]); return ""; },
    finalize: async () => { rec.calls.push("finalize"); },
    stdout: (...a) => rec.out.push(a.join(" ")),
    stderr: (...a) => rec.err.push(a.join(" ")),
    env: {}, comfyDir: "/c",
    ...over,
  };
  return { io, rec };
}

test("generate()'s defaults are the real calls: the graph comes from buildGraphForRun (the one that lets --wan-decode auto read the card), the rest from their modules", () => {
  assert.equal(GENERATE_IO.buildGraph, buildGraphForRun);
  assert.equal(GENERATE_IO.submit, submitChecked);
  assert.equal(GENERATE_IO.verifyDevices, verifyMulticardDevices);
  assert.equal(GENERATE_IO.verifyPack, verifyMulticardPack);
  assert.equal(GENERATE_IO.poll, pollOutputs);
  assert.equal(GENERATE_IO.view, fetchView);
  assert.equal(GENERATE_IO.write, writeFileAtomic);
  assert.equal(GENERATE_IO.tailLog, tailComfyLog);
  assert.equal(GENERATE_IO.finalize, finalizeRun);
  assert.equal(GENERATE_IO.resolveCli, resolveCli);
});

test("generate() under multicard checks the devices and then the pack, before it builds or submits anything, and finalizes last", async () => {
  const { io, rec } = fakeIo();
  await generate("o.mp4", "http://x", ["o.mp4", "s.png", "p"], {}, PLAN, io);
  assert.deepEqual(rec.calls, ["verifyDevices", "verifyPack", "buildGraph", "resolveCli", "submit", "poll", "view", "write", "tailLog", "finalize"]);
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

test("generate() reads the console of THIS placement's instance (its key) after the run, and says when ComfyUI retried the plain decode tiled", async () => {
  const tail = "got prompt\nWarning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\nPrompt executed in 5000 seconds\n";
  const { io, rec } = fakeIo({ tailLog: (dir, n, key) => { rec.tails.push([dir, n, key]); return tail; } });
  await generate("o.mp4", "http://x", [], {}, PLAN, io);
  assert.deepEqual(rec.tails, [["/c", 4000, "mc-cccc3333-aaaa1111"]], "the multicard instance's own log, not the default one");
  assert.ok(rec.err.some((l) => /^WAN-DECODE-FALLBACK: ComfyUI ran out of memory in the plain decode and retried it tiled \(see .*offload-comfyui-mc-cccc3333-aaaa1111\.log\)/.test(l)), rec.err.join("\n"));
  const clean = fakeIo();
  await generate("o.mp4", "http://x", [], {}, PLAN, clean.io);
  assert.deepEqual(clean.rec.err, [], "a console without the retry says nothing");
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
  assert.match(code, /await withGpuSlot\(run\.slot, \(\) => generate\(run\.out, run\.api, run\.pos, run\.flags, run\.multicard\)\);/);
});

// ---- the console watch in the run ------------------------------------------------------------------------------

test("generate() under multicard hands the poll a watch on THIS instance's console, made before the prompt is submitted; without the placement the poll gets none", async () => {
  const made = [];
  const cursor = { path: "/c/log", read: () => ({ text: "Got an OOM, unloading all loaded models.\n", state: "ok" }) };
  const { io, rec } = fakeIo({ logCursor: (dir, key) => { rec.calls.push("logCursor"); made.push([dir, key]); return cursor; } });
  await generate("o.mp4", "http://x", [], {}, PLAN, io);
  assert.deepEqual(made, [["/c", "mc-cccc3333-aaaa1111"]]);
  assert.ok(rec.calls.indexOf("logCursor") > rec.calls.indexOf("verifyPack") && rec.calls.indexOf("logCursor") < rec.calls.indexOf("submit"), rec.calls.join(" "));
  assert.equal(typeof rec.polls[0].watch, "function");
  assert.match(rec.polls[0].watch(), /^WAN-OOM: /, "it is the OOM watch of that console");
  const plain = fakeIo({ logCursor: () => { throw new Error("no console is read without the placement"); } });
  await generate("o.mp4", "http://x", [], {}, null, plain.io);
  assert.ok(!("watch" in plain.rec.polls[0]), "the poll's options are what they always were");
});
