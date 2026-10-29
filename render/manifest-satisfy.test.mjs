import { test } from "node:test";
import assert from "node:assert/strict";
import { satisfyModels, defaultSatisfyDeps, parseExtraModelPaths, modelCandidates } from "./manifest-satisfy.mjs";
import { mkdtempSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve, sep } from "node:path";
import { tmpdir } from "node:os";
import { satisfyManifest, buildHostConstraints, publicPin, isSpawnFailure, runRetryOnSpawn } from "./manifest-satisfy.mjs";
import { parseManifest } from "./manifest.mjs";
import { makeStub, stubAvailable } from "./igpu-stub.mjs";
import { scratchDir } from "./test-scratch.mjs";
import { buildCompileCmd, buildInstallCmd, buildClonePackCmd, makeEnsurePack } from "./manifest-satisfy.mjs";

const base = {
  comfyDir: "C:/ComfyUI",
  exists: () => false, sentinelOk: () => false, writeSentinel: () => {},
  download: async () => {}, sha256: async () => "HASH",
};

test("present + sentinel-ok model is skipped", async () => {
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: "HASH" }],
    { ...base, exists: () => true, sentinelOk: () => true });
  assert.deepEqual(r, { ok: true, unverified: [] });
});

test("missing model with matching sha downloads + verifies", async () => {
  let dl = 0;
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: "HASH" }],
    { ...base, download: async () => { dl++; }, sha256: async () => "HASH" });
  assert.equal(dl, 1); assert.equal(r.ok, true);
});

test("sha mismatch DEFERs MODEL_SHA_MISMATCH", async () => {
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: "WANT" }],
    { ...base, sha256: async () => "GOT" });
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "MODEL_SHA_MISMATCH");
  assert.equal(r.defer.ref, "models/checkpoints/x.safetensors");
});

test("null-sha model downloads and is reported unverified", async () => {
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: null }], base);
  assert.equal(r.ok, true);
  assert.deepEqual(r.unverified, ["models/checkpoints/x.safetensors"]);
});

test("no source_url for a missing model DEFERs MODEL_DOWNLOAD_FAILED", async () => {
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "", sha256: null }], base);
  assert.equal(r.defer.code, "MODEL_DOWNLOAD_FAILED");
});

// --- extra_model_paths.yaml awareness (models living outside comfyDir/models) ---
// Live defect (CMP session, 2026-07-19): presence resolved ONLY under comfyDir, so a
// model on the V: Optane tree (registered via ComfyUI's extra_model_paths.yaml) read as
// MISSING and was re-downloaded to C: — 15GB per run.

test("parseExtraModelPaths maps a simple category onto base_path", () => {
  const roots = parseExtraModelPaths(`<node-b>_optane:\n    base_path: V:/models/\n    checkpoints: checkpoints\n`);
  assert.deepEqual(roots.checkpoints, ["V:/models/checkpoints"]);
});

test("parseExtraModelPaths expands a block scalar into MULTIPLE dirs", () => {
  // ComfyUI's real shape: `unet: |` with two physical dirs beneath it.
  const roots = parseExtraModelPaths(`<node-b>_optane:\n    base_path: V:/models/\n    unet: |\n        diffusion_models\n        unet\n`);
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
  const r = await satisfyModels([{ path: "models/unet/x.gguf", source_url: "https://huggingface.co/x/y", sha256: "HASH" }], {
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
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: "HASH" }], {
    ...base,
    exists: () => true,
    sentinelOk: () => false,          // pre-provisioned: no sidecar
    sha256: async () => "HASH",       // but the bytes are correct
    download: async () => { dl++; },
    writeSentinel: (p, h) => { wrote = [p, h]; },
  });
  assert.equal(dl, 0, "a byte-correct present file must be adopted, never re-downloaded");
  assert.deepEqual(wrote, [join("C:/ComfyUI", "models/checkpoints/x.safetensors"), "HASH"], "adoption writes the sentinel");
  assert.equal(r.ok, true);
});

test("adoption writes the sentinel BESIDE the file it actually found (extra root)", async () => {
  const onV = join("V:/m/vae", "v.safetensors");
  let wrote = null;
  await satisfyModels([{ path: "models/vae/v.safetensors", source_url: "https://huggingface.co/x/y", sha256: "HASH" }], {
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
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: "WANT" }], {
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
  const r = await satisfyModels([{ path: "models/checkpoints/x.safetensors", source_url: "", sha256: "WANT" }], {
    ...base, exists: () => true, sentinelOk: () => false, sha256: async () => "GOT",
  });
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "MODEL_SHA_MISMATCH", "must name the real problem, not 'missing on disk'");
});

const okDeps = {
  comfyDir: "C:/ComfyUI",
  retryDelayMs: 1,          // never pay the production 500ms sleep in tests
  exists: () => true, sentinelOk: () => true, writeSentinel: () => {},
  download: async () => {}, sha256: async () => "HASH",
  satisfierAvailable: () => true, comfyVersion: async () => "0.23.5",
  resolveDeps: async () => {}, ensurePack: async () => ({ changed: false }),
  pipCheck: async () => ({ ok: true }),
  // These tests use stand-in packs ("r", "c"): the pack allowlist is exercised, with the real table,
  // in the tests at the end of this file and in node-packs.test.mjs.
  packAllowed: () => null,
};
const M = parseManifest({
  comfyui_min_version: "0.23.0",
  node_packs: [{ name: "A", repo: "r" }],
  models: [{ path: "models/checkpoints/x.safetensors", source_url: "https://huggingface.co/x/y", sha256: "HASH" }],
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

test("VENV_INCOHERENT surfaces the pipCheck reason (host constraint drift is actionable, not opaque)", async () => {
  // A pack that moved a protected host package (e.g. torch) must report WHICH package drifted in
  // the defer detail — a consuming workflow can't act on a generic "conflicting dependencies".
  const drift = "host constraint drift — a protected package moved during provisioning: expected [torch==2.11.0] got [torch==2.13.0]";
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
  const mo = parseManifest({ node_packs: [], models: [{ path: "models/checkpoints/y.safetensors", source_url: "https://huggingface.co/x/y", sha256: "HASH" }] });
  const r = await satisfyManifest(mo, { ...okDeps, satisfierAvailable: () => false });
  assert.equal(r.ok, true);
});

// --- spawn-failure classification (live CMP report 2026-07-20) ---
// A spawn-level failure ("spawn UNKNOWN"/ENOENT/EACCES - the subprocess never ran) was caught
// and relabeled VENV_INCOHERENT: "the check could not run" reported as "your venv is broken",
// pointing the operator at healthy torch pins. Spawn failures must be classified distinctly
// (SATISFIER_SPAWN_FAILED), retried once for the python-touching steps (CMP's was transient
// after a long batch), and - for a pack set the persisted marker proves was already satisfied -
// must not fail a previously-coherent env at all.

const spawnErr = () => Object.assign(new Error("spawn UNKNOWN"), { code: "UNKNOWN", syscall: "spawn" });
// M's packs key, for marker-based tests.
const M_KEY = (M.node_packs || []).map((p) => `${p.name}@`).sort().join("|"); // the commit is what ensurePack reports; the stand-ins report none

test("isSpawnFailure recognizes Node spawn errors and nothing else", () => {
  assert.equal(isSpawnFailure(spawnErr()), true);
  assert.equal(isSpawnFailure(Object.assign(new Error("spawn python ENOENT"), { code: "ENOENT", syscall: "spawn python" })), true);
  // The message-only fallback (wrapped/re-thrown errors that lost their syscall property):
  assert.equal(isSpawnFailure(new Error("spawn EPERM")), true, "message-only spawn errors must classify");
  assert.equal(isSpawnFailure(new Error("python exit 1: pip check found conflicts")), false, "a real non-zero exit is NOT a spawn failure");
  assert.equal(isSpawnFailure(new Error("random")), false);
  assert.equal(isSpawnFailure(null), false);
});

test("spawn failure in the git checkout stage defers SATISFIER_SPAWN_FAILED with the pack as ref", async () => {
  const r = await satisfyManifest(M, { ...okDeps, ensurePack: async () => { throw spawnErr(); } });
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "SATISFIER_SPAWN_FAILED");
  assert.equal(r.defer.ref, M.node_packs[0].name, "ref must name the failing stage");
});

test("ensurePack is NOT retried (a retry recomputes HEAD-before against its own side effects)", async () => {
  let calls = 0;
  await satisfyManifest(M, { ...okDeps, ensurePack: async () => { calls++; throw spawnErr(); } });
  assert.equal(calls, 1, "the checkout stage defers typed on first spawn failure; the CALLER retries the whole satisfy");
});

test("spawn failure in resolveDeps defers SATISFIER_SPAWN_FAILED/uv-resolve, not VENV_INCOHERENT", async () => {
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    resolveDeps: async () => { throw spawnErr(); },
  });
  assert.equal(r.defer.code, "SATISFIER_SPAWN_FAILED", "spawn failure must not masquerade as venv incoherence");
  assert.equal(r.defer.ref, "uv-resolve");
  assert.match(r.defer.detail, /spawn UNKNOWN/);
});

test("a TRANSIENT resolveDeps spawn failure is retried once and satisfy succeeds", async () => {
  let calls = 0;
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    resolveDeps: async () => { if (++calls === 1) throw spawnErr(); },
  });
  assert.equal(r.ok, true, "one transient spawn failure must be absorbed");
  assert.equal(calls, 2);
});

test("a REAL resolve failure (non-zero exit) still defers VENV_INCOHERENT", async () => {
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    resolveDeps: async () => { throw new Error("uv exit 1: no solution within host constraints"); },
  });
  assert.equal(r.defer.code, "VENV_INCOHERENT");
});

test("runRetryOnSpawn retries ONCE on a spawn failure and succeeds", async () => {
  let calls = 0;
  const out = await runRetryOnSpawn(async () => { if (++calls === 1) throw spawnErr(); return "ok"; }, { delayMs: 1 });
  assert.equal(out, "ok");
  assert.equal(calls, 2);
});

test("runRetryOnSpawn does NOT retry a real failure, and gives up after the one retry", async () => {
  let calls = 0;
  await assert.rejects(
    () => runRetryOnSpawn(async () => { calls++; throw new Error("uv exit 1: conflict"); }, { delayMs: 1 }),
    /exit 1/);
  assert.equal(calls, 1, "non-spawn failures are not retried");
  let calls2 = 0;
  await assert.rejects(
    () => runRetryOnSpawn(async () => { calls2++; throw spawnErr(); }, { delayMs: 1 }),
    /spawn UNKNOWN/);
  assert.equal(calls2, 2, "exactly one retry for a persistent spawn failure");
});

// --- the deps-satisfied marker (adversarial-review redesign, 2026-07-22) ---
// "git didn't move this run" is NOT proof the deps were ever installed: a run that checks
// packs out and fails before resolveDeps leaves changed=false forever. The skip (and the
// fail-open) key on a marker written ONLY after a fully successful resolve+check.

test("unchanged packs WITHOUT a satisfied marker still run the full resolve", async () => {
  let resolved = false;
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: false }),
    readDepsMarker: () => null,             // e.g. a prior run died between checkout and install
    resolveDeps: async () => { resolved = true; },
  });
  assert.equal(r.ok, true);
  assert.equal(resolved, true, "no proof of a prior successful install - must resolve");
});

test("unchanged packs WITH a matching marker skip the resolve (the common re-run)", async () => {
  let resolved = false, checked = false;
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: false }),
    readDepsMarker: () => M_KEY,
    resolveDeps: async () => { resolved = true; },
    pipCheck: async () => { checked = true; return { ok: true }; },
  });
  assert.equal(r.ok, true);
  assert.equal(resolved, false, "pack set unchanged AND proven satisfied - nothing to resolve");
  assert.equal(checked, true, "the cheap coherence check still runs");
});

test("a STALE marker (different pack set) does not authorize the skip", async () => {
  let resolved = false;
  await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: false }),
    readDepsMarker: () => "A@OLD-COMMIT",
    resolveDeps: async () => { resolved = true; },
  });
  assert.equal(resolved, true, "marker for a different pack set proves nothing about this one");
});

test("the marker is written ONLY after resolve+check both succeed", async () => {
  let wrote = null;
  await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    writeDepsMarker: (k) => { wrote = k; },
  });
  assert.equal(wrote, M_KEY, "success writes the pack-set key");
  wrote = null;
  await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    pipCheck: async () => ({ ok: false, reason: "conflict" }),
    writeDepsMarker: (k) => { wrote = k; },
  });
  assert.equal(wrote, null, "a failed check must NOT record the env as satisfied");
});

test("changed packs still run the full resolve even with a matching marker", async () => {
  let resolved = false;
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    readDepsMarker: () => M_KEY,
    resolveDeps: async () => { resolved = true; },
  });
  assert.equal(r.ok, true);
  assert.equal(resolved, true, "a moved checkout always re-resolves");
});

test("proven-satisfied env + pipCheck SPAWN failure fails OPEN with a warning", async () => {
  const r = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: false }),
    readDepsMarker: () => M_KEY,
    pipCheck: async () => { throw spawnErr(); },
  });
  assert.equal(r.ok, true, "marker proves prior coherence - a check that could not RUN is not evidence against it");
  assert.match(r.warning || "", /coherence check skipped/i);
});

test("UNPROVEN env + pipCheck spawn failure fails CLOSED (SATISFIER_SPAWN_FAILED)", async () => {
  const noMarker = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: false }),
    readDepsMarker: () => null,
    pipCheck: async () => { throw spawnErr(); },
  });
  assert.equal(noMarker.ok, false);
  assert.equal(noMarker.defer.code, "SATISFIER_SPAWN_FAILED");
  assert.equal(noMarker.defer.ref, "pip-check");
  const justChanged = await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    readDepsMarker: () => M_KEY,
    pipCheck: async () => { throw spawnErr(); },
  });
  assert.equal(justChanged.defer.code, "SATISFIER_SPAWN_FAILED", "a just-modified env cannot fail open");
});

test("PRODUCTION pipCheck rethrows a real child_process spawn failure (not {ok:false})", async () => {
  // The glue this release exists for: a REAL spawn error from a nonexistent interpreter must
  // escape pipCheck as a throw (for classification), never be swallowed into a venv verdict.
  const deps = defaultSatisfyDeps({
    comfyDir: "C:/ComfyUI",
    comfyPy: "C:/definitely/not/a/real/python-interpreter.exe",
    api: "http://127.0.0.1:1",
  });
  await assert.rejects(() => deps.pipCheck(), (e) => isSpawnFailure(e),
    "a spawn-level failure must propagate as a throw that classifies as a spawn failure");
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

// #5: clone neutralizes a leading-dash repo with a literal `--` before the positionals.
test("buildClonePackCmd inserts -- so a leading-dash repo is a positional, not a flag", () => {
  const cmd = buildClonePackCmd({ name: "A", repo: "--upload-pack=evil" }, { comfyDir: "C:/ComfyUI" });
  const dd = cmd.indexOf("--");
  assert.ok(dd > 0, "-- present");
  assert.ok(cmd.indexOf("--upload-pack=evil") > dd, "repo comes AFTER the -- terminator");
});

// A render request never moves an installed pack: no git command runs, and the commit it is at is reported.
test("makeEnsurePack: an installed pack is used as it is → no git command, changed:false, its commit recorded", async () => {
  const ran = [], recorded = [];
  const ep = makeEnsurePack({
    comfyDir: "C:/ComfyUI", exists: () => true, packAllowed: () => null,
    gitHead: async () => "samehead", gitRun: async (cmd) => { ran.push(cmd); },
    record: (pack, commit) => recorded.push([pack.name, commit]),
  });
  const r = await ep({ name: "A", repo: "r" });
  assert.equal(r.changed, false);
  assert.equal(r.commit, "samehead");
  assert.deepEqual(ran, []);
  assert.deepEqual(recorded, [["A", "samehead"]]);
});
test("makeEnsurePack: fresh clone (dir absent) → changed:true, clones once, pulls nothing", async () => {
  let cloned = 0, pulled = 0;
  const ep = makeEnsurePack({
    comfyDir: "C:/ComfyUI", exists: () => false, packAllowed: () => null,
    gitHead: async () => "h", gitRun: async (cmd) => { if (cmd.includes("clone")) cloned++; if (cmd.includes("pull")) pulled++; }, record: () => {},
  });
  const r = await ep({ name: "A", repo: "r" });
  assert.equal(r.changed, true);
  assert.equal(cloned, 1);
  assert.equal(pulled, 0);
});

// #9 nameless pack: parseManifest derives a filesystem-safe dir name from the repo URL.
test("parseManifest derives a safe pack name from the repo when name is absent", () => {
  const m = parseManifest({ node_packs: [{ repo: "https://github.com/x/ComfyUI-RMBG" }] });
  assert.equal(m.node_packs[0].name, "ComfyUI-RMBG");
  assert.ok(!m.node_packs[0].name.includes("/"));
  assert.ok(!m.node_packs[0].name.includes(":"));
});
test("parseManifest strips a trailing .git from the derived pack name", () => {
  const m = parseManifest({ node_packs: [{ repo: "https://github.com/x/ComfyUI-RMBG.git" }] });
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

test("buildHostConstraints returns [] for empty / no-match / non-protected input", () => {
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
  const deps = defaultSatisfyDeps({ comfyDir: dir, comfyPy: join(dir, "python"), api: "http://127.0.0.1:0" });
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

// Production leak: the host-constraints file and the pack lock `resolveDeps` writes were pid-named files in the OS temp dir
// that nothing removed, so every provisioning run left two behind. They now live in a folder of the step's own that goes
// when the step ends, however it ends. The real glue runs here against stub executables for the venv python (it answers
// `pip freeze` with a torch the host holds) and for uv (it writes the lock it is asked for at `-o`, then exits as told), with
// the OS temp dir pointed at a private folder.
async function resolveInPrivateTemp(t, { uvExit }) {
  const bin = scratchDir(t, "satisfy-bin-");
  const comfy = scratchDir(t, "satisfy-comfy-");
  const priv = scratchDir(t, "satisfy-tmp-");
  const uvLog = join(bin, "uv-calls.jsonl");
  const py = makeStub(bin, "python", { stdout: ["torch==2.11.0+cu128", "requests==2.32.0"] });
  makeStub(bin, process.platform === "win32" ? "uv" : "uv.exe", { // resolveDeps looks for uv.exe beside the python
    exit: uvExit, record: uvLog, writes: { kind: "file_o", base64: Buffer.from("requests==2.32.0\n").toString("base64") },
  });
  mkdirSync(join(comfy, "custom_nodes", "PackA"), { recursive: true });
  writeFileSync(join(comfy, "custom_nodes", "PackA", "requirements.txt"), "requests\n");
  const saved = { TEMP: process.env.TEMP, TMP: process.env.TMP, TMPDIR: process.env.TMPDIR };
  t.after(() => { for (const [k, v] of Object.entries(saved)) { if (v === undefined) delete process.env[k]; else process.env[k] = v; } });
  process.env.TEMP = process.env.TMP = process.env.TMPDIR = priv;
  const deps = defaultSatisfyDeps({ comfyDir: comfy, comfyPy: py, api: "http://127.0.0.1:0" });
  const outcome = await deps.resolveDeps([{ name: "PackA" }]).then(() => "resolved", (e) => e);
  const compile = JSON.parse(readFileSync(uvLog, "utf8").trim().split("\n")[0]).argv; // uv pip compile ... -o <lock>
  const lock = compile[compile.indexOf("-o") + 1];
  return { outcome, left: readdirSync(priv), lock, priv };
}

const stubSkip = () => (stubAvailable() ? false : "cannot build a stub executable here (needs go on Windows)");
const underDir = (file, dir) => resolve(file).startsWith(resolve(dir) + sep);

test("defaultSatisfyDeps.resolveDeps leaves no host-constraints file or pack lock in the OS temp dir when the install resolves", { skip: stubSkip() }, async (t) => {
  const r = await resolveInPrivateTemp(t, { uvExit: 0 });
  assert.equal(r.outcome, "resolved");
  assert.ok(underDir(r.lock, r.priv), `uv was asked to write the lock under the OS temp dir (${r.lock}), so the check below is not vacuous`);
  assert.deepEqual(r.left, []);
});

test("defaultSatisfyDeps.resolveDeps leaves no host-constraints file or pack lock in the OS temp dir when uv fails", { skip: stubSkip() }, async (t) => {
  const r = await resolveInPrivateTemp(t, { uvExit: 1 });
  assert.ok(r.outcome instanceof Error && /exit 1/.test(r.outcome.message), "the uv failure still reaches the caller: " + r.outcome);
  assert.ok(underDir(r.lock, r.priv), `uv had written the lock under the OS temp dir (${r.lock}) before it failed`);
  assert.deepEqual(r.left, []);
});

// --- trust on first use (the template run path) ---
//
// A model file with no hash declared is hashed the first time a template run meets it and the hash is recorded beside the file; later runs
// refuse a file whose hash is not the recorded one. A declared sha256 is the satisfier's own business and is left to it.

import { verifyTofuModels, defaultTofuDeps } from "./manifest-satisfy.mjs";

const TOFU_FILE = join("C:/ComfyUI", "models/vae/v.safetensors");
const TOFU_MODEL = { path: "models/vae/v.safetensors", source_url: "", sha256: null, trust: "tofu" };

// A fake of the record store: records by file path, a stat per file, a hash per file, and a count of the hashes taken.
function tofuDeps({ files = { [TOFU_FILE]: { size: 10, mtimeMs: 5, hash: "H1" } }, records = {}, over = {} } = {}) {
  const state = { records: { ...records }, hashed: [], writes: [] };
  const deps = {
    comfyDir: "C:/ComfyUI",
    extraRoots: {},
    exists: (p) => Object.hasOwn(files, p),
    now: () => "2026-10-14T00:00:00.000Z",
    tofu: {
      stat: (p) => ({ size: files[p].size, mtimeMs: files[p].mtimeMs }),
      read: (p) => (Object.hasOwn(state.records, p) ? state.records[p] : null),
      write: (p, rec) => { state.writes.push([p, rec]); state.records[p] = rec; },
      sha256: async (p) => { state.hashed.push(p); return files[p].hash; },
    },
    ...over,
  };
  return { deps, state, files };
}

test("tofu: the first sighting hashes the file, records the hash beside it and reports it as first seen", async () => {
  const { deps, state } = tofuDeps();
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.deepEqual(r, { ok: true, first_seen: ["models/vae/v.safetensors"], matched: [] });
  assert.deepEqual(state.hashed, [TOFU_FILE]);
  assert.deepEqual(state.writes, [[TOFU_FILE, { schema_version: 1, sha256: "H1", size: 10, mtime_ms: 5, first_seen: "2026-10-14T00:00:00.000Z" }]]);
});

test("tofu: a later run on the same file (same size and modification time) trusts the record and hashes nothing", async () => {
  const rec = { schema_version: 1, sha256: "H1", size: 10, mtime_ms: 5, first_seen: "2026-10-01T00:00:00.000Z" };
  const { deps, state } = tofuDeps({ records: { [TOFU_FILE]: rec } });
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.deepEqual(r, { ok: true, first_seen: [], matched: ["models/vae/v.safetensors"] });
  assert.deepEqual(state.hashed, []);
  assert.deepEqual(state.writes, []);
});

test("tofu: a file whose size or time changed is hashed again, and the same hash keeps the first-seen date", async () => {
  const rec = { schema_version: 1, sha256: "H1", size: 10, mtime_ms: 5, first_seen: "2026-10-01T00:00:00.000Z" };
  const { deps, state } = tofuDeps({ files: { [TOFU_FILE]: { size: 10, mtimeMs: 99, hash: "H1" } }, records: { [TOFU_FILE]: rec } });
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.equal(r.ok, true);
  assert.deepEqual(state.hashed, [TOFU_FILE]);
  assert.deepEqual(state.writes, [[TOFU_FILE, { ...rec, mtime_ms: 99 }]]);
});

test("tofu: a file whose SIZE changed at the recorded modification time is hashed again, and a different hash is refused", async () => {
  const rec = { schema_version: 1, sha256: "H1", size: 10, mtime_ms: 5, first_seen: "2026-10-01T00:00:00.000Z" };
  const { deps, state } = tofuDeps({ files: { [TOFU_FILE]: { size: 11, mtimeMs: 5, hash: "H2" } }, records: { [TOFU_FILE]: rec } });
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.equal(r.ok, false, "the time alone does not vouch for a file whose size moved");
  assert.equal(r.defer.code, "MODEL_SHA_MISMATCH");
  assert.deepEqual(state.hashed, [TOFU_FILE]);
  assert.deepEqual(state.writes, [], "the record is kept");
});

test("tofu: a hash that is not the recorded one is refused as MODEL_SHA_MISMATCH, naming both, and the record is kept", async () => {
  const rec = { schema_version: 1, sha256: "H1", size: 10, mtime_ms: 5, first_seen: "2026-10-01T00:00:00.000Z" };
  const { deps, state } = tofuDeps({ files: { [TOFU_FILE]: { size: 11, mtimeMs: 6, hash: "H2" } }, records: { [TOFU_FILE]: rec } });
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "MODEL_SHA_MISMATCH");
  assert.equal(r.defer.ref, "models/vae/v.safetensors");
  assert.match(r.defer.detail, /trust on first use/);
  assert.match(r.defer.detail, /H1/);
  assert.match(r.defer.detail, /H2/);
  assert.match(r.defer.detail, /2026-10-01/);
  assert.deepEqual(state.writes, [], "a mismatch never overwrites the record it was refused by");
});

test("tofu: a record that cannot be read, or cannot be written, refuses the model as MODEL_TOFU_RECORD", async () => {
  const unreadable = tofuDeps();
  unreadable.deps.tofu.read = () => { throw new Error("record is not JSON"); };
  const a = await verifyTofuModels([TOFU_MODEL], unreadable.deps);
  assert.deepEqual([a.ok, a.defer.code, a.defer.ref], [false, "MODEL_TOFU_RECORD", "models/vae/v.safetensors"]);
  assert.match(a.defer.detail, /not JSON/);
  const unwritable = tofuDeps();
  unwritable.deps.tofu.write = () => { throw new Error("EROFS: read-only"); };
  const b = await verifyTofuModels([TOFU_MODEL], unwritable.deps);
  assert.deepEqual([b.ok, b.defer.code], [false, "MODEL_TOFU_RECORD"]);
  assert.match(b.defer.detail, /EROFS/);
});

test("tofu: a model with a declared hash, or without the tofu trust, is left alone", async () => {
  const { deps, state } = tofuDeps();
  const r = await verifyTofuModels([{ ...TOFU_MODEL, sha256: "DECLARED", trust: "declared" }, { path: "models/vae/v.safetensors", source_url: "", sha256: null }], deps);
  assert.deepEqual(r, { ok: true, first_seen: [], matched: [] });
  assert.deepEqual(state.hashed, []);
});

test("tofu: a model that is not on the disk is refused as MODEL_DOWNLOAD_FAILED (a template run never downloads)", async () => {
  const { deps } = tofuDeps({ files: {} });
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.deepEqual([r.ok, r.defer.code, r.defer.ref], [false, "MODEL_DOWNLOAD_FAILED", "models/vae/v.safetensors"]);
  assert.match(r.defer.detail, /missing on disk/);
});

test("tofu: the record sits beside the file that was found, on a secondary tree too", async () => {
  const onV = join("V:/m/vae", "v.safetensors");
  const { deps, state } = tofuDeps({ files: { [onV]: { size: 4, mtimeMs: 1, hash: "HV" } }, over: { extraRoots: { vae: ["V:/m/vae"] } } });
  const r = await verifyTofuModels([TOFU_MODEL], deps);
  assert.equal(r.ok, true);
  assert.equal(state.writes[0][0], onV);
});

test("tofu: every model in the list is checked", async () => {
  const other = join("C:/ComfyUI", "models/loras/l.safetensors");
  const { deps, state } = tofuDeps({ files: { [TOFU_FILE]: { size: 10, mtimeMs: 5, hash: "H1" }, [other]: { size: 3, mtimeMs: 2, hash: "HL" } } });
  const r = await verifyTofuModels([TOFU_MODEL, { ...TOFU_MODEL, path: "models/loras/l.safetensors" }], deps);
  assert.deepEqual(r.first_seen, ["models/vae/v.safetensors", "models/loras/l.safetensors"]);
  assert.equal(state.writes.length, 2);
});

test("tofu: the default deps keep the record in <file>.tofu, notice a changed file and refuse it (real files)", async (t) => {
  const dir = scratchDir(t, "tofu-");
  const file = join(dir, "models/vae/v.safetensors");
  mkdirSync(join(dir, "models/vae"), { recursive: true });
  writeFileSync(file, "weights one");
  const deps = { ...defaultSatisfyDeps({ comfyDir: dir, comfyPy: join(dir, "py"), api: "http://127.0.0.1:1" }), tofu: defaultTofuDeps() };
  const first = await verifyTofuModels([TOFU_MODEL], deps);
  assert.deepEqual(first.first_seen, ["models/vae/v.safetensors"]);
  assert.ok(existsSync(file + ".tofu"), "the record is beside the file");
  assert.equal(JSON.parse(readFileSync(file + ".tofu", "utf8")).sha256.length, 64);
  const again = await verifyTofuModels([TOFU_MODEL], deps);
  assert.deepEqual(again.matched, ["models/vae/v.safetensors"]);
  writeFileSync(file, "weights two!");
  const swapped = await verifyTofuModels([TOFU_MODEL], deps);
  assert.equal(swapped.ok, false);
  assert.equal(swapped.defer.code, "MODEL_SHA_MISMATCH");
});

// --- the satisfier looks where ComfyUI looks: the legacy class directories ---

test("modelCandidates: a diffusion_models path is also looked for under unet, and a text_encoders path under clip (ComfyUI reads both)", () => {
  assert.deepEqual(modelCandidates("models/diffusion_models/w.gguf", "C:/ComfyUI", { unet: ["V:/m/unet"], diffusion_models: ["V:/m/dm"] }), [
    join("C:/ComfyUI", "models/diffusion_models/w.gguf"),
    join("V:/m/dm", "w.gguf"),
    join("C:/ComfyUI", "models/unet/w.gguf"),
    join("V:/m/unet", "w.gguf"),
  ]);
  assert.deepEqual(modelCandidates("models/text_encoders/t.safetensors", "C:/ComfyUI", { clip: ["V:/m/clip"] }), [
    join("C:/ComfyUI", "models/text_encoders/t.safetensors"),
    join("C:/ComfyUI", "models/clip/t.safetensors"),
    join("V:/m/clip", "t.safetensors"),
  ]);
});

// --- the pack allowlist (the real table; no stand-in policy) ---
// A manifest that names a pack makes the node clone its repository and pip-install its requirements, so it
// runs the caller's code. Only a pack whose repository is in render/node-packs.json is taken;
// anything else is refused before the first git command, whatever the models and the tooling say.
const VHS = { name: "ComfyUI-VideoHelperSuite", repo: "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite" };
const HOSTILE = { name: "payload", repo: "https://example.invalid/payload" };
const realPolicy = { ...okDeps, packAllowed: undefined };

function spies() {
  const calls = [];
  return {
    calls,
    deps: {
      ...realPolicy,
      satisfierAvailable: () => { calls.push("satisfierAvailable"); return true; },
      download: async () => { calls.push("download"); },
      exists: () => false,
      ensurePack: async () => { calls.push("ensurePack"); return { changed: false }; },
      resolveDeps: async () => { calls.push("resolveDeps"); },
      pipCheck: async () => { calls.push("pipCheck"); return { ok: true }; },
    },
  };
}

test("satisfyManifest refuses a pack that is not allowlisted before anything is downloaded, cloned or installed", async () => {
  const { calls, deps } = spies();
  const m = parseManifest({ node_packs: [HOSTILE], models: [{ path: "models/checkpoints/x.safetensors", source_url: "https://example.invalid/x", sha256: null }] });
  const r = await satisfyManifest(m, deps);
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "NODE_PACK_NOT_ALLOWED");
  assert.equal(r.defer.ref, "payload", "the refusal names the pack");
  assert.deepEqual(calls, [], "nothing ran: no tooling probe, no model download, no checkout, no install");
});

test("satisfyManifest refuses a listed repository under another directory name", async () => {
  for (const pack of [{ ...VHS, name: "ComfyUI-Manager" }, { ...VHS, name: "../../x" }]) {
    const { calls, deps } = spies();
    const r = await satisfyManifest(parseManifest({ node_packs: [pack], models: [] }), deps);
    assert.equal(r.defer && r.defer.code, "NODE_PACK_NOT_ALLOWED", JSON.stringify(pack));
    assert.deepEqual(calls, []);
  }
});

test("satisfyManifest refuses the whole manifest when one of its packs is not allowlisted", async () => {
  const { calls, deps } = spies();
  const r = await satisfyManifest(parseManifest({ node_packs: [VHS, HOSTILE], models: [] }), deps);
  assert.equal(r.defer && r.defer.code, "NODE_PACK_NOT_ALLOWED");
  assert.deepEqual(calls, [], "the allowlisted pack was not checked out ahead of the refusal");
});

test("satisfyManifest takes a manifest that names only allowlisted packs", async () => {
  const { calls, deps } = spies();
  const r = await satisfyManifest(parseManifest({ node_packs: [VHS], models: [] }), deps);
  assert.equal(r.ok, true, JSON.stringify(r));
  assert.ok(calls.includes("ensurePack"));
});

test("makeEnsurePack refuses a pack that is not allowlisted before it asks git anything, and never builds a clone command", async (t) => {
  const touched = [];
  // A directory of its own: the first clone below records the pack beside it.
  const comfyDir = scratchDir(t, "offload-ensure-");
  const ep = makeEnsurePack({
    comfyDir,
    exists: (d) => { touched.push(["exists", d]); return false; },
    gitHead: async () => { touched.push(["gitHead"]); return "h"; },
    gitRun: async (cmd) => { touched.push(["gitRun", cmd]); },
    buildClone: (...a) => { touched.push(["buildClone", a]); return ["git", "clone"]; },
    record: () => {},
    readRecord: () => null,
  });
  await assert.rejects(() => ep(HOSTILE), (e) => e.code === "NODE_PACK_NOT_ALLOWED" && /payload/.test(e.message));
  assert.deepEqual(touched, [], "the clone command was built or run for a pack that is not allowlisted");
  const r = await ep(VHS);
  assert.equal(r.changed, true);
  assert.ok(touched.some((x) => x[0] === "buildClone"), "an allowlisted pack is cloned");
});

test("a refusal from the checkout stage is the same typed defer, not a broken venv", async (t) => {
  const comfyDir = scratchDir(t, "offload-ensure-");
  const ep = makeEnsurePack({ comfyDir, exists: () => false, gitHead: async () => "h", gitRun: async () => {} });
  const r = await satisfyManifest(parseManifest({ node_packs: [VHS], models: [] }), { ...realPolicy, ensurePack: (p) => ep({ ...p, name: "ComfyUI-Manager" }) });
  assert.equal(r.defer && r.defer.code, "NODE_PACK_NOT_ALLOWED");
});

// --- a model lands under models/<class>/ and nowhere else ---------------------------------------------------------
// A holder of the fleet bearer can have the node render, not write a file of its choosing: the entries are checked
// before the first download, before any file is looked for, and before a pack is cloned. The table is the one
// internal/mediacap/manifestmodels_test.go reads.
const modelCases = JSON.parse(readFileSync(new URL("./testdata/manifest-model-cases.json", import.meta.url), "utf8")).cases;

function spyDeps(extra = {}) {
  const calls = [];
  const note = (name) => (...a) => { calls.push([name, ...a]); };
  return {
    calls,
    deps: {
      comfyDir: "/opt/comfy",
      exists: (p) => { calls.push(["exists", p]); return false; },
      sentinelOk: () => { calls.push(["sentinelOk"]); return false; },
      writeSentinel: note("writeSentinel"),
      download: async (url, dest) => { calls.push(["download", url, dest]); },
      sha256: async () => "HASH",
      ...extra,
    },
  };
}

for (const c of modelCases) {
  test(`satisfyModels: ${c.name}`, async () => {
    const { calls, deps } = spyDeps();
    const r = await satisfyModels([{ path: c.path, source_url: c.source_url, sha256: null }], deps);
    if (c.ok) {
      assert.notEqual(r.defer && r.defer.code, "MODEL_NOT_ALLOWED", "refused an entry the node provisions");
      if (c.source_url) {
        const dl = calls.find((x) => x[0] === "download");
        assert.ok(dl, "a listed source was not downloaded");
        assert.ok(resolve(dl[2]).startsWith(resolve("/opt/comfy", "models") + sep), `the file lands at ${dl[2]}, outside models/`);
      }
      return;
    }
    assert.equal(r.ok, false);
    assert.equal(r.defer.code, "MODEL_NOT_ALLOWED");
    assert.deepEqual(calls, [], "a refused entry must touch nothing: no lookup, no download, no sentinel");
  });
}

test("satisfyModels refuses a hostile entry even when the file is already on disk (no adoption, no sentinel)", async () => {
  const { calls, deps } = spyDeps({ exists: () => true });
  const r = await satisfyModels([{ path: "custom_nodes/evil/__init__.py", source_url: "", sha256: "HASH" }], deps);
  assert.equal(r.defer.code, "MODEL_NOT_ALLOWED");
  assert.deepEqual(calls, []);
});

test("satisfyModels checks every entry before it provisions the first", async () => {
  const { calls, deps } = spyDeps();
  const r = await satisfyModels([
    { path: "models/vae/ok.safetensors", source_url: "https://huggingface.co/x/y", sha256: null },
    { path: "custom_nodes/evil/__init__.py", source_url: "https://huggingface.co/x/y", sha256: null },
  ], deps);
  assert.equal(r.defer.code, "MODEL_NOT_ALLOWED");
  assert.equal(r.defer.ref, "custom_nodes/evil/__init__.py");
  assert.deepEqual(calls, [], "the first, valid entry was provisioned before the second was refused");
});

test("satisfyModels does not throw on an entry whose path is not a string", async () => {
  for (const path of [5, ["models/vae/x.safetensors"], { a: 1 }]) {
    const { deps } = spyDeps();
    const r = await satisfyModels([{ path, source_url: "", sha256: null }], deps);
    assert.equal(r.defer.code, "MODEL_NOT_ALLOWED");
  }
});

test("satisfyManifest refuses a hostile model before the tooling probe, the version read, a download or a clone", async () => {
  const called = [];
  const mark = (n, v) => () => { called.push(n); return v; };
  const m = parseManifest({
    comfyui_min_version: "0.23.0",
    node_packs: [{ name: "A", repo: "r" }],
    models: [{ path: "custom_nodes/evil/__init__.py", source_url: "https://huggingface.co/x/y", sha256: null }],
  });
  const r = await satisfyManifest(m, {
    ...okDeps,
    exists: mark("exists", false), sentinelOk: mark("sentinelOk", false),
    download: async () => { called.push("download"); },
    satisfierAvailable: mark("satisfierAvailable", true),
    comfyVersion: async () => { called.push("comfyVersion"); return "0.23.5"; },
    ensurePack: async () => { called.push("ensurePack"); return { changed: false }; },
    resolveDeps: async () => { called.push("resolveDeps"); },
  });
  assert.equal(r.ok, false);
  assert.equal(r.defer.code, "MODEL_NOT_ALLOWED");
  assert.deepEqual(called, []);
});

test("a manifest of the shipped shape (a model under models/, a listed source) still provisions", async () => {
  const { calls, deps } = spyDeps();
  const r = await satisfyManifest(parseManifest({ node_packs: [], models: [{ path: "models/unet/x.gguf", source_url: "https://huggingface.co/x/y/resolve/main/x.gguf", sha256: null }] }),
    { ...deps, satisfierAvailable: () => true, comfyVersion: async () => "0.23.5" });
  assert.equal(r.ok, true);
  assert.deepEqual(r.unverified, ["models/unet/x.gguf"]);
  assert.equal(calls.filter((x) => x[0] === "download").length, 1);
});
