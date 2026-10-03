// derive-cpu-fixtures.mjs — builds the sd.cpp CPU-placement NEGATIVE fixtures from the real
// healthy logs. No CPU run was ever captured (the node's operator forbids running a model on
// its CPU, even to make a fixture), so each negative is a healthy log with the device
// substituted in exactly the two line shapes sd.cpp prints, as read from the pinned source
// (see README.md): the params-buffer line and the compute-buffer line.
//
//   params : "... prepared params backend buffers (<n> MB, <t> tensors, <b> blocks, VRAM) on Vulkan0"
//            -> "... (<n> MB, <t> tensors, <b> blocks, RAM) on CPU"
//   compute: "<module> compute buffer size: <n> MB(VRAM) on Vulkan0 (peak ...)"
//            -> "<module> compute buffer size: <n> MB(RAM) on CPU (peak ...)"
//
// Modes: "all" substitutes every such line; "diffusion-compute" substitutes only the
// diffusion stage's compute-buffer line (the text encoder and VAE stay on the GPU).
//
// Run `node render/testdata/derive-cpu-fixtures.mjs` to (re)write the files; the node test
// re-derives them in memory and fails when a checked-in file drifts from its source.
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import { dirname, join } from "node:path";

const AUX = /(?:^|[\s_.-])(?:t5\w*|umt5\w*|clip\w*|llm\w*|\w*vae\w*|tae\w*|taehv|taesd|esrgan|text_?enc\w*|conditioner|control\w*|vision\w*)(?:[\s_.-]|$)/i;

export const DERIVED = [
  { out: "sdcpp-video-cpu-derived.log", from: "sdcpp-video-healthy.log", mode: "all" },
  { out: "sdcpp-video-cpu-compute-derived.log", from: "sdcpp-video-healthy.log", mode: "diffusion-compute" },
  { out: "sdcpp-video-tae-cpu-derived.log", from: "sdcpp-video-tae.log", mode: "all" },
];

export function header(from) {
  return `# derived from ${from} by substituting the device; no CPU run was captured because the node's operator forbids model compute on its CPU`;
}

export function derive(sourceText, from, mode) {
  const lines = sourceText.split("\n");
  let changed = 0;
  const out = lines.map((line) => {
    if (/prepared params backend buffers .*\bVRAM\) on Vulkan\d+/.test(line) && mode === "all") {
      changed++;
      return line.replace(/\bVRAM\) on Vulkan\d+/, "RAM) on CPU");
    }
    const m = /(?:^|\s-\s)(\S+) compute buffer size: .*MB\(VRAM\) on Vulkan\d+/.exec(line);
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
