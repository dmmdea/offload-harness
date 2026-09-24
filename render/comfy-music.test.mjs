// node --test render/comfy-music.test.mjs
// Tests comfy-music.mjs arg parsing + ACE-Step graph building with ComfyUI STUBBED —
// no live render, no network, no GPU. Mirrors the seams comfy-video.mjs uses: the
// worker exports pure parseArgs()/buildGraphFromArgs() and only runs withGpuSlot when
// invoked as the main module (so importing it here has no side effects).
import { test } from "node:test";
import assert from "node:assert";
import { writeFileSync, mkdtempSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  parseArgs, buildGraphFromArgs, computeRenderSeconds, RESERVE_VRAM_DEFAULT,
  cleanupFailedDeadAirOutput,
} from "./comfy-music.mjs";

test("parseArgs: positionals + flags (out, prompt, --seconds/--seed/--lyrics/--reserve-vram)", () => {
  const { pos, flags } = parseArgs([
    "out.flac", "calm lo-fi piano, soft rain",
    "--seconds", "8", "--seed", "42", "--lyrics", "la la la", "--reserve-vram", "2.0",
  ]);
  assert.equal(pos[0], "out.flac");
  assert.equal(pos[1], "calm lo-fi piano, soft rain");
  assert.equal(flags.seconds, "8");
  assert.equal(flags.seed, "42");
  assert.equal(flags.lyrics, "la la la");
  assert.equal(flags["reserve-vram"], "2.0");
});

test("parseArgs: boolean flags (--no-lock, --keep-comfy) take no value", () => {
  const { pos, flags } = parseArgs(["out.flac", "techno", "--no-lock", "--keep-comfy", "--seconds", "10"]);
  assert.equal(flags["no-lock"], true);
  assert.equal(flags["keep-comfy"], true);
  assert.equal(flags.seconds, "10"); // still parsed as a value flag after the booleans
  assert.equal(pos.length, 2);
});

test("buildGraphFromArgs: prompt is the second positional; seed/seconds threaded in", () => {
  const { pos, flags } = parseArgs(["out.flac", "upbeat corporate, 120 bpm", "--seconds", "8", "--seed", "42"]);
  const { graph, seed } = buildGraphFromArgs(pos, flags);
  assert.equal(seed, 42, "explicit --seed is honored (reproducibility)");
  // ACE-Step graph shape (mirrors wf-acestep.test.mjs)
  const types = Object.values(graph).map((n) => n.class_type);
  for (const need of ["UNETLoader", "DualCLIPLoader", "TextEncodeAceStepAudio1.5", "EmptyAceStep1.5LatentAudio", "KSampler", "VAEDecodeAudio", "SaveAudio"]) {
    assert.ok(types.includes(need), `graph must include ${need}`);
  }
  // seconds wired into the empty latent
  const lat = Object.values(graph).find((n) => n.class_type === "EmptyAceStep1.5LatentAudio");
  assert.equal(lat.inputs.seconds, 8);
  // seed wired into KSampler
  const ks = Object.values(graph).find((n) => n.class_type === "KSampler");
  assert.equal(ks.inputs.seed, 42);
  // the single v1.5 encoder carries the style tags
  const encs = Object.values(graph).filter((n) => n.class_type === "TextEncodeAceStepAudio1.5");
  assert.ok(encs.some((e) => e.inputs.tags.includes("corporate")), "positive carries the tags");
});

test("buildGraphFromArgs: --lyrics flows into the positive encoder (vocals support)", () => {
  const { pos, flags } = parseArgs(["out.flac", "pop ballad", "--lyrics", "hello from the other side"]);
  const { graph } = buildGraphFromArgs(pos, flags);
  const encs = Object.values(graph).filter((n) => n.class_type === "TextEncodeAceStepAudio1.5");
  assert.ok(encs.some((e) => e.inputs.lyrics.includes("hello from the other side")), "lyrics carried on the encoder");
  // v1.5 has ONE encoder + a ConditioningZeroOut negative (no second empty encoder)
  assert.ok(Object.values(graph).some((n) => n.class_type === "ConditioningZeroOut"), "negative is a zeroed-out conditioning");
});

test("buildGraphFromArgs: no --seed mints a positive seed (still reproducible/reported)", () => {
  const { pos, flags } = parseArgs(["out.flac", "ambient drone"]);
  const { seed } = buildGraphFromArgs(pos, flags);
  assert.ok(Number.isInteger(seed) && seed > 0, "a seed is always minted");
});

test("buildGraphFromArgs: missing prompt throws (defer-not-crash maps exit!=0 → defer)", () => {
  const { pos, flags } = parseArgs(["out.flac"]); // no prompt
  assert.throws(() => buildGraphFromArgs(pos, flags), /prompt/i);
});

test("buildGraphFromArgs: --graph <file> passthrough uses the supplied graph verbatim", () => {
  const customGraph = { "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "x.safetensors" } } };
  const gf = join(mkdtempSync(join(tmpdir(), "music-graph-")), "wf.json");
  writeFileSync(gf, JSON.stringify(customGraph));
  const { graph } = buildGraphFromArgs(["out.flac"], { graph: gf, seed: "5" });
  assert.deepEqual(graph, customGraph);
});

test("RESERVE_VRAM_DEFAULT: an ACE-Step-appropriate default is exported (overridable)", () => {
  // ACE-Step's 3.5B all-in-one is lighter than Wan 14B; a conservative reserve still fits 8GB.
  assert.ok(typeof RESERVE_VRAM_DEFAULT === "string" && Number(RESERVE_VRAM_DEFAULT) > 0, "a numeric string default");
});

// ---- over-render + trim (dead-air mitigation, 2026-09-23) -----------------------
// See audio-qa.mjs's header for the root cause (ACE-Step's LM plans short
// instrumental renders to end 2-6s early) and the measured numbers these cases pin.

test("computeRenderSeconds: matches the measured 30s->36s example (a +6s floor, not +20%)", () => {
  // The defect writeup measured 30s requests reliably finishing music by ~24-28s;
  // 36s requests (30 + 6, the floor, since 20% of 30 is exactly 6) put music across
  // the whole span in 3/3 seeds.
  assert.equal(computeRenderSeconds(30), 36);
});

test("computeRenderSeconds: the +6s floor governs short requests (20% would be too small)", () => {
  assert.equal(computeRenderSeconds(8), 14); // ceil(0.2*8)=2 < 6, so +6
  assert.equal(computeRenderSeconds(5), 11); // ceil(0.2*5)=1 < 6, so +6
});

test("computeRenderSeconds: 20% takes over once it exceeds the +6s floor", () => {
  assert.equal(computeRenderSeconds(60), 72); // ceil(0.2*60)=12 > 6, so +12
});

test("buildGraphFromArgs: trim:true builds the graph at the over-length renderSeconds, not the requested seconds", () => {
  const { pos, flags } = parseArgs(["out.flac", "upbeat latin pop instrumental", "--seconds", "30"]);
  const { graph, seconds, renderSeconds } = buildGraphFromArgs(pos, flags, { trim: true });
  assert.equal(seconds, 30, "the requested seconds is still reported (the caller trims back to it)");
  assert.equal(renderSeconds, 36, "renderSeconds is the over-length target");
  // Both consumers of "seconds" in wf-acestep.mjs must get the OVER-length value —
  // the whole point is the LM plans against a longer duration.
  const enc = Object.values(graph).find((n) => n.class_type === "TextEncodeAceStepAudio1.5");
  assert.equal(enc.inputs.duration, 36, "TextEncodeAceStepAudio1.5.duration gets renderSeconds");
  const lat = Object.values(graph).find((n) => n.class_type === "EmptyAceStep1.5LatentAudio");
  assert.equal(lat.inputs.seconds, 36, "EmptyAceStep1.5LatentAudio.seconds gets renderSeconds");
});

test("buildGraphFromArgs: trim defaults to false — unchanged behavior for an existing caller that doesn't pass it", () => {
  const { pos, flags } = parseArgs(["out.flac", "ambient drone", "--seconds", "30"]);
  const { seconds, renderSeconds } = buildGraphFromArgs(pos, flags);
  assert.equal(seconds, 30);
  assert.equal(renderSeconds, undefined, "no over-render requested = no trim needed downstream");
  const { graph } = buildGraphFromArgs(pos, flags);
  const lat = Object.values(graph).find((n) => n.class_type === "EmptyAceStep1.5LatentAudio");
  assert.equal(lat.inputs.seconds, 30, "graph is built at the requested seconds, exactly as before this fix");
});

test("buildGraphFromArgs: --graph passthrough ignores trim:true (its duration is opaque to this function)", () => {
  const customGraph = { "1": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "x.safetensors" } } };
  const gf = join(mkdtempSync(join(tmpdir(), "music-graph-trim-")), "wf.json");
  writeFileSync(gf, JSON.stringify(customGraph));
  const { graph, seconds, renderSeconds } = buildGraphFromArgs(["out.flac"], { graph: gf, seed: "5" }, { trim: true });
  assert.deepEqual(graph, customGraph);
  assert.equal(seconds, undefined, "a --graph passthrough never reports a requested seconds");
  assert.equal(renderSeconds, undefined, "and so never triggers a downstream trim");
});

// ---- cleanupFailedDeadAirOutput (R1 follow-up, 2026-09-23) ----------------------
// renderOnce writes ComfyUI's raw SaveAudio bytes (always FLAC) straight to `out`;
// only normalizeLoudness transcodes to match the requested extension, and it never
// runs once dead air persists after the retry. Before this fix, a persistent-dead-
// air music request with a "*.wav" out path left FLAC bytes under that .wav name on
// disk after the DEAD_AIR defer — reproduced R1, 2026-09-23. generate() now calls
// this before throwing; these tests pin the cleanup decision itself.
test("cleanupFailedDeadAirOutput: removes the stray file left by a failed render", () => {
  const dir = mkdtempSync(join(tmpdir(), "music-deadair-"));
  const p = join(dir, "out.wav");
  writeFileSync(p, "flac-bytes-under-a-wav-name");
  assert.equal(existsSync(p), true, "fixture file must exist before cleanup");
  cleanupFailedDeadAirOutput(p);
  assert.equal(existsSync(p), false, "the stray mismatched-container file must be gone");
});

test("cleanupFailedDeadAirOutput: best-effort — an unlink failure never throws (the DEAD_AIR error matters more)", () => {
  assert.doesNotThrow(() => {
    cleanupFailedDeadAirOutput("whatever.wav", { unlink: () => { throw new Error("EPERM: simulated"); } });
  });
});

test("cleanupFailedDeadAirOutput: calls the injected unlink with the exact output path", () => {
  let called = null;
  cleanupFailedDeadAirOutput("D:/media/music-abc123.wav", { unlink: (p) => { called = p; } });
  assert.equal(called, "D:/media/music-abc123.wav");
});
