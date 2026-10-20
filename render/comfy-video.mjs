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
//   --wan-placement single|multicard (wan only; single, today's, is the default): multicard is the CARD-ONLY placement on the
//   two non-display cards of a lease, no host RAM (render/wan-multicard.mjs; docs/systems/media-generation.md, "Card-only Wan").
//   It reads the two cards from the lease (GPU_LEASE_DEVICES) or --wan-cards <card>,<card> (nvidia-smi indices or UUIDs), refuses
//   a display card, launches its own ComfyUI on them (--gpu-only, a read-only safetensors reader, the harness's node folder) and
//   builds the graph with the DisTorch2 loaders: --wan-resident-gb N (default 3) is the GiB of each expert kept on the compute card.
//   Which of the two computes: the larger card; of two cards within 1% of one size the higher board power (power.default_limit, 1.25x or more);
//   of two equal cards the higher UUID. The environment variable WAN_COMPUTE_UUID (an index, a UUID or a UUID prefix of 8+ characters) names it
//   instead; a value that names no card is refused, one outside the pair is ignored with a note.
//   Sage attention: with --use-sage-attention (or an argparse prefix of 8+ characters) in COMFY_EXTRA_ARGS the sampler's activations are sized at 15.2 GiB at the
//   film's size (12.2 without it), so the default --wan-resident-gb 3 is refused from about 58k tokens up on a 15.9 GiB card and a sage film call passes --wan-resident-gb 0 (0.2 at
//   most at 720x1280x81, 2.5 at 720x1280x65; 720x1280x61 still admits 3.1); the lease sizing holds 15.2 GiB for any clip, so --ram 0 states the real figure for a smaller one.
//   --wan-placement multicard --model ltx25 is the same placement for LTX-2.5 (a 20 GiB int8 DiT, a 14 GiB Gemma encoder): the lease's compute card is cuda:0 (the launch lists it
//   first: an int8 DiT that computes on another device faults, ComfyUI-MultiGPU #220) and the storage card cuda:1; --wan-resident-gb N (default 9) is the GiB of the DiT kept on
//   the compute card, the encoder is split over both cards and taken off them once the prompt is encoded (OffloadFreeAfterEncode), and both VAEs sit on the storage card. No
//   --wan-decode, --high-unet or --low-unet; a bf16 or GGUF --transformer is refused. A split model stays on its cards when ComfyUI loads the latent upscaler (the
//   pack's eviction guard), and OffloadFreeForDecode (check false) empties both cards before the tiled decodes. Measured on one run at 1280x704x121 with the default shares:
//   peaks 11.54 GiB compute and 12.71 GiB storage, 124.0 s of ComfyUI time; a larger share or clip is unmeasured.
//   --wan-decode plain under multicard frees both cards of the experts before the plain VAEDecode and saves the sampled latent
//   (a LATENT line names it); a plain decode that ComfyUI retried tiled fails the run (WAN-DECODE-FALLBACK) and keeps the clip beside <out> as
//   <name>.seamed<ext>; --wan-loader is not used. Needs --high-unet / --low-unet to name the experts: the fp8 safetensors files, or the Q8_0 GGUF files (14.349 GiB each, loaded through the
//   GGUF flavour of the DisTorch2 loader; the launch then also loads ComfyUI-GGUF and the donor is the storage card, never host RAM). The wait is 7200 s unless
//   --wait-sec / COMFY_WAIT_SEC say otherwise, and the run watches the instance's console for an out-of-memory wedge (WAN-OOM).
//
// Before anything is submitted, every node class the graph names is checked against the
// running ComfyUI's /object_info (render/comfy-nodes.mjs): a missing custom-node pack is a
// one-line MISSING_NODE defer naming the class and the pack, not a 400 at the POST.
import { readFileSync, existsSync } from "node:fs";
import { writeFileAtomic } from "./atomic-out.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { familySignature } from "./comfy-family.mjs";
import { COMFY_DIR, comfyApi, comfyLogPath, comfyLogCursor } from "./comfy-lifecycle.mjs";
import { stageInput as stageToInput } from "./comfy-input.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { assertNodeClasses, missingNodeClasses } from "./comfy-nodes.mjs";
import { buildHunyuan15I2V } from "./wf-hunyuan15-i2v.mjs";
import { buildWan22I2V, chooseWanDecode, WAN_DECODE_MODES, WAN_DECODE_DEFAULT } from "./wf-wan22-i2v.mjs";
import { buildLtx25I2V } from "./wf-ltx25-i2v.mjs";
import { buildH3AV } from "./wf-h3-av.mjs";
import { buildAceStep } from "./wf-acestep.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";
import { planMulticard, checkComfyDevices, portNote, createOomWatch, MULTICARD_WAIT_SEC, wanGeometry, tiledSeamFrames, describeFrames } from "./wan-multicard.mjs";

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

// runsLtxGraph: does this invocation build the LTX-2.5 graph (so --wan-placement multicard places it)?
export function runsLtxGraph(flags) {
  return !flags.graph && flags.model === "ltx25";
}

// ComfyUI's LoadImage reads from <COMFY_DIR>/input. Stage the still there.
function stageInput(stillPath) {
  return stageToInput("render_in", stillPath);
}

// buildGraphFromArgs builds the API-format graph the flags describe. stage copies a still
// into ComfyUI's input dir and returns the name LoadImage reads (injectable for tests).
// vramTotalBytes is the render card's total VRAM when the caller has read it (buildGraphForRun
// does); only the Wan graph's --wan-decode auto reads it, and without it auto builds the tiled decode.
export function buildGraphFromArgs(pos, flags, { stage = stageInput, vramTotalBytes, multicard } = {}) {
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
      // multicard (planMulticard) is the card-only placement: the DiT's resident share on the compute card (the builder holds the encoder's split)
      if (multicard) common.multicard = { residentGb: multicard.residentGb };
      graph = buildLtx25I2V(common);
    } else {
      // The decode mode goes to the Wan builder alone: Hunyuan 1.5 and LTX 2.5 keep their own
      // VAEDecodeTiled (other VAEs, never measured), so their calls above are untouched by it.
      // multicard (planMulticard) is the card-only placement; absent, the call is the one that always was.
      graph = model === "hunyuan" ? buildHunyuan15I2V(common) : buildWan22I2V({ ...common, decode: wanDecodeMode(flags), vramTotalBytes, ...(multicard ? { placement: "multicard", residentGb: multicard.residentGb } : {}) });
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
export async function buildGraphForRun(pos, flags, api, { stage = stageInput, fetchImpl = fetch, log = (m) => console.error(m), multicard } = {}) {
  const wan = runsWanGraph(flags);
  const mode = wan ? wanDecodeMode(flags) : undefined; // a bad --wan-decode throws before anything is staged
  const buildable = !!pos[1] && !!(pos[2] || flags.prompt);
  const card = wan && mode === "auto" && buildable ? await readRenderCard(api, { fetchImpl }) : undefined;
  const built = buildGraphFromArgs(pos, flags, { stage, vramTotalBytes: card?.vramTotal, multicard });
  if (wan) log(wanDecodeLine(mode, card, built.graph));
  // Under multicard the decode that was not asked for is the tiled one (the default), which fits beside the expert and seams the clip: said once.
  if (wan && multicard && Object.values(built.graph).some((n) => n.class_type === "VAEDecodeTiled")) {
    const seams = describeFrames(tiledSeamFrames(wanGeometry(flags).f));
    log(`WAN-PLACEMENT-NOTE: the tiled decode under multicard fits beside the expert (+2.3 GiB measured) but tiles the decode, so the clip can seam where its tiles meet${seams ? `, in time at ${seams}` : ""}; --wan-decode plain frees both cards of the experts first and decodes in one pass`);
  }
  return built;
}

// submitChecked is the ONE path a graph takes to ComfyUI: node-class preflight first,
// then the shared submission. A class the server positively lacks throws MISSING_NODE and
// nothing is POSTed. Deps are injectable for tests.
export async function submitChecked({ api, graph, clientId, cli, fetchImpl = fetch, submit = submitGraph, comfyDir = COMFY_DIR }) {
  await assertNodeClasses(api, graph, { fetchImpl, comfyDir });
  return submit({ api, graph, clientId, cli });
}

// verifyMulticardDevices: ComfyUI numbers its cards in the order of the CUDA_VISIBLE_DEVICES it was launched with, and the graph
// names cuda:0 (storage) and cuda:1 (compute). Before anything is submitted, the running ComfyUI's own device list is compared with the
// two cards it was pinned to; a mismatch stops the run with nothing on the cards. An unreadable /system_stats is a refusal too.
export async function verifyMulticardDevices(api, plan, { fetchImpl = fetch } = {}) {
  let body;
  try {
    const r = await fetchImpl(`${api}/system_stats`, { signal: AbortSignal.timeout(8000) });
    if (!r || !r.ok) throw new Error(`HTTP ${r?.status}`);
    body = await r.json();
  } catch (e) {
    throw new Error(`WAN-PLACEMENT-MISMATCH: GET /system_stats gave no device list (${e?.message || e}), so the cards ComfyUI numbered cannot be checked`);
  }
  const problems = checkComfyDevices(body?.devices, plan.pair, plan.launch?.order);
  if (problems.length) throw new Error(`WAN-PLACEMENT-MISMATCH: ${problems.join("; ")}. Nothing was submitted.`);
}

// verifyMulticardPack: the harness's node pack (render/comfyui-ext) is what installs the read-only safetensors reader, and it raises on import
// when it cannot, so a pack that is not registered means each expert would charge its whole file to host commit. The graph names one of its classes
// only under the plain decode, so the node-class preflight alone would let any other decode run without the reader: this asks for it every time,
// and, unlike that preflight, an answer that cannot be had is a refusal too (the server just answered /system_stats).
export async function verifyMulticardPack(api, plan, { fetchImpl = fetch } = {}) {
  // the LTX-2.5 graph names the pack's encoder loader and release beside the decode's free node
  const asked = plan?.family === "ltx25" ? ["OffloadCLIPLoaderDisTorch2", "OffloadFreeAfterEncode", "OffloadFreeForDecode"] : ["OffloadFreeForDecode"];
  const res = await missingNodeClasses(api, Object.fromEntries(asked.map((cls, i) => [i + 1, { class_type: cls }])), { fetchImpl });
  if (!res.checked) throw new Error(`WAN-PLACEMENT-PACK: cannot tell whether the harness's node pack (offload_multicard) loaded: ${res.reason}. Nothing was submitted.`);
  if (res.missing.length) {
    throw new Error(`WAN-PLACEMENT-PACK: ComfyUI at ${api} has no ${res.missing.join(" or ")}, so the harness's node pack offload_multicard failed to import (it refuses to load when its read-only safetensors reader cannot be installed, and without that reader each expert would charge its whole file to host commit): ComfyUI's console log says why (${comfyLogPath(COMFY_DIR, plan.launch.key)}). Nothing was submitted.`);
  }
}

// The calls generate() makes to the outside world, as one table a test can replace entry by entry (generate's last argument). The
// defaults are the real ones; nothing else about the function changes.
export const GENERATE_IO = Object.freeze({
  verifyDevices: verifyMulticardDevices, verifyPack: verifyMulticardPack, buildGraph: buildGraphForRun, submit: submitChecked,
  // a literal call, not the bare function: gpu_progress_test.go finds the render helpers that heartbeat (gpulease.HeartbeatHelperNames) by their `pollOutputs(` call
  poll: (opts) => pollOutputs(opts), view: fetchView, write: writeFileAtomic, logCursor: comfyLogCursor, finalize: finalizeRun, resolveCli,
  stdout: (...a) => console.log(...a), stderr: (...a) => console.error(...a), env: process.env, comfyDir: COMFY_DIR,
});

export async function generate(out, API, pos, flags, multicard, io = {}) {
  const x = { ...GENERATE_IO, ...io };
  if (multicard) {
    await x.verifyDevices(API, multicard);
    await x.verifyPack(API, multicard);
  }
  const { graph, seed, model } = await x.buildGraph(pos, flags, API, { multicard });
  // Shared submission/polling/retrieval (comfy-submit.mjs): CLI-preferred submit with
  // byte-identical raw fallback; hardened poll loop (dead-server watchdog).
  const cli = x.resolveCli();
  // Under multicard the instance's console is watched while the prompt runs (an out-of-memory wedge ComfyUI does not report). The cursor is
  // made BEFORE the submit, so it reads this run's lines and not an earlier run's.
  const watch = multicard ? createOomWatch({ cursor: x.logCursor(x.comfyDir, multicard.launch.key), say: x.stderr }) : null;
  const { promptId } = await x.submit({ api: API, graph, clientId: "video-" + seed, cli });
  x.stdout("queued", promptId, flags.graph ? `(graph ${flags.graph})` : `${model} seed ${seed}`);
  // Poll budget: the native quality recipe at 720p legitimately exceeds the old ~20-min
  // ceiling. Default 90 min; the Go harness passes COMFY_WAIT_SEC aligned to its own
  // videogen timeout and its process-tree kill remains the hard stop.
  // The card-only placement samples for about 86 minutes at the film's size, so its default is longer (MULTICARD_WAIT_SEC); a budget the caller
  // chose, by flag or environment, is theirs.
  const chosen = flags["wait-sec"] || x.env.COMFY_WAIT_SEC;
  const waitSec = Number(chosen || (multicard ? MULTICARD_WAIT_SEC : 5400));
  if (multicard && !chosen) x.stderr(`WAN-WAIT-NOTE: no --wait-sec or COMFY_WAIT_SEC, so the wait for the video is ${MULTICARD_WAIT_SEC} s (${multicard.family === "ltx25" ? "the one LTX-2.5 run on the two cards, 1280x704x121, took 124 s of ComfyUI time and 146 s of runner wall" : "the 20-step film recipe was projected at about 5170 s and measured 5136 s of runner wall"}); pass --wait-sec <seconds> to change it`);
  const h = await x.poll({
    api: API, promptId, waitSec,
    isDone: (entry) => !!firstOutputFile(entry.outputs, graph),
    noOutputMsg: "no video produced in time",
    onExecError: () => x.finalize({ api: API, promptId, cli }),
    ...(watch ? { watch } : {}),
  });
  const file = firstOutputFile(h.outputs, graph);
  const bytes = await x.view({ api: API, file });
  // multicard + plain decode: the sampled latent the free node saved (the line a failed decode's message carries too).
  const latents = (h.outputs?.["21"]?.latents || []).map((f) => (f.subfolder ? `${f.subfolder}/${f.filename}` : f.filename));
  const sayLatents = () => { for (const l of latents) x.stdout(`LATENT ${l} (in ComfyUI's output directory)`); };
  // --wan-decode plain asks for ONE decode pass, a clip without seams. ComfyUI retries a plain decode that ran out of memory TILED, with temporal tiles it
  // sizes itself, and that clip can seam where its tiles meet: judged by what THIS run's console said (the watch's cursor began before the submit), never
  // by the tail of a log that a kept instance shares with earlier runs. The tiled decode (the default) is the decode that was asked for.
  const plain = !!watch && Object.values(graph).some((n) => n.class_type === "VAEDecode");
  if (watch) watch.drain();
  const verdict = watch ? watch.verdict : "clean";
  const log = comfyLogPath(x.comfyDir, multicard ? multicard.launch.key : "");
  if (plain && verdict === "fallback") {
    // The finished clip is kept beside the requested path, not thrown away (the sample cost the whole run), and the run fails: a caller that keys on
    // the exit code or on WROTE must not ship the seams.
    const seamed = seamedPath(out);
    x.write(seamed, bytes);
    sayLatents();
    await x.finalize({ api: API, promptId, cli });
    throw new Error(`WAN-DECODE-FALLBACK: ComfyUI ran out of memory in the plain decode and retried it tiled with temporal tiles of its own, so the clip is not the single-pass decode that was asked for and can seam where its tiles meet (console: ${log}). The finished clip was kept as ${seamed} instead of ${out}${latents.length ? `; the sampled latent is ${latents.join(", ")} in ComfyUI's output directory, to decode again with more room` : ""}.`);
  }
  x.write(out, bytes);
  x.stdout("WROTE", out);
  sayLatents();
  if (plain && verdict === "blind") {
    x.stderr(`WAN-DECODE-UNVERIFIED: ComfyUI's console (${log}) could not be read to the end (it is missing, or its capture reached the 5 MB cap), so whether ComfyUI retried the plain decode tiled is unknown; check the clip for seams where temporal tiles would meet`);
  }
  await x.finalize({ api: API, promptId, cli });
}

/** seamedPath: where a clip that is not the one that was asked for is kept, beside the requested path (out/o.mp4 -> out/o.seamed.mp4). */
export function seamedPath(out) {
  const dot = out.lastIndexOf("."), slash = Math.max(out.lastIndexOf("/"), out.lastIndexOf("\\"));
  return dot > slash + 1 ? `${out.slice(0, dot)}.seamed${out.slice(dot)}` : `${out}.seamed`;
}

const USAGE = 'usage: node comfy-video.mjs <out.mp4> <still> "<prompt>" [--model hunyuan|wan] [flags]   |   <out.mp4> --graph wf.json';

/**
 * planRun: everything main() decides before it takes the GPU. The card-only placement is planned first: it refuses, before anything is
 * launched, every request it cannot honour (no cards named, not two, a display card). Its own environment names the instance, so the api and
 * the lease slot read it too. `slot` is withGpuSlot's options: the card-only launch and its environment ride in it only for that placement.
 */
export function planRun(argv, { env = process.env, comfyDir = COMFY_DIR, planFn = planMulticard, say = (m) => console.error(m) } = {}) {
  const { pos, flags } = parseArgs(argv);
  const multicard = planFn({ flags, env, comfyDir, isWanGraph: runsWanGraph(flags), isLtxGraph: runsLtxGraph(flags), say });
  const api = comfyApi(flags.api, multicard ? multicard.env : env);
  const note = multicard ? portNote(flags, multicard.env, api) : "";
  const slot = {
    noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, api, reserveVram: flags["reserve-vram"],
    family: familySignature("video-" + (flags.model || "wan"), flags.transformer || flags["high-unet"]),
    ...(multicard ? { launch: multicard.launch, instanceEnv: multicard.env } : {}),
  };
  return { pos, flags, out: pos[0], multicard, api, note, slot };
}

async function main() {
  const run = planRun(process.argv.slice(2));
  const API = run.api;
  if (run.note) console.error(run.note);
  if (!run.out) { console.error(USAGE); process.exit(2); }
  // run.slot already says it (planRun builds it); the literal below is what comfy-instance.test.mjs lints every runner for: a managed slot that gets the api.
  await withGpuSlot({ ...run.slot, comfyManaged: true, api: API }, () => generate(run.out, API, run.pos, run.flags, run.multicard));
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
