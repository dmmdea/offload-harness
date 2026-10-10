// node --test render/sdcpp-generate-main.test.mjs
//
// sdcpp-generate.mjs main() WIRING against a STUB sd-cli (render/igpu-stub.mjs: an executable with the
// engine's interface). sd-cli writes the file named by -o itself, so a full disk or a kill mid-write left a
// truncated png at the output path, over a good one if there was one. The runner now has sd-cli write a
// staged sibling (the extension stays last, sd-cli picks its format from it) and renames it onto the output
// only once the engine exited 0 with a non-empty file and the alpha rewrite is done.
//
// Nothing here needs a GPU: the Vulkan device is pinned by env, --no-lock takes no lease, and llama-swap is
// pointed at a closed port so the lease-less path can never reach a real one. Needs, on Windows, a Go
// toolchain for the stub launcher (the same prerequisite as igpu-runners-main.test.mjs).
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { makeStub, stubAvailable } from "./igpu-stub.mjs";
import { decodePng, encodePng } from "./png-alpha.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const skip = !stubAvailable() ? "cannot build a stub engine binary here (needs go on Windows)" : false;

// a 2x2 RGBA png, the way sd.cpp's qwen-image-2.1 VAE writes it: partly transparent
const RGBA = encodePng({ width: 2, height: 2, channels: 4, pixels: Buffer.from([255, 0, 0, 255, 0, 255, 0, 243, 0, 0, 255, 0, 10, 20, 30, 128]) });
const GOOD = Buffer.from("the previous good render");

function sandbox() {
  const root = mkdtempSync(join(tmpdir(), "sdcpp-main-"));
  const outDir = join(root, "renders");
  const work = join(root, "work");
  mkdirSync(outDir);
  mkdirSync(work);
  return { root, outDir, work, out: join(outDir, "a.png"), done: () => rmSync(root, { recursive: true, force: true }) };
}

function runMain(sb, stubSpec) {
  const record = join(sb.work, "calls.jsonl");
  const bin = makeStub(sb.work, "sd-cli", { record, ...stubSpec });
  const env = { ...process.env, SDCPP_BIN: bin, GGML_VK_VISIBLE_DEVICES: "0", LLAMA_SWAP_API: "http://127.0.0.1:9" };
  for (const k of Object.keys(env)) if (k.startsWith("GPU_LEASE")) delete env[k];
  return new Promise((resolve) => {
    const c = spawn(process.execPath, [join(HERE, "sdcpp-generate.mjs"), sb.out, "a red bike", "--no-lock"], { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "";
    c.stdout.on("data", (d) => { stdout += d; });
    c.stderr.on("data", (d) => { stderr += d; });
    const t = setTimeout(() => c.kill("SIGKILL"), 120_000);
    c.on("close", (status) => {
      clearTimeout(t);
      const calls = existsSync(record) ? readFileSync(record, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l)) : [];
      resolve({ status, stdout, stderr, calls });
    });
  });
}

test("sdcpp-generate: sd-cli writes a staged sibling, and only the finished (alpha-flattened) image is renamed onto the output", { skip }, async () => {
  const sb = sandbox();
  try {
    writeFileSync(sb.out, GOOD);
    const r = await runMain(sb, { writes: { kind: "file_o", base64: RGBA.toString("base64") } });
    assert.equal(r.status, 0, r.stderr);
    assert.equal(r.calls.length, 1);
    const target = r.calls[0].argv[r.calls[0].argv.indexOf("-o") + 1];
    assert.notEqual(target, sb.out, "the engine must not be pointed at the output path itself");
    assert.equal(dirname(target), sb.outDir, "the staged file is a sibling, so the rename never crosses a volume");
    assert.match(basename(target), /^\..*\.partial-\d+-\d+\.png$/, "hidden, and the extension is last for sd-cli's format pick");
    assert.equal(decodePng(readFileSync(sb.out)).channels, 3, "the default render is delivered opaque");
    assert.deepEqual(readdirSync(sb.outDir), ["a.png"], "nothing is left beside the output");
  } finally { sb.done(); }
});

test("sdcpp-generate: an engine that fails after writing part of the image leaves the previous render untouched and nothing beside it", { skip }, async () => {
  const sb = sandbox();
  try {
    writeFileSync(sb.out, GOOD);
    const r = await runMain(sb, { writes: { kind: "file_o", base64: RGBA.subarray(0, 20).toString("base64") }, exit: 1 });
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /sd-cli exited 1/);
    assert.deepEqual(readFileSync(sb.out), GOOD, "the half-written image must never replace the previous one");
    assert.deepEqual(readdirSync(sb.outDir), ["a.png"]);
  } finally { sb.done(); }
});

test("sdcpp-generate: an engine that exits 0 with an EMPTY file is a failure, and no zero-byte file reaches the output path", { skip }, async () => {
  const sb = sandbox();
  try {
    const r = await runMain(sb, { writes: { kind: "file_o" } });
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, /exited 0 but produced an empty file/);
    assert.deepEqual(readdirSync(sb.outDir), [], "nothing at the output path and nothing staged");
  } finally { sb.done(); }
});
