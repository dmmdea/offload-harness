// comfy-submit.test.mjs — offline tests for the shared ComfyUI submission layer.
// No ComfyUI, no comfyui-pp-cli binary, no network beyond 127.0.0.1 ephemeral-port
// servers this file starts itself (CI contract: `node --test render/*.test.mjs` on a
// bare runner). The CLI path is exercised through an injected spawn double.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { EventEmitter } from "node:events";
import {
  resolveCli, runCli, parseCliJson, submitGraph, pollOutputs, fetchView, finalizeRun,
} from "./comfy-submit.mjs";
// The D-38 (#72) additions are read off the namespace so a missing export fails ITS test, not the whole file.
import * as submit from "./comfy-submit.mjs";

// ---------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------

// fakeSpawn: a scriptable child_process.spawn double. Each call shifts the next script:
// { code, stdout, stderr, error } — `error` emits the spawn "error" event (ENOENT class).
// Records every invocation (cmd, args, stdin) for assertions.
function fakeSpawn(scripts) {
  const calls = [];
  const impl = (cmd, args, opts) => {
    const script = scripts.shift() || { code: 0, stdout: "", stderr: "" };
    const child = new EventEmitter();
    child.stdout = new EventEmitter();
    child.stderr = new EventEmitter();
    child.stdout.destroy = () => {};
    child.stderr.destroy = () => {};
    let stdin = "";
    child.stdin = { write: (d) => { stdin += d; }, end: () => {}, on: () => {}, destroy: () => {} };
    child.kill = () => {};
    calls.push({ cmd, args, opts, get stdin() { return stdin; }, script });
    queueMicrotask(() => {
      if (script.error) { child.emit("error", script.error); return; }
      if (script.hang) return; // never closes — exercises killAfterMs
      if (script.stdout) child.stdout.emit("data", script.stdout);
      if (script.stderr) child.stderr.emit("data", script.stderr);
      child.emit("close", script.code);
    });
    return child;
  };
  return { impl, calls };
}

const devNull = { write: () => {} };
const enoent = () => Object.assign(new Error("spawn comfyui-pp-cli ENOENT"), { code: "ENOENT" });

// serve: one-shot local HTTP server; returns { api, close, requests }.
function serve(handler) {
  const requests = [];
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (d) => { body += d; });
    req.on("end", () => {
      requests.push({ method: req.method, url: req.url, body });
      handler(req, res, body);
    });
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      resolve({ api: `http://127.0.0.1:${port}`, close: () => server.close(), requests });
    });
  });
}

// tick/clock: deterministic time + instant sleep for pollOutputs. Each sleep(2000)
// advances the virtual clock by 2s so watchdog math runs without real waiting.
function clock() {
  let t = 1_000_000;
  return {
    now: () => t,
    sleep: async (ms) => { t += ms; },
    jump: (ms) => { t += ms; },
  };
}

// ---------------------------------------------------------------------------------------
// resolveCli
// ---------------------------------------------------------------------------------------

test("resolveCli: explicit COMFYUI_PP_CLI wins when the file exists", () => {
  const dir = mkdtempSync(join(tmpdir(), "cs-"));
  try {
    const bin = join(dir, "cli.exe");
    writeFileSync(bin, "");
    const cli = resolveCli({ env: { COMFYUI_PP_CLI: bin }, scriptDir: dir });
    assert.deepEqual(cli, { cmd: bin, source: "env" });
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("resolveCli: explicit COMFYUI_PP_CLI pointing nowhere fails LOUD, never degrades", () => {
  assert.throws(
    () => resolveCli({ env: { COMFYUI_PP_CLI: join(tmpdir(), "no-such-cli-xyz.exe") }, scriptDir: tmpdir() }),
    /COMFYUI_PP_CLI is set to .*no such file/,
  );
});

test("resolveCli: repo-local tools/comfyui/bin build is found relative to the script dir", () => {
  const dir = mkdtempSync(join(tmpdir(), "cs-"));
  try {
    const renderDir = join(dir, "render");
    const binDir = join(dir, "tools", "comfyui", "bin");
    const ext = process.platform === "win32" ? ".exe" : "";
    const bin = join(binDir, "comfyui-pp-cli" + ext);
    mkdirSync(renderDir, { recursive: true });
    mkdirSync(binDir, { recursive: true });
    writeFileSync(bin, "");
    const cli = resolveCli({ env: {}, scriptDir: renderDir });
    assert.deepEqual(cli, { cmd: bin, source: "repo-bin" });
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("resolveCli: nothing configured -> a PATH guess (probed at first use)", () => {
  const dir = mkdtempSync(join(tmpdir(), "cs-"));
  try {
    const cli = resolveCli({ env: {}, scriptDir: dir });
    assert.equal(cli.source, "path");
    assert.match(cli.cmd, /^comfyui-pp-cli/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

// ---------------------------------------------------------------------------------------
// parseCliJson
// ---------------------------------------------------------------------------------------

test("parseCliJson: direct envelope and results-wrapped envelope both resolve", () => {
  assert.equal(parseCliJson('{"prompt_id":"a"}').prompt_id, "a");
  assert.equal(parseCliJson('{"results":{"prompt_id":"b"}}').prompt_id, "b");
});

test("parseCliJson: garbage fails loudly with a sample", () => {
  assert.throws(() => parseCliJson("not json"), /unparseable JSON: not json/);
  assert.throws(() => parseCliJson("[1,2]"), /unexpected shape/);
});

// ---------------------------------------------------------------------------------------
// submitGraph — raw path
// ---------------------------------------------------------------------------------------

test("submitGraph raw: POST body carries the graph under prompt + the per-run client_id", async () => {
  const srv = await serve((req, res) => {
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ prompt_id: "raw-1" }));
  });
  try {
    const graph = { 9: { class_type: "SaveImage", inputs: {} } };
    const r = await submitGraph({ api: srv.api, graph, clientId: "render-42", cli: null });
    assert.deepEqual(r, { promptId: "raw-1", via: "raw", submit: null });
    assert.equal(srv.requests.length, 1);
    assert.equal(srv.requests[0].url, "/prompt");
    const body = JSON.parse(srv.requests[0].body);
    assert.deepEqual(body, { prompt: graph, client_id: "render-42" }); // exact legacy shape
  } finally { srv.close(); }
});

test("submitGraph raw: an HTTP error surfaces status + body excerpt", async () => {
  const srv = await serve((req, res) => { res.statusCode = 400; res.end('{"error":"bad graph"}'); });
  try {
    await assert.rejects(
      submitGraph({ api: srv.api, graph: {}, clientId: "x", cli: null }),
      /\/prompt -> 400 .*bad graph/,
    );
  } finally { srv.close(); }
});

// ---------------------------------------------------------------------------------------
// submitGraph — CLI path
// ---------------------------------------------------------------------------------------

test("submitGraph cli: accepted submit returns the CLI prompt_id; graph goes over stdin; lint stays off", async () => {
  const { impl, calls } = fakeSpawn([
    { code: 0, stdout: '{"action":"submitted","attached":false,"prompt_id":"cli-1"}' },
  ]);
  const graph = { 3: { class_type: "KSampler", inputs: {} } };
  const r = await submitGraph({
    api: "http://127.0.0.1:9", graph, clientId: "render-7",
    cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl, stderr: devNull,
  });
  assert.equal(r.promptId, "cli-1");
  assert.equal(r.via, "cli");
  assert.deepEqual(calls[0].args, ["submit", "-", "--json", "--skip-lint"]);
  assert.equal(calls[0].stdin, JSON.stringify(graph));
  // The env is the ONLY channel by which the runner's --api/COMFY_API reaches the CLI:
  // dropping it would silently submit every render to the CLI's default server.
  assert.equal(calls[0].opts.env.COMFYUI_BASE_URL, "http://127.0.0.1:9");
  assert.equal(calls[0].opts.windowsHide, true); // house rule: no visible console windows
});

test("submitGraph cli: exit 1 with a POSTED envelope must NOT fall back — retrying raw would double-render", async () => {
  // A post-POST local step (--deliver sink failure, profile teardown) can replace the
  // error and exit 1 AFTER the server queued the render. The envelope is the evidence:
  // prompt_id / a real http_status mean the POST landed.
  const { impl } = fakeSpawn([
    { code: 1, stdout: '{"action":"submitted","prompt_id":"landed-1","http_status":200}', stderr: "deliver: webhook returned 500" },
  ]);
  await assert.rejects(
    submitGraph({
      api: "http://127.0.0.1:9", graph: {}, clientId: "x",
      cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl, stderr: devNull,
    }),
    /exited 1 AFTER the POST landed .*landed-1/,
  );
});

test("submitGraph cli: a pre-POST envelope (http_status 0, no prompt_id) still falls back on exit 1", async () => {
  const srv = await serve((req, res) => res.end(JSON.stringify({ prompt_id: "raw-pre" })));
  try {
    const { impl } = fakeSpawn([
      { code: 1, stdout: '{"error":"opening the local run store: disk I/O error","http_status":0}', stderr: "" },
    ]);
    let warned = "";
    const r = await submitGraph({
      api: srv.api, graph: {}, clientId: "x",
      cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl,
      stderr: { write: (s) => { warned += s; } },
    });
    assert.equal(r.via, "raw");
    assert.equal(r.promptId, "raw-pre");
    assert.match(warned, /comfyui-pp-cli \(env: cli\.exe\).*failed locally before POSTing/);
  } finally { srv.close(); }
});

test("submitGraph cli: a LOCAL pre-POST failure (usage/config/generic) falls back to raw LOUDLY instead of killing the render", async () => {
  for (const code of [1, 2, 10]) {
    const srv = await serve((req, res) => res.end(JSON.stringify({ prompt_id: "raw-local-" + code })));
    try {
      const { impl } = fakeSpawn([{ code, stdout: "", stderr: "local failure " + code }]);
      let warned = "";
      const r = await submitGraph({
        api: srv.api, graph: { a: 1 }, clientId: "render-9",
        cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl,
        stderr: { write: (s) => { warned += s; } },
      });
      assert.equal(r.via, "raw", "exit " + code + " must fall back");
      assert.equal(r.promptId, "raw-local-" + code);
      assert.match(warned, new RegExp("failed locally before POSTing \\(exit " + code + "\\)"));
      assert.equal(JSON.parse(srv.requests[0].body).client_id, "render-9");
    } finally { srv.close(); }
  }
});

test("submitGraph cli: PATH guess that ENOENTs falls back to raw with a notice", async () => {
  const srv = await serve((req, res) => res.end(JSON.stringify({ prompt_id: "raw-fb" })));
  try {
    const { impl } = fakeSpawn([{ error: enoent() }]);
    let notice = "";
    const r = await submitGraph({
      api: srv.api, graph: { a: 1 }, clientId: "video-1",
      cli: { cmd: "comfyui-pp-cli", source: "path" }, spawnImpl: impl,
      stderr: { write: (s) => { notice += s; } },
    });
    assert.equal(r.via, "raw");
    assert.equal(r.promptId, "raw-fb");
    assert.match(notice, /not found on PATH/);
    assert.equal(JSON.parse(srv.requests[0].body).client_id, "video-1");
  } finally { srv.close(); }
});

test("submitGraph cli: a POSITIVELY resolved binary that cannot start is an error, not a fallback", async () => {
  const { impl } = fakeSpawn([{ error: enoent() }]);
  await assert.rejects(
    submitGraph({
      api: "http://127.0.0.1:9", graph: {}, clientId: "x",
      cli: { cmd: "C:/bin/cli.exe", source: "env" }, spawnImpl: impl, stderr: devNull,
    }),
    /failed to start/,
  );
});

test("submitGraph cli: non-zero exit surfaces the code and stderr tail", async () => {
  const { impl } = fakeSpawn([
    { code: 21, stdout: '{"action":"rejected","exit_code":21}', stderr: "FAIL rejected (HTTP 400) — nothing was queued" },
  ]);
  await assert.rejects(
    submitGraph({
      api: "http://127.0.0.1:9", graph: {}, clientId: "x",
      cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl, stderr: devNull,
    }),
    /submit exited 21: .*nothing was queued/,
  );
});

test("submitGraph cli: PARTIAL ACCEPT (exit 22) warns loudly and keeps rendering — the raw path's behavior", async () => {
  const { impl } = fakeSpawn([
    { code: 22, stdout: '{"action":"partial-accept","prompt_id":"cli-p","exit_code":22}', stderr: "WARN PARTIAL ACCEPT" },
  ]);
  let warned = "";
  const r = await submitGraph({
    api: "http://127.0.0.1:9", graph: {}, clientId: "x",
    cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl,
    stderr: { write: (s) => { warned += s; } },
  });
  assert.equal(r.promptId, "cli-p");
  assert.match(warned, /partial accept/);
});

test("submitGraph cli: attached to a LIVE identical run reuses its prompt_id (the lease working)", async () => {
  const { impl, calls } = fakeSpawn([
    { code: 0, stdout: '{"action":"attached","attached":true,"prompt_id":"live-1"}' },
    { code: 0, stdout: '{"found":true,"attached":true,"in_flight":true,"live_state":"running","prompt_id":"live-1"}' },
  ]);
  let note = "";
  const r = await submitGraph({
    api: "http://127.0.0.1:9", graph: {}, clientId: "x",
    cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl,
    stderr: { write: (s) => { note += s; } },
  });
  assert.equal(r.promptId, "live-1");
  assert.deepEqual(calls[1].args, ["attach", "live-1", "--json"]);
  assert.match(note, /attached to live-1/);
});

test("submitGraph cli: a STALE lease (live_state unknown) forces a fresh render — today's always-POST behavior", async () => {
  const { impl, calls } = fakeSpawn([
    { code: 0, stdout: '{"action":"attached","attached":true,"prompt_id":"stale-1"}' },
    { code: 0, stdout: '{"found":true,"live_state":"unknown","prompt_id":"stale-1"}' },
    { code: 0, stdout: '{"action":"submitted","attached":false,"prompt_id":"fresh-2"}' },
  ]);
  const r = await submitGraph({
    api: "http://127.0.0.1:9", graph: {}, clientId: "x",
    cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl, stderr: devNull,
  });
  assert.equal(r.promptId, "fresh-2");
  assert.deepEqual(calls[2].args, ["submit", "-", "--json", "--skip-lint", "--force"]);
});

test("submitGraph cli: a FAILED liveness probe counts as stale (uncertainty must not hang a render on a dead prompt) and NAMES the probe failure", async () => {
  const { impl, calls } = fakeSpawn([
    { code: 0, stdout: '{"action":"attached","attached":true,"prompt_id":"stale-2"}' },
    { code: 3, stdout: "", stderr: "no run recorded" },
    { code: 0, stdout: '{"action":"submitted","prompt_id":"fresh-3"}' },
  ]);
  let note = "";
  const r = await submitGraph({
    api: "http://127.0.0.1:9", graph: {}, clientId: "x",
    cli: { cmd: "cli.exe", source: "env" }, spawnImpl: impl,
    stderr: { write: (s) => { note += s; } },
  });
  assert.equal(r.promptId, "fresh-3");
  assert.equal(calls.length, 3);
  // A systematically broken `attach` must not read as routine staleness forever.
  assert.match(note, /liveness probe exited 3: no run recorded/);
});

test("submitGraph cli: a PATH-guess failure that is NOT ENOENT throws — a broken binary is an error, not the raw tier", async () => {
  const eacces = Object.assign(new Error("spawn comfyui-pp-cli EACCES"), { code: "EACCES" });
  const { impl } = fakeSpawn([{ error: eacces }]);
  await assert.rejects(
    submitGraph({
      api: "http://127.0.0.1:9", graph: {}, clientId: "x",
      cli: { cmd: "comfyui-pp-cli", source: "path" }, spawnImpl: impl, stderr: devNull,
    }),
    /failed to start: spawn comfyui-pp-cli EACCES/,
  );
});

test("submitGraph raw: a 200 with no prompt_id fails NOW, not after burning the poll budget on /history/undefined", async () => {
  const srv = await serve((req, res) => { res.end("{}"); });
  try {
    await assert.rejects(
      submitGraph({ api: srv.api, graph: {}, clientId: "x", cli: null }),
      /\/prompt returned no prompt_id/,
    );
  } finally { srv.close(); }
});

// ---------------------------------------------------------------------------------------
// pollOutputs
// ---------------------------------------------------------------------------------------

const entry = (obj) => ({ ok: true, json: async () => obj, text: async () => "" });

test("pollOutputs: returns the history entry once isDone accepts it", async () => {
  const c = clock();
  let polls = 0;
  const fetchImpl = async () => {
    polls++;
    if (polls < 3) return entry({}); // not in history yet
    return entry({ p1: { outputs: { 9: { images: [{ filename: "a.png" }] } }, status: { status_str: "success" } } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 60,
    isDone: (e) => !!e.outputs?.[9]?.images?.[0],
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  });
  assert.equal(h.outputs[9].images[0].filename, "a.png");
  assert.equal(polls, 3);
});

test("pollOutputs: an exec-error status throws with the unified prefix and fires onExecError first", async () => {
  const c = clock();
  const status = { status_str: "error", messages: [] };
  const fetchImpl = async () => entry({ p1: { outputs: {}, status } });
  let hooked = false;
  await assert.rejects(
    pollOutputs({
      api: "http://x", promptId: "p1", waitSec: 60, isDone: () => false,
      onExecError: async () => { hooked = true; },
      fetchImpl, sleep: c.sleep, now: c.now, env: {},
    }),
    /^Error: ComfyUI exec error: \{"status_str":"error"/,
  );
  assert.ok(hooked, "onExecError must run before the throw");
});

test("pollOutputs: ComfyUI's own exception message reaches the error whole, even when the status is long enough for the 500-character cut to lose it", async () => {
  const c = clock();
  const message = "OFFLOAD-DECODE-NOFIT: cuda:0 has 6.1 GiB free and the plain decode needs 10.0 GiB; the sampled latent is saved as latents/wan_00001_.latent";
  const status = {
    status_str: "error", completed: false,
    messages: [
      ["execution_start", { prompt_id: "p1", timestamp: 1 }],
      ["execution_cached", { nodes: [], prompt_id: "p1", timestamp: 2 }],
      ["execution_error", {
        prompt_id: "p1", node_id: "21", node_type: "OffloadFreeForDecode", executed: Array.from({ length: 20 }, (_, i) => String(i + 1)),
        exception_message: message, exception_type: "RuntimeError", traceback: ["  File \"execution.py\", line 1, in execute\n"], current_inputs: {}, current_outputs: {},
      }],
    ],
  };
  assert.ok(!JSON.stringify(status).slice(0, 500).includes("latents/wan_00001_.latent"), "premise: the cut lands before the message");
  const fetchImpl = async () => entry({ p1: { outputs: {}, status } });
  const err = await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: () => false, fetchImpl, sleep: c.sleep, now: c.now, env: {} }).then(() => null, (e) => e);
  assert.match(err.message, /^ComfyUI exec error: \{"status_str":"error"/, "the unified prefix is what it was");
  assert.ok(err.message.includes(message), err.message);
  assert.match(err.message, /OffloadFreeForDecode.*RuntimeError/s, "and it says which node raised what");
  // a status that is short enough to show whole says nothing twice
  const short = await pollOutputs({ api: "http://x", promptId: "p1", waitSec: 60, isDone: () => false, fetchImpl: async () => entry({ p1: { outputs: {}, status: { status_str: "error", messages: [["execution_error", { exception_message: "boom", node_type: "N", exception_type: "E" }]] } } }), sleep: c.sleep, now: c.now, env: {} }).then(() => null, (e) => e);
  assert.equal(short.message.split("boom").length - 1, 1, short.message);
});

test("pollOutputs: budget exhaustion throws the caller's message", async () => {
  const c = clock();
  const fetchImpl = async () => entry({});
  await assert.rejects(
    pollOutputs({
      api: "http://x", promptId: "p1", waitSec: 10, isDone: () => false,
      noOutputMsg: "no edited image produced in time",
      fetchImpl, sleep: c.sleep, now: c.now, env: {},
    }),
    /no edited image produced in time/,
  );
});

test("pollOutputs: dead-server watchdog aborts after COMFY_DEAD_SEC of consecutive network failures", async () => {
  const c = clock();
  const fetchImpl = async () => { throw new Error("fetch failed: ECONNREFUSED"); };
  await assert.rejects(
    pollOutputs({
      api: "http://x", promptId: "p1", waitSec: 3600, isDone: () => false,
      fetchImpl, sleep: c.sleep, now: c.now, env: { COMFY_DEAD_SEC: "10" },
    }),
    /stopped answering mid-render \(unreachable \d+s, COMFY_DEAD_SEC=10\); aborting early to release the GPU slot/,
  );
});

test("pollOutputs: an HTTP error status IS an answer — it resets the watchdog instead of feeding it", async () => {
  const c = clock();
  let polls = 0;
  const fetchImpl = async (url) => {
    // A healthy server's /system_stats (C-83's probe): only /history errors here, which is
    // what this test is about. A /system_stats that errored too would be a poisoned server.
    if (String(url).endsWith("/system_stats")) return entry({ system: {} });
    polls++;
    // Polls 1-20: HTTP 500s — 40 virtual seconds, far past COMFY_DEAD_SEC=10, but every
    // one is an ANSWER (server alive), so each must re-base the watchdog clock.
    if (polls <= 20) return { ok: false, status: 500, text: async () => "internal", json: async () => ({}) };
    // Poll 21: ONE network failure. Only 2s since the last ANSWER — if the 500s had not
    // re-based lastAnswerAt, deadFor would be 42s here and the watchdog would abort a
    // server that answered two seconds ago.
    if (polls === 21) throw new Error("one transient network blip");
    return entry({ p1: { outputs: { 9: { images: [{ filename: "late.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 3600, isDone: (e) => !!e.outputs,
    fetchImpl, sleep: c.sleep, now: c.now, env: { COMFY_DEAD_SEC: "10" },
  });
  assert.equal(h.outputs[9].images[0].filename, "late.png");
});

test("pollOutputs: suspend/resume fence — a timer jump past 120s never counts as dead time", async () => {
  // The machine "sleeps" 10 minutes between poll 1 and poll 2 (lid closed mid-render,
  // by design on this fleet): the inter-poll sleep spans the suspend, so the virtual
  // clock jumps 600s across it. The fence detects the jump at the top of the next
  // iteration and re-bases lastAnswerAt; the fence protects exactly this boundary.
  const c = clock();
  let polls = 0;
  let nap = false;
  const sleep = async (ms) => { c.jump(nap ? 600_000 : ms); nap = false; };
  const fetchImpl = async () => {
    polls++;
    if (polls === 1) { nap = true; throw new Error("network blip before the nap"); }
    if (polls === 2) throw new Error("still waking up");
    return entry({ p1: { outputs: { 9: { images: [{ filename: "resumed.png" }] } }, status: {} } });
  };
  // COMFY_DEAD_SEC=30: without the fence, poll 2's failure would see 600s+ of "dead"
  // time and abort a healthy render. With it, the render survives the nap.
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 3600, isDone: (e) => !!e.outputs,
    fetchImpl, sleep, now: c.now, env: { COMFY_DEAD_SEC: "30" },
  });
  assert.equal(h.outputs[9].images[0].filename, "resumed.png");
});

// C-83, 2026-10-01 12:00:10: a sticky CUDA OOM killed ComfyUI's prompt worker on card 2.
// HTTP kept answering: /history/<id> said 200 {} for 48 minutes (read as a slow render)
// while /system_stats said HTTP 500 (torch.cuda.mem_get_info on the dead context), and
// the media lease held all three cards with nothing running.
const http500 = () => ({ ok: false, status: 500, text: async () => "500 Internal Server Error", json: async () => ({}) });
const isStats = (url) => String(url).endsWith("/system_stats");

test("pollOutputs: the wait budget is wall-clock — slow /history answers cannot stretch it (C-83)", async () => {
  // COMFY_WAIT_SEC=1500 ran 2,894 s: the loop counted polls, not seconds.
  const c = clock();
  const start = c.now();
  const fetchImpl = async () => { c.jump(2000); return entry({}); }; // every answer takes 2 s
  await assert.rejects(
    pollOutputs({
      api: "http://x", promptId: "p1", waitSec: 60, isDone: () => false,
      noOutputMsg: "no image produced in time",
      fetchImpl, sleep: c.sleep, now: c.now, env: {},
    }),
    /no image produced in time/,
  );
  const spent = (c.now() - start) / 1000;
  assert.ok(spent <= 66, `a 60 s budget spent ${spent} s`);
});

test("pollOutputs: a prompt that never finishes while /system_stats answers HTTP 500 is a poisoned server — abort in about a minute, marked serverUnusable (C-83)", async () => {
  const c = clock();
  const start = c.now();
  let probes = 0;
  const fetchImpl = async (url) => {
    if (isStats(url)) { probes++; return http500(); }
    return entry({});
  };
  const err = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 1500, isDone: () => false,
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  }).then(() => null, (e) => e);
  assert.ok(err, "a poisoned server must not be waited on for the whole budget");
  assert.equal(err.serverUnusable, true);
  assert.match(err.message, /\/system_stats answered HTTP 500/);
  assert.match(err.message, /restart/);
  assert.ok(probes >= 2, `one failed probe is not a verdict (probes: ${probes})`);
  const spent = (c.now() - start) / 1000;
  assert.ok(spent <= 90, `must abort within about a minute, spent ${spent} s`);
});

test("pollOutputs: a poisoned server whose /history ALSO answers HTTP errors is caught by the probe too (C-83 review)", async () => {
  // The incident server said 200 {} on /history; a variant answers 5xx there. Every error
  // status counts as an answer (the watchdog stays quiet), so only the probe can catch it.
  const c = clock();
  const start = c.now();
  const fetchImpl = async () => http500();
  const err = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 1500, isDone: () => false,
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  }).then(() => null, (e) => e);
  assert.ok(err, "must not wait out the whole budget");
  assert.equal(err.serverUnusable, true);
  assert.match(err.message, /\/system_stats answered HTTP 500/);
  const spent = (c.now() - start) / 1000;
  assert.ok(spent <= 90, `must abort within about a minute, spent ${spent} s`);
});

test("pollOutputs: a healthy /system_stats keeps a slow-but-honest render alive across many probes", async () => {
  const c = clock();
  let hist = 0;
  const fetchImpl = async (url) => {
    if (isStats(url)) return entry({ system: {}, devices: [] });
    hist++;
    if (hist < 300) return entry({}); // ten minutes of "not finished yet"
    return entry({ p1: { outputs: { 9: { images: [{ filename: "slow.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 1800, isDone: (e) => !!e.outputs,
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  });
  assert.equal(h.outputs[9].images[0].filename, "slow.png");
});

test("pollOutputs: a /system_stats failure followed by a healthy probe resets the count — only consecutive failures are a verdict", async () => {
  const c = clock();
  let probes = 0, hist = 0;
  const fetchImpl = async (url) => {
    if (isStats(url)) { probes++; return probes % 2 === 1 ? http500() : entry({ system: {} }); }
    hist++;
    if (hist < 200) return entry({});
    return entry({ p1: { outputs: { 9: { images: [{ filename: "flaky.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 1800, isDone: (e) => !!e.outputs,
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  });
  assert.equal(h.outputs[9].images[0].filename, "flaky.png");
  assert.ok(probes >= 4, `the probe must have run and failed more than once (probes: ${probes})`);
});

test("pollOutputs: a /system_stats probe that gets no answer at all is not a poisoned-server verdict", async () => {
  // A network failure on the probe is the dead-server watchdog's question (it watches
  // /history); only an ANSWERED error status says the context is broken.
  const c = clock();
  let hist = 0;
  const fetchImpl = async (url) => {
    if (isStats(url)) throw new Error("fetch failed: socket hang up");
    hist++;
    if (hist < 100) return entry({});
    return entry({ p1: { outputs: { 9: { images: [{ filename: "late.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 1800, isDone: (e) => !!e.outputs,
    fetchImpl, sleep: c.sleep, now: c.now, env: {},
  });
  assert.equal(h.outputs[9].images[0].filename, "late.png");
});

test("pollOutputs: the dead-server watchdog's abort is marked serverUnusable — no later job can run on a vanished server (C-83)", async () => {
  const c = clock();
  const fetchImpl = async () => { throw new Error("fetch failed: ECONNREFUSED"); };
  const err = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 3600, isDone: () => false,
    fetchImpl, sleep: c.sleep, now: c.now, env: { COMFY_DEAD_SEC: "10" },
  }).then(() => null, (e) => e);
  assert.ok(err);
  assert.match(err.message, /stopped answering mid-render/);
  assert.equal(err.serverUnusable, true);
});

test("pollOutputs: a suspend DURING an in-flight /history poll is neither dead time nor budget — no server-unusable abort on wake", async () => {
  // Review finding on C-83: the fence only ran at the top of the next tick, so a lid that
  // closed while a request was in flight woke into a failed fetch whose dead time covered
  // the whole nap; with the server-unusable flag that would stop a whole batch.
  const c = clock();
  let hist = 0;
  const fetchImpl = async (url) => {
    if (isStats(url)) return entry({ system: {} });
    hist++;
    if (hist === 3) { c.jump(1_000_000); throw new Error("fetch failed: socket reset after resume"); }
    if (hist < 6) return entry({});
    return entry({ p1: { outputs: { 9: { images: [{ filename: "woke.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs,
    fetchImpl, sleep: c.sleep, now: c.now, env: { COMFY_DEAD_SEC: "240" },
  });
  assert.equal(h.outputs[9].images[0].filename, "woke.png");
});

test("pollOutputs: a suspend (timer jump past 120 s) does not consume the wall-clock budget", async () => {
  const c = clock();
  let hist = 0;
  let nap = false;
  const sleep = async (ms) => { c.jump(nap ? 600_000 : ms); nap = false; };
  const fetchImpl = async (url) => {
    if (isStats(url)) return entry({ system: {} });
    hist++;
    if (hist === 1) nap = true; // the lid closes for ten minutes after the first poll
    if (hist < 10) return entry({});
    return entry({ p1: { outputs: { 9: { images: [{ filename: "after-nap.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 60, isDone: (e) => !!e.outputs,
    fetchImpl, sleep, now: c.now, env: {},
  });
  assert.equal(h.outputs[9].images[0].filename, "after-nap.png");
});

// ---------------------------------------------------------------------------------------
// fetchView
// ---------------------------------------------------------------------------------------

test("fetchView: bytes come back exactly and the query carries filename/subfolder/type defaults", async () => {
  const payload = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x00, 0xff, 0x01]);
  const srv = await serve((req, res) => { res.end(payload); });
  try {
    const buf = await fetchView({ api: srv.api, file: { filename: "a.png" } });
    assert.deepEqual(buf, payload);
    assert.equal(srv.requests[0].url, "/view?filename=a.png&subfolder=&type=output");
  } finally { srv.close(); }
});

test("fetchView: a non-OK response throws the legacy message shape", async () => {
  const srv = await serve((req, res) => { res.statusCode = 404; res.end(); });
  try {
    await assert.rejects(fetchView({ api: srv.api, file: { filename: "x" } }), /view fetch 404/);
  } finally { srv.close(); }
});

// ---------------------------------------------------------------------------------------
// finalizeRun
// ---------------------------------------------------------------------------------------

test("finalizeRun: no CLI -> no-op (raw mode adds zero calls)", async () => {
  assert.equal(await finalizeRun({ api: "http://x", promptId: "p", cli: null }), null);
});

test("finalizeRun: prints the authoritative timing line from the CLI wait envelope", async () => {
  const { impl, calls } = fakeSpawn([
    { code: 0, stdout: '{"prompt_id":"p","outcome":"completed","status":{"duration_ms":72400}}' },
  ]);
  let line = "";
  const r = await finalizeRun({
    api: "http://x", promptId: "p", cli: { cmd: "cli.exe", source: "env" },
    spawnImpl: impl, stdout: { write: (s) => { line += s; } }, stderr: devNull,
  });
  assert.deepEqual(r, { durationMs: 72400 });
  assert.match(line, /timing 72\.4s \(history execution_start -> execution_success, via comfyui-pp-cli\)/);
  assert.deepEqual(calls[0].args, ["wait", "p", "--timeout", "5s", "--json"]);
});

test("finalizeRun: a GENUINE finalization failure (not-found after restart) degrades to a warning — never re-opens the render", async () => {
  const { impl, calls } = fakeSpawn([{ code: 3, stdout: "", stderr: "prompt p is in neither the queue nor /history" }]);
  let warned = "";
  const r = await finalizeRun({
    api: "http://x", promptId: "p", cli: { cmd: "cli.exe", source: "env" },
    spawnImpl: impl, stdout: devNull, stderr: { write: (s) => { warned += s; } },
  });
  assert.equal(r, null);
  assert.match(warned, /wait exited 3/);
  assert.equal(calls[0].opts.env.COMFYUI_BASE_URL, "http://x"); // the CLI must be aimed at the caller's server
});

test("finalizeRun: wait's terminal-OUTCOME codes (13/21/22/24/25) ARE successful finalizations — no cry-wolf warning", async () => {
  // Exit 21 = the RENDER failed; the run row was finalized with the verbatim error.
  // The caller already knows the render's fate — warning here would make every failed
  // render report a bogus bookkeeping failure.
  const { impl } = fakeSpawn([
    { code: 21, stdout: '{"prompt_id":"p","outcome":"failed","status":{"duration_ms":0}}', stderr: "FAIL run failed" },
  ]);
  let warned = "";
  const r = await finalizeRun({
    api: "http://x", promptId: "p", cli: { cmd: "cli.exe", source: "env" },
    spawnImpl: impl, stdout: devNull, stderr: { write: (s) => { warned += s; } },
  });
  assert.deepEqual(r, { durationMs: 0 });
  assert.equal(warned, "");
});

test("runCli: killAfterMs tears the child down and resolves with a pseudo spawnError", async () => {
  const { impl } = fakeSpawn([{ hang: true }]);
  const r = await runCli("cli.exe", ["wait", "p"], { spawnImpl: impl, killAfterMs: 50 });
  assert.equal(r.code, null);
  assert.match(r.spawnError.message, /did not exit within 50ms/);
});

// ---------------------------------------------------------------------------------------
// runCli plumbing
// ---------------------------------------------------------------------------------------

test("runCli: resolves with code/stdout/stderr and never rejects on spawn error", async () => {
  const { impl } = fakeSpawn([{ error: enoent() }]);
  const r = await runCli("comfyui-pp-cli", ["version"], { spawnImpl: impl });
  assert.equal(r.code, null);
  assert.match(r.spawnError.message, /ENOENT/);
});

test("runCli REAL SPAWN: a child that exits before draining a large stdin resolves with its exit code — never an uncaughtException", async () => {
  // Regression for the EPIPE/EOF crash class (silent-failure audit, 2026-08-14): a CLI
  // that prints usage and dies before reading stdin (flag skew, startup panic, missing
  // DLL) made the pending stdin write emit an unhandled 'error' that killed the whole
  // runner outside every catch — no typed DEFER, no GPU teardown. Deterministic once
  // the input exceeds the OS pipe buffer, so 100KB forces it. Uses the real node
  // binary, so it runs on any CI runner.
  const r = await runCli(process.execPath, ["-e", "process.exit(2)"], { input: "x".repeat(100_000) });
  assert.equal(r.spawnError, null);
  assert.equal(r.code, 2);
});

test("finalizeRun: a non-ENOENT spawn failure on the PATH guess still WARNS (a wedged wait must not vanish)", async () => {
  const eacces = Object.assign(new Error("spawn comfyui-pp-cli EACCES"), { code: "EACCES" });
  const { impl } = fakeSpawn([{ error: eacces }]);
  let warned = "";
  const r = await finalizeRun({
    api: "http://x", promptId: "p", cli: { cmd: "comfyui-pp-cli", source: "path" },
    spawnImpl: impl, stdout: devNull, stderr: { write: (s) => { warned += s; } },
  });
  assert.equal(r, null);
  assert.match(warned, /timing capture skipped .*EACCES/);
});

test("finalizeRun: PATH-guess ENOENT stays silent (submit already printed the one raw-fallback notice)", async () => {
  const { impl } = fakeSpawn([{ error: enoent() }]);
  let warned = "";
  const r = await finalizeRun({
    api: "http://x", promptId: "p", cli: { cmd: "comfyui-pp-cli", source: "path" },
    spawnImpl: impl, stdout: devNull, stderr: { write: (s) => { warned += s; } },
  });
  assert.equal(r, null);
  assert.equal(warned, "");
});

test("pollOutputs: an error status whose BODY read fails still counts as an answer, not dead time", async () => {
  const c = clock();
  let polls = 0;
  const fetchImpl = async (url) => {
    if (String(url).endsWith("/system_stats")) return entry({ system: {} }); // healthy: see the C-83 probe
    polls++;
    if (polls <= 20) {
      // Server answers 502 but the error-page body read dies mid-stream.
      return { ok: false, status: 502, text: async () => { throw new Error("aborted mid-body"); }, json: async () => ({}) };
    }
    return entry({ p1: { outputs: { 9: { images: [{ filename: "ok.png" }] } }, status: {} } });
  };
  const h = await pollOutputs({
    api: "http://x", promptId: "p1", waitSec: 3600, isDone: (e) => !!e.outputs,
    fetchImpl, sleep: c.sleep, now: c.now, env: { COMFY_DEAD_SEC: "10" },
  });
  assert.equal(h.outputs[9].images[0].filename, "ok.png");
});

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

// ---------------------------------------------------------------------------------------
// D-38 (#72): POST /prompt and GET /view are bounded
// ---------------------------------------------------------------------------------------
//
// rawSubmit and fetchView passed no AbortSignal (the /system_stats, /queue and /history calls always did), so a
// ComfyUI that accepts the connection and never answers held the exclusive GPU slot for as long as the runner's
// whole-process timeout. Each call is now bounded: SUBMIT_TIMEOUT_MS for POST /prompt (it validates and queues, it
// does not run) and VIEW_TIMEOUT_MS for GET /view, which covers the BODY read too. Every test here races the call
// against its own sentinel timer, so the failure today is an assertion, not a hung test runner.

// settles: how a promise ended within `ms`, or { hung: true }. The timer is cleared, so it keeps nothing alive.
async function settles(promise, ms = 3000) {
  let timer;
  const sentinel = new Promise((resolve) => { timer = setTimeout(() => resolve({ hung: true }), ms); });
  try {
    return await Promise.race([promise.then((value) => ({ ok: true, value }), (error) => ({ ok: false, error })), sentinel]);
  } finally { clearTimeout(timer); }
}

// stalledFetch: a fetchImpl that honours its signal and otherwise never answers. calls keeps every (url, opts).
function stalledFetch(calls = []) {
  return (url, opts = {}) => new Promise((resolve, reject) => {
    calls.push({ url, opts });
    const s = opts.signal;
    if (s) {
      if (s.aborted) return reject(s.reason);
      s.addEventListener("abort", () => reject(s.reason), { once: true });
    }
  });
}

test("the timeouts are exported constants a test can shorten: 120 s to queue a prompt, 600 s to fetch an output", () => {
  assert.equal(submit.SUBMIT_TIMEOUT_MS, 120_000);
  assert.equal(submit.VIEW_TIMEOUT_MS, 600_000);
});

test("rawSubmit rejects a POST /prompt that never answers, naming the call and the bound", async () => {
  assert.equal(typeof submit.rawSubmit, "function", "comfy-submit.mjs must export rawSubmit");
  const r = await settles(submit.rawSubmit({ api: "http://comfy.invalid", graph: { 1: {} }, clientId: "c", fetchImpl: stalledFetch(), timeoutMs: 60 }));
  assert.ok(!r.hung, "rawSubmit never settled: a ComfyUI that accepts the connection and never answers holds the GPU slot");
  assert.equal(r.ok, false);
  assert.match(r.error.message, /POST \/prompt/);
  assert.match(r.error.message, /60 ms/);
  assert.match(r.error.message, /timeout/i);
});

test("fetchView rejects a GET /view that never answers, naming the call and the bound", async () => {
  const r = await settles(fetchView({ api: "http://comfy.invalid", file: { filename: "a.png" }, fetchImpl: stalledFetch(), timeoutMs: 60 }));
  assert.ok(!r.hung, "fetchView never settled: a ComfyUI that stops answering GET /view holds the GPU slot");
  assert.equal(r.ok, false);
  assert.match(r.error.message, /GET \/view/);
  assert.match(r.error.message, /60 ms/);
  assert.match(r.error.message, /timeout/i);
});

test("fetchView bounds the whole request: a body that stalls after the headers is cut at the same bound", async () => {
  // the headers arrive at once; arrayBuffer() only settles when the signal aborts (as undici's does mid-body)
  const fetchImpl = (url, opts = {}) => Promise.resolve({
    ok: true, status: 200,
    arrayBuffer: () => new Promise((_, reject) => { opts.signal.addEventListener("abort", () => reject(opts.signal.reason), { once: true }); }),
  });
  const r = await settles(fetchView({ api: "http://comfy.invalid", file: { filename: "big.mp4" }, fetchImpl, timeoutMs: 60 }));
  assert.ok(!r.hung, "the body read was not bounded");
  assert.equal(r.ok, false);
  assert.match(r.error.message, /GET \/view/);
});

test("both calls pass an AbortSignal", async () => {
  const calls = [];
  const ok = (body) => ({ ok: true, status: 200, json: async () => body, arrayBuffer: async () => new ArrayBuffer(1) });
  await submit.rawSubmit({ api: "http://x", graph: {}, clientId: "c", fetchImpl: async (url, opts) => { calls.push({ url, opts }); return ok({ prompt_id: "p1" }); } });
  await fetchView({ api: "http://x", file: { filename: "a.png" }, fetchImpl: async (url, opts) => { calls.push({ url, opts }); return ok({}); } });
  assert.equal(calls.length, 2);
  for (const c of calls) assert.ok(c.opts && c.opts.signal instanceof AbortSignal, `${c.url} carries no AbortSignal`);
});

test("submitGraph passes its raw POST the submit bound (the production caller of rawSubmit)", async () => {
  const r = await settles(submitGraph({ api: "http://comfy.invalid", graph: {}, clientId: "c", cli: null, fetchImpl: stalledFetch(), submitTimeoutMs: 60 }));
  assert.ok(!r.hung, "submitGraph's raw path never settled");
  assert.equal(r.ok, false);
  assert.match(r.error.message, /POST \/prompt/);
});

test("a real socket that accepts and never answers: the raw POST /prompt and fetchView both end at their bound", async () => {
  const sockets = new Set();
  const server = createServer(() => { /* accept the request, never answer */ });
  server.on("connection", (s) => { sockets.add(s); s.on("close", () => sockets.delete(s)); });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const api = `http://127.0.0.1:${server.address().port}`;
  try {
    const a = await settles(submitGraph({ api, graph: {}, clientId: "c", cli: null, submitTimeoutMs: 150 }), 5000);
    assert.ok(!a.hung && a.ok === false, "the raw POST /prompt did not end at its bound on a real socket");
    const b = await settles(fetchView({ api, file: { filename: "a.png" }, timeoutMs: 150 }), 5000);
    assert.ok(!b.hung && b.ok === false, "fetchView did not end at its bound on a real socket");
  } finally {
    for (const s of sockets) s.destroy();
    server.close();
  }
});

test("the healthy paths are unchanged: a prompt that answers is queued, an output that answers comes back byte for byte", async () => {
  const srv = await serve((req, res) => {
    if (req.url === "/prompt") { res.setHeader("Content-Type", "application/json"); res.end(JSON.stringify({ prompt_id: "pid-7" })); }
    else res.end(Buffer.from([1, 2, 3]));
  });
  try {
    const q = await submitGraph({ api: srv.api, graph: {}, clientId: "c", cli: null });
    assert.equal(q.promptId, "pid-7");
    assert.deepEqual(await fetchView({ api: srv.api, file: { filename: "a.png" } }), Buffer.from([1, 2, 3]));
  } finally { srv.close(); }
});

test("comfyUnusableClassLine: only a serverUnusable error names the class, on a line of its own", () => {
  assert.equal(typeof submit.comfyUnusableClassLine, "function", "comfy-submit.mjs must export comfyUnusableClassLine");
  const unusable = submit.serverUnusableError("ComfyUI stopped answering mid-render");
  assert.equal(submit.comfyUnusableClassLine(unusable), "IGPU_CLASS=comfy_unusable");
  assert.equal(submit.comfyUnusableClassLine(new Error("view fetch 404")), "");
  assert.equal(submit.comfyUnusableClassLine(null), "");
});
