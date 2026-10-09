// node --test render/igpu-deadline.test.mjs
// The runners' DEADLINE wiring (CT-49 G4/G20, pinned in round 2): --timeout-sec counts from the
// process start (so a slow pre-spawn phase spends it), every step that follows asks the deadline
// before it starts (deadline.enforce) and every external call is bounded by what is left. The
// REAL runners run against stub engines with a test-only preload (render/testdata/clock-preload.cjs)
// that busy-waits before the runner starts, moves the clock an hour ahead after an exact step, and
// records how ffmpeg / ffprobe were called.
import { test } from "node:test";
import assert from "node:assert";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import {
  here, skipReason, sandbox, runNode, videoSetup, animateSetup, audioSetup, records, GOOD_SD_HEADER,
} from "./igpu-testkit.mjs";
import { processStartMs, makeDeadline } from "./igpu-engine.mjs";

const opts = { skip: skipReason };
const preload = [join(here, "testdata", "clock-preload.cjs")];
const BUDGET = "600"; // seconds: large enough that only a clock jump can spend it
const lastLine = (stderr) => stderr.trim().split(/\r?\n/).pop();

// -------------------------------------------------------------- units

test("processStartMs: the moment this process started, not the moment it was asked", async () => {
  await new Promise((r) => setTimeout(r, 300));
  const start = processStartMs();
  assert.ok(Date.now() - start >= 300, "uptime is counted: a process that has run for 300 ms started at least 300 ms ago");
  assert.ok(Math.abs(Date.now() - start - process.uptime() * 1000) < 100);
  const d = makeDeadline(10);
  assert.ok(d.atMs <= start + 10000 + 50 && d.atMs >= start + 10000 - 50, "makeDeadline counts from the process start by default");
  assert.equal(makeDeadline(0).active, false);
  assert.equal(makeDeadline(0).remainingMs(), 0);
});

// -------------------------------------------------------------- a slow start spends the budget

test("sdcpp-video main: a start that already spent the --timeout-sec budget (the deadline counts from the process start) refuses the engine", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: GOOD_SD_HEADER });
    const r = await runNode("sdcpp-video.mjs", [...v.args.slice(0, v.args.indexOf("--")), "--timeout-sec", "2", ...v.args.slice(v.args.indexOf("--"))], sb, { preload, env: { IGPU_TEST_DELAY_MS: "3500" } });
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /sd-cli timeout: the 2s budget \(counted from the runner's start\) was spent/);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=timeout");
    assert.deepEqual(records(v.rec), [], "the engine never started");
    assert.ok(!existsSync(v.out));
  } finally { sb.done(); }
});

test("sdcpp-animate main: a start that already spent the budget refuses the first step", opts, async () => {
  const sb = sandbox();
  try {
    const a = animateSetup(sb, { extra: ["--timeout-sec", "2"] });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb, { preload, env: { IGPU_TEST_DELAY_MS: "3500" } });
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /driver frame extraction timeout: the 2s budget/);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=timeout");
    assert.deepEqual(records(a.dRec), []);
    assert.deepEqual(records(a.sdRec), []);
  } finally { sb.done(); }
});

test("audiocpp main: a start that already spent the budget refuses the engine", opts, async () => {
  const sb = sandbox();
  try {
    const a = audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"] }, ["--timeout-sec", "2"]);
    const r = await runNode("audiocpp-generate.mjs", a.args, sb, { preload, env: { IGPU_TEST_DELAY_MS: "3500" } });
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /audiocpp_cli timeout: the 2s budget/);
    assert.deepEqual(records(a.rec), []);
  } finally { sb.done(); }
});

test("a hanging engine is killed at what is LEFT of the budget after a slow start, not at the full budget", opts, async () => {
  for (const [script, mk] of [
    ["sdcpp-video.mjs", (sb) => videoSetup(sb, { log: GOOD_SD_HEADER, hang: true }, ["--timeout-sec", "8"])],
    ["audiocpp-generate.mjs", (sb) => audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], hang: true }, ["--timeout-sec", "8"])],
  ]) {
    const sb = sandbox();
    try {
      const v = mk(sb);
      const r = await runNode(script, v.args, sb, { preload, env: { IGPU_TEST_DELAY_MS: "3000" } });
      assert.equal(r.status, 1, r.stderr);
      // about 8 - 3 (the slow start) - node's own start = 4-5 s; counting from main() it would say 8
      assert.match(r.stderr, /timeout after [2-6]s \(killed\)/, `${script}: ${r.stderr.slice(-300)}`);
    } finally { sb.done(); }
  }
  const sb = sandbox();
  try {
    const a = animateSetup(sb, { depth: { hang: true }, extra: ["--timeout-sec", "12"] });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb, { preload, env: { IGPU_TEST_DELAY_MS: "3000" } });
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /depth-anything timeout after [3-9]s \(killed\)/, r.stderr.slice(-300));
  } finally { sb.done(); }
});

// -------------------------------------------------------------- every later step asks the deadline

// After the call that matches `jump`, the clock is an hour ahead: the budget is spent before the next
// step, which must refuse to start and say which step it was.
async function spentAfter(script, setup, jump, extra = {}) {
  const sb = sandbox();
  try {
    const s = setup(sb);
    const r = await runNode(script, [...s.args.slice(0, s.args.indexOf("--")), "--timeout-sec", BUDGET, ...s.args.slice(s.args.indexOf("--"))], sb, { preload, env: { IGPU_TEST_JUMP: jump } });
    return { r, s };
  } finally { sb.done(); }
}

test("sdcpp-video main: the budget spent after the engine refuses the mp4 encode; spent after the encode refuses the clip check", opts, async () => {
  const mk = (sb) => videoSetup(sb, { log: GOOD_SD_HEADER });
  let { r, s } = await spentAfter("sdcpp-video.mjs", mk, "spawn:sd-cli:1");
  assert.equal(r.status, 1, r.stderr);
  assert.match(r.stderr, /ffmpeg mp4 encode timeout: the 600s budget/);
  assert.equal(lastLine(r.stderr), "IGPU_CLASS=timeout");
  ({ r, s } = await spentAfter("sdcpp-video.mjs", mk, "spawnSync:libx264:1"));
  assert.equal(r.status, 1, r.stderr);
  assert.match(r.stderr, /clip check timeout: the 600s budget/);
  assert.ok(!existsSync(s.out), "an unchecked clip is not delivered");
});

test("sdcpp-animate main: the budget spent after each step refuses the next one, by name", opts, async () => {
  const mk = (sb) => animateSetup(sb);
  for (const [jump, step] of [
    ["spawnSync:force_original_aspect_ratio:1", "depth-anything"], // after the frame extraction
    ["spawn:da3-cli:5", "depth frame conversion"], // after the last of the 5 depth frames
    ["spawnSync:bicubic:1", "sd-cli"], // after the RGB conversion
    ["spawn:sd-cli:1", "frame count"], // after sd-cli
    ["spawnSync:nb_read_frames:1", "ffmpeg mp4 encode"], // after the ffprobe frame count
    ["spawnSync:libx264:1", "clip check"], // after the encode
  ]) {
    const { r, s } = await spentAfter("sdcpp-animate.mjs", mk, jump);
    assert.equal(r.status, 1, `${jump}: ${r.stderr.slice(-400)}`);
    assert.match(r.stderr, new RegExp(`${step} timeout: the 600s budget`), `${jump}: ${r.stderr.slice(-400)}`);
    assert.ok(!existsSync(s.out), `${jump}: nothing is delivered`);
  }
});

test("audiocpp main: the budget spent after the engine refuses the finalize steps", opts, async () => {
  const mk = (sb) => audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], writes: { kind: "wav", seconds: 3 } });
  const { r, s } = await spentAfter("audiocpp-generate.mjs", mk, "spawn:audiocpp_cli:1");
  assert.equal(r.status, 1, r.stderr);
  assert.match(r.stderr, /audio finalize timeout: the 600s budget/);
  assert.ok(!existsSync(s.out));
});

test("audiocpp main: the budget spent inside the finalize refuses the next ffmpeg / ffprobe step, by name, and delivers nothing", opts, async () => {
  const mk = (sb) => audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], writes: { kind: "wav", seconds: 3 } });
  for (const [jump, step] of [
    ["spawnSync:silenceremove:1", "ffprobe duration"], // after the trailing-silence trim
    ["spawnSync:loudnorm:1", "dead-air gate"], // after the master
  ]) {
    const { r, s } = await spentAfter("audiocpp-generate.mjs", mk, jump);
    assert.equal(r.status, 1, `${jump}: ${r.stderr.slice(-400)}`);
    assert.match(r.stderr, new RegExp(`${step} timeout: the 600s budget`), `${jump}: ${r.stderr.slice(-400)}`);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=timeout");
    assert.ok(!existsSync(s.out), `${jump}: an unchecked render is not delivered`);
  }
});

// -------------------------------------------------------------- every external call is bounded

test("the runners bound every ffmpeg / ffprobe call by what is left of the budget", opts, async () => {
  const bounded = (rows, needles) => {
    for (const needle of needles) {
      const row = rows.find((x) => x.line.includes(needle));
      assert.ok(row, `no call matching ${needle} was recorded: ${rows.map((x) => x.line.slice(0, 80)).join(" | ")}`);
      assert.ok(typeof row.timeout === "number" && row.timeout > 0 && row.timeout <= Number(BUDGET) * 1000, `${needle}: timeout option ${row.timeout}, want 0 < t <= ${BUDGET}s`);
    }
  };
  let sb = sandbox();
  try {
    const rec = join(sb.work, "calls.jsonl");
    const v = videoSetup(sb, { log: GOOD_SD_HEADER });
    const r = await runNode("sdcpp-video.mjs", [...v.args.slice(0, v.args.indexOf("--")), "--timeout-sec", BUDGET, ...v.args.slice(v.args.indexOf("--"))], sb, { preload, env: { IGPU_TEST_RECORD: rec } });
    assert.equal(r.status, 0, r.stderr);
    bounded(records(rec), ["libx264", "blackdetect"]); // the encode, the clip check
  } finally { sb.done(); }
  sb = sandbox();
  try {
    const rec = join(sb.work, "calls.jsonl");
    const a = animateSetup(sb, { extra: ["--timeout-sec", BUDGET] });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb, { preload, env: { IGPU_TEST_RECORD: rec } });
    assert.equal(r.status, 0, r.stderr);
    // the driver frame extraction, the RGB conversion, the ffprobe frame count, the encode, the clip check
    bounded(records(rec), ["force_original_aspect_ratio", "bicubic", "nb_read_frames", "libx264", "blackdetect"]);
  } finally { sb.done(); }
  // the audio runner: the trim, both duration probes, the master and the dead-air measurement (music), and the
  // re-encode of a non-wav delivery (voice to flac)
  for (const [kind, flac, needles] of [
    ["music", false, ["silenceremove", "format=duration", "loudnorm", "silencedetect"]],
    ["voice", true, ["48000", "silencedetect"]],
  ]) {
    sb = sandbox();
    try {
      const rec = join(sb.work, "calls.jsonl");
      const a = audioSetup(sb, kind, { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], writes: { kind: "wav", seconds: 3 } }, ["--timeout-sec", BUDGET]);
      const args = flac ? a.args.map((x) => (x === a.out ? x.replace(/\.wav$/, ".flac") : x)) : a.args;
      const r = await runNode("audiocpp-generate.mjs", args, sb, { preload, env: { IGPU_TEST_RECORD: rec } });
      assert.equal(r.status, 0, r.stderr);
      const rows = records(rec).filter((x) => !x.line.includes("-version")); // resolving the binaries is not a media call
      bounded(rows, needles);
      for (const row of rows) assert.ok(typeof row.timeout === "number" && row.timeout > 0, `${kind}: unbounded call ${row.line.slice(0, 100)}`);
    } finally { sb.done(); }
  }
  // and without --timeout-sec nothing is bounded (no deadline = unbounded, as before)
  sb = sandbox();
  try {
    const rec = join(sb.work, "calls.jsonl");
    const v = videoSetup(sb, { log: GOOD_SD_HEADER });
    const r = await runNode("sdcpp-video.mjs", v.args, sb, { preload, env: { IGPU_TEST_RECORD: rec } });
    assert.equal(r.status, 0, r.stderr);
    for (const row of records(rec)) assert.equal(row.timeout, null, row.line.slice(0, 80));
    assert.ok(readFileSync(rec, "utf8").length > 0);
  } finally { sb.done(); }
  sb = sandbox();
  try {
    const rec = join(sb.work, "calls.jsonl");
    const a = audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], writes: { kind: "wav", seconds: 3 } });
    const r = await runNode("audiocpp-generate.mjs", a.args, sb, { preload, env: { IGPU_TEST_RECORD: rec } });
    assert.equal(r.status, 0, r.stderr);
    const rows = records(rec).filter((x) => x.line.includes("silenceremove") || x.line.includes("format=duration") || x.line.includes("loudnorm") || x.line.includes("silencedetect"));
    assert.ok(rows.length >= 5, "the audio finalize calls were recorded");
    for (const row of rows) assert.equal(row.timeout, null, row.line.slice(0, 80));
  } finally { sb.done(); }
});
