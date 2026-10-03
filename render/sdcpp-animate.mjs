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
// Usage: node render/sdcpp-animate.mjs <out.mp4> <ref> <driver> "<prompt>"
//        --sd-bin P --model P --vae P --t5xxl P --backend vulkan0 --depth-bin P --depth-model P
//        [--frames N] [--width N] [--height N] [--steps N] [--cfg F] [--flow-shift F]
//        [--seed N] [--negative S] [--extra-args '<json>'] [--depth-extra-args '<json>']
//        [--max-tokens N --vae-stride 8|16] [--timeout-sec N] [--no-lock]
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
// UNVERIFIED (named in the CT-49 report): the depth step's argv below is bound from the
// depth-anything.cpp README ("da3-cli depth --model M --input photo.jpg --png depth.png"),
// not from a --help of the binary on the target node. It is isolated in buildDepthArgs so a
// correction is one function (and animategen_depth_extra_args is the config escape hatch).
// It runs once PER FRAME, so the depth model is reloaded N times (N = 49 by default); on an
// iGPU that load is real wall time. The README also shows `--input a.jpg --input b.jpg
// --out-prefix scene` for one process over many images; its output naming is not documented,
// so it is not used until a conductor run confirms it.
import { existsSync, mkdirSync, readdirSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg } from "./audio-qa.mjs";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, refuseExtraArgs, runEngine, createLogGuard, checkTokenCap,
  installLifecycle, makeDeadline, normalizeFrames, floorFrames, normalizeSize, finiteNum, encodeMp4, makeTempDir,
  vulkanDeviceFromBackend,
} from "./igpu-engine.mjs";

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

// buildDepthArgs: depth-anything.cpp argv for ONE frame -> one depth PNG. UNVERIFIED against
// the node's binary (see the header): bound from the project README.
export function buildDepthArgs({ model, input, outPng, extra = [] }) {
  return ["depth", "--model", model, "--input", input, "--png", outPng, ...extra];
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
  const a = ["-M", "vid_gen", "--diffusion-model", flags.model, "--vae", flags.vae, "--t5xxl", flags.t5xxl,
    "-i", ref, "--control-video", depthDir, "-p", prompt];
  if (flags.negative) a.push("-n", flags.negative);
  if (finiteNum(flags.cfg) !== undefined) a.push("--cfg-scale", String(finiteNum(flags.cfg)));
  if (finiteNum(flags.steps) !== undefined) a.push("--steps", String(Math.round(finiteNum(flags.steps))));
  if (finiteNum(flags["flow-shift"]) !== undefined) a.push("--flow-shift", String(finiteNum(flags["flow-shift"])));
  a.push("-W", String(p.width), "-H", String(p.height), "--video-frames", String(p.frames), "--fps", String(OUTPUT_FPS));
  if (finiteNum(flags.seed) !== undefined) a.push("-s", String(Math.round(finiteNum(flags.seed))));
  a.push("--backend", flags.backend, "--diffusion-fa", "--vae-tiling", "-v");
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
  for (const [k, v] of [["--sd-bin", flags["sd-bin"]], ["--model", flags.model], ["--vae", flags.vae], ["--t5xxl", flags.t5xxl],
    ["--depth-bin", flags["depth-bin"]], ["--depth-model", flags["depth-model"]], ["reference image", ref], ["driver clip", driver]]) {
    if (!existsSync(v)) {
      console.error(`SDCPP ANIMATE FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const ffmpeg = resolveFfmpeg();
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");

  const framesTmp = makeTempDir("sdcpp-animate-frames-");
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
          bin: flags["depth-bin"],
          args: buildDepthArgs({ model: flags["depth-model"], input: join(framesTmp.dir, frames[i]), outPng: join(depthTmp.dir, frames[i]), extra: depthExtra }),
          env: denv, timeoutMs: deadline.remainingMs(), label: "depth-anything",
          guard: createLogGuard({ engine: "da3" }),
        });
        if (code !== 0) throw new Error(`depth-anything exited ${code} on frame ${i + 1}/${n}`);
        if (!existsSync(join(depthTmp.dir, frames[i]))) throw new Error(`depth-anything produced no depth image for frame ${i + 1}/${n}`);
        console.error(`sdcpp-animate: depth ${i + 1}/${n}`);
      }
      // 3. VACE with the depth directory as the control video
      const args = buildSdAnimateArgs({ outFile: webm, ref, depthDir: depthTmp.dir, prompt, flags: finalFlags, extra });
      deadline.enforce("sd-cli");
      const guard = createLogGuard({ engine: "sdcpp", echoes: [prompt, flags.negative] });
      const { code } = await runEngine({ bin: flags["sd-bin"], args, timeoutMs: deadline.remainingMs(), label: "sd-cli", guard });
      if (code !== 0) throw new Error("sd-cli exited " + code);
      if (!existsSync(webm)) throw new Error("sd-cli exited 0 but produced no video at " + webm);
    });
    deadline.enforce("ffmpeg mp4 encode");
    encodeMp4(ffmpeg, webm, out, OUTPUT_FPS, deadline.remainingMs());
    console.log("WROTE", out);
  } finally {
    framesTmp.cleanup();
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
