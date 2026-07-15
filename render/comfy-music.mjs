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

// generate: POST the graph to ComfyUI, poll /history, fetch the produced audio via /view,
// write it to out. ComfyUI is already up (ensureComfy ran inside withGpuSlot).
async function generate(out, API, graph, seed) {
  const j = async (url, opts) => { const r = await fetch(url, opts); if (!r.ok) throw new Error(url + " -> " + r.status + " " + (await r.text()).slice(0, 300)); return r.json(); };
  const { prompt_id } = await j(API + "/prompt", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ prompt: graph, client_id: "music-" + seed }) });
  console.log("queued", prompt_id, "ace-step seed", seed);
  let file = null;
  for (let i = 0; i < 600; i++) { // up to ~20 min (TextEncodeAceStepAudio can be slow on some commits)
    await new Promise((r) => setTimeout(r, 2000));
    let hist; try { hist = await j(`${API}/history/${prompt_id}`); } catch { continue; }
    const h = hist[prompt_id]; if (!h) continue;
    if (h.status && h.status.status_str === "error") throw new Error("ComfyUI exec error: " + JSON.stringify(h.status).slice(0, 500));
    file = firstOutputFile(h.outputs); if (file) break;
  }
  if (!file) throw new Error("no audio produced in time");
  const q = new URLSearchParams({ filename: file.filename, subfolder: file.subfolder, type: file.type });
  const r = await fetch(`${API}/view?` + q.toString()); if (!r.ok) throw new Error("view fetch " + r.status);
  writeFileSync(out, Buffer.from(await r.arrayBuffer()));
  console.log("WROTE", out);
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
