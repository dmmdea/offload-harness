#!/usr/bin/env bash
# Fetch the RKNPU artifacts named in models.json and verify every one against its publisher's digest (ADR 0024
# lane). Nothing is pinned: at run time the script resolves the NEWEST non-draft, non-prerelease release of each
# GitHub repo under "sources" in models.json that carries every file asked of it (a half-published release steps
# back to the one before), reads each file's publisher digest from the GitHub API (the git blob id of the file at
# that release's tag) or, for the timm weights, from the Hugging Face tree API (lfs.oid at main), and refuses
# bytes that do not match. A file that fails is not kept: the sidecar would only move the failure to first use.
# Every file that passes is recorded as {file, digest, publisher_digest, size, source, fetched_at} in
# models.installed.json beside the weights; digest is the sha256 of the bytes, the value the sidecar re-checks
# before it serves a file (no record: served, reported as unverified).
#
#   fetch-models.sh             downloads everything that has a public source: the runtime (librknnrt.so into
#                               $RKNPU_HOME/lib, the RKNN-Toolkit-Lite2 aarch64 wheel for the venv's python into
#                               $RKNPU_HOME/wheels; older wheels there are removed) and, into the models dir, the
#                               label files and the test images. No model has a public .rknn (PP-YOLOE+ s, the two
#                               ResNet-50 builds, the CLIP image tower), so the models are only checked against
#                               the digest recorded when they were built; exit 1 while one is absent.
#   fetch-models.sh --dry-run   resolves and prints every publisher digest (and the timm weights' lfs.oid);
#                               downloads and writes nothing.
#   fetch-models.sh --convert   BUILDS those models (rknn-toolkit2, target rk3588) into the models dir, then
#                               records each one's sha256. Run it on an x86_64 Linux host whose
#                               $RKNPU_CONVERT_PYTHON (default python3) imports rknn-toolkit2, and timm and
#                               torch for the ResNet-50 export:
#                                   uv venv --python 3.12 conv && P="uv pip install --python conv/bin/python"
#                                   $P --index-url https://download.pytorch.org/whl/cpu 'torch>=2.4.0' 'torchvision>=0.19.0'
#                                   $P 'numpy>=1.26.4' 'protobuf>=4.21.6,<=4.25.4' psutil ruamel.yaml scipy tqdm \
#                                      opencv-python-headless fast-histogram 'onnx>=1.16.1' 'onnxruntime>=1.17.1' \
#                                      timm safetensors
#                                   $P --no-deps <the x86_64 wheel under rknn-toolkit2/packages/x86_64/ of the
#                                      newest release of airockchip/rknn-toolkit2, for the venv's python>
#                               then RKNPU_MODELS_DIR=$PWD/out RKNPU_CONVERT_PYTHON=conv/bin/python
#                               ./fetch-models.sh --convert, copy the .rknn files and models.installed.json to the
#                               board's models dir and run this script there (no flag) to check them.
#
# A model's recipe ("convert" in models.json) names its source in one of two ways:
#   "onnx"    an ONNX on the model zoo's own download host: the link is read from the zoo's download_model.sh at the
#             resolved release. That host publishes no digest, so the sha256 the script observed is recorded.
#   "export"  a timm model whose ONNX is hosted nowhere: the weights file is downloaded and checked against the Hugging
#             Face lfs.oid, timm builds the network around that local file (nothing else is fetched), and
#             torch.onnx.export writes the ONNX with the recipe's input name, size and opset (the TorchScript
#             exporter: newer torch defaults to another one, so dynamo=False is passed wherever torch has the
#             argument). The exported ONNX has no digest of its own: an export is not reproducible across torch
#             versions. It is cached, and shared by every model that names the same export.
# Calibration images ("dataset") come from a list: a URL ("list") or a file of this repository, relative to the
# manifest ("list_file", one image name per line, so a copy of this directory made for converting needs calib/
# beside models.json; the board needs neither), plus the base URL the images are fetched from. Every entry is
# checked before the first image is fetched.
#
# A conversion is not bit-reproducible: the .rknn container embeds build metadata, so two runs of the same
# recipe on the same inputs differ by about a kilobyte. The record names the exact bytes that were built.
#
# Env: RKNPU_HOME (else the directory holding venv/ above this script), RKNPU_MODELS_DIR, RKNPU_MANIFEST,
# RKNPU_CONVERT_PYTHON, RKNPU_CONVERT_CACHE, GITHUB_TOKEN or GH_TOKEN (optional: lifts the API rate limit).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${RKNPU_MANIFEST:-$HERE/models.json}"
mode=fetch
dry=0
case "${1:-}" in
  '') ;;
  --convert) mode=convert ;;
  --dry-run) dry=1 ;;
  *) echo "usage: fetch-models.sh [--convert | --dry-run]" >&2; exit 2 ;;
esac
if [ -z "${RKNPU_HOME:-}" ]; then
  for cand in "$HERE" "$(dirname "$HERE")" "$(dirname "$(dirname "$HERE")")"; do
    if [ -d "$cand/venv" ]; then RKNPU_HOME="$cand"; break; fi
  done
fi
DEST="${RKNPU_MODELS_DIR:-${RKNPU_HOME:+$RKNPU_HOME/models}}"
if [ -z "$DEST" ] && [ "$dry" = 0 ]; then
  echo "fetch-models.sh: no RKNPU_HOME (no venv/ above $HERE) and no RKNPU_MODELS_DIR" >&2
  exit 2
fi
DEST="${DEST:-.}"
BASE="${RKNPU_HOME:-$(dirname "$DEST")}"
[ "$dry" = 1 ] || mkdir -p "$DEST"

# Shared by both modes: HTTP with retries, GitHub release and contents resolution, records.
read -r -d '' COMMON <<'PY' || true
import fnmatch, hashlib, json, os, re, sys, time, urllib.error, urllib.parse, urllib.request

API = "https://api.github.com"


def stamp():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def http(url, binary=False, soft=False):
    headers = {"User-Agent": "offload-harness-fetch-models"}
    if url.startswith(API):
        headers["Accept"] = "application/vnd.github+json"
        token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
        if token:
            headers["Authorization"] = "Bearer " + token
    last = None
    for attempt in range(3):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=300) as r:
                data = r.read()
            return data if binary else json.loads(data)
        except urllib.error.HTTPError as e:
            if soft and e.code == 404:
                return None
            last = e
        except Exception as e:  # network error: retry, then report
            last = e
        time.sleep(1 + attempt)
    sys.exit(f"FAILED   {url}: {last}")


def blob_id(data):
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def lookup(repo, tag, want):
    """One required file at a tag: ("file", path) or ("glob", dir, pattern: the last name in order wins)."""
    ref = urllib.parse.quote(tag)
    if want[0] == "file":
        e = http(f"{API}/repos/{repo}/contents/{urllib.parse.quote(want[1])}?ref={ref}", soft=True)
        return e if isinstance(e, dict) and e.get("type") == "file" else None
    listing = http(f"{API}/repos/{repo}/contents/{urllib.parse.quote(want[1])}?ref={ref}", soft=True)
    if not isinstance(listing, list):
        return None
    hits = sorted((x for x in listing if x.get("type") == "file" and fnmatch.fnmatch(x["name"], want[2])),
                  key=lambda x: x["name"])
    return hits[-1] if hits else None


def resolve(source, wants):
    """The newest non-draft, non-prerelease release of source["repo"] (of the last 10) that carries every wanted file,
    each with a publisher digest (its git blob id). Returns (tag, {want: contents entry})."""
    repo = source["repo"]
    rels = http(f"{API}/repos/{repo}/releases?per_page=10")
    for rel in rels:
        if rel.get("draft") or rel.get("prerelease"):
            continue
        found = {}
        for w in wants:
            e = lookup(repo, rel["tag_name"], w)
            if e is None or not e.get("sha") or not e.get("download_url"):
                break
            found[w] = e
        else:
            return rel["tag_name"], found
    sys.exit(f"FAILED   no non-draft release of {repo} (of the newest 10) carries every file asked of it: {wants}")


def fetch_blob(entry):
    """The file's bytes, or None when they do not match the publisher's blob id."""
    data = http(entry["download_url"], binary=True)
    return data if blob_id(data) == entry["sha"] else None


def load_records(path):
    try:
        with open(path, encoding="utf-8") as fh:
            return {r["file"]: r for r in json.load(fh).get("installed", [])}
    except (OSError, ValueError, KeyError, TypeError, AttributeError):
        return {}


def save_records(path, records):
    with open(path + ".part", "w", encoding="utf-8", newline="\n") as fh:
        json.dump({"installed": sorted(records.values(), key=lambda r: r["file"])}, fh, indent=2)
        fh.write("\n")
    os.replace(path + ".part", path)


def record(records, name, digest, size, source, publisher=None):
    when = stamp()
    old = records.get(name, {})
    if old.get("digest") == "sha256:" + digest:
        when = old.get("fetched_at", when)  # unchanged bytes keep the time they were fetched
    records[name] = {"file": name, "digest": "sha256:" + digest, "publisher_digest": publisher, "size": size,
                     "source": source, "fetched_at": when}
PY

read -r -d '' FETCH <<'PY' || true
manifest, dest, base, pytag, dry = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5] == "1"
man = json.load(open(manifest, encoding="utf-8"))
RECORDS = os.path.join(dest, "models.installed.json")

# Every artifact with a public source: where (lib, wheels, models), name (None: the matched file's own), source, want.
items = []
rt = man["runtime"]
w = rt["wheel"]
items.append(dict(where="wheels", name=None, source=w["source"],
                  want=("glob", w["dir"], w["pattern"].replace("{pytag}", pytag))))
lib = rt["librknnrt"]
items.append(dict(where="lib", name=lib["file"], source=lib["source"], want=("file", lib["path"])))
for spec in man["models"].values():
    if "path" in spec:
        items.append(dict(where="models", name=spec["file"], source=spec["source"], want=("file", spec["path"])))
    else:
        items.append(dict(where="models", name=spec["file"], source=None, want=None))
for section in ("labels", "testdata"):
    for name, spec in man.get(section, {}).items():
        items.append(dict(where="models", name=name, source=spec["source"], want=("file", spec["path"])))

resolved = {}
for src in sorted({i["source"] for i in items if i["source"]}):
    wants = [i["want"] for i in items if i["source"] == src]
    tag, found = resolve(man["sources"][src], wants)
    resolved[src] = (tag, found)
    print(f"resolved {man['sources'][src]['repo']}@{tag}")

if dry:  # the weights a conversion exports from: their publisher digest, for the record
    shown = set()
    for spec in man["models"].values():
        wsrc = spec.get("convert", {}).get("export", {}).get("weights")
        if wsrc and (wsrc["hf"], wsrc["path"]) not in shown:
            shown.add((wsrc["hf"], wsrc["path"]))
            tree = http(f"https://huggingface.co/api/models/{wsrc['hf']}/tree/main?recursive=true")
            hit = next((e for e in tree if e["path"] == wsrc["path"] and e.get("lfs")), None)
            print(f"resolved huggingface.co/{wsrc['hf']}/{wsrc['path']}  sha256:{hit['lfs']['oid'] if hit else '?'}  "
                  f"{hit['lfs']['size'] if hit else '?'} bytes  (weights for the conversion)")

records = load_records(RECORDS)
fail = 0
for item in items:
    where, name = item["where"], item["name"]
    folder = os.path.join(base, where) if where in ("lib", "wheels") else dest
    key = name if where == "models" else None
    if item["want"] is None:  # a converted model: no public download, only checked against what was recorded
        out = os.path.join(folder, name)
        if dry:
            continue
        if not os.path.isfile(out):
            print(f"MISSING  {name} (no public download: build it on an x86_64 host with fetch-models.sh --convert, "
                  f"then copy it and models.installed.json to {folder})")
            fail = 1
            continue
        want = records.get(name, {}).get("digest")
        got = "sha256:" + sha256_file(out)
        if want is None:
            print(f"ok       {name} (unverified: no digest recorded for it)")
        elif want != got:
            print(f"FAILED   {name} (sha256 {got} differs from the recorded {want})")
            fail = 1
        else:
            print(f"ok       {name} (matches the digest recorded when it was built)")
        continue
    tag, found = resolved[item["source"]]
    entry = found[item["want"]]
    name = name or entry["name"]
    key = name if where == "models" else f"{where}/{name}"
    repo = man["sources"][item["source"]]["repo"]
    if dry:
        print(f"resolved {where}/{name}  git-blob-sha1:{entry['sha']}  {entry['size']} bytes  {repo}@{tag}")
        continue
    os.makedirs(folder, exist_ok=True)
    out = os.path.join(folder, name)
    data = None
    if os.path.isfile(out):
        with open(out, "rb") as fh:
            have = fh.read()
        if blob_id(have) == entry["sha"]:
            data = have
    if data is None:
        print(f"fetching {name}")
        data = fetch_blob(entry)
        if data is None:
            print(f"FAILED   {name} (the downloaded bytes do not match the publisher's digest git-blob-sha1:{entry['sha']}) - not kept")
            fail = 1
            continue
        with open(out + ".part", "wb") as fh:
            fh.write(data)
        os.replace(out + ".part", out)
    record(records, key, sha256_bytes(data), len(data), f"github:{repo}@{tag}:{entry['path']}", "git-blob-sha1:" + entry["sha"])
    if where == "wheels":
        for stale in os.listdir(folder):
            if stale != name and stale.startswith("rknn_toolkit_lite2-") and stale.endswith(".whl"):
                os.remove(os.path.join(folder, stale))
    print(f"ok       {name}")

if not dry:
    save_records(RECORDS, records)
sys.exit(fail)
PY

read -r -d '' CONVERT <<'PY' || true
import inspect, tempfile

manifest, dest = sys.argv[1], sys.argv[2]
man = json.load(open(manifest, encoding="utf-8"))
RECORDS = os.path.join(dest, "models.installed.json")
cache = os.environ.get("RKNPU_CONVERT_CACHE") or os.path.join(tempfile.gettempdir(), "rknpu-convert-cache")
os.makedirs(cache, exist_ok=True)


def download(url, path):
    if not os.path.isfile(path):
        print(f"fetching {url}")
        with open(path + ".part", "wb") as fh:
            fh.write(http(url, binary=True))
        os.replace(path + ".part", path)


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


zoo_wants = [("file", r["onnx"]["script"]) for r in (s.get("convert", {}) for s in man["models"].values()) if "onnx" in r]
zoo = resolve(man["sources"]["zoo"], zoo_wants) if zoo_wants else None


def zoo_onnx(spec):
    """The zoo's ONNX: its download link is read from the zoo's download_model.sh at the resolved release. The host
    publishes no digest, so the sha256 observed is what gets recorded."""
    tag, found = zoo
    script = fetch_blob(found[("file", spec["script"])])
    if script is None:
        sys.exit(f"FAILED   {spec['script']} does not match the publisher's digest")
    hit = re.search(r"https?://\S+/" + re.escape(spec["name"]) + r"(?=\s|$)", script.decode("utf-8", "replace"))
    if hit is None:
        sys.exit(f"FAILED   {spec['script']} no longer names {spec['name']}")
    folder = os.path.join(cache, "zoo-" + tag)
    os.makedirs(folder, exist_ok=True)
    path = os.path.join(folder, spec["name"])
    download(hit.group(0), path)
    return path, f"{hit.group(0)} (the host publishes no digest; sha256 observed {sha256_file(path)})"


def export_onnx(spec):
    """The ONNX of a timm model (see the header): weights checked against Hugging Face's lfs.oid, network built
    around them, exported once."""
    import timm
    import torch

    w = spec["weights"]
    tree = http(f"https://huggingface.co/api/models/{w['hf']}/tree/main?recursive=true")
    hit = next((e for e in tree if e["path"] == w["path"] and e.get("lfs")), None)
    if hit is None:
        sys.exit(f"FAILED   huggingface.co/{w['hf']} has no LFS file {w['path']} at main")
    oid = hit["lfs"]["oid"]
    weights = os.path.join(cache, f"{spec['timm']}-{oid[:12]}.safetensors")
    if not (os.path.isfile(weights) and sha256_file(weights) == oid):
        download_to = weights
        print(f"fetching huggingface.co/{w['hf']}/{w['path']}")
        with open(download_to + ".part", "wb") as fh:
            fh.write(http(f"https://huggingface.co/{w['hf']}/resolve/main/{w['path']}", binary=True))
        os.replace(download_to + ".part", weights)
        if sha256_file(weights) != oid:
            os.remove(weights)
            sys.exit(f"FAILED   huggingface.co/{w['hf']}/{w['path']}: sha256 does not match the publisher's lfs.oid {oid}")
    size = spec["input_size"]
    out = os.path.join(cache, f"{spec['timm']}-{oid[:12]}-opset{spec['opset']}-{'x'.join(map(str, size))}.onnx")
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
    return out, f"huggingface.co/{w['hf']}/{w['path']} (lfs.oid sha256:{oid}), exported with torch"


from rknn.api import RKNN  # before any build: a host without the toolkit stops here with the ImportError

try:
    from importlib.metadata import version as _version
    toolkit = _version("rknn-toolkit2")
except Exception:
    toolkit = "unknown version"

records = load_records(RECORDS)
failed = 0
for key, spec in man["models"].items():
    recipe = spec.get("convert")
    if recipe is None:
        continue
    if "export" in recipe:
        try:
            onnx, origin = export_onnx(recipe["export"])
        except ImportError as e:
            print(f"FAILED   {spec['file']} (its ONNX is exported with timm and torch, which {sys.executable} cannot import: {e})")
            failed = 1
            continue
    else:
        onnx, origin = zoo_onnx(recipe["onnx"])
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
    got = sha256_file(out)
    record(records, spec["file"], got, os.path.getsize(out),
           f"converted with rknn-toolkit2 {toolkit}, target {man['runtime']['target']}, from {origin}")
    print(f"built    {spec['file']} sha256:{got}")
save_records(RECORDS, records)
sys.exit(failed)
PY

if [ "$mode" = convert ]; then
  exec "${RKNPU_CONVERT_PYTHON:-python3}" -I -c "$COMMON"$'\n'"$CONVERT" "$MANIFEST" "$DEST"
fi

# The wheel is tagged for one interpreter: the venv's when it exists, else this python3.
PYTHON=""
for cand in "${RKNPU_HOME:+$RKNPU_HOME/venv/bin/python}" "${RKNPU_HOME:+$RKNPU_HOME/venv/Scripts/python.exe}"; do
  if [ -n "$cand" ] && [ -x "$cand" ]; then PYTHON="$cand"; break; fi
done
if [ -n "$PYTHON" ] || [ "$dry" = 0 ]; then
  PYTAG="$("${PYTHON:-python3}" -I -c 'import sys; print("cp%d%d" % sys.version_info[:2])' | tr -d '\r')"
else
  PYTAG='cp3*'  # a dry run on a box with no venv: show whichever wheel the release carries
fi
exec python3 -I -c "$COMMON"$'\n'"$FETCH" "$MANIFEST" "$DEST" "$BASE" "$PYTAG" "$dry"
