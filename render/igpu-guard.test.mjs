// node --test render/igpu-guard.test.mjs
// The positive GPU-evidence guard against request text and its own shapes (CT-49 round 2):
//  - the request's prompt / text / lyrics can never switch the CPU detector off, whatever fragment
//    of a real placement line they are (SIL9, TST3), and a forged evidence line inside them is not
//    evidence;
//  - the parameter-dump blocks belong to sd.cpp only, on its own record shape; audio.cpp's
//    "[TIMING ts=...]" / "[TRACE ts=...]" heads are recognised;
//  - sd.cpp's auxiliary module names are matched whole (CODE4): a diffusion model called
//    "...-Control" is the diffusion stage;
//  - the shape-level survivors the mutation round named (TST20).
import { test } from "node:test";
import assert from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { createLogGuard, scanLog, runEngine, SD_AUX_MODULE, CPU_PLACEMENT, GPU_RESET } from "./igpu-engine.mjs";

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
