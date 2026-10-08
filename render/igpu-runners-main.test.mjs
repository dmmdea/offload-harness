// node --test render/igpu-runners-main.test.mjs
// The runners' main() WIRING (CT-49 G30): each of sdcpp-video.mjs, sdcpp-animate.mjs and
// audiocpp-generate.mjs is run as the harness runs it, against a STUB ENGINE BINARY written to a
// temp dir (render/igpu-stub.mjs: an executable with the engine's interface that records its
// argv and env, prints a real captured log, writes the file the engine would, or hangs). What is
// pinned here is what no pure-builder test can see: the argv really reaches the engine, the
// GPU pin reaches the depth child, the output lands where --out says, a CPU line / missing
// GPU evidence / GPU reset / timeout kills the engine (the pid is dead) and leaves no output,
// the gates (black clip, dead air) remove what they reject, and every temp dir is gone.
//
// The runner's temp dir is redirected to a private directory (TEMP/TMP/TMPDIR) so "temp dirs
// removed" is an assertion on that directory, and LLAMA_SWAP_API points at a closed loopback port
// so the lease-less --no-lock path can never reach a real llama-swap. Tests write only under
// the OS temp dir. Needs ffmpeg + ffprobe, and on Windows a Go toolchain for the stub launcher.
import { test } from "node:test";
import assert from "node:assert";
import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { makeStub, stubAvailable } from "./igpu-stub.mjs";
import { resolveFfmpeg, resolveFfprobe } from "./audio-qa.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const ffmpeg = resolveFfmpeg();
const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
const skip = !ffmpeg || !ffprobe ? "no ffmpeg/ffprobe" : !stubAvailable() ? "cannot build a stub engine binary here (needs go on Windows)" : false;
const opts = { skip };
const fixture = (n) => join(here, "testdata", n);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const alive = (pid) => { try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; } };
async function waitGone(pid, ms = 15000) {
  const end = Date.now() + ms;
  while (Date.now() < end && alive(pid)) await sleep(50);
  return !alive(pid);
}

// A sandbox per test: a work dir (stubs, model files, outputs) and a private TEMP for the runner.
function sandbox() {
  const root = mkdtempSync(join(tmpdir(), "igpu-main-test-"));
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

function runNode(script, args, sb, { timeoutMs = 170000 } = {}) {
  const env = { ...process.env, TEMP: sb.priv, TMP: sb.priv, TMPDIR: sb.priv, FFMPEG_PATH: ffmpeg, LLAMA_SWAP_API: "http://127.0.0.1:9", IGPU_PARENT_POLL_MS: "250" };
  for (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE") || k === "GGML_VK_VISIBLE_DEVICES") delete env[k];
  return new Promise((resolve) => {
    const c = spawn(process.execPath, [join(here, script), ...args], { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    c.stdout.on("data", (d) => { stdout += d; });
    c.stderr.on("data", (d) => { stderr += d; });
    const t = setTimeout(() => c.kill("SIGKILL"), timeoutMs);
    c.on("close", (status) => { clearTimeout(t); resolve({ status, stdout, stderr }); });
  });
}

const records = (file) => (existsSync(file) ? readFileSync(file, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l)) : []);
const at = (argv, flag) => argv[argv.indexOf(flag) + 1];
const probe = (file, entries) => spawnSync(ffprobe, ["-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", `stream=${entries}`, "-of", "default=nw=1", file], { encoding: "utf8" }).stdout;
const frameCount = (file) => Number(/nb_read_frames=(\d+)/.exec(probe(file, "nb_read_frames"))[1]);
const rate = (file) => /r_frame_rate=(\S+)/.exec(probe(file, "r_frame_rate"))[1];
// the runner's last stderr line: every typed failure ends with its IGPU_CLASS= line (internal/gpugen reads it)
const lastLine = (stderr) => stderr.trim().split(/\r?\n/).pop();
const noTempLeft = (sb) => assert.deepEqual(readdirSync(sb.priv), [], "the runner's temp dirs are removed");

const GOOD_SD_HEADER = [
  "ggml_vulkan: Found 1 Vulkan devices:",
  "ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1 | bf16: 0 | warp size: 64",
  "model manager prepared params backend buffers (4112.00 MB, 1264 tensors, 5 blocks, VRAM) on Vulkan0",
  "wan compute buffer size: 512.00 MB(VRAM) on Vulkan0 (peak across 1 segment)",
];

// ---------------------------------------------------------------- sdcpp-video

function videoSetup(sb, spec, extra = []) {
  const rec = join(sb.work, "rec.jsonl");
  const pid = join(sb.work, "engine.pid");
  const bin = makeStub(sb.work, "sd-cli", { record: rec, pidFile: pid, writes: { kind: "video", ffmpeg }, ...spec });
  const out = join(sb.work, "nested", "deep", "clip.mp4");
  const args = ["--sd-bin", bin, "--model", sb.f("model.gguf"), "--vae", sb.f("vae.safetensors"), "--t5xxl", sb.f("t5.gguf"),
    "--backend", "vulkan0", "--frames", "5", "--width", "64", "--height", "64", "--fps", "8", "--seed", "7", "--no-lock", ...extra,
    "--", out, sb.f("still.png"), "--- Intro --- a calm sea"];
  return { rec, pid, out, args };
}

test("sdcpp-video main: the argv reaches the engine, the mp4 lands (creating its directory) at the asked fps, temp dirs are gone", opts, async () => {
  const sb = sandbox();
  try {
    const tae = sb.f("taew2_2.safetensors");
    const hn = sb.f("high.gguf");
    const v = videoSetup(sb, { log: GOOD_SD_HEADER }, ["--tae", tae, "--high-noise-model", hn, "--high-noise-cfg", "1", "--high-noise-steps", "3", "--high-noise-sampler", "euler"]);
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /WROTE .*clip\.mp4/);
    assert.ok(existsSync(v.out));
    assert.equal(frameCount(v.out), 5, "the mp4 carries the requested frames");
    assert.match(rate(v.out), /^8\/1$/, "the mp4 is encoded at the requested fps, not a hard-coded one");
    const [call] = records(v.rec);
    assert.deepEqual(call.argv.slice(0, 2), ["-M", "vid_gen"]);
    assert.equal(at(call.argv, "--backend"), "vulkan0");
    assert.equal(at(call.argv, "-p"), "--- Intro --- a calm sea", "a prompt that starts with -- stays the prompt (the -- terminator)");
    assert.equal(at(call.argv, "-i"), join(sb.work, "still.png"));
    assert.equal(at(call.argv, "-s"), "7");
    assert.equal(at(call.argv, "--video-frames"), "5");
    assert.equal(at(call.argv, "--vae-tile-overlap"), "0.25", "the measured best tile overlap is the default");
    assert.ok(call.argv.includes("--vae-tiling") && call.argv.includes("-v") && call.argv.includes("--diffusion-fa"));
    assert.equal(at(call.argv, "--taesd"), tae, "--tae reaches sd-cli as --taesd");
    assert.equal(at(call.argv, "--high-noise-diffusion-model"), hn);
    assert.equal(at(call.argv, "--high-noise-cfg-scale"), "1");
    assert.equal(at(call.argv, "--high-noise-steps"), "3");
    assert.equal(at(call.argv, "--high-noise-sampling-method"), "euler");
    assert.ok(!call.argv.includes("--offload-to-cpu"), "never --offload-to-cpu");
    assert.match(at(call.argv, "-o"), /out\.webm$/);
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-video main: without --tae there is no --taesd (the full VAE is the default)", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: GOOD_SD_HEADER });
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 0, r.stderr);
    assert.ok(!records(v.rec)[0].argv.includes("--taesd"));
    assert.ok(!records(v.rec)[0].argv.includes("--high-noise-cfg-scale"));
  } finally { sb.done(); }
});

test("sdcpp-video main: --offload-to-cpu in the extra args is sanctioned spill: it reaches sd-cli, the run passes (params on the host, compute on Vulkan)", opts, async () => {
  const sb = sandbox();
  try {
    const log = readFileSync(fixture("sdcpp-video-healthy.log"), "utf8")
      .split("\n").map((l) => (/prepared params backend buffers/.test(l) ? l.replace(/VRAM\) on Vulkan0/, "RAM) on Vulkan_Host") : l)).join("\n");
    assert.match(log, /RAM\) on Vulkan_Host/);
    const logFile = join(sb.work, "host-params.log");
    writeFileSync(logFile, log);
    const v = videoSetup(sb, { logFile }, ["--extra-args", JSON.stringify(["--offload-to-cpu"])]);
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 0, r.stderr);
    const [call] = records(v.rec);
    assert.ok(call.argv.includes("--offload-to-cpu"), "the binding's extra args carry it, so sd-cli gets it");
    assert.equal(at(call.argv, "--backend"), "vulkan0");
  } finally { sb.done(); }
});

test("sdcpp-video main: a CPU compute line kills the engine (its pid is dead), exits 1 CPU_PLACEMENT, writes no mp4 and leaves no temp dir", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: [...GOOD_SD_HEADER, "t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)"], hang: true });
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /CPU_PLACEMENT/);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=cpu_placement");
    assert.ok(!existsSync(v.out), "no clip is delivered");
    const pid = Number(readFileSync(v.pid, "utf8"));
    assert.ok(await waitGone(pid), "the engine process is dead");
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-video main: an engine that exits 0 without ever showing a GPU device is CPU_PLACEMENT (no evidence is no pass)", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: ["sd.cpp: all done"] });
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /CPU_PLACEMENT: no GPU evidence/);
    assert.ok(!existsSync(v.out));
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-video main: a device reset in the log is GPU_RESET naming the token cap, never 'exited 1'", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { logFile: fixture("sdcpp-vace-device-lost.log"), exit: 1 });
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /GPU_RESET/);
    assert.match(r.stderr, /2 s lockup timeout/);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=gpu_reset", "the class is the last line: the long human line is cut by gpugen's 400-byte tail");
    assert.ok(!existsSync(v.out));
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-video main: an entirely black clip is BLACK_CLIP and the mp4 is removed", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: GOOD_SD_HEADER, writes: { kind: "video", ffmpeg, black: true } });
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /BLACK_CLIP/);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=black_clip");
    assert.ok(!existsSync(v.out), "a black clip is never delivered");
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-video main: --timeout-sec kills a hanging engine (the pid is dead) with a timeout error", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: GOOD_SD_HEADER, hang: true }, ["--timeout-sec", "12"]);
    const r = await runNode("sdcpp-video.mjs", v.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /timeout/i);
    assert.ok(await waitGone(Number(readFileSync(v.pid, "utf8"))), "the engine process is dead");
    assert.ok(!existsSync(v.out));
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-video main: a bare binary name is refused (BINARY_NOT_ABSOLUTE), an unwritable out dir is refused before any spawn", opts, async () => {
  const sb = sandbox();
  try {
    const v = videoSetup(sb, { log: GOOD_SD_HEADER });
    const bare = v.args.slice();
    bare[bare.indexOf("--sd-bin") + 1] = "sd-cli";
    const r1 = await runNode("sdcpp-video.mjs", bare, sb);
    assert.equal(r1.status, 1);
    assert.match(r1.stderr, /BINARY_NOT_ABSOLUTE/);
    // the out dir's parent is a regular file: it cannot be created
    const blocker = sb.f("blocker", "i am a file");
    const bad = v.args.slice();
    bad[bad.indexOf("--") + 1] = join(blocker, "clip.mp4");
    const r2 = await runNode("sdcpp-video.mjs", bad, sb);
    assert.equal(r2.status, 1);
    assert.match(r2.stderr, /OUT_DIR_UNWRITABLE/);
    assert.deepEqual(records(v.rec), [], "the engine was never started");
  } finally { sb.done(); }
});

// ---------------------------------------------------------------- sdcpp-animate

function animateSetup(sb, { sd = {}, depth = {}, extra = [] } = {}) {
  const drv = join(sb.work, "driver.mp4");
  const r = spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=96x64:d=1:r=16", "-c:v", "libx264", "-pix_fmt", "yuv420p", drv]);
  assert.equal(r.status, 0, String(r.stderr));
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

test("sdcpp-animate main: depth runs once per frame with --no-invert on the pinned device, sd-cli gets RGB frames at exactly W x H, the mp4 has N frames, temp dirs are gone", opts, async () => {
  const sb = sandbox();
  try {
    const a = animateSetup(sb);
    const r = await runNode("sdcpp-animate.mjs", a.args, sb);
    assert.equal(r.status, 0, r.stderr);
    assert.ok(existsSync(a.out));
    assert.equal(frameCount(a.out), 5);
    const depthCalls = records(a.dRec);
    assert.equal(depthCalls.length, 5, "one depth process per frame");
    for (const c of depthCalls) {
      assert.equal(c.argv[0], "depth");
      assert.ok(c.argv.includes("--no-invert"), "near=bright, the depth-control convention");
      assert.equal(c.argv.filter((x) => x === "--input").length, 1, "never the multi-view --input a --input b form");
      assert.equal(c.env.GGML_VK_VISIBLE_DEVICES, "1", "the depth child is pinned to the sd backend's device");
    }
    const [sd] = records(a.sdRec);
    assert.equal(at(sd.argv, "--backend"), "vulkan1");
    assert.equal(at(sd.argv, "-p"), "--- a knight");
    assert.equal(at(sd.argv, "-i"), join(sb.work, "ref.png"), "-i is the VACE reference image");
    assert.ok(!sd.argv.includes("--offload-to-cpu"));
    assert.equal(at(sd.argv, "--vae-tile-overlap"), "0.25");
    assert.equal(sd.controlFrames.length, 5);
    for (const h of sd.controlFrames) {
      assert.deepEqual([h.width, h.height, h.bitDepth, h.colorType], [64, 64, 8, 2], `control frame ${h.name} is 8-bit RGB at exactly 64x64`);
    }
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-animate main: sd.cpp that decodes N+4 frames (reference latent kept) is trimmed to N; one that decodes N is left alone", opts, async () => {
  const sb = sandbox();
  try {
    const a = animateSetup(sb, { sd: { writes: { kind: "video", ffmpeg, extraFrames: 4 } } });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb);
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stderr, /dropping the first 4/);
    assert.equal(frameCount(a.out), 5, "the mp4 carries exactly the requested frames");
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-animate main: a depth process on the CPU stops the run at the first frame; sd-cli is never started", opts, async () => {
  const sb = sandbox();
  try {
    const a = animateSetup(sb, { depth: { logFile: undefined, log: ["[da3] da::Backend using device: CPU"] } });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /CPU_PLACEMENT/);
    assert.equal(records(a.dRec).length, 1, "the first frame is the probe");
    assert.deepEqual(records(a.sdRec), []);
    assert.ok(!existsSync(a.out));
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-animate main: a depth step with no GPU evidence at all is CPU_PLACEMENT on the first frame", opts, async () => {
  const sb = sandbox();
  try {
    const a = animateSetup(sb, { depth: { logFile: undefined, log: ["[da3] depth done"] } });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /CPU_PLACEMENT: no GPU evidence/);
    assert.equal(records(a.dRec).length, 1);
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-animate main: a model sd-cli refuses (the public VACE GGUF) is MODEL_INCOMPATIBLE naming the model file", opts, async () => {
  const sb = sandbox();
  try {
    const log = [...GOOD_SD_HEADER,
      "[ERROR] Diffusion model tensor 'model.diffusion_model.vace_patch_embedding.weight' not in model metadata",
      "[ERROR] model metadata validation failed"];
    const a = animateSetup(sb, { sd: { logFile: undefined, log, exit: 1, writes: undefined } });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /MODEL_INCOMPATIBLE/);
    assert.ok(r.stderr.includes("vace.safetensors"), "names the model file");
    assert.match(r.stderr, /vace_patch_embedding\.weight/);
    assert.ok(!existsSync(a.out));
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("sdcpp-animate main: --timeout-sec kills a hanging depth process (pid dead) and cleans up", opts, async () => {
  const sb = sandbox();
  try {
    const a = animateSetup(sb, { depth: { hang: true }, extra: ["--timeout-sec", "25"] });
    const r = await runNode("sdcpp-animate.mjs", a.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /timeout/i);
    assert.ok(existsSync(a.dPid), `the depth engine never started inside the budget (the host is too loaded for this test): ${r.stderr}`);
    assert.ok(await waitGone(Number(readFileSync(a.dPid, "utf8"))), "the depth process is dead");
    noTempLeft(sb);
  } finally { sb.done(); }
});

// ---------------------------------------------------------------- audiocpp-generate

function audioSetup(sb, kind, spec, extra = []) {
  const rec = join(sb.work, "rec.jsonl");
  const pid = join(sb.work, "engine.pid");
  const bin = makeStub(sb.work, "audiocpp_cli", { record: rec, pidFile: pid, ...spec });
  const out = join(sb.work, "deep", `${kind}.wav`);
  const args = ["--kind", kind, "--bin", bin, "--family", kind === "music" ? "ace_step" : "chatterbox", "--model", sb.f("model.gguf"),
    "--backend", "vulkan", "--device", "0", "--seed", "5", "--no-lock", ...extra, "--", out, "--- Intro --- warm lo-fi bed"];
  return { rec, pid, out, args };
}

const loudness = (file) => {
  const r = spawnSync(ffmpeg, ["-hide_banner", "-nostats", "-i", file, "-af", "loudnorm=I=-14:TP=-1:LRA=11:print_format=json", "-f", "null", "-"], { encoding: "utf8" });
  return Number(/"input_i"\s*:\s*"(-?[\d.]+)"/.exec(r.stderr)[1]);
};
const durationOf = (file) => Number(spawnSync(ffprobe, ["-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file], { encoding: "utf8" }).stdout.trim());
const sampleRate = (file) => Number(spawnSync(ffprobe, ["-v", "error", "-select_streams", "a:0", "-show_entries", "stream=sample_rate", "-of", "csv=p=0", file], { encoding: "utf8" }).stdout.trim());

test("audiocpp main (music): the argv reaches the engine, the 3 s silent tail is trimmed, the result is mastered to 48 kHz near -14 LUFS, temp dirs are gone", opts, async () => {
  const sb = sandbox();
  try {
    const a = audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], writes: { kind: "wav", seconds: 3, tailSilence: 3 } },
      ["--seconds", "30", "--lyrics", "--- Verse ---"]);
    const r = await runNode("audiocpp-generate.mjs", a.args, sb);
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /WROTE .*music\.wav/);
    const [call] = records(a.rec);
    assert.deepEqual([at(call.argv, "--task"), at(call.argv, "--family"), at(call.argv, "--backend"), at(call.argv, "--device")], ["gen", "ace_step", "vulkan", "0"]);
    assert.equal(at(call.argv, "--text"), "--- Intro --- warm lo-fi bed", "text that starts with -- stays the text (the -- terminator)");
    assert.equal(at(call.argv, "--lyrics"), "--- Verse ---");
    assert.equal(at(call.argv, "--duration-seconds"), "30");
    assert.equal(at(call.argv, "--seed"), "5");
    assert.ok(call.argv.includes("--log") && call.argv.includes("--metrics"));
    assert.notEqual(at(call.argv, "--out"), a.out, "the engine writes to a temp path; finalize delivers --out");
    assert.equal(sampleRate(a.out), 48000);
    const d = durationOf(a.out);
    assert.ok(d > 2.5 && d < 4.2, `the 3 s tail of silence is trimmed (3 s tone + a short kept tail), got ${d}s`);
    const lufs = loudness(a.out);
    assert.ok(lufs > -16 && lufs < -12, `mastered near -14 LUFS, got ${lufs}`);
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("audiocpp main (voice): the engine's wav is delivered untouched (24 kHz, not re-mastered) and the clone reference reaches the engine as --task clon", opts, async () => {
  const sb = sandbox();
  try {
    const ref = sb.f("ref.wav");
    const a = audioSetup(sb, "voice", { logFile: fixture("audiocpp-voice-clone.log"), writes: { kind: "wav", seconds: 2 } }, ["--clone", ref, "--lang", "en"]);
    const r = await runNode("audiocpp-generate.mjs", a.args, sb);
    assert.equal(r.status, 0, r.stderr);
    const [call] = records(a.rec);
    assert.equal(at(call.argv, "--task"), "clon");
    assert.equal(at(call.argv, "--voice-ref"), ref);
    assert.equal(at(call.argv, "--language"), "en");
    assert.equal(sampleRate(a.out), 24000, "voice is the engine's file as it wrote it");
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("audiocpp main: a silent render (a Vulkan NaN failure writes zeros and exits 0) is DEAD_AIR and the file is removed - voice and music", opts, async () => {
  for (const kind of ["voice", "music"]) {
    const sb = sandbox();
    try {
      const a = audioSetup(sb, kind, { log: ["[TIMING ts=1] chatterbox.t3.weights.buffer_name Vulkan0"], writes: { kind: "wav", seconds: 4, silent: true } });
      const r = await runNode("audiocpp-generate.mjs", a.args, sb);
      assert.equal(r.status, 1, `${kind}: ${r.stderr}`);
      assert.match(r.stderr, /DEAD_AIR/, kind);
      assert.ok(!existsSync(a.out), `${kind}: a silent file is never delivered`);
      noTempLeft(sb);
    } finally { sb.done(); }
  }
});

test("audiocpp main: the REAL host-prefill log (planner weights on the CPU) kills the engine - CPU_PLACEMENT, pid dead, nothing delivered", opts, async () => {
  const sb = sandbox();
  try {
    const a = audioSetup(sb, "music", { logFile: fixture("audiocpp-music-host-prefill.log"), hang: true, writes: { kind: "wav", seconds: 3 } });
    const r = await runNode("audiocpp-generate.mjs", a.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /CPU_PLACEMENT/);
    assert.equal(lastLine(r.stderr), "IGPU_CLASS=cpu_placement");
    assert.ok(!existsSync(a.out));
    assert.ok(await waitGone(Number(readFileSync(a.pid, "utf8"))), "the engine process is dead");
    noTempLeft(sb);
  } finally { sb.done(); }
});

test("audiocpp main: --timeout-sec kills a hanging engine; vulkan0 as the backend is refused (audio.cpp takes --device separately)", opts, async () => {
  const sb = sandbox();
  try {
    const a = audioSetup(sb, "music", { log: ["[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"], hang: true }, ["--timeout-sec", "12"]);
    const r = await runNode("audiocpp-generate.mjs", a.args, sb);
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /timeout/i);
    assert.ok(await waitGone(Number(readFileSync(a.pid, "utf8"))), "the engine process is dead");
    noTempLeft(sb);
    const bad = a.args.slice();
    bad[bad.indexOf("--backend") + 1] = "vulkan0";
    const r2 = await runNode("audiocpp-generate.mjs", bad, sb);
    assert.equal(r2.status, 1);
    assert.match(r2.stderr, /CPU_BACKEND_REFUSED/);
    assert.match(r2.stderr, /--device 0/);
  } finally { sb.done(); }
});
