// node --test render/sdcpp-generate.test.mjs
// Tests sdcpp-generate.mjs arg parsing + the OUR-flags -> sd.cpp-CLI mapping with NO
// spawn / GPU / SDCPP_BIN. The runner exports pure functions and only runs withGpuSlot
// as the main module (importing it here is side-effect-free), mirroring tts.test.mjs.
import { test } from "node:test";
import assert from "node:assert";
import { parseArgs, buildSdArgs } from "./sdcpp-generate.mjs";

test("parseArgs: positionals, value flags, repeated --extra, --no-lock", () => {
  const { pos, flags, extra } = parseArgs(["out.png", "a red fox",
    "--model", "D:/m/z.gguf", "--model-kind", "diffusion", "--steps", "8",
    "--extra", "--vae-on-cpu", "--extra", "--clip-on-cpu", "--no-lock"]);
  assert.equal(pos[0], "out.png");
  assert.equal(pos[1], "a red fox");
  assert.equal(flags.model, "D:/m/z.gguf");
  assert.equal(flags["model-kind"], "diffusion");
  assert.equal(flags.steps, "8");
  assert.deepEqual(extra, ["--vae-on-cpu", "--clip-on-cpu"]);
  assert.equal(flags["no-lock"], true);
});

test("buildSdArgs: diffusion model-kind maps to --diffusion-model, companions + samplers map to the pinned sd.cpp flag names", () => {
  const flags = {
    model: "D:/m/z_image_turbo-Q8_0.gguf", "model-kind": "diffusion",
    vae: "D:/m/ae.safetensors", "clip-l": "D:/m/clip_l.st", "clip-g": "D:/m/clip_g.st",
    t5xxl: "D:/m/t5.st", llm: "D:/m/qwen3-4b-Q4_K_M.gguf",
    negative: "blurry", width: "1024", height: "768", steps: "8", seed: "42",
    cfg: "1", sampler: "euler",
  };
  const a = buildSdArgs("out.png", "a red fox", flags, ["--vae-on-cpu"]);
  const s = a.join(" ");
  assert.match(s, /--diffusion-model D:\/m\/z_image_turbo-Q8_0\.gguf/);
  assert.doesNotMatch(s, /(^| )-m /); // diffusion kind must NOT also pass -m
  // pinned-release spellings: underscores in clip flags, -p/-n/-W/-H/-s, --cfg-scale,
  // --sampling-method, -o output last
  assert.match(s, /--vae D:\/m\/ae\.safetensors/);
  assert.match(s, /--clip_l D:\/m\/clip_l\.st/);
  assert.match(s, /--clip_g D:\/m\/clip_g\.st/);
  assert.match(s, /--t5xxl D:\/m\/t5\.st/);
  assert.match(s, /--llm D:\/m\/qwen3-4b-Q4_K_M\.gguf/);
  assert.match(s, /-p a red fox/);
  assert.match(s, /-n blurry/);
  assert.match(s, /-W 1024/);
  assert.match(s, /-H 768/);
  assert.match(s, /--steps 8/);
  assert.match(s, /-s 42/);
  assert.match(s, /--cfg-scale 1/);
  assert.match(s, /--sampling-method euler/);
  assert.match(s, /--vae-on-cpu/);
  assert.equal(a[a.length - 2], "-o");
  assert.equal(a[a.length - 1], "out.png");
});

test("buildSdArgs: checkpoint kind (default) maps to -m; absent flags emit nothing", () => {
  const a = buildSdArgs("o.png", "p", { model: "D:/m/sd15.safetensors" }, []);
  assert.deepEqual(a, ["-m", "D:/m/sd15.safetensors", "-p", "p", "-o", "o.png"]);
});
