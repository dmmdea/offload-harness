// node --test render/comfy-render.test.mjs
// The family switch (buildRenderGraph) is pure and imported directly; the exit-2
// contract is also exercised through the real script (spawned), because the harness
// sees exit codes, not exceptions.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { buildRenderGraph, parseRenderArgs, parseBoolFlag, UsageError, KNOWN_FAMILIES, BOOL_FLAGS } from "./comfy-render.mjs";
import { rgbaPrompt } from "./wf-qwen-image-21.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const SCRIPT = join(HERE, "comfy-render.mjs");

const classes = (g) => Object.values(g).map((n) => n.class_type);
const one = (g, cls) => {
  const ns = Object.values(g).filter((n) => n.class_type === cls);
  assert.equal(ns.length, 1, `exactly one ${cls}`);
  return ns[0].inputs;
};
const build = (argv, env = {}) => buildRenderGraph({ ...parseRenderArgs(argv), env });

const QI21 = ["out.png", "a glass bottle", "--family", "qwen-image-2.1",
  "--ckpt", "qwen_image_2.1_bf16.safetensors", "--clip", "qwen3vl_8b_bf16.safetensors",
  "--vae", "qwen_image_2.1_vae_bf16.safetensors", "--seed", "42"];

test("unknown --family is a UsageError that names it and the known set", () => {
  assert.throws(() => build(["o.png", "p", "--family", "qwen-image-21"]), (e) =>
    e instanceof UsageError && /unknown --family 'qwen-image-21'/.test(e.message) && e.message.includes(KNOWN_FAMILIES.join(", ")));
  assert.throws(() => build(["o.png", "p", "--family", "flux"]), UsageError);
});

test("unknown --family exits 2 through the real script, before any ComfyUI work", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-render-"));
  const r = spawnSync(process.execPath, [SCRIPT, join(dir, "o.png"), "a prompt", "--family", "sdxl-turbo",
    "--api", "http://127.0.0.1:9"], { encoding: "utf8", timeout: 30000 });
  assert.equal(r.status, 2, `exit ${r.status}; stderr: ${r.stderr}`);
  assert.match(r.stderr, /unknown --family 'sdxl-turbo'/);
  assert.doesNotMatch(r.stderr, /not reachable/, "must refuse before polling ComfyUI");
});

test("absent --family and explicit 'sdxl' both build the generic SDXL graph", () => {
  for (const argv of [["o.png", "p"], ["o.png", "p", "--family", "sdxl"], ["o.png", "p", "--family", ""]]) {
    const { graph } = build(argv);
    assert.ok(classes(graph).includes("CheckpointLoaderSimple"), argv.join(" "));
  }
});

test("the existing families still dispatch to their own builders", () => {
  assert.ok(classes(build(["o.png", "p", "--family", "krea2", "--ckpt", "k.safetensors"]).graph).includes("UNETLoader"));
  assert.ok(classes(build(["o.png", "p", "--family", "qwen-image", "--ckpt", "q.gguf"]).graph).includes("EmptySD3LatentImage"));
  assert.ok(classes(build(["o.png", "p", "--family", "hidream-o1", "--ckpt", "h.safetensors"]).graph).includes("EmptyHiDreamO1LatentImage"));
});

test("--graph bypasses the family check and posts the caller's workflow", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-render-"));
  const f = join(dir, "wf.json");
  writeFileSync(f, JSON.stringify({ "1": { class_type: "X", inputs: {} } }));
  const { graph } = build(["o.png", "--graph", f, "--family", "whatever"]);
  assert.deepEqual(graph, { "1": { class_type: "X", inputs: {} } });
});

test("qwen-image-2.1: files, official schedule, native 2048 default, opaque output", () => {
  const { graph, width, height, seed } = build(QI21);
  assert.equal(seed, 42);
  assert.equal(width, 2048);
  assert.equal(height, 2048);
  assert.deepEqual(one(graph, "UNETLoader"), { unet_name: "qwen_image_2.1_bf16.safetensors", weight_dtype: "default" });
  assert.deepEqual(one(graph, "CLIPLoader"), { clip_name: "qwen3vl_8b_bf16.safetensors", type: "qwen_image", device: "default" });
  assert.deepEqual(one(graph, "VAELoader"), { vae_name: "qwen_image_2.1_vae_bf16.safetensors" });
  assert.ok(classes(graph).includes("ManualSigmas"), "official is the default schedule");
  assert.ok(classes(graph).includes("SplitImageWithAlpha"), "opaque is the default");
  assert.equal(one(graph, "TextEncodeQwenImage21").prompt, "a glass bottle");
});

test("qwen-image-2.1: --schedule comfy, --transparent, snapped dims, steps+cfg pair", () => {
  const c = build([...QI21, "--schedule", "comfy", "--steps", "30", "--cfg", "1"]).graph;
  const ks = one(c, "KSampler");
  assert.equal(ks.steps, 30);
  assert.equal(ks.scheduler, "simple");
  assert.ok(!classes(c).includes("ManualSigmas"));
  const t = build([...QI21, "--transparent", "1"]).graph;
  assert.ok(!classes(t).includes("SplitImageWithAlpha"));
  assert.equal(one(t, "TextEncodeQwenImage21").prompt, rgbaPrompt("a glass bottle"));
  assert.ok(classes(build([...QI21, "--transparent", "false"]).graph).includes("SplitImageWithAlpha"));
  const s = build([...QI21, "--width", "2050", "--height", "1030"]);
  assert.equal(s.width, 2048, "the reported size is the snapped one");
  assert.equal(s.height, 1024);
  assert.deepEqual(one(s.graph, "EmptyLatentImage"), { width: 2048, height: 1024, batch_size: 1 });
});

test("qwen-image-2.1 refusals: clip, vae, builtin vae, gguf, pool flags, half pair, bad enums", () => {
  const without = (flag) => {
    const a = [...QI21];
    const i = a.indexOf(flag);
    a.splice(i, 2);
    return a;
  };
  const cases = [
    [without("--clip"), /requires --clip/],
    [without("--vae"), /requires --vae/],
    [without("--ckpt"), /requires --ckpt/],
    [[...QI21, "--vae", "builtin"], /no built-in VAE/],
    [[...QI21, "--ckpt", "qwen-image-2.1-Q8_0.gguf"], /no GGUF loader/],
    [[...QI21, "--pool-vvram", "12"], /no pooled loader wired \(--pool-vvram\)/],
    [[...QI21, "--pool-compute", "cuda:1"], /no pooled loader wired \(--pool-compute\)/],
    [[...QI21, "--pool-donor", "cuda:2"], /no pooled loader wired \(--pool-donor\)/],
    [[...QI21, "--steps", "20"], /--steps and --cfg together/],
    [[...QI21, "--cfg", "2"], /--steps and --cfg together/],
    [[...QI21, "--schedule", "karras"], /--schedule must be one of official\|comfy/],
    [[...QI21, "--transparent", "maybe"], /--transparent takes/],
    [[...QI21, "--width", "0"], /positive number/],
  ];
  for (const [argv, re] of cases) {
    assert.throws(() => build(argv), (e) => e instanceof UsageError && re.test(e.message), `${argv.slice(-2).join(" ")} -> ${re}`);
  }
  // COMFY_CKPT / COMFY_VAE env still stand in for the flags, like every other family.
  const envOnly = without("--ckpt").filter((x, i, a) => !(x === "--vae" || a[i - 1] === "--vae"));
  const { graph } = build(envOnly, { COMFY_CKPT: "qwen_image_2.1_int8_convrot.safetensors", COMFY_VAE: "qwen_image_2.1_vae_bf16.safetensors" });
  assert.equal(one(graph, "UNETLoader").unet_name, "qwen_image_2.1_int8_convrot.safetensors");
});

test("parseBoolFlag accepts the usual spellings and refuses anything else", () => {
  for (const v of ["1", "true", "TRUE", "yes", "on"]) assert.equal(parseBoolFlag("x", v), true);
  for (const v of [undefined, "", "0", "false", "no", "off"]) assert.equal(parseBoolFlag("x", v), false);
  assert.throws(() => parseBoolFlag("x", "2"), UsageError);
});

test("parseRenderArgs: no-lock/keep-comfy/no-lifecycle are booleans, trailing or mid-argv (gap 4 lifecycle flags)", () => {
  // Trailing: comfy-generate.mjs appends --no-lifecycle after every other flag.
  let { pos, flags } = parseRenderArgs(["out.png", "a prompt", "--seed", "42", "--no-lifecycle"]);
  assert.equal(flags["no-lifecycle"], true);
  assert.equal(flags.seed, "42");
  assert.deepEqual(pos, ["out.png", "a prompt"]);
  // Mid-argv, followed by a value flag: that flag must keep its value, not get
  // swallowed as the bool flag's "value" (the exact bug comfy-video.mjs's own
  // BOOL_FLAGS list was created to fix).
  ({ pos, flags } = parseRenderArgs(["out.png", "p", "--no-lock", "--family", "qwen-image"]));
  assert.equal(flags["no-lock"], true);
  assert.equal(flags.family, "qwen-image");
  assert.deepEqual(pos, ["out.png", "p"], "the family name must not become a positional");
  ({ pos, flags } = parseRenderArgs(["out.png", "p", "--keep-comfy", "--ckpt", "x.safetensors"]));
  assert.equal(flags["keep-comfy"], true);
  assert.equal(flags.ckpt, "x.safetensors");
  assert.deepEqual(BOOL_FLAGS, ["no-lock", "keep-comfy", "no-lifecycle"]);
});

test("standalone (no --no-lifecycle): self-manages via withGpuSlot — refuses fast with no GPU lease, before any ComfyUI network call", () => {
  // Gap 4: comfy-render.mjs used to have NO lifecycle at all, so this exact invocation
  // (no --no-lifecycle) used to hang waiting on waitServer() against an address nothing
  // answers on. Now withGpuSlot's own "no lease, no --no-lock" refusal fires immediately
  // — proof the self-managed lifecycle is actually wired into main(), without needing a
  // real ComfyUI or a slow poll budget in this test.
  const dir = mkdtempSync(join(tmpdir(), "comfy-render-"));
  const env = { ...process.env };
  delete env.GPU_LEASE_DIR; delete env.GPU_LEASE_EPOCH; delete env.GPU_LEASE_CLASS;
  const r = spawnSync(process.execPath, [SCRIPT, join(dir, "o.png"), "a prompt", "--api", "http://127.0.0.1:9"],
    { encoding: "utf8", timeout: 15000, env });
  assert.equal(r.status, 1, `exit ${r.status}; stderr: ${r.stderr}`);
  assert.match(r.stderr, /GPU lease missing/);
});

test("--no-lifecycle: skips withGpuSlot entirely — unset lease does not block it (falls through to waitServer instead)", () => {
  // The inverse proof: with --no-lifecycle, the SAME missing-lease condition must NOT
  // refuse (a batch child must never need its own lease — the parent already has one).
  // It instead reaches waitServer()'s poll loop against the same bad API, which has no
  // short-circuit for "nothing is listening" — so this test bounds the wait itself
  // (a short spawnSync timeout) rather than letting waitServer's own ~3min budget run;
  // a forced kill (signal set / ETIMEDOUT) after that short window is itself the proof
  // the process was still alive and polling, not refused-and-exited like the sibling test.
  const dir = mkdtempSync(join(tmpdir(), "comfy-render-"));
  const env = { ...process.env };
  delete env.GPU_LEASE_DIR; delete env.GPU_LEASE_EPOCH; delete env.GPU_LEASE_CLASS;
  const r = spawnSync(process.execPath,
    [SCRIPT, join(dir, "o.png"), "a prompt", "--api", "http://127.0.0.1:9", "--no-lifecycle"],
    { encoding: "utf8", timeout: 3000, env });
  assert.ok(r.signal || (r.error && r.error.code === "ETIMEDOUT"), `expected the process to still be running (polling) when killed, got status=${r.status} signal=${r.signal} error=${r.error}`);
  assert.doesNotMatch(r.stderr || "", /GPU lease missing/, "must not require a lease under --no-lifecycle");
});
