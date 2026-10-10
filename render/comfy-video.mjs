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
//        [--wan-vvram-gb 7] [--wan-loader auto|native|gguf-distorch] [--wan-decode auto|plain|tiled] \
//        [--upscale-model name.pth] [--upscale-width 1920] [--upscale-height 1080] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy]   |   <out.mp4> --graph wf.json
//   --fast: OPT-IN distilled speed path (wan; 8-step lightx2v, weaker motion). --hero:
//   accepted for compatibility; the native no-LoRA pass IS the default. --wan-vvram-gb:
//   GiB of each Wan expert DisTorch2 parks in system RAM (the harness passes this box's
//   videogen_wan_virtual_vram_gb; ignored by an expert on the native loader). --wan-loader:
//   auto (default, decides per expert file by extension) | native (plain UNETLoader, no
//   DisTorch2/MultiGPU, dynamic-VRAM streaming does the offload; refused on a .gguf
//   expert) | gguf-distorch (forces the historical DisTorch2/MultiGPU wrapper on both
//   experts). --wan-decode (the harness passes videogen_wan_decode; wan only): tiled (default;
//   VAEDecodeTiled, the graph's historical node) | plain (VAEDecode) | auto. plain and auto are an explicit
//   opt-in until a live render at the 16 GB tiers' shape (1280x720x81) has shown the plain decode fits there
//   (docs/systems/media-generation.md, "The Wan decode is per card"). auto reads the
//   render card's total VRAM from GET /system_stats (devices[0], the primary device) and runs the
//   plain decode on a card of at least 12 GiB, else tiled; an unreadable answer is tiled. The
//   chosen node and why go to stderr. LTX 2.5 and Hunyuan 1.5 ignore it (they keep VAEDecodeTiled).
//   --upscale-model: post-decode ESRGAN upscale (+ --upscale-width/height to resize, e.g.
//   720p->1080p).
//
// Before anything is submitted, every node class the graph names is checked against the
// running ComfyUI's /object_info (render/comfy-nodes.mjs): a missing custom-node pack is a
// one-line MISSING_NODE defer naming the class and the pack, not a 400 at the POST.
import { readFileSync, existsSync } from "node:fs";
import { writeFileAtomic } from "./atomic-out.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR, comfyApi } from "./comfy-lifecycle.mjs";
import { stageInput as stageToInput } from "./comfy-input.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { assertNodeClasses } from "./comfy-nodes.mjs";
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";
import { buildWan22I2V, chooseWanDecode, WAN_DECODE_MODES, WAN_DECODE_DEFAULT } from "./wf-wan22-i2v.mjs";
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

// wanDecodeMode parses --wan-decode (the harness passes config videogen_wan_decode): absent is the builder's
// default, tiled, and a value outside the builder's modes is refused rather than guessed, because one that
// quietly fell back to the default would hide a typo behind a render that still works. An empty or dangling
// flag is refused too.
export function wanDecodeMode(flags) {
  if (!("wan-decode" in flags)) return WAN_DECODE_DEFAULT;
  const raw = flags["wan-decode"];
  if (!WAN_DECODE_MODES.includes(raw)) {
    throw new Error(`--wan-decode must be ${WAN_DECODE_MODES.join("|")}, got ${JSON.stringify(raw)}`);
  }
  return raw;
}

// readRenderCard asks the ComfyUI this run submits to how big its render card is: GET /system_stats,
// devices[0].vram_total (bytes). devices[0] is the primary device, the one a VAE decode runs on: ComfyUI
// lists it first on purpose, so clients that read devices[0] keep working while it also lists every
// MultiGPU donor behind it. A CPU or MPS device reports the host's RAM as vram_total (ComfyUI's
// get_total_memory), which says nothing about a card, so it is no reading. Returns { vramTotal, name }, or
// { error } for every way the answer can fail to name a card; the caller then keeps the tiled decode.
export async function readRenderCard(api, { fetchImpl = fetch, timeoutMs = 8000 } = {}) {
  let r;
  try {
    r = await fetchImpl(`${api}/system_stats`, { signal: AbortSignal.timeout(timeoutMs) });
  } catch (e) {
    return { error: `GET /system_stats failed: ${e?.message || e}` };
  }
  if (!r || !r.ok) return { error: `GET /system_stats answered HTTP ${r?.status}` };
  let body;
  try { body = await r.json(); } catch { return { error: "GET /system_stats returned no JSON" }; }
  const dev = Array.isArray(body?.devices) ? body.devices[0] : undefined;
  if (!dev || typeof dev !== "object") return { error: "GET /system_stats lists no device" };
  if (dev.type === "cpu" || dev.type === "mps") {
    return { error: `the primary device is ${dev.type}, whose vram_total is the host's RAM, not a card's` };
  }
  const bytes = dev.vram_total;
  if (typeof bytes !== "number" || !Number.isFinite(bytes) || bytes <= 0) {
    return { error: "GET /system_stats gives its primary device no usable vram_total" };
  }
  return { vramTotal: bytes, name: typeof dev.name === "string" ? dev.name : "" };
}

// NON_WAN_MODELS are the --model values buildGraphFromArgs sends to a builder other than the Wan one;
// every other value falls through to Wan, exactly as the dispatch there does. A test reads this file's
// dispatch literals and fails when one is missing here, so a new family cannot start reading a card.
const NON_WAN_MODELS = Object.freeze(["ace", "h3", "ltx25", "hunyuan"]);

// runsWanGraph: does this invocation build the Wan 2.2 graph (so --wan-decode means something)?
export function runsWanGraph(flags) {
  return !flags.graph && !NON_WAN_MODELS.includes(flags.model);
}

// ComfyUI's LoadImage reads from <COMFY_DIR>/input. Stage the still there.
function stageInput(stillPath) {
  return stageToInput("render_in", stillPath);
}

// buildGraphFromArgs builds the API-format graph the flags describe. stage copies a still
// into ComfyUI's input dir and returns the name LoadImage reads (injectable for tests).
// vramTotalBytes is the render card's total VRAM when the caller has read it (buildGraphForRun
// does); only the Wan graph's --wan-decode auto reads it, and without it auto builds the tiled decode.
export function buildGraphFromArgs(pos, flags, { stage = stageInput, vramTotalBytes } = {}) {
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
      // The decode mode goes to the Wan builder alone: Hunyuan 1.5 and LTX 2.5 keep their own
      // VAEDecodeTiled (other VAEs, never measured), so their calls above are untouched by it.
      graph = model === "hunyuan" ? buildHunyuan15I2V(common) : buildWan22I2V({ ...common, decode: wanDecodeMode(flags), vramTotalBytes });
    }
  }
  return { graph, seed, model };
}

// wanDecodeLine is the one stderr line that says which decode this run built and why. The node comes
// from the graph that was built, and the reason from the same rule the builder used, so the line
// cannot claim a decode the graph does not have.
function wanDecodeLine(mode, card, graph) {
  const node = Object.values(graph).map((n) => n.class_type).find((c) => /^VAEDecode/.test(c)) || "no VAE decode node";
  const detail = card?.error ? `; ${card.error}` : card?.name ? `; card ${card.name}` : "";
  return `wan-decode: ${node} (${chooseWanDecode(mode, card?.vramTotal).why}${detail})`;
}

// buildGraphForRun is the one path generate() takes to a graph. A run that builds the Wan graph in auto
// decode mode first reads the render card's VRAM from the ComfyUI it is about to submit to, so
// buildGraphFromArgs stays synchronous and the read happens once, only for the graph that uses it: an
// tiled (the default) or plain makes no request, and neither does LTX 2.5, Hunyuan 1.5, ace, h3 or a --graph
// file. A run buildGraphFromArgs would refuse for a missing still or prompt reads nothing either: it
// exits right after, and exiting with a socket still closing is the Windows crash main() warns about.
// Every Wan run logs its decode (log defaults to stderr).
export async function buildGraphForRun(pos, flags, api, { stage = stageInput, fetchImpl = fetch, log = (m) => console.error(m) } = {}) {
  const wan = runsWanGraph(flags);
  const mode = wan ? wanDecodeMode(flags) : undefined; // a bad --wan-decode throws before anything is staged
  const buildable = !!pos[1] && !!(pos[2] || flags.prompt);
  const card = wan && mode === "auto" && buildable ? await readRenderCard(api, { fetchImpl }) : undefined;
  const built = buildGraphFromArgs(pos, flags, { stage, vramTotalBytes: card?.vramTotal });
  if (wan) log(wanDecodeLine(mode, card, built.graph));
  return built;
}

// submitChecked is the ONE path a graph takes to ComfyUI: node-class preflight first,
// then the shared submission. A class the server positively lacks throws MISSING_NODE and
// nothing is POSTed. Deps are injectable for tests.
export async function submitChecked({ api, graph, clientId, cli, fetchImpl = fetch, submit = submitGraph, comfyDir = COMFY_DIR }) {
  await assertNodeClasses(api, graph, { fetchImpl, comfyDir });
  return submit({ api, graph, clientId, cli });
}

async function generate(out, API, pos, flags) {
  const { graph, seed, model } = await buildGraphForRun(pos, flags, API);
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
  writeFileAtomic(out, await fetchView({ api: API, file }));
  console.log("WROTE", out);
  await finalizeRun({ api: API, promptId, cli });
}

async function main() {
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const out = pos[0];
  const API = comfyApi(flags.api);
  if (!out) { console.error('usage: node comfy-video.mjs <out.mp4> <still> "<prompt>" [--model hunyuan|wan] [flags]   |   <out.mp4> --graph wf.json'); process.exit(2); }
  await withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, api: API, reserveVram: flags["reserve-vram"] },
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
