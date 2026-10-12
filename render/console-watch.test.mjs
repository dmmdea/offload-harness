// node --test render/console-watch.test.mjs
// The console watch the card-only placements share: a ComfyUI whose prompt worker died after an out-of-memory error keeps answering HTTP (/history lists
// nothing, /system_stats is healthy, /queue still shows the prompt), so nothing the poll asks the server can tell. The runner reads the instance's own console
// instead: comfyLogCursor (comfy-lifecycle.mjs) reads what it gains, oomWedgeLine / createOomWatch (wan-multicard.mjs) recognise the two lines that mean the
// worker is gone, and pollOutputs (comfy-submit.mjs) takes the result as its `watch`. The Wan runner has had these since its review round; the image and
// edit runners use the same ones (multicard-image.mjs), so they are pinned here beside them, in a file of their own.
import { test } from "node:test";
import assert from "node:assert/strict";
import { writeFileSync, appendFileSync } from "node:fs";
import * as M from "./wan-multicard.mjs";
import * as L from "./comfy-lifecycle.mjs";
import { pollOutputs } from "./comfy-submit.mjs";
import { scratchDir } from "./test-scratch.mjs";

function clock() {
  let t = 1_000_000;
  return {
    now: () => t,
    sleep: async (ms) => { t += ms; },
    jump: (ms) => { t += ms; },
  };
}
const entry = (obj) => ({ ok: true, json: async () => obj, text: async () => "" });
const isStats = (url) => String(url).endsWith("/system_stats");

// ---------------------------------------------------------------------------------------
// pollOutputs: the watch hook (a wedge the server does not report)
// ---------------------------------------------------------------------------------------
// A ComfyUI whose prompt worker died after an out-of-memory error keeps answering HTTP: /history lists nothing, /system_stats is healthy, /queue
// still shows the prompt, and the lease keeps beating. Nothing the poll asks the server can tell, so a caller that can read the server's
// console hands the poll a `watch` that does: a message ends the wait.

test("pollOutputs: a watch that reports a wedge ends the wait with a server-unusable error, and is asked no more often than every 15 s", async () => {
  const c = clock();
  const start = c.now();
  const asked = [];
  const fetchImpl = async (url) => (isStats(url) ? entry({ system: {}, devices: [] }) : entry({}));
  const err = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 5400, isDone: () => false,
    watch: () => { asked.push(c.now() - start); return asked.length < 4 ? null : "WAN-OOM: the worker died"; },
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  }).then(() => null, (e) => e);
  assert.ok(err, "a wedged prompt must not be waited on for the whole budget");
  assert.match(err.message, /^WAN-OOM: the worker died/);
  assert.equal(err.serverUnusable, true, "no later job can run on a worker that is gone");
  assert.equal(asked.length, 4);
  for (let i = 1; i < asked.length; i++) assert.ok(asked[i] - asked[i - 1] >= 15_000, `asked ${asked[i] - asked[i - 1]} ms after the last time`);
  assert.ok((c.now() - start) / 1000 <= 90, `ended within about a minute, spent ${(c.now() - start) / 1000} s`);
});

test("pollOutputs: a watch that throws or rejects is best-effort, never the end of the wait", async () => {
  const c = clock();
  let polls = 0, asked = 0;
  const fetchImpl = async (url) => {
    if (isStats(url)) return entry({ system: {} });
    polls++;
    return polls < 40 ? entry({}) : entry({ p1: { outputs: { 9: { images: [{ filename: "a.png" }] } }, status: { status_str: "success" } } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 600, isDone: (e) => !!e.outputs?.[9]?.images?.[0],
    watch: async () => { asked++; if (asked % 2) throw new Error("the log cannot be read"); return Promise.reject(new Error("nor this")); },
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  });
  assert.equal(h.outputs[9].images[0].filename, "a.png");
  assert.ok(asked >= 2, "it was asked, and the failures did not stop the wait");
});

test("pollOutputs: a prompt that ComfyUI itself reports as failed is reported with ComfyUI's own words, whatever the watch would say", async () => {
  const c = clock();
  const status = { status_str: "error", messages: [] };
  const fetchImpl = async () => entry({ p1: { outputs: {}, status } });
  await assert.rejects(
    pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: () => false, watch: () => "WAN-OOM: would pre-empt", fetchImpl, sleep: c.sleep, now: c.now, env: {} }),
    /^Error: ComfyUI exec error: /,
  );
});

// ---- the console watch: an out-of-memory wedge ComfyUI does not report (the review of 2026-10-11) ---------------------------
// The benchmark's results note: "After an OOM inside DisTorch2 distribution ComfyUI can wedge with no error in history". The log of that
// run (the full-size probe) shows how: the node's error handler logs "Got an OOM, unloading all loaded models" and the unload itself runs
// out of memory, which kills the prompt worker thread. The lines below are that log's, with the ANSI colours ComfyUI prints.

const ESC = String.fromCharCode(27);
const WEDGE_LOG = [
  `  0%|          | 0/1 [00:00<?, ?it/s]${ESC}[1m${ESC}[33m[WARNING]${ESC}[0m FP8 _scaled_mm failed: Allocation on device 0 would exceed allowed memory. (out of memory)`,
  "Currently allocated     : 14.44 GiB",
  `${ESC}[1m${ESC}[31m[ERROR]${ESC}[0m !!! Exception during processing !!! Allocation on device 0 would exceed allowed memory. (out of memory)`,
  `${ESC}[1m${ESC}[31m[ERROR]${ESC}[0m Got an OOM, unloading all loaded models.`,
  "Exception in thread Thread-5 (prompt_worker):",
  "Traceback (most recent call last):",
  "torch.OutOfMemoryError: Allocation on device 0 would exceed allowed memory. (out of memory)",
].join("\n");

test("oomWedgeLine: the line that says the prompt worker is gone, found through the colour codes, and only that", () => {
  assert.equal(M.oomWedgeLine(WEDGE_LOG), "[ERROR] Got an OOM, unloading all loaded models.");
  assert.equal(M.oomWedgeLine("Exception in thread Thread-5 (prompt_worker):\nTraceback"), "Exception in thread Thread-5 (prompt_worker):");
  // recoverable or already reported: ComfyUI falls back, or puts the error in /history where the poll reads it
  assert.equal(M.oomWedgeLine(WEDGE_LOG.split("\n").slice(0, 3).join("\n")), "", "the _scaled_mm warning and the node error alone are not the wedge");
  assert.equal(M.oomWedgeLine("Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n"), "");
  assert.equal(M.oomWedgeLine("Exception in thread Thread-9 (some_other_worker):"), "");
  assert.equal(M.oomWedgeLine(""), "");
  assert.equal(M.oomWedgeLine(undefined), "");
});

test("comfyLogCursor reads only what the console gains after it was made, whatever the file held before", (t) => {
  const dir = scratchDir(t, "wan-mc-");
  const key = "mc-cccc3333-aaaa1111";
  const path = L.comfyLogPath(dir, key);
  writeFileSync(path, "an earlier run: Got an OOM, unloading all loaded models.\n");
  const cur = L.comfyLogCursor(dir, key);
  assert.deepEqual(cur.read(), { text: "", state: "ok" }, "the earlier run's lines are not this run's");
  appendFileSync(path, "step 1\n");
  assert.deepEqual(cur.read(), { text: "step 1\n", state: "ok" });
  assert.deepEqual(cur.read(), { text: "", state: "ok" }, "nothing new, nothing returned");
  // a line cut by the end of a read is finished by the next one
  appendFileSync(path, "Got an OOM, unl");
  const a = cur.read();
  appendFileSync(path, "oading all loaded models.\n");
  const b = cur.read();
  assert.equal(M.oomWedgeLine(a.text), "");
  assert.equal(M.oomWedgeLine(b.text), "Got an OOM, unloading all loaded models.");
  // a log that was replaced by a shorter one is read from its start
  writeFileSync(path, "fresh\n");
  assert.deepEqual(cur.read(), { text: "fresh\n", state: "ok" });
  // the capture's own cap marker means everything after it is lost to a reader
  appendFileSync(path, "\n... [offload-comfyui.log capped at 5MB for this run] ...\n");
  assert.equal(cur.read().state, "capped");
});

test("comfyLogCursor: a console file that does not exist is said, and one that appears later is read from its start", (t) => {
  const dir = scratchDir(t, "wan-mc-");
  const cur = L.comfyLogCursor(dir, "mc-cccc3333-aaaa1111");
  assert.deepEqual(cur.read(), { text: "", state: "absent" });
  writeFileSync(L.comfyLogPath(dir, "mc-cccc3333-aaaa1111"), "boot\n");
  assert.deepEqual(cur.read(), { text: "boot\n", state: "ok" });
});

test("createOomWatch remembers ComfyUI's plain-decode retry across reads, and says whether the console could be trusted to tell", () => {
  const verdict = (reads) => {
    const queue = [...reads];
    const watch = M.createOomWatch({ cursor: { path: "/c/x.log", read: () => queue.shift() || { text: "", state: "ok" } }, say: () => {} });
    for (let i = 0; i < reads.length; i++) watch();
    return watch.verdict;
  };
  const RETRY = "Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n";
  assert.equal(verdict([{ text: "a\n", state: "ok" }, { text: "b\n", state: "ok" }]), "clean");
  assert.equal(verdict([{ text: RETRY, state: "ok" }, { text: "later\n", state: "ok" }]), "fallback", "a retry seen on an earlier read is not forgotten");
  assert.equal(verdict([{ text: "a\n", state: "ok" }, { text: "", state: "absent" }]), "blind", "a console that is gone at the end cannot say");
  assert.equal(verdict([{ text: "x", state: "capped" }, { text: "", state: "ok" }]), "blind", "a capture that reached its cap is blind for the rest of the run, whatever it reads later");
  assert.equal(verdict([{ text: RETRY, state: "ok" }, { text: "", state: "absent" }]), "fallback", "seen is seen");
  assert.equal(verdict([]), "clean", "a watch never asked has seen nothing wrong");
});

test("createOomWatch: silent while the console is quiet, one message naming the line and the log when the worker is gone, a loud word when it cannot see", () => {
  const reads = [];
  const said = [];
  const cursor = { path: "/c/offload-comfyui-mc-x.log", read: () => reads.shift() };
  const watch = M.createOomWatch({ cursor, say: (m) => said.push(m) });
  reads.push({ text: "", state: "ok" }, { text: "step 2\n", state: "ok" });
  assert.equal(watch(), null);
  assert.equal(watch(), null);
  reads.push({ text: WEDGE_LOG, state: "ok" });
  const msg = watch();
  assert.match(msg, /^WAN-OOM: ComfyUI ran out of memory and its prompt worker did not survive it/);
  assert.match(msg, /\[ERROR\] Got an OOM, unloading all loaded models\./);
  assert.match(msg, /offload-comfyui-mc-x\.log/);
  assert.match(msg, /ComfyUI must be restarted/);
  assert.deepEqual(said, []);
  // blind: said once each, never a verdict
  const blind = [];
  const w2 = M.createOomWatch({ cursor: { path: "/c/x.log", read: () => blind.shift() }, say: (m) => said.push(m) });
  blind.push({ text: "", state: "absent" }, { text: "", state: "absent" }, { text: "", state: "capped" }, { text: "", state: "capped" });
  for (let i = 0; i < 4; i++) assert.equal(w2(), null);
  assert.equal(said.filter((m) => /^WAN-OOM-WATCH: .*no console file/.test(m)).length, 1);
  assert.equal(said.filter((m) => /^WAN-OOM-WATCH: .*capped/.test(m)).length, 1);
});
