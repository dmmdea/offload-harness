// node --test render/comfy-lifecycle.test.mjs
// Tests the shared ComfyUI cold-start lifecycle via injected deps (no real spawn, no
// network). Verifies: already-up => returns null (don't manage it); down => spawns with
// the zero-always-warm flags incl. a per-workflow-overridable --reserve-vram (invariant
// 5); and a never-ready spawn is killed + throws.
import { test } from "node:test";
import assert from "node:assert";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough } from "node:stream";
import { EventEmitter } from "node:events";
import {
  ensureComfy, resolveComfyPy, resolveComfyDir, cudaVisibleEnv,
  comfyLogPath, rotateComfyLog, tailComfyLog, COMFY_LOG_TAIL_LINES,
} from "./comfy-lifecycle.mjs";

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

// ---- ComfyUI console capture (F-38 audit, 2026-09-24) ---------------------------
// `stdio: "ignore"` used to discard every line ComfyUI itself ever printed, so the
// only diagnostic signal for a failed render was the terse /history execution_error
// JSON — finding a real cause (once, a plain disk-space error) took a hand-built
// stdout-capturing bypass copy of render/. These tests exercise the rotating,
// size-bounded capture end to end through ensureComfy's real (non-injected) capture
// wiring, using a fake child with REAL stream objects (spawn itself stays injected —
// no real ComfyUI/network — but the streams are genuine PassThrough/EventEmitter so
// the capture code under test runs unmodified).

function fakeChildWithStreams() {
  const child = new EventEmitter();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.kill = () => {};
  return child;
}

test("comfyLogPath: beside the ComfyUI install, matching the .offload-*.json marker convention", () => {
  assert.equal(comfyLogPath("/some/comfy"), join("/some/comfy", "offload-comfyui.log"));
});

test("ensureComfy captures a spawned ComfyUI's stdout+stderr to the rotating log file", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-log-"));
  let ups = 0;
  const child = await ensureComfy({
    comfyUp: async () => (ups++ > 0), // first poll: down (spawns); then up
    spawn: () => {
      const c = fakeChildWithStreams();
      setImmediate(() => {
        c.stdout.write("hello from comfyui stdout\n");
        c.stderr.write("[Errno 28] No space left on device\n");
        c.emit("exit", 1);
      });
      return c;
    },
    envFor: () => process.env,
    comfyDir: dir,
    pollMs: 1,
  });
  assert.ok(child, "ensureComfy still returns the spawned child");
  // Give the async pipeline (PassThrough -> fs write stream) a moment to land.
  await new Promise((r) => setTimeout(r, 300));
  const logged = readFileSync(comfyLogPath(dir), "utf8");
  assert.ok(logged.includes("hello from comfyui stdout"), `log missing stdout line: ${logged}`);
  assert.ok(logged.includes("[Errno 28] No space left on device"), `log missing stderr line: ${logged}`);
});

test("captured ComfyUI output is capped so a long-lived batch session cannot grow the log unbounded", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-log-cap-"));
  let ups = 0;
  await ensureComfy({
    comfyUp: async () => (ups++ > 0),
    spawn: () => {
      const c = fakeChildWithStreams();
      setImmediate(() => {
        const big = "x".repeat(1024 * 1024); // 1MB chunks
        for (let i = 0; i < 8; i++) c.stdout.write(big); // 8MB total, over the 5MB cap
        c.emit("exit", 0);
      });
      return c;
    },
    envFor: () => process.env,
    comfyDir: dir,
    pollMs: 1,
  });
  await new Promise((r) => setTimeout(r, 400));
  const size = statSync(comfyLogPath(dir)).size;
  assert.ok(size < 6 * 1024 * 1024, `log grew past its 5MB cap: ${size} bytes`);
});

test("rotateComfyLog: ages out old numbered logs, keeping only the configured number of previous runs", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-log-rotate-"));
  const base = comfyLogPath(dir);
  writeFileSync(base, "run-current\n");
  writeFileSync(`${base}.1`, "run-1\n");
  writeFileSync(`${base}.2`, "run-2\n");
  writeFileSync(`${base}.3`, "run-3 (oldest, must be dropped)\n");
  rotateComfyLog(dir);
  assert.equal(existsSync(base), false, "the slot is free for the new run");
  assert.equal(readFileSync(`${base}.1`, "utf8"), "run-current\n");
  assert.equal(readFileSync(`${base}.2`, "utf8"), "run-1\n");
  assert.equal(readFileSync(`${base}.3`, "utf8"), "run-2\n");
  assert.equal(existsSync(`${base}.4`), false, "only the configured number of previous runs are kept");
});

test("rotateComfyLog: a totally fresh comfyDir (first run ever) is a safe no-op", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-log-rotate-fresh-"));
  assert.doesNotThrow(() => rotateComfyLog(dir));
});

test("tailComfyLog: the last N lines; empty string (never a throw) when nothing has been captured", () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-log-tail-"));
  assert.equal(tailComfyLog(dir), "", "no log yet = empty, not a throw");
  const lines = Array.from({ length: 30 }, (_, i) => `line ${i}`);
  writeFileSync(comfyLogPath(dir), lines.join("\n") + "\n");
  assert.deepEqual(tailComfyLog(dir, 5).split("\n"), lines.slice(-5));
  assert.equal(tailComfyLog(dir).split("\n").length, COMFY_LOG_TAIL_LINES, "default tail length");
});

// TestCaptureComfyOutput handles a stream-level "error" event (e.g. EPIPE when
// withGpuSlot's teardown kills the child mid-write) the same way it already
// handles the write stream's own "error" — never an unhandled crash. Node's
// EventEmitter throws synchronously for an "error" event with no listener, so
// this test IS the assertion: reaching the end without an uncaught exception
// (node:test fails the test itself on one) proves the listener is attached.
test("captureComfyOutput never crashes on a stdout/stderr stream error (EPIPE-class)", async () => {
  const dir = mkdtempSync(join(tmpdir(), "comfy-log-streamerr-"));
  let ups = 0;
  const child = await ensureComfy({
    comfyUp: async () => (ups++ > 0),
    spawn: () => {
      const c = fakeChildWithStreams();
      setImmediate(() => {
        c.stdout.emit("error", new Error("EPIPE: simulated"));
        c.stderr.emit("error", new Error("EPIPE: simulated"));
        c.stdout.write("line survives after the error event\n");
        c.emit("exit", 1);
      });
      return c;
    },
    envFor: () => process.env,
    comfyDir: dir,
    pollMs: 1,
  });
  assert.ok(child, "ensureComfy still returns the spawned child");
  await new Promise((r) => setTimeout(r, 200));
});
