// node --test render/igpu-engine.test.mjs
// The shared iGPU-runner plumbing: the no-CPU guards (backend allowlist, extra-args screen,
// the POSITIVE GPU-evidence log guard run against REAL engine logs, GPU_RESET), the token
// cap, the kill semantics (the engine process must be dead, not just the promise rejected),
// the lifecycle (signal / parent gone) and the 4k+1 / %32 normalization. The "engine" in the
// runEngine tests is a node one-liner; no real engine, GPU or lease is touched.
import { test } from "node:test";
import assert from "node:assert";
import { readFileSync, existsSync, writeFileSync, mkdtempSync, rmSync, mkdirSync } from "node:fs";
import { spawn } from "node:child_process";
import { tmpdir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";
import { dirname, join } from "node:path";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, screenExtraArgs, refuseExtraArgs, createLogGuard, scanLog,
  runEngine, normalizeFrames, floorFrames, normalizeSize, vulkanDeviceFromBackend, finiteNum, mp4Args, makeTempDir,
  latentTokens, checkTokenCap, tokenCapFromFlags, makeDeadline, processStartMs,
  refuseRelativeBinary, ensureOutDir, modelMetadataError, encodeMp4,
  BINARY_NOT_ABSOLUTE, OUT_DIR_UNWRITABLE, MODEL_INCOMPATIBLE,
  CPU_PLACEMENT, CPU_BACKEND_REFUSED, GPU_RESET, TOKEN_CAP_EXCEEDED, EXTRA_ARGS_REFUSED, ILLEGAL_INSTRUCTION,
} from "./igpu-engine.mjs";
import { DERIVED, derive } from "./testdata/derive-cpu-fixtures.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const fixturePath = (n) => join(here, "testdata", n);
const fixture = (n) => readFileSync(fixturePath(n), "utf8");
const isWin = process.platform === "win32";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function alive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}
async function waitGone(pid, ms = 4000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (!alive(pid)) return true;
    await sleep(50);
  }
  return !alive(pid);
}
async function waitFile(p, ms = 8000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (existsSync(p)) {
      const s = readFileSync(p, "utf8");
      if (s.trim() !== "") return s;
    }
    await sleep(25);
  }
  throw new Error("timed out waiting for " + p);
}
const scratch = () => mkdtempSync(join(tmpdir(), "igpu-engine-test-"));

// ---------------------------------------------------------------- backend allowlist

test("refuseCpuBackend: only vulkan / vulkanN is accepted; cpu, unset, best/auto, other backends and typos are refused", () => {
  for (const b of ["cpu", "CPU", " cpu ", "cpu0", "", "  ", undefined, "diffusion=vulkan0,vae=cpu", "clip=cpu", "vulkan0,cpu",
    "diffusion=cuda0&cpu", "best", "auto", "diffusion=best", "blas", "opencl", "rpc", "vulcan", "cuda0", "vulkan0 vulkan1", "vulkan-0"]) {
    assert.throws(() => refuseCpuBackend(b), new RegExp(CPU_BACKEND_REFUSED), `backend ${JSON.stringify(b)} must be refused`);
  }
  for (const b of ["vulkan0", "Vulkan1", "diffusion=vulkan0,vae=vulkan0", "diffusion=vulkan0&vulkan1", "vulkan"]) {
    assert.equal(refuseCpuBackend(b), b);
  }
});

// ---------------------------------------------------------------- extra args screen

test("screenExtraArgs: anything that changes the backend or the placement is refused, in every spelling", () => {
  const bad = [
    ["--backend", "cpu"], ["--backend", "vulkan0"], ["--backend=vulkan0"], ["-b", "vulkan0"], ["-b=cpu"], ["--BACKEND", "x"],
    ["--params-backend", "cpu"], ["--params-backend=vulkan0"], ["--clip-on-cpu"], ["--vae-on-cpu"],
    ["--control-net-cpu"], ["--rpc", "192.0.2.1:50052"], ["--rpc=192.0.2.1:50052"], ["--cpu-moe"], ["--n-cpu-moe", "8"],
    ["--offload-params-to-cpu"], ["--offload-to-cpu=cpu"], ["--some-flag", "cpu"], ["--some-flag", "CPU0"], ["--assign=te=cpu"], ["--assign", "te=cpu,vae=vulkan0"],
    ["--assign", "diffusion=vulkan0&cpu"],
  ];
  for (const a of bad) {
    const hit = screenExtraArgs(a, { engine: "sdcpp" });
    assert.ok(hit, `${JSON.stringify(a)} must be refused`);
    assert.throws(() => refuseExtraArgs(a, { engine: "sdcpp", key: "sdcpp_extra_args" }), new RegExp(EXTRA_ARGS_REFUSED));
  }
  // --device is the audiocpp_device key's, but only audio.cpp has it
  assert.ok(screenExtraArgs(["--device", "1"], { engine: "audiocpp" }));
  assert.ok(screenExtraArgs(["--device=1"], { engine: "audiocpp" }));
  assert.equal(screenExtraArgs(["--device", "1"], { engine: "sdcpp" }), null);
  // --offload-to-cpu is sanctioned spill (weights parked in RAM, staged to the device, all compute on
  // the GPU): accepted on every engine, and it never masks a placement flag that follows it
  for (const engine of ["sdcpp", "da3", "audiocpp"]) {
    assert.equal(screenExtraArgs(["--vae-tiling", "--offload-to-cpu", "--diffusion-fa"], { engine }), null, engine);
    assert.equal(screenExtraArgs(["--OFFLOAD-TO-CPU"], { engine }), null, engine);
  }
  assert.deepEqual(refuseExtraArgs(["--offload-to-cpu"], { engine: "sdcpp" }), ["--offload-to-cpu"]);
  assert.equal(screenExtraArgs(["--offload-to-cpu", "--clip-on-cpu"], { engine: "sdcpp" }).index, 1);
  const good = [["--vae-tiling"], ["--vae-tile-overlap", "0.25"], ["--flag with space"], ["--diffusion-fa"], ["--lora-model-dir", "/models/loras"], ["--threads", "4"], []];
  for (const a of good) assert.equal(screenExtraArgs(a, { engine: "sdcpp" }), null, JSON.stringify(a));
  assert.deepEqual(refuseExtraArgs(["--threads", "4"], { engine: "da3" }), ["--threads", "4"]);
  const hit = screenExtraArgs(["--threads", "4", "--clip-on-cpu"]);
  assert.equal(hit.index, 2);
  assert.equal(hit.arg, "--clip-on-cpu");
});

// ---------------------------------------------------------------- the positive guard on REAL logs

const SD_HEALTHY = ["sdcpp-video-healthy.log", "sdcpp-video-tae.log"];

test("scanLog: each real healthy sd.cpp log passes (device line + a diffusion-stage compute buffer on Vulkan)", () => {
  for (const f of SD_HEALTHY) {
    const r = scanLog(fixture(f), { engine: "sdcpp" });
    assert.equal(r.fatal, null, `${f}: ${JSON.stringify(r.fatal)}`);
    assert.equal(r.verdict.ok, true, f);
    assert.ok(r.verdict.evidence.some((e) => /Wan2\.2-TI2V-5B compute on Vulkan0/.test(e)), f);
  }
});

test("scanLog: the real da3-cli and audio.cpp voice logs pass; the host-only-tensors line is not a placement", () => {
  const da = scanLog(fixture("da3-healthy.log"), { engine: "da3" });
  assert.equal(da.fatal, null);
  assert.equal(da.verdict.ok, true);
  assert.match(fixture("da3-healthy.log"), /host-only tensors kept on CPU/, "the fixture must carry the line that must not trip");
  const au = scanLog(fixture("audiocpp-voice-clone.log"), { engine: "audiocpp" });
  assert.equal(au.fatal, null);
  assert.equal(au.verdict.ok, true);
});

test("scanLog: the REAL audio.cpp host-prefill log (planner weights on CPU) is CPU_PLACEMENT at that line", () => {
  const r = scanLog(fixture("audiocpp-music-host-prefill.log"), { engine: "audiocpp" });
  assert.ok(r.fatal, "a *.weights.buffer_name CPU line must be fatal");
  assert.equal(r.fatal.kind, CPU_PLACEMENT);
  assert.match(r.fatal.line, /ace_step\.planner\.weights\.buffer_name CPU/);
});

test("scanLog: any <component>.weights.buffer_name CPU line is CPU_PLACEMENT, whichever component", () => {
  for (const c of ["chatterbox.t3", "ace_step.dit", "framework.hift", "x.y.z"]) {
    const r = scanLog(`ggml_vulkan: Found 1 Vulkan devices:\n[TIMING ts=1] ${c}.weights.buffer_name CPU\n`, { engine: "audiocpp" });
    assert.equal(r.fatal?.kind, CPU_PLACEMENT, c);
  }
});

test("scanLog: the derived sd.cpp negatives each fail, and each checked-in file is exactly what its source derives to", () => {
  for (const d of DERIVED) {
    const text = fixture(d.out);
    assert.equal(text.split("\n")[0], `# derived from ${d.from} by substituting the device; no CPU run was captured because the node's operator forbids model compute on its CPU`);
    assert.equal(text, derive(fixture(d.from), d.from, d.mode), `${d.out} drifted from ${d.from}: run render/testdata/derive-cpu-fixtures.mjs`);
    const r = scanLog(text, { engine: "sdcpp" });
    assert.ok(r.fatal, `${d.out} must be fatal`);
    assert.equal(r.fatal.kind, CPU_PLACEMENT);
    assert.match(r.fatal.line, /compute buffer size: .*\(RAM\) on CPU/, d.out);
  }
});

test("scanLog: params in host RAM with the compute on Vulkan is the sanctioned overflow, not a placement", () => {
  const text = fixture("sdcpp-video-healthy.log")
    .split("\n").map((l) => (/prepared params backend buffers/.test(l) ? l.replace(/VRAM\) on Vulkan0/, "RAM) on CPU") : l)).join("\n");
  assert.match(text, /RAM\) on CPU/);
  const r = scanLog(text, { engine: "sdcpp" });
  assert.equal(r.fatal, null);
  assert.equal(r.verdict.ok, true);
  const host = scanLog(fixture("sdcpp-video-healthy.log").replace(/VRAM\) on Vulkan0\s*$/m, "RAM) on Vulkan_Host"), { engine: "sdcpp" });
  assert.equal(host.fatal, null, "Vulkan_Host is host-pinned memory, not a CPU placement");
  assert.equal(host.verdict.ok, true, "--offload-to-cpu's log shape (params on the host, compute on Vulkan) is a pass");
  // the same host-resident params with a diffusion-stage COMPUTE buffer on the CPU is still a placement
  const hostCompute = fixture("sdcpp-video-healthy.log").replace(/VRAM\) on Vulkan0\s*$/m, "RAM) on Vulkan_Host")
    .replace(/(Wan\S* compute buffer size: [\d.]+ MB)\(VRAM\) on Vulkan0/, "$1(RAM) on CPU");
  assert.match(hostCompute, /compute buffer size: .*\(RAM\) on CPU/);
  const bad = scanLog(hostCompute, { engine: "sdcpp" });
  assert.equal(bad.fatal?.kind, CPU_PLACEMENT, "params on the host never excuses compute on the CPU");
});

test("scanLog: no positive line fails (sd.cpp, da3, audio.cpp) and the message says no GPU evidence was seen", () => {
  const regs = "load_backend: loaded Vulkan backend from /opt/x/libggml-vulkan.so\nload_backend: loaded CPU backend from /opt/x/libggml-cpu-haswell.so\n[VERBOSE] ggml_extend_backend.cpp:404  - Initializing backend: CPU\n";
  for (const [engine, text] of [
    ["sdcpp", regs], ["sdcpp", ""], ["da3", regs], ["audiocpp", regs],
    // device seen, but no diffusion-stage compute buffer on the GPU
    ["sdcpp", "ggml_vulkan: Found 1 Vulkan devices:\nggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1\n[VERBOSE] ggml_runner.cpp:1019 - t5 compute buffer size: 297.00 MB(VRAM) on Vulkan0 (peak across 1 segment)\n"],
    // da3 on a non-Vulkan device line is a CPU run
  ]) {
    const r = scanLog(text, { engine });
    assert.equal(r.fatal, null, `${engine}: registration lines are not fatal`);
    assert.equal(r.verdict.ok, false, `${engine} must not pass without positive evidence`);
  }
  const g = createLogGuard({ engine: "da3" });
  g.scan("ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1");
  assert.equal(g.verdict().ok, false, "a ggml_vulkan device line alone is not da3 evidence");
});

test("scanLog: no devices, a software Vulkan device and a CPU backend line are all CPU", () => {
  const cpu = [
    "ggml_vulkan: Found 0 Vulkan devices:",
    "ggml_vulkan: 0 = llvmpipe (LLVM 17.0.6, 256 bits) (llvmpipe) | uma: 0 | fp16: 1",
    "ggml_vulkan: 0 = Lavapipe (lavapipe) | uma: 0",
    "ggml_vulkan: 0 = SwiftShader Device (swiftshader) | uma: 0",
    "[WARN   ] ggml_extend_backend.cpp:676 - loading CPU backend",
    "[VERBOSE] ggml_extend_backend.cpp:681 - Using CPU backend",
    "[ERROR  ] ggml_extend_backend.cpp:635 - No devices found!",
    "[INFO   ] backend_fit.cpp:346  -     DiT          params   5162 MiB, compute reserve  2048 MiB -> compute CPU, params RAM",
    "[WARN   ] backend_fit.cpp:446 - auto-fit: no GPU memory budget available; using CPU",
    "[VERBOSE] ggml_runner.cpp:1019 - t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)",
    "[VERBOSE] ggml_runner.cpp:1019 - Wan2.2-TI2V-5B compute buffer size: 297.00 MB(RAM) on CPU0 (peak across 1 segment)",
  ];
  for (const l of cpu) assert.equal(scanLog(l, { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT, l);
  for (const l of cpu.slice(0, 4)) {
    for (const engine of ["da3", "audiocpp"]) assert.equal(scanLog(l, { engine }).fatal?.kind, CPU_PLACEMENT, `${engine}: ${l}`);
  }
  assert.equal(scanLog("[da3] da::Backend using device: CPU", { engine: "da3" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("[da3] offload_weights: 239 weights -> CPU", { engine: "da3" }).fatal?.kind, CPU_PLACEMENT);
});

test("scanLog: one real-shape line per pattern, so no pattern shadows another", () => {
  // every SD_CPU_SHAPES entry alone trips (the previous detector's patterns shadowed each other)
  const one = {
    "loading CPU backend": "[WARN   ] loading CPU backend",
    "Using CPU backend": "[VERBOSE] Using CPU backend",
    "No devices found": "[ERROR  ] No devices found!",
    "plan": "-> compute CPU, params RAM",
    "auto-fit": "auto-fit: no GPU memory budget available; using CPU",
  };
  for (const [k, l] of Object.entries(one)) assert.ok(scanLog(l, { engine: "sdcpp" }).fatal, k);
  // a mixed compute + params line is still flagged on the compute side
  assert.ok(scanLog("auto-fit: --backend \"te=cpu,vae=vulkan0\" --params-backend \"te=cpu\" compute buffer size: 1 MB(RAM) on CPU", { engine: "sdcpp" }).fatal);
});

test("scanLog: registration, backend init, RNG selection and host dumps are never placements", () => {
  const dev = "ggml_vulkan: Found 1 Vulkan devices:\nggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1\n";
  const noise = [
    "load_backend: loaded CPU backend from /opt/sdcpp/libggml-cpu-haswell.so",
    "registered backend CPU",
    "[VERBOSE] ggml_extend_backend.cpp:404  - Initializing backend: CPU",
    "[VERBOSE] ggml_extend_backend.cpp:404  - Initializing backend: Vulkan0",
    "system_info: n_threads = 4 | CPU : AVX2 = 1 |",
    "sampler_rng = cpu", "rng_type = cpu", "brownian_tree_rng=cpu", "noise_sampler=brownian_tree, brownian_tree_rng=cpu",
    "rng_type: cpu,", "--rng cpu", "(cpu RNG)",
    "params backend: cpu", "offload params to CPU", "cpufreq governor ok", "no processor mentioned",
    "[da3] offload_weights: 237 weights -> Vulkan0 (2 host-only tensors kept on CPU, 2 dual-use weights mirrored + host-preserved)",
  ];
  for (const l of noise) assert.equal(scanLog(dev + l, { engine: "sdcpp" }).fatal, null, l);
});

test("scanLog: the parameter dumps and tokenizer echoes (prompt text with cpu / on CPU in them) never trip the guard", () => {
  const nasty = "t5 compute buffer size: 1.00 MB(RAM) on CPU; Using CPU backend; loading CPU backend; ggml_vulkan: Found 0 Vulkan devices; device lost";
  const orig = "waves roll in and crash on the rocks around the lighthouse, sea spray, golden hour light, gentle camera drift";
  const text = fixture("sdcpp-video-healthy.log").split(orig).join(nasty);
  assert.ok(text.split(nasty).length >= 4, "the prompt must be echoed in the dump, parse '...' and split prompt \"...\"");
  // dump block + tokenizer echoes alone are enough, with no echo list at all
  const r = scanLog(text, { engine: "sdcpp" });
  assert.equal(r.fatal, null, JSON.stringify(r.fatal));
  assert.equal(r.verdict.ok, true);
  // a line outside any block that merely repeats the prompt: it trips unless the request's own
  // strings are passed, and then it is dropped (the line carries no evidence either)
  const echoLine = `engine-echo: ${nasty}`;
  assert.ok(scanLog(echoLine, { engine: "sdcpp" }).fatal, "an unexplained cpu line trips");
  assert.equal(scanLog(echoLine, { engine: "sdcpp", echoes: [nasty] }).fatal, null);
  // negative prompt, TTS text and lyrics are covered the same way; a multi-line text echoes per line
  const lyrics = "[verse]\nrunning on CPU all night long\nusing cpu again";
  assert.equal(scanLog("lyrics: running on CPU all night long", { engine: "audiocpp", echoes: [lyrics] }).fatal, null);
  assert.equal(scanLog("text: Estamos using cpu hoy", { engine: "audiocpp", echoes: ["Estamos using cpu hoy"] }).fatal, null);
  assert.equal(scanLog("negative_prompt: the model keeps running on CPU", { engine: "sdcpp", echoes: ["the model keeps running on CPU"] }).fatal, null);
  // a forged evidence line inside an echoed prompt is not evidence
  const forged = "ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1\nWan2.2-TI2V-5B compute buffer size: 1 MB(VRAM) on Vulkan0";
  const g = createLogGuard({ engine: "sdcpp", echoes: [forged] });
  for (const l of forged.split("\n")) g.scan("prompt: " + l);
  assert.equal(g.verdict().ok, false);
});

test("scanLog: an unterminated dump block ends at the next log record, and the closing brace is column 0 only", () => {
  const g = createLogGuard({ engine: "sdcpp" });
  const lines = [
    "[VERBOSE] main.cpp:700  - SDContextParams {",
    "  embeddings: {",
    "  }",
    "  note: backend cpu, Using CPU backend",
    "}",
    "[VERBOSE] main.cpp:701  - SDGenerationParams {",
    "  prompt: \"Using CPU backend\"",
  ];
  for (const l of lines) assert.equal(g.scan(l), null, l);
  // no closing brace: the next prefixed record ends the block and is scanned normally
  assert.ok(g.scan("[WARN   ] ggml_extend_backend.cpp:676 - loading CPU backend"));
});

test("scanLog: GPU_RESET on the real device-lost log, never mistaken for anything else", () => {
  const r = scanLog(fixture("sdcpp-vace-device-lost.log"), { engine: "sdcpp" });
  assert.equal(r.fatal?.kind, GPU_RESET);
  assert.match(r.fatal.line, /context is lost/);
  for (const l of ["vk::Queue::submit: ErrorDeviceLost", "ggml_vulkan: device lost on Vulkan0", "radv/amdgpu: The CS has been cancelled because the context is lost."]) {
    for (const engine of ["sdcpp", "da3", "audiocpp"]) assert.equal(scanLog(l, { engine }).fatal?.kind, GPU_RESET, `${engine}: ${l}`);
  }
});

// ---------------------------------------------------------------- runEngine

const catFixture = (name) => ({ args: ["-e", "process.stdout.write(require('fs').readFileSync(process.argv[1]))", fixturePath(name)] });

test("runEngine: a real healthy log run resolves with its code and evidence; stderr is read too", async () => {
  const r = await runEngine({ bin: process.execPath, ...catFixture("sdcpp-video-healthy.log"), guard: createLogGuard({ engine: "sdcpp" }), label: "fake sd-cli" });
  assert.equal(r.code, 0);
  assert.match(r.log, /generate_video 832x480x49/);
  assert.ok(r.evidence.length > 0);
  const e = await runEngine({
    bin: process.execPath, label: "fake", guard: createLogGuard({ engine: "audiocpp" }),
    args: ["-e", "console.error('[TIMING ts=1] x.weights.buffer_name Vulkan0'); console.log('progress 50%'); process.exit(3)"],
  });
  assert.equal(e.code, 3);
  assert.match(e.log, /progress 50%/);
});

test("runEngine: a clean exit with no GPU evidence rejects CPU_PLACEMENT, whatever the engine printed", async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "console.log('all done quietly')"], guard: createLogGuard({ engine: "audiocpp" }), label: "quiet" }),
    (e) => new RegExp(CPU_PLACEMENT).test(e.message) && /no GPU evidence was seen/.test(e.message) && /quiet/.test(e.message),
  );
});

test("runEngine: runEngine refuses to run without a guard", () => {
  assert.throws(() => runEngine({ bin: process.execPath, args: ["-e", "0"] }), /log guard/);
});

test("runEngine: the first CPU line kills the engine AND its grandchild, and both are dead when the promise rejects", async () => {
  const dir = scratch();
  try {
    const pidFile = join(dir, "engine.pid");
    const gcFile = join(dir, "grandchild.pid");
    const script = [
      "const fs=require('fs'),cp=require('child_process');",
      "const c=cp.spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});",
      "fs.writeFileSync(process.argv[1],String(process.pid));fs.writeFileSync(process.argv[2],String(c.pid));",
      "console.log('ggml_vulkan: Found 1 Vulkan devices:');console.log('t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)');",
      "setInterval(()=>{},1000);",
    ].join("");
    const t0 = Date.now();
    await assert.rejects(
      runEngine({ bin: process.execPath, args: ["-e", script, pidFile, gcFile], timeoutMs: 30000, label: "fake", guard: createLogGuard({ engine: "sdcpp" }) }),
      (e) => new RegExp(CPU_PLACEMENT).test(e.message) && /on CPU/.test(e.message),
    );
    assert.ok(Date.now() - t0 < 20000, "killed on the line, not at the timeout");
    const pid = Number(readFileSync(pidFile, "utf8"));
    const gc = Number(readFileSync(gcFile, "utf8"));
    assert.ok(await waitGone(pid), "the engine process must be dead");
    assert.ok(await waitGone(gc), "the engine's grandchild must be dead");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("runEngine: a placement line printed on stderr, and a final line with no trailing newline, are both caught", async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "console.error('ggml_vulkan: Found 0 Vulkan devices:'); setInterval(()=>{},1000)"], timeoutMs: 20000, guard: createLogGuard({ engine: "sdcpp" }) }),
    new RegExp(CPU_PLACEMENT),
  );
  await assert.rejects(
    runEngine({
      bin: process.execPath, timeoutMs: 20000, guard: createLogGuard({ engine: "audiocpp" }),
      args: ["-e", "console.log('[TIMING ts=1] a.weights.buffer_name Vulkan0'); process.stdout.write('[TIMING ts=2] b.weights.buffer_name CPU')"],
    }),
    (e) => /placed a model on the CPU/.test(e.message) && /buffer_name CPU/.test(e.message),
  );
});

test("runEngine: a lost GPU is GPU_RESET (the real log), naming the 2 s lockup timeout and the token cap; the engine is dead", async () => {
  const dir = scratch();
  try {
    const pidFile = join(dir, "pid");
    const script = "require('fs').writeFileSync(process.argv[2],String(process.pid));process.stdout.write(require('fs').readFileSync(process.argv[1]));setInterval(()=>{},1000)";
    await assert.rejects(
      runEngine({ bin: process.execPath, args: ["-e", script, fixturePath("sdcpp-vace-device-lost.log"), pidFile], timeoutMs: 30000, guard: createLogGuard({ engine: "sdcpp" }), label: "sd-cli" }),
      (e) => new RegExp(GPU_RESET).test(e.message) && /2 s lockup timeout/.test(e.message) && /sdcpp_max_tokens/.test(e.message) && /never retried/.test(e.message),
    );
    assert.ok(await waitGone(Number(readFileSync(pidFile, "utf8"))));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("runEngine: a GPU_RESET engine that exits 1 by itself is still GPU_RESET, not 'exited 1'", async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, ...catFixture("sdcpp-vace-device-lost.log"), guard: createLogGuard({ engine: "sdcpp" }) }),
    (e) => new RegExp(GPU_RESET).test(e.message),
  );
});

test("runEngine: the timeout kills the engine and its tree, and the engine is dead when the promise rejects", async () => {
  const dir = scratch();
  try {
    const pidFile = join(dir, "pid");
    await assert.rejects(
      runEngine({
        bin: process.execPath, args: ["-e", "require('fs').writeFileSync(process.argv[1],String(process.pid));setInterval(()=>{},1000)", pidFile],
        timeoutMs: 700, label: "slow", guard: createLogGuard({ engine: "audiocpp" }),
      }),
      /slow timeout after/,
    );
    assert.ok(await waitGone(Number(readFileSync(pidFile, "utf8"))));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("runEngine: exit 132 is ILLEGAL_INSTRUCTION naming the instruction-set mismatch", async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "process.exit(132)"], guard: createLogGuard({ engine: "audiocpp" }), label: "audiocpp_cli" }),
    (e) => new RegExp(ILLEGAL_INSTRUCTION).test(e.message) && /instruction-set mismatch/.test(e.message) && /AVX-512/.test(e.message),
  );
});

test("runEngine: death by SIGILL / SIGKILL carries the signal name; SIGKILL names the OOM killer", { skip: isWin && "POSIX signals" }, async () => {
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "process.kill(process.pid,'SIGILL');setInterval(()=>{},1000)"], guard: createLogGuard({ engine: "audiocpp" }), label: "audiocpp_cli" }),
    (e) => new RegExp(ILLEGAL_INSTRUCTION).test(e.message) && /SIGILL/.test(e.message),
  );
  await assert.rejects(
    runEngine({ bin: process.execPath, args: ["-e", "process.kill(process.pid,'SIGKILL');setInterval(()=>{},1000)"], guard: createLogGuard({ engine: "audiocpp" }), label: "sd-cli" }),
    (e) => /killed by signal SIGKILL/.test(e.message) && /OOM/.test(e.message),
  );
});

test("runEngine: a binary that cannot start rejects with its label", async () => {
  await assert.rejects(runEngine({ bin: join(here, "no-such-engine-binary"), args: [], label: "ghost", guard: createLogGuard({ engine: "sdcpp" }) }), /ghost failed to start/);
});

// ---------------------------------------------------------------- lifecycle

// A harness the lifecycle tests spawn: it installs the lifecycle, makes a temp dir and runs a
// hanging engine under runEngine (the engine prints GPU evidence so the guard is content).
function writeHarness(dir) {
  const mod = pathToFileURL(join(here, "igpu-engine.mjs")).href;
  const file = join(dir, "harness.mjs");
  writeFileSync(file, `
import { installLifecycle, runEngine, createLogGuard, makeTempDir } from ${JSON.stringify(mod)};
import { writeFileSync } from "node:fs";
installLifecycle({ pollMs: 100 });
const t = makeTempDir("igpu-lifecycle-test-");
const info = process.argv[2];
const engine = "const fs=require('fs');fs.writeFileSync(process.argv[1],String(process.pid));console.log('[TIMING ts=1] x.weights.buffer_name Vulkan0');setInterval(()=>{},1000);";
const enginePid = info + ".engine";
runEngine({ bin: process.execPath, args: ["-e", engine, enginePid], guard: createLogGuard({ engine: "audiocpp" }), label: "hang" }).catch(() => {});
writeFileSync(info, JSON.stringify({ harness: process.pid, tempDir: t.dir }));
`);
  return file;
}

test("lifecycle: SIGTERM to the runner kills its engine and removes its temp dir", { skip: isWin && "POSIX signals" }, async () => {
  const dir = scratch();
  try {
    const info = join(dir, "info.json");
    const h = spawn(process.execPath, [writeHarness(dir), info], { stdio: "ignore" });
    const { tempDir } = JSON.parse(await waitFile(info));
    const enginePid = Number(await waitFile(info + ".engine"));
    assert.ok(existsSync(tempDir));
    h.kill("SIGTERM");
    assert.ok(await waitGone(h.pid), "the runner must exit");
    assert.ok(await waitGone(enginePid), "the engine must die with the runner");
    assert.ok(!existsSync(tempDir), "the temp dir must be removed");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("lifecycle: when the parent process disappears the runner kills its engine, removes its temp dir and exits", async () => {
  const dir = scratch();
  try {
    const info = join(dir, "info.json");
    const harness = writeHarness(dir);
    // P spawns H detached, waits until H and its engine are up, then exits: H's parent is gone
    // while H (and its engine) are running
    const p = spawn(process.execPath, ["-e",
      `const fs=require('fs');const c=require('child_process').spawn(process.execPath,[${JSON.stringify(harness)},${JSON.stringify(info)}],{stdio:'ignore',detached:true});c.unref();` +
      `const t=setInterval(()=>{if(fs.existsSync(${JSON.stringify(info + ".engine")})&&fs.existsSync(${JSON.stringify(info)})){clearInterval(t);setTimeout(()=>process.exit(0),200);}},50);`,
    ], { stdio: "ignore" });
    await new Promise((r) => p.on("close", r));
    const { harness: hpid, tempDir } = JSON.parse(await waitFile(info));
    const enginePid = Number(await waitFile(info + ".engine"));
    assert.ok(await waitGone(hpid, 40000), "the orphaned runner must notice and exit");
    assert.ok(await waitGone(enginePid), "the engine must be killed");
    assert.ok(!existsSync(tempDir), "the temp dir must be removed");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

// ---------------------------------------------------------------- token cap

test("latentTokens: the shared table (render/testdata/token-cap-table.json) — the Go twin reads the same file", () => {
  const t = JSON.parse(fixture("token-cap-table.json"));
  assert.ok(t.rows.some((r) => r.tokens === 5070 && r.stride === 16));
  assert.ok(t.rows.some((r) => r.tokens === 15600 && r.stride === 8 && r.ref === 1));
  assert.ok(t.rows.some((r) => r.tokens === 5760 && r.stride === 8 && r.ref === 1));
  for (const r of t.rows) {
    assert.equal(latentTokens({ width: r.width, height: r.height, frames: r.frames, stride: r.stride, refLatentFrames: r.ref }), r.tokens, r.note);
  }
});

test("checkTokenCap: no cap configured = no check; over the cap names tokens, cap and how to fit; stride is required", () => {
  assert.equal(checkTokenCap({ flags: {}, width: 4096, height: 4096, frames: 121 }), 0);
  assert.equal(checkTokenCap({ flags: { "max-tokens": "5070", "vae-stride": "16" }, width: 832, height: 480, frames: 49 }), 5070);
  assert.throws(
    () => checkTokenCap({ flags: { "max-tokens": "5000", "vae-stride": "16" }, width: 832, height: 480, frames: 49 }),
    (e) => new RegExp(TOKEN_CAP_EXCEEDED).test(e.message) && /5070/.test(e.message) && /cap is 5000/.test(e.message) && /up to 45 frames fit/.test(e.message) && /Not retried/.test(e.message),
  );
  assert.throws(
    () => checkTokenCap({ flags: { "max-tokens": "3000", "vae-stride": "8" }, width: 480, height: 832, frames: 33, refLatentFrames: 1 }),
    (e) => /15600/.test(e.message) && /even 5 frames do not fit/.test(e.message) && /reference/.test(e.message),
  );
  assert.throws(() => checkTokenCap({ flags: { "max-tokens": "5000" }, width: 64, height: 64, frames: 5 }), /--vae-stride must be 8 or 16/);
  assert.throws(() => checkTokenCap({ flags: { "max-tokens": "5000", "vae-stride": "4" }, width: 64, height: 64, frames: 5 }), /--vae-stride must be 8 or 16/);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "0", "vae-stride": "8" }), /positive integer/);
  assert.throws(() => tokenCapFromFlags({ "max-tokens": "-3", "vae-stride": "8" }), /positive integer/);
  assert.equal(tokenCapFromFlags({}), null);
});

// ---------------------------------------------------------------- deadline

test("makeDeadline: counts from the process start (pre-spawn work spends it) and refuses a step once spent", () => {
  const d0 = makeDeadline(0);
  assert.equal(d0.active, false);
  assert.equal(d0.remainingMs(), 0);
  d0.enforce("x");
  const now = Date.now();
  const d = makeDeadline(100, now - 90_000);
  assert.ok(d.remainingMs() > 0 && d.remainingMs() <= 10_000, `remaining ${d.remainingMs()}`);
  d.enforce("fine");
  const spent = makeDeadline(100, now - 101_000);
  assert.equal(spent.remainingMs(), 1);
  assert.throws(() => spent.enforce("sd-cli"), /sd-cli timeout: the 100s budget/);
  assert.ok(processStartMs() <= Date.now());
});

// ---------------------------------------------------------------- helpers

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

test("parseArgs: a bare -- ends flag parsing, so a prompt or lyrics starting with -- stays positional (G11)", () => {
  const { pos, flags } = parseArgs(["--model", "/m.gguf", "--no-lock", "--", "out.wav", "--- Intro ---", "--seed", "-- x"]);
  assert.deepEqual(pos, ["out.wav", "--- Intro ---", "--seed", "-- x"]);
  assert.equal(flags.model, "/m.gguf");
  assert.equal(flags["no-lock"], true);
  assert.ok(!("seed" in flags) && !("Intro ---" in flags));
  // without the terminator the same words are flags: the harness must send it
  assert.deepEqual(parseArgs(["out.wav", "--seed", "5"]).pos, ["out.wav"]);
  // a single dash and the lone "--" at the end are harmless
  assert.deepEqual(parseArgs(["-x", "--"]).pos, ["-x"]);
});

test("refuseRelativeBinary: ONE resolution rule - an absolute existing path passes; a bare name or a relative path is BINARY_NOT_ABSOLUTE; a missing file is 'not found' (G10/G23)", () => {
  const d = scratch();
  try {
    const bin = join(d, "sd-cli");
    writeFileSync(bin, "x");
    assert.equal(refuseRelativeBinary("--sd-bin", bin), bin);
    for (const bare of ["sd-cli", "./sd-cli", "bin/sd-cli", "", undefined]) {
      assert.throws(() => refuseRelativeBinary("--sd-bin", bare), (e) => e.message.startsWith(BINARY_NOT_ABSOLUTE) && e.message.includes("--sd-bin"), JSON.stringify(bare));
    }
    assert.throws(() => refuseRelativeBinary("--depth-bin", join(d, "nope")), /--depth-bin not found/);
  } finally { rmSync(d, { recursive: true, force: true }); }
});

test("ensureOutDir: creates the output's directory before anything spawns; an uncreatable one is OUT_DIR_UNWRITABLE naming it (G27)", () => {
  const d = scratch();
  try {
    const out = join(d, "a", "b", "clip.mp4");
    assert.equal(ensureOutDir(out), join(d, "a", "b"));
    assert.ok(existsSync(join(d, "a", "b")));
    assert.equal(ensureOutDir(out), join(d, "a", "b"), "idempotent");
    writeFileSync(join(d, "file"), "x");
    assert.throws(() => ensureOutDir(join(d, "file", "clip.mp4")), (e) => e.message.startsWith(OUT_DIR_UNWRITABLE) && e.message.includes(join(d, "file")));
  } finally { rmSync(d, { recursive: true, force: true }); }
});

test("modelMetadataError: sd-cli's 'not in model metadata' / 'model metadata validation failed' is a typed MODEL_INCOMPATIBLE naming the model file and the missing tensor; any other log is null", () => {
  const log = [
    "[INFO ] model.cpp:1203 - loading tensors from /models/wan2.1-vace-1.3b-q8_0.gguf",
    "[ERROR] stable-diffusion.cpp: Diffusion model tensor 'model.diffusion_model.vace_patch_embedding.weight' not in model metadata",
    "[ERROR] model metadata validation failed",
  ].join("\n");
  const e = modelMetadataError(log, "/models/wan2.1-vace-1.3b-q8_0.gguf");
  assert.ok(e.message.startsWith(MODEL_INCOMPATIBLE));
  assert.ok(e.message.includes("/models/wan2.1-vace-1.3b-q8_0.gguf"));
  assert.match(e.message, /model\.diffusion_model\.vace_patch_embedding\.weight/);
  assert.match(e.message, /\.safetensors VACE model/);
  assert.match(modelMetadataError("model metadata validation failed", "m.gguf").message, /model metadata validation failed/);
  assert.equal(modelMetadataError("[INFO] sampling step 3/20", "m.gguf"), null);
  assert.equal(modelMetadataError(undefined, "m.gguf"), null);
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
  assert.ok(!a.includes("-vf"), "no trim unless asked");
  const t = mp4Args("in.webm", "out.mp4", 16, { trimFirst: 4 });
  assert.equal(t[t.indexOf("-vf") + 1], "trim=start_frame=4,setpts=PTS-STARTPTS", "drops the 4 reference-latent frames and restarts the clock");
  assert.ok(t.indexOf("-vf") > t.indexOf("-i") && t.indexOf("-vf") < t.indexOf("-c:v"));
});

test("encodeMp4: a missing ffmpeg is FFMPEG_UNAVAILABLE; a failing one carries its stderr", () => {
  assert.throws(() => encodeMp4("", "a.webm", "b.mp4", 16), /FFMPEG_UNAVAILABLE/);
  assert.throws(() => encodeMp4(process.execPath, "a.webm", join(tmpdir(), "igpu-no-such-dir-xyz", "b.mp4"), 16), /ffmpeg mp4 encode failed/);
});

test("makeTempDir: cleanup removes the directory and is idempotent", () => {
  const t = makeTempDir("igpu-test-");
  assert.ok(existsSync(t.dir));
  t.cleanup();
  assert.ok(!existsSync(t.dir));
  t.cleanup();
});

// ---------------------------------------------------------------- Go / Node parity

test("the screens agree with the shared parity table (internal/config reads the same file): empty per-module parts, unicode spaces", () => {
  const t = JSON.parse(fixture("screen-parity-table.json"));
  assert.ok(t.backend_refused.length >= 20 && t.extra_args_refused.length >= 10);
  for (const b of t.backend_refused) assert.throws(() => refuseCpuBackend(b), new RegExp(CPU_BACKEND_REFUSED), `backend ${JSON.stringify(b)}`);
  for (const b of t.backend_allowed) assert.equal(refuseCpuBackend(b), b.trim(), `backend ${JSON.stringify(b)}`);
  for (const a of t.extra_args_refused) assert.ok(screenExtraArgs(a, { engine: "sdcpp" }), `extra args ${JSON.stringify(a)} must be refused`);
  for (const a of t.extra_args_allowed) assert.equal(screenExtraArgs(a, { engine: "sdcpp" }), null, `extra args ${JSON.stringify(a)} must pass`);
});
