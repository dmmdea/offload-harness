#!/usr/bin/env node
// render/compose-hyperframes.mjs — the ONLY door between the harness and the HyperFrames CLI
// (offload_compose_video / `compose-video` / fleet task compose-video; ADR 0059).
//
// HyperFrames (npm `hyperframes`, HeyGen, Apache-2.0) renders an HTML/CSS composition to video
// with headless Chrome + FFmpeg. Its defaults do not fit this harness, and every guard below is
// tied to a behaviour read in the pinned source (0.8.61):
//
//   - it phones home (PostHog telemetry on by default)            -> HYPERFRAMES_NO_TELEMETRY=1, DO_NOT_TRACK=1
//   - every command without --json pings the npm registry and GitHub (update + skills checks),
//     and a GLOBAL install self-upgrades in a detached process    -> --json on EVERY invocation, a pinned
//                                                                    project-local install, NO_UPDATE_CHECK/
//                                                                    NO_AUTO_INSTALL=1 (value must be exactly "1")
//   - `init` / `skills update` install agent skills into ~/.claude and every other agent dir
//                                                                 -> subcommand allowlist + HYPERFRAMES_SKIP_SKILLS=1
//                                                                    + a harness-owned HOME
//   - it auto-loads ./.env, `capture` prefers OPENROUTER_API_KEY and `snapshot --describe` calls
//     Gemini when a key is present                                -> an ALLOWLISTED child env (no cloud key can
//                                                                    reach it), cwd = a fresh empty work dir,
//                                                                    snapshot always --describe false
//   - Chrome launches with --no-sandbox and site isolation off     -> compositions are TRUSTED code; the fleet
//                                                                    door renders only vetted templates
//   - browser GPU defaults to "auto" (the display card)            -> --no-browser-gpu + PRODUCER_BROWSER_GPU_MODE
//                                                                    =software: CPU-class, no GPU lease (ADR 0026)
//   - Windows antivirus locks fail ffmpeg spawns with EBUSY (#4058) -> retry a step ONCE on EBUSY
//
// Pipeline for `render`: lint --json (errorCount must be 0) -> check --json --no-browser-gpu (exit 0)
// -> render --batch <rows.json> --json (one row) -> ffprobe gate -> optional snapshot frames.
// Every failure is ONE typed line on stdout: `COMPOSE-FAIL: <CLASS>: <detail>`; the last stdout line
// is always one JSON result object, also written to --result (the file the Go side reads).
//
// Dependency-free on purpose (node: builtins only): CI runs its tests with no HyperFrames installed.
import { spawn } from "node:child_process";
import {
  cpSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, renameSync, rmSync, statSync,
  writeFileSync,
} from "node:fs";
import { basename, delimiter, dirname, extname, isAbsolute, join, resolve, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { tmpdir } from "node:os";

const __dirname = dirname(fileURLToPath(import.meta.url));

export const PINNED_VERSION = "0.8.61";
export const CLI_ENTRY = join("node_modules", "hyperframes", "bin", "hyperframes.mjs");
export const DEFAULT_TEMPLATES_DIR = join(__dirname, "compose-templates");

// ---------------------------------------------------------------------------------------------
// The child environment. ONLY these keys pass through from the parent; everything else (every
// *_API_KEY / *_TOKEN, NODE_OPTIONS, GPU_LEASE_*, proxies, CI flags) is dropped by construction.
// ---------------------------------------------------------------------------------------------
export const PASSTHROUGH_ENV_KEYS = Object.freeze([
  "PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "HOME",
  "LOCALAPPDATA", "APPDATA", "XDG_CACHE_HOME",
]);

// Fixed values the harness always sets. Update/auto-install gates compare against the exact
// string "1" (autoUpdate.ts / updateCheck.ts); the telemetry gate accepts 1/true/yes/on.
export const FORCED_ENV = Object.freeze({
  HYPERFRAMES_NO_TELEMETRY: "1",
  DO_NOT_TRACK: "1",
  HYPERFRAMES_NO_UPDATE_CHECK: "1",
  HYPERFRAMES_NO_AUTO_INSTALL: "1",
  HYPERFRAMES_SKIP_SKILLS: "1",
  PRODUCER_BROWSER_GPU_MODE: "software",
});

// buildChildEnv builds the COMPLETE environment of a HyperFrames child. Windows env names are
// case-insensitive ("Path", "SystemRoot", "windir"), so matching folds case there and emits the
// canonical upper-case name once; POSIX names are case-sensitive and match exactly.
//   opts.ffmpeg / opts.ffprobe  absolute binaries (HYPERFRAMES_FFMPEG_PATH / _FFPROBE_PATH) — pinned so
//                               an ffmpeg.exe planted in the cwd can never win (ffBinaries.ts scans cwd first)
//   opts.browser                pinned chrome-headless-shell (HYPERFRAMES_BROWSER_PATH)
//   opts.extractCacheDir        HYPERFRAMES_EXTRACT_CACHE_DIR (#4060: frames cache off the system drive)
//   opts.tempDir                TEMP/TMP override (Windows renders stage every frame under os.tmpdir())
//   opts.home                   harness-owned HOME: HyperFrames' own state (~/.hyperframes, ~/.cache/hyperframes)
//                               lands there, and nothing it could ever write reaches the operator's ~/.claude
export function buildChildEnv(parentEnv, opts = {}, platform = process.platform) {
  const env = {};
  const win = platform === "win32";
  for (const [k, v] of Object.entries(parentEnv || {})) {
    if (v === undefined || v === null) continue;
    const name = win ? k.toUpperCase() : k;
    if (!PASSTHROUGH_ENV_KEYS.includes(name)) continue;
    if (Object.hasOwn(env, name)) continue; // first spelling wins; never two spellings of one name
    env[name] = String(v);
  }
  if (opts.home) {
    env.HOME = opts.home;
    if (win) {
      env.USERPROFILE = opts.home;
      const m = /^([A-Za-z]:)(.*)$/.exec(opts.home);
      if (m) {
        env.HOMEDRIVE = m[1];
        env.HOMEPATH = m[2] || "\\";
      } else {
        delete env.HOMEDRIVE;
        delete env.HOMEPATH;
      }
    }
  }
  if (opts.tempDir) {
    env.TEMP = opts.tempDir;
    env.TMP = opts.tempDir;
  }
  Object.assign(env, FORCED_ENV);
  if (opts.ffmpeg) env.HYPERFRAMES_FFMPEG_PATH = opts.ffmpeg;
  if (opts.ffprobe) env.HYPERFRAMES_FFPROBE_PATH = opts.ffprobe;
  if (opts.browser) env.HYPERFRAMES_BROWSER_PATH = opts.browser;
  if (opts.extractCacheDir) env.HYPERFRAMES_EXTRACT_CACHE_DIR = opts.extractCacheDir;
  return env;
}

// ---------------------------------------------------------------------------------------------
// The subcommand allowlist. Anything else HyperFrames offers (init, skills, cloud, lambda, cloudrun,
// capture, upgrade, publish, tts, transcribe, ...) is refused BEFORE a process is spawned.
// ---------------------------------------------------------------------------------------------
export const ALLOWED_SUBCOMMANDS = Object.freeze(["lint", "check", "render", "snapshot", "browser", "--version"]);
export const ALLOWED_BROWSER_SUBCOMMANDS = Object.freeze(["ensure", "path"]);

export class ComposeError extends Error {
  constructor(cls, detail) {
    super(`${cls}: ${detail}`);
    this.cls = cls;
    this.detail = detail;
  }
}

// assertAllowedInvocation is the last check before a spawn: an allowlisted subcommand, --json
// present (it is what skips the npm + GitHub network block in cli.ts), no browser GPU, and a
// snapshot that can never call Gemini. It throws; it never repairs an argv.
export function assertAllowedInvocation(args) {
  if (!Array.isArray(args) || args.length === 0) throw new ComposeError("BAD_INPUT", "empty HyperFrames invocation refused");
  const sub = args[0];
  if (!ALLOWED_SUBCOMMANDS.includes(sub)) {
    throw new ComposeError("BAD_INPUT", `HyperFrames subcommand "${sub}" is not allowlisted (allowed: ${ALLOWED_SUBCOMMANDS.join(", ")})`);
  }
  if (sub === "browser" && !ALLOWED_BROWSER_SUBCOMMANDS.includes(args[1])) {
    throw new ComposeError("BAD_INPUT", `HyperFrames "browser ${args[1] ?? ""}" is not allowlisted (allowed: ensure, path)`);
  }
  if (!args.includes("--json")) {
    throw new ComposeError("BAD_INPUT", `HyperFrames "${sub}" without --json refused (--json is what skips the npm/GitHub update checks)`);
  }
  for (const a of args) {
    if (a === "--browser-gpu" || a === "--gpu" || a === "--docker" || a.startsWith("--browser-gpu=")) {
      throw new ComposeError("BAD_INPUT", `HyperFrames flag ${a} refused: the compose lane is CPU-class (software GL, CPU encode, no GPU lease)`);
    }
  }
  if (sub === "snapshot") {
    const i = args.indexOf("--describe");
    if (i < 0 || args[i + 1] !== "false") {
      throw new ComposeError("BAD_INPUT", "HyperFrames snapshot without --describe false refused (describe calls Gemini)");
    }
  }
  return true;
}

// ---------------------------------------------------------------------------------------------
// Argument builders — one per step, each verified against the 0.8.61 source (render.ts:130-409,
// check.ts CHECK_COMMAND_ARGS, lint.ts, snapshot.ts:656-724, browser.ts).
// ---------------------------------------------------------------------------------------------
export const FORMATS = Object.freeze(["mp4", "webm", "mov", "png-sequence", "gif"]);
export const QUALITIES = Object.freeze(["draft", "standard", "high"]);
export const RESOLUTIONS = Object.freeze(["landscape", "portrait", "landscape-4k", "portrait-4k", "square", "square-4k"]);
export const RESOLUTION_DIMS = Object.freeze({
  "landscape": [1920, 1080], "portrait": [1080, 1920], "landscape-4k": [3840, 2160],
  "portrait-4k": [2160, 3840], "square": [1080, 1080], "square-4k": [2160, 2160],
});
export const ALPHA_FORMATS = Object.freeze(["webm", "mov", "png-sequence"]);
export const FORMAT_EXT = Object.freeze({ "mp4": ".mp4", "webm": ".webm", "mov": ".mov", "gif": ".gif", "png-sequence": "" });
export const MAX_WORKERS = 24; // render.ts caps an explicit --workers at 24 (parallelCoordinator.ts:156)

export function buildLintArgs(projectDir) {
  return ["lint", projectDir, "--json"];
}

export function buildCheckArgs(projectDir) {
  return ["check", projectDir, "--json", "--no-browser-gpu"];
}

export function buildVersionArgs() {
  return ["--version", "--json"];
}

export function buildBrowserArgs(sub) {
  return ["browser", sub, "--json"];
}

// buildRenderArgs: one batch row renders exactly one output; --json only produces the manifest
// document WITH --batch (render.ts "json": "With --batch, emit exactly one final JSON result document").
export function buildRenderArgs(o) {
  const args = ["render", o.projectDir, "--batch", o.rowsFile, "--json", "-o", o.output,
    "--format", o.format, "--quality", o.quality, "--workers", String(o.workers ?? "auto"),
    "--no-browser-gpu", "--no-best-effort"];
  if (o.strict) args.push("--strict", "--strict-variables");
  if (o.fps) args.push("--fps", String(o.fps));
  if (o.resolution) args.push("--resolution", o.resolution);
  if (o.composition) args.push("--composition", o.composition);
  return args;
}

export function buildSnapshotArgs(o) {
  const args = ["snapshot", o.projectDir, "--at", o.at.map((t) => String(t)).join(","), "--no-end",
    "--describe", "false", "--json", "--no-browser-gpu", "-o", o.outDir];
  return args;
}

// ---------------------------------------------------------------------------------------------
// Parsing helpers
// ---------------------------------------------------------------------------------------------

// parseJsonDoc pulls the JSON document out of a CLI's stdout. lint/check print one PRETTY document
// (multi-line), render --batch --json prints one single-line document last; anything else on stdout
// (a stray log line) is skipped.
export function parseJsonDoc(stdout) {
  const text = String(stdout || "").trim();
  if (!text) return null;
  try { return JSON.parse(text); } catch { /* fall through */ }
  const lines = text.split(/\r?\n/);
  for (let i = lines.length - 1; i >= 0; i--) {
    const ln = lines[i].trim();
    if (ln.startsWith("{") && ln.endsWith("}")) {
      try { return JSON.parse(ln); } catch { /* keep looking */ }
    }
  }
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i].startsWith("{")) {
      try { return JSON.parse(lines.slice(i).join("\n")); } catch { /* keep looking */ }
    }
  }
  return null;
}

export function parseCli(argv) {
  const pos = [];
  const flags = {};
  const booleans = new Set(["keep-work", "no-strict", "strict"]);
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith("--")) {
      const eq = a.indexOf("=");
      if (eq > 0) { flags[a.slice(2, eq)] = a.slice(eq + 1); continue; }
      const key = a.slice(2);
      if (booleans.has(key)) { flags[key] = true; continue; }
      const v = argv[i + 1];
      if (v === undefined) throw new ComposeError("BAD_INPUT", `flag --${key} needs a value`);
      flags[key] = v;
      i++;
    } else {
      pos.push(a);
    }
  }
  return { op: pos[0] || "render", pos: pos.slice(1), flags };
}

// classifyFailure maps a CLI step's failure text onto the typed classes. EBUSY is checked first:
// it is transient (antivirus holding ffmpeg.exe, #4058) and is the only class that is retried.
export function classifyFailure(step, text) {
  const s = String(text || "");
  if (/\bEBUSY\b/.test(s)) return "SPAWN_EBUSY";
  if (/Disk capture may need|free up disk space|\bENOSPC\b|not enough (?:free )?(?:disk )?space/i.test(s)) return "DISK_HEADROOM";
  if (/ffmpeg|ffprobe/i.test(s) && /not found|ENOENT|could not find|no such file/i.test(s)) return "FFMPEG_MISSING";
  if (/Failed to launch the browser|Browser was not found|Could not find (?:Chrome|chromium|expected browser)|chrome-headless-shell[^\n]*(?:not found|missing)|HYPERFRAMES_BROWSER_PATH/i.test(s)) return "BROWSER_MISSING";
  if (/Variable validation failed|undeclared|type-mismatch|Invalid (?:fps|quality|format|resolution|workers)|Conflicting flags|Invalid batch/i.test(s)) return "BAD_INPUT";
  if (step === "lint") return "LINT_ERRORS";
  if (step === "check") return "CHECK_FAILED";
  return "RENDER_FAILED";
}

// resolveBinary resolves an executable the way the harness config names it: an absolute (or
// relative-with-separator) path must exist; a bare name is searched on PATH (with .exe on Windows).
export function resolveBinary(value, env = process.env, platform = process.platform) {
  if (!value) return "";
  if (isAbsolute(value) || value.includes("/") || value.includes("\\")) {
    const p = resolve(value);
    return existsSync(p) && statSync(p).isFile() ? p : "";
  }
  const pathVar = env.PATH ?? env.Path ?? "";
  const exts = platform === "win32" ? [".exe", ".cmd", ""] : [""];
  for (const dir of String(pathVar).split(platform === "win32" ? ";" : delimiter)) {
    if (!dir) continue;
    for (const ext of exts) {
      const cand = join(dir, value.toLowerCase().endsWith(ext) ? value : value + ext);
      try { if (statSync(cand).isFile()) return cand; } catch { /* next */ }
    }
  }
  return "";
}

// siblingFfprobe derives ffprobe from the configured ffmpeg: same directory, same extension.
export function siblingFfprobe(ffmpegPath) {
  if (!ffmpegPath) return "";
  const dir = dirname(ffmpegPath);
  const ext = extname(ffmpegPath);
  const cand = join(dir, "ffprobe" + (ext.toLowerCase() === ".exe" ? ".exe" : ext));
  return existsSync(cand) ? cand : "";
}

function numberOr(v, def) {
  const n = Number(v);
  return Number.isFinite(n) ? n : def;
}

// ---------------------------------------------------------------------------------------------
// Input validation (BAD_INPUT) — the same rules the Go side enforces, repeated here because the
// runner is also a door (acceptance, the installer and the smoke call it directly).
// ---------------------------------------------------------------------------------------------
export const TEMPLATE_NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

export function validateRequest(f) {
  const inputs = ["project-dir", "html-file", "template"].filter((k) => f[k]);
  if (inputs.length !== 1) {
    throw new ComposeError("BAD_INPUT", `exactly one of project_dir, html or template is required (got ${inputs.length ? inputs.join(" + ") : "none"})`);
  }
  if (!f.out) throw new ComposeError("BAD_INPUT", "--out is required");
  const format = f.format || "mp4";
  if (!FORMATS.includes(format)) throw new ComposeError("BAD_INPUT", `format "${format}" is not one of ${FORMATS.join(", ")}`);
  const quality = f.quality || "high";
  if (!QUALITIES.includes(quality)) throw new ComposeError("BAD_INPUT", `quality "${quality}" is not one of ${QUALITIES.join(", ")}`);
  let fps = 0;
  if (f.fps !== undefined && f.fps !== "" && f.fps !== "0") {
    fps = Number(f.fps);
    if (!Number.isInteger(fps) || fps < 1 || fps > 240) throw new ComposeError("BAD_INPUT", `fps "${f.fps}" must be an integer 1-240`);
  }
  if (f.resolution && !RESOLUTIONS.includes(f.resolution)) {
    throw new ComposeError("BAD_INPUT", `resolution "${f.resolution}" is not one of ${RESOLUTIONS.join(", ")}`);
  }
  let workers = "auto";
  if (f.workers !== undefined && f.workers !== "" && f.workers !== "auto") {
    const w = Number(f.workers);
    if (!Number.isInteger(w) || w < 1 || w > MAX_WORKERS) throw new ComposeError("BAD_INPUT", `workers "${f.workers}" must be "auto" or an integer 1-${MAX_WORKERS}`);
    workers = String(w);
  }
  let snapshots = [];
  if (f.snapshots) {
    snapshots = String(f.snapshots).split(",").map((s) => s.trim()).filter(Boolean).map(Number);
    if (snapshots.some((t) => !Number.isFinite(t) || t < 0) || snapshots.length > 16) {
      throw new ComposeError("BAD_INPUT", `snapshots "${f.snapshots}" must be up to 16 non-negative seconds`);
    }
  }
  if (f.composition) {
    const c = String(f.composition);
    if (isAbsolute(c) || c.split(/[\\/]/).includes("..")) {
      throw new ComposeError("BAD_INPUT", `composition "${c}" must be a relative path inside the project`);
    }
  }
  if (f.template && !TEMPLATE_NAME_RE.test(f.template)) {
    throw new ComposeError("BAD_INPUT", `template name "${f.template}" is invalid (lower-case letters, digits and dashes)`);
  }
  const timeoutSec = numberOr(f["timeout-sec"], 1800);
  if (!(timeoutSec > 0)) throw new ComposeError("BAD_INPUT", "--timeout-sec must be positive");
  return { format, quality, fps, workers, snapshots, timeoutSec, strict: !f["no-strict"] && f.strict !== "false" };
}

export function readVariables(file) {
  if (!file) return {};
  let v;
  try { v = JSON.parse(readFileSync(file, "utf8")); } catch (e) {
    throw new ComposeError("BAD_INPUT", `variables file is not JSON: ${e.message}`);
  }
  if (v === null || typeof v !== "object" || Array.isArray(v)) throw new ComposeError("BAD_INPUT", "variables must be a JSON object");
  return v;
}

// ---------------------------------------------------------------------------------------------
// Vetted templates. A template is a directory under render/compose-templates/<name>/ holding an
// index.html whose root declares data-composition-variables='[...]' and a template.json manifest.
// The caller's variables are merged into the DECLARED DEFAULTS of the copied source, so lint and
// check judge the caller's real text and colors (contrast, overflow) — not the placeholder copy.
// A declared "duration" variable also rewrites the root's data-duration: HyperFrames reads the
// root duration from SOURCE, never from a variable (docs/concepts/variables.mdx "What can't be a
// variable"), so this source edit is the supported way to make duration a template parameter.
// ---------------------------------------------------------------------------------------------
const VARS_ATTR_RE = /data-composition-variables='([^']*)'/;

function htmlAttrUnescape(s) {
  return s.replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
}

function htmlAttrEscape(s) {
  return s.replace(/&/g, "&amp;").replace(/'/g, "&#39;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

const COLOR_RE = /^(#[0-9a-fA-F]{3,8}|rgba?\([0-9.,%\s]+\)|hsla?\([0-9.,%\sdeg]+\)|[a-zA-Z]{3,20})$/;

export function applyTemplateVariables(html, variables, manifest = {}) {
  const m = VARS_ATTR_RE.exec(html);
  if (!m) throw new ComposeError("BAD_INPUT", "template declares no data-composition-variables");
  let decls;
  try { decls = JSON.parse(htmlAttrUnescape(m[1])); } catch (e) {
    throw new ComposeError("BAD_INPUT", `template variable declaration is not JSON: ${e.message}`);
  }
  const byId = new Map(decls.map((d) => [d.id, d]));
  for (const [id, value] of Object.entries(variables || {})) {
    const d = byId.get(id);
    if (!d) throw new ComposeError("BAD_INPUT", `variable "${id}" is not declared by this template (declared: ${[...byId.keys()].join(", ")})`);
    switch (d.type) {
      case "string":
        if (typeof value !== "string") throw new ComposeError("BAD_INPUT", `variable "${id}" must be a string`);
        if (value.length > (d.maxLength || 200)) throw new ComposeError("BAD_INPUT", `variable "${id}" is longer than ${d.maxLength || 200} characters`);
        break;
      case "color":
        if (typeof value !== "string" || !COLOR_RE.test(value.trim())) throw new ComposeError("BAD_INPUT", `variable "${id}" must be a CSS color (e.g. #1e293b)`);
        break;
      case "number": {
        if (typeof value !== "number" || !Number.isFinite(value)) throw new ComposeError("BAD_INPUT", `variable "${id}" must be a number`);
        if (d.min !== undefined && value < d.min) throw new ComposeError("BAD_INPUT", `variable "${id}" must be >= ${d.min}`);
        if (d.max !== undefined && value > d.max) throw new ComposeError("BAD_INPUT", `variable "${id}" must be <= ${d.max}`);
        break;
      }
      case "boolean":
        if (typeof value !== "boolean") throw new ComposeError("BAD_INPUT", `variable "${id}" must be a boolean`);
        break;
      case "enum":
        if (!Array.isArray(d.options) || !d.options.includes(value)) throw new ComposeError("BAD_INPUT", `variable "${id}" must be one of ${(d.options || []).join(", ")}`);
        break;
      default:
        throw new ComposeError("BAD_INPUT", `variable "${id}" has type "${d.type}", which templates do not accept from callers`);
    }
    d.default = value;
  }
  let out = html.replace(VARS_ATTR_RE, `data-composition-variables='${htmlAttrEscape(JSON.stringify(decls))}'`);
  const durVar = manifest.duration_variable;
  if (durVar && byId.has(durVar)) {
    const dur = byId.get(durVar).default;
    if (typeof dur !== "number" || !(dur > 0)) throw new ComposeError("BAD_INPUT", `template duration "${durVar}" must be a positive number`);
    out = rewriteRootDuration(out, dur);
  }
  return out;
}

// rewriteRootDuration sets data-duration on the ROOT composition element — the first tag carrying
// data-composition-id. Only that tag is touched; nested clips keep their own timing.
export function rewriteRootDuration(html, seconds) {
  const tagRe = /<[a-zA-Z][^>]*\bdata-composition-id=[^>]*>/;
  const m = tagRe.exec(html);
  if (!m) throw new ComposeError("BAD_INPUT", "template has no root element with data-composition-id");
  const tag = m[0];
  const value = String(Math.round(seconds * 1000) / 1000);
  const next = /\bdata-duration="[^"]*"/.test(tag)
    ? tag.replace(/\bdata-duration="[^"]*"/, `data-duration="${value}"`)
    : tag.replace(/>$/, ` data-duration="${value}">`);
  return html.slice(0, m.index) + next + html.slice(m.index + tag.length);
}

export function listTemplates(templatesDir = DEFAULT_TEMPLATES_DIR) {
  try {
    return readdirSync(templatesDir, { withFileTypes: true })
      .filter((d) => d.isDirectory() && TEMPLATE_NAME_RE.test(d.name) && existsSync(join(templatesDir, d.name, "index.html")))
      .map((d) => d.name).sort();
  } catch {
    return [];
  }
}

// materializeTemplate copies a vetted template (plus the shared font kit) into projectDir and
// applies the caller's variables to the copy. The template directory itself is never written.
export function materializeTemplate(templatesDir, name, variables, projectDir) {
  const src = join(templatesDir, name);
  if (!TEMPLATE_NAME_RE.test(name) || !existsSync(join(src, "index.html"))) {
    throw new ComposeError("BAD_INPUT", `unknown template "${name}" (available: ${listTemplates(templatesDir).join(", ") || "none"})`);
  }
  cpSync(src, projectDir, { recursive: true, filter: (p) => !/(^|[\\/])(README\.md|template\.json)$/.test(p) || p === src });
  const shared = join(templatesDir, "_shared");
  if (existsSync(shared)) cpSync(shared, join(projectDir, "shared"), { recursive: true });
  let manifest = {};
  const mf = join(src, "template.json");
  if (existsSync(mf)) {
    try { manifest = JSON.parse(readFileSync(mf, "utf8")); } catch (e) {
      throw new ComposeError("BAD_INPUT", `template.json of "${name}" is not JSON: ${e.message}`);
    }
  }
  const index = join(projectDir, "index.html");
  writeFileSync(index, applyTemplateVariables(readFileSync(index, "utf8"), variables, manifest));
  return manifest;
}

// compositionMeta reads the root's declared size/fps so the ffprobe gate knows what to expect.
export function compositionMeta(html) {
  const tag = (/<[a-zA-Z][^>]*\bdata-composition-id=[^>]*>/.exec(html) || [""])[0];
  const attr = (n) => {
    const m = new RegExp(`\\b${n}="([^"]*)"`).exec(tag);
    return m ? m[1] : "";
  };
  return {
    width: numberOr(attr("data-width"), 0),
    height: numberOr(attr("data-height"), 0),
    fps: numberOr(attr("data-fps"), 0),
    duration: numberOr(attr("data-duration"), 0),
    hasAudio: /<audio\b/i.test(html),
  };
}

// ---------------------------------------------------------------------------------------------
// ffprobe gate
// ---------------------------------------------------------------------------------------------
function parseRate(r) {
  if (!r || typeof r !== "string") return 0;
  const [n, d] = r.split("/").map(Number);
  if (!d) return numberOr(n, 0);
  return n / d;
}

// summarizeProbe folds ffprobe JSON into the payload fields. VP9 alpha is side data: the native
// decoder reports yuv420p, so the stream's alpha_mode tag (written by HyperFrames' encoder,
// chunkEncoder.ts "-metadata:s:v:0 alpha_mode=1") is the alpha signal for webm.
export function summarizeProbe(probe) {
  const streams = (probe && probe.streams) || [];
  const v = streams.find((s) => s.codec_type === "video") || {};
  const tags = Object.fromEntries(Object.entries(v.tags || {}).map(([k, val]) => [k.toLowerCase(), String(val)]));
  const pix = String(v.pix_fmt || "");
  const fps = parseRate(v.avg_frame_rate) || parseRate(v.r_frame_rate);
  const duration = numberOr(probe?.format?.duration, numberOr(v.duration, 0));
  let frames = Number.parseInt(v.nb_read_packets || v.nb_frames || "0", 10) || 0;
  if (!frames && duration && fps) frames = Math.round(duration * fps);
  return {
    codec: String(v.codec_name || ""),
    pix_fmt: pix,
    width: Number(v.width || 0),
    height: Number(v.height || 0),
    fps: Math.round(fps * 1000) / 1000,
    duration_sec: Math.round(duration * 1000) / 1000,
    frames,
    has_alpha: /^(yuva|rgba|argb|bgra|abgr|ya|gbrap)/.test(pix) || tags.alpha_mode === "1",
    has_audio: streams.some((s) => s.codec_type === "audio"),
  };
}

const EXPECTED_CODECS = Object.freeze({
  "mp4": ["h264", "hevc"], "webm": ["vp9"], "mov": ["prores"], "gif": ["gif"], "png-sequence": ["png"],
});

// verifyOutput is the medium-level gate: codec, alpha for the alpha formats, size, fps, duration and
// audio presence. It returns a list of problems; empty = the file is what was asked for.
export function verifyOutput(summary, expect) {
  const problems = [];
  const codecs = EXPECTED_CODECS[expect.format] || [];
  if (codecs.length && !codecs.includes(summary.codec)) problems.push(`codec ${summary.codec || "none"} (want ${codecs.join("|")})`);
  if (ALPHA_FORMATS.includes(expect.format) && !summary.has_alpha) problems.push(`no alpha channel in a ${expect.format} (pix_fmt ${summary.pix_fmt || "?"})`);
  if (expect.width && summary.width !== expect.width) problems.push(`width ${summary.width} (want ${expect.width})`);
  if (expect.height && summary.height !== expect.height) problems.push(`height ${summary.height} (want ${expect.height})`);
  if (expect.fps && expect.format !== "gif" && Math.abs(summary.fps - expect.fps) > 0.01) problems.push(`fps ${summary.fps} (want ${expect.fps})`);
  if (expect.duration && summary.fps) {
    const tol = 1 / summary.fps + 0.05;
    if (Math.abs(summary.duration_sec - expect.duration) > tol) problems.push(`duration ${summary.duration_sec}s (want ${expect.duration}s ±1 frame)`);
  }
  if (!summary.frames) problems.push("zero frames");
  if (expect.hasAudio && !summary.has_audio) problems.push("the composition carries <audio> but the output has no audio stream");
  return problems;
}

// ---------------------------------------------------------------------------------------------
// Process control
// ---------------------------------------------------------------------------------------------
function killTree(child) {
  if (!child || child.exitCode !== null || !child.pid) return;
  if (process.platform === "win32") {
    const root = process.env.SYSTEMROOT || process.env.SystemRoot || "C:\\Windows";
    try {
      spawn(join(root, "System32", "taskkill.exe"), ["/T", "/F", "/PID", String(child.pid)], { windowsHide: true, stdio: "ignore" });
    } catch { /* best effort */ }
  } else {
    try { process.kill(-child.pid, "SIGKILL"); } catch { try { child.kill("SIGKILL"); } catch { /* gone */ } }
  }
}

const OUTPUT_CAP = 8 << 20;

// runProcess spawns one child with a deadline; it never throws — the caller classifies.
export function runProcess(exe, args, { cwd, env, deadline }) {
  return new Promise((resolveP) => {
    const ms = Math.max(1, deadline - Date.now());
    let child;
    try {
      child = spawn(exe, args, { cwd, env, windowsHide: true, stdio: ["ignore", "pipe", "pipe"], detached: process.platform !== "win32" });
    } catch (e) {
      resolveP({ code: -1, stdout: "", stderr: "", spawnError: e });
      return;
    }
    let stdout = "";
    let stderr = "";
    let timedOut = false;
    child.stdout.on("data", (d) => { if (stdout.length < OUTPUT_CAP) stdout += d; });
    child.stderr.on("data", (d) => { stderr = (stderr + d).slice(-OUTPUT_CAP); });
    const timer = setTimeout(() => { timedOut = true; killTree(child); }, ms);
    child.on("error", (e) => {
      clearTimeout(timer);
      resolveP({ code: -1, stdout, stderr, spawnError: e, timedOut });
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      resolveP({ code: code ?? -1, stdout, stderr, timedOut });
    });
  });
}

// ---------------------------------------------------------------------------------------------
// The runner
// ---------------------------------------------------------------------------------------------
export function makeContext(f) {
  const hfDir = f["hyperframes-dir"] ? resolve(f["hyperframes-dir"]) : "";
  const cacheDir = resolve(f["cache-dir"] || join(tmpdir(), "offload-compose"));
  return {
    hfDir,
    entry: hfDir ? join(hfDir, CLI_ENTRY) : "",
    node: f.node || process.execPath,
    cacheDir,
    home: resolve(f.home || (hfDir ? join(hfDir, "home") : join(cacheDir, "home"))),
    templatesDir: resolve(f["templates-dir"] || DEFAULT_TEMPLATES_DIR),
  };
}

// installedVersion reads the version the install actually holds (node_modules/hyperframes/
// package.json). "" = unreadable.
export function installedVersion(hfDir) {
  try {
    const v = JSON.parse(readFileSync(join(hfDir, "node_modules", "hyperframes", "package.json"), "utf8")).version;
    return typeof v === "string" ? v : "";
  } catch {
    return "";
  }
}

// assertPinnedInstall refuses an install that is not the version every guard in this file was
// verified against: the env gates, the --json network skip and the flag set are read from the
// PINNED source, and a drifted install (a hand `npm install`, a half-applied bump) may not keep
// them. The fix is the installer's hyperframes step, which installs from the committed lockfile.
export function assertPinnedInstall(hfDir) {
  const have = installedVersion(hfDir);
  if (have !== PINNED_VERSION) {
    throw new ComposeError("CLI_MISSING", `the install at ${hfDir} holds hyperframes ${have || "(unreadable)"}, not the runner's pin ${PINNED_VERSION} — re-run the installer's hyperframes step (npm ci from setup/hyperframes)`);
  }
  return have;
}

function scriptRunner(exe) {
  // A .mjs/.js "binary" is run under node — the seam the stub-CLI tests use for ffprobe.
  return /\.(mjs|cjs|js)$/i.test(exe) ? { exe: process.execPath, pre: [exe] } : { exe, pre: [] };
}

async function invokeCli(ctx, args, { cwd, env, deadline, log }) {
  assertAllowedInvocation(args);
  let attempt = 0;
  for (;;) {
    attempt++;
    const r = await runProcess(ctx.node, [ctx.entry, ...args], { cwd, env, deadline });
    const text = `${r.stdout}\n${r.stderr}\n${r.spawnError ? r.spawnError.code + " " + r.spawnError.message : ""}`;
    const ebusy = (r.spawnError && r.spawnError.code === "EBUSY") || (r.code !== 0 && /\bEBUSY\b/.test(text));
    if (ebusy && attempt === 1 && !r.timedOut) {
      log(`compose: ${args[0]} hit EBUSY (antivirus lock, hyperframes #4058) — retrying once`);
      continue;
    }
    return { ...r, text, attempts: attempt };
  }
}

function tailText(s, n = 600) {
  const t = String(s || "").replace(/\s+/g, " ").trim();
  return t.length > n ? "…" + t.slice(-n) : t;
}

function failFrom(step, r) {
  if (r.timedOut) return new ComposeError("TIMEOUT", `${step} exceeded the compose deadline`);
  if (r.spawnError && r.spawnError.code === "EBUSY") return new ComposeError("SPAWN_EBUSY", `${step}: spawn EBUSY twice (antivirus lock on the CLI or ffmpeg)`);
  if (r.spawnError) return new ComposeError("CLI_MISSING", `${step}: could not start HyperFrames: ${r.spawnError.message}`);
  return new ComposeError(classifyFailure(step, r.text), `${step} exit ${r.code}: ${tailText(r.text)}`);
}

function findingsOf(section, name) {
  return ((section && section.findings) || [])
    .filter((x) => x && (x.severity === "error" || x.severity === "warning"))
    .map((x) => ({ section: name, code: String(x.code || ""), severity: String(x.severity), message: String(x.message || "").slice(0, 240) }));
}

export function summarizeCheck(report) {
  if (!report || typeof report !== "object") return { ok: false, findings: [] };
  const findings = [];
  for (const name of ["lint", "runtime", "layout", "motion", "contrast"]) findings.push(...findingsOf(report[name], name));
  return { ok: report.ok === true, findings: findings.slice(0, 40) };
}

function moveInto(src, dst) {
  mkdirSync(dirname(dst), { recursive: true });
  try {
    renameSync(src, dst);
  } catch {
    cpSync(src, dst, { recursive: true });
    rmSync(src, { recursive: true, force: true });
  }
}

function isDirEmpty(p) {
  try { return readdirSync(p).length === 0; } catch { return true; }
}

async function probe(ctx, ffprobe, target, env, deadline) {
  const { exe, pre } = scriptRunner(ffprobe);
  const base = ["-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-count_packets"];
  let r;
  if (/\.webm$/i.test(target)) {
    // VP9 alpha is side data the native decoder drops: through libvpx the stream reads
    // yuva420p, a real alpha signal rather than only the alpha_mode tag. An ffprobe built
    // without libvpx falls back to the plain probe (the tag still carries the answer).
    r = await runProcess(exe, [...pre, ...base, "-codec:v", "libvpx-vp9", target], { cwd: dirname(target), env, deadline });
    if (r.code !== 0 && !r.timedOut) r = null;
  }
  if (!r) r = await runProcess(exe, [...pre, ...base, target], { cwd: dirname(target), env, deadline });
  if (r.timedOut) throw new ComposeError("TIMEOUT", "ffprobe exceeded the compose deadline");
  if (r.spawnError) throw new ComposeError(r.spawnError.code === "EBUSY" ? "SPAWN_EBUSY" : "FFMPEG_MISSING", `ffprobe could not start: ${r.spawnError.message}`);
  const doc = parseJsonDoc(r.stdout);
  if (r.code !== 0 || !doc) throw new ComposeError("RENDER_FAILED", `ffprobe could not read the output: ${tailText(r.stderr)}`);
  return doc;
}

export async function runCompose(argv, { log = (s) => process.stdout.write(s + "\n"), parentEnv = process.env } = {}) {
  const started = Date.now();
  const { op, flags: f } = parseCli(argv);
  const ctx = makeContext(f);
  if (!["render", "browser", "version"].includes(op)) {
    throw new ComposeError("BAD_INPUT", `unknown op "${op}" (render | browser | version)`);
  }
  if (!ctx.hfDir) throw new ComposeError("BAD_INPUT", "--hyperframes-dir is required");
  if (!existsSync(ctx.entry)) {
    throw new ComposeError("CLI_MISSING", `HyperFrames CLI not found at ${ctx.entry} (run the installer's hyperframes step: npm ci in hyperframes_dir)`);
  }
  const installed = assertPinnedInstall(ctx.hfDir);
  mkdirSync(ctx.home, { recursive: true });

  if (op === "version") {
    const env = buildChildEnv(parentEnv, { home: ctx.home });
    const cwd = mkdtempSync(join(tmpdir(), "offload-compose-v-"));
    try {
      const r = await invokeCli(ctx, buildVersionArgs(), { cwd, env, deadline: Date.now() + 30000, log });
      if (r.code !== 0) throw failFrom("version", r);
      const version = String(r.stdout).trim().split(/\r?\n/).pop();
      if (version !== PINNED_VERSION) {
        throw new ComposeError("CLI_MISSING", `the CLI reports ${version}, not the runner's pin ${PINNED_VERSION} (package.json says ${installed})`);
      }
      return { ok: true, op, version, pinned: PINNED_VERSION, matches_pin: true, cli: ctx.entry };
    } finally {
      rmSync(cwd, { recursive: true, force: true });
    }
  }

  if (op === "browser") {
    const env = buildChildEnv(parentEnv, { home: ctx.home });
    const cwd = mkdtempSync(join(tmpdir(), "offload-compose-b-"));
    const deadline = Date.now() + numberOr(f["timeout-sec"], 900) * 1000;
    try {
      const ens = await invokeCli(ctx, buildBrowserArgs("ensure"), { cwd, env, deadline, log });
      if (ens.code !== 0) {
        const e = failFrom("browser ensure", ens);
        throw new ComposeError(e.cls === "TIMEOUT" ? "TIMEOUT" : "BROWSER_MISSING", e.detail);
      }
      const pth = await invokeCli(ctx, buildBrowserArgs("path"), { cwd, env, deadline, log });
      if (pth.code !== 0) throw new ComposeError("BROWSER_MISSING", `browser path exit ${pth.code}: ${tailText(pth.text)}`);
      const browserPath = String(pth.stdout).trim().split(/\r?\n/).map((s) => s.trim()).filter(Boolean).pop() || "";
      if (!browserPath || !existsSync(browserPath)) {
        throw new ComposeError("BROWSER_MISSING", `browser path printed "${browserPath}", which does not exist`);
      }
      const managedRoot = join(ctx.home, ".cache", "hyperframes", "chrome");
      const managed = resolve(browserPath).toLowerCase().startsWith(managedRoot.toLowerCase() + sep);
      const vm = /(\d+\.\d+\.\d+\.\d+)/.exec(browserPath);
      return { ok: true, op, browser_path: browserPath, managed, chrome_version: vm ? vm[1] : "", home: ctx.home };
    } finally {
      rmSync(cwd, { recursive: true, force: true });
    }
  }

  // ---- render ----
  const req = validateRequest(f);
  const deadline = started + req.timeoutSec * 1000;
  const ffmpeg = resolveBinary(f.ffmpeg || "ffmpeg", parentEnv);
  if (!ffmpeg) throw new ComposeError("FFMPEG_MISSING", `ffmpeg "${f.ffmpeg || "ffmpeg"}" not found (no such file, and not on PATH)`);
  const ffprobe = f.ffprobe ? resolveBinary(f.ffprobe, parentEnv) : (siblingFfprobe(ffmpeg) || resolveBinary("ffprobe", parentEnv));
  if (!ffprobe) throw new ComposeError("FFMPEG_MISSING", `ffprobe "${f.ffprobe || "ffprobe (next to ffmpeg)"}" not found`);
  const browser = f.browser || "";
  if (!browser || !existsSync(browser)) {
    throw new ComposeError("BROWSER_MISSING", `pinned chrome-headless-shell not found at "${browser}" (hyperframes_browser_path; run the installer's \`browser ensure\` step)`);
  }
  const variables = readVariables(f["variables-file"]);

  const workRoot = join(ctx.cacheDir, "work");
  const tempDir = join(ctx.cacheDir, "tmp");
  const extractCacheDir = join(ctx.cacheDir, "extract");
  for (const d of [workRoot, tempDir, extractCacheDir]) mkdirSync(d, { recursive: true });
  const work = mkdtempSync(join(workRoot, "job-"));
  if (!isDirEmpty(work) || existsSync(join(work, ".env"))) throw new ComposeError("BAD_INPUT", `work dir ${work} is not fresh`);
  const env = buildChildEnv(parentEnv, { home: ctx.home, tempDir, ffmpeg, ffprobe, browser, extractCacheDir });
  const keep = !!f["keep-work"];
  try {
    // 1. the project
    let projectDir;
    let templateManifest = {};
    if (f["project-dir"]) {
      projectDir = resolve(f["project-dir"]);
      const entryFile = f.composition ? join(projectDir, f.composition) : join(projectDir, "index.html");
      if (!existsSync(entryFile)) throw new ComposeError("BAD_INPUT", `project_dir has no ${f.composition || "index.html"} (${entryFile})`);
    } else {
      projectDir = join(work, "project");
      mkdirSync(projectDir, { recursive: true });
      if (f.template) {
        templateManifest = materializeTemplate(ctx.templatesDir, f.template, variables, projectDir);
      } else {
        const htmlFile = resolve(f["html-file"]);
        if (!existsSync(htmlFile)) throw new ComposeError("BAD_INPUT", `html file ${htmlFile} does not exist`);
        writeFileSync(join(projectDir, "index.html"), readFileSync(htmlFile));
      }
    }
    const entryHtml = readFileSync(f.composition ? join(projectDir, f.composition) : join(projectDir, "index.html"), "utf8");
    const meta = compositionMeta(entryHtml);
    const step = (name) => log(`compose: ${name}`);

    // 2. lint — static, no browser
    step("lint");
    const lr = await invokeCli(ctx, buildLintArgs(projectDir), { cwd: work, env, deadline, log });
    if (lr.timedOut || lr.spawnError) throw failFrom("lint", lr);
    const lint = parseJsonDoc(lr.stdout);
    if (!lint || typeof lint.errorCount !== "number") throw failFrom("lint", lr);
    const lintSummary = { errors: lint.errorCount, warnings: lint.warningCount || 0 };
    if (lint.errorCount > 0 && req.strict) {
      const first = (lint.findings || []).filter((x) => x.severity === "error").slice(0, 3)
        .map((x) => `${x.code || "error"}: ${x.message || ""}`).join(" | ");
      throw new ComposeError("LINT_ERRORS", `${lint.errorCount} lint error(s): ${tailText(first, 400)}`);
    }

    // 3. check — one software-GL browser session: runtime errors, layout, motion, contrast
    step("check");
    const cr = await invokeCli(ctx, buildCheckArgs(projectDir), { cwd: work, env, deadline, log });
    if (cr.timedOut || cr.spawnError) throw failFrom("check", cr);
    const checkDoc = parseJsonDoc(cr.stdout);
    const checkSummary = summarizeCheck(checkDoc);
    if (checkDoc && checkDoc.error && !checkDoc.ok && req.strict) {
      throw new ComposeError(classifyFailure("check", checkDoc.error), `check failed: ${tailText(checkDoc.error, 400)}`);
    }
    if (cr.code !== 0 && req.strict) {
      const why = checkSummary.findings.filter((x) => x.severity === "error").slice(0, 3)
        .map((x) => `${x.section}/${x.code}: ${x.message}`).join(" | ");
      throw new ComposeError(checkDoc ? "CHECK_FAILED" : classifyFailure("check", cr.text), `check exit ${cr.code}: ${tailText(why || cr.text, 400)}`);
    }

    // 4. render — one batch row, so --json yields the manifest document
    step("render");
    const outDir = join(work, "out");
    mkdirSync(outDir, { recursive: true });
    const rowsFile = join(work, "rows.json");
    writeFileSync(rowsFile, JSON.stringify([variables]));
    const target = req.format === "png-sequence" ? join(outDir, "frames") : join(outDir, "output" + FORMAT_EXT[req.format]);
    const rargs = buildRenderArgs({
      projectDir, rowsFile, output: target, format: req.format, quality: req.quality, workers: req.workers,
      strict: req.strict, fps: req.fps, resolution: f.resolution, composition: f.composition,
    });
    const rr = await invokeCli(ctx, rargs, { cwd: work, env, deadline, log });
    if (rr.timedOut || rr.spawnError) throw failFrom("render", rr);
    const manifest = parseJsonDoc(rr.stdout);
    const row = manifest && Array.isArray(manifest.rows) ? manifest.rows[0] : null;
    if (!row || row.status !== "completed") {
      const why = row && row.error ? row.error : rr.text;
      if (/\bEBUSY\b/.test(why)) throw new ComposeError("SPAWN_EBUSY", `render: ${tailText(why, 400)}`);
      throw new ComposeError(classifyFailure("render", why), `render ${row ? row.status : "exit " + rr.code}: ${tailText(why, 400)}`);
    }
    const produced = row.outputPath || target;
    if (!existsSync(produced)) throw new ComposeError("RENDER_FAILED", `render reported completed but ${produced} does not exist`);

    // 5. ffprobe — inspect the output in its own medium before claiming it
    step("verify");
    let summary;
    let probeTarget = produced;
    let pngs = [];
    if (req.format === "png-sequence") {
      pngs = readdirSync(produced).filter((n) => n.toLowerCase().endsWith(".png")).sort();
      if (!pngs.length) throw new ComposeError("RENDER_FAILED", `png-sequence ${produced} holds no PNG frames`);
      probeTarget = join(produced, pngs[0]);
    }
    summary = summarizeProbe(await probe(ctx, ffprobe, probeTarget, env, deadline));
    if (req.format === "png-sequence") {
      summary.frames = pngs.length;
      summary.fps = req.fps || meta.fps || 30;
      summary.duration_sec = Math.round((pngs.length / summary.fps) * 1000) / 1000;
      summary.has_audio = existsSync(join(produced, "audio.aac"));
    }
    const [rw, rh] = f.resolution ? RESOLUTION_DIMS[f.resolution] : [meta.width, meta.height];
    const expectDuration = row.durationMs ? row.durationMs / 1000 : meta.duration;
    const problems = verifyOutput(summary, {
      format: req.format, width: rw, height: rh, fps: req.fps || meta.fps || 0,
      duration: req.format === "png-sequence" ? 0 : expectDuration, hasAudio: meta.hasAudio,
    });
    if (problems.length) throw new ComposeError("RENDER_FAILED", `output failed verification: ${problems.join("; ")}`);

    // 6. snapshots — frame PNGs, never a cloud description
    const snapDir = join(work, "snaps");
    let snapFiles = [];
    if (req.snapshots.length) {
      step("snapshot");
      mkdirSync(snapDir, { recursive: true });
      const sr = await invokeCli(ctx, buildSnapshotArgs({ projectDir, at: req.snapshots, outDir: snapDir }), { cwd: work, env, deadline, log });
      if (sr.code !== 0) throw failFrom("snapshot", sr);
      snapFiles = readdirSync(snapDir).filter((n) => /^frame-.*\.png$/i.test(n)).sort();
      if (!snapFiles.length) throw new ComposeError("RENDER_FAILED", "snapshot produced no frames");
    }

    // 7. publish: move the output (and snapshots) out of the work dir
    const out = resolve(f.out);
    if (req.format === "png-sequence") {
      if (existsSync(out) && !isDirEmpty(out)) throw new ComposeError("BAD_INPUT", `out directory ${out} exists and is not empty (never overwritten)`);
      if (existsSync(out)) rmSync(out, { recursive: true, force: true });
    } else if (existsSync(out) && statSync(out).isDirectory()) {
      throw new ComposeError("BAD_INPUT", `out ${out} is a directory`);
    }
    moveInto(produced, out);
    const stem = req.format === "png-sequence" ? out : out.slice(0, out.length - extname(out).length);
    const snapshots = snapFiles.map((n) => {
      const dst = `${stem}-snap-${n.replace(/^frame-/, "")}`;
      moveInto(join(snapDir, n), dst);
      return dst;
    });
    const result = {
      ok: true,
      engine: "hyperframes",
      version: installed,
      format: req.format,
      quality: req.quality,
      workers: req.workers,
      template: f.template || "",
      ...(req.format === "png-sequence" ? { frames_dir: out } : { video_path: out }),
      duration_sec: summary.duration_sec,
      fps: summary.fps,
      frames: summary.frames,
      width: summary.width,
      height: summary.height,
      has_alpha: summary.has_alpha,
      has_audio: summary.has_audio,
      codec: summary.codec,
      pix_fmt: summary.pix_fmt,
      render_ms: Number(row.renderTimeMs || 0),
      wall_ms: Date.now() - started,
      lint: lintSummary,
      check: checkSummary,
      snapshots,
      template_duration_variable: templateManifest.duration_variable || undefined,
    };
    return result;
  } finally {
    if (!keep) rmSync(work, { recursive: true, force: true });
  }
}

function writeResultFile(path, obj) {
  if (!path) return;
  mkdirSync(dirname(path), { recursive: true });
  const tmp = `${path}.tmp-${process.pid}`;
  writeFileSync(tmp, JSON.stringify(obj));
  renameSync(tmp, path);
}

export async function main(argv = process.argv.slice(2)) {
  let resultPath = "";
  try {
    resultPath = parseCli(argv).flags.result || "";
  } catch { /* reported below */ }
  try {
    const result = await runCompose(argv);
    writeResultFile(resultPath, result);
    process.stdout.write(JSON.stringify(result) + "\n");
    return 0;
  } catch (e) {
    const cls = e instanceof ComposeError ? e.cls : "RENDER_FAILED";
    const detail = e instanceof ComposeError ? e.detail : (e && e.stack) || String(e);
    const oneLine = String(detail).replace(/\s+/g, " ").trim();
    const fail = { ok: false, class: cls, detail: oneLine.slice(0, 1200) };
    try { writeResultFile(resultPath, fail); } catch { /* the stdout line still carries it */ }
    process.stdout.write(`COMPOSE-FAIL: ${cls}: ${oneLine.slice(0, 300)}\n`);
    process.stdout.write(JSON.stringify(fail) + "\n");
    return 1;
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().then((code) => { process.exitCode = code; });
}
