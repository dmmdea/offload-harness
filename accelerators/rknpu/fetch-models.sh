#!/usr/bin/env bash
# Fetch the RKNPU artifacts named in models.json and verify every sha256 (ADR 0024 lane). A file whose
# hash does not match is deleted and reported: the sidecar refuses to serve a mismatched file, so leaving
# one on disk would only move the failure to first use.
#
#   fetch-models.sh             downloads everything that has a public URL: the runtime (librknnrt.so into
#                               $RKNPU_HOME/lib, the RKNN-Toolkit-Lite2 wheel into $RKNPU_HOME/wheels) and,
#                               into the models dir, resnet18, the label files and the test images. The
#                               models that have no public download (yolov8n, the CLIP image tower) are
#                               only verified; exit 1 while one is absent.
#   fetch-models.sh --convert   BUILDS those models (rknn_model_zoo v2.3.2 recipes, target rk3588) into the
#                               models dir, then reports each one's sha256. Run it on an x86_64 Linux host
#                               whose $RKNPU_CONVERT_PYTHON (default python3) imports rknn-toolkit2 2.3.2:
#                                   uv venv --python 3.12 conv && P="uv pip install --python conv/bin/python"
#                                   $P --index-url https://download.pytorch.org/whl/cpu torch==2.4.0
#                                   $P numpy==1.26.4 'protobuf>=4.21.6,<=4.25.4' psutil ruamel.yaml scipy tqdm \
#                                      opencv-python-headless fast-histogram onnx==1.16.1 onnxruntime==1.17.1
#                                   $P --no-deps https://raw.githubusercontent.com/airockchip/rknn-toolkit2/v2.3.2/rknn-toolkit2/packages/x86_64/rknn_toolkit2-2.3.2-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl
#                               then RKNPU_MODELS_DIR=$PWD/out RKNPU_CONVERT_PYTHON=conv/bin/python
#                               ./fetch-models.sh --convert, copy the .rknn files to the board's models dir
#                               and run this script there (no flag) to verify them.
#
# A conversion is not bit-reproducible: the .rknn container embeds build metadata, so two runs of the same
# recipe on the same inputs differ by about a kilobyte (measured 2026-09-29: yolov8n 937 bytes, CLIP 814).
# models.json therefore pins the exact bytes that were verified on the board. After a conversion of your
# own the script prints the new hash; record it in models.json (the model's "sha256") to serve that file.
# The inputs ARE pinned: every ONNX source is checked against its sha256 before it is converted.
#
# Sources: airockchip/rknn-toolkit2 and airockchip/rknn_model_zoo at v2.3.2 (raw GitHub URLs; the ONNX
# files come from the model zoo's own download host). Env: RKNPU_HOME (else the directory holding venv/
# above this script), RKNPU_MODELS_DIR, RKNPU_MANIFEST, RKNPU_CONVERT_PYTHON, RKNPU_CONVERT_CACHE.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${RKNPU_MANIFEST:-$HERE/models.json}"
mode=fetch
case "${1:-}" in
  '') ;;
  --convert) mode=convert ;;
  *) echo "usage: fetch-models.sh [--convert]" >&2; exit 2 ;;
esac
if [ -z "${RKNPU_HOME:-}" ]; then
  for cand in "$HERE" "$(dirname "$HERE")" "$(dirname "$(dirname "$HERE")")"; do
    if [ -d "$cand/venv" ]; then RKNPU_HOME="$cand"; break; fi
  done
fi
DEST="${RKNPU_MODELS_DIR:-${RKNPU_HOME:+$RKNPU_HOME/models}}"
if [ -z "$DEST" ]; then
  echo "fetch-models.sh: no RKNPU_HOME (no venv/ above $HERE) and no RKNPU_MODELS_DIR" >&2
  exit 2
fi
BASE="${RKNPU_HOME:-$(dirname "$DEST")}"
mkdir -p "$DEST"

if [ "$mode" = convert ]; then
  exec "${RKNPU_CONVERT_PYTHON:-python3}" - "$MANIFEST" "$DEST" <<'PY'
import hashlib, json, os, sys, tempfile, urllib.request

manifest, dest = sys.argv[1], sys.argv[2]
man = json.load(open(manifest, encoding="utf-8"))
cache = os.environ.get("RKNPU_CONVERT_CACHE") or os.path.join(tempfile.gettempdir(), "rknpu-convert-cache")
os.makedirs(cache, exist_ok=True)


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def download(url, path, sha=None):
    if not (os.path.isfile(path) and (sha is None or sha256(path) == sha)):
        print(f"fetching {url}")
        urllib.request.urlretrieve(url, path + ".part")
        os.replace(path + ".part", path)
    if sha is not None and sha256(path) != sha:
        os.remove(path)
        sys.exit(f"FAILED   {url}: sha256 does not match models.json")


def dataset(name):
    """The quantisation images: a list file plus the images it names, fetched next to each other."""
    spec, root = man["datasets"][name], os.path.join(cache, name)
    os.makedirs(root, exist_ok=True)
    listing = os.path.join(root, "list.txt")
    download(spec["list"], listing)
    for rel in (line.strip() for line in open(listing, encoding="utf-8") if line.strip()):
        img = os.path.join(root, rel)
        os.makedirs(os.path.dirname(img), exist_ok=True)
        download(spec["base"] + rel.lstrip("./"), img)
    return listing


from rknn.api import RKNN  # before any download: a host without the toolkit stops here with the ImportError

failed = 0
for key, spec in man["models"].items():
    recipe = spec.get("convert")
    if recipe is None:
        continue
    onnx = os.path.join(cache, os.path.basename(recipe["onnx"]["url"]))
    download(recipe["onnx"]["url"], onnx, recipe["onnx"]["sha256"])
    out = os.path.join(dest, spec["file"])
    rknn = RKNN(verbose=False)
    rknn.config(mean_values=recipe["mean_values"], std_values=recipe["std_values"],
                target_platform=man["runtime"]["target"])
    steps = (lambda: rknn.load_onnx(model=onnx, **recipe.get("load_onnx", {})),
             lambda: rknn.build(do_quantization=recipe["quantize"],
                                dataset=dataset(recipe["dataset"]) if recipe.get("dataset") else None),
             lambda: rknn.export_rknn(out))
    if any(step() != 0 for step in steps):
        print(f"FAILED   {spec['file']} (conversion)")
        failed = 1
        continue
    rknn.release()
    got = sha256(out)
    if got == spec["sha256"]:
        print(f"ok       {spec['file']} (identical to the pin)")
    else:
        print(f"built    {spec['file']} sha256 {got}\n"
              f"         models.json pins {spec['sha256']}: put the new hash under \"{key}\" to serve this file")
        failed = 1
sys.exit(failed)
PY
fi

# Every artifact with a source: kind, destination dir, file, sha256, url ("-" for a converted model).
list_artifacts() {
  python3 - "$MANIFEST" <<'PY'
import json, sys
m = json.load(open(sys.argv[1], encoding="utf-8"))
def row(kind, where, name, sha, url="-"):
    print("\t".join((kind, where, name, sha, url)))
for spec in m["runtime"].values():
    if isinstance(spec, dict):
        row("get", "lib" if spec["file"].endswith(".so") else "wheels", spec["file"], spec["sha256"], spec["url"])
for spec in m["models"].values():
    row("get" if "url" in spec else "convert", "models", spec["file"], spec["sha256"], spec.get("url", "-"))
for section in ("labels", "testdata"):
    for name, spec in m.get(section, {}).items():
        row("get", "models", name, spec["sha256"], spec["url"])
PY
}

rows="$(list_artifacts)"  # a manifest that does not parse must stop the script, not read as "nothing to fetch"
fail=0
while IFS=$'\t' read -r kind where name sha url; do
  case "$where" in lib|wheels) dir="$BASE/$where" ;; *) dir="$DEST" ;; esac
  mkdir -p "$dir"
  out="$dir/$name"
  if [ -f "$out" ] && [ "$(sha256sum "$out" | cut -d' ' -f1)" = "$sha" ]; then
    echo "ok       $name"; continue
  fi
  if [ "$kind" = convert ]; then
    echo "MISSING  $name (no public download: build it on an x86_64 host with fetch-models.sh --convert, then copy it to $dir)"
    fail=1; continue
  fi
  echo "fetching $name"
  if ! curl -fsSL --retry 3 --max-time 300 -o "$out.part" "$url"; then
    echo "FAILED   $name (download)"; rm -f "$out.part"; fail=1; continue
  fi
  got="$(sha256sum "$out.part" | cut -d' ' -f1)"
  if [ "$got" != "$sha" ]; then
    echo "FAILED   $name (sha256 $got != $sha) — removed"; rm -f "$out.part"; fail=1; continue
  fi
  mv "$out.part" "$out"; echo "ok       $name"
done <<<"$rows"
exit $fail
