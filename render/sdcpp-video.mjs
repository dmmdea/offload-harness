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
// Usage: node render/sdcpp-video.mjs <out.mp4> [<still>] "<prompt>"
//        --sd-bin PATH --model PATH [--high-noise-model PATH] --vae PATH --t5xxl PATH
//        --backend vulkan0 [--frames N] [--width N] [--height N] [--fps N] [--steps N]
//        [--cfg F] [--flow-shift F] [--sampler S] [--seed N] [--negative S]
//        [--extra-args '<json array>'] [--timeout-sec N] [--no-lock]
// Env:   FFMPEG_PATH — ffmpeg (else ffmpeg on PATH). GPU_LEASE_* — the inherited lease.
//
// Normalization (documented, tested):
//   frames  -> the NEAREST 4k+1 (a tie goes up), minimum 5; default 49. The Wan family
//              takes 4k+1 latent-aligned frame counts.
//   width/height -> FLOORED to a multiple of 32 (never grow a render past the memory the
//              caller sized for), minimum 32; defaults 832x480.
// sd-cli writes a .webm; this script re-encodes it to H.264 mp4 (yuv420p, CRF 16, the
// given fps) at <out.mp4>.
//
// THE NO-CPU RULE: a cpu (or unset) --backend is refused before anything spawns
// (CPU_BACKEND_REFUSED), and the engine's log is scanned while it runs: the first line
// that places a model on the CPU kills the process tree and fails the job with
// CPU_PLACEMENT (igpu-engine.mjs detectCpuPlacement).
import { existsSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg } from "./audio-qa.mjs";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, runEngine, normalizeFrames, normalizeSize,
  finiteNum, encodeMp4, makeTempDir,
} from "./igpu-engine.mjs";

export { parseArgs };

export const DEFAULT_FRAMES = 49;
export const DEFAULT_WIDTH = 832;
export const DEFAULT_HEIGHT = 480;
export const DEFAULT_FPS = 24;

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
  a.push("-W", String(p.width), "-H", String(p.height), "--video-frames", String(p.frames), "--fps", String(p.fps));
  if (finiteNum(flags.seed) !== undefined) a.push("-s", String(Math.round(finiteNum(flags.seed))));
  a.push("--backend", flags.backend, "--diffusion-fa", "--vae-tiling", "-v");
  for (const e of extra) a.push(e);
  a.push("-o", outFile);
  return a;
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
  const { pos, flags } = parseArgs(process.argv.slice(2));
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
  const bin = flags["sd-bin"];
  for (const [k, v] of [["--sd-bin", bin], ["--model", flags.model], ["--vae", flags.vae], ["--t5xxl", flags.t5xxl],
    ["--high-noise-model", flags["high-noise-model"]], ["image", shape.still]]) {
    if (v && !existsSync(v)) {
      console.error(`SDCPP VIDEO FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const extra = parseExtraArgs(flags["extra-args"]);
  const ffmpeg = resolveFfmpeg();
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const { fps } = resolveParams(flags);
  const timeoutMs = Math.round((finiteNum(flags["timeout-sec"]) ?? 0) * 1000);
  const tmp = makeTempDir("sdcpp-video-");
  try {
    const webm = join(tmp.dir, "out.webm");
    const args = buildSdVideoArgs({ outFile: webm, still: shape.still, prompt: shape.prompt, flags, extra });
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      const { code } = await runEngine({ bin, args, timeoutMs, label: "sd-cli" });
      if (code !== 0) throw new Error("sd-cli exited " + code);
      if (!existsSync(webm)) throw new Error("sd-cli exited 0 but produced no video at " + webm);
    });
    encodeMp4(ffmpeg, webm, pos[0], fps);
    console.log("WROTE", pos[0]);
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
