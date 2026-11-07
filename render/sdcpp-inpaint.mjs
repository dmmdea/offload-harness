// sdcpp-inpaint.mjs — masked image repaint on the iGPU tier with no ComfyUI: stable-diffusion.cpp's
// img2img with a mask, on the image lane's own model (Z-Image Turbo needs no dedicated inpaint
// checkpoint: sd.cpp blends the denoise mask into the latents for every model), spawn-per-job
// under the shared GPU lease, nothing resident:
//
//   sd-cli --diffusion-model <z> --vae <ae> --llm <qwen> -i <in> --mask <mask> -p <prompt>
//          --steps 8 --cfg-scale 1 -s <seed> --vae-tiling --backend vulkanN -v --strength 1.0 -o <out>
//
// (white in the mask = repaint, black = keep; the same convention as the ComfyUI route.)
//
// Usage: node render/sdcpp-inpaint.mjs [flags] -- <out.png> <image> <mask> "<prompt>"
//        --sd-bin ABS --model P --backend vulkan0
//        [--model-kind checkpoint|diffusion] [--vae P] [--clip-l P] [--clip-g P] [--t5xxl P] [--llm P]
//        [--negative S] [--steps N] [--cfg F] [--sampler S] [--seed N]
//        [--denoise F (default 1.0, becomes --strength)] [--grow-mask N (default 16)]
//        [--extra-args '<json>'] [--timeout-sec N] [--no-lock]
//
// THE COMPOSITE. The VAE round trip moves the pixels the mask never covered (a mean of 2.5/255 was
// measured), so the render is never delivered as it is: it is merged back over the SOURCE through the
// mask (ffmpeg maskedmerge), which leaves the source exact outside the seam.
//
// THE MASK is grown by --grow-mask pixels first (default 16, the ComfyUI route's own default; 0 = no
// growth). A tight mask left a ghost of the old content at its edge, because the feather pulls source
// pixels back in. The GROWN mask is used for BOTH sd-cli and the composite, and the composite feathers
// it with a blur of about grow/3 pixels, so the original mask area stays fully repainted. The mask is
// scaled to the source's size first (nearest) when it differs.
//
// SIZES. sd-cli gets a PNG working copy of the source, snapped to a multiple of 16 per side (a latent
// model's grid), with the mask at the same size; its output is scaled back to the source's size before
// the composite. The delivered picture has the source's exact size (alpha is dropped).
//
// A render that is a flat picture while the source is not (BLACK_CLIP), or a result identical to the
// source (FROZEN_CLIP), is refused, the way the video runners refuse a black or frozen clip.
//
// THE NO-CPU RULE (same as the other iGPU runners): a non-Vulkan --backend and any extra-args element
// that changes the backend or placement is refused before anything spawns, and the engine's log is a
// POSITIVE guard (igpu-engine.mjs createLogGuard). The VAE stays on the GPU; --vae-tiling is the
// stability lever. The image lane's sdcpp_extra_args are NOT forwarded (on the AMD tier they hold
// --vae-on-cpu); this route has its own inpaint_sdcpp_extra_args.
import { existsSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { resolveFfmpeg } from "./audio-qa.mjs";
import { imageSize } from "./image-size.mjs";
import { partialSibling, commitPartial, discardPartial } from "./atomic-out.mjs";
import { BLACK_CLIP, FROZEN_CLIP, defaultRun, runFailure } from "./igpu-qa.mjs";
import {
  parseArgs, parseExtraArgs, refuseCpuBackend, refuseExtraArgs, runEngine, createLogGuard,
  installLifecycle, makeDeadline, finiteNum, makeTempDir, registerCleanup, refuseRelativeBinary, ensureOutDir,
  engineExitError, failAndExit,
} from "./igpu-engine.mjs";

export { parseArgs };

export const DEFAULT_GROW_MASK = 16;
export const DEFAULT_DENOISE = 1.0;

// resolveParams: the normalized numeric parameters. Pure. grow-mask is a non-negative integer (0 = none);
// denoise is in (0, 1].
export function resolveParams(flags) {
  let grow = DEFAULT_GROW_MASK;
  if (flags["grow-mask"] !== undefined && flags["grow-mask"] !== "") {
    const g = finiteNum(flags["grow-mask"]);
    if (g === undefined || g < 0 || !Number.isInteger(g)) throw new Error(`--grow-mask must be a non-negative integer, got ${JSON.stringify(flags["grow-mask"])}`);
    grow = g;
  }
  let denoise = DEFAULT_DENOISE;
  if (flags.denoise !== undefined && flags.denoise !== "") {
    const d = finiteNum(flags.denoise);
    if (d === undefined || d <= 0 || d > 1) throw new Error(`--denoise must be in (0, 1], got ${JSON.stringify(flags.denoise)}`);
    denoise = d;
  }
  return { grow, denoise };
}

// snap16: a side snapped to the nearest multiple of 16 (minimum 16).
export function snap16(n) {
  return Math.max(16, Math.round(Number(n) / 16) * 16);
}

// buildMaskArgs: ffmpeg argv that writes the grown mask as a gray PNG at width x height. The mask is
// scaled with nearest first, then grown (a blur of grow/2 and a threshold, which grows a hard edge by
// about 0.77 x grow px). grow 0 = no growth. Pure.
export function buildMaskArgs({ mask, dst, width, height, grow }) {
  let vf = `scale=${width}:${height}:flags=neighbor,format=gray`;
  if (grow > 0) vf += `,gblur=sigma=${grow / 2},lut=y='if(gt(val,16),255,0)'`;
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", mask, "-vf", vf, "-frames:v", "1", "-update", "1", dst];
}

// buildWorkArgs: ffmpeg argv for the PNG working copy of the source (rgb24, no alpha) at width x height. Pure.
export function buildWorkArgs({ src, dst, width, height, srcWidth, srcHeight }) {
  const scale = width !== srcWidth || height !== srcHeight ? `scale=${width}:${height}:flags=lanczos,` : "";
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", src, "-vf", `${scale}format=rgb24`, "-frames:v", "1", "-update", "1", dst];
}

// featherSigma: the composite's blur of the grown mask: about grow/3, so the original mask area (grow/4
// to 3 x grow/4 inside the grown edge) stays fully repainted. 0 = a hard merge. Pure.
export function featherSigma(grow) {
  return grow > 0 ? Math.round((grow / 3) * 100) / 100 : 0;
}

// buildCompositeArgs: ffmpeg argv that merges the render over the source through the grown mask:
// source exact where the mask is black, render where it is white, a feathered blend between. Inputs
// are the source, the render and the grown mask at the SOURCE's size; the render is scaled to it.
// Pure. (Measured: a difference of 0.000 from the source far from the seam.)
export function buildCompositeArgs({ src, render, mask, dst, width, height, sigma }) {
  const m = sigma > 0 ? `[2:v]format=gbrp,gblur=sigma=${sigma}[m]` : "[2:v]format=gbrp[m]";
  const graph = `${m};[0:v]format=gbrp[a];[1:v]scale=${width}:${height}:flags=lanczos,format=gbrp[b];[a][b][m]maskedmerge,format=rgb24`;
  return ["-hide_banner", "-loglevel", "error", "-y", "-i", src, "-i", render, "-i", mask, "-filter_complex", graph, "-frames:v", "1", "-update", "1", dst];
}

// buildDiffArgs: ffmpeg argv that prints the luma range of the AMPLIFIED per-channel difference of two
// same-sized pictures (any channel off by one level reads as full scale), so YMAX 0 means identical. Pure.
export function buildDiffArgs(a, b) {
  const amp = "clip(val*255,0,255)";
  return ["-hide_banner", "-loglevel", "error", "-i", a, "-i", b, "-filter_complex",
    `[0:v]format=gbrp[x];[1:v]format=gbrp[y];[x][y]blend=all_mode=difference,lutrgb=r='${amp}':g='${amp}':b='${amp}',format=gray,signalstats,metadata=print:file=-`,
    "-f", "null", "-"];
}

// buildLumaArgs: ffmpeg argv that prints a picture's luma range. Pure.
export function buildLumaArgs(file) {
  return ["-hide_banner", "-loglevel", "error", "-i", file, "-vf", "format=gray,signalstats,metadata=print:file=-", "-f", "null", "-"];
}

// parseLumaRange: {min, max} of the lavfi.signalstats.YMIN / YMAX lines the metadata filter printed
// (across all frames), or null when none were printed. Pure.
export function parseLumaRange(text) {
  let min = Infinity;
  let max = -Infinity;
  for (const m of String(text ?? "").matchAll(/lavfi\.signalstats\.(YMIN|YMAX)=(\d+(?:\.\d+)?)/g)) {
    const v = Number(m[2]);
    if (m[1] === "YMIN") min = Math.min(min, v);
    else max = Math.max(max, v);
  }
  return Number.isFinite(min) && Number.isFinite(max) ? { min, max } : null;
}

// buildSdInpaintArgs: the sd-cli argv. Pure. -v is required: the placement guard reads the per-module
// backend lines it prints.
export function buildSdInpaintArgs({ outFile, image, mask, prompt, width, height, flags, denoise, extra = [] }) {
  const a = [];
  if (flags["model-kind"] === "diffusion") a.push("--diffusion-model", flags.model);
  else a.push("-m", flags.model);
  for (const [ours, theirs] of [["vae", "--vae"], ["clip-l", "--clip_l"], ["clip-g", "--clip_g"], ["t5xxl", "--t5xxl"], ["llm", "--llm"]]) {
    if (flags[ours]) a.push(theirs, flags[ours]);
  }
  a.push("-i", image, "--mask", mask, "-p", prompt);
  if (flags.negative) a.push("-n", flags.negative);
  a.push("-W", String(width), "-H", String(height));
  if (finiteNum(flags.steps) !== undefined) a.push("--steps", String(Math.round(finiteNum(flags.steps))));
  if (finiteNum(flags.seed) !== undefined) a.push("-s", String(Math.round(finiteNum(flags.seed))));
  if (finiteNum(flags.cfg) !== undefined) a.push("--cfg-scale", String(finiteNum(flags.cfg)));
  if (flags.sampler) a.push("--sampling-method", flags.sampler);
  a.push("--strength", String(denoise), "--vae-tiling", "--backend", flags.backend, "-v");
  for (const e of extra) if (e) a.push(e);
  a.push("-o", outFile);
  return a;
}

// ffmpegStep: run ffmpeg once under the runner's deadline; throws with the stderr tail on failure.
function ffmpegStep(ffmpeg, args, what, deadline) {
  deadline.enforce(what);
  const r = defaultRun(ffmpeg, args, { timeoutMs: deadline.remainingMs() });
  if (r.error || r.status !== 0) throw runFailure(what, r);
  return r;
}

// lumaRange: a picture's luma range (or, given `other`, the range of its amplified difference to it).
function lumaRange(ffmpeg, file, other, what, deadline) {
  const r = ffmpegStep(ffmpeg, other ? buildDiffArgs(file, other) : buildLumaArgs(file), what, deadline);
  const range = parseLumaRange(r.stdout);
  if (!range) throw new Error(`UNMEASURABLE: ${what} printed no luma statistics for ${file}`);
  return range;
}

async function main() {
  installLifecycle();
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const deadline = makeDeadline(flags["timeout-sec"]);
  const [out, image, mask, prompt] = pos;
  if (!out || !image || !mask || !prompt) {
    console.error('usage: node sdcpp-inpaint.mjs <out.png> <image> <mask> "<prompt>" --sd-bin ABS --model P --backend vulkan0 [flags]');
    process.exit(2);
  }
  const need = ["sd-bin", "model", "backend"].filter((n) => !flags[n]);
  if (need.length) {
    console.error("SDCPP INPAINT FAILED: missing " + need.map((m) => "--" + m).join(", "));
    process.exit(2);
  }
  refuseCpuBackend(flags.backend);
  const extra = refuseExtraArgs(parseExtraArgs(flags["extra-args"]), { engine: "sdcpp", key: "--extra-args" });
  const { grow, denoise } = resolveParams(flags);
  const sdBin = refuseRelativeBinary("--sd-bin", flags["sd-bin"]);
  ensureOutDir(out);
  for (const [k, v] of [["--model", flags.model], ["--vae", flags.vae], ["--clip-l", flags["clip-l"]], ["--clip-g", flags["clip-g"]],
    ["--t5xxl", flags.t5xxl], ["--llm", flags.llm], ["source image", image], ["mask", mask]]) {
    if (v && !existsSync(v)) {
      console.error(`SDCPP INPAINT FAILED: ${k} not found: ${v}`);
      process.exit(1);
    }
  }
  const src = imageSize(readFileSync(image));
  if (!(src.width > 0 && src.height > 0)) throw new Error(`cannot read the size of ${image} (PNG, JPEG and WebP are read)`);
  const ffmpeg = resolveFfmpeg();
  if (!ffmpeg) throw new Error("FFMPEG_UNAVAILABLE: ffmpeg could not be resolved (set ffmpeg_path, or put ffmpeg on PATH)");
  const workW = snap16(src.width);
  const workH = snap16(src.height);

  const tmp = makeTempDir("sdcpp-inpaint-");
  const staged = partialSibling(out, { extLast: true });
  const unregister = registerCleanup(() => discardPartial(staged));
  try {
    // 1. the grown mask at the source's size (the composite's), the working copy and the mask at the working size (sd-cli's)
    const growSrc = join(tmp.dir, "mask-src.png");
    const work = join(tmp.dir, "work.png");
    const growWork = join(tmp.dir, "mask-work.png");
    ffmpegStep(ffmpeg, buildMaskArgs({ mask, dst: growSrc, width: src.width, height: src.height, grow }), "mask preparation", deadline);
    if (lumaRange(ffmpeg, growSrc, null, "mask check", deadline).max === 0) {
      throw new Error(`the mask ${mask} repaints nothing: it has no white pixel (white = repaint, black = keep)`);
    }
    ffmpegStep(ffmpeg, buildWorkArgs({ src: image, dst: work, width: workW, height: workH, srcWidth: src.width, srcHeight: src.height }), "source preparation", deadline);
    const workMask = workW === src.width && workH === src.height ? growSrc : growWork;
    if (workMask === growWork) {
      ffmpegStep(ffmpeg, buildMaskArgs({ mask: growSrc, dst: growWork, width: workW, height: workH, grow: 0 }), "mask resize", deadline);
    }

    // 2. the render, on the GPU, guarded by the log scan
    const raw = join(tmp.dir, "render.png");
    await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
      deadline.enforce("sd-cli");
      const args = buildSdInpaintArgs({ outFile: raw, image: work, mask: workMask, prompt, width: workW, height: workH, flags, denoise, extra });
      const guard = createLogGuard({ engine: "sdcpp", echoes: [prompt, flags.negative] });
      const { code, log } = await runEngine({ bin: sdBin, args, timeoutMs: deadline.remainingMs(), label: "sd-cli", guard });
      if (code !== 0) throw engineExitError("sd-cli", code, log, flags.model);
    });
    if (!existsSync(raw) || statSync(raw).size === 0) throw new Error("sd-cli exited 0 but produced no image at " + raw);

    // 3. a flat render of a picture that is not flat is a failed render
    const made = lumaRange(ffmpeg, raw, null, "render check", deadline);
    const before = lumaRange(ffmpeg, image, null, "source check", deadline);
    if (made.max - made.min < 2 && before.max - before.min >= 2) {
      throw new Error(`${BLACK_CLIP}: sd-cli's render is a flat picture (luma ${made.min}..${made.max}) while the source is not; refused`);
    }

    // 4. the composite: the source exact outside the (grown, feathered) mask
    ffmpegStep(ffmpeg, buildCompositeArgs({ src: image, render: raw, mask: growSrc, dst: staged, width: src.width, height: src.height, sigma: featherSigma(grow) }), "composite", deadline);
    const done = imageSize(readFileSync(staged));
    if (done.width !== src.width || done.height !== src.height) {
      throw new Error(`the composite is ${done.width}x${done.height}, expected ${src.width}x${src.height}`);
    }
    if (lumaRange(ffmpeg, image, staged, "change check", deadline).max === 0) {
      throw new Error(`${FROZEN_CLIP}: the repaint changed nothing: the result is identical to the source; refused`);
    }
    commitPartial(staged, out);
    console.log("WROTE", out);
  } finally {
    unregister();
    discardPartial(staged);
    tmp.cleanup();
  }
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    failAndExit("SDCPP INPAINT", e);
  });
}
