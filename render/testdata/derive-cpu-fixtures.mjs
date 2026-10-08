// derive-cpu-fixtures.mjs — builds the sd.cpp CPU-placement NEGATIVE fixtures from the real
// healthy logs. No CPU run was ever captured (the node's operator forbids running a model on
// its CPU, even to make a fixture), so each negative is a healthy log with the device
// substituted in exactly the line shapes sd.cpp prints, as read from the pinned source
// (see README.md): the params-buffer line, the compute-buffer line and the auto-fit plan line.
//
//   params : "... prepared params backend buffers (<n> MB, <t> tensors, <b> blocks, VRAM) on Vulkan0"
//            -> "... (<n> MB, <t> tensors, <b> blocks, RAM) on CPU"
//   compute: "<module> compute buffer size: <n> MB(VRAM) on Vulkan0 (peak ...)"
//            -> "<module> compute buffer size: <n> MB(RAM) on CPU (peak ...)"
//   plan   : "DiT   params <n> MiB, compute reserve <m> MiB -> compute Vulkan0, params Vulkan0"
//            -> "DiT   params <n> MiB, compute reserve <m> MiB -> compute CPU, params RAM"
//
// The substitution touches only the device words, so it works on both sd.cpp log formats: the
// master-929 one ("[VERBOSE] ggml_runner.cpp:1019 - <message>") and the master-945 one
// ("[V] <message> --- ggml_runner.cpp:1019"). The line's head and tail are left as they are.
//
// Modes: "all" substitutes every params and compute-buffer line; "diffusion-compute"
// substitutes only the diffusion stage's compute-buffer line (the text encoder and VAE stay on
// the GPU); "plan" substitutes only the auto-fit plan's DiT line (everything else stays healthy,
// so what fails is the plan shape alone).
//
// Run `node render/testdata/derive-cpu-fixtures.mjs` to (re)write the files; the node test
// re-derives them in memory and fails when a checked-in file drifts from its source.
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import { dirname, join } from "node:path";

const AUX = /(?:^|[\s_.-])(?:t5\w*|umt5\w*|clip\w*|llm\w*|\w*vae\w*|tae\w*|taehv|taesd|esrgan|text_?enc\w*|conditioner|control\w*|vision\w*)(?:[\s_.-]|$)/i;

export const DERIVED = [
  // master-929 (stable-diffusion.cpp 3f8527a), "[VERBOSE] file.cpp:N - message"
  { out: "sdcpp-video-cpu-derived.log", from: "sdcpp-video-healthy.log", mode: "all" },
  { out: "sdcpp-video-cpu-compute-derived.log", from: "sdcpp-video-healthy.log", mode: "diffusion-compute" },
  { out: "sdcpp-video-cpu-plan-derived.log", from: "sdcpp-video-healthy.log", mode: "plan" },
  { out: "sdcpp-video-tae-cpu-derived.log", from: "sdcpp-video-tae.log", mode: "all" },
  // master-945 (stable-diffusion.cpp a1ded76), "[V] message --- file.cpp:N"
  { out: "sdcpp945-video-cpu-derived.log", from: "sdcpp945-video-healthy.log", mode: "all" },
  { out: "sdcpp945-video-cpu-compute-derived.log", from: "sdcpp945-video-healthy.log", mode: "diffusion-compute" },
  { out: "sdcpp945-video-cpu-plan-derived.log", from: "sdcpp945-video-healthy.log", mode: "plan" },
  { out: "sdcpp945-video-tae-cpu-derived.log", from: "sdcpp945-video-tae.log", mode: "all" },
  { out: "sdcpp945-vace-cpu-compute-derived.log", from: "sdcpp945-vace-healthy.log", mode: "diffusion-compute" },
];

export function header(from) {
  return `# derived from ${from} by substituting the device; no CPU run was captured because the node's operator forbids model compute on its CPU`;
}

export function derive(sourceText, from, mode) {
  const lines = sourceText.split("\n");
  let changed = 0;
  const out = lines.map((line) => {
    if (mode === "plan") {
      // the plan line of the diffusion component: "<head>    DiT   params ... -> compute Vulkan0, params Vulkan0 <tail>"
      if (/\bDiT\s+params\s.*->\s*compute\s+Vulkan\d+,\s*params\s+\S+/.test(line)) {
        changed++;
        return line.replace(/->\s*compute\s+Vulkan\d+,\s*params\s+\S+/, "-> compute CPU, params RAM");
      }
      return line;
    }
    if (/prepared params backend buffers .*\bVRAM\) on Vulkan\d+/.test(line) && mode === "all") {
      changed++;
      return line.replace(/\bVRAM\) on Vulkan\d+/, "RAM) on CPU");
    }
    // the module name is the word before " compute buffer size:", whatever head the line carries
    const m = /(?:^|\s)(\S+) compute buffer size: .*MB\(VRAM\) on Vulkan\d+/.exec(line);
    if (m && (mode === "all" || !AUX.test(m[1]))) {
      changed++;
      return line.replace(/MB\(VRAM\) on Vulkan\d+/, "MB(RAM) on CPU");
    }
    return line;
  });
  if (changed === 0) throw new Error(`derive: no placement line shape found in ${from}`);
  return header(from) + "\n" + out.join("\n");
}

if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  const here = dirname(fileURLToPath(import.meta.url));
  for (const d of DERIVED) {
    writeFileSync(join(here, d.out), derive(readFileSync(join(here, d.from), "utf8"), d.from, d.mode));
    console.log("wrote", d.out);
  }
}
