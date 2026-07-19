// render/manifest-satisfy.mjs — satisfy a node manifest = make the graph's env real
// (spec §3). "Satisfied" means import + coherence (Task 6 adds packs + the /object_info
// gate); this file starts with the models leg. All side-effecting deps are injected so
// the whole satisfier is testable with zero network/disk.
import { join, dirname } from "node:path";

const defer = (code, ref, detail) => ({ ok: false, defer: { code, ref, detail } });

export async function satisfyModels(models, deps) {
  const { comfyDir, exists, download, sha256, sentinelOk, writeSentinel } = deps;
  const unverified = [];
  for (const m of models || []) {
    const abs = join(comfyDir, m.path);
    if (exists(abs) && (m.sha256 ? sentinelOk(abs) : exists(abs))) {
      if (!m.sha256) { /* present but unverifiable — trust on-disk, no re-flag */ }
      continue;
    }
    if (!m.source_url) return defer("MODEL_DOWNLOAD_FAILED", m.path, "missing on disk and no source_url");
    try { await download(m.source_url, abs); }
    catch (e) { return defer("MODEL_DOWNLOAD_FAILED", m.path, String(e.message || e)); }
    if (m.sha256) {
      // Guard the post-download hash read AND the sentinel write: an FS failure here
      // must defer typed, not escape as an untyped process crash (the sentinel write
      // was previously outside any try — a fresh download that failed to write its
      // .sha-ok sidecar took the whole run down).
      try {
        const got = await sha256(abs);
        if (got !== m.sha256) return defer("MODEL_SHA_MISMATCH", m.path, `want ${m.sha256} got ${got}`);
        writeSentinel(abs, got);
      } catch (e) {
        return defer("MODEL_DOWNLOAD_FAILED", m.path, "post-download verify/sentinel failed: " + String(e.message || e));
      }
    } else {
      unverified.push(m.path);           // downloaded, UNVERIFIED — reported as data
    }
  }
  return { ok: true, unverified };
}

function versionBelow(have, min) {
  if (!min || !have) return false;
  const norm = (v) => v.split(".").map((n) => parseInt(n, 10) || 0);
  const [a, b] = [norm(have), norm(min)];
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const x = a[i] || 0, y = b[i] || 0;
    if (x !== y) return x < y;
  }
  return false;
}

// satisfyManifest: DISK provisioning only. Order: tooling → version → models →
// packs CHECKOUT (git) → unified deps resolve+install → pip check. Any failure →
// typed DEFER. Returns `changed` so the orchestrator knows whether packs moved.
// The /object_info node-class gate lives in preflight (comfy-run-graph.mjs), post-start.
//
// ORDER NOTE (live finding 2026-07-17): packs are cloned/checked out BEFORE the deps
// resolve — the unified `uv pip compile` reads each pack's on-disk requirements.txt,
// so the files must exist first. (The original design resolved via `cm-cli install
// --uv <refs>`, but the installed cm-cli has no --uv flag; uv is driven directly now,
// which the resolve-only spike already proved live.) "Unified, never sequential
// per-pack" is unchanged — one compile over ALL packs' requirements at once.
export async function satisfyManifest(manifest, deps) {
  const {
    satisfierAvailable, comfyVersion, resolveDeps, ensurePack, pipCheck,
  } = deps;

  const hasPacks = (manifest.node_packs || []).length > 0;

  // The satisfier tooling (uv) is ONLY needed to install PACKS. A models-only or
  // empty manifest (a graph using only built-in nodes) must NOT require it.
  if (hasPacks && !satisfierAvailable()) return defer("SATISFIER_UNAVAILABLE", "uv", "uv not found in the ComfyUI venv (pip install uv)");

  // comfyui_min_version is only enforceable once ComfyUI is reachable; versionBelow is
  // null-safe (returns false when the version can't be read), so this never wrongly defers.
  if (manifest.comfyui_min_version) {
    const have = await comfyVersion();
    if (versionBelow(have, manifest.comfyui_min_version))
      return defer("COMFY_VERSION_BELOW_MIN", have || "unknown", `needs >= ${manifest.comfyui_min_version}`);
  }

  const models = await satisfyModels(manifest.models, deps);
  if (!models.ok) return models;

  let changed = false;
  if (hasPacks) {
    // 1. Checkout every pack at its pin (git-only; no python side effects).
    for (const pack of manifest.node_packs) {
      let res;
      try { res = await ensurePack(pack); }
      catch (e) { return defer("VENV_INCOHERENT", pack.name, String(e.message || e)); }
      if (res.changed) changed = true;
    }
    // 2. ONE unified resolve+install over all packs' on-disk requirements
    //    (under the host-torch constraints env), never per-pack pip.
    try { await resolveDeps(manifest.node_packs); }
    catch (e) { return defer("VENV_INCOHERENT", "uv-resolve", String(e.message || e)); }
    const pc = await pipCheck();
    if (!pc.ok) return defer("VENV_INCOHERENT", "pip-check", pc.reason);
  }

  return { ok: true, changed, unverified: models.unverified };
}

import { spawn as nodeSpawn } from "node:child_process";
import { existsSync, writeFileSync, mkdirSync } from "node:fs";
import { createHash } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";

// Command builders are pure (unit-tested); execution is in defaultSatisfyDeps.
// Unified resolve: ONE `uv pip compile` across ALL packs' requirements files at once
// (cross-pack conflict detection — the ecosystem's documented failure mode is
// last-writer-wins sequential pip), then ONE constrained install of the lock.
// (cm-cli was the original vehicle, but the installed cm-cli has no --uv flag —
// live finding 2026-07-17; uv is driven directly, as the spike proved.)
export function buildCompileCmd(reqPaths, { uvExe, comfyPy, lockPath }) {
  return [uvExe, "pip", "compile", "--python", comfyPy, ...reqPaths, "-o", lockPath];
}
export function buildInstallCmd(lockPath, { comfyPy }) {
  return [comfyPy, "-m", "pip", "install", "-r", lockPath];
}

// isSafeRef: a git ref/commit that cannot be mistaken for a git OPTION. Rejects a leading
// dash (the arg-injection vector — e.g. `--upload-pack=...`) and anything outside the safe
// ref charset. `checkout <commit>` accepts no `--`-before-positional escape (that would mean
// "these are paths"), so a commit MUST be validated rather than neutralized.
function isSafeRef(s) {
  return typeof s === "string" && s.length > 0 && !s.startsWith("-") && /^[A-Za-z0-9._\/-]+$/.test(s);
}

export function buildEnsurePackCmd(pack, { comfyDir }) {
  const dir = `${comfyDir}/custom_nodes/${pack.name}`;
  // #5: a commit starting with `-` (or carrying shell/ref-unsafe chars) could be read as a
  // git flag — validate and reject rather than run it. checkout takes no `--` positional escape.
  if (!isSafeRef(pack.commit)) throw new Error(`unsafe commit ref: ${JSON.stringify(pack.commit)}`);
  // git -C <dir> checkout <commit>; clone first if absent (handled in glue).
  return ["git", "-C", dir, "checkout", pack.commit];
}

// buildClonePackCmd: `git clone -- <repo> <dir>`. The literal `--` terminates option parsing,
// so a `repo` value starting with `-` is treated as a positional (a path/URL), never a flag (#5).
export function buildClonePackCmd(pack, { comfyDir }) {
  const dir = `${comfyDir}/custom_nodes/${pack.name}`;
  return ["git", "clone", "--", pack.repo, dir];
}

// --- host-constraints (v1 protection) --------------------------------------------------
// Live finding 2026-07-17: the scene-swap packs' unified lock resolved torch 2.11.0+cu128
// → 2.13.0 — installing it as-resolved would REPLACE ComfyUI's CUDA torch and break the
// existing video/image render path. Pack provisioning must be ADDITIVE around the host's
// CUDA stack: every pip/uv the satisfier spawns runs under a constraints file pinning
// these packages (PIP_CONSTRAINT is honored by pip, UV_CONSTRAINT by uv). A resolve that
// cannot satisfy a pack within the host pins fails loud → typed VENV_INCOHERENT defer,
// never a silently replaced torch.
const HOST_PINNED = ["torch", "torchvision", "torchaudio", "numpy"];

// publicPin: strip a PEP 440 local-version suffix ("torch==2.11.0+cu128" →
// "torch==2.11.0") for the CONSTRAINTS FILE — local builds live on the PyTorch
// index, not PyPI, so a resolver given the +cu128 pin finds "no version exists"
// (live finding 2026-07-17). PEP 440: the installed +cu128 build SATISFIES the
// public ==2.11.0 pin, so pip neither reinstalls nor upgrades it; the post-install
// TRIPWIRE still compares the full local-version pins, guarding the exact build.
export function publicPin(line) {
  return line.replace(/\+[A-Za-z0-9.]+$/, "");
}

// buildHostConstraints: extract the protected packages' pinned lines from `pip freeze`
// output (local-version suffixes like +cu128 preserved). Pure; unit-tested.
export function buildHostConstraints(freezeText) {
  return String(freezeText || "")
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => {
      const eq = l.indexOf("==");
      return eq > 0 && HOST_PINNED.includes(l.slice(0, eq).trim().toLowerCase());
    });
}

async function run(cmd, opts = {}) {
  return new Promise((resolve, reject) => {
    const c = nodeSpawn(cmd[0], cmd.slice(1), { stdio: "pipe", ...opts });
    let err = "";
    c.stderr.on("data", (d) => (err += d));
    c.on("error", reject);
    c.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`${cmd[0]} exit ${code}: ${err.slice(-300)}`))));
  });
}

// runOut: like run() but resolves with trimmed stdout (used to read `git rev-parse HEAD`).
async function runOut(cmd, opts = {}) {
  return new Promise((resolve, reject) => {
    const c = nodeSpawn(cmd[0], cmd.slice(1), { stdio: ["ignore", "pipe", "pipe"], ...opts });
    let out = "", err = "";
    c.stdout.on("data", (d) => (out += d));
    c.stderr.on("data", (d) => (err += d));
    c.on("error", reject);
    c.on("exit", (code) => (code === 0 ? resolve(out.trim()) : reject(new Error(`${cmd[0]} exit ${code}: ${err.slice(-300)}`))));
  });
}

// makeEnsurePack: build the per-pack "clone if absent, checkout the pin, report changed"
// step from injectable primitives so it is unit-testable without spawning git. #3: `changed`
// is decided by the git HEAD before-vs-after (a fresh clone, or a checkout that MOVED HEAD),
// NOT merely "did the dir exist" — a same-commit checkout is correctly a no-op.
export function makeEnsurePack({ comfyDir, exists = existsSync, gitHead, gitRun }) {
  return async (pack) => {
    const dir = `${comfyDir}/custom_nodes/${pack.name}`;
    const existed = exists(dir);
    const headBefore = existed ? await gitHead(dir) : null;
    if (!existed) await gitRun(buildClonePackCmd(pack, { comfyDir }));
    await gitRun(buildEnsurePackCmd(pack, { comfyDir }));
    const headAfter = await gitHead(dir);
    return { changed: !existed || headBefore !== headAfter };
  };
}

export function defaultSatisfyDeps({ comfyDir, comfyPy, api, cmCli }) {
  // Host pins are captured ONCE, lazily, at the first python-touching step
  // (resolveDeps — ensurePack before it is git-only), so the tripwire below always
  // compares against the pre-install state and the constraints file exists before
  // any pip/uv spawns.
  let hostPinsP = null;
  const hostPins = () => (hostPinsP ??= (async () => {
    const pins = buildHostConstraints(await runOut([comfyPy, "-m", "pip", "freeze"]));
    let env = {};
    if (pins.length) {
      const p = join(tmpdir(), `offload-host-constraints-${process.pid}.txt`);
      // Constraints file carries the PUBLIC pins (local +cuXXX builds don't exist
      // on PyPI → unresolvable); the tripwire compares the FULL pins.
      writeFileSync(p, pins.map(publicPin).join("\n") + "\n");
      env = { PIP_CONSTRAINT: p, UV_CONSTRAINT: p };
    }
    return { pins, env };
  })());
  return {
    comfyDir,
    // The pack satisfier's hard tool is uv (unified resolve); derived from the venv
    // python's Scripts dir. cmCli is accepted for compat but no longer load-bearing.
    satisfierAvailable: () => existsSync(comfyPy) && existsSync(join(dirname(comfyPy), "uv.exe")),
    comfyVersion: async () => {
      try { const r = await fetch(`${api}/system_stats`); const j = await r.json(); return j?.system?.comfyui_version || j?.comfyui_version || null; }
      catch { return null; }
    },
    exists: (p) => existsSync(p),
    sentinelOk: (p) => existsSync(p + ".sha-ok"),
    writeSentinel: (p, h) => writeFileSync(p + ".sha-ok", h),
    download: async (url, dest) => {
      const r = await fetch(url); if (!r.ok) throw new Error(`${r.status}`);
      mkdirSync(dirname(dest), { recursive: true });
      // Stream the body straight to disk. Buffer.from(await r.arrayBuffer()) buffered the WHOLE
      // file in memory and threw a ">2GB length" RangeError on Node's Buffer/ArrayBuffer cap, so
      // any model over ~2GB (Qwen-Image-Edit GGUF ~14GB, RealVisXL 6.94GB) could never download.
      // Streaming has no size limit; pipeline closes both streams and rejects on any error.
      await pipeline(Readable.fromWeb(r.body), createWriteStream(dest));
    },
    sha256: (p) => new Promise((res, rej) => {
      const h = createHash("sha256"); const s = createReadStream(p);
      s.on("data", (d) => h.update(d)); s.on("end", () => res(h.digest("hex"))); s.on("error", rej);
    }),
    resolveDeps: async (packs) => {
      // Runs AFTER ensurePack (packs on disk at their pins): gather every pack's
      // requirements.txt, ONE uv compile (UV_CONSTRAINT pins the host CUDA stack),
      // ONE constrained pip install of the lock. Packs with no requirements file
      // are import-only — nothing to install.
      const reqPaths = packs
        .map((p) => join(comfyDir, "custom_nodes", p.name, "requirements.txt"))
        .filter((p) => existsSync(p));
      if (!reqPaths.length) return;
      const { env } = await hostPins();
      const uvExe = join(dirname(comfyPy), "uv.exe");
      const lockPath = join(tmpdir(), `offload-pack-lock-${process.pid}.txt`);
      await run(buildCompileCmd(reqPaths, { uvExe, comfyPy, lockPath }), { env: { ...process.env, ...env } });
      await run(buildInstallCmd(lockPath, { comfyPy }), { env: { ...process.env, ...env } });
    },
    ensurePack: makeEnsurePack({
      comfyDir,
      exists: existsSync,
      gitHead: (dir) => runOut(["git", "-C", dir, "rev-parse", "HEAD"]),
      gitRun: (cmd) => run(cmd),
    }),
    // Returns { ok: true } or { ok: false, reason } — the reason is surfaced in the
    // VENV_INCOHERENT defer detail so a consuming workflow can tell host-pin DRIFT
    // (which pinned package moved) from an ordinary dependency CONFLICT, instead of an
    // opaque "conflicting installed dependencies".
    pipCheck: async () => {
      try {
        try {
          await run([comfyPy, "-m", "pip", "check"]);
        } catch (e) {
          return { ok: false, reason: "pip check reported conflicting installed dependencies: " + String(e.message || e) };
        }
        // Tripwire (belt + suspenders under the constraints env): if provisioning moved a
        // HOST_PINNED package anyway, that venv would break the existing render path —
        // report incoherent (→ typed VENV_INCOHERENT defer), never a silent torch swap.
        const { pins } = await hostPins();
        if (pins.length) {
          const now = buildHostConstraints(await runOut([comfyPy, "-m", "pip", "freeze"]));
          if ([...pins].sort().join("|") !== [...now].sort().join("|")) {
            const detail = "host-pin drift — a pinned package moved during provisioning: expected [" + pins.join(", ") + "] got [" + now.join(", ") + "]";
            console.error(detail);
            return { ok: false, reason: detail };
          }
        }
        return { ok: true };
      } catch (e) { return { ok: false, reason: "venv coherence check failed: " + String(e.message || e) }; }
    },
  };
}
