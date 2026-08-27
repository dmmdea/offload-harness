// node --test render/comfy-lifecycle.test.mjs
// Tests the shared ComfyUI cold-start lifecycle via injected deps (no real spawn, no
// network). Verifies: already-up => returns null (don't manage it); down => spawns with
// the zero-always-warm flags incl. a per-workflow-overridable --reserve-vram (invariant
// 5); and a never-ready spawn is killed + throws.
import { test } from "node:test";
import assert from "node:assert";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ensureComfy, resolveComfyPy, resolveComfyDir, cudaVisibleEnv } from "./comfy-lifecycle.mjs";

// A bound ComfyUI dir, injected so the lifecycle tests exercise the SPAWN path on
// every OS. Without it they inherited the platform default — "C:/ComfyUI" on Windows,
// "" on Linux — so on Linux every one tripped the unbound-dir guard before reaching
// the behaviour under test, and the whole file was silently Windows-only (caught when
// the node suite entered CI). spawn is faked, so the dir is never used beyond the guard.
const BOUND_DIR = "/fake/comfyui";

test("already running => returns null (don't manage someone else's ComfyUI)", async () => {
  const child = await ensureComfy({
    comfyUp: async () => true,
    spawn: () => { throw new Error("should not spawn when already up"); }, envFor: () => process.env, envFor: () => process.env,
  });
  assert.equal(child, null);
});

test("down => spawns with zero-always-warm flags + default --reserve-vram 1.0", async () => {
  let spawnedArgs = null;
  let ups = 0;
  const fake = { kill() {} };
  const child = await ensureComfy({
    comfyUp: async () => (ups++ > 0), // first poll: down; then up
    spawn: (py, args) => { spawnedArgs = args; return fake; }, envFor: () => process.env,
    comfyDir: BOUND_DIR,
    pollMs: 1,
  });
  assert.equal(child, fake, "returns the spawned child so the caller can kill it");
  assert.ok(spawnedArgs.includes("--disable-smart-memory"), "smart-memory off");
  assert.ok(spawnedArgs.includes("--cache-none"), "cache-none");
  const ri = spawnedArgs.indexOf("--reserve-vram");
  assert.ok(ri >= 0, "passes --reserve-vram");
  assert.equal(spawnedArgs[ri + 1], "1.0", "default reserve 1.0");
});

test("--reserve-vram is per-workflow-overridable (invariant 5)", async () => {
  let spawnedArgs = null;
  let ups = 0;
  const child = await ensureComfy({
    comfyUp: async () => (ups++ > 0),
    spawn: (py, args) => { spawnedArgs = args; return { kill() {} }; },
    reserveVram: "2.0",
    comfyDir: BOUND_DIR,
    pollMs: 1,
  });
  assert.ok(child);
  const ri = spawnedArgs.indexOf("--reserve-vram");
  assert.equal(spawnedArgs[ri + 1], "2.0", "override threaded through");
});

test("warm:true omits --cache-none but keeps the other flags (batch session)", async () => {
  let spawnedArgs = null;
  let ups = 0;
  const child = await ensureComfy({
    comfyUp: async () => (ups++ > 0),
    spawn: (py, args) => { spawnedArgs = args; return { kill() {} }; },
    warm: true,
    comfyDir: BOUND_DIR,
    pollMs: 1,
  });
  assert.ok(child);
  assert.ok(!spawnedArgs.includes("--cache-none"), "warm session must not disable the model cache");
  assert.ok(spawnedArgs.includes("--disable-smart-memory"), "smart-memory stays off");
  const ri = spawnedArgs.indexOf("--reserve-vram");
  assert.ok(ri >= 0, "still reserves VRAM for the display");
});

test("warm defaults to false (zero-always-warm unchanged)", async () => {
  let spawnedArgs = null;
  let ups = 0;
  await ensureComfy({
    comfyUp: async () => (ups++ > 0),
    spawn: (py, args) => { spawnedArgs = args; return { kill() {} }; },
    comfyDir: BOUND_DIR,
    pollMs: 1,
  });
  assert.ok(spawnedArgs.includes("--cache-none"), "default launch still passes --cache-none");
});

test("never ready => kills the child and throws", async () => {
  let killed = 0;
  await assert.rejects(
    ensureComfy({
      comfyUp: async () => false, // always down
      spawn: () => ({ kill() { killed++; } }), envFor: () => process.env,
      comfyDir: BOUND_DIR,
      pollMs: 1,
      maxPolls: 3,
    }),
    /did not become ready/
  );
  assert.equal(killed, 1, "spawned child killed when it never came up");
});

test("COMFY_EXTRA_ARGS appends verbatim launch flags (J4 seam); unset = byte-identical", async () => {
  let spawnedArgs = null;
  process.env.COMFY_EXTRA_ARGS = "--directml --some-flag 1";
  try {
    await ensureComfy({
      comfyUp: async () => spawnedArgs !== null, // down first, up after spawn
      spawn: (_py, args) => { spawnedArgs = args; return { kill() {} }; },
      comfyDir: BOUND_DIR,
      envFor: () => process.env,
      pollMs: 1,
    });
  } finally {
    delete process.env.COMFY_EXTRA_ARGS;
  }
  assert.deepEqual(spawnedArgs.slice(-3), ["--directml", "--some-flag", "1"]);
  // and without the env, the tail stays the standard flag set
  let plainArgs = null;
  await ensureComfy({
    comfyUp: async () => plainArgs !== null,
    spawn: (_py, args) => { plainArgs = args; return { kill() {} }; },
    comfyDir: BOUND_DIR,
    envFor: () => process.env,
    pollMs: 1,
  });
  assert.equal(plainArgs.includes("--directml"), false);
});

// --- cross-platform engine resolution -------------------------------------------------
// A Linux node could not launch ComfyUI at all: the venv auto-detect probed ONLY Windows
// paths (.venv/Scripts/python.exe, venv/Scripts/python.exe, python_embeded/python.exe)
// and otherwise fell back to a bare "python", which on Ubuntu is either absent or the
// system interpreter without torch. render/tts.mjs already probed both families — this
// is that pattern, applied where it was missed.
test("resolveComfyPy: finds a POSIX venv python", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-py-"));
  mkdirSync(join(dir, ".venv/bin"), { recursive: true });
  writeFileSync(join(dir, ".venv/bin/python"), "");
  assert.equal(resolveComfyPy(dir, {}), join(dir, ".venv/bin/python"));
});

test("resolveComfyPy: an explicit COMFY_PY always wins", () => {
  assert.equal(resolveComfyPy("/anything", { COMFY_PY: "/opt/py" }), "/opt/py");
});

test("resolveComfyPy: Windows candidates keep priority (Windows resolution unchanged)", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-py-"));
  mkdirSync(join(dir, ".venv/Scripts"), { recursive: true });
  mkdirSync(join(dir, ".venv/bin"), { recursive: true });
  writeFileSync(join(dir, ".venv/Scripts/python.exe"), "");
  writeFileSync(join(dir, ".venv/bin/python"), "");
  assert.equal(resolveComfyPy(dir, {}), join(dir, ".venv/Scripts/python.exe"));
});

test("resolveComfyPy: no venv falls back to the platform's interpreter name", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-py-"));
  assert.equal(resolveComfyPy(dir, {}), process.platform === "win32" ? "python" : "python3");
});

test("resolveComfyPy: an empty comfyDir never fabricates a relative candidate", () => {
  assert.equal(resolveComfyPy("", {}), process.platform === "win32" ? "python" : "python3");
});

// COMFY_DIR defaulted to "C:/ComfyUI" on EVERY platform, so a Linux node reported a
// ComfyUI install it cannot have — and the fleet advertised the routes that drive it.
test("resolveComfyDir: the env value always wins", () => {
  assert.equal(resolveComfyDir({ COMFY_DIR: "/srv/comfyui" }), "/srv/comfyui");
});

test("resolveComfyDir: the Windows default is Windows-only", () => {
  const got = resolveComfyDir({});
  if (process.platform === "win32") {
    assert.equal(got, "C:/ComfyUI");
  } else {
    assert.equal(got, "", "an unset COMFY_DIR off Windows must be UNBOUND, not a C:/ path");
  }
});

test("ensureComfy: an unbound COMFY_DIR fails with a reason, not a bad cwd", async () => {
  await assert.rejects(
    () => ensureComfy({ comfyUp: async () => false, comfyDir: "", spawn: () => { throw new Error("must not spawn"); }, envFor: () => process.env, envFor: () => process.env, pollMs: 1 }),
    /COMFY_DIR/,
  );
});

// ComfyUI >=0.34 hides all but the first CUDA device on Windows unless told
// otherwise (upstream #15737/#15813); cudaVisibleEnv restores full visibility for
// the spawned child, and ONLY when the operator has not already scoped devices.
test("cudaVisibleEnv: multi-GPU Windows box gets every device listed", () => {
  const got = cudaVisibleEnv({}, () => 2);
  if (process.platform === "win32") {
    assert.equal(got.CUDA_VISIBLE_DEVICES, "0,1");
  } else {
    assert.equal(got.CUDA_VISIBLE_DEVICES, undefined);
  }
});

test("cudaVisibleEnv: an operator-set CUDA_VISIBLE_DEVICES always wins", () => {
  const env = { CUDA_VISIBLE_DEVICES: "1" };
  assert.equal(cudaVisibleEnv(env, () => 2), env);
});

test("cudaVisibleEnv: a --cuda-device in COMFY_EXTRA_ARGS wins (per-box escape hatch)", () => {
  const env = { COMFY_EXTRA_ARGS: "--cuda-device 0" };
  assert.equal(cudaVisibleEnv(env, () => 2), env);
});

test("cudaVisibleEnv: single-GPU and no-nvidia-smi boxes are left on the upstream default", () => {
  assert.equal(cudaVisibleEnv({}, () => 1).CUDA_VISIBLE_DEVICES, undefined);
  assert.equal(cudaVisibleEnv({}, () => 0).CUDA_VISIBLE_DEVICES, undefined);
});

test("multi-GPU spawn env carries --disable-pinned-memory (upstream #15737 guidance)", async () => {
  let got = null;
  const fakeSpawn = (cmd, args, opts) => { got = { args, env: opts.env }; return { kill() {} }; };
  const fakeEnv = { ...process.env, CUDA_VISIBLE_DEVICES: "0,1" };
  await ensureComfy({
    comfyDir: "C:/x", py: "py", comfyUp: async () => got !== null,
    spawn: fakeSpawn, envFor: () => fakeEnv, pollMs: 1, maxPolls: 3,
  }).catch(() => {});
  assert.ok(got, "spawn was not called");
  assert.ok(got.args.includes("--disable-pinned-memory"), "flag missing: " + got.args.join(" "));
  assert.equal(got.env.CUDA_VISIBLE_DEVICES, "0,1");
});
