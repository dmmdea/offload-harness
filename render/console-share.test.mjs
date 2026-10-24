// node --test render/console-share.test.mjs
//
// Why the attached runs in render/ keep `windowsHide: true` AND the operator's Ctrl-C.
//
// libuv puts CREATE_NO_WINDOW on a child only when none of its stdio is inherited from the parent. A child
// with `stdio: "inherit"` (or any inherited entry) therefore stays on the parent's console and gets the
// operator's Ctrl-C and a closed terminal like the parent does, while a child with piped or ignored stdio
// moves to a console of its own (hidden, and out of Ctrl-C's reach). The sd-cli, tts and comfy-render runs
// that render/ attaches to its own stdio rely on the first half: nothing reaps them if the parent dies on a
// signal, because they never leave the parent's console.
//
// Section 1 measures that with real processes (Windows only): a helper with a hidden console of its own
// (CREATE_NO_WINDOW, so nothing here can open a window) starts a PowerShell child and reads, from the child,
// who else is on its console. Section 2 pins that the three attached runs inherit stdio.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { scratchDir } from "./test-scratch.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));

// the helper: started with a hidden console, it starts the probe three ways and prints what each saw
const HELPER = `
const { spawn } = require("node:child_process");
const ps = 'Add-Type -MemberDefinition \\'[DllImport("kernel32.dll")] public static extern uint GetConsoleProcessList(uint[] l, uint c);\\' -Name K -Namespace W; $a = New-Object uint32[] 64; $n = [W.K]::GetConsoleProcessList($a, 64); Write-Output ("PIDS " + ($a[0..($n-1)] -join ",") + " SELF " + $PID)';
const run = (stdio) => new Promise((resolve) => {
  const c = spawn("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", ps], { stdio, windowsHide: true });
  let out = "";
  if (c.stdout) c.stdout.on("data", (d) => { out += d; });
  c.on("close", () => resolve(out.trim()));
});
(async () => {
  // an inherited stdout cannot be read back: the probe writes it straight to this process's stdout
  const piped = await run(["ignore", "pipe", "pipe"]);
  const inherited = await new Promise((resolve) => {
    const c = spawn("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", ps + " | Out-File -Encoding ascii $env:PROBE_OUT"], { stdio: "inherit", windowsHide: true, env: { ...process.env, PROBE_OUT: process.env.PROBE_OUT } });
    c.on("close", () => resolve(require("node:fs").readFileSync(process.env.PROBE_OUT, "utf8").trim()));
  });
  process.stdout.write(JSON.stringify({ helper: process.pid, piped, inherited }));
})();
`;

const parseProbe = (line) => {
  const m = /PIDS ([0-9,]*) SELF (\d+)/.exec(line || "");
  assert.ok(m, `no probe line in ${JSON.stringify(line)}`);
  return { pids: m[1].split(",").filter(Boolean), self: m[2] };
};

test("a child with inherited stdio stays on its parent's console, a piped one leaves it (windowsHide: true on both)", { skip: process.platform !== "win32" }, async (t) => {
  const dir = scratchDir(t, "console-share-");
  const probeOut = join(dir, "probe.txt");
  const result = await new Promise((resolve, reject) => {
    // piped stdio + windowsHide: the helper gets a console of its own that has no window
    const h = spawn(process.execPath, ["-e", HELPER], { stdio: ["ignore", "pipe", "pipe"], windowsHide: true, env: { ...process.env, PROBE_OUT: probeOut } });
    let out = "", err = "";
    h.stdout.on("data", (d) => { out += d; });
    h.stderr.on("data", (d) => { err += d; });
    h.on("error", reject);
    h.on("close", (code) => (code === 0 ? resolve(JSON.parse(out)) : reject(new Error(`helper exited ${code}: ${err}`))));
  });
  const piped = parseProbe(result.piped);
  const inherited = parseProbe(result.inherited);
  assert.deepEqual(piped.pids, [piped.self], "a child with piped stdio and windowsHide: true is alone on a console of its own");
  assert.ok(inherited.pids.includes(String(result.helper)) && inherited.pids.includes(inherited.self),
    `a child with inherited stdio and windowsHide: true shares its parent's console, so the operator's Ctrl-C reaches both (console processes ${inherited.pids}, parent ${result.helper}, child ${inherited.self})`);
});

// ---- 2. the attached runs inherit stdio ------------------------------------------------------------------

// each of these starts a long-running child that is attached to this process's own stdio; with an inherited
// stdio entry it never leaves the console, so it needs no reaping on SIGINT
const ATTACHED = [
  ["tts.mjs", /spawn\(TTS_PY, args, \{[^}]*stdio: "inherit"[^}]*windowsHide: true/],
  ["sdcpp-generate.mjs", /spawn\(bin, args, \{[^}]*stdio: \["inherit", "pipe", "pipe"\][^}]*windowsHide: true/],
  ["comfy-generate.mjs", /spawn\("node", args, \{[^}]*stdio: \["inherit", "inherit", "pipe"\][^}]*windowsHide: true/],
];

for (const [file, re] of ATTACHED) {
  test(`${file}: the attached run inherits stdio (it stays on this process's console, so Ctrl-C reaches it)`, () => {
    const src = readFileSync(join(HERE, file), "utf8");
    assert.match(src, re, `${file}: the attached child no longer inherits stdio; if it is piped it leaves the console and an interrupt would leave it running`);
  });
}
