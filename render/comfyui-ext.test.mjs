// node --test render/comfyui-ext.test.mjs
// Runs the Python tests of the ComfyUI-side pack (render/comfyui-ext/test_offload_multicard.py) with the ComfyUI venv's interpreter.
// They need torch and safetensors and no GPU (CUDA_VISIBLE_DEVICES=-1, ComfyUI stubbed); where the interpreter or the packages are
// missing the test is skipped, not failed, because a node without ComfyUI has nothing of this pack to check.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { COMFY_PY } from "./comfy-lifecycle.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const env = { ...process.env, CUDA_VISIBLE_DEVICES: "-1", PYTHONDONTWRITEBYTECODE: "1", PYTHONUTF8: "1" };
const run = (args, timeout) => spawnSync(COMFY_PY, ["-B", ...args], { encoding: "utf8", env, timeout, windowsHide: true });

const probe = run(["-c", "import torch, safetensors"], 180000);
const skip = probe.error || probe.status !== 0 ? `no interpreter with torch and safetensors (${COMFY_PY})` : false;

test("the ComfyUI-side pack: the read-only reader and the free node pass their Python tests", { skip }, () => {
  const r = run([join(here, "comfyui-ext", "test_offload_multicard.py")], 300000);
  assert.equal(r.status, 0, `python tests failed:\n${r.stdout}\n${r.stderr}`);
  assert.match(r.stderr, /Ran \d+ tests/);
  assert.match(r.stderr, /\nOK/);
});

// The idle bound's pack (idle-nodes/offload_idle) is standard library only, so ANY Python 3 runs its tests: the ComfyUI interpreter when it
// exists, else python, else python3. Each is probed first with `-I -c`, because a Windows machine's python3 is often the Store stub that
// exits non-zero. The test is skipped only when none answers, and CI (no ComfyUI install, no torch) runs it through python3.
let idlePython = null;
for (const candidate of [existsSync(COMFY_PY) ? COMFY_PY : null, "python", "python3"]) {
  if (!candidate) continue;
  const probed = spawnSync(candidate, ["-I", "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)"], { encoding: "utf8", timeout: 60000, windowsHide: true });
  if (!probed.error && probed.status === 0) { idlePython = candidate; break; }
}

test("the idle bound's pack: its Python tests pass under any Python 3", { skip: idlePython ? false : "no Python 3 interpreter on this machine" }, () => {
  const r = spawnSync(idlePython, ["-I", "-B", join(here, "comfyui-ext", "test_offload_idle.py")], { encoding: "utf8", env, timeout: 120000, windowsHide: true });
  assert.equal(r.status, 0, `python tests failed:\n${r.stdout}\n${r.stderr}`);
  assert.match(r.stderr, /Ran \d+ tests/);
  assert.match(r.stderr, /\nOK/);
});
