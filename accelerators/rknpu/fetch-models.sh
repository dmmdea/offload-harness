#!/usr/bin/env bash
# Fetch the RKNPU artifacts named in models.json and verify every sha256 (ADR 0024 lane). A file whose
# hash does not match is deleted and reported: the sidecar refuses to serve a mismatched file, so leaving
# one on disk would only move the failure to first use.
#
#   fetch-models.sh             downloads everything that has a public URL: the runtime (librknnrt.so into
#                               $RKNPU_HOME/lib, the RKNN-Toolkit-Lite2 wheel into $RKNPU_HOME/wheels) and,
#                               into the models dir, the label files and the test images. No model has a
#                               public .rknn (PP-YOLOE+ s, the two ResNet-50 builds, the CLIP image tower), so
#                               the models are only verified; exit 1 while one is absent.
#   fetch-models.sh --convert   BUILDS those models (rknn-toolkit2 2.3.2, target rk3588) into the models dir,
#                               then reports each one's sha256. Run it on an x86_64 Linux host whose
#                               $RKNPU_CONVERT_PYTHON (default python3) imports rknn-toolkit2 2.3.2, and
#                               timm and torch for the ResNet-50 export:
#                                   uv venv --python 3.12 conv && P="uv pip install --python conv/bin/python"
#                                   $P --index-url https://download.pytorch.org/whl/cpu torch==2.4.0 torchvision==0.19.0
#                                   $P numpy==1.26.4 'protobuf>=4.21.6,<=4.25.4' psutil ruamel.yaml scipy tqdm \
#                                      opencv-python-headless fast-histogram onnx==1.16.1 onnxruntime==1.17.1 \
#                                      timm safetensors
#                                   $P --no-deps https://raw.githubusercontent.com/airockchip/rknn-toolkit2/v2.3.2/rknn-toolkit2/packages/x86_64/rknn_toolkit2-2.3.2-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl
#                               then RKNPU_MODELS_DIR=$PWD/out RKNPU_CONVERT_PYTHON=conv/bin/python
#                               ./fetch-models.sh --convert, copy the .rknn files to the board's models dir
#                               and run this script there (no flag) to verify them.
#
# A model's recipe ("convert" in models.json) names its source in one of two ways:
#   "onnx"    a hosted ONNX (the zoo's own download host): downloaded and checked against its sha256 first.
#   "export"  a timm model whose ONNX is hosted nowhere: the weights file is downloaded and checked against its
#             sha256, timm builds the network around that local file (nothing else is fetched), and
#             torch.onnx.export writes the ONNX with the recipe's input name, size and opset (the TorchScript
#             exporter: torch >= 2.9 defaults to another one, so dynamo=False is passed wherever torch has the
#             argument). The exported ONNX has NO sha256 check: an export is not reproducible across torch
#             versions, so only its weights are pinned. It is cached, and shared by every model that names the
#             same export.
# Calibration images ("dataset") come from a list: a URL ("list") or a file of this repository, relative to the
# manifest ("list_file", one image name per line, so a copy of this directory made for converting needs calib/
# beside models.json; the board needs neither), plus the base URL the images are fetched from. Every entry is
# checked before the first image is fetched, and the images are not pinned by hash.
#
# A conversion is not bit-reproducible: the .rknn container embeds build metadata, so two runs of the same
# recipe on the same inputs differ by about a kilobyte (measured yolov8n 937 bytes, CLIP 814 on 2026-09-29;
# ppyoloe_s 1040, resnet50tv2-i8 923, resnet50tv2-fp16 1002 on 2026-09-30).
# models.json therefore pins the exact bytes that were built and checked. After a conversion of your
# own the script prints the new hash; record it in models.json (the model's "sha256") to serve that file.
# Every downloaded ONNX and every timm weights file is checked against its sha256 before it is used.
#
# Sources: airockchip/rknn-toolkit2 and airockchip/rknn_model_zoo at v2.3.2 (raw GitHub URLs; the ONNX
# files come from the model zoo's own download host), the timm weights from Hugging Face, and the COCO
# val2017 images from images.cocodataset.org. Env: RKNPU_HOME (else the directory holding venv/
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
import hashlib, inspect, json, os, sys, tempfile, urllib.request

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


def entry_path(name, rel):
    """One calibration-list entry, normalised. The list is downloaded or read from a file, so an entry that is
    absolute or climbs out with ".." would make the join below write outside the cache: refuse it before anything
    is joined. The same check refuses a manifest's "list_file" that leaves the manifest's directory."""
    posix = rel.replace("\\", "/")
    if posix.startswith("/") or os.path.isabs(rel) or os.path.splitdrive(rel)[0] or ".." in posix.split("/"):
        sys.exit(f"FAILED   dataset {name}: refusing the list entry {rel!r} (absolute or containing '..')")
    return os.path.normpath(posix)


def dataset(name):
    """The quantisation images: a list file plus the images it names, fetched next to each other. The list is a URL
    ("list", downloaded) or a file of this repository ("list_file", relative to the manifest); either way every entry
    is checked before the first image is fetched."""
    spec, root = man["datasets"][name], os.path.join(cache, name)
    if ("list" in spec) == ("list_file" in spec):
        sys.exit(f"FAILED   dataset {name}: exactly one of \"list\" and \"list_file\" is required")
    os.makedirs(root, exist_ok=True)
    listing = os.path.join(root, "list.txt")
    if "list_file" in spec:
        local = os.path.join(os.path.dirname(os.path.abspath(manifest)), entry_path(name, spec["list_file"]))
        with open(local, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    else:
        download(spec["list"], listing)
        with open(listing, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    entries = [entry_path(name, line.strip()) for line in lines if line.strip()]
    if "list_file" in spec:  # written next to the images: the toolkit resolves the entries against the list's directory
        with open(listing, "w", encoding="utf-8", newline="\n") as fh:
            fh.write("".join(rel.replace(os.sep, "/") + "\n" for rel in entries))
    for rel in entries:
        img = os.path.join(root, rel)
        os.makedirs(os.path.dirname(img), exist_ok=True)
        download(spec["base"] + rel.replace(os.sep, "/"), img)
    return listing


def export_onnx(spec):
    """The ONNX of a timm model (see the header): weights checked, network built around them, exported once."""
    import timm
    import torch

    weights = os.path.join(cache, spec["timm"] + ".safetensors")
    download(spec["weights_url"], weights, spec["weights_sha256"])
    size = spec["input_size"]
    out = os.path.join(cache, f"{spec['timm']}-{spec['weights_sha256'][:12]}-opset{spec['opset']}-{'x'.join(map(str, size))}.onnx")
    if not os.path.isfile(out):
        model = timm.create_model(spec["timm"], pretrained=True, pretrained_cfg_overlay=dict(file=weights)).eval()
        kwargs = dict(input_names=[spec["input_name"]], output_names=["logits"], opset_version=spec["opset"],
                      do_constant_folding=True)
        if "dynamo" in inspect.signature(torch.onnx.export).parameters:
            kwargs["dynamo"] = False
        print(f"exporting {spec['timm']} to {out}")
        with torch.no_grad():
            torch.onnx.export(model, torch.randn(*size), out + ".part", **kwargs)
        os.replace(out + ".part", out)
    return out


from rknn.api import RKNN  # before any download: a host without the toolkit stops here with the ImportError

failed = 0
for key, spec in man["models"].items():
    recipe = spec.get("convert")
    if recipe is None:
        continue
    if "export" in recipe:
        try:
            onnx = export_onnx(recipe["export"])
        except ImportError as e:
            print(f"FAILED   {spec['file']} (its ONNX is exported with timm and torch, which {sys.executable} cannot import: {e})")
            failed = 1
            continue
    else:
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

# A manifest that does not parse must stop the script, not read as "nothing to fetch" (pipefail carries the failure
# through tr). tr drops the CR that a native Windows python (Git Bash) puts before every newline: it would ride into
# the last field of each row, the URL, and curl refuses "https://...\r".
rows="$(list_artifacts | tr -d '\r')"
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
