// multicard-image.mjs — the card-only placement of the image and edit runners (--placement multicard on comfy-generate.mjs,
// comfy-render.mjs and comfy-edit.mjs): the Wan placement's planner (wan-multicard.mjs) with the image runners' flag names and the
// recipes of render/multicard-recipes.mjs.
//
// WHAT IT DECIDES, all of it before ComfyUI starts, so a wrong card is a refusal and not a render:
//   - which recipe (route, family, DiT file, text encoder), or why there is none, by name (multicard-recipes.mjs findRecipe);
//   - which two cards: the lease's (GPU_LEASE_DEVICES, else the lease record; --cards names them only when no lease does and can
//     only confirm them under one), read from the driver's card table; neither may drive a display, and a card whose display
//     state the driver does not report is refused too; the larger card computes (cuda:1), equal cards keep the lease's order;
//   - whether the recipe fits those two cards: its per-card peak against each card less --reserve-vram, and a resident share of
//     the DiT that the compute card can hold beside the sampler's activations;
//   - the ComfyUI launch the placement needs (flags, env, the instance key of the pair), as plain data for ensureComfy: the same
//     launch the Wan placement makes, so one pair has one instance whichever family asks, with the packs the recipe needs.
// Dependency-free; the card table and the pack scan are injectable for tests.
import { readdirSync } from "node:fs";
import { leaseDevices } from "./gpu-lock.mjs";
import { missingNodeClasses } from "./comfy-nodes.mjs";
import { comfyLogPath } from "./comfy-lifecycle.mjs";
import {
  queryCards, wanCardTokens, resolveWanCards, confirmWithLease, refuseEqualsSpelling, multicardLaunch, multicardEnv, packDirs,
  checkComfyDevices, PACK_MULTIGPU, PACK_GGUF,
} from "./wan-multicard.mjs";
import { findRecipe, recipeResident, PLACEMENTS, PLACEMENT_DEFAULT, MULTICARD_STORAGE, MULTICARD_COMPUTE } from "./multicard-recipes.mjs";

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
 * checkPeaks: the recipe's per-card peak against the two cards it was handed, each less --reserve-vram. The DiT's total is the invariant, so
 * a resident share moves bytes between the cards one for one: a larger share raises the compute card's peak, and a smaller one puts the
 * same bytes on the storage card (the allocation string keeps `residentGb` on the compute card and sends the REMAINDER to the storage card).
 * The storage figure is never lowered by a larger share, because no run measured that: the recipe's own stands. For a recipe whose storage
 * peak is its text-encoder stage (the 2.1 bf16 one) a smaller share is therefore refused somewhat early, on the conservative side.
 */
function checkPeaks(recipe, residentGb, pair, flags) {
  const reserve = reserveGiB(flags);
  const shift = residentGb === null ? 0 : residentGb - recipe.unet.residentGb;
  const asks = [["compute", pair.compute, recipe.peakGiB.compute + shift], ["storage", pair.storage, recipe.peakGiB.storage + Math.max(0, -shift)]];
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
 * share the compute card cannot hold, a missing pack. route is "image" or "edit"; family/unet/clip are the call's own words for the binding;
 * `cards` reads the driver's card table (injectable); `listFn` lists the custom_nodes folders; `leaseDevicesFn` names the cards the lease holds.
 */
export function planImageMulticard({
  route, family, unet, clip, flags, env = process.env, comfyDir, isGraph = false,
  cards = queryCards, launchFn, listFn, leaseDevicesFn = leaseDevices, say = () => {},
}) {
  refuseEqualsSpelling(flags, IMAGE_FLAG_NAMES);
  if (imagePlacementMode(flags) !== "multicard") return null;
  if (isGraph) {
    throw new Error("--placement multicard places the graphs this runner builds; this invocation posts a --graph file, which it does not touch");
  }
  const recipe = findRecipe({ route, family, unet, clip });
  const residentGb = recipeResident(recipe, imageResidentGb(flags));
  const pair = relabel(() => {
    const { tokens, source } = wanCardTokens({ flags, env, leaseDevicesFn, cardsFlag: "cards" });
    // no card named: refused before the driver is asked anything
    const table = tokens.length ? cards() : [];
    const found = resolveWanCards({ tokens, source, cards: table, cardsFlag: "cards" });
    if (source === "--cards") confirmWithLease(found, leaseDevicesFn(env), "cards");
    return { ...found, source };
  });
  checkResident(recipe, residentGb, pair);
  checkPeaks(recipe, residentGb, pair, flags);
  const launch = launchFn ? launchFn(pair, { comfyDir, recipe }) : relabel(() => multicardLaunch(pair, { comfyDir, packDirsFn: (d) => imagePackDirs(d, recipe, listFn) }));
  const plan = { pair, launch, residentGb, recipe, comfyDir, env: multicardEnv(env, launch) };
  const line = (role, dev, c) => `${role} ${dev} = idx ${c.index} ${c.name} (${(c.totalMiB / 1024).toFixed(1)} GiB, ${c.uuid.slice(0, 12)})`;
  const share = residentGb === null ? "the whole DiT on the compute card" : `${residentGb} GiB of the DiT resident on the compute card`;
  say(`placement: multicard on ${pair.source}: ${line("storage", MULTICARD_STORAGE, pair.storage)}; ${line("compute", MULTICARD_COMPUTE, pair.compute)}; recipe ${recipe.id}, ${share}; basis ${recipe.basis}`);
  return plan;
}

/** lifecycleOptions: what withGpuSlot is handed for a plan (the launch and the environment the instance is resolved from); nothing for none. */
export function lifecycleOptions(plan) {
  return plan ? { launch: plan.launch, instanceEnv: plan.env } : {};
}

/**
 * verifyImagePlacement: ComfyUI numbers its cards in the order of the CUDA_VISIBLE_DEVICES it was launched with, and the graph names cuda:0
 * (storage) and cuda:1 (compute). Before anything is submitted, the running ComfyUI's own device list is compared with the two cards it was
 * pinned to, and the harness's node pack (render/comfyui-ext, which installs the read-only safetensors reader and raises on import when it
 * cannot) is asked for by one of its classes. A mismatch, a pack that did not load and an answer that cannot be had are all refusals.
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
  const problems = checkComfyDevices(body?.devices, plan.pair);
  if (problems.length) throw new Error(`IMAGE-PLACEMENT-MISMATCH: ${problems.join("; ")}. Nothing was submitted.`);
  const res = await missingNodeClasses(api, { 1: { class_type: "OffloadFreeForDecode" } }, { fetchImpl });
  if (!res.checked) throw new Error(`IMAGE-PLACEMENT-PACK: cannot tell whether the harness's node pack (offload_multicard) loaded: ${res.reason}. Nothing was submitted.`);
  if (res.missing.length) {
    throw new Error(`IMAGE-PLACEMENT-PACK: ComfyUI at ${api} has no OffloadFreeForDecode, so the harness's node pack offload_multicard failed to import (it refuses to load when its read-only safetensors reader cannot be installed, and without that reader each weight file would charge its whole size to host commit): ComfyUI's console log says why (${comfyLogPath(plan.comfyDir || "", plan.launch.key)}). Nothing was submitted.`);
  }
}

export { PLACEMENTS, PLACEMENT_DEFAULT };
