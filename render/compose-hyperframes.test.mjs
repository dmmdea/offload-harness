// node --test render/compose-hyperframes.test.mjs
// compose-hyperframes.mjs against a STUB HyperFrames CLI: a fake node_modules/hyperframes/bin/
// hyperframes.mjs that records its argv, env and cwd, then answers from a behaviour file. Nothing
// here needs HyperFrames, Chrome, ffmpeg or a network — CI runs it on a bare Node.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { createHash } from "node:crypto";
import vm from "node:vm";
import { DEFAULTS as CAPTION_DEFAULTS, PACES as CAPTION_PACES, TEMPLATE_LIMITS as CAPTION_LIMITS, captionsFromSegments } from "./captions-groups.mjs";
import { copyAtomic } from "./atomic-out.mjs";
import { fileURLToPath } from "node:url";
import {
  ALLOWED_BROWSER_SUBCOMMANDS, ALLOWED_SUBCOMMANDS, FORCED_ENV, PASSTHROUGH_ENV_KEYS, PINNED_VERSION, applyTemplateVariables, assertAllowedInvocation,
  buildCheckArgs, buildChildEnv, buildRenderArgs, buildSnapshotArgs, classifyFailure, compositionMeta,
  listTemplates, materializeTemplate, moveInto, parseJsonDoc, rewriteRootDuration, summarizeProbe, verifyOutput,
} from "./compose-hyperframes.mjs";

const __dirname = dirname(fileURLToPath(import.meta.url));
const RUNNER = join(__dirname, "compose-hyperframes.mjs");

// The keys a leaked environment would carry. None may ever reach the HyperFrames child.
const SECRETS = {
  OPENROUTER_API_KEY: "sk-or-test", GEMINI_API_KEY: "g-test", GOOGLE_API_KEY: "goog-test",
  ELEVENLABS_API_KEY: "el-test", HEYGEN_API_KEY: "hg-test", HYPERFRAMES_API_KEY: "hf-test",
  OPENAI_API_KEY: "oa-test", GROQ_API_KEY: "gq-test", FIGMA_TOKEN: "fg-test", NVIDIA_API_KEY: "nv-test",
};
const ALSO_DROPPED = { NODE_OPTIONS: "--no-deprecation", GPU_LEASE_DIR: "/tmp/lease", GPU_LEASE_EPOCH: "7", GPU_LEASE_CLASS: "media", HTTPS_PROXY: "http://proxy:1", CI: "true" };

const STUB_CLI = String.raw`
import { appendFileSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const b = JSON.parse(readFileSync(join(here, "behavior.json"), "utf8"));
appendFileSync(join(here, "calls.jsonl"), JSON.stringify({ argv, execArgv: process.execArgv, env: process.env, cwd: process.cwd(), cwdEntries: readdirSync(process.cwd()) }) + "\n");
const counter = (name) => { const f = join(here, name + ".count"); const n = existsSync(f) ? Number(readFileSync(f, "utf8")) + 1 : 1; writeFileSync(f, String(n)); return n; };
const sleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const flag = (n) => { const i = argv.indexOf(n); return i >= 0 ? argv[i + 1] : undefined; };
const sub = argv[0];
if (sub === "--version") { console.log(b.version); process.exit(0); }
if (sub === "browser") {
  if (argv[1] === "ensure") { console.log("Path: " + b.browserPath); process.exit(0); }
  console.log(b.browserPath); process.exit(0);
}
if (sub === "lint") {
  const l = b.lint || {};
  console.log(JSON.stringify({ ok: !l.errorCount, errorCount: l.errorCount || 0, warningCount: l.warningCount || 0, infoCount: 0, findings: l.findings || [], filesScanned: 1, _meta: { version: b.version } }, null, 2));
  process.exit(l.errorCount ? 1 : 0);
}
if (sub === "check") {
  const c = b.check || {};
  if (c.sleepMs) sleep(c.sleepMs);
  const rep = c.report || { ok: (c.code || 0) === 0, strict: false, lint: { ok: true, errorCount: 0, warningCount: 0, infoCount: 0, findings: [] }, runtime: { ok: true, findings: [] }, layout: { ok: true, findings: [] }, motion: { ok: true, findings: [] }, contrast: { ok: true, findings: [] } };
  console.log(JSON.stringify(rep, null, 2));
  process.exit(c.code || 0);
}
if (sub === "render") {
  const r = b.render || {};
  if (r.ebusyTimes && counter("render") <= r.ebusyTimes) { console.error("Error: spawn EBUSY"); process.exit(1); }
  if (r.sleepMs) sleep(r.sleepMs);
  const out = flag("-o");
  const rows = JSON.parse(readFileSync(flag("--batch"), "utf8"));
  writeFileSync(join(here, "rows.seen.json"), JSON.stringify(rows));
  const status = r.status || "completed";
  if (status === "completed") {
    if (flag("--format") === "png-sequence") { mkdirSync(out, { recursive: true }); for (let i = 0; i < 3; i++) writeFileSync(join(out, "frame_" + String(i).padStart(6, "0") + ".png"), "png"); }
    else { mkdirSync(dirname(out), { recursive: true }); writeFileSync(out, "fake-video-bytes"); }
  }
  console.log(JSON.stringify({ type: "batch-complete", manifestPath: join(dirname(out), "manifest.json"), total: 1, completed: status === "completed" ? 1 : 0, failed: status === "completed" ? 0 : 1, skipped: 0,
    rows: [{ index: 0, outputPath: out, status, durationMs: r.durationMs === undefined ? 5000 : r.durationMs, renderTimeMs: 1234, error: r.error || null, variables: rows[0] }] }));
  process.exit(status === "completed" ? 0 : 1);
}
if (sub === "snapshot") {
  const dir = flag("-o"); mkdirSync(dir, { recursive: true });
  for (const t of flag("--at").split(",")) writeFileSync(join(dir, "frame-" + t + "-at-" + t + "s.png"), "png");
  writeFileSync(join(dir, "contact-sheet.jpg"), "jpg");
  process.exit(0);
}
console.error("stub: unexpected subcommand " + sub); process.exit(9);
`;

const STUB_FFPROBE = String.raw`
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
const here = dirname(fileURLToPath(import.meta.url));
process.stdout.write(readFileSync(join(here, "probe.json"), "utf8"));
`;

function probeDoc({ codec = "h264", pix = "yuv420p", w = 1920, h = 1080, fps = "30/1", dur = "5.000000", packets = "150", tags = {}, audio = false } = {}) {
  const streams = [{ index: 0, codec_type: "video", codec_name: codec, pix_fmt: pix, width: w, height: h, r_frame_rate: fps, avg_frame_rate: fps, nb_read_packets: packets, tags }];
  if (audio) streams.push({ index: 1, codec_type: "audio", codec_name: "aac" });
  return JSON.stringify({ streams, format: { duration: dur } });
}

const TEMPLATE_HTML = `<!doctype html><html><body>
<div id="root" data-composition-id="t" data-start="0" data-duration="6" data-width="1920" data-height="1080" data-fps="30" data-no-timeline
 data-composition-variables='[{"id":"title","type":"string","default":"Hello"},{"id":"accent","type":"color","default":"#ff0000"},{"id":"duration","type":"number","default":6,"min":1,"max":60}]'>
<h1 class="clip" data-start="0" data-duration="6" data-var-text="title">Hello</h1></div></body></html>`;

// fixture builds a throwaway hyperframes dir + stubs + templates, returns paths and a runner.
function fixture(behavior = {}, probe = probeDoc()) {
  const root = mkdtempSync(join(tmpdir(), "compose-test-"));
  const hf = join(root, "hf");
  const bin = join(hf, "node_modules", "hyperframes", "bin");
  mkdirSync(bin, { recursive: true });
  writeFileSync(join(bin, "hyperframes.mjs"), STUB_CLI);
  writeFileSync(join(hf, "node_modules", "hyperframes", "package.json"), JSON.stringify({ name: "hyperframes", version: behavior.installed || PINNED_VERSION }));
  const browserPath = join(root, "chrome-headless-shell.exe");
  writeFileSync(browserPath, "chrome");
  writeFileSync(join(bin, "behavior.json"), JSON.stringify({ browserPath, version: PINNED_VERSION, ...behavior }));
  const ffmpeg = join(root, "ffmpeg.exe");
  writeFileSync(ffmpeg, "ffmpeg");
  const ffprobe = join(root, "ffprobe-stub.mjs");
  writeFileSync(ffprobe, STUB_FFPROBE);
  writeFileSync(join(root, "probe.json"), probe);
  const templates = join(root, "templates");
  mkdirSync(join(templates, "demo"), { recursive: true });
  writeFileSync(join(templates, "demo", "index.html"), TEMPLATE_HTML);
  writeFileSync(join(templates, "demo", "template.json"), JSON.stringify({ duration_variable: "duration" }));
  writeFileSync(join(templates, "demo", "README.md"), "readme");
  mkdirSync(join(templates, "_shared", "fonts"), { recursive: true });
  writeFileSync(join(templates, "_shared", "fonts", "x.woff2"), "font");
  const cache = join(root, "cache");
  const base = ["--hyperframes-dir", hf, "--ffmpeg", ffmpeg, "--ffprobe", ffprobe, "--browser", browserPath,
    "--cache-dir", cache, "--templates-dir", templates];
  const run = (args, extraEnv = {}) => {
    const r = spawnSync(process.execPath, [RUNNER, ...args], {
      encoding: "utf8", env: { ...process.env, ...SECRETS, ...ALSO_DROPPED, ...extraEnv }, timeout: 60000,
    });
    const lines = (r.stdout || "").trim().split(/\r?\n/);
    let last = null;
    try { last = JSON.parse(lines[lines.length - 1]); } catch { /* asserted by callers */ }
    return { ...r, lines, last };
  };
  const calls = () => {
    const f = join(bin, "calls.jsonl");
    return existsSync(f) ? readFileSync(f, "utf8").trim().split(/\r?\n/).map((l) => JSON.parse(l)) : [];
  };
  return { root, hf, bin, cache, base, run, calls, browserPath, ffmpeg, ffprobe };
}

function renderArgs(fx, extra = []) {
  return ["render", ...fx.base, "--template", "demo", "--out", join(fx.root, "final", "clip.mp4"), "--result", join(fx.root, "result.json"), ...extra];
}

// --- the environment ----------------------------------------------------------------------------

test("buildChildEnv: only the allowlist passes; every cloud key, NODE_OPTIONS and GPU_LEASE_* is dropped", () => {
  const parent = { ...SECRETS, ...ALSO_DROPPED, PATH: "/usr/bin", HOME: "/srv/h", TEMP: "/t", XDG_CACHE_HOME: "/c", RANDOM_THING: "x" };
  const env = buildChildEnv(parent, { ffmpeg: "/f/ffmpeg", ffprobe: "/f/ffprobe", browser: "/b/chs", extractCacheDir: "/x" }, "linux");
  for (const k of Object.keys(SECRETS)) assert.equal(env[k], undefined, `${k} leaked`);
  for (const k of Object.keys(ALSO_DROPPED)) assert.equal(env[k], undefined, `${k} leaked`);
  assert.equal(env.RANDOM_THING, undefined);
  assert.equal(env.PATH, "/usr/bin");
  assert.equal(env.XDG_CACHE_HOME, "/c");
  for (const [k, v] of Object.entries(FORCED_ENV)) assert.equal(env[k], v, `${k} must be forced to ${v}`);
  assert.equal(env.HYPERFRAMES_NO_UPDATE_CHECK, "1", 'the update gates compare against exactly "1"');
  assert.equal(env.HYPERFRAMES_FFMPEG_PATH, "/f/ffmpeg");
  assert.equal(env.HYPERFRAMES_FFPROBE_PATH, "/f/ffprobe");
  assert.equal(env.HYPERFRAMES_BROWSER_PATH, "/b/chs");
  assert.equal(env.HYPERFRAMES_EXTRACT_CACHE_DIR, "/x");
  const allowed = new Set([...PASSTHROUGH_ENV_KEYS, ...Object.keys(FORCED_ENV), "HYPERFRAMES_FFMPEG_PATH", "HYPERFRAMES_FFPROBE_PATH", "HYPERFRAMES_BROWSER_PATH", "HYPERFRAMES_EXTRACT_CACHE_DIR"]);
  for (const k of Object.keys(env)) assert.ok(allowed.has(k), `unexpected key ${k} in the child env`);
});

test("buildChildEnv: Windows folds case once and redirects HOME/USERPROFILE/HOMEDRIVE/HOMEPATH to the harness home", () => {
  const parent = { Path: "C:\\Windows", SystemRoot: "C:\\Windows", windir: "C:\\Windows", USERPROFILE: "C:\\Users\\user", HOMEDRIVE: "C:", HOMEPATH: "\\Users\\user", OpenRouter_Api_Key: "leak" };
  const env = buildChildEnv(parent, { home: "D:\\stack\\hyperframes\\home", tempDir: "D:\\cache\\tmp" }, "win32");
  assert.equal(env.PATH, "C:\\Windows");
  assert.equal(env.SYSTEMROOT, "C:\\Windows");
  assert.equal(env.WINDIR, "C:\\Windows");
  assert.equal(env.Path, undefined, "one spelling per name");
  assert.equal(env.OpenRouter_Api_Key, undefined);
  assert.equal(env.OPENROUTER_API_KEY, undefined);
  assert.equal(env.USERPROFILE, "D:\\stack\\hyperframes\\home");
  assert.equal(env.HOME, "D:\\stack\\hyperframes\\home");
  assert.equal(env.HOMEDRIVE, "D:");
  assert.equal(env.HOMEPATH, "\\stack\\hyperframes\\home");
  assert.equal(env.TEMP, "D:\\cache\\tmp");
  assert.equal(env.TMP, "D:\\cache\\tmp");
});

// --- the allowlist ------------------------------------------------------------------------------

test("assertAllowedInvocation: only lint/check/render/snapshot/browser ensure|path/--version, always with --json", () => {
  assert.deepEqual([...ALLOWED_SUBCOMMANDS], ["lint", "check", "render", "snapshot", "browser", "--version"]);
  for (const bad of [["init", "--json"], ["skills", "update", "--json"], ["cloud", "render", "--json"], ["lambda", "render", "--json"],
    ["cloudrun", "--json"], ["capture", "https://x", "--json"], ["upgrade", "--json"], ["publish", "--json"], ["browser", "clear", "--json"], []]) {
    assert.throws(() => assertAllowedInvocation(bad), (e) => e.cls === "BAD_INPUT", `${bad.join(" ")} must be refused`);
  }
  assert.throws(() => assertAllowedInvocation(["lint", "/p"]), /without --json/);
  assert.throws(() => assertAllowedInvocation(["render", "/p", "--json", "--browser-gpu"]), /CPU-class/);
  assert.throws(() => assertAllowedInvocation(["render", "/p", "--json", "--gpu"]), /CPU-class/);
  assert.throws(() => assertAllowedInvocation(["snapshot", "/p", "--json", "--at", "1"]), /describe/);
  assert.throws(() => assertAllowedInvocation(["snapshot", "/p", "--json", "--describe", "true"]), /describe/);
  assert.ok(assertAllowedInvocation(buildSnapshotArgs({ projectDir: "/p", at: [1, 2.5], outDir: "/o" })));
  assert.ok(assertAllowedInvocation(buildCheckArgs("/p")));
  assert.ok(assertAllowedInvocation(["--version", "--json"]));
  assert.ok(assertAllowedInvocation(["browser", "ensure", "--json"]));
});

test("buildRenderArgs: batch + json + every quality/determinism flag, verified against render.ts", () => {
  const a = buildRenderArgs({ projectDir: "/p", rowsFile: "/w/rows.json", output: "/w/out/output.webm", format: "webm", quality: "high", workers: "auto", strict: true, fps: 30, resolution: "landscape", composition: "c.html" });
  const has = (k, v) => { const i = a.indexOf(k); assert.ok(i >= 0, `missing ${k}`); if (v !== undefined) assert.equal(a[i + 1], v, k); };
  assert.equal(a[0], "render");
  assert.equal(a[1], "/p");
  has("--batch", "/w/rows.json"); has("--json"); has("-o", "/w/out/output.webm"); has("--format", "webm"); has("--quality", "high");
  has("--workers", "auto"); has("--no-browser-gpu"); has("--no-best-effort"); has("--strict"); has("--strict-variables");
  has("--fps", "30"); has("--resolution", "landscape"); has("--composition", "c.html");
  const loose = buildRenderArgs({ projectDir: "/p", rowsFile: "/r", output: "/o", format: "mp4", quality: "draft", workers: "2", strict: false });
  assert.ok(!loose.includes("--strict") && !loose.includes("--fps") && !loose.includes("--resolution"));
});

// --- end to end through the stub CLI ------------------------------------------------------------

test("render: lint -> check -> render -> ffprobe, every call --json, scrubbed env, fresh empty cwd, result line + file", () => {
  const fx = fixture();
  const r = fx.run(renderArgs(fx, ["--variables-file", (() => { const p = join(fx.root, "vars.json"); writeFileSync(p, JSON.stringify({ title: "Q3 results", duration: 4 })); return p; })(), "--snapshots", "1,2.5"]));
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.equal(r.last.ok, true);
  const calls = fx.calls();
  assert.deepEqual(calls.map((c) => c.argv[0]), ["lint", "check", "render", "snapshot"]);
  for (const c of calls) {
    assert.ok(c.argv.includes("--json"), `${c.argv[0]} ran without --json`);
    for (const k of [...Object.keys(SECRETS), ...Object.keys(ALSO_DROPPED)]) assert.equal(c.env[k], undefined, `${k} reached ${c.argv[0]}`);
    for (const [k, v] of Object.entries(FORCED_ENV)) assert.equal(c.env[k], v);
    assert.equal(c.env.HYPERFRAMES_BROWSER_PATH, fx.browserPath);
    assert.ok(c.env.HYPERFRAMES_FFMPEG_PATH.endsWith("ffmpeg.exe"));
    assert.ok(c.env.HYPERFRAMES_EXTRACT_CACHE_DIR.startsWith(fx.cache));
    assert.ok(c.cwd.startsWith(join(fx.cache, "work")), `cwd ${c.cwd} is not a harness work dir`);
    assert.ok(!c.cwdEntries.includes(".env"), "cwd must hold no .env");
  }
  assert.ok(calls[0].cwdEntries.every((n) => n === "project"), `lint cwd must be fresh (only the materialized project): ${calls[0].cwdEntries}`);
  const render = calls[2].argv;
  for (const flag of ["--batch", "--no-browser-gpu", "--strict", "--no-best-effort", "--strict-variables"]) assert.ok(render.includes(flag), `render lacks ${flag}`);
  assert.equal(render[render.indexOf("--quality") + 1], "high", "quality first: the default is high");
  assert.deepEqual(JSON.parse(readFileSync(join(fx.bin, "rows.seen.json"), "utf8")), [{ title: "Q3 results", duration: 4 }]);
  const snap = calls[3].argv;
  assert.equal(snap[snap.indexOf("--describe") + 1], "false");
  // the materialized copy carries the caller's values as declared defaults + the rewritten root duration
  const out = join(fx.root, "final", "clip.mp4");
  assert.ok(existsSync(out));
  assert.equal(readFileSync(out, "utf8"), "fake-video-bytes");
  const res = JSON.parse(readFileSync(join(fx.root, "result.json"), "utf8"));
  assert.deepEqual(res, r.last);
  assert.equal(res.video_path, out);
  assert.equal(res.codec, "h264");
  assert.equal(res.width, 1920);
  assert.equal(res.render_ms, 1234);
  assert.deepEqual(res.lint, { errors: 0, warnings: 0 });
  assert.equal(res.check.ok, true);
  assert.equal(res.snapshots.length, 2);
  for (const s of res.snapshots) assert.ok(existsSync(s), `snapshot ${s} not published`);
  assert.equal(readdirSync(join(fx.cache, "work")).length, 0, "the work dir is removed after the run");
});

test("render: the vetted-template copy gets the caller's values and duration; the template dir is never written", () => {
  const fx = fixture();
  const vars = join(fx.root, "vars.json");
  writeFileSync(vars, JSON.stringify({ title: "It's <b>bold</b>", duration: 4 }));
  const r = fx.run(renderArgs(fx, ["--variables-file", vars, "--keep-work"]));
  assert.equal(r.status, 0, r.stdout + r.stderr);
  const job = readdirSync(join(fx.cache, "work"))[0];
  const html = readFileSync(join(fx.cache, "work", job, "project", "index.html"), "utf8");
  assert.match(html, /data-duration="4"/);
  assert.ok(!html.includes("<b>bold</b>"), "a variable value must be attribute-escaped, never live markup");
  assert.ok(existsSync(join(fx.cache, "work", job, "project", "shared", "fonts", "x.woff2")), "the shared font kit rides along");
  assert.ok(!existsSync(join(fx.cache, "work", job, "project", "README.md")), "template docs are not part of the composition");
  assert.equal(readFileSync(join(fx.root, "templates", "demo", "index.html"), "utf8"), TEMPLATE_HTML);
});

test("render: lint errors fail typed LINT_ERRORS before any browser runs", () => {
  const fx = fixture({ lint: { errorCount: 2, findings: [{ severity: "error", code: "missing_duration", message: "no duration" }] } });
  const r = fx.run(renderArgs(fx));
  assert.equal(r.status, 1);
  assert.ok(r.lines.some((l) => l.startsWith("COMPOSE-FAIL: LINT_ERRORS: 2 lint error(s)")), r.stdout);
  assert.equal(r.last.class, "LINT_ERRORS");
  assert.deepEqual(fx.calls().map((c) => c.argv[0]), ["lint"]);
  assert.equal(JSON.parse(readFileSync(join(fx.root, "result.json"), "utf8")).class, "LINT_ERRORS");
});

test("render: check exit != 0 fails typed CHECK_FAILED", () => {
  const fx = fixture({ check: { code: 1, report: { ok: false, runtime: { findings: [{ severity: "error", code: "console_error", message: "boom" }] } } } });
  const r = fx.run(renderArgs(fx));
  assert.equal(r.status, 1);
  assert.equal(r.last.class, "CHECK_FAILED");
  assert.match(r.last.detail, /runtime\/console_error: boom/);
  assert.deepEqual(fx.calls().map((c) => c.argv[0]), ["lint", "check"]);
});

test("render: strict=false keeps going past lint errors and a failed check", () => {
  const fx = fixture({ lint: { errorCount: 1 }, check: { code: 1 } });
  const r = fx.run(renderArgs(fx, ["--no-strict"]));
  assert.equal(r.status, 0, r.stdout);
  assert.deepEqual(r.last.lint, { errors: 1, warnings: 0 });
  assert.equal(r.last.check.ok, false);
  assert.ok(!fx.calls()[2].argv.includes("--strict"));
});

test("render: a failed row fails RENDER_FAILED; the headroom gate fails DISK_HEADROOM", () => {
  let fx = fixture({ render: { status: "failed", error: "Page crashed" } });
  let r = fx.run(renderArgs(fx));
  assert.equal(r.last.class, "RENDER_FAILED");
  fx = fixture({ render: { status: "failed", error: "Disk capture may need ~16000.0 MB of temporary frame storage, but only 12.0 MB is free" } });
  r = fx.run(renderArgs(fx));
  assert.equal(r.last.class, "DISK_HEADROOM");
});

test("render: spawn EBUSY is retried ONCE (#4058); twice fails SPAWN_EBUSY", () => {
  let fx = fixture({ render: { ebusyTimes: 1 } });
  let r = fx.run(renderArgs(fx));
  assert.equal(r.status, 0, r.stdout);
  assert.equal(fx.calls().filter((c) => c.argv[0] === "render").length, 2);
  fx = fixture({ render: { ebusyTimes: 2 } });
  r = fx.run(renderArgs(fx));
  assert.equal(r.last.class, "SPAWN_EBUSY");
  assert.equal(fx.calls().filter((c) => c.argv[0] === "render").length, 2, "exactly one retry");
});

test("render: the deadline kills the step and fails TIMEOUT", () => {
  const fx = fixture({ check: { sleepMs: 8000 } });
  const t0 = Date.now();
  const r = fx.run(renderArgs(fx, ["--timeout-sec", "2"]));
  assert.equal(r.last.class, "TIMEOUT", r.stdout);
  assert.ok(Date.now() - t0 < 7000, "the child must be killed at the deadline, not waited out");
});

test("render: missing browser / ffmpeg / CLI are typed before anything spawns", () => {
  let fx = fixture();
  let r = fx.run(renderArgs(fx, ["--browser", join(fx.root, "nope.exe")]));
  assert.equal(r.last.class, "BROWSER_MISSING");
  fx = fixture();
  r = fx.run(renderArgs(fx, ["--ffmpeg", join(fx.root, "no-ffmpeg.exe")]));
  assert.equal(r.last.class, "FFMPEG_MISSING");
  fx = fixture();
  r = fx.run(renderArgs(fx, ["--hyperframes-dir", join(fx.root, "empty")]));
  assert.equal(r.last.class, "CLI_MISSING");
  assert.equal(fx.calls().length, 0);
});

test("render: bad input is refused BAD_INPUT", () => {
  const fx = fixture();
  const cases = [
    ["--html-file", join(fx.root, "x.html")], // template + html
    ["--format", "hls"], ["--quality", "looks"], ["--fps", "0.5"], ["--workers", "99"], ["--resolution", "8k"],
    ["--snapshots", "1,-2"], ["--composition", "../escape.html"],
  ];
  for (const extra of cases) {
    const r = fx.run(renderArgs(fx, extra));
    assert.equal(r.last && r.last.class, "BAD_INPUT", `${extra.join(" ")}: ${r.stdout}`);
  }
  for (const tpl of ["../demo", "nope", "Demo"]) {
    const r = fx.run(["render", ...fx.base, "--template", tpl, "--out", join(fx.root, "o.mp4")]);
    assert.equal(r.last.class, "BAD_INPUT", tpl);
  }
  const vars = join(fx.root, "vars.json");
  writeFileSync(vars, JSON.stringify({ undeclared: "x" }));
  const r = fx.run(renderArgs(fx, ["--variables-file", vars]));
  assert.equal(r.last.class, "BAD_INPUT");
  assert.match(r.last.detail, /not declared/);
  assert.equal(fx.calls().length, 0, "bad input never reaches the CLI");
});

test("render: the ffprobe gate refuses an alpha format without alpha, and a size mismatch", () => {
  let fx = fixture({}, probeDoc({ codec: "vp9", pix: "yuv420p" }));
  let r = fx.run(renderArgs(fx, ["--format", "webm", "--out", join(fx.root, "o.webm")]));
  assert.equal(r.last.class, "RENDER_FAILED");
  assert.match(r.last.detail, /no alpha/);
  fx = fixture({}, probeDoc({ codec: "vp9", pix: "yuv420p", tags: { ALPHA_MODE: "1" } }));
  r = fx.run(renderArgs(fx, ["--format", "webm", "--out", join(fx.root, "o.webm")]));
  assert.equal(r.status, 0, r.stdout);
  assert.equal(r.last.has_alpha, true);
  fx = fixture({}, probeDoc({ w: 1280, h: 720 }));
  r = fx.run(renderArgs(fx));
  assert.match(r.last.detail, /width 1280 \(want 1920\)/);
});

test("render: png-sequence publishes a frames_dir and never overwrites a non-empty directory", () => {
  const fx = fixture({}, probeDoc({ codec: "png", pix: "rgba", packets: "1" }));
  const outDir = join(fx.root, "frames-out");
  const r = fx.run(renderArgs(fx, ["--format", "png-sequence", "--out", outDir, "--fps", "30"]));
  assert.equal(r.status, 0, r.stdout);
  assert.equal(r.last.frames_dir, outDir);
  assert.equal(r.last.frames, 3);
  const again = fx.run(renderArgs(fx, ["--format", "png-sequence", "--out", outDir]));
  assert.equal(again.last.class, "BAD_INPUT");
});

test("version and browser ops run through the same allowlist and env", () => {
  const fx = fixture();
  let r = fx.run(["version", "--hyperframes-dir", fx.hf]);
  assert.equal(r.status, 0, r.stdout);
  assert.equal(r.last.version, PINNED_VERSION);
  assert.equal(r.last.matches_pin, true);
  r = fx.run(["browser", "--hyperframes-dir", fx.hf]);
  assert.equal(r.status, 0, r.stdout);
  assert.equal(r.last.browser_path, fx.browserPath);
  const calls = fx.calls();
  assert.deepEqual(calls.map((c) => c.argv.slice(0, 2).join(" ")), ["--version --json", "browser ensure", "browser path"]);
  // --dns-result-order=ipv4first (D-defect: <node-f> wave session 5d227d30 §5a — a
  // dead IPv6 route to storage.googleapis.com hung `browser ensure` with no
  // failover) reaches the node PROCESS that runs "browser ensure" and nothing
  // else: not --version, not "browser path", not lint/check/render/snapshot.
  assert.deepEqual(calls[1].execArgv, ["--dns-result-order=ipv4first"], "browser ensure must carry the node flag");
  assert.deepEqual(calls[0].execArgv, [], "--version must not carry it");
  assert.deepEqual(calls[2].execArgv, [], "browser path must not carry it (no network access)");
  for (const c of calls) {
    assert.ok(c.argv.includes("--json"));
    assert.equal(c.env.OPENROUTER_API_KEY, undefined);
    assert.equal(c.env.HOME, join(fx.hf, "home"), "HyperFrames state lands in the harness-owned home");
  }
});

test("a drifted install (not the runner's pin) is refused CLI_MISSING before anything spawns", () => {
  const fx = fixture({ installed: "0.9.0" });
  let r = fx.run(renderArgs(fx));
  assert.equal(r.last.class, "CLI_MISSING", r.stdout);
  assert.match(r.last.detail, /holds hyperframes 0\.9\.0, not the runner's pin/);
  r = fx.run(["version", "--hyperframes-dir", fx.hf]);
  assert.equal(r.status, 1, "acceptance runs the version op: a drifted install must fail it");
  assert.equal(r.last.class, "CLI_MISSING");
  assert.equal(fx.calls().length, 0);
  // the CLI's own answer is checked too, not only package.json
  const fy = fixture({ version: "0.8.60" });
  r = fy.run(["version", "--hyperframes-dir", fy.hf]);
  assert.equal(r.last.class, "CLI_MISSING", r.stdout);
});

test("the runner's pin IS the committed lockfile's pin (one version, three places)", () => {
  const setup = join(__dirname, "..", "setup", "hyperframes");
  const pkg = JSON.parse(readFileSync(join(setup, "package.json"), "utf8"));
  const lock = JSON.parse(readFileSync(join(setup, "package-lock.json"), "utf8"));
  assert.equal(pkg.dependencies.hyperframes, PINNED_VERSION, "package.json pins an exact version equal to PINNED_VERSION");
  assert.equal(lock.packages["node_modules/hyperframes"].version, PINNED_VERSION);
  assert.equal(lock.packages[""].dependencies.hyperframes, PINNED_VERSION);
  assert.equal(lock.packages["node_modules/hyperframes"].integrity,
    "sha512-dIHdDQ//Wapovreuc0q2B+xLS5gbDUSles3ysXafkvxC4YXzZjc3NPvMqweEi5IZQDXefxZOJws5AgDByOiRAQ==",
    "the lock carries the registry integrity verified for 0.8.114 — a bump must re-verify it");
  assert.equal(pkg.private, true);
  assert.deepEqual(Object.keys(pkg.dependencies), ["hyperframes"], "exactly one dependency");
  // npm >= 11 blocks dependency install scripts unless package.json approves them, which turns the
  // installers' deliberate `npm rebuild esbuild` into a silent no-op. The ONE approval is pinned to
  // the lock's esbuild, so a bump that moves esbuild must re-approve it by name and version.
  assert.deepEqual(pkg.allowScripts, { [`esbuild@${lock.packages["node_modules/esbuild"].version}`]: true });
});

// --- pure helpers -------------------------------------------------------------------------------

test("parseJsonDoc: pretty documents, a trailing single-line document, and noise", () => {
  assert.deepEqual(parseJsonDoc('{\n  "a": 1\n}\n'), { a: 1 });
  assert.deepEqual(parseJsonDoc('log line\n{"type":"batch-complete","rows":[]}'), { type: "batch-complete", rows: [] });
  assert.deepEqual(parseJsonDoc('warn: x\n{\n  "errorCount": 0\n}'), { errorCount: 0 });
  assert.equal(parseJsonDoc("no json here"), null);
});

test("classifyFailure: typed classes from real failure text", () => {
  assert.equal(classifyFailure("render", "Error: spawn EBUSY"), "SPAWN_EBUSY");
  assert.equal(classifyFailure("render", "Disk capture may need ~9 MB of temporary frame storage"), "DISK_HEADROOM");
  assert.equal(classifyFailure("render", "ffmpeg not found at C:/x/ffmpeg.exe"), "FFMPEG_MISSING");
  assert.equal(classifyFailure("render", "Failed to launch the browser process!"), "BROWSER_MISSING");
  assert.equal(classifyFailure("render", "Variable validation failed"), "BAD_INPUT");
  assert.equal(classifyFailure("lint", "something"), "LINT_ERRORS");
  assert.equal(classifyFailure("check", "something"), "CHECK_FAILED");
  assert.equal(classifyFailure("render", "something"), "RENDER_FAILED");
});

test("applyTemplateVariables: typed values only, declared ids only, duration rewrites the root", () => {
  const out = applyTemplateVariables(TEMPLATE_HTML, { title: "Hi", accent: "#00ff00", duration: 9.5 }, { duration_variable: "duration" });
  assert.match(out, /data-duration="9.5" data-width/);
  assert.match(out, /<h1 class="clip" data-start="0" data-duration="6"/, "nested clips keep their own timing");
  const decl = JSON.parse(/data-composition-variables='([^']*)'/.exec(out)[1]);
  assert.equal(decl.find((d) => d.id === "title").default, "Hi");
  assert.equal(decl.find((d) => d.id === "accent").default, "#00ff00");
  assert.throws(() => applyTemplateVariables(TEMPLATE_HTML, { title: 5 }), /must be a string/);
  assert.throws(() => applyTemplateVariables(TEMPLATE_HTML, { accent: "red;background:url(x)" }), /CSS color/);
  assert.throws(() => applyTemplateVariables(TEMPLATE_HTML, { duration: 0.5 }), />= 1/);
  assert.throws(() => applyTemplateVariables(TEMPLATE_HTML, { nope: 1 }), /not declared/);
  assert.equal(rewriteRootDuration('<div data-composition-id="x">', 3), '<div data-composition-id="x" data-duration="3">');
});

// A caller's text is data, whatever characters it carries. String.prototype.replace reads $-patterns in a
// replacement STRING ($1, $&, $`, $', $$), so the declaration JSON has to go in through a replacer function:
// through a string, ordinary speech such as "over $100 million" corrupts the attribute, and "$&" splices the
// old attribute (with its closing quote) into the new one, which lets the caller's text become attributes of
// the composition's root element.
const attrJson = (html) => {
  const raw = /data-composition-variables='([^']*)'/.exec(html);
  assert.ok(raw, "the root lost its data-composition-variables attribute");
  return JSON.parse(raw[1].replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&"));
};
const declaredDefault = (html, id) => attrJson(html).find((d) => d.id === id).default;
// The attribute names an HTML parser would give the composition's root tag (quoted and unquoted values skipped).
function rootAttributeNames(html) {
  const tag = /<[a-zA-Z][^>]*\bdata-composition-id=[^>]*>/.exec(html)[0];
  const names = [];
  let i = tag.indexOf(" ");
  while (i < tag.length) {
    while (i < tag.length && /[\s/>]/.test(tag[i])) i++;
    const start = i;
    while (i < tag.length && !/[\s=/>]/.test(tag[i])) i++;
    if (i > start) names.push(tag.slice(start, i));
    while (i < tag.length && /\s/.test(tag[i])) i++;
    if (tag[i] === "=") {
      i++;
      while (i < tag.length && /\s/.test(tag[i])) i++;
      if (tag[i] === "'" || tag[i] === '"') {
        const q = tag[i];
        i = tag.indexOf(q, i + 1);
        i = i < 0 ? tag.length : i + 1;
      } else {
        while (i < tag.length && !/[\s>]/.test(tag[i])) i++;
      }
    }
  }
  return names;
}
const DOLLAR_VALUES = [
  "costs $1", "$10", "$100", "$1,000", "$1.2M", "x $& y", "x $' y", "x $` y", "a $$ b", "$$$", "$", "R&D $& co",
  "it's $1, don't $&", "$5", "$ 1", "$0", "$2",
];

test("applyTemplateVariables: a value carrying $ replacement patterns arrives in the declaration byte for byte", () => {
  for (const value of DOLLAR_VALUES) {
    const out = applyTemplateVariables(TEMPLATE_HTML, { title: value });
    assert.equal(declaredDefault(out, "title"), value, `title ${JSON.stringify(value)}`);
    // and the other declarations are exactly as they were
    assert.deepEqual(attrJson(out).filter((d) => d.id !== "title"), attrJson(TEMPLATE_HTML).filter((d) => d.id !== "title"));
  }
  // the shipped templates, with the values the docs advertise and ordinary speech
  const load = (name) => [readFileSync(join(__dirname, "compose-templates", name, "index.html"), "utf8"), JSON.parse(readFileSync(join(__dirname, "compose-templates", name, "template.json"), "utf8"))];
  const [stat, statManifest] = load("stat-card");
  assert.equal(declaredDefault(applyTemplateVariables(stat, { stat: "$1.2M" }, statManifest), "stat"), "$1.2M");
  const [card, cardManifest] = load("title-card");
  assert.equal(declaredDefault(applyTemplateVariables(card, { title: "Only $10 today" }, cardManifest), "title"), "Only $10 today");
  const [callout, calloutManifest] = load("callout-label");
  assert.equal(declaredDefault(applyTemplateVariables(callout, { term: "$1.2M" }, calloutManifest), "term"), "$1.2M");
});

test("applyTemplateVariables: a caption list with dollar amounts and apostrophes reaches captions-bar intact", () => {
  const html = readFileSync(join(__dirname, "compose-templates", "captions-bar", "index.html"), "utf8");
  const manifest = JSON.parse(readFileSync(join(__dirname, "compose-templates", "captions-bar", "template.json"), "utf8"));
  const words = JSON.stringify([[0.5, 2.3, "over $100 million raised"], [3, 5, "don't stop"], [5.2, 7, "it costs $1,000 & $&"]]);
  const out = applyTemplateVariables(html, { words_json: words, duration: 8 }, manifest);
  assert.equal(declaredDefault(out, "words_json"), words);
  assert.deepEqual(JSON.parse(declaredDefault(out, "words_json")).map((t) => t[2]), ["over $100 million raised", "don't stop", "it costs $1,000 & $&"]);
});

test("applyTemplateVariables: caller text never adds an attribute to the root element or changes the rest of the page", () => {
  const before = rootAttributeNames(TEMPLATE_HTML);
  const stripped = (html) => html.replace(/data-composition-variables='[^']*'/, "");
  const hostile = [
    "$& onanimationstart=alert(1) x", "$' onclick=alert(1) y", "$` onload=alert(1) z", "$1 onfocus=alert(1)",
    "x' onmouseover='alert(1)", "\" onerror=\"alert(1)", "&#39; onclick=alert(1) ", "<img src=x onerror=alert(1)>",
    "$&amp; data-injected=1 $&#39; data-injected=2",
  ];
  for (const value of hostile) {
    const out = applyTemplateVariables(TEMPLATE_HTML, { title: value });
    assert.deepEqual(rootAttributeNames(out), before, `root attributes changed by ${JSON.stringify(value)}`);
    assert.equal(declaredDefault(out, "title"), value, `title ${JSON.stringify(value)}`);
    assert.equal(stripped(out), stripped(TEMPLATE_HTML), `the page outside the declaration changed for ${JSON.stringify(value)}`);
  }
});

test("render: a value with $ patterns reaches the composition the runner copies, and the template dir is untouched", () => {
  const fx = fixture();
  const vars = join(fx.root, "vars.json");
  const value = "Only $100, $& more, it's $1";
  writeFileSync(vars, JSON.stringify({ title: value, duration: 4 }));
  const r = fx.run(renderArgs(fx, ["--variables-file", vars, "--keep-work"]));
  assert.equal(r.status, 0, r.stdout + r.stderr);
  const job = readdirSync(join(fx.cache, "work"))[0];
  const html = readFileSync(join(fx.cache, "work", job, "project", "index.html"), "utf8");
  assert.equal(declaredDefault(html, "title"), value);
  assert.deepEqual(rootAttributeNames(html), [...rootAttributeNames(TEMPLATE_HTML)]);
  assert.match(html, /data-duration="4"/);
});

test("summarizeProbe + verifyOutput: codec, alpha, size, fps, duration, audio", () => {
  const s = summarizeProbe(JSON.parse(probeDoc({ codec: "prores", pix: "yuva444p10le", dur: "6.0", packets: "180" })));
  assert.equal(s.has_alpha, true);
  assert.equal(s.frames, 180);
  assert.deepEqual(verifyOutput(s, { format: "mov", width: 1920, height: 1080, fps: 30, duration: 6 }), []);
  const probs = verifyOutput(s, { format: "mp4", duration: 4, hasAudio: true });
  assert.ok(probs.some((p) => p.startsWith("codec prores")));
  assert.ok(probs.some((p) => p.startsWith("duration")));
  assert.ok(probs.some((p) => p.includes("<audio>")));
});

test("compositionMeta reads the root's declared size, fps and duration", () => {
  assert.deepEqual(compositionMeta(TEMPLATE_HTML), { width: 1920, height: 1080, fps: 30, duration: 6, hasAudio: false });
});

// --- the shipped templates are vetted: deterministic, offline, declared --------------------------

test("shipped templates: offline, deterministic, declared variables, a duration parameter, a README", () => {
  const dir = join(__dirname, "compose-templates");
  const names = listTemplates(dir);
  assert.ok(names.includes("title-card") && names.includes("lower-third"), `shipped: ${names}`);
  for (const n of names) {
    const html = readFileSync(join(dir, n, "index.html"), "utf8");
    assert.ok(!/Math\.random|Date\.now|new Date|performance\.now|crypto\./.test(html), `${n}: non-deterministic API`);
    assert.ok(!/https?:\/\//i.test(html.replace(/xmlns="http:\/\/www\.w3\.org\/[^"]*"/g, "")), `${n}: a network URL (renders must be offline)`);
    assert.ok(!/gsap|<script[^>]+src=/i.test(html), `${n}: vendored or remote scripts are not allowed`);
    assert.ok(/@font-face/.test(html), `${n}: fonts must be declared locally (an undeclared family triggers a Google Fonts fetch)`);
    const manifest = JSON.parse(readFileSync(join(dir, n, "template.json"), "utf8"));
    const applied = applyTemplateVariables(html, {}, manifest);
    const decls = JSON.parse(/data-composition-variables='([^']*)'/.exec(html)[1].replace(/&#39;/g, "'"));
    assert.ok(decls.find((d) => d.id === manifest.duration_variable && d.type === "number"), `${n}: duration variable declared`);
    assert.equal(compositionMeta(applied).duration, decls.find((d) => d.id === manifest.duration_variable).default);
    assert.ok(existsSync(join(dir, n, "README.md")), `${n}: README`);
  }
});

// --- the hyperframes-student-kit ports ----------------------------------------------------------
// Four templates adapted from a community teaching kit at ONE pinned commit, kept under the kit's own
// licences (MIT for its original material, plus its own use permission for the style library and
// templates). The kit is a reference, never an install: nothing here runs its scripts or its CLI.
// These tests are the contract of the port: what ships, what must NOT survive the port (creator
// names, brand palette, placeholder copy, CDN hosts), and proof that the licence texts were kept as
// received. They run on a bare Node, like the rest of this file.

const TEMPLATES_DIR = join(__dirname, "compose-templates");
const KIT_COMMIT = "0d30152";
// template -> the kit card it was adapted from (registry id) and whether it is an alpha overlay.
const KIT_PORTS = {
  "stat-card": { card: "kallaway.t1.stat.figure", alpha: false },
  "section-title": { card: "kallaway.t1.section.breath", alpha: false },
  "callout-label": { card: "kallaway.t2.label.callout", alpha: true },
  "checklist-card": { card: "kallaway.t1.overview.checklist", alpha: false },
};
const KIT_TEMPLATE_NAMES = [...Object.keys(KIT_PORTS), "captions-bar"];

function templateFiles(name) {
  const dir = join(TEMPLATES_DIR, name);
  return readdirSync(dir).map((f) => ({ file: f, text: readFileSync(join(dir, f), "utf8") }));
}
function declaredVariables(html) {
  return JSON.parse(/data-composition-variables='([^']*)'/.exec(html)[1].replace(/&#39;/g, "'"));
}

test("kit ports: the four templates ship with the canvas, alpha flag and duration variable the plan names", () => {
  const shipped = listTemplates(TEMPLATES_DIR);
  for (const [name, want] of Object.entries(KIT_PORTS)) {
    assert.ok(shipped.includes(name), `${name} is not shipped (shipped: ${shipped})`);
    const manifest = JSON.parse(readFileSync(join(TEMPLATES_DIR, name, "template.json"), "utf8"));
    const html = readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8");
    assert.equal(manifest.name, name);
    assert.deepEqual([manifest.width, manifest.height, manifest.fps], [1920, 1080, 30], `${name}: canvas`);
    assert.equal(manifest.alpha, want.alpha, `${name}: alpha flag`);
    assert.equal(manifest.duration_variable, "duration");
    assert.ok(manifest.description.length > 40, `${name}: description says what it draws`);
    assert.equal(manifest.formats[0], want.alpha ? "webm" : "mp4", `${name}: the natural format leads`);
    for (const f of ["webm", "mov", "png-sequence"]) assert.ok(manifest.formats.includes(f) || !want.alpha, `${name}: an alpha overlay offers ${f}`);
    // the page's root says what the manifest says, and never carries audio (render silent, mux later)
    const meta = compositionMeta(html);
    assert.deepEqual([meta.width, meta.height, meta.fps], [manifest.width, manifest.height, manifest.fps], `${name}: root vs manifest`);
    assert.equal(/data-composition-id="([^"]+)"/.exec(html)[1], name, `${name}: composition id is the template name`);
    assert.match(html, /\bdata-no-timeline\b/, `${name}: CSS keyframes only, seeked by the CSS adapter`);
    assert.equal(meta.hasAudio, false, `${name}: audio stays out of templates`);
    // an alpha template must not paint an opaque page background
    if (want.alpha) assert.match(html, /html,\s*body\s*\{[^}]*background:\s*transparent/, `${name}: transparent page background`);
  }
});

test("kit ports: every declared variable is read by the page, typed for callers, bounded, and documented", () => {
  for (const name of KIT_TEMPLATE_NAMES) {
    const html = readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8");
    const readme = readFileSync(join(TEMPLATES_DIR, name, "README.md"), "utf8");
    const manifest = JSON.parse(readFileSync(join(TEMPLATES_DIR, name, "template.json"), "utf8"));
    const decls = declaredVariables(html);
    const ids = decls.map((d) => d.id);
    assert.equal(new Set(ids).size, ids.length, `${name}: duplicate variable id`);
    for (const d of decls) {
      assert.ok(["string", "color", "number", "boolean", "enum"].includes(d.type), `${name}.${d.id}: type ${d.type} is not accepted from callers`);
      assert.ok(d.label, `${name}.${d.id}: label`);
      assert.ok(readme.includes("`" + d.id + "`"), `${name}: the README's variable table does not document ${d.id}`);
      const read = html.includes(`data-var-text="${d.id}"`) || new RegExp(`var\\(--${d.id}\\b`).test(html) || new RegExp(`getVariables\\(\\)[^;]*\\b${d.id}\\b`).test(html) || d.id === manifest.duration_variable; // the runner reads the duration variable
      assert.ok(read, `${name}.${d.id}: declared, but nothing in the page reads it`);
      if (d.type === "string") {
        assert.ok(Number.isInteger(d.maxLength) && d.maxLength > 0, `${name}.${d.id}: a string variable needs an explicit maxLength (the runner's silent cap is 200)`);
        assert.ok(d.default.length <= d.maxLength, `${name}.${d.id}: default longer than maxLength`);
        assert.ok(!/['&<>]/.test(d.default), `${name}.${d.id}: default carries a character the attribute would have to escape`);
      }
      if (d.type === "number") {
        assert.ok(Number.isFinite(d.min) && Number.isFinite(d.max) && d.default >= d.min && d.default <= d.max, `${name}.${d.id}: number needs min <= default <= max`);
      }
    }
    for (const m of html.matchAll(/data-var-text="([^"]+)"/g)) assert.ok(ids.includes(m[1]), `${name}: data-var-text="${m[1]}" is not declared`);
  }
});

test("kit ports: every variable takes its declared limit and refuses one past it", () => {
  for (const name of KIT_TEMPLATE_NAMES) {
    const html = readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8");
    const manifest = JSON.parse(readFileSync(join(TEMPLATES_DIR, name, "template.json"), "utf8"));
    for (const d of declaredVariables(html)) {
      if (d.type === "string") {
        applyTemplateVariables(html, { [d.id]: "W".repeat(d.maxLength) }, manifest);
        assert.throws(() => applyTemplateVariables(html, { [d.id]: "W".repeat(d.maxLength + 1) }, manifest), /longer than/, `${name}.${d.id}`);
      } else if (d.type === "number") {
        applyTemplateVariables(html, { [d.id]: d.min }, manifest);
        applyTemplateVariables(html, { [d.id]: d.max }, manifest);
        assert.throws(() => applyTemplateVariables(html, { [d.id]: d.min - 1 }, manifest), />= /, `${name}.${d.id}`);
        assert.throws(() => applyTemplateVariables(html, { [d.id]: d.max + 1 }, manifest), /<= /, `${name}.${d.id}`);
      }
    }
  }
});

// The residue gate. Names are matched CASE-SENSITIVELY on purpose: the source card ids in the
// provenance line are lower-case registry ids (kallaway.t1.stat.figure), which the provenance line
// records so a port can be traced to its source card, while the capitalised style names, the brand
// acronym and the placeholder people are exactly what the port must neutralise. The kit author's own
// name is confined to the retained licence text.
const KIT_RESIDUE = [
  [/\bAIS\b|AI Automation|Vox\b|Kallaway|\bInfinite\b|Dana Whitlock|Senior Correspondent|McKinsey|MCKINSEY|Global Payments/, "a creator, brand or placeholder name"],
  [/jsdelivr|googleapis|gstatic|cdnjs|unpkg/i, "a CDN host"],
  [/Nate Herk|nateherk/i, "the kit author's name outside the retained licence texts"],
];
// The kit's own placeholder copy for the four cards: none of it may ship as a default.
const KIT_PLACEHOLDER_COPY = [
  "ADOPTION RATE", "of organizations have", "deployed AI tools", "core workflows this year", "Module 04", "Why Attention",
  "Why This Matters", "Context collapse", "silent killer", "Complete Pre-Launch", "Thumbnail tested", "front-loaded", "First 30 seconds", "End screen CTA",
];

test("kit ports: no creator name, brand palette, placeholder copy or CDN host survives in a shipped template", () => {
  for (const name of KIT_TEMPLATE_NAMES) {
    for (const { file, text } of templateFiles(name)) {
      for (const [re, what] of KIT_RESIDUE) assert.ok(!re.test(text), `${name}/${file}: ${what} (${re})`);
      for (const copy of KIT_PLACEHOLDER_COPY) assert.ok(!text.includes(copy), `${name}/${file}: kit placeholder copy "${copy}"`);
      // the kit's AIS accent and its brand yellow/red/orange must not be the defaults of the port
      assert.ok(!/#37bdf8|55,\s*189,\s*248|#f5d82a|#ff3b30|#ff8a5c/i.test(text), `${name}/${file}: a kit brand colour`);
    }
  }
});

test("kit ports: CSS animation is finite, nothing waits on a timer, and every @font-face lands in the shared kit", () => {
  for (const name of listTemplates(TEMPLATES_DIR)) {
    const html = readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8");
    assert.ok(!/\binfinite\b/.test(html), `${name}: an infinite animation cannot be captured to a fixed length`);
    assert.ok(!/setTimeout|setInterval|requestAnimationFrame|await\s|async\s/.test(html), `${name}: timelines must be built synchronously`);
    assert.ok(!/@import/.test(html), `${name}: @import is a network request`);
    const declared = new Set();
    for (const m of html.matchAll(/@font-face\s*\{([^}]*)\}/g)) {
      declared.add(/font-family:\s*"([^"]+)"/.exec(m[1])[1]);
      const src = /url\("([^"]+)"\)/.exec(m[1])[1];
      assert.ok(src.startsWith("shared/fonts/") && existsSync(join(TEMPLATES_DIR, "_shared", src.slice("shared/".length))), `${name}: @font-face src ${src} is not in the shared kit`);
    }
    const css = html.replace(/@font-face\s*\{[^}]*\}/g, "");
    for (const m of css.matchAll(/font-family:\s*([^;}]+)[;}]/g)) {
      for (const fam of m[1].split(",").map((s) => s.trim())) {
        const quoted = /^"([^"]+)"$/.exec(fam);
        if (quoted) assert.ok(declared.has(quoted[1]), `${name}: font family ${fam} is used but not declared with @font-face`);
        else assert.match(fam, /^(sans-serif|serif|monospace|system-ui|inherit)$/, `${name}: unquoted family ${fam}`);
      }
    }
  }
});

test("kit ports: each README names its source card at the pinned kit commit and points at the retained licence texts", () => {
  for (const [name, want] of Object.entries(KIT_PORTS)) {
    const readme = readFileSync(join(TEMPLATES_DIR, name, "README.md"), "utf8");
    assert.ok(readme.includes(`adapted from hyperframes-student-kit @${KIT_COMMIT} card ${want.card}`), `${name}: provenance line`);
    const link = /\]\((\.\.\/_third_party\/hyperframes-student-kit\/[^)]*)\)/.exec(readme);
    assert.ok(link, `${name}: no link to the retained licence texts`);
    assert.ok(existsSync(join(TEMPLATES_DIR, name, link[1])), `${name}: README link ${link[1]} does not resolve`);
    assert.match(readme, /## Changes from the source card/, `${name}: the README lists what was changed`);
    assert.match(readme, /## Measured/, `${name}: the README carries a measured render`);
  }
});

test("kit ports: the kit's MIT licence and use permission are kept verbatim under _third_party, which is never a template", () => {
  const dir = join(TEMPLATES_DIR, "_third_party", "hyperframes-student-kit");
  // hashes of the two files as they sit at the pinned kit commit (LF); a CRLF checkout is folded first
  const sha = (f) => createHash("sha256").update(readFileSync(join(dir, f), "utf8").replace(/\r\n/g, "\n")).digest("hex");
  assert.equal(sha("LICENSE"), "263623d3ac61f5b09eeceec5af2395bb15e566d0507558f5b6c405f2ba132bd1");
  assert.equal(sha("PIPELINE-USE-PERMISSION.txt"), "51fc8998336079bd888662ac3ebc708ba05444a98b5ad26af757567f8125259d");
  assert.ok(!listTemplates(TEMPLATES_DIR).includes("_third_party"));
  const scratch = mkdtempSync(join(tmpdir(), "compose-tp-"));
  assert.throws(() => materializeTemplate(TEMPLATES_DIR, "_third_party", {}, scratch), (e) => e.cls === "BAD_INPUT");
  assert.ok(/kit_commit|0d30152a82b9ceb93cfdd9bdbf46f0d5ab3cde86/.test(readFileSync(join(dir, "PROVENANCE.md"), "utf8")), "PROVENANCE.md pins the full kit commit");
});

// --- captions-bar: word groups from a transcript, over transparency ------------------------------------
// Authored for this harness (no kit card exists for it). Its one variable that matters is words_json:
// [start,end,text] triples that render/captions-groups.mjs builds from offload_transcribe's
// <base>.segments.json. HyperFrames accepted a 16 KB string variable through lint, check and
// --strict-variables (measured), which is the cap this template declares.

test("captions-bar: an alpha overlay whose words arrive as one bounded string variable, read through getVariables()", () => {
  const dir = join(TEMPLATES_DIR, "captions-bar");
  assert.ok(listTemplates(TEMPLATES_DIR).includes("captions-bar"), "captions-bar is not shipped");
  const html = readFileSync(join(dir, "index.html"), "utf8");
  const manifest = JSON.parse(readFileSync(join(dir, "template.json"), "utf8"));
  assert.equal(manifest.name, "captions-bar");
  assert.deepEqual([manifest.width, manifest.height, manifest.fps, manifest.alpha, manifest.duration_variable], [1920, 1080, 30, true, "duration"]);
  assert.equal(manifest.formats[0], "webm");
  assert.match(html, /html,\s*body\s*\{[^}]*background:\s*transparent/, "transparent page background");
  assert.match(html, /\bdata-no-timeline\b/);
  const decls = declaredVariables(html);
  const words = decls.find((d) => d.id === "words_json");
  assert.ok(words, "words_json is declared");
  assert.equal(words.type, "string");
  assert.equal(words.maxLength, 16000, "the cap that was measured");
  const duration = decls.find((d) => d.id === "duration");
  assert.deepEqual([duration.type, duration.min >= 1, duration.max], ["number", true, 600]);
  // the default is a valid, ordered list of triples that fits inside the default duration
  const triples = JSON.parse(words.default);
  assert.ok(Array.isArray(triples) && triples.length >= 3, "a default worth looking at");
  triples.forEach((t, i) => {
    assert.ok(Array.isArray(t) && t.length === 3 && typeof t[0] === "number" && typeof t[1] === "number" && typeof t[2] === "string" && t[2], `default group ${i}`);
    assert.ok(t[0] >= 0 && t[1] > t[0] && t[1] <= duration.default, `default group ${i} times`);
    if (i) assert.ok(t[0] >= triples[i - 1][1], `default group ${i} overlaps the one before`);
  });
  // The runtime strips braces from every string it copies into a CSS custom property, so a JSON string can
  // only be read through getVariables(); the text goes in with textContent, never as markup.
  assert.match(html, /window\.__hyperframes\.getVariables\(\)/);
  assert.ok(!/var\(--words_json/.test(html), "the custom property has lost its braces");
  assert.ok(!/innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function/.test(html), "text only, never markup or code");
  assert.match(html, /textContent/);
  // a malformed variable must fail the composition (check reports the exception) instead of rendering nothing
  assert.match(html, /throw new Error\(/);
  // and the finite, clock-free contract every template keeps (the shared test above covers the rest)
  assert.ok(!/\bDate\b|Math\.random|performance\.now|setTimeout|setInterval|requestAnimationFrame/.test(html));
});

test("captions-bar: its limits are the helper's template limits, the helper's default chunk fits them, and its README names the helper and keeps audio out", () => {
  const html = readFileSync(join(TEMPLATES_DIR, "captions-bar", "index.html"), "utf8");
  const decls = declaredVariables(html);
  const wordsVar = decls.find((d) => d.id === "words_json");
  const durationVar = decls.find((d) => d.id === "duration");
  // what the template accepts is the helper's TEMPLATE_LIMITS (the most a caller may ask for) ...
  assert.equal(wordsVar.maxLength, CAPTION_LIMITS.variableChars);
  assert.equal(durationVar.max, CAPTION_LIMITS.variableSec);
  // ... and the size the helper cuts at by default is a chunk the template accepts
  assert.ok(CAPTION_DEFAULTS.variableChars <= wordsVar.maxLength, "the default character cap fits words_json");
  assert.ok(CAPTION_DEFAULTS.variableSec >= durationVar.min && CAPTION_DEFAULTS.variableSec <= durationVar.max, "the default chunk length is a legal duration");
  const readme = readFileSync(join(TEMPLATES_DIR, "captions-bar", "README.md"), "utf8");
  assert.ok(readme.includes("render/captions-groups.mjs"), "README names the helper");
  assert.ok(readme.includes("offload_media"), "README says how the audio is put back");
  assert.match(readme, /silent/i, "README says the render is silent");
  assert.match(readme, /## Measured/);
});

// --- captions-bar's script, executed ------------------------------------------------------------------
// The template keeps its whole behaviour (the seek listener, the group lookup, the fades, the validation) in one
// inline script, and a check that only reads that script's source cannot tell a working page from one whose event
// name or field has drifted. So the script is run: under node:vm with a stub window and document, the way the
// page's runtime would, then read back after synthetic hf-seek events. Nothing here needs HyperFrames or Chrome.

function loadCaptionsPage(wordsJson) {
  const html = readFileSync(join(TEMPLATES_DIR, "captions-bar", "index.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  assert.equal(scripts.length, 1, "captions-bar has exactly one inline script");
  const listeners = [];
  const cap = { style: {}, textContent: "" };
  const win = {
    __hyperframes: { getVariables: () => ({ words_json: wordsJson }) },
    addEventListener: (type, fn) => listeners.push({ type, fn }),
  };
  vm.runInNewContext(scripts[0][1], { window: win, document: { getElementById: (id) => (id === "cap" ? cap : null) } });
  // seek(t) is one hf-seek event carrying the time; what the page painted is read back from its one element
  const seek = (time) => {
    for (const l of listeners) if (l.type === "hf-seek") l.fn({ detail: { time } });
    return { text: cap.textContent, opacity: Number(cap.style.opacity), transform: cap.style.transform };
  };
  return { listeners, seek, cap };
}

const CAPTIONS_DEFAULT_WORDS = JSON.stringify([[0.5, 2.7, "Captions follow the words"], [2.9, 5.1, "one short group at a time"], [5.3, 7.5, "then they get out of the way"]]);
const near = (a, b, msg) => assert.ok(Math.abs(a - b) < 1e-9, `${msg}: ${a} is not ${b}`);

test("captions-bar script: it listens for hf-seek only, and paints the group on screen with its fades and rise", () => {
  const page = loadCaptionsPage(CAPTIONS_DEFAULT_WORDS);
  assert.deepEqual(page.listeners.map((l) => l.type), ["hf-seek"], "one listener, on the event HyperFrames dispatches");
  assert.equal(page.cap.style.opacity, "0", "before any seek nothing shows");
  assert.equal(page.seek(0.3).opacity, 0, "before the first group");
  // the first group starts at 0.5 s: it enters over 0.14 s (ease-out cubic) and rises 12 px
  const start = page.seek(0.5);
  assert.deepEqual([start.text, start.opacity, start.transform], ["Captions follow the words", 0, "translateY(12.000px)"]);
  const half = page.seek(0.57); // halfway through the entry: 1 - (1 - 0.5)^3 = 0.875
  near(half.opacity, 0.875, "opacity halfway through the entry");
  assert.equal(half.transform, "translateY(1.500px)");
  const held = page.seek(1.5);
  assert.deepEqual([held.text, held.opacity, held.transform], ["Captions follow the words", 1, "translateY(0.000px)"]);
  assert.equal(page.seek(0.64).opacity, 1, "fully in after 0.14 s");
  // the exit is the last 0.12 s of the group: 2.64 s is halfway, and the group is gone at its end
  near(page.seek(2.64).opacity, 0.5, "opacity halfway through the exit");
  assert.equal(page.seek(2.7).opacity, 0, "gone at its end");
  assert.equal(page.seek(2.8).opacity, 0, "nothing between two groups");
  // later groups show their own text, and the last one fades out and is gone after its end
  assert.equal(page.seek(3.5).text, "one short group at a time");
  assert.equal(page.seek(3.5).opacity, 1);
  const tail = page.seek(7.44);
  assert.equal(tail.text, "then they get out of the way");
  assert.ok(tail.opacity > 0.4 && tail.opacity < 0.6, `the last group is fading out: ${tail.opacity}`);
  assert.equal(page.seek(7.5).opacity, 0);
  assert.equal(page.seek(30).opacity, 0, "long after the last group");
});

test("captions-bar script: a frame is a pure function of the seek time, whatever order the workers ask in", () => {
  const a = loadCaptionsPage(CAPTIONS_DEFAULT_WORDS);
  const b = loadCaptionsPage(CAPTIONS_DEFAULT_WORDS);
  // what a viewer sees: an invisible element has no text or offset that matters
  const seen = (f) => JSON.stringify(f.opacity === 0 ? { opacity: 0 } : f);
  const times = [0.2, 0.5, 0.55, 1.5, 2.64, 2.8, 3.0, 3.06, 4, 5.2, 5.3, 5.36, 6, 7.44, 7.6];
  const expected = new Map(times.map((t) => [t, seen(a.seek(t))]));
  // b visits the same times shuffled and repeated: every answer must match the first pass
  for (const t of [7.44, 0.5, 6, 0.2, 2.64, 7.6, 3.06, 1.5, 5.2, 0.55, 4, 2.8, 5.36, 3.0, 5.3, 6, 0.5]) assert.equal(seen(b.seek(t)), expected.get(t), `t=${t}`);
  assert.ok([...expected.values()].some((v) => JSON.parse(v).opacity > 0), "the times include visible frames");
});

test("captions-bar script: the group lookup finds the right group among hundreds, and an empty list paints nothing", () => {
  const groups = Array.from({ length: 300 }, (_, i) => [i * 2 + 0.5, i * 2 + 1.7, `group ${i}`]);
  const page = loadCaptionsPage(JSON.stringify(groups));
  for (const i of [0, 1, 2, 149, 150, 151, 298, 299]) {
    const inside = page.seek(i * 2 + 1.1);
    assert.deepEqual([inside.text, inside.opacity], [`group ${i}`, 1], `inside group ${i}`);
    assert.equal(page.seek(i * 2 + 1.85).opacity, 0, `the gap after group ${i}`);
  }
  const empty = loadCaptionsPage("[]");
  for (const t of [0, 1, 100]) assert.equal(empty.seek(t).opacity, 0);
});

test("captions-bar script: text goes in as text, never markup", () => {
  const page = loadCaptionsPage(JSON.stringify([[0, 2, "<b>bold</b> & <i>x</i> $& \"quoted\""]]));
  const shown = page.seek(1);
  assert.equal(shown.text, "<b>bold</b> & <i>x</i> $& \"quoted\"");
  assert.ok(!("innerHTML" in page.cap), "the element's markup is never written");
});

test("captions-bar script: a malformed words_json throws its own message, so check fails the render instead of an empty overlay", () => {
  const bad = (json, message) => assert.throws(() => loadCaptionsPage(json), (e) => message.test(String(e && e.message)), json);
  bad("not json", /^captions-bar: words_json is not JSON: /);
  bad('{"a":1}', /^captions-bar: words_json must be an array of \[start, end, text\] triples$/);
  bad('"text"', /^captions-bar: words_json must be an array of \[start, end, text\] triples$/);
  const entry = /^captions-bar: words_json entry 0 is not \[start, end, text\] with 0 <= start < end$/;
  bad("[[0,1]]", entry);
  bad('[[0,1,"a","b"]]', entry);
  bad('[["0",1,"a"]]', entry);
  bad('[[0,"1","a"]]', entry);
  bad("[[0,1,5]]", entry);
  bad('[[-1,1,"a"]]', entry);
  bad('[[1,1,"a"]]', entry);
  bad('[[2,1,"a"]]', entry);
  bad('[null]', entry);
  bad('[[0,1,"a"],[0.5,2,"b"]]', /^captions-bar: words_json entry 1 starts before entry 0 ends \(one group is on screen at a time\)$/);
  // valid shapes load: an empty list, groups that touch, and a single group
  for (const ok of ["[]", '[[0,1,"a"],[1,2,"b"]]', '[[0,0.5,"only"]]']) assert.doesNotThrow(() => loadCaptionsPage(ok), ok);
});

test("captions-bar script: every chunk the helper emits from a transcript longer than one chunk loads, shows each group, and is accepted by the runner", () => {
  // 320 segments of 7 words at 0.4 s, each word two tokens (a word-initial token and a "s"-style continuation)
  const segments = [];
  let t = 0;
  for (let i = 0; i < 320; i++) {
    const words = [];
    let text = "";
    for (let k = 0; k < 7; k++) {
      words.push({ word: ` w${i}x${k}`, start: t, end: t + 0.25, probability: 0.9 }, { word: k === 6 ? "." : "s", start: t + 0.25, end: t + 0.4, probability: 0.9 });
      text += ` w${i}x${k}${k === 6 ? "." : "s"}`;
      t += 0.4;
    }
    segments.push({ id: i, start: t - 2.8, end: t, text, words });
    t += 0.35;
  }
  const html = readFileSync(join(TEMPLATES_DIR, "captions-bar", "index.html"), "utf8");
  const manifest = JSON.parse(readFileSync(join(TEMPLATES_DIR, "captions-bar", "template.json"), "utf8"));
  // at the helper's default cut (no option) and at the longest chunk the template accepts (its own maximum)
  for (const [label, variableSec, limit] of [["the default cut", undefined, CAPTION_DEFAULTS.variableSec], ["the template's maximum", CAPTION_LIMITS.variableSec, CAPTION_LIMITS.variableSec]]) {
    const { chunks, groups } = captionsFromSegments(segments, { pace: "punchy", variableSec });
    assert.ok(t > limit && chunks.length >= 2, `${label}: ${Math.round(t)} s of speech should need more than one chunk (got ${chunks.length})`);
    let shown = 0;
    for (const [k, c] of chunks.entries()) {
      assert.ok(c.duration_sec <= limit, `${label}, chunk ${k}: ${c.duration_sec} s is past the cap`);
      const out = applyTemplateVariables(html, { words_json: c.words_json, duration: c.duration_sec }, manifest);
      assert.equal(declaredDefault(out, "words_json"), c.words_json, `${label}, chunk ${k}: the runner keeps the variable byte for byte`);
      assert.match(out, new RegExp(`data-duration="${String(c.duration_sec).replace(".", "\\.")}"`), `${label}, chunk ${k}: the runner sets the chunk's duration`);
      const page = loadCaptionsPage(c.words_json);
      const triples = JSON.parse(c.words_json);
      for (const [s, e, text] of triples) {
        assert.ok(e <= c.duration_sec + 1e-9, `${label}, chunk ${k}: a caption ends at ${e} s, after the ${c.duration_sec} s the chunk declares`);
        const mid = page.seek((s + e) / 2);
        assert.equal(mid.text, text, `${label}, chunk ${k}: the page shows the group it should at ${(s + e) / 2} s`);
        assert.ok(mid.opacity > 0, `${label}, chunk ${k}: "${text}" is visible at its middle`);
        shown++;
      }
    }
    assert.equal(shown, groups.length, `${label}: every group of the transcript is shown by some chunk`);
    assert.ok(groups.every((g) => !/\s[.,:;!?]/.test(g.text)), "and none has a space before punctuation");
  }
});

// The page depends on HyperFrames' seek event, which the pinned package dispatches but does not document (only
// getVariables() is documented), so a HyperFrames bump must render this template again. The README records the
// version it was last measured on; this fails on a bump until that section is redone.
test("captions-bar: its Measured section names the HyperFrames version it was measured on, which is the runner's pin", () => {
  // Each check reads only the paragraph or section it is about: a mention elsewhere in the file must not satisfy it.
  const readme = readFileSync(join(TEMPLATES_DIR, "captions-bar", "README.md"), "utf8").replace(/\r\n/g, "\n");
  const at = readme.indexOf("\n## Measured\n");
  assert.ok(at >= 0, "captions-bar/README.md has a Measured section");
  const next = readme.indexOf("\n## ", at + 1);
  const measured = readme.slice(at, next < 0 ? readme.length : next);
  assert.ok(measured.includes(`HyperFrames ${PINNED_VERSION}`), `the Measured section of captions-bar/README.md must say it was measured on HyperFrames ${PINNED_VERSION}; after a version bump, render it again (default sample, and a known group at a known time) and update that section`);
  const guide = readFileSync(join(__dirname, "..", "docs", "OPERATOR-GUIDE.md"), "utf8").replace(/\r\n/g, "\n");
  const start = guide.indexOf("**Bumping HyperFrames.**");
  assert.ok(start >= 0, "the operator guide has a 'Bumping HyperFrames' step");
  const end = guide.indexOf("\n\n", start);
  const bump = guide.slice(start, end < 0 ? guide.length : end);
  assert.match(bump, /captions-bar/, "the bump step itself names captions-bar");
  assert.match(bump, /hf-seek/, "and says why: the template reads the runtime's undocumented seek event");
});

// --- alpha overlays stay transparent ------------------------------------------------------------------
// The pixel proof is a render (README "Measured": the alpha plane at fixed times), which a bare-Node run cannot
// make. What can be read statically is where the page paints: the page, the body and the root must paint nothing,
// and no full-frame layer may paint a background, because an opaque frame under a template that still encodes as
// yuva420p keeps has_alpha true (it reads the pixel format) while covering the footage it is laid over.

function cssRules(html) {
  const css = /<style>([\s\S]*?)<\/style>/.exec(html)[1]
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/@font-face\s*\{[^}]*\}/g, "")
    .replace(/@keyframes\s+[\w-]+\s*\{(?:[^{}]|\{[^{}]*\})*\}/g, "");
  return [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
    selectors: m[1].split(",").map((s) => s.trim()).filter(Boolean),
    decls: m[2].split(";").map((d) => d.trim()).filter(Boolean).map((d) => [d.slice(0, d.indexOf(":")).trim().toLowerCase(), d.slice(d.indexOf(":") + 1).trim()]),
  }));
}
const isBackgroundProp = (p) => p === "background" || p === "background-color" || p === "background-image";
const paintsNothing = (v) => /^(transparent|none)$/i.test(v);

test("alpha overlays: html, body and #root paint nothing, and no full-frame layer paints a background", () => {
  const alpha = listTemplates(TEMPLATES_DIR).filter((n) => JSON.parse(readFileSync(join(TEMPLATES_DIR, n, "template.json"), "utf8")).alpha === true);
  for (const n of ["lower-third", "callout-label", "captions-bar"]) assert.ok(alpha.includes(n), `${n} is an alpha template`);
  for (const name of alpha) {
    const rules = cssRules(readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8"));
    for (const sel of ["html", "body", "#root"]) {
      const bgs = rules.filter((r) => r.selectors.includes(sel)).flatMap((r) => r.decls.filter(([p]) => isBackgroundProp(p)));
      assert.ok(bgs.length > 0, `${name}: ${sel} must state background: transparent`);
      for (const [p, v] of bgs) assert.ok(paintsNothing(v), `${name}: ${sel} paints ${p}: ${v}, so the overlay would cover the footage`);
    }
    for (const r of rules) {
      if (r.selectors.some((s) => ["html", "body", "#root", "*"].includes(s))) continue;
      const props = Object.fromEntries(r.decls);
      const zero = (v) => /^0(px)?$/.test(v || "");
      const fullFrame = (/^(1920px|100%|100vw)$/.test(props.width || "") && /^(1080px|100%|100vh)$/.test(props.height || ""))
        || zero(props.inset)
        || ["top", "right", "bottom", "left"].every((k) => zero(props[k]));
      if (!fullFrame) continue;
      for (const [p, v] of r.decls) if (isBackgroundProp(p)) assert.ok(paintsNothing(v), `${name}: ${r.selectors.join(", ")} covers the whole frame and paints ${p}: ${v}`);
    }
  }
});

// The determinism guard. Measured on this batch: a keyframe that scales an element ABOVE its resting size
// (a halo breathing to 1.06, a ring popping to 1.1) made the frames depend on which render worker drew
// them, so 1 worker, 4 workers and a second run at 4 workers gave three different videos (about 80 of 180
// frames, a few pixels off in the gradient); the same cards with the scale-up replaced by an opacity
// pulse rendered identically at 1, 4 and 4 workers. Scaling UP TO the resting size (an entrance from
// 0.6 or 0.9 to 1) was identical in every run. The ports therefore never scale past 1. This is a static
// stand-in for the real gate, which is comparing framemd5 across worker counts (README, "Adding a template"),
// and it reads the ports and captions-bar only: title-card predates the rule, drifts a glow out to 1.12, and
// measured identical across 1, 2, 4, 6 and auto workers, so the rule is a rule of thumb rather than a law.
test("kit ports: no keyframe scales an element above its resting size", () => {
  for (const name of KIT_TEMPLATE_NAMES) {
    const html = readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8");
    // a page driven from the seek event has no @keyframes: its transforms are set in script, so read those
    if (/hf-seek/.test(html)) {
      assert.ok(!/scale/i.test(html), `${name}: a scripted transform must not scale (frames would depend on the worker count)`);
      continue;
    }
    let blocks = 0;
    for (const block of html.matchAll(/@keyframes\s+([\w-]+)\s*\{((?:[^{}]|\{[^{}]*\})*)\}/g)) {
      blocks++;
      for (const fn of block[2].matchAll(/scale(?:X|Y|Z|3d)?\(([^)]*)\)/g)) {
        for (const n of fn[1].split(",").map((s) => Number.parseFloat(s))) {
          assert.ok(Number.isFinite(n) && n <= 1, `${name}: @keyframes ${block[1]} scales to ${fn[0]} (past its resting size: frames would depend on the worker count)`);
        }
      }
    }
    assert.ok(blocks >= 2, `${name}: the guard found no @keyframes to read`);
  }
});

// --- the hyperframes-compose skill (canonical copy in this repo) ---------------------------------------
// A skill is documentation an agent acts on, so the parts that can silently rot are tested: the catalog
// must list every shipped template with its real alpha flag and default length, and the hard bans must
// be stated. (Installing the skill anywhere else is an operator decision and is not part of this repo.)

test("hyperframes-compose skill: catalog names every shipped template with its alpha flag and default length; the hard bans are stated", () => {
  const file = join(__dirname, "..", "skill", "hyperframes-compose", "SKILL.md");
  assert.ok(existsSync(file), "skill/hyperframes-compose/SKILL.md is missing");
  const md = readFileSync(file, "utf8").replace(/\r\n/g, "\n");
  const front = /^---\nname: hyperframes-compose\ndescription: ([^\n]+)\n---\n/.exec(md);
  assert.ok(front, "front matter must be name + description");
  assert.ok(front[1].includes("offload_compose_video"), "the description names the tool it routes to");
  assert.ok(front[1].length >= 150, "the description says when to use the skill, not just what it is");
  const rows = new Map();
  for (const line of md.split("\n")) {
    const cells = line.split("|").map((c) => c.trim());
    const m = /^`([a-z0-9-]+)`$/.exec(cells[1] || "");
    if (cells.length >= 6 && m) rows.set(m[1], cells);
  }
  for (const name of listTemplates(join(__dirname, "compose-templates"))) {
    const cells = rows.get(name);
    assert.ok(cells, `the catalog has no row for ${name}`);
    const manifest = JSON.parse(readFileSync(join(__dirname, "compose-templates", name, "template.json"), "utf8"));
    const html = readFileSync(join(__dirname, "compose-templates", name, "index.html"), "utf8");
    const dur = declaredVariables(html).find((d) => d.id === manifest.duration_variable).default;
    assert.equal(cells[3], manifest.alpha ? "yes" : "no", `${name}: alpha column`);
    assert.equal(cells[4], `${dur} s`, `${name}: default length column`);
  }
  // the hard bans, in one section, naming every subcommand the runner refuses
  const bans = /## Hard bans\n([\s\S]*?)\n## /.exec(md);
  assert.ok(bans, "a '## Hard bans' section");
  for (const banned of ["npx hyperframes", "init", "skills", "upgrade", "add", "npx skills add"]) {
    assert.ok(bans[1].includes(banned), `the hard bans do not mention ${banned}`);
  }
  assert.match(md, /offload_generate_svg/, "says when to use the SVG tool instead");
  assert.match(md, /offload_media/, "says what offload_media is for");
  assert.match(md, /snapshots/, "carries the snapshot verification loop");
  assert.match(md, /captions-groups\.mjs/, "carries the captions workflow");
});

// The shipped list is written by hand in several places; this keeps two of them honest (the tool
// description is pinned by a Go test, internal/mcpserver/composevideo_test.go).
test("the shipped template list is complete in the templates README and in the media-generation doc", () => {
  const names = listTemplates(TEMPLATES_DIR);
  const readme = readFileSync(join(TEMPLATES_DIR, "README.md"), "utf8");
  const doc = readFileSync(join(__dirname, "..", "docs", "systems", "media-generation.md"), "utf8");
  const shipped = /\(shipped:([^)]*)\)/.exec(doc);
  assert.ok(shipped, "media-generation.md has no '(shipped: ...)' sentence");
  for (const n of names) {
    assert.ok(readme.includes("[`" + n + "`](" + n + "/README.md)"), `${n}: not in the templates README table`);
    assert.ok(shipped[1].includes("`" + n + "`"), `${n}: not in the media-generation shipped list`);
  }
  assert.ok(names.length >= 7, `expected the 7 shipped templates, found ${names}`);
});

// --- drift guards: what the docs, the skill and the pages state must be what the code does ----------------------
// The limits, numbers and lists below are written by hand in several places. Each test reads the one table or
// sentence it is about, so a name or a number mentioned elsewhere in the same file cannot satisfy it, and compares
// it with the source of truth: the page's own declarations, the helper's exports, the runner's allowlist.

const readRepo = (...parts) => readFileSync(join(__dirname, "..", ...parts), "utf8").replace(/\r\n/g, "\n");
// sectionOf returns the text under a heading, up to the next heading of the same or a higher level.
function sectionOf(md, title) {
  const m = new RegExp(`^(#{1,6}) ${title}\\s*$`, "m").exec(md);
  assert.ok(m, `no "${title}" heading`);
  const rest = md.slice(m.index + m[0].length);
  const next = new RegExp(`^#{1,${m[1].length}} `, "m").exec(rest);
  return rest.slice(0, next ? next.index : rest.length);
}
const pageOf = (name) => readFileSync(join(TEMPLATES_DIR, name, "index.html"), "utf8");
const manifestOf = (name) => JSON.parse(readFileSync(join(TEMPLATES_DIR, name, "template.json"), "utf8"));

// The variable table of a template's README: id, type cell, default cell.
function readmeRows(name) {
  const rows = [];
  for (const line of sectionOf(readRepo("render", "compose-templates", name, "README.md"), "Variables").split("\n")) {
    const cells = line.split("|").map((c) => c.trim());
    const id = /^`([a-z0-9_]+)`$/.exec(cells[1] || "");
    if (cells.length >= 5 && id) rows.push({ id: id[1], type: cells[2], def: cells[3] });
  }
  return rows;
}

test("templates: each README's variable table states exactly the ids, types, limits and defaults its page declares", () => {
  for (const name of listTemplates(TEMPLATES_DIR)) {
    const decls = declaredVariables(pageOf(name));
    const rows = readmeRows(name);
    assert.deepEqual(rows.map((r) => r.id).sort(), decls.map((d) => d.id).sort(), `${name}: the table lists exactly the declared variables`);
    for (const d of decls) {
      const row = rows.find((r) => r.id === d.id);
      const at = `${name}.${d.id}`;
      const quoted = "`" + d.default + "`";
      if (d.type === "string") {
        assert.equal(row.type, `string (≤ ${d.maxLength})`, `${at}: type cell`);
        if (row.def.startsWith("`")) assert.equal(row.def, quoted, `${at}: default cell`); // a long default may be described in prose
      } else if (d.type === "number") {
        assert.match(row.type, new RegExp(`^number, ${d.min}-${d.max}( s)?$`), `${at}: type cell "${row.type}"`);
        assert.equal(row.def, quoted, `${at}: default cell`);
      } else {
        assert.equal(row.type, d.type, `${at}: type cell`);
        assert.equal(row.def, quoted, `${at}: default cell`);
      }
    }
  }
});

test("templates: the templates README table states each template's alpha flag, default length and range as it declares them", () => {
  const md = readRepo("render", "compose-templates", "README.md");
  for (const name of listTemplates(TEMPLATES_DIR)) {
    const line = md.split("\n").find((l) => l.startsWith(`| [\`${name}\`](`));
    assert.ok(line, `${name}: no row in the templates README table`);
    const cells = line.split("|").map((c) => c.trim());
    const manifest = manifestOf(name);
    const dur = declaredVariables(pageOf(name)).find((d) => d.id === manifest.duration_variable);
    assert.equal(cells[3].startsWith("yes"), manifest.alpha, `${name}: alpha cell "${cells[3]}"`);
    assert.ok(cells[4].startsWith(`${dur.default} s`), `${name}: default length cell "${cells[4]}", declared ${dur.default} s`);
    const range = /\((\d+) to (\d+) s\)/.exec(cells[4]);
    if (range) assert.deepEqual([Number(range[1]), Number(range[2])], [dur.min, dur.max], `${name}: range cell "${cells[4]}"`);
  }
});

test("captions: the paces, breaks, holds, chunk caps and default cut quoted in the skill, the README and the media doc are the helper's own", () => {
  const num = (s) => Number(s.replace(/,/g, ""));
  const docs = [
    ["the skill", sectionOf(readRepo("skill", "hyperframes-compose", "SKILL.md"), "Captions from a transcript"), { linger: false, minHold: false }],
    ["the captions-bar README", sectionOf(readRepo("render", "compose-templates", "captions-bar", "README.md"), "From a transcript"), { linger: true, minHold: true }],
    ["the media doc", sectionOf(readRepo("docs", "systems", "media-generation.md"), "Captions from a transcript"), { linger: true, minHold: false }],
  ];
  for (const [where, section, has] of docs) {
    const text = section.replace(/\s+/g, " "); // the docs are hard-wrapped: a line break counts as one space
    const one = (re, what) => {
      const m = re.exec(text);
      assert.ok(m, `${where} does not state ${what}`);
      return m;
    };
    assert.equal(num(one(/`punchy`\s+(?:\(up to\s+)?(\d+)/, "the punchy pace")[1]), CAPTION_PACES.punchy.maxWords, `${where}: punchy words`);
    assert.equal(num(one(/`conversational`\s+\(?(\d+)/, "the conversational pace")[1]), CAPTION_PACES.conversational.maxWords, `${where}: conversational words`);
    assert.equal(num(one(/`calm`\s+\(?(\d+)/, "the calm pace")[1]), CAPTION_PACES.calm.maxWords, `${where}: calm words`);
    assert.equal(Number(one(/pause of ([\d.]+) s or more/, "the pause that ends a group")[1]), CAPTION_DEFAULTS.gapSec, `${where}: pause`);
    assert.equal(num(one(/(?:pass|past) (\d+) characters/, "the line width")[1]), CAPTION_DEFAULTS.maxChars, `${where}: characters per group`);
    // what the template accepts: its own limits, which are the helper's TEMPLATE_LIMITS ...
    const cap = one(/at most ([\d,]+) characters and (\d+) s/, "the chunk cap");
    assert.deepEqual([num(cap[1]), num(cap[2])], [CAPTION_LIMITS.variableChars, CAPTION_LIMITS.variableSec], `${where}: chunk cap`);
    // ... and where the helper cuts by default, which is a different and smaller number
    assert.equal(Number(one(/the helper cuts at (\d+) s by default/i, "where the helper cuts by default")[1]), CAPTION_DEFAULTS.variableSec, `${where}: default chunk`);
    assert.equal(Number(one(/`--chunk-sec` sets another cap up to the template's (\d+) s/, "how far --chunk-sec may go")[1]), CAPTION_LIMITS.variableSec, `${where}: --chunk-sec limit`);
    if (has.linger) assert.equal(Number(one(/(?:held |holds each group )([\d.]+) s past its last word/, "the linger")[1]), CAPTION_DEFAULTS.lingerSec, `${where}: linger`);
    if (has.minHold) assert.equal(Number(one(/at least ([\d.]+) s\b/, "the minimum hold")[1]), CAPTION_DEFAULTS.minHoldSec, `${where}: minimum hold`);
  }
  // the skill's one canvas sentence holds for every template
  const canvas = /Every template is (\d+) x (\d+) at (\d+) fps and takes a `(\w+)` variable/.exec(readRepo("skill", "hyperframes-compose", "SKILL.md"));
  assert.ok(canvas, "the skill states the canvas every template shares");
  for (const name of listTemplates(TEMPLATES_DIR)) {
    const m = manifestOf(name);
    assert.deepEqual([m.width, m.height, m.fps, m.duration_variable], [Number(canvas[1]), Number(canvas[2]), Number(canvas[3]), canvas[4]], `${name}: the skill's canvas sentence`);
  }
});

// The template list is hand-written in six more places than the tool description (which a Go test pins). Each
// is read as the sentence it is, both ways: every shipped template is named, and nothing else is.
test("the shipped template list in the CLI help, the README, the operator guide, the glossary and the doctor line is complete and names nothing else", () => {
  const shipped = listTemplates(TEMPLATES_DIR).sort();
  const ticked = (s) => [...s.matchAll(/`([^`]+)`/g)].map((m) => m[1]);
  const lists = {
    "the --template help (main.go)": /"a vetted template on this machine \(([^)]*)\)"/.exec(readRepo("main.go")),
    "the README tool row": /a vetted template \(([^)]*)\)/.exec(readRepo("README.md")),
    "the operator guide": /The vetted templates are ([^.]*)\./.exec(readRepo("docs", "OPERATOR-GUIDE.md")),
    "the glossary entry": /`render\/compose-templates\/`: ([^.]*)\./.exec(readRepo("docs", "glossary.md")),
    "the media doc's shipped list": /\(shipped: ([^)]*)\)/.exec(readRepo("docs", "systems", "media-generation.md")),
    "the setup doctor line": /templates=([a-z0-9,-]+)/.exec(readRepo("setup", "SETUP-AGENT.md")),
  };
  for (const [where, m] of Object.entries(lists)) {
    assert.ok(m, `${where}: the sentence that lists the templates was not found`);
    const listed = (where === "the --template help (main.go)" ? m[1].split(",").map((s) => s.trim()) : where === "the setup doctor line" ? m[1].split(",") : ticked(m[1]));
    assert.deepEqual([...listed].sort(), shipped, `${where} must list exactly the shipped templates`);
  }
  assert.deepEqual(lists["the setup doctor line"][1].split(","), shipped, "the doctor prints them sorted");
});

test("kit ports: each text slot sits where the card reads it, so no two variables are wired to each other's place", () => {
  // Reading order, top to bottom. Declaration order is what a caller sees and is free to differ; this is the layout.
  const READING = {
    "stat-card": ["kicker", "stat", "label_pre", "label_em", "label_post", "source"],
    "section-title": ["kicker", "headline_pre", "headline_em", "headline_post"],
    "callout-label": ["term", "detail"],
    "checklist-card": ["title_pre", "title_em", "title_post", "item1", "item2", "item3", "item4"],
  };
  for (const [name, order] of Object.entries(READING)) {
    assert.deepEqual([...pageOf(name).matchAll(/data-var-text="([^"]+)"/g)].map((m) => m[1]), order, `${name}: text slots in reading order`);
  }
});

test("templates: every font weight a page asks for has a matching @font-face, so no face is synthesised or fetched", () => {
  for (const name of listTemplates(TEMPLATES_DIR)) {
    const html = pageOf(name);
    const declared = new Set([...html.matchAll(/@font-face\s*\{([^}]*)\}/g)].map((m) => Number((/font-weight:\s*(\d+)/.exec(m[1]) || [])[1])));
    assert.ok(declared.size > 0 && ![...declared].some(Number.isNaN), `${name}: every @font-face states a numeric weight`);
    const css = html.replace(/@font-face\s*\{[^}]*\}/g, "");
    const used = new Set();
    for (const m of css.matchAll(/font-weight:\s*([a-z0-9]+)/gi)) {
      const v = m[1].toLowerCase();
      used.add(v === "normal" ? 400 : v === "bold" ? 700 : /^\d+$/.test(v) ? Number(v) : v);
    }
    for (const u of used) {
      assert.equal(typeof u, "number", `${name}: font-weight ${u} is relative; use a number so it can be matched to a face`);
      assert.ok(declared.has(u), `${name}: font-weight ${u} is used but no @font-face declares it (declared: ${[...declared]})`);
    }
    assert.ok(!/(^|[;{\s])font:\s*[^;}]*\d/.test(css), `${name}: the font shorthand hides a weight from this check; use font-family and font-weight`);
  }
});

test("templates: the exit is as long as the time it is held back by, the --duration fallbacks are the declared default, and the shortest clip outlasts the exit", () => {
  for (const name of listTemplates(TEMPLATES_DIR)) {
    const html = pageOf(name);
    const dur = declaredVariables(html).find((d) => d.id === manifestOf(name).duration_variable);
    for (const m of html.matchAll(/var\(--duration,\s*([\d.]+)\)/g)) assert.equal(Number(m[1]), dur.default, `${name}: var(--duration, ${m[1]}) against the declared default ${dur.default}`);
    // animation: out <D>s <easing> calc(var(--duration, N) * 1s - <D>s) forwards
    const exits = [...html.matchAll(/animation:\s*out\s+([\d.]+)s\s+[^;]*?calc\(var\(--duration,\s*[\d.]+\)\s*\*\s*1s\s*-\s*([\d.]+)s\)/g)];
    if (/animation:\s*out\b/.test(html)) assert.ok(exits.length > 0, `${name}: the exit animation is not in the form this check reads`);
    for (const m of exits) {
      assert.equal(m[1], m[2], `${name}: the exit lasts ${m[1]} s but starts ${m[2]} s before the end`);
      assert.ok(dur.min > Number(m[1]), `${name}: the shortest clip (${dur.min} s) is not longer than its ${m[1]} s exit`);
    }
    // the exit really fades out: its last stop is fully transparent
    const keyframes = /@keyframes\s+out\s*\{((?:[^{}]|\{[^{}]*\})*)\}/.exec(html);
    if (keyframes) assert.match(/\bto\s*\{([^}]*)\}/.exec(keyframes[1])[1], /\bopacity:\s*0\s*(;|$)/, `${name}: the exit keyframes must end at opacity 0`);
    // a breathing loop: max(var(--duration, N) - X, floor) * 1s / 2, then a delay of X s, twice
    for (const m of html.matchAll(/calc\(max\(var\(--duration,\s*[\d.]+\)\s*-\s*([\d.]+),\s*[\d.]+\)\s*\*\s*1s\s*\/\s*2\)\s*[a-z-]+\s+([\d.]+)s\s+2\b/g)) {
      assert.equal(m[1], m[2], `${name}: a breathing loop starts at ${m[2]} s but takes ${m[1]} s from the duration`);
    }
  }
});

test("kit ports: every string variable round-trips apostrophes, ampersands, angle brackets and quotes through the declaration", () => {
  // The two characters the single-quoted attribute depends on escaping are the two speech carries most: ' and &.
  const sample = `don't R&D & co <b>&amp;</b> "q" it's `;
  for (const name of KIT_TEMPLATE_NAMES) {
    const html = pageOf(name);
    const manifest = manifestOf(name);
    for (const d of declaredVariables(html).filter((x) => x.type === "string")) {
      const value = d.id === "words_json" ? JSON.stringify([[0, 2, sample.trim()], [2.5, 4, "R&D's <i>"]]) : sample.repeat(Math.ceil(d.maxLength / sample.length)).slice(0, d.maxLength);
      const out = applyTemplateVariables(html, { [d.id]: value }, manifest);
      assert.equal(declaredDefault(out, d.id), value, `${name}.${d.id}`);
      assert.deepEqual(rootAttributeNames(out), rootAttributeNames(html), `${name}.${d.id}: the root's attributes`);
    }
  }
});

// Angle brackets and quotes are legal inside a single-quoted attribute, so a browser would read a raw one fine. The
// runner's own tag scanning is a regex ([^>]*), and so is much of HyperFrames' HTML handling: a raw ">" inside the
// declaration ends the root tag early, and the data-duration the runner appends would land INSIDE the attribute.
test("applyTemplateVariables: the declaration holds no raw angle bracket or quote, so the root tag cannot end inside it", () => {
  for (const value of ["a > b", "a < b", "<b>bold</b>", "x' y", "></div><script>", "R&D > 3"]) {
    const out = applyTemplateVariables(TEMPLATE_HTML, { title: value });
    const raw = /data-composition-variables='([^']*)'/.exec(out)[1];
    assert.ok(!/[<>']/.test(raw), `the attribute carries a raw bracket or quote for ${JSON.stringify(value)}: ${raw}`);
    assert.equal(declaredDefault(out, "title"), value);
  }
  // a root with no data-duration of its own gets one appended: after the tag's last attribute, never inside a value
  const bare = `<!doctype html><html><body><div id="root" data-composition-id="x" data-width="1920" data-height="1080" data-fps="30" data-composition-variables='[{"id":"title","type":"string","label":"T","maxLength":40,"default":"hi"},{"id":"duration","type":"number","label":"D","min":1,"max":10,"default":5}]'></div></body></html>`;
  const out = applyTemplateVariables(bare, { title: "a > b", duration: 7 }, { duration_variable: "duration" });
  assert.equal(compositionMeta(out).duration, 7);
  assert.equal(declaredDefault(out, "title"), "a > b");
  assert.deepEqual(rootAttributeNames(out), ["id", "data-composition-id", "data-width", "data-height", "data-fps", "data-composition-variables", "data-duration"]);
});

test("hyperframes-compose skill: the allowed and the banned subcommands are exactly the runner's, each refused before any spawn", () => {
  const bans = sectionOf(readRepo("skill", "hyperframes-compose", "SKILL.md"), "Hard bans").replace(/\n\s*/g, " ");
  const ticks = (s) => [...s.matchAll(/`([^`]+)`/g)].map((m) => m[1]);
  const allowed = /refuses everything except ([^.]*)\./.exec(bans);
  assert.ok(allowed, "the first ban says what the runner refuses everything except");
  assert.deepEqual(ticks(allowed[1]), ALLOWED_SUBCOMMANDS.map((s) => (s === "browser" ? `browser ${ALLOWED_BROWSER_SUBCOMMANDS.join("|")}` : s)));
  // the explicit, complete list of what is banned by name (a change here is deliberate friction)
  const BANNED = ["init", "skills", "upgrade", "add", "capture", "preview", "tts", "transcribe", "publish", "cloud", "lambda", "cloudrun"];
  const never = /\*\*Never ((?:`[^`]+`(?:, | or )?)+)\.\*\*/.exec(bans);
  assert.ok(never, "a bold 'Never ...' sentence lists the banned subcommands");
  assert.deepEqual(ticks(never[1]), BANNED);
  for (const sub of BANNED) assert.throws(() => assertAllowedInvocation([sub, "--json"]), (e) => e.cls === "BAD_INPUT" && /not allowlisted/.test(e.detail), sub);
  assert.ok(bans.includes("`npx hyperframes`") && bans.includes("`npx skills add`"), "the two commands never to run are named");
});

test("licence hygiene: the retained texts stay LF by rule, and the provenance record pins one commit everywhere", () => {
  assert.match(readRepo(".gitattributes"), /^render\/compose-templates\/_third_party\/\*\* +text +eol=lf$/m, "the rule that keeps the retained licence texts byte-identical on every checkout");
  const prov = readFileSync(join(TEMPLATES_DIR, "_third_party", "hyperframes-student-kit", "PROVENANCE.md"), "utf8").replace(/\r\n/g, "\n");
  const commits = [...prov.matchAll(/\b[0-9a-f]{40}\b/g)].map((m) => m[0]);
  assert.ok(commits.length >= 2, "the table and the re-check steps both name the commit");
  assert.deepEqual([...new Set(commits)], ["0d30152a82b9ceb93cfdd9bdbf46f0d5ab3cde86"], "every mention is the same full commit");
  assert.ok(commits[0].startsWith(KIT_COMMIT), "and it is the short commit every README names");
  // the four source-card digests can only be checked against upstream (PROVENANCE.md says how); offline they are at least well formed and distinct
  const digests = [...prov.matchAll(/`([0-9a-f]{64})`/g)].map((m) => m[1]);
  assert.equal(digests.length, Object.keys(KIT_PORTS).length, "one digest per adapted card");
  assert.equal(new Set(digests).size, digests.length, "no two cards share a digest");
});

// The kit's brand colours, as the RGB triples they are, so a different spelling of the same colour is caught:
// #37bdf8, #f5d82a, #ff3b30 and #ff8a5c, written as hex (3, 4, 6 or 8 digits) or as rgb()/rgba() with commas or spaces.
const KIT_BRAND_RGB = [[55, 189, 248], [245, 216, 42], [255, 59, 48], [255, 138, 92]];
function colorTriples(text) {
  const out = [];
  for (const m of text.matchAll(/#([0-9a-f]{3,8})\b/gi)) {
    let h = m[1];
    if (h.length === 3 || h.length === 4) h = [...h].map((c) => c + c).join("");
    if (h.length === 6 || h.length === 8) out.push([0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16)));
  }
  for (const m of text.matchAll(/rgba?\(\s*(\d{1,3})[\s,]+(\d{1,3})[\s,]+(\d{1,3})/gi)) out.push([m[1], m[2], m[3]].map(Number));
  return out;
}

test("kit ports: the kit's brand colours are absent however they are spelled", () => {
  // the parser reads every spelling: upper-case hex, three-digit hex (expanded), eight-digit hex (alpha dropped), rgb() with spaces, rgba() with commas
  assert.deepEqual(
    colorTriples("a #37BDF8 b rgb(55 189 248) c rgba(55, 189, 248, .5) d #3bf e #37bdf8cc"),
    [[55, 189, 248], [51, 187, 255], [55, 189, 248], [55, 189, 248], [55, 189, 248]],
  );
  assert.deepEqual(colorTriples("rgb(55 189 248)"), [[55, 189, 248]]);
  assert.deepEqual(colorTriples("#f5d82a"), [[245, 216, 42]]);
  for (const name of KIT_TEMPLATE_NAMES) {
    for (const { file, text } of templateFiles(name)) {
      for (const t of colorTriples(text)) assert.ok(!KIT_BRAND_RGB.some((b) => b.every((v, i) => v === t[i])), `${name}/${file}: the colour rgb(${t.join(" ")}) is one of the kit's brand colours`);
    }
  }
});

// moveInto publishes the rendered output at its delivery path. The work dir is usually on another
// volume, so the copy fallback is the common path, and it used to copy straight onto the destination:
// a destination that filled up kept a partial video there that looked finished.
function publishDirs() {
  const root = mkdtempSync(join(tmpdir(), "compose-move-"));
  const work = join(root, "work"); const out = join(root, "out");
  mkdirSync(work); mkdirSync(out);
  return { work, out, src: join(work, "output.mp4"), dst: join(out, "clip.mp4") };
}
const crossVolume = () => { throw Object.assign(new Error("EXDEV: cross-device link not permitted, rename"), { code: "EXDEV" }); };

test("moveInto: where the rename works the output moves and nothing is left behind", () => {
  const { src, dst, work, out } = publishDirs();
  writeFileSync(src, "the clip");
  moveInto(src, dst);
  assert.equal(readFileSync(dst, "utf8"), "the clip");
  assert.deepEqual(readdirSync(work), []);
  assert.deepEqual(readdirSync(out), ["clip.mp4"]);
});

test("moveInto: when the rename cannot be used the copy lands through a staged sibling, then the source goes", () => {
  const { src, dst, work, out } = publishDirs();
  writeFileSync(src, "the clip");
  writeFileSync(dst, "an older clip");
  moveInto(src, dst, { rename: crossVolume });
  assert.equal(readFileSync(dst, "utf8"), "the clip");
  assert.deepEqual(readdirSync(out), ["clip.mp4"], "no staged copy left beside the delivery path");
  assert.deepEqual(readdirSync(work), [], "the source is removed only after the copy landed");
});

test("moveInto: a destination that fills up during the fallback copy leaves no partial file, keeps the previous clip and keeps the source", () => {
  const { src, dst, work, out } = publishDirs();
  writeFileSync(src, "the clip");
  writeFileSync(dst, "an older clip");
  const fullDisk = (from, to) => copyAtomic(from, to, {
    copy: (_f, partial) => { writeFileSync(partial, "th"); throw Object.assign(new Error("ENOSPC: no space left on device, write"), { code: "ENOSPC" }); },
  });
  assert.throws(() => moveInto(src, dst, { rename: crossVolume, copy: fullDisk }), (e) => e.code === "ENOSPC");
  assert.equal(readFileSync(dst, "utf8"), "an older clip", "a half-copied clip must never replace the previous one");
  assert.deepEqual(readdirSync(out), ["clip.mp4"], "and nothing is left beside it");
  assert.equal(readFileSync(src, "utf8"), "the clip", "the rendered clip is still in the work dir");
  assert.deepEqual(readdirSync(work), ["output.mp4"]);
});

test("moveInto: a frames directory copies whole through a staged sibling", () => {
  const root = mkdtempSync(join(tmpdir(), "compose-move-"));
  const frames = join(root, "work", "frames"); const dst = join(root, "out", "frames");
  mkdirSync(frames, { recursive: true });
  writeFileSync(join(frames, "frame-000001.png"), "1");
  writeFileSync(join(frames, "frame-000002.png"), "2");
  moveInto(frames, dst, { rename: crossVolume });
  assert.deepEqual(readdirSync(dst).sort(), ["frame-000001.png", "frame-000002.png"]);
  assert.deepEqual(readdirSync(join(root, "out")), ["frames"]);
  assert.equal(existsSync(frames), false);
});
