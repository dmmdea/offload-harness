// node --test render/sdcpp-video.test.mjs
// sdcpp-video.mjs: the OUR-flags -> `sd-cli -M vid_gen` argv mapping, the 4k+1 / %32
// normalization and the no-CPU refusal at the script's own door. No spawn of a real engine.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { buildSdVideoArgs, resolveParams, splitPositionals, missingFlags, parseArgs, vaeTileOverlap } from "./sdcpp-video.mjs";

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
  const a = buildSdVideoArgs({ outFile: "/tmp/o.webm", still: "/in/still.png", prompt: "a calm sea", flags, extra: ["--some-extra-flag"] });
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
  assert.ok(a.indexOf("--some-extra-flag") > a.indexOf("-v"));
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

// What parseArgs hands over for a flag given with no value is `undefined` (a key that is present), and a
// numeric option may arrive as "" or text: none of it may reach sd-cli as an option or as the word "undefined".
const junkFree = (a, what) => {
  for (const x of a) assert.ok(!/undefined|NaN|null/.test(String(x)), `${what}: ${JSON.stringify(x)} must not reach the argv`);
};

test("buildSdVideoArgs: options that are unset, empty or not a number never reach the argv - including the high-noise recipe of a bound high-noise model", () => {
  const unset = {
    ...baseFlags, negative: undefined, cfg: undefined, steps: undefined, sampler: undefined, "flow-shift": undefined, seed: undefined, tae: undefined,
    "high-noise-model": "/m/high.gguf", "high-noise-cfg": undefined, "high-noise-steps": undefined, "high-noise-sampler": undefined,
  };
  const empty = { ...unset, negative: "", cfg: "", steps: "", sampler: "", "flow-shift": "", seed: "", tae: "", "high-noise-cfg": "", "high-noise-steps": "", "high-noise-sampler": "" };
  const text = { ...unset, cfg: "abc", steps: "x", "flow-shift": "y", seed: "z", "high-noise-cfg": "n/a", "high-noise-steps": "-" };
  for (const [name, flags] of [["unset", unset], ["empty", empty], ["not a number", text]]) {
    const a = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags });
    junkFree(a, name);
    assert.ok(a.includes("--high-noise-diffusion-model"), `${name}: the bound high-noise model is passed`);
    for (const f of ["-n", "--cfg-scale", "--steps", "--sampling-method", "--flow-shift", "-s", "--taesd", "--high-noise-cfg-scale", "--high-noise-steps", "--high-noise-sampling-method"]) {
      assert.ok(!a.includes(f), `${name}: ${f} must be omitted`);
    }
  }
});

test("buildSdVideoArgs: frames are normalized to 4k+1 and width/height to multiples of 32 in the argv", () => {
  const a = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...baseFlags, frames: "50", width: "854", height: "485" } });
  assert.equal(a[a.indexOf("--video-frames") + 1], "49");
  assert.equal(a[a.indexOf("-W") + 1], "832");
  assert.equal(a[a.indexOf("-H") + 1], "480");
  const b = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...baseFlags, frames: "81" } });
  assert.equal(b[b.indexOf("--video-frames") + 1], "81");
});

test("buildSdVideoArgs: the A14B high-noise expert's own recipe is mapped (and only with a high-noise model)", () => {
  const flags = { ...baseFlags, "high-noise-model": "/m/high.gguf", cfg: "1", "high-noise-cfg": "1", "high-noise-steps": "2", "high-noise-sampler": "euler" };
  const a = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags });
  const at = (k) => a[a.indexOf(k) + 1];
  assert.equal(at("--high-noise-cfg-scale"), "1");
  assert.equal(at("--high-noise-steps"), "2");
  assert.equal(at("--high-noise-sampling-method"), "euler");
  // without the high-noise model the keys name nothing: sd-cli would reject them
  const b = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...flags, "high-noise-model": "" } });
  for (const f of ["--high-noise-cfg-scale", "--high-noise-steps", "--high-noise-sampling-method"]) assert.ok(!b.includes(f), f);
  // a pair with no high-noise recipe leaves sd-cli's own defaults
  const c = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...baseFlags, "high-noise-model": "/m/high.gguf" } });
  assert.ok(!c.includes("--high-noise-cfg-scale"));
});

test("buildSdVideoArgs: --vae-tile-overlap 0.25 by default (the measured best decode), overridable; --taesd only when a TAE is bound", () => {
  const a = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: baseFlags });
  assert.equal(a[a.indexOf("--vae-tile-overlap") + 1], "0.25");
  assert.ok(a.includes("--vae-tiling"));
  assert.ok(!a.includes("--taesd"));
  const b = buildSdVideoArgs({ outFile: "o.webm", still: "", prompt: "p", flags: { ...baseFlags, "vae-tile-overlap": "0.5", tae: "/m/taew2_2.safetensors" } });
  assert.equal(b[b.indexOf("--vae-tile-overlap") + 1], "0.5");
  assert.equal(b[b.indexOf("--taesd") + 1], "/m/taew2_2.safetensors");
  assert.equal(vaeTileOverlap({}), 0.25);
  assert.equal(vaeTileOverlap({ "vae-tile-overlap": "0" }), 0);
  for (const bad of ["1", "-0.1", "2"]) assert.throws(() => vaeTileOverlap({ "vae-tile-overlap": bad }), /\[0, 1\)/);
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

test("the -- terminator: a prompt that starts with -- is a positional, flags after the positionals still parse", () => {
  const { pos, flags } = parseArgs(["--backend", "vulkan0", "--", "o.mp4", "--- Intro ---", "--not-a-flag"]);
  assert.deepEqual(pos, ["o.mp4", "--- Intro ---", "--not-a-flag"]);
  assert.equal(flags.backend, "vulkan0");
  assert.ok(!("not-a-flag" in flags));
});

test("the script refuses a bare binary name (BINARY_NOT_ABSOLUTE): one resolution rule, the harness passes the absolute path", () => {
  const r = run(["--sd-bin", "sd-cli", "--model", "x", "--vae", "x", "--t5xxl", "x", "--backend", "vulkan0", "--no-lock", "--", "out.mp4", "p"]);
  assert.equal(r.status, 1);
  assert.match(r.stderr, /BINARY_NOT_ABSOLUTE/);
});

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

const videoBase = ["out.mp4", "p", "--sd-bin", "/no/such/sd-cli", "--model", "/no/such/m", "--vae", "/no/such/v", "--t5xxl", "/no/such/t", "--backend", "vulkan0", "--no-lock"];

test("the script refuses a request over the token cap BEFORE any file check or spawn (TOKEN_CAP_EXCEEDED, exit 1); no cap = no check", () => {
  // 832x480x49 on a 16x VAE is 5070 latent tokens
  const over = run([...videoBase, "--max-tokens", "5000", "--vae-stride", "16"]);
  assert.equal(over.status, 1, over.stderr);
  assert.match(over.stderr, /TOKEN_CAP_EXCEEDED/);
  assert.match(over.stderr, /needs 5070 latent tokens/);
  assert.match(over.stderr, /cap is 5000/);
  assert.doesNotMatch(over.stderr, /not found/, "the cap is checked before the file checks, so before any spawn");
  // exactly at the cap passes the cap and moves on to the (missing) files
  const at = run([...videoBase, "--max-tokens", "5070", "--vae-stride", "16"]);
  assert.match(at.stderr, /not found/);
  assert.doesNotMatch(at.stderr, /TOKEN_CAP_EXCEEDED/);
  // no cap configured: a huge request is not checked
  const none = run([...videoBase, "--width", "1920", "--height", "1088", "--frames", "121"]);
  assert.match(none.stderr, /not found/);
  // a cap without a stride is a usage error, not a silent pass
  const noStride = run([...videoBase, "--max-tokens", "5000"]);
  assert.equal(noStride.status, 1);
  assert.match(noStride.stderr, /--vae-stride must be 8 or 16/);
});

test("the script refuses extra args that change the backend or placement (EXTRA_ARGS_REFUSED, exit 1) before any spawn", () => {
  for (const extra of [["--backend", "cpu"], ["--clip-on-cpu"], ["--vae-on-cpu"], ["--offload-to-cpu", "--clip-on-cpu"], ["--params-backend", "cpu"], ["-b", "vulkan0"]]) {
    const r = run([...videoBase, "--extra-args", JSON.stringify(extra)]);
    assert.equal(r.status, 1, `${extra}: ${r.stderr}`);
    assert.match(r.stderr, /EXTRA_ARGS_REFUSED/);
    assert.doesNotMatch(r.stderr, /not found/);
  }
  const ok = run([...videoBase, "--extra-args", JSON.stringify(["--vae-tile-overlap", "0.25"])]);
  assert.match(ok.stderr, /not found/);
  // --offload-to-cpu is sanctioned spill (weights in RAM, compute on the GPU): it gets past the
  // screen to the file checks, and it does not mask a placement flag after it
  const spill = run([...videoBase, "--extra-args", JSON.stringify(["--offload-to-cpu"])]);
  assert.match(spill.stderr, /not found/);
  assert.doesNotMatch(spill.stderr, /EXTRA_ARGS_REFUSED/);
});
