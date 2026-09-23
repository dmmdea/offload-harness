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
import { fileURLToPath } from "node:url";
import {
  ALLOWED_SUBCOMMANDS, FORCED_ENV, PASSTHROUGH_ENV_KEYS, PINNED_VERSION, applyTemplateVariables, assertAllowedInvocation,
  buildCheckArgs, buildChildEnv, buildRenderArgs, buildSnapshotArgs, classifyFailure, compositionMeta,
  listTemplates, parseJsonDoc, rewriteRootDuration, summarizeProbe, verifyOutput,
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
appendFileSync(join(here, "calls.jsonl"), JSON.stringify({ argv, env: process.env, cwd: process.cwd(), cwdEntries: readdirSync(process.cwd()) }) + "\n");
const counter = (name) => { const f = join(here, name + ".count"); const n = existsSync(f) ? Number(readFileSync(f, "utf8")) + 1 : 1; writeFileSync(f, String(n)); return n; };
const sleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const flag = (n) => { const i = argv.indexOf(n); return i >= 0 ? argv[i + 1] : undefined; };
const sub = argv[0];
if (sub === "--version") { console.log(b.version || "0.8.61"); process.exit(0); }
if (sub === "browser") {
  if (argv[1] === "ensure") { console.log("Path: " + b.browserPath); process.exit(0); }
  console.log(b.browserPath); process.exit(0);
}
if (sub === "lint") {
  const l = b.lint || {};
  console.log(JSON.stringify({ ok: !l.errorCount, errorCount: l.errorCount || 0, warningCount: l.warningCount || 0, infoCount: 0, findings: l.findings || [], filesScanned: 1, _meta: { version: "0.8.61" } }, null, 2));
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
  writeFileSync(join(bin, "behavior.json"), JSON.stringify({ browserPath, ...behavior }));
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
  assert.equal(r.last.version, "0.8.61");
  assert.equal(r.last.matches_pin, true);
  r = fx.run(["browser", "--hyperframes-dir", fx.hf]);
  assert.equal(r.status, 0, r.stdout);
  assert.equal(r.last.browser_path, fx.browserPath);
  const calls = fx.calls();
  assert.deepEqual(calls.map((c) => c.argv.slice(0, 2).join(" ")), ["--version --json", "browser ensure", "browser path"]);
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
    "sha512-OZeccCLZe7ZsfaAgfLB72mYCqQRQAKv5/WzHZgZRFHgWJggA1+5CxId9o5KsfSccJPrLtPTBAlDwi1e8Q/FPmw==",
    "the lock carries the registry integrity verified for 0.8.61 — a bump must re-verify it");
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
