// multicard-image.mjs — the card-only placement of the image and edit runners (--placement multicard on comfy-generate.mjs,
// comfy-render.mjs and comfy-edit.mjs): the Wan placement's planner (wan-multicard.mjs) with the image runners' flag names and the
// recipes of render/multicard-recipes.mjs.
//
// WHAT IT DECIDES, all of it before ComfyUI starts, so a wrong card is a refusal and not a render:
//   - which recipe (route, family, DiT file, text encoder), or why there is none, by name (multicard-recipes.mjs findRecipe);
//   - which two cards: the lease's (GPU_LEASE_DEVICES, else the lease record; --cards names them only when no lease does and can
//     only confirm them under one), read from the driver's card table; neither may drive a display, and a card whose display
//     state the driver does not report is refused too; the roles are the Wan planner's (wan-multicard.mjs chooseRoles): the larger card computes (cuda:1), except that of two cards within
//     1 percent of one size the higher board power computes (at least 1.25 times the other's), equal cards: the lower UUID stores and the higher computes (the order a lease sorts its ids in);
//     WAN_COMPUTE_UUID, an environment variable, names the compute card of the pair instead;
//   - whether the recipe fits those two cards: its per-card peak against each card less --reserve-vram, and a resident share of
//     the DiT that the compute card can hold beside the sampler's activations;
//   - the ComfyUI launch the placement needs (flags, env, the instance key of the pair), as plain data for ensureComfy: the same
//     launch the Wan placement makes, so one pair has one instance whichever family asks, with the packs the recipe needs.
// Dependency-free; the card table and the pack scan are injectable for tests.
import { readdirSync } from "node:fs";
import { leaseDevices } from "./gpu-lock.mjs";
import { fetchNodeInfo, missingNodeMessage } from "./comfy-nodes.mjs";
import { comfyLogPath, comfyLogCursor } from "./comfy-lifecycle.mjs";
import {
  queryCardsRanked, computeOverride, computeChosen, wanCardTokens, resolveWanCards, confirmWithLease, refuseEqualsSpelling, multicardLaunch, multicardEnv, packDirs,
  checkComfyDevices, createOomWatch, PACK_MULTIGPU, PACK_GGUF,
} from "./wan-multicard.mjs";
import {
  findRecipe, recipeResident, recipePeaks, multicardLoaders, canvasRefusal, rowDevices, PLACEMENTS, PLACEMENT_DEFAULT,
} from "./multicard-recipes.mjs";

/** The image runners' flags: the Wan placement's --wan-placement, --wan-resident-gb and --wan-cards with the prefix dropped. */
export const IMAGE_FLAG_NAMES = Object.freeze(["placement", "resident-gb", "cards"]);

/** GiB the compute card keeps for the CUDA context and the allocator's slack, beyond a recipe's activations (the Wan placement's figure). */
const COMPUTE_CARD_CONTEXT_GIB = 0.5;
/** --reserve-vram when the call names none: the runners' own 1.0. */
const RESERVE_VRAM_DEFAULT_GIB = 1.0;

const placementError = (msg) => new Error("IMAGE-PLACEMENT: " + msg);

/** The planner's refusals are the Wan planner's own helpers' words; this puts them under this runner's name. */
function relabel(fn) {
  try { return fn(); } catch (e) {
    if (e && typeof e.message === "string" && e.message.startsWith("WAN-PLACEMENT: ")) e.message = "IMAGE-PLACEMENT: " + e.message.slice("WAN-PLACEMENT: ".length);
    throw e;
  }
}

/** imagePlacementMode parses --placement: absent is the single placement, today's, byte for byte; anything else is refused. */
export function imagePlacementMode(flags) {
  if (!("placement" in flags)) return PLACEMENT_DEFAULT;
  const raw = flags.placement;
  if (!PLACEMENTS.includes(raw)) throw new Error(`--placement must be ${PLACEMENTS.join("|")}, got ${JSON.stringify(raw)}`);
  return raw;
}

/** imageResidentGb parses --resident-gb: GiB of the DiT kept on the compute card; undefined when absent (the recipe's own). */
export function imageResidentGb(flags) {
  if (!("resident-gb" in flags)) return undefined;
  const raw = flags["resident-gb"];
  const n = Number(raw);
  if (raw === undefined || raw === "" || !Number.isFinite(n) || n < 0 || n > 64) {
    throw new Error(`--resident-gb must be a number of GiB from 0 to 64, got ${JSON.stringify(raw)}`);
  }
  return n;
}

/**
 * imagePackDirs: the custom-node folders a launch for this recipe must load, by name: the MultiGPU pack always, ComfyUI-GGUF for a
 * GGUF recipe. A recipe has no use for the video pack, so it is not loaded.
 */
export function imagePackDirs(comfyDir, recipe, list = (d) => readdirSync(d, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => e.name)) {
  const required = [PACK_MULTIGPU];
  if ((recipe.packs || []).includes("gguf")) required.push(PACK_GGUF);
  return packDirs(comfyDir, list, required);
}

function reserveGiB(flags) {
  const n = Number(flags && flags["reserve-vram"]);
  return Number.isFinite(n) && n > 0 ? n : RESERVE_VRAM_DEFAULT_GIB;
}

/** checkResident: a resident share larger than the compute card can hold beside the recipe's activations would run out of memory in the first sampler step. */
function checkResident(recipe, residentGb, pair) {
  if (residentGb === null) return;
  const cardGib = pair.compute.totalMiB / 1024;
  const most = cardGib - recipe.activationsGiB - COMPUTE_CARD_CONTEXT_GIB;
  if (residentGb > most) {
    throw placementError(`--resident-gb ${residentGb} leaves the compute card (idx ${pair.compute.index}, ${cardGib.toFixed(1)} GiB) about ${recipe.activationsGiB.toFixed(1)} GiB for the sampler's activations (the ${recipe.id} recipe's figure) and ${COMPUTE_CARD_CONTEXT_GIB} GiB for the CUDA context: at most ${most.toFixed(1)} GiB of the DiT can stay resident there (the recipe keeps ${recipe.unet.residentGb})`);
  }
}

/**
 * checkPeaks: the recipe's per-card peak (recipePeaks: a resident share moves bytes between the cards one for one) against the two cards it was
 * handed, each less --reserve-vram.
 */
function checkPeaks(recipe, residentGb, pair, flags) {
  const reserve = reserveGiB(flags);
  const peaks = recipePeaks(recipe, residentGb);
  const asks = [["compute", pair.compute, peaks.compute], ["storage", pair.storage, peaks.storage]];
  for (const [role, card, peak] of asks) {
    const total = card.totalMiB / 1024;
    if (peak > total - reserve) {
      throw placementError(`the ${recipe.id} recipe needs the ${role} card (idx ${card.index}, ${card.name}) to hold ${peak.toFixed(1)} GiB, and it has ${total.toFixed(1)} GiB (${(total - reserve).toFixed(1)} GiB usable after --reserve-vram ${reserve}): lease two cards that hold it`);
    }
  }
}

/**
 * planImageMulticard is the one call a runner makes before anything starts: null for the single placement (the default, which reads no card
 * table and touches nothing), otherwise the whole plan of the card-only placement. It refuses, with the reason, every request it cannot honour:
 * the placement's own flags spelled with '=', a --graph file, a (route, family, DiT, text encoder) with no recipe, no cards named, not two
 * cards, a card the lease does not hold, a display card or one whose display state is unknown, a recipe that does not fit the cards, a resident
 * share the compute card cannot hold, a missing pack, a canvas above the recipe's pixel cap. route is "image" or "edit"; family/unet/clip are the call's own words for
 * the binding; `canvas` ({ width, height }) is the size the call names, when it names one (a call that names none is judged where the runner knows its default,
 * comfy-render.mjs buildRenderGraph; the edit route's canvas is held to `maxMegapixels` by comfy-edit.mjs, so it passes none);
 * `cards` reads the driver's card table (injectable); `listFn` lists the custom_nodes folders; `leaseDevicesFn` names the cards the lease holds.
 */
export function planImageMulticard({
  route, family, unet, clip, flags, env = process.env, comfyDir, isGraph = false, canvas,
  spawnSync, cards = () => queryCardsRanked(spawnSync), launchFn, listFn, leaseDevicesFn = leaseDevices, say = () => {},
}) {
  refuseEqualsSpelling(flags, IMAGE_FLAG_NAMES);
  if (imagePlacementMode(flags) !== "multicard") return null;
  if (isGraph) {
    throw new Error("--placement multicard places the graphs this runner builds; this invocation posts a --graph file, which it does not touch");
  }
  const recipe = findRecipe({ route, family, unet, clip });
  const tooBig = canvas ? canvasRefusal(recipe, canvas.width, canvas.height) : "";
  if (tooBig) throw placementError(tooBig);
  const residentGb = recipeResident(recipe, imageResidentGb(flags));
  const pair = relabel(() => {
    const { tokens, source } = wanCardTokens({ flags, env, leaseDevicesFn, cardsFlag: "cards" });
    // no card named: refused before the driver is asked anything
    const table = tokens.length ? cards() : [];
    const found = resolveWanCards({ tokens, source, cards: table, cardsFlag: "cards", compute: computeOverride(env) });
    if (source === "--cards") confirmWithLease(found, leaseDevicesFn(env), "cards");
    return { ...found, source };
  });
  checkResident(recipe, residentGb, pair);
  checkPeaks(recipe, residentGb, pair, flags);
  // a compute-first row (rowDevices) is launched with the compute card first, under that order's own instance key
  const devices = rowDevices(recipe);
  const launch = launchFn ? launchFn(pair, { comfyDir, recipe }) : relabel(() => multicardLaunch(pair, { comfyDir, packDirsFn: (d) => imagePackDirs(d, recipe, listFn), order: devices.order }));
  const plan = { pair, launch, residentGb, recipe, comfyDir, env: multicardEnv(env, launch) };
  const line = (role, dev, c) => `${role} ${dev} = idx ${c.index} ${c.name} (${(c.totalMiB / 1024).toFixed(1)} GiB, ${c.uuid.slice(0, 12)})`;
  const share = residentGb === null ? "the whole DiT on the compute card" : `${residentGb} GiB of the DiT resident on the compute card`;
  const cardLines = devices.compute === "cuda:0"
    ? `${line("compute", devices.compute, pair.compute)}; ${line("storage", devices.storage, pair.storage)}`
    : `${line("storage", devices.storage, pair.storage)}; ${line("compute", devices.compute, pair.compute)}`;
  say(`placement: multicard on ${pair.source}: ${cardLines}; recipe ${recipe.id}, ${share}; basis ${recipe.basis}; ${computeChosen(pair)}`);
  return plan;
}

/** lifecycleOptions: what withGpuSlot is handed for a plan (the launch and the environment the instance is resolved from); nothing for none. */
export function lifecycleOptions(plan) {
  return plan ? { launch: plan.launch, instanceEnv: plan.env } : {};
}

/** recipeLoaderNodes: the three loader nodes ("1" DiT, "2" text encoder, "3" VAE) the plan's recipe puts in a graph, with placeholder file names. */
export function recipeLoaderNodes(plan) {
  return multicardLoaders(plan.recipe, { unet: "unet", clip: "clip", vae: "vae", residentGb: plan.residentGb ?? undefined });
}

/** The inputs a /object_info entry declares, whatever section (required, optional, hidden) names them; null when the entry says nothing about inputs. */
function declaredInputs(entry) {
  const input = entry && typeof entry === "object" ? entry.input : null;
  if (!input || typeof input !== "object") return null;
  return new Set(["required", "optional", "hidden"].flatMap((k) => (input[k] && typeof input[k] === "object" ? Object.keys(input[k]) : [])));
}

/**
 * verifyImagePlacement: ComfyUI numbers its cards in the order of the CUDA_VISIBLE_DEVICES it was launched with, and the graph names cuda:0
 * (storage) and cuda:1 (compute). Before anything is submitted, the running ComfyUI's own device list is compared with the two cards it was
 * pinned to, and the harness's node pack (render/comfyui-ext, which installs the read-only safetensors reader and raises on import when it
 * cannot) is asked for by one of its classes. The recipe's own loader classes are asked for too (a class the pack does not register is
 * MISSING_NODE, with its pack hint), and each must still declare every input the recipe sets: ComfyUI drops an input a class does not declare
 * without a word, so a renamed `expert_mode_allocations` would run with no split at all. A mismatch, a pack or class that did not load and an
 * answer that cannot be had are all refusals.
 */
export async function verifyImagePlacement(api, plan, { fetchImpl = fetch } = {}) {
  let body;
  try {
    const r = await fetchImpl(`${api}/system_stats`, { signal: AbortSignal.timeout(8000) });
    if (!r || !r.ok) throw new Error(`HTTP ${r?.status}`);
    body = await r.json();
  } catch (e) {
    throw new Error(`IMAGE-PLACEMENT-MISMATCH: GET /system_stats gave no device list (${e?.message || e}), so the cards ComfyUI numbered cannot be checked. Nothing was submitted.`);
  }
  const problems = checkComfyDevices(body?.devices, plan.pair, plan.launch?.order);
  if (problems.length) throw new Error(`IMAGE-PLACEMENT-MISMATCH: ${problems.join("; ")}. Nothing was submitted.`);
  const loaders = Object.values(recipeLoaderNodes(plan));
  const classes = ["OffloadFreeForDecode", ...new Set(loaders.map((n) => n.class_type))];
  const res = await fetchNodeInfo(api, classes, { fetchImpl });
  if (!res.checked) throw new Error(`IMAGE-PLACEMENT-PACK: cannot tell whether the harness's node pack (offload_multicard) and the ${plan.recipe.id} recipe's loaders are there: ${res.reason}. Nothing was submitted.`);
  if (res.info.OffloadFreeForDecode === undefined) {
    throw new Error(`IMAGE-PLACEMENT-PACK: ComfyUI at ${api} has no OffloadFreeForDecode, so the harness's node pack offload_multicard failed to import (it refuses to load when its read-only safetensors reader cannot be installed, and without that reader each weight file would charge its whole size to host commit): ComfyUI's console log says why (${comfyLogPath(plan.comfyDir || "", plan.launch.key)}). Nothing was submitted.`);
  }
  const missing = classes.filter((cls) => cls !== "OffloadFreeForDecode" && res.info[cls] === undefined);
  if (missing.length) throw new Error(missingNodeMessage(api, missing, plan.comfyDir || ""));
  for (const node of loaders) {
    const declared = declaredInputs(res.info[node.class_type]);
    const lost = declared ? Object.keys(node.inputs).filter((k) => !declared.has(k)) : [];
    if (lost.length) {
      throw new Error(`IMAGE-PLACEMENT-NODE-INPUT: ComfyUI at ${api} has ${node.class_type}, but it declares no input ${lost.join(", ")}, which the ${plan.recipe.id} recipe sets. ComfyUI drops an input a class does not declare without a word, so the placement would silently not happen (the pack on this ComfyUI is not the one the recipe was written against). Nothing was submitted.`);
    }
  }
}

// ---- the console watch and the decode verdict -----------------------------------------------------------------------------

const toImageWords = (m) => String(m).replace(/^WAN-OOM(-WATCH)?: /, (_, w) => `IMAGE-PLACEMENT-OOM${w || ""}: `);

/**
 * beginPlacedWatch: the `watch` the runner hands the poll while a card-only prompt runs; null for no plan. A ComfyUI whose prompt worker died after an
 * out-of-memory error inside DisTorch2 keeps answering HTTP (/history lists nothing), so the wait would run to the end of its budget with both cards
 * leased; the instance's own console says so, and the wait ends with a message that names the line, the recipe and the per-card peaks it was held to.
 * Made BEFORE the prompt is submitted, so it reads this run's lines and not an earlier run's. It also remembers whether ComfyUI retried the decode tiled
 * (`.drain()` then `.verdict`: "clean", "fallback" or "blind"; settlePlacedRun reads them). The detection is the Wan runner's (createOomWatch), in the
 * image runners' words.
 */
export function beginPlacedWatch(plan, { cursorFn = comfyLogCursor, comfyDir = plan && plan.comfyDir, say = (m) => console.error(m) } = {}) {
  if (!plan) return null;
  const inner = createOomWatch({ cursor: cursorFn(comfyDir, plan.launch.key), say: (m) => say(toImageWords(m)) });
  const peaks = recipePeaks(plan.recipe, plan.residentGb);
  const gib = (c) => (c.totalMiB / 1024).toFixed(1);
  const numbers = ` The ${plan.recipe.id} recipe holds ${peaks.compute.toFixed(1)} GiB on the compute card (idx ${plan.pair.compute.index}, ${gib(plan.pair.compute)} GiB) and ${peaks.storage.toFixed(1)} GiB on the storage card (idx ${plan.pair.storage.index}, ${gib(plan.pair.storage)} GiB).`;
  const watch = () => {
    const verdict = inner();
    return verdict ? toImageWords(verdict) + numbers : verdict;
  };
  watch.drain = () => inner.drain();
  Object.defineProperty(watch, "verdict", { get: () => inner.verdict });
  return watch;
}

/** tiledPath: where an image that is not the one that was asked for is kept, beside the requested path (out/o.png -> out/o.tiled.png). */
export function tiledPath(out) {
  const dot = out.lastIndexOf("."), slash = Math.max(out.lastIndexOf("/"), out.lastIndexOf("\\"));
  return dot > slash + 1 ? `${out.slice(0, dot)}.tiled${out.slice(dot)}` : `${out}.tiled`;
}

/**
 * settlePlacedRun: the bytes of a finished card-only render, written. ComfyUI retries a VAE decode that ran out of memory TILED, with tiles it sizes
 * itself, and says so only in its console: an image made that way can show seams where its tiles meet, and is not the single-pass decode the recipe was
 * measured on. Judged by what THIS run's console said (the watch's cursor began before the submit), never by the tail of a log that a kept instance
 * shares with earlier runs. The finished image is kept beside the requested path (the sampling cost the whole run), the run is finalized, and it fails: a
 * caller that keys on the exit code or on WROTE must not ship the seams. A console that cannot be read to the end is said, and the image is delivered:
 * unknown is not a fallback. With no watch (no plan) the bytes are simply written to `out`. Returns the verdict.
 */
export async function settlePlacedRun({ watch, plan, out, bytes, write, finalize, stderr = (m) => console.error(m) }) {
  if (!watch) {
    write(out, bytes);
    return "clean";
  }
  watch.drain();
  const verdict = watch.verdict;
  const log = comfyLogPath(plan.comfyDir || "", plan.launch.key);
  if (verdict === "fallback") {
    const kept = tiledPath(out);
    write(kept, bytes);
    await finalize();
    throw new Error(`IMAGE-DECODE-FALLBACK: ComfyUI ran out of memory in the VAE decode and retried it tiled with tiles of its own, so the image is not the single-pass decode the ${plan.recipe.id} recipe was measured on and can show seams where its tiles meet (console: ${log}). The finished image was kept as ${kept} instead of ${out}.`);
  }
  write(out, bytes);
  if (verdict === "blind") {
    stderr(`IMAGE-DECODE-UNVERIFIED: ComfyUI's console (${log}) could not be read to the end (it is missing, or its capture reached the 5 MB cap), so whether ComfyUI retried the VAE decode tiled is unknown; check the image for seams where tiles would meet`);
  }
  return verdict;
}

export { PLACEMENTS, PLACEMENT_DEFAULT };
