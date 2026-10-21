// comfy-edit.mjs — GENERATIVE, mask-free image edit runner.
// Families: Qwen-Image-Edit 2511 (the default graph, wf-qwen-image-edit.mjs) and
// Qwen-Image-2.1 (--family qwen-image-2.1, wf-qwen-image-21.mjs: up to 10 images,
// the first is the edit target). Wraps the chosen builder with the standard
// lifecycle: single-slot GPU lock, free llama-swap, on-demand ComfyUI,
// zero-always-warm teardown (withGpuSlot). Stages every input into
// <COMFY_DIR>/input (LoadImage reads only from there) and removes them all in a
// finally. Dependency-free, Node 18+.
//
// Sibling routes, kept distinct on purpose:
//   edit_image    — deterministic PIL ops (crop/resize/composite). No model.
//   inpaint_image — SDXL re-denoise INSIDE a user-supplied mask.
//   this          — instruction-following rewrite of the whole frame, no mask.
//
// Usage:
//   node render/comfy-edit.mjs <out.png> <image> "<instruction>" \
//        [--negative ...] [--unet name] [--preset full|lightning8|lightning4] \
//        [--lora name] [--lora-strength F] [--steps N] [--cfg F] \
//        [--megapixels F] \
//        [--clip name] [--vae name] [--sampler s] [--scheduler s] [--shift F] \
//        [--seed N] [--api http://127.0.0.1:8188] [--no-lock] [--reserve-vram F]
//        [--placement single|multicard [--resident-gb N] [--cards <a>,<b>]]   # multicard: the 2511 edit and the qwen-image-2.1 edit on the TWO non-display cards of a lease
//                                      # (docs/systems/media-generation.md, "Card-only image and edit"); flags spelled `--flag value`
//   node render/comfy-edit.mjs <out.png> <target> "<instruction>" --family qwen-image-2.1 \
//        --unet qwen_image_2.1_bf16.safetensors --clip qwen3vl_8b_bf16.safetensors \
//        --vae qwen_image_2.1_vae_bf16.safetensors [--ref <image2> --ref <image3> ...] \
//        [--resolution 1024] [--cache-device auto|gpu|cpu|off] [--cache-dtype default|int8|int4] \
//        [--transparent 1] [--steps 40 --cfg 1]
//   (the 2.1 edit under --placement multicard: --cache-device defaults to gpu and takes gpu|off only; the canvas is the DiT sequence, see planEdit)
//
// The 2511-only knobs (--preset, --lora, --lora-strength, --megapixels, --shift) and
// the 2.1-only knobs (--ref, --resolution, --cache-*, --transparent) are REFUSED on
// the other family (exit 2): a flag a graph never reads is a render that silently
// differs from what was asked.
import { copyFileSync, readFileSync, unlinkSync, statSync } from "node:fs";
import { join } from "node:path";
import { writeFileAtomic } from "./atomic-out.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { familySignature } from "./comfy-family.mjs";
import { COMFY_DIR, comfyApi, comfyLogCursor } from "./comfy-lifecycle.mjs";
import { assertNodeClasses } from "./comfy-nodes.mjs";
import { planImageMulticard, lifecycleOptions, verifyImagePlacement, imagePlacementMode, beginPlacedWatch, settlePlacedRun } from "./multicard-image.mjs";
import { findRecipe, RecipeRefusal } from "./multicard-recipes.mjs";
import { stagedInputName } from "./comfy-input.mjs";
import { buildQwenImageEdit, QWEN_EDIT_PRESETS, resolveEditMegapixels } from "./wf-qwen-image-edit.mjs";
import {
  buildQwenImage21Edit, QWEN_IMAGE_21_MAX_REFS, QWEN_IMAGE_21_RESOLUTION,
  QWEN_IMAGE_21_CACHE_DEVICES, QWEN_IMAGE_21_CACHE_DTYPES,
 QWEN_IMAGE_21_EDIT_SCHEDULES } from "./wf-qwen-image-21.mjs";
import { imageSize } from "./image-size.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

/** A caller mistake: main() prints it and exits 2. */
export class UsageError extends Error {}

/** The default graph family (empty --family) and the opt-in one. */
export const EDIT_DEFAULT_FAMILY = "qwen-image-edit-2511";
export const EDIT_FAMILIES = Object.freeze([EDIT_DEFAULT_FAMILY, "qwen-image-2.1"]);

const BOOL_FLAGS = ["no-lock"];
const ONLY_2511 = ["preset", "lora", "lora-strength", "megapixels", "shift", "loader"];
const ONLY_21 = ["resolution", "cache-device", "cache-dtype", "transparent", "schedule"];

/** Positionals + flags; --ref may repeat (collected in order into refs). */
export function parseEditArgs(argv) {
  const pos = []; const flags = {}; const refs = [];
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) {
      const k = argv[i].slice(2);
      if (BOOL_FLAGS.includes(k)) flags[k] = true;
      else if (k === "ref") { refs.push(argv[i + 1]); i++; }
      else { flags[k] = argv[i + 1]; i++; }
    } else pos.push(argv[i]);
  }
  return { pos, flags, refs };
}

function boolFlag(name, v) {
  if (v === undefined || v === null || v === "") return false;
  const s = String(v).trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(s)) return true;
  if (["0", "false", "no", "off"].includes(s)) return false;
  throw new UsageError(`error: --${name} takes 1|true|0|false, got '${v}'`);
}

/**
 * planEdit resolves the family and every knob, refusing caller mistakes BEFORE any
 * GPU work. It returns the source images (target first) and a build(stagedNames)
 * that turns the staged filenames into the graph. measure(path) returns the file's
 * {width,height} (injectable; the 2511 canvas needs it, and so does the card-only 2.1 canvas at --resolution 0).
 */
export function planEdit({ pos, flags, refs = [], env = process.env, measure = (p) => imageSize(readFileSync(p)), exists = defaultExists, random = Math.random, multicard }) {
  const [out, imagePath, prompt] = pos;
  if (!out || !imagePath || !prompt) throw new UsageError('usage: node comfy-edit.mjs <out.png> <image> "<instruction>" [flags]');
  const family = flags.family || EDIT_DEFAULT_FAMILY;
  if (!EDIT_FAMILIES.includes(family)) {
    throw new UsageError(`error: unknown --family '${family}' (known: ${EDIT_FAMILIES.join(", ")}; omit it for the 2511 edit graph)`);
  }
  const unet = flags.unet || env.COMFY_EDIT_UNET;
  if (!unet) throw new UsageError("error: --unet is required (this machine's edit_unet binding)");
  // --placement single (today's graph, the default) | multicard (the card-only recipe of render/multicard-recipes.mjs). The plan (the two cards
  // and their launch) is made by the runner before it gets here; a graph is never built as "multicard" without one, and never as single.
  let placementMode;
  try { placementMode = imagePlacementMode(flags); } catch (e) { throw new UsageError("error: " + e.message); }
  let recipe = null;
  if (placementMode === "multicard") {
    if (!multicard) throw new UsageError("error: --placement multicard needs the plan the runner makes before it builds a graph (the two cards and their launch); a direct call of planEdit has none");
    try {
      recipe = findRecipe({ route: "edit", family, unet, clip: flags.clip });
    } catch (e) {
      if (e instanceof RecipeRefusal) throw new UsageError("error: " + e.message);
      throw e;
    }
  }
  const seed = Number(flags.seed || Math.floor(random() * 1e15));
  for (const p of [imagePath, ...refs]) {
    if (!p) throw new UsageError("error: --ref needs a path");
    if (!exists(p)) throw new UsageError(`error: input image not found: ${p}`);
  }

  if (family === EDIT_DEFAULT_FAMILY) {
    for (const k of ONLY_21) {
      if (flags[k] != null) throw new UsageError(`error: --${k} is a qwen-image-2.1 edit knob; the 2511 graph has no such input (pass --family qwen-image-2.1)`);
    }
    if (refs.length) throw new UsageError("error: --ref is multi-reference; the 2511 edit graph takes ONE image (pass --family qwen-image-2.1 for up to 10)");
    // A preset pairs steps+cfg+lora. Mixing a Lightning LoRA with base-model steps
    // (or vice versa) still renders — it just renders badly — so the pairing is
    // resolved here rather than left to whatever the caller half-specifies.
    const presetName = flags.preset || env.COMFY_EDIT_PRESET || "lightning8";
    if (!Object.hasOwn(QWEN_EDIT_PRESETS, presetName)) {
      throw new UsageError(`error: --preset must be one of ${Object.keys(QWEN_EDIT_PRESETS).join("|")}, got ${presetName}`);
    }
    const preset = QWEN_EDIT_PRESETS[presetName];
    const steps = Number(flags.steps || 0) || preset.steps;
    const cfg = Number(flags.cfg || 0) || preset.cfg;
    const lora = flags.lora != null ? flags.lora : preset.lora;
    // Working canvas. Unset means "keep the source's own resolution, capped" — so the
    // source is measured here, where the real file is still on disk, rather than guessed
    // in the graph builder (which only ever sees the staged filename). An unreadable
    // header is not fatal: resolveEditMegapixels falls back to the cap.
    const configuredMP = Number(flags.megapixels || env.COMFY_EDIT_MEGAPIXELS || 0);
    let src = { width: 0, height: 0 };
    try { src = measure(imagePath); } catch { /* size stays unknown */ }
    const megapixels = resolveEditMegapixels({ configured: configuredMP, width: src.width, height: src.height });
    // The card-only recipe's peaks are scaled to a canvas of at most recipe.maxMegapixels. resolveEditMegapixels holds a canvas it derives from the source to the
    // cap itself, but honours a pinned one verbatim, and a pinned canvas above the cap is one no peak was measured or scaled for: on a 16 GB card it is the
    // out-of-memory with both cards held. Refused here, before anything is started.
    if (recipe && recipe.maxMegapixels > 0 && megapixels > recipe.maxMegapixels) {
      const via = flags.megapixels ? `--megapixels ${flags.megapixels}` : `COMFY_EDIT_MEGAPIXELS ${env.COMFY_EDIT_MEGAPIXELS} (gen_edit_megapixels reaches the runner as --megapixels)`;
      throw new UsageError(`error: ${via} pins a ${megapixels} MP edit canvas, and the card-only recipe ${recipe.id} is scaled to at most ${recipe.maxMegapixels.toFixed(1)} MP (its compute card holds ${recipe.activationsGiB} GiB of activations there, and a larger canvas needs more than the two cards have left): pin ${recipe.maxMegapixels.toFixed(1)} or less, or drop --placement multicard; nothing was started`);
    }
    const srcNote = src.width > 0 ? `${src.width}x${src.height}` : "size-unknown";
    return {
      family, sources: [imagePath], seed,
      describe: `family ${family} preset ${presetName} steps ${steps} cfg ${cfg} ${lora ? "lora " + lora : "no-lora"} src ${srcNote} mp ${megapixels}${recipe ? ` placement multicard (${recipe.id})` : ""}`,
      build: (staged) => buildQwenImageEdit({
        image: staged[0], prompt,
        negative: flags.negative || "",
        unet,
        loader: flags.loader || "auto",
        clip: flags.clip || undefined,
        vae: flags.vae || undefined,
        lora, loraStrength: Number(flags["lora-strength"] || 0) || undefined,
        steps, cfg, megapixels,
        sampler: flags.sampler || undefined,
        scheduler: flags.scheduler || undefined,
        shift: Number(flags.shift || 0) || undefined,
        seed,
        ...(recipe ? { placement: "multicard", residentGb: multicard.residentGb ?? undefined } : {}),
      }),
    };
  }

  // qwen-image-2.1
  for (const k of ONLY_2511) {
    if (flags[k] != null) throw new UsageError(`error: --${k} is a 2511 edit knob; qwen-image-2.1 has no such input (its recipe is 40 steps / cfg 1, set with --steps and --cfg together)`);
  }
  const sources = [imagePath, ...refs];
  if (sources.length > QWEN_IMAGE_21_MAX_REFS) {
    throw new UsageError(`error: qwen-image-2.1 edits take at most ${QWEN_IMAGE_21_MAX_REFS} images (the target plus ${QWEN_IMAGE_21_MAX_REFS - 1} --ref), got ${sources.length}`);
  }
  if (!flags.clip) throw new UsageError("error: --family qwen-image-2.1 requires --clip (the Qwen3-VL-8B text encoder; bind gen_edit_clip)");
  if (!flags.vae) throw new UsageError("error: --family qwen-image-2.1 requires --vae (the 2.1 RGBA VAE; bind gen_edit_vae)");
  if ((flags.steps != null) !== (flags.cfg != null)) {
    throw new UsageError("error: --family qwen-image-2.1 takes --steps and --cfg together or not at all (official recipe 40 / 1.0)");
  }
  const cacheDevice = flags["cache-device"] || (recipe ? "gpu" : "auto");
  if (!QWEN_IMAGE_21_CACHE_DEVICES.includes(cacheDevice)) {
    throw new UsageError(`error: --cache-device must be one of ${QWEN_IMAGE_21_CACHE_DEVICES.join("|")}, got '${cacheDevice}'`);
  }
  if (recipe && (cacheDevice === "auto" || cacheDevice === "cpu")) {
    throw new UsageError(`error: --cache-device ${cacheDevice} can pin the prefix cache in host RAM, which the card-only placement does not use: leave it unset (gpu, the default there) or pass gpu or off; nothing was started`);
  }
  const cacheDtype = flags["cache-dtype"] || "default";
  if (!QWEN_IMAGE_21_CACHE_DTYPES.includes(cacheDtype)) {
    throw new UsageError(`error: --cache-dtype must be one of ${QWEN_IMAGE_21_CACHE_DTYPES.join("|")}, got '${cacheDtype}'`);
  }
  const schedule = flags.schedule || "comfy";
  if (!QWEN_IMAGE_21_EDIT_SCHEDULES.includes(schedule)) {
    throw new UsageError(`error: --schedule must be one of ${QWEN_IMAGE_21_EDIT_SCHEDULES.join("|")} on the qwen-image-2.1 edit graph (turbo = the Turbo checkpoint's saved 8-step sigmas, steps 8 / cfg 1), got '${schedule}'`);
  }
  const resolution = flags.resolution != null ? Number(flags.resolution) : QWEN_IMAGE_21_RESOLUTION;
  if (!Number.isInteger(resolution) || resolution < 0 || resolution > 4096 || resolution % 32 !== 0) {
    throw new UsageError(`error: --resolution must be 0 (keep each image's size) or a multiple of 32 up to 4096, got '${flags.resolution}'`);
  }
  // The card-only recipe's peaks are scaled to a DiT sequence of at most recipe.maxPixels: the target's latent plus every image's, one token per 16x16 pixels, so
  // (images + 1) grids of resolution squared (the target is counted twice, once as a reference and once as the latent). At --resolution 0 each image counts at its own
  // size rounded to 32, and an unreadable header cannot be counted: refused by name, not guessed. Refused here, before anything is started.
  if (recipe && recipe.maxPixels > 0) {
    let pixels = 0;
    if (resolution > 0) pixels = (sources.length + 1) * resolution * resolution;
    else {
      const areas = sources.map((s) => {
        let size;
        try { size = measure(s); } catch { throw new UsageError(`error: --resolution 0 keeps each image's own size and ${s} has no readable header to count it by, so the canvas of the card-only recipe ${recipe.id} cannot be judged: pass a numeric --resolution; nothing was started`); }
        return Math.max(32, Math.round(size.width / 32) * 32) * Math.max(32, Math.round(size.height / 32) * 32);
      });
      pixels = areas.reduce((a, b) => a + b, 0) + areas[0];
    }
    if (pixels > recipe.maxPixels) {
      const mp = (n) => (n / 1e6).toFixed(2);
      const fits = Math.floor(Math.sqrt(recipe.maxPixels / (sources.length + 1)) / 32) * 32;
      throw new UsageError(`error: the card-only recipe ${recipe.id} cannot take this edit: ${sources.length} image${sources.length === 1 ? "" : "s"} at --resolution ${resolution} make a ${mp(pixels)} MP DiT sequence (the target counted twice), and it is held to at most ${mp(recipe.maxPixels)} MP (${recipe.maxPixels} pixels). ${recipe.maxPixelsNote ? recipe.maxPixelsNote + ". " : ""}Pass --resolution ${fits} or less or fewer --ref, or drop --placement multicard; nothing was started`);
    }
  }
  const transparent = boolFlag("transparent", flags.transparent);
  const steps = flags.steps != null ? Number(flags.steps) : undefined;
  const cfg = flags.cfg != null ? Number(flags.cfg) : undefined;
  return {
    family, sources, seed,
    describe: `family ${family} images ${sources.length} resolution ${resolution} cache ${cacheDevice}/${cacheDtype} schedule ${schedule}${transparent ? " transparent" : ""}${recipe ? ` placement multicard (${recipe.id})` : ""}`,
    build: (staged) => buildQwenImage21Edit({
      images: staged, prompt,
      negative: flags.negative || "",
      unet, clip: flags.clip, vae: flags.vae,
      steps, cfg,
      sampler: flags.sampler || undefined, scheduler: flags.scheduler || undefined,
      resolution, cacheDevice, cacheDtype, schedule, transparent, seed,
      ...(recipe ? { placement: "multicard", residentGb: multicard.residentGb ?? undefined } : {}),
    }),
  };
}

function defaultExists(p) {
  try { return statSync(p).isFile(); } catch { return false; }
}

/**
 * stageInputs copies every source into <inputDir> under a unique name, pushing each
 * name onto `staged` AS IT LANDS — so a copy that fails half-way still leaves the
 * caller's finally a complete list of what to remove.
 */
export function stageInputs(sources, staged, { inputDir, copy = copyFileSync, now = Date.now, pid, rand } = {}) {
  const stamp = now();
  sources.forEach((p, i) => {
    const name = stagedInputName("edit_in", p, { now: () => stamp, pid, rand, n: i });
    copy(p, join(inputDir, name));
    staged.push(name);
  });
  return staged;
}

/** unstageInputs removes every staged name; a missing file is not an error. */
export function unstageInputs(staged, { inputDir, unlink = unlinkSync } = {}) {
  for (const n of staged) { try { unlink(join(inputDir, n)); } catch {} }
}

// The outside world renderEdit() touches, replaced by recording fakes in render/multicard-image-run.test.mjs; the defaults are the real calls.
// The poll is a literal `pollOutputs(` call: gpu_progress_test.go finds the render helpers that heartbeat by it.
export const RENDER_IO = Object.freeze({
  verifyPlacement: verifyImagePlacement, preflight: assertNodeClasses, logCursor: comfyLogCursor, stage: stageInputs, unstage: unstageInputs, resolveCli,
  submit: submitGraph, poll: (opts) => pollOutputs(opts), view: fetchView, write: writeFileAtomic, finalize: finalizeRun,
  stdout: (...a) => console.log(...a), stderr: (...a) => console.error(...a), env: process.env, comfyDir: COMFY_DIR,
});

/** renderEdit: what the run does once it holds the GPU slot: check the placement, stage the inputs, submit, wait, write, finalize, unstage. */
export async function renderEdit({ out, API, plan, placement, inputDir }, io = {}) {
  const x = { ...RENDER_IO, ...io };
  // The card-only placement: the running ComfyUI's own cards must be the pinned pair and the harness's pack must have loaded, before
  // anything is staged or posted (a mismatch submits nothing).
  if (placement) await x.verifyPlacement(API, placement);
  const staged = [];
  try {
    x.stage(plan.sources, staged, { inputDir });
    const graph = plan.build(staged);
    // Shared submission/polling/retrieval (comfy-submit.mjs): CLI-preferred submit with
    // byte-identical raw fallback; hardened poll loop (dead-server watchdog).
    const cli = x.resolveCli();
    // The card-only placement: the graph's node classes are asked for before it is posted, and the instance's console is watched while the prompt runs
    // (an out-of-memory wedge ComfyUI does not report, a decode it retried tiled); the cursor is made BEFORE the submit.
    if (placement) await x.preflight(API, graph, { comfyDir: x.comfyDir });
    const watch = beginPlacedWatch(placement, { cursorFn: x.logCursor, comfyDir: x.comfyDir, say: x.stderr });
    const { promptId } = await x.submit({ api: API, graph, clientId: "edit-" + plan.seed, cli });
    x.stdout("queued", promptId, "seed", plan.seed, plan.describe);
    const waitSec = Number(x.env.COMFY_WAIT_SEC || 1800);
    const h = await x.poll({
      api: API, promptId, waitSec,
      isDone: (entry) => !!firstOutputFile(entry.outputs, graph),
      noOutputMsg: "no edited image produced in time",
      onExecError: () => x.finalize({ api: API, promptId, cli }),
      ...(watch ? { watch } : {}),
    });
    const file = firstOutputFile(h.outputs, graph);
    const bytes = await x.view({ api: API, file });
    await settlePlacedRun({ watch, plan: placement, out, bytes, write: x.write, finalize: () => x.finalize({ api: API, promptId, cli }), stderr: x.stderr });
    x.stdout("WROTE", out);
    await x.finalize({ api: API, promptId, cli });
  } finally {
    x.unstage(staged, { inputDir });
  }
}

async function main() {
  const { pos, flags, refs } = parseEditArgs(process.argv.slice(2));
  // The card-only placement is planned first: it refuses, before anything is launched, every request it cannot honour (no recipe, no cards
  // named, not two, a display card). Its own environment names the instance, so the api and the lease slot read it too.
  let placement = null;
  try {
    placement = planImageMulticard({ route: "edit", family: flags.family || EDIT_DEFAULT_FAMILY, unet: flags.unet || process.env.COMFY_EDIT_UNET, clip: flags.clip, flags, comfyDir: COMFY_DIR, say: (m) => console.error(m) });
  } catch (e) { console.error("PLACEMENT REFUSED: " + e.message); process.exit(2); }
  const API = comfyApi(flags.api, placement ? placement.env : process.env);
  let plan;
  try {
    plan = planEdit({ pos, flags, refs, multicard: placement });
  } catch (e) {
    if (e instanceof UsageError) { console.error(e.message); process.exit(2); }
    throw e;
  }
  const out = pos[0];
  const inputDir = join(COMFY_DIR, "input");

  const render = () => renderEdit({ out, API, plan, placement, inputDir });

  await withGpuSlot(
    { noLock: flags["no-lock"], comfyManaged: true, api: API, reserveVram: flags["reserve-vram"],
      family: familySignature(flags.family || EDIT_DEFAULT_FAMILY, flags.unet || process.env.COMFY_EDIT_UNET),
      ...lifecycleOptions(placement) },
    render,
  );
}

// Script entry only (the test imports planEdit/stageInputs). endsWith, not a URL
// comparison — see comfy-render.mjs for why.
if (process.argv[1] && /comfy-edit\.mjs$/.test(process.argv[1])) {
  main().catch((e) => { console.error("EDIT FAILED:", e.message); process.exit(1); });
}
