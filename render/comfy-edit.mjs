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
//   node render/comfy-edit.mjs <out.png> <target> "<instruction>" --family qwen-image-2.1 \
//        --unet qwen_image_2.1_bf16.safetensors --clip qwen3vl_8b_bf16.safetensors \
//        --vae qwen_image_2.1_vae_bf16.safetensors [--ref <image2> --ref <image3> ...] \
//        [--resolution 1024] [--cache-device auto|gpu|cpu|off] [--cache-dtype default|int8|int4] \
//        [--transparent 1] [--steps 40 --cfg 1]
//
// The 2511-only knobs (--preset, --lora, --lora-strength, --megapixels, --shift) and
// the 2.1-only knobs (--ref, --resolution, --cache-*, --transparent) are REFUSED on
// the other family (exit 2): a flag a graph never reads is a render that silently
// differs from what was asked.
import { copyFileSync, readFileSync, writeFileSync, unlinkSync, statSync } from "node:fs";
import { join, basename } from "node:path";
import { withGpuSlot } from "./gpu-lock.mjs";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";
import { buildQwenImageEdit, QWEN_EDIT_PRESETS, resolveEditMegapixels } from "./wf-qwen-image-edit.mjs";
import {
  buildQwenImage21Edit, QWEN_IMAGE_21_MAX_REFS, QWEN_IMAGE_21_RESOLUTION,
  QWEN_IMAGE_21_CACHE_DEVICES, QWEN_IMAGE_21_CACHE_DTYPES,
} from "./wf-qwen-image-21.mjs";
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
const ONLY_21 = ["resolution", "cache-device", "cache-dtype", "transparent"];

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
 * {width,height} (injectable; only the 2511 canvas needs it).
 */
export function planEdit({ pos, flags, refs = [], env = process.env, measure = (p) => imageSize(readFileSync(p)), exists = defaultExists, random = Math.random }) {
  const [out, imagePath, prompt] = pos;
  if (!out || !imagePath || !prompt) throw new UsageError('usage: node comfy-edit.mjs <out.png> <image> "<instruction>" [flags]');
  const family = flags.family || EDIT_DEFAULT_FAMILY;
  if (!EDIT_FAMILIES.includes(family)) {
    throw new UsageError(`error: unknown --family '${family}' (known: ${EDIT_FAMILIES.join(", ")}; omit it for the 2511 edit graph)`);
  }
  const unet = flags.unet || env.COMFY_EDIT_UNET;
  if (!unet) throw new UsageError("error: --unet is required (this machine's edit_unet binding)");
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
    const srcNote = src.width > 0 ? `${src.width}x${src.height}` : "size-unknown";
    return {
      family, sources: [imagePath], seed,
      describe: `family ${family} preset ${presetName} steps ${steps} cfg ${cfg} ${lora ? "lora " + lora : "no-lora"} src ${srcNote} mp ${megapixels}`,
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
  const cacheDevice = flags["cache-device"] || "auto";
  if (!QWEN_IMAGE_21_CACHE_DEVICES.includes(cacheDevice)) {
    throw new UsageError(`error: --cache-device must be one of ${QWEN_IMAGE_21_CACHE_DEVICES.join("|")}, got '${cacheDevice}'`);
  }
  const cacheDtype = flags["cache-dtype"] || "default";
  if (!QWEN_IMAGE_21_CACHE_DTYPES.includes(cacheDtype)) {
    throw new UsageError(`error: --cache-dtype must be one of ${QWEN_IMAGE_21_CACHE_DTYPES.join("|")}, got '${cacheDtype}'`);
  }
  const resolution = flags.resolution != null ? Number(flags.resolution) : QWEN_IMAGE_21_RESOLUTION;
  if (!Number.isInteger(resolution) || resolution < 0 || resolution > 4096 || resolution % 32 !== 0) {
    throw new UsageError(`error: --resolution must be 0 (keep each image's size) or a multiple of 32 up to 4096, got '${flags.resolution}'`);
  }
  const transparent = boolFlag("transparent", flags.transparent);
  const steps = flags.steps != null ? Number(flags.steps) : undefined;
  const cfg = flags.cfg != null ? Number(flags.cfg) : undefined;
  return {
    family, sources, seed,
    describe: `family ${family} images ${sources.length} resolution ${resolution} cache ${cacheDevice}/${cacheDtype}${transparent ? " transparent" : ""}`,
    build: (staged) => buildQwenImage21Edit({
      images: staged, prompt,
      negative: flags.negative || "",
      unet, clip: flags.clip, vae: flags.vae,
      steps, cfg,
      sampler: flags.sampler || undefined, scheduler: flags.scheduler || undefined,
      resolution, cacheDevice, cacheDtype, transparent, seed,
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
export function stageInputs(sources, staged, { inputDir, copy = copyFileSync, now = Date.now } = {}) {
  const stamp = now();
  sources.forEach((p, i) => {
    const name = `edit_in_${stamp}_${i}_${basename(p)}`;
    copy(p, join(inputDir, name));
    staged.push(name);
  });
  return staged;
}

/** unstageInputs removes every staged name; a missing file is not an error. */
export function unstageInputs(staged, { inputDir, unlink = unlinkSync } = {}) {
  for (const n of staged) { try { unlink(join(inputDir, n)); } catch {} }
}

async function main() {
  const { pos, flags, refs } = parseEditArgs(process.argv.slice(2));
  const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
  let plan;
  try {
    plan = planEdit({ pos, flags, refs });
  } catch (e) {
    if (e instanceof UsageError) { console.error(e.message); process.exit(2); }
    throw e;
  }
  const out = pos[0];
  const inputDir = join(COMFY_DIR, "input");

  async function render() {
    const staged = [];
    try {
      stageInputs(plan.sources, staged, { inputDir });
      const graph = plan.build(staged);
      // Shared submission/polling/retrieval (comfy-submit.mjs): CLI-preferred submit with
      // byte-identical raw fallback; hardened poll loop (dead-server watchdog).
      const cli = resolveCli();
      const { promptId } = await submitGraph({ api: API, graph, clientId: "edit-" + plan.seed, cli });
      console.log("queued", promptId, "seed", plan.seed, plan.describe);
      const waitSec = Number(process.env.COMFY_WAIT_SEC || 1800);
      const h = await pollOutputs({
        api: API, promptId, waitSec,
        isDone: (entry) => !!firstOutputFile(entry.outputs),
        noOutputMsg: "no edited image produced in time",
        onExecError: () => finalizeRun({ api: API, promptId, cli }),
      });
      const file = firstOutputFile(h.outputs);
      writeFileSync(out, await fetchView({ api: API, file }));
      console.log("WROTE", out);
      await finalizeRun({ api: API, promptId, cli });
    } finally {
      unstageInputs(staged, { inputDir });
    }
  }

  await withGpuSlot(
    { noLock: flags["no-lock"], comfyManaged: true, reserveVram: flags["reserve-vram"] },
    render,
  );
}

// Script entry only (the test imports planEdit/stageInputs). endsWith, not a URL
// comparison — see comfy-render.mjs for why.
if (process.argv[1] && /comfy-edit\.mjs$/.test(process.argv[1])) {
  main().catch((e) => { console.error("EDIT FAILED:", e.message); process.exit(1); });
}
