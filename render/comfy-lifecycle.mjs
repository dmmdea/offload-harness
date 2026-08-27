// comfy-lifecycle.mjs — the shared, on-demand ComfyUI cold-start lifecycle. This was
// byte-identical across comfy-generate.mjs and comfy-video.mjs; centralized here so the
// cold-start + ~4-min ready-poll + zero-always-warm launch flags
// (--disable-smart-memory --cache-none --reserve-vram) live in ONE place. A `warm`
// batch session omits --cache-none so a checkpoint loads once for N renders (the
// caller still tears the session down at the batch boundary). tts.mjs does
// NOT use this (its Chatterbox worker is not ComfyUI; it passes comfyManaged:false to
// withGpuSlot). Dependency-free; deps are injectable purely for tests.
import { existsSync } from "node:fs";
import { join } from "node:path";
import { spawn as nodeSpawn, spawnSync as nodeSpawnSync } from "node:child_process";

// resolveComfyDir: the ComfyUI install this machine drives. The old default was
// "C:/ComfyUI" on EVERY platform, so a Linux node reported an install it cannot have and
// the fleet advertised the routes that drive it. Off Windows an unset COMFY_DIR is
// UNBOUND ("") — the honest answer, which the harness reports as NOT CONFIGURED instead
// of a path that will never exist.
export function resolveComfyDir(env = process.env) {
  return env.COMFY_DIR || (process.platform === "win32" ? "C:/ComfyUI" : "");
}

// resolveComfyPy: ComfyUI's deps live in its venv, not the system python. Probes BOTH
// platform families — the Windows candidates first, so Windows resolution is unchanged.
// Only Windows paths were probed before, so a Linux node fell through to a bare "python"
// that is either absent on Ubuntu or the system interpreter without torch: ComfyUI could
// not be launched by the harness at all. tts.mjs already probed both families; this is
// that pattern applied where it was missed.
export function resolveComfyPy(comfyDir = COMFY_DIR, env = process.env) {
  if (env.COMFY_PY) return env.COMFY_PY;
  const bare = process.platform === "win32" ? "python" : "python3";
  if (!comfyDir) return bare;
  return [".venv/Scripts/python.exe", "venv/Scripts/python.exe", "python_embeded/python.exe",
          ".venv/bin/python", "venv/bin/python"]
           .map((p) => join(comfyDir, p)).find((p) => existsSync(p))
    || bare;
}

export const COMFY_DIR = resolveComfyDir();
export const COMFY_PY = resolveComfyPy(COMFY_DIR);

// cudaVisibleEnv: ComfyUI >= 0.34 defaults WINDOWS to CUDA_VISIBLE_DEVICES=0 when the
// operator passed no device selection (upstream #15737 "Limit Windows multi-GPU
// visibility" + #15813) — silently hiding every card but the first. Our pooled
// multi-GPU tiers (DisTorch2 image/video seats) need them all: a graph naming
// cuda:1 as donor then fails prompt validation with "donor_device: 'cuda:1' not in
// ['cpu','cuda:0']" (measured on the blackwell-2x16 box, 2026-08-27). Restore full
// visibility for the CHILD process only, and only when the operator has not already
// scoped devices via CUDA_VISIBLE_DEVICES or a --cuda-device in COMFY_EXTRA_ARGS.
// Env-based (not --cuda-device all) so older ComfyUI versions, which take only an
// integer there, keep working. listGpus is injectable for tests.
export function cudaVisibleEnv(env = process.env, listGpus = defaultListGpus) {
  if (process.platform !== "win32") return env;
  if (env.CUDA_VISIBLE_DEVICES !== undefined) return env;
  if ((env.COMFY_EXTRA_ARGS || "").includes("--cuda-device")) return env;
  const n = listGpus();
  if (!(n > 1)) return env; // 0/1 GPU or no nvidia-smi: upstream default is fine
  const all = Array.from({ length: n }, (_, i) => i).join(",");
  return { ...env, CUDA_VISIBLE_DEVICES: all };
}

function defaultListGpus() {
  try {
    const r = nodeSpawnSync("nvidia-smi", ["-L"], { encoding: "utf8", timeout: 10000 });
    if (r.status !== 0 || !r.stdout) return 0;
    return (r.stdout.match(/GPU [0-9]+/g) || []).length;
  } catch {
    return 0;
  }
}

// comfyUp: is a ComfyUI HTTP server already answering on api?
export async function comfyUp(api = process.env.COMFY_API || "http://127.0.0.1:8188") {
  try { const r = await fetch(api + "/system_stats", { signal: AbortSignal.timeout(8000) }); return r.ok; } catch { return false; }
}

// ensureComfy: if ComfyUI is already up, return null (don't manage someone else's).
// Otherwise launch it on-demand with the zero-always-warm flags and poll until ready
// (~4 min: 120 polls × 2s), returning the spawned child so the caller can kill it.
// --reserve-vram holds VRAM back for the Windows display/WDDM; 1.0 leaves the most for
// the GGUF model on 8GB — it is PER-WORKFLOW-OVERRIDABLE (invariant 5: raise to 1.5-2.0
// for Wan; ACE-Step differs). Deps (comfyUp/spawn/timing) are injectable for tests only;
// production calls use the real defaults.
export async function ensureComfy(opts = {}) {
  const {
    api = process.env.COMFY_API || "http://127.0.0.1:8188",
    comfyDir = COMFY_DIR,
    py = COMFY_PY,
    reserveVram = "1.0",
    warm = false,
    comfyUp: up = comfyUp,
    spawn = nodeSpawn,
    envFor = cudaVisibleEnv,
    pollMs = 2000,
    // Startup budget: a laptop cold start (custom nodes + models on a slow disk)
    // legitimately exceeds the old hardcoded ~4 min. Default 10 min, env-tunable
    // (COMFY_START_WAIT_SEC) — same pattern as the render polls' COMFY_WAIT_SEC.
    maxPolls = Math.max(1, Math.ceil(Number(process.env.COMFY_START_WAIT_SEC || 600) * 1000 / 2000)),
  } = opts;
  if (await up(api)) return null; // already running — don't manage it
  // An unbound COMFY_DIR must fail with its reason, not with a bad cwd from spawn(): on a
  // machine with no ComfyUI binding the caller's defer should say WHY.
  if (!comfyDir) {
    throw new Error("COMFY_DIR is not set — this machine has no ComfyUI install bound (set comfy_dir in the harness config)");
  }
  const reserve = String(reserveVram || "1.0");
  // warm: a BATCH session keeps ComfyUI's model cache ON so the checkpoint loads once
  // for N renders; the caller still tears the whole session down at the batch boundary
  // (zero-always-warm moves from per-render to per-batch). Default stays cache-none.
  const flags = ["--disable-smart-memory"];
  if (!warm) flags.push("--cache-none");
  flags.push("--reserve-vram", reserve);
  // J4 seam: COMFY_EXTRA_ARGS appends verbatim (whitespace-split) launch flags —
  // the per-box escape hatch for non-CUDA backends (--directml, device pinning)
  // without touching shared code. Empty/unset = byte-identical launch.
  if (process.env.COMFY_EXTRA_ARGS) {
    flags.push(...process.env.COMFY_EXTRA_ARGS.split(/\s+/).filter(Boolean));
  }
  const spawnEnv = envFor();
  // Upstream's own guidance for the multi-GPU Windows path it now hides by default:
  // "pass --cuda-device all --disable-pinned-memory" — pinned memory with multiple
  // visible devices risks CUDA host-transfer failures on Windows (#15737). We restore
  // visibility via env, so we also carry the second half of that guidance.
  if (spawnEnv !== process.env && String(spawnEnv.CUDA_VISIBLE_DEVICES || "").includes(",")
      && !flags.includes("--disable-pinned-memory")) {
    flags.push("--disable-pinned-memory");
  }
  const child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: "ignore", detached: false, env: spawnEnv });
  for (let i = 0; i < maxPolls; i++) {
    await new Promise((r) => setTimeout(r, pollMs));
    if (await up(api)) return child;
  }
  try { child.kill(); } catch {}
  throw new Error("ComfyUI did not become ready on " + api + " after ~" + Math.round(maxPolls * pollMs / 60000) + "min (COMFY_START_WAIT_SEC to extend)");
}
