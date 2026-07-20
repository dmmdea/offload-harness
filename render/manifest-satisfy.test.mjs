import { test } from "node:test";
import assert from "node:assert/strict";
import { satisfyModels, defaultSatisfyDeps, parseExtraModelPaths, modelCandidates } from "./manifest-satisfy.mjs";
import { mkdtempSync, existsSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { satisfyManifest, buildHostConstraints, publicPin } from "./manifest-satisfy.mjs";
import { parseManifest } from "./manifest.mjs";
import { buildCompileCmd, buildInstallCmd, buildEnsurePackCmd, buildClonePackCmd, makeEnsurePack } from "./manifest-satisfy.mjs";

const base = {
  comfyDir: "C:/ComfyUI",
  exists: () => false, sentinelOk: () => false, writeSentinel: () => {},
  download: async () => {}, sha256: async () => "HASH",
};

test("present + sentinel-ok model is skipped", async () => {
  const r = await satisfyModels([{ path: "m/x", source_url: "u", sha256: "HASH" }],
    { ...base, exists: () => true, sentinelOk: () => true });
  assert.deepEqual(r, { ok: true, unverified: [] });
});

test("missing model with matching sha downloads + verifies", async () => {
  let dl = 0;
  const r = await satisfyModels([{ path: "m/x", source_url: "u", sha256: "HASH" }],
    { ...base, download: async () => { dl++; }, sha256: async () => "HASH" });
  assert.equal(dl, 1); assert.equal(r.ok, true);
});

test("sha mismatch DEFERs MODEL_SHA_MISMATCH", async () => {
  const r = await satisfyModels([{ path: "m/x", source_url: "u", sha256: "WANT" }],
    { ...base, sha256: async () => "GOT" });
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "MODEL_SHA_MISMATCH");
  assert.equal(r.defer.ref, "m/x");
});

test("null-sha model downloads and is reported unverified", async () => {
  const r = await satisfyModels([{ path: "m/x", source_url: "u", sha256: null }], base);
  assert.equal(r.ok, true);
  assert.deepEqual(r.unverified, ["m/x"]);
});

test("no source_url for a missing model DEFERs MODEL_DOWNLOAD_FAILED", async () => {
  const r = await satisfyModels([{ path: "m/x", source_url: "", sha256: null }], base);
  assert.equal(r.defer.code, "MODEL_DOWNLOAD_FAILED");
});

// --- extra_model_paths.yaml awareness (models living outside comfyDir/models) ---
// Live defect (CMP session, 2026-07-19): presence resolved ONLY under comfyDir, so a
// model on the V: Optane tree (registered via ComfyUI's extra_model_paths.yaml) read as
// MISSING and was re-downloaded to C: — 15GB per run.

test("parseExtraModelPaths maps a simple category onto base_path", () => {
  const roots = parseExtraModelPaths(`qube_optane:\n    base_path: V:/models/\n    checkpoints: checkpoints\n`);
  assert.deepEqual(roots.checkpoints, ["V:/models/checkpoints"]);
});

test("parseExtraModelPaths expands a block scalar into MULTIPLE dirs", () => {
  // ComfyUI's real shape: `unet: |` with two physical dirs beneath it.
  const roots = parseExtraModelPaths(`qube_optane:\n    base_path: V:/models/\n    unet: |\n        diffusion_models\n        unet\n`);
  assert.deepEqual(roots.unet, ["V:/models/diffusion_models", "V:/models/unet"]);
});

test("parseExtraModelPaths ignores comments/blanks and keeps an absolute category path", () => {
  const roots = parseExtraModelPaths(`# a comment\n\nprov:\n    base_path: V:/models/\n    # another\n    vae: D:/elsewhere/vae\n`);
  assert.deepEqual(roots.vae, ["D:/elsewhere/vae"]);
});

test("parseExtraModelPaths fails SAFE on garbage (never throws)", () => {
  assert.deepEqual(parseExtraModelPaths("%%% not yaml ::: ["), {});
  assert.deepEqual(parseExtraModelPaths(""), {});
  assert.deepEqual(parseExtraModelPaths(null), {});
});

test("modelCandidates falls back to comfyDir alone when no extra roots", () => {
  assert.deepEqual(modelCandidates("models/unet/x.gguf", "C:/ComfyUI", {}),
    [join("C:/ComfyUI", "models/unet/x.gguf")]);
});

test("modelCandidates adds every registered dir for the path's category", () => {
  const got = modelCandidates("models/unet/x.gguf", "C:/ComfyUI",
    { unet: ["V:/m/diffusion_models", "V:/m/unet"] });
  assert.deepEqual(got, [
    join("C:/ComfyUI", "models/unet/x.gguf"),
    join("V:/m/diffusion_models", "x.gguf"),
    join("V:/m/unet", "x.gguf"),
  ]);
});

test("modelCandidates preserves nested subpaths under the category", () => {
  const got = modelCandidates("models/loras/sub/y.safetensors", "C:/ComfyUI", { loras: ["V:/m/loras"] });
  assert.equal(got[1], join("V:/m/loras", "sub/y.safetensors"));
});

test("modelCandidates ignores unknown categories and non-models paths", () => {
  assert.equal(modelCandidates("models/nope/x", "C:/ComfyUI", { unet: ["V:/m/unet"] }).length, 1);
  assert.equal(modelCandidates("custom/x", "C:/ComfyUI", { unet: ["V:/m/unet"] }).length, 1);
});

test("model present ONLY under an extra root is NOT re-downloaded", async () => {
  // The exact CMP defect: file lives on V:, absent from C: — must be seen as present.
  const onV = join("V:/m/unet", "x.gguf");
  let dl = 0;
  const r = await satisfyModels([{ path: "models/unet/x.gguf", source_url: "u", sha256: "HASH" }], {
    ...base,
    extraRoots: { unet: ["V:/m/unet"] },
    exists: (p) => p === onV,
    sentinelOk: (p) => p === onV,
    download: async () => { dl++; },
  });
  assert.equal(dl, 0, "must not download a model already present on the extra root");
  assert.deepEqual(r, { ok: true, unverified: [] });
});

// --- pre-provisioned file adoption (sentinel written after a one-time hash) ---
// Live defect (CMP session): a hand/curl-provisioned file with a byte-correct sha but no
// .sha-ok sidecar FAILED the skip gate and fell into the DOWNLOAD branch — re-downloading
// instead of adopting it.

test("present pinned-sha file WITHOUT a sentinel is adopted, not re-downloaded", async () => {
  let dl = 0, wrote = null;
  const r = await satisfyModels([{ path: "m/x", source_url: "u", sha256: "HASH" }], {
    ...base,
    exists: () => true,
    sentinelOk: () => false,          // pre-provisioned: no sidecar
    sha256: async () => "HASH",       // but the bytes are correct
    download: async () => { dl++; },
    writeSentinel: (p, h) => { wrote = [p, h]; },
  });
  assert.equal(dl, 0, "a byte-correct present file must be adopted, never re-downloaded");
  assert.deepEqual(wrote, [join("C:/ComfyUI", "m/x"), "HASH"], "adoption writes the sentinel");
  assert.equal(r.ok, true);
});

test("adoption writes the sentinel BESIDE the file it actually found (extra root)", async () => {
  const onV = join("V:/m/vae", "v.safetensors");
  let wrote = null;
  await satisfyModels([{ path: "models/vae/v.safetensors", source_url: "u", sha256: "HASH" }], {
    ...base,
    extraRoots: { vae: ["V:/m/vae"] },
    exists: (p) => p === onV,
    sentinelOk: () => false,
    sha256: async () => "HASH",
    writeSentinel: (p, h) => { wrote = [p, h]; },
  });
  assert.deepEqual(wrote, [onV, "HASH"], "sentinel must land next to the V: file, not the C: path");
});

test("present file whose hash MISMATCHES the pin is re-downloaded", async () => {
  let dl = 0, calls = 0;
  const r = await satisfyModels([{ path: "m/x", source_url: "u", sha256: "WANT" }], {
    ...base,
    exists: () => true,
    sentinelOk: () => false,
    // 1st call = the present file (wrong bytes); 2nd = the post-download verify (correct).
    sha256: async () => (++calls === 1 ? "GOT" : "WANT"),
    download: async () => { dl++; },
  });
  assert.equal(dl, 1, "a corrupt/wrong present file must be replaced");
  assert.equal(r.ok, true);
});

test("present mismatching file with NO source_url DEFERs MODEL_SHA_MISMATCH", async () => {
  const r = await satisfyModels([{ path: "m/x", source_url: "", sha256: "WANT" }], {
    ...base, exists: () => true, sentinelOk: () => false, sha256: async () => "GOT",
  });
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "MODEL_SHA_MISMATCH", "must name the real problem, not 'missing on disk'");
});

const okDeps = {
  comfyDir: "C:/ComfyUI",
  exists: () => true, sentinelOk: () => true, writeSentinel: () => {},
  download: async () => {}, sha256: async () => "HASH",
  satisfierAvailable: () => true, comfyVersion: async () => "0.23.5",
  resolveDeps: async () => {}, ensurePack: async () => ({ changed: false }),
  pipCheck: async () => ({ ok: true }),
};
const M = parseManifest({
  comfyui_min_version: "0.23.0",
  node_packs: [{ name: "A", repo: "r", commit: "c" }],
  models: [{ path: "m/x", source_url: "u", sha256: "HASH" }],
});

test("fully satisfied → ok, no change", async () => {
  const r = await satisfyManifest(M, okDeps);
  assert.equal(r.ok, true); assert.equal(r.changed, false);
});

test("DEFER SATISFIER_UNAVAILABLE when tooling missing", async () => {
  const r = await satisfyManifest(M, { ...okDeps, satisfierAvailable: () => false });
  assert.equal(r.defer.code, "SATISFIER_UNAVAILABLE");
});

test("DEFER COMFY_VERSION_BELOW_MIN", async () => {
  const r = await satisfyManifest(M, { ...okDeps, comfyVersion: async () => "0.22.0" });
  assert.equal(r.defer.code, "COMFY_VERSION_BELOW_MIN");
});

test("DEFER VENV_INCOHERENT when pip check fails", async () => {
  const r = await satisfyManifest(M, { ...okDeps, pipCheck: async () => ({ ok: false, reason: "conflicting installed dependencies" }) });
  assert.equal(r.defer.code, "VENV_INCOHERENT");
});

test("VENV_INCOHERENT surfaces the pipCheck reason (host-pin drift is actionable, not opaque)", async () => {
  // A pack that moved a host-pinned package (e.g. torch) must report WHICH package drifted in
  // the defer detail — a consuming workflow can't act on a generic "conflicting dependencies".
  const drift = "host-pin drift — a pinned package moved during provisioning: expected [torch==2.11.0] got [torch==2.13.0]";
  const r = await satisfyManifest(M, { ...okDeps, pipCheck: async () => ({ ok: false, reason: drift }) });
  assert.equal(r.defer.code, "VENV_INCOHERENT");
  assert.equal(r.defer.detail, drift);
});

// Regression (live acceptance 2026-07-17): an empty/models-only manifest must NOT invoke the
// pack satisfier (cm-cli). Previously it ran `cm-cli install --uv` with zero refs and DEFERed.
test("empty manifest (no packs) skips the satisfier entirely — no cm-cli", async () => {
  const empty = parseManifest({ node_packs: [], models: [] });
  let touched = false;
  const spy = {
    ...okDeps,
    satisfierAvailable: () => { touched = true; return false; },
    resolveDeps: async () => { touched = true; },
    pipCheck: async () => { touched = true; return { ok: false, reason: "x" }; },
  };
  const r = await satisfyManifest(empty, spy);
  assert.equal(r.ok, true); assert.equal(r.changed, false);
  assert.equal(touched, false, "no pack tooling should run for an empty manifest");
});

test("models-only manifest satisfies without the pack satisfier present", async () => {
  const mo = parseManifest({ node_packs: [], models: [{ path: "m/y", source_url: "u", sha256: "HASH" }] });
  const r = await satisfyManifest(mo, { ...okDeps, satisfierAvailable: () => false });
  assert.equal(r.ok, true);
});

// NODE_CLASS_MISSING moved OUT of satisfyManifest to the orchestrator's post-start preflight
// (satisfy is now disk-only). That branch is covered in comfy-run-graph.test.mjs.

test("changed=true propagates from a pack checkout", async () => {
  const r = await satisfyManifest(M, { ...okDeps, ensurePack: async () => ({ changed: true }) });
  assert.equal(r.ok, true); assert.equal(r.changed, true);
});

test("buildCompileCmd: ONE uv compile across ALL packs' requirements (never per-pack)", () => {
  const cmd = buildCompileCmd(["A/requirements.txt", "B/requirements.txt"],
    { uvExe: "UV", comfyPy: "PY", lockPath: "LOCK" });
  assert.equal(cmd[0], "UV");
  assert.deepEqual(cmd.slice(1, 3), ["pip", "compile"]);
  assert.ok(cmd.includes("A/requirements.txt") && cmd.includes("B/requirements.txt"), "both reqs in one invocation");
  assert.deepEqual(cmd.slice(-2), ["-o", "LOCK"]);
});

test("buildInstallCmd installs the unified lock with the venv python", () => {
  assert.deepEqual(buildInstallCmd("LOCK", { comfyPy: "PY" }), ["PY", "-m", "pip", "install", "-r", "LOCK"]);
});

test("buildEnsurePackCmd checks out the pinned commit", () => {
  const cmd = buildEnsurePackCmd({ name: "A", repo: "https://x/a", commit: "deadbeef" }, { comfyDir: "C:/ComfyUI" });
  assert.match(cmd.join(" "), /deadbeef/);
  assert.match(cmd.join(" "), /custom_nodes/);
});

// #5 arg-injection: a leading-dash commit could be read as a git flag → reject it.
test("buildEnsurePackCmd rejects a leading-dash (option-injection) commit", () => {
  assert.throws(() => buildEnsurePackCmd({ name: "A", commit: "--upload-pack=evil" }, { comfyDir: "C:/ComfyUI" }), /unsafe commit ref/);
});
test("buildEnsurePackCmd rejects a commit with shell/ref-unsafe chars", () => {
  assert.throws(() => buildEnsurePackCmd({ name: "A", commit: "a;rm -rf" }, { comfyDir: "C:/ComfyUI" }), /unsafe commit ref/);
});

// #5: clone neutralizes a leading-dash repo with a literal `--` before the positionals.
test("buildClonePackCmd inserts -- so a leading-dash repo is a positional, not a flag", () => {
  const cmd = buildClonePackCmd({ name: "A", repo: "--upload-pack=evil" }, { comfyDir: "C:/ComfyUI" });
  const dd = cmd.indexOf("--");
  assert.ok(dd > 0, "-- present");
  assert.ok(cmd.indexOf("--upload-pack=evil") > dd, "repo comes AFTER the -- terminator");
});

// #3 drift: `changed` is decided by the git HEAD before-vs-after, not "did the dir exist".
test("makeEnsurePack: HEAD moved on checkout → changed:true", async () => {
  let head = "aaaaaaa";
  const ep = makeEnsurePack({
    comfyDir: "C:/ComfyUI", exists: () => true,
    gitHead: async () => head,
    gitRun: async (cmd) => { if (cmd.includes("checkout")) head = "bbbbbbb"; }, // checkout moves HEAD
  });
  const r = await ep({ name: "A", repo: "r", commit: "bbbbbbb" });
  assert.equal(r.changed, true);
});
test("makeEnsurePack: HEAD unchanged (same-commit checkout) → changed:false", async () => {
  const ep = makeEnsurePack({
    comfyDir: "C:/ComfyUI", exists: () => true,
    gitHead: async () => "samehead", gitRun: async () => {},
  });
  const r = await ep({ name: "A", repo: "r", commit: "samehead" });
  assert.equal(r.changed, false);
});
test("makeEnsurePack: fresh clone (dir absent) → changed:true, clones once", async () => {
  let cloned = 0;
  const ep = makeEnsurePack({
    comfyDir: "C:/ComfyUI", exists: () => false,
    gitHead: async () => "h", gitRun: async (cmd) => { if (cmd.includes("clone")) cloned++; },
  });
  const r = await ep({ name: "A", repo: "r", commit: "c" });
  assert.equal(r.changed, true);
  assert.equal(cloned, 1);
});

// #9 nameless pack: parseManifest derives a filesystem-safe dir name from the repo URL.
test("parseManifest derives a safe pack name from the repo when name is absent", () => {
  const m = parseManifest({ node_packs: [{ repo: "https://github.com/x/ComfyUI-RMBG", commit: "c" }] });
  assert.equal(m.node_packs[0].name, "ComfyUI-RMBG");
  assert.ok(!m.node_packs[0].name.includes("/"));
  assert.ok(!m.node_packs[0].name.includes(":"));
});
test("parseManifest strips a trailing .git from the derived pack name", () => {
  const m = parseManifest({ node_packs: [{ repo: "https://github.com/x/ComfyUI-RMBG.git", commit: "c" }] });
  assert.equal(m.node_packs[0].name, "ComfyUI-RMBG");
});

// --- host-constraints (v1 protection: pack provisioning must never move the CUDA stack) ---
test("buildHostConstraints extracts only protected host pins, +cu local versions preserved", () => {
  const freeze = [
    "aiohttp==3.12.0", "torch==2.11.0+cu128", "torchvision==0.26.0+cu128",
    "numpy==2.1.2", "transformers==4.44.0", "torchaudio==2.11.0+cu128", "requests==2.32.0",
  ].join("\n");
  assert.deepEqual(buildHostConstraints(freeze), [
    "torch==2.11.0+cu128", "torchvision==0.26.0+cu128", "numpy==2.1.2", "torchaudio==2.11.0+cu128",
  ]);
});

test("buildHostConstraints is case-insensitive on the package name", () => {
  assert.deepEqual(buildHostConstraints("Torch==2.13.0\nNumPy==2.4.6"), ["Torch==2.13.0", "NumPy==2.4.6"]);
});

test("buildHostConstraints returns [] for empty / no-match / non-pinned input", () => {
  assert.deepEqual(buildHostConstraints(""), []);
  assert.deepEqual(buildHostConstraints(null), []);
  assert.deepEqual(buildHostConstraints("requests==2.32.0\ntorch @ file:///wheel"), []);
});

test("publicPin strips a PEP 440 local-version suffix, leaves public pins alone", () => {
  assert.equal(publicPin("torch==2.11.0+cu128"), "torch==2.11.0");
  assert.equal(publicPin("numpy==2.1.2"), "numpy==2.1.2");
  assert.equal(publicPin("torchvision==0.26.0+rocm6.2"), "torchvision==0.26.0");
});

// Regression: defaultSatisfyDeps is the production glue, previously untested. Its
// writeSentinel/download closures called require("node:fs") inside this ESM module,
// which throws "require is not defined" — a present-but-unsentineled model deferred
// MODEL_DOWNLOAD_FAILED, and a fresh download crashed the process. These exercise the
// real closures.
function realDeps() {
  const dir = mkdtempSync(join(tmpdir(), "satisfy-real-"));
  const deps = defaultSatisfyDeps({ comfyDir: dir, comfyPy: join(dir, "python"), api: "http://127.0.0.1:0", cmCli: "" });
  return { dir, deps };
}

test("defaultSatisfyDeps.writeSentinel writes the sidecar (no require in ESM)", () => {
  const { dir, deps } = realDeps();
  try {
    const model = join(dir, "model.safetensors");
    deps.writeSentinel(model, "deadbeef");
    assert.ok(existsSync(model + ".sha-ok"), "sentinel sidecar written");
    assert.equal(readFileSync(model + ".sha-ok", "utf8"), "deadbeef");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("defaultSatisfyDeps.download streams the body to disk (no >2GB Buffer cap)", async () => {
  // The response body is consumed as a STREAM (Readable.fromWeb), not buffered whole via
  // arrayBuffer() — that buffering threw ">2GB length" on Node's Buffer cap, so a >~2GB model
  // (Qwen-Image-Edit GGUF, RealVisXL) could never download. A mock that only exposes `body`
  // (and no arrayBuffer) proves the code takes the streaming path.
  const { dir, deps } = realDeps();
  const bytes = new TextEncoder().encode("model-bytes");
  const origFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    body: new ReadableStream({ start(c) { c.enqueue(bytes); c.close(); } }),
  });
  try {
    const dest = join(dir, "nested", "sub", "model.bin"); // dirname must be mkdir'd
    await deps.download("http://example/model.bin", dest);
    assert.ok(existsSync(dest), "downloaded file written into created dirs");
    assert.equal(readFileSync(dest, "utf8"), "model-bytes");
  } finally {
    globalThis.fetch = origFetch;
    rmSync(dir, { recursive: true, force: true });
  }
});
