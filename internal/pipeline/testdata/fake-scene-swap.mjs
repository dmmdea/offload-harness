// fake-scene-swap.mjs — GPU-free stand-in for the CMP scene-swap CLI
// (docs contract: node scripts/run-scene-swap.mjs --job <path> --tier <id>
// --out <root> [--backend harness] [--comfy-input <dir>] [--assets-root <dir>]).
// Used by internal/pipeline/pipeline_job_test.go (Task 6, Step 4) to exercise
// runPipelineJob end to end with no real ComfyUI/GPU involved.
//
// Contract mirrored exactly:
//   - success: writes final.png + qa-report.json under <out>/<job.id>/, exits 0.
//   - failure: when the job spec's id is "fail-case", prints exactly one line
//     "SCENE-SWAP-FAIL stage=<last completed stage or none>: <message>" to
//     stderr and exits 1.
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import path from "node:path";

function argVal(flag) {
  const i = process.argv.indexOf(flag);
  return i >= 0 ? process.argv[i + 1] : undefined;
}

const jobPath = argVal("--job");
const outRoot = argVal("--out");
const tier = argVal("--tier") ?? "";

if (!jobPath || !outRoot) {
  console.error("SCENE-SWAP-FAIL stage=none: missing --job or --out");
  process.exit(1);
}

let job;
try {
  job = JSON.parse(readFileSync(jobPath, "utf8"));
} catch (e) {
  console.error(`SCENE-SWAP-FAIL stage=none: unreadable job spec: ${e.message}`);
  process.exit(1);
}

const id = job.id;

if (id === "fail-case") {
  console.error("SCENE-SWAP-FAIL stage=background: boom");
  process.exit(1);
}

const outDir = path.join(outRoot, id);
mkdirSync(outDir, { recursive: true });

// Minimal valid 1x1 transparent PNG (magic bytes + a real, tiny IHDR/IDAT/IEND
// stream) — enough for any downstream "is this a real file" check.
const png = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
  "base64"
);
writeFileSync(path.join(outDir, "final.png"), png);
writeFileSync(
  path.join(outDir, "qa-report.json"),
  JSON.stringify({ ok: true, job_id: id, tier, backend: argVal("--backend") ?? "harness" }, null, 2)
);
process.exit(0);
