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
//        [--extra TOKEN]... [--transparent 1] [--no-lock]
// Env:   SDCPP_BIN (required) — the sd-cli.exe from the pinned win-vulkan release.
//        GPU_LOCK / GPU_LOCK_WAIT_MS — the shared single-slot lock (gpu-lock.mjs).
//
// Alpha (D5, ADR 0058): stable-diffusion.cpp's qwen-image-2.1 VAE emits RGBA
// UNCONDITIONALLY — there is no sd.cpp flag for it, the channel is just always there
// (binxarn wave session 5d227d30 §3c measured this on an ORDINARY, non-RGBA prompt:
// alpha 243-255, 5.53% of pixels < 255). Left alone, that hands every "opaque" render
// a partially-transparent PNG a downstream compositor would read wrong. So this
// runner, not sd-cli, owns the request's `transparent` contract end to end:
//   --transparent unset/0 (default): the prompt renders as given; after sd-cli exits,
//     the written PNG is re-encoded with the alpha channel DROPPED (png-alpha.mjs's
//     flattenToOpaqueRGB — a channel split, same as the ComfyUI graph's
//     SplitImageWithAlpha, never a composite onto a background).
//   --transparent 1: the prompt is wrapped in the official RGBA template
//     (wf-qwen-image-21.mjs's rgbaPrompt — the SAME constant the ComfyUI builder
//     uses, imported rather than duplicated) and the written PNG is left exactly as
//     sd-cli wrote it, alpha intact.
// The pipeline's SupportsTransparentImage gate already restricts --transparent to the
// qwen-image-2.1 family before this script ever runs, so no family check happens here.
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { rgbaPrompt } from "./wf-qwen-image-21.mjs";
import { flattenToOpaqueRGB } from "./png-alpha.mjs";

// truthy: the same value spellings imagegen.go's paramTrue accepts — Go always sends
// "1", but a human running this script by hand may type --transparent true/yes/on.
function truthy(v) {
  return v === true || v === "1" || v === "true" || v === "TRUE" || v === "True" || v === "yes" || v === "on";
}

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
// names — see header). Pure; unit-tested without spawning anything. transparent
// wraps the prompt in the official RGBA template — sd.cpp has no CLI flag for
// alpha; the model reads it from the prompt wording alone (header comment).
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
  const p = truthy(flags.transparent) ? rgbaPrompt(prompt) : prompt;
  a.push("-p", p);
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

// postprocessOutput: the D5 alpha contract applied to an already-written PNG.
// transparent=false (default) flattens RGBA to opaque RGB in place; transparent=true
// leaves the file untouched (sd.cpp's native alpha stays). Returns true when the file
// was rewritten. A PNG shape flattenToOpaqueRGB cannot decode (anything but 8-bit,
// non-interlaced) is left exactly as sd-cli wrote it — a cosmetic post-process step
// must never fail an otherwise-successful render.
export function postprocessOutput(out, transparent) {
  if (truthy(transparent)) return false;
  const before = readFileSync(out);
  let after;
  try {
    after = flattenToOpaqueRGB(before);
  } catch (e) {
    console.error("sdcpp-generate: alpha flatten skipped (" + e.message + ") — output kept as sd-cli wrote it");
    return false;
  }
  if (after === before || after.equals(before)) return false;
  writeFileSync(out, after);
  return true;
}

// parseVulkanDeviceList: `<bin> --list-devices` prints one "VulkanN <adapter
// name>" line per ICD-visible device (any other line is ignored). Pure.
export function parseVulkanDeviceList(text) {
  const devices = [];
  const re = /^\s*Vulkan(\d+)\s+(.+?)\s*$/;
  for (const line of String(text || "").split(/\r?\n/)) {
    const m = re.exec(line);
    if (m) devices.push({ index: m[1], name: m[2] });
  }
  return devices;
}

// isIntegratedGpuName: a known Intel-integrated part — never the adapter to pick
// for a diffusion render (measured 2026-09-23 on the OptiPlex: 565-608 s/step on
// an Intel UHD 630 vs 4.85 s/step on the same box's RTX 5060).
export function isIntegratedGpuName(name) {
  return /\bintel\b|\buhd\b|\biris\b|\barc\b/i.test(name || "");
}

// isDiscreteGpuName: a discrete NVIDIA/AMD adapter — the one worth pinning.
export function isDiscreteGpuName(name) {
  return /\bnvidia\b|\bgeforce\b|\bquadro\b|\bamd\b|\bradeon\b/i.test(name || "") && !isIntegratedGpuName(name);
}

// pickDiscreteVulkanDevice: the first discrete adapter's index in listed order,
// null when none of devices is discrete. Pure.
export function pickDiscreteVulkanDevice(devices) {
  const d = (devices || []).find((d) => isDiscreteGpuName(d.name));
  return d ? d.index : null;
}

// resolveVulkanDevice picks the GGML_VK_VISIBLE_DEVICES value this render will
// use: an explicit env override always wins (never guessed around); otherwise the
// first discrete adapter `<bin> --list-devices` reports; otherwise device "0"
// (ggml-Vulkan's own default — the pre-fix behavior when nothing better can be
// determined). `list` is injectable for tests (no real spawn).
//
// No memoization here: this whole script is spawn-per-job (header) and main()
// calls this exactly once per process, so "cached per process" falls out of the
// architecture rather than needing a module-level cache — which would also wrongly
// leak a stale device across unrelated node:test cases sharing this process.
//
// Fixes the OptiPlex defect (2026-09-23, REMEDIATION R8): this runner used to pin
// device 0 unconditionally whenever the environment left it unset. On a box with
// an enabled integrated GPU, ggml-Vulkan enumerates the iGPU FIRST (Vulkan0 =
// Intel UHD 630, Vulkan1 = the RTX 5060 there), so every render silently ran on
// the iGPU — no error, exit 0, just 100x slower (565-608 s/step vs 4.85 s/step).
export function resolveVulkanDevice(bin, envValue, { list = defaultListDevices } = {}) {
  if (envValue) return envValue;
  let devices = [];
  try {
    devices = parseVulkanDeviceList(list(bin));
  } catch (e) {
    console.error("sdcpp-generate: --list-devices probe failed (" + e.message + ") — defaulting to Vulkan device 0");
    return "0";
  }
  const discrete = pickDiscreteVulkanDevice(devices);
  if (discrete != null) return discrete;
  console.error("sdcpp-generate: no discrete (NVIDIA/AMD) Vulkan device found — defaulting to Vulkan device 0" +
    (devices.length ? " (" + devices.map((d) => `Vulkan${d.index} ${d.name}`).join("; ") + ")" : " (--list-devices reported nothing)"));
  return "0";
}

// defaultListDevices: the real probe, `<bin> --list-devices`, combined
// stdout+stderr (sd.cpp release builds have printed device enumeration to either
// stream across versions). Synchronous — this runs once, before the GPU slot is
// even taken, and every other step in main() is already sequential.
function defaultListDevices(bin) {
  const r = spawnSync(bin, ["--list-devices"], { encoding: "utf8", timeout: 15000 });
  if (r.error) throw r.error;
  return (r.stdout || "") + "\n" + (r.stderr || "");
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
  // Pick ONE Vulkan device deterministically: an operator override already in the
  // environment always wins; otherwise auto-detect the discrete adapter (see
  // resolveVulkanDevice's header — device 0 is NOT safe to assume, an enabled
  // iGPU enumerates first on some boxes). Multi-ICD boxes otherwise enumerate
  // unstably, which pinning ONE value (rather than leaving it to ggml's own
  // default) still protects against.
  if (!process.env.GGML_VK_VISIBLE_DEVICES) {
    process.env.GGML_VK_VISIBLE_DEVICES = resolveVulkanDevice(bin, "");
  }
  const args = buildSdArgs(out, prompt, flags, extra);
  await withGpuSlot({ noLock: flags["no-lock"], comfyManaged: false }, async () => {
    const code = await new Promise((res) => spawn(bin, args, { stdio: "inherit" }).on("close", res));
    if (code !== 0) throw new Error("sd-cli exited " + code);
    if (!existsSync(out)) throw new Error("sd-cli exited 0 but produced no output at " + out);
    postprocessOutput(out, flags.transparent);
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
