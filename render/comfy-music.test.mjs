// node --test render/comfy-music.test.mjs
// Tests comfy-music.mjs arg parsing + ACE-Step graph building with ComfyUI STUBBED —
// no live render, no network, no GPU. Mirrors the seams comfy-video.mjs uses: the
// worker exports pure parseArgs()/buildGraphFromArgs() and only runs withGpuSlot when
// invoked as the main module (so importing it here has no side effects).
import { test } from "node:test";
import assert from "node:assert";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs, buildGraphFromArgs, RESERVE_VRAM_DEFAULT } from "./comfy-music.mjs";

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
