// comfy-animate.mjs — local CHARACTER-ANIMATION runner (WAN-Animate-2 distilled).
// Retargets the motion of a driver video onto a reference character image via
// ComfyUI. Single-slot GPU-locked + zero-always-warm, mirroring comfy-video.mjs.
// Dependency-free (Node 18+).
//
// Usage:
//   node render/comfy-animate.mjs <out.mp4> <ref.(png|jpg)> <driver.mp4> "<prompt>" \
//        [--motion-prompt "..."] [--negative "..."] [--width 482] [--height 854] \
//        [--frames 81] [--steps 10] [--seed N] [--pose-strength 1.0] [--ref-strength 1.0] \
//        [--unet f.safetensors] [--text-encoder f] [--clip-vision f] [--vae f] \
//        [--cache-device cpu|gpu] [--cache-dtype default|int8] \
//        [--placement single|multicard] [--resident-gb N] [--cards <card>,<card>] [--print-graph] \
//        [--api http://127.0.0.1:8188] [--no-lock] [--keep-comfy]
//   <prompt> describes the CHARACTER + BACKGROUND (what the output should show);
//   --motion-prompt describes the driver video (what motion is being transferred).
//   --placement single|multicard (single, today's, is the default): multicard is the CARD-ONLY placement on the two non-display cards of a lease, no host
//   RAM (render/wan-multicard.mjs, docs/systems/media-generation.md, "Card-only animate"). The distilled int8 DiT is 15.5 GiB, which one 16 GB card cannot
//   hold beside the sampler's activations and a --gpu-only node cannot stream, so it is split over both: --resident-gb N (default 5) is the GiB of it kept
//   on the compute card (cuda:0; the launch lists it first, as for LTX-2.5), the rest sits on the storage card (cuda:1) with the umt5 encoder, clip_vision_h
//   and the VAE; the encoder is taken off the cards once the prompt is encoded, before the DiT is placed. The two cards come from the lease
//   (GPU_LEASE_DEVICES) or --cards <card>,<card> (nvidia-smi indices or UUIDs; under a lease it can only confirm them); a display card is refused, and so is
//   a share one of the cards cannot hold (the arithmetic is in wan-multicard.mjs checkAnimateResident), all before ComfyUI starts. WAN_COMPUTE_UUID names the
//   compute card of the pair, as for the video placement. A .gguf, bf16 or fp16 --unet is refused. No run was ever made on two cards: the sizes are chosen.
//   --print-graph: print the resolved API graph as JSON on stdout and exit. It stages no input, takes no lease and starts or contacts no ComfyUI; under
//   --placement multicard it still plans the pair, which reads the driver's card table (nvidia-smi) and refuses what the real run would refuse.
import { writeFileAtomic } from "./atomic-out.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { familySignature } from "./comfy-family.mjs";
import { COMFY_DIR, comfyApi, comfyLogCursor } from "./comfy-lifecycle.mjs";
import { stageInput as stageToInput } from "./comfy-input.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { buildWanAnimate2 } from "./wf-wan-animate2.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";
import { planMulticard, portNote, createOomWatch, multicardInstanceKey, CARD_ORDER_COMPUTE_FIRST } from "./wan-multicard.mjs";
import { verifyMulticardDevices, verifyMulticardPack, submitChecked } from "./comfy-video.mjs";

const argv = process.argv.slice(2);
const pos = []; const flags = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) {
    const k = argv[i].slice(2);
    if (["no-lock", "keep-comfy", "print-graph"].includes(k)) flags[k] = true;
    else { flags[k] = argv[i + 1]; i++; }
  } else pos.push(argv[i]);
}
const [out, ref, driver, promptPos] = pos;
const prompt = promptPos || flags.prompt;
if (!out || !ref || !driver || !prompt) {
  console.error('usage: node comfy-animate.mjs <out.mp4> <ref.png> <driver.mp4> "<prompt>" [flags]');
  process.exit(2);
}

// The card-only placement is planned first, before anything is staged, leased or launched: it refuses every request it cannot honour (no cards named, not two, a display
// card, a share a card cannot hold). Its own environment names the instance, so the api and the lease slot read it too. Null for the single-card path (the default).
// --print-graph launches nothing, so its plan carries a launch of the same shape that needs no ComfyUI install (the instance key is the real one).
const printOnly = !!flags["print-graph"];
const printLaunch = (pair) => ({ key: multicardInstanceKey(pair, CARD_ORDER_COMPUTE_FIRST), cards: [pair.compute.uuid, pair.storage.uuid], order: CARD_ORDER_COMPUTE_FIRST, flags: [], env: {} });
let multicard = null;
try {
  multicard = planMulticard({ flags, env: process.env, comfyDir: COMFY_DIR, isAnimateGraph: true, say: (m) => console.error(m), ...(printOnly ? { launchFn: printLaunch } : {}) });
} catch (e) {
  console.error("ANIMATE FAILED:", e.message);
  process.exit(1);
}
const API = comfyApi(flags.api, multicard ? multicard.env : process.env);

// ComfyUI's LoadImage/LoadVideo read from C:\ComfyUI\input. Stage both there.
function stageInput(srcPath) {
  return stageToInput("render_in", srcPath);
}

// buildGraph builds the API-format graph the flags describe; refName / driverName are the names LoadImage / LoadVideo read.
function buildGraph(seed, refName, driverName) {
  const common = {
    refImagePath: refName,
    driverVideoPath: driverName,
    prompt, seed,
  };
  if (flags["motion-prompt"]) common.motionPrompt = flags["motion-prompt"];
  if (flags.negative) common.negative = flags.negative;
  if (flags.width) common.width = Number(flags.width);
  if (flags.height) common.height = Number(flags.height);
  if (flags.frames) common.length = Number(flags.frames);
  if (flags.steps) common.steps = Number(flags.steps);
  if (flags["pose-strength"]) common.poseStrength = Number(flags["pose-strength"]);
  if (flags["ref-strength"]) common.refStrength = Number(flags["ref-strength"]);
  // Per-machine weight binding (quality-first), same pattern as comfy-video.mjs:
  // unset = the builder's defaults (the reference box's on-disk filenames).
  if (flags.unet) common.unet = flags.unet;
  if (flags["text-encoder"]) common.textEncoder = flags["text-encoder"];
  if (flags["clip-vision"]) common.clipVision = flags["clip-vision"];
  if (flags.vae) common.vae = flags.vae;
  // Cache override is explicit-only; the builder pins cpu/default (gpu/int8
  // hard-kills ComfyUI on the reference box — see wf-wan-animate2.mjs header).
  if (flags["cache-device"]) common.cacheDevice = flags["cache-device"];
  if (flags["cache-dtype"]) common.cacheDtype = flags["cache-dtype"];
  // multicard (planMulticard) is the card-only placement: the DiT's resident share on the compute card (the builder holds the devices of the rest)
  if (multicard) common.multicard = { residentGb: multicard.residentGb };
  return buildWanAnimate2(common);
}

async function animate() {
  // Under multicard the running ComfyUI's device list is compared with the two cards it was pinned to, and the harness's node pack must have loaded, before anything is
  // staged or submitted (comfy-video.mjs, the same checks the video placement makes).
  if (multicard) {
    await verifyMulticardDevices(API, multicard);
    await verifyMulticardPack(API, multicard);
  }
  const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));
  const graph = buildGraph(seed, stageInput(ref), stageInput(driver));

  const cli = resolveCli();
  // Under multicard the instance's console is watched while the prompt runs (an out-of-memory wedge ComfyUI does not report); the cursor is made BEFORE the submit.
  const watch = multicard ? createOomWatch({ cursor: comfyLogCursor(COMFY_DIR, multicard.launch.key), say: (m) => console.error(m) }) : null;
  const clientId = "animate-" + seed;
  // multicard: every node class the graph names is checked against the running ComfyUI first (a missing MultiGPU or harness class is one line, not a 400 at the POST)
  const { promptId } = multicard ? await submitChecked({ api: API, graph, clientId, cli }) : await submitGraph({ api: API, graph, clientId, cli });
  console.log("queued", promptId, `wan-animate2 seed ${seed}`);
  const waitSec = Number(flags["wait-sec"] || process.env.COMFY_WAIT_SEC || 3600);
  const h = await pollOutputs({
    api: API, promptId, waitSec,
    isDone: (entry) => !!firstOutputFile(entry.outputs, graph),
    noOutputMsg: "no video produced in time",
    onExecError: () => finalizeRun({ api: API, promptId, cli }),
    ...(watch ? { watch } : {}),
  });
  const file = firstOutputFile(h.outputs, graph);
  writeFileAtomic(out, await fetchView({ api: API, file }));
  console.log("WROTE", out);
  await finalizeRun({ api: API, promptId, cli });
}

if (printOnly) {
  // the names the staged inputs would have are not needed: the graph is shown with the files as given
  const base = (p) => String(p).split(/[\\/]/).pop();
  const seed = Number(flags.seed || 0);
  console.log(JSON.stringify(buildGraph(seed, "render_in/" + base(ref), "render_in/" + base(driver)), null, 2));
} else {
  if (multicard) {
    const note = portNote(flags, process.env, API, "animate-placement");
    if (note) console.error(note);
  }
  withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, api: API, reserveVram: flags["reserve-vram"],
      family: familySignature("wan-animate2", flags.unet),
      ...(multicard ? { launch: multicard.launch, instanceEnv: multicard.env } : {}) },
    animate,
  ).catch((e) => { console.error("ANIMATE FAILED:", e.message); process.exit(1); });
}
