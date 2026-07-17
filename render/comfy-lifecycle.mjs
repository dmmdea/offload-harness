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
import { spawn as nodeSpawn } from "node:child_process";

export const COMFY_DIR = process.env.COMFY_DIR || "C:/ComfyUI";

// COMFY_PY: ComfyUI deps live in its venv, not the system python. Auto-detect; override
// via COMFY_PY. (Identical resolution to the runners' prior inline logic.)
export const COMFY_PY = process.env.COMFY_PY
  || [".venv/Scripts/python.exe", "venv/Scripts/python.exe", "python_embeded/python.exe"]
       .map((p) => join(COMFY_DIR, p)).find((p) => existsSync(p))
  || "python";

// comfyUp: is a ComfyUI HTTP server already answering on api?
export async function comfyUp(api = process.env.COMFY_API || "http://127.0.0.1:8188") {
  try { const r = await fetch(api + "/system_stats"); return r.ok; } catch { return false; }
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
    pollMs = 2000,
    // Startup budget: a laptop cold start (custom nodes + models on a slow disk)
    // legitimately exceeds the old hardcoded ~4 min. Default 10 min, env-tunable
    // (COMFY_START_WAIT_SEC) — same pattern as the render polls' COMFY_WAIT_SEC.
    maxPolls = Math.max(1, Math.ceil(Number(process.env.COMFY_START_WAIT_SEC || 600) * 1000 / 2000)),
  } = opts;
  if (await up(api)) return null; // already running — don't manage it
  const reserve = String(reserveVram || "1.0");
  // warm: a BATCH session keeps ComfyUI's model cache ON so the checkpoint loads once
  // for N renders; the caller still tears the whole session down at the batch boundary
  // (zero-always-warm moves from per-render to per-batch). Default stays cache-none.
  const flags = ["--disable-smart-memory"];
  if (!warm) flags.push("--cache-none");
  flags.push("--reserve-vram", reserve);
  const child = spawn(py, ["main.py", ...flags], { cwd: comfyDir, stdio: "ignore", detached: false });
  for (let i = 0; i < maxPolls; i++) {
    await new Promise((r) => setTimeout(r, pollMs));
    if (await up(api)) return child;
  }
  try { child.kill(); } catch {}
  throw new Error("ComfyUI did not become ready on " + api + " after ~" + Math.round(maxPolls * pollMs / 60000) + "min (COMFY_START_WAIT_SEC to extend)");
}
