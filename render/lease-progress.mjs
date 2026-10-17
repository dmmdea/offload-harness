// lease-progress.mjs — a running render helper's heartbeat to the progress file of the GPU lease it runs
// under (F8, 2026-10-09).
//
// WHY. A lease is judged by its holder's own word: `gpu reserve` renews a lease at the end of its term only
// while something vouches for the job, and for an owner it cannot tell apart (a recipe script, a session the
// registry never held) the only voucher is a progress file that keeps moving (internal/gpulease/term.go,
// ADR 0070). The render helpers never wrote one. A 1,394 s Qwen-Image still under `gpu reserve --for ...`
// therefore had its lease labelled "expired ... open to a takeover" while it was rendering normally, and a
// waiter reading `gpu status` was told a healthy job was abandoned.
//
// WHERE THE FILE COMES FROM. `gpu reserve -- node render/comfy-*.mjs ...` declares the contract on the lease
// (a file under <state root>/progress/ and a stall window, or the one the caller passed with
// --progress-file) and hands the path to its child in GPU_LEASE_PROGRESS_FILE. A helper that was not started
// by the wrapper form but runs under a lease whose record declares a progress file (a detached hold taken
// with --progress-file) finds it in the record through GPU_LEASE_DIR / GPU_LEASE_EPOCH. With neither there
// is nothing to write and every call here is a no-op.
//
// WHAT A BEAT MEANS. The file's modification time is what the lease reads, so a line is appended only when
// the helper has evidence the RENDER is alive, never merely that this process is: comfy-submit.mjs
// (pollOutputs) beats on a poll that ComfyUI answered AND for a prompt that ComfyUI itself still lists
// (queued, running, or in its history). A helper that hangs stops polling; a ComfyUI that stops answering
// is an unanswered poll; a prompt the server lost (restart) is listed nowhere; a poisoned CUDA context is
// the poll loop's own stats probe, which ends the wait. Each of those stops the beats, the file stops
// moving, and the lease reads held-stalled after its stall window, exactly as a hung job should.
// WHAT IT DOES NOT CATCH: a prompt that sits in ComfyUI's running queue with a wedged sampler while HTTP
// keeps answering and /system_stats stays healthy. ComfyUI offers no polled step counter (its progress
// events are websocket-only and this tree is dependency-free), so that case is bounded by the poll
// budget (COMFY_WAIT_SEC) and the lease's maximum total, not by a stall verdict. (The one wedge that is known to
// leave a ComfyUI in that state, a prompt worker killed by an out-of-memory error, is caught on the card-only Wan
// run by a watch on the instance's console: pollOutputs' `watch`, wan-multicard.mjs createOomWatch.)
//
// The beat is appended, never written over: the file may be a log the caller keeps for its own purposes
// (--progress-file /abs/work/log.jsonl), and the lease reads only its modification time and the last
// line's "detail". It never throws: a heartbeat that cannot be written costs the lease its renewal
// evidence, it must never cost the render.
//
// Dependency-free, Node 18+. Standalone on purpose (it does not import gpu-lock.mjs, which imports half the
// render layer, and comfy-submit.mjs imports this; its one import is lease-read.mjs, a leaf with no imports of its
// own): the lease-record reader below is the same two paths gpu-lock.mjs readEpochRecord / readLease read, pinned by
// lease-progress.test.mjs.
import { appendFileSync } from "node:fs";
import { isAbsolute, join } from "node:path";
import { readSharedJson } from "./lease-read.mjs";

/** The env var `gpu reserve` sets on the command it wraps: the absolute path of the lease's progress file. */
export const PROGRESS_FILE_ENV = "GPU_LEASE_PROGRESS_FILE";

/**
 * How often a helper beats at most. CHOSEN, not measured: far inside the shortest stall window anything
 * declares (the wrapper's own is 15 minutes), and a beat is one small append, so a 7 hour render writes
 * about 840 lines.
 */
export const BEAT_EVERY_MS = 30_000;

// The lease record the Go side wrote for this epoch: a card-scoped lease keeps e/<epoch>.json, a whole-node
// lease meta.json (internal/gpulease; render/gpu-lock.mjs readEpochRecord / readLease read the same two). It reads
// through lease-read.mjs, as they do: Go replaces both by rename-over, and a refused open of that instant is waited out there.
function readJson(path) {
  return readSharedJson(path);
}

function recordProgressFile(env, readFile) {
  const dir = String(env.GPU_LEASE_DIR || "").trim();
  const epoch = Number(String(env.GPU_LEASE_EPOCH || "").trim());
  if (!dir || !Number.isFinite(epoch) || epoch <= 0) return "";
  const rec = readFile(join(dir, "e", `${epoch}.json`)) || readFile(join(dir, "meta.json"));
  if (!rec || Number(rec.epoch) !== epoch) return "";
  const file = rec.progress && typeof rec.progress.file === "string" ? rec.progress.file.trim() : "";
  return file;
}

/**
 * The progress file of the lease this process runs under, or "" when it has none. The wrapper's env var
 * wins; the inherited lease's own record is the fallback. A relative path is refused: the lease records an
 * absolute one, and a relative path here would be relative to this helper's directory, not the reader's.
 */
export function leaseProgressFile(env = process.env, readFile = readJson) {
  const fromEnv = String(env[PROGRESS_FILE_ENV] || "").trim();
  const file = fromEnv || recordProgressFile(env, readFile);
  return file && isAbsolute(file) ? file : "";
}

/**
 * createLeaseHeartbeat: the heartbeat of one helper process.
 *   due()                 true when a beat would be written now (a file exists and the throttle has passed)
 *   beat(detail, opts)    append one line; false when there is no file, the throttle has not passed
 *                         (unless opts.force), or the append failed. Never throws.
 * `detail` is the sentence a status call prints after "progress:" ("ComfyUI is running prompt 1a2b3c4d").
 */
export function createLeaseHeartbeat({
  env = process.env,
  now = Date.now,
  every = BEAT_EVERY_MS,
  append = appendFileSync,
  readFile = readJson,
} = {}) {
  const file = leaseProgressFile(env, readFile);
  let last = -Infinity;
  const due = () => !!file && now() - last >= every;
  const beat = (detail = "", { force = false } = {}) => {
    if (!file) return false;
    const t = now();
    if (!force && t - last < every) return false;
    // Claimed before the write so a failing disk is retried at the throttle's pace, not on every poll.
    last = t;
    try {
      const line = { at: t, pid: process.pid };
      if (detail) line.detail = String(detail).replace(/[\r\n]+/g, " ").slice(0, 200);
      append(file, JSON.stringify(line) + "\n");
      return true;
    } catch {
      return false;
    }
  };
  return { file, due, beat };
}

let processBeat = null;

/**
 * A process-wide heartbeat for the callers that are not a poll loop (a batch parent between items, a
 * runner at a stage boundary). Created on first use from the process environment.
 */
export function leaseBeat(detail, opts) {
  processBeat ??= createLeaseHeartbeat();
  return processBeat.beat(detail, opts);
}

/** Test seam: forget the process-wide heartbeat so the next leaseBeat re-reads the environment. */
export function resetLeaseBeatForTest() { processBeat = null; }
