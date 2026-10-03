// node --test render/audiocpp-generate.test.mjs
// audiocpp-generate.mjs: the audiocpp_cli argv for voice (tts / clon) and music (gen) and
// the no-CPU refusal at the script's own door. No spawn of a real engine.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { buildAudiocppArgs } from "./audiocpp-generate.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, "audiocpp-generate.mjs");

test("voice: --task tts with the chatterbox family, language and seed", () => {
  const a = buildAudiocppArgs({
    kind: "voice", outFile: "/t/o.wav", text: "hola mundo",
    flags: { family: "chatterbox", model: "/m/chatterbox-q8_0.gguf", backend: "vulkan", device: "0", lang: "es", seed: "5" },
  });
  const at = (k) => a[a.indexOf(k) + 1];
  assert.equal(at("--task"), "tts");
  assert.equal(at("--family"), "chatterbox");
  assert.equal(at("--model"), "/m/chatterbox-q8_0.gguf");
  assert.equal(at("--backend"), "vulkan");
  assert.equal(at("--device"), "0");
  assert.equal(at("--text"), "hola mundo");
  assert.equal(at("--language"), "es");
  assert.equal(at("--seed"), "5");
  assert.ok(!a.includes("--voice-ref"));
  assert.ok(a.includes("--metrics") && a.includes("--log"), "--log feeds the CPU-placement guard");
  assert.equal(a[a.length - 2], "--out");
  assert.equal(a[a.length - 1], "/t/o.wav");
});

test("voice with a clone reference: --task clon (the CLI enum, not the spec word clone) and --voice-ref", () => {
  const a = buildAudiocppArgs({
    kind: "voice", outFile: "o.wav", text: "t",
    flags: { family: "chatterbox", model: "m", backend: "vulkan", clone: "/ref/me.wav" },
  });
  assert.equal(a[a.indexOf("--task") + 1], "clon");
  assert.equal(a[a.indexOf("--voice-ref") + 1], "/ref/me.wav");
  assert.equal(a[a.indexOf("--language") + 1], "es", "the house default language");
  assert.equal(a[a.indexOf("--device") + 1], "0", "device defaults to 0");
});

test("music: --task gen with the ace_step family, lyrics and --duration-seconds; no language, no voice-ref", () => {
  const a = buildAudiocppArgs({
    kind: "music", outFile: "o.wav", text: "warm lo-fi piano bed",
    flags: { family: "ace_step", model: "/m/ace-step-1.5-turbo-bf16.gguf", backend: "vulkan", device: "1", seconds: "30", lyrics: "la la", clone: "/ignored.wav" },
    extra: ["--max-steps", "8"],
  });
  const at = (k) => a[a.indexOf(k) + 1];
  assert.equal(at("--task"), "gen");
  assert.equal(at("--family"), "ace_step");
  assert.equal(at("--text"), "warm lo-fi piano bed");
  assert.equal(at("--lyrics"), "la la");
  assert.equal(at("--duration-seconds"), "30");
  assert.equal(at("--device"), "1");
  assert.ok(!a.includes("--language") && !a.includes("--voice-ref"));
  assert.ok(!a.includes("--seed"), "no seed when none is given");
  assert.ok(a.indexOf("--max-steps") > a.indexOf("--log"), "extra args follow the script's own flags");
  assert.equal(a[a.length - 1], "o.wav");
});

test("an unknown kind throws", () => {
  assert.throws(() => buildAudiocppArgs({ kind: "sfx", outFile: "o", text: "t", flags: {} }), /voice or music/);
});

function run(args) {
  return spawnSync(process.execPath, [script, ...args], { encoding: "utf8", timeout: 30000 });
}

test("the script refuses a cpu backend before touching anything (exit 1, CPU_BACKEND_REFUSED)", () => {
  for (const kind of ["voice", "music"]) {
    const r = run(["--kind", kind, "o.wav", "t", "--bin", "x", "--family", "f", "--model", "x", "--backend", "cpu", "--no-lock"]);
    assert.equal(r.status, 1, `${kind}: ${r.stderr}`);
    assert.match(r.stderr, /CPU_BACKEND_REFUSED/);
  }
});

test("the script reports a bad kind and missing flags with exit 2", () => {
  assert.equal(run(["--kind", "sfx", "o.wav", "t"]).status, 2);
  const r = run(["--kind", "voice", "o.wav", "t", "--bin", "x", "--family", "f", "--model", "x"]);
  assert.equal(r.status, 2);
  assert.match(r.stderr, /--backend/);
});

// finalizeAudio drives the real ffmpeg; the tests skip where none resolves.
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { finalizeAudio } from "./audiocpp-generate.mjs";
import { resolveFfmpeg, resolveFfprobe } from "./audio-qa.mjs";

const ffmpeg = resolveFfmpeg();
const ffprobe = ffmpeg ? resolveFfprobe(ffmpeg) : "";
const probe = (f) => spawnSync(ffprobe, ["-v", "error", "-show_entries", "stream=codec_name,sample_rate", "-of", "csv=p=0", f], { encoding: "utf8" }).stdout.trim();

test("finalizeAudio: music is normalized and delivered at 48 kHz in the extension asked for (flac)", { skip: !ffmpeg || !ffprobe }, () => {
  const dir = mkdtempSync(join(tmpdir(), "fa-"));
  try {
    const wav = join(dir, "in.wav");
    assert.equal(spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", wav]).status, 0);
    const out = join(dir, "bed.flac");
    assert.equal(finalizeAudio({ ffmpeg, wav, out, normalize: true, workDir: dir }), "normalized");
    assert.equal(probe(out), "flac,48000");
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("finalizeAudio: voice to a .wav is the engine's file untouched; to another extension it is re-encoded, not renamed", { skip: !ffmpeg || !ffprobe }, () => {
  const dir = mkdtempSync(join(tmpdir(), "fa-"));
  try {
    const wav = join(dir, "in.wav");
    assert.equal(spawnSync(ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", wav]).status, 0);
    const same = join(dir, "v.wav");
    assert.equal(finalizeAudio({ ffmpeg, wav, out: same, normalize: false, workDir: dir }), "copied");
    assert.ok(readFileSync(same).equals(readFileSync(wav)));
    const flac = join(dir, "v.flac");
    assert.equal(finalizeAudio({ ffmpeg, wav, out: flac, normalize: false, workDir: dir }), "converted");
    assert.match(probe(flac), /^flac,/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("finalizeAudio: with no ffmpeg the engine's wav is delivered as it is", () => {
  const dir = mkdtempSync(join(tmpdir(), "fa-"));
  try {
    const wav = join(dir, "in.wav");
    writeFileSync(wav, "RIFFfake");
    const out = join(dir, "o.wav");
    assert.equal(finalizeAudio({ ffmpeg: "", wav, out, normalize: true, workDir: dir }), "copied");
    assert.ok(existsSync(out));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("the script refuses extra args that change the backend or device (EXTRA_ARGS_REFUSED, exit 1) before any spawn", () => {
  for (const extra of [["--backend", "cpu"], ["--device", "1"], ["--device=1"], ["--backend=vulkan"]]) {
    const r = run(["--kind", "music", "o.wav", "t", "--bin", "/no/bin", "--family", "f", "--model", "/no/m", "--backend", "vulkan", "--no-lock",
      "--extra-args", JSON.stringify(extra)]);
    assert.equal(r.status, 1, `${extra}: ${r.stderr}`);
    assert.match(r.stderr, /EXTRA_ARGS_REFUSED/);
    assert.doesNotMatch(r.stderr, /not found/);
  }
});
