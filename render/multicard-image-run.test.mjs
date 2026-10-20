// node --test render/multicard-image-run.test.mjs
// The glue of the card-only placement in the image and edit runners: generate() in comfy-render.mjs and renderEdit() in comfy-edit.mjs (what each does once
// it holds the GPU slot), with every call to the outside world replaced by a recording fake. multicard-image.test.mjs covers the placement's own
// functions; this file covers the places they are wired in, which a one-line change can break without failing any of them. Same shape as
// wan-multicard-run.test.mjs, which does it for the Wan runner.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generate, GENERATE_IO } from "./comfy-render.mjs";
import { renderEdit, RENDER_IO, stageInputs, unstageInputs } from "./comfy-edit.mjs";
import { resolveCli, submitGraph, fetchView, finalizeRun } from "./comfy-submit.mjs";
import { writeFileAtomic } from "./atomic-out.mjs";
import { assertNodeClasses } from "./comfy-nodes.mjs";
import { comfyLogCursor } from "./comfy-lifecycle.mjs";
import * as W from "./wan-multicard.mjs";
import * as I from "./multicard-image.mjs";
import { verifyImagePlacement } from "./multicard-image.mjs";

// Synthetic ids: a repeated-nibble head is a placeholder to the leak gate, and none of these names a real card.
const U0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const U2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000";
const CARDS = W.parseCardTable([
  `0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`,
  `1, ${U1}, Test GPU B, 16303, 13800, Disabled, Yes`,
  `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`,
].join("\n"));
const LEASE = { GPU_LEASE_DEVICES: "gpu-cccc3333-dddd-eeee-ffff-000000000000,gpu-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" };
// the plan the runners are handed: the real planner on the synthetic table (2512 Q5_1 GGUF, the launchable image row: 3 GiB of the DiT resident, peaks 4.9 / 12.6 GiB)
const PLAN = I.planImageMulticard({
  route: "image", family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined,
  flags: { placement: "multicard" }, env: { ...LEASE }, comfyDir: "/c", cards: () => CARDS, listFn: () => ["ComfyUI-MultiGPU", "ComfyUI-GGUF"],
});
const KEY = PLAN.launch.key;
const GRAPH = { "9": { class_type: "SaveImage", inputs: {} }, "11": { class_type: "VAEDecode", inputs: {} } };
const IMG_DONE = { outputs: { "9": { images: [{ filename: "render_00001_.png", subfolder: "", type: "output" }] } } };

function fakeIo(over = {}) {
  const rec = { calls: [], out: [], err: [], polls: [], written: [], submits: [], cursors: [], finalized: 0, staged: [], unstaged: [] };
  const io = {
    verifyPlacement: async (...a) => { rec.calls.push("verifyPlacement"); rec.verified = a; },
    resolveCli: () => { rec.calls.push("resolveCli"); return null; },
    submit: async (o) => { rec.calls.push("submit"); rec.submits.push(o); return { promptId: "p1" }; },
    poll: async (o) => { rec.calls.push("poll"); rec.polls.push(o); return IMG_DONE; },
    view: async () => { rec.calls.push("view"); return Buffer.from("png"); },
    write: (p, b) => { rec.calls.push("write"); rec.written.push([p, String(b)]); },
    finalize: async () => { rec.calls.push("finalize"); rec.finalized++; },
    preflight: async (...a) => { rec.calls.push("preflight"); rec.preflighted = a; },
    // the instance's console as the run's cursor reads it: quiet unless a test says otherwise (rec.reads is a queue of { text, state })
    logCursor: (dir, key) => { rec.calls.push("logCursor"); rec.cursors.push([dir, key]); return { path: `${dir}/offload-comfyui-${key}.log`, read: () => (rec.reads && rec.reads.length ? rec.reads.shift() : { text: "", state: "ok" }) }; },
    stdout: (...a) => rec.out.push(a.join(" ")),
    stderr: (...a) => rec.err.push(a.join(" ")),
    env: {}, comfyDir: "/c",
    ...over,
  };
  return { io, rec };
}

const BUILT = { graph: GRAPH, seed: 7, width: 1024, height: 1024 };

test("generate()'s defaults are the real calls, and the poll is pollOutputs", async () => {
  assert.equal(GENERATE_IO.verifyPlacement, verifyImagePlacement);
  assert.equal(GENERATE_IO.resolveCli, resolveCli);
  assert.equal(GENERATE_IO.submit, submitGraph);
  assert.equal(GENERATE_IO.view, fetchView);
  assert.equal(GENERATE_IO.write, writeFileAtomic);
  assert.equal(GENERATE_IO.finalize, finalizeRun);
  const seen = [];
  const entry = IMG_DONE;
  const h = await GENERATE_IO.poll({
    api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs, env: {}, sleep: async () => {}, now: () => 0,
    fetchImpl: async (url) => { seen.push(String(url)); return { ok: true, json: async () => ({ p1: entry }) }; },
  });
  assert.equal(h, entry, "it is pollOutputs: the history entry of the finished prompt");
  assert.ok(seen.some((u) => u.endsWith("/history/p1")), seen.join());
});

test("generate() without a plan touches nothing of the placement: the order is today's, and the poll's options are what they always were", async () => {
  const { io, rec } = fakeIo();
  await generate("o.png", "http://x", BUILT, {}, null, io);
  assert.deepEqual(rec.calls, ["resolveCli", "submit", "poll", "view", "write", "finalize"]);
  assert.deepEqual(rec.written, [["o.png", "png"]]);
  assert.deepEqual(rec.out, ["queued p1 seed 7 1024x1024", "WROTE o.png 3 bytes"]);
  assert.deepEqual(Object.keys(rec.polls[0]).sort(), ["api", "isDone", "noOutputMsg", "onExecError", "promptId", "waitSec"], "no watch");
  assert.equal(rec.polls[0].waitSec, 1800);
  assert.equal(rec.submits[0].clientId, "render-7");
  assert.deepEqual(rec.err, []);
});

test("generate() hands the poll the run's wait budget: --wait-sec, else COMFY_WAIT_SEC, else 1800", async () => {
  const wait = async (flags, env) => { const { io, rec } = fakeIo({ env }); await generate("o.png", "http://x", BUILT, flags, null, io); return rec.polls[0].waitSec; };
  assert.equal(await wait({ "wait-sec": "123" }, { COMFY_WAIT_SEC: "999" }), 123);
  assert.equal(await wait({}, { COMFY_WAIT_SEC: "999" }), 999);
  assert.equal(await wait({}, {}), 1800);
});

test("generate() under a plan verifies the pair before anything is built into a submission", async () => {
  const { io, rec } = fakeIo();
  await generate("o.png", "http://x", BUILT, {}, PLAN, io);
  assert.equal(rec.calls[0], "verifyPlacement");
  assert.deepEqual(rec.verified, ["http://x", PLAN]);
  assert.ok(rec.calls.indexOf("verifyPlacement") < rec.calls.indexOf("submit"));
  // a refusal submits nothing and writes nothing
  const bad = fakeIo({ verifyPlacement: async () => { throw new Error("IMAGE-PLACEMENT-MISMATCH: x. Nothing was submitted."); } });
  await assert.rejects(generate("o.png", "http://x", BUILT, {}, PLAN, bad.io), /IMAGE-PLACEMENT-MISMATCH/);
  assert.deepEqual(bad.rec.calls, []);
});

// ---- renderEdit -------------------------------------------------------------------------------------------------

const EDIT_PLAN = { sources: ["in.png"], seed: 9, describe: "family qwen-image-edit-2511", build: (staged) => ({ ...GRAPH, staged }) };

function fakeEditIo(over = {}) {
  const f = fakeIo({
    stage: (sources, staged) => { f.rec.calls.push("stage"); f.rec.staged.push([...sources]); staged.push("edit_in_0.png"); },
    unstage: (staged) => { f.rec.calls.push("unstage"); f.rec.unstaged.push([...staged]); },
    poll: async (o) => { f.rec.calls.push("poll"); f.rec.polls.push(o); return { outputs: { "9": { images: [{ filename: "edit_00001_.png", subfolder: "", type: "output" }] } } }; },
    ...over,
  });
  return f;
}

test("renderEdit()'s defaults are the real calls", () => {
  assert.equal(RENDER_IO.verifyPlacement, verifyImagePlacement);
  assert.equal(RENDER_IO.stage, stageInputs);
  assert.equal(RENDER_IO.unstage, unstageInputs);
  assert.equal(RENDER_IO.resolveCli, resolveCli);
  assert.equal(RENDER_IO.submit, submitGraph);
  assert.equal(RENDER_IO.view, fetchView);
  assert.equal(RENDER_IO.write, writeFileAtomic);
  assert.equal(RENDER_IO.finalize, finalizeRun);
});

test("renderEdit() without a plan: stage, submit, poll, write, finalize, unstage, in today's order, and no watch", async () => {
  const { io, rec } = fakeEditIo();
  await renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: null, inputDir: "/c/input" }, io);
  assert.deepEqual(rec.calls, ["stage", "resolveCli", "submit", "poll", "view", "write", "finalize", "unstage"]);
  assert.deepEqual(rec.out, ["queued p1 seed 9 family qwen-image-edit-2511", "WROTE o.png"]);
  assert.deepEqual(rec.written, [["o.png", "png"]]);
  assert.deepEqual(rec.unstaged, [["edit_in_0.png"]]);
  assert.equal(rec.submits[0].clientId, "edit-9");
  assert.deepEqual(Object.keys(rec.polls[0]).sort(), ["api", "isDone", "noOutputMsg", "onExecError", "promptId", "waitSec"], "no watch");
  assert.deepEqual(rec.submits[0].graph.staged, ["edit_in_0.png"], "the graph is built from the staged names");
});

test("renderEdit() under a plan verifies the pair before anything is staged, and unstages even when the run fails", async () => {
  const { io, rec } = fakeEditIo();
  await renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: PLAN, inputDir: "/c/input" }, io);
  assert.equal(rec.calls[0], "verifyPlacement");
  assert.deepEqual(rec.verified, ["http://x", PLAN]);
  assert.ok(rec.calls.indexOf("verifyPlacement") < rec.calls.indexOf("stage"));
  const bad = fakeEditIo({ verifyPlacement: async () => { throw new Error("IMAGE-PLACEMENT-PACK: x. Nothing was submitted."); } });
  await assert.rejects(renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: PLAN, inputDir: "/c/input" }, bad.io), /IMAGE-PLACEMENT-PACK/);
  assert.deepEqual(bad.rec.calls, [], "nothing staged, nothing submitted");
  const boom = fakeEditIo({ poll: async () => { throw new Error("ComfyUI exec error: x"); } });
  await assert.rejects(renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: null, inputDir: "/c/input" }, boom.io), /exec error/);
  assert.deepEqual(boom.rec.unstaged, [["edit_in_0.png"]], "the staged copy is removed on failure too");
  assert.deepEqual(boom.rec.written, []);
});

// ---- the placement's watch, preflight and decode verdict (the review of 2026-10-11) ---------------------------------------
// A DisTorch2 out-of-memory error can kill ComfyUI's prompt worker without an error in /history; the poll then waits out its whole budget with both
// cards leased. ComfyUI also retries a decode that ran out of memory TILED, without a word to the caller. The Wan runner watches the instance's console
// for both; so do these. The cursor is made before the submit, so it reads this run's lines and not an earlier run's.

const ESC = String.fromCharCode(27);
const WEDGE = `${ESC}[1m${ESC}[31m[ERROR]${ESC}[0m Got an OOM, unloading all loaded models.\nException in thread Thread-5 (prompt_worker):\n`;
const RETRY = "Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n";

test("generate() under a plan preflights the graph's node classes, then makes the console cursor, both before it submits; without a plan it does neither", async () => {
  const { io, rec } = fakeIo();
  await generate("o.png", "http://x", BUILT, {}, PLAN, io);
  assert.deepEqual(rec.calls, ["verifyPlacement", "resolveCli", "preflight", "logCursor", "submit", "poll", "view", "write", "finalize"]);
  assert.deepEqual(rec.preflighted, ["http://x", GRAPH, { comfyDir: "/c" }], "the graph that is about to be posted, with the install to name in the hint");
  assert.deepEqual(rec.cursors, [["/c", KEY]], "this placement's own instance console");
  assert.equal(typeof rec.polls[0].watch, "function");
  const plain = fakeIo({ preflight: async () => { throw new Error("no preflight without a plan"); }, logCursor: () => { throw new Error("no console is read without a plan"); } });
  await generate("o.png", "http://x", BUILT, {}, null, plain.io);
  assert.ok(!("watch" in plain.rec.polls[0]));
});

test("a node class the server lacks stops the run before anything is submitted, with the pack hint, and nothing is written", async () => {
  const { io, rec } = fakeIo({ preflight: async () => { throw new Error("MISSING_NODE: ComfyUI at http://x has no node class CLIPLoaderDisTorch2MultiGPU (custom node pack ComfyUI-MultiGPU). Nothing was submitted."); } });
  await assert.rejects(generate("o.png", "http://x", BUILT, {}, PLAN, io), /^Error: MISSING_NODE: .*CLIPLoaderDisTorch2MultiGPU.*ComfyUI-MultiGPU/s);
  assert.ok(!rec.calls.includes("submit") && !rec.calls.includes("write") && !rec.calls.includes("logCursor"), rec.calls.join(" "));
});

test("the poll's watch is the placement's out-of-memory watch, in the image words, with the recipe's numbers; a quiet console says nothing", async () => {
  const { io, rec } = fakeIo();
  rec.reads = [{ text: "step 1\n", state: "ok" }];
  await generate("o.png", "http://x", BUILT, {}, PLAN, io);
  const watch = rec.polls[0].watch;
  assert.equal(watch(), null, "step 1 is not a wedge");
  rec.reads.push({ text: WEDGE, state: "ok" });
  const msg = watch();
  assert.match(msg, /^IMAGE-PLACEMENT-OOM: ComfyUI ran out of memory and its prompt worker did not survive it/);
  assert.match(msg, /\[ERROR\] Got an OOM, unloading all loaded models\./);
  assert.match(msg, new RegExp(`offload-comfyui-${KEY}\\.log`));
  assert.match(msg, /ComfyUI must be restarted/);
  assert.match(msg, /qwen-image\/gguf-q5_1/, "the recipe");
  assert.match(msg, /11\.9 GiB.*compute card.*8\.3 GiB.*storage card/s, "the per-card peaks it was held to");
  assert.match(msg, /15\.9 GiB/, "and the cards it had");
  assert.ok(!/WAN/.test(msg), msg);
});

test("a console the watch cannot see is said once each, in the image words, and is never a verdict", async () => {
  const { io, rec } = fakeIo();
  rec.reads = [{ text: "", state: "absent" }, { text: "", state: "absent" }, { text: "", state: "capped" }, { text: "", state: "capped" }];
  await generate("o.png", "http://x", BUILT, {}, PLAN, io);
  const watch = rec.polls[0].watch;
  for (let i = 0; i < 4; i++) assert.equal(watch(), null);
  assert.equal(rec.err.filter((m) => /^IMAGE-PLACEMENT-OOM-WATCH: .*no console file/.test(m)).length, 1, rec.err.join("\n"));
  assert.equal(rec.err.filter((m) => /^IMAGE-PLACEMENT-OOM-WATCH: .*capped/.test(m)).length, 1);
});

// ---- the decode that ComfyUI retried tiled ----------------------------------------------------------------------

test("a decode that ComfyUI retried tiled is not written to the requested path: the image is kept beside it and the run fails, naming the console", async () => {
  const { io, rec } = fakeIo();
  rec.reads = [{ text: "got prompt\n", state: "ok" }, { text: RETRY + "Prompt executed in 215 seconds\n", state: "ok" }];
  await assert.rejects(generate("out/o.png", "http://x", BUILT, {}, PLAN, io), /^Error: IMAGE-DECODE-FALLBACK: ComfyUI ran out of memory in the VAE decode and retried it tiled.*can show seams.*out\/o\.tiled\.png.*instead of out\/o\.png/s);
  assert.deepEqual(rec.written, [["out/o.tiled.png", "png"]], "nothing at the requested path, and the finished image is not thrown away");
  assert.ok(!rec.out.some((l) => l.startsWith("WROTE")));
  assert.equal(rec.finalized, 1, "the run is finalized before it fails");
});

test("a console that shows no retry delivers the image to the requested path and says nothing", async () => {
  const { io, rec } = fakeIo();
  rec.reads = [{ text: "step\n", state: "ok" }];
  await generate("o.png", "http://x", BUILT, {}, PLAN, io);
  assert.deepEqual(rec.written, [["o.png", "png"]]);
  assert.deepEqual(rec.err, []);
});

test("a retry the watch saw DURING the wait still counts at the end", async () => {
  const f = fakeIo();
  const io = { ...f.io, poll: async (o) => { f.rec.polls.push(o); f.rec.reads = [{ text: RETRY, state: "ok" }]; o.watch(); f.rec.reads = [{ text: "later lines\n", state: "ok" }]; return IMG_DONE; } };
  await assert.rejects(generate("o.png", "http://x", BUILT, {}, PLAN, io), /IMAGE-DECODE-FALLBACK/);
  assert.deepEqual(f.rec.written.map((w) => w[0]), ["o.tiled.png"]);
});

test("a console that cannot be read says so loudly, and the image is still delivered: unknown is not a fallback", async () => {
  for (const reads of [[{ text: "", state: "absent" }], [{ text: "boot\n... [offload-comfyui.log capped at 5MB for this run] ...\n", state: "capped" }, { text: "", state: "ok" }]]) {
    const { io, rec } = fakeIo();
    rec.reads = reads;
    await generate("o.png", "http://x", BUILT, {}, PLAN, io);
    assert.deepEqual(rec.written.map((w) => w[0]), ["o.png"]);
    assert.ok(rec.err.some((l) => /^IMAGE-DECODE-UNVERIFIED: .*console.*could not be read to the end.*check the image for seams/s.test(l)), rec.err.join("\n"));
  }
});

test("the tiled copy's name sits beside the requested path and keeps the extension", () => {
  assert.equal(I.tiledPath("out/o.png"), "out/o.tiled.png");
  assert.equal(I.tiledPath("C:\\out\\o.v2.png"), "C:\\out\\o.v2.tiled.png");
  assert.equal(I.tiledPath("o"), "o.tiled");
  assert.equal(I.tiledPath("dir.d/o"), "dir.d/o.tiled", "a dot in a directory is not an extension");
});

// ---- renderEdit: the same three, with the staged copy removed whatever happens ----------------------------------------

const EDIT_PLAN_REAL = I.planImageMulticard({
  route: "edit", family: "qwen-image-edit-2511", unet: "qwen_image_edit_2511_fp8mixed.safetensors", clip: undefined,
  flags: { placement: "multicard" }, env: { ...LEASE }, comfyDir: "/c", cards: () => CARDS, listFn: () => ["ComfyUI-MultiGPU"],
});

test("renderEdit() under a plan preflights the graph it built from the staged names, then makes the console cursor, before it submits", async () => {
  const f = fakeEditIo();
  await renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: EDIT_PLAN_REAL, inputDir: "/c/input" }, f.io);
  assert.deepEqual(f.rec.calls, ["verifyPlacement", "stage", "resolveCli", "preflight", "logCursor", "submit", "poll", "view", "write", "finalize", "unstage"]);
  assert.deepEqual(f.rec.preflighted, ["http://x", { ...GRAPH, staged: ["edit_in_0.png"] }, { comfyDir: "/c" }]);
  assert.deepEqual(f.rec.cursors, [["/c", EDIT_PLAN_REAL.launch.key]]);
  assert.equal(typeof f.rec.polls[0].watch, "function");
  assert.match((() => { f.rec.reads = [{ text: WEDGE, state: "ok" }]; return f.rec.polls[0].watch(); })(), /^IMAGE-PLACEMENT-OOM: .*qwen-image-edit-2511\/fp8mixed.*13\.0 GiB.*compute card.*11\.0 GiB.*storage card/s);
});

test("renderEdit(): a missing node class or a decode retried tiled fails the run, unstages, and writes nothing to the requested path", async () => {
  const miss = fakeEditIo({ preflight: async () => { throw new Error("MISSING_NODE: x. Nothing was submitted."); } });
  await assert.rejects(renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: EDIT_PLAN_REAL, inputDir: "/c/input" }, miss.io), /MISSING_NODE/);
  assert.ok(!miss.rec.calls.includes("submit"));
  assert.deepEqual(miss.rec.unstaged, [["edit_in_0.png"]], "the staged copy is removed when the preflight refuses too");
  const tiled = fakeEditIo();
  tiled.rec.reads = [{ text: RETRY, state: "ok" }];
  await assert.rejects(renderEdit({ out: "d/o.png", API: "http://x", plan: EDIT_PLAN, placement: EDIT_PLAN_REAL, inputDir: "/c/input" }, tiled.io), /^Error: IMAGE-DECODE-FALLBACK: .*d\/o\.tiled\.png.*instead of d\/o\.png/s);
  assert.deepEqual(tiled.rec.written.map((w) => w[0]), ["d/o.tiled.png"]);
  assert.equal(tiled.rec.finalized, 1);
  assert.deepEqual(tiled.rec.unstaged, [["edit_in_0.png"]]);
  const clean = fakeEditIo();
  await renderEdit({ out: "o.png", API: "http://x", plan: EDIT_PLAN, placement: null, inputDir: "/c/input" }, { ...clean.io, preflight: async () => { throw new Error("no preflight without a plan"); }, logCursor: () => { throw new Error("no console without a plan"); } });
  assert.ok(!("watch" in clean.rec.polls[0]));
});

test("the defaults of the new calls are the real ones", () => {
  assert.equal(GENERATE_IO.preflight, assertNodeClasses);
  assert.equal(GENERATE_IO.logCursor, comfyLogCursor);
  assert.equal(RENDER_IO.preflight, assertNodeClasses);
  assert.equal(RENDER_IO.logCursor, comfyLogCursor);
});
