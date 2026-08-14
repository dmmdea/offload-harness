// comfy-music.mjs — local TEXT-TO-MUSIC runner (ACE-Step v1.5 turbo, split stack).
// The single entrypoint the local-offload `generate_audio` MCP tool shells out to for
// kind=music. Mirrors comfy-video.mjs: single-slot GPU lock + free llama-swap first +
// on-demand ComfyUI on :8188 + guarded zero-always-warm teardown, all via the shared
// withGpuSlot (gpu-lock.mjs) + ensureComfy (comfy-lifecycle.mjs) — NOT a duplicated
// lifecycle. Builds the ACE-Step v1.5 split graph via wf-acestep.mjs (UNET DiT +
// DualCLIP qwen encoders + music VAE). Seed-reproducible, so --seed is honored and
// reported. Output is FLAC via SaveAudio. Dependency-free (Node 18+).
//
// Usage:
//   node render/comfy-music.mjs <out.flac> "<style tags>" \
//        [--lyrics "..."] [--seconds N] [--seed N] [--steps N] [--cfg X] [--shift X] \
//        [--unet name.safetensors] [--reserve-vram X] [--api http://127.0.0.1:8188] \
//        [--no-lock] [--keep-comfy]   |   <out.flac> --graph wf.json
import { writeFileSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { buildAceStep } from "./wf-acestep.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";

// ACE-Step's 3.5B all-in-one checkpoint is far lighter than the 14B video models, so the
// generic 1.0 reserve (held back for the Windows display/WDDM) fits comfortably on 8GB.
// Per invariant 5 it stays per-workflow overridable via --reserve-vram.
export const RESERVE_VRAM_DEFAULT = "1.0";

// parseArgs: positionals + flags. --no-lock/--keep-comfy are boolean (no value);
// everything else consumes the next argv token. (Same shape as comfy-video.mjs.)
export function parseArgs(argv) {
  const pos = []; const flags = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) {
      const k = argv[i].slice(2);
      if (["no-lock", "keep-comfy"].includes(k)) flags[k] = true;
      else { flags[k] = argv[i + 1]; i++; }
    } else pos.push(argv[i]);
  }
  return { pos, flags };
}

// buildGraphFromArgs: resolve the ACE-Step graph + the concrete seed from parsed args.
// --graph wins (verbatim passthrough). Otherwise the prompt (style tags) is pos[1] (or
// --prompt), --lyrics/--seconds/--steps/--cfg/--shift flow into wf-acestep. A missing
// --seed mints a positive one so the render is still reproducible AND reported. Throws on
// a missing prompt (the Go wrapper maps a non-zero exit → a clean defer, invariant 4).
export function buildGraphFromArgs(pos, flags) {
  const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));
  if (flags.graph) {
    return { graph: JSON.parse(readFileSync(flags.graph, "utf8")), seed };
  }
  const prompt = pos[1] || flags.prompt;
  if (!prompt) throw new Error('comfy-music: a "<style tags>" prompt is required (e.g. "calm lo-fi piano, soft rain")');
  const common = { prompt, seed, seconds: Number(flags.seconds || 30) };
  if (flags.lyrics != null) common.lyrics = flags.lyrics;
  if (flags.steps) common.steps = Number(flags.steps);
  if (flags.cfg) common.cfg = Number(flags.cfg);
  if (flags.shift) common.shift = Number(flags.shift);
  if (flags.unet) common.unet = flags.unet; // v1.5 UNET override (was --ckpt in the retired v1 graph)
  return { graph: buildAceStep(common), seed };
}

// generate: submit the graph to ComfyUI, poll /history, fetch the produced audio via
// /view, write it to out. ComfyUI is already up (ensureComfy ran inside withGpuSlot).
// Submission/polling/retrieval are the shared comfy-submit.mjs layer: CLI-preferred
// submit with byte-identical raw fallback; hardened poll loop (dead-server watchdog).
async function generate(out, API, graph, seed) {
  const cli = resolveCli();
  const { promptId } = await submitGraph({ api: API, graph, clientId: "music-" + seed, cli });
  console.log("queued", promptId, "ace-step seed", seed);
  // waitSec 1200 = the historical fixed 600 x 2s polls (~20 min; TextEncodeAceStepAudio
  // can be slow on some commits). Deliberately NOT COMFY_WAIT_SEC-driven — this runner
  // never honored it, and preserving that is part of the step-4 exact-behavior contract.
  const h = await pollOutputs({
    api: API, promptId, waitSec: 1200,
    isDone: (entry) => !!firstOutputFile(entry.outputs),
    noOutputMsg: "no audio produced in time",
    onExecError: () => finalizeRun({ api: API, promptId, cli }),
  });
  const file = firstOutputFile(h.outputs);
  writeFileSync(out, await fetchView({ api: API, file }));
  console.log("WROTE", out);
  await finalizeRun({ api: API, promptId, cli });
}

// main: the executable path. Only runs when this file is invoked directly (so importing
// it in tests has no side effects — no GPU lock, no ComfyUI, no network).
async function main() {
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const out = pos[0];
  const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
  if (!out) { console.error('usage: node comfy-music.mjs <out.flac> "<style tags>" [--lyrics "..."] [--seconds N] [--seed N] [--reserve-vram X]   |   <out.flac> --graph wf.json'); process.exit(2); }
  const { graph, seed } = buildGraphFromArgs(pos, flags);
  await withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] || RESERVE_VRAM_DEFAULT },
    () => generate(out, API, graph, seed),
  );
}

// Run only as the CLI entrypoint (argv[1] is this file); a test `import` skips this.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main().catch((e) => { console.error("MUSIC GEN FAILED:", e.message); process.exit(1); });
}
