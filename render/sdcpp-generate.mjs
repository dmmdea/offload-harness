// sdcpp-generate.mjs — text-to-image via stable-diffusion.cpp (J2: the AMD/Vulkan
// tier's engine). Spawn-per-job single native binary under the shared GPU lock —
// zero-warm by construction (the process exits, the VRAM/GTT is gone; nothing to
// evict, nothing to /free). No ComfyUI, no Python anywhere on this path:
// comfyManaged:false frees llama-swap's slot semantics only.
//
// The Go side (internal/imagegen.sdcppArgs) speaks OUR generic flag surface; THIS
// file owns the mapping to sd.cpp's real CLI (verified against the pinned release
// master-789-5114672: --diffusion-model vs -m, --clip_l/--clip_g underscores,
// -p/-n prompts, -W/-H, -s seed, --cfg-scale, --sampling-method, -o output,
// --llm for LLM-class text encoders (Z-Image's Qwen3), --vae-on-cpu etc. arrive
// verbatim through repeated --extra tokens). sd.cpp flag drift on a pin bump is
// fixed HERE, never in Go.
//
// Usage: node render/sdcpp-generate.mjs <out.png> "<prompt>" [--negative S]
//        [--width N] [--height N] [--steps N] [--seed N] [--cfg F] [--sampler S]
//        [--model PATH] [--model-kind checkpoint|diffusion] [--vae PATH]
//        [--clip-l PATH] [--clip-g PATH] [--t5xxl PATH] [--llm PATH]
//        [--extra TOKEN]... [--no-lock]
// Env:   SDCPP_BIN (required) — the sd-cli.exe from the pinned win-vulkan release.
//        GPU_LOCK / GPU_LOCK_WAIT_MS — the shared single-slot lock (gpu-lock.mjs).
import { existsSync } from "node:fs";
import { spawn } from "node:child_process";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";

// parseArgs splits argv into positionals + flags. --no-lock is boolean; --extra is
// REPEATABLE (collected into an array); all other --flags take the next token.
export function parseArgs(argv) {
  const pos = [];
  const flags = {};
  const extra = [];
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) {
      const k = argv[i].slice(2);
      if (k === "no-lock") flags[k] = true;
      else if (k === "extra") {
        extra.push(argv[i + 1]);
        i++;
      } else {
        flags[k] = argv[i + 1];
        i++;
      }
    } else pos.push(argv[i]);
  }
  return { pos, flags, extra };
}

// buildSdArgs maps our generic surface to the sd.cpp CLI (pinned-release flag
// names — see header). Pure; unit-tested without spawning anything.
export function buildSdArgs(out, prompt, flags, extra) {
  const a = [];
  // Model: an all-in-one checkpoint loads via -m; a bare DiT via --diffusion-model.
  if (flags.model) {
    if (flags["model-kind"] === "diffusion") a.push("--diffusion-model", flags.model);
    else a.push("-m", flags.model);
  }
  for (const [ours, theirs] of [
    ["vae", "--vae"],
    ["clip-l", "--clip_l"],
    ["clip-g", "--clip_g"],
    ["t5xxl", "--t5xxl"],
    ["llm", "--llm"],
  ]) {
    if (flags[ours]) a.push(theirs, flags[ours]);
  }
  a.push("-p", prompt);
  if (flags.negative) a.push("-n", flags.negative);
  if (flags.width) a.push("-W", flags.width);
  if (flags.height) a.push("-H", flags.height);
  if (flags.steps) a.push("--steps", flags.steps);
  if (flags.seed) a.push("-s", flags.seed);
  if (flags.cfg) a.push("--cfg-scale", flags.cfg);
  if (flags.sampler) a.push("--sampling-method", flags.sampler);
  for (const e of extra) if (e) a.push(e);
  a.push("-o", out);
  return a;
}

async function main() {
  const { pos, flags, extra } = parseArgs(process.argv.slice(2));
  const out = pos[0];
  const prompt = pos[1];
  if (!out || !prompt) {
    console.error('usage: node sdcpp-generate.mjs <out.png> "<prompt>" [--model PATH ...] [--extra TOKEN]... [--no-lock]');
    process.exit(2);
  }
  const bin = process.env.SDCPP_BIN;
  if (!bin || !existsSync(bin)) {
    console.error("SDCPP FAILED: SDCPP_BIN not set or missing: " + (bin || "(unset)"));
    process.exit(1);
  }
  const args = buildSdArgs(out, prompt, flags, extra);
  await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
    const code = await new Promise((res) => spawn(bin, args, { stdio: "inherit" }).on("close", res));
    if (code !== 0) throw new Error("sd-cli exited " + code);
    if (!existsSync(out)) throw new Error("sd-cli exited 0 but produced no output at " + out);
    console.log("WROTE", out);
  });
}

// Run only as the main module — importing this file (tests) has no side effects.
if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  main().catch((e) => {
    console.error("SDCPP FAILED:", e.message);
    process.exit(1);
  });
}
