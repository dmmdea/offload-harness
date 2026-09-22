// comfy-render.mjs — a general image-generation runner for the local ComfyUI HTTP API.
//
// Part of the local-offload harness's vision side: the harness READS/assesses images
// with the Qwen3-VL tier (`local-offload vqa|ocr|extract-image|assess-image`); this
// standalone tool GENERATES them. It bakes in NO style, prompt, negative, model, or
// tuning — everything is a flag (or your own workflow), so it can be used as broadly
// or as narrowly as you like.
//
// Two modes:
//   1. --graph <workflow.json>   POST an arbitrary ComfyUI API-format workflow as-is
//                                (full control: any nodes, any model, any pipeline).
//   2. (default) a minimal SDXL text2img graph, fully parameterized via flags.
// Either way it POSTs the graph, polls /history, fetches the first output image, and
// writes it to <out>.
//
// Why SDXL for the built-in convenience graph: SDXL honors real NEGATIVE prompts at
// normal CFG, so hard exclusions (e.g. "no people / no text") are enforceable when you
// want them — pass them via --negative. Pair with `local-offload assess-image` to QA a
// render against such exclusions. (Nothing here assumes you want them; the default
// negative is empty.)
//
// Requires: Node 18+ (built-in fetch) and a running ComfyUI (default :8188). No npm deps.
//
// Usage:
//   node comfy-render.mjs <out.png> "<prompt>" [seed] [width] [height] \
//        [--negative "..."] [--ckpt name.safetensors] [--vae name.safetensors] \
//        [--steps 30] [--cfg 7] [--sampler dpmpp_2m] [--scheduler karras] \
//        [--api http://127.0.0.1:8188]
//   node comfy-render.mjs <out.png> --graph my-workflow.json [--api ...]
//   node comfy-render.mjs <out.png> "<prompt>" --family qwen-image --ckpt qwen-image-2512-Q5_1.gguf \
//        [--preset full|lightning4] [--clip te.safetensors] [--lora l.safetensors] [--shift 3.1]
//   node comfy-render.mjs <out.png> "<prompt>" --family qwen-image-2.1 --ckpt qwen_image_2.1_bf16.safetensors \
//        --clip qwen3vl_8b_bf16.safetensors --vae qwen_image_2.1_vae_bf16.safetensors \
//        [--schedule official|comfy] [--transparent 1] [--steps 40 --cfg 1]
//
// --family is a closed set (KNOWN_FAMILIES). An unrecognised value exits 2 BEFORE any
// ComfyUI work: it used to fall through to the SDXL graph silently, so a typo'd
// binding rendered the wrong model family (or died in ComfyUI validation after the
// GPU lease and a cold start had been paid for).

import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { buildHiDreamO1 } from "./wf-hidream-o1.mjs";
import { buildKrea2 } from "./wf-krea2.mjs";
import { buildQwenImage, QWEN_IMAGE_PRESETS } from "./wf-qwen-image.mjs";
import { buildQwenImage21, snapDim, QWEN_IMAGE_21_DEFAULT_SIZE, QWEN_IMAGE_21_SCHEDULES } from "./wf-qwen-image-21.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

/** A caller mistake: main() prints the message and exits 2 (never 1, never a render). */
export class UsageError extends Error {}

/**
 * Every --family this runner builds a graph for. "sdxl" (and an absent flag) is the
 * generic CheckpointLoaderSimple graph.
 */
export const KNOWN_FAMILIES = Object.freeze(["sdxl", "hidream-o1", "hidream-o1-dev", "krea2", "qwen-image", "qwen-image-2.1"]);

/** Boolean flag values this runner accepts (every --flag carries a value in this parser). */
export function parseBoolFlag(name, v) {
  if (v === undefined || v === null || v === "") return false;
  const s = String(v).trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(s)) return true;
  if (["0", "false", "no", "off"].includes(s)) return false;
  throw new UsageError(`error: --${name} takes 1|true|0|false, got '${v}'`);
}

export function parseRenderArgs(argv) {
  const pos = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) { flags[argv[i].slice(2)] = argv[i + 1]; i++; }
    else pos.push(argv[i]);
  }
  return { pos, flags };
}

/**
 * buildRenderGraph turns positionals + flags into the ComfyUI graph this run submits.
 * Pure apart from reading --graph from disk; caller mistakes throw UsageError. Returns
 * { graph, seed, width, height } (width/height as they will actually render).
 */
export function buildRenderGraph({ pos, flags, env = process.env, random = Math.random }) {
  let graph;
  let seed = Number(pos[2] || flags.seed || Math.floor(random() * 1e15));
  let width = Number(pos[3] || flags.width || 1024);
  let height = Number(pos[4] || flags.height || 1024);

  if (!flags.graph && flags.family !== undefined && flags.family !== "" && !KNOWN_FAMILIES.includes(flags.family)) {
    throw new UsageError(`error: unknown --family '${flags.family}' (known: ${KNOWN_FAMILIES.join(", ")}; omit it for the generic SDXL graph) — refusing to render a different model family than the one bound`);
  }

  if (flags.graph) {
    graph = JSON.parse(readFileSync(flags.graph, "utf8"));
  } else if (flags.family === "hidream-o1" || flags.family === "hidream-o1-dev") {
    // Model-family-correct graph (quality-first): the generic SDXL-shaped KSampler graph
    // is WRONG for HiDream-O1 (pixel-space DiT — wrong latent node, missing ModelNoiseScale
    // + patch-seam smoothing, off-distribution below ~4MP). --family selects the graph the
    // model was actually shipped with; which family a checkpoint is belongs to per-machine
    // config (imagegen_family), never shared code.
    const positive = pos[1] || flags.prompt || "";
    if (!positive) throw new UsageError("error: a prompt is required (positional or --prompt), unless you pass --graph");
    if (!flags.ckpt && !env.COMFY_CKPT) throw new UsageError("error: --family hidream-o1 requires --ckpt (the o1 checkpoint filename)");
    // O1 native resolution is 2048x2048 (trained ~4MP): positional/flag dims override,
    // otherwise the builder's native default applies — NOT the SDXL 1024 default.
    width = Number(pos[3] || flags.width || 2048);
    height = Number(pos[4] || flags.height || 2048);
    graph = buildHiDreamO1({
      prompt: positive, negative: flags.negative || "",
      ckpt: flags.ckpt || env.COMFY_CKPT,
      variant: flags.family === "hidream-o1-dev" ? "dev" : "base",
      width, height, seed,
      steps: Number(flags.steps || 0), cfg: Number(flags.cfg || 0),
      sampler: flags.sampler || "",
    });
  } else if (flags.family === "krea2") {
    // Model-family-correct graph: Krea 2 Turbo rides the Qwen-Image stack (shared
    // qwen VAE, Qwen3-VL encoder as CLIP type "krea2") but with ITS OWN template —
    // no shift node, regular latent, turbo recipe baked into the weights. --ckpt
    // carries the UNET filename (the per-machine imagegen_ckpt binding); encoder +
    // VAE default to the family's standard split files, overridable via
    // --clip / --vae. The 32GB-pool doctrine flags (--pool-vvram GiB donated to
    // --pool-donor, compute on --pool-compute) select the DisTorch2 pooled loader;
    // 0/absent renders single-GPU (the small-fleet shape).
    const positive = pos[1] || flags.prompt || "";
    if (!positive) throw new UsageError("error: a prompt is required (positional or --prompt), unless you pass --graph");
    if (!flags.ckpt && !env.COMFY_CKPT) throw new UsageError("error: --family krea2 requires --ckpt (the krea2 UNET filename)");
    // Loud here, not as a downstream node error: this family wires no GGUF
    // loader (the 32GB seat is bf16 by binding decision) — a .gguf name would
    // otherwise die inside ComfyUI validation with a message that never names
    // the actual constraint.
    if (String(flags.ckpt || env.COMFY_CKPT).toLowerCase().endsWith(".gguf")) {
      throw new UsageError("error: --family krea2 has no GGUF loader wired (bf16 seat) — bind a .safetensors UNET, or wire UnetLoaderGGUFDisTorch2MultiGPU first");
    }
    // Same half-override trap as qwen-image: steps and cfg travel together or not
    // at all — the turbo recipe (8/1.0) is the builder's baked default.
    if ((flags.steps != null) !== (flags.cfg != null)) {
      throw new UsageError("error: --family krea2 takes --steps and --cfg together or not at all — the turbo recipe (8 steps / cfg 1.0) is the sanctioned default; a half-override renders burned-out mush");
    }
    const vaeFlag = flags.vae || env.COMFY_VAE || "";
    if (["builtin", "none", "checkpoint"].includes(String(vaeFlag).toLowerCase())) {
      throw new UsageError("error: --family krea2 has no built-in VAE (the UNET file carries no VAE weights); unset imagegen_vae/--vae or name one (default qwen_image_vae.safetensors)");
    }
    width = Number(pos[3] || flags.width || 1024);
    height = Number(pos[4] || flags.height || 1024);
    // Optionals pass undefined so the BUILDER stays the single source of truth
    // for defaults — same rule as the qwen-image branch.
    graph = buildKrea2({
      prompt: positive, negative: flags.negative || "",
      unet: flags.ckpt || env.COMFY_CKPT,
      clip: flags.clip || undefined,
      vae: vaeFlag || undefined,
      steps: flags.steps != null ? Number(flags.steps) : undefined,
      cfg: flags.cfg != null ? Number(flags.cfg) : undefined,
      sampler: flags.sampler || undefined, scheduler: flags.scheduler || undefined,
      poolVvramGb: flags["pool-vvram"] != null ? Number(flags["pool-vvram"]) : undefined,
      poolCompute: flags["pool-compute"] || undefined,
      poolDonor: flags["pool-donor"] || undefined,
      width, height, seed,
    });
  } else if (flags.family === "qwen-image") {
    // Model-family-correct graph: Qwen-Image 2512 is an SD3-latent DiT whose text
    // encoder and VAE ship as separate files — there is no all-in-one checkpoint, so
    // the generic CheckpointLoaderSimple graph cannot even load it. --ckpt carries
    // the UNET filename (the per-machine imagegen_ckpt binding, e.g.
    // qwen-image-2512-Q5_1.gguf); text encoder + VAE default to the family's
    // standard split files, overridable via --clip / --vae.
    const positive = pos[1] || flags.prompt || "";
    if (!positive) throw new UsageError("error: a prompt is required (positional or --prompt), unless you pass --graph");
    if (!flags.ckpt && !env.COMFY_CKPT) throw new UsageError("error: --family qwen-image requires --ckpt (the qwen-image UNET filename, .gguf or .safetensors)");
    // The preset resolves the steps/cfg/LoRA pairing (wrong pairings render noise or
    // mush); --steps/--cfg override it only TOGETHER. A half-override is the classic
    // silent ruin: the harness forwards a caller's per-request steps but cfg only
    // ever comes from binding config, so "steps 8" alone would render the base model
    // at few steps / cfg 4 — technically successful, actually noise. Pair or preset.
    if ((flags.steps != null) !== (flags.cfg != null)) {
      throw new UsageError("error: --family qwen-image takes --steps and --cfg together or not at all — a half-override of the preset pairing renders noise or mush; pick a --preset (" + Object.keys(QWEN_IMAGE_PRESETS).join("|") + ") for a sanctioned pairing instead");
    }
    const presetName = flags.preset || "full";
    // hasOwn, not truthy-lookup: '--preset __proto__' must hit the helpful error,
    // not resolve up the prototype chain into a confusing builder throw.
    if (!Object.hasOwn(QWEN_IMAGE_PRESETS, presetName)) throw new UsageError(`error: unknown --preset '${presetName}' (known: ${Object.keys(QWEN_IMAGE_PRESETS).join(", ")})`);
    const preset = QWEN_IMAGE_PRESETS[presetName];
    // The UNET file carries no VAE weights — a "builtin" VAE binding is a config
    // error for this family (it means the machine's imagegen_vae still points at an
    // all-in-one checkpoint family like HiDream). Fail loud with the fix.
    const vaeFlag = flags.vae || env.COMFY_VAE || "";
    if (["builtin", "none", "checkpoint"].includes(String(vaeFlag).toLowerCase())) {
      throw new UsageError("error: --family qwen-image has no built-in VAE (the UNET file carries no VAE weights); unset imagegen_vae/--vae or name one (default qwen_image_vae.safetensors)");
    }
    width = Number(pos[3] || flags.width || 1328);
    height = Number(pos[4] || flags.height || 1328);
    // Everything optional passes undefined so the BUILDER stays the single source
    // of truth for defaults (sampler/scheduler/shift/strength) — restating them
    // here is how a future builder-default change silently misses this entrypoint.
    graph = buildQwenImage({
      prompt: positive, negative: flags.negative || "",
      unet: flags.ckpt || env.COMFY_CKPT,
      loader: flags.loader || undefined,
      clip: flags.clip || undefined,
      vae: vaeFlag || undefined,
      lora: flags.lora != null ? flags.lora : preset.lora,
      loraStrength: flags["lora-strength"] != null ? Number(flags["lora-strength"]) : undefined,
      steps: flags.steps != null ? Number(flags.steps) : preset.steps,
      cfg: flags.cfg != null ? Number(flags.cfg) : preset.cfg,
      sampler: flags.sampler || undefined, scheduler: flags.scheduler || undefined,
      shift: flags.shift != null ? Number(flags.shift) : undefined,
      width, height, seed,
    });
  } else if (flags.family === "qwen-image-2.1") {
    // Qwen-Image-2.1 (wf-qwen-image-21.mjs): a different model from 2512 — Qwen3-VL-8B
    // encoder, RGBA VAE, exponential FLUX-type schedule. Its three files are separate,
    // there is no default for any of them (bf16 vs int8_convrot is a per-node binding
    // choice), so --ckpt, --clip and --vae are all required here.
    const positive = pos[1] || flags.prompt || "";
    if (!positive) throw new UsageError("error: a prompt is required (positional or --prompt), unless you pass --graph");
    if (!flags.ckpt && !env.COMFY_CKPT) throw new UsageError("error: --family qwen-image-2.1 requires --ckpt (the 2.1 UNET filename, e.g. qwen_image_2.1_bf16.safetensors)");
    if (!flags.clip) throw new UsageError("error: --family qwen-image-2.1 requires --clip (the Qwen3-VL-8B text encoder, e.g. qwen3vl_8b_bf16.safetensors; bind imagegen_clip)");
    const vaeFlag = flags.vae || env.COMFY_VAE || "";
    if (!vaeFlag) throw new UsageError("error: --family qwen-image-2.1 requires --vae (the 2.1 RGBA VAE, qwen_image_2.1_vae_bf16.safetensors; bind imagegen_vae)");
    if (["builtin", "none", "checkpoint"].includes(String(vaeFlag).toLowerCase())) {
      throw new UsageError("error: --family qwen-image-2.1 has no built-in VAE (the UNET file carries no VAE weights, and the 2.1 VAE is a new 4-channel model); name qwen_image_2.1_vae_bf16.safetensors");
    }
    if (String(flags.ckpt || env.COMFY_CKPT).toLowerCase().endsWith(".gguf")) {
      throw new UsageError("error: --family qwen-image-2.1 has no GGUF loader wired (2.1 GGUFs need the leejet ComfyUI-GGUF fork) — bind the bf16 or int8_convrot .safetensors UNET");
    }
    // v1 is SINGLE-CARD: placement comes from the launch profile (comfy_cuda_device
    // + dynamic VRAM streaming), not from a DisTorch2 pool. Pool flags would be
    // parsed and never consulted — the silent-config state the pool warnings exist
    // to prevent — so they are refused until a pooled 2.1 loader is wired.
    for (const k of ["pool-vvram", "pool-compute", "pool-donor"]) {
      if (flags[k] != null && flags[k] !== "" && !(k === "pool-vvram" && Number(flags[k]) === 0)) {
        throw new UsageError(`error: --family qwen-image-2.1 has no pooled loader wired (--${k}) — it renders single-card; place it with comfy_cuda_device and unset imagegen_pool_* on this binding`);
      }
    }
    if ((flags.steps != null) !== (flags.cfg != null)) {
      throw new UsageError("error: --family qwen-image-2.1 takes --steps and --cfg together or not at all — the official recipe (40 steps / cfg 1.0) is the default; a half-override is a silently different render");
    }
    const schedule = flags.schedule || "official";
    if (!QWEN_IMAGE_21_SCHEDULES.includes(schedule)) {
      throw new UsageError(`error: --schedule must be one of ${QWEN_IMAGE_21_SCHEDULES.join("|")}, got '${schedule}'`);
    }
    const transparent = parseBoolFlag("transparent", flags.transparent);
    // Native 2K; W/H snap DOWN to /32 — report the size the latent really has.
    const wantW = Number(pos[3] || flags.width || QWEN_IMAGE_21_DEFAULT_SIZE);
    const wantH = Number(pos[4] || flags.height || QWEN_IMAGE_21_DEFAULT_SIZE);
    try { width = snapDim(wantW); height = snapDim(wantH); } catch (e) { throw new UsageError("error: " + e.message); }
    graph = buildQwenImage21({
      prompt: positive, negative: flags.negative || "",
      unet: flags.ckpt || env.COMFY_CKPT,
      clip: flags.clip,
      vae: vaeFlag,
      steps: flags.steps != null ? Number(flags.steps) : undefined,
      cfg: flags.cfg != null ? Number(flags.cfg) : undefined,
      sampler: flags.sampler || undefined, scheduler: flags.scheduler || undefined,
      schedule, transparent,
      width, height, seed,
    });
  } else {
    const positive = pos[1] || flags.prompt || "";
    if (!positive) throw new UsageError("error: a prompt is required (positional or --prompt), unless you pass --graph");
    const negative = flags.negative || "";                 // neutral default: no baked exclusions
    const ckpt = flags.ckpt || env.COMFY_CKPT || "RealVisXL_V5.0_fp16.safetensors";
    const vae = flags.vae || env.COMFY_VAE || "sdxl_vae.safetensors";
    const steps = Number(flags.steps || 30);
    const cfg = Number(flags.cfg || 7);
    const sampler = flags.sampler || "dpmpp_2m";
    const scheduler = flags.scheduler || "karras";
    // The graph shape is the same for SDXL and for a DiT (HiDream); what differs is
    // WHERE the VAE comes from. `--vae builtin` decodes with the VAE that the CHECKPOINT
    // LOADER supplies (its 3rd output, ["4",2] — verified against /object_info:
    // CheckpointLoaderSimple.output = [MODEL, CLIP, VAE]) instead of loading a standalone
    // one. That is REQUIRED for HiDream: its .safetensors carries NO VAE weights at all
    // (1474 tensors, all DiT/text/vision — it is pixel-space), so a standalone 4-channel
    // sdxl_vae cannot decode its output. Any other --vae value keeps the standalone
    // VAELoader (SDXL's default path), so callers that don't pass --vae are unaffected.
    const builtinVAE = ["builtin", "none", "checkpoint"].includes(String(vae).toLowerCase());
    graph = {
      "4":  { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: ckpt } },
      "5":  { class_type: "EmptyLatentImage", inputs: { width, height, batch_size: 1 } },
      "6":  { class_type: "CLIPTextEncode", inputs: { text: positive, clip: ["4", 1] } },
      "7":  { class_type: "CLIPTextEncode", inputs: { text: negative, clip: ["4", 1] } },
      "3":  { class_type: "KSampler", inputs: { seed, steps, cfg, sampler_name: sampler, scheduler, denoise: 1, model: ["4", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["5", 0] } },
      "8":  { class_type: "VAEDecode", inputs: { samples: ["3", 0], vae: builtinVAE ? ["4", 2] : ["10", 0] } },
      "9":  { class_type: "SaveImage", inputs: { filename_prefix: "render", images: ["8", 0] } },
    };
    if (!builtinVAE) graph["10"] = { class_type: "VAELoader", inputs: { vae_name: vae } };
  }
  return { graph, seed, width, height };
}

async function waitServer(API) {
  for (let i = 0; i < 90; i++) {
    // Per-probe abort: a wedged-but-listening server hangs sockets; without a signal the
    // fetch would stall this loop far past its intended ~3min cap.
    try { const r = await fetch(API + "/system_stats", { signal: AbortSignal.timeout(8000) }); if (r.ok) return true; } catch {}
    await new Promise(r => setTimeout(r, 2000));
  }
  throw new Error("ComfyUI not reachable on " + API + " after ~3min");
}

// firstImage: the first node output under `images` — this runner produces images, so a
// graph whose only outputs are e.g. text previews keeps polling until the budget ends
// (unchanged from the inline loop this replaces).
const firstImage = (outputs) => {
  for (const node of Object.values(outputs || {})) { if (node.images && node.images[0]) return node.images[0]; }
  return null;
};

async function main() {
  const { pos, flags } = parseRenderArgs(process.argv.slice(2));
  const out = pos[0];
  const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
  if (!out) { console.error('usage: node comfy-render.mjs <out.png> "<prompt>" [seed] [w] [h] [flags]   |   <out.png> --graph wf.json'); process.exit(2); }
  // Create the output parent UP-FRONT: a bad path must fail in 0s, not after the
  // render — an ENOENT at the write site used to discard a finished image after
  // every second of GPU work had succeeded (it cost a full A/B arm, 2026-08-10).
  mkdirSync(dirname(out) || ".", { recursive: true });

  // Build the graph: either the caller's full workflow, or a family / SDXL graph.
  let built;
  try {
    built = buildRenderGraph({ pos, flags });
  } catch (e) {
    if (e instanceof UsageError) { console.error(e.message); process.exit(2); }
    throw e;
  }
  const { graph, seed, width, height } = built;

  await waitServer(API);
  // Submission + polling + retrieval live in comfy-submit.mjs (shared by every runner):
  // submission prefers the vendored comfyui-pp-cli (idempotent lease, typed outcomes,
  // node_errors verbatim, run-row provenance) with raw POST as the byte-identical
  // fallback; polling keeps the dead-server watchdog + suspend/resume fence documented
  // there (2026-07-30 incident class).
  const cli = resolveCli();
  const { promptId } = await submitGraph({ api: API, graph, clientId: "render-" + seed, cli });
  console.log("queued", promptId, flags.graph ? `(graph: ${flags.graph})` : `seed ${seed} ${width}x${height}`);
  // Poll budget: quality-first renders (e.g. HiDream-O1 bf16 at native 2048, 40-step
  // SDE, RAM-offloaded) legitimately run far beyond the old ~6-min ceiling. Default
  // 30 min; the Go harness passes COMFY_WAIT_SEC aligned to its own timeout and its
  // process-tree kill remains the hard stop.
  const waitSec = Number(flags["wait-sec"] || process.env.COMFY_WAIT_SEC || 1800);
  const h = await pollOutputs({
    api: API, promptId, waitSec,
    isDone: (entry) => !!firstImage(entry.outputs),
    noOutputMsg: "no image produced in time",
    onExecError: () => finalizeRun({ api: API, promptId, cli }),
  });
  const img = firstImage(h.outputs);
  const buf = await fetchView({ api: API, file: img });
  writeFileSync(out, buf);
  console.log("WROTE", out, buf.length, "bytes");
  // Bookkeeping AFTER the artifact is safe on disk: records authoritative timing
  // (execution_start -> execution_success) and releases the CLI's submission lease.
  // Warn-only inside — a finished render is never re-opened by bookkeeping.
  await finalizeRun({ api: API, promptId, cli });
}

// Run only as a script (comfy-generate.mjs spawns this file), never on import — the
// test imports buildRenderGraph. endsWith, not a URL comparison: the harness may reach
// render/ through a junction, and Node reports the main module by its real path.
if (process.argv[1] && /comfy-render\.mjs$/.test(process.argv[1])) {
  main().catch(e => { console.error("RENDER FAILED:", e.message); process.exit(1); });
}
