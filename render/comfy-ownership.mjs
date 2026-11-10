// render/comfy-ownership.mjs — durable marker so "did the harness start this ComfyUI?"
// is decidable (spec §5). Makes the up-managed vs up-external restart branch sound.
import { writeFileSync, readFileSync, rmSync, renameSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join } from "node:path";

const MARKER = ".offload-owned.json";
const p = (dir) => join(dir, MARKER);

export function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}

// writeOwner: persist the ownership marker. The run-graph flow keys ownership on
// `manifestHash` (+ the downloaded `unverified` model list) and passes NO pid — the pid was
// the ephemeral node process, never a stable ComfyUI-ownership signal. `pid` stays optional
// for the legacy isOwnedByUs(pid-liveness) callers/tests below. `packs` is the sorted `name@commit`
// key of the node packs on disk when the marker was written (the manifest hash holds no commit).
export function writeOwner(dir, { pid, manifestHash, packs, unverified = [] }) {
  const rec = { startedAt: Date.now(), manifestHash, unverified };
  if (typeof packs === "string") rec.packs = packs;
  if (typeof pid === "number") rec.pid = pid;
  writeFileSync(p(dir), JSON.stringify(rec));
}
export function readOwner(dir) {
  try { return JSON.parse(readFileSync(p(dir), "utf8")); } catch { return null; }
}
export function isOwnedByUs(dir) {
  const m = readOwner(dir);
  return !!(m && typeof m.pid === "number" && pidAlive(m.pid));
}
export function clearOwner(dir) {
  try { rmSync(p(dir), { force: true }); } catch {}
}

// --- launch ownership (the launch-profile guard, comfy-lifecycle.mjs) ----------------
//
// A SEPARATE marker from run-graph's: that one is a provisioning cache keyed on the
// manifest hash and deliberately carries no pid, so folding the launch record into it
// would either break the cache or make ownership undecidable. This one records every
// ComfyUI the harness itself spawned: the python pid, the exact argv it was given, the
// profile it was launched for, and the node process that spawned it (ownerPid).
//
// It is a FINGERPRINT, not a claim: an instance counts as harness-launched only when
// the pid is alive AND /system_stats reports the very argv recorded here. A stale
// marker (the instance died, or a different server now holds the port) therefore never
// makes a foreign ComfyUI look like ours.
//
// PER-CARD INSTANCES (plan P13a). The default instance (key "") keeps this exact file
// and record. Every other instance keeps its own `.offload-launch-<key>.json`, so two
// ComfyUI processes on one box never share, overwrite or clear each other's fingerprint;
// its record also names the port and, when the launch ran under a GPU lease, the lease
// epoch (GPU_LEASE_EPOCH), so a later reader can tell whose lease an instance belongs to.
const LAUNCH_MARKER = ".offload-launch.json";
const lp = (dir, key = "") => join(dir, key ? `.offload-launch-${key}.json` : LAUNCH_MARKER);

export function writeLaunchOwner(dir, { pid, ownerPid = process.pid, args, profile = {}, key = "", port, leaseEpoch, idleExitSec }) {
  const rec = { startedAt: Date.now(), pid, ownerPid, args, profile };
  // idleExitSec: the instance ends itself after that long idle (the idle pack's process bound, ADR 0097), so a lease's routine
  // release leaves it running for the next job (internal/comfyinst reads it).
  if (typeof idleExitSec === "number" && idleExitSec > 0) rec.idleExitSec = idleExitSec;
  if (key) {
    rec.key = key;
    if (typeof port === "number") rec.port = port;
    if (typeof leaseEpoch === "number") rec.leaseEpoch = leaseEpoch;
  }
  writeFileSync(lp(dir, key), JSON.stringify(rec));
}
export function readLaunchOwner(dir, key = "") {
  try { return JSON.parse(readFileSync(lp(dir, key), "utf8")); } catch { return null; }
}
/**
 * restampLaunchOwner: hand a kept keyed instance to the lease that is reusing it. The marker
 * records the lease epoch whose HOLDER stops the instance on release (internal/comfyinst matches
 * the epoch exactly); an instance that outlived its lease (a crashed or fenced-out holder: it is
 * detached) and is reused by the next lease must carry THAT lease's epoch, or the new holder's
 * release stops nothing and the instance outlives every later lease. Only the epoch changes: the
 * launch time, pid, argv and profile are the instance's own and are what prove it is ours.
 *
 * It claims only an instance that already belonged to a lease. A marker with no epoch (an instance
 * kept outside any lease: whoever kept it owns it) is left alone, or the lease would stop an
 * instance it never started. Written to a temporary file and renamed, so a reader never sees half
 * a marker. Throws on an I/O failure; the caller says so and carries on.
 */
export function restampLaunchOwner(dir, key, leaseEpoch) {
  const rec = readLaunchOwner(dir, key);
  if (!key || !rec || rec.key !== key) return { changed: false, why: "no marker" };
  if (typeof rec.leaseEpoch !== "number") return { changed: false, why: "the marker names no lease" };
  if (rec.leaseEpoch === leaseEpoch) return { changed: false, why: "already this lease's" };
  const previous = rec.leaseEpoch;
  rec.leaseEpoch = leaseEpoch;
  const path = lp(dir, key), tmp = path + ".tmp";
  writeFileSync(tmp, JSON.stringify(rec));
  try { renameSync(tmp, path); } catch (e) { try { rmSync(tmp, { force: true }); } catch {} throw e; }
  return { changed: true, previous };
}
/**
 * stampLaunchFamily: record whose weights a kept instance may hold. ComfyUI keeps the models it loaded
 * in host memory between prompts (its RAM-pressure cache evicts only under pressure of its own, which
 * on a 128 GiB box is far above where this harness needs it), so an instance that served one family and
 * is then handed a job of another probably holds BOTH until something tells it to let go (reconstructed
 * from the code and the surviving logs, not observed). The incident of 2026-10-09 was one keyed instance
 * at 57 GiB private; the probable cause is a qwen-image model cached from an earlier lease next to the
 * krea2 model of the current one (comfy-family.mjs says why that cannot be confirmed and what it does
 * not explain). `lastFamily` is that something's memory: the signature
 * (comfy-family.mjs) of the job whose weights the instance may still hold; "" clears it (the instance
 * was just told to free everything, so it holds nothing).
 *
 * Only the family changes: the pid, argv, profile and lease epoch are what prove the instance is ours.
 * It stamps only an instance that has a marker (one this harness launched); a foreign ComfyUI has none,
 * and gets none. Written beside, then renamed over, so a reader never sees half a marker. Throws on an
 * I/O failure; the caller says so and carries on.
 */
export function stampLaunchFamily(dir, key, family) {
  const rec = readLaunchOwner(dir, key);
  if (!rec || (key && rec.key !== key)) return { changed: false, why: "no marker" };
  const previous = typeof rec.lastFamily === "string" ? rec.lastFamily : "";
  const next = String(family ?? "");
  if (previous === next) return { changed: false, previous };
  if (next) rec.lastFamily = next; else delete rec.lastFamily;
  const path = lp(dir, key), tmp = path + ".tmp";
  writeFileSync(tmp, JSON.stringify(rec));
  try { renameSync(tmp, path); } catch (e) { try { rmSync(tmp, { force: true }); } catch {} throw e; }
  return { changed: true, previous };
}
export function clearLaunchOwner(dir, key = "") {
  try { rmSync(lp(dir, key), { force: true }); } catch {}
}

/** sameArgv: the recorded spawn argv and the server's sys.argv, element for element. */
export function sameArgv(a, b) {
  return Array.isArray(a) && Array.isArray(b) && a.length === b.length && a.every((x, i) => String(x) === String(b[i]));
}

/**
 * harnessLaunched reports whether the ComfyUI answering with `argv` is one this harness
 * spawned: a launch marker whose pid is alive and whose recorded argv is exactly it.
 */
export function harnessLaunched(marker, argv, alive = pidAlive) {
  return !!(marker && typeof marker.pid === "number" && alive(marker.pid) && sameArgv(marker.args, argv));
}

// --- ownership proof that needs no HTTP (a wedged instance cannot answer) -------------------
//
// harnessLaunched above needs the instance to report its argv over HTTP, which a hung ComfyUI never
// does: the incident was a keyed instance that kept its port after a CUDA illegal-memory-access and
// answered nothing, so no path could prove it ours and nothing ever stopped it. The primitives below
// prove it from the operating system instead: the process's start time and command line, and who
// listens on the port. Every one of them THROWS on an introspection error; the caller treats a throw
// as "not proved" and kills nothing.
const PS_TIMEOUT_MS = 15000;
/** Pid-recycle slack, as the Go side (internal/comfyinst startSlackMs): the marker is written right after the spawn. */
export const START_SLACK_MS = 10_000;

// psArgs/psOut wrap one powershell.exe call; each call site makes the call itself, with windowsHide in its own options,
// so the no-visible-windows scan can check it.
const psArgs = (script) => ["-NoProfile", "-NonInteractive", "-Command", script];
function psOut(r) {
  if (!r || r.error || r.status !== 0) throw new Error(`powershell did not answer (${r && r.error ? r.error.message : "exit " + (r && r.status)})`);
  return String(r.stdout || "").trim();
}

/**
 * processInfo: { startMs, cmd, ppid } of a live process, or null when there is no such process. win32 asks
 * Win32_Process (CreationDate, CommandLine, ParentProcessId); linux reads /proc/<pid>/stat (starttime, ppid) and
 * /proc/<pid>/cmdline. Throws when the platform cannot say.
 */
export function processInfo(pid, { platform = process.platform, run = spawnSync } = {}) {
  if (!Number.isInteger(pid) || pid <= 0) return null;
  if (platform === "win32") {
    const out = psOut(run("powershell.exe", psArgs(`$p = Get-CimInstance Win32_Process -Filter 'ProcessId=${pid}'; if ($p) { @{ startMs = [DateTimeOffset]::new($p.CreationDate).ToUnixTimeMilliseconds(); cmd = [string]$p.CommandLine; ppid = [int]$p.ParentProcessId } | ConvertTo-Json -Compress }`), { encoding: "utf8", timeout: PS_TIMEOUT_MS, windowsHide: true }));
    if (!out) return null;
    const j = JSON.parse(out);
    return { startMs: Number(j.startMs), cmd: String(j.cmd || ""), ppid: Number(j.ppid) };
  }
  let stat;
  try { stat = readFileSync(`/proc/${pid}/stat`, "utf8"); } catch (e) { if (e.code === "ENOENT" || e.code === "ESRCH") return null; throw e; }
  // The command name sits in parentheses and may hold anything, so the fields are counted from the LAST ")".
  const f = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
  const ppid = Number(f[1]), ticks = Number(f[19]);
  if (!Number.isFinite(ppid) || !Number.isFinite(ticks)) throw new Error(`cannot parse /proc/${pid}/stat`);
  const btime = Number((readFileSync("/proc/stat", "utf8").match(/^btime\s+(\d+)/m) || [])[1]);
  if (!Number.isFinite(btime)) throw new Error("cannot read the boot time from /proc/stat");
  const hz = Number(String(run("getconf", ["CLK_TCK"], { encoding: "utf8", timeout: 5000, windowsHide: true }).stdout || "").trim()) || 100;
  const cmd = readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").filter(Boolean).join(" ");
  return { startMs: btime * 1000 + Math.round(ticks * 1000 / hz), cmd, ppid };
}

/**
 * listenerPids: the pids that listen on a TCP port: [] when nothing does, 0 for a listener the system cannot name an
 * owner for (the shape of the incident: a dead or stuck process whose socket the driver still holds). win32 asks
 * Get-NetTCPConnection (the sockets with no remote end), linux `ss -ltnp`. Throws when it cannot ask.
 */
export function listenerPids(port, { platform = process.platform, run = spawnSync } = {}) {
  if (platform === "win32") {
    // By port alone, keeping the sockets with no remote end: `-State Listen` matched nothing for a live listener on a current Windows
    // 11 build (the State column comes back empty), which would read as "no listener" and let a foreign holder look like a ghost.
    // No socket at all is an ObjectNotFound error of the cmdlet (exit 1 even when silenced); any other error stays an error.
    const out = psOut(run("powershell.exe", psArgs(`try { Get-NetTCPConnection -LocalPort ${Number(port)} -ErrorAction Stop | Where-Object { $_.RemotePort -eq 0 } | ForEach-Object { $_.OwningProcess } } catch { if ($_.CategoryInfo.Category -ne 'ObjectNotFound') { throw } }; exit 0`), { encoding: "utf8", timeout: PS_TIMEOUT_MS, windowsHide: true }));
    return out.split(/\s+/).filter(Boolean).map(Number).filter(Number.isInteger);
  }
  const r = run("ss", ["-ltnp"], { encoding: "utf8", timeout: PS_TIMEOUT_MS, windowsHide: true });
  if (!r || r.error || r.status !== 0) throw new Error(`ss did not answer (${r && r.error ? r.error.message : "exit " + (r && r.status)})`);
  const pids = [];
  for (const line of String(r.stdout || "").split("\n")) {
    const cols = line.trim().split(/\s+/);
    if (!/^LISTEN$/i.test(cols[0]) || !(cols[3] || "").endsWith(":" + port)) continue;
    const ids = [...line.matchAll(/pid=(\d+)/g)].map((m) => Number(m[1]));
    if (ids.length) pids.push(...ids); else pids.push(0);
  }
  return pids;
}

/**
 * killProcessTree: end pid and everything it started. win32 `taskkill /T /F /PID`; posix SIGKILL to the process group
 * (a detached launch leads its own), else to the pid. A process that is already gone is not an error.
 */
export function killProcessTree(pid, { platform = process.platform, run = spawnSync, kill = process.kill.bind(process) } = {}) {
  if (platform === "win32") {
    const r = run("taskkill", ["/T", "/F", "/PID", String(pid)], { encoding: "utf8", timeout: PS_TIMEOUT_MS, windowsHide: true });
    // 128: no such process.
    if (!r || r.error || (r.status !== 0 && r.status !== 128)) throw new Error(`taskkill failed (${r && r.error ? r.error.message : "exit " + (r && r.status)})`);
    return;
  }
  try { kill(-pid, "SIGKILL"); return; } catch (e) { if (e.code !== "ESRCH" && e.code !== "EPERM") throw e; }
  try { kill(pid, "SIGKILL"); } catch (e) { if (e.code !== "ESRCH") throw e; }
}

const norm = (s) => String(s).replace(/\\/g, "/").toLowerCase();

/**
 * wedgedProofGap: why a live process is NOT shown to be the instance this marker recorded ("" = shown), from the OS alone:
 * it began no later than the marker plus the slack (else the pid was recycled), and its command line is ComfyUI's main.py
 * with this instance's --port and --output-directory.
 */
export function wedgedProofGap({ marker, info, port, outputDir }) {
  if (!info) return `process ${marker.pid} does not exist`;
  if (marker.startedAt > 0 && Number.isFinite(info.startMs) && info.startMs > marker.startedAt + START_SLACK_MS) {
    return `process ${marker.pid} began after the launch marker was written, so the pid was recycled`;
  }
  const cmd = norm(info.cmd);
  if (!/(^|[\s"'/])main\.py($|[\s"'])/.test(cmd)) return `process ${marker.pid} is not running main.py`;
  if (!new RegExp(`--port[ =]["']?${Number(port)}(["']|\\s|$)`).test(cmd)) return `process ${marker.pid} does not run with --port ${port}`;
  if (!outputDir || !cmd.includes(norm(outputDir))) return `process ${marker.pid} does not run with --output-directory ${outputDir}`;
  return "";
}
