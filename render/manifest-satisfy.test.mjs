import { test } from "node:test";
import assert from "node:assert/strict";
import { satisfyModels, defaultSatisfyDeps, parseExtraModelPaths, modelCandidates } from "./manifest-satisfy.mjs";
import { mkdtempSync, existsSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { satisfyManifest, buildHostConstraints, publicPin, isSpawnFailure, runRetryOnSpawn } from "./manifest-satisfy.mjs";
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
  retryDelayMs: 1,          // never pay the production 500ms sleep in tests
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

// --- spawn-failure classification (live CMP report 2026-07-20) ---
// A spawn-level failure ("spawn UNKNOWN"/ENOENT/EACCES - the subprocess never ran) was caught
// and relabeled VENV_INCOHERENT: "the check could not run" reported as "your venv is broken",
// pointing the operator at healthy torch pins. Spawn failures must be classified distinctly
// (SATISFIER_SPAWN_FAILED), retried once for the python-touching steps (CMP's was transient
// after a long batch), and - for a pin-set the persisted marker proves was already satisfied -
// must not fail a previously-coherent env at all.

const spawnErr = () => Object.assign(new Error("spawn UNKNOWN"), { code: "UNKNOWN", syscall: "spawn" });
// M's packs key, for marker-based tests.
const M_KEY = (M.node_packs || []).map((p) => `${p.name}@${p.commit}`).sort().join("|");

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
  assert.equal(resolved, false, "pin-set unchanged AND proven satisfied - nothing to resolve");
  assert.equal(checked, true, "the cheap coherence check still runs");
});

test("a STALE marker (different pin-set) does not authorize the skip", async () => {
  let resolved = false;
  await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: false }),
    readDepsMarker: () => "A@OLD-COMMIT",
    resolveDeps: async () => { resolved = true; },
  });
  assert.equal(resolved, true, "marker for a different pin-set proves nothing about this one");
});

test("the marker is written ONLY after resolve+check both succeed", async () => {
  let wrote = null;
  await satisfyManifest(M, {
    ...okDeps,
    ensurePack: async () => ({ changed: true }),
    writeDepsMarker: (k) => { wrote = k; },
  });
  assert.equal(wrote, M_KEY, "success writes the pin-set key");
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
    api: "http://127.0.0.1:1", cmCli: "",
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
