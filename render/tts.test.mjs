// node --test render/tts.test.mjs
// Tests tts.mjs arg parsing + worker selection + worker-arg building with NO spawn / GPU.
// tts.mjs exports pure functions and only runs withGpuSlot as the main module (importing
// it here is side-effect-free), mirroring comfy-music.mjs / wf-*.mjs.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { parseArgs, selectWorker, buildWorkerArgs, GENERALIST_WORKER, FT_WORKER } from "./tts.mjs";

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

test("buildWorkerArgs: generalist threads out/text/lang/ref only", () => {
  const args = buildWorkerArgs("out.wav", "hola", { clone: "/r/ref.wav" }, {});
  assert.deepEqual(args, ["--out", "out.wav", "--text", "hola", "--lang", "es", "--ref", "/r/ref.wav"]);
});

test("buildWorkerArgs: finetuned threads --model/--base-dir + recipe kwargs", () => {
  const args = buildWorkerArgs("out.wav", "hola",
    { engine: "finetuned", model: "/m/x.safetensors", "base-dir": "/m/base", clone: "/r/dan.wav",
      temperature: "0.6", "cfg-weight": "0.5" }, {});
  assert.equal(args[args.indexOf("--model") + 1], "/m/x.safetensors");
  assert.equal(args[args.indexOf("--base-dir") + 1], "/m/base");
  assert.equal(args[args.indexOf("--temperature") + 1], "0.6");
  assert.equal(args[args.indexOf("--cfg-weight") + 1], "0.5");
  assert.equal(args[args.indexOf("--ref") + 1], "/r/dan.wav");
});

test("buildWorkerArgs: lang defaults es; TTS_REF env used when no --clone", () => {
  const args = buildWorkerArgs("out.wav", "hola", {}, { TTS_REF: "/r/env.wav" });
  assert.equal(args[args.indexOf("--lang") + 1], "es");
  assert.equal(args[args.indexOf("--ref") + 1], "/r/env.wav");
});

test("tts_chatterbox_ft.py skeleton: validates + defers (exit 3 + marker), stdlib-only", () => {
  const py = process.env.TTS_PY || "python";
  const worker = join(dirname(fileURLToPath(import.meta.url)), "tts_chatterbox_ft.py");
  const r = spawnSync(py, [worker, "--out", "x.wav", "--text", "hola",
    "--model", "/no/such/model.safetensors", "--base-dir", "/no/such/dir"], { encoding: "utf8" });
  if (r.error && r.error.code === "ENOENT") return; // no python on PATH — skip
  assert.equal(r.status, 3, "skeleton must exit 3 (defer signal)");
  assert.ok((r.stderr || "").includes("FT_ENGINE_NOT_VENDORED"), "must emit the distinct marker");
});
