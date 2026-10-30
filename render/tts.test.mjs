// node --test render/tts.test.mjs
// Tests tts.mjs arg parsing + worker selection + worker-arg building with NO spawn / GPU.
// tts.mjs exports pure functions and only runs withGpuSlot as the main module (importing
// it here is side-effect-free), mirroring comfy-music.mjs / wf-*.mjs.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { parseArgs, selectWorker, buildWorkerArgs, parseCli, SAMPLING_FLAGS, GENERALIST_WORKER, FT_WORKER } from "./tts.mjs";
import { scratchDir } from "./test-scratch.mjs";
import { isolatedRunnerEnv } from "./test-isolation.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));

// The workers' argv carries every value in the SAME token as its flag (`--text=hola`): a separate value token is read by
// argparse as an option when it opens with a dash and has no space ("---", "-x", "-1e-2" on Python 3.11), and the worker
// then exits 2 ("expected one argument") after the lock was taken. argOf reads a value back out of such an argv.
const argOf = (args, k) => {
  const tok = args.find((a) => a.startsWith("--" + k + "="));
  return tok === undefined ? undefined : tok.slice(k.length + 3);
};

test("parseArgs: positionals + engine/model/base-dir/recipe flags", () => {
  const { pos, flags } = parseArgs(["out.wav", "hola",
    "--engine", "finetuned", "--model", "/m/x.safetensors", "--base-dir", "/m/base", "--temperature", "0.6"]);
  assert.equal(pos[0], "out.wav");
  assert.equal(pos[1], "hola");
  assert.equal(flags.engine, "finetuned");
  assert.equal(flags.model, "/m/x.safetensors");
  assert.equal(flags["base-dir"], "/m/base");
  assert.equal(flags.temperature, "0.6");
});

test("selectWorker: absent/generalist -> stock worker; finetuned -> ft worker", () => {
  assert.equal(selectWorker({}), GENERALIST_WORKER);
  assert.equal(selectWorker({ engine: "generalist" }), GENERALIST_WORKER);
  assert.equal(selectWorker({ engine: "finetuned" }), FT_WORKER);
});

test("buildWorkerArgs: with no sampling flag given, generalist threads out/text/lang/ref only", () => {
  const args = buildWorkerArgs("out.wav", "hola", { clone: "/r/ref.wav" }, {});
  assert.deepEqual(args, ["--out=out.wav", "--text=hola", "--lang=es", "--ref=/r/ref.wav"]);
});

test("buildWorkerArgs: finetuned threads --model/--base-dir + recipe kwargs", () => {
  const args = buildWorkerArgs("out.wav", "hola",
    { engine: "finetuned", model: "/m/x.safetensors", "base-dir": "/m/base", clone: "/r/dan.wav",
      temperature: "0.6", "cfg-weight": "0.5" }, {});
  assert.equal(argOf(args, "model"), "/m/x.safetensors");
  assert.equal(argOf(args, "base-dir"), "/m/base");
  assert.equal(argOf(args, "temperature"), "0.6");
  assert.equal(argOf(args, "cfg-weight"), "0.5");
  assert.equal(argOf(args, "ref"), "/r/dan.wav");
});

test("buildWorkerArgs: lang defaults es; TTS_REF env used when no --clone", () => {
  const args = buildWorkerArgs("out.wav", "hola", {}, { TTS_REF: "/r/env.wav" });
  assert.equal(argOf(args, "lang"), "es");
  assert.equal(argOf(args, "ref"), "/r/env.wav");
});

test("buildWorkerArgs: every value travels in the same token as its flag, so no value can be read as an option", () => {
  const all = buildWorkerArgs("-o.wav", "---",
    { engine: "finetuned", model: "-m.safetensors", "base-dir": "-b", clone: "-ref.wav", lang: "-es", ...SIX }, {});
  assert.deepEqual(all.map((a) => a.slice(0, a.indexOf("=") + 1)),
    ["--out=", "--text=", "--lang=", "--ref=", "--model=", "--base-dir=", ...SIX_NAMES.map((k) => "--" + k + "=")]);
  assert.equal(argOf(all, "out"), "-o.wav");
  assert.equal(argOf(all, "text"), "---");
  assert.equal(argOf(all, "lang"), "-es");
  assert.equal(argOf(all, "ref"), "-ref.wav");
  assert.equal(argOf(all, "model"), "-m.safetensors");
  assert.equal(argOf(all, "base-dir"), "-b");
  // a value that itself holds an equals sign or spaces stays one token, and is read back whole
  const odd = buildWorkerArgs("o.wav", "a = b -- c", { clone: "/r/x=y.wav" }, {});
  assert.equal(argOf(odd, "text"), "a = b -- c");
  assert.equal(argOf(odd, "ref"), "/r/x=y.wav");
});

test("tts_chatterbox_ft.py skeleton: validates + defers (exit 3 + marker), stdlib-only", () => {
  const py = process.env.TTS_PY || "python";
  const worker = join(dirname(fileURLToPath(import.meta.url)), "tts_chatterbox_ft.py");
  const r = spawnSync(py, [worker, "--out", "x.wav", "--text", "hola",
    "--model", "/no/such/model.safetensors", "--base-dir", "/no/such/dir"], { encoding: "utf8", windowsHide: true });
  if (r.error && r.error.code === "ENOENT") return; // no python on PATH — skip
  assert.equal(r.status, 3, "skeleton must exit 3 (defer signal)");
  assert.ok((r.stderr || "").includes("FT_ENGINE_NOT_VENDORED"), "must emit the distinct marker");
});

// ---- the sampling flags reach BOTH engines (0.185.1) ------------------------------------------------------
// Until 0.185.1 tts.mjs forwarded --temperature / --cfg-weight / ... only to the fine-tuned worker; the generalist
// worker had no argparse entries for them and never passed them to generate(), so a documented flag changed
// nothing, and a flag the launcher did not know was dropped without a word.

// the six keyword arguments of ChatterboxMultilingualTTS.generate(), pinned here so a list that loses one in
// tts.mjs cannot also lose it from the test
const SIX_NAMES = ["temperature", "cfg-weight", "exaggeration", "repetition-penalty", "min-p", "top-p"];
const SIX = { temperature: "0.7", "cfg-weight": "0.3", exaggeration: "0.6", "repetition-penalty": "1.5", "min-p": "0.05", "top-p": "0.9" };
const SIX_CLI = SIX_NAMES.flatMap((k) => ["--" + k, SIX[k]]); // as typed on the tts.mjs command line
const SIX_TOKENS = SIX_NAMES.map((k) => "--" + k + "=" + SIX[k]); // as the workers receive them
const BASE = ["out.wav", "hola"];

test("SAMPLING_FLAGS: the six flags generate() takes, in forwarding order", () => {
  assert.deepEqual(SAMPLING_FLAGS, SIX_NAMES);
});

test("buildWorkerArgs: the generalist engine forwards every sampling flag to the worker under the same flag name", () => {
  const want = ["--out=out.wav", "--text=hola", "--lang=es", "--ref=/r/ref.wav", ...SIX_TOKENS];
  assert.deepEqual(buildWorkerArgs("out.wav", "hola", { ...SIX, clone: "/r/ref.wav" }, {}), want);
  assert.deepEqual(buildWorkerArgs("out.wav", "hola", { ...SIX, clone: "/r/ref.wav", engine: "generalist" }, {}), want);
});

test("buildWorkerArgs: both engines forward a sampling flag that is given, and none that is not", () => {
  for (const engine of [undefined, "generalist", "finetuned"]) {
    for (const k of SIX_NAMES) {
      const args = buildWorkerArgs("o.wav", "t", { ...(engine ? { engine } : {}), [k]: "0.25" }, {});
      assert.equal(argOf(args, k), "0.25", `${engine || "default"} engine drops --${k}`);
      assert.equal(args.filter((a) => SIX_NAMES.some((n) => a.startsWith("--" + n + "="))).length, 1,
        `${engine || "default"} engine forwards a sampling flag nobody gave, next to --${k}`);
    }
  }
});

test("buildWorkerArgs: the finetuned argv carries the same flags (model, base-dir, then the six)", () => {
  const args = buildWorkerArgs("out.wav", "hola",
    { ...SIX, engine: "finetuned", model: "/m/x.safetensors", "base-dir": "/m/base", clone: "/r/dan.wav", lang: "en" }, {});
  assert.deepEqual(args, ["--out=out.wav", "--text=hola", "--lang=en", "--ref=/r/dan.wav",
    "--model=/m/x.safetensors", "--base-dir=/m/base", ...SIX_TOKENS]);
});

test("parseCli: a sampling value that is not a finite number refuses, naming the flag, for either engine", () => {
  for (const engine of [[], ["--engine", "finetuned", "--model", "/m/x", "--base-dir", "/m/b"]]) {
    for (const k of SIX_NAMES) {
      for (const bad of ["abc", "", " ", "NaN", "Infinity", "-Infinity", "1e999", "-1e999", "0x10", "1,5", "0.5x", "--lang", "1_0", "- 1", "1e", "e1", "--5"]) {
        const r = parseCli([...BASE, ...engine, "--" + k, bad]);
        assert.equal(r.ok, false, `--${k} ${JSON.stringify(bad)} (${engine[1] || "default engine"}) was accepted`);
        assert.ok(r.error.includes("--" + k), `the refusal for --${k} ${JSON.stringify(bad)} must name the flag: ${r.error}`);
      }
    }
  }
});

test("parseCli: finite numbers in every spelling the worker's float() reads are accepted and forwarded verbatim", () => {
  // the negative ones with an exponent or a trailing dot (-1e-2, -5., -1E3, -5.e2, -.5e1) are read by Python 3.11's argparse as a
  // FLAG when they are a separate token; they travel as `--cfg-weight=-1e-2`, which argparse always reads as a value
  for (const good of ["0.3", "1", "0", "-1", "-0.5", "-.5", ".5", "5.", "1e-2", "+2", "2E3", "-1e-2", "-5.", "-1E3", "-5.e2", "-.5e1", "+.5E-3", "-0"]) {
    const r = parseCli([...BASE, "--cfg-weight", good]);
    assert.equal(r.ok, true, `${good}: ${r.error}`);
    assert.equal(argOf(r.args, "cfg-weight"), good);
  }
});

test("parseCli: a value flag with no value refuses, naming the flag (it used to be dropped without a word)", () => {
  for (const k of ["clone", "lang", "engine", "model", "base-dir", ...SIX_NAMES]) {
    const last = parseCli([...BASE, "--" + k]);
    assert.equal(last.ok, false, `--${k} as the last token was accepted`);
    assert.ok(last.error.includes("--" + k), last.error);
    const swallowed = parseCli([...BASE, "--" + k, "--no-lock"]);
    assert.equal(swallowed.ok, false, `--${k} followed by a flag swallowed that flag as its value`);
    assert.ok(swallowed.error.includes("--" + k), swallowed.error);
    // an empty value (an unset shell variable in `--clone "$REF"`) is no value
    const empty = parseCli([...BASE, "--" + k, ""]);
    assert.equal(empty.ok, false, `--${k} "" was accepted`);
    assert.ok(empty.error.includes("--" + k), empty.error);
  }
});

test("parseCli: an unknown or misspelled flag refuses, naming it", () => {
  for (const bad of ["--bogus", "--cfg_weight", "--temprature", "--seed", "--cfgweight", "--repetition_penalty", "--topp", "--language", "--__proto__", "--constructor"]) {
    const r = parseCli([...BASE, bad, "0.3"]);
    assert.equal(r.ok, false, `${bad} was accepted`);
    assert.ok(r.error.includes(bad), `the refusal must name ${bad}: ${r.error}`);
    // it is judged before the positionals, so a flag in front of the text is not read as a missing text
    const first = parseCli(["out.wav", bad, "0.3", "hola"]);
    assert.equal(first.ok, false);
    assert.ok(first.error.includes(bad), first.error);
  }
  const eq = parseCli([...BASE, "--cfg-weight=0.3"]);
  assert.equal(eq.ok, false, "--flag=value is not a spelling this CLI reads");
  assert.ok(eq.error.includes("--cfg-weight=0.3"), eq.error);
});

test("parseCli: --model and --base-dir belong to --engine finetuned; the generalist would drop them, so they refuse", () => {
  for (const k of ["model", "base-dir"]) {
    for (const engine of [[], ["--engine", "generalist"]]) {
      const r = parseCli([...BASE, ...engine, "--" + k, "/m/x"]);
      assert.equal(r.ok, false, `--${k} with ${engine[1] || "the default engine"} was accepted and then ignored`);
      assert.ok(r.error.includes("--" + k), r.error);
    }
  }
  const both = parseCli([...BASE, "--engine", "finetuned", "--model", "/m/x", "--base-dir", "/m/b"]);
  assert.equal(both.ok, true, `--model and --base-dir with finetuned: ${both.error}`);
});

// tts_chatterbox_ft.py requires both (argparse `required=True`): without them it exits 2 after the lock was taken and
// llama-swap was drained, which the Go wrapper reports as a failure, not as the clean defer (exit 3) of an engine that
// is not vendored. The launcher refuses it first, naming the missing flag. (The Go voice lane always sends both.)
test("parseCli: --engine finetuned needs --model and --base-dir, and says which one is missing", () => {
  for (const [given, missing] of [[[], "--model"], [["--base-dir", "/m/b"], "--model"], [["--model", "/m/x"], "--base-dir"]]) {
    const r = parseCli([...BASE, "--engine", "finetuned", ...given]);
    assert.equal(r.ok, false, `finetuned with ${given.join(" ") || "neither --model nor --base-dir"} was accepted; the worker would exit 2 after the lock`);
    // the message always names both flags in its explanation, so it is the "needs <flag>" part that must be the missing one
    assert.ok(r.error.includes(`needs ${missing} `), `the refusal must say it needs ${missing}: ${r.error}`);
  }
});

test("parseCli: --flag=value names the spelling to use, and --no-lock=1 says it takes no value", () => {
  assert.match(parseCli([...BASE, "--cfg-weight=0.3"]).error, /--cfg-weight 0\.3/);
  assert.match(parseCli([...BASE, "--no-lock=1"]).error, /--no-lock takes no value/);
  assert.doesNotMatch(parseCli([...BASE, "--bogus=1"]).error, /takes its value|takes no value/, "no spelling hint for a flag that does not exist");
});

// A text that opens with dashes reaches tts.mjs from the harness: `generate-audio out.wav -- "--- Intro ---"` (CT-52) keeps the text, and
// pipeline.go puts it second, before the flags. A token is a flag only when it is shaped like one.
test("parseCli: a text that opens with dashes but is not shaped like a flag is the text", () => {
  for (const text of ["--- Intro ---", "-- hola --", "--Intro--", "---", "--- ", "--1", "-- 2", "-x", "-hola", "-5", "-"]) {
    const r = parseCli(["out.wav", text, "--clone", "/r/ref.wav", "--lang", "es"], {});
    assert.equal(r.ok, true, `${JSON.stringify(text)}: ${r.error}`);
    assert.equal(r.text, text);
    assert.deepEqual(r.args, ["--out=out.wav", `--text=${text}`, "--lang=es", "--ref=/r/ref.wav"]);
  }
});

test("parseCli: a bare -- ends the flags, so even a flag-shaped text can be given", () => {
  const r = parseCli(["--lang", "en", "out.wav", "--", "--hola"], {});
  assert.equal(r.ok, true, r.error);
  assert.equal(r.text, "--hola");
  assert.equal(r.flags.lang, "en");
  assert.equal(parseCli(["out.wav", "--hola"], {}).ok, false, "without the terminator --hola is a flag, and an unknown one");
  const after = parseCli(["out.wav", "--", "hola", "--lang", "en"], {});
  assert.equal(after.ok, false, "everything after the terminator is a positional, --lang included");
  assert.match(after.error, /extra argument/);
});

test("parseCli: --engine takes generalist or finetuned only", () => {
  assert.equal(parseCli([...BASE, "--engine", "generalist"]).ok, true, "generalist");
  const ft = parseCli([...BASE, "--engine", "finetuned", "--model", "/m/x", "--base-dir", "/m/b"]);
  assert.equal(ft.ok, true, `finetuned: ${ft.error}`);
  const r = parseCli([...BASE, "--engine", "finetunned"]);
  assert.equal(r.ok, false, "a misspelled engine silently ran the generalist worker");
  assert.ok(r.error.includes("--engine") && r.error.includes("finetunned"), r.error);
});

test("parseCli: a third positional refuses (an unquoted text used to be cut after its first word)", () => {
  const r = parseCli([...BASE, "mundo"]);
  assert.equal(r.ok, false);
  assert.ok(r.error.includes("mundo"), r.error);
});

test("parseCli: the missing positionals are named; --no-lock is a boolean that swallows nothing", () => {
  assert.equal(parseCli([]).ok, false);
  assert.equal(parseCli(["out.wav"]).ok, false);
  const r = parseCli([...BASE, "--no-lock", "--lang", "en"]);
  assert.equal(r.ok, true, r.error);
  assert.equal(r.flags["no-lock"], true);
  assert.equal(r.flags.lang, "en");
});

test("parseCli: a good command line yields the worker and its argv", () => {
  const r = parseCli([...BASE, "--engine", "finetuned", "--model", "/m/x", "--base-dir", "/m/b", "--top-p", "0.9"], {});
  assert.equal(r.ok, true, r.error);
  assert.equal(r.worker, FT_WORKER);
  assert.deepEqual(r.args, ["--out=out.wav", "--text=hola", "--lang=es", "--model=/m/x", "--base-dir=/m/b", "--top-p=0.9"]);
  assert.equal(parseCli([...BASE], {}).worker, GENERALIST_WORKER);
});

// ---- every flag the harness itself sends is accepted ------------------------------------------------------

// The flags internal/pipeline/pipeline_audio.go puts on a tts.mjs command line: the literals of the voice branch
// of the audio route and of appendVoiceRecipe. Read from the source so a flag added there is judged here.
function goVoiceLaneFlags() {
  const src = readFileSync(join(HERE, "..", "internal", "pipeline", "pipeline_audio.go"), "utf8");
  const from = src.indexOf('// CLI: tts.mjs <out> "<text>"');
  const to = src.indexOf("} else { // music", from);
  const recipe = src.indexOf("func appendVoiceRecipe(");
  const recipeEnd = recipe < 0 ? -1 : src.slice(recipe).search(/\r?\n\}\r?\n/);
  assert.ok(from >= 0 && to > from, "the voice branch of pipeline_audio.go moved: re-anchor goVoiceLaneFlags");
  assert.ok(recipe >= 0 && recipeEnd > 0, "appendVoiceRecipe moved: re-anchor goVoiceLaneFlags");
  const region = src.slice(from, to) + "\n" + src.slice(recipe, recipe + recipeEnd);
  return [...new Set([...region.matchAll(/"--([a-z][a-z-]*)"/g)].map((m) => m[1]))];
}

const valueFor = (k) => (k === "engine" ? "finetuned" : SIX_NAMES.includes(k) ? "0.5" : "x");

test("the Go voice lane: every flag pipeline_audio.go sends to tts.mjs is accepted, alone and together", () => {
  const sent = goVoiceLaneFlags();
  for (const must of ["engine", "model", "base-dir", "clone", "lang", "temperature", "cfg-weight", "exaggeration", "repetition-penalty"]) {
    assert.ok(sent.includes(must), `the scan of pipeline.go lost --${must}: ${sent.join(" ")}`);
  }
  for (const k of sent) {
    // pipeline.go sends --engine finetuned only with --model AND --base-dir (it defers when either is unset), and
    // --model / --base-dir only with it: the finetuned arm carries all three, whichever flag is under test
    const ft = ["--engine", "finetuned", "--model", "/m/x", "--base-dir", "/m/b"];
    const engine = k === "engine" ? ft : ["model", "base-dir"].includes(k) ? ft : [...ft, "--" + k, valueFor(k)];
    const r = parseCli([...BASE, ...engine]);
    assert.equal(r.ok, true, `pipeline.go sends --${k} and tts.mjs refuses it: ${r.error}`);
    if (!["model", "base-dir", "engine"].includes(k)) {
      const bare = parseCli([...BASE, "--" + k, valueFor(k)]);
      assert.equal(bare.ok, true, `pipeline.go sends --${k} on the generalist lane and tts.mjs refuses it: ${bare.error}`);
    }
  }
  const all = parseCli([...BASE, ...sent.flatMap((k) => ["--" + k, valueFor(k)])]);
  assert.equal(all.ok, true, all.error);
});

test("the Go voice lane: the two argv shapes it builds run through parseCli to the workers' argv", () => {
  // generalist: clone + lang
  const g = parseCli(["/media/voice-ab12cd34.wav", "Hola, mundo.", "--clone", "/refs/es.wav", "--lang", "es"], {});
  assert.equal(g.ok, true, g.error);
  assert.equal(g.worker, GENERALIST_WORKER);
  assert.deepEqual(g.args, ["--out=/media/voice-ab12cd34.wav", "--text=Hola, mundo.", "--lang=es", "--ref=/refs/es.wav"]);
  // finetuned: engine, model, base dir, clone, lang, then the recipe appendVoiceRecipe appends
  const f = parseCli(["/media/voice-ab12cd34.wav", "Hola, mundo.", "--engine", "finetuned", "--model", "/m/ft.safetensors", "--base-dir", "/m/base",
    "--clone", "/refs/es.wav", "--lang", "es", "--temperature", "0.6", "--cfg-weight", "0.5", "--exaggeration", "0.4", "--repetition-penalty", "1.2"], {});
  assert.equal(f.ok, true, f.error);
  assert.equal(f.worker, FT_WORKER);
  assert.deepEqual(f.args, ["--out=/media/voice-ab12cd34.wav", "--text=Hola, mundo.", "--lang=es", "--ref=/refs/es.wav",
    "--model=/m/ft.safetensors", "--base-dir=/m/base", "--temperature=0.6", "--cfg-weight=0.5", "--exaggeration=0.4", "--repetition-penalty=1.2"]);
});

// ---- the command line, run as a process --------------------------------------------------------------------

// A refusal happens before the interpreter is resolved, the lock is taken or a worker starts. TTS_PY names a file that
// does not exist, --no-lock takes no lease, and the env is the isolated one (a dead llama-swap, no lease), so a command
// line that is NOT refused cannot reach a model, a GPU or the machine's llama-swap: it ends at the missing interpreter.
function runCli(t, ...args) {
  const d = scratchDir(t, "tts-cli-");
  return spawnSync(process.execPath, [join(HERE, "tts.mjs"), join(d, "out.wav"), ...args], {
    encoding: "utf8", windowsHide: true, timeout: 60000,
    env: isolatedRunnerEnv({ TTS_PY: join(d, "no-such-python") }),
  });
}

test("tts.mjs as a process: an unknown flag exits 2 with a usage line naming it, before any worker", (t) => {
  const r = runCli(t, "hola", "--bogus", "1", "--no-lock");
  assert.equal(r.status, 2, r.stdout + r.stderr);
  assert.match(r.stderr, /--bogus/);
  assert.match(r.stderr, /usage: node tts\.mjs/);
  assert.doesNotMatch(r.stdout, /WROTE/);
});

test("tts.mjs as a process: --engine finetuned without --model exits 2 naming it, before any worker", (t) => {
  const r = runCli(t, "hola", "--engine", "finetuned", "--base-dir", "/m/b", "--no-lock");
  assert.equal(r.status, 2, r.stdout + r.stderr);
  assert.match(r.stderr, /--model/);
  assert.doesNotMatch(r.stdout, /WROTE/);
});

test("tts.mjs as a process: a non-number sampling value exits 2 naming the flag, and the usage line lists all six", (t) => {
  const r = runCli(t, "hola", "--cfg-weight", "abc", "--no-lock");
  assert.equal(r.status, 2, r.stdout + r.stderr);
  assert.match(r.stderr, /--cfg-weight/);
  const usage = r.stderr.split("\n").find((l) => l.startsWith("usage:")) || "";
  for (const k of SIX_NAMES) assert.ok(usage.includes("--" + k), `the usage line leaves out --${k}: ${usage}`);
});

// ---- the python workers, run against stub modules (no torch, no model, no GPU) -------------------------------

function findPython() {
  for (const c of [process.env.TTS_PY, "python3", "python"].filter(Boolean)) {
    const r = spawnSync(c, ["-c", "import sys; print(sys.version_info[0])"], { encoding: "utf8", windowsHide: true });
    if (!r.error && r.status === 0 && r.stdout.trim() === "3") return c;
  }
  return null;
}
const PY = findPython();
const pySkip = !PY && "no python 3 on PATH";

// Stub torch / torchaudio / chatterbox, written beside a driver. Nothing is loaded: generate() has the real
// signature of ChatterboxMultilingualTTS.generate (so a keyword it does not take is a TypeError) and prints the
// keywords it was GIVEN as one `GEN {json}` line. `python -I` ignores PYTHONPATH and keeps the script directory off
// sys.path, so the driver puts the stubs on sys.path itself and runs the real worker with runpy.
function writeStubs(t) {
  const dir = scratchDir(t, "tts-stub-");
  writeFileSync(join(dir, "torch.py"), [
    "class _T:",
    "    def dim(self): return 2",
    "    def unsqueeze(self, i): return self",
    "    def detach(self): return self",
    "    def cpu(self): return self",
    "    def float(self): return self",
    "def is_tensor(x): return True",
    "def as_tensor(x): return x",
    "class cuda:",
    "    @staticmethod",
    "    def is_available(): return False",
    "",
  ].join("\n"));
  writeFileSync(join(dir, "torchaudio.py"), [
    "def save(path, wav, sr):",
    "    with open(path, 'wb') as f:",
    "        f.write(b'RIFFxxxxWAVE')",
    "",
  ].join("\n"));
  mkdirSync(join(dir, "chatterbox"));
  writeFileSync(join(dir, "chatterbox", "__init__.py"), "");
  writeFileSync(join(dir, "chatterbox", "mtl_tts.py"), [
    "import json",
    "from torch import _T",
    "_U = object()",
    "class ChatterboxMultilingualTTS:",
    "    sr = 24000",
    "    @classmethod",
    "    def from_pretrained(cls, device=None): return cls()",
    "    def generate(self, text, language_id, audio_prompt_path=_U, exaggeration=_U, cfg_weight=_U,",
    "                 temperature=_U, repetition_penalty=_U, min_p=_U, top_p=_U):",
    "        given = dict(audio_prompt_path=audio_prompt_path, exaggeration=exaggeration, cfg_weight=cfg_weight,",
    "                     temperature=temperature, repetition_penalty=repetition_penalty, min_p=min_p, top_p=top_p)",
    "        print('GEN ' + json.dumps({'text': text, 'language_id': language_id,",
    "                                   'kwargs': {k: v for k, v in given.items() if v is not _U}}), flush=True)",
    "        return _T()",
    "",
  ].join("\n"));
  writeFileSync(join(dir, "driver.py"), [
    "import runpy, sys",
    "sys.dont_write_bytecode = True",
    "stubs, worker = sys.argv[1], sys.argv[2]",
    "sys.path.insert(0, stubs)",
    "sys.argv = [worker] + sys.argv[3:]",
    "runpy.run_path(worker, run_name='__main__')",
    "",
  ].join("\n"));
  return dir;
}

// Run the real worker script (`worker`, a file in render/) against the stubs. `argvOf(stubsDir)` is the argv after the
// script name; `gen` is the keywords generate() was given (null when it never ran). `inStubs` runs it with the stub
// directory as its working directory (for a relative --out).
function runWorker(t, worker, argvOf, { inStubs = false } = {}) {
  const stubs = writeStubs(t);
  const r = spawnSync(PY, ["-I", "-B", join(stubs, "driver.py"), stubs, join(HERE, worker), ...argvOf(stubs)], {
    encoding: "utf8", windowsHide: true, timeout: 60000, cwd: inStubs ? stubs : undefined,
    env: { ...process.env, TTS_DEVICE: "cpu", HF_HUB_OFFLINE: "1" },
  });
  const m = /^GEN (.*)$/m.exec(r.stdout || "");
  return { status: r.status, stderr: r.stderr, gen: m ? JSON.parse(m[1]) : null, wav: join(stubs, "voice.wav"), dir: stubs };
}
// the real generalist worker, run with exactly the argv tts.mjs builds from the command line `cliOf(stubsDir)`
const viaLauncher = (t, cliOf, opts) => runWorker(t, GENERALIST_WORKER, (d) => {
  const plan = parseCli(cliOf(d), {});
  assert.equal(plan.ok, true, plan.error);
  assert.equal(plan.worker, GENERALIST_WORKER);
  return plan.args;
}, opts);
// the generalist worker with the usual out/text and `extra`; the kwargs generate() got
const generalist = (t, extra) => runWorker(t, GENERALIST_WORKER, (d) => ["--out", join(d, "voice.wav"), "--text", "hola", ...extra]);
const kwargsOf = (t, extra) => {
  const r = generalist(t, extra);
  assert.equal(r.status, 0, r.stderr);
  return r.gen.kwargs;
};

test("tts_chatterbox.py: every sampling flag reaches generate() under the model's keyword name, as a float", { skip: pySkip }, (t) => {
  const r = generalist(t, SIX_CLI); // the worker still reads the separate-token spelling too
  assert.equal(r.status, 0, r.stderr);
  assert.deepStrictEqual(r.gen.kwargs, { exaggeration: 0.6, cfg_weight: 0.3, temperature: 0.7, repetition_penalty: 1.5, min_p: 0.05, top_p: 0.9 });
  assert.equal(readFileSync(r.wav, "utf8"), "RIFFxxxxWAVE", "the wav is still delivered");
});

test("tts_chatterbox.py: with no sampling flag nothing is passed, so the model keeps its own defaults", { skip: pySkip }, (t) => {
  assert.deepStrictEqual(kwargsOf(t, []), {});
  assert.deepStrictEqual(kwargsOf(t, ["--ref", "/r/ref.wav"]), { audio_prompt_path: "/r/ref.wav" });
});

test("tts_chatterbox.py: only the flags given are passed, and a given 0 is passed too", { skip: pySkip }, (t) => {
  assert.deepStrictEqual(kwargsOf(t, ["--cfg-weight", "0.3"]), { cfg_weight: 0.3 });
  assert.deepStrictEqual(kwargsOf(t, ["--top-p", "0.9", "--min-p", "0.1"]), { top_p: 0.9, min_p: 0.1 });
  assert.deepStrictEqual(kwargsOf(t, ["--exaggeration", "0"]), { exaggeration: 0 }, "0 is a value, not an absent flag");
  assert.deepStrictEqual(kwargsOf(t, ["--temperature", "-1"]), { temperature: -1 });
});

test("tts_chatterbox.py: a non-finite or non-numeric sampling value fails in argument parsing (exit 2), before the model loads", { skip: pySkip }, (t) => {
  for (const [flag, bad] of [["--cfg-weight", "abc"], ["--temperature", "nan"], ["--top-p", "inf"], ["--min-p", "1e999"], ["--exaggeration", ""]]) {
    const r = generalist(t, [flag, bad]);
    assert.equal(r.status, 2, `${flag} ${JSON.stringify(bad)}: ${r.stderr}`);
    // refused as a bad VALUE of a flag the worker knows, not as an unrecognized argument
    assert.match(r.stderr, new RegExp(`argument ${flag}: .*finite number`), r.stderr);
    assert.ok(!r.stderr.includes("loading model"), "the model must not load for a refused argument");
    assert.equal(r.gen, null);
  }
});

test("tts.mjs -> tts_chatterbox.py: the argv the launcher builds for the generalist is read by the worker, flag for flag", { skip: pySkip }, (t) => {
  const r = viaLauncher(t, (d) => [join(d, "voice.wav"), "hola", "--clone", "/r/ref.wav", "--lang", "es", ...SIX_CLI]);
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.gen.language_id, "es");
  assert.deepStrictEqual(r.gen.kwargs, { audio_prompt_path: "/r/ref.wav", exaggeration: 0.6, cfg_weight: 0.3, temperature: 0.7, repetition_penalty: 1.5, min_p: 0.05, top_p: 0.9 });
});

// A value that opens with a dash and has no space is an OPTION to argparse when it is a separate argv token ("---", "--Intro--",
// "--1", "-x", "-hola"): the worker exited 2 ("argument --text: expected one argument") after the lock was taken and llama-swap was
// drained, for a command line the launcher had accepted. The launcher hands every value over as `--flag=value`.
test("tts.mjs -> tts_chatterbox.py: a text that opens with a dash and has no space reaches generate() whole", { skip: pySkip }, (t) => {
  for (const text of ["---", "--Intro--", "--1", "-x", "-hola", "-5", "-", "--- Intro ---", "-- hola --", "a=b"]) {
    const r = viaLauncher(t, (d) => [join(d, "voice.wav"), text]);
    assert.equal(r.status, 0, `${JSON.stringify(text)}: ${r.stderr}`);
    assert.equal(r.gen.text, text);
  }
});

test("tts.mjs -> tts_chatterbox.py: a --clone path, a --lang and an out path that open with a dash reach the worker whole", { skip: pySkip }, (t) => {
  const r = viaLauncher(t, () => ["-o.wav", "hola", "--clone", "-ref.wav", "--lang", "-es"], { inStubs: true });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.gen.language_id, "-es");
  assert.deepStrictEqual(r.gen.kwargs, { audio_prompt_path: "-ref.wav" });
  assert.equal(readFileSync(join(r.dir, "-o.wav"), "utf8"), "RIFFxxxxWAVE", "the wav is delivered at the dash-led --out");
});

// Python 3.11's argparse reads "-1e-2", "-5.", "-1E3", "-5.e2" and "-.5e1" as flags when they are a separate token (3.14 reads them
// as values), so the launcher used to refuse them. As `--temperature=-1e-2` every Python reads them, so it accepts them. Run this
// file again with TTS_PY on a 3.11 interpreter to see the difference.
test("tts.mjs -> tts_chatterbox.py: a negative sampling value in any numeric spelling reaches generate()", { skip: pySkip }, (t) => {
  for (const [spelling, value] of [["-1e-2", -0.01], ["-5.", -5], ["-1E3", -1000], ["-5.e2", -500], ["-.5e1", -5], ["-1", -1], ["-0.5", -0.5]]) {
    const r = viaLauncher(t, (d) => [join(d, "voice.wav"), "hola", "--temperature", spelling]);
    assert.equal(r.status, 0, `--temperature ${spelling}: ${r.stderr}`);
    assert.deepStrictEqual(r.gen.kwargs, { temperature: value }, spelling);
  }
});

test("tts_chatterbox_ft.py: reads the same six flags the launcher forwards (it still validates and defers, exit 3)", { skip: pySkip }, () => {
  const plan = parseCli(["x.wav", "hola", "--engine", "finetuned", "--model", "/no/such/model.safetensors", "--base-dir", "/no/such/dir", ...SIX_CLI], {});
  assert.equal(plan.ok, true, plan.error);
  assert.equal(plan.worker, FT_WORKER);
  const r = spawnSync(PY, ["-I", "-B", join(HERE, plan.worker), ...plan.args], { encoding: "utf8", windowsHide: true, timeout: 60000 });
  assert.equal(r.status, 3, `argparse must accept every forwarded flag (exit 2 means it did not): ${r.stderr}`);
  assert.ok(r.stderr.includes("FT_ENGINE_NOT_VENDORED"), r.stderr);
});

test("tts.mjs -> tts_chatterbox_ft.py: a dash-led text, model path and base dir are read by the worker (exit 3, not 2)", { skip: pySkip }, () => {
  const plan = parseCli(["x.wav", "---", "--engine", "finetuned", "--model", "-m.safetensors", "--base-dir", "-b"], {});
  assert.equal(plan.ok, true, plan.error);
  const r = spawnSync(PY, ["-I", "-B", join(HERE, plan.worker), ...plan.args], { encoding: "utf8", windowsHide: true, timeout: 60000 });
  assert.equal(r.status, 3, `a separate value token that opens with a dash is an option to argparse (exit 2): ${r.stderr}`);
  assert.match(r.stderr, /model not found: -m\.safetensors/);
});

// The premise of the launcher's finetuned rule: the worker requires both flags, and a missing one is argparse's exit 2 (a failure to
// the Go wrapper), not the exit 3 defer of an engine that is not vendored.
test("tts_chatterbox_ft.py requires --model and --base-dir (argparse exit 2)", { skip: pySkip }, () => {
  const r = spawnSync(PY, ["-I", "-B", join(HERE, FT_WORKER), "--out=x.wav", "--text=hola"], { encoding: "utf8", windowsHide: true, timeout: 60000 });
  assert.equal(r.status, 2, r.stderr);
  assert.match(r.stderr, /required: --model, --base-dir/);
});
