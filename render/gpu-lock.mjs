// gpu-lock.mjs — the Node side of the machine-wide, FENCED GPU lease.
//
// THIS FILE NO LONGER ACQUIRES. internal/gpulease (Go) is the ONE implementation of
// acquisition, staleness, fencing and the epoch counter; a render INHERITS a lease that
// the Go caller already holds (GPU_LEASE_DIR / GPU_LEASE_EPOCH / GPU_LEASE_CLASS).
//
// WHY THE DUPLICATE WAS DELETED RATHER THAN FIXED. Two languages independently
// implementing one concurrency rule produced a new divergence in every review round:
// different atomic tokens (both sides ended up holding the lease), different liveness
// rules (EPERM meant "alive" here and "dead" there), a non-atomic epoch write that a
// measurement showed restarting the fence at 1 in 24.5% of concurrent reads, and one
// side deleting the other's in-progress claim. Each was fixed individually and the next
// round found the next one, because the defect was never a bug — it was the duplication.
// There is now exactly one rule, in one language, and this file is a consumer of it.
//
// What remains here is everything that is genuinely Node's job:
//   - honour an inherited lease and FENCE against it before anything irreversible,
//   - elect ONE unloader per lease so a batch costs one teardown, not N,
//   - DRAIN llama-swap before unloading, because the unload route does not,
//   - the ComfyUI lifecycle and the guarded teardown.
//
// No npm dependencies.
import { writeFileSync, readFileSync, unlinkSync } from "node:fs";
import { join } from "node:path";
import { ensureComfy as defaultEnsureComfy, tailComfyLog, comfyLogPath, COMFY_LOG_TAIL_LINES, resolveInstance, DEFAULT_COMFY_PORT, COMFY_DIR } from "./comfy-lifecycle.mjs";
import { settleInstanceFamily, releaseInstanceFamily } from "./comfy-family.mjs";

// LEASE_FORMAT_SIGNATURE is what `local-offload gpu doctor` looks for in a copy of this
// file (internal/gpulease/audit.go, FormatSignature, pinned by a Go test): a reader that
// carries it understands lease record v2 and fences per epoch, so a host may write
// card-scoped leases. A copy of this file without it reads a directory holding only
// card-scoped leases as a free card. Do not edit one side without the other.
export const LEASE_FORMAT_SIGNATURE = "gpu-lease-format-2/per-epoch-fence";

function metaPath(lockPath) {
  return join(lockPath, "meta.json");
}

// readLease parses the shared lease record written by internal/gpulease. Node only ever
// READS it — the schema is owned on the Go side.
export function readLease(lockPath) {
  try {
    return JSON.parse(readFileSync(metaPath(lockPath), "utf8"));
  } catch { return null; }
}

// inheritedLease: the Go holder threads its lease down. Absent env means no lease, which
// is now an ERROR for a GPU job rather than a cue to acquire one (see withGpuSlot).
export function inheritedLease(env = process.env) {
  const dir = (env.GPU_LEASE_DIR || "").trim();
  const epoch = (env.GPU_LEASE_EPOCH || "").trim();
  if (!dir || !epoch) return null;
  return { dir, epoch: Number(epoch), class: (env.GPU_LEASE_CLASS || "").trim() || undefined };
}

// readEpochRecord parses e/<epoch>.json: the record of a CARD-SCOPED lease (record v2).
// A card-scoped lease never writes meta.json; this file and the per-card claims are its
// whole record. Null when there is no such lease.
export function readEpochRecord(lockPath, epoch) {
  try {
    return JSON.parse(readFileSync(join(lockPath, "e", `${epoch}.json`), "utf8"));
  } catch { return null; }
}

// readCardClaim parses cards/<uuid>.claim: {epoch, at_ms}, or a bare epoch number.
export function readCardClaim(lockPath, device) {
  try {
    const raw = readFileSync(join(lockPath, "cards", `${device}.claim`), "utf8").trim();
    try {
      const j = JSON.parse(raw);
      if (j && typeof j === "object") return j;
      if (typeof j === "number") return { epoch: j };
    } catch { /* fall through to null */ }
    return null;
  } catch { return null; }
}

// A device id names a file under cards/, so it must be a plain token (mirrors
// gpulease.NormalizeDevices): a record naming "../x" is not a lease we will open.
const DEVICE_ID = /^[a-z0-9][a-z0-9._-]*$/;

// checkInheritedLease is the FENCE. Before an irreversible action, confirm the epoch we
// were handed is still current. A closing laptop lid is not a crash — the process
// survives and resumes, and without this it would resume and unload models on top of
// whoever holds the card now, which is the original incident replayed by a lid.
//
// THE FENCE IS PER EPOCH. A card-scoped lease (record v2) lives in e/<epoch>.json plus
// one cards/<uuid>.claim per card, and several can be live at once, so "the epoch in
// meta.json is mine" is wrong for it: that compare would fence out every lease but one.
// Such a lease is current while its own record is active and each card it names carries
// a claim naming its epoch. A lease with no such record is a whole-node lease and keeps
// the meta.json epoch compare.
export function checkInheritedLease(lease, read = readLease, readRecord = readEpochRecord, readClaim = readCardClaim) {
  if (!lease) return true;
  const rec = readRecord(lease.dir, lease.epoch);
  if (rec) {
    if (Number(rec.epoch) !== Number(lease.epoch)) return false;
    if (rec.state !== "active") return false; // still being granted, or not a lease
    for (const d of Array.isArray(rec.devices) ? rec.devices : []) {
      if (typeof d !== "string" || !DEVICE_ID.test(d)) return false;
      const c = readClaim(lease.dir, d);
      if (!c || Number(c.epoch) !== Number(lease.epoch)) return false;
    }
    return true;
  }
  const meta = read(lease.dir);
  if (!meta) return false;
  return Number(meta.epoch) === Number(lease.epoch);
}

// claimLeaseUnload: elect the ONE job that unloads for this lease. Exclusive creation of
// a per-epoch marker makes the winner unambiguous even when several jobs start under one
// lease at once. Markers are cleaned up by the Go release path.
//
// This is what makes the hoist real. Skipping the unload under an inherited lease while
// nothing performed it left a leased render running with every model resident.
export function claimLeaseUnload(lease) {
  if (!lease || !lease.dir || !Number.isFinite(lease.epoch)) return true;
  try {
    writeFileSync(join(lease.dir, `unloaded.${lease.epoch}`), String(process.pid), { flag: "wx" });
    return true;
  } catch (e) {
    if (e.code === "EEXIST") return false; // someone already unloaded for this lease
    return true; // cannot tell => unload. Correctness over saving one teardown.
  }
}

// releaseLeaseUnloadMarker lets a standalone run clean up after itself. The Go release
// path sweeps these too; this is belt and braces for a job that owns its own marker.
export function releaseLeaseUnloadMarker(lease) {
  if (!lease) return;
  try { unlinkSync(join(lease.dir, `unloaded.${lease.epoch}`)); } catch {}
}

// MEMORY_STACK: the load-bearing mem0 models (small; on the reference box pinned to the
// utility card, not the render card). freeLlamaSwap must NEVER unload these — the
// unload-ALL route did, tearing down the memory stack on every gen job for no VRAM the
// render could use. `gpu reserve --unload-seat` keeps them too (register C-87).
//
// SOURCED FROM CONFIG/ENV, not a buried const: the Go harness threads the config's
// MemoryStack as MEMORY_STACK, so a renamed or added member is honored instead
// of silently unloaded. The literal below is the fallback for a direct CLI run. It is the
// same list as config.Default().MemoryStack (internal/config), and a Go test reads this
// line to keep the two in step: embeddinggemma-ams is the id the memory authority node
// serves its embedder under (register A-122b), embeddinggemma2 is the EmbeddingGemma-2 entry the
// memory stack is moving to (2026-10-09), and a name a box does not serve is inert.
const DEFAULT_MEMORY_STACK = ["embeddinggemma", "bge-reranker-v2-m3", "embeddinggemma-ams", "embeddinggemma2"];
export function memoryStack(env = process.env.MEMORY_STACK) {
  if (env && env.trim()) {
    return new Set(env.split(",").map((s) => s.trim()).filter(Boolean));
  }
  return new Set(DEFAULT_MEMORY_STACK);
}

// GPU_LEASE_UNLOAD_MODELS: the models the Go wrapper says may leave under a lease that holds some
// cards, not the whole node (plan P5, register C-86). `gpu reserve --devices <cards> -- <render>`
// computes it (the roster, minus the memory stack, minus the seats pinned to cards the lease does
// not hold) and exports it; freeLlamaSwap then unloads that list instead of every model off the
// memory stack, so a card-2 render no longer empties the seats on card 0.
//
//   unset / empty -> null   today's rule: every model off the memory stack
//   "-"           -> []     the wrapper says nothing may leave
//   "a,b"         -> [a,b]  exactly these
export function parseUnloadModels(raw = process.env.GPU_LEASE_UNLOAD_MODELS) {
  if (raw === undefined || raw === null) return null;
  const t = String(raw).trim();
  if (t === "") return null;
  if (t === "-") return [];
  return t.split(",").map((s) => s.trim()).filter(Boolean);
}

// withTimeout: every network call here needs its own deadline. Node's fetch has NO
// default request timeout, so a socket that accepts and then stalls would hang the
// drain — and with it the render — indefinitely, regardless of any timeoutMs we track.
function withTimeout(ms) {
  return AbortSignal.timeout(ms);
}

// quiesceLlamaSwap: wait for in-flight work on `ids` to finish before unloading them.
//
// MEASURED, not defensive: on llama-swap v242 an unload issued during a generation
// returned in 1,265ms without draining and the in-flight request died at 4,107ms with
// 502 Bad Gateway. The unload route does not honour in-flight work, so a caller that
// wants to avoid killing someone's request must drain first.
//
// Signal: llama-server's own /slots via llama-swap's /upstream/<id>/slots, where
// is_processing is true exactly while a slot is generating (verified against a
// 23s/1500-token generation).
//
// FAIL-SAFE, NOT FAIL-OPEN-SILENT. A 404 is only "idle" when the model is genuinely
// NOT LOADED — it is also what a loaded upstream without a /slots route returns (any
// non-llama.cpp backend on :11436; whisper is one). Accepting that as idle reported a
// verified drain while in-flight work was killed, which is the exact 502 this exists to
// prevent. So a 404 is cross-checked against /running, and anything we cannot observe
// is named in `unknown` rather than assumed quiet.
//
// SECOND SIGNAL (2026-10-09): a vLLM seat has no /slots route either, so it read as
// unknown, got the brief grace and was unloaded under a request in flight (a 27B vLLM
// seat on the reference 3-card box: llama-swap answered the request 500 "aborted" 4 s
// after the lease started). vLLM and llama-server both publish an exposition at
// /upstream/<id>/metrics; the gauges below are the ones the Go-side drain reads
// (internal/seatload.InflightGauges). A loaded seat whose /slots is absent is read there;
// only a seat that answers neither is unknown.
export const INFLIGHT_GAUGES = [
  "vllm:num_requests_running", "vllm:num_requests_waiting",
  "llamacpp:requests_processing", "llamacpp:requests_deferred",
];

// parseInflightMetrics sums the in-flight gauges of a Prometheus exposition. Labels
// (`{engine="0",model_name="m"}`) and float samples are accepted; every other line is
// ignored. Returns null when the text carries none of the gauges (then the exposition
// says nothing about requests, and the caller must not read it as idle).
export function parseInflightMetrics(text) {
  let total = 0, seen = false;
  for (const raw of String(text || "").split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const m = /^([A-Za-z_:][A-Za-z0-9_:]*)(\{[^}]*\})?\s+(-?[0-9.]+(?:[eE][-+]?[0-9]+)?)/.exec(line);
    if (!m || !INFLIGHT_GAUGES.includes(m[1])) continue;
    const v = Number(m[3]);
    if (!Number.isFinite(v)) continue;
    seen = true;
    total += v;
  }
  return seen ? total : null;
}

export async function quiesceLlamaSwap(ids, {
  api = process.env.LLAMA_SWAP_API || "http://localhost:11436",
  timeoutMs = 60_000, pollMs = 500, graceMs = 1_500,
  fetchImpl = fetch, nowFn = Date.now, requestTimeoutMs = 5_000,
} = {}) {
  const deadline = nowFn() + timeoutMs;
  const unknown = new Set();
  const started = nowFn();

  // Which models does the server consider loaded? Used to disambiguate a 404.
  const loaded = async () => {
    try {
      const r = await fetchImpl(`${api}/running`, { signal: withTimeout(requestTimeoutMs) });
      if (!r.ok) return null;
      const j = await r.json();
      return new Set((j.running || []).map((m) => m.model).filter(Boolean));
    } catch { return null; }
  };

  // /running GATES every probe. On llama-swap v208 (live-found on an ampere-6 node),
  // `/upstream/<id>/slots` is `Any /upstream/*` -> proxyToUpstream: requesting it
  // for a model that is NOT loaded swaps the model IN — the drain was loading ~3GB
  // into VRAM immediately before the render it existed to protect. So:
  //   - id not in /running  => nothing to drain, and NO probe is ever sent;
  //   - /running unreadable => we are blind, and hands-off beats a probe that may
  //     LOAD a model: every id is named unknown, no /upstream request fires.
  // metricsBusy: the second signal for a loaded seat without /slots. true/false when the
  // exposition carries an in-flight gauge; null when it does not answer (404/501, a
  // non-exposition body, a timeout), which the caller names unknown.
  const metricsBusy = async (id) => {
    try {
      const r = await fetchImpl(`${api}/upstream/${id}/metrics`, { signal: withTimeout(requestTimeoutMs) });
      if (!r.ok) return null;
      const n = parseInflightMetrics(await r.text());
      return n === null ? null : n > 0;
    } catch { return null; }
  };

  const busy = async (id, loadedSet) => {
    if (!loadedSet) { unknown.add(id); return false; } // blind: never probe
    if (!loadedSet.has(id)) return false;              // not loaded: nothing in flight
    try {
      const r = await fetchImpl(`${api}/upstream/${id}/slots`, { signal: withTimeout(requestTimeoutMs) });
      if (r.status === 404 || r.status === 501) {
        // Loaded but no /slots route (vLLM, whisper, any non-llama.cpp backend): read
        // the exposition instead; a seat that answers neither is unknown, never idle.
        const mb = await metricsBusy(id);
        if (mb === null) { unknown.add(id); return false; }
        return mb;
      }
      if (!r.ok) { unknown.add(id); return false; }
      const j = await r.json();
      const slots = Array.isArray(j) ? j : [j];
      return slots.some((s) => s && s.is_processing === true);
    } catch { unknown.add(id); return false; }
  };

  // An unobservable tier reads as "not busy", which would otherwise end the drain on
  // the first transient blip. Retry a BOUNDED number of extra rounds so a momentary
  // 502 (a model swapping, say) resolves, while a permanently unobservable tier — one
  // with no /slots route at all — still costs only a few polls instead of the whole
  // timeout before every render.
  const unknownRetries = 2;
  let unknownRounds = 0;
  for (;;) {
    unknown.clear(); // judge each round on its own evidence, not a stale transient
    const loadedSet = await loaded();
    const flags = await Promise.all(ids.map((id) => busy(id, loadedSet)));
    if (!flags.some(Boolean)) {
      if (unknown.size === 0) break;               // verified idle
      if (++unknownRounds > unknownRetries) break; // accept: genuinely unobservable
    } else {
      unknownRounds = 0;
    }
    if (nowFn() >= deadline) {
      return { drained: false, waitedMs: nowFn() - started, unknown: [...unknown] };
    }
    await new Promise((r) => setTimeout(r, pollMs));
  }
  if (unknown.size > 0) {
    // We could not observe some tiers. Give in-flight work a brief grace rather than
    // claiming a drain we did not verify.
    await new Promise((r) => setTimeout(r, graceMs));
  }
  return { drained: unknown.size === 0, waitedMs: nowFn() - started, unknown: [...unknown] };
}

// freeLlamaSwap: free the GPU-resident llama-swap models so their VRAM goes to a gen
// job, while leaving the memory stack warm. DRAINS FIRST (see quiesceLlamaSwap).
//
// Called ONCE PER LEASE, not per job — withGpuSlot enforces that via claimLeaseUnload.
// Errors are reported through `log` rather than swallowed: silently unloading nothing
// leaves the render to OOM against a full card.
export async function freeLlamaSwap(api = process.env.LLAMA_SWAP_API || "http://localhost:11436", opts = {}) {
  const {
    quiesce = quiesceLlamaSwap, drainTimeoutMs = 60_000, requestTimeoutMs = 10_000,
    log = (m) => console.error(m),
  } = opts;
  const keep = memoryStack();
  // The list the wrapper handed over (null = today's rule). A list narrows what may leave; the
  // memory stack still never does, whatever the list names.
  const supplied = opts.unloadModels !== undefined ? opts.unloadModels : parseUnloadModels();
  const allowed = supplied ? new Set(supplied) : null;
  let ids = [];
  try {
    const r = await fetch(api + "/v1/models", { signal: withTimeout(requestTimeoutMs) });
    if (!r.ok) { log(`freeLlamaSwap: /v1/models returned ${r.status}; NOT unloading (the render keeps a shared card)`); return; }
    const j = await r.json();
    ids = (j.data || []).map((m) => m.id).filter((id) => id && !keep.has(id) && (!allowed || allowed.has(id)));
  } catch (e) {
    log(`freeLlamaSwap: could not list models (${e && e.message}); NOT unloading (the render keeps a shared card)`);
    return;
  }
  if (ids.length === 0) return;

  // /running decides what actually holds VRAM. Unloading a model that is not
  // loaded is not merely pointless: on llama-swap v208 the per-model unload
  // route does not even exist, so each such request was a full requestTimeoutMs
  // of dead wait per configured model, on every render. And if /running cannot
  // be read, unloading blind is how a shared card gets a load-bearing tier torn
  // down — hands off, loudly, like the /v1/models failure above.
  let running;
  try {
    const r = await fetch(api + "/running", { signal: withTimeout(requestTimeoutMs) });
    if (!r.ok) { log(`freeLlamaSwap: /running returned ${r.status}; NOT unloading (cannot see what is loaded)`); return; }
    const j = await r.json();
    running = new Set((j.running || []).map((m) => m.model).filter(Boolean));
  } catch (e) {
    log(`freeLlamaSwap: could not read /running (${e && e.message}); NOT unloading (cannot see what is loaded)`);
    return;
  }
  const loaded = ids.filter((id) => running.has(id));
  if (loaded.length === 0) return; // nothing of ours is holding VRAM

  try {
    const res = await quiesce(loaded, { api, timeoutMs: drainTimeoutMs });
    if (!res.drained) {
      // Loud, not silent: proceeding here may kill someone's in-flight request. The
      // alternative — blocking the render forever behind a stuck tier — is worse, so
      // we proceed and say so.
      log(`freeLlamaSwap: proceeding without a verified drain after ${res.waitedMs}ms` +
          (res.unknown.length ? ` (could not read /slots for: ${res.unknown.join(",")})` : ""));
    }
  } catch (e) {
    log(`freeLlamaSwap: drain failed (${e && e.message}); proceeding to unload`);
  }

  // Per-model unload first (llama-swap >= v24x). Any failure falls back ONCE to
  // GET /unload — v208's ONLY unload route, and still present-and-total on v242,
  // which is why the gate below applies on every version — and that
  // fallback is gated on the memory stack not being resident: tearing down the
  // always-on tier to free VRAM the render may not be able to use would trade a render for
  // the memory stack (invariant 1).
  const failures = [];
  await Promise.all(loaded.map(async (id) => {
    try {
      const r = await fetch(api + "/api/models/unload/" + id, { method: "POST", signal: withTimeout(requestTimeoutMs) });
      if (!r.ok) failures.push(id);
    } catch (e) {
      log(`freeLlamaSwap: unload ${id} failed: ${e && e.message}`);
      failures.push(id);
    }
  }));
  if (failures.length === 0) return;

  // GET /unload is total: it ignores which model was asked for. It may run only when nothing that
  // has to stay is resident: the memory stack, and (under a card lease) every seat the wrapper
  // left off the list because it sits on cards the lease does not hold.
  const keepResident = [...running].filter((id) => keep.has(id) || (allowed && !allowed.has(id)));
  if (keepResident.length > 0) {
    log(`freeLlamaSwap: per-model unload unavailable for ${failures.join(",")} and a model that must stay (${keepResident.join(",")}: the memory stack or a seat on cards this lease does not hold) is resident — cannot unload-all; the render runs against whatever VRAM remains`);
    return;
  }
  try {
    const r = await fetch(api + "/unload", { signal: withTimeout(requestTimeoutMs) });
    if (r.ok) log(`freeLlamaSwap: per-model unload unavailable (${failures.join(",")}); GET /unload (unload-all, llama-swap v208 route) succeeded`);
    else log(`freeLlamaSwap: unload-all fallback returned ${r.status}; the render runs against whatever VRAM remains`);
  } catch (e) {
    log(`freeLlamaSwap: unload-all fallback failed: ${e && e.message}`);
  }
}

// freeComfy: tell ComfyUI to drop loaded models and free the memory behind them (zero-warm). Both
// flags, always: unload_models releases the weights, free_memory also resets the executor's caches;
// either alone left an instance holding what the other keeps (comfy-family.mjs says why that matters).
//
// It RESOLVES TO true when ComfyUI acknowledged the request and to false when it did not, after one
// retry, and says so once on `log`. It used to swallow every failure: a /free that never reached an
// instance mid-prompt looked exactly like one that did, and the instance kept its models for the next
// family to be loaded beside. An instance that is simply not there (connection refused) holds nothing,
// so that is quiet and counts as freed. Never throws: a failing free must not turn a finished job into a
// failed one.
export async function freeComfy(api = process.env.COMFY_API || "http://127.0.0.1:8188", {
  fetchImpl = fetch, log = (m) => console.error(m), attempts = 2, timeoutMs = 10_000,
} = {}) {
  let why = "";
  for (let i = 0; i < attempts; i++) {
    try {
      const r = await fetchImpl(api + "/free", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ unload_models: true, free_memory: true }),
        signal: withTimeout(timeoutMs),
      });
      if (r && r.ok) return true;
      why = `HTTP ${r && r.status}`;
    } catch (e) {
      if (e && e.cause && e.cause.code === "ECONNREFUSED") return true; // nothing is listening: nothing is held
      why = (e && e.message) || String(e);
    }
  }
  log(`COMFY-FREE-WARN: POST ${api}/free did not succeed (${why}); the instance may still hold its models`);
  return false;
}

// withGpuSlot centralizes the single-slot GPU lifecycle every gen runner shares:
//   1. REQUIRE a lease (inherited from the Go caller) unless noLock — this file no
//      longer acquires, so a GPU job without one is a wiring error, not a cue to grab
//      the card,
//   2. FENCE against it, then unload llama-swap ONCE PER LEASE and only for a `media`
//      lease — a `text` lease is a benchmark's reservation and unloading under it
//      destroys exactly the run it was taken to protect,
//   3. optionally ensureComfy(); warm:true is the BATCH-SESSION mode,
//   4. await fn(),
//   5. run ONE guarded teardown: freeComfy() + kill a ComfyUI we spawned.
// `api` is the ComfyUI endpoint the runner talks to. Together with COMFY_INSTANCE /
// COMFY_CARD_UUID it names the ComfyUI instance (comfy-lifecycle.mjs resolveInstance):
// a keyed per-card instance is launched, freed and log-tailed on ITS endpoint and files. An
// UNKEYED instance on the default endpoint is launched exactly as before (ensureComfy gets no api).
// An UNKEYED instance whose `api` names ANOTHER endpoint is handed that endpoint: an unkeyed launch
// has no --port, so it can only start 8188, and starting 8188 for a run that submits elsewhere put a
// stray instance inside a card lease (plan section 7, finding 7). ensureComfy reuses the endpoint
// if it answers and otherwise fails with COMFY-ENDPOINT-DOWN, never launching.
// Deps (freeLlamaSwap/ensureComfy/freeComfy/checkLease/claimUnload/tailLog) are
// injectable for tests only.
export async function withGpuSlot(opts, fn) {
  const {
    noLock = false,
    keepComfy = false,
    comfyManaged = true,
    warm = false,
    reserveVram,
    api,
    instanceEnv = process.env,
    freeLlamaSwap: freeLS = freeLlamaSwap,
    ensureComfy = defaultEnsureComfy,
    freeComfy: freeCfy = freeComfy,
    lease = inheritedLease(),
    claimUnload = claimLeaseUnload,
    checkLease = checkInheritedLease,
    tailLog = tailComfyLog,
    // family: the weights signature of this runner's job (comfy-family.mjs familySignature). When given,
    // the launch marker of the instance remembers it, and a kept instance whose marker names ANOTHER
    // family is freed before the first job, so an instance never holds two families' weights.
    family = "",
    comfyDir = COMFY_DIR,
    settleFamily = settleInstanceFamily,
    releaseFamily = releaseInstanceFamily,
  } = opts || {};

  // No lease, and not explicitly opted out => refuse. Acquiring here is exactly the
  // duplicate implementation that was deleted; silently rendering unarbitrated is the
  // behaviour that tore the text tier down in the first place.
  if (!noLock && !lease) {
    throw new Error(
      "GPU lease missing: this runner no longer acquires the GPU itself. " +
      "Run it under the harness (which takes the lease and threads GPU_LEASE_DIR/EPOCH/CLASS), " +
      "or for a standalone run wrap it: `local-offload gpu reserve --class media -- node <script> ...`. " +
      "Use --no-lock only when you know nothing else can touch the GPU.");
  }

  // A lane with no ComfyUI (voice) never resolves an instance.
  const instance = comfyManaged ? resolveInstance({ api, env: instanceEnv }) : null;
  const instanceKey = instance ? instance.key : "";

  let comfyChild = null;
  let cleaning = false;
  const cleanup = async () => {
    if (cleaning) return; cleaning = true;
    if (comfyManaged) {
      // ALWAYS, kept or not: a kept instance outlives this runner, and what it still holds is what the
      // next lease's family is loaded beside. Only a free that reported failure leaves the family
      // recorded (the next runner then tries again); a fake that returns nothing counts as done.
      let freed;
      try { freed = await (instanceKey || api ? freeCfy(instance.api) : freeCfy()); } catch { freed = false; }
      if (freed !== false && family) releaseFamily({ comfyDir, key: instanceKey });
    }
    if (comfyChild && !keepComfy) { try { comfyChild.kill(); } catch {} }
  };
  const onSig = async () => { await cleanup(); process.exit(130); };
  for (const sig of ["SIGINT", "SIGTERM", "SIGBREAK"]) process.on(sig, onSig);
  try {
    // THE FENCE COMES FIRST, AND IT IS UNCONDITIONAL. It used to sit inside the
    // unload-election branch, so it was skipped for every job that lost the election
    // (jobs 2..N of a batch) and for every `text` lease — those jobs went straight to
    // submitting a graph while fenced out, i.e. rendering on somebody else's card.
    // Submitting a graph is irreversible GPU work, so it needs the same guard the
    // unload does.
    if (lease && !checkLease(lease)) {
      throw new Error(
        `GPU lease epoch ${lease.epoch} is no longer current — this process was fenced out ` +
        `(the card was handed to another holder while we were suspended). Refusing to touch the GPU.`);
    }
    // Then the class gate, then the once-per-lease election.
    const mayUnload = !lease || lease.class !== "text";
    if (mayUnload && (!lease || claimUnload(lease))) {
      await freeLS();
    }
    if (comfyManaged) {
      comfyChild = await ensureComfy({
        ...(instanceKey || instance.port !== DEFAULT_COMFY_PORT ? { api: instance.api } : {}),
        ...(reserveVram != null ? { reserveVram } : {}),
        ...(warm ? { warm: true } : {}),
        // A kept instance is spawned detached and unref'd (comfy-lifecycle.mjs): it must not hold
        // this runner's event loop, and its lease's holder stops it. Absent unless asked, so the
        // call is unchanged for a runner that tears its own ComfyUI down.
        ...(keepComfy ? { keep: true } : {}),
      });
    }
    if (comfyManaged && family) {
      // The instance may be one a previous lease kept, still holding that lease's family. Free it
      // BEFORE the first job when this job is another family (awaited: the job must not be queued
      // beside the old weights), and record this one.
      try {
        await settleFamily({ comfyDir, key: instanceKey, api: instance.api, family, free: (a) => freeCfy(a) });
      } catch (e) {
        // Said, never fatal: a bookkeeping fault must not fail a render that would have worked.
        console.error(`COMFY-FAMILY-WARN: could not settle the instance's family (${e && e.message}); the job runs, and the end-of-run free still follows`);
      }
    }
    try {
      return await fn({ comfyChild, lease });
    } catch (err) {
      // F-38 audit: ComfyUI's own console output used to be discarded entirely
      // (`stdio: "ignore"`), so a render failure carried no diagnostic beyond a
      // terse execution_error JSON — finding the real cause (once, a plain disk-
      // space error) took a hand-built bypass copy of render/ that captured
      // stdout. comfyChild is only non-null when THIS run launched ComfyUI itself
      // (never on a reused foreign instance — comfy-lifecycle.mjs's reuse branch
      // returns null), so the tail is always this render's own console, never
      // another job's leftover output.
      if (comfyManaged && comfyChild) {
        const tail = instanceKey ? tailLog(undefined, undefined, instanceKey) : tailLog();
        if (tail) {
          const enriched = new Error(
            `${err.message}\n\n--- last ${COMFY_LOG_TAIL_LINES} line(s) of ComfyUI's own console (${comfyLogPath(undefined, instanceKey)}) ---\n${tail}`,
            { cause: err },
          );
          enriched.stack = err.stack;
          // The rewrap must not drop the flag comfy-render.mjs turns into exit 3 (C-83).
          if (err && err.serverUnusable) enriched.serverUnusable = true;
          throw enriched;
        }
      }
      throw err;
    }
  } finally {
    await cleanup();
    for (const sig of ["SIGINT", "SIGTERM", "SIGBREAK"]) process.removeListener(sig, onSig);
  }
}
