// node --test render/igpu-engine.test.mjs
// The shared iGPU-runner plumbing: the no-CPU guards (backend refusal + CPU_PLACEMENT log
// detection, including the live kill-on-first-line path), the 4k+1 / %32 normalization and
// the argv/JSON helpers. No real engine, GPU or lease is touched: the "engine" below is a
// node one-liner.
import { test } from "node:test";
import assert from "node:assert";
import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, detectCpuPlacement, detectCpuPlacementLine, runEngine,
  normalizeFrames, floorFrames, normalizeSize, vulkanDeviceFromBackend, finiteNum, mp4Args, makeTempDir,
  CPU_PLACEMENT, CPU_BACKEND_REFUSED,
} from "./igpu-engine.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const fixture = (n) => readFileSync(join(here, "testdata", n), "utf8");

test("refuseCpuBackend: cpu, unset and any per-module cpu assignment are refused", () => {
  for (const b of ["cpu", "CPU", " cpu ", "cpu0", "", "  ", undefined, "diffusion=vulkan0,vae=cpu", "clip=cpu", "vulkan0,cpu", "diffusion=cuda0&cpu", "best", "auto", "diffusion=best"]) {
    assert.throws(() => refuseCpuBackend(b), new RegExp(CPU_BACKEND_REFUSED), `backend ${JSON.stringify(b)} must be refused`);
  }
  for (const b of ["vulkan0", "Vulkan1", "cuda0", "diffusion=vulkan0,vae=vulkan0", "diffusion=cuda0&cuda1", "vulkan", "mycpu=vulkan0"]) {
    assert.equal(refuseCpuBackend(b), b);
  }
});

test("detectCpuPlacement: a captured-shape sd.cpp log with the text encoder on CPU is caught at its line", () => {
  const hit = detectCpuPlacement(fixture("sdcpp-vid-gen-cpu-placement.log"));
  assert.ok(hit, "CPU placement must be detected");
  assert.match(hit.line, /text encoder: Using CPU backend/);
  assert.equal(hit.lineNo, 8);
});

test("detectCpuPlacement: a Vulkan sd.cpp log is clean (host dump, loaded CPU backend, rng and params lines are not placements)", () => {
  assert.equal(detectCpuPlacement(fixture("sdcpp-vid-gen-vulkan.log")), null);
});

test("detectCpuPlacement: audio.cpp backend line", () => {
  const hit = detectCpuPlacement(fixture("audiocpp-cpu-placement.log"));
  assert.ok(hit);
  assert.match(hit.line, /backend: cpu/);
  assert.equal(detectCpuPlacement(fixture("audiocpp-vulkan.log")), null);
});

test("detectCpuPlacementLine: the shapes in and out", () => {
  const yes = [
    "[INFO ] clip: Using CPU backend",
    "diffusion model backend = CPU",
    "backend -> cpu0",
    "vae=cpu",
    "falling back to CPU",
    "fell back to CPU for the text encoder",
    "running on CPU",
    "Using CPU backend with 4 threads",
    "ggml_vulkan: 0 = llvmpipe (LLVM 17.0.6, 256 bits) (llvmpipe) | uma: 0 | fp16: 1",
    "Vulkan0: lavapipe (software rasterizer)",
  ];
  for (const l of yes) assert.equal(detectCpuPlacementLine(l), true, l);
  const no = [
    "",
    "no processor mentioned",
    "load_backend: loaded CPU backend from /x/libggml-cpu.so",
    "registered backend CPU",
    "system_info: n_threads = 4 | CPU : AVX2 = 1 |",
    "rng: cpu, sampler rng: cpu",
    "--rng cpu",
    "brownian_tree_rng cpu",
    "params backend: cpu",
    "offload params to CPU",
    "cpufreq governor ok",
    "Using Vulkan0 backend",
  ];
  for (const l of no) assert.equal(detectCpuPlacementLine(l), false, l);
});

test("runEngine: the first CPU-placement line kills the engine and rejects CPU_PLACEMENT, long before it would have finished", async () => {
  const script = "console.log('loading'); console.log('te: Using CPU backend'); setInterval(()=>{}, 1000);";
  const t0 = Date.now();
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", script], timeoutMs: 20000, label: "fake" }),
    (e) => new RegExp(CPU_PLACEMENT).test(e.message) && /Using CPU backend/.test(e.message),
  );
  assert.ok(Date.now() - t0 < 15000, "the engine must be killed on the line, not run to the timeout");
});

test("runEngine: a clean engine resolves with its code and log; stderr is read too", async () => {
  const r = await runEngine({
    bin: process.execPath, label: "fake",
    args: ["-e", "console.log('Using Vulkan0 backend'); console.error('progress 50%'); process.exit(3)"],
  });
  assert.equal(r.code, 3);
  assert.match(r.log, /Using Vulkan0 backend/);
  assert.match(r.log, /progress 50%/);
});

test("runEngine: a placement line printed on stderr is caught as well", async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "console.error('backend: cpu'); setInterval(()=>{},1000)"], timeoutMs: 20000 }),
    new RegExp(CPU_PLACEMENT),
  );
});

test("runEngine: timeout kills the engine", async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "setInterval(()=>{},1000)"], timeoutMs: 600, label: "slow" }),
    /slow timeout after/,
  );
});

test("runEngine: a binary that cannot start rejects with its label", async () => {
  await assert.rejects(runEngine({ bin: join(here, "no-such-engine-binary"), args: [], label: "ghost" }), /ghost failed to start/);
});

test("normalizeFrames: nearest 4k+1, ties up, minimum 5, default 49", () => {
  const cases = [[49, 49], [50, 49], [51, 53], [52, 53], [53, 53], [81, 81], [80, 81], [48, 49], [5, 5], [4, 5], [1, 5], [3, 5], [6, 5], [7, 9], [121, 121]];
  for (const [n, want] of cases) assert.equal(normalizeFrames(n), want, `normalizeFrames(${n})`);
  for (const bad of [undefined, null, 0, -4, NaN, "abc"]) assert.equal(normalizeFrames(bad), 49);
  assert.equal(normalizeFrames(undefined, 81), 81);
  assert.equal(normalizeFrames(undefined, 50), 49);
  for (let n = 1; n < 300; n++) assert.equal((normalizeFrames(n) - 1) % 4, 0, `n=${n}`);
});

test("floorFrames: the largest 4k+1 not above what exists, 0 under 5", () => {
  assert.equal(floorFrames(49), 49);
  assert.equal(floorFrames(52), 49);
  assert.equal(floorFrames(48), 45);
  assert.equal(floorFrames(5), 5);
  assert.equal(floorFrames(4), 0);
  assert.equal(floorFrames(0), 0);
});

test("normalizeSize: floored to a multiple of 32, minimum 32, default for junk", () => {
  assert.equal(normalizeSize(832, 1), 832);
  assert.equal(normalizeSize(854, 1), 832);
  assert.equal(normalizeSize(480, 1), 480);
  assert.equal(normalizeSize(481, 1), 480);
  assert.equal(normalizeSize(31, 1), 32);
  assert.equal(normalizeSize("720", 1), 704);
  for (const bad of [undefined, 0, -3, "x"]) assert.equal(normalizeSize(bad, 480), 480);
});

test("parseArgs: positionals, value flags and booleans", () => {
  const { pos, flags } = parseArgs(["out.mp4", "a prompt", "--model", "/m.gguf", "--no-lock", "--fps", "24"]);
  assert.deepEqual(pos, ["out.mp4", "a prompt"]);
  assert.equal(flags.model, "/m.gguf");
  assert.equal(flags.fps, "24");
  assert.equal(flags["no-lock"], true);
});

test("parseExtraArgs: a JSON string array, nothing else", () => {
  assert.deepEqual(parseExtraArgs(undefined), []);
  assert.deepEqual(parseExtraArgs(""), []);
  assert.deepEqual(parseExtraArgs('["--vae-tiling","--flag with space"]'), ["--vae-tiling", "--flag with space"]);
  assert.throws(() => parseExtraArgs("not json"), /JSON array/);
  assert.throws(() => parseExtraArgs('{"a":1}'), /JSON array/);
  assert.throws(() => parseExtraArgs("[1,2]"), /JSON array/);
});

test("vulkanDeviceFromBackend / finiteNum / mp4Args", () => {
  assert.equal(vulkanDeviceFromBackend("vulkan1"), "1");
  assert.equal(vulkanDeviceFromBackend("diffusion=vulkan2,vae=vulkan2"), "2");
  assert.equal(vulkanDeviceFromBackend("vulkan"), "");
  assert.equal(vulkanDeviceFromBackend(undefined), "");
  assert.equal(finiteNum("3.5"), 3.5);
  assert.equal(finiteNum(""), undefined);
  assert.equal(finiteNum("x"), undefined);
  const a = mp4Args("in.webm", "out.mp4", 24);
  for (const [k, v] of [["-c:v", "libx264"], ["-pix_fmt", "yuv420p"], ["-crf", "16"], ["-r", "24"]]) {
    assert.equal(a[a.indexOf(k) + 1], v);
  }
  assert.equal(a[a.length - 1], "out.mp4");
});

test("makeTempDir: cleanup removes the directory and is idempotent", () => {
  const t = makeTempDir("igpu-test-");
  assert.ok(existsSync(t.dir));
  t.cleanup();
  assert.ok(!existsSync(t.dir));
  t.cleanup();
});
