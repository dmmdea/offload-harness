#!/usr/bin/env bash
# Fetch the Coral zoo artifacts named in models.json into CORAL_MODELS_DIR (Coral D7). Nothing is pinned here:
# at run time the script resolves the tip of the default branch of google-coral/test_data (Apache-2.0) through
# the GitHub API, reads each file's publisher digest (the git blob id and size of the file at that tip),
# downloads the bytes and refuses any file whose own blob id differs. A file that fails the check is deleted
# and reported (the sidecar would only move the failure to first use). Every file that passes is recorded as
# {file, digest, publisher_digest, size, source, fetched_at} in models.installed.json beside the weights;
# digest is the sha256 of the bytes, the value the sidecar re-checks before it loads a model.
#
#   fetch-models.sh             fetch whatever is absent or differs from the publisher's current file
#   fetch-models.sh --dry-run   resolve and print each file's publisher digest and size; download nothing
#
# Env: CORAL_MODELS_DIR, CORAL_MANIFEST, GITHUB_TOKEN or GH_TOKEN (optional: lifts the API rate limit).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${CORAL_MANIFEST:-$HERE/models.json}"
DEST="${CORAL_MODELS_DIR:-$(dirname "$HERE")/models}"
dry=0
case "${1:-}" in
  '') ;;
  --dry-run) dry=1 ;;
  *) echo "usage: fetch-models.sh [--dry-run]" >&2; exit 2 ;;
esac
[ "$dry" = 1 ] || mkdir -p "$DEST"
exec python3 -I - "$MANIFEST" "$DEST" "$dry" <<'PY'
import hashlib, json, os, sys, time, urllib.request

manifest, dest, dry = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
man = json.load(open(manifest, encoding="utf-8"))
repo = man["source"]["repo"]
API = "https://api.github.com"
RECORDS = os.path.join(dest, "models.installed.json")


def get(url, binary=False):
    headers = {"User-Agent": "offload-harness-fetch-models", "Accept": "application/vnd.github+json"}
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if token and url.startswith(API):
        headers["Authorization"] = "Bearer " + token
    last = None
    for attempt in range(3):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=300) as r:
                data = r.read()
            return data if binary else json.loads(data)
        except Exception as e:  # network or HTTP error: retry, then report
            last = e
            time.sleep(1 + attempt)
    raise SystemExit(f"FAILED   {url}: {last}")


def blob_id(data):
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


branch = get(f"{API}/repos/{repo}")["default_branch"]
tree = get(f"{API}/repos/{repo}/git/trees/{branch}?recursive=1")
if tree.get("truncated"):
    raise SystemExit(f"FAILED   {repo}: the tree listing is truncated, cannot resolve publisher digests")
index = {e["path"]: e for e in tree["tree"] if e["type"] == "blob"}

names, seen = [], set()
for spec in man["models"].values():
    for key in ("file", "labels"):
        f = spec.get(key)
        if f and f not in seen:
            seen.add(f); names.append(f)
for f in man.get("testdata", []):
    if f not in seen:
        seen.add(f); names.append(f)

try:
    records = {r["file"]: r for r in json.load(open(RECORDS, encoding="utf-8")).get("installed", [])}
except (OSError, ValueError):
    records = {}

fail = 0
for name in names:
    entry = index.get(name)
    if entry is None:
        print(f"FAILED   {name} (not in {repo}@{branch})"); fail = 1; continue
    want = entry["sha"]
    if dry:
        print(f"resolved {name}  git-blob-sha1:{want}  {entry['size']} bytes  {repo}@{branch}")
        continue
    out = os.path.join(dest, name)
    data = None
    if os.path.isfile(out):
        with open(out, "rb") as fh:
            have = fh.read()
        if blob_id(have) == want:
            data = have
    verb = "ok      "
    if data is None:
        print(f"fetching {name}")
        data = get(f"https://raw.githubusercontent.com/{repo}/{branch}/{name}", binary=True)
        if blob_id(data) != want:
            print(f"FAILED   {name} (the downloaded bytes do not match the publisher's digest git-blob-sha1:{want}) - not kept")
            fail = 1; continue
        with open(out + ".part", "wb") as fh:
            fh.write(data)
        os.replace(out + ".part", out)
    digest = "sha256:" + hashlib.sha256(data).hexdigest()
    when = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    if records.get(name, {}).get("digest") == digest:
        when = records[name].get("fetched_at", when)  # unchanged bytes keep the time they were fetched
    records[name] = {"file": name, "digest": digest, "publisher_digest": "git-blob-sha1:" + want,
                     "size": len(data), "source": f"github:{repo}@{branch}:{name}", "fetched_at": when}
    print(f"{verb} {name}")

if not dry:
    with open(RECORDS + ".part", "w", encoding="utf-8", newline="\n") as fh:
        json.dump({"installed": sorted(records.values(), key=lambda r: r["file"])}, fh, indent=2)
        fh.write("\n")
    os.replace(RECORDS + ".part", RECORDS)
sys.exit(fail)
PY
