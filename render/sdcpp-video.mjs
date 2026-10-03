// sdcpp-video.mjs — image-to-video / text-to-video via stable-diffusion.cpp
// `sd-cli -M vid_gen` (CT-49: the iGPU tier's video engine). A spawn-per-job native
// binary on a Vulkan GPU under the shared GPU lease: zero-warm by construction (the
// process exits, the memory is gone), no ComfyUI, no Python. Mirrors sdcpp-generate.mjs
// for the lease, the spawn and the progress-to-stderr contract.
//
// The Go side (internal/pipeline runGenerateVideoSdcpp) speaks OUR generic flag surface;
// THIS file owns the mapping to sd-cli's real flags (verified against sd.cpp master-929,
// `sd-cli --help`): --diffusion-model / --high-noise-diffusion-model / --vae / --t5xxl,
// -i (the still), -p/-n, --cfg-scale, --steps, --sampling-method, --flow-shift, -W/-H,
// --video-frames, --fps, -s, --backend, --diffusion-fa, --vae-tiling, -v, -o. sd.cpp flag
// drift on a pin bump is fixed HERE, never in Go.
//
// Usage: node render/sdcpp-video.mjs [flags] -- <out.mp4> [<still>] "<prompt>"
//        --sd-bin ABSOLUTE-PATH --model PATH [--high-noise-model PATH] --vae PATH --t5xxl PATH
//        --backend vulkan0 [--frames N] [--width N] [--height N] [--fps N] [--steps N]
//        [--cfg F] [--flow-shift F] [--sampler S] [--seed N] [--negative S]
//        [--high-noise-cfg F] [--high-noise-steps N] [--high-noise-sampler S]
//        [--tae PATH] [--vae-tile-overlap F]
//        [--extra-args '<json array>'] [--max-tokens N --vae-stride 8|16] [--timeout-sec N] [--no-lock]
// (the positionals may also come first when none of them starts with "--"; the harness always
// sends `--` before them so a prompt such as "--- Intro ---" stays a prompt.)
// Env:   FFMPEG_PATH — ffmpeg (else ffmpeg on PATH). GPU_LEASE_* — the inherited lease.
//
// A14B high/low pair: --high-noise-cfg / --high-noise-steps / --high-noise-sampler map to
// sd-cli's --high-noise-cfg-scale / --high-noise-steps / --high-noise-sampling-method (only with
// --high-noise-model; sd-cli's own default there is cfg 7.0, which doubles the iGPU time of a
// distilled recipe). Decode: --vae-tiling with --vae-tile-overlap 0.25 by default (measured best:
// 17 frames, overlap 0.5 = 364 s, 0.25 = 299 s, untiled = 355 s); --tae PATH is the OPT-IN tiny
// autoencoder (sd-cli --taesd; taew2_2 decodes 17 frames in 2.8 s, SSIM 0.95 vs the full VAE).
//
// Normalization (documented, tested):
//   frames  -> the NEAREST 4k+1 (a tie goes up), minimum 5; default 49. The Wan family
//              takes 4k+1 latent-aligned frame counts.
//   width/height -> FLOORED to a multiple of 32 (never grow a render past the memory the
//              caller sized for), minimum 32; defaults 832x480.
// sd-cli writes a .webm; this script re-encodes it to H.264 mp4 (yuv420p, CRF 16, the
// given fps) at <out.mp4>.
//
// THE NO-CPU RULE: a non-Vulkan (cpu, unset, best/auto, ...) --backend, and any --extra-args
// element that changes the backend or placement, is refused before anything spawns
// (CPU_BACKEND_REFUSED / EXTRA_ARGS_REFUSED). The engine's log is a POSITIVE guard
// (igpu-engine.mjs createLogGuard): the run passes only with the device line and a
// diffusion-stage compute buffer on Vulkan, the first line placing compute on the CPU kills
// the process tree (CPU_PLACEMENT), and a device reset is GPU_RESET.
// TOKEN CAP: --max-tokens with --vae-stride (8 or 16) refuses a request whose latent token
// count is over the cap (TOKEN_CAP_EXCEEDED) before sd-cli is spawned; no cap = no check.
// DEADLINE: --timeout-sec counts from this process's start, so the llama-swap drain and every
// pre-spawn step spend it; SIGTERM/SIGINT/SIGHUP and a vanished parent kill the engine tree
// and remove the temp dir (installLifecycle).
import { existsSync, rmSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg } from "./audio-qa.mjs";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, refuseExtraArgs, runEngine, createLogGuard, checkTokenCap,
  installLifecycle, makeDeadline, normalizeFrames, normalizeSize, finiteNum, encodeMp4, makeTempDir,
  refuseRelativeBinary, ensureOutDir, modelMetadataError,
} from "./igpu-engine.mjs";
import { checkClip } from "./igpu-qa.mjs";

export { parseArgs };

export const DEFAULT_FRAMES = 49;
export const DEFAULT_WIDTH = 832;
export const DEFAULT_HEIGHT = 480;
export const DEFAULT_FPS = 24;
export const DEFAULT_VAE_TILE_OVERLAP = 0.25;

// resolveParams: the normalized render parameters from the parsed flags. Pure.
export function resolveParams(flags) {
  const fps = Math.round(finiteNum(flags.fps) ?? DEFAULT_FPS);
  return {
    frames: normalizeFrames(finiteNum(flags.frames), DEFAULT_FRAMES),
    width: normalizeSize(flags.width, DEFAULT_WIDTH),
    height: normalizeSize(flags.height, DEFAULT_HEIGHT),
    fps: fps > 0 ? fps : DEFAULT_FPS,
  };
}

// buildSdVideoArgs: the sd-cli argv. Pure; unit-tested without spawning. -v is NOT
// optional: the CPU-placement guard reads the per-module backend lines it prints.
export function buildSdVideoArgs({ outFile, still, prompt, flags, extra = [] }) {
  const p = resolveParams(flags);
  const a = ["-M", "vid_gen", "--diffusion-model", flags.model];
  if (flags["high-noise-model"]) a.push("--high-noise-diffusion-model", flags["high-noise-model"]);
  a.push("--vae", flags.vae, "--t5xxl", flags.t5xxl);
  if (still) a.push("-i", still);
  a.push("-p", prompt);
  if (flags.negative) a.push("-n", flags.negative);
  if (finiteNum(flags.cfg) !== undefined) a.push("--cfg-scale", String(finiteNum(flags.cfg)));
  if (finiteNum(flags.steps) !== undefined) a.push("--steps", String(Math.round(finiteNum(flags.steps))));
  if (flags.sampler) a.push("--sampling-method", flags.sampler);
  if (finiteNum(flags["flow-shift"]) !== undefined) a.push("--flow-shift", String(finiteNum(flags["flow-shift"])));
  // the high-noise expert's own recipe (A14B pair): without these sd-cli runs it at its defaults
  if (flags["high-noise-model"]) {
    if (finiteNum(flags["high-noise-cfg"]) !== undefined) a.push("--high-noise-cfg-scale", String(finiteNum(flags["high-noise-cfg"])));
    if (finiteNum(flags["high-noise-steps"]) !== undefined) a.push("--high-noise-steps", String(Math.round(finiteNum(flags["high-noise-steps"]))));
    if (flags["high-noise-sampler"]) a.push("--high-noise-sampling-method", flags["high-noise-sampler"]);
  }
  a.push("-W", String(p.width), "-H", String(p.height), "--video-frames", String(p.frames), "--fps", String(p.fps));
  if (finiteNum(flags.seed) !== undefined) a.push("-s", String(Math.round(finiteNum(flags.seed))));
  a.push("--backend", flags.backend, "--diffusion-fa", "--vae-tiling", "--vae-tile-overlap", String(vaeTileOverlap(flags)), "-v");
  if (flags.tae) a.push("--taesd", flags.tae);
  for (const e of extra) a.push(e);
  a.push("-o", outFile);
  return a;
}

// vaeTileOverlap: the VAE tile overlap (fraction of a tile): --vae-tile-overlap or 0.25.
export function vaeTileOverlap(flags) {
  const v = finiteNum(flags["vae-tile-overlap"]);
  if (v === undefined) return DEFAULT_VAE_TILE_OVERLAP;
  if (v < 0 || v >= 1) throw new Error(`--vae-tile-overlap must be in [0, 1) (got ${v})`);
  return v;
}

// splitPositionals: <out> [<still>] <prompt> -> {out, still, prompt}; null when the shape
// is wrong.
export function splitPositionals(pos) {
  if (pos.length === 2) return { out: pos[0], still: "", prompt: pos[1] };
  if (pos.length === 3) return { out: pos[0], still: pos[1], prompt: pos[2] };
  return null;
}

// requireFlags: the names of the required flags that are absent. Pure.
export function missingFlags(flags, names) {
  return names.filter((n) => !flags[n]);
}

async function main() {
  installLifecycle();
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const deadline = makeDeadline(flags["timeout-sec"]);
  const shape = splitPositionals(pos);
  if (!shape || !shape.prompt) {
    console.error('usage: node sdcpp-video.mjs <out.mp4> [<still>] "<prompt>" --sd-bin P --model P --vae P --t5xxl P --backend vulkan0 [flags]');
    process.exit(2);
  }
  const missing = missingFlags(flags, ["sd-bin", "model", "vae", "t5xxl", "backend"]);
  if (missing.length) {
    console.error("SDCPP VIDEO FAILED: missing " + missing.map((m) => "--" + m).join(", "));
    process.exit(2);
  }
  refuseCpuBackend(flags.backend);
  const extra = refuseExtraArgs(parseExtraArgs(flags["extra-args"]), { engine: "sdcpp", key: "--extra-args" });
  const params = resolveParams(flags);
  checkTokenCap({ flags, width: params.width, height: params.height, frames: params.frames });
  const bin = refuseRelativeBinary("--sd-bin", flags["sd-bin"]);
  vaeTileOverlap(flags);
  // the out dir is made (or refused) NOW: a render that ends in "cannot write the output" has spent minutes of GPU
  ensureOutDir(shape.out);
  for (const [k, v] of [["--model", flags.model], ["--vae", flags.vae], ["--t5xxl", flags.t5xxl], ["--tae", flags.tae],
    ["--high-noise-model", flags["high-noise-model"]], ["image", shape.still]]) {
    if (v && !existsSync(v)) {
      console.error(`SDCPP VIDEO FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const ffmpeg = resolveFfmpeg();
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const { fps } = params;
  const tmp = makeTempDir("sdcpp-video-");
  try {
    const webm = join(tmp.dir, "out.webm");
    const args = buildSdVideoArgs({ outFile: webm, still: shape.still, prompt: shape.prompt, flags, extra });
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      deadline.enforce("sd-cli");
      const guard = createLogGuard({ engine: "sdcpp", echoes: [shape.prompt, flags.negative] });
      const { code, log } = await runEngine({ bin, args, timeoutMs: deadline.remainingMs(), label: "sd-cli", guard });
      if (code !== 0) throw modelMetadataError(log, flags.model) || new Error("sd-cli exited " + code);
      if (!existsSync(webm)) throw new Error("sd-cli exited 0 but produced no video at " + webm);
    });
    deadline.enforce("ffmpeg mp4 encode");
    encodeMp4(ffmpeg, webm, shape.out, fps, deadline.remainingMs());
    // a clip that is entirely black or entirely frozen is a failed render that exited 0: never delivered
    deadline.enforce("clip check");
    try {
      checkClip(ffmpeg, shape.out, { timeoutMs: deadline.remainingMs() });
    } catch (e) {
      try { rmSync(shape.out, { force: true }); } catch { /* best effort */ }
      throw e;
    }
    console.log("WROTE", shape.out);
  } finally {
    tmp.cleanup();
  }
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    console.error("SDCPP VIDEO FAILED:", e.message);
    process.exit(1);
  });
}
