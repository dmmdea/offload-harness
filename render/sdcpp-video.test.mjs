// node --test render/sdcpp-video.test.mjs
// sdcpp-video.mjs: the OUR-flags -> `sd-cli -M vid_gen` argv mapping, the 4k+1 / %32
// normalization and the no-CPU refusal at the script's own door. No spawn of a real engine.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { buildSdVideoArgs, resolveParams, splitPositionals, missingFlags, parseArgs } from "./sdcpp-video.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, "sdcpp-video.mjs");

const baseFlags = {
  model: "/m/wan2.2-ti2v-5b-q8_0.gguf", vae: "/m/wan2.2_vae.safetensors", t5xxl: "/m/umt5-xxl-q8_0.gguf",
  backend: "vulkan0",
};

test("buildSdVideoArgs: the full I2V argv in the verified master-929 spelling", () => {
  const flags = {
    ...baseFlags, "high-noise-model": "/m/high.gguf", negative: "blurry", cfg: "1", steps: "3", sampler: "euler",
    "flow-shift": "3", width: "832", height: "480", frames: "49", fps: "24", seed: "7",
  };
  const a = buildSdVideoArgs({ outFile: "/tmp/o.webm", still: "/in/still.png", prompt: "a calm sea", flags, extra: ["--clip-on-cpu-NOT"] });
  assert.deepEqual(a.slice(0, 4), ["-M", "vid_gen", "--diffusion-model", "/m/wan2.2-ti2v-5b-q8_0.gguf"]);
  const at = (k) => a[a.indexOf(k) + 1];
  assert.equal(at("--high-noise-diffusion-model"), "/m/high.gguf");
  assert.equal(at("--vae"), "/m/wan2.2_vae.safetensors");
  assert.equal(at("--t5xxl"), "/m/umt5-xxl-q8_0.gguf");
  assert.equal(at("-i"), "/in/still.png");
  assert.equal(at("-p"), "a calm sea");
  assert.equal(at("-n"), "blurry");
  assert.equal(at("--cfg-scale"), "1");
  assert.equal(at("--steps"), "3");
  assert.equal(at("--sampling-method"), "euler");
  assert.equal(at("--flow-shift"), "3");
  assert.equal(at("-W"), "832");
  assert.equal(at("-H"), "480");
  assert.equal(at("--video-frames"), "49");
  assert.equal(at("--fps"), "24");
  assert.equal(at("-s"), "7");
  assert.equal(at("--backend"), "vulkan0");
  for (const f of ["--diffusion-fa", "--vae-tiling", "-v"]) assert.ok(a.includes(f), `${f} must be present (-v feeds the CPU-placement guard)`);
  // extras land before the output, which is last
  assert.ok(a.indexOf("--clip-on-cpu-NOT") > a.indexOf("-v"));
  assert.equal(a[a.length - 2], "-o");
  assert.equal(a[a.length - 1], "/tmp/o.webm");
});

test("buildSdVideoArgs: a T2V render has no -i and no high-noise model; unset sampling flags are omitted", () => {
  const a = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: baseFlags });
  for (const f of ["-i", "--high-noise-diffusion-model", "-n", "--cfg-scale", "--steps", "--sampling-method", "--flow-shift", "-s"]) {
    assert.ok(!a.includes(f), `${f} must be omitted when unset`);
  }
  assert.equal(a[a.indexOf("-W") + 1], "832");
  assert.equal(a[a.indexOf("-H") + 1], "480");
  assert.equal(a[a.indexOf("--video-frames") + 1], "49");
  assert.equal(a[a.indexOf("--fps") + 1], "24");
});

test("buildSdVideoArgs: frames are normalized to 4k+1 and width/height to multiples of 32 in the argv", () => {
  const a = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...baseFlags, frames: "50", width: "854", height: "485" } });
  assert.equal(a[a.indexOf("--video-frames") + 1], "49");
  assert.equal(a[a.indexOf("-W") + 1], "832");
  assert.equal(a[a.indexOf("-H") + 1], "480");
  const b = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...baseFlags, frames: "81" } });
  assert.equal(b[b.indexOf("--video-frames") + 1], "81");
});

test("resolveParams: defaults and normalization", () => {
  assert.deepEqual(resolveParams({}), { frames: 49, width: 832, height: 480, fps: 24 });
  assert.deepEqual(resolveParams({ frames: "17", width: "100", height: "100", fps: "16" }), { frames: 17, width: 96, height: 96, fps: 16 });
  assert.equal(resolveParams({ fps: "0" }).fps, 24);
});

test("splitPositionals: <out> [<still>] <prompt>", () => {
  assert.deepEqual(splitPositionals(["o.mp4", "p"]), { out: "o.mp4", still: "", prompt: "p" });
  assert.deepEqual(splitPositionals(["o.mp4", "s.png", "p"]), { out: "o.mp4", still: "s.png", prompt: "p" });
  assert.equal(splitPositionals(["o.mp4"]), null);
  assert.equal(splitPositionals([]), null);
});

test("missingFlags / parseArgs", () => {
  assert.deepEqual(missingFlags({ a: "1" }, ["a", "b", "c"]), ["b", "c"]);
  const { pos, flags } = parseArgs(["o.mp4", "p", "--backend", "vulkan0", "--no-lock"]);
  assert.deepEqual(pos, ["o.mp4", "p"]);
  assert.equal(flags.backend, "vulkan0");
  assert.equal(flags["no-lock"], true);
});

function run(args) {
  return spawnSync(process.execPath, [script, ...args], { encoding: "utf8", timeout: 30000 });
}

test("the script refuses a cpu backend before touching anything (exit 1, CPU_BACKEND_REFUSED)", () => {
  for (const backend of ["cpu", "diffusion=vulkan0,vae=cpu"]) {
    const r = run(["out.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x", "--backend", backend, "--no-lock"]);
    assert.equal(r.status, 1, `backend ${backend}: ${r.stderr}`);
    assert.match(r.stderr, /CPU_BACKEND_REFUSED/);
  }
});

test("the script refuses a missing backend and missing required flags (exit 2)", () => {
  const r = run(["out.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x", "--no-lock"]);
  assert.equal(r.status, 2);
  assert.match(r.stderr, /--backend/);
  const u = run([]);
  assert.equal(u.status, 2);
  assert.match(u.stderr, /usage/);
});
