// sdcpp-upscale.mjs — image upscale on the iGPU tier with no ComfyUI: stable-diffusion.cpp's
// own upscale mode runs an ESRGAN-family weight over the picture, spawn-per-job under the
// shared GPU lease, nothing resident:
//
//   sd-cli -M upscale --upscale-model <model> -i <in> --backend vulkanN -v -o <out>
//
// (measured on a Vulkan GPU: a 512x512 input gives 2048x2048 in 8 s with the 4xNomos2_hq_mosr
// weight; no diffusion model, VAE or text encoder is loaded in this mode).
//
// Usage: node render/sdcpp-upscale.mjs [flags] -- <out.png> <in.png>
//        --sd-bin ABS --model P --backend vulkan0
//        [--scale F] [--width N --height N] [--method lanczos|bicubic|bilinear|area|nearest-exact]
//        [--extra-args '<json>'] [--timeout-sec N] [--no-lock]
//
// THE SCALE comes from the model (4x for the bound weight). A request for another size (--scale,
// or --width and --height) is met by resampling the model's output with ffmpeg to the EXACT size
// (lanczos unless --method says otherwise): the Go side rejects a result whose size is off by more
// than 2 px, and the render gate asks for --scale 2. The resample is a plain image resize, the same
// kind of operation as edit_image's resize; no model runs on the CPU.
//
// A FAILED UPSCALER LOAD IS SILENT in sd-cli: it logs "new_upscaler_ctx failed", saves the
// un-upscaled image and exits 0. So the runner never trusts the exit code: the engine's output must
// really be larger than the input, else MODEL_INCOMPATIBLE.
//
// THE NO-CPU RULE (same as the other iGPU runners): a non-Vulkan --backend and any extra-args element
// that changes the backend or placement is refused before anything spawns, and the engine's log is a
// POSITIVE guard (igpu-engine.mjs createLogGuard, stage "upscale": the device line plus the
// upscaler module's compute buffer on Vulkan; its small host-side companion buffer is fine).
//
// The finished picture is written to a staged sibling of --out and renamed onto it (atomic): a failed
// or killed run never leaves a half-written file, nor replaces a good one.
import { existsSync, readFileSync, copyFileSync, statSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { extname, join } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg } from "./audio-qa.mjs";
import { imageSize } from "./image-size.mjs";
import { partialSibling, commitPartial, discardPartial } from "./atomic-out.mjs";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, refuseExtraArgs, runEngine, createLogGuard,
  installLifecycle, makeDeadline, finiteNum, makeTempDir, registerCleanup, refuseRelativeBinary, ensureOutDir,
  engineExitError, failAndExit, MODEL_INCOMPATIBLE,
} from "./igpu-engine.mjs";

export { parseArgs };

// The resamplers the Go side accepts (internal/pipeline upscaleMethods) and ffmpeg's name for each.
export const METHOD_TO_FFMPEG = { lanczos: "lanczos", bicubic: "bicubic", bilinear: "bilinear", area: "area", "nearest-exact": "neighbor" };

// buildSdUpscaleArgs: the sd-cli argv. Pure. -v is required: the placement guard reads the per-module
// backend lines it prints.
export function buildSdUpscaleArgs({ model, input, backend, outFile, extra = [] }) {
  return ["-M", "upscale", "--upscale-model", model, "-i", input, "--backend", backend, "-v", ...extra, "-o", outFile];
}

// targetSize: the exact size the request fixes, or null (the model's own factor stands). width and height
// win over scale; a scale is applied to the measured source and rounded to the nearest pixel. Pure.
export function targetSize(src, { width, height, scale }) {
  const w = finiteNum(width);
  const h = finiteNum(height);
  if (w !== undefined && h !== undefined && w > 0 && h > 0) return { width: Math.round(w), height: Math.round(h) };
  const s = finiteNum(scale);
  if (s !== undefined && s > 0) return { width: Math.max(1, Math.round(src.width * s)), height: Math.max(1, Math.round(src.height * s)) };
  return null;
}

// buildResampleArgs: ffmpeg argv that writes ONE picture of exactly width x height (or, with no size,
// a plain re-encode to the destination's format). Pure.
export function buildResampleArgs({ input, dst, size, method = "lanczos" }) {
  const flags = METHOD_TO_FFMPEG[method];
  if (!flags) throw new Error(`--method must be one of ${Object.keys(METHOD_TO_FFMPEG).join("|")}, got ${JSON.stringify(method)}`);
  const vf = size ? ["-vf", `scale=${size.width}:${size.height}:flags=${flags}`] : [];
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", input, ...vf, "-frames:v", "1", "-update", "1", dst];
}

// verifyUpscaled: the engine's raw output must be larger than the source on at least one axis. Throws
// MODEL_INCOMPATIBLE otherwise (the silent failed-load case), naming both sizes.
export function verifyUpscaled(src, raw, model) {
  if (!(raw.width > 0 && raw.height > 0)) {
    throw new Error(`${MODEL_INCOMPATIBLE}: sd-cli exited 0 but its output is not a readable image`);
  }
  if (!(raw.width > src.width || raw.height > src.height)) {
    throw new Error(`${MODEL_INCOMPATIBLE}: sd-cli exited 0 but the output is ${raw.width}x${raw.height}, not larger than the ${src.width}x${src.height} input: the upscale model did not load (sd-cli saves the un-upscaled image when it cannot load it). Check that ${model} is an ESRGAN-family weight sd.cpp can read.`);
  }
}

async function main() {
  installLifecycle();
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const deadline = makeDeadline(flags["timeout-sec"]);
  const [out, input] = pos;
  if (!out || !input) {
    console.error("usage: node sdcpp-upscale.mjs <out.png> <in.png> --sd-bin ABS --model P --backend vulkan0 [--scale F] [--width N --height N] [--method M]");
    process.exit(2);
  }
  const need = ["sd-bin", "model", "backend"].filter((n) => !flags[n]);
  if (need.length) {
    console.error("SDCPP UPSCALE FAILED: missing " + need.map((m) => "--" + m).join(", "));
    process.exit(2);
  }
  refuseCpuBackend(flags.backend);
  const extra = refuseExtraArgs(parseExtraArgs(flags["extra-args"]), { engine: "sdcpp", key: "--extra-args" });
  const sdBin = refuseRelativeBinary("--sd-bin", flags["sd-bin"]);
  ensureOutDir(out);
  for (const [k, v] of [["--model", flags.model], ["input image", input]]) {
    if (!existsSync(v)) {
      console.error(`SDCPP UPSCALE FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const method = flags.method || "lanczos";
  if (!METHOD_TO_FFMPEG[method]) throw new Error(`--method must be one of ${Object.keys(METHOD_TO_FFMPEG).join("|")}, got ${JSON.stringify(method)}`);
  const src = imageSize(readFileSync(input));
  if (!(src.width > 0 && src.height > 0)) throw new Error(`cannot read the size of ${input} (PNG, JPEG and WebP are read)`);
  const target = targetSize(src, { width: flags.width, height: flags.height, scale: flags.scale });
  const ffmpeg = resolveFfmpeg();
  // ffmpeg writes the delivered picture (resample, or a re-encode to a non-PNG --out)
  const needFfmpeg = target !== null || extname(out).toLowerCase() !== ".png";
  if (needFfmpeg && !ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");

  const work = makeTempDir("sdcpp-upscale-");
  const staged = partialSibling(out, { extLast: true });
  const unregister = registerCleanup(() => discardPartial(staged));
  try {
    const raw = join(work.dir, "raw.png");
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      deadline.enforce("sd-cli");
      const args = buildSdUpscaleArgs({ model: flags.model, input, backend: flags.backend, outFile: raw, extra });
      const guard = createLogGuard({ engine: "sdcpp", stage: "upscale" });
      const { code, log } = await runEngine({ bin: sdBin, args, timeoutMs: deadline.remainingMs(), label: "sd-cli", guard });
      if (code !== 0) throw engineExitError("sd-cli", code, log, flags.model);
    });
    if (!existsSync(raw) || statSync(raw).size === 0) throw new Error("sd-cli exited 0 but produced no image at " + raw);
    const made = imageSize(readFileSync(raw));
    verifyUpscaled(src, made, flags.model);
    console.error(`sdcpp-upscale: ${src.width}x${src.height} -> ${made.width}x${made.height} by the model`);

    const resample = target !== null && (target.width !== made.width || target.height !== made.height);
    if (!resample && extname(out).toLowerCase() === ".png") {
      copyFileSync(raw, staged);
    } else {
      deadline.enforce("ffmpeg resample");
      const r = spawnSync(ffmpeg, buildResampleArgs({ input: raw, dst: staged, size: resample ? target : null, method }), {
        encoding: "utf8", ...(deadline.active ? { timeout: deadline.remainingMs(), killSignal: "SIGKILL" } : {}), windowsHide: true,
      });
      if (r.error || r.status !== 0 || !existsSync(staged)) {
        throw new Error((r.error && r.error.code === "ETIMEDOUT" ? "ffmpeg resample timeout (killed): " : "ffmpeg resample failed: ") + (r.error ? r.error.message : String(r.stderr || "").trim().slice(-300)));
      }
      if (resample) console.error(`sdcpp-upscale: resampled to ${target.width}x${target.height} (${method})`);
    }
    const done = imageSize(readFileSync(staged));
    if (target !== null && (done.width !== target.width || done.height !== target.height)) {
      throw new Error(`the delivered picture is ${done.width}x${done.height}, expected ${target.width}x${target.height}`);
    }
    commitPartial(staged, out);
    console.log("WROTE", out);
  } finally {
    unregister();
    discardPartial(staged);
    work.cleanup();
  }
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    failAndExit("SDCPP UPSCALE", e);
  });
}
