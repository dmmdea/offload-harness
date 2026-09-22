// node --test render/comfy-edit.test.mjs
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, existsSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { parseEditArgs, planEdit, stageInputs, unstageInputs, UsageError, EDIT_FAMILIES } from "./comfy-edit.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const SCRIPT = join(HERE, "comfy-edit.mjs");

const one = (g, cls) => {
  const ns = Object.values(g).filter((n) => n.class_type === cls);
  assert.equal(ns.length, 1, `exactly one ${cls}`);
  return ns[0].inputs;
};
const always = () => true;
const plan = (argv, extra = {}) => planEdit({ ...parseEditArgs(argv), env: {}, exists: always, measure: () => ({ width: 1024, height: 1024 }), ...extra });

const E2511 = ["o.png", "src.png", "make it snow", "--unet", "qwen-image-edit-2511-Q5_1.gguf"];
const E21 = ["o.png", "target.png", "put the shirt from <image2> on the person in <image1>", "--family", "qwen-image-2.1",
  "--unet", "qwen_image_2.1_bf16.safetensors", "--clip", "qwen3vl_8b_bf16.safetensors",
  "--vae", "qwen_image_2.1_vae_bf16.safetensors", "--seed", "5"];

test("parseEditArgs: --ref repeats in order; --no-lock is boolean", () => {
  const { pos, flags, refs } = parseEditArgs(["o.png", "--ref", "a.png", "t.png", "--no-lock", "--ref", "b.png", "p"]);
  assert.deepEqual(pos, ["o.png", "t.png", "p"]);
  assert.deepEqual(refs, ["a.png", "b.png"]);
  assert.equal(flags["no-lock"], true);
});

test("default family stays the 2511 graph, single image, preset-resolved", () => {
  const p = plan(E2511);
  assert.equal(p.family, "qwen-image-edit-2511");
  assert.deepEqual(p.sources, ["src.png"]);
  const g = p.build(["staged.png"]);
  assert.ok(Object.values(g).some((n) => n.class_type === "TextEncodeQwenImageEditPlus"));
  assert.equal(one(g, "LoadImage").image, "staged.png");
  const ks = one(g, "KSampler");
  assert.equal(ks.steps, 8, "lightning8 is the default preset");
  assert.match(p.describe, /preset lightning8/);
  assert.equal(plan([...E2511, "--family", "qwen-image-edit-2511"]).family, "qwen-image-edit-2511", "the default is also nameable");
});

test("2511 refuses the multi-reference and 2.1-only knobs; unknown family refused", () => {
  const cases = [
    [[...E2511, "--ref", "b.png"], /takes ONE image/],
    [[...E2511, "--resolution", "1024"], /--resolution is a qwen-image-2.1 edit knob/],
    [[...E2511, "--cache-device", "gpu"], /--cache-device is a qwen-image-2.1 edit knob/],
    [[...E2511, "--transparent", "1"], /--transparent is a qwen-image-2.1 edit knob/],
    [[...E2511, "--preset", "__proto__"], /--preset must be one of/],
    [[...E2511, "--family", "qwen-image-21"], /unknown --family 'qwen-image-21'/],
  ];
  for (const [argv, re] of cases) assert.throws(() => plan(argv), (e) => e instanceof UsageError && re.test(e.message), argv.slice(-2).join(" "));
  assert.deepEqual(EDIT_FAMILIES, ["qwen-image-edit-2511", "qwen-image-2.1"]);
});

test("2.1: target first, then --ref in order, wired to images.image_1..N; knobs pass through", () => {
  const p = plan([...E21, "--ref", "shirt.png", "--ref", "hat.png", "--cache-device", "gpu", "--cache-dtype", "int8", "--resolution", "0"]);
  assert.equal(p.family, "qwen-image-2.1");
  assert.deepEqual(p.sources, ["target.png", "shirt.png", "hat.png"]);
  const g = p.build(["s_target.png", "s_shirt.png", "s_hat.png"]);
  const enc = one(g, "TextEncodeQwenImage21");
  ["s_target.png", "s_shirt.png", "s_hat.png"].forEach((name, i) => {
    assert.equal(g[enc[`images.image_${i + 1}`][0]].inputs.image, name, `image_${i + 1}`);
  });
  assert.equal(enc.resolution, 0);
  assert.deepEqual(one(g, "QwenImage21Cache"), { model: ["1", 0], device: "gpu", dtype: "int8" });
  assert.equal(one(g, "KSampler").seed, 5);
  assert.equal(one(g, "KSampler").steps, 40, "official recipe by default");
  const t = plan([...E21, "--transparent", "true"]).build(["x.png"]);
  assert.ok(Object.values(t).some((n) => n.class_type === "JoinImageWithAlpha"));
  const d = plan(E21).build(["x.png"]);
  assert.deepEqual(one(d, "QwenImage21Cache"), { model: ["1", 0], device: "auto", dtype: "default" });
  assert.equal(one(d, "TextEncodeQwenImage21").resolution, 1024);
});

test("2.1 refusals: >10 images, missing files, 2511 knobs, pair guard, enums", () => {
  const nine = Array.from({ length: 9 }, (_, i) => ["--ref", `r${i}.png`]).flat();
  assert.equal(plan([...E21, ...nine]).sources.length, 10, "target + 9 refs = the limit");
  const cases = [
    [[...E21, ...nine, "--ref", "one-too-many.png"], /at most 10 images/],
    [[...E21, "--preset", "lightning8"], /--preset is a 2511 edit knob/],
    [[...E21, "--lora", "x.safetensors"], /--lora is a 2511 edit knob/],
    [[...E21, "--megapixels", "2"], /--megapixels is a 2511 edit knob/],
    [[...E21, "--steps", "20"], /--steps and --cfg together/],
    [[...E21, "--cache-device", "cuda"], /--cache-device must be one of/],
    [[...E21, "--cache-dtype", "fp8"], /--cache-dtype must be one of/],
    [[...E21, "--resolution", "1000"], /--resolution must be 0/],
    [[...E21, "--transparent", "2"], /--transparent takes/],
    [E21.filter((x, i, a) => !(x === "--clip" || a[i - 1] === "--clip")), /requires --clip/],
    [E21.filter((x, i, a) => !(x === "--vae" || a[i - 1] === "--vae")), /requires --vae/],
    [E21.filter((x, i, a) => !(x === "--unet" || a[i - 1] === "--unet")), /--unet is required/],
  ];
  for (const [argv, re] of cases) assert.throws(() => plan(argv), (e) => e instanceof UsageError && re.test(e.message), argv.slice(-2).join(" "));
  // A missing reference is refused before anything is staged.
  assert.throws(() => plan([...E21, "--ref", "gone.png"], { exists: (p) => p !== "gone.png" }),
    (e) => e instanceof UsageError && /input image not found: gone.png/.test(e.message));
});

test("stageInputs/unstageInputs: every source staged under a unique name and every one removed", () => {
  const root = mkdtempSync(join(tmpdir(), "comfy-edit-"));
  const inputDir = join(root, "input");
  mkdirSync(inputDir);
  const srcs = ["t.png", "a.png", "b.png"].map((n) => { const p = join(root, n); writeFileSync(p, n); return p; });
  const staged = [];
  stageInputs(srcs, staged, { inputDir, now: () => 123 });
  assert.deepEqual(staged, ["edit_in_123_0_t.png", "edit_in_123_1_a.png", "edit_in_123_2_b.png"]);
  for (const n of staged) assert.ok(existsSync(join(inputDir, n)));
  unstageInputs(staged, { inputDir });
  assert.deepEqual(readdirSync(inputDir), []);
});

test("a copy failure half-way still leaves the staged list complete for the finally", () => {
  const root = mkdtempSync(join(tmpdir(), "comfy-edit-"));
  const inputDir = join(root, "input");
  mkdirSync(inputDir);
  const good = join(root, "t.png");
  writeFileSync(good, "x");
  const staged = [];
  assert.throws(() => stageInputs([good, join(root, "missing.png")], staged, { inputDir, now: () => 1 }), /ENOENT/);
  assert.deepEqual(staged, ["edit_in_1_0_t.png"], "the first copy landed and is recorded");
  unstageInputs(staged, { inputDir });
  assert.deepEqual(readdirSync(inputDir), []);
});

test("the real script exits 2 on a caller mistake, before the GPU slot is touched", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-edit-"));
  const img = join(dir, "t.png");
  writeFileSync(img, "x");
  for (const [extra, re] of [
    [["--family", "flux-kontext"], /unknown --family/],
    [["--ref", img], /takes ONE image/],
  ]) {
    const r = spawnSync(process.execPath, [SCRIPT, join(dir, "o.png"), img, "edit it", "--unet", "u.gguf", ...extra],
      { encoding: "utf8", timeout: 30000, env: { ...process.env, GPU_LEASE_DIR: "", GPU_LEASE_EPOCH: "", GPU_LEASE_CLASS: "" } });
    assert.equal(r.status, 2, `exit ${r.status}; stderr: ${r.stderr}`);
    assert.match(r.stderr, re);
    assert.doesNotMatch(r.stderr, /GPU lease missing/, "refused before withGpuSlot");
  }
});
