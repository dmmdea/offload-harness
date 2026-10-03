// igpu-engine.mjs — shared plumbing for the iGPU media runners (sdcpp-video.mjs,
// sdcpp-animate.mjs, audiocpp-generate.mjs): the spawn-per-job native CLIs that serve
// video, animate, voice and music on a box whose only GPU is a Vulkan iGPU.
//
// THE RULE THIS FILE ENFORCES: no model runs on CPU on these engines. Two guards:
//   1. refuseCpuBackend — a cpu (or unset) --backend is refused before anything spawns
//      (mirrors config.CPUBackendRefusal on the Go side; the Go side refuses first, this
//      is the runner's own door for a hand-run or a stale config).
//   2. detectCpuPlacement + runEngine — the engine's own log is read line by line while it
//      runs, and the FIRST line that places a compute module on the CPU kills the process
//      tree and fails the job with CPU_PLACEMENT, rather than letting a 4-minute render
//      finish on the wrong silicon. gpugen.ClassifyErr maps the "CPU_PLACEMENT" text to the
//      "cpu_placement" error class.
//
// Dependency-free (Node 18+ built-ins only).
import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const CPU_PLACEMENT = "CPU_PLACEMENT";
export const CPU_BACKEND_REFUSED = "CPU_BACKEND_REFUSED";

// parseArgs: positionals + --flags. `booleans` names the flags that take no value.
// Every other --flag consumes the next token (a missing value is undefined, which every
// consumer treats as unset).
export function parseArgs(argv, booleans = ["no-lock"]) {
  const pos = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (typeof a === "string" && a.startsWith("--") && a.length > 2) {
      const k = a.slice(2);
      if (booleans.includes(k)) flags[k] = true;
      else {
        flags[k] = argv[i + 1];
        i++;
      }
    } else pos.push(a);
  }
  return { pos, flags };
}

// parseExtraArgs: a JSON array of strings (the Go side marshals config's *_extra_args
// that way, so a token with spaces survives). Empty/undefined = [].
export function parseExtraArgs(raw) {
  if (raw === undefined || raw === null || String(raw).trim() === "") return [];
  let v;
  try {
    v = JSON.parse(String(raw));
  } catch (e) {
    throw new Error("--extra-args must be a JSON array of strings: " + e.message);
  }
  if (!Array.isArray(v) || v.some((x) => typeof x !== "string")) {
    throw new Error("--extra-args must be a JSON array of strings");
  }
  return v;
}

function isCpuName(v) {
  if (!v.startsWith("cpu")) return false;
  return /^\d*$/.test(v.slice(3));
}

// refuseCpuBackend: throws CPU_BACKEND_REFUSED for a backend that is unset or places any
// module on the CPU. sd-cli's --backend takes one value ("vulkan0") or per-module
// assignments ("diffusion=vulkan0,vae=cpu"; "&" joins the devices of one module), so every
// assignment is checked. Returns the trimmed backend when it is acceptable.
export function refuseCpuBackend(backend) {
  const b = String(backend ?? "").trim();
  if (b === "") {
    throw new Error(CPU_BACKEND_REFUSED + ": --backend is unset (name a GPU backend such as vulkan0; no model runs on CPU on this engine)");
  }
  for (const part of b.toLowerCase().split(/[,&]/)) {
    let v = part.trim();
    const eq = v.lastIndexOf("=");
    if (eq >= 0) v = v.slice(eq + 1).trim();
    if (isCpuName(v)) {
      throw new Error(CPU_BACKEND_REFUSED + `: --backend ${JSON.stringify(b)} places a model on the CPU (no model runs on CPU on this engine)`);
    }
  }
  return b;
}

// Lines that mention "cpu" without placing a compute module on it. A line matching any of
// these is never a CPU-placement finding:
//   - the host description both engines print ("system_info: n_threads = 4 | CPU : AVX2 = 1"),
//   - ggml listing every backend it loaded (the CPU backend is always registered, even on a
//     Vulkan run: "load_backend: loaded CPU backend from ...", "registered backend CPU"),
//   - RNG selection (--rng cpu, sampler_rng, brownian_tree_rng: the noise generator is not a
//     model),
//   - parameter STORAGE lines (params backend / offload / prefetch / weights): where bytes
//     rest is the sanctioned RAM overflow, not where the compute runs.
const NOT_PLACEMENT = [
  /system[_ ]?info/i,
  /\bCPU\s*:\s*[A-Za-z0-9_]+\s*=\s*\d/, // "CPU : SSE3 = 1 | AVX = 1" style host feature dumps
  /load(?:ed)?_?backend|loaded\s+\S+\s+backend\s+from|registered\s+(?:\S+\s+)?backend|register_backend|backend\s+registry/i,
  /\brng\b/i,
  /params?[ _-]?backend|offload|prefetch|\bweights?\b|\bmmap\b/i,
];

// What a compute-module CPU placement looks like in the engines' logs:
//   "<module>: Using CPU backend" / "using CPU backend"      (sd.cpp, per module)
//   "<module> backend: CPU" / "backend = cpu0" / "backend -> CPU"  (assignment echo)
//   "<module>=cpu"                                           (per-module --backend echo)
//   "selected backend: cpu" / "Using backend: CPU"           (audio.cpp's backend line)
//   "running on CPU" / "falling back to CPU" / "fallback to cpu"
// The patterns are anchored on the word "cpu" as a whole token (so "cpufreq" and "mycpu"
// never match) next to a placement word.
const PLACEMENT = [
  /\busing\s+cpu\b/i,
  /\bcpu\s+backend\b/i,
  /\bbackend\s*(?:[:=]|->|is)\s*cpu\d*\b/i,
  /\b[a-z][\w.-]*\s*=\s*cpu\d*\b/i,
  /\b(?:running|run|runs|computing|compute|placed|placing|fall(?:ing)?\s*back|fallback)\s+(?:on|to|onto)\s+cpu\b/i,
  /\bfell\s+back\s+to\s+cpu\b/i,
];

// detectCpuPlacement: the first log line (of `text`, any line separator) that places a
// compute module on the CPU, as {line, lineNo}; null when none does. Pure.
export function detectCpuPlacement(text) {
  const lines = String(text ?? "").split(/\r\n|\r|\n/);
  for (let i = 0; i < lines.length; i++) {
    const hit = detectCpuPlacementLine(lines[i]);
    if (hit) return { line: lines[i].trim(), lineNo: i + 1 };
  }
  return null;
}

// detectCpuPlacementLine: the single-line predicate runEngine applies as output streams.
export function detectCpuPlacementLine(line) {
  const l = String(line ?? "");
  if (!/cpu/i.test(l)) return false;
  if (NOT_PLACEMENT.some((re) => re.test(l))) return false;
  return PLACEMENT.some((re) => re.test(l));
}

export function cpuPlacementError(hit) {
  return new Error(`${CPU_PLACEMENT}: the engine placed a model on the CPU (log line ${hit.lineNo}: ${hit.line}) — no model runs on CPU on this engine; aborted`);
}

// killTree: the whole process tree, not just the child (a Windows node-kill orphans the
// grandchildren; the engines here are single processes today but ffmpeg/depth helpers are
// not). Best-effort, never throws.
export function killTree(child) {
  if (!child || child.pid === undefined || child.exitCode !== null) return;
  try {
    if (process.platform === "win32") {
      spawnSync("taskkill", ["/PID", String(child.pid), "/T", "/F"], { stdio: "ignore" });
    } else {
      try { process.kill(-child.pid, "SIGKILL"); } catch { child.kill("SIGKILL"); }
    }
  } catch { /* nothing more to do */ }
}

// runEngine: spawn `bin args`, tee every output line to stderr (that is the progress the Go
// side tails), scan each line with `detect`, and settle:
//   resolves {code, log}            the process exited (any code; the caller judges it)
//   rejects  CPU_PLACEMENT          `detect` matched a line — the tree is killed first
//   rejects  timeout                timeoutMs elapsed — the tree is killed first
// stdout and stderr are both read (an engine may print its placement lines to either).
export function runEngine({ bin, args, env, timeoutMs = 0, detect = detectCpuPlacementLine, label = "engine", spawnImpl = spawn }) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const child = spawnImpl(bin, args, {
      env: { ...process.env, ...(env || {}) },
      stdio: ["ignore", "pipe", "pipe"],
      detached: process.platform !== "win32",
      windowsHide: true,
    });
    const chunks = [];
    let timer = null;
    const finish = (fn, v) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      fn(v);
    };
    const onLine = (line) => {
      if (settled) return;
      chunks.push(line);
      process.stderr.write(line + "\n");
      if (detect(line)) {
        killTree(child);
        finish(reject, cpuPlacementError({ line: line.trim(), lineNo: chunks.length }));
      }
    };
    const feed = (stream) => {
      let buf = "";
      stream.setEncoding("utf8");
      stream.on("data", (d) => {
        buf += d;
        const parts = buf.split(/\r\n|\r|\n/);
        buf = parts.pop();
        for (const p of parts) if (p !== "") onLine(p);
      });
      stream.on("end", () => { if (buf !== "") onLine(buf); });
    };
    feed(child.stdout);
    feed(child.stderr);
    child.on("error", (e) => finish(reject, new Error(`${label} failed to start: ${e.message}`)));
    child.on("close", (code) => finish(resolve, { code, log: chunks.join("\n") }));
    if (timeoutMs > 0) {
      timer = setTimeout(() => {
        killTree(child);
        finish(reject, new Error(`${label} timeout after ${Math.round(timeoutMs / 1000)}s (killed)`));
      }, timeoutMs);
    }
  });
}

// normalizeFrames: a Wan-family video model takes 4k+1 frames. Round to the NEAREST 4k+1
// (a tie goes up), minimum 5. A non-numeric or non-positive request returns `def`
// (itself normalized).
export function normalizeFrames(n, def = 49) {
  let v = Math.round(Number(n));
  if (!Number.isFinite(v) || v <= 0) v = Math.round(Number(def));
  if (!Number.isFinite(v) || v <= 5) return 5;
  const down = Math.floor((v - 1) / 4) * 4 + 1;
  const up = down + 4;
  return v - down < up - v ? down : up;
}

// floorFrames: the largest 4k+1 not above `available`, or 0 when fewer than 5 exist. Used
// when a driver clip is shorter than the frames the caller asked for.
export function floorFrames(available) {
  const a = Math.floor(Number(available));
  if (!Number.isFinite(a) || a < 5) return 0;
  return Math.floor((a - 1) / 4) * 4 + 1;
}

// normalizeSize: width/height to a multiple of 32, FLOORED (never grow a render past the
// memory the caller sized for), minimum 32. Non-numeric or non-positive returns `def`.
export function normalizeSize(v, def) {
  const n = Math.floor(Number(v));
  if (!Number.isFinite(n) || n <= 0) return def;
  return Math.max(32, Math.floor(n / 32) * 32);
}

// vulkanDeviceFromBackend: "vulkan1" -> "1", "vulkan" -> "", per-module forms -> the first
// vulkanN found. Used to pin GGML_VK_VISIBLE_DEVICES for a helper whose CLI has no backend
// flag (depth-anything.cpp), so it sees the same device the sd-cli backend names.
export function vulkanDeviceFromBackend(backend) {
  const m = /vulkan(\d+)/i.exec(String(backend ?? ""));
  return m ? m[1] : "";
}

// finiteNum: a flag value as a number, or undefined (unset or not a number).
export function finiteNum(v) {
  if (v === undefined || v === null || v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}

// ffmpegToMp4: re-encode a sd-cli .webm/.avi to an H.264 mp4 (yuv420p, CRF 16, `fps`).
export function mp4Args(src, dst, fps) {
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", src,
    "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "16", "-r", String(fps), "-movflags", "+faststart", dst];
}

export function encodeMp4(ffmpeg, src, dst, fps) {
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const r = spawnSync(ffmpeg, mp4Args(src, dst, fps), { encoding: "utf8" });
  if (r.error || r.status !== 0 || !existsSync(dst)) {
    throw new Error("ffmpeg mp4 encode failed: " + (r.error ? r.error.message : String(r.stderr || "").trim().slice(-300)));
  }
}

// makeTempDir: a private work dir under the OS temp dir, removed by cleanup() on every
// exit path: the caller's finally, AND a process 'exit' hook (withGpuSlot turns SIGINT /
// SIGTERM into process.exit, which fires it), so a killed job leaves no frames behind.
export function makeTempDir(prefix) {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  let done = false;
  const cleanup = () => {
    if (done) return;
    done = true;
    try { rmSync(dir, { recursive: true, force: true }); } catch { /* best effort */ }
  };
  process.on("exit", cleanup);
  return { dir, cleanup };
}
