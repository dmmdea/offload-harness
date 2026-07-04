// node --test render/comfy-lifecycle.test.mjs
// Tests the shared ComfyUI cold-start lifecycle via injected deps (no real spawn, no
// network). Verifies: already-up => returns null (don't manage it); down => spawns with
// the zero-always-warm flags incl. a per-workflow-overridable --reserve-vram (invariant
// 5); and a never-ready spawn is killed + throws.
import { test } from "node:test";
import assert from "node:assert";
import { ensureComfy } from "./comfy-lifecycle.mjs";

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
