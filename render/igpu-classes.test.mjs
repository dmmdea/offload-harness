// node --test render/igpu-classes.test.mjs
// The error classes the iGPU runners end with: the TOKEN: -> class map and the one short
// "IGPU_CLASS=<class>" line every typed failure prints last (internal/gpugen reads the class from
// it, anywhere in the output, because the long human line is cut by the 400-byte display tail),
// an engine crash is its own class (not a timeout through the word "killed"), and ggml / sd.cpp
// memory exhaustion is OUT_OF_MEMORY whether the engine exits 1 or dies of SIGABRT.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  errorClass, reportFatal, CLASS_MARKER, createLogGuard, runEngine, engineExitError, memoryFailure,
  ENGINE_CRASHED, OUT_OF_MEMORY, gpuResetError,
} from "./igpu-engine.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const isWin = process.platform === "win32";

test("errorClass: every typed token maps to gpugen's class name; timeout is found by word; the rest is untyped", () => {
  const table = {
    "CPU_PLACEMENT: x": "cpu_placement", "CPU_BACKEND_REFUSED: x": "cpu_backend_refused", "GPU_RESET: x": "gpu_reset",
    "TOKEN_CAP_EXCEEDED: x": "token_cap_exceeded", "EXTRA_ARGS_REFUSED: x": "extra_args_refused",
    "ILLEGAL_INSTRUCTION: x": "illegal_instruction", "BLACK_CLIP: x": "black_clip", "FROZEN_CLIP: x": "frozen_clip",
    "DEPTH_FRAMES_INVALID: x": "depth_frames_invalid", "MODEL_INCOMPATIBLE: x": "model_incompatible",
    "BINARY_NOT_ABSOLUTE: x": "binary_not_absolute", "OUT_DIR_UNWRITABLE: x": "out_dir_unwritable",
    "DEAD_AIR: x": "dead_air", "FFMPEG_UNAVAILABLE: x": "ffmpeg_unavailable", "UNMEASURABLE: x": "unmeasurable",
    "ENGINE_CRASHED: x": "engine_crashed", "OUT_OF_MEMORY: x": "oom", "DEVICE_INVALID: x": "device_invalid",
    "sd-cli timeout after 5s (killed)": "timeout", "the 30s budget was spent: ffmpeg mp4 encode timeout": "timeout",
    "sd-cli exited 1": "", "something else": "", "NOT_A_CLASS: x": "",
  };
  for (const [msg, want] of Object.entries(table)) assert.equal(errorClass(msg), want, msg);
  assert.equal(errorClass(undefined), "");
});

test("reportFatal: the human line, then the class line LAST; an untyped failure prints no class line", () => {
  const code = `
    import { reportFatal } from ${JSON.stringify(pathToFileURL(join(here, "igpu-engine.mjs")).href)};
    reportFatal("SDCPP VIDEO", new Error(process.argv[1]));`;
  const run = (msg) => spawnSync(process.execPath, ["--input-type=module", "-e", code, msg], { encoding: "utf8" }).stderr.trim().split(/\r?\n/);
  const typed = run("GPU_RESET: the GPU reset " + "x".repeat(600));
  assert.match(typed[0], /^SDCPP VIDEO FAILED: GPU_RESET:/);
  assert.ok(typed[0].length > 600);
  assert.equal(typed[typed.length - 1], CLASS_MARKER + "gpu_reset");
  const untyped = run("sd-cli exited 3");
  assert.equal(untyped.length, 1);
  assert.ok(!untyped[0].includes(CLASS_MARKER));
  assert.equal(CLASS_MARKER, "IGPU_CLASS=");
});

const guard = () => createLogGuard({ engine: "sdcpp" });
const deviceLine = "console.error('ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1');";

test("runEngine: a crash signal is ENGINE_CRASHED, never a timeout through the word 'killed'", { skip: isWin && "POSIX signals" }, async () => {
  // each of the six crash signals on its own: dropping any one from CRASH_SIGNALS turns it into "killed by signal"
  for (const sig of ["SIGSEGV", "SIGABRT", "SIGBUS", "SIGFPE", "SIGTRAP", "SIGSYS"]) {
    await assert.rejects(
      runEngine({ bin: process.execPath, args: ["-e", `${deviceLine}process.kill(process.pid,'${sig}');setInterval(()=>{},1000)`], guard: guard(), label: "sd-cli" }),
      (e) => new RegExp(ENGINE_CRASHED).test(e.message) && e.message.includes(sig) && !/killed/i.test(e.message) && errorClass(e.message) === "engine_crashed",
      sig,
    );
  }
});

test("runEngine: ggml 'insufficient memory' before a SIGABRT, and 'alloc compute buffer failed' on exit 1, are OUT_OF_MEMORY", { skip: isWin && "POSIX signals" }, async () => {
  await assert.rejects(
    runEngine({
      bin: process.execPath, guard: guard(), label: "sd-cli",
      args: ["-e", `${deviceLine}console.error('ggml_backend_alloc_ctx_tensors_from_buft: insufficient memory (attempted to allocate 5162.00 MB)');process.kill(process.pid,'SIGABRT');setInterval(()=>{},1000)`],
    }),
    (e) => new RegExp(OUT_OF_MEMORY).test(e.message) && /5162/.test(e.message) && errorClass(e.message) === "oom",
  );
});

test("engineExitError: an out-of-memory log is OUT_OF_MEMORY, a refused model is MODEL_INCOMPATIBLE, anything else 'exited N'", () => {
  const oom = engineExitError("sd-cli", 1, "a\n[ERROR] ggml_runner.cpp:991 - wan alloc compute buffer failed\nb", "/m/x.safetensors");
  assert.match(oom.message, /^OUT_OF_MEMORY: sd-cli ran out of memory \(exit 1\)/);
  assert.match(oom.message, /alloc compute buffer failed/);
  const model = engineExitError("sd-cli", 1, "[ERROR] model metadata validation failed", "/m/x.safetensors");
  assert.match(model.message, /^MODEL_INCOMPATIBLE:/);
  const plain = engineExitError("depth-anything", 2, "all fine", "");
  assert.equal(plain.message, "depth-anything exited 2");
  // a model file is only named when the caller has one (depth-anything has none)
  assert.equal(engineExitError("depth-anything", 2, "model metadata validation failed", "").message, "depth-anything exited 2");
});

test("memoryFailure: each real out-of-memory shape is found, ordinary lines are not", () => {
  for (const l of [
    "ggml_backend_alloc_ctx_tensors_from_buft: insufficient memory (attempted to allocate 5162.00 MB)",
    "[ERROR] ggml_runner.cpp:991 - wan alloc compute buffer failed",
    "ggml_vulkan: Device memory allocation of size 5368709120 failed.",
    "vk::Device::allocateMemory: ErrorOutOfDeviceMemory", "vk::Device::allocateMemory: ErrorOutOfHostMemory",
    "std::bad_alloc: out of memory",
  ]) assert.ok(memoryFailure([l]), l);
  for (const l of ["t5 compute buffer size: 297.00 MB(VRAM) on Vulkan0", "model manager prepared params backend buffers (4112.00 MB)", "memory: 12 GB free"]) {
    assert.equal(memoryFailure([l]), "", l);
  }
});

test("gpuResetError: the advice fits the engine - a token cap for sd.cpp, a shorter request for audio.cpp", () => {
  const hit = { lineNo: 12, line: "ggml_vulkan: device lost on Vulkan0" };
  assert.match(gpuResetError(hit).message, /sdcpp_max_tokens/);
  const audio = gpuResetError(hit, "audiocpp").message;
  assert.match(audio, /^GPU_RESET:/);
  assert.ok(!/sdcpp_max_tokens/.test(audio), "audio.cpp has no token cap to name");
  assert.match(audio, /shorten the request/);
});

test("failAndExit: the end of a failure survives a stderr pipe that is full and read slowly (process.exit would drop it)", async () => {
  const mod = pathToFileURL(join(here, "igpu-engine.mjs")).href;
  // About 175 KB of engine chatter (more than twice a 64 KiB pipe buffer, so most of it can only be queued),
  // then the typed failure and its class line, then exit. failAndExit gives a pipe nobody drains 3 s, so the
  // reader below has to be slow RELATIVE TO THE WRITER and cheap in absolute time: it takes the 3 s bound
  // out of reach of a loaded host (this test failed once in eight parallel runs with 400 KB and 20 ms pauses,
  // when the host stretched each of those pauses far past 20 ms).
  const code = `
    import { failAndExit } from ${JSON.stringify(mod)};
    for (let i = 0; i < 1600; i++) process.stderr.write("engine log line " + i + " " + "x".repeat(90) + "\\n");
    failAndExit("SDCPP VIDEO", new Error("GPU_RESET: the GPU reset"));`;
  const { spawn } = await import("node:child_process");
  const child = spawn(process.execPath, ["--input-type=module", "-e", code], { stdio: ["ignore", "ignore", "pipe"] });
  let err = "";
  // a slow reader: one chunk, then a short pause - the child's writes outrun it and queue up
  child.stderr.on("data", (d) => {
    err += d;
    child.stderr.pause();
    setTimeout(() => child.stderr.resume(), 5);
  });
  const status = await new Promise((r) => child.on("close", r));
  assert.equal(status, 1);
  const lines = err.trim().split(/\r?\n/);
  assert.equal(lines[lines.length - 1], "IGPU_CLASS=gpu_reset", "the class line is the last line and it arrived");
  assert.match(lines[lines.length - 2], /^SDCPP VIDEO FAILED: GPU_RESET:/);
  assert.ok(err.includes("engine log line 1599 "), "and so did all the log before it");
  assert.ok(err.length > 2 * 65536, "the premise: more than two pipe buffers went through a reader that could not take it at once");
});
