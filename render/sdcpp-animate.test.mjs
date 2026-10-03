// node --test render/sdcpp-animate.test.mjs
// sdcpp-animate.mjs: the three argv builders (ffmpeg frames, depth, VACE), the frame
// arithmetic, the depth device pin and the no-CPU refusal at the script's own door.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  resolveParams, buildExtractArgs, buildDepthArgs, depthEnv, buildSdAnimateArgs, framesToRender, OUTPUT_FPS, DRIVER_FPS,
} from "./sdcpp-animate.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, "sdcpp-animate.mjs");

const flags = {
  model: "/m/wan2.1-vace-1.3b-q8_0.gguf", vae: "/m/wan_2.1_vae.safetensors", t5xxl: "/m/umt5-xxl-q8_0.gguf",
  backend: "vulkan0", "depth-bin": "/d/da3-cli", "depth-model": "/d/depth.gguf",
};

test("resolveParams: 4k+1 frames (default 49), %32 size (default 832x480)", () => {
  assert.deepEqual(resolveParams({}), { frames: 49, width: 832, height: 480 });
  assert.deepEqual(resolveParams({ frames: "50", width: "854", height: "485" }), { frames: 49, width: 832, height: 480 });
  assert.equal(resolveParams({ frames: "81" }).frames, 81);
});

test("buildExtractArgs: 16 fps, scaled to cover W x H and cropped, first N frames, numbered PNGs", () => {
  const a = buildExtractArgs({ driver: "/in/drive.mp4", framesDir: "/t/frames", width: 832, height: 480, frames: 49 });
  assert.equal(a[a.indexOf("-i") + 1], "/in/drive.mp4");
  const vf = a[a.indexOf("-vf") + 1];
  assert.match(vf, new RegExp(`fps=${DRIVER_FPS}`));
  assert.match(vf, /scale=832:480:force_original_aspect_ratio=increase/);
  assert.match(vf, /crop=832:480/);
  assert.equal(a[a.indexOf("-frames:v") + 1], "49");
  assert.match(a[a.length - 1].replace(/\\/g, "/"), /\/t\/frames\/%05d\.png$/);
});

test("buildDepthArgs: one frame in, one depth PNG out (README-bound; extra args last)", () => {
  assert.deepEqual(buildDepthArgs({ model: "/d/depth.gguf", input: "f.png", outPng: "d.png" }),
    ["depth", "--model", "/d/depth.gguf", "--input", "f.png", "--png", "d.png"]);
  assert.deepEqual(buildDepthArgs({ model: "m", input: "i", outPng: "o", extra: ["--x", "1"] }).slice(-2), ["--x", "1"]);
});

test("depthEnv: pins the depth step to the vulkan device the sd backend names, unless the operator already did", () => {
  assert.deepEqual(depthEnv("vulkan1", {}), { GGML_VK_VISIBLE_DEVICES: "1" });
  assert.deepEqual(depthEnv("vulkan", {}), {});
  assert.deepEqual(depthEnv("vulkan1", { GGML_VK_VISIBLE_DEVICES: "0" }), {});
});

test("buildSdAnimateArgs: VACE with the depth directory as --control-video and the reference as -i", () => {
  const a = buildSdAnimateArgs({
    outFile: "/t/o.webm", ref: "/in/ref.png", depthDir: "/t/depth", prompt: "a knight",
    flags: { ...flags, negative: "blurry", cfg: "6", steps: "20", "flow-shift": "5", seed: "9", frames: "49", width: "832", height: "480" },
    extra: ["--extra-flag"],
  });
  const at = (k) => a[a.indexOf(k) + 1];
  assert.deepEqual(a.slice(0, 4), ["-M", "vid_gen", "--diffusion-model", "/m/wan2.1-vace-1.3b-q8_0.gguf"]);
  assert.equal(at("--vae"), "/m/wan_2.1_vae.safetensors");
  assert.equal(at("--t5xxl"), "/m/umt5-xxl-q8_0.gguf");
  assert.equal(at("-i"), "/in/ref.png");
  assert.equal(at("--control-video"), "/t/depth");
  assert.equal(at("-p"), "a knight");
  assert.equal(at("-n"), "blurry");
  assert.equal(at("--cfg-scale"), "6");
  assert.equal(at("--steps"), "20");
  assert.equal(at("--flow-shift"), "5");
  assert.equal(at("--video-frames"), "49");
  assert.equal(at("--fps"), String(OUTPUT_FPS));
  assert.equal(at("-s"), "9");
  assert.equal(at("--backend"), "vulkan0");
  for (const f of ["--diffusion-fa", "--vae-tiling", "-v"]) assert.ok(a.includes(f));
  assert.ok(a.indexOf("--extra-flag") > a.indexOf("-v"));
  assert.equal(a[a.length - 2], "-o");
  assert.equal(a[a.length - 1], "/t/o.webm");
});

test("framesToRender: the request when the driver has enough, else the largest 4k+1 that exists, else 0", () => {
  assert.equal(framesToRender(49, 49), 49);
  assert.equal(framesToRender(49, 80), 49);
  assert.equal(framesToRender(49, 30), 29);
  assert.equal(framesToRender(49, 4), 0);
});

function run(args) {
  return spawnSync(process.execPath, [script, ...args], { encoding: "utf8", timeout: 30000 });
}

test("the script refuses a cpu backend before touching anything (exit 1, CPU_BACKEND_REFUSED)", () => {
  for (const backend of ["cpu", "clip=cpu,diffusion=vulkan0"]) {
    const r = run(["o.mp4", "ref.png", "drive.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x",
      "--depth-bin", "x", "--depth-model", "x", "--backend", backend, "--no-lock"]);
    assert.equal(r.status, 1, `backend ${backend}: ${r.stderr}`);
    assert.match(r.stderr, /CPU_BACKEND_REFUSED/);
  }
});

test("the script reports missing flags (exit 2) and a missing depth binary by name (exit 1)", () => {
  const r = run(["o.mp4", "ref.png", "drive.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x", "--backend", "vulkan0"]);
  assert.equal(r.status, 2);
  assert.match(r.stderr, /--depth-bin/);
  const m = run(["o.mp4", "ref.png", "drive.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x",
    "--depth-bin", "x", "--depth-model", "x", "--backend", "vulkan0", "--no-lock"]);
  assert.equal(m.status, 1);
  assert.match(m.stderr, /not found/);
});
