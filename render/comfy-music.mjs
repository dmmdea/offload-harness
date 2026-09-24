// comfy-music.mjs — local TEXT-TO-MUSIC runner (ACE-Step v1.5 turbo, split stack).
// The single entrypoint the local-offload `generate_audio` MCP tool shells out to for
// kind=music. Mirrors comfy-video.mjs: single-slot GPU lock + free llama-swap first +
// on-demand ComfyUI on :8188 + guarded zero-always-warm teardown, all via the shared
// withGpuSlot (gpu-lock.mjs) + ensureComfy (comfy-lifecycle.mjs) — NOT a duplicated
// lifecycle. Builds the ACE-Step v1.5 split graph via wf-acestep.mjs (UNET DiT +
// DualCLIP qwen encoders + music VAE). Seed-reproducible, so --seed is honored and
// reported. Output is FLAC via SaveAudio. Dependency-free (Node 18+).
//
// Over-render + trim (2026-09-23, see audio-qa.mjs's header for the root cause and
// the measured per-second RMS data this is built from): the ACE-Step LM plans short
// instrumental renders to end 2-6s before the requested duration and pads the rest
// with near-silent codes. When the graph is built from args (not a verbatim --graph
// passthrough) and ffmpeg/ffprobe resolve, generate() asks buildAceStep for
// renderSeconds = seconds + max(6, ceil(0.2*seconds)) (computeRenderSeconds below)
// instead of the requested seconds, then trims the produced file back down to
// exactly what was asked for (1.0s fade-out on the cut, via audio-qa.mjs's
// trimToSeconds) BEFORE the dead-air gate measures it. A --graph passthrough, or
// ffmpeg/ffprobe being unavailable, renders the requested seconds exactly, as
// before this fix.
//
// Usage:
//   node render/comfy-music.mjs <out.flac> "<style tags>" \
//        [--lyrics "..."] [--seconds N] [--seed N] [--steps N] [--cfg X] [--shift X] \
//        [--unet name.safetensors] [--reserve-vram X] [--api http://127.0.0.1:8188] \
//        [--no-lock] [--keep-comfy]   |   <out.flac> --graph wf.json
import { writeFileSync, readFileSync, renameSync, unlinkSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { withGpuSlot } from "./gpu-lock.mjs";
import { firstOutputFile } from "./comfy-output.mjs";
import { buildAceStep } from "./wf-acestep.mjs";
import { resolveCli, submitGraph, pollOutputs, fetchView, finalizeRun } from "./comfy-submit.mjs";
import {
  resolveFfmpeg, resolveFfprobe, measure, assessDeadAir, normalizeLoudness, rewireSeed,
  trimToSeconds, LOUDNESS_TARGET_LUFS, TRUE_PEAK_TARGET_DBTP,
} from "./audio-qa.mjs";

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

// computeRenderSeconds: the over-length target for a requested `seconds`, per the
// measured ACE-Step LM behavior in audio-qa.mjs's header (short instrumental
// renders reliably end 2-6s early). +6s minimum (below which a 2-6s early ending
// would eat the whole clip) or +20% for longer requests, whichever is larger — the
// 36s-vs-30s measurement that motivated this (audio-qa.mjs header) is +6s, i.e.
// exactly this floor.
export function computeRenderSeconds(seconds) {
  return seconds + Math.max(6, Math.ceil(0.2 * seconds));
}

// buildGraphFromArgs: resolve the ACE-Step graph + the concrete seed from parsed args.
// --graph wins (verbatim passthrough) and is never over-rendered (its duration is
// opaque to this function). Otherwise the prompt (style tags) is pos[1] (or --prompt),
// --lyrics/--seconds/--steps/--cfg/--shift flow into wf-acestep. A missing --seed mints
// a positive one so the render is still reproducible AND reported. Throws on a missing
// prompt (the Go wrapper maps a non-zero exit → a clean defer, invariant 4).
//
// opts.trim (default false; the caller passes true only when ffmpeg/ffprobe resolve —
// see main()) makes buildAceStep's `seconds` (and so both TextEncodeAceStepAudio1.5's
// duration and EmptyAceStep1.5LatentAudio's seconds) the over-length
// computeRenderSeconds(seconds) instead of the requested seconds; the caller trims the
// produced file back down afterward. The returned `seconds`/`renderSeconds` tell the
// caller what to trim to and whether trimming applies at all (undefined = no trim,
// either because opts.trim was false or because --graph was used).
export function buildGraphFromArgs(pos, flags, { trim = false } = {}) {
  const seed = Number(flags.seed || Math.floor(Math.random() * 1e15));
  if (flags.graph) {
    return { graph: JSON.parse(readFileSync(flags.graph, "utf8")), seed };
  }
  const prompt = pos[1] || flags.prompt;
  if (!prompt) throw new Error('comfy-music: a "<style tags>" prompt is required (e.g. "calm lo-fi piano, soft rain")');
  const seconds = Number(flags.seconds || 30);
  const renderSeconds = trim ? computeRenderSeconds(seconds) : seconds;
  const common = { prompt, seed, seconds: renderSeconds };
  if (flags.lyrics != null) common.lyrics = flags.lyrics;
  if (flags.steps) common.steps = Number(flags.steps);
  if (flags.cfg) common.cfg = Number(flags.cfg);
  if (flags.shift) common.shift = Number(flags.shift);
  if (flags.unet) common.unet = flags.unet; // v1.5 UNET override (was --ckpt in the retired v1 graph)
  return { graph: buildAceStep(common), seed, seconds, renderSeconds: trim ? renderSeconds : undefined };
}

// renderOnce: submit the graph to ComfyUI, poll /history, fetch the produced audio
// via /view, write it to out. ComfyUI is already up (ensureComfy ran inside
// withGpuSlot). Submission/polling/retrieval are the shared comfy-submit.mjs layer:
// CLI-preferred submit with byte-identical raw fallback; hardened poll loop
// (dead-server watchdog). Split out of generate() so the audio-QA retry (below) can
// call it a second time with a re-seeded graph without duplicating this plumbing.
async function renderOnce(out, API, graph, seed, cli) {
  const { promptId } = await submitGraph({ api: API, graph, clientId: "music-" + seed, cli });
  console.log("queued", promptId, "ace-step seed", seed);
  // waitSec 1200 = the historical fixed 600 x 2s polls (~20 min; TextEncodeAceStepAudio
  // can be slow on some commits). Deliberately NOT COMFY_WAIT_SEC-driven — this runner
  // never honored it, and preserving that is part of the step-4 exact-behavior contract.
  const h = await pollOutputs({
    api: API, promptId, waitSec: 1200,
    isDone: (entry) => !!firstOutputFile(entry.outputs, graph),
    noOutputMsg: "no audio produced in time",
    onExecError: () => finalizeRun({ api: API, promptId, cli }),
  });
  const file = firstOutputFile(h.outputs, graph);
  writeFileSync(out, await fetchView({ api: API, file }));
  console.log("WROTE", out);
  await finalizeRun({ api: API, promptId, cli });
}

// applyTrim: best-effort in-place trim of `out` down to `seconds` (write-to-tmp then
// rename over the original — ffmpeg cannot read and write the same file, same
// convention normalizeLoudness below uses). Called once per render (the initial one
// and the dead-air retry, if any) whenever generate() over-rendered. Never throws —
// a trim failure falls through to measuring/shipping the over-length file as-is
// (house rule: a QA/post-processing hiccup never costs an already-produced render).
function applyTrim(ffmpeg, out, seconds) {
  const tmpOut = out + ".trim.tmp" + (out.match(/\.[^.]+$/)?.[0] || ".flac");
  if (trimToSeconds(ffmpeg, out, seconds, tmpOut)) {
    unlinkSync(out);
    renameSync(tmpOut, out);
    console.error(`audio-qa: trimmed the over-length render to the requested ${seconds}s (1.0s fade-out on the cut)`);
  } else {
    console.error("audio-qa: trim to the requested length failed — measuring the over-length render as-is");
  }
}

// cleanupFailedDeadAirOutput: best-effort removal of a failed render's leftover
// file at `out`. renderOnce always writes ComfyUI's raw SaveAudio bytes (FLAC,
// unconditionally — see the file header) straight to `out`, whatever extension
// the caller requested; applyTrim above re-muxes it to match that extension when
// it runs and succeeds (it re-encodes for the fade-out regardless, so ffmpeg
// picks the container from `out`'s own name), but it does NOT run on a `--graph`
// passthrough (renderSeconds is never set — see buildGraphFromArgs) and can fail
// silently on its own ffmpeg call (logged, never fatal — the over-length render
// ships as-is per house content-preservation rule). Either way, `out` at the
// point of a persisting DEAD_AIR is EITHER genuinely mismatched-container bytes
// (no trim ran, or it failed) — exactly the "FLAC bytes in a .wav name" finding
// (R1, 2026-09-23 OptiPlex remediation) — OR a correctly-muxed file that still
// failed content QA. Neither is a result to leave at the caller's requested
// path, so cleanup removes it unconditionally rather than only in the narrower
// mismatched-container case. `unlink` is injectable so this decision is unit-
// tested without a live ComfyUI; failures are swallowed on purpose — a cleanup
// hiccup must never hide the real DEAD_AIR error the caller needs.
export function cleanupFailedDeadAirOutput(out, { unlink = unlinkSync } = {}) {
  try {
    unlink(out);
  } catch {
    // best-effort: the file may already be gone, or removal may be denied; either
    // way the caller is about to see the real DEAD_AIR error, which matters more.
  }
}

// generate: renderOnce, then (when over-rendered — see comfy-music.mjs's header and
// computeRenderSeconds) trim back to the requested length, then the audio-QA gate
// (F-35 regression follow-up, 2026-09-23 — see audio-qa.mjs for the root-cause
// writeup). Dead air (trailing/leading silence > 1.0s, or > 10% of the clip silent)
// gets exactly ONE re-render with a fresh seed, trimmed the same way; if it persists
// the run fails with a DEAD_AIR-tagged error so gpugen.ClassifyErr (Go side) can defer
// it typed rather than as a bare timeout/other. The accepted render is always
// loudness-normalized (independent of the dead-air verdict — the unmanaged 0 dBFS
// true peak measured on the original defect renders is a separate issue). ffmpeg/
// ffprobe unavailable = no over-render happened (buildGraphFromArgs never got
// opts.trim) and the gate skips itself entirely; it never turns an otherwise-
// successful render into a failure just because the measuring tool is missing.
async function generate(out, API, graph, seed, { ffmpeg, ffprobe, seconds, renderSeconds } = {}) {
  const cli = resolveCli();
  await renderOnce(out, API, graph, seed, cli);

  if (!ffmpeg || !ffprobe) {
    console.error("audio-qa: ffmpeg/ffprobe not available (set FFMPEG_PATH or put ffmpeg on PATH) — skipping the dead-air/loudness gate");
    return;
  }

  // renderSeconds is only set when buildGraphFromArgs actually over-rendered (args-
  // built graph + ffmpeg/ffprobe resolved at build time); a --graph passthrough never
  // sets it, so trimsApply stays false and the file is measured exactly as produced.
  const trimsApply = renderSeconds != null && seconds != null && renderSeconds !== seconds;
  if (trimsApply) applyTrim(ffmpeg, out, seconds);

  let verdict = assessDeadAir(measure(ffmpeg, ffprobe, out));
  const seedsTried = [seed];
  if (verdict.deadAir) {
    const retrySeed = Math.floor(Math.random() * 1e15);
    seedsTried.push(retrySeed);
    console.error(`audio-qa: dead air detected (${verdict.reason}) — retrying once with seed ${retrySeed}`);
    await renderOnce(out, API, rewireSeed(graph, retrySeed), retrySeed, cli);
    if (trimsApply) applyTrim(ffmpeg, out, seconds);
    verdict = assessDeadAir(measure(ffmpeg, ffprobe, out));
    if (verdict.deadAir) {
      cleanupFailedDeadAirOutput(out);
      throw new Error(`DEAD_AIR: dead air persisted after a retry (${verdict.reason}); seeds tried: ${seedsTried.join(", ")}`);
    }
    console.error(`audio-qa: retry clean (${verdict.reason})`);
  }

  const tmpOut = out + ".loudnorm.tmp" + (out.match(/\.[^.]+$/)?.[0] || ".flac");
  if (normalizeLoudness(ffmpeg, out, tmpOut)) {
    unlinkSync(out);
    renameSync(tmpOut, out);
    console.error(`audio-qa: loudness-normalized to I=${LOUDNESS_TARGET_LUFS} LUFS / TP=${TRUE_PEAK_TARGET_DBTP} dBTP`);
  } else {
    console.error("audio-qa: loudness normalization failed or skipped — shipping the un-normalized render (never withhold an already-produced render)");
  }
}

// main: the executable path. Only runs when this file is invoked directly (so importing
// it in tests has no side effects — no GPU lock, no ComfyUI, no network).
async function main() {
  const { pos, flags } = parseArgs(process.argv.slice(2));
  const out = pos[0];
  const API = flags.api || process.env.COMFY_API || "http://127.0.0.1:8188";
  if (!out) { console.error('usage: node comfy-music.mjs <out.flac> "<style tags>" [--lyrics "..."] [--seconds N] [--seed N] [--reserve-vram X]   |   <out.flac> --graph wf.json'); process.exit(2); }
  // Resolved once, up front: whether to over-render (and later trim) depends on
  // ffmpeg/ffprobe being available, and buildAceStep's `seconds` has to be decided
  // at graph-build time (below) — before ComfyUI/the GPU lock are even touched.
  const ffmpeg = resolveFfmpeg();
  const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
  const trim = !flags.graph && !!ffmpeg && !!ffprobe;
  const { graph, seed, seconds, renderSeconds } = buildGraphFromArgs(pos, flags, { trim });
  await withGpuSlot(
    { noLock: flags["no-lock"], keepComfy: flags["keep-comfy"], comfyManaged: true, reserveVram: flags["reserve-vram"] || RESERVE_VRAM_DEFAULT },
    () => generate(out, API, graph, seed, { ffmpeg, ffprobe, seconds, renderSeconds }),
  );
}

// Run only as the CLI entrypoint (argv[1] is this file); a test `import` skips this.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main().catch((e) => { console.error("MUSIC GEN FAILED:", e.message); process.exit(1); });
}
