#!/usr/bin/env node
// window-watch-run.mjs - run a command with NO console of its own and report every window that appears.
//
//   node scripts/window-watch-run.mjs [--out <dir>] [--interval-ms 30] [--cwd <dir>] [--timeout-sec 3600] [--hidden-console] -- <exe> [args...]
//   node scripts/window-watch-run.mjs --selftest
//
// The rule it checks: a console program started by a process that has no console of its own gets a NEW console
// window, a Windows Terminal window by default. So the command is started DETACHED (no console at all) and the
// children it starts are what would open windows. scripts/window-watch.ps1 polls the desktop meanwhile and logs
// each new window. Run a compiled test binary (`go test -c`) rather than `go test`: go.exe is itself a console
// program and would open a window of its own when started with no console.
//
// --hidden-console starts the command with a console of its own that has no window instead (CREATE_NO_WINDOW), for a
// command whose OWN runner starts children without hiding them (`node --test` starts every test file with a plain
// spawn: started with no console, it opens a Windows Terminal window per file, whatever the files do). The children of
// the code under test still start console-less where the code starts them detached, which is what the watch is for.
//
// Exit codes: 0 the command ended and no console window appeared; 3 a console window appeared (Windows
// Terminal host, console host or pseudo console); 2 a usage error or a watcher that did not start; otherwise
// the command's own exit code. Windows only.
//
// --selftest is the positive control, and puts nothing on the screen: it starts the watcher with
// -IncludeHidden, has a child create a HIDDEN window, and requires the log to list it. The run itself relies on
// Node starting a detached child with DETACHED_PROCESS (libuv), i.e. with no console at all; the console tests in
// internal/winexec start their helpers the same way and read the child's console back.
import { spawn } from "node:child_process";
import { closeSync, createWriteStream, existsSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const WATCHER = join(HERE, "window-watch.ps1");
const CONSOLE_CLASSES = new Set(["ConsoleWindowClass", "CASCADIA_HOSTING_WINDOW_CLASS", "PseudoConsoleWindow"]);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function usage(msg) {
  process.stderr.write(`${msg ? msg + "\n" : ""}usage: node scripts/window-watch-run.mjs [--out <dir>] [--interval-ms N] [--cwd <dir>] [--timeout-sec N] [--hidden-console] -- <exe> [args...]\n       node scripts/window-watch-run.mjs --selftest\n`);
  process.exit(2);
}

function startWatcher(work, { interval = 30, includeHidden = false } = {}) {
  const log = join(work, "watch.log");
  const stop = join(work, "watch.stop");
  rmSync(stop, { force: true });
  const args = ["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", WATCHER, "-OutFile", log, "-StopFile", stop, "-IntervalMs", String(interval)];
  if (includeHidden) args.push("-IncludeHidden");
  const child = spawn("powershell.exe", args, { stdio: "ignore", windowsHide: true });
  const done = new Promise((res) => child.on("close", res));
  return { log, stop, child, done };
}

async function ready(w, timeoutMs = 60000) {
  const until = Date.now() + timeoutMs;
  while (Date.now() < until) {
    if (existsSync(w.log) && readFileSync(w.log, "utf8").includes("# watching")) return true;
    await sleep(100);
  }
  return false;
}

async function finish(w) {
  writeFileSync(w.stop, "stop");
  await Promise.race([w.done, sleep(30000)]);
  const text = existsSync(w.log) ? readFileSync(w.log, "utf8") : "";
  const windows = text.split(/\r?\n/).filter((l) => l && !l.startsWith("#")).map((l) => {
    const [ms, hwnd, pid, proc, cls, title, state] = l.split("|");
    return { ms: Number(ms), hwnd, pid, proc, cls, title, state, console: CONSOLE_CLASSES.has(cls) };
  });
  const done = /# done polls=(\d+) new=(\d+) console=(\d+)/.exec(text);
  return { windows, polls: done ? Number(done[1]) : 0, closed: Boolean(done) };
}

// runDetached: the command with no console of its own, its output in a file
function runDetached(exe, args, { cwd, outFile, timeoutSec, hiddenConsole = false }) {
  if (hiddenConsole) {
    // piped stdio + windowsHide: libuv adds CREATE_NO_WINDOW, so the child owns a console that has no window
    return new Promise((res) => {
      const sink = createWriteStream(outFile);
      const child = spawn(exe, args, { cwd, stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
      child.stdout.pipe(sink, { end: false });
      child.stderr.pipe(sink, { end: false });
      const timer = setTimeout(() => { try { process.kill(child.pid); } catch { /* gone */ } }, timeoutSec * 1000);
      child.on("error", (e) => { clearTimeout(timer); sink.end(); res({ code: 2, error: e.message }); });
      child.on("close", (code) => { clearTimeout(timer); sink.end(() => res({ code: code ?? 1 })); });
    });
  }
  const fd = openSync(outFile, "w");
  return new Promise((res) => {
    const child = spawn(exe, args, { cwd, detached: true, stdio: ["ignore", fd, fd] });
    const timer = setTimeout(() => { try { process.kill(child.pid); } catch { /* gone */ } }, timeoutSec * 1000);
    child.on("error", (e) => { clearTimeout(timer); closeSync(fd); res({ code: 2, error: e.message }); });
    child.on("close", (code) => { clearTimeout(timer); closeSync(fd); res({ code: code ?? 1 }); });
  });
}

async function selftest() {
  const work = mkdtempSync(join(tmpdir(), "window-watch-self-"));
  try {
    // the watcher lists a window that appears (a hidden one, created by a child, so nothing is shown)
    const w = startWatcher(work, { interval: 20, includeHidden: true });
    if (!(await ready(w))) throw new Error("the watcher did not start");
    const form = "Add-Type -AssemblyName System.Windows.Forms; $f = New-Object System.Windows.Forms.Form; $f.Text = 'window-watch-selftest'; $f.ShowInTaskbar = $false; $null = $f.Handle; Start-Sleep -Milliseconds 800; $f.Dispose()";
    const child = spawn("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", form], { stdio: "ignore", windowsHide: true });
    await new Promise((res) => child.on("close", res));
    await sleep(200);
    const seen = await finish(w);
    const hit = seen.windows.find((x) => x.title === "window-watch-selftest" && x.state === "hidden");
    if (!hit) throw new Error(`the watcher did not list the hidden test window (it saw ${seen.windows.length} new windows, ${seen.polls} polls)`);
    process.stdout.write(`selftest ok: the watcher listed the hidden test window (pid ${hit.pid}, class ${hit.cls}) after ${seen.polls} polls\n`);
    return 0;
  } catch (e) {
    process.stderr.write(`selftest FAILED: ${e.message}\n`);
    return 1;
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

async function main() {
  const argv = process.argv.slice(2);
  if (argv[0] === "--selftest") process.exit(await selftest());
  const dash = argv.indexOf("--");
  if (dash < 0 || dash === argv.length - 1) usage("no command after --");
  const opts = { out: "", interval: 30, cwd: process.cwd(), timeout: 3600, hiddenConsole: false };
  for (let i = 0; i < dash; i += 2) {
    const k = argv[i], v = argv[i + 1];
    if (k === "--hidden-console") { opts.hiddenConsole = true; i--; continue; }
    if (v === undefined) usage(`${k} needs a value`);
    if (k === "--out") opts.out = resolve(v);
    else if (k === "--interval-ms") opts.interval = Number(v);
    else if (k === "--cwd") opts.cwd = resolve(v);
    else if (k === "--timeout-sec") opts.timeout = Number(v);
    else usage(`unknown option ${k}`);
  }
  const [exe, ...args] = argv.slice(dash + 1);
  const work = opts.out || mkdtempSync(join(tmpdir(), "window-watch-"));
  mkdirSync(work, { recursive: true });
  const w = startWatcher(work, { interval: opts.interval });
  if (!(await ready(w))) { process.stderr.write("the watcher did not start\n"); process.exit(2); }
  const outFile = join(work, "command.out");
  const started = Date.now();
  const r = await runDetached(exe, args, { cwd: opts.cwd, outFile, timeoutSec: opts.timeout, hiddenConsole: opts.hiddenConsole });
  await sleep(1500); // windows of children that outlived the command
  const seen = await finish(w);
  const consoles = seen.windows.filter((x) => x.console);
  process.stdout.write(`command exited ${r.code} after ${((Date.now() - started) / 1000).toFixed(1)} s; watched ${seen.polls} polls; new windows ${seen.windows.length}; console windows ${consoles.length}\n`);
  for (const x of seen.windows) process.stdout.write(`  ${x.console ? "CONSOLE" : "other  "} +${x.ms}ms pid ${x.pid} ${x.proc} ${x.cls} "${x.title}" ${x.state}\n`);
  process.stdout.write(`log: ${w.log}\ncommand output: ${outFile}\n`);
  if (!seen.closed) { process.stderr.write("the watcher's log was not closed: its result is incomplete\n"); process.exit(2); }
  process.exit(consoles.length ? 3 : r.code);
}

main().catch((e) => { process.stderr.write(`${e.stack || e}\n`); process.exit(2); });
