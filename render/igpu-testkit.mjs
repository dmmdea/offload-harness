// igpu-testkit.mjs - shared set-up for the tests that run the REAL iGPU runners against stub engine
// binaries (render/igpu-stub.mjs): a sandbox with a private TEMP, runNode (with optional node
// --require preloads and extra env), and healthy set-ups for the video, animate and audio runners.
// Test support only; never imported by a runner.
import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { makeStub, stubAvailable } from "./igpu-stub.mjs";
import { resolveFfmpeg, resolveFfprobe } from "./audio-qa.mjs";

export const here = dirname(fileURLToPath(import.meta.url));
export const ffmpeg = resolveFfmpeg();
export const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
export const skipReason = !ffmpeg || !ffprobe ? "no ffmpeg/ffprobe" : !stubAvailable() ? "cannot build a stub engine binary here (needs go on Windows)" : false;
export const fixture = (n) => join(here, "testdata", n);
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
export const alive = (pid) => { try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; } };
export async function waitGone(pid, ms = 15000) {
  const end = Date.now() + ms;
  while (Date.now() < end && alive(pid)) await sleep(50);
  return !alive(pid);
}
export const records = (file) => (existsSync(file) ? readFileSync(file, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l)) : []);
export const at = (argv, flag) => argv[argv.indexOf(flag) + 1];

export const GOOD_SD_HEADER = [
  "ggml_vulkan: Found 1 Vulkan devices:",
  "ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1 | bf16: 0 | warp size: 64",
  "model manager prepared params backend buffers (4112.00 MB, 1264 tensors, 5 blocks, VRAM) on Vulkan0",
  "wan compute buffer size: 512.00 MB(VRAM) on Vulkan0 (peak across 1 segment)",
];

// A sandbox per test: a work dir (stubs, model files, outputs) and a private TEMP for the runner.
export function sandbox() {
  const root = mkdtempSync(join(tmpdir(), "igpu-kit-"));
  const work = join(root, "work");
  const priv = join(root, "tmp");
  mkdirSync(work);
  mkdirSync(priv);
  const f = (name, content = "x") => {
    const p = join(work, name);
    writeFileSync(p, content);
    return p;
  };
  return { root, work, priv, f, done: () => rmSync(root, { recursive: true, force: true }) };
}

// runNode: run a runner script as the harness does. `preload` is a list of node --require files,
// `env` extra environment. Resolves {status, stdout, stderr, ms}.
export function runNode(script, args, sb, { timeoutMs = 170000, preload = [], env: extraEnv = {} } = {}) {
  const env = { ...process.env, TEMP: sb.priv, TMP: sb.priv, TMPDIR: sb.priv, FFMPEG_PATH: ffmpeg, LLAMA_SWAP_API: "http://127.0.0.1:9", IGPU_PARENT_POLL_MS: "250", ...extraEnv };
  for (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE") || k === "GGML_VK_VISIBLE_DEVICES") delete env[k];
  const execArgv = preload.flatMap((p) => ["--require", p]);
  return new Promise((resolve) => {
    const t0 = Date.now();
    const c = spawn(process.execPath, [...execArgv, join(here, script), ...args], { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    c.stdout.on("data", (d) => { stdout += d; });
    c.stderr.on("data", (d) => { stderr += d; });
    const t = setTimeout(() => c.kill("SIGKILL"), timeoutMs);
    c.on("close", (status) => { clearTimeout(t); resolve({ status, stdout, stderr, ms: Date.now() - t0 }); });
  });
}

export function videoSetup(sb, spec, extra = []) {
  const rec = join(sb.work, "rec.jsonl");
  const pid = join(sb.work, "engine.pid");
  const bin = makeStub(sb.work, "sd-cli", { record: rec, pidFile: pid, writes: { kind: "video", ffmpeg }, ...spec });
  const out = join(sb.work, "nested", "deep", "clip.mp4");
  const args = ["--sd-bin", bin, "--model", sb.f("model.gguf"), "--vae", sb.f("vae.safetensors"), "--t5xxl", sb.f("t5.gguf"),
    "--backend", "vulkan0", "--frames", "5", "--width", "64", "--height", "64", "--fps", "8", "--seed", "7", "--no-lock", ...extra,
    "--", out, sb.f("still.png"), "--- Intro --- a calm sea"];
  return { rec, pid, out, args };
}

export function animateSetup(sb, { sd = {}, depth = {}, extra = [] } = {}) {
  const drv = join(sb.work, "driver.mp4");
  const r = spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=96x64:d=1:r=16", "-c:v", "libx264", "-pix_fmt", "yuv420p", drv]);
  if (r.status !== 0) throw new Error("cannot make the driver clip: " + String(r.stderr));
  const sdRec = join(sb.work, "sd.jsonl");
  const dRec = join(sb.work, "depth.jsonl");
  const sdPid = join(sb.work, "sd.pid");
  const dPid = join(sb.work, "depth.pid");
  const sdBin = makeStub(sb.work, "sd-cli", { record: sdRec, pidFile: sdPid, logFile: fixture("sdcpp-video-healthy.log"), inspectControlVideo: true, writes: { kind: "video", ffmpeg }, ...sd });
  const depthBin = makeStub(sb.work, "da3-cli", { record: dRec, pidFile: dPid, logFile: fixture("da3-healthy.log"), writes: { kind: "gray_png", width: 40, height: 72 }, ...depth });
  const out = join(sb.work, "out", "animate.mp4");
  const args = ["--sd-bin", sdBin, "--model", sb.f("vace.safetensors"), "--vae", sb.f("vae.safetensors"), "--t5xxl", sb.f("t5.gguf"),
    "--backend", "vulkan1", "--depth-bin", depthBin, "--depth-model", sb.f("depth.gguf"),
    "--frames", "5", "--width", "64", "--height", "64", "--seed", "3", "--no-lock", ...extra,
    "--", out, sb.f("ref.png"), drv, "--- a knight"];
  return { sdRec, dRec, sdPid, dPid, out, args };
}

export function audioSetup(sb, kind, spec, extra = []) {
  const rec = join(sb.work, "rec.jsonl");
  const pid = join(sb.work, "engine.pid");
  const bin = makeStub(sb.work, "audiocpp_cli", { record: rec, pidFile: pid, ...spec });
  const out = join(sb.work, "deep", `${kind}.wav`);
  const args = ["--kind", kind, "--bin", bin, "--family", kind === "music" ? "ace_step" : "chatterbox", "--model", sb.f("model.gguf"),
    "--backend", "vulkan", "--device", "0", "--seed", "5", "--no-lock", ...extra, "--", out, "--- Intro --- warm lo-fi bed"];
  return { rec, pid, out, args };
}
