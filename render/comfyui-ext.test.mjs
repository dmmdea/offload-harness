// node --test render/comfyui-ext.test.mjs
// Runs the Python tests of the ComfyUI-side pack (render/comfyui-ext/test_offload_multicard.py) with the ComfyUI venv's interpreter.
// They need torch and safetensors and no GPU (CUDA_VISIBLE_DEVICES=-1, ComfyUI stubbed); where the interpreter or the packages are
// missing the test is skipped, not failed, because a node without ComfyUI has nothing of this pack to check.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
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
