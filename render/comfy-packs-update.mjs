// render/comfy-packs-update.mjs - move the installed ComfyUI node packs to the newest head of their repository's default
// branch, behind a gate, and undo the move when the gate says no. Reached as `local-offload update --component comfy-packs`
// (docs/systems/update.md, "Node packs"); the operator runs it under the GPU lease wrapper, because the gate starts ComfyUI's
// own import pass, which initializes torch. A render request never gets here: a request clones an absent pack and uses an
// installed one as it is (manifest-satisfy.mjs).
//
// Per installed pack whose origin is a row of node-packs.json:
//   1. `git ls-remote <origin> HEAD` is the candidate. Equal to the checkout's HEAD, or listed in the pack's record as
//      rejected: nothing to do (checked_at is written).
//   2. A tree with tracked changes, or whose HEAD is not an ancestor of the candidate (local commits, a branch of its own),
//      is reported and left alone. Nothing here ever resets such a tree.
//   3. The exact candidate is fetched and FETCH_HEAD must be it, so what is gated is what is promoted.
//   4. `pip freeze --all` and `pip check` are taken as the baseline; `git merge --ff-only` moves the pack.
//   5. ONE unified resolve over every installed allowlisted pack, under the host-torch constraints (resolveDeps).
//   6. The gate: `pip check` no worse than the baseline, the host constraints unmoved, and
//      `python main.py --quick-test-for-ci` in the ComfyUI directory exiting 0 with no "(IMPORT FAILED)" line for an
//      updated pack in its "Import times for custom nodes" block.
//   7. Pass: the schema-2 record (previous = the commit the pack held) and the dependency marker are written.
//      Fail: `git reset --hard <previous>`, the pip snapshot restored with --no-deps, freeze and `pip check` required to
//      equal the baseline, the failed commit appended to rejected[], exit 1.
// A remote that cannot be reached, a gate that timed out or found the card busy, or a resolve that lost the network is
// inconclusive: whatever moved is moved back, nothing is recorded as rejected, exit 0, the next run tries again.
// `--rollback` moves each pack back to its record's previous commit the same way, gate included.
//
// Exit codes (docs/systems/update.md): 0 done, no-op, inconclusive, kept or refused; 1 a candidate was rejected (or a
// rollback failed its gate) and was undone; 2 an error, including a restore that did not return the venv to the baseline.
//
// Dependency-free, Node 18+. Every effect is a field of `deps`, so a caller can drive it against a scratch directory.
import { existsSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { join, dirname } from "node:path";
import { tmpdir } from "node:os";
import { spawn } from "node:child_process";
import { pathToFileURL } from "node:url";
import { ALLOWED_NODE_PACKS } from "./node-packs.mjs";
import { packNameFromRepo } from "./manifest.mjs";
import { comfyUp, comfyApi, resolveComfyDir, resolveComfyPy } from "./comfy-lifecycle.mjs";
import { defaultSatisfyDeps, recordPackInstall, readPackRecord, buildHostConstraints } from "./manifest-satisfy.mjs";
import { writeFileAtomic } from "./atomic-out.mjs";

const SHA = /^[0-9a-f]{40}$/;
const COMPONENT = "comfy-packs";
const GIT_ENV = { GIT_TERMINAL_PROMPT: "0" };
const NETWORK = /could not resolve host|name or service not known|temporary failure in name resolution|connection (timed out|refused|reset|aborted)|network is unreachable|no route to host|failed to connect|unable to access|\b(ssl|tls)\b|timed out|max retries exceeded|connectionerror|read timed out|remote end hung up/i;
const BUSY = /out of memory|CUDA error: (busy|out of memory)|device or resource busy/i;

// --- pure helpers (exported for the one real run that drives them) -------------------------------------------------------------

// importFailures: the lines of ComfyUI's "Import times for custom nodes" block, as {failed, name}: `name` is the last path
// segment of the module the line names (the pack directory, or a single-file node's file name).
export function importLines(text) {
  const out = [];
  for (const raw of String(text || "").split(/\r?\n/)) {
    const m = /^\s*\d+(?:\.\d+)?\s+seconds(\s+\(IMPORT FAILED\))?:\s*(.+?)\s*$/.exec(raw);
    if (!m) continue;
    const segs = m[2].split(/[\\/]+/).filter(Boolean);
    out.push({ failed: !!m[1], name: segs[segs.length - 1] || m[2] });
  }
  return out;
}

// judgeQuickTest: the verdict on one `main.py --quick-test-for-ci` run. `updated` names the pack directories that moved.
// pass: exit 0 and no IMPORT FAILED line for a moved pack. reject: such a line, or a non-zero exit. inconclusive: a
// timeout, or a failure that is the card being busy or out of memory with no moved pack named as failed (it says nothing
// about the pack). error: the process could not be started.
export function judgeQuickTest({ code, out, timedOut = false, spawnError = null }, updated) {
  if (spawnError) return { verdict: "error", reason: "quick test could not start: " + String(spawnError.message || spawnError), failed: [], listed: [] };
  const lines = importLines(out);
  const failed = updated.filter((n) => lines.some((l) => l.failed && l.name === n));
  const listed = updated.filter((n) => lines.some((l) => l.name === n));
  const others = [...new Set(lines.filter((l) => l.failed && !updated.includes(l.name)).map((l) => l.name))];
  const extra = others.length ? { otherFailures: others } : {};
  if (failed.length) return { verdict: "reject", reason: `import failed for ${failed.join(", ")}`, failed, listed, ...extra };
  if (timedOut) return { verdict: "inconclusive", reason: "the quick test did not finish in time", failed: [], listed, ...extra };
  if (code !== 0) {
    if (BUSY.test(String(out || ""))) return { verdict: "inconclusive", reason: "the card was busy or out of memory", failed: [], listed, ...extra };
    return { verdict: "reject", reason: `the quick test exited ${code} with no pack named as failed`, failed: [], listed, ...extra };
  }
  return { verdict: "pass", reason: "", failed: [], listed, ...extra };
}

const normName = (n) => n.toLowerCase().replace(/[-_.]+/g, "-");
function freezeKey(line) {
  const t = line.trim();
  if (!t || t.startsWith("#")) return null;
  const m = /^([A-Za-z0-9][A-Za-z0-9._-]*)\s*(?:===|==|~=|@|\s|$)/.exec(t);
  return m ? normName(m[1]) : t;
}
function freezeMap(text) {
  const map = new Map();
  for (const line of String(text || "").split(/\r?\n/)) {
    const k = freezeKey(line);
    if (k !== null) map.set(k, line.trim());
  }
  return map;
}

// diffFreeze: what turns the venv now (`current`) back into the snapshot: the snapshot lines to install again (a package
// whose version moved, or that went missing) and the names to uninstall (a package the snapshot does not have).
export function diffFreeze(snapshot, current) {
  const was = freezeMap(snapshot), now = freezeMap(current);
  const install = [], remove = [];
  for (const [k, line] of was) if (now.get(k) !== line) install.push(line);
  for (const [k, line] of now) if (!was.has(k) && !line.startsWith("-e") && /^[A-Za-z0-9]/.test(k)) remove.push(k);
  return { install, remove };
}
export function sameFreeze(a, b) {
  const x = freezeMap(a), y = freezeMap(b);
  if (x.size !== y.size) return false;
  for (const [k, line] of x) if (y.get(k) !== line) return false;
  return true;
}

// --- effects ------------------------------------------------------------------------------------------------------------------

// exec: run a command to its end. Never rejects: {code, out, err, timedOut, spawnError}.
export function exec(cmd, { cwd, env, timeoutMs = 0 } = {}) {
  return new Promise((resolve) => {
    let out = "", err = "", timedOut = false, done = false;
    const fin = (r) => { if (!done) { done = true; resolve({ out, err, timedOut, spawnError: null, ...r }); } };
    let c;
    try {
      c = spawn(cmd[0], cmd.slice(1), { cwd, env: { ...process.env, ...GIT_ENV, ...(env || {}) }, stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
    } catch (e) { return fin({ code: -1, spawnError: e }); }
    const timer = timeoutMs > 0 ? setTimeout(() => { timedOut = true; try { c.kill(); } catch { /* already gone */ } }, timeoutMs) : null;
    c.stdout.on("data", (d) => { out += d; });
    c.stderr.on("data", (d) => { err += d; });
    c.on("error", (e) => { if (timer) clearTimeout(timer); fin({ code: -1, spawnError: e }); });
    c.on("close", (code) => { if (timer) clearTimeout(timer); fin({ code: code === null ? -1 : code }); });
  });
}

// defaultUpdateDeps: the real effects for one ComfyUI directory.
export function defaultUpdateDeps({ comfyDir, comfyPy, api, dryRun = false, rollback = false, log = (l) => console.error(l) }) {
  return {
    comfyDir, comfyPy, api, dryRun, rollback, log,
    rows: ALLOWED_NODE_PACKS,
    exec, comfyUp,
    now: () => new Date().toISOString(),
    satisfy: defaultSatisfyDeps({ comfyDir, comfyPy, api }),
    gitTimeoutMs: 120000,
    pipTimeoutMs: 1800000,
    quickTestTimeoutMs: 1200000,
  };
}

const short = (s) => (s ? String(s).slice(0, 12) : "");
const tail = (s, n = 300) => String(s || "").trim().slice(-n);

// updatePacks: one run. Returns the report {component, action, detail, exit, dry_run, mode, packs[], notes[]}.
export async function updatePacks(deps) {
  const d = { gitTimeoutMs: 120000, pipTimeoutMs: 1800000, quickTestTimeoutMs: 1200000, rows: ALLOWED_NODE_PACKS, exec, comfyUp, now: () => new Date().toISOString(), log: () => {}, ...deps };
  const { comfyDir, comfyPy, rollback, dryRun, log } = d;
  const mode = rollback ? "rollback" : "update";
  const packs = [], notes = [];
  const finish = (action, detail, exit = 0) => ({ component: COMPONENT, mode, dry_run: !!dryRun, action, detail, exit, packs, notes });
  const git = (dir, args, t = d.gitTimeoutMs) => d.exec(["git", "-C", dir, ...args], { timeoutMs: t });
  const py = (args, t = d.pipTimeoutMs) => d.exec([comfyPy, ...args], { cwd: comfyDir || undefined, timeoutMs: t });
  const entry = (name, props) => { const e = { name, ...props }; packs.push(e); return e; };

  const nodesDir = comfyDir ? `${comfyDir}/custom_nodes` : "";
  if (!nodesDir || !existsSync(nodesDir)) {
    return finish("not-installed", comfyDir ? `no custom_nodes directory under ${comfyDir}; no node pack is installed` : "no ComfyUI directory is configured (COMFY_DIR / comfy_dir); no node pack is installed");
  }

  // --- discover: the installed packs whose origin is an allowlist row ---
  const nameOk = (row, dirName) => dirName === row.name || dirName === packNameFromRepo(row.repo);
  const dirNames = [...new Set(d.rows.flatMap((r) => [r.name, packNameFromRepo(r.repo)]))];
  const items = [];
  for (const dirName of dirNames) {
    const dir = `${nodesDir}/${dirName}`;
    if (!existsSync(dir)) continue;
    const hr = await git(dir, ["rev-parse", "HEAD"]);
    const head = hr.code === 0 ? hr.out.trim() : "";
    if (!SHA.test(head)) { entry(dirName, { action: "skipped", code: "NOT_A_CHECKOUT", detail: "not a git checkout (or its HEAD cannot be read); left alone" }); continue; }
    const or = await git(dir, ["remote", "get-url", "origin"]);
    const origin = or.code === 0 ? or.out.trim() : "";
    const row = d.rows.find((r) => r.repo === origin && nameOk(r, dirName));
    if (!row) { entry(dirName, { action: "skipped", installed: head, code: "ORIGIN_NOT_ALLOWED", detail: `origin ${origin || "(none)"} is not a row of node-packs.json; left alone` }); continue; }
    items.push({ pack: { name: dirName, repo: origin }, dir, head, origin, e: null });
  }
  if (!items.length && !packs.length) return finish("not-installed", `no allowlisted node pack is installed under ${nodesDir}`);
  if (!items.length) return finish("skipped", "no installed pack is one this verb may move");

  // ComfyUI cannot gain or lose packs under a live instance, and the gate starts ComfyUI's own import pass.
  if (!dryRun && await d.comfyUp(d.api)) {
    for (const it of items) entry(it.pack.name, { action: "refused", installed: it.head, code: "COMFY_RUNNING", detail: "a ComfyUI answers on its port; stop it and run again" });
    return finish("refused", `a ComfyUI answers on ${d.api}: the pack set cannot change under a live instance; nothing was changed`);
  }

  // --- plan ---
  const moves = [];
  const touch = (it, mut) => {
    if (dryRun) return;
    try {
      recordPackInstall(it.pack, it.head, { comfyDir, origin: it.origin });
      const rec = readPackRecord(it.pack, { comfyDir });
      if (!rec) return;
      mut(rec);
      writeFileAtomic(`${nodesDir}/${it.pack.name}.installed.json`, JSON.stringify(rec, null, 2) + "\n");
    } catch { /* the record is informational */ }
  };
  for (const it of items) {
    const name = it.pack.name;
    const dirty = await git(it.dir, ["status", "--porcelain", "--untracked-files=no"]);
    const isDirty = dirty.code !== 0 || dirty.out.trim() !== "";
    const rec = readPackRecord(it.pack, { comfyDir });

    if (rollback) {
      const prev = rec && rec.previous && rec.previous.commit;
      if (!prev || !SHA.test(prev)) { it.e = entry(name, { action: "kept", installed: it.head, code: "NO_PREVIOUS", detail: "the record names no previous commit" }); continue; }
      if (isDirty) { it.e = entry(name, { action: "kept", installed: it.head, code: "DIRTY_TREE", detail: "tracked files are modified; left alone" }); continue; }
      if (!rec || rec.commit !== it.head) { it.e = entry(name, { action: "kept", installed: it.head, code: "NOT_AT_RECORD", detail: "the checkout is not at the commit its record names (moved by hand); left alone" }); continue; }
      const have = await git(it.dir, ["cat-file", "-e", `${prev}^{commit}`]);
      const anc = have.code === 0 ? await git(it.dir, ["merge-base", "--is-ancestor", prev, it.head]) : { code: 1 };
      if (anc.code !== 0) { it.e = entry(name, { action: "kept", installed: it.head, code: "PREVIOUS_NOT_BEHIND", detail: "the previous commit is not in this checkout's history behind HEAD; left alone" }); continue; }
      it.e = entry(name, { action: dryRun ? "rollback-available" : "pending", installed: it.head, resolved: prev });
      moves.push({ it, from: it.head, to: prev, how: "reset" });
      continue;
    }

    const lr = await d.exec(["git", "ls-remote", "--", it.origin, "HEAD"], { timeoutMs: d.gitTimeoutMs });
    const cand = lr.code === 0 ? (lr.out.split(/\r?\n/).map((l) => l.trim().split(/\s+/)).find((p) => p[1] === "HEAD") || [])[0] : "";
    if (!cand || !SHA.test(cand)) {
      it.e = entry(name, { action: "inconclusive", installed: it.head, code: "REMOTE_UNREACHABLE", detail: lr.code === 0 ? "the remote named no HEAD" : `git ls-remote failed: ${tail(lr.spawnError ? lr.spawnError.message : lr.err)}` });
      continue;
    }
    const rejectedHere = rec && Array.isArray(rec.rejected) && rec.rejected.some((r) => r && r.commit === cand);
    if (cand === it.head || rejectedHere) {
      it.e = entry(name, { action: "current", installed: it.head, resolved: cand, ...(rejectedHere && cand !== it.head ? { detail: "the newest commit failed an earlier update and is not tried again" } : {}) });
      touch(it, (r) => { r.checked_at = d.now(); });
      continue;
    }
    if (isDirty) { it.e = entry(name, { action: "kept", installed: it.head, resolved: cand, code: "DIRTY_TREE", detail: "tracked files are modified; left alone" }); continue; }

    let have = await git(it.dir, ["cat-file", "-e", `${cand}^{commit}`]);
    if (have.code !== 0 && !dryRun) {
      const fr = await d.exec(["git", "-C", it.dir, "fetch", "--no-tags", "--", it.origin, cand], { timeoutMs: d.gitTimeoutMs });
      const fh = fr.code === 0 ? await git(it.dir, ["rev-parse", "FETCH_HEAD"]) : null;
      if (!fh || fh.code !== 0 || fh.out.trim() !== cand) {
        it.e = entry(name, { action: "inconclusive", installed: it.head, resolved: cand, code: "FETCH_FAILED", detail: fr.code !== 0 ? `git fetch failed: ${tail(fr.spawnError ? fr.spawnError.message : fr.err)}` : "FETCH_HEAD is not the commit the remote named" });
        continue;
      }
      have = { code: 0 };
    }
    if (have.code !== 0) {
      it.e = entry(name, { action: "update-available", installed: it.head, resolved: cand, detail: "not fetched in a dry run; whether HEAD is behind it is checked at fetch" });
      continue;
    }
    const fwd = await git(it.dir, ["merge-base", "--is-ancestor", it.head, cand]);
    if (fwd.code !== 0) {
      const back = await git(it.dir, ["merge-base", "--is-ancestor", cand, it.head]);
      it.e = entry(name, { action: "kept", installed: it.head, resolved: cand, code: "LOCAL_COMMITS", detail: back.code === 0 ? "the checkout holds local commits on top of the remote's head; left alone" : "the checkout's HEAD is not an ancestor of the remote's head (its own branch or local commits); left alone" });
      continue;
    }
    it.e = entry(name, { action: dryRun ? "update-available" : "pending", installed: it.head, resolved: cand });
    moves.push({ it, from: it.head, to: cand, how: "merge" });
  }

  if (!moves.length || dryRun) {
    const any = (a) => packs.some((p) => p.action === a);
    if (dryRun && moves.length) return finish("available", `${moves.length} pack(s) can move; run without --dry-run under the GPU lease`);
    if (any("inconclusive")) return finish("inconclusive", "a remote could not be read; nothing changed, the next run tries again");
    if (any("kept") || any("skipped")) return finish("current", "nothing to move; some packs are kept as they are (see packs)");
    return finish("current", "every installed pack is at the remote's head");
  }

  // --- apply ---
  if (!existsSync(join(comfyDir, "main.py"))) {
    for (const m of moves) m.it.e.action = "kept";
    return finish("error", `${comfyDir}/main.py is not there: the gate cannot run, nothing was changed`, 2);
  }
  const freezeCmd = ["-m", "pip", "freeze", "--all"];
  const base = await py(freezeCmd);
  if (base.code !== 0) {
    for (const m of moves) m.it.e.action = "kept";
    return finish("error", `pip freeze failed (${base.spawnError ? base.spawnError.message : tail(base.err)}): no snapshot, nothing was changed`, 2);
  }
  const baseCheck = await py(["-m", "pip", "check"]);
  const baseText = baseCheck.out.trim();

  const applied = [];
  for (const m of moves) {
    log(`comfy-packs: ${m.it.pack.name} ${short(m.from)} -> ${short(m.to)}`);
    if (m.how === "merge") await touch(m.it, () => {});   // bring the record to what is on disk before the pack moves
    else if (!dryRun) { try { recordPackInstall(m.it.pack, m.from, { comfyDir, origin: m.it.origin }); } catch { /* informational */ } }
    const r = await git(m.it.dir, m.how === "merge" ? ["merge", "--ff-only", m.to] : ["reset", "--hard", m.to]);
    const h = r.code === 0 ? await git(m.it.dir, ["rev-parse", "HEAD"]) : null;
    if (r.code === 0 && h && h.out.trim() === m.to) { applied.push(m); continue; }
    if (m.how === "merge") {
      // a fast-forward that git refused changed nothing
      m.it.e.action = "kept"; m.it.e.code = "MERGE_REFUSED"; m.it.e.detail = `git merge --ff-only refused: ${tail(r.err)}`;
      continue;
    }
    applied.push(m);   // a reset that half-ran is moved back like the rest
    return await undo(applied, { kind: "error", reason: `git reset failed on ${m.it.pack.name}: ${tail(r.err)}`, code: "RESET_FAILED" });
  }
  if (!applied.length) return finish("current", "no pack could be moved; see packs");

  // --- resolve + gate ---
  const allPacks = items.map((it) => it.pack);
  let failure = null;
  const fail = (kind, reason, code, failed = []) => { failure = failure || { kind, reason, code, failed }; };
  try { await d.satisfy.resolveDeps(allPacks); }
  catch (e) {
    const msg = String(e && e.message || e);
    if (e && (e.syscall && String(e.syscall).startsWith("spawn") || /^spawn\b/.test(msg))) fail("error", "the resolve could not start: " + msg, "SATISFIER_SPAWN_FAILED");
    else if (NETWORK.test(msg)) fail("inconclusive", "the dependency resolve lost the network: " + msg, "RESOLVE_NETWORK");
    else fail("reject", "dependency resolve or install failed: " + msg, "RESOLVE_FAILED");
  }
  if (!failure) {
    const chk = await py(["-m", "pip", "check"]);
    if (chk.spawnError) fail("error", "pip check could not start: " + chk.spawnError.message, "SATISFIER_SPAWN_FAILED");
    else if ((baseCheck.code === 0 && chk.code !== 0) || (baseCheck.code !== 0 && chk.out.trim() !== baseText)) fail("reject", "pip check reports conflicts the baseline did not: " + tail(chk.out), "PIP_CHECK");
  }
  if (!failure) {
    const after = await py(freezeCmd);
    const was = buildHostConstraints(base.out).sort().join("|"), now = buildHostConstraints(after.out).sort().join("|");
    if (after.code !== 0) fail("error", "pip freeze failed after the resolve: " + tail(after.err), "FREEZE_FAILED");
    else if (was !== now) fail("reject", `host constraint drift: a protected package moved: expected [${was}] got [${now}]`, "HOST_DRIFT");
  }
  let quick = null;
  if (!failure) {
    log("comfy-packs: gate: python main.py --quick-test-for-ci");
    const q = await d.exec([comfyPy, "main.py", "--quick-test-for-ci"], { cwd: comfyDir, timeoutMs: d.quickTestTimeoutMs, env: { PYTHONIOENCODING: "utf-8" } });
    quick = judgeQuickTest({ code: q.code, out: q.out + "\n" + q.err, timedOut: q.timedOut, spawnError: q.spawnError }, applied.map((m) => m.it.pack.name));
    if (quick.otherFailures) notes.push(`import failed for ${quick.otherFailures.join(", ")} (not moved by this run)`);
    for (const m of applied) if (!quick.listed.includes(m.it.pack.name)) notes.push(`${m.it.pack.name} is not listed in the import block`);
    if (quick.verdict !== "pass") fail(quick.verdict, quick.reason, quick.verdict === "reject" ? "IMPORT_GATE" : quick.verdict === "error" ? "GATE_SPAWN_FAILED" : "GATE_INCONCLUSIVE", quick.failed);
  }
  if (failure) return await undo(applied, failure);

  // --- pass: record ---
  for (const m of applied) {
    recordPackInstall(m.it.pack, m.to, { comfyDir, origin: m.it.origin });
    m.it.e.action = rollback ? "rollback" : "updated";
    m.it.e.installed = m.to; m.it.e.previous = m.from;
  }
  try {
    const key = items.map((it) => `${it.pack.name}@${applied.find((m) => m.it === it)?.to || it.head}`).sort().join("|");
    d.satisfy.writeDepsMarker(key);
  } catch { /* the marker is an optimization */ }
  return finish(rollback ? "rollback" : "updated", `${applied.length} pack(s) ${rollback ? "moved back" : "moved"}; the gate passed`);

  // undo: put the packs and the venv back to the baseline, then say why.
  async function undo(done, f) {
    const problems = [];
    for (const m of [...done].reverse()) {
      const r = await git(m.it.dir, ["reset", "--hard", m.from]);
      const h = r.code === 0 ? await git(m.it.dir, ["rev-parse", "HEAD"]) : null;
      if (!h || h.out.trim() !== m.from) problems.push(`${m.it.pack.name} did not return to ${short(m.from)}: ${tail(r.err)}`);
    }
    const now = await py(freezeCmd);
    if (now.code === 0) {
      const { install, remove } = diffFreeze(base.out, now.out);
      if (install.length) {
        const scratch = mkdtempSync(join(tmpdir(), "offload-packs-restore-"));
        try {
          const file = join(scratch, "restore.txt");
          writeFileSync(file, install.join("\n") + "\n");
          const r = await py(["-m", "pip", "install", "--no-deps", "-r", file]);
          if (r.code !== 0) problems.push("pip install of the snapshot failed: " + tail(r.err || r.out));
        } finally { try { rmSync(scratch, { recursive: true, force: true }); } catch { /* best effort */ } }
      }
      if (remove.length) {
        const r = await py(["-m", "pip", "uninstall", "-y", ...remove]);
        if (r.code !== 0) problems.push("pip uninstall failed: " + tail(r.err || r.out));
      }
    } else problems.push("pip freeze failed while restoring: " + tail(now.err));
    const fin = await py(freezeCmd);
    if (fin.code !== 0 || !sameFreeze(base.out, fin.out)) {
      const a = freezeMap(base.out), b = freezeMap(fin.out);
      const diff = [...new Set([...a.keys(), ...b.keys()])].filter((k) => a.get(k) !== b.get(k)).slice(0, 5);
      problems.push(`the venv is not back at its baseline (differs: ${diff.join(", ") || "freeze unreadable"})`);
    }
    const fc = await py(["-m", "pip", "check"]);
    if ((baseCheck.code === 0) !== (fc.code === 0) || (baseCheck.code !== 0 && fc.out.trim() !== baseText)) problems.push("pip check differs from the baseline after the restore");
    if (problems.length) {
      for (const m of done) { m.it.e.action = "rollback-failed"; m.it.e.detail = f.reason; }
      return finish("rollback-failed", `${f.reason}; THE RESTORE FAILED: ${problems.join("; ")}`, 2);
    }
    const blamed = new Set(f.failed || []);
    const rejectAll = f.kind === "reject" && blamed.size === 0 && !rollback;
    for (const m of done) {
      const e = m.it.e;
      e.installed = m.from; e.code = f.code;
      if (f.kind === "reject" && !rollback && (rejectAll || blamed.has(m.it.pack.name))) {
        e.action = "rejected"; e.detail = f.reason;
        touch(m.it, (r) => {
          r.rejected = (Array.isArray(r.rejected) ? r.rejected : []).filter((x) => x && x.commit !== m.to);
          r.rejected.push({ commit: m.to, reason: String(f.reason).slice(0, 300), ts: d.now() });
          r.checked_at = d.now();
        });
      } else {
        e.action = f.kind === "reject" ? "reverted" : f.kind === "inconclusive" ? "inconclusive" : "error";
        e.detail = f.kind === "reject" && !rollback ? `moved back with the rest: ${f.reason}; tried again next run` : f.reason;
      }
    }
    if (f.kind === "reject") return finish("rejected", `${f.reason}; every moved pack was put back (git and pip) and the venv equals its baseline`, 1);
    if (f.kind === "inconclusive") return finish("inconclusive", `${f.reason}; everything was put back, nothing was rejected, the next run tries again`, 0);
    return finish("error", `${f.reason}; everything was put back`, 2);
  }
}

// --- CLI ----------------------------------------------------------------------------------------------------------------------

export function parseArgs(argv) {
  const o = { dryRun: false, json: false, rollback: false };
  for (const a of argv) {
    if (a === "--dry-run") o.dryRun = true;
    else if (a === "--resolve") o.dryRun = true;   // the candidates are part of every dry report
    else if (a === "--json") o.json = true;
    else if (a === "--rollback") o.rollback = true;
    else return { error: `unknown argument ${a}` };
  }
  return o;
}

export function renderText(r) {
  const lines = [`${r.component}: ${r.action}${r.dry_run ? " (dry run)" : ""} - ${r.detail}`];
  for (const p of r.packs) {
    lines.push(`  ${p.name}: ${p.action}${p.installed ? ` ${short(p.installed)}` : ""}${p.resolved ? ` -> ${short(p.resolved)}` : ""}${p.detail ? ` - ${p.detail}` : ""}`);
  }
  for (const n of r.notes) lines.push(`  note: ${n}`);
  return lines.join("\n");
}

async function main() {
  const o = parseArgs(process.argv.slice(2));
  const emit = (r) => {
    console.log(o.json ? JSON.stringify(r) : renderText(r)); // keep: the report is this script's output
    process.exitCode = r.exit;
  };
  if (o.error) {
    console.error(`comfy-packs-update: ${o.error}`);
    process.exitCode = 2;
    return;
  }
  let deps;
  try {
    const comfyDir = resolveComfyDir();
    const comfyPy = resolveComfyPy(comfyDir);
    const api = comfyApi();
    deps = defaultUpdateDeps({ comfyDir, comfyPy, api, dryRun: o.dryRun, rollback: o.rollback });
  } catch (e) {
    emit({ component: COMPONENT, mode: o.rollback ? "rollback" : "update", dry_run: o.dryRun, action: "error", detail: String(e && e.message || e), exit: 2, packs: [], notes: [] });
    return;
  }
  try { emit(await updatePacks(deps)); }
  catch (e) {
    emit({ component: COMPONENT, mode: o.rollback ? "rollback" : "update", dry_run: o.dryRun, action: "error", detail: "unexpected: " + String(e && e.stack || e), exit: 2, packs: [], notes: [] });
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
