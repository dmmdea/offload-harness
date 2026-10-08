// node --test render/audiocpp-generate.test.mjs
// audiocpp-generate.mjs: the audiocpp_cli argv for voice (tts / clon) and music (gen) and
// the no-CPU refusal at the script's own door. No spawn of a real engine.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { buildAudiocppArgs, parseArgs } from "./audiocpp-generate.mjs";

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

// ---------------------------------------------------------------- finalizeAudio (stubbed ffmpeg)
// finalizeAudio takes an injectable `run` / `measureFn` / `durationFn`, so every decision is
// tested against captured-shape tool results without spawning ffmpeg; the real chain is run
// end to end by igpu-runners-main.test.mjs.
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync, readdirSync } from "node:fs";
import { extname } from "node:path";
import { tmpdir } from "node:os";
import {
  finalizeAudio, gateDeadAir, buildTrimTailArgs, buildMasterArgs, buildConvertArgs, fadeSeconds,
  refuseAudioBackend, refuseAudioDevice, AUDIO_BACKENDS, TAIL_SILENCE_DB,
} from "./audiocpp-generate.mjs";

function work() {
  const dir = mkdtempSync(join(tmpdir(), "fa-"));
  const wav = join(dir, "in.wav");
  writeFileSync(wav, "RIFFengine-wav");
  return { dir, wav, out: (n) => join(dir, n), done: () => rmSync(dir, { recursive: true, force: true }) };
}
// a stub ffmpeg: records the argv and writes the LAST argument (the destination) unless told to fail
function stubRun({ failAt = -1, status = 1, stderr = "boom", noWrite = false } = {}) {
  const calls = [];
  const run = (cmd, args) => {
    const n = calls.length;
    calls.push({ cmd, args });
    if (n === failAt) return { status, stderr };
    if (!noWrite) writeFileSync(args[args.length - 1], `stub-output-${n}`);
    return { status: 0, stderr: "" };
  };
  return { run, calls };
}
const CLEAN = { duration: 26.6, silences: [], integratedLUFS: -14, truePeakDBFS: -1.2, exitStatus: 0, exitSignal: null };
const base = { ffmpeg: "ffmpeg", ffprobe: "ffprobe", measureFn: () => CLEAN, durationFn: (_p, f) => (f.endsWith("trimmed.wav") ? 26.55 : 30), log: () => {} };

test("finalizeAudio music: trim the trailing silence below -45 dB, then fade-out + loudnorm -14 LUFS / -1 dBTP at 48 kHz, then the dead-air gate", () => {
  const w = work();
  try {
    const { run, calls } = stubRun();
    const logged = [];
    const r = finalizeAudio({ ...base, wav: w.wav, out: w.out("bed.wav"), kind: "music", workDir: w.dir, run, log: (m) => logged.push(m) });
    assert.equal(r.did, "mastered");
    assert.ok(Math.abs(r.trimmedSec - 3.45) < 0.01);
    assert.match(logged.join("\n"), /trimmed 3\.45s of trailing silence/);
    assert.equal(calls.length, 2);
    const [trim, master] = calls.map((c) => c.args.join(" "));
    assert.match(trim, /areverse,silenceremove=start_periods=1:start_threshold=-45dB:start_silence=0\.15,areverse/);
    assert.equal(TAIL_SILENCE_DB, -45);
    assert.match(master, /afade=t=out:st=25\.550:d=1\.000/);
    assert.match(master, /loudnorm=I=-14:TP=-1:LRA=11/);
    assert.match(master, /-ar 48000/);
    assert.ok(existsSync(w.out("bed.wav")));
  } finally { w.done(); }
});

test("finalizeAudio: any ffmpeg failure is an error carrying its stderr tail, never a silent copy of the raw engine file (G22)", () => {
  for (const [kind, ext, failAt] of [["music", "wav", 0], ["music", "flac", 1], ["voice", "flac", 0]]) {
    const w = work();
    try {
      const { run } = stubRun({ failAt, stderr: "Error while filtering: Invalid argument" });
      assert.throws(() => finalizeAudio({ ...base, wav: w.wav, out: w.out(`o.${ext}`), kind, workDir: w.dir, run }),
        /failed \(exit 1\): Error while filtering: Invalid argument/, `${kind}.${ext} step ${failAt}`);
      assert.ok(!existsSync(w.out(`o.${ext}`)), "nothing is delivered after a failed step");
    } finally { w.done(); }
  }
});

test("finalizeAudio: an ffmpeg that exits 0 but wrote nothing, and one that cannot start, are errors too", () => {
  const w = work();
  try {
    assert.throws(() => finalizeAudio({ ...base, wav: w.wav, out: w.out("o.flac"), kind: "voice", workDir: w.dir, run: stubRun({ noWrite: true }).run }), /wrote no/);
    const enoent = () => ({ error: Object.assign(new Error("spawn ffmpeg ENOENT"), { code: "ENOENT" }) });
    assert.throws(() => finalizeAudio({ ...base, wav: w.wav, out: w.out("o.flac"), kind: "voice", workDir: w.dir, run: enoent }), /ffmpeg re-encode failed: spawn ffmpeg ENOENT/);
  } finally { w.done(); }
});

test("finalizeAudio: with no ffmpeg or ffprobe it throws FFMPEG_UNAVAILABLE and the raw wav is NOT copied out as a success", () => {
  const w = work();
  try {
    for (const miss of [{ ffmpeg: "" }, { ffprobe: "" }]) {
      assert.throws(() => finalizeAudio({ ...base, ...miss, wav: w.wav, out: w.out("o.wav"), kind: "music", workDir: w.dir, run: stubRun().run }), /FFMPEG_UNAVAILABLE/);
      assert.ok(!existsSync(w.out("o.wav")));
    }
  } finally { w.done(); }
});

test("finalizeAudio voice: a .wav is the engine's file byte for byte (no ffmpeg call); another extension is re-encoded, not renamed", () => {
  const w = work();
  try {
    const wavRun = stubRun();
    const r = finalizeAudio({ ...base, wav: w.wav, out: w.out("v.wav"), kind: "voice", workDir: w.dir, run: wavRun.run });
    assert.equal(r.did, "copied");
    assert.equal(wavRun.calls.length, 0);
    assert.ok(readFileSync(w.out("v.wav")).equals(readFileSync(w.wav)));
    const flacRun = stubRun();
    const f = finalizeAudio({ ...base, wav: w.wav, out: w.out("v.flac"), kind: "voice", workDir: w.dir, run: flacRun.run });
    assert.equal(f.did, "converted");
    assert.equal(flacRun.calls.length, 1);
    // ffmpeg writes a PARTIAL beside the delivery path (same directory, same extension), which is then
    // renamed onto it - never the delivery path itself
    const dst = flacRun.calls[0].args[flacRun.calls[0].args.length - 1];
    assert.equal(dirname(dst), dirname(w.out("v.flac")));
    assert.ok(dst !== w.out("v.flac") && dst.endsWith(".flac") && dst.includes(".part."), dst);
    assert.ok(!existsSync(dst), "the partial is gone once it has been renamed onto the result");
    assert.equal(readFileSync(w.out("v.flac"), "utf8"), "stub-output-0", "ffmpeg's output, not the raw wav bytes under a .flac name");
  } finally { w.done(); }
});

test("finalizeAudio: the dead-air gate runs on the DELIVERED file for voice and music; a silent or unmeasurable render is DEAD_AIR and the file is removed (G26)", () => {
  const silent = { duration: 20, silences: [{ start: 0, end: 20, duration: 20 }], integratedLUFS: -70, truePeakDBFS: -70, exitStatus: 0, exitSignal: null };
  for (const [kind, out] of [["voice", "v.wav"], ["music", "m.wav"]]) {
    const w = work();
    try {
      const seen = [];
      assert.throws(() => finalizeAudio({ ...base, wav: w.wav, out: w.out(out), kind, workDir: w.dir, run: stubRun().run, measureFn: (_f, _p, file) => { seen.push(file); return silent; } }), /DEAD_AIR: .*silence/);
      assert.equal(seen.length, 1);
      assert.equal(dirname(seen[0]), dirname(w.out(out)), "measured on the file that would be delivered (the partial beside the result)");
      assert.ok(seen[0].endsWith(extname(out)) && seen[0].includes(".part."), seen[0]);
      assert.ok(!existsSync(seen[0]), "the rejected partial is removed");
      assert.ok(!existsSync(w.out(out)), `${kind}: a failed gate removes what it rejects`);
      assert.throws(() => finalizeAudio({ ...base, wav: w.wav, out: w.out(out), kind, workDir: w.dir, run: stubRun().run, measureFn: () => null }), /DEAD_AIR: the delivered audio could not be measured/);
    } finally { w.done(); }
  }
  const w = work();
  try {
    // the trim removed everything: the whole render was silence
    assert.throws(() => finalizeAudio({ ...base, wav: w.wav, out: w.out("m.wav"), kind: "music", workDir: w.dir, run: stubRun().run, durationFn: (_p, f) => (f.endsWith("trimmed.wav") ? 0 : 30) }), /DEAD_AIR: the trailing-silence trim left no audio/);
  } finally { w.done(); }
});

test("gateDeadAir: a clean measurement passes and returns the verdict", () => {
  assert.equal(gateDeadAir({ ffmpeg: "f", ffprobe: "p", file: "x.wav", measureFn: () => CLEAN }).deadAir, false);
});

test("the ffmpeg argv builders: trim, master and convert", () => {
  const t = buildTrimTailArgs({ src: "a.wav", dst: "t.wav" });
  assert.equal(t[t.indexOf("-i") + 1], "a.wav");
  assert.equal(t[t.length - 1], "t.wav");
  assert.equal(t[t.indexOf("-c:a") + 1], "pcm_f32le", "float PCM between the steps: nothing is quantized twice");
  assert.equal(fadeSeconds(30), 1);
  assert.equal(fadeSeconds(2), 0.2);
  assert.equal(fadeSeconds(0.1), 0.05);
  const m = buildMasterArgs({ src: "t.wav", dst: "o.flac", duration: 10 });
  assert.match(m.join(" "), /afade=t=out:st=9\.000:d=1\.000,loudnorm/);
  assert.deepEqual(buildConvertArgs({ src: "a.wav", dst: "b.flac" }).slice(-3), ["-ar", "48000", "b.flac"]);
});

test("refuseAudioBackend: audio.cpp's own values only; vulkan0 is sd.cpp's spelling and is refused with the --device hint; cpu and best are refused", () => {
  for (const b of ["vulkan", "Vulkan"]) assert.equal(refuseAudioBackend(b), b.toLowerCase());
  assert.deepEqual(AUDIO_BACKENDS, ["vulkan"], "SIL13: the evidence guard only recognises Vulkan buffers");
  // cuda / hip / rocm / metal would read CONFIGURED and end every call CPU_PLACEMENT "no GPU evidence"
  for (const b of ["vulkan0", "vulkan1", "cpu", "best", "auto", "", undefined, "blas", "vulcan", "cuda", "hip", "rocm", "metal"]) {
    assert.throws(() => refuseAudioBackend(b), /CPU_BACKEND_REFUSED/, String(b));
  }
  assert.throws(() => refuseAudioBackend("vulkan1"), /--backend vulkan --device 1/);
  assert.equal(refuseAudioDevice(undefined), "0");
  assert.equal(refuseAudioDevice("2"), "2");
  // SIL13: a bad device is its own class (DEVICE_INVALID), not a backend refusal
  for (const d of ["-1", "x", "0.5", "vulkan0"]) {
    assert.throws(() => refuseAudioDevice(d), (e) => /^DEVICE_INVALID:/.test(e.message) && /device index/.test(e.message) && !/CPU_BACKEND_REFUSED/.test(e.message), d);
  }
});

test("the -- terminator: lyrics or text that start with -- stay positional", () => {
  const { pos, flags } = parseArgs(["--kind", "music", "--", "o.wav", "--- Intro ---"]);
  assert.deepEqual(pos, ["o.wav", "--- Intro ---"]);
  assert.equal(flags.kind, "music");
});

test("the script refuses vulkan0 as an audio.cpp backend (exit 1, CPU_BACKEND_REFUSED naming --device) and a bare binary name", () => {
  const r = run(["--kind", "music", "o.wav", "t", "--bin", "x", "--family", "f", "--model", "x", "--backend", "vulkan0", "--no-lock"]);
  assert.equal(r.status, 1);
  assert.match(r.stderr, /CPU_BACKEND_REFUSED/);
  assert.match(r.stderr, /--device 0/);
  const bare = run(["--kind", "music", "o.wav", "t", "--bin", "audiocpp_cli", "--family", "f", "--model", "x", "--backend", "vulkan", "--no-lock"]);
  assert.equal(bare.status, 1);
  assert.match(bare.stderr, /BINARY_NOT_ABSOLUTE/);
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
