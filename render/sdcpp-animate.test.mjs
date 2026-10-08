// node --test render/sdcpp-animate.test.mjs
// sdcpp-animate.mjs: the three argv builders (ffmpeg frames, depth, VACE), the frame
// arithmetic, the depth device pin and the no-CPU refusal at the script's own door.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
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

test("buildDepthArgs: ONE frame in, one depth PNG out, --no-invert (near=bright); never the multi-view form; extra args last", () => {
  const a = buildDepthArgs({ model: "/d/depth.gguf", input: "f.png", outPng: "d.png" });
  assert.deepEqual(a, ["depth", "--model", "/d/depth.gguf", "--input", "f.png", "--png", "d.png", "--no-invert"]);
  assert.equal(a.filter((x) => x === "--input").length, 1, "`--input a --input b` is multi-view joint depth, not per-frame");
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

test("buildSdAnimateArgs: --vae-tile-overlap 0.25 by default (override allowed), --taesd only with a TAE, never --offload-to-cpu", () => {
  const base = { outFile: "o.webm", ref: "r.png", depthDir: "d", prompt: "p" };
  const a = buildSdAnimateArgs({ ...base, flags });
  assert.equal(a[a.indexOf("--vae-tile-overlap") + 1], "0.25");
  assert.ok(!a.includes("--taesd") && !a.includes("--offload-to-cpu"));
  const b = buildSdAnimateArgs({ ...base, flags: { ...flags, tae: "/m/taew2_2.safetensors", "vae-tile-overlap": "0.5" } });
  assert.equal(b[b.indexOf("--taesd") + 1], "/m/taew2_2.safetensors");
  assert.equal(b[b.indexOf("--vae-tile-overlap") + 1], "0.5");
  assert.throws(() => buildSdAnimateArgs({ ...base, flags: { ...flags, "vae-tile-overlap": "1.5" } }), /vae-tile-overlap/);
});

// What parseArgs hands over for a flag given with no value is `undefined` (a key that is present), and a
// numeric option may arrive as "" or text: none of it may reach sd-cli as an option or as the word "undefined".
test("buildSdAnimateArgs: options that are unset, empty or not a number never reach the argv", () => {
  const base = { outFile: "o.webm", ref: "r.png", depthDir: "d", prompt: "p" };
  const unset = { ...flags, negative: undefined, cfg: undefined, steps: undefined, "flow-shift": undefined, seed: undefined, tae: undefined };
  const empty = { ...flags, negative: "", cfg: "", steps: "", "flow-shift": "", seed: "", tae: "" };
  const text = { ...flags, cfg: "abc", steps: "x", "flow-shift": "y", seed: "z" };
  for (const [name, f] of [["absent", flags], ["unset", unset], ["empty", empty], ["not a number", text]]) {
    const a = buildSdAnimateArgs({ ...base, flags: f });
    for (const x of a) assert.ok(!/undefined|NaN|null/.test(String(x)), `${name}: ${JSON.stringify(x)} must not reach the argv`);
    for (const o of ["-n", "--cfg-scale", "--steps", "--flow-shift", "-s", "--taesd"]) assert.ok(!a.includes(o), `${name}: ${o} must be omitted`);
  }
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

test("the script reports missing flags (exit 2), a bare binary name (BINARY_NOT_ABSOLUTE) and a missing depth binary by name (exit 1)", () => {
  const r = run(["o.mp4", "ref.png", "drive.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x", "--backend", "vulkan0"]);
  assert.equal(r.status, 2);
  assert.match(r.stderr, /--depth-bin/);
  const bare = run(["o.mp4", "ref.png", "drive.mp4", "p", "--sd-bin", "x", "--model", "x", "--vae", "x", "--t5xxl", "x",
    "--depth-bin", "x", "--depth-model", "x", "--backend", "vulkan0", "--no-lock"]);
  assert.equal(bare.status, 1);
  assert.match(bare.stderr, /BINARY_NOT_ABSOLUTE/);
  const missing = run(["o.mp4", "ref.png", "drive.mp4", "p", "--sd-bin", process.execPath, "--model", "x", "--vae", "x", "--t5xxl", "x",
    "--depth-bin", join(tmpdir(), "no-such-da3-cli"), "--depth-model", "x", "--backend", "vulkan0", "--no-lock"]);
  assert.equal(missing.status, 1);
  assert.match(missing.stderr, /--depth-bin not found/);
});

const animBase = ["o.mp4", "/no/ref.png", "/no/drive.mp4", "p", "--sd-bin", "/no/sd", "--model", "/no/m", "--vae", "/no/v", "--t5xxl", "/no/t",
  "--depth-bin", "/no/d", "--depth-model", "/no/dm", "--backend", "vulkan0", "--no-lock"];

test("animate: the token cap counts the VACE reference as one more latent frame, and refuses before any file check or spawn", () => {
  // 480x832x33 + reference on an 8x VAE is 15600 tokens (device lost on the node); 288x512 is 5760
  const over = run([...animBase, "--width", "480", "--height", "832", "--frames", "33", "--max-tokens", "5760", "--vae-stride", "8"]);
  assert.equal(over.status, 1, over.stderr);
  assert.match(over.stderr, /TOKEN_CAP_EXCEEDED/);
  assert.match(over.stderr, /needs 15600 latent tokens/);
  assert.match(over.stderr, /reference/);
  assert.doesNotMatch(over.stderr, /not found/);
  const fits = run([...animBase, "--width", "288", "--height", "512", "--frames", "33", "--max-tokens", "5760", "--vae-stride", "8"]);
  assert.match(fits.stderr, /not found/);
  assert.doesNotMatch(fits.stderr, /TOKEN_CAP_EXCEEDED/);
  const none = run([...animBase, "--width", "480", "--height", "832", "--frames", "33"]);
  assert.match(none.stderr, /not found/, "no cap configured = no check");
});

test("animate: extra args and depth extra args that change the backend or placement are refused before any spawn", () => {
  // sanctioned spill (weights in RAM, compute on the GPU) is not refused: it reaches the file checks
  const spill = run([...animBase, "--extra-args", JSON.stringify(["--offload-to-cpu"])]);
  assert.doesNotMatch(spill.stderr, /EXTRA_ARGS_REFUSED/);
  assert.match(spill.stderr, /not found/);
  const sd = run([...animBase, "--extra-args", JSON.stringify(["--clip-on-cpu"])]);
  assert.equal(sd.status, 1);
  assert.match(sd.stderr, /EXTRA_ARGS_REFUSED/);
  assert.match(sd.stderr, /--extra-args\[0\]/);
  const depth = run([...animBase, "--depth-extra-args", JSON.stringify(["--threads", "4", "--backend", "cpu"])]);
  assert.equal(depth.status, 1);
  assert.match(depth.stderr, /EXTRA_ARGS_REFUSED/);
  assert.match(depth.stderr, /--depth-extra-args\[2\]/);
});
