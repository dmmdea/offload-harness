// comfy-submit.mjs — the ONE ComfyUI submission/polling/output-retrieval layer for the
// render runners (master plan step 4: re-express submission plumbing through the vendored
// comfyui-pp-cli). Before this module, six runners each carried their own copy of the raw
// POST /prompt + poll /history + GET /view logic, and only comfy-render.mjs had the
// dead-server watchdog fix (2026-07-30) — the siblings still burned their whole poll
// budget on a wedged server while holding the exclusive GPU slot.
//
// Division of labor (each side does what only it can do well):
//
//   comfyui-pp-cli (tools/comfyui), WHEN a binary is resolvable:
//     - submission: `submit - --json --skip-lint` — idempotent lease on the graph's
//       content hash (never double-renders an identical in-flight graph), typed
//       accept/reject/partial classification, node_errors verbatim, client-minted
//       prompt_id recorded BEFORE the POST, durable run row in its local store.
//     - timing + finalization: after the render reaches a terminal state,
//       `wait <id> --timeout 5s --json` records the authoritative duration
//       (/history execution_start -> execution_success — never the server log's
//       "Prompt executed" line, which is stale mid-run) and finalizes the run row
//       so the lease is released and `jobs list`/`timing` accumulate real data.
//
//   this module, raw and always available (also the ONLY path when no CLI binary):
//     - history polling with the dead-server watchdog + suspend/resume fence — a
//       capability the CLI's `wait` deliberately lacks (it warns and retries to its
//       timeout; the harness must abort in ~COMFY_DEAD_SEC seconds to RELEASE the
//       GPU slot). Surfaced as a CLI gap in the 2026-08-14 nightshift-4 ledger.
//     - output retrieval: GET /view -> Buffer -> caller-named file. The CLI's `view`
//       routes bytes through its structured-output pipeline; byte-exact binary
//       delivery through it is unproven, so files are fetched raw.
//
// CLI resolution order: $COMFYUI_PP_CLI (explicit — a wrong value fails LOUD, it never
// silently degrades) > <repo>/tools/comfyui/bin/comfyui-pp-cli[.exe] (a local
// `make build`) > `comfyui-pp-cli` on PATH (probed by spawning; ENOENT falls back).
// No binary anywhere -> raw submission with one stderr notice. CI and binary-less fleet
// nodes therefore keep byte-identical behavior; a box that builds the CLI gets the
// lease + provenance + timing for free.
//
// Dependency-free, Node 18+.
import { existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";

const __dirname = dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------------------------------
// CLI resolution
// ---------------------------------------------------------------------------------------

// resolveCli: pick the comfyui-pp-cli binary this process will use, or null for raw mode.
// Kinds: "env" and "repo-bin" are POSITIVE resolutions (the file exists; a later spawn
// failure is an error, not a fallback); "path" is a GUESS probed at first use (ENOENT
// falls back to raw). Deliberately cheap and synchronous — no version spawn per render.
export function resolveCli({ env = process.env, scriptDir = __dirname } = {}) {
  const explicit = env.COMFYUI_PP_CLI;
  if (explicit) {
    if (!existsSync(explicit)) {
      // Explicit config must never silently degrade: the operator named a binary.
      throw new Error(
        "COMFYUI_PP_CLI is set to " + explicit + " but no such file exists — fix the path or unset it to use raw HTTP submission",
      );
    }
    return { cmd: explicit, source: "env" };
  }
  const ext = process.platform === "win32" ? ".exe" : "";
  const repoBin = join(scriptDir, "..", "tools", "comfyui", "bin", "comfyui-pp-cli" + ext);
  if (existsSync(repoBin)) return { cmd: repoBin, source: "repo-bin" };
  return { cmd: "comfyui-pp-cli" + ext, source: "path" };
}

// runCli: spawn the CLI once, feed stdin, collect both streams. Resolves (never rejects)
// with { code, stdout, stderr, spawnError } so callers can classify ENOENT separately
// from a real non-zero exit. killAfterMs guards the warn-only calls (finalize) against a
// wedged child; submission passes 0 = no artificial bound, mirroring the raw POST which
// has no timeout either.
export function runCli(cmd, args, { input = "", env = {}, killAfterMs = 0, spawnImpl = spawn } = {}) {
  return new Promise((resolve) => {
    const child = spawnImpl(cmd, args, {
      env: { ...process.env, ...env },
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    });
    let stdout = "", stderr = "", done = false;
    let timer = null;
    const finish = (res) => {
      if (done) return;
      done = true;
      if (timer) clearTimeout(timer);
      resolve(res);
    };
    if (killAfterMs > 0) {
      timer = setTimeout(() => {
        try { child.kill(); } catch { /* already gone */ }
        // Tear the pipes down too: a child that ignores the signal must not keep a
        // finished runner's event loop alive on its dangling stdio.
        for (const s of [child.stdout, child.stderr, child.stdin]) { try { s.destroy(); } catch { /* best effort */ } }
        finish({ code: null, stdout, stderr, spawnError: new Error("comfyui-pp-cli did not exit within " + killAfterMs + "ms") });
      }, killAfterMs);
      if (typeof timer.unref === "function") timer.unref();
    }
    child.on("error", (e) => finish({ code: null, stdout, stderr, spawnError: e }));
    // utf8 decoding at the STREAM level: `+= chunk` on raw Buffers re-decodes each chunk
    // independently, so a multi-byte sequence split across a chunk boundary becomes
    // U+FFFD inside node_errors text. (Guarded: test doubles are bare emitters.)
    if (typeof child.stdout.setEncoding === "function") child.stdout.setEncoding("utf8");
    if (typeof child.stderr.setEncoding === "function") child.stderr.setEncoding("utf8");
    child.stdout.on("data", (d) => { stdout += d; });
    child.stderr.on("data", (d) => { stderr += d; });
    child.on("close", (code) => finish({ code, stdout, stderr, spawnError: null }));
    // EPIPE/EOF fence: a child that exits before draining stdin (usage error, startup
    // panic, missing DLL) makes the pending write emit 'error' on the stdin socket —
    // UNHANDLED, that is an uncaughtException that kills the whole runner OUTSIDE every
    // catch/finally (no typed DEFER, no GPU teardown). The child's own exit code +
    // stderr carry the real story, so the write failure itself is deliberately dropped.
    child.stdin.on("error", () => { /* see fence comment above */ });
    if (input) child.stdin.write(input);
    child.stdin.end();
  });
}

// parseCliJson: the CLI prints its --json envelope directly; some framework paths wrap
// payloads under {results: ...}. Accept both; throw (with a stdout sample) on neither —
// a submission whose handle cannot be read must fail loudly, never continue blind.
export function parseCliJson(stdout) {
  let obj;
  try { obj = JSON.parse(stdout); } catch {
    throw new Error("comfyui-pp-cli printed unparseable JSON: " + String(stdout).slice(0, 300));
  }
  if (obj && typeof obj === "object" && !Array.isArray(obj)) {
    if (obj.results && typeof obj.results === "object" && !Array.isArray(obj.results)) return obj.results;
    return obj;
  }
  throw new Error("comfyui-pp-cli JSON had an unexpected shape: " + String(stdout).slice(0, 300));
}

const tail = (s, n) => { const t = String(s || "").trim(); return t.length > n ? t.slice(-n) : t; };

// ---------------------------------------------------------------------------------------
// Submission
// ---------------------------------------------------------------------------------------

// jfetch: fetch that throws on !ok with the body excerpt AND carries e.httpStatus, so the
// poll loop can tell "server answered with an error" (alive) from "no answer" (dead time).
async function jfetch(fetchImpl, url, opts) {
  const r = await fetchImpl(url, opts);
  if (!r.ok) {
    // The status is already in hand — a body read that itself fails (abort mid-read,
    // connection reset on an error page) must not demote an ANSWERED error status to
    // watchdog dead time by throwing without httpStatus.
    let body = "";
    try { body = (await r.text()).slice(0, 300); } catch { /* status alone suffices */ }
    const e = new Error(url + " -> " + r.status + " " + body);
    e.httpStatus = r.status;
    throw e;
  }
  return r.json();
}

// submitGraph: queue a graph, preferring the CLI, and return { promptId, via }.
//
// CLI path notes (each is a deliberate exact-behavior decision, ledgered 2026-08-14):
//   --skip-lint   the server stays the sole validator, exactly as the raw POST — the
//                 CLI's absolute-host-path lint could refuse a --graph file that a
//                 local ComfyUI would happily load.
//   exit 22       PARTIAL ACCEPT (some output branches dropped; only possible on
//                 multi-output --graph graphs). The raw path would render whatever
//                 survived, so: warn loudly, keep the prompt_id, keep going.
//   attached      the lease found an identical graph in flight. Attach ONLY when the
//                 CLI positively confirms it live (running/pending — the exact
//                 resubmit-after-kill burn the lease exists to stop); anything else
//                 (stale row, restarted server, probe failure) resubmits with --force,
//                 which IS today's always-POST behavior.
//   client_id     the CLI sends its fixed "comfyui-pp-cli" (no --client-id flag yet —
//                 surfaced as an upstream gap); the raw path keeps the per-run id.
export async function submitGraph({ api, graph, clientId, cli, fetchImpl = fetch, spawnImpl = spawn, stderr = process.stderr }) {
  if (cli) {
    const env = { COMFYUI_BASE_URL: api };
    const input = JSON.stringify(graph);
    const submitOnce = (force) => runCli(
      cli.cmd,
      force ? ["submit", "-", "--json", "--skip-lint", "--force"] : ["submit", "-", "--json", "--skip-lint"],
      { input, env, spawnImpl },
    );

    let r = await submitOnce(false);
    if (r.spawnError) {
      if (cli.source === "path" && r.spawnError.code === "ENOENT") {
        // Only genuinely-absent counts as the raw tier. EACCES/EMFILE/etc. mean a
        // binary EXISTS but is broken or the box is exhausted — falling back there
        // would both lie about the cause and silently swap submission semantics.
        stderr.write("comfy-submit: comfyui-pp-cli not found on PATH (raw HTTP submission; set COMFYUI_PP_CLI or `make -C tools/comfyui build` to enable the CLI path)\n");
        return rawSubmit({ api, graph, clientId, fetchImpl });
      }
      // env/repo-bin resolved to a real file, or a PATH binary failed for a reason
      // other than absence: that is an error, not a tier.
      throw new Error("comfyui-pp-cli (" + cli.source + ": " + cli.cmd + ") failed to start: " + r.spawnError.message);
    }

    const accept = (res, note) => {
      const out = parseCliJson(res.stdout);
      if (!out.prompt_id) throw new Error("comfyui-pp-cli submit returned no prompt_id: " + tail(res.stdout, 300));
      if (note) stderr.write(note + "\n");
      return { promptId: out.prompt_id, via: "cli", submit: out };
    };

    // Exit-code classification (code-review 2026-08-14): a LOCAL pre-POST failure —
    // 2 usage (flag-skewed binary), 10 config (unwritable state dir / bad SQLite),
    // 1 generic local (e.g. unusable --home; empirically 0 POSTs issued) — must not
    // kill a render the raw POST would have completed: the CLI's own store/config is
    // bookkeeping, not the render path. Fall back to raw LOUDLY, naming the real cause.
    // Server-verdict / possibly-POSTed codes (21 rejected, 23 malformed, 4/5/26
    // transport-api — the CLI's own "the POST may have landed" branch exits 5) stay
    // fatal: retrying those raw could double-render a 20-minute graph.
    const localOnlyFailure = (code) => code === 1 || code === 2 || code === 10;
    // postedAlready: the code list alone is not proof the POST never happened — a
    // post-POST local step (a --deliver sink failure, a profile teardown) can replace
    // the error and exit 1 AFTER the server queued the render. The envelope is the
    // evidence: a landed POST carries prompt_id (and a real http_status); a genuine
    // pre-POST failure prints http_status 0 and no prompt_id, or no envelope at all.
    const postedAlready = (res) => {
      try { const o = parseCliJson(res.stdout); return !!(o.prompt_id || o.http_status); }
      catch { return false; }
    };
    const submitFailure = (res, label) => {
      if (localOnlyFailure(res.code)) {
        if (postedAlready(res)) {
          throw new Error("comfyui-pp-cli " + label + " exited " + res.code + " AFTER the POST landed — not retrying raw, that would double-render; the envelope carries the handle: " + tail(res.stdout || res.stderr, 400));
        }
        stderr.write("comfy-submit WARN: comfyui-pp-cli (" + cli.source + ": " + cli.cmd + ") " + label + " failed locally before POSTing (exit " + res.code + "): "
          + tail(res.stderr || res.stdout, 300) + " — falling back to raw HTTP submission\n");
        return null; // caller falls back to rawSubmit
      }
      throw new Error("comfyui-pp-cli " + label + " exited " + res.code + ": " + tail(res.stderr || res.stdout, 500));
    };

    if (r.code === 22) {
      return accept(r, "comfy-submit WARN: partial accept — ComfyUI dropped some output branches (see node_errors): " + tail(r.stderr, 600));
    }
    if (r.code !== 0) {
      submitFailure(r, "submit"); // throws on server-verdict codes
      return rawSubmit({ api, graph, clientId, fetchImpl });
    }
    const out = parseCliJson(r.stdout);
    if (!out.attached) return accept(r);

    // Lease hit: identical graph recorded as in flight. Trust it only when live.
    const probe = await runCli(cli.cmd, ["attach", out.prompt_id, "--json"], { env, spawnImpl, killAfterMs: 30_000 });
    let live = "unknown";
    let liveInFlight = false;
    let probeNote = "";
    if (probe.spawnError) {
      probeNote = " (liveness probe failed to run: " + probe.spawnError.message + ")";
    } else if (probe.code !== 0) {
      probeNote = " (liveness probe exited " + probe.code + ": " + tail(probe.stderr || probe.stdout, 200) + ")";
    } else {
      try {
        const p = parseCliJson(probe.stdout);
        live = p.live_state || "unknown";
        liveInFlight = p.in_flight === true;
      } catch { probeNote = " (liveness probe output unparseable)"; }
    }
    // Attach when the server POSITIVELY confirms the identical run alive: running or
    // queued, or present in /history but not yet terminal (live_state "history" with
    // in_flight still true — representable if a future ComfyUI writes history entries
    // mid-run). live_state "unknown" deliberately does NOT attach even when the CLI's
    // in_flight flag is true: that combination means "could not verify, local record
    // only", and attaching to an unverified prompt is the budget-burning hang the
    // stale branch exists to avoid.
    if (live === "running" || live === "pending" || (live === "history" && liveInFlight)) {
      stderr.write("comfy-submit: identical graph already in flight — attached to " + out.prompt_id + " instead of double-rendering (lease)\n");
      return { promptId: out.prompt_id, via: "cli", submit: out };
    }
    // Stale or unverifiable lease -> a fresh render is what the raw path always did.
    // probeNote distinguishes "the CLI answered not-live" from "the probe itself broke",
    // so a systematically broken `attach` cannot hide as routine staleness forever.
    stderr.write("comfy-submit: lease for an identical graph is stale (live_state=" + live + probeNote + "); resubmitting with --force\n");
    r = await submitOnce(true);
    if (r.spawnError) throw new Error("comfyui-pp-cli resubmit failed to start: " + r.spawnError.message);
    if (r.code === 22) {
      return accept(r, "comfy-submit WARN: partial accept — ComfyUI dropped some output branches (see node_errors): " + tail(r.stderr, 600));
    }
    if (r.code !== 0) {
      submitFailure(r, "submit --force"); // throws on server-verdict codes
      return rawSubmit({ api, graph, clientId, fetchImpl });
    }
    return accept(r);
  }
  return rawSubmit({ api, graph, clientId, fetchImpl });
}

// rawSubmit: the exact POST the six runners used to carry (body byte-shape preserved).
async function rawSubmit({ api, graph, clientId, fetchImpl = fetch }) {
  const resp = await jfetch(fetchImpl, api + "/prompt", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ prompt: graph, client_id: clientId }),
  });
  // A 200 with no prompt_id (proxy interference, API drift) must fail HERE: polling
  // /history/undefined answers 200 {} forever, which would hold the exclusive GPU slot
  // for the whole budget before dying with a misleading "no output produced in time".
  if (!resp || !resp.prompt_id) {
    throw new Error("ComfyUI /prompt returned no prompt_id: " + JSON.stringify(resp).slice(0, 300));
  }
  return { promptId: resp.prompt_id, via: "raw", submit: null };
}

// ---------------------------------------------------------------------------------------
// Polling (KEEP-raw by design — see header)
// ---------------------------------------------------------------------------------------

// pollOutputs: poll /history/<id> every 2s until isDone(entry) or the budget runs out.
// This is comfy-render.mjs's hardened loop (2026-07-30 dead-server watchdog + 2026-08
// suspend/resume fence + per-poll 30s abort), now shared by every runner:
//   - an HTTP error status IS an answer (server alive) — only network/abort failures
//     accrue dead time; COMFY_DEAD_SEC (default 240s, min 10) of consecutive dead time
//     aborts EARLY so the exclusive GPU slot is released in seconds, not budget-minutes;
//   - a timer jump >120s means the MACHINE slept (lids close mid-render on this fleet by
//     design) — that time never counts as dead;
//   - a slow-but-honest render keeps answering /history with no outputs yet, which
//     resets the counter: the long waitSec budget still governs that case, and the Go
//     side's process-tree kill remains the hard stop.
// onExecError (optional) runs best-effort before an exec-error throw — the CLI-path
// hook that finalizes the run row with the server's verbatim failure.
export async function pollOutputs({
  api, promptId, waitSec = 1800, isDone,
  noOutputMsg = "no output produced in time",
  onExecError = null,
  fetchImpl = fetch,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  now = Date.now,
  env = process.env,
}) {
  const deadRaw = Number(env.COMFY_DEAD_SEC);
  const deadSec = Number.isFinite(deadRaw) ? Math.max(10, deadRaw) : 240;
  let lastAnswerAt = now();
  let prevTickAt = now();
  for (let i = 0; i < Math.max(1, Math.ceil(waitSec / 2)); i++) {
    await sleep(2000);
    if (now() - prevTickAt > 120_000) lastAnswerAt = now(); // suspend/resume fence
    prevTickAt = now();
    let hist;
    try {
      hist = await jfetch(fetchImpl, `${api}/history/${promptId}`, { signal: AbortSignal.timeout(30_000) });
    } catch (e) {
      if (e && e.httpStatus) { lastAnswerAt = now(); continue; } // an error status is an answer
      const deadFor = Math.floor((now() - lastAnswerAt) / 1000);
      if (deadFor >= deadSec) {
        throw new Error(`ComfyUI stopped answering mid-render (unreachable ${deadFor}s, COMFY_DEAD_SEC=${deadSec}); aborting early to release the GPU slot`);
      }
      continue;
    }
    lastAnswerAt = now();
    const h = hist[promptId];
    if (!h) continue;
    if (h.status && h.status.status_str === "error") {
      if (onExecError) { try { await onExecError(); } catch { /* best-effort only */ } }
      throw new Error("ComfyUI exec error: " + JSON.stringify(h.status).slice(0, 500));
    }
    if (isDone(h)) return h;
  }
  throw new Error(noOutputMsg);
}

// ---------------------------------------------------------------------------------------
// Output retrieval (KEEP-raw by design — see header)
// ---------------------------------------------------------------------------------------

// fetchView: GET /view for one output descriptor and return the raw bytes.
export async function fetchView({ api, file, fetchImpl = fetch }) {
  const q = new URLSearchParams({ filename: file.filename, subfolder: file.subfolder || "", type: file.type || "output" });
  const r = await fetchImpl(`${api}/view?` + q.toString());
  if (!r.ok) throw new Error("view fetch " + r.status);
  return Buffer.from(await r.arrayBuffer());
}

// ---------------------------------------------------------------------------------------
// Finalization + authoritative timing (CLI path only; warn-only, never fatal)
// ---------------------------------------------------------------------------------------

// finalizeRun: after the render reached a terminal state server-side, let the CLI record
// it — `wait <id> --timeout 5s` returns immediately on an already-terminal entry, writes
// the authoritative execution_start -> execution_success timing into its store, and
// finalizes the run row (releasing the submission lease). Prints one timing line on
// stdout when a duration exists. EVERY failure here degrades to a stderr warning: the
// render outcome is already decided and must never be re-opened by bookkeeping.
export async function finalizeRun({ api, promptId, cli, spawnImpl = spawn, stdout = process.stdout, stderr = process.stderr }) {
  if (!cli) return null;
  const r = await runCli(cli.cmd, ["wait", promptId, "--timeout", "5s", "--json"], {
    env: { COMFYUI_BASE_URL: api },
    spawnImpl,
    killAfterMs: 30_000,
  });
  if (r.spawnError) {
    // Silent ONLY for the PATH-guess-absent case (submit already printed the one raw-
    // fallback notice). Everything else — kill-timeout, EACCES, a broken env binary —
    // must warn: a wedged `wait` that vanished silently would also never release the
    // lease, surfacing later as chronic misleading "stale lease" churn.
    const quiet = cli.source === "path" && r.spawnError.code === "ENOENT";
    if (!quiet) stderr.write("comfy-submit WARN: timing capture skipped (" + r.spawnError.message + ")\n");
    return null;
  }
  // `wait`'s terminal-OUTCOME codes (13 validation/model, 21 failed, 22 outputs-pending,
  // 24 interrupted, 25 OOM) mean the run reached a terminal state and WAS recorded —
  // finalization succeeded; the code describes the RENDER, whose fate the caller already
  // knows. Warning on them would make every failed render cry wolf about bookkeeping.
  // Genuine finalization failures are the rest: 2 usage, 3 not-found (restarted server
  // dropped its RAM history), 10 config/store, 20 wait-timeout, or a spawn-level death.
  const finalized = r.code === 0 || [13, 21, 22, 24, 25].includes(r.code);
  if (!finalized) {
    stderr.write("comfy-submit WARN: comfyui-pp-cli wait exited " + r.code + " during finalization: " + tail(r.stderr || r.stdout, 300) + "\n");
    return null;
  }
  try {
    const out = parseCliJson(r.stdout);
    const durationMs = out?.status?.duration_ms || 0;
    if (durationMs > 0) {
      stdout.write("timing " + (durationMs / 1000).toFixed(1) + "s (history execution_start -> execution_success, via comfyui-pp-cli)\n");
    }
    return { durationMs };
  } catch (e) {
    stderr.write("comfy-submit WARN: could not parse finalization output: " + e.message + "\n");
    return null;
  }
}
