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
import { verifyImagePlacement } from "./multicard-image.mjs";

// the plan the runners are handed (only what the glue reads)
const PLAN = { launch: { key: "mc-aaaa1111-cccc3333" }, comfyDir: "/c", pair: {}, env: {}, recipe: { id: "qwen-image-2.1/bf16" }, residentGb: 8 };
const GRAPH = { "9": { class_type: "SaveImage", inputs: {} }, "11": { class_type: "VAEDecode", inputs: {} } };
const IMG_DONE = { outputs: { "9": { images: [{ filename: "render_00001_.png", subfolder: "", type: "output" }] } } };

function fakeIo(over = {}) {
  const rec = { calls: [], out: [], err: [], polls: [], written: [], submits: [], finalized: 0, staged: [], unstaged: [] };
  const io = {
    verifyPlacement: async (...a) => { rec.calls.push("verifyPlacement"); rec.verified = a; },
    resolveCli: () => { rec.calls.push("resolveCli"); return null; },
    submit: async (o) => { rec.calls.push("submit"); rec.submits.push(o); return { promptId: "p1" }; },
    poll: async (o) => { rec.calls.push("poll"); rec.polls.push(o); return IMG_DONE; },
    view: async () => { rec.calls.push("view"); return Buffer.from("png"); },
    write: (p, b) => { rec.calls.push("write"); rec.written.push([p, String(b)]); },
    finalize: async () => { rec.calls.push("finalize"); rec.finalized++; },
    stdout: (...a) => rec.out.push(a.join(" ")),
    env: {},
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
