// render/manifest-satisfy.mjs — satisfy a node manifest = make the graph's env real
// (spec §3). "Satisfied" means import + coherence (Task 6 adds packs + the /object_info
// gate); this file starts with the models leg. All side-effecting deps are injected so
// the whole satisfier is testable with zero network/disk.
import { join, dirname } from "node:path";
import { checkNodePack, checkModelEntry, normalizeModelPath } from "./node-packs.mjs";

const defer = (code, ref, detail) => ({ ok: false, defer: { code, ref, detail } });

const isAbsPath = (p) => /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("/") || p.startsWith("\\");
const joinBase = (base, dir) =>
  base ? base.replace(/[\\/]+$/, "") + "/" + dir.replace(/^[\\/]+/, "") : dir;

// parseExtraModelPaths: read ComfyUI's extra_model_paths.yaml well enough to know WHERE a
// model class may physically live. This repo ships zero npm dependencies, so rather than
// pull in a YAML parser we handle exactly the shape ComfyUI documents — a provider block
// with `base_path` plus `category: dir` entries, where a category may also be a block
// scalar (`unet: |`) listing several dirs. It is deliberately FAIL-SAFE: anything it cannot
// parse yields no roots, which degrades to the historical comfyDir-only behavior.
export function parseExtraModelPaths(text) {
  const roots = {};
  if (!text || typeof text !== "string") return roots;
  const add = (category, basePath, dir) => {
    if (!dir) return;
    (roots[category] ||= []).push(isAbsPath(dir) ? dir : joinBase(basePath, dir));
  };
  try {
    let basePath = "", pendingKey = null, pendingIndent = 0;
    for (const raw of text.split(/\r?\n/)) {
      const line = raw.replace(/\s+$/, "");
      const t = line.trim();
      if (!t || t.startsWith("#")) continue;
      const indent = line.length - line.trimStart().length;
      // Deeper-indented lines after `key: |` are that category's extra dirs.
      if (pendingKey && indent > pendingIndent) { add(pendingKey, basePath, t); continue; }
      pendingKey = null;
      const m = /^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/.exec(t);
      if (!m) continue;
      const key = m[1], val = m[2].replace(/\s+#.*$/, "").trim();
      if (indent === 0) { basePath = ""; continue; }   // provider block header
      if (key === "base_path") { basePath = val; continue; }
      if (val === "|" || val === ">" || val === "") { pendingKey = key; pendingIndent = indent; continue; }
      add(key, basePath, val);
    }
  } catch { return {}; }
  return roots;
}

// modelCandidates: every absolute path a manifest model MIGHT occupy — the canonical
// comfyDir location first, then each dir registered for that model class in
// extra_model_paths.yaml. Manifest paths look like `models/<category>/<rest>`.
//
// ComfyUI reads two classes from a second, legacy directory too: diffusion_models also lists models/unet, and text_encoders also lists
// models/clip (folder_paths.py), and a yaml provider that registers the legacy key (`unet:`, `clip:`) adds its directories to the same
// class. A path that names the modern class is therefore also looked for under the legacy directory and the legacy key, after the
// modern ones. A path that names the legacy class is looked for as it always was (a template names the modern class).
const LEGACY_OF = { diffusion_models: "unet", text_encoders: "clip" };

export function modelCandidates(relPath, comfyDir, extraRoots) {
  const out = [join(comfyDir, relPath)];
  const m = /^models\/([^/]+)\/(.+)$/.exec(String(relPath || "").replace(/\\/g, "/"));
  if (!m) return out;
  for (const dir of (extraRoots && extraRoots[m[1]]) || []) out.push(join(dir, m[2]));
  const legacy = Object.hasOwn(LEGACY_OF, m[1]) ? LEGACY_OF[m[1]] : null;
  if (legacy) {
    out.push(join(comfyDir, "models", legacy, m[2]));
    for (const dir of (extraRoots && extraRoots[legacy]) || []) out.push(join(dir, m[2]));
  }
  return [...new Set(out)];
}

export async function satisfyModels(models, deps) {
  const { comfyDir, exists, download, sha256, sentinelOk, writeSentinel, extraRoots = {} } = deps;
  // FIRST, before any file is looked for, hashed or fetched: every entry must land under models/<class>/ and come, if
  // from anywhere, over https from a listed host (node-packs.json). The path is joined onto the ComfyUI directory and
  // the source is fetched into it before ComfyUI starts, so an entry the caller chose freely is a file the caller
  // placed where ComfyUI loads code, or anywhere above it. All entries are read before the first is provisioned, so a
  // refusal leaves nothing downloaded.
  for (const m of models || []) {
    const refusal = checkModelEntry(m);
    if (refusal) return defer(refusal.code, refusal.ref, refusal.detail);
  }
  // beat (optional) tells the lease's progress file which model the satisfier is on (lease-progress.mjs): this
  // step runs before ComfyUI exists, so no poll loop is beating, and a manifest of several large models is the one
  // place a run-graph job is quiet for longer than a lease's stall window. One model's download or hash can still
  // outlast it; that stretch reads held-stalled until the next beat.
  const beat = typeof deps.beat === "function" ? deps.beat : () => {};
  const unverified = [];
  for (const m of models || []) {
    beat(`manifest: checking ${m.path}`);
    const rel = normalizeModelPath(m.path);
    const abs = join(comfyDir, rel);
    // Presence resolves across ComfyUI's FULL search path, not just comfyDir. A model that
    // lives only on a secondary tree (e.g. the V: Optane drive registered via
    // extra_model_paths.yaml) otherwise reads as MISSING and is re-downloaded to C: — 15GB
    // per run for a big GGUF, which is exactly what the scene-swap consumer hit live.
    const found = modelCandidates(rel, comfyDir, extraRoots).find((p) => exists(p)) || null;
    if (found) {
      if (!m.sha256) continue;            // present but unverifiable — trust on-disk
      if (sentinelOk(found)) continue;    // already verified once
      // Present, sha pinned, but no .sha-ok sidecar: the hand/curl-provisioned case. Hash it
      // ONCE and adopt it by writing the sentinel. Previously this failed the skip gate and
      // fell into the download branch, re-fetching a file that was already byte-correct.
      try {
        const got = await sha256(found);
        if (got === m.sha256) { writeSentinel(found, got); continue; }
        // Wrong bytes: replaceable only if we have a source. Otherwise name the REAL problem
        // (a mismatching file) instead of the misleading "missing on disk".
        if (!m.source_url) return defer("MODEL_SHA_MISMATCH", m.path, `want ${m.sha256} got ${got}`);
      } catch (e) {
        return defer("MODEL_DOWNLOAD_FAILED", m.path, "present-file verify failed: " + String(e.message || e));
      }
    }
    if (!m.source_url) return defer("MODEL_DOWNLOAD_FAILED", m.path, "missing on disk and no source_url");
    beat(`manifest: downloading ${m.path}`);
    try { await download(m.source_url, abs); }
    catch (e) { return defer("MODEL_DOWNLOAD_FAILED", m.path, String(e.message || e)); }
    beat(`manifest: downloaded ${m.path}`);
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
// so the files must exist first. uv is driven directly. "Unified, never sequential
// per-pack": one compile over ALL packs' requirements at once.
// isSpawnFailure: was this error the SUBPROCESS FAILING TO START (vs the subprocess running
// and reporting a real problem)? Node's child_process surfaces these via the "error" event
// with a syscall of "spawn ..." and codes like ENOENT/EACCES/UNKNOWN. The distinction is
// load-bearing: a spawn failure says nothing about the venv, so reporting it as
// VENV_INCOHERENT (live CMP report 2026-07-20, "spawn UNKNOWN" after a long batch) sends the
// operator chasing a healthy torch install.
export function isSpawnFailure(e) {
  if (!e || typeof e !== "object") return false;
  if (typeof e.syscall === "string" && e.syscall.startsWith("spawn")) return true;
  return typeof e.message === "string" && /^spawn\b/.test(e.message) && !/exit \d+/.test(e.message);
}

// runRetryOnSpawn: run fn; on a SPAWN failure only, wait briefly and retry exactly once.
// CMP's live spawn failure was transient (appeared after a long render batch, gone later) —
// one cheap retry absorbs that class. Real failures (non-zero exits) are never retried:
// pip/uv/git are deterministic enough that a second identical run just doubles the latency.
export async function runRetryOnSpawn(fn, { delayMs = 500 } = {}) {
  try { return await fn(); }
  catch (e) {
    if (!isSpawnFailure(e)) throw e;
    await new Promise((r) => setTimeout(r, delayMs));
    return fn();
  }
}

// NETWORK / BUSY: the messages of a command that failed because the network or the card was not there, not because of what it
// was asked to install. Shared with the update script (comfy-packs-update.mjs), so both classify the same way.
export const NETWORK = /could not resolve host|name or service not known|temporary failure in name resolution|connection (timed out|refused|reset|aborted)|network is unreachable|no route to host|failed to connect|unable to access|\b(ssl|tls)\b|timed out|max retries exceeded|connectionerror|read timed out|remote end hung up/i;
export const BUSY = /out of memory|CUDA error: (busy|out of memory)|device or resource busy/i;

// isTransientFailure: a failure that says nothing about the pack, only that the machine could not do the work just now
// (a subprocess that failed to start, a lost network, a timeout, a busy or full card). It is never held against a commit.
export function isTransientFailure(e) {
  if (isSpawnFailure(e)) return true;
  const msg = String((e && e.message) || e || "");
  return NETWORK.test(msg) || BUSY.test(msg);
}

// freezeDigest: a short sha256 of a `pip freeze` text (sorted, trimmed, comment lines dropped). A rejection stores it, so a
// rejection written against one venv is not held against a venv that has changed since.
export function freezeDigest(text) {
  const lines = String(text || "").split(/\r?\n/).map((l) => l.trim()).filter((l) => l && !l.startsWith("#")).sort();
  return lines.length ? createHash("sha256").update(lines.join("\n")).digest("hex").slice(0, 16) : "";
}

// pipCheckGained: the lines of a `pip check` output that were not in the baseline.
export function pipCheckGained(baselineText, nowText) {
  const had = new Set(String(baselineText || "").split(/\r?\n/).map((l) => l.trim()).filter(Boolean));
  return String(nowText || "").split(/\r?\n/).map((l) => l.trim()).filter((l) => l && !had.has(l));
}

export async function satisfyManifest(manifest, deps) {
  const {
    satisfierAvailable, comfyVersion, resolveDeps, ensurePack, pipCheck,
  } = deps;
  // beat (optional) keeps the lease's progress file moving through the pack leg: a clone, a uv compile and a pip install
  // are quiet stretches, and satisfyModels beats only for models.
  const beat = typeof deps.beat === "function" ? deps.beat : () => {};
  // discardFreshPacks (optional): undo what THIS satisfy created when a later step fails, so a failed first install leaves
  // no half pack on disk (see defaultSatisfyDeps).
  const discardFresh = async (fresh, opts) => {
    if (!fresh.length || typeof deps.discardFreshPacks !== "function") return;
    try { await deps.discardFreshPacks(fresh, opts); } catch { /* best effort: the typed defer matters more */ }
  };

  const hasPacks = (manifest.node_packs || []).length > 0;

  // FIRST, before any tooling probe, model download or git command: a pack is taken only from an allowlisted
  // repository (node-packs.json). Naming one makes this node run the caller's code, and a manifest that gets
  // that far with a model download already done has spent the node's disk on a request it was going to refuse.
  // deps.packAllowed is a seam for tests that use stand-in packs; production leaves it unset, which is the table.
  const packAllowed = deps.packAllowed || checkNodePack;
  for (const pack of manifest.node_packs || []) {
    const refusal = packAllowed(pack);
    if (refusal) return defer(refusal.code, refusal.ref, refusal.detail);
  }
  // Models too: a path that leaves models/<class>/ or a source that is not a listed https host is refused here, before
  // the tooling probe, the version read, a download or a clone (satisfyModels checks again for a caller that reaches it directly).
  for (const m of manifest.models || []) {
    const refusal = checkModelEntry(m);
    if (refusal) return defer(refusal.code, refusal.ref, refusal.detail);
  }

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

  // Spawn failures get their own typed code everywhere below: "the tool could not RUN" and
  // "the tool ran and found your venv broken" demand different operator actions.
  const spawnOrVenv = (e, ref) =>
    isSpawnFailure(e)
      ? defer("SATISFIER_SPAWN_FAILED", ref, String(e.message || e))
      : defer("VENV_INCOHERENT", ref, String(e.message || e));

  const retryOpts = { delayMs: deps.retryDelayMs ?? 500 };
  // The durable "this exact pack set was resolved+checked successfully" marker. Injectable;
  // production reads/writes a sidecar under custom_nodes. Read is fail-safe (null on error).
  const readDepsMarker = deps.readDepsMarker || (() => null);
  const writeDepsMarker = deps.writeDepsMarker || (() => {});
  // The packs key is built from the commits the checkout stage reports, once every pack is on disk.
  let packsKey = "";

  let changed = false;
  let warning;
  if (hasPacks) {
    // 1. Bring every pack to the head of its default branch (git-only). Deliberately NOT retried as a unit:
    //    a retry would recompute the HEAD-before against attempt 1's surviving side effects
    //    (fresh clone / moved checkout) and misreport changed=false for a pack that just
    //    changed (adversarial-review repro, 2026-07-22). A git spawn failure defers typed —
    //    the caller can re-run the whole satisfy, which IS idempotent at this level.
    // The venv as pip check sees it BEFORE this satisfy clones or installs anything: a conflict that is already there is
    // not the new pack's doing, so only lines pip check gains afterwards count against a fresh pack (below).
    let pipBaseline = null;
    if (typeof deps.pipCheckText === "function") { try { pipBaseline = await deps.pipCheckText(); } catch { pipBaseline = null; } }
    const keyParts = [];
    const fresh = [];   // packs this satisfy cloned: the only directories it may remove again
    for (const pack of manifest.node_packs) {
      let res;
      beat(`manifest: checking out ${pack.name}`);
      try { res = await ensurePack(pack); }
      catch (e) {
        await discardFresh(fresh, { reject: false, reason: "" });
        // The checkout stage refuses a pack off the allowlist too (makeEnsurePack), and refuses one whose newest commit
        // failed an earlier install: the same typed answer, never "the venv is broken".
        if (e && (e.code === "NODE_PACK_NOT_ALLOWED" || e.code === "NODE_PACK_REJECTED")) return defer(e.code, e.ref || pack.name, e.detail || String(e.message || e));
        return spawnOrVenv(e, pack.name);
      }
      if (res.changed) changed = true;
      if (res.fresh) fresh.push({ pack, commit: res.commit || "" });
      keyParts.push(`${pack.name}@${res.commit || ""}`);
      beat(`manifest: ${pack.name} checked out`);
    }
    packsKey = keyParts.sort().join("|");
    // 2. ONE unified resolve+install over all packs' on-disk requirements (under the
    //    host-torch constraints env), never per-pack pip. Skipped ONLY when nothing moved
    //    AND the persisted marker proves this exact pack set was already resolved+checked
    //    successfully — "git didn't move this run" alone is NOT that proof: a prior run
    //    that checked packs out and then failed before installing leaves changed=false
    //    forever (adversarial-review finding, 2026-07-22).
    const alreadySatisfied = !changed && readDepsMarker() === packsKey && packsKey !== "";
    if (!alreadySatisfied) {
      beat("manifest: resolving and installing pack requirements");
      try { await runRetryOnSpawn(() => resolveDeps(manifest.node_packs), retryOpts); }
      catch (e) {
        // A subprocess that would not start, a lost network, a timeout or a busy card says nothing about the pack: the
        // directory this satisfy cloned is removed again, the commit is NOT recorded as rejected, and the defer is retryable.
        const transient = isTransientFailure(e);
        await discardFresh(fresh, { reject: !transient, reason: "dependency resolve or install failed: " + String(e.message || e) });
        const out = spawnOrVenv(e, "uv-resolve");
        if (transient && !isSpawnFailure(e)) out.defer.detail = "retryable (network or busy, not held against the pack): " + out.defer.detail;
        return out;
      }
    }
    // 3. The cheap coherence check always runs. If its subprocess cannot even START:
    //    fail OPEN with a warning ONLY for a proven-satisfied unchanged env (the marker,
    //    not just an idle git); anything else fails closed.
    let pc;
    beat("manifest: checking the venv is coherent");
    try { pc = await runRetryOnSpawn(() => pipCheck(), retryOpts); }
    catch (e) {
      if (!isSpawnFailure(e)) {
        await discardFresh(fresh, { reject: !isTransientFailure(e), reason: "venv coherence check failed: " + String(e.message || e) });
        return defer("VENV_INCOHERENT", "pip-check", String(e.message || e));
      }
      if (!alreadySatisfied) {
        await discardFresh(fresh, { reject: false, reason: "" });
        return defer("SATISFIER_SPAWN_FAILED", "pip-check", String(e.message || e));
      }
      warning = "coherence check skipped (its subprocess failed to spawn: " + String(e.message || e) + "); pack set unchanged and previously satisfied, trusting it";
      pc = { ok: true };
    }
    if (!pc.ok) {
      // Reject the commit only when pip check now reports lines the baseline did not have. A conflict that was there before
      // this satisfy, or a check that could not run again, is not a verdict on the pack. A drift of a protected package
      // is not a pip check line and stays the pack's.
      let own = true, note = "";
      if (pipBaseline && /^pip check reported/.test(String(pc.reason || ""))) {
        try {
          const post = await deps.pipCheckText();
          if (!pipCheckGained(pipBaseline.out, post.out).length) { own = false; note = " (the same conflicts were there before this install; not held against the pack)"; }
        } catch { own = false; }
      }
      await discardFresh(fresh, { reject: own, reason: String(pc.reason || "venv coherence check failed") });
      return defer("VENV_INCOHERENT", "pip-check", String(pc.reason || "") + note);
    }
    // Record success — the ONLY writer of the marker, after resolve+check both passed.
    try { writeDepsMarker(packsKey); } catch { /* marker is an optimization; never fail satisfy */ }
  }

  return { ok: true, changed, unverified: models.unverified, packsKey, ...(warning ? { warning } : {}) };
}

import { spawn as nodeSpawn } from "node:child_process";

// SATISFY_LIVE: the children run(), runOut() and pipCheckText() have in flight, so a caller's signal handler can stop a
// pip or uv that is still writing the venv before it restores that venv (comfy-packs-update's killLive).
export const SATISFY_LIVE = new Set();
function track(c) {
  SATISFY_LIVE.add(c);
  c.once("close", () => SATISFY_LIVE.delete(c)); c.once("error", () => SATISFY_LIVE.delete(c));
  return c;
}
import { existsSync, writeFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { writeFileAtomic } from "./atomic-out.mjs";
import { createHash } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";

// Command builders are pure (unit-tested); execution is in defaultSatisfyDeps.
// Unified resolve: ONE `uv pip compile` across ALL packs' requirements files at once
// (cross-pack conflict detection — the ecosystem's documented failure mode is
// last-writer-wins sequential pip), then ONE constrained install of the lock.
export function buildCompileCmd(reqPaths, { uvExe, comfyPy, lockPath }) {
  return [uvExe, "pip", "compile", "--python", comfyPy, ...reqPaths, "-o", lockPath];
}
export function buildInstallCmd(lockPath, { comfyPy }) {
  return [comfyPy, "-m", "pip", "install", "-r", lockPath];
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
// CUDA stack: every pip/uv the satisfier spawns runs under a constraints file holding
// these packages where they are (PIP_CONSTRAINT is honored by pip, UV_CONSTRAINT by uv). The constraints
// are read from the live venv on each run and discarded after the step; none is stored. A
// resolve that cannot satisfy a pack within them fails loud → typed VENV_INCOHERENT defer,
// never a silently replaced torch.
const HOST_PROTECTED = ["torch", "torchvision", "torchaudio", "numpy"];

// publicPin: strip a PEP 440 local-version suffix ("torch 2.11.0+cu128" →
// "torch 2.11.0") for the CONSTRAINTS FILE — local builds live on the PyTorch
// index, not PyPI, so a resolver given the +cu128 line finds "no version exists"
// (live finding 2026-07-17). PEP 440: the installed +cu128 build SATISFIES the
// public 2.11.0 requirement, so pip neither reinstalls nor upgrades it; the post-install
// TRIPWIRE still compares the full local-version lines, guarding the exact build.
export function publicPin(line) {
  return line.replace(/\+[A-Za-z0-9.]+$/, "");
}

// buildHostConstraints: extract the protected packages' lines from `pip freeze`
// output (local-version suffixes like +cu128 preserved). Pure; unit-tested.
export function buildHostConstraints(freezeText) {
  return String(freezeText || "")
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => {
      const eq = l.indexOf("==");
      return eq > 0 && HOST_PROTECTED.includes(l.slice(0, eq).trim().toLowerCase());
    });
}

async function run(cmd, opts = {}) {
  return new Promise((resolve, reject) => {
    const c = track(nodeSpawn(cmd[0], cmd.slice(1), { stdio: "pipe", ...opts, windowsHide: true }));
    let err = "";
    c.stderr.on("data", (d) => (err += d));
    c.on("error", reject);
    c.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`${cmd[0]} exit ${code}: ${err.slice(-300)}`))));
  });
}

// runOut: like run() but resolves with trimmed stdout (used to read `git rev-parse HEAD`).
async function runOut(cmd, opts = {}) {
  return new Promise((resolve, reject) => {
    const c = track(nodeSpawn(cmd[0], cmd.slice(1), { stdio: ["ignore", "pipe", "pipe"], ...opts, windowsHide: true }));
    let out = "", err = "";
    c.stdout.on("data", (d) => (out += d));
    c.stderr.on("data", (d) => (err += d));
    c.on("error", reject);
    c.on("exit", (code) => (code === 0 ? resolve(out.trim()) : reject(new Error(`${cmd[0]} exit ${code}: ${err.slice(-300)}`))));
  });
}

// readPackRecord: the pack's record beside it, or null when there is none or it does not parse.
export function readPackRecord(pack,{ comfyDir, read = (p) => readFileSync(p, "utf8") }) {
  try { return JSON.parse(read(`${comfyDir}/custom_nodes/${pack.name}.installed.json`)); } catch { return null; }
}

// recordPackInstall: write what a pack is at, and when it got there, beside the pack
// (custom_nodes/<name>.installed.json: ComfyUI loads only directories and .py files from custom_nodes, so the
// record is inert there). Schema 2: {schema, repo, commit, installed_at, checked_at, previous, rejected}. `repo` is the
// origin the checkout itself reports (`origin`), else the manifest's string; `previous` is the commit and time the pack
// held before this one (null when it never held another); `rejected` is the commits an earlier install of this pack
// failed on, kept as they were. A record that already says schema 2 and names this commit and repo is left alone, so
// installed_at stays the time the pack reached it. It is a record of what is installed, not an instruction to install it.
// A record the node cannot write never fails the install.
export function recordPackInstall(pack, commit, { comfyDir, write = writeFileAtomic, read = (p) => readFileSync(p, "utf8"), now = () => new Date().toISOString(), origin = null }) {
  try {
    const path = `${comfyDir}/custom_nodes/${pack.name}.installed.json`;
    const held = readPackRecord(pack, { comfyDir, read });
    const repo = origin || pack.repo;
    if (held && held.schema === 2 && held.commit === commit && held.repo === repo) return;
    const same = !!(held && held.commit === commit);
    const stamp = now();
    const rec = {
      schema: 2,
      repo,
      commit,
      installed_at: same && held.installed_at ? held.installed_at : stamp,
      checked_at: stamp,
      previous: same ? (held.previous || null)
        : (held && held.commit ? { commit: held.commit, installed_at: held.installed_at || null } : null),
      rejected: held && Array.isArray(held.rejected) ? held.rejected : [],
    };
    write(path, JSON.stringify(rec, null, 2) + "\n");
  } catch { /* the record is informational */ }
}

// rejectPackInstall: a pack this satisfy cloned did not survive the install, so its directory is gone: the record now says
// that nothing is installed and, when `commit` is given, names the commit that failed, so a later request does not clone
// and fail on the same one.
export function rejectPackInstall(pack, commit, reason, { comfyDir, write = writeFileAtomic, read = (p) => readFileSync(p, "utf8"), now = () => new Date().toISOString(), origin = null, freeze = "" }) {
  try {
    const path = `${comfyDir}/custom_nodes/${pack.name}.installed.json`;
    const held = readPackRecord(pack, { comfyDir, read });
    const rejected = held && Array.isArray(held.rejected) ? held.rejected.filter((r) => r && r.commit !== commit) : [];
    const stamp = now();
    if (commit) rejected.push({ commit, reason: String(reason || "").slice(0, 300), ts: stamp, ...(freeze ? { freeze } : {}) });
    write(path, JSON.stringify({ schema: 2, repo: origin || pack.repo, commit: null, installed_at: null, checked_at: stamp, previous: null, rejected }, null, 2) + "\n");
  } catch { /* the record is informational */ }
}

// currentPacksKey: the sorted `name@commit` key of the packs a manifest names, as they are on disk now (an absent pack
// has an empty commit). It is the same key satisfyManifest builds after its checkout stage, so a cache that holds the
// key it stored can tell that a pack was moved, replaced or deleted since.
export async function currentPacksKey(packs, { comfyDir, exists = existsSync, gitHead }) {
  const parts = [];
  for (const pack of packs || []) {
    const dir = `${comfyDir}/custom_nodes/${pack.name}`;
    let commit = "";
    if (exists(dir)) { try { commit = await gitHead(dir); } catch { commit = ""; } }
    parts.push(`${pack.name}@${commit}`);
  }
  return parts.sort().join("|");
}

// makeEnsurePack: build the per-pack "clone if absent, report changed" step from injectable primitives so it is
// unit-testable without spawning git. A pack that is absent is cloned at the head of its repository's default branch
// (changed); a pack already on disk is used as it is (not changed). A render request never moves an installed pack:
// moving one to a newer commit is the operator's update (`git -C custom_nodes/<name> pull --ff-only`, or the fleet's
// update tooling), so a caller holding only the render bearer cannot make a node fetch and run whatever an upstream
// pushed a minute ago. The commit the pack is at is returned and recorded (recordPackInstall); the dependency marker
// and the run-graph cache are keyed on it, so a pack the operator moved is re-resolved on the next request that names it.
// `fresh` marks a pack this call cloned. An absent pack whose record lists rejected commits is cloned only when the
// repository's newest commit (`lsRemoteHead`, null when it cannot be read) is not one of them: a commit that already
// failed an install is not cloned and failed again, and a newer one is a fresh candidate. A rejection that carries the digest of the
// venv's `pip freeze` it was written against (`freeze`) is ignored once the venv's digest (`freezeHash`, "" when unreadable) differs: a
// venv that has been fixed since is not locked out of the commit.
export function makeEnsurePack({ comfyDir, freezeHash = async () => "", exists = existsSync, gitHead, gitRun, packAllowed = checkNodePack, buildClone = buildClonePackCmd, gitOrigin = async () => null, lsRemoteHead = async () => null, readRecord = (pack) => readPackRecord(pack, { comfyDir }), record = (pack, commit, extra) => recordPackInstall(pack, commit, { comfyDir, ...extra }) }) {
  return async (pack) => {
    // The last stop before git: the directory, the clone and the checkout all come from the pack, so a pack that is
    // not allowlisted is refused before any of them is looked at (satisfyManifest has refused it already; this is for a
    // caller that reaches the checkout stage another way).
    const refusal = packAllowed(pack);
    if (refusal) {
      throw Object.assign(new Error(`node pack ${refusal.ref} refused: ${refusal.detail}`), refusal);
    }
    const dir = `${comfyDir}/custom_nodes/${pack.name}`;
    const existed = exists(dir);
    if (!existed) {
      const held = readRecord(pack);
      const held_rejected = held && Array.isArray(held.rejected) ? held.rejected.filter((r) => r && r.commit) : [];
      let venv = null;   // the venv's digest, read once and only when a rejection carries one
      if (held_rejected.some((r) => r.freeze)) { try { venv = await freezeHash(); } catch { venv = ""; } }
      const rejected = held_rejected.filter((r) => !r.freeze || !venv || r.freeze === venv).map((r) => r.commit);
      if (rejected.length) {
        let head = null;
        try { head = await lsRemoteHead(pack.repo); } catch { head = null; }
        if (head && rejected.includes(head)) {
          throw Object.assign(new Error(`node pack ${pack.name} not installed: its newest commit failed an earlier install`), {
            code: "NODE_PACK_REJECTED", ref: pack.name,
            detail: `the newest commit of ${pack.repo} (${head}) failed an earlier install on this node and is not cloned again; a newer commit is tried when the repository moves`,
          });
        }
      }
      await gitRun(buildClone(pack, { comfyDir }));
    }
    const commit = await gitHead(dir);
    let origin = null;
    try { origin = await gitOrigin(dir); } catch { origin = null; }
    record(pack, commit, { origin });
    return { changed: !existed, commit, fresh: !existed };
  };
}

export function defaultSatisfyDeps({ comfyDir, comfyPy, api }) {
  // Host constraints are captured ONCE, lazily, at the first python-touching step
  // (resolveDeps — ensurePack before it is git-only), so the tripwire below always
  // compares against the pre-install state. resolveDeps writes the constraints file
  // from them before any pip/uv spawns, and removes it when the step ends.
  let hostConstraintsP = null;
  // A REJECTED promise must not stay memoized: ??= treats it as present, so a transient
  // spawn failure in `pip freeze` would make every retry re-await the same stale rejection
  // (adversarial-review finding, 2026-07-22). Clear the memo on rejection so a retry
  // actually re-spawns.
  const hostConstraints = () => (hostConstraintsP ??= ((async () => ({
    held: buildHostConstraints(await runOut([comfyPy, "-m", "pip", "freeze"])),
  }))()).catch((e) => { hostConstraintsP = null; throw e; }));
  return {
    comfyDir,
    // The pack satisfier's hard tool is uv (unified resolve); derived from the venv
    // python's Scripts dir.
    satisfierAvailable: () => existsSync(comfyPy) && existsSync(join(dirname(comfyPy), "uv.exe")),
    comfyVersion: async () => {
      try { const r = await fetch(`${api}/system_stats`); const j = await r.json(); return j?.system?.comfyui_version || j?.comfyui_version || null; }
      catch { return null; }
    },
    // ComfyUI resolves models across extra_model_paths.yaml; so must presence-checking, or
    // a model held on a secondary tree looks missing and gets re-downloaded. Read once,
    // fail-safe: an unreadable/absent file leaves the roots empty (comfyDir-only behavior).
    extraRoots: (() => {
      try {
        const p = join(comfyDir, "extra_model_paths.yaml");
        return existsSync(p) ? parseExtraModelPaths(readFileSync(p, "utf8")) : {};
      } catch { return {}; }
    })(),
    exists: (p) => existsSync(p),
    // pip check as text: {code, out}, whatever the exit code (the baseline and the delta of satisfyManifest). Rejects only when pip could not start.
    pipCheckText: () => new Promise((resolve, reject) => {
      const c = track(nodeSpawn(comfyPy, ["-m", "pip", "check"], { stdio: ["ignore", "pipe", "pipe"], windowsHide: true }));
      let out = "";
      c.stdout.on("data", (d) => (out += d));
      c.stderr.on("data", (d) => (out += d));
      c.on("error", reject);
      c.on("close", (code) => resolve({ code: code === null ? -1 : code, out }));
    }),
    // The digest of this venv's `pip freeze --all`; "" when it cannot be read (an unreadable venv never invalidates a rejection).
    freezeHash: async () => { try { return freezeDigest(await runOut([comfyPy, "-m", "pip", "freeze", "--all"])); } catch { return ""; } },
    // Durable "this exact pack set was resolved+checked successfully" marker. Written only
    // after a fully successful resolve+check; read fail-safe. It attests VENV state, so it
    // lives INSIDE the venv (beside the interpreter's home): a wiped/recreated venv takes
    // the claim down with it. The other wipe case needs no marker help — deleting pack dirs
    // forces a fresh clone, which reports changed=true and re-resolves regardless.
    readDepsMarker: () => {
      try { return readFileSync(join(dirname(dirname(comfyPy)), ".offload-deps-satisfied"), "utf8").trim(); }
      catch { return null; }
    },
    writeDepsMarker: (key) => {
      try { writeFileSync(join(dirname(dirname(comfyPy)), ".offload-deps-satisfied"), key); }
      catch { /* optimization only — never fail satisfy over the marker */ }
    },
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
      // Runs AFTER ensurePack (packs on disk): gather every pack's
      // requirements.txt, ONE uv compile (UV_CONSTRAINT holds the host CUDA stack),
      // ONE constrained pip install of the lock. Packs with no requirements file
      // are import-only — nothing to install.
      const reqPaths = packs
        .map((p) => join(comfyDir, "custom_nodes", p.name, "requirements.txt"))
        .filter((p) => existsSync(p));
      if (!reqPaths.length) return;
      const { held } = await hostConstraints();
      const uvExe = join(dirname(comfyPy), "uv.exe");
      // The constraints file and the unified lock are scratch for THIS step: they live in a folder of
      // their own that is removed when the step ends, however it ends. (They were pid-named files in
      // the OS temp dir that nothing removed, so every provisioning run left two behind.)
      const scratch = mkdtempSync(join(tmpdir(), "offload-satisfy-"));
      try {
        let env = {};
        if (held.length) {
          const p = join(scratch, "host-constraints.txt");
          // Constraints file carries the PUBLIC lines (local +cuXXX builds don't exist
          // on PyPI → unresolvable); the tripwire compares the FULL lines.
          writeFileSync(p, held.map(publicPin).join("\n") + "\n");
          env = { PIP_CONSTRAINT: p, UV_CONSTRAINT: p };
        }
        const lockPath = join(scratch, "pack-lock.txt");
        await run(buildCompileCmd(reqPaths, { uvExe, comfyPy, lockPath }), { env: { ...process.env, ...env } });
        await run(buildInstallCmd(lockPath, { comfyPy }), { env: { ...process.env, ...env } });
      } finally {
        try { rmSync(scratch, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }); } catch { /* best effort: the install's own result matters more */ }
      }
    },
    ensurePack: makeEnsurePack({
      comfyDir,
      exists: existsSync,
      gitHead: (dir) => runOut(["git", "-C", dir, "rev-parse", "HEAD"]),
      gitRun: (cmd) => run(cmd),
      gitOrigin: (dir) => runOut(["git", "-C", dir, "remote", "get-url", "origin"]),
      freezeHash: async () => { try { return freezeDigest(await runOut([comfyPy, "-m", "pip", "freeze", "--all"])); } catch { return ""; } },
      lsRemoteHead: async (repo) => {
        const out = await runOut(["git", "ls-remote", "--", repo, "HEAD"]);
        const sha = out.split(/\s+/)[0] || "";
        return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
      },
    }),
    // The key of the packs a manifest names as they are on disk now (comfy-run-graph's cache test).
    packsKey: (packs) => currentPacksKey(packs, { comfyDir, gitHead: (dir) => runOut(["git", "-C", dir, "rev-parse", "HEAD"]) }),
    // Undo what a failed satisfy created: remove ONLY the directories it cloned itself (never a pack that was already
    // there), and, when the failure is the pack's own (`reject`), record the commit so it is not cloned and failed again.
    discardFreshPacks: async (fresh, { reject, reason }) => {
      let freeze = "";
      if (reject) { try { freeze = freezeDigest(await runOut([comfyPy, "-m", "pip", "freeze", "--all"])); } catch { freeze = ""; } }
      for (const { pack, commit } of fresh) {
        const dir = join(comfyDir, "custom_nodes", pack.name);
        if (checkNodePack(pack)) continue;   // a name the table does not give is never a path to remove
        let origin = null;
        try { origin = await runOut(["git", "-C", dir, "remote", "get-url", "origin"]); } catch { origin = null; }
        try { rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }); } catch { /* best effort */ }
        rejectPackInstall(pack, reject ? commit : null, reason, { comfyDir, origin, freeze });
      }
    },
    // Returns { ok: true } or { ok: false, reason } — the reason is surfaced in the
    // VENV_INCOHERENT defer detail so a consuming workflow can tell host constraint DRIFT
    // (which protected package moved) from an ordinary dependency CONFLICT, instead of an
    // opaque "conflicting installed dependencies".
    pipCheck: async () => {
      try {
        try {
          await run([comfyPy, "-m", "pip", "check"]);
        } catch (e) {
          // A spawn failure means pip never RAN — rethrow so the caller classifies it as
          // SATISFIER_SPAWN_FAILED instead of blaming the venv. Only a real non-zero exit
          // is evidence of conflicting dependencies.
          if (isSpawnFailure(e)) throw e;
          return { ok: false, reason: "pip check reported conflicting installed dependencies: " + String(e.message || e) };
        }
        // Tripwire (belt + suspenders under the constraints env): if provisioning moved a
        // HOST_PROTECTED package anyway, that venv would break the existing render path —
        // report incoherent (→ typed VENV_INCOHERENT defer), never a silent torch swap.
        const { held } = await hostConstraints();
        if (held.length) {
          const now = buildHostConstraints(await runOut([comfyPy, "-m", "pip", "freeze"]));
          if ([...held].sort().join("|") !== [...now].sort().join("|")) {
            const detail = "host constraint drift — a protected package moved during provisioning: expected [" + held.join(", ") + "] got [" + now.join(", ") + "]";
            console.error(detail);
            return { ok: false, reason: detail };
          }
        }
        return { ok: true };
      } catch (e) {
        if (isSpawnFailure(e)) throw e;   // never report "could not run" as incoherence
        return { ok: false, reason: "venv coherence check failed: " + String(e.message || e) };
      }
    },
  };
}

// --- trust on first use -------------------------------------------------------------------------------------------------------------
//
// Most template annotations carry no hash, so a model file the template run meets has nothing to be checked against. Trust on first use
// keeps the one guarantee that is left: the first run that meets the file hashes it and records the hash (and the file's size and
// modification time) in a record beside the file, `<file>.tofu`; a later run refuses a file whose hash is not the recorded one. It is a
// separate record from the `.sha-ok` sentinel on purpose: the sentinel means "matched the hash a manifest PINNED", and a trusted-on-first-
// use hash is no pin, so it must never make a pinned model skip its own check.
//
// A file whose size and modification time are the recorded ones is trusted without hashing it again (a 20 GB file takes minutes to hash on
// every run); a file whose either moved is hashed again, and only the same hash passes. The limit is the usual one of a stat cache: bytes
// replaced under an unchanged size and time are not seen, and a record is the first sighting's word for a file, not a statement about its
// origin. To trust a replaced file on purpose, delete its `.tofu` record.
//
// Only a model that asks for it (`trust: "tofu"`) and declares no hash is checked here; the rest is satisfyModels's business. The check
// runs on every run, a cached one included (comfy-run-graph.mjs): the cache says the environment was provisioned, not that the file is
// still the one that was trusted.
export async function verifyTofuModels(models, deps) {
  const { comfyDir, exists, extraRoots = {}, tofu } = deps;
  const now = typeof deps.now === "function" ? deps.now : () => new Date().toISOString();
  const beat = typeof deps.beat === "function" ? deps.beat : () => {};
  const first_seen = [];
  const matched = [];
  for (const m of models || []) {
    if (m.trust !== "tofu" || m.sha256) continue;
    const found = modelCandidates(m.path, comfyDir, extraRoots).find((p) => exists(p)) || null;
    if (!found) return defer("MODEL_DOWNLOAD_FAILED", m.path, "missing on disk (a template run never downloads a model)");
    beat(`manifest: trust on first use ${m.path}`);
    let rec;
    try { rec = tofu.read(found); } catch (e) { return defer("MODEL_TOFU_RECORD", m.path, `the trust-on-first-use record of ${found} cannot be read: ${String(e.message || e)}`); }
    if (rec !== null && rec !== undefined && (typeof rec !== "object" || typeof rec.sha256 !== "string" || rec.sha256 === "")) {
      return defer("MODEL_TOFU_RECORD", m.path, `the trust-on-first-use record of ${found} is malformed`);
    }
    let st;
    try { st = tofu.stat(found); } catch (e) { return defer("MODEL_DOWNLOAD_FAILED", m.path, `cannot stat ${found}: ${String(e.message || e)}`); }
    if (rec && rec.size === st.size && rec.mtime_ms === st.mtimeMs) { matched.push(m.path); continue; }
    let got;
    try { got = await tofu.sha256(found); } catch (e) { return defer("MODEL_TOFU_RECORD", m.path, `cannot hash ${found}: ${String(e.message || e)}`); }
    if (!rec) {
      try { tofu.write(found, { schema_version: 1, sha256: got, size: st.size, mtime_ms: st.mtimeMs, first_seen: now() }); }
      catch (e) { return defer("MODEL_TOFU_RECORD", m.path, `the trust-on-first-use record of ${found} cannot be written: ${String(e.message || e)}`); }
      first_seen.push(m.path);
      continue;
    }
    if (got !== rec.sha256) {
      return defer("MODEL_SHA_MISMATCH", m.path, `trust on first use: ${found} has sha256 ${got}, but the run that first met it (${rec.first_seen ?? "date unknown"}) recorded ${rec.sha256}; delete ${found}.tofu to trust this file instead`);
    }
    try { tofu.write(found, { ...rec, size: st.size, mtime_ms: st.mtimeMs }); }
    catch (e) { return defer("MODEL_TOFU_RECORD", m.path, `the trust-on-first-use record of ${found} cannot be updated: ${String(e.message || e)}`); }
    matched.push(m.path);
  }
  return { ok: true, first_seen, matched };
}

// defaultTofuDeps: the record store on the real filesystem. The record is `<file>.tofu` (JSON), written atomically.
export function defaultTofuDeps() {
  const record = (p) => `${p}.tofu`;
  return {
    stat: (p) => { const s = statSync(p); return { size: s.size, mtimeMs: s.mtimeMs }; },
    read: (p) => (existsSync(record(p)) ? JSON.parse(readFileSync(record(p), "utf8")) : null),
    write: (p, rec) => writeFileAtomic(record(p), `${JSON.stringify(rec, null, 1)}\n`),
    sha256: (p) => new Promise((res, rej) => {
      const h = createHash("sha256"); const s = createReadStream(p);
      s.on("data", (d) => h.update(d)); s.on("end", () => res(h.digest("hex"))); s.on("error", rej);
    }),
  };
}
