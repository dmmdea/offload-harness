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
import { ensureComfy as defaultEnsureComfy } from "./comfy-lifecycle.mjs";

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

// checkInheritedLease is the FENCE. Before an irreversible action, confirm the epoch we
// were handed is still current. A closing laptop lid is not a crash — the process
// survives and resumes, and without this it would resume and unload models on top of
// whoever holds the card now, which is the original incident replayed by a lid.
export function checkInheritedLease(lease, read = readLease) {
  if (!lease) return true;
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

// MEMORY_STACK: the always-loaded, CPU-only mem0 models (they hold ZERO GPU VRAM).
// freeLlamaSwap must NEVER unload these — the unload-ALL route did, needlessly tearing
// down the load-bearing memory stack on every gen job for no VRAM benefit.
//
// SOURCED FROM CONFIG/ENV, not a buried const: the Go harness threads the config's
// MemoryStack as MEMORY_STACK, so a renamed/added 3rd CPU member is honored instead
// of silently unloaded. The literal below is the fallback for a direct CLI run.
const DEFAULT_MEMORY_STACK = ["embeddinggemma", "bge-reranker-v2-m3"];
export function memoryStack(env = process.env.MEMORY_STACK) {
  if (env && env.trim()) {
    return new Set(env.split(",").map((s) => s.trim()).filter(Boolean));
  }
  return new Set(DEFAULT_MEMORY_STACK);
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

  // /running GATES every probe. On llama-swap v208 (live-found on <node-c>),
  // `/upstream/<id>/slots` is `Any /upstream/*` -> proxyToUpstream: requesting it
  // for a model that is NOT loaded swaps the model IN — the drain was loading ~3GB
  // into VRAM immediately before the render it existed to protect. So:
  //   - id not in /running  => nothing to drain, and NO probe is ever sent;
  //   - /running unreadable => we are blind, and hands-off beats a probe that may
  //     LOAD a model: every id is named unknown, no /upstream request fires.
  const busy = async (id, loadedSet) => {
    if (!loadedSet) { unknown.add(id); return false; } // blind: never probe
    if (!loadedSet.has(id)) return false;              // not loaded: nothing in flight
    try {
      const r = await fetchImpl(`${api}/upstream/${id}/slots`, { signal: withTimeout(requestTimeoutMs) });
      if (r.status === 404) {
        // Loaded but no /slots route (whisper, any non-llama.cpp backend): we are
        // blind to it, and must say so instead of claiming a drain.
        unknown.add(id);
        return false;
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
// job, while leaving the CPU memory stack warm. DRAINS FIRST (see quiesceLlamaSwap).
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
  let ids = [];
  try {
    const r = await fetch(api + "/v1/models", { signal: withTimeout(requestTimeoutMs) });
    if (!r.ok) { log(`freeLlamaSwap: /v1/models returned ${r.status}; NOT unloading (the render keeps a shared card)`); return; }
    const j = await r.json();
    ids = (j.data || []).map((m) => m.id).filter((id) => id && !keep.has(id));
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
  // v208's only unload route — GET /unload, which unloads EVERYTHING — and that
  // fallback is gated on the memory stack not being resident: tearing down the
  // always-on CPU tier to free VRAM it does not hold would trade a render for
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

  const keepResident = [...running].filter((id) => keep.has(id));
  if (keepResident.length > 0) {
    log(`freeLlamaSwap: per-model unload unavailable for ${failures.join(",")} and the memory stack (${keepResident.join(",")}) is resident — cannot unload-all; the render runs against whatever VRAM remains`);
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

// freeComfy: tell ComfyUI to drop loaded models + free VRAM after a job (zero-warm).
export async function freeComfy(api = process.env.COMFY_API || "http://127.0.0.1:8188") {
  try {
    await fetch(api + "/free", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ unload_models: true, free_memory: true }),
      signal: withTimeout(10_000),
    });
  } catch {}
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
// Deps (freeLlamaSwap/ensureComfy/freeComfy/checkLease/claimUnload) are injectable for
// tests only.
export async function withGpuSlot(opts, fn) {
  const {
    noLock = false,
    keepComfy = false,
    comfyManaged = true,
    warm = false,
    reserveVram,
    freeLlamaSwap: freeLS = freeLlamaSwap,
    ensureComfy = defaultEnsureComfy,
    freeComfy: freeCfy = freeComfy,
    lease = inheritedLease(),
    claimUnload = claimLeaseUnload,
    checkLease = checkInheritedLease,
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

  let comfyChild = null;
  let cleaning = false;
  const cleanup = async () => {
    if (cleaning) return; cleaning = true;
    if (comfyManaged) { try { await freeCfy(); } catch {} }
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
        ...(reserveVram != null ? { reserveVram } : {}),
        ...(warm ? { warm: true } : {}),
      });
    }
    return await fn({ comfyChild, lease });
  } finally {
    await cleanup();
    for (const sig of ["SIGINT", "SIGTERM", "SIGBREAK"]) process.removeListener(sig, onSig);
  }
}
