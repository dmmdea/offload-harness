#!/usr/bin/env python3
"""Fetch and verify everything the Axera AX650 lane runs: the runtime pieces and the models.

    fetch_models.py [--home DIR] [--only KEY[,KEY...]] [--runtime] [--models] [--dry-run] [--verify]

Reads models.json beside this file. It names files and sources only: no version, commit or digest
(ADR 0099). Every source is resolved at the moment this runs and the integrity check comes from the
publisher:

  * Hugging Face models: the repository's current commit on its default branch, the tree API's
    per-file id (the LFS sha256 for a weight, the git blob id for a small file) and the size. The
    downloaded bytes must match both.
  * GitHub runtime assets (the pyaxengine wheel): the newest non-draft,
    non-prerelease release that carries the asset, checked against the asset digest the GitHub API
    publishes.

A file that passes is recorded in <home>/models/models.installed.json (models) or
<home>/runtime.installed.json (runtime) as {file, digest, publisher_digest, size, source, fetched_at}.
The record is data about what was installed, not a rule: the next run resolves again. The sidecar
re-hashes a model against its record before it loads it and refuses a mismatch.

--dry-run prints the resolution (commit or release, each file, size and publisher digest) and writes
nothing. --verify re-hashes files already on disk instead of trusting size + record. Standard library
only, so it runs before the venv exists.
"""
from __future__ import annotations

import argparse
import fnmatch
import hashlib
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
UA = "offload-harness-axera-fetch"


def _home(arg: str | None) -> str:
    if arg:
        return os.path.abspath(arg)
    if os.environ.get("AXERA_HOME"):
        return os.path.abspath(os.environ["AXERA_HOME"])
    # The same walk the launcher does: the home is the first ancestor holding venv/ or models/.
    for cand in (HERE, os.path.dirname(HERE), os.path.dirname(os.path.dirname(HERE))):
        if os.path.isdir(os.path.join(cand, "venv")) or os.path.isdir(os.path.join(cand, "models")):
            return cand
    return os.path.dirname(HERE)


def _get(url: str, accept: str = "application/json", timeout: int = 60):
    headers = {"User-Agent": UA, "Accept": accept}
    tok = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if tok and urllib.parse.urlparse(url).hostname == "api.github.com":
        headers["Authorization"] = "Bearer " + tok
    hf = os.environ.get("HF_TOKEN")
    if hf and urllib.parse.urlparse(url).hostname == "huggingface.co":
        headers["Authorization"] = "Bearer " + hf
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=timeout)


def _json(url: str):
    with _get(url) as r:
        return json.load(r)


def _sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _git_blob_id(path: str) -> str:
    size = os.path.getsize(path)
    h = hashlib.sha1(b"blob %d\0" % size)
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _download(url: str, dest: str, want_size: int | None) -> None:
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".part-", dir=os.path.dirname(dest))
    got = 0
    t0 = time.monotonic()
    try:
        with os.fdopen(fd, "wb") as out, _get(url, accept="*/*", timeout=120) as r:
            for chunk in iter(lambda: r.read(1 << 20), b""):
                out.write(chunk)
                got += len(chunk)
        if want_size is not None and got != want_size:
            raise IOError(f"size {got} != published {want_size}")
        os.replace(tmp, dest)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    dt = max(time.monotonic() - t0, 1e-3)
    print(f"  got {os.path.relpath(dest)} {got / 1e6:.1f} MB in {dt:.0f} s ({got / 1e6 / dt:.1f} MB/s)", flush=True)


def _load(path: str) -> dict:
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def _save(path: str, data: dict) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=1, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)


def _now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


# ---- Hugging Face models -------------------------------------------------------------------------

def _hf_resolve(src: dict, repo: str) -> tuple[str, list[dict]]:
    meta = _json(src["api"].format(repo=repo))
    rev = meta.get("sha") or "main"
    tree = _json(src["tree"].format(repo=repo, rev=rev))
    return rev, [e for e in tree if e.get("type") == "file"]


def _wanted(entry: dict, files: list[dict]) -> list[dict]:
    pats = entry["files"]
    out = [f for f in files if any(fnmatch.fnmatchcase(f["path"], p) for p in pats)]
    missing = [p for p in pats if not any(fnmatch.fnmatchcase(f["path"], p) for f in files)]
    if missing:
        raise LookupError(f"not published in the repository: {', '.join(missing)}")
    return out


def fetch_models(manifest: dict, home: str, only: set[str], dry: bool, verify: bool) -> int:
    src = manifest["sources"]["huggingface"]
    models_dir = os.path.join(home, "models")
    rec_path = os.path.join(models_dir, "models.installed.json")
    record = _load(rec_path)
    failures = 0
    for key, entry in manifest["models"].items():
        if only and key not in only:
            continue
        repo = entry["repo"]
        try:
            rev, files = _hf_resolve(src, repo)
            chosen = _wanted(entry, files)
        except (urllib.error.URLError, LookupError, ValueError) as e:
            print(f"[{key}] {repo}: {e}", file=sys.stderr)
            failures += 1
            continue
        total = sum(f.get("size", 0) for f in chosen)
        print(f"[{key}] {repo} @ {rev[:12]}: {len(chosen)} files, {total / 1e6:.1f} MB", flush=True)
        for f in chosen:
            rel = f"{key}/{f['path']}"
            dest = os.path.join(models_dir, key, f["path"])
            lfs = f.get("lfs") or {}
            pub = ("sha256:" + lfs["oid"]) if lfs.get("oid") else ("git-blob:" + f["oid"])
            size = lfs.get("size", f.get("size"))
            if dry:
                print(f"  would fetch {rel} {size} B {pub}")
                continue
            old = record.get(rel, {})
            if os.path.isfile(dest) and old.get("publisher_digest") == pub and os.path.getsize(dest) == size and not verify:
                continue
            try:
                if not (os.path.isfile(dest) and os.path.getsize(dest) == size and _matches(dest, pub)):
                    _download(src["file"].format(repo=repo, rev=rev, path=urllib.parse.quote(f["path"])), dest, size)
                if not _matches(dest, pub):
                    os.unlink(dest)
                    raise IOError(f"publisher digest mismatch for {rel} ({pub}); file deleted")
                record[rel] = {"file": rel, "digest": "sha256:" + _sha256(dest), "publisher_digest": pub,
                               "size": size, "source": f"hf:{repo}@{rev}", "fetched_at": _now()}
                _save(rec_path, record)
            except (urllib.error.URLError, IOError, OSError) as e:
                print(f"  FAILED {rel}: {e}", file=sys.stderr)
                failures += 1
    return failures


def _matches(path: str, pub: str) -> bool:
    kind, _, want = pub.partition(":")
    if kind == "sha256":
        return _sha256(path) == want
    if kind == "git-blob":
        return _git_blob_id(path) == want
    return False


# ---- GitHub runtime assets -----------------------------------------------------------------------

def _gh_release_with(repo: str, asset_glob: str) -> tuple[dict, dict]:
    rels = _json(f"https://api.github.com/repos/{repo}/releases?per_page=30")
    rels = [r for r in rels if not r.get("draft") and not r.get("prerelease")]
    rels.sort(key=lambda r: r.get("published_at") or "", reverse=True)
    for r in rels:
        for a in r.get("assets", []):
            if fnmatch.fnmatchcase(a["name"], asset_glob):
                return r, a
    raise LookupError(f"no published release of {repo} carries {asset_glob}")


def fetch_runtime(manifest: dict, home: str, only: set[str], dry: bool, verify: bool) -> int:
    rec_path = os.path.join(home, "runtime.installed.json")
    record = _load(rec_path)
    failures = 0
    for key, entry in manifest.get("runtime", {}).items():
        if only and key not in only:
            continue
        try:
            rel, asset = _gh_release_with(entry["github"], entry["asset"])
        except (urllib.error.URLError, LookupError, ValueError) as e:
            print(f"[{key}] {entry['github']}: {e}", file=sys.stderr)
            failures += 1
            continue
        pub = asset.get("digest") or ""
        dest = os.path.join(home, entry["dest"], entry.get("as") or asset["name"])
        print(f"[{key}] {entry['github']} release {rel.get('tag_name')} ({(rel.get('published_at') or '')[:10]}): "
              f"{asset['name']} {asset['size']} B {pub or 'NO PUBLISHER DIGEST'}", flush=True)
        if dry:
            continue
        if not pub.startswith("sha256:"):
            print(f"  refused: the release publishes no sha256 digest for {asset['name']}", file=sys.stderr)
            failures += 1
            continue
        old = record.get(key, {})
        if os.path.isfile(dest) and old.get("publisher_digest") == pub and not verify:
            continue
        try:
            if not (os.path.isfile(dest) and "sha256:" + _sha256(dest) == pub):
                _download(asset["browser_download_url"], dest, asset["size"])
            if "sha256:" + _sha256(dest) != pub:
                os.unlink(dest)
                raise IOError(f"digest mismatch for {asset['name']}; file deleted")
            if entry.get("executable"):
                os.chmod(dest, 0o755)
            record[key] = {"file": os.path.relpath(dest, home), "digest": pub, "publisher_digest": pub,
                           "size": asset["size"], "source": f"github:{entry['github']}@{rel.get('tag_name')}",
                           "fetched_at": _now()}
            _save(rec_path, record)
        except (urllib.error.URLError, IOError, OSError) as e:
            print(f"  FAILED {key}: {e}", file=sys.stderr)
            failures += 1
    return failures


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--home")
    ap.add_argument("--only", default="", help="comma-separated manifest keys")
    ap.add_argument("--runtime", action="store_true", help="fetch the runtime assets only")
    ap.add_argument("--models", action="store_true", help="fetch the models only")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--verify", action="store_true")
    ap.add_argument("--manifest", default=os.path.join(HERE, "models.json"))
    a = ap.parse_args()
    home = _home(a.home)
    with open(a.manifest, encoding="utf-8") as f:
        manifest = json.load(f)
    only = {k.strip() for k in a.only.split(",") if k.strip()}
    both = not a.runtime and not a.models
    print(f"AXERA_HOME={home}{' (dry run)' if a.dry_run else ''}", flush=True)
    fails = 0
    if a.runtime or both:
        fails += fetch_runtime(manifest, home, only, a.dry_run, a.verify)
    if a.models or both:
        fails += fetch_models(manifest, home, only, a.dry_run, a.verify)
    print("done" if not fails else f"{fails} failure(s)", flush=True)
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
