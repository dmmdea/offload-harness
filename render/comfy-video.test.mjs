// node --test render/comfy-video.test.mjs
// comfy-video.mjs arg parsing, graph building and the pre-submit node-class preflight,
// with ComfyUI STUBBED — no render, no network, no GPU. Importing the runner has no side
// effects (it runs main() only when invoked as the script).
import { test } from "node:test";
import assert from "node:assert";
import { parseArgs, wanVvramGb, buildGraphFromArgs, submitChecked } from "./comfy-video.mjs";

const stage = (p) => "staged_" + p; // stands in for the copy into <COMFY_DIR>/input

// A fake /object_info: answers 200 with the class present, or 200 {} for a class the
// server does not have (what ComfyUI returns for an unknown class).
function objectInfo(present) {
  const calls = [];
  const fetchImpl = async (url) => {
    calls.push(url);
    const cls = decodeURIComponent(url.split("/object_info/")[1]);
    return { ok: true, status: 200, json: async () => (present.has(cls) ? { [cls]: { input: {} } } : {}) };
  };
  return { fetchImpl, calls };
}

test("parseArgs: --fast is a boolean, trailing or mid-argv (it used to swallow the next token)", () => {
  // Trailing: the harness appends --fast after every value flag.
  let { pos, flags } = parseArgs(["o.mp4", "s.png", "push in", "--seed", "42", "--fast"]);
  assert.equal(flags.fast, true);
  assert.equal(flags.seed, "42");
  assert.deepEqual(pos, ["o.mp4", "s.png", "push in"]);
  // Mid-argv, followed by a value flag: that flag must keep its value.
  ({ pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--fast", "--upscale-model", "4x.pth"]));
  assert.equal(flags.fast, true);
  assert.equal(flags["upscale-model"], "4x.pth");
  assert.deepEqual(pos, ["o.mp4", "s.png", "p"], "the upscale model name must not become a positional");
});

test("buildGraphFromArgs: --fast selects the lightx2v LoRA recipe; absent = the native recipe", () => {
  const fast = buildGraphFromArgs(...Object.values(parseArgs(["o.mp4", "s.png", "p", "--seed", "1", "--fast"])), { stage }).graph;
  const loras = Object.values(fast).filter((n) => n.class_type === "LoraLoaderModelOnly");
  assert.equal(loras.length, 2, "fast mode loads both distill LoRAs");
  const native = buildGraphFromArgs(...Object.values(parseArgs(["o.mp4", "s.png", "p", "--seed", "1"])), { stage }).graph;
  assert.equal(Object.values(native).filter((n) => n.class_type === "LoraLoaderModelOnly").length, 0);
});

test("--wan-vvram-gb threads into both Wan DisTorch2 loaders; absent keeps the builder default", () => {
  const split = (argv) => {
    const { pos, flags } = parseArgs(argv);
    const g = buildGraphFromArgs(pos, flags, { stage }).graph;
    return [g["7"].inputs.virtual_vram_gb, g["9"].inputs.virtual_vram_gb];
  };
  assert.deepEqual(split(["o.mp4", "s.png", "p", "--wan-vvram-gb", "4.5"]), [4.5, 4.5]);
  assert.deepEqual(split(["o.mp4", "s.png", "p"]), [7, 7], "the builder default is unchanged");
});

test("--wan-loader threads through to the builder's loader choice", () => {
  const classesFor = (argv) => {
    const { pos, flags } = parseArgs(argv);
    const g = buildGraphFromArgs(pos, flags, { stage }).graph;
    return new Set([g["7"].class_type, g["9"].class_type]);
  };
  // default filenames are GGUF: --wan-loader native must refuse them
  assert.throws(
    () => buildGraphFromArgs(...Object.values(parseArgs(["o.mp4", "s.png", "p", "--wan-loader", "native"])), { stage }),
    /loader:"native" cannot load a GGUF expert/,
  );
  // with safetensors experts bound, native produces the plain loader, no flag = unchanged (GGUF/DisTorch2)
  const nativeArgv = ["o.mp4", "s.png", "p", "--high-unet", "high.safetensors", "--low-unet", "low.safetensors", "--wan-loader", "native"];
  assert.deepEqual(classesFor(nativeArgv), new Set(["UNETLoader"]));
  assert.deepEqual(classesFor(["o.mp4", "s.png", "p"]), new Set(["UnetLoaderGGUFDisTorch2MultiGPU"]), "unset --wan-loader keeps the historical default");
});

test("wanVvramGb refuses a value that is not a positive number", () => {
  assert.equal(wanVvramGb({}), undefined);
  assert.equal(wanVvramGb({ "wan-vvram-gb": "11" }), 11);
  for (const bad of ["0", "-3", "seven", ""]) {
    assert.throws(() => wanVvramGb({ "wan-vvram-gb": bad }), /--wan-vvram-gb must be a positive number/, `value ${JSON.stringify(bad)}`);
  }
});

test("submitChecked: a class the server lacks is a MISSING_NODE defer naming the pack, and nothing is POSTed", async () => {
  const { pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--seed", "1"]);
  const { graph } = buildGraphFromArgs(pos, flags, { stage });
  const all = new Set(Object.values(graph).map((n) => n.class_type));
  all.delete("VHS_VideoCombine");
  const { fetchImpl } = objectInfo(all);
  let submitted = 0;
  await assert.rejects(
    submitChecked({ api: "http://comfy.test", graph, clientId: "c", cli: null, fetchImpl, submit: async () => { submitted++; return { promptId: "x" }; }, comfyDir: "D:/ComfyUI" }),
    (e) => {
      assert.match(e.message, /^MISSING_NODE: /);
      assert.match(e.message, /VHS_VideoCombine \(custom node pack ComfyUI-VideoHelperSuite/);
      assert.match(e.message, /D:\/ComfyUI\/custom_nodes/);
      return true;
    },
  );
  assert.equal(submitted, 0, "the graph must never reach the POST");
});

test("submitChecked: every class present -> submitted once, each class asked about once", async () => {
  const { pos, flags } = parseArgs(["o.mp4", "s.png", "p", "--seed", "1"]);
  const { graph } = buildGraphFromArgs(pos, flags, { stage });
  const classes = new Set(Object.values(graph).map((n) => n.class_type));
  const { fetchImpl, calls } = objectInfo(classes);
  let submitted = 0;
  const res = await submitChecked({ api: "http://comfy.test", graph, clientId: "c", cli: null, fetchImpl, submit: async () => { submitted++; return { promptId: "p1" }; } });
  assert.equal(res.promptId, "p1");
  assert.equal(submitted, 1);
  assert.equal(calls.length, classes.size);
});

test("submitChecked: an unreadable /object_info steps aside and lets the submission speak", async () => {
  const graph = { "1": { class_type: "VHS_VideoCombine", inputs: {} } };
  const fetchImpl = async () => { throw new Error("ECONNREFUSED"); };
  let submitted = 0;
  await submitChecked({ api: "http://comfy.test", graph, clientId: "c", cli: null, fetchImpl, submit: async () => { submitted++; return { promptId: "p" }; } });
  assert.equal(submitted, 1);
});
