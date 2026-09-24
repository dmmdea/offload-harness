// comfy-video.mjs — local image-to-video runner. Animates a still into a short b-roll
// clip via ComfyUI. PRIMARY model Wan 2.2 14B I2V (default: the native quality recipe;
// --fast = the 8-step lightx2v distill); SECONDARY HunyuanVideo 1.5 480p I2V (--model
// hunyuan; needs 3 files absent on the 16GB box).
// Single-slot GPU-locked + zero-always-warm (frees llama-swap before, frees ComfyUI after).
// Dependency-free (Node 18+). Mirrors render/comfy-render.mjs.
//
// Usage:
//   node render/comfy-video.mjs <out.mp4> <still.(png|jpg)> "<prompt>" \
//        [--model wan|hunyuan] [--frames 49] [--width 832] [--height 480] \
//        [--steps N] [--cfg X] [--fast] [--hero] [--seed N] [--negative "..."] \
//        [--wan-vvram-gb 7] [--wan-loader auto|native|gguf-distorch] \
//        [--upscale-model name.pth] [--upscale-width 1920] [--upscale-height 1080] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy]   |   <out.mp4> --graph wf.json
//   --fast: OPT-IN distilled speed path (wan; 8-step lightx2v, weaker motion). --hero:
//   accepted for compatibility; the native no-LoRA pass IS the default. --wan-vvram-gb:
//   GiB of each Wan expert DisTorch2 parks in system RAM (the harness passes this box's
//   videogen_wan_virtual_vram_gb; ignored by an expert on the native loader). --wan-loader:
//   auto (default, decides per expert file by extension) | native (plain UNETLoader, no
//   DisTorch2/MultiGPU, dynamic-VRAM streaming does the offload; refused on a .gguf
//   expert) | gguf-distorch (forces the historical DisTorch2/MultiGPU wrapper on both
//   experts). --upscale-model: post-decode ESRGAN upscale
//   (+ --upscale-width/height to resize, e.g. 720p->1080p).
//
// Before anything is submitted, every node class the graph names is checked against the
// running ComfyUI's /object_info (render/comfy-nodes.mjs): a missing custom-node pack is a
// one-line MISSING_NODE defer naming the class and the pack, not a 400 at the POST.
import { writeFileSync, copyFileSync, readFileSync, existsSync } from "node:fs";
import { basename, join } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { assertNodeClasses } from "./comfy-nodes.mjs";
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";
import { buildWan22I2V } from "./wf-wan22-i2v.mjs";
import { buildLtx25I2V } from "./wf-ltx25-i2v.mjs";
import { buildH3AV } from "./wf-h3-av.mjs";
import { buildAceStep } from "./wf-acestep.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

// BOOL_FLAGS take no value. --fast was missing from this list, so the parser took the
// NEXT token as its value: a trailing --fast (the harness appends it after --hero) read
// as undefined and silently rendered the native recipe, and a --fast followed by
// --upscale-model swallowed that flag and turned its model name into a positional.
export const BOOL_FLAGS = Object.freeze(["no-lock", "keep-comfy", "hero", "fast"]);

// parseArgs splits argv into positionals + flags; BOOL_FLAGS are true when present, every
// other --flag takes the next token as its value.
export function parseArgs(argv) {
  const pos = []; const flags = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) {
      const k = argv[i].slice(2);
      if (BOOL_FLAGS.includes(k)) flags[k] = true;
      else { flags[k] = argv[i + 1]; i++; }
    } else pos.push(argv[i]);
  }
  return { pos, flags };
}

// wanVvramGb parses --wan-vvram-gb: undefined when absent (the builder keeps its own
// default), a positive number otherwise. Anything else is refused rather than guessed —
// a wrong split either OOMs the card or parks the whole expert in RAM.
export function wanVvramGb(flags) {
  const raw = flags["wan-vvram-gb"];
  if (raw === undefined) return undefined;
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) {
    throw new Error(`--wan-vvram-gb must be a positive number of GiB, got ${JSON.stringify(raw)}`);
  }
  return n;
}

// ComfyUI's LoadImage reads from <COMFY_DIR>/input. Stage the still there.
function stageInput(stillPath) {
  const name = "render_in_" + Date.now() + "_" + basename(stillPath);
  copyFileSync(stillPath, join(COMFY_DIR, "input", name));
  return name;
}

// buildGraphFromArgs builds the API-format graph the flags describe. stage copies a still
// into ComfyUI's input dir and returns the name LoadImage reads (injectable for tests).
export function buildGraphFromArgs(pos, flags, { stage = stageInput } = {}) {
  // model default is wan (Hunyuan needs files absent on this box). Declared here (NOT at
  // the graph-selection line) so the width/length ternaries below and the log line can
  // read it without a temporal-dead-zone ReferenceError.
  let graph;
  const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));
  const model = flags.model || "wan";
  if (flags.graph) {
    graph = JSON.parse(readFileSync(flags.graph, "utf8"));
  } else if (flags.model === "ace") {
    // text-to-music (ACE-Step): no still is USED — but a caller may still SUPPLY one,
    // because the Go pipeline builds `<out> <still> <prompt>` whenever a still is
    // present, for every model. Reading pos[1] unconditionally therefore passed the
    // IMAGE PATH as the style-tags prompt for `model:"ace"` + still (found by the
    // 0.73.1 review round). When three positionals arrive the prompt is the third;
    // with two it is the second, matching the documented CLI shape.
    const prompt = pos[2] || pos[1] || flags.prompt;
    if (!prompt) { console.error('error: --model ace needs a "<style tags>" prompt (e.g. "upbeat corporate, 120 bpm")'); process.exit(2); }
    // Fail-loud guard for the residual shape (still supplied, prompt omitted): a
    // "prompt" that names an existing file is the bug, not a style description.
    // Log the resolved style prompt: output audio alone cannot show WHICH text
    // conditioned it, and this line is what makes that provable from a run log.
    console.log("ace style prompt:", prompt);
    if (existsSync(prompt)) {
      console.error('error: --model ace received a file path (' + prompt + ') where style tags belong — text-to-music uses no still image; pass a prompt like "upbeat corporate, 120 bpm"');
      process.exit(2);
    }
    const common = { prompt, seed, seconds: Number(flags.seconds || 30) };
    if (flags.steps) common.steps = Number(flags.steps);
    graph = buildAceStep(common);
  } else if (flags.model === "h3") {
    // MiniMax-H3 joint-AV (opt-in family, T4 verdict recipe): the still is
    // OPTIONAL — no still = t2v (storyboard direction, which H3 uniquely
    // follows), still = i2v. Same positional flexibility as the ace branch.
    const still = pos[2] ? pos[1] : "";
    const prompt = pos[2] || pos[1] || flags.prompt;
    if (!prompt) { console.error('error: --model h3 needs a "<prompt>" (still optional: t2v without, i2v with)'); process.exit(2); }
    const common = { prompt, seed };
    if (still) common.imagePath = stage(still);
    if (flags.width) common.width = Number(flags.width);
    if (flags.height) common.height = Number(flags.height);
    if (flags.frames) common.length = Number(flags.frames);
    if (flags.seconds) common.seconds = Number(flags.seconds);
    if (flags.steps) common.steps = Number(flags.steps);
    if (flags.hero) common.hero = true; // non-LoRA 20-step path; turbo-8 is the default
    if (flags.fps) common.frameRate = Number(flags.fps);
    graph = buildH3AV(common);
  } else {
    const still = pos[1], prompt = pos[2] || flags.prompt;
    if (!still || !prompt) { console.error('error: need <still> and "<prompt>" (or --graph)'); process.exit(2); }
    const imageName = stage(still);
    const common = {
      imagePath: imageName, prompt, negative: flags.negative || "", seed,
      // family-native defaults: ltx25 = the bench-proven 1920x1088@24fps 5s recipe
      width: Number(flags.width || (model === "hunyuan" ? 848 : model === "ltx25" ? 1920 : 832)),
      height: Number(flags.height || (model === "ltx25" ? 1088 : 480)),
      length: Number(flags.frames || (model === "hunyuan" ? 33 : model === "ltx25" ? 121 : 49)),
    };
    if (flags.steps) common.steps = Number(flags.steps);
    if (flags.cfg) common.cfg = Number(flags.cfg);
    if (flags.hero) common.hero = true; // backward compat: native IS the default now
    if (flags.fast) common.fast = true; // OPT-IN distilled speed path (wan)
    // Per-machine weight binding (quality-first): the machine's config names its Wan
    // expert weights + text encoder; unset = the builder's defaults (unchanged).
    if (flags["high-unet"]) common.highUnet = flags["high-unet"];
    if (flags["low-unet"]) common.lowUnet = flags["low-unet"];
    if (flags["text-encoder"]) common.textEncoder = flags["text-encoder"];
    // Per-machine DisTorch2 split for the Wan experts (videogen_wan_virtual_vram_gb):
    // unset = the builder's default. Measured per card — never a shared constant.
    const vvram = wanVvramGb(flags);
    if (vvram !== undefined) common.virtualVramGb = vvram;
    // videogen_wan_loader (this box's, or a videogen_families[wan22] override):
    // "auto" (unset) decides per expert file by extension, unchanged behavior.
    if (flags["wan-loader"]) common.loader = flags["wan-loader"];
    if (flags["upscale-model"]) {       // optional post-decode upscale (wan)
      common.upscaleModel = flags["upscale-model"];
      if (flags["upscale-width"]) common.upscaleWidth = Number(flags["upscale-width"]);
      if (flags["upscale-height"]) common.upscaleHeight = Number(flags["upscale-height"]);
    }
    if (model === "ltx25") {
      // LTX-2.5 joint-AV family (Seat Frontier Leg 3): per-machine weight binding
      // comes from config via these flags; pooled loading per the 32GB doctrine.
      if (flags.transformer) common.transformer = flags.transformer;
      if (flags["text-encoder"]) common.textEncoder = flags["text-encoder"];
      if (flags["video-vae"]) common.videoVae = flags["video-vae"];
      if (flags["audio-vae"]) common.audioVae = flags["audio-vae"];
      if (flags["latent-upscaler"]) common.latentUpscaler = flags["latent-upscaler"];
      if (flags.fps) common.frameRate = Number(flags.fps);
      if (flags["pool-vvram-gb"]) common.poolVvramGb = Number(flags["pool-vvram-gb"]);
      if (flags["pool-compute"]) common.poolCompute = flags["pool-compute"];
      if (flags["pool-donor"]) common.poolDonor = flags["pool-donor"];
      graph = buildLtx25I2V(common);
    } else {
      graph = model === "hunyuan" ? buildHunyuan15I2V(common) : buildWan22I2V(common);
    }
  }
  return { graph, seed, model };
}

// submitChecked is the ONE path a graph takes to ComfyUI: node-class preflight first,
// then the shared submission. A class the server positively lacks throws MISSING_NODE and
// nothing is POSTed. Deps are injectable for tests.
export async function submitChecked({ api, graph, clientId, cli, fetchImpl = fetch, submit = submitGraph, comfyDir = COMFY_DIR }) {
  await assertNodeClasses(api, graph, { fetchImpl, comfyDir });
  return submit({ api, graph, clientId, cli });
}

async function generate(out, API, pos, flags) {
  const { graph, seed, model } = buildGraphFromArgs(pos, flags);
  // Shared submission/polling/retrieval (comfy-submit.mjs): CLI-preferred submit with
  // byte-identical raw fallback; hardened poll loop (dead-server watchdog).
  const cli = resolveCli();
  const { promptId } = await submitChecked({ api: API, graph, clientId: "video-" + seed, cli });
  console.log("queued", promptId, flags.graph ? `(graph ${flags.graph})` : `${model} seed ${seed}`);
  // Poll budget: the native quality recipe at 720p legitimately exceeds the old ~20-min
  // ceiling. Default 90 min; the Go harness passes COMFY_WAIT_SEC aligned to its own
  // videogen timeout and its process-tree kill remains the hard stop.
  const waitSec = Number(flags["wait-sec"] || process.env.COMFY_WAIT_SEC || 5400);
  const h = await pollOutputs({
    api: API, promptId, waitSec,
    isDone: (entry) => !!firstOutputFile(entry.outputs, graph),
    noOutputMsg: "no video produced in time",
    onExecError: () => finalizeRun({ api: API, promptId, cli }),
  });
  const file = firstOutputFile(h.outputs, graph);
  writeFileSync(out, await fetchView({ api: API, file }));
  console.log("WROTE", out);
  await finalizeRun({ api: API, promptId, cli });
}

async function main() {
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const out = pos[0];
  const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
  if (!out) { console.error('usage: node comfy-video.mjs <out.mp4> <still> "<prompt>" [--model hunyuan|wan] [flags]   |   <out.mp4> --graph wf.json'); process.exit(2); }
  await withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
    () => generate(out, API, pos, flags),
  );
}

// Run only as a script (the harness spawns this file), never on import — the test imports
// the pure seams. endsWith, not a URL comparison: the harness may reach render/ through a
// junction, and Node reports the main module by its real path.
if (process.argv[1] && /comfy-video\.mjs$/.test(process.argv[1])) {
  main().catch((e) => {
    console.error("VIDEO GEN FAILED:", e.message);
    // exitCode, not process.exit(): exiting while a fetch socket is still closing trips
    // Windows libuv's UV_HANDLE_CLOSING assertion (exit 0xc0000409), which buried the real
    // failure under a crash (reg3, 2026-09-22; render/preflight-graph.mjs has the same
    // note). The unref'd timer is the backstop if a handle keeps the loop alive.
    process.exitCode = 1;
    setTimeout(() => process.exit(1), 5000).unref();
  });
}
