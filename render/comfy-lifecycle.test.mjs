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
import { ensureComfy, resolveComfyPy, resolveComfyDir } from "./comfy-lifecycle.mjs";

test("already running => returns null (don't manage someone else's ComfyUI)", async () => {
  const child = await ensureComfy({
    comfyUp: async () => true,
    spawn: () => { throw new Error("should not spawn when already up"); },
  });
  assert.equal(child, null);
});

test("down => spawns with zero-always-warm flags + default --reserve-vram 1.0", async () => {
  let spawnedArgs = null;
  let ups = 0;
  const fake = { kill() {} };
  const child = await ensureComfy({
    comfyUp: async () => (ups++ > 0), // first poll: down; then up
    spawn: (py, args) => { spawnedArgs = args; return fake; },
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
    pollMs: 1,
  });
  assert.ok(spawnedArgs.includes("--cache-none"), "default launch still passes --cache-none");
});

test("never ready => kills the child and throws", async () => {
  let killed = 0;
  await assert.rejects(
    ensureComfy({
      comfyUp: async () => false, // always down
      spawn: () => ({ kill() { killed++; } }),
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
    () => ensureComfy({ comfyUp: async () => false, comfyDir: "", spawn: () => { throw new Error("must not spawn"); }, pollMs: 1 }),
    /COMFY_DIR/,
  );
});
