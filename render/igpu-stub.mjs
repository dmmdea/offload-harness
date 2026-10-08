// igpu-stub.mjs — a stub ENGINE BINARY for the iGPU runners' main() tests (never shipped
// behaviour, never run by the harness; render/igpu-runners-main.test.mjs imports makeStub).
//
// The runners spawn their engine as a real executable with the engine's own argv (sd-cli's
// `-M vid_gen ...`, da3-cli's `depth ...`, audiocpp_cli's `--task ...`), so a stand-in has to be
// an executable file with that interface. makeStub writes one into a temp dir:
//   POSIX    a #!/bin/sh script that execs node on this file with a JSON spec,
//   Windows  a tiny Go launcher (built once, cached) that does the same, because a .cmd / .js
//            cannot be spawned without a shell and node.exe would parse the engine's flags.
// Run as a script, this file IS the engine: it records its argv, prints a log, writes the
// output the real engine would, and exits (or hangs, for the kill tests).
//
// spec = {
//   record:  path of a JSONL file; each invocation appends {argv, env, controlFrames}
//   pidFile: path the stub writes its own pid to (the "engine is dead" assertions read it)
//   log:     [lines] printed to stderr, in order, before anything else
//   logFile: path of a captured log printed first (then `log`)
//   exit:    exit code (default 0)
//   hang:    after the log, never exit (the runner has to kill it)
//   echoFlags: ["-p", ...]  also print the value after each of these flags, one bare line per line of it
//   frozen / black (writes.video): a still colour / a black clip
//   writes:  one of
//     {kind:"video", extraFrames, black, frozen, ffmpeg}   a real clip at `-o` (frames = --video-frames + extraFrames)
//     {kind:"wav", seconds, tailSilence, silent}    a PCM16 wav at `--out`
//     {kind:"gray_png"}                              a 1-channel PNG at `--png`
//   inspectControlVideo: record the PNG headers found in the directory after --control-video
// }
//
// Dependency-free (Node 18+ built-ins only).
import { spawnSync } from "node:child_process";
import { appendFileSync, chmodSync, copyFileSync, existsSync, mkdirSync, readdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";
import { deflateSync } from "node:zlib";

const self = fileURLToPath(import.meta.url);

// ---------------------------------------------------------------- building a stub

const LAUNCHER_GO = `package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	exe, _ := os.Executable()
	b, err := os.ReadFile(strings.TrimSuffix(exe, filepath.Ext(exe)) + ".target")
	if err != nil {
		os.Stderr.WriteString("stub launcher: " + err.Error() + "\\n")
		os.Exit(97)
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(b)), "\\r", ""), "\\n")
	args := append([]string{lines[1], lines[2]}, os.Args[1:]...)
	c := exec.Command(lines[0], args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		os.Exit(98)
	}
}
`;

let launcherExe = null;

// windowsLauncher: the compiled launcher, built once per machine (cached under the OS temp dir).
function windowsLauncher() {
  if (launcherExe) return launcherExe;
  const dir = join(tmpdir(), "igpu-stub-launcher-v1");
  const exe = join(dir, "launcher.exe");
  if (!existsSync(exe)) {
    mkdirSync(dir, { recursive: true });
    const unique = join(dir, `build-${process.pid}-${Date.now()}`);
    mkdirSync(unique, { recursive: true });
    writeFileSync(join(unique, "launcher.go"), LAUNCHER_GO);
    const out = join(unique, "launcher.exe");
    const r = spawnSync("go", ["build", "-o", out, "launcher.go"], { cwd: unique, encoding: "utf8", env: { ...process.env, GOFLAGS: "" } });
    if (r.status !== 0 || !existsSync(out)) throw new Error("cannot build the stub launcher with go: " + (r.stderr || r.error?.message || "unknown"));
    try { renameSync(out, exe); } catch { if (!existsSync(exe)) copyFileSync(out, exe); }
  }
  launcherExe = exe;
  return exe;
}

// stubAvailable: whether makeStub can work here (POSIX always; Windows needs `go`).
export function stubAvailable() {
  if (process.platform !== "win32") return true;
  try {
    windowsLauncher();
    return true;
  } catch {
    return false;
  }
}

// makeStub: write an executable `name` under dir that behaves as `spec` says; returns its path.
export function makeStub(dir, name, spec) {
  const specPath = join(dir, `${name}.spec.json`);
  writeFileSync(specPath, JSON.stringify(spec));
  if (process.platform === "win32") {
    const exe = join(dir, `${name}.exe`);
    copyFileSync(windowsLauncher(), exe);
    writeFileSync(join(dir, `${name}.target`), [process.execPath, self, specPath].join("\n"));
    return exe;
  }
  const sh = join(dir, name);
  writeFileSync(sh, `#!/bin/sh\nexec "${process.execPath}" "${self}" "${specPath}" "$@"\n`);
  chmodSync(sh, 0o755);
  return sh;
}

// ---------------------------------------------------------------- the files an engine writes

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();
function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, "latin1"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}

// grayPng: a real 8-bit grayscale (1-channel) PNG, what depth-anything writes.
export function grayPng(width, height) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 0; // colour type: grayscale
  const raw = Buffer.alloc((width + 1) * height);
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) raw[y * (width + 1) + 1 + x] = (x * 255 / Math.max(1, width - 1)) | 0;
  }
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr), chunk("IDAT", deflateSync(raw)), chunk("IEND", Buffer.alloc(0))]);
}

// pcmWav: mono PCM16 wav at 24 kHz: `seconds` of a 440 Hz tone, then `tailSilence` of zeros;
// silent = zeros throughout.
export function pcmWav({ seconds, tailSilence = 0, silent = false }) {
  const rate = 24000;
  const n = Math.round((seconds + tailSilence) * rate);
  const toneN = Math.round(seconds * rate);
  const pcm = Buffer.alloc(n * 2);
  for (let i = 0; i < n; i++) {
    const v = !silent && i < toneN ? Math.round(Math.sin(2 * Math.PI * 440 * i / rate) * 9000) : 0;
    pcm.writeInt16LE(v, i * 2);
  }
  const h = Buffer.alloc(44);
  h.write("RIFF", 0, "latin1");
  h.writeUInt32LE(36 + pcm.length, 4);
  h.write("WAVEfmt ", 8, "latin1");
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20);
  h.writeUInt16LE(1, 22);
  h.writeUInt32LE(rate, 24);
  h.writeUInt32LE(rate * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write("data", 36, "latin1");
  h.writeUInt32LE(pcm.length, 40);
  return Buffer.concat([h, pcm]);
}

function pngHeader(file) {
  const b = readFileSync(file).subarray(0, 33);
  return { name: file.split(/[\\/]/).pop(), width: b.readUInt32BE(16), height: b.readUInt32BE(20), bitDepth: b[24], colorType: b[25] };
}

// ---------------------------------------------------------------- the engine itself

function argAfter(argv, flag) {
  const i = argv.indexOf(flag);
  return i >= 0 ? argv[i + 1] : undefined;
}

function runAsEngine() {
  const spec = JSON.parse(readFileSync(process.argv[2], "utf8"));
  const argv = process.argv.slice(3);
  const rec = { argv, env: { GGML_VK_VISIBLE_DEVICES: process.env.GGML_VK_VISIBLE_DEVICES ?? null } };
  if (spec.inspectControlVideo) {
    const dir = argAfter(argv, "--control-video");
    rec.controlFrames = dir && existsSync(dir) ? readdirSync(dir).filter((f) => f.endsWith(".png")).sort().map((f) => pngHeader(join(dir, f))) : [];
  }
  if (spec.record) appendFileSync(spec.record, JSON.stringify(rec) + "\n");
  if (spec.pidFile) writeFileSync(spec.pidFile, String(process.pid));

  const lines = [];
  if (spec.logFile) lines.push(...readFileSync(spec.logFile, "utf8").split(/\r\n|\r|\n/));
  if (spec.log) lines.push(...spec.log);
  for (const l of lines) process.stderr.write(l + "\n");
  // echoFlags: print the value after each named flag, one bare line per line of it (an engine that
  // echoes the request's own text into its log)
  for (const flag of spec.echoFlags || []) {
    const v = argAfter(argv, flag);
    if (v) for (const l of v.split(/\r\n|\r|\n/)) process.stderr.write(l + "\n");
  }

  const w = spec.writes;
  if (w && w.kind === "gray_png") {
    const out = argAfter(argv, "--png");
    const input = argAfter(argv, "--input");
    // a depth map at the model's own working size, not the frame's
    writeFileSync(out, grayPng(Number(w.width) || 40, Number(w.height) || 72));
    if (input && !existsSync(input)) process.stderr.write(`stub: input ${input} does not exist\n`);
  } else if (w && w.kind === "wav") {
    writeFileSync(argAfter(argv, "--out"), pcmWav(w));
  } else if (w && w.kind === "video") {
    const out = argAfter(argv, "-o");
    const width = argAfter(argv, "-W") || "64";
    const height = argAfter(argv, "-H") || "64";
    const fps = argAfter(argv, "--fps") || "16";
    const frames = Number(argAfter(argv, "--video-frames") || 5) + (w.extraFrames || 0);
    // black: an all-black clip; frozen: one still colour (not black, never changing); else a moving test pattern
    const src = w.black ? `color=c=black:s=${width}x${height}:r=${fps}` : w.frozen ? `color=c=0x2060c0:s=${width}x${height}:r=${fps}` : `testsrc2=s=${width}x${height}:r=${fps}`;
    const r = spawnSync(w.ffmpeg, ["-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src, "-frames:v", String(frames), "-an", out], { encoding: "utf8" });
    if (r.status !== 0) {
      process.stderr.write("stub: ffmpeg failed: " + r.stderr + "\n");
      process.exit(99);
    }
  }
  if (spec.hang) {
    setInterval(() => {}, 1000);
    return;
  }
  process.exit(spec.exit ?? 0);
}

if (import.meta.url === pathToFileURL(process.argv[1] || "").href) runAsEngine();
