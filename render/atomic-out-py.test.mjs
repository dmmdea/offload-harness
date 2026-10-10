// node --test render/atomic-out-py.test.mjs
//
// The Python render workers deliver their output through render/atomic_out.py: edit_image.py (PIL) and
// tts_chatterbox.py (torch). The helper's own selftest runs on a bare python; the workers are run for
// real with the failure that matters injected: a save that writes a few bytes and then hits a full disk.
// tts_chatterbox.py runs against stub torch / torchaudio / chatterbox modules (nothing is loaded, no GPU);
// edit_image.py needs PIL and is skipped without it, like the Go suite's worker tests. Everything skips
// cleanly when there is no python 3 on PATH.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const GOOD = Buffer.from("the previous good file");

function findPython() {
  for (const c of [process.env.TTS_PY, "python3", "python"].filter(Boolean)) {
    const r = spawnSync(c, ["-c", "import sys; print(sys.version_info[0])"], { encoding: "utf8" });
    if (!r.error && r.status === 0 && r.stdout.trim() === "3") return c;
  }
  return null;
}
const PY = findPython();
const PIL = PY && spawnSync(PY, ["-c", "import PIL"]).status === 0;
const names = (d) => readdirSync(d).sort();
// no bytecode beside the workers: the sibling import would otherwise leave a render/__pycache__ behind
const pyEnv = (extra = {}) => ({ ...process.env, PYTHONDONTWRITEBYTECODE: "1", ...extra });

test("atomic_out.py --selftest passes on a bare python", { skip: !PY && "no python 3 on PATH" }, () => {
  const r = spawnSync(PY, [join(HERE, "atomic_out.py"), "--selftest"], { encoding: "utf8", env: pyEnv() });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.match(r.stdout, /SELFTEST PASS/);
});

// stub modules for tts_chatterbox.py: torchaudio.save writes a few bytes and, in STUB_SAVE=full mode,
// then fails the way a full disk does
function stubTorch() {
  const dir = mkdtempSync(join(tmpdir(), "tts-stub-"));
  writeFileSync(join(dir, "torch.py"), [
    "class _T:",
    "    def dim(self): return 2",
    "    def unsqueeze(self, i): return self",
    "    def detach(self): return self",
    "    def cpu(self): return self",
    "    def float(self): return self",
    "def is_tensor(x): return True",
    "def as_tensor(x): return x",
    "class cuda:",
    "    @staticmethod",
    "    def is_available(): return False",
    "",
  ].join("\n"));
  writeFileSync(join(dir, "torchaudio.py"), [
    "import os",
    "def save(path, wav, sr):",
    "    with open(path, 'wb') as f:",
    "        f.write(b'RIFFxxxxWAVE')",
    "    if os.environ.get('STUB_SAVE') == 'full':",
    "        raise OSError(28, 'No space left on device')",
    "",
  ].join("\n"));
  mkdirSync(join(dir, "chatterbox"));
  writeFileSync(join(dir, "chatterbox", "__init__.py"), "");
  writeFileSync(join(dir, "chatterbox", "mtl_tts.py"), [
    "from torch import _T",
    "class ChatterboxMultilingualTTS:",
    "    sr = 24000",
    "    @classmethod",
    "    def from_pretrained(cls, device=None): return cls()",
    "    def generate(self, text, language_id=None, **kw): return _T()",
    "",
  ].join("\n"));
  return dir;
}

function runTts(out, mode) {
  const stubs = stubTorch();
  return spawnSync(PY, [join(HERE, "tts_chatterbox.py"), "--out", out, "--text", "hola"], {
    encoding: "utf8",
    env: pyEnv({ PYTHONPATH: stubs, STUB_SAVE: mode, TTS_DEVICE: "cpu", HF_HUB_OFFLINE: "1" }),
  });
}

test("tts_chatterbox.py delivers the wav through the atomic helper: a good run replaces the file and leaves nothing beside it", { skip: !PY && "no python 3 on PATH" }, () => {
  const d = mkdtempSync(join(tmpdir(), "tts-out-")); const out = join(d, "voice.wav");
  writeFileSync(out, GOOD);
  const r = runTts(out, "ok");
  assert.equal(r.status, 0, r.stderr);
  assert.equal(readFileSync(out, "utf8"), "RIFFxxxxWAVE");
  assert.deepEqual(names(d), ["voice.wav"]);
});

test("tts_chatterbox.py: a full disk during the save fails the worker, keeps the previous wav and leaves no partial file", { skip: !PY && "no python 3 on PATH" }, () => {
  const d = mkdtempSync(join(tmpdir(), "tts-out-")); const out = join(d, "voice.wav");
  writeFileSync(out, GOOD);
  const r = runTts(out, "full");
  assert.notEqual(r.status, 0, "the worker must fail loud");
  assert.match(r.stderr, /No space left on device/);
  assert.deepEqual(readFileSync(out), GOOD, "the previous good wav must survive");
  assert.deepEqual(names(d), ["voice.wav"], "and no partial file may remain");

  const fresh = join(d, "new.wav");
  assert.notEqual(runTts(fresh, "full").status, 0);
  assert.deepEqual(names(d), ["voice.wav"], "a failed first delivery leaves no file at all at the output path");
});

// a 1x1 png, the input for the edit worker
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", "base64");

function editRequest(d, out) {
  const image = join(d, "in.png");
  writeFileSync(image, PNG);
  return JSON.stringify({ image, ops: [{ op: "resize", width: 4 }], out });
}

test("edit_image.py delivers through the atomic helper: a good run replaces the file and leaves nothing beside it", { skip: (!PY && "no python 3 on PATH") || (!PIL && "no PIL") }, () => {
  const d = mkdtempSync(join(tmpdir(), "edit-out-")); const out = join(d, "out.png");
  writeFileSync(out, GOOD);
  const r = spawnSync(PY, [join(HERE, "edit_image.py")], { input: editRequest(d, out), encoding: "utf8", env: pyEnv() });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.ok(readFileSync(out).subarray(0, 4).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47])), "the new png is at out");
  assert.deepEqual(names(d), ["in.png", "out.png"]);
});

test("edit_image.py: a full disk during the save fails the worker, keeps the previous image and leaves no partial file", { skip: (!PY && "no python 3 on PATH") || (!PIL && "no PIL") }, () => {
  const d = mkdtempSync(join(tmpdir(), "edit-out-")); const out = join(d, "out.png");
  writeFileSync(out, GOOD);
  // run the real worker with PIL's save replaced by one that writes a few bytes and then hits a full disk
  const driver = join(d, "driver.py");
  writeFileSync(driver, [
    "import errno, runpy, sys",
    "from PIL import Image",
    "def full_disk(self, fp, *a, **k):",
    "    with open(fp, 'wb') as f:",
    "        f.write(b'PNG')",
    "    raise OSError(errno.ENOSPC, 'No space left on device')",
    "Image.Image.save = full_disk",
    "sys.argv = ['edit_image.py']",
    "runpy.run_path(sys.argv_path, run_name='__main__')",
    "",
  ].join("\n").replace("sys.argv_path", JSON.stringify(join(HERE, "edit_image.py"))));
  const r = spawnSync(PY, [driver], { input: editRequest(d, out), encoding: "utf8", env: pyEnv() });
  assert.equal(r.status, 3, "a failed save is the worker's defer-class exit; " + r.stdout + r.stderr);
  assert.match(r.stdout, /No space left on device/);
  assert.deepEqual(readFileSync(out), GOOD, "the previous good image must survive");
  assert.deepEqual(names(d), ["driver.py", "in.png", "out.png"], "and no partial file may remain");
});
