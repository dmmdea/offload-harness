// node --test render/sdcpp-generate.test.mjs
// Tests sdcpp-generate.mjs arg parsing + the OUR-flags -> sd.cpp-CLI mapping with NO
// spawn / GPU / SDCPP_BIN. The runner exports pure functions and only runs withGpuSlot
// as the main module (importing it here is side-effect-free), mirroring tts.test.mjs.
import { test } from "node:test";
import assert from "node:assert";
import { mkdtempSync, rmSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  parseArgs, buildSdArgs, postprocessOutput,
  parseVulkanDeviceList, isIntegratedGpuName, isDiscreteGpuName,
  pickDiscreteVulkanDevice, resolveVulkanDevice,
} from "./sdcpp-generate.mjs";
import { encodePng, decodePng } from "./png-alpha.mjs";
import { RGBA_PROMPT_PREFIX, RGBA_PROMPT_SUFFIX } from "./wf-qwen-image-21.mjs";

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

// --- D5: transparent wraps the prompt in the OFFICIAL RGBA template; sd.cpp has no
// --transparent CLI flag of its own, so this is the only place the request reaches
// the model. Reuses wf-qwen-image-21.mjs's own constants — never a duplicated string.
test("buildSdArgs: transparent wraps the prompt in the official RGBA template (no --transparent token reaches sd-cli)", () => {
  const a = buildSdArgs("o.png", "a fox sticker", { model: "m.gguf", transparent: "1" }, []);
  const s = a.join(" ");
  assert.match(s, /-p This is an RGBA image with transparency\. a fox sticker\. The image has alpha channel/);
  assert.doesNotMatch(s, /--transparent/, "sd.cpp has no such flag; --transparent is OUR flag only, consumed before buildSdArgs");
});

test("buildSdArgs: transparent absent/falsy leaves the prompt untouched", () => {
  for (const flags of [{ model: "m.gguf" }, { model: "m.gguf", transparent: "0" }, { model: "m.gguf", transparent: "no" }]) {
    const a = buildSdArgs("o.png", "a fox sticker", flags, []);
    assert.match(a.join(" "), /-p a fox sticker(?!\.)/);
  }
});

// --- D5: postprocessOutput — the default flattens RGBA to opaque RGB in place;
// transparent keeps the file (and its alpha) exactly as sd-cli wrote it.
function tmpPng(rgba) {
  const dir = mkdtempSync(join(tmpdir(), "sdcpp-alpha-"));
  const path = join(dir, "out.png");
  writeFileSync(path, encodePng(rgba));
  return { dir, path };
}

test("postprocessOutput: default (no transparent) flattens a fixture RGBA PNG to opaque RGB — no alpha channel in the output", () => {
  const rgba = { width: 2, height: 2, channels: 4, pixels: Buffer.from([
    255, 0, 0, 255,   0, 255, 0, 243,
    0, 0, 255, 0,     10, 20, 30, 128,
  ]) };
  const { dir, path } = tmpPng(rgba);
  try {
    const changed = postprocessOutput(path, undefined);
    assert.equal(changed, true);
    const out = decodePng(readFileSync(path));
    assert.equal(out.channels, 3, "the default render must carry no alpha channel at all");
    // RGB bytes survive untouched (a channel drop, not a black/white composite).
    assert.equal(out.pixels[0], 255); assert.equal(out.pixels[1], 0); assert.equal(out.pixels[2], 0);
    assert.equal(out.pixels[9], 10); assert.equal(out.pixels[10], 20); assert.equal(out.pixels[11], 30);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("postprocessOutput: transparent:true keeps the file (and its alpha) exactly as written", () => {
  const rgba = { width: 1, height: 1, channels: 4, pixels: Buffer.from([1, 2, 3, 77]) };
  const { dir, path } = tmpPng(rgba);
  try {
    const before = readFileSync(path);
    const changed = postprocessOutput(path, "1");
    assert.equal(changed, false);
    const after = readFileSync(path);
    assert.ok(after.equals(before), "transparent output must not be touched");
    assert.equal(decodePng(after).channels, 4);
    assert.equal(decodePng(after).pixels[3], 77);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("wf-qwen-image-21.mjs's RGBA template constants are what buildSdArgs reuses (no drift)", () => {
  assert.equal(RGBA_PROMPT_PREFIX, "This is an RGBA image with transparency. ");
  assert.equal(RGBA_PROMPT_SUFFIX, ". The image has alpha channel and the background is transparent.");
});

// --- Vulkan device auto-pick (2026-09-23 OptiPlex fix): this runner used to pin
// GGML_VK_VISIBLE_DEVICES=0 whenever unset, which on a box with an enabled
// integrated GPU pinned the iGPU (Vulkan0 there) instead of the discrete card
// (Vulkan1) — every render ran 100x slower with no error. These tests fix that
// class of regression at the seam resolveVulkanDevice actually decides through.
const TWO_DEVICE_LISTING = "Vulkan0 Intel(R) UHD Graphics 630\nVulkan1 NVIDIA GeForce RTX 5060\n";

test("parseVulkanDeviceList: parses VulkanN <name> lines, ignores anything else", () => {
  const got = parseVulkanDeviceList(TWO_DEVICE_LISTING);
  assert.deepEqual(got, [
    { index: "0", name: "Intel(R) UHD Graphics 630" },
    { index: "1", name: "NVIDIA GeForce RTX 5060" },
  ]);
  assert.deepEqual(parseVulkanDeviceList("garbage\nnot a device line\n"), []);
  assert.deepEqual(parseVulkanDeviceList(""), []);
});

test("isIntegratedGpuName / isDiscreteGpuName classify Intel iGPUs vs NVIDIA/AMD discrete cards", () => {
  for (const n of ["Intel(R) UHD Graphics 630", "Intel Iris Xe Graphics", "Intel(R) Arc(TM) A750 Graphics"]) {
    assert.equal(isIntegratedGpuName(n), true, n);
    assert.equal(isDiscreteGpuName(n), false, n);
  }
  for (const n of ["NVIDIA GeForce RTX 5060", "AMD Radeon RX 7900 XTX"]) {
    assert.equal(isIntegratedGpuName(n), false, n);
    assert.equal(isDiscreteGpuName(n), true, n);
  }
});

test("pickDiscreteVulkanDevice: returns the first discrete adapter's index; null when only an iGPU is listed", () => {
  assert.equal(pickDiscreteVulkanDevice(parseVulkanDeviceList(TWO_DEVICE_LISTING)), "1");
  assert.equal(pickDiscreteVulkanDevice(parseVulkanDeviceList("Vulkan0 Intel(R) UHD Graphics 630\n")), null);
  assert.equal(pickDiscreteVulkanDevice([]), null);
});

test("resolveVulkanDevice: OptiPlex shape — iGPU at Vulkan0, RTX at Vulkan1 — auto-picks the RTX, never device 0", () => {
  const got = resolveVulkanDevice("sd-cli", "", { list: () => TWO_DEVICE_LISTING });
  assert.equal(got, "1");
});

test("resolveVulkanDevice: an explicit env override always wins, even over a listed discrete adapter", () => {
  const got = resolveVulkanDevice("sd-cli", "0", { list: () => TWO_DEVICE_LISTING });
  assert.equal(got, "0");
});

test("resolveVulkanDevice: no discrete adapter listed (iGPU-only box) falls back to device 0", () => {
  const got = resolveVulkanDevice("sd-cli", "", { list: () => "Vulkan0 Intel(R) UHD Graphics 630\n" });
  assert.equal(got, "0");
});

test("resolveVulkanDevice: a --list-devices probe failure falls back to device 0, never throws", () => {
  const got = resolveVulkanDevice("sd-cli", "", { list: () => { throw new Error("spawn ENOENT"); } });
  assert.equal(got, "0");
});
