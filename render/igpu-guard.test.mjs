// node --test render/igpu-guard.test.mjs
// The positive GPU-evidence guard against request text and its own shapes (CT-49 round 2):
//  - the request's prompt / text / lyrics can never switch the CPU detector off, whatever fragment
//    of a real placement line they are (SIL9, TST3), and a forged evidence line inside them is not
//    evidence;
//  - the parameter-dump blocks belong to sd.cpp only, on its own record shape; audio.cpp's
//    "[TIMING ts=...]" / "[TRACE ts=...]" heads are recognised;
//  - sd.cpp's auxiliary module names are matched whole (CODE4): a diffusion model called
//    "...-Control" is the diffusion stage;
//  - the shape-level survivors the mutation round named (TST20);
//  - CT-49 round 3 (the second half of this file): sd.cpp master-945 prints "[V] <message> --- file.cpp:N"
//    where master-929 printed "[VERBOSE] file.cpp:N - <message>". The guard reads BOTH shapes: the
//    normalisation, the dump block that closes on "} --- main.cpp:699", the auto-fit plan lines, the escaped
//    newlines of a prompt echo, and every round-2 property restated for the new shape.
import { test } from "node:test";
import assert from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { createLogGuard, scanLog, runEngine, normalizeSdLine, SD_AUX_MODULE, CPU_PLACEMENT, GPU_RESET } from "./igpu-engine.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const fixture = (n) => readFileSync(join(here, "testdata", n), "utf8");

const DEVICE = "ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1 | bf16: 0 | warp size: 64";
const head = (m) => `[VERBOSE] ggml_runner.cpp:1019 - ${m}`;
const sdLog = (...lines) => [DEVICE, head("Wan2.2-TI2V-5B compute buffer size: 478.17 MB(VRAM) on Vulkan0 (peak across 1 segment)"), ...lines].join("\n");

// the real healthy log with the text encoder's compute buffer moved to the CPU
const healthy = fixture("sdcpp-video-healthy.log");
const T5_LINE = "[VERBOSE] ggml_runner.cpp:1019 - t5 compute buffer size: 297.00 MB(VRAM) on Vulkan0 (peak across 1 segment)";
assert.ok(healthy.includes(T5_LINE), "the fixture still has the t5 compute buffer line this test edits");
const t5OnCpu = healthy.replace(T5_LINE, "[VERBOSE] ggml_runner.cpp:1019 - t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)");

test("SIL9/TST3: a prompt that is a fragment of a real placement line cannot blind the sd.cpp detector", () => {
  assert.equal(scanLog(healthy, { engine: "sdcpp" }).fatal, null, "the baseline is healthy");
  assert.equal(scanLog(t5OnCpu, { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT, "the baseline CPU run is caught");
  for (const fragment of ["MB(RAM) on CPU", "(RAM) on CPU", "on CPU", "compute buffer size: 297.00 MB(RAM) on CPU", "t5 compute buffer size", "(peak across 1 segment)"]) {
    const r = scanLog(t5OnCpu, { engine: "sdcpp", echoes: [fragment] });
    assert.equal(r.fatal?.kind, CPU_PLACEMENT, `a prompt of ${JSON.stringify(fragment)} must not hide the text encoder on the CPU`);
    assert.match(r.fatal.line, /t5 compute buffer size: 297\.00 MB\(RAM\) on CPU/);
  }
  // the whole placement line as the prompt, too
  const whole = "t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)";
  assert.equal(scanLog(t5OnCpu, { engine: "sdcpp", echoes: [whole, head(whole)] }).fatal?.kind, CPU_PLACEMENT);
});

test("SIL9/TST3: the same for audio.cpp - the real host-prefill line survives a text that is a fragment of it", () => {
  const log = fixture("audiocpp-music-host-prefill.log");
  assert.equal(scanLog(log, { engine: "audiocpp" }).fatal?.kind, CPU_PLACEMENT);
  for (const fragment of ["planner.weights.buffer_name CPU", "buffer_name CPU", "ace_step.planner.weights.buffer_name CPU", "weights.buffer_name"]) {
    const r = scanLog(log, { engine: "audiocpp", echoes: [fragment] });
    assert.equal(r.fatal?.kind, CPU_PLACEMENT, `a text of ${JSON.stringify(fragment)} must not hide the host copy of the planner`);
    assert.match(r.fatal.line, /ace_step\.planner\.weights\.buffer_name CPU/);
  }
});

test("TST3: the opposite direction - a prompt that is a fragment of the evidence lines does not fail a healthy run", () => {
  for (const fragment of ["Vulkan", "Vulkan0", "on Vulkan0", "compute buffer size", "ggml_vulkan", "(peak across 1 segment)"]) {
    const r = scanLog(healthy, { engine: "sdcpp", echoes: [fragment] });
    assert.equal(r.fatal, null, fragment);
    assert.equal(r.verdict.ok, true, `a prompt of ${JSON.stringify(fragment)} must not remove the evidence`);
  }
  const audio = fixture("audiocpp-voice-clone.log");
  for (const fragment of ["Vulkan0", "weights.buffer_name", "buffer_name Vulkan0"]) {
    assert.equal(scanLog(audio, { engine: "audiocpp", echoes: [fragment] }).verdict.ok, true, fragment);
  }
});

test("SIL9: request text is not evidence - forged device / buffer lines, one echoed line at a time or as a multi-line text", () => {
  const forged = `${DEVICE}\nWan2.2-TI2V-5B compute buffer size: 1 MB(VRAM) on Vulkan0`;
  const g = createLogGuard({ engine: "sdcpp", echoes: [forged] });
  for (const l of forged.split("\n")) g.scan(l); // bare, the way a raw echo of a multi-line prompt would print
  assert.equal(g.verdict().ok, false, "each line of a multi-line text counts as an echo on its own");
  const g2 = createLogGuard({ engine: "sdcpp", echoes: [forged] });
  for (const l of forged.split("\n")) g2.scan("prompt: " + l);
  assert.equal(g2.verdict().ok, false);
  // audio.cpp: a text that looks like an evidence line, printed bare
  const text = "[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0";
  const a = createLogGuard({ engine: "audiocpp", echoes: [text] });
  a.scan(text);
  assert.equal(a.verdict().ok, false);
  const b = createLogGuard({ engine: "audiocpp" });
  b.scan(text);
  assert.equal(b.verdict().ok, true, "the same line without the echo list is real evidence");
});

test("SIL9: an echoed text about a lost device does not end a healthy run, and a real reset still does", () => {
  const prompt = "a desert where every device lost its signal";
  const echoLine = `[VERBOSE] t.cpp:1 - note: ${prompt}`;
  assert.equal(scanLog(echoLine, { engine: "sdcpp", echoes: [prompt] }).fatal, null);
  assert.equal(scanLog(echoLine, { engine: "sdcpp" }).fatal?.kind, GPU_RESET, "without the echo list the same words read as a reset");
  assert.equal(scanLog(fixture("sdcpp-vace-device-lost.log"), { engine: "sdcpp", echoes: [prompt] }).fatal?.kind, GPU_RESET);
});

test("SIL9: the dump-block skip belongs to sd.cpp's own record shape - text ending in 'SDCliParams {' opens nothing", () => {
  const evidence = "[TIMING ts=1] chatterbox.t3.weights.buffer_name Vulkan0";
  // audio.cpp: a text echo that ends the way a dump header does must not open a block that swallows
  // the placement line after it (the old guard was engine-agnostic and closed only on a bare '}')
  const a = createLogGuard({ engine: "audiocpp", echoes: ["x SDCliParams {"] });
  assert.equal(a.scan(evidence), null);
  assert.equal(a.scan("[TIMING ts=2] note SDCliParams {"), null);
  assert.equal(a.scan("[TIMING ts=3] ace_step.planner.weights.buffer_name CPU")?.kind, CPU_PLACEMENT);
  // sd.cpp: only its own head ("[LEVEL] file.cpp:N - SDCliParams {") opens one
  const s = createLogGuard({ engine: "sdcpp" });
  assert.equal(s.scan("prompt: SDCliParams {"), null);
  assert.equal(s.scan("[VERBOSE] ggml_runner.cpp:1 - the prompt was SDCliParams {"), null);
  assert.equal(s.scan("[VERBOSE] ggml_runner.cpp:2 - t5 compute buffer size: 1 MB(RAM) on CPU")?.kind, CPU_PLACEMENT);
  // ... and the real one still does
  const r = createLogGuard({ engine: "sdcpp" });
  assert.equal(r.scan("[VERBOSE] main.cpp:699  - SDCliParams {"), null);
  assert.equal(r.scan("  prompt: \"Using CPU backend\","), null);
  assert.equal(r.scan("}"), null);
  assert.equal(r.scan("[WARN   ] ggml_extend_backend.cpp:676 - loading CPU backend")?.kind, CPU_PLACEMENT, "a column-0 brace closes the block");
  // an indented brace does not
  const i = createLogGuard({ engine: "sdcpp" });
  i.scan("[VERBOSE] main.cpp:700  - SDContextParams {");
  i.scan("  embeddings: {");
  assert.equal(i.scan("  }"), null);
  assert.equal(i.scan("  note: Using CPU backend"), null, "still inside the block");
});

test("SIL9: audio.cpp's [TIMING ts=...] and [TRACE ts=...] heads are recognised, as evidence and as a placement", () => {
  const g = createLogGuard({ engine: "audiocpp" });
  assert.equal(g.scan("[TRACE ts=20261003-172127] ace_step.dit.weights.buffer_name Vulkan0"), null);
  assert.equal(g.verdict().ok, true);
  assert.equal(g.scan("[TRACE ts=20261003-172128] ace_step.vae.weights.buffer_name CPU")?.kind, CPU_PLACEMENT);
  assert.equal(createLogGuard({ engine: "audiocpp" }).scan("[TIMING ts=1] a.weights.buffer_name CPU")?.kind, CPU_PLACEMENT);
  // an sd.cpp dump block ended by an audio-style head (the head is part of the record shapes)
  const s = createLogGuard({ engine: "sdcpp" });
  s.scan("[VERBOSE] main.cpp:699  - SDCliParams {");
  assert.equal(s.scan("  prompt: \"on CPU\","), null);
  assert.equal(s.scan("[TIMING ts=5] note: Using CPU backend")?.kind, CPU_PLACEMENT, "a [TIMING ts=..] record ends an unterminated block");
});

test("CODE4/TST20: sd.cpp's auxiliary runner names are matched whole; a diffusion model whose name holds a word is the diffusion stage", () => {
  const aux = ["t5", "T5", "t5xxl", "umt5", "umt5xxl", "clip", "clip_l", "clip_g", "clip_vision", "llm", "text_enc", "text_encoder", "conditioner",
    "vae", "wan_vae", "flux_vae", "taehv", "tae", "taesd", "tae_wan", "control_net", "controlnet", "esrgan", "pmid", "vision"];
  const diffusion = ["Wan2.2-TI2V-5B", "Wan2.1-VACE-1.3B", "Wan2.1-Fun-14B-Control", "Wan2.1-Fun-14B-InP", "Wan2.2-I2V-A14B-HighNoise", "wan", "ltx_video", "flux", "qwen_image",
    "Wan2.1-Control", "control-dit", "Controlnet-Union-DiT", "wan_vision_dit", "Wan-VACE-14B", "mmdit", "unet", "my-t5-finetuned-dit"];
  for (const name of aux) assert.ok(SD_AUX_MODULE.test(name), `${name} is auxiliary`);
  for (const name of diffusion) assert.ok(!SD_AUX_MODULE.test(name), `${name} is a diffusion model`);
  const onlyAux = (name) => scanLog(`${DEVICE}\n${head(`${name} compute buffer size: 5.00 MB(VRAM) on Vulkan0 (peak across 1 segment)`)}`, { engine: "sdcpp" });
  for (const name of aux) assert.equal(onlyAux(name).verdict.ok, false, `a log whose only Vulkan compute buffer is ${name} is not diffusion-stage evidence`);
  for (const name of diffusion) assert.equal(onlyAux(name).verdict.ok, true, `${name} on Vulkan0 is the diffusion stage`);
});

test("CODE4: the healthy 'Wan2.1-Fun-14B-Control' log (the named probe) passes, and its CPU twin fails", () => {
  const log = sdLog().replace("Wan2.2-TI2V-5B", "Wan2.1-Fun-14B-Control");
  const r = scanLog(log, { engine: "sdcpp" });
  assert.equal(r.fatal, null);
  assert.equal(r.verdict.ok, true, JSON.stringify(r.verdict));
  assert.equal(scanLog(log.replace("MB(VRAM) on Vulkan0", "MB(RAM) on CPU"), { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
});

test("TST20: the sd.cpp verdict needs the device line AND the diffusion stage; a non-CPU, non-Vulkan target is no evidence", () => {
  const noDevice = scanLog(head("Wan2.2-TI2V-5B compute buffer size: 478.17 MB(VRAM) on Vulkan0 (peak across 1 segment)"), { engine: "sdcpp" });
  assert.equal(noDevice.verdict.ok, false);
  assert.match(noDevice.verdict.expected, /missing/);
  const noDiffusion = scanLog(`${DEVICE}\n${head("wan_vae compute buffer size: 206.69 MB(VRAM) on Vulkan0")}`, { engine: "sdcpp" });
  assert.equal(noDiffusion.verdict.ok, false);
  const hostPinned = scanLog(`${DEVICE}\n${head("Wan2.2-TI2V-5B compute buffer size: 478.17 MB(RAM) on Vulkan_Host")}`, { engine: "sdcpp" });
  assert.equal(hostPinned.fatal, null, "pinned host memory is not a CPU placement");
  assert.equal(hostPinned.verdict.ok, false, "and it is not GPU evidence either");
  const audioHost = createLogGuard({ engine: "audiocpp" });
  audioHost.scan("[TIMING ts=1] a.weights.buffer_name Vulkan_Host");
  assert.equal(audioHost.verdict().ok, false);
});

test("TST20: each remaining placement shape, ANSI colour, unknown engines", () => {
  // 'No devices found' as ggml prints it, on its own
  assert.equal(scanLog("ggml_vulkan: No devices found.", { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("ggml_vulkan: Found 0 Vulkan devices:", { engine: "audiocpp" }).fatal?.kind, CPU_PLACEMENT);
  // da3: both CPU shapes alone, and the device line on CPU
  assert.equal(scanLog("[da3] flash-attn: 12 node(s) run on CPU", { engine: "da3" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("[da3] offload_weights: 237 weights -> CPU (none on the device)", { engine: "da3" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("[da3] da::Backend using device: CPU", { engine: "da3" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("[da3] da::Backend using device: Vulkan0", { engine: "da3" }).verdict.ok, true);
  // a text that only mentions the da3 shapes is not one
  assert.equal(scanLog("prompt: the flash-attn: 12 node(s) run on CPU saga", { engine: "da3" }).fatal, null);
  // ANSI colour around a line does not hide it
  assert.equal(scanLog("\u001b[33m[WARN   ] ggml_extend_backend.cpp:676 - loading CPU backend\u001b[0m", { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog(`\u001b[32m${DEVICE}\u001b[0m\n${head("wan compute buffer size: 1 MB(VRAM) on Vulkan0")}`, { engine: "sdcpp" }).verdict.ok, true);
  // an engine the guard does not know is a programming error, not a pass
  assert.throws(() => createLogGuard({ engine: "whisper" }), /unknown engine/);
});

test("TST19: the evidence gate is for a clean exit only - an engine that refuses the model after its device line is not CPU_PLACEMENT", async () => {
  const script = [
    `console.error(${JSON.stringify(DEVICE)});`,
    "console.error(\"[ERROR] Diffusion model tensor 'model.diffusion_model.vace_patch_embedding.weight' not in model metadata\");",
    "process.exit(1);",
  ].join("");
  const r = await runEngine({ bin: process.execPath, args: ["-e", script], guard: createLogGuard({ engine: "sdcpp" }), label: "sd-cli" });
  assert.equal(r.code, 1, "resolves with the exit code so the caller can type the failure");
  assert.match(r.log, /not in model metadata/);
});

test("TST19: a CPU line rejects only once the engine is DEAD, not when the line is seen", async () => {
  const script = [
    "const fs=require('fs');fs.writeFileSync(process.argv[1],String(process.pid));",
    `console.error(${JSON.stringify(DEVICE)});`,
    "console.error('[VERBOSE] ggml_runner.cpp:1 - t5 compute buffer size: 1 MB(RAM) on CPU');",
    "setInterval(()=>{},1000);",
  ].join("");
  const { mkdtempSync, rmSync, readFileSync: rf } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const dir = mkdtempSync(join(tmpdir(), "igpu-guard-test-"));
  try {
    const pidFile = join(dir, "pid");
    await assert.rejects(runEngine({ bin: process.execPath, args: ["-e", script, pidFile], timeoutMs: 60000, guard: createLogGuard({ engine: "sdcpp" }), label: "sd-cli" }), new RegExp(CPU_PLACEMENT));
    const pid = Number(rf(pidFile, "utf8"));
    let alive = true;
    try { process.kill(pid, 0); } catch (e) { alive = e.code === "EPERM"; }
    assert.equal(alive, false, "the GPU is free when the promise settles: the engine pid is already gone");
  } finally { rmSync(dir, { recursive: true, force: true }); }
}, { timeout: 90000 });

test("SIL9: placement shapes are anchored at the start of a record - text before them makes the line something else", () => {
  // request text echoed in the middle of a line cannot forge a placement (a false CPU_PLACEMENT) ...
  for (const l of [
    "prompt: t5 compute buffer size: 1.00 MB(RAM) on CPU",
    "[INFO] negative prompt: wan compute buffer size: 1 MB(RAM) on CPU (peak)",
    "a b t5 compute buffer size: 1 MB(RAM) on CPU",
  ]) assert.equal(scanLog(l, { engine: "sdcpp" }).fatal, null, l);
  for (const l of ["text: planner.weights.buffer_name CPU", "[INFO] lyrics: a.b.weights.buffer_name CPU please", "x y.weights.buffer_name CPU"]) {
    assert.equal(scanLog(l, { engine: "audiocpp" }).fatal, null, l);
  }
  // ... and cannot forge evidence either
  assert.equal(scanLog(`${DEVICE}\nprompt: Wan2.2-TI2V-5B compute buffer size: 1 MB(VRAM) on Vulkan0`, { engine: "sdcpp" }).verdict.ok, false);
  assert.equal(scanLog("text: a.weights.buffer_name Vulkan0", { engine: "audiocpp" }).verdict.ok, false);
  // the real shapes, with and without the record head, are still read
  assert.equal(scanLog("t5 compute buffer size: 1 MB(RAM) on CPU", { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog(head("t5 compute buffer size: 1 MB(RAM) on CPU"), { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("[TIMING ts=1] a.weights.buffer_name CPU", { engine: "audiocpp" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog("a.weights.buffer_name CPU", { engine: "audiocpp" }).fatal?.kind, CPU_PLACEMENT);
});

test("SIL9: audio.cpp never opens a dump block, even on an sd.cpp-shaped header; the whole placement line as the text still trips", () => {
  const a = createLogGuard({ engine: "audiocpp", echoes: ["a text"] });
  assert.equal(a.scan("[INFO ] main.cpp:699 - SDCliParams {"), null);
  assert.equal(a.scan("ace_step.planner.weights.buffer_name CPU")?.kind, CPU_PLACEMENT, "a bare placement line right after it: nothing swallowed it");
  // a text that IS the placement line (the worst case of coverage) still cannot hide it
  const line = "[TIMING ts=20261003-172137] ace_step.planner.weights.buffer_name CPU";
  assert.equal(scanLog(line, { engine: "audiocpp", echoes: [line] }).fatal?.kind, CPU_PLACEMENT);
  const sdLine = head("t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)");
  assert.equal(scanLog(sdLine, { engine: "sdcpp", echoes: [sdLine] }).fatal?.kind, CPU_PLACEMENT);
});

// ---------------------------------------------------------------- CT-49 round 3: sd.cpp's master-945 record shape

// master-945 line: "[V] <message> --- <source>"; the tail is on the LAST line of a record
const head945 = (m, src = "ggml_runner.cpp:1019") => `[V] ${m} --- ${src}`;
// a build between #2104 and #2106 puts " - " in front of the source (read from the upstream commits; no log of it was captured)
const head944 = (m, src = "ggml_runner.cpp:1019") => `[V] ${m} - ${src}`;
const COMPUTE = "Wan2.2-TI2V-5B compute buffer size: 192.53 MB(VRAM) on Vulkan0 (peak across 1 segment)";
const planLine = (shape, component, compute, params) => (shape === "945"
  ? `[I]     ${component.padEnd(12)} params   5162 MiB, compute reserve  2048 MiB -> compute ${compute}, params ${params} --- backend_fit.cpp:346`
  : `[INFO   ] backend_fit.cpp:346  -     ${component.padEnd(12)} params   5162 MiB, compute reserve  2048 MiB -> compute ${compute}, params ${params}`);

const healthy945 = fixture("sdcpp945-video-healthy.log");
const T5_LINE_945 = "[V] t5 compute buffer size: 297.00 MB(VRAM) on Vulkan0 (peak across 1 segment) --- ggml_runner.cpp:1019";
const T5_CPU_945 = "[V] t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment) --- ggml_runner.cpp:1019";
assert.ok(healthy945.includes(T5_LINE_945), "the master-945 fixture still has the t5 compute buffer line this test edits");
const t5OnCpu945 = healthy945.replace(T5_LINE_945, T5_CPU_945);

test("normalizeSdLine: one head and one tail are cut, in either record shape, and nothing else", () => {
  const n = normalizeSdLine;
  // master-929: the long tag and the source it prints in front
  assert.deepEqual(n("[VERBOSE] ggml_runner.cpp:1019 - Wan compute buffer size: 1 MB(VRAM) on Vulkan0"), { text: "Wan compute buffer size: 1 MB(VRAM) on Vulkan0", tagged: true });
  assert.deepEqual(n("[VERBOSE] main.cpp:699  - SDCliParams {"), { text: "SDCliParams {", tagged: true });
  for (const tag of ["INFO   ", "WARN   ", "ERROR  ", "DEBUG  "]) assert.deepEqual(n(`[${tag}] x.cpp:5    - hello`), { text: "hello", tagged: true }, tag);
  assert.equal(n("[INFO   ] backend_fit.cpp:346  -     DiT          params 1 MiB").text, "DiT          params 1 MiB", "the old head eats the plan's indentation");
  // master-945: the one-letter tag in front, the source behind
  assert.deepEqual(n("[V] Wan compute buffer size: 1 MB(VRAM) on Vulkan0 (peak across 1 segment) --- ggml_runner.cpp:1019"), { text: "Wan compute buffer size: 1 MB(VRAM) on Vulkan0 (peak across 1 segment)", tagged: true });
  for (const tag of ["D", "V", "I", "W", "E", "?"]) assert.deepEqual(n(`[${tag}] hello --- a.cpp:1`), { text: "hello", tagged: true }, tag);
  assert.equal(n("[I]     DiT          params 1 MiB --- backend_fit.cpp:346").text, "    DiT          params 1 MiB", "the new head keeps the plan's indentation");
  assert.equal(n("[I] Version: Wan 2.2 TI2V  --- diffusion_engine.cpp:996").text, "Version: Wan 2.2 TI2V", "two spaces before the tail go with it");
  // the lines of a record of several lines: the tail of the last one alone, the middle ones untouched
  assert.deepEqual(n("} --- main.cpp:699"), { text: "}", tagged: false });
  assert.deepEqual(n("   SSE3 = 1 |    SSSE3 = 1 |  --- main.cpp:698"), { text: "   SSE3 = 1 |    SSSE3 = 1 |", tagged: false });
  assert.deepEqual(n("  embeddings: {"), { text: "  embeddings: {", tagged: false });
  assert.equal(n("  } --- main.cpp:699").text, "  }", "an indented brace stays indented: it is not the dump's close");
  // a build between #2104 and #2106 separates the source with " - " (read from the upstream commits alone)
  assert.deepEqual(n("[V] hello - ggml_runner.cpp:1019"), { text: "hello", tagged: true });
  // at most one head and one tail, only at the two ends of the line
  assert.equal(n("[V] a --- x.cpp:1 b").text, "a --- x.cpp:1 b", "a tail-like text in the middle is text");
  assert.equal(n("[V] a --- x.cpp:1 --- y.cpp:2").text, "a --- x.cpp:1", "one tail");
  assert.equal(n("[V] [V] a").text, "[V] a", "one head");
  assert.equal(n("[V] a --- x.cpp").text, "a --- x.cpp", "a source with no line number is no tail");
  // bare lines, tags that are not heads, nothing at all
  assert.deepEqual(n("ggml_vulkan: Found 1 Vulkan devices:"), { text: "ggml_vulkan: Found 1 Vulkan devices:", tagged: false });
  assert.deepEqual(n("[INFO] x"), { text: "[INFO] x", tagged: false }, "a long tag with no source in front is not a head");
  assert.deepEqual(n("[V]x"), { text: "[V]x", tagged: false }, "the one-letter tag needs its space");
  assert.deepEqual(n("[X] x"), { text: "[X] x", tagged: false }, "and is one of D V I W E");
  assert.deepEqual(n(""), { text: "", tagged: false });
  assert.deepEqual(n(undefined), { text: "", tagged: false });
});

test("normalizeSdLine costs linear time: a prompt padded with 200,000 spaces (or dashes) does not freeze the runner", { timeout: 20000 }, () => {
  for (const pad of [" ", "- ", "-", "\t", " -"]) {
    const line = `  prompt: "a${pad.repeat(200000)}b",`;
    const t0 = performance.now();
    assert.equal(normalizeSdLine(line).text, line);
    assert.equal(normalizeSdLine(`[V] ${line} --- main.cpp:701`).text, line);
    assert.equal(scanLog(line, { engine: "sdcpp" }).fatal, null);
    assert.ok(performance.now() - t0 < 5000, `${JSON.stringify(pad)}: ${performance.now() - t0} ms`);
  }
});

test("a dump block closed by '} --- main.cpp:699' closes: what follows is scanned again; an indented brace or another text does not close it", () => {
  const after = (closer, next) => {
    const g = createLogGuard({ engine: "sdcpp" });
    assert.equal(g.scan("[V] SDCliParams {"), null);
    assert.equal(g.scan("  note: Using CPU backend,"), null, "inside the block nothing is scanned");
    assert.equal(g.scan(closer), null, closer);
    return g.scan(next);
  };
  const bare = "t5 compute buffer size: 1 MB(RAM) on CPU (peak across 1 segment)";
  for (const closer of ["} --- main.cpp:699", "}  --- main.cpp:699 ", "}", "} - main.cpp:699"]) {
    assert.equal(after(closer, bare)?.kind, CPU_PLACEMENT, `${closer}: a bare placement line right after the close is read`);
    assert.equal(after(closer, T5_CPU_945)?.kind, CPU_PLACEMENT, `${closer}: and so is a one-letter-tag record`);
  }
  for (const notCloser of ["  } --- main.cpp:699", "  }", "}, --- main.cpp:699", "} extra --- main.cpp:699", "x } --- main.cpp:699"]) {
    assert.equal(after(notCloser, bare), null, `${JSON.stringify(notCloser)} does not close the block`);
  }
  // the three real blocks of a log, one after the other
  const g = createLogGuard({ engine: "sdcpp" });
  for (const [open, close] of [["SDCliParams", 699], ["SDContextParams", 700], ["SDGenerationParams", 701]]) {
    assert.equal(g.scan(`[V] ${open} {`), null);
    assert.equal(g.scan("  embeddings: {"), null);
    assert.equal(g.scan("  }"), null);
    assert.equal(g.scan(`} --- main.cpp:${close}`), null);
  }
  assert.equal(g.scan(bare)?.kind, CPU_PLACEMENT, "after the third close the log is scanned");
});

test("a master-945 dump block whose close the guard does not recognise is NOT healed by the next one-letter record: the run ends without its evidence", () => {
  // The one-letter tags are deliberately no valve (see the guard's comment): a block that does not close
  // is a record shape this guard does not understand, and the loud outcome is the right one. The real
  // healthy log with its three closing lines changed to a shape nobody knows:
  const drifted = healthy945.replace(/^\} --- main\.cpp:(\d+)$/gm, "}} ### main.cpp:$1");
  assert.equal(drifted.split("\n").filter((l) => l.startsWith("}} ###")).length, 3);
  const r = scanLog(drifted, { engine: "sdcpp" });
  assert.equal(r.fatal, null);
  assert.equal(r.verdict.ok, false, "everything after the first block was skipped: no diffusion evidence");
  assert.match(r.verdict.expected, /diffusion-stage .*\(missing\)/);
  assert.equal(scanLog(healthy945, { engine: "sdcpp" }).verdict.ok, true, "and the same log with the real closing lines passes");
  // the old shape keeps its valve: the next long-tag record ends an unterminated block
  const g = createLogGuard({ engine: "sdcpp" });
  g.scan("[VERBOSE] main.cpp:699  - SDCliParams {");
  assert.equal(g.scan("  prompt: \"Using CPU backend\","), null);
  assert.equal(g.scan("[WARN   ] ggml_extend_backend.cpp:676 - loading CPU backend")?.kind, CPU_PLACEMENT);
});

test("a record that IS a line of the request opens no dump block, in either shape (a multi-line prompt that spells a dump header)", () => {
  for (const header of ["[V] SDCliParams {", "[VERBOSE] main.cpp:699  - SDCliParams {"]) {
    const text = `a calm sea\n${header}\nand a sky`;
    const g = createLogGuard({ engine: "sdcpp", echoes: [text] });
    // an engine echoing the prompt line by line (the master-929 shape does; the new one escapes the newlines)
    for (const l of text.split("\n")) assert.equal(g.scan(l), null, l);
    assert.equal(g.scan("t5 compute buffer size: 1 MB(RAM) on CPU (peak across 1 segment)")?.kind, CPU_PLACEMENT, `${header}: nothing swallowed the log`);
    // the engine's own header, with the same request, still opens a block
    const real = createLogGuard({ engine: "sdcpp", echoes: ["a calm sea"] });
    assert.equal(real.scan(header), null);
    assert.equal(real.scan("  prompt: \"Using CPU backend\","), null, `${header}: the real one opens`);
  }
  // a bare "SDCliParams {" (no record head at all) is only text: it opens nothing either
  const bare = createLogGuard({ engine: "sdcpp" });
  assert.equal(bare.scan("SDCliParams {"), null);
  assert.equal(bare.scan("t5 compute buffer size: 1 MB(RAM) on CPU (peak across 1 segment)")?.kind, CPU_PLACEMENT, "nothing was swallowed");
});

test("SIL9/TST3 (master-945): a prompt that is a fragment of a real placement line cannot blind the detector in the new record shape either", () => {
  assert.equal(scanLog(healthy945, { engine: "sdcpp" }).fatal, null, "the baseline is healthy");
  assert.equal(scanLog(t5OnCpu945, { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT, "the baseline CPU run is caught");
  const message = "t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)";
  for (const fragment of ["MB(RAM) on CPU", "(RAM) on CPU", "on CPU", "compute buffer size: 297.00 MB(RAM) on CPU", "t5 compute buffer size", "(peak across 1 segment)",
    "ggml_runner.cpp:1019", "--- ggml_runner.cpp:1019", "(peak across 1 segment) --- ggml_runner.cpp:1019", message, T5_CPU_945, `${message}\nsecond line`, `${message}\r\nsecond line`]) {
    const r = scanLog(t5OnCpu945, { engine: "sdcpp", echoes: [fragment] });
    assert.equal(r.fatal?.kind, CPU_PLACEMENT, `a prompt of ${JSON.stringify(fragment)} must not hide the text encoder on the CPU`);
    assert.match(r.fatal.line, /t5 compute buffer size: 297\.00 MB\(RAM\) on CPU/);
    assert.equal(r.fatal.line, T5_CPU_945, "the raw record, head and tail, is what the error names");
  }
  // the opposite direction: a fragment of the evidence lines does not fail the healthy run
  for (const fragment of ["Vulkan", "Vulkan0", "on Vulkan0", "compute buffer size", "ggml_vulkan", "(peak across 1 segment)", "--- ggml_runner.cpp:1019", "-> compute Vulkan0, params Vulkan0", "DiT"]) {
    const r = scanLog(healthy945, { engine: "sdcpp", echoes: [fragment] });
    assert.equal(r.fatal, null, fragment);
    assert.equal(r.verdict.ok, true, `a prompt of ${JSON.stringify(fragment)} must not remove the evidence`);
  }
  // the real prompts of the fixtures, handed to the guard the way the runners do, change nothing
  for (const [file, prompt] of [
    ["sdcpp945-video-healthy.log", "waves crash on the rocks around the lighthouse, golden hour"],
    ["sdcpp945-video-tae.log", "waves crash on the rocks around the lighthouse, golden hour"],
    ["sdcpp945-vace-healthy.log", "a clay figure of a bearded man in a plaid shirt waves hello, stop-motion clay style, warm evening light"],
    ["sdcpp-video-healthy.log", "waves roll in and crash on the rocks around the lighthouse, sea spray, golden hour light, gentle camera drift"],
  ]) {
    const r = scanLog(fixture(file), { engine: "sdcpp", echoes: [prompt, ""] });
    assert.equal(r.fatal, null, file);
    assert.equal(r.verdict.ok, true, file);
  }
  for (const [file, text] of [
    ["audiocpp091-voice-clone.log", "Every clip on this node now renders on the integrated graphics, and nothing runs on the processor. This is the first voice test."],
    ["audiocpp091-music.log", "warm acoustic guitar and soft piano, gentle documentary underscore, 90 bpm, instrumental"],
  ]) {
    const r = scanLog(fixture(file), { engine: "audiocpp", echoes: [text, "[verse]\nline two"] });
    assert.equal(r.fatal, null, file);
    assert.equal(r.verdict.ok, true, file);
  }
});

test("the auto-fit plan, in both record shapes: '-> compute CPU' is a placement for any component; params in RAM or on the CPU alone is the sanctioned spill; the DiT line on Vulkan is evidence", () => {
  for (const shape of ["929", "945"]) {
    for (const component of ["DiT", "Conditioner", "VAE", "ControlNet"]) {
      assert.equal(scanLog(`${DEVICE}\n${planLine(shape, component, "CPU", "RAM")}`, { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT, `${shape} ${component} computing on the CPU`);
      for (const params of ["RAM", "CPU", "Vulkan_Host", "Vulkan0"]) {
        assert.equal(scanLog(`${DEVICE}\n${planLine(shape, component, "Vulkan0", params)}`, { engine: "sdcpp" }).fatal, null, `${shape} ${component} params ${params}: spill is no placement`);
      }
    }
    // the DiT line alone (plus the device) shows the diffusion stage on the GPU, params wherever they rest
    for (const params of ["Vulkan0", "RAM", "CPU"]) {
      const r = scanLog(`${DEVICE}\n${planLine(shape, "DiT", "Vulkan0", params)}`, { engine: "sdcpp" });
      assert.equal(r.verdict.ok, true, `${shape}: DiT -> compute Vulkan0, params ${params}`);
      assert.ok(r.verdict.evidence.includes("DiT plan: compute on Vulkan0"));
    }
    // the other components, a compute backend that is no Vulkan device, and no device line are not that evidence
    for (const line of [planLine(shape, "Conditioner", "Vulkan0", "Vulkan0"), planLine(shape, "VAE", "Vulkan0", "Vulkan0"), planLine(shape, "DiT", "Vulkan_Host", "RAM"), planLine(shape, "DiT", "Metal0", "RAM")]) {
      assert.equal(scanLog(`${DEVICE}\n${line}`, { engine: "sdcpp" }).verdict.ok, false, line);
    }
    assert.equal(scanLog(planLine(shape, "DiT", "Vulkan0", "Vulkan0"), { engine: "sdcpp" }).verdict.ok, false, "the device line is still needed");
    // text that only contains a plan line is not one, and a plan line the request spelled is no evidence
    assert.equal(scanLog(`${DEVICE}\nprompt: ${planLine(shape, "DiT", "Vulkan0", "Vulkan0").trim()}`, { engine: "sdcpp" }).verdict.ok, false);
    const forged = planLine(shape, "DiT", "Vulkan0", "Vulkan0").trim();
    assert.equal(scanLog(`${DEVICE}\n${forged}`, { engine: "sdcpp", echoes: [forged] }).verdict.ok, false, "the whole record as the prompt");
    assert.equal(scanLog(`${DEVICE}\n${forged}`, { engine: "sdcpp", echoes: [forged.replace(/^\[[^\]]+\]\s+(?:\S+:\d+\s+-\s+)?/, "").replace(/ --- \S+$/, "")] }).verdict.ok, false, "and its message alone");
    // while a prompt that is the CPU twin of the plan line hides nothing
    const cpuPlan = planLine(shape, "DiT", "CPU", "RAM").trim();
    assert.equal(scanLog(`${DEVICE}\n${cpuPlan}`, { engine: "sdcpp", echoes: [cpuPlan] }).fatal?.kind, CPU_PLACEMENT);
  }
  // the evidence can come from either source: without the diffusion compute buffer, the plan carries the run; without both, it fails
  const noCompute = healthy945.split("\n").filter((l) => !/^\[V\] Wan2\.2-TI2V-5B compute buffer size/.test(l)).join("\n");
  assert.notEqual(noCompute, healthy945);
  assert.equal(scanLog(noCompute, { engine: "sdcpp" }).verdict.ok, true, "the plan's DiT line stands in for the compute buffer");
  const noPlan = noCompute.split("\n").filter((l) => !/^\[I\]\s+DiT\s+params/.test(l)).join("\n");
  assert.notEqual(noPlan, noCompute);
  const gone = scanLog(noPlan, { engine: "sdcpp" });
  assert.equal(gone.fatal, null);
  assert.equal(gone.verdict.ok, false, "neither source: no diffusion evidence");
  assert.ok(gone.verdict.expected.includes("(or the auto-fit plan's DiT \"-> compute Vulkan<N>\" line) (missing)"), `the message names both sources: ${gone.verdict.expected}`);
  // a plan that puts one component on the CPU kills the otherwise healthy run at that line
  const vaeOnCpu = healthy945.replace(/(\[I\]\s+VAE\s+params .*-> compute )Vulkan0(, params )Vulkan0/, "$1CPU$2RAM");
  assert.notEqual(vaeOnCpu, healthy945);
  const k = scanLog(vaeOnCpu, { engine: "sdcpp" });
  assert.equal(k.fatal?.kind, CPU_PLACEMENT);
  assert.match(k.fatal.line, /VAE\s+params .*-> compute CPU, params RAM --- backend_fit\.cpp:346$/);
});

test("master-945 prints a prompt on ONE line with its newlines escaped: that spelling of the text is an echo, so a forged evidence line inside it counts for nothing", () => {
  // two lines of the same length, so that neither is 60% of the escaped one-line echo on its own
  const forged = "t compute buffer size: 1 MB on Vulkan0 (x)";
  const second = "a lighthouse in a storm at golden hour now";
  assert.equal(forged.length, second.length);
  const text = `${forged}\n${second}`;
  assert.ok(forged.length < 0.6 * (forged.length + 2 + second.length) && second.length < 0.6 * (forged.length + 2 + second.length));
  const record = head945(`${text.replace(/\n/g, "\\n")}`, "conditioner.hpp:1487");
  const bare = text.replace(/\n/g, "\\n");
  for (const echoed of [record, bare]) {
    assert.equal(scanLog(`${DEVICE}\n${echoed}`, { engine: "sdcpp" }).verdict.ok, true, `without the request the same line is a compute buffer on Vulkan0: ${echoed}`);
    assert.equal(scanLog(`${DEVICE}\n${echoed}`, { engine: "sdcpp", echoes: [text] }).verdict.ok, false, `with it, the escaped echo is the request's own text: ${echoed}`);
  }
  // a CR is escaped as well ("\r"), and the CRLF text matches its escaped spelling
  const crlf = `${forged}\r\n${second}`;
  assert.equal(scanLog(`${DEVICE}\n${crlf.replace(/\r/g, "\\r").replace(/\n/g, "\\n")}`, { engine: "sdcpp", echoes: [crlf] }).verdict.ok, false);
  // a text about a lost device, echoed escaped, ends nothing; a real reset in the new shape still does
  const prompt = "a desert where every device lost its signal\nand a second line";
  const echoLine = head945(`note: ${prompt.replace(/\n/g, "\\n")}`, "x.cpp:1");
  assert.equal(scanLog(echoLine, { engine: "sdcpp", echoes: [prompt] }).fatal, null);
  assert.equal(scanLog(echoLine, { engine: "sdcpp" }).fatal?.kind, GPU_RESET, "without the request the same words read as a reset");
  for (const real of ["[E] Vulkan0 workspace synchronization failed during segment cleanup: vk::Queue::submit: ErrorDeviceLost --- compute_workspace.cpp:243",
    "[E] ggml: ggml_vulkan: device lost on Vulkan0", "radv/amdgpu: The CS has been cancelled because the context is lost. This context is innocent.",
    "[E] Wan2.1-VACE-1.3B graph execution failed on Vulkan0: vk::Queue::submit: ErrorDeviceLost --- ggml_runner.cpp:659"]) {
    for (const echoes of [[], [prompt]]) assert.equal(scanLog(real, { engine: "sdcpp", echoes }).fatal?.kind, GPU_RESET, real);
  }
  // a reset inside a dump block is not read (the block holds the request's text); after its close it is
  const g = createLogGuard({ engine: "sdcpp" });
  g.scan("[V] SDGenerationParams {");
  assert.equal(g.scan("  prompt: \"device lost\","), null);
  assert.equal(g.scan("} --- main.cpp:701"), null);
  assert.equal(g.scan("[E] diffusion model compute failed: ErrorDeviceLost --- diffusion_engine.cpp:2594")?.kind, GPU_RESET);
});

test("the head and the tail of a record do not dilute the share the request's text has of it: the message alone is asked too", () => {
  // 38 characters of forged evidence: under 60% of the 67-character master-945 record (tail included) and of the
  // 71-character master-929 record (head included), but all of the message
  const forged = "t compute buffer size: 1 MB on Vulkan0";
  for (const record of [head945(forged, "ggml_runner.cpp:1019"), head944(forged, "ggml_runner.cpp:1019"), `[VERBOSE] ggml_runner.cpp:1019 - ${forged}`]) {
    assert.ok(forged.length < 0.6 * record.length, record);
    assert.equal(scanLog(`${DEVICE}\n${record}`, { engine: "sdcpp" }).verdict.ok, true, `real evidence without the request: ${record}`);
    assert.equal(scanLog(`${DEVICE}\n${record}`, { engine: "sdcpp", echoes: [forged] }).verdict.ok, false, `the request's own text is no evidence: ${record}`);
  }
});

test("a build between #2104 and #2106 (' - <source>' behind the message) is read like the other two shapes", () => {
  const log = (...lines) => [DEVICE, ...lines].join("\n");
  assert.equal(scanLog(log(head944(COMPUTE)), { engine: "sdcpp" }).verdict.ok, true);
  assert.equal(scanLog(log(head944("t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)")), { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
  assert.equal(scanLog(log(head944("DiT          params   5162 MiB, compute reserve  2048 MiB -> compute CPU, params RAM", "backend_fit.cpp:346")), { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT);
  // a whole dump block, closed by '} - main.cpp:699', with the diffusion evidence after it
  const r = scanLog(log("[V] SDCliParams {", "  note: Using CPU backend,", "} - main.cpp:699", head944(COMPUTE)), { engine: "sdcpp" });
  assert.equal(r.fatal, null);
  assert.equal(r.verdict.ok, true);
});

test("GPU_RESET and the placement shapes read the whole raw line of a master-945 record, the old shape's lines too", () => {
  for (const l of ["[E] Vulkan0 workspace synchronization failed: vk::Queue::submit: ErrorDeviceLost --- compute_workspace.cpp:243", "[E] ggml: ggml_vulkan: device lost on Vulkan0"]) {
    assert.equal(scanLog(l, { engine: "sdcpp" }).fatal?.kind, GPU_RESET, l);
  }
  for (const l of ["[W] loading CPU backend --- ggml_extend_backend.cpp:676", "[E] No devices found! --- ggml_extend_backend.cpp:635", "[V] Using CPU backend --- ggml_extend_backend.cpp:681",
    "ggml_vulkan: Found 0 Vulkan devices:", "[W] ggml: ggml_vulkan: Found 0 Vulkan devices:", "[I] ggml: ggml_vulkan: 0 = llvmpipe (LLVM 17.0.6, 256 bits) (llvmpipe) | uma: 0 | fp16: 1"]) {
    assert.equal(scanLog(l, { engine: "sdcpp" }).fatal?.kind, CPU_PLACEMENT, l);
  }
  // the registration and backend-init lines in the new shape are not placements
  for (const l of ["[V] Initializing backend: CPU --- ggml_extend_backend.cpp:404", "[V] Initializing backend: Vulkan0 --- ggml_extend_backend.cpp:404", "load_backend: loaded CPU backend from /opt/sdcpp/libggml-cpu-haswell.so",
    "[I] total params memory size = 12265.07MB (VRAM 12265.07MB, RAM 0.00MB): text_encoders 5757.05MB(VRAM) --- diffusion_engine.cpp:1328"]) {
    assert.equal(scanLog(`${DEVICE}\n${l}`, { engine: "sdcpp" }).fatal, null, l);
  }
});

test("the tokenizer echoes of the new shape are skipped whatever the prompt says, and only they: a line that merely contains the words is scanned", () => {
  const nasty = "t5 compute buffer size: 1.00 MB(RAM) on CPU; Using CPU backend; ggml_vulkan: Found 0 Vulkan devices; device lost";
  const escaped = `${nasty}\\nsecond line`;
  assert.equal(scanLog(head945(`split prompt "${escaped}" to tokens ["a", "b"]`, "t5_unigram_tokenizer.cpp:346"), { engine: "sdcpp" }).fatal, null);
  assert.equal(scanLog(head945(`parse '${escaped}' to [['${escaped}', 1], ]`, "conditioner.hpp:1487"), { engine: "sdcpp" }).fatal, null);
  assert.equal(scanLog(head945(`prompt was "${escaped}"`, "x.cpp:1"), { engine: "sdcpp" }).fatal?.kind, GPU_RESET, "any other line is scanned (and the plain words trip it)");
  assert.equal(scanLog(`${DEVICE}\n${head945(COMPUTE)}\n${head945(`split prompt "${escaped}" to tokens []`)}`, { engine: "sdcpp" }).verdict.ok, true);
});
