// sdcpp-animate.mjs — character animation on the iGPU tier (CT-49): a reference image is
// animated by a driver clip's motion with stable-diffusion.cpp's Wan2.1 VACE 1.3B and a
// depth control video. Three spawn-per-job native steps under ONE GPU lease, nothing
// resident:
//   1. ffmpeg extracts the driver's frames (16 fps, scaled to W x H with a centre crop, the
//      first N = 4k+1 frames),
//   2. depth-anything.cpp (ggml, Vulkan) writes one depth PNG per frame,
//   3. `sd-cli -M vid_gen --diffusion-model <vace gguf> --vae --t5xxl -i <ref image>
//      --control-video <depth dir> ...` (sd.cpp docs/wan.md "V2V with Wan2.1 VACE" uses
//      exactly a depth control directory) writes a .webm that ffmpeg re-encodes to mp4.
// Every temp directory (frames, depth, output) is removed on every exit path.
//
// Usage: node render/sdcpp-animate.mjs [flags] -- <out.mp4> <ref> <driver> "<prompt>"
//        --sd-bin ABS --model P --vae P --t5xxl P --backend vulkan0 --depth-bin ABS --depth-model P
//        [--frames N] [--width N] [--height N] [--steps N] [--cfg F] [--flow-shift F]
//        [--seed N] [--negative S] [--extra-args '<json>'] [--depth-extra-args '<json>']
//        [--tae PATH] [--vae-tile-overlap F]
//        [--max-tokens N --vae-stride 8|16] [--timeout-sec N] [--no-lock]
// (the positionals may also come first when none starts with "--"; the harness sends `--` before them.)
//
// THE NO-CPU RULE (same guards as sdcpp-video.mjs): a non-Vulkan --backend and any extra-args
// element that changes the backend or placement is refused before anything spawns, and BOTH
// engines' logs are POSITIVE guards (igpu-engine.mjs createLogGuard) — the first line placing
// a model on the CPU kills the tree (CPU_PLACEMENT), and a run that ends without its GPU
// evidence is CPU_PLACEMENT too: sd-cli needs the device line and a diffusion-stage compute
// buffer on Vulkan, da3-cli needs "da::Backend using device: Vulkan<N>" on EVERY frame.
// depth-anything.cpp has no backend flag (its ggml backends are compile-time:
// DA_GGML_VULKAN), so the depth step is pinned to the same Vulkan device through
// GGML_VK_VISIBLE_DEVICES and its log is the evidence it ran on the GPU.
// TOKEN CAP: --max-tokens with --vae-stride refuses a request over the cap (the VACE
// reference adds one latent frame) before anything spawns; DEADLINE: --timeout-sec counts
// from this process's start; signals and a vanished parent kill the engines and clean up.
//
// THE DEPTH STEP (verified on the target node, depth-anything.cpp 14f7461, DA_GGML_VULKAN):
//   da3-cli depth --model M --input <one frame> --png <out.png> --no-invert
// once PER FRAME. `--input a --input b` is MULTI-VIEW joint depth, not per-frame, and is never
// used. --no-invert is required: the default writes near=dark / far=bright, --no-invert writes
// near=bright / far=dark, which is the depth-control convention. The PNG is 1-channel at the
// model's working size (518x896 for a 480x832 input) and sd-cli refuses 1-channel PNGs, so every
// depth frame is converted to rgb24 at exactly W x H by ffmpeg into the directory sd-cli reads,
// and the headers are read back (igpu-qa.mjs convertDepthFrames). The model is reloaded per
// frame (about 1.5 s each, 33 frames in 51 s on the node).
//
// THE VACE ARGV (sd.cpp docs/wan.md "V2V with Wan2.1 VACE" at 3f8527a): the reference image is
// `-i`, the depth frames are `--control-video <dir>`. The docs also pass --offload-to-cpu; this
// runner never does (no model runs on the CPU, and extra args naming it are refused). The model
// must be a .safetensors VACE checkpoint: the public GGUFs lack vace_patch_embedding.weight and
// sd-cli refuses them ("model metadata validation failed"), surfaced here as MODEL_INCOMPATIBLE.
// Frame count: sd.cpp samples N+4 frames with a reference image; the measured build trims the
// reference latent itself and decodes exactly N, so the mp4 is trimmed only when the decoded
// count is exactly N+4 (igpu-qa.mjs trimDecision, probed with ffprobe). The finished clip is
// checked for black / frozen output before it is delivered.
import { existsSync, mkdirSync, readdirSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg, resolveFfprobe } from "./audio-qa.mjs";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, refuseExtraArgs, runEngine, createLogGuard, checkTokenCap,
  installLifecycle, makeDeadline, normalizeFrames, floorFrames, normalizeSize, finiteNum, encodeMp4, makeTempDir,
  vulkanDeviceFromBackend, refuseRelativeBinary, ensureOutDir, modelMetadataError,
} from "./igpu-engine.mjs";
import { checkClip, convertDepthFrames, countVideoFrames, trimDecision } from "./igpu-qa.mjs";
import { vaeTileOverlap } from "./sdcpp-video.mjs";

export { parseArgs };

export const DEFAULT_FRAMES = 49;
export const DEFAULT_WIDTH = 832;
export const DEFAULT_HEIGHT = 480;
export const DRIVER_FPS = 16;
// The output is written at the driver's extraction rate: the motion keeps the driver's timing.
export const OUTPUT_FPS = DRIVER_FPS;

// resolveParams: the normalized render parameters. Pure. Frames are requested as 4k+1
// (nearest, default 49); width/height are floored to a multiple of 32.
export function resolveParams(flags) {
  return {
    frames: normalizeFrames(finiteNum(flags.frames), DEFAULT_FRAMES),
    width: normalizeSize(flags.width, DEFAULT_WIDTH),
    height: normalizeSize(flags.height, DEFAULT_HEIGHT),
  };
}

// buildExtractArgs: ffmpeg argv for the driver's frames — DRIVER_FPS, scaled to cover W x H
// and centre-cropped (no aspect distortion), the first `frames` frames, numbered PNGs.
export function buildExtractArgs({ driver, framesDir, width, height, frames }) {
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", driver,
    "-vf", `fps=${DRIVER_FPS},scale=${width}:${height}:force_original_aspect_ratio=increase,crop=${width}:${height}`,
    "-frames:v", String(frames), join(framesDir, "%05d.png")];
}

// buildDepthArgs: depth-anything.cpp argv for ONE frame -> one depth PNG, near=bright
// (--no-invert), verified on the node. Never the multi-view form (--input a --input b).
export function buildDepthArgs({ model, input, outPng, extra = [] }) {
  return ["depth", "--model", model, "--input", input, "--png", outPng, "--no-invert", ...extra];
}

// depthEnv: pin the depth step to the Vulkan device the sd backend names (its CLI has no
// backend flag). {} when the backend names no numbered vulkan device, or the operator already
// set GGML_VK_VISIBLE_DEVICES.
export function depthEnv(backend, env = process.env) {
  const dev = vulkanDeviceFromBackend(backend);
  if (dev === "" || env.GGML_VK_VISIBLE_DEVICES) return {};
  return { GGML_VK_VISIBLE_DEVICES: dev };
}

// buildSdAnimateArgs: the sd-cli argv for the VACE depth-controlled render. Pure. -v is
// required: the CPU-placement guard reads the per-module backend lines it prints.
export function buildSdAnimateArgs({ outFile, ref, depthDir, prompt, flags, extra = [] }) {
  const p = resolveParams(flags);
  // `-i` is the VACE reference image (sd.cpp docs/wan.md "V2V": `-i <reference image>
  // --control-video <dir of frames>`); `-r/--ref-image` is for Flux Kontext / MiniMax-H3 only.
  // Never --offload-to-cpu, which that doc page also shows: no model runs on the CPU here.
  const a = ["-M", "vid_gen", "--diffusion-model", flags.model, "--vae", flags.vae, "--t5xxl", flags.t5xxl,
    "-i", ref, "--control-video", depthDir, "-p", prompt];
  if (flags.negative) a.push("-n", flags.negative);
  if (finiteNum(flags.cfg) !== undefined) a.push("--cfg-scale", String(finiteNum(flags.cfg)));
  if (finiteNum(flags.steps) !== undefined) a.push("--steps", String(Math.round(finiteNum(flags.steps))));
  if (finiteNum(flags["flow-shift"]) !== undefined) a.push("--flow-shift", String(finiteNum(flags["flow-shift"])));
  a.push("-W", String(p.width), "-H", String(p.height), "--video-frames", String(p.frames), "--fps", String(OUTPUT_FPS));
  if (finiteNum(flags.seed) !== undefined) a.push("-s", String(Math.round(finiteNum(flags.seed))));
  a.push("--backend", flags.backend, "--diffusion-fa", "--vae-tiling", "--vae-tile-overlap", String(vaeTileOverlap(flags)), "-v");
  if (flags.tae) a.push("--taesd", flags.tae);
  for (const e of extra) a.push(e);
  a.push("-o", outFile);
  return a;
}

// listFrames: the numbered PNGs in a directory, sorted.
export function listFrames(dir) {
  return readdirSync(dir).filter((f) => f.toLowerCase().endsWith(".png")).sort();
}

// framesToRender: how many 4k+1 frames to render given the request and what the driver
// yielded. The request when the driver has enough; else the largest 4k+1 that exists; 0
// when fewer than 5. Pure.
export function framesToRender(requested, available) {
  if (available >= requested) return requested;
  return floorFrames(available);
}

async function main() {
  installLifecycle();
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const deadline = makeDeadline(flags["timeout-sec"]);
  const [out, ref, driver, prompt] = pos;
  if (!out || !ref || !driver || !prompt) {
    console.error('usage: node sdcpp-animate.mjs <out.mp4> <ref> <driver> "<prompt>" --sd-bin P --model P --vae P --t5xxl P --backend vulkan0 --depth-bin P --depth-model P [flags]');
    process.exit(2);
  }
  const need = ["sd-bin", "model", "vae", "t5xxl", "backend", "depth-bin", "depth-model"].filter((n) => !flags[n]);
  if (need.length) {
    console.error("SDCPP ANIMATE FAILED: missing " + need.map((m) => "--" + m).join(", "));
    process.exit(2);
  }
  refuseCpuBackend(flags.backend);
  const extra = refuseExtraArgs(parseExtraArgs(flags["extra-args"]), { engine: "sdcpp", key: "--extra-args" });
  const depthExtra = refuseExtraArgs(parseExtraArgs(flags["depth-extra-args"]), { engine: "da3", key: "--depth-extra-args" });
  const p = resolveParams(flags);
  // the VACE reference image occupies one latent frame on top of the clip's
  checkTokenCap({ flags, width: p.width, height: p.height, frames: p.frames, refLatentFrames: 1 });
  // one binary-resolution rule: the harness passes absolute paths, a runner refuses anything else
  const sdBin = refuseRelativeBinary("--sd-bin", flags["sd-bin"]);
  const depthBin = refuseRelativeBinary("--depth-bin", flags["depth-bin"]);
  vaeTileOverlap(flags);
  ensureOutDir(out);
  for (const [k, v] of [["--model", flags.model], ["--vae", flags.vae], ["--t5xxl", flags.t5xxl], ["--tae", flags.tae],
    ["--depth-model", flags["depth-model"]], ["reference image", ref], ["driver clip", driver]]) {
    if (v && !existsSync(v)) {
      console.error(`SDCPP ANIMATE FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const ffmpeg = resolveFfmpeg();
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const ffprobe = resolveFfprobe(ffmpeg);
  if (!ffprobe) throw new Error("FFMPEG_UNAVAILABLE: ffprobe could not be resolved (it counts the decoded VACE frames; put it beside ffmpeg or on PATH)");

  const framesTmp = makeTempDir("sdcpp-animate-frames-");
  const depthRawTmp = makeTempDir("sdcpp-animate-depthraw-");
  const depthTmp = makeTempDir("sdcpp-animate-depth-");
  const outTmp = makeTempDir("sdcpp-animate-out-");
  try {
    // 1. the driver's frames
    const ex = spawnSync(ffmpeg, buildExtractArgs({ driver, framesDir: framesTmp.dir, width: p.width, height: p.height, frames: p.frames }),
      { encoding: "utf8", ...(deadline.active ? { timeout: deadline.remainingMs(), killSignal: "SIGKILL" } : {}) });
    if (ex.error || ex.status !== 0) {
      throw new Error("driver frame extraction failed: " + (ex.error ? ex.error.message : String(ex.stderr || "").trim().slice(-300)));
    }
    const frames = listFrames(framesTmp.dir);
    const n = framesToRender(p.frames, frames.length);
    if (n < 5) throw new Error(`the driver clip yields only ${frames.length} frame(s) at ${DRIVER_FPS} fps; at least 5 are needed`);
    if (n !== p.frames) console.error(`sdcpp-animate: driver gave ${frames.length} frames; rendering ${n} (4k+1) instead of ${p.frames}`);
    const finalFlags = { ...flags, frames: String(n) };

    const webm = join(outTmp.dir, "out.webm");
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      // 2. depth, one PNG per frame, on the GPU (guarded by the log scan)
      mkdirSync(depthTmp.dir, { recursive: true });
      const denv = depthEnv(flags.backend);
      for (let i = 0; i < n; i++) {
        deadline.enforce("depth-anything");
        const { code } = await runEngine({
          bin: depthBin,
          args: buildDepthArgs({ model: flags["depth-model"], input: join(framesTmp.dir, frames[i]), outPng: join(depthRawTmp.dir, frames[i]), extra: depthExtra }),
          env: denv, timeoutMs: deadline.remainingMs(), label: "depth-anything",
          guard: createLogGuard({ engine: "da3" }),
        });
        if (code !== 0) throw new Error(`depth-anything exited ${code} on frame ${i + 1}/${n}`);
        if (!existsSync(join(depthRawTmp.dir, frames[i]))) throw new Error(`depth-anything produced no depth image for frame ${i + 1}/${n}`);
        console.error(`sdcpp-animate: depth ${i + 1}/${n}`);
      }
      // 2b. sd-cli refuses 1-channel PNGs: rgb24 at exactly W x H into the directory it reads
      deadline.enforce("depth frame conversion");
      convertDepthFrames({ ffmpeg, rawDir: depthRawTmp.dir, outDir: depthTmp.dir, width: p.width, height: p.height, count: n, timeoutMs: deadline.remainingMs() });
      // 3. VACE with the depth directory as the control video
      const args = buildSdAnimateArgs({ outFile: webm, ref, depthDir: depthTmp.dir, prompt, flags: finalFlags, extra });
      deadline.enforce("sd-cli");
      const guard = createLogGuard({ engine: "sdcpp", echoes: [prompt, flags.negative] });
      const { code, log } = await runEngine({ bin: sdBin, args, timeoutMs: deadline.remainingMs(), label: "sd-cli", guard });
      if (code !== 0) throw modelMetadataError(log, flags.model) || new Error("sd-cli exited " + code);
      if (!existsSync(webm)) throw new Error("sd-cli exited 0 but produced no video at " + webm);
    });
    // the mp4 carries exactly N frames: probe what sd-cli decoded, drop the reference latent only if it is there
    deadline.enforce("frame count");
    const decoded = countVideoFrames(ffprobe, webm, { timeoutMs: deadline.remainingMs() });
    const trim = trimDecision(decoded, n);
    if (trim.note) console.error("sdcpp-animate: " + trim.note);
    deadline.enforce("ffmpeg mp4 encode");
    encodeMp4(ffmpeg, webm, out, OUTPUT_FPS, deadline.remainingMs(), { trimFirst: trim.trimFirst });
    deadline.enforce("clip check");
    try {
      checkClip(ffmpeg, out, { timeoutMs: deadline.remainingMs() });
    } catch (e) {
      try { rmSync(out, { force: true }); } catch { /* best effort */ }
      throw e;
    }
    console.log("WROTE", out);
  } finally {
    framesTmp.cleanup();
    depthRawTmp.cleanup();
    depthTmp.cleanup();
    outTmp.cleanup();
  }
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    console.error("SDCPP ANIMATE FAILED:", e.message);
    process.exit(1);
  });
}
