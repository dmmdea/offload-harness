// Test-only preload (node --require) for render/igpu-deadline.test.mjs: it lets a test move the clock
// the REAL runners see, at an exact point of their step sequence, and record how the runners call
// ffmpeg / ffprobe. Never used outside the tests.
//
//   IGPU_TEST_DELAY_MS   busy-wait this long before the runner starts (a slow pre-spawn phase: the
//                        llama-swap drain, a cold node start), so the deadline that counts from the
//                        process start is already partly or wholly spent when main() begins
//   IGPU_TEST_JUMP      "spawn:sd-cli:1,spawnSync:libx264:1" (kind:needle:n): after the Nth call of
//                        child_process.spawn / spawnSync whose command line holds `needle` returns,
//                        Date.now() jumps an hour ahead - the budget is spent before the NEXT step,
//                        whose deadline.enforce must say so
//   IGPU_TEST_RECORD     a file: every spawnSync call is appended as {cmd, timeout} (the timeout
//                        option the runner passed, null when it passed none)
const cp = require("node:child_process");
const fs = require("node:fs");
const Module = require("node:module");

const realNow = Date.now.bind(Date);
const delay = Number(process.env.IGPU_TEST_DELAY_MS) || 0;
if (delay > 0) {
  const end = realNow() + delay;
  while (realNow() < end) { /* busy wait: the process is "starting up" */ }
}

let offset = 0;
Date.now = () => realNow() + offset;

const jumps = (process.env.IGPU_TEST_JUMP || "").split(",").filter(Boolean).map((j) => {
  const [kind, needle, n] = j.split(":");
  return { kind, needle, n: Number(n), seen: 0 };
});
const record = process.env.IGPU_TEST_RECORD;
for (const name of ["spawn", "spawnSync"]) {
  const orig = cp[name];
  cp[name] = function wrapped(...args) {
    const result = orig.apply(this, args);
    const line = JSON.stringify(args.filter((a) => typeof a === "string" || Array.isArray(a)));
    if (record && name === "spawnSync") {
      const opts = args.find((a) => a && typeof a === "object" && !Array.isArray(a));
      fs.appendFileSync(record, JSON.stringify({ cmd: String(args[0]), line, timeout: opts && opts.timeout !== undefined ? opts.timeout : null }) + "\n");
    }
    for (const j of jumps) {
      if (j.kind === name && line.includes(j.needle) && ++j.seen === j.n) offset = 3600000;
    }
    return result;
  };
}
Module.syncBuiltinESMExports();
