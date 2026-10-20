// multicard-recipes.mjs — the card-only recipes of the image and edit routes, and the loader nodes they put in a graph.
//
// WHAT A RECIPE IS. A (route, family, quant) that was run, or is reasoned from components that were run, on TWO cards with no
// host RAM: which loader holds which weight on which card, the DiT's resident share on the compute card, the loader of the
// LoRA, and the per-card VRAM peak a call is held to (render/multicard-recipes.json, which holds the figures and the evidence
// for each; internal/hostneed keeps a copy of the figures that decide a host-RAM declaration, and a Go test pins the copy).
//   cuda:0 = the STORAGE card: the text encoder, the VAE and the donor blocks of the DiT;
//   cuda:1 = the COMPUTE card: the DiT's resident share and the sampler's activations.
// The same two names the Wan placement uses (wf-wan22-i2v.mjs), so one launch (wan-multicard.mjs multicardLaunch) serves both. A row with
// `cardOrder: "compute-first"` (the qwen-image-2.1 rows, whose DiT runs comfy_kitchen kernels that work only on cuda:0) is launched the other way round,
// the LTX-2.5 placement's launch: cuda:0 computes and cuda:1 stores (rowDevices says where each loader goes).
//
// WHAT IT REFUSES, by name and before a lease is taken: a family or quant with no recipe (2512 bf16 fits no card, SDXL and
// HiDream fit one card); an int8 TEXT ENCODER (PSNR 28.65 dB against the 30 dB gate in the benchmark;
// the int8_convrot DiT is a different file and a recipe of its own, held back for now: its `blocked` sentence says why); a text
// encoder other than the one the recipe was built for.
// A row can carry a CANVAS CAP (`maxPixels`, refused by canvasRefusal; its `maxPixelsNote` says whether it was measured): the 2512 Q5_1 GGUF row is held to
// 1536x1152 (measured at 1328x1328 and 928x1664), the krea2 bf16 row to 1536x1024 (measured), the 2.1 bf16 row to the family's 2048x2048 default. The edit rows have `maxMegapixels` instead, the edit canvas their peaks were scaled to (comfy-edit.mjs refuses above it).
// A row can be HELD BACK: it stays in the table (its data is the proof arm's), carries a `blocked` sentence that says why, and
// findRecipe refuses it by name, so no planner, builder or door treats it as a recipe. The qwen-image-2.1 int8_convrot row is held back today
// (its int8 DiT on cuda:1 never ran). The launchable image rows are the 2512 Q5_1 GGUF, the krea2 bf16 DiT (its text encoder loads with the CLIP type
// the row names, `clipType`, "krea2"; a row without one is "qwen_image") and the qwen-image-2.1 bf16 DiT.
// An fp8 text encoder is never placed on cuda:1 (two measured runs aborted the process there), so a recipe pins it whole to
// cuda:0 (in both card orders an encoder computes on cuda:0); only a bf16 encoder too big for one card is split (the two 2.1 rows are the ones that do), and a test walks every
// recipe to keep it that way. That encoder is the `distorch2-host` clip loader: under --gpu-only the stock DisTorch2 loader builds the whole
// 16.33 GiB encoder on one 15.9 GiB card before its allocation applies, so the harness's own OffloadCLIPLoaderDisTorch2
// (render/comfyui-ext/nodes/offload_multicard) builds it off the cards and places it block by block (the plain `distorch2` kind stays for a split
// encoder that does fit one card while it is built).
//
// Classes are the ones ComfyUI-MultiGPU's own NODE_CLASS_MAPPINGS registers (read from the installed pack's __init__.py): the
// plain UNETLoaderMultiGPU / CLIPLoaderMultiGPU / VAELoaderMultiGPU (input `device`), the UNETLoaderDisTorch2MultiGPU and
// CLIPLoaderDisTorch2MultiGPU (`compute_device` / `device`, `virtual_vram_gb`, `donor_device`, `expert_mode_allocations`,
// `eject_models`) and, when ComfyUI-GGUF is installed, UnetLoaderGGUFDisTorch2MultiGPU. The UNET DisTorch2 loader ejects the
// loaded models unless told not to, so `eject_models: false` is always spelled out.
// Dependency-free.
import { readFileSync } from "node:fs";

export const MULTICARD_STORAGE = "cuda:0";
export const MULTICARD_COMPUTE = "cuda:1";

/** A row's `cardOrder`: "storage-first" (absent, the default: the two names above) or "compute-first" (rowDevices). The spellings are wan-multicard.mjs's CARD_ORDER_*. */
export const CARD_ORDER_STORAGE_FIRST = "storage-first";
export const CARD_ORDER_COMPUTE_FIRST = "compute-first";

/**
 * rowDevices: where a row's loaders put each weight, as ComfyUI numbers the two cards, and the order the launch lists them in (wan-multicard.mjs multicardLaunch).
 * A storage-first row (no `cardOrder`) is launched storage card first: the DiT computes on cuda:1, its donor blocks and the VAE are on cuda:0. A compute-first
 * row (`cardOrder: "compute-first"`, the qwen-image-2.1 rows) is launched compute card first, so its DiT computes on cuda:0: that DiT runs comfy_kitchen's
 * fused kernels, which run only on cuda:0 here (ComfyUI-MultiGPU #220; a 2026-10-20 run of the 2.1 bf16 DiT computing on cuda:1 staged every kernel's tensors
 * through host RAM, host free memory fell 12.5 GB in its first step, and the process aborted). The text encoder computes on cuda:0 in both orders (the device
 * every encoder of this table ran on; an fp8 one aborted on cuda:1) and parks its split share on cuda:1; the VAE is on the storage card in both.
 */
export function rowDevices(row) {
  if (row && row.cardOrder === CARD_ORDER_COMPUTE_FIRST) {
    return { order: CARD_ORDER_COMPUTE_FIRST, compute: "cuda:0", storage: "cuda:1", encoder: "cuda:0", encoderDonor: "cuda:1" };
  }
  return { order: CARD_ORDER_STORAGE_FIRST, compute: MULTICARD_COMPUTE, storage: MULTICARD_STORAGE, encoder: MULTICARD_STORAGE, encoderDonor: MULTICARD_COMPUTE };
}

/** The placements of the image and edit builders: single is today's graph (the default), multicard is the card-only one. */
export const PLACEMENTS = Object.freeze(["single", "multicard"]);
export const PLACEMENT_DEFAULT = "single";

function deepFreeze(o) {
  if (o && typeof o === "object" && !Object.isFrozen(o)) {
    Object.freeze(o);
    for (const v of Object.values(o)) deepFreeze(v);
  }
  return o;
}

/** The recipes, frozen: read once from multicard-recipes.json. */
export const RECIPES = deepFreeze(JSON.parse(readFileSync(new URL("./multicard-recipes.json", import.meta.url), "utf8")).rows);

/** A refusal to place a call, with the reason; the runners print it and exit before anything starts. */
export class RecipeRefusal extends Error {}

/** The text-encoder files that are never placed: the int8 encoder failed the 30 dB gate on the one prompt it was measured on. */
const BANNED_TEXT_ENCODER = /int8|int4|nf4/i;

const FAMILY_HINTS = Object.freeze({
  "qwen-image": "2512 bf16 fits no card (a 38 GiB DiT); bind the Q5_1 GGUF (14.3 GiB), which has a recipe",
  krea2: "the card-only recipe is the bf16 DiT with the bf16 Qwen3-VL-4B text encoder; bind those files",
  sdxl: "it fits one card, so there is nothing to place across two",
  "hidream-o1": "it fits one card pair only with host RAM and was never measured card-only",
  "hidream-o1-dev": "it fits one card pair only with host RAM and was never measured card-only",
  "qwen-image-2.1": "the card-only recipe is the bf16 DiT with the bf16 Qwen3-VL-8B text encoder; bind those files (the int8_convrot row is held back, see its `blocked`)",
});

// The hints above describe the IMAGE route's rows. The edit route has its own, because a family can have rows on one route and none on the other:
// qwen-image-2.1 has two image rows and no edit row, and its edit graph takes no placement argument, so the image rows would not give it one.
const EDIT_FAMILY_HINTS = Object.freeze({
  "qwen-image-2.1": "the 2.1 edit graph has no card-only placement (it takes no placement argument), so there is no recipe for it to hold back or lift",
});

/**
 * routeOffer: what the table offers on a route, as one sentence read from the table itself: the launchable rows, then the held-back ones. Written by hand it went
 * stale: it listed qwen-image-2.1 as having recipes after both of that family's rows were held back.
 */
function routeOffer(route) {
  const mine = RECIPES.filter((r) => r.route === route);
  const launchable = mine.filter((r) => !r.blocked).map((r) => r.id);
  const held = mine.filter((r) => r.blocked).map((r) => r.id);
  const offer = launchable.length ? `the card-only ${route} placement has recipes for ${launchable.join(", ")} only` : `the card-only ${route} placement has no launchable recipe`;
  return held.length ? `${offer}; ${held.join(", ")} ${held.length === 1 ? "is" : "are"} held back (see each row's \`blocked\`)` : offer;
}

/**
 * findRecipe: the recipe for a call, or a RecipeRefusal that says why there is none. route is "image" or "edit"; family is the
 * graph family ("" is the generic SDXL graph on the image route and the 2511 graph on the edit route); unet is the DiT file the
 * call names; clip is the text encoder it names (the recipe's default stands in when the builder has one and the call names none).
 */
export function findRecipe({ route, family, unet, clip } = {}) {
  const fam = String(family || "").trim() || (route === "edit" ? "qwen-image-edit-2511" : "sdxl");
  const rows = RECIPES.filter((r) => r.route === route && r.family === fam);
  const noRecipe = (why) => new RecipeRefusal(`no measured card-only recipe for ${fam}${route === "edit" ? " edit" : ""}${unet ? ` (${unet})` : ""}: ${why}`);
  if (rows.length === 0) {
    // an edit hint says why the family has no edit row and is followed by what the route does offer; an image hint stands alone; a family with neither gets the offer
    if (route === "edit") throw noRecipe([EDIT_FAMILY_HINTS[fam], routeOffer(route)].filter(Boolean).join("; "));
    throw noRecipe(FAMILY_HINTS[fam] || routeOffer(route));
  }
  const unetName = String(unet || "").trim();
  if (!unetName) throw new RecipeRefusal(`the card-only placement needs the DiT file to pick its recipe for ${fam}${route === "edit" ? " edit" : ""}: name it (--ckpt, or --unet on the edit route)`);
  const clipName = String(clip || "").trim();
  if (clipName && BANNED_TEXT_ENCODER.test(clipName)) {
    throw new RecipeRefusal(`the text encoder ${clipName} is int8: an int8 text encoder moved the render off the bf16 one (PSNR 28.65 dB against the 30 dB gate), so the card-only placement never uses one; name the bf16 or fp8 encoder the recipe was built for`);
  }
  const row = rows.find((r) => new RegExp(r.unetMatch, "i").test(unetName));
  if (!row) {
    const known = rows.map((r) => r.quant).join(", ");
    throw noRecipe(`the recipes of this family are ${known}; ${FAMILY_HINTS[fam] || "name one of those files"}`);
  }
  // A row that exists and is held back (its `blocked` says why) is refused by name here, the one choke point every graph builder and the planner go through.
  if (row.blocked) throw new RecipeRefusal(`the ${row.id} recipe is held back: ${row.blocked}`);
  const want = clipName || row.defaultClip || "";
  if (!want) throw new RecipeRefusal(`the card-only placement needs the text encoder file to pick its recipe for ${row.id}: name it (--clip)`);
  if (!new RegExp(row.clipMatch, "i").test(want)) {
    throw new RecipeRefusal(`the text encoder ${want} is not the one the ${row.id} recipe was built for (a ${row.clipMatch} file): the placement of any other was never measured`);
  }
  return row;
}

/**
 * canvasRefusal: why a row refuses a canvas of width x height pixels, or "" when it takes it. Only a row with a `maxPixels` cap refuses, and only a pair of positive
 * numbers is judged (a size not given is judged where the runner knows the default: comfy-render.mjs buildRenderGraph). The cap of the qwen-image/gguf-q5_1 row is
 * chosen, not measured; its `maxPixelsNote` says so, and the sentence repeats it. The edit rows have no pixel cap: the edit runner holds a pinned canvas to their
 * `maxMegapixels` itself (comfy-edit.mjs planEdit).
 */
export function canvasRefusal(row, width, height) {
  const w = Number(width), h = Number(height);
  if (!row || !(row.maxPixels > 0) || typeof width === "boolean" || typeof height === "boolean" || !Number.isFinite(w) || !Number.isFinite(h) || w <= 0 || h <= 0) return "";
  const asked = w * h;
  if (asked <= row.maxPixels) return "";
  const mp = (n) => (n / 1e6).toFixed(2);
  const note = row.maxPixelsNote ? `${row.maxPixelsNote}. ` : "";
  return `the card-only recipe ${row.id} cannot take a ${w}x${h} canvas (${mp(asked)} MP): it is held to at most ${mp(row.maxPixels)} MP (${row.maxPixels} pixels). ${note}Name a smaller --width and --height, or use the single placement`;
}

/**
 * allocationString: DisTorch2's expert-bytes allocation that keeps `residentGb` GiB on the device `first` (by default the compute card of a storage-first row) and
 * the remainder on `rest` (by default its storage card). The unit letter is required (`cuda:0,*` alone is rejected), so the share is always written with `gb`.
 */
export function allocationString(residentGb, first = MULTICARD_COMPUTE, rest = MULTICARD_STORAGE) {
  if (typeof residentGb !== "number" || !Number.isFinite(residentGb) || residentGb < 0) {
    throw new Error(`allocationString: residentGb must be a number of GiB, 0 or more, got ${JSON.stringify(residentGb)}`);
  }
  return `${first},${Number(residentGb.toFixed(2))}gb;${rest},*`;
}

/**
 * recipePeaks: the per-card VRAM peak (GiB) a call on this recipe is held to at a given resident share (null = the recipe's own, or a plain pin that has none).
 * The DiT's total is the invariant: --resident-gb keeps that many GiB of it on the compute card and the allocation string sends the REMAINDER to the storage
 * card. A larger share raises the compute peak; a smaller one puts the same bytes on the storage card. The storage figure is never lowered by a larger share,
 * because no run measured that: the recipe's own stands. For a recipe whose storage peak is its text-encoder stage (the 2.1 bf16 one) a smaller share is
 * therefore refused somewhat early, on the conservative side.
 */
export function recipePeaks(row, residentGb) {
  const shift = residentGb === null || residentGb === undefined || row.unet.loader === "plain" ? 0 : residentGb - row.unet.residentGb;
  return { compute: row.peakGiB.compute + shift, storage: row.peakGiB.storage + Math.max(0, -shift) };
}

/**
 * recipeResident: the GiB of the DiT kept on the compute card: the recipe's own, or the caller's override (--resident-gb). A recipe
 * that puts the whole DiT on the compute card has no share to set, and an override for it is refused rather than ignored.
 */
export function recipeResident(row, override) {
  if (row.unet.loader === "plain") {
    if (override !== undefined && override !== null) throw new Error(`--resident-gb means nothing to the ${row.id} recipe: it puts the whole DiT on the compute card`);
    return null;
  }
  const v = override === undefined || override === null ? row.unet.residentGb : override;
  if (typeof v !== "number" || !Number.isFinite(v) || v < 0 || v > 64) {
    throw new Error(`the DiT's resident share on the compute card must be a number of GiB from 0 to 64, got ${JSON.stringify(v)}`);
  }
  return v;
}

/**
 * multicardLoaders: the three loader nodes of a graph ("1" DiT, "2" text encoder, "3" VAE, the ids the single-card builders use), for a
 * recipe. unet/clip/vae are the file names the call binds; residentGb overrides the recipe's share of the DiT on the compute card.
 */
export function multicardLoaders(row, { unet, clip, vae, residentGb } = {}) {
  const share = recipeResident(row, residentGb);
  const d = rowDevices(row);
  let unetNode;
  switch (row.unet.loader) {
    case "plain":
      unetNode = { class_type: "UNETLoaderMultiGPU", inputs: { unet_name: unet, weight_dtype: "default", device: d.compute } };
      break;
    case "distorch2":
      unetNode = { class_type: "UNETLoaderDisTorch2MultiGPU", inputs: { unet_name: unet, weight_dtype: "default", compute_device: d.compute, virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: allocationString(share, d.compute, d.storage), eject_models: false } };
      break;
    case "gguf-distorch2":
      unetNode = { class_type: "UnetLoaderGGUFDisTorch2MultiGPU", inputs: { unet_name: unet, compute_device: d.compute, virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: allocationString(share, d.compute, d.storage), eject_models: false } };
      break;
    default:
      throw new Error(`multicardLoaders: recipe ${row.id} names the DiT loader ${JSON.stringify(row.unet.loader)}, which is none of plain|distorch2|gguf-distorch2`);
  }
  let clipNode;
  switch (row.clip.loader) {
    case "plain":
      clipNode = { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: clip, type: row.clipType || "qwen_image", device: d.encoder } };
      break;
    case "distorch2":
      clipNode = { class_type: "CLIPLoaderDisTorch2MultiGPU", inputs: { clip_name: clip, type: row.clipType || "qwen_image", device: d.encoder, virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: allocationString(row.clip.residentGb, d.encoderDonor, d.encoder), eject_models: false } };
      break;
    case "distorch2-host":
      // the harness's own loader: the encoder is built off the cards and placed by the allocation (it computes on `device`, cuda:0; the `residentGb` of it is
      // parked on the other card, cuda:1, and streamed across at encode time), so it never has to fit one card while it is built
      clipNode = { class_type: "OffloadCLIPLoaderDisTorch2", inputs: { clip_name: clip, type: row.clipType || "qwen_image", device: d.encoder, expert_mode_allocations: allocationString(row.clip.residentGb, d.encoderDonor, d.encoder), eject_models: false } };
      break;
    default:
      throw new Error(`multicardLoaders: recipe ${row.id} names the text-encoder loader ${JSON.stringify(row.clip.loader)}, which is none of plain|distorch2|distorch2-host`);
  }
  return {
    "1": unetNode,
    "2": clipNode,
    "3": { class_type: "VAELoaderMultiGPU", inputs: { vae_name: vae, device: d.storage } },
  };
}
